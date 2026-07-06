# WorkflowPlan JSON Projection

Status: v0 conformance aid

`WorkflowPlan` is canonically written by the Go compiler as Cap'n Proto using [`workflow-plan.capnp`](workflow-plan.capnp). Tests and reviews also need a human-diffable projection. This document defines that projection so fixture diffs are stable without making JSON the compiled plan artifact.

The projection is a deterministic JSON rendering of the logical Cap'n Proto field tree:

- object keys are sorted lexicographically at every level,
- list order is the plan's semantic order,
- unions render as objects with `kind` plus the selected value fields,
- `HashRef` renders as `sha256:<hex>`,
- `ArtifactRef` renders as `{ "key", "hash", "contentType" }`,
- `SchemaEntry.canonicalJson` is sorted-key canonical JSON for the lowered portable schema value,
- wall-clock fields, including compile time and bundle emission time, are not part of the canonical Cap'n Proto artifacts and must not appear in the projection,
- absent/default Cap'n Proto scalar values are omitted only if the projection spec for that field says so; v0 fixtures should prefer explicit values,
- projected plans must not contain dangling references: every step node's `contractRef` resolves to an entry in `contracts`, and every contract's `environmentRef` resolves to an entry in `environments`.

Shape:

```json
{
  "schemaVersion": 0,
  "planHash": "sha256:<hex>",
  "specHash": "sha256:<hex>",
  "graph": {
    "workflowName": "linear-chain",
    "inputSchema": "sha256:<hex>",
    "outputSchema": "sha256:<hex>",
    "startNode": "__start",
    "endNode": "__end",
    "nodes": [
      { "id": "__start", "kind": "start" },
      {
        "id": "double",
        "kind": "step",
        "inputSchema": "sha256:<hex>",
        "outputSchema": "sha256:<hex>",
        "symbolRef": "linear-chain/double",
        "contractRef": "sha256:<hex>",
        "mergeInputs": []
      }
    ],
    "edges": [
      { "from": "__start", "to": "double" }
    ]
  },
  "schemas": [
    { "hash": "sha256:<hex>", "canonicalJson": "{\"type\":\"number\"}" }
  ],
  "symbols": [
    {
      "symbolRef": "linear-chain/double",
      "packageId": "ts-main",
      "language": "typescript",
      "module": "./workflow.ts",
      "export": "double"
    }
  ],
  "sourcePackages": [
    {
      "language": "typescript",
      "manifest": { "contentType": "application/vnd.capnp", "hash": "sha256:<hex>", "key": "packages/sha256-<hex>/source-manifest.capnp" },
      "packageHash": "sha256:<hex>",
      "packageId": "ts-main",
      "sourceArchive": { "contentType": "application/zstd", "hash": "sha256:<hex>", "key": "packages/sha256-<hex>/source.tar.zst" }
    }
  ],
  "environments": [
    {
      "envRef": "sha256:<hex>",
      "kind": "skipped",
      "specHash": "sha256:<hex>"
    }
  ],
  "contracts": [
    {
      "contractRef": "sha256:<hex>",
      "environmentRef": "sha256:<hex>",
      "network": { "egress": "none" },
      "resources": { "cpu": "500m", "memory": "512Mi" },
      "secrets": []
    }
  ],
  "targets": [],
  "datastoreManifests": [],
  "provenance": {
    "compilerName": "massive-go",
    "compilerVersion": "0.0.0",
    "sourceSpecHash": "sha256:<hex>"
  }
}
```

The projection is intentionally not a compatibility promise for runners. Runners consume the persisted Cap'n Proto plan and target manifests.
