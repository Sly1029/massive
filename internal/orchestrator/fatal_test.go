package orchestrator

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Sly1029/massive/internal/datastore"
	"github.com/Sly1029/massive/internal/runjournal"
)

func TestFatalPublicationKeepsFirstSafeCauseAndRejectsUnboundedRecords(t *testing.T) {
	store, err := datastore.NewLocalDatastore(datastore.LocalConfig{Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	key, err := FatalRunKey("fatal-conformance", "run-1")
	if err != nil {
		t.Fatal(err)
	}
	if failure, err := ReadFatalRun(ctx, store, key); err != nil || failure != nil {
		t.Fatalf("empty run: %+v %v", failure, err)
	}
	first := runjournal.FatalFailure{Type: "input.invalid", Message: strings.Repeat("🚀", 1024), NodeID: "stop"}
	if err := PublishFatalRun(ctx, store, key, first); err != nil {
		t.Fatal(err)
	}
	later := runjournal.FatalFailure{Type: "source.unavailable", Message: "Upstream source unavailable", NodeID: "sibling"}
	if err := PublishFatalRun(ctx, store, key, later); err != nil {
		t.Fatal(err)
	}
	if got, err := ReadFatalRun(ctx, store, key); err != nil || got == nil || *got != first {
		t.Fatalf("first cause: %+v %v", got, err)
	}
	for _, body := range []string{`{"kind":"fatal","type":"input.invalid","message":"Safe cause","nodeId":"stop","exception":"SECRET"}`, strings.Repeat("x", 14*1024)} {
		if _, err := store.Put(ctx, key, []byte(body), datastore.PutOptions{}); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadFatalRun(ctx, store, key); err == nil {
			t.Fatal("invalid fatal record accepted")
		}
	}
}

func TestFatalPollingRetriesInvalidStoreReadsWithoutCancelling(t *testing.T) {
	store, err := datastore.NewLocalDatastore(datastore.LocalConfig{Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	key, err := FatalRunKey("fatal-conformance", "polling")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put(t.Context(), key, []byte("invalid JSON"), datastore.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	parent, end := context.WithTimeout(t.Context(), 20*time.Second)
	defer end()
	ctx, stop := WithFatalRunCancellation(parent, store, key)
	defer stop()
	select {
	case <-ctx.Done():
		t.Fatalf("read failure cancelled invocation: %v", context.Cause(ctx))
	case <-time.After(5100 * time.Millisecond):
	}
	failure := runjournal.FatalFailure{Message: "Upstream source unavailable", NodeID: "stop"}
	body := []byte(`{"message":"Upstream source unavailable","nodeId":"stop"}`)
	if _, err := store.Put(parent, key, body, datastore.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	<-ctx.Done()
	var fatal *FatalRunError
	if !errors.As(context.Cause(ctx), &fatal) || fatal.Failure != failure {
		t.Fatalf("polling cause: %v", context.Cause(ctx))
	}
}
