package schema_test

import (
	"bytes"
	"os/exec"
	"strings"
	"testing"
)

func TestCapnpSchemasCompile(t *testing.T) {
	runCapnp(t, nil, "compile", "-o-", "workflow-plan.capnp", "bundle-manifest.capnp")
}

func TestWorkflowPlanCapnpRoundTrip(t *testing.T) {
	text := []byte(`(
  schemaVersion = 0,
  planHash = (algorithm = "sha256", digestHex = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"),
  specHash = (algorithm = "sha256", digestHex = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"),
  graph = (
    workflowName = "linear-chain",
    inputSchema = (algorithm = "sha256", digestHex = "1111111111111111111111111111111111111111111111111111111111111111"),
    outputSchema = (algorithm = "sha256", digestHex = "2222222222222222222222222222222222222222222222222222222222222222"),
    startNode = "__start",
    endNode = "__end",
    nodes = [
      (id = "__start", start = void),
      (id = "double", step = (
        inputSchema = (algorithm = "sha256", digestHex = "1111111111111111111111111111111111111111111111111111111111111111"),
        outputSchema = (algorithm = "sha256", digestHex = "1111111111111111111111111111111111111111111111111111111111111111"),
        symbolRef = "linear-chain/double",
        contractRef = (algorithm = "sha256", digestHex = "8888888888888888888888888888888888888888888888888888888888888888"),
        mergeInputs = []
      )),
      (id = "__end", end = void)
    ],
    edges = [
      (from = "__start", to = "double"),
      (from = "double", to = "__end")
    ]
  ),
  schemas = [
    (
      hash = (algorithm = "sha256", digestHex = "1111111111111111111111111111111111111111111111111111111111111111"),
      canonicalJson = "{\"type\":\"number\"}"
    )
  ],
  symbols = [
    (
      symbolRef = "linear-chain/double",
      packageId = "ts-main",
      language = typescript,
      module = "./workflow.ts",
      export = "double"
    )
  ],
  sourcePackages = [],
  environments = [
    (
      envRef = (algorithm = "sha256", digestHex = "7777777777777777777777777777777777777777777777777777777777777777"),
      specHash = (algorithm = "sha256", digestHex = "7777777777777777777777777777777777777777777777777777777777777777"),
      skipped = void
    )
  ],
  contracts = [
    (
      contractRef = (algorithm = "sha256", digestHex = "8888888888888888888888888888888888888888888888888888888888888888"),
      environmentRef = (algorithm = "sha256", digestHex = "7777777777777777777777777777777777777777777777777777777777777777"),
      resources = (cpu = "500m", memory = "512Mi"),
      secrets = [],
      network = (egress = none)
    )
  ],
  targets = [],
  datastoreManifests = [],
  provenance = (
    compilerName = "massive-go",
    compilerVersion = "0.0.0",
    sourceSpecHash = (algorithm = "sha256", digestHex = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
  )
)`)

	binary := runCapnp(t, text, "convert", "text:binary", "workflow-plan.capnp", "WorkflowPlan")
	decoded := runCapnp(t, binary, "convert", "binary:text", "workflow-plan.capnp", "WorkflowPlan")
	decodedText := string(decoded)

	for _, expected := range []string{
		`workflowName = "linear-chain"`,
		`symbolRef = "linear-chain/double"`,
		`module = "./workflow.ts"`,
		`compilerName = "massive-go"`,
	} {
		if !strings.Contains(decodedText, expected) {
			t.Fatalf("decoded WorkflowPlan missing %q:\n%s", expected, decodedText)
		}
	}
}

