package artifact

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/Sly1029/massive/internal/datastore"
)

const (
	testBody       = `{"value":42}`
	testBodyHash   = "sha256:dc60e632a90329ccfd34fbe904d94704dbbb6669575185e26389854ff64139c3"
	testSchema     = `{"additionalProperties":false,"properties":{"value":{"type":"integer"}},"required":["value"],"type":"object"}`
	testSchemaHash = "sha256:cc6d2156c280bb3efad77622be3c070cf9a18fbf7ddaf4db6a7c6988a417048a"
	testPlanHash   = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
)

func TestPublishJSONCommitsBodyBeforeManifestAndConvergesOnRetry(t *testing.T) {
	ctx := context.Background()
	store := localStore(t)
	putSchema(t, store)
	bodyKey := datastore.MustKey("blobs/sha256/dc60e632a90329ccfd34fbe904d94704dbbb6669575185e26389854ff64139c3")
	if _, err := store.Put(ctx, bodyKey, []byte(testBody), datastore.PutOptions{ContentType: JSONContentType, IfAbsent: true}); err != nil {
		t.Fatal(err)
	}
	destination := Destination{
		ManifestKey: datastore.MustKey("projects/project/runs/run-1/steps/task/1/output-manifest.json"),
		Schema:      testSchemaHash,
	}
	producer := Producer{ProjectKey: "project", PlanHash: testPlanHash, RunID: "run-1", NodeID: "task", Attempt: 1}

	first, err := PublishJSON(ctx, store, destination, producer, []byte(testBody))
	if err != nil {
		t.Fatal(err)
	}
	second, err := PublishJSON(ctx, store, destination, producer, []byte(testBody))
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("retry publication differs: first=%#v second=%#v", first, second)
	}
	if first.Body.Key != bodyKey.String() || first.Body.Hash != testBodyHash || first.Body.Size != len(testBody) {
		t.Fatalf("published body = %#v", first.Body)
	}

	manifest, err := store.Get(ctx, destination.ManifestKey)
	if err != nil {
		t.Fatal(err)
	}
	wantManifest := `{"body":{"contentType":"application/json","hash":"sha256:dc60e632a90329ccfd34fbe904d94704dbbb6669575185e26389854ff64139c3","key":"blobs/sha256/dc60e632a90329ccfd34fbe904d94704dbbb6669575185e26389854ff64139c3","size":12},"encoding":"canonical-json-v0","kind":"DataArtifactManifest","producer":{"attempt":1,"nodeId":"task","planHash":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","projectKey":"project","runId":"run-1"},"schema":"sha256:cc6d2156c280bb3efad77622be3c070cf9a18fbf7ddaf4db6a7c6988a417048a","schemaVersion":0}`
	if !bytes.Equal(manifest.Body, []byte(wantManifest)) {
		t.Fatalf("manifest bytes\n got: %s\nwant: %s", manifest.Body, wantManifest)
	}

	resolved, body, err := ResolveJSON(ctx, store, destination, producer)
	if err != nil {
		t.Fatal(err)
	}
	if resolved != first || !bytes.Equal(body, []byte(testBody)) {
		t.Fatalf("resolved publication = %#v body=%s", resolved, body)
	}
}

func TestPublishJSONRejectsBodyAndManifestConflicts(t *testing.T) {
	t.Run("body", func(t *testing.T) {
		ctx := context.Background()
		store := localStore(t)
		putSchema(t, store)
		bodyKey := datastore.MustKey("blobs/sha256/dc60e632a90329ccfd34fbe904d94704dbbb6669575185e26389854ff64139c3")
		if _, err := store.Put(ctx, bodyKey, []byte(`{"value":0}`), datastore.PutOptions{ContentType: JSONContentType, IfAbsent: true}); err != nil {
			t.Fatal(err)
		}
		_, err := PublishJSON(ctx, store, testDestination(), testProducer(), []byte(testBody))
		if !errors.Is(err, ErrBodyConflict) {
			t.Fatalf("PublishJSON error = %v, want ErrBodyConflict", err)
		}
	})

	t.Run("manifest", func(t *testing.T) {
		ctx := context.Background()
		store := localStore(t)
		putSchema(t, store)
		if _, err := PublishJSON(ctx, store, testDestination(), testProducer(), []byte(testBody)); err != nil {
			t.Fatal(err)
		}
		_, err := PublishJSON(ctx, store, testDestination(), testProducer(), []byte(`{"value":43}`))
		if !errors.Is(err, ErrManifestConflict) {
			t.Fatalf("PublishJSON error = %v, want ErrManifestConflict", err)
		}
	})
}

func testDestination() Destination {
	return Destination{
		ManifestKey: datastore.MustKey("projects/project/runs/run-1/steps/task/1/output-manifest.json"),
		Schema:      testSchemaHash,
	}
}

func testProducer() Producer {
	return Producer{ProjectKey: "project", PlanHash: testPlanHash, RunID: "run-1", NodeID: "task", Attempt: 1}
}

func localStore(t *testing.T) datastore.Datastore {
	t.Helper()
	store, err := datastore.NewLocalDatastore(datastore.LocalConfig{Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func putSchema(t *testing.T, store datastore.Datastore) {
	t.Helper()
	key := datastore.MustKey("blobs/sha256/cc6d2156c280bb3efad77622be3c070cf9a18fbf7ddaf4db6a7c6988a417048a")
	if _, err := store.Put(context.Background(), key, []byte(testSchema), datastore.PutOptions{ContentType: JSONContentType, IfAbsent: true}); err != nil {
		t.Fatal(err)
	}
}
