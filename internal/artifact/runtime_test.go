package artifact

import (
	"bytes"
	"context"
	"errors"
	"sync"
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

func TestPublishJSONValidatesBeforeWritingAnything(t *testing.T) {
	ctx := context.Background()
	store := localStore(t)
	putSchema(t, store)

	_, err := PublishJSON(ctx, store, testDestination(), testProducer(), []byte(`{"value":"wrong"}`))
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("PublishJSON error = %v, want ErrValidation", err)
	}
	published, err := store.Exists(ctx, testDestination().ManifestKey)
	if err != nil {
		t.Fatal(err)
	}
	if published {
		t.Fatal("invalid value published a manifest")
	}
	blobs, err := store.List(ctx, datastore.MustKey("blobs"))
	if err != nil {
		t.Fatal(err)
	}
	if len(blobs) != 1 || blobs[0].Key.String() != "blobs/sha256/cc6d2156c280bb3efad77622be3c070cf9a18fbf7ddaf4db6a7c6988a417048a" {
		t.Fatalf("invalid value wrote content bodies: %#v", blobs)
	}
}

func TestPublishJSONRejectsUnsafeProducerIdentityBeforeWriting(t *testing.T) {
	invalidProducers := []Producer{
		{ProjectKey: "example/../escape", PlanHash: testPlanHash, RunID: "run-1", NodeID: "task", Attempt: 1},
		{ProjectKey: "example/python-e2e", PlanHash: testPlanHash, RunID: "../escape", NodeID: "task", Attempt: 1},
		{ProjectKey: "example/python-e2e", PlanHash: testPlanHash, RunID: "run-1", NodeID: "nested/task", Attempt: 1},
		{ProjectKey: "example\\python", PlanHash: testPlanHash, RunID: "run-1", NodeID: "task", Attempt: 1},
		{ProjectKey: "example/python-e2e", PlanHash: testPlanHash, RunID: ".", NodeID: "task", Attempt: 1},
	}

	for _, producer := range invalidProducers {
		t.Run(producer.ProjectKey+producer.RunID+producer.NodeID, func(t *testing.T) {
			ctx := context.Background()
			store := localStore(t)
			putSchema(t, store)
			if _, err := PublishJSON(ctx, store, testDestination(), producer, []byte(testBody)); !errors.Is(err, ErrValidation) {
				t.Fatalf("PublishJSON error = %v, want ErrValidation", err)
			}
			assertOnlySchemaBlob(t, store)
			published, err := store.Exists(ctx, testDestination().ManifestKey)
			if err != nil {
				t.Fatal(err)
			}
			if published {
				t.Fatal("unsafe producer identity published a manifest")
			}
		})
	}
}

func TestPublishJSONAcceptsExistingSafeIdentifierCharacters(t *testing.T) {
	ctx := context.Background()
	store := localStore(t)
	putSchema(t, store)
	producer := Producer{ProjectKey: ".example/_python-e2e", PlanHash: testPlanHash, RunID: "_run", NodeID: ".task", Attempt: 1}
	destination := Destination{
		ManifestKey: datastore.MustKey("projects/.example/_python-e2e/runs/_run/steps/.task/1/output-manifest.json"),
		Schema:      testSchemaHash,
	}
	if _, err := PublishJSON(ctx, store, destination, producer, []byte(testBody)); err != nil {
		t.Fatalf("PublishJSON rejected safe producer identifiers: %v", err)
	}
}

func TestPublishJSONConcurrentIdenticalPublicationsConverge(t *testing.T) {
	ctx := context.Background()
	store := localStore(t)
	putSchema(t, store)

	const publishers = 32
	start := make(chan struct{})
	results := make(chan PublishedJSON, publishers)
	errs := make(chan error, publishers)
	var wait sync.WaitGroup
	for range publishers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			published, err := PublishJSON(ctx, store, testDestination(), testProducer(), []byte(testBody))
			if err != nil {
				errs <- err
				return
			}
			results <- published
		}()
	}
	close(start)
	wait.Wait()
	close(results)
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent PublishJSON: %v", err)
	}

	var first PublishedJSON
	for published := range results {
		if first == (PublishedJSON{}) {
			first = published
			continue
		}
		if published != first {
			t.Fatalf("concurrent publications diverged: got %#v, want %#v", published, first)
		}
	}
	if first == (PublishedJSON{}) {
		t.Fatal("no concurrent publication succeeded")
	}
	resolved, body, err := ResolveJSON(ctx, store, testDestination(), testProducer())
	if err != nil {
		t.Fatal(err)
	}
	if resolved != first || !bytes.Equal(body, []byte(testBody)) {
		t.Fatalf("resolved concurrent publication = %#v body=%s", resolved, body)
	}
}

