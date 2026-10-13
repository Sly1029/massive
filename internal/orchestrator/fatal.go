package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"time"

	"github.com/Sly1029/massive/internal/canonical"
	"github.com/Sly1029/massive/internal/datastore"
	"github.com/Sly1029/massive/internal/runjournal"
)

// FatalRunKey is run-scoped; a remote target publishes the first safe fatal cause
// before stopping sibling invocations. No author logs or exception chains enter it.
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

// WithFatalRunCancellation cooperatively stops an invocation when another
// invocation publishes the run's safe cause. Read failures back off and retry;
// they never establish a fatal run. The returned stop waits for polling to end.
func WithFatalRunCancellation(parent context.Context, store datastore.Datastore, key datastore.Key) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancelCause(parent)
	done := make(chan struct{})
	go func() {
		defer close(done)
		backoff := time.Second
		for {
			timer := time.NewTimer(time.Second + time.Duration(rand.Int64N(int64(backoff))))
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
			readCtx, stop := context.WithTimeout(ctx, 5*time.Second)
			fatal, err := ReadFatalRun(readCtx, store, key)
			stop()
			if err != nil {
				backoff = min(4*time.Second, 2*backoff)
				continue
			}
			backoff = time.Second
			if fatal != nil {
				cancel(&FatalRunError{Failure: *fatal})
				return
			}
		}
	}()
	return ctx, func() {
		cancel(context.Canceled)
		<-done
	}
}
