package argo

import (
	"fmt"
	"regexp"
	"strings"
)

// The vendored CRD schema carries no name patterns, so offline structure
// validation cannot catch an invalid name; without this gate a bad name only
// surfaces at apply time on the cluster.

// workflowFieldName is Argo's accepted shape for template and DAG task names
// (workflowFieldNameFmt in argoproj/argo-workflows).
var workflowFieldName = regexp.MustCompile(`^[a-zA-Z0-9][-a-zA-Z0-9]*$`)

// rfc1123Subdomain is the Kubernetes metadata.name shape.
var rfc1123Subdomain = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`)

const (
	maxMetadataNameLength = 253
	// maxWorkflowFieldLength is Argo's limit on template and DAG task names
	// (workflowFieldMaxLength in argoproj/argo-workflows). The vendored CRD schema
	// carries no length constraint, so it is enforced here.
	maxWorkflowFieldLength = 128
	// maxStepNodeIDLength keeps the step-<id> template name within the field
	// limit: len("step-") is added to every node id, so a node id may be at most
	// maxWorkflowFieldLength - len(stepTemplatePrefix) characters. This is the
	// stricter bound and also satisfies the task-name (bare node id) limit.
	maxStepNodeIDLength = maxWorkflowFieldLength - len(stepTemplatePrefix)
)

// validateNames gates the workflow template resource name and every step id
// that becomes an Argo template/task name. Spec node ids allow a wider charset
// (`_ . @ / : #`, uppercase) than Argo accepts and no length bound, so this
// fails compile with a diagnostic naming the offender instead of emitting a
// bundle the cluster would reject.
func validateNames(templateName string, index planIndex) error {
	if len(templateName) > maxMetadataNameLength || !rfc1123Subdomain.MatchString(templateName) {
		return fmt.Errorf("workflow template name %q is not a valid Kubernetes resource name (lowercase RFC 1123 subdomain, max %d chars); rename the workflow or set targets[].workflowTemplateName", templateName, maxMetadataNameLength)
	}
	for _, nodeID := range index.stepOrder {
		if !workflowFieldName.MatchString(nodeID) {
			return fmt.Errorf("step id %q cannot be an Argo template/task name; the argo target requires step ids matching %s", nodeID, workflowFieldName.String())
		}
		if strings.HasPrefix(nodeID, wfSystemPrefix) {
			return fmt.Errorf("step id %q uses the reserved %q prefix; it is namespaced for compiler-generated system tasks", nodeID, wfSystemPrefix)
		}
		if len(nodeID) > maxStepNodeIDLength {
			return fmt.Errorf("step id %q is %d characters; the argo target requires step ids of at most %d characters so both the DAG task name and the %q template name stay within Argo's %d-character workflow-field limit", nodeID, len(nodeID), maxStepNodeIDLength, stepTemplatePrefix+"<id>", maxWorkflowFieldLength)
		}
	}
	return nil
}
