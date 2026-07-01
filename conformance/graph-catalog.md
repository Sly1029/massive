# Graph Catalog

Status: v0 contract

This catalog names the graph shapes every compiler, runner, target backend, and conformance fixture must support in v0. The `Case ID` values are stable fixture keys. Do not rename them without a contract version bump.

The catalog intentionally describes topology, not language-specific authoring syntax. TypeScript, Python, and future SDKs should emit equivalent `WorkflowSpec` graph IR for these cases.

The canonical machine-readable artifact is [`graph-catalog.json`](graph-catalog.json). This markdown table is the human-readable view. Tests must consume the JSON, not parse this table.

<!-- graph-catalog:start -->
| Case ID | Shape | Topology | Executable steps | Directed edges | Merge inputs |
|---------|-------|----------|------------------|----------------|--------------|
| `passthrough` | Passthrough | start -> end | 0 | 1 | none |
| `single-step` | Single step | start -> hello -> end | 1 | 2 | none |
| `linear-chain` | Linear chain | start -> double -> increment -> label -> end | 3 | 4 | none |
| `diamond` | Fan-out/fan-in diamond | start -> split -> {left,right} -> merge -> end | 4 | 6 | merge: left, right |
| `uneven-fan-in` | Uneven branch-depth fan-in | start -> split -> short -> merge -> end and split -> long -> long-tail -> merge | 5 | 7 | merge: short, long-tail |
| `multi-stage-merge` | Multi-stage fan-in | split fans into a1,a2,b1,b2; pairwise merges feed merge-final | 8 | 12 | merge-a: a1, a2; merge-b: b1, b2; merge-final: merge-a, merge-b |
| `batch-merge-100` | 100-way split/merge | start -> split -> worker-000..worker-099 -> merge -> end | 102 | 202 | merge: worker-000..worker-099 (100 sources) |
<!-- graph-catalog:end -->

## Fixture Requirements

Every downstream fixture directory should use these case IDs:

```text
conformance/fixtures/specs/<case-id>/workflow-spec.json
conformance/fixtures/plans/<case-id>/workflow-plan.json
conformance/fixtures/bundles/<case-id>/<target>/
```

Descriptor fixtures may either use the graph case ID when they describe a full graph run, or a case-specific suffix when they focus on one step:

```text
conformance/fixtures/descriptors/<case-id>/descriptor.json
conformance/fixtures/descriptors/<case-id>/<node-id>.json
```

## Support Expectations

- `passthrough`, `single-step`, `linear-chain`, and `diamond` are the minimum smoke set for SDK spec emission, Go compiler validation, local orchestration, and Argo bundle generation.
- `uneven-fan-in` and `multi-stage-merge` prove dependency wiring is not just depth-symmetric.
- `batch-merge-100` proves graph lowering and target compilation handle wide DAGs deterministically.
