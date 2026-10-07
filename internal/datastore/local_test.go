package datastore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
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
	if err := os.MkdirAll(filepath.Join(store.root, localMetadataDirName), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store.root, store.metadataPath(key)), []byte(`{"contentType":"application/json"}`), 0o644); err != nil {
		t.Fatal(err)
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

func TestLocalDatastoreRejectsSymlinkEscapes(t *testing.T) {
	for _, operation := range []string{"get", "exists", "list", "put", "put-if-absent"} {
		t.Run(operation, func(t *testing.T) {
			root, outside := t.TempDir(), t.TempDir()
			if err := os.WriteFile(filepath.Join(outside, "original.txt"), []byte("outside"), 0o644); err != nil {
				t.Fatal(err)
			}
			link := filepath.Join(root, "objects")
			if err := os.Symlink(outside, link); err != nil {
				if runtime.GOOS == "windows" {
					t.Skipf("symlink creation unavailable: %v", err)
				}
				t.Fatal(err)
			}
			store, err := NewLocalDatastore(LocalConfig{Root: root})
			if err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			switch operation {
			case "get":
				_, err = store.Get(ctx, MustKey("objects/original.txt"))
			case "exists":
				_, err = store.Exists(ctx, MustKey("objects/original.txt"))
			case "list":
				_, err = store.List(ctx, MustKey("objects"))
			case "put", "put-if-absent":
				_, err = store.Put(ctx, MustKey("objects/nested/new.txt"), []byte("escaped"), PutOptions{IfAbsent: operation == "put-if-absent"})
			}
			if err == nil {
				t.Error("operation followed a symlink outside the datastore")
			}
			entries, err := os.ReadDir(outside)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 1 || entries[0].Name() != "original.txt" {
				t.Fatalf("operation modified the outside directory: %v", entries)
			}
		})
	}
}

func TestLocalDatastoreRejectsMetadataSymlinkEscape(t *testing.T) {
	for _, ifAbsent := range []bool{false, true} {
		t.Run(fmt.Sprint(ifAbsent), func(t *testing.T) {
			root, outside := t.TempDir(), t.TempDir()
			if err := os.Symlink(outside, filepath.Join(root, localMetadataDirName)); err != nil {
				if runtime.GOOS == "windows" {
					t.Skipf("symlink creation unavailable: %v", err)
				}
				t.Fatal(err)
			}
			store, err := NewLocalDatastore(LocalConfig{Root: root})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.Put(context.Background(), MustKey("objects/value"), []byte("value"), PutOptions{IfAbsent: ifAbsent}); err == nil {
				t.Error("metadata publication followed an escaping symlink")
			}
			entries, err := os.ReadDir(outside)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 0 {
				t.Fatalf("metadata publication wrote outside the datastore: %v", entries)
			}
		})
	}
}

func TestLocalDatastoreConfinesLeafSymlinks(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "value"), []byte("outside"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "value"), filepath.Join(root, "value")); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("symlink creation unavailable: %v", err)
		}
		t.Fatal(err)
	}
	store, err := NewLocalDatastore(LocalConfig{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(context.Background(), MustKey("value")); err == nil {
		t.Fatal("read followed an escaping leaf symlink")
	}
	if err := os.WriteFile(filepath.Join(root, "inside"), []byte("inside"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("inside", filepath.Join(root, "alias")); err != nil {
		t.Fatal(err)
	}
	object, err := store.Get(context.Background(), MustKey("alias"))
	if err != nil || string(object.Body) != "inside" {
		t.Fatalf("contained relative symlink: body=%q err=%v", object.Body, err)
	}
}

func TestLocalDatastoreReadsDoNotCreateMissingRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "missing")
	store, err := NewLocalDatastore(LocalConfig{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	ctx, key := context.Background(), MustKey("objects/value")
	if _, err := store.Get(ctx, key); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get missing root: %v", err)
	}
	if exists, err := store.Exists(ctx, key); err != nil || exists {
		t.Fatalf("exists missing root: %t, %v", exists, err)
	}
	if objects, err := store.List(ctx, MustKey("objects")); err != nil || len(objects) != 0 {
		t.Fatalf("list missing root: %v, %v", objects, err)
	}
	if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read operations created the missing store: %v", err)
	}
}
