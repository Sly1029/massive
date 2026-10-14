package controlplane

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/Sly1029/massive/internal/datastore"
	"github.com/Sly1029/massive/internal/environment"
	"github.com/Sly1029/massive/internal/orchestrator"
)

func runCoordinatorLocal(ctx context.Context, request LocalRunRequest) (*LocalRunResult, error) {
	root, err := resolveStore(request.Store)
	if err != nil {
		return nil, err
	}
	project := request.Project
	if project == "" && request.Frontend != nil {
		project, err = projectFromGitOrigin(request.Frontend.PackageRoot)
		if err != nil {
			return nil, err
		}
	}
	if project == "" {
		return nil, fmt.Errorf("resume requires --project")
	}
	envRequest, err := PythonEnvironment()
	if err != nil {
		return nil, err
	}
	config := orchestrator.CoordinatorRunConfig{StoreRoot: root, ProjectID: project, RunID: request.RunID, Resume: request.Resume, WorkersQuiescent: request.WorkersQuiescent, ProcessLimit: request.Workers, Input: request.Input, Hooks: request.Hooks, EnvironmentRequest: envRequest}
	store, err := datastore.NewLocalDatastore(datastore.LocalConfig{Root: root})
	if err != nil {
		return nil, err
	}
	if frontend := request.Frontend; frontend != nil {
		if frontend.Coordinator == nil {
			return nil, fmt.Errorf("resume capability: provide a coordinator entry")
		}
		config.Definition = frontend.Coordinator
		config.SourceRoot = frontend.PackageRoot
		if report := frontend.Environment; report != nil && report.Record != nil {
			config.Interpreter = report.Interpreter.Executable
			config.Environment, err = environment.Store(ctx, store, report.Record)
			if err != nil {
				return nil, err
			}
		}
	}
	run, runErr := orchestrator.RunCoordinator(ctx, config)
	result := &LocalRunResult{Run: run, Store: root}
	if run == nil {
		return result, runErr
	}
	if runErr == nil {
		readContext, stop := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer stop()
		object, err := store.Get(readContext, datastore.MustKey(run.ResultKey))
		if err != nil {
			return result, err
		}
		result.Result = append(json.RawMessage(nil), object.Body...)
	}
	return result, runErr
}
