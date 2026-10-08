package controlplane

import (
	"context"
	"fmt"

	"github.com/Sly1029/massive/conformance/schema/materializationpb"
	"github.com/Sly1029/massive/internal/canonical"
	"github.com/Sly1029/massive/internal/datastore"
	"github.com/Sly1029/massive/internal/environment"
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

// InspectEnvironment reads the realized environment a run recorded, verifying
// the stored bytes and both identities before returning them.
func InspectEnvironment(ctx context.Context, storeRoot, project, runID string) (*runjournal.Manifest, *materializationpb.RealizedEnvironment, error) {
	journal, err := Inspect(ctx, storeRoot, project, runID)
	if err != nil {
		return nil, nil, err
	}
	if journal.Environment == nil {
		return nil, nil, fmt.Errorf("run %q recorded no dependency environment; only Python runs through massive run check one", runID)
	}
	root, err := resolveStore(storeRoot)
	if err != nil {
		return nil, nil, err
	}
	store, err := datastore.NewLocalDatastore(datastore.LocalConfig{Root: root})
	if err != nil {
		return nil, nil, err
	}
	reference := journal.Environment.Record
	key, err := datastore.ParseKey(reference.Key)
	if err != nil {
		return nil, nil, fmt.Errorf("invalid realized environment key: %w", err)
	}
	object, err := store.Get(ctx, key)
	if err != nil {
		return nil, nil, fmt.Errorf("read realized environment record: %w", err)
	}
	if canonical.DigestBytes(object.Body) != reference.Hash || len(object.Body) != reference.Size || reference.ContentType != environment.RecordContentType {
		return nil, nil, fmt.Errorf("realized environment record does not match the run journal reference")
	}
	record, err := environment.ParseRecord(object.Body)
	if err != nil {
		return nil, nil, err
	}
	if record.GetRequirementHash() != journal.Environment.RequirementHash || record.GetRealizationHash() != journal.Environment.RealizationHash {
		return nil, nil, fmt.Errorf("realized environment record identities differ from the run journal")
	}
	return journal, record, nil
}