func TestWorkflowPlanDiamondContainerCapnpRoundTrip(t *testing.T) {
	text := []byte(`(
  schemaVersion = 0,
  planHash = (algorithm = "sha256", digestHex = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"),
  specHash = (algorithm = "sha256", digestHex = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"),
  graph = (
    workflowName = "diamond",
    inputSchema = (algorithm = "sha256", digestHex = "1111111111111111111111111111111111111111111111111111111111111111"),
    outputSchema = (algorithm = "sha256", digestHex = "1111111111111111111111111111111111111111111111111111111111111111"),
    startNode = "__start",
    endNode = "__end",
    nodes = [
      (id = "__start", start = void),
      (id = "split", step = (
        inputSchema = (algorithm = "sha256", digestHex = "1111111111111111111111111111111111111111111111111111111111111111"),
        outputSchema = (algorithm = "sha256", digestHex = "1111111111111111111111111111111111111111111111111111111111111111"),
        symbolRef = "diamond/split",
        contractRef = (algorithm = "sha256", digestHex = "8888888888888888888888888888888888888888888888888888888888888888"),
        mergeInputs = []
      )),
      (id = "left", step = (
        inputSchema = (algorithm = "sha256", digestHex = "1111111111111111111111111111111111111111111111111111111111111111"),
        outputSchema = (algorithm = "sha256", digestHex = "1111111111111111111111111111111111111111111111111111111111111111"),
        symbolRef = "diamond/left",
        contractRef = (algorithm = "sha256", digestHex = "8888888888888888888888888888888888888888888888888888888888888888"),
        mergeInputs = []
      )),
      (id = "right", step = (
        inputSchema = (algorithm = "sha256", digestHex = "1111111111111111111111111111111111111111111111111111111111111111"),
        outputSchema = (algorithm = "sha256", digestHex = "1111111111111111111111111111111111111111111111111111111111111111"),
        symbolRef = "diamond/right",
        contractRef = (algorithm = "sha256", digestHex = "8888888888888888888888888888888888888888888888888888888888888888"),
        mergeInputs = []
      )),
      (id = "merge", step = (
        inputSchema = (algorithm = "sha256", digestHex = "3333333333333333333333333333333333333333333333333333333333333333"),
        outputSchema = (algorithm = "sha256", digestHex = "1111111111111111111111111111111111111111111111111111111111111111"),
        symbolRef = "diamond/merge",
        contractRef = (algorithm = "sha256", digestHex = "8888888888888888888888888888888888888888888888888888888888888888"),
        mergeInputs = ["left", "right"]
      )),
      (id = "__end", end = void)
    ],
    edges = [
      (from = "__start", to = "split"),
      (from = "split", to = "left"),
      (from = "split", to = "right"),
      (from = "left", to = "merge"),
      (from = "right", to = "merge"),
      (from = "merge", to = "__end")
    ]
  ),
  schemas = [
    (
      hash = (algorithm = "sha256", digestHex = "3333333333333333333333333333333333333333333333333333333333333333"),
      canonicalJson = "{\"items\":{\"type\":\"number\"},\"type\":\"array\"}"
    )
  ],
  symbols = [],
  sourcePackages = [],
  environments = [
    (
      envRef = (algorithm = "sha256", digestHex = "7777777777777777777777777777777777777777777777777777777777777777"),
      specHash = (algorithm = "sha256", digestHex = "7777777777777777777777777777777777777777777777777777777777777777"),
      container = (
        image = "ghcr.io/massive-dev/typescript-runner:v0",
        sourceFetch = (
          key = "packages/sha256-dddddd/source.tar.zst",
          hash = (algorithm = "sha256", digestHex = "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"),
          contentType = "application/zstd"
        )
      )
    )
  ],
  contracts = [],
  targets = [],
  datastoreManifests = [],
  provenance = (
    compilerName = "massive-go",
    compilerVersion = "0.0.0",
    sourceSpecHash = (algorithm = "sha256", digestHex = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc")
  )
)`)

	binary := runCapnp(t, text, "convert", "text:binary", "workflow-plan.capnp", "WorkflowPlan")
	decoded := runCapnp(t, binary, "convert", "binary:text", "workflow-plan.capnp", "WorkflowPlan")
	decodedText := string(decoded)

	for _, expected := range []string{
		`workflowName = "diamond"`,
		`mergeInputs = ["left", "right"]`,
		`container = (`,
		`image = "ghcr.io/massive-dev/typescript-runner:v0"`,
		`canonicalJson = "{\"items\":{\"type\":\"number\"},\"type\":\"array\"}"`,
	} {
		if !strings.Contains(decodedText, expected) {
			t.Fatalf("decoded diamond WorkflowPlan missing %q:\n%s", expected, decodedText)
		}
	}
}

func TestTargetBundleManifestCapnpRoundTrip(t *testing.T) {
	text := []byte(`(
  schemaVersion = 0,
  target = argo,
  planHash = (algorithm = "sha256", digestHex = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"),
  bundleHash = (algorithm = "sha256", digestHex = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"),
  files = [
    (
      path = "workflow-template.yaml",
      artifact = (
        key = "targets/sha256-aaaaaaaa/argo/workflow-template.yaml",
        hash = (algorithm = "sha256", digestHex = "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"),
        contentType = "application/yaml"
      ),
      role = workflowTemplate
    )
  ],
  validations = [
    (name = "dag-integrity", passed = true, diagnostic = "")
  ],
  provenance = (
    compilerName = "massive-go",
    compilerVersion = "0.0.0"
  )
)`)

	binary := runCapnp(t, text, "convert", "text:binary", "bundle-manifest.capnp", "TargetBundleManifest")
	decoded := runCapnp(t, binary, "convert", "binary:text", "bundle-manifest.capnp", "TargetBundleManifest")
	decodedText := string(decoded)

	for _, expected := range []string{
		`target = argo`,
		`path = "workflow-template.yaml"`,
		`name = "dag-integrity"`,
		`compilerName = "massive-go"`,
	} {
		if !strings.Contains(decodedText, expected) {
			t.Fatalf("decoded TargetBundleManifest missing %q:\n%s", expected, decodedText)
		}
	}
}

func runCapnp(t *testing.T, stdin []byte, args ...string) []byte {
	t.Helper()

	cmd := exec.Command("capnp", args...)
	cmd.Dir = "."
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}

	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("capnp %s failed: %v\n%s", strings.Join(args, " "), err, output)
	}
	return output
}
