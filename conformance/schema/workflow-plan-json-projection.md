# WorkflowPlan Canonical JSON Encoding

Status: v0 artifact contract

`WorkflowPlan` is canonically written by the Go compiler as JSON typed by [`workflow-plan.proto`](workflow-plan.proto). The `.proto` schema defines the field names and types; `protojson` maps those fields to this JSON body. Artifact identity still comes from the canonical field tree defined in [`hashing.md`](hashing.md), not from any binary wire encoding.

The artifact body is a deterministic JSON rendering of the typed plan field tree:

- object keys are sorted lexicographically at every level,
- list order is the plan's semantic order,
- node and environment variants render as objects with explicit `kind` strings plus the selected value fields,
- digest fields render as `sha256:<hex>` strings,
- `ArtifactRef` renders as `{ "key", "hash", "contentType" }`,
- `GraphIR.irVersion` is the explicit semantic graph contract version and is distinct from the enclosing transport `schemaVersion`,
- `hashing` is required and identifies the exact plan-hash recipe; source
  package references carry their required source-package recipe independently,
- `specHashing` is required and identifies the recipe for the foreign
  `specHash` digest carried by the plan,
- `SchemaEntry.canonicalJson` is sorted-key canonical JSON for the lowered portable schema value,
- wall-clock fields, including compile time and bundle emission time, are not part of the canonical JSON artifacts and must not appear in the plan,
- scalar fields use explicit presence (`optional` in the `.proto`): a set field always appears, including zero values such as `"schemaVersion": 0`,
- repeated fields appear only when non-empty; empty lists are omitted (a step node with no fan-in has no `mergeInputs` member). This matches `protojson`'s default marshaling, so the canonical writer is plain `protojson.Marshal` of a fully populated message,
- plans must not contain dangling references: every step node's `contractRef` resolves to an entry in `contracts`, and every contract's `environmentRef` resolves to an entry in `environments`.

Graph IR `0.2` adds only data-only exhaustive routing records. A `decision`
node carries its input schema, string `selector`, and ordered
`cases[{tag,schema}]`; a conditional edge carries `case`; and a `select` node
carries `decisionRef`, an output schema, and ordered
`selectInputs[{case,source}]`. The compiler preserves those declarations in the
typed plan without serializing predicates, transforms, or Python/TypeScript
callables. A target that cannot execute these semantics must reject the plan
with a semantic capability diagnostic rather than treating it as a static DAG.

Shape:

```json
{
  "schemaVersion": 0,
  "hashing": {
    "algorithm": "sha256",
    "canonicalization": "canonical-json-v0",
    "recipe": "workflow-plan",
    "recipeVersion": 1
  },
  "specHashing": {
    "algorithm": "sha256",
    "canonicalization": "canonical-json-v0",
    "recipe": "workflow-spec",
    "recipeVersion": 1
  },
  "planHash": "sha256:<hex>",
  "specHash": "sha256:<hex>",
  "graph": {
    "irVersion": "0.1",
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
        "contractRef": "sha256:<hex>"
      },
      {
        "id": "merge",
        "kind": "step",
        "inputSchema": "sha256:<hex>",
        "outputSchema": "sha256:<hex>",
        "symbolRef": "linear-chain/merge",
        "contractRef": "sha256:<hex>",
        "mergeInputs": ["left", "right"]
      },
      {
        "id": "route",
        "kind": "decision",
        "inputSchema": "sha256:<hex>",
        "selector": "kind",
        "cases": [
          { "tag": "accepted", "schema": "sha256:<hex>" },
          { "tag": "rejected", "schema": "sha256:<hex>" }
        ]
      },
      {
        "id": "choose",
        "kind": "select",
        "decisionRef": "route",
        "outputSchema": "sha256:<hex>",
        "selectInputs": [
          { "case": "accepted", "source": "accept" },
          { "case": "rejected", "source": "reject" }
        ]
      }
    ],
    "edges": [
      { "from": "__start", "to": "double" },
      { "from": "route", "to": "accept", "case": "accepted" }
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
      "packageHash": "sha256:<hex>",
      "packageId": "ts-main"
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
      "resources": { "cpu": "500m", "memory": "512Mi" }
    }
  ],
  "provenance": {
    "compilerName": "massive-go",
    "compilerVersion": "0.0.0",
    "sourceSpecHash": "sha256:<hex>"
  }
}
```

`manifest` and `sourceArchive` are optional post-materialization fields. The
pure WorkflowSpec compiler does not emit them because no bytes have been
committed yet. Remote target lowering must obtain concrete, body-hash-verified
artifact references from a materialization manifest; it must not predict keys
or reuse `packageHash` as an archive-body hash.

This JSON shape is the compatibility promise for runners. `WorkflowPlan`, source manifests, environment manifests, provenance records, and target bundle manifests are canonical JSON artifacts typed by the `.proto` schemas.
