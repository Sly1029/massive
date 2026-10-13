package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Sly1029/massive/internal/runevent"
)

// A real pipe fills until the human renderer stalls. The event file must still
// receive every ordered record, and closing must drain both independent sinks.
func TestRunEventsDrainWhileStderrIsBlocked(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	defer writer.Close()
	path := filepath.Join(t.TempDir(), "events.ndjson")
	progress, err := newRunProgress(path, "plain", writer)
	if err != nil {
		t.Fatal(err)
	}
	const count = 5000
	produced := make(chan struct{})
	go func() {
		for index := range count {
			duration := 0.0
			progress.event(runevent.Event{SchemaVersion: 1, Type: "step.finished", Sequence: index + 1, ProjectKey: "test", RunID: "blocked", Time: time.Now().UTC().Format(time.RFC3339Nano), ElapsedMS: float64(index), NodeID: fmt.Sprintf("step-%d", index), Status: "succeeded", DurationMS: &duration})
		}
		progress.event(runevent.Event{SchemaVersion: 1, Type: "run.finished", Sequence: count + 1, ProjectKey: "test", RunID: "blocked", Time: time.Now().UTC().Format(time.RFC3339Nano), ElapsedMS: count, Status: "succeeded"})
		close(produced)
	}()
	select {
	case <-produced:
	case <-time.After(5 * time.Second):
		t.Fatal("producer blocked on stderr")
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		body, _ := os.ReadFile(path)
		if bytes.Count(body, []byte("\n")) == count+1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("event file blocked on stderr or dropped events")
		}
		time.Sleep(10 * time.Millisecond)
	}
	closed := make(chan error, 1)
	go func() { closed <- progress.close() }()
	var rendered bytes.Buffer
	drained := make(chan struct{})
	go func() { _, _ = io.Copy(&rendered, reader); close(drained) }()
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	<-drained
	events := readProgressEvents(t, path)
	if len(events) != count+1 {
		t.Fatalf("got %d events", len(events))
	}
	for index, event := range events {
		if event.Sequence != index+1 {
			t.Fatal("event order changed")
		}
	}
	if !strings.Contains(rendered.String(), "step-4999") || !strings.Contains(rendered.String(), "run blocked succeeded") {
		t.Fatal("summary lost coalesced live events")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("event permissions = %v", info.Mode())
	}
}

func TestDisabledProgressHasNoObserver(t *testing.T) {
	progress, err := newRunProgress("", "none", os.Stderr)
	if err != nil || progress != nil {
		t.Fatalf("disabled progress allocated an observer: %v, %v", progress, err)
	}
}
