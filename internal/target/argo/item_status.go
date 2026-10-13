package argo

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/Sly1029/massive/internal/executionphase"
	"github.com/Sly1029/massive/internal/mapexec"
	corev1 "k8s.io/api/core/v1"
)

type itemSchedulerNode struct {
	Type     string   `json:"type"`
	Phase    string   `json:"phase"`
	Children []string `json:"children"`
}

type itemSchedulerWorkflow struct {
	Metadata struct {
		UID string `json:"uid"`
	} `json:"metadata"`
	Status struct {
		Nodes                    map[string]itemSchedulerNode `json:"nodes"`
		CompressedNodes          string                       `json:"compressedNodes"`
		OffloadNodeStatusVersion string                       `json:"offloadNodeStatusVersion"`
	} `json:"status"`
}

// ReadMissingItemFailures reconciles missing envelopes with real terminal
// scheduler state. Transport/executor errors and lost pods fail the run; only
// an OOM or recorded signal termination of main becomes an item failure.
func ReadMissingItemFailures(ctx context.Context, namespace, workflow string, nodeIDs []string) (map[string]mapexec.Failure, error) {
	if namespace == "" || workflow == "" {
		return nil, fmt.Errorf("missing item outcomes require the Argo workflow and namespace")
	}
	client, endpoint, token, err := schedulerClient()
	if err != nil {
		return nil, err
	}
	defer client.CloseIdleConnections()
	read := func(path string, document any) error {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+path, nil)
		if err != nil {
			return err
		}
		request.Header.Set("Authorization", "Bearer "+token)
		response, err := client.Do(request)
		if err != nil {
			return fmt.Errorf("read terminal scheduler state: %w", err)
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusOK {
			return fmt.Errorf("read terminal scheduler state: HTTP %d", response.StatusCode)
		}
		return json.NewDecoder(response.Body).Decode(document)
	}
	var state itemSchedulerWorkflow
	if err := read("/apis/argoproj.io/v1alpha1/namespaces/"+url.PathEscape(namespace)+"/workflows/"+url.PathEscape(workflow), &state); err != nil {
		return nil, err
	}
	if state.Status.OffloadNodeStatusVersion != "" {
		return nil, fmt.Errorf("missing outcomes require inline Argo node status; offloaded node status is unavailable")
	}
	if state.Status.CompressedNodes != "" {
		compressed, err := base64.StdEncoding.DecodeString(state.Status.CompressedNodes)
		if err != nil {
			return nil, fmt.Errorf("decode Argo node status: %w", err)
		}
		reader, err := gzip.NewReader(bytes.NewReader(compressed))
		if err != nil {
			return nil, err
		}
		defer reader.Close()
		if err := json.NewDecoder(reader).Decode(&state.Status.Nodes); err != nil {
			return nil, err
		}
	}
	var pods corev1.PodList
	selector := url.QueryEscape("workflows.argoproj.io/workflow=" + workflow)
	if err := read("/api/v1/namespaces/"+url.PathEscape(namespace)+"/pods?labelSelector="+selector, &pods); err != nil {
		return nil, err
	}
	failures := make(map[string]mapexec.Failure, len(nodeIDs))
	for _, id := range nodeIDs {
		failure, err := missingItemFailure(state, pods.Items, id)
		if err != nil {
			return nil, err
		}
		failures[id] = failure
	}
	return failures, nil
}

