package controlplane

import (
	"context"
	"fmt"

	"github.com/Sly1029/massive/internal/datastore"
	"github.com/Sly1029/massive/internal/orchestrator"
	"github.com/Sly1029/massive/internal/runjournal"
)

// Inspect reads one project's durable journal without executing author code.
// Project is explicit so identical run IDs never require scanning other projects.
func Inspect(ctx context.Context, storeRoot, project, runID string) (*runjournal.Manifest, error) {
	if project == "" {
		return nil, fmt.Errorf("project is required; provide --project")
	}
	if !orchestrator.ValidSafePathSegment(runID) {
		return nil, fmt.Errorf("invalid run ID; use one safe path segment")
	}
	root, err := resolveStore(storeRoot)
	if err != nil {
		return nil, err
	}
	store, err := datastore.NewLocalDatastore(datastore.LocalConfig{Root: root})
	if err != nil {
		return nil, err
	}
	projectKey := orchestrator.NormalizeProjectKey(project)
	key := datastore.MustKey("projects/" + projectKey + "/runs/" + runID + "/run-manifest.json")
	object, err := store.Get(ctx, key)
	if err != nil {
		return nil, fmt.Errorf("read run %q; check --project, --store and run ID: %w", runID, err)
	}
	journal, err := runjournal.Parse(object.Body)
	if err != nil {
		return nil, err
	}
	if journal.ProjectKey != projectKey || journal.RunID != runID {
		return nil, fmt.Errorf("run journal identity does not match its location; restore the original journal")
	}
	return journal, nil
}
