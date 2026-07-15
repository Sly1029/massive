package argo

import (
	"fmt"
	"regexp"
)

// The vendored CRD schema carries no name patterns, so offline structure
// validation cannot catch an invalid name; without this gate a bad name only
// surfaces at apply time on the cluster.

// workflowFieldName is Argo's accepted shape for template and DAG task names
// (workflowFieldNameFmt in argoproj/argo-workflows).
var workflowFieldName = regexp.MustCompile(`^[a-zA-Z0-9][-a-zA-Z0-9]*$`)

// rfc1123Subdomain is the Kubernetes metadata.name shape.
var rfc1123Subdomain = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`)

const maxMetadataNameLength = 253

// validateNames gates the workflow template resource name and every step id
// that becomes an Argo template/task name. Spec node ids allow a wider charset
// (`_ . @ / : #`, uppercase) than Argo accepts, so this fails compile with a
// diagnostic naming the offender instead of emitting a bundle the cluster
// would reject.
func validateNames(templateName string, index planIndex) error {
	if len(templateName) > maxMetadataNameLength || !rfc1123Subdomain.MatchString(templateName) {
		return fmt.Errorf("workflow template name %q is not a valid Kubernetes resource name (lowercase RFC 1123 subdomain, max %d chars); rename the workflow or set targets[].workflowTemplateName", templateName, maxMetadataNameLength)
	}
	for _, nodeID := range index.stepOrder {
		if !workflowFieldName.MatchString(nodeID) {
			return fmt.Errorf("step id %q cannot be an Argo template/task name; the argo target requires step ids matching %s", nodeID, workflowFieldName.String())
		}
	}
	return nil
}