func missingItemFailure(workflow itemSchedulerWorkflow, pods []corev1.Pod, id string) (mapexec.Failure, error) {
	failed := func(reason string) (mapexec.Failure, error) {
		return mapexec.Failure{}, fmt.Errorf("item node %s has no outcome: %s", id, reason)
	}
	node, ok := workflow.Status.Nodes[id]
	if !ok || node.Type != "Pod" || (node.Phase != "Failed" && node.Phase != "Error") {
		return failed("terminal pod node status is unavailable")
	}
	attempts := 1
	for _, parent := range workflow.Status.Nodes {
		if parent.Type != "Retry" || !slices.Contains(parent.Children, id) {
			continue
		}
		if len(parent.Children) == 0 || parent.Children[len(parent.Children)-1] != id || (parent.Phase != "Failed" && parent.Phase != "Error") {
			return failed("retry group is not terminal")
		}
		attempts = len(parent.Children)
		for _, child := range parent.Children {
			if workflow.Status.Nodes[child].Type != "Pod" {
				return failed("retry group contains a non-pod attempt")
			}
		}
		break
	}
	var terminal *corev1.Pod
	for i := range pods {
		pod := &pods[i]
		if pod.Annotations["workflows.argoproj.io/node-id"] != id {
			continue
		}
		if terminal != nil {
			return failed("multiple pods claim the terminal node")
		}
		terminal = pod
	}
	if terminal == nil {
		return failed("terminal pod is unavailable; preserve pods until collection")
	}
	// A pod from another workflow cannot establish evidence for this run.
	ownerMatches := false
	for _, owner := range terminal.OwnerReferences {
		if owner.Kind == "Workflow" && string(owner.UID) == workflow.Metadata.UID {
			ownerMatches = true
		}
	}
	if !ownerMatches || workflow.Metadata.UID == "" {
		return failed("terminal pod belongs to another workflow")
	}
	if terminal.Status.Phase != corev1.PodFailed || terminal.Status.Reason != "" {
		return failed("pod failed without evidence of author process death")
	}
	for _, container := range terminal.Status.ContainerStatuses {
		if container.Name != "main" {
			continue
		}
		termination := container.State.Terminated
		if termination == nil {
			return failed("main container has no terminal status")
		}
		fileEvidence := slices.ContainsFunc(terminal.Spec.Containers, func(spec corev1.Container) bool {
			return spec.Name == "main" && spec.TerminationMessagePolicy == corev1.TerminationMessageReadFile
		})
		if !fileEvidence {
			return failed("main container must use terminationMessagePolicy File")
		}
		marker, err := executionphase.Parse(termination.Message)
		if err != nil {
			return failed(fmt.Sprintf("execution phase evidence is absent or invalid: %v", err))
		}
		if marker.Fatal {
			return failed("runtime reported a fatal infrastructure or contract error")
		}
		if marker.Phase != executionphase.Author {
			return failed(fmt.Sprintf("container died during runtime phase %s, not author execution", marker.Phase))
		}
		if termination.Reason != "OOMKilled" && termination.Signal <= 0 {
			return failed("main container exited without OOM or signal evidence")
		}
		return mapexec.NewFailure(mapexec.FailureKilled, attempts, "runner-killed (signal; Argo author-phase reconciliation)"), nil
	}
	return failed("main container status is unavailable")
}

// schedulerClient owns the API transport for control pods only.
func schedulerClient() (*http.Client, string, string, error) {
	const credentials = "/var/run/secrets/kubernetes.io/serviceaccount/"
	token, err := os.ReadFile(credentials + "token")
	if err != nil {
		return nil, "", "", fmt.Errorf("read scheduler token: %w", err)
	}
	ca, err := os.ReadFile(credentials + "ca.crt")
	if err != nil {
		return nil, "", "", fmt.Errorf("read scheduler CA: %w", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(ca) {
		return nil, "", "", fmt.Errorf("invalid scheduler CA")
	}
	host := os.Getenv("KUBERNETES_SERVICE_HOST")
	port := os.Getenv("KUBERNETES_SERVICE_PORT_HTTPS")
	if host == "" || port == "" {
		return nil, "", "", fmt.Errorf("missing Kubernetes API service address")
	}
	transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}}
	client := &http.Client{Transport: transport, Timeout: 15 * time.Second}
	endpoint := "https://" + net.JoinHostPort(host, port)

	return client, endpoint, strings.TrimSpace(string(token)), nil
}
