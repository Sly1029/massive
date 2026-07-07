package datastore

import (
	"bytes"
	"context"
	"errors"
	"sort"
	"testing"
)

func RunDatastoreContract(t *testing.T, factory func(t *testing.T) Datastore) {
	t.Helper()

	t.Run("put get round trip", func(t *testing.T) {
		store := factory(t)
		key := MustKey("objects/round-trip.txt")

		info, err := store.Put(context.Background(), key, []byte("hello"), PutOptions{ContentType: "text/plain"})
		if err != nil {
			t.Fatalf("put: %v", err)
		}
		if info.Key != key || info.Size != 5 || info.ContentType != "text/plain" {
			t.Fatalf("unexpected put info: %#v", info)
		}

		object, err := store.Get(context.Background(), key)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if !bytes.Equal(object.Body, []byte("hello")) {
			t.Fatalf("body = %q, want hello", object.Body)
		}
		if object.Info.ContentType != "text/plain" {
			t.Fatalf("content type = %q, want text/plain", object.Info.ContentType)
		}
	})

	t.Run("exists", func(t *testing.T) {
		store := factory(t)
		key := MustKey("objects/exists.txt")

		exists, err := store.Exists(context.Background(), key)
		if err != nil {
			t.Fatalf("exists missing: %v", err)
		}
		if exists {
			t.Fatal("missing key exists")
		}

		if _, err := store.Put(context.Background(), key, []byte("present"), PutOptions{}); err != nil {
			t.Fatalf("put: %v", err)
		}

		exists, err = store.Exists(context.Background(), key)
		if err != nil {
			t.Fatalf("exists present: %v", err)
		}
		if !exists {
			t.Fatal("present key does not exist")
		}
	})

	t.Run("conditional write conflict", func(t *testing.T) {
		store := factory(t)
		key := MustKey("objects/conditional.txt")

		if _, err := store.Put(context.Background(), key, []byte("first"), PutOptions{IfAbsent: true}); err != nil {
			t.Fatalf("first put: %v", err)
		}
		if _, err := store.Put(context.Background(), key, []byte("second"), PutOptions{IfAbsent: true}); !errors.Is(err, ErrAlreadyExists) {
			t.Fatalf("second put error = %v, want ErrAlreadyExists", err)
		}

		object, err := store.Get(context.Background(), key)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if string(object.Body) != "first" {
			t.Fatalf("conditional write replaced object: %q", object.Body)
		}
	})

	t.Run("listing under prefix", func(t *testing.T) {
		store := factory(t)
		for key, body := range map[string]string{
			"prefix/a.json":        "a",
			"prefix/nested/b.json": "b",
			"outside/c.json":       "c",
		} {
			if _, err := store.Put(context.Background(), MustKey(key), []byte(body), PutOptions{ContentType: "application/json"}); err != nil {
				t.Fatalf("put %s: %v", key, err)
			}
		}

		objects, err := store.List(context.Background(), MustKey("prefix"))
		if err != nil {
			t.Fatalf("list: %v", err)
		}

		got := make([]string, 0, len(objects))
		for _, object := range objects {
			got = append(got, object.Key.String())
			if object.ContentType != "application/json" {
				t.Fatalf("%s content type = %q, want application/json", object.Key, object.ContentType)
			}
		}
		sort.Strings(got)
		want := []string{"prefix/a.json", "prefix/nested/b.json"}
		if !equalStrings(got, want) {
			t.Fatalf("listed keys = %v, want %v", got, want)
		}
	})

	t.Run("key validation", func(t *testing.T) {
		invalid := []string{
			"",
			"/leading",
			`objects\backslash`,
			"objects//empty",
			"objects/./dot",
			"objects/../escape",
			"..",
			"C:/absolute",
		}
		for _, value := range invalid {
			if _, err := ParseKey(value); !errors.Is(err, ErrInvalidKey) {
				t.Fatalf("ParseKey(%q) error = %v, want ErrInvalidKey", value, err)
			}
		}
	})

	t.Run("content addressed blob helpers", func(t *testing.T) {
		store := factory(t)
		body := []byte("test")
		key := BlobKeyForBytes(body)
		if key.String() != "blobs/sha256/9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08" {
			t.Fatalf("blob key = %s", key)
		}

		if _, err := store.Put(context.Background(), key, body, PutOptions{IfAbsent: true, ContentType: "application/octet-stream"}); err != nil {
			t.Fatalf("put blob: %v", err)
		}
		object, err := store.Get(context.Background(), key)
		if err != nil {
			t.Fatalf("get blob: %v", err)
		}
		if !bytes.Equal(object.Body, body) {
			t.Fatalf("blob body = %q, want %q", object.Body, body)
		}
	})
}

func equalStrings(left []string, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