func TestPublishJSONConcurrentConflictsHaveOneWinnerAndNeverOverwrite(t *testing.T) {
	ctx := context.Background()
	store := localStore(t)
	putSchema(t, store)

	start := make(chan struct{})
	type result struct {
		body string
		err  error
	}
	results := make(chan result, 2)
	for _, body := range []string{testBody, `{"value":43}`} {
		go func(body string) {
			<-start
			_, err := PublishJSON(ctx, store, testDestination(), testProducer(), []byte(body))
			results <- result{body: body, err: err}
		}(body)
	}
	close(start)

	var winner string
	for range 2 {
		result := <-results
		if result.err == nil {
			if winner != "" {
				t.Fatalf("two conflicting publications succeeded: %q and %q", winner, result.body)
			}
			winner = result.body
			continue
		}
		if !errors.Is(result.err, ErrManifestConflict) {
			t.Fatalf("conflicting PublishJSON error = %v, want ErrManifestConflict", result.err)
		}
	}
	if winner == "" {
		t.Fatal("no conflicting publication won")
	}
	_, body, err := ResolveJSON(ctx, store, testDestination(), testProducer())
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != winner {
		t.Fatalf("manifest was overwritten: got %s, want winning %s", body, winner)
	}
}

func TestDataArtifactManifestSchemaContract(t *testing.T) {
	valid := []byte(`{"body":{"contentType":"application/json","hash":"sha256:dc60e632a90329ccfd34fbe904d94704dbbb6669575185e26389854ff64139c3","key":"blobs/sha256/dc60e632a90329ccfd34fbe904d94704dbbb6669575185e26389854ff64139c3","size":12},"encoding":"canonical-json-v0","kind":"DataArtifactManifest","producer":{"attempt":1,"nodeId":"task","planHash":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","projectKey":"example/python-e2e","runId":"run-1"},"schema":"sha256:cc6d2156c280bb3efad77622be3c070cf9a18fbf7ddaf4db6a7c6988a417048a","schemaVersion":0}`)
	if err := validateManifestSchema(valid); err != nil {
		t.Fatalf("valid shared manifest schema rejected fixture: %v", err)
	}

	for name, document := range map[string][]byte{
		"additional property": bytes.Replace(valid, []byte(`"schemaVersion":0`), []byte(`"schemaVersion":0,"unexpected":true`), 1),
		"content type":        bytes.Replace(valid, []byte(`"application/json"`), []byte(`"text/plain"`), 1),
		"schema version":      bytes.Replace(valid, []byte(`"schemaVersion":0`), []byte(`"schemaVersion":1`), 1),
		"unsafe project key":  bytes.Replace(valid, []byte(`"example/python-e2e"`), []byte(`"example/../escape"`), 1),
		"unsafe run ID":       bytes.Replace(valid, []byte(`"run-1"`), []byte(`"nested/run"`), 1),
		"unsafe node ID":      bytes.Replace(valid, []byte(`"nodeId":"task"`), []byte(`"nodeId":".."`), 1),
	} {
		t.Run(name, func(t *testing.T) {
			if err := validateManifestSchema(document); err == nil {
				t.Fatalf("shared manifest schema accepted invalid document: %s", document)
			}
		})
	}
}

func TestResolveJSONRejectsTamperedManifestAndBody(t *testing.T) {
	t.Run("manifest", func(t *testing.T) {
		ctx := context.Background()
		store := localStore(t)
		putSchema(t, store)
		if _, err := PublishJSON(ctx, store, testDestination(), testProducer(), []byte(testBody)); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Put(ctx, testDestination().ManifestKey, []byte(`{}`), datastore.PutOptions{ContentType: ManifestContentType}); err != nil {
			t.Fatal(err)
		}
		if _, _, err := ResolveJSON(ctx, store, testDestination(), testProducer()); !errors.Is(err, ErrIntegrity) {
			t.Fatalf("ResolveJSON error = %v, want ErrIntegrity", err)
		}
	})

	t.Run("body", func(t *testing.T) {
		ctx := context.Background()
		store := localStore(t)
		putSchema(t, store)
		published, err := PublishJSON(ctx, store, testDestination(), testProducer(), []byte(testBody))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.Put(ctx, datastore.MustKey(published.Body.Key), []byte(`{"value":0}`), datastore.PutOptions{ContentType: JSONContentType}); err != nil {
			t.Fatal(err)
		}
		if _, _, err := ResolveJSON(ctx, store, testDestination(), testProducer()); !errors.Is(err, ErrIntegrity) {
			t.Fatalf("ResolveJSON error = %v, want ErrIntegrity", err)
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

func assertOnlySchemaBlob(t *testing.T, store datastore.Datastore) {
	t.Helper()
	blobs, err := store.List(context.Background(), datastore.MustKey("blobs"))
	if err != nil {
		t.Fatal(err)
	}
	if len(blobs) != 1 || blobs[0].Key.String() != "blobs/sha256/cc6d2156c280bb3efad77622be3c070cf9a18fbf7ddaf4db6a7c6988a417048a" {
		t.Fatalf("unexpected content bodies: %#v", blobs)
	}
}
