# Vendored Argo Workflows JSON Schema

Status: v0 conformance contract

`argo-workflows-v3.7.16.schema.json` is the generated JSON Schema for Argo
Workflows CRDs, vendored so the Go Argo backend (WS-7) can validate generated
`WorkflowTemplate` manifests **offline** — no cluster and no network at compile
or test time.

## Pinned version

- **Version:** `v3.7.16` (latest stable Argo Workflows v3.x at time of vendoring)
- **Upstream source:**
  `https://raw.githubusercontent.com/argoproj/argo-workflows/v3.7.16/api/jsonschema/schema.json`
- **Vendored verbatim.** The file is the upstream artifact unchanged; do not
  hand-edit it.

The version is also recorded in `schema.go` as `ArgoWorkflowsCRDVersion` and in
[`../../docs/spec/open-questions.md`](../../docs/spec/open-questions.md) (Argo
compiler CRD-version decision).

## Shape

- JSON Schema draft 2020-12.
- Self-contained: every `$ref` is an internal `#/definitions/...` pointer, so the
  document compiles without any network resolution.
- The Argo backend validates the generated template against the definition
  `io.argoproj.workflow.v1alpha1.WorkflowTemplate`.

## Updating

1. Download the `api/jsonschema/schema.json` for the new pinned tag from the
   upstream repo.
2. Replace `argo-workflows-v<version>.schema.json` and update the `//go:embed`
   directive plus `ArgoWorkflowsCRDVersion` in `schema.go`.
3. Update the version above and in `open-questions.md`.
4. Re-run `go test ./internal/target/...` — the offline structure-validation
   tests are the regression gate.
