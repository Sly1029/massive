package main

import (
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
)

type progressStep struct {
	status   string
	duration float64
	started  time.Time
	detail   string
}
type runProgress struct {
	mu       sync.Mutex
	writer   io.Writer
	events   *os.File
	eventErr error
	mode     string
	tty      bool
	active   bool
	steps    map[string]*progressStep
	order    []string
	stop     chan struct{}
	done     chan struct{}
}

func newRunProgress(path, mode string, stderr *os.File) (*runProgress, error) {
	progress := &runProgress{writer: stderr, mode: mode, steps: make(map[string]*progressStep)}
	if path != "" {
		// A new regular file keeps events away from stdout, existing journals, and
		// special descriptors. Direct writes flush each complete NDJSON record.
		file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return nil, fmt.Errorf("create run events file: %w", err)
		}
		progress.events = file
	}
	progress.tty = mode == "auto" && progressTerminal(stderr)
	if progress.tty {
		progress.stop, progress.done = make(chan struct{}), make(chan struct{})
		go func() {
			defer close(progress.done)
			ticker := time.NewTicker(250 * time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case <-progress.stop:
					return
				case <-ticker.C:
					progress.mu.Lock()
					progress.compact()
					progress.mu.Unlock()
				}
			}
		}()
	}
	return progress, nil
}

func (progress *runProgress) close() error {
	if progress.stop != nil {
		close(progress.stop)
		<-progress.done
	}
	progress.mu.Lock()
	defer progress.mu.Unlock()
	progress.clear()
	if progress.events != nil {
		if err := progress.events.Close(); progress.eventErr == nil {
			progress.eventErr = err
		}
	}
	return progress.eventErr
}

func (progress *runProgress) event(event runevent.Event) {
	progress.mu.Lock()
	defer progress.mu.Unlock()
	if progress.events != nil && progress.eventErr == nil {
		body, err := json.Marshal(event)
		if err == nil {
			_, err = runevent.Parse(body)
		}
		if err == nil {
			_, err = progress.events.Write(append(body, '\n'))
		}
		if err != nil {
			progress.eventErr = fmt.Errorf("write run event stream: %w", err)
		}
	}
	if progress.mode == "none" || progress.mode == "" {
		return
	}
	progress.clear()
	label := event.NodeID
	if event.ItemIndex != nil {
		label = fmt.Sprintf("%s[%d]", label, *event.ItemIndex)
	}
	line := ""
	switch event.Type {
	case "run.started":
		line = fmt.Sprintf("run %s started (%d steps)", event.RunID, *event.NodeCount)
	case "step.started":
		progress.steps[event.NodeID] = &progressStep{status: "running", started: time.Now()}
		progress.order = append(progress.order, event.NodeID)
		line = label + " started"
	case "step.attempt.started":
		line = fmt.Sprintf("%s attempt %d/%d started", label, event.Attempt, event.MaxAttempts)
		if step := progress.steps[event.NodeID]; step != nil {
			step.detail = fmt.Sprintf("attempt %d/%d", event.Attempt, event.MaxAttempts)
		}
	case "step.attempt.finished":
		line = fmt.Sprintf("%s attempt %d/%d %s (%.2fs) [%s]", label, event.Attempt, event.MaxAttempts, event.Status, *event.DurationMS/1000, event.Classification)
	case "map.progress":
		line = fmt.Sprintf("%s map %d/%d completed, %d failed", label, *event.Completed, *event.Total, *event.Failed)
		if step := progress.steps[event.NodeID]; step != nil {
			step.detail = fmt.Sprintf("%d/%d, %d failed", *event.Completed, *event.Total, *event.Failed)
		}
	case "step.finished":
		step := progress.steps[event.NodeID]
		if step == nil {
			step = &progressStep{}
			progress.steps[event.NodeID] = step
			progress.order = append(progress.order, event.NodeID)
		}
		step.status, step.duration = event.Status, *event.DurationMS
		line = fmt.Sprintf("%s %s (%.2fs)", label, event.Status, *event.DurationMS/1000)
	case "run.finished":
		fmt.Fprintln(progress.writer, "\nSTEP                     STATUS          DURATION")
		for _, id := range progress.order {
			step := progress.steps[id]
			fmt.Fprintf(progress.writer, "%-24s %-15s %.2fs\n", safeProgressText(id), step.status, step.duration/1000)
		}
		fmt.Fprintf(progress.writer, "run %s %s; total wall time %.2fs\n", event.RunID, event.Status, *event.DurationMS/1000)
		if event.ExitHook != nil {
			fmt.Fprintf(progress.writer, "exit hook %s %s\n", event.ExitHook.NodeID, event.ExitHook.Status)
		}
	}
	// Compact terminals retain finished lines and immediate failure context;
	// transient starts and map counts live on the single updating status line.
	if line != "" && (!progress.tty || event.Type == "run.started" || event.Type == "step.finished" || (event.Type == "step.attempt.finished" && event.Status != "succeeded")) {
		fmt.Fprintln(progress.writer, safeProgressText(line))
	}
	if event.Type == "step.attempt.finished" && event.Status != "succeeded" {
		if event.Diagnostic != "" {
			fmt.Fprintf(progress.writer, "  diagnostic: %s\n", safeProgressText(event.Diagnostic))
		}
		if event.LogTail != "" {
			fmt.Fprintf(progress.writer, "  log tail (%s):\n%s\n", safeProgressText(label), safeProgressText(event.LogTail))
		}
	}
	progress.compact()
}

func (progress *runProgress) clear() {
	if progress.active {
		fmt.Fprint(progress.writer, "\r\x1b[2K")
		progress.active = false
	}
}

func (progress *runProgress) compact() {
	if !progress.tty {
		return
	}
	var active []string
	for id, step := range progress.steps {
		if step.status == "running" {
			active = append(active, fmt.Sprintf("%s %s %.1fs", safeProgressText(id), step.detail, time.Since(step.started).Seconds()))
		}
	}
	if len(active) == 0 {
		return
	}
	sort.Strings(active)
	text := strings.Join(active, " | ")
	// Keep the compact view to one line even on small terminals.
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
