package datastore

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"
)

func TestLocalDatastoreContract(t *testing.T) {
	RunDatastoreContract(t, func(t *testing.T) Datastore {
		t.Helper()

		store, err := NewLocalDatastore(LocalConfig{Root: t.TempDir()})
		if err != nil {
			t.Fatalf("new local datastore: %v", err)
		}
		return store
	})
}

func TestLocalDatastoreIfAbsentMetadataIsPublishedBeforeBodyAndNeverOverwritten(t *testing.T) {
	store, err := NewLocalDatastore(LocalConfig{Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	key := MustKey("objects/race.json")
	body := []byte(`{"value":42}`)

	start := make(chan struct{})
	type result struct {
		contentType string
		winner      bool
		err         error
	}
	results := make(chan result, 2)
	var wait sync.WaitGroup
	for _, contentType := range []string{"application/json", "application/x-race"} {
		wait.Add(1)
		go func(contentType string) {
			defer wait.Done()
			<-start
			_, err := store.Put(context.Background(), key, body, PutOptions{ContentType: contentType, IfAbsent: true})
			if err == nil {
				results <- result{contentType: contentType, winner: true}
				return
			}
			if !errors.Is(err, ErrAlreadyExists) {
				results <- result{contentType: contentType, err: err}
				return
			}
			results <- result{contentType: contentType}
		}(contentType)
	}
	close(start)
	wait.Wait()
	close(results)

	winners := 0
	winningContentType := ""
	for result := range results {
		if result.err != nil {
			t.Fatal(result.err)
		}
		if result.winner {
			winners++
			winningContentType = result.contentType
		}
	}
	if winners != 1 {
		t.Fatalf("conditional metadata race winners = %d, want 1", winners)
	}
	object, err := store.Get(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(object.Body, body) {
		t.Fatalf("body changed during conditional race: %s", object.Body)
	}
	if object.Info.ContentType != winningContentType {
		t.Fatalf("loser overwrote content type: got %q, want winner %q", object.Info.ContentType, winningContentType)
	}
}

func TestLocalDatastoreRecoversMetadataOnlyIfAbsentPublication(t *testing.T) {
	store, err := NewLocalDatastore(LocalConfig{Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	key := MustKey("objects/recover.json")
	if installed, err := store.writeMetadataIfAbsent(key, "application/json"); err != nil || !installed {
		t.Fatalf("create metadata-only crash state: installed=%t err=%v", installed, err)
	}
	if _, err := store.Get(context.Background(), key); !errors.Is(err, ErrNotFound) {
		t.Fatalf("metadata-only object Get error = %v, want ErrNotFound", err)
	}
	if _, err := store.Put(context.Background(), key, []byte(`{"recovered":true}`), PutOptions{ContentType: "application/json", IfAbsent: true}); err != nil {
		t.Fatalf("recover metadata-only publication: %v", err)
	}
	object, err := store.Get(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	if object.Info.ContentType != "application/json" || string(object.Body) != `{"recovered":true}` {
		t.Fatalf("recovered object = %#v", object)
	}
}
