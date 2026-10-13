package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/Sly1029/massive/internal/runevent"
	"golang.org/x/term"
)

type progressStep struct {
	status   string
	duration float64
	started  time.Time
	detail   string
}

type progressModel struct {
	steps map[string]*progressStep
	order []string
	last  runevent.Event
}

func (model *progressModel) observe(event runevent.Event) {
	model.last = event
	if event.NodeID == "" {
		return
	}
	step := model.steps[event.NodeID]
	if step == nil {
		step = &progressStep{status: "running", started: time.Now()}
		model.steps[event.NodeID] = step
		model.order = append(model.order, event.NodeID)
	}
	switch event.Type {
	case "step.attempt.started":
		step.detail = fmt.Sprintf("attempt %d", event.Attempt)
		if event.ItemIndex != nil {
			step.detail = fmt.Sprintf("item %d attempt %d", *event.ItemIndex, event.Attempt)
		}
	case "step.finished":
		step.status, step.duration = event.Status, *event.DurationMS
	}
}

type runProgress struct {
	// The ordered queue absorbs bursts without blocking workers on either
	// output. A buffered wake channel transfers ownership to the sole file
	// writer. Its renderer channel is independent and best-effort.
	mu          sync.Mutex
	queue       []runevent.Event
	closing     bool
	wake        chan struct{}
	done        chan struct{}
	live        chan runevent.Event
	complete    chan progressModel
	renderDone  chan struct{}
	writer      io.Writer
	events      *os.File
	eventErr    error
	mode        string
	tty, active bool
}

func newRunProgress(path, mode string, stderr *os.File) (*runProgress, error) {
	if path == "" && mode == "none" {
		return nil, nil
	}
	progress := &runProgress{writer: stderr, mode: mode, wake: make(chan struct{}, 1), done: make(chan struct{})}
	if path != "" {
		file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return nil, fmt.Errorf("create run events file: %w", err)
		}
		progress.events = file
	}
	progress.tty = mode == "auto" && term.IsTerminal(int(stderr.Fd()))
	if mode != "none" {
		progress.live = make(chan runevent.Event, 128)
		progress.complete = make(chan progressModel, 1)
		progress.renderDone = make(chan struct{})
		go progress.render()
	}
	go progress.write()
	return progress, nil
}

func (progress *runProgress) event(event runevent.Event) {
	progress.mu.Lock()
	progress.queue = append(progress.queue, event)
	progress.mu.Unlock()
	select {
	case progress.wake <- struct{}{}:
	default:
	}
}

func (progress *runProgress) write() {
	defer close(progress.done)
	model := progressModel{steps: make(map[string]*progressStep)}
	var buffer *bufio.Writer
	var encoder *json.Encoder
	if progress.events != nil {
		buffer = bufio.NewWriter(progress.events)
		encoder = json.NewEncoder(buffer)
	}
	for {
		<-progress.wake
		progress.mu.Lock()
		batch, closing := progress.queue, progress.closing
		progress.queue = nil
		progress.mu.Unlock()
		for _, event := range batch {
			if encoder != nil && progress.eventErr == nil {
				progress.eventErr = encoder.Encode(event)
			}
			if progress.live != nil {
				model.observe(event)
				select {
				case progress.live <- event:
				default:
				}
			}
		}
		if buffer != nil && progress.eventErr == nil {
			progress.eventErr = buffer.Flush()
		}
		if closing {
			break
		}
	}
	if progress.events != nil {
		if err := progress.events.Close(); progress.eventErr == nil {
			progress.eventErr = err
		}
		if progress.eventErr != nil {
			progress.eventErr = fmt.Errorf("write run event stream: %w", progress.eventErr)
		}
	}
	if progress.live != nil {
		// Summary state sees every event even when live rendering drops lines.
		progress.complete <- model
		close(progress.live)
	}
}

func (progress *runProgress) close() error {
	if progress == nil {
		return nil
	}
	progress.mu.Lock()
	progress.closing = true
	progress.mu.Unlock()
	select {
	case progress.wake <- struct{}{}:
	default:
	}
	<-progress.done
	if progress.renderDone != nil {
		<-progress.renderDone
	}
	return progress.eventErr
}

func (progress *runProgress) render() {
	defer close(progress.renderDone)
	model := progressModel{steps: make(map[string]*progressStep)}
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case event, ok := <-progress.live:
			if !ok {
				progress.clear()
				complete := <-progress.complete
				if complete.last.Type == "run.finished" {
					fmt.Fprintln(progress.writer, "\nSTEP                     STATUS          DURATION")
					for _, id := range complete.order {
						step := complete.steps[id]
						fmt.Fprintf(progress.writer, "%-24s %-15s %.2fs\n", safeProgressText(id), step.status, step.duration/1000)
					}
					fmt.Fprintf(progress.writer, "run %s %s; total wall time %.2fs\n", safeProgressText(complete.last.RunID), complete.last.Status, complete.last.ElapsedMS/1000)
				}
				return
			}
			model.observe(event)
			if progress.mode == "plain" || progress.tty {
				progress.renderEvent(event)
			}
			progress.compact(model)
		case <-ticker.C:
			progress.compact(model)
		}
	}
}

func (progress *runProgress) renderEvent(event runevent.Event) {
	progress.clear()
	label := event.NodeID
	if event.ItemIndex != nil {
		label = fmt.Sprintf("%s[%d]", label, *event.ItemIndex)
	}
	line := ""
	switch event.Type {
	case "run.started":
		line = fmt.Sprintf("run %s started", event.RunID)
	case "step.attempt.started":
		line = fmt.Sprintf("%s attempt %d started", label, event.Attempt)
	case "step.attempt.finished":
		line = fmt.Sprintf("%s attempt %d %s (%.2fs) [%s]", label, event.Attempt, event.Status, *event.DurationMS/1000, event.Classification)
	case "step.finished":
		line = fmt.Sprintf("%s %s (%.2fs)", label, event.Status, *event.DurationMS/1000)
	}
	if line != "" && (!progress.tty || event.Type == "run.started" || event.Type == "step.finished" || (event.Type == "step.attempt.finished" && event.Status != "succeeded")) {
		fmt.Fprintln(progress.writer, safeProgressText(line))
	}
	if event.Type == "step.attempt.finished" && event.Status != "succeeded" && event.Diagnostic != "" {
		fmt.Fprintf(progress.writer, "  diagnostic: %s\n", safeProgressText(event.Diagnostic))
	}
}

func (progress *runProgress) clear() {
	if progress.active {
		fmt.Fprint(progress.writer, "\r\x1b[2K")
		progress.active = false
	}
}

func (progress *runProgress) compact(model progressModel) {
	if !progress.tty {
		return
	}
	var active []string
	for id, step := range model.steps {
		if step.status == "running" {
			active = append(active, fmt.Sprintf("%s %s %.1fs", safeProgressText(id), step.detail, time.Since(step.started).Seconds()))
		}
	}
	if len(active) == 0 {
		return
	}
	sort.Strings(active)
	text := strings.Join(active, " | ")
	if len([]rune(text)) > 72 {
		text = string([]rune(text)[:71]) + "…"
	}
	fmt.Fprintf(progress.writer, "\r\x1b[2K%s", text)
	progress.active = true
}

func safeProgressText(text string) string {
	return strings.Map(func(character rune) rune {
		if unicode.IsControl(character) && character != '\n' && character != '\t' {
			return '�'
		}
		return character
	}, text)
}
