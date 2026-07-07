package datastore

import "testing"

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
