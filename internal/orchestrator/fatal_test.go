package orchestrator

import (
	"context"
	"strings"
	"testing"

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
	first := runjournal.FatalFailure{Kind: "fatal", Category: "budget.exhausted", Message: strings.Repeat("🚀", 1024), NodeID: "stop"}
	if err := PublishFatalRun(ctx, store, key, first); err != nil {
		t.Fatal(err)
	}
	later := runjournal.FatalFailure{Kind: "fatal", Category: "credential.revoked", Message: "Credential revoked", NodeID: "sibling"}
	if err := PublishFatalRun(ctx, store, key, later); err != nil {
		t.Fatal(err)
	}
	if got, err := ReadFatalRun(ctx, store, key); err != nil || got == nil || *got != first {
		t.Fatalf("first cause: %+v %v", got, err)
	}
	for _, body := range []string{`{"kind":"fatal","category":"policy","message":"Safe cause","nodeId":"stop","exception":"SECRET"}`, strings.Repeat("x", 14*1024)} {
		if _, err := store.Put(ctx, key, []byte(body), datastore.PutOptions{}); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadFatalRun(ctx, store, key); err == nil {
			t.Fatal("invalid fatal record accepted")
		}
	}
}
