package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/Sly1029/massive/internal/canonical"
	"github.com/Sly1029/massive/internal/datastore"
	"github.com/Sly1029/massive/internal/runjournal"
)

// FatalRunKey is run-scoped; a remote target publishes the first safe fatal cause
// before signalling its scheduler. No author logs or exception chains enter it.
func FatalRunKey(project, runID string) (datastore.Key, error) {
	if !ValidSafePathSegment(runID) {
		return datastore.Key{}, fmt.Errorf("invalid fatal run id")
	}
	return datastore.ParseKey("projects/" + NormalizeProjectKey(project) + "/runs/" + runID + "/fatal.json")
}

func PublishFatalRun(ctx context.Context, store datastore.Datastore, key datastore.Key, failure runjournal.FatalFailure) error {
	body, err := canonical.Marshal(failure)
	if err != nil {
		return err
	}
	if _, err := runjournal.ParseFatalFailure(body); err != nil {
		return fmt.Errorf("invalid fatal cause: %w", err)
	}
	_, err = store.Put(ctx, key, body, datastore.PutOptions{ContentType: jsonContentType, IfAbsent: true})
	if errors.Is(err, datastore.ErrAlreadyExists) {
		return nil
	}
	return err
}

// ReadFatalRun bounds and schema-validates the cause independently of the
// process that published it. Absence means the run has no fatal signal yet.
func ReadFatalRun(ctx context.Context, store datastore.Datastore, key datastore.Key) (*runjournal.FatalFailure, error) {
	reader, info, err := store.Open(ctx, key)
	if errors.Is(err, datastore.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	const limit = 12*1024 + 1024
	if info.Size > limit {
		return nil, fmt.Errorf("fatal cause exceeds size limit")
	}
	body, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, err
	}
	if len(body) > limit {
		return nil, fmt.Errorf("fatal cause exceeds size limit")
	}
	return runjournal.ParseFatalFailure(body)
}
