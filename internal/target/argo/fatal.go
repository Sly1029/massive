package argo

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/Sly1029/massive/internal/datastore"
	"github.com/Sly1029/massive/internal/orchestrator"
)

// WatchFatalRun runs as a control-only daemon. Stop cancels non-exit pods and
// prevents dispatch, while preserving onExit in Argo v3.7.16. failFast alone
// would wait for all running loop children; Terminate would skip the exit hook.
func WatchFatalRun(ctx context.Context, store datastore.Datastore, key datastore.Key, namespace, workflow, uid, readyFile string) error {
	client, endpoint, token, err := schedulerClient()
	if err != nil {
		return err
	}
	defer client.CloseIdleConnections()
	body, err := json.Marshal(map[string]any{"metadata": map[string]string{"uid": uid}, "spec": map[string]string{"shutdown": "Stop"}})
	if err != nil {
		return err
	}
	path := endpoint + "/apis/argoproj.io/v1alpha1/namespaces/" + url.PathEscape(namespace) + "/workflows/" + url.PathEscape(workflow)
	stop := func(dryRun bool) error {
		target := path
		if dryRun {
			target += "?dryRun=All"
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodPatch, target, bytes.NewReader(body))
		if err != nil {
			return err
		}
		request.Header.Set("Authorization", "Bearer "+token)
		request.Header.Set("Content-Type", "application/merge-patch+json")
		response, err := client.Do(request)
		if err != nil {
			return fmt.Errorf("stop fatal workflow: %w", err)
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusOK {
			return fmt.Errorf("stop fatal workflow: HTTP %d", response.StatusCode)
		}
		return nil
	}
	// Refuse dispatch if this deployment cannot enforce a fatal stop.
	if err := stop(true); err != nil {
		return err
	}
	if _, err := orchestrator.ReadFatalRun(ctx, store, key); err != nil {
		return err
	}
	if err := os.WriteFile(readyFile, []byte("ready"), 0600); err != nil {
		return err
	}
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		failure, readErr := orchestrator.ReadFatalRun(ctx, store, key)
		if failure != nil || readErr != nil {
			// Stop also on lost datastore access: the daemon can no longer enforce
			// a policy abort. Retry API errors while invocations check the marker. The run context bounds each request and the daemon.
			if err := stop(false); err == nil {
				<-ctx.Done()
				return ctx.Err()
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
