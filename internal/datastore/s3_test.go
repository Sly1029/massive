package datastore

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/Sly1029/massive/internal/datastore/miniotest"
)

func TestS3DatastoreContract(t *testing.T) {
	endpoint := miniotest.Start(t)
	t.Setenv("AWS_ACCESS_KEY_ID", miniotest.AccessKey)
	t.Setenv("AWS_SECRET_ACCESS_KEY", miniotest.SecretKey)
	t.Setenv("AWS_SESSION_TOKEN", "")

	RunDatastoreContract(t, func(t *testing.T) Datastore {
		t.Helper()

		store, err := NewS3Datastore(context.Background(), S3Config{
			Endpoint:     endpoint,
			Bucket:       "massive-datastore-contract",
			Region:       "us-east-1",
			Prefix:       strings.ToLower(t.Name()),
			CreateBucket: true,
		})
		if err != nil {
			t.Fatalf("new s3 datastore: %v", err)
		}
		return store
	})
}

func TestMain(m *testing.M) {
	os.Exit(m.Run())
}

func TestS3RequiresExplicitApplicationCredentials(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY", "obsolete-alias")
	t.Setenv("AWS_SECRET_KEY", "obsolete-alias")
	for _, key := range []string{"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN", "AWS_WEB_IDENTITY_TOKEN_FILE", "AWS_CONTAINER_CREDENTIALS_RELATIVE_URI", "AWS_CONTAINER_CREDENTIALS_FULL_URI"} {
		t.Setenv(key, "")
	}
	_, err := NewS3Datastore(context.Background(), S3Config{Endpoint: "localhost:9000", Bucket: "test", Region: "us-east-1"})
	if err == nil || !strings.Contains(err.Error(), "bind AWS access credentials") {
		t.Fatalf("missing credentials: %v", err)
	}
}

// A read the credentials may not perform is classified, so callers can treat a
// denied archive like a missing one instead of retrying it.
func TestS3DeniedReadsAreAccessDenied(t *testing.T) {
	endpoint := miniotest.Start(t)
	t.Setenv("AWS_ACCESS_KEY_ID", miniotest.AccessKey)
	t.Setenv("AWS_SECRET_ACCESS_KEY", miniotest.SecretKey)
	t.Setenv("AWS_SESSION_TOKEN", "")
	config := S3Config{Endpoint: endpoint, Bucket: "massive-denied", Region: "us-east-1", CreateBucket: true}
	store, err := NewS3Datastore(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	key := MustKey("packages/archive.tar")
	if _, err := store.Put(context.Background(), key, []byte("archive"), PutOptions{ContentType: "application/octet-stream"}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AWS_SECRET_ACCESS_KEY", "not-the-secret")
	config.CreateBucket = false
	denied, err := NewS3Datastore(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	// Get and Open classify the same response and keep the S3 error code in
	// their diagnostics; a wrong secret is refused as SignatureDoesNotMatch.
	if _, err := denied.Get(context.Background(), key); !errors.Is(err, ErrAccessDenied) || !strings.Contains(err.Error(), "SignatureDoesNotMatch") {
		t.Fatalf("denied read error = %v", err)
	}
	if _, _, err := denied.Open(context.Background(), key); !errors.Is(err, ErrAccessDenied) || !strings.Contains(err.Error(), "SignatureDoesNotMatch") {
		t.Fatalf("denied open error = %v", err)
	}
	if _, err := store.Get(context.Background(), MustKey("packages/missing.tar")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing object error = %v", err)
	}
}
