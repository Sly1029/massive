package orchestrator

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/Sly1029/massive/internal/datastore"
)

func TestInvocationDatastoreConfigUsesTheRunnerContract(t *testing.T) {
	for _, invalid := range []string{
		`{}`, `{ "kind":"s3", "bucket":"bucket" }`,
		`{ "kind":"s3", "bucket":"bucket", "region":"us-east-1", "accessKey":"forbidden" }`,
		`{ "kind":"s3", "bucket":"bucket", "region":"us-east-1", "prefix":"../escape" }`,
		`{ "kind":"local", "path":"/tmp/store", "bucket":"mixed" }`,
	} {
		if _, err := ParseDatastoreDescriptor([]byte(invalid)); err == nil {
			t.Fatalf("accepted invalid config: %s", invalid)
		}
	}
	config, _ := json.Marshal(LocalDatastoreDescriptor{Kind: "local", Path: filepath.Join(t.TempDir(), "objects")})
	descriptor, err := ParseDatastoreDescriptor(config)
	if err != nil {
		t.Fatal(err)
	}
	writer, err := openInvocationDatastore(context.Background(), descriptor)
	if err != nil {
		t.Fatal(err)
	}
	key := datastore.MustKey("test/value")
	if _, err := writer.Put(context.Background(), key, []byte("shared"), datastore.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	reader, err := openInvocationDatastore(context.Background(), descriptor)
	if err != nil {
		t.Fatal(err)
	}
	object, err := reader.Get(context.Background(), key)
	if err != nil || string(object.Body) != "shared" {
		t.Fatalf("reopened store: %q, %v", object.Body, err)
	}
}

func TestS3InvocationRejectsEndpointsWithCredentialsOrPaths(t *testing.T) {
	for _, endpoint := range []string{"http://user:password@host", "https://host/path", "ftp://host", "https://host?secret=value"} {
		_, err := openInvocationDatastore(context.Background(), S3DatastoreDescriptor{Kind: "s3", Bucket: "bucket", Region: "us-east-1", Endpoint: endpoint})
		if err == nil {
			t.Fatalf("accepted invalid S3 endpoint: %s", endpoint)
		}
	}
}
