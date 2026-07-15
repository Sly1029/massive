# Implementation Roadmap

Status: draft

This roadmap turns the architecture in [overview.md](overview.md), [ir-and-datastore.md](ir-and-datastore.md), [argo-backend.md](argo-backend.md), and [environment-materialization.md](environment-materialization.md) into an implementation plan that can be **handed off and parallelized** across multiple agents or engineers.

It is organized so that independent workstreams can proceed at the same time behind frozen contracts, rather than as a single serial slice list. Read this order:

1. [Current State](#current-state) — ground truth of what exists today.
2. [Target v2 Architecture](#target-v2-architecture) — where we are pushing.
3. [Parallelization Principle](#parallelization-principle) — how contracts + fixtures unlock concurrency.
4. [Milestones](#milestones) — the four gates that define "done."
5. [Workstreams](#workstreams) — assignable units of ownership with dependencies.
6. [Dependency Graph](#dependency-graph) — what blocks what.
7. [Detailed Task Breakdown](#detailed-task-breakdown) — task IDs, acceptance criteria, and dependencies for handoff.
8. [Cross-Cutting Requirements](#cross-cutting-requirements) — determinism, hashing, testing policy.
9. [Handoff Checklist](#handoff-checklist) — how a picker-upper starts a task.

---

## Current State

Ground truth as of this revision. The spec docs describe the **target** architecture; most runtime code is still an earlier prototype. Shared contracts in `conformance/` are landing; do not assume the docs describe running software end to end.

**What exists (`packages/sdk`, TypeScript):**

- `workflow.ts` — a typed Graphology-based builder: `workflow()`, `step()`, fluent `path()`, `channel()`, `stateSchema()`, fan-in via `mergeInputs`. This is real and largely matches [authoring-model.md](authoring-model.md). Channels are authored but not yet emitted — `emit.ts` rejects any workflow that declares them (post-M2 schema work).
- `emit.ts` — emits a deterministic `WorkflowSpec` (JSON) that conforms to the shared schema. **This is the spec side of the spec/plan split; the SDK no longer writes plans.** Source-package globbing, symlink-escape guards, and per-file content hashing live in `source-package.ts`.
- `datastore/` — the consolidated datastore client (`put`/`get`/`exists`/`list`) with a local filesystem backend and an S3-compatible backend.
- `runner/` — the TypeScript step runner (language adapter): it resolves a step symbol from a `StepInvocationDescriptor` and executes it against the datastore. There is no in-SDK execution path; the legacy in-memory runner, `compile.ts` plan emitter, and `argo.ts` emitter have all been removed.
- `stable.ts` — `sha256*` + `stableStringify` (sorted-key canonical JSON). Reusable as the basis for canonical hashing.
- `schema.ts` — Zod → portable schema lowering (`lowerPortableSchema`).

**What exists (`conformance/` contracts + Go bootstrap):**

- `conformance/graph-catalog.json` and generated `conformance/graph-catalog.md` — canonical v0 graph shapes (passthrough, linear, diamond, fan-in variants, etc.) keyed by stable case IDs.
- `conformance/schema/workflow-spec.schema.json` — shared `WorkflowSpec` JSON Schema (draft 2020-12). Validating shape fixtures under `conformance/fixtures/specs/` for `passthrough`, `linear-chain`, `diamond`, and `invalid-missing-contract-ref`; checked by `pnpm check:conformance`.
- `conformance/schema/workflow-plan.proto` and `bundle-manifest.proto` — proto3 schemas for `WorkflowPlan` and bundle manifests. Canonical JSON is the artifact body, documented in `workflow-plan-json-projection.md`. `protoc` compile and protojson round-trip tests live in `conformance/schema/proto_schema_test.go`; checked by `pnpm check:proto`.
- `conformance/schema/step-invocation-descriptor.schema.json` — `StepInvocationDescriptor` JSON Schema with a golden fixture under `conformance/fixtures/descriptors/`; checked by `pnpm check:conformance`.
- `conformance/schema/datastore-layout.md` — frozen key templates, digest encoding, and project-key normalization (WS-0.5).
- `conformance/schema/hashing.md` — canonical hashing spec with golden vectors under `conformance/fixtures/hashing/`, tested cross-language by `packages/sdk/test/hashing.test.ts` (TS) and `conformance/schema/hashing_vector_test.go` (Go).
- `conformance/fixtures/plans/` — golden `WorkflowPlan` JSON artifacts for passthrough, linear-chain, and diamond, checked for structural consistency against the spec fixtures.
- `go.mod` — module `github.com/Sly1029/massive`, Go 1.24.4. Go code today is limited to the schema conformance tests above; there is no `WorkflowSpec` → `WorkflowPlan` compiler yet.

**What does NOT exist yet:**

- No Node environment materialization (WS-9); the container escape hatch is the only supported environment for deployable targets.
- No Argo cluster execution of real steps yet (WS-8): the Argo backend emits and validates a `WorkflowTemplate` bundle offline, but submitting it to a live cluster with a pod-reachable datastore is WS-8.
- The Go compiler is the only plan writer: the legacy `compile.ts`/`argo.ts` plan-JSON surface has been removed from the SDK, which now emits only a `WorkflowSpec`.

**Gap summary:** WS-0 through WS-7 have landed: contracts, SDK `WorkflowSpec` emission, the Go compiler (`internal/spec`, `internal/plan`, `cmd/massive-compiler`), datastores (Go local/S3 + TS clients), the TS step runner, the Go local orchestrator (WS-5), the `massive run` CLI (WS-6, M1), and the Argo `WorkflowTemplate` compiler (WS-7) via a backend-neutral `internal/target` interface with an `internal/target/argo` backend. What remains for M2 is WS-8: submitting the generated template to a local cluster with a pod-reachable datastore so real steps execute end to end.

---

## Target v2 Architecture

```text
TypeScript authoring source
  -> TS SDK emits deterministic WorkflowSpec JSON  (conforms to shared schema)
  -> Go compiler validates spec, materializes/records artifacts
  -> Go writes canonical WorkflowPlan JSON + manifests to the datastore
  -> Go orchestrator schedules DAG, emits StepInvocationDescriptors
  -> TS step runner (language adapter) executes each step from the datastore
  -> run artifacts land in the datastore (local FS or S3-compatible)
```

Same path for `local` and `argo`. No in-memory execution path. See [overview.md](overview.md) for the full contract statement.

---

## Parallelization Principle

**Contracts are frozen first, as versioned fixtures. Everything else is built against fixtures, not against other teams.**

The single biggest risk to parallel work is teams blocking on each other's live code. We remove that by making the boundaries between components **checked-in artifacts**:

- The **TS SDK** is built to *produce* `WorkflowSpec` JSON that validates against a JSON Schema and matches golden fixtures. It does not need the Go compiler to exist.
- The **Go compiler** is built to *consume* checked-in `WorkflowSpec` JSON fixtures and *produce* `WorkflowPlan` fixtures. It does not need the SDK to exist.
- The **TS step runner** is built to *consume* checked-in `StepInvocationDescriptor` JSON fixtures. It does not need the Go orchestrator to exist.
- The **Argo compiler** is built to *consume* checked-in `WorkflowPlan` fixtures and *produce* bundle fixtures. It does not need a live orchestrator.
- The **datastore** implementations are built against a **shared contract test suite** that both local FS and S3/MinIO must pass.

The coordination artifact is a top-level **`conformance/`** directory (created in WS-0):

```text
conformance/
  schema/                     # JSON Schemas + proto schemas (the contracts)
  fixtures/
    specs/<case>/workflow-spec.json           # golden WorkflowSpec inputs
    plans/<case>/workflow-plan.json           # golden WorkflowPlan JSON
    descriptors/<case>/descriptor.json        # golden StepInvocationDescriptor
    bundles/<case>/argo/...                   # golden Argo bundle output
  graph-catalog.md            # the canonical v0 graph shapes every layer must support
```

Every workstream's tests assert against `conformance/`. When a contract must change, it changes in `conformance/` with a version bump, and dependents update against the new fixtures — asynchronously.

**Consequence:** once WS-0 lands (even as a v0 draft), WS-1, WS-2, WS-3, WS-4, and WS-9 can all start the same day.

---

## Milestones

| Milestone | Definition of done | Gated by |
|-----------|--------------------|----------|
| **M0 — Contracts frozen** | `conformance/` exists with v0 JSON Schemas, proto schemas, descriptor schema, datastore layout, hashing spec, and at least the passthrough + linear + diamond graph fixtures. | WS-0 |
| **M1 — Local round trip** | `massive run workflow.ts` emits a `WorkflowSpec`, Go compiles a `WorkflowPlan` to the datastore, the Go orchestrator drives the TS step runner per step, and real step outputs land in the datastore. The in-memory `run.ts` path is deleted. | WS-1, WS-2, WS-3, WS-4, WS-5, WS-6 |
| **M2 — Argo executable wedge** | `env.container(...)` workflow compiles to a real `WorkflowTemplate` bundle; a run submitted from it on a local cluster executes real steps that read/write a pod-reachable datastore (MinIO) and reaches Succeeded. `env.node(...)` is rejected for Argo with a clear diagnostic. | WS-7, WS-8 (+ M1) |
| **M3 — Node env materialization** | `env.node(...)` materializes a real dependency environment from a lockfile, deduped by env key, usable by local and (then) Argo targets. | WS-9 (+ M1) |
| **M4 — Hardening** | Field-level provenance, determinism guarantees, S3/MinIO parity, and the full invariant set from [argo-backend.md](argo-backend.md) beyond the wedge. | WS-10 (+ M2) |

Ship M1 before starting M2. Ship M2 before M3/M4 unless staffing allows a dedicated env-materialization track (WS-9 is off the critical path — see the graph).

---

## Workstreams

Each workstream is an ownable unit with a clear interface boundary. "Can start at M0" means it only needs frozen contracts, not other teams' code.

| ID | Workstream | Language | Depends on | Unblocks | Can start at |
|----|------------|----------|------------|----------|--------------|
| **WS-0** | Contracts & conformance fixtures | schema/docs | — | everything | now |
| **WS-1** | TS SDK: `WorkflowSpec` emission | TS | WS-0 | WS-6 | M0 |
| **WS-2** | Go compiler core: spec → plan | Go | WS-0 | WS-5, WS-7 | M0 |
| **WS-3** | Datastore: Go (local+S3) + shared contract tests + TS client | Go + TS | WS-0 (layout) | WS-4, WS-5, WS-7 | M0 |
| **WS-4** | TS step runner (language adapter) | TS | WS-0, WS-3 | WS-5, WS-7 | M0 (against fixtures) |
| **WS-5** | Go local orchestrator + run manifests | Go | WS-2, WS-3, WS-4 | WS-6 | after WS-2/WS-3 |
| **WS-6** | CLI `massive run` orchestration (integration) | TS | WS-1, WS-5 | **M1** | after WS-1/WS-5 |
| **WS-7** | Argo `WorkflowTemplate` compiler | Go | WS-2, WS-3, WS-4 | WS-8 | after WS-2/WS-3 |
| **WS-8** | Argo cluster test harness | TS/Go | WS-7 | **M2** | after WS-7 |
| **WS-9** | Node environment materialization | Go (+TS) | WS-0, WS-3 | M3 | M0 (off critical path) |
| **WS-10** | Determinism, provenance, invariants, S3 parity | Go | WS-2, WS-7 | **M4** | after WS-2 |

---

## Dependency Graph

```text
                         WS-0 Contracts & fixtures (M0)
        ┌───────────────┬──────────────┬───────────────┬───────────────┐
        │               │              │               │               │
      WS-1            WS-2           WS-3            WS-4            WS-9
   SDK spec        Go compiler     Datastore     TS step runner   Env materialize
   emission        (spec→plan)   (local+S3+TS)   (adapter)        (off critical path)
        │               │   \        /   \          /   \
        │               │    \      /     \        /     \
        │               │     WS-5 Go local orchestrator   \
        │               │        (needs WS-2+WS-3+WS-4)      \
        │               │              │                      \
        └──────► WS-6 CLI `massive run` ◄──────────────────    WS-7 Argo compiler
                    │  == M1 local round trip ==          \      (needs WS-2+WS-3+WS-4)
                    │                                       \          │
                    │                                        \      WS-8 Argo cluster harness
                    ▼                                         \        │  == M2 wedge ==
              (M1 unblocks M2 execution work)                  └──► WS-10 hardening == M4 ==
```

**Start-today set (all parallel once WS-0 drafts land): WS-1, WS-2, WS-3, WS-4, WS-9.**

---

## Detailed Task Breakdown

Task IDs are `WS-<n>.<k>`. Each has an acceptance criterion (AC) and dependencies. All tests must be **mock-free** per [testing-strategy.md](testing-strategy.md) and org policy — real filesystems, real MinIO, real clusters, real generated artifacts, schema validation. Run `node scripts/check-no-test-mocks.mjs` before finishing any task.

### WS-0 — Contracts & Conformance Fixtures  *(blocking; land fast, ideally one owner or a tight pair)*

The point of this workstream is to unblock everyone else, so bias toward landing **v0 drafts quickly** and iterating via versioned bumps rather than perfecting up front.

- **WS-0.1 — Create `conformance/` and the graph catalog.** Port the v0 graph catalog from [testing-strategy.md](testing-strategy.md) (passthrough, single-step, linear, diamond, uneven branch-depth fan-in, multi-stage fan-in, 100-way split/merge) into canonical `conformance/graph-catalog.json` with a stable case ID per shape and a generated human-readable `conformance/graph-catalog.md` view.
  - AC: every case has an ID; downstream fixtures key off these IDs.
  - Status: implemented in [`../../conformance/graph-catalog.json`](../../conformance/graph-catalog.json) and [`../../conformance/graph-catalog.md`](../../conformance/graph-catalog.md), checked by `pnpm check:conformance`. Go compiler work in WS-2 must read the same JSON catalog for its matching conformance assertion.
- **WS-0.2 — `WorkflowSpec` JSON Schema.** Author the shared schema for `WorkflowSpec` per [ir-and-datastore.md](ir-and-datastore.md): `GraphIR`, schema table, symbol table, source package table, environment table, effective execution-contract table, per-node `contractRef`, target requests. Provide it as a JSON Schema (draft 2020-12) under `conformance/schema/workflow-spec.schema.json`.
  - AC: schema validates a hand-authored spec for the passthrough + linear + diamond cases; rejects a spec missing a `contractRef`.
  - Status: implemented in [`../../conformance/schema/workflow-spec.schema.json`](../../conformance/schema/workflow-spec.schema.json), with validating shape fixtures under [`../../conformance/fixtures/specs`](../../conformance/fixtures/specs) and Ajv-backed checks in `pnpm check:conformance`. The v0 schema covers DAG step nodes and `mergeInputs` fan-in only; channels, branches, foreach/map, and reducers are post-M2 schema work.
- **WS-0.3 — Proto-typed canonical JSON plan schemas.** Define `workflow-plan.proto` (`WorkflowPlan` joining `GraphIR` + `ExecutionContract` + materialization refs + provenance) and `bundle-manifest.proto`, plus a documented canonical JSON encoding of the plan.
  - AC: `protoc` compile succeeds; a protojson round-trip test preserves the JSON field tree.
  - Status: supersedes the earlier Cap'n Proto schema checkpoint. Implemented in [`../../conformance/schema/workflow-plan.proto`](../../conformance/schema/workflow-plan.proto), [`../../conformance/schema/bundle-manifest.proto`](../../conformance/schema/bundle-manifest.proto), and [`../../conformance/schema/workflow-plan-json-projection.md`](../../conformance/schema/workflow-plan-json-projection.md), with real `protoc` compile and protojson checks in `pnpm check:proto`.
- **WS-0.4 — `StepInvocationDescriptor` schema.** Define the descriptor as a shared schema message (JSON transport for v0) with all fields from [ir-and-datastore.md](ir-and-datastore.md#step-invocation-descriptor). Keep the logical fields transport-neutral.
  - AC: JSON Schema under `conformance/schema/step-invocation-descriptor.schema.json`; a golden descriptor validates.
  - Status: implemented in [`../../conformance/schema/step-invocation-descriptor.schema.json`](../../conformance/schema/step-invocation-descriptor.schema.json), with a validating golden fixture at [`../../conformance/fixtures/descriptors/linear-chain/descriptor.json`](../../conformance/fixtures/descriptors/linear-chain/descriptor.json), checked by `pnpm check:conformance`.
- **WS-0.5 — Datastore layout + key rules.** Freeze the storage layout and content-addressing rules from [ir-and-datastore.md](ir-and-datastore.md#storage-layout) as `conformance/schema/datastore-layout.md`: blob/spec/env/package/plan/target/run key templates, `sha256:<hex>` full-digest keys, project-key normalization.
  - AC: documented key templates with at least one worked example per template.
  - Status: implemented in [`../../conformance/schema/datastore-layout.md`](../../conformance/schema/datastore-layout.md).
- **WS-0.6 — Canonical hashing spec.** Specify field-tree canonicalization and hashing for `specHash`, `planHash`, source-package hash, env key, and runtime-artifact hash. Baseline: the sorted-key JSON approach already in `packages/sdk/src/stable.ts`. Document exactly which fields each hash covers (see [Cross-Cutting](#cross-cutting-requirements)).
  - AC: `conformance/schema/hashing.md` with a golden input → expected digest vector that both TS and Go test against.
  - Status: implemented in [`../../conformance/schema/hashing.md`](../../conformance/schema/hashing.md), with golden vectors under [`../../conformance/fixtures/hashing`](../../conformance/fixtures/hashing) and cross-language checks in `packages/sdk/test/hashing.test.ts` and `conformance/schema/hashing_vector_test.go`.
- **WS-0.7 — Seed golden fixtures.** Produce `conformance/fixtures/specs/<case>/workflow-spec.json` for at least passthrough, linear, and diamond, plus matching `plans/` JSON artifacts and one `descriptors/` example.
  - AC: spec and descriptor fixtures validate against the WS-0.2/WS-0.4 JSON Schemas in CI; plan fixtures (which have no JSON Schema in v0 — see WS-0.8) are checked by structural-consistency and digest assertions against their spec fixtures.
  - Status: implemented with spec fixtures under [`../../conformance/fixtures/specs`](../../conformance/fixtures/specs), plan JSON artifacts under [`../../conformance/fixtures/plans`](../../conformance/fixtures/plans), and a descriptor example at [`../../conformance/fixtures/descriptors/linear-chain/descriptor.json`](../../conformance/fixtures/descriptors/linear-chain/descriptor.json), checked by `pnpm check:conformance`.
- **WS-0.8 — `WorkflowPlan` JSON Schema.** Author a JSON Schema for the plan JSON artifact documented in `workflow-plan-json-projection.md`, so plan fixtures are schema-validated rather than only structurally spot-checked. *(depends: WS-0.3; off the critical path)*
  - AC: `conformance/fixtures/plans/` fixtures validate against it in CI; unblocks stronger WS-2.3 conformance assertions.

### WS-1 — TS SDK: `WorkflowSpec` Emission  *(depends: WS-0)*

Refactor `packages/sdk` from JSON *plan* compilation to `WorkflowSpec` *emission*. The Go compiler becomes the only plan writer.

- **WS-1.1 — Introduce the `WorkflowSpec` emitter.** New `packages/sdk/src/emit.ts` that lowers a `WorkflowBuilder` into `WorkflowSpec` JSON conforming to `conformance/schema/workflow-spec.schema.json`. Reuse `lowerPortableSchema` (`schema.ts`) for the schema table and `stable.ts` for `specHash`.
  - AC: emits valid specs for every graph-catalog case; `specHash` matches the WS-0.6 vector; validated against the JSON Schema in tests.
  - Status: implemented in `packages/sdk/src/emit.ts`, tested by `packages/sdk/test/emit.test.ts` (schema validation across all graph-catalog cases, determinism, `specHash` self-exclusion per `hashing.md`).
- **WS-1.2 — Symbol table + source-package manifest.** Emit stable symbol IDs (`packageId` + module + export) and a source-package manifest listing exact files + content hashes.
  - AC: symbols are stable across runs; manifest lists exact files/hashes; a changed source file changes the package hash and nothing else.
  - Status: implemented in `packages/sdk/src/emit.ts` + `source-package.ts` (source-package manifest with per-file `sha256:<hex>` hashes, code-unit path ordering); package-hash sensitivity covered in `emit.test.ts`.
- **WS-1.3 — Execution-contract merge → effective contracts.** Implement `contract()`, `env.node()`, `env.container()`, `net.*`, `secret.ref()` authoring primitives ([authoring-model.md](authoring-model.md#execution-contracts-in-authoring)); merge workflow defaults with step overrides into effective contracts deduped by content hash; emit the environment table keyed by env-spec hash; put `contractRef` on every executable node.
  - AC: two steps with the same effective env share one env-table entry even if resources/secrets differ; every step node has a `contractRef`.
  - Status: implemented in `packages/sdk/src/contract.ts` (authoring primitives + merge) and `emit.ts` (dedup by content hash, env table keyed by env-spec hash); covered in `emit.test.ts`.
- **WS-1.4 — `massive.config.ts` + target requests.** Implement `defineWorkflowPackage(...)` ([ir-and-datastore.md](ir-and-datastore.md#source-packages)) with `include`, `entrypoint`, `environment`, `targets` (`target.local`, `target.argo`). Emit target requests into the spec.
  - AC: a package config produces a spec with the requested targets; Argo target request round-trips into the spec.
  - Status: implemented in `packages/sdk/src/config.ts` (`defineWorkflowPackage`, `target.local`, `target.argo`); targets participate in `specHash`; covered in `packages/sdk/test/config.test.ts`.
- **WS-1.5 — Entrypoint resolution + zero-config.** Implement `workflow.ts`, `workflow.ts#named`, ambiguity error on multiple exports, and directory resolution via config. Zero-config single-file synthesizes an ephemeral config with `local` target only; deployable targets require explicit config.
  - AC: all four resolution cases behave per [open-questions.md](open-questions.md#workflow-entrypoints-and-package-roots); zero-config refuses to request Argo.
  - Status: implemented in `packages/sdk/src/resolve.ts`; all resolution cases + zero-config Argo refusal covered in `packages/sdk/test/resolve.test.ts` against real temp-dir fixtures.
- **WS-1.6 — Remove the in-memory runtime as a supported path.** Delete/retire `run.ts` and `argo.ts`'s `runArgoLocal` from the public surface (keep step `run` closures for the *runner* to execute, not for in-SDK execution). Update `index.ts` exports.
  - AC: `index.ts` no longer exports an in-memory `run`; SDK tests assert on emitted spec/artifacts only.
  - Status: implemented — `run.ts` deleted and the in-memory/`runArgoLocal` execution path removed from the public surface; the legacy `argo.ts` and `compile.ts` plan emitters have since been retired entirely, leaving `emit.ts` as the only emitter. `sdk.test.ts` asserts on the emitted `WorkflowSpec` and datastore artifacts (the SDK no longer emits plans).

### WS-2 — Go Compiler Core: spec → plan  *(depends: WS-0)*

New `go/` module. Suggested layout: `go/cmd/massive-compiler`, `go/internal/spec`, `go/internal/plan`, `go/internal/planpb` (generated).

- **WS-2.1 — Bootstrap the Go module + proto codegen.** `go.mod`, protobuf Go bindings generated from WS-0.3, CI wiring, `go test ./...`.
  - AC: `go build ./...` and generated proto types compile. WS-0.3 validates schemas with the real `protoc` CLI and protojson round-trip test.
  - Status: complete — module bootstrapped with generated `planpb` bindings; protojson round-trip checks in `conformance/schema/proto_schema_test.go`.
- **WS-2.2 — Read + validate `WorkflowSpec`.** Parse spec JSON; validate portable schema conformance, graph integrity (DAG, reachability, single start/end), contract references exist, symbol references exist, target requests, datastore references.
  - AC: accepts all WS-0.7 valid fixtures; produces a specific diagnostic for a spec with a dangling `contractRef` and for a cyclic graph. Emit deterministic JSON dumps of the parsed spec for conformance assertions.
  - Status: implemented in `internal/spec` (embedded JSON-Schema validation + typed semantic diagnostics: cycles named, reachability, dangling contract/env/symbol refs, mergeInputs).
- **WS-2.3 — Emit canonical JSON `WorkflowPlan`.** Join `GraphIR` + `ExecutionContract` + symbol table + (initially empty) materialization refs + provenance + compiler version; compute `planHash` per WS-0.6.
  - AC: byte-stable canonical JSON plan for identical inputs; plan JSON matches `conformance/fixtures/plans/`.
  - Status: implemented in `internal/plan` + `internal/canonical` (RFC 8785 canonicalizer validated against the shared golden vector); golden tests compare against plan fixtures with digest values normalized (fixtures carry placeholders); byte-stability asserted. CLI: `cmd/massive-compiler`.
  - Decision: [open-questions.md](open-questions.md#plan-artifact-encoding) records the July 2026 checkpoint resolution in favor of proto3 schemas with canonical JSON bodies.
- **WS-2.4 — Topological schedule.** Produce the deterministic execution order used by the local orchestrator and by Argo dependency wiring.
  - AC: diamond and multi-stage fan-in produce correct, stable orders; used by both WS-5 and WS-7.
  - Status: implemented as `plan.BuildSchedule` — Kahn order with UTF-16 code-unit tie-breaks and per-node depths; table-driven tests incl. 100-way split/merge.

### WS-3 — Datastore  *(depends: WS-0.5)*

- **WS-3.1 — Shared datastore contract test suite.** A single suite (Go) that any implementation must pass: put/get/exists, content-type, conditional write, listing, key validation, content-addressing.
  - AC: suite runs against an implementation instance passed in; no mocks.
  - Status: implemented as `datastore.RunDatastoreContract` in `internal/datastore`.
- **WS-3.2 — Go local filesystem datastore.** Port `packages/sdk/src/datastore/local.ts` semantics (atomic temp-rename write, key traversal guards) to Go.
  - AC: passes WS-3.1 in a temp dir.
  - Status: implemented (`internal/datastore/local.go`), atomic temp+rename writes and traversal guards ported from the TS client.
- **WS-3.3 — Go S3-compatible datastore.** Endpoint-configurable (`datastore.s3({ endpoint })`) so R2/MinIO work. Test against a real MinIO container.
  - AC: passes WS-3.1 against MinIO; documented as opt-in/tagged if MinIO isn't guaranteed in CI ([open-questions.md](open-questions.md#datastore)).
  - Status: implemented (`internal/datastore/s3.go`, minio-go, env-only credentials); contract suite runs against a real MinIO container via docker with clean skip when unavailable. Listing stats each object because bucket listings do not carry content types.
- **WS-3.4 — TS datastore client for the step runner.** The runner needs to fetch source packages/inputs and write outputs. Provide a TS client with the same key rules; support local FS + S3-compatible.
  - AC: TS client and Go client interoperate on the same layout (a Go-written artifact is readable by TS and vice versa) in a functional test.
  - Status: implemented (`packages/sdk/src/datastore/` local + S3 clients on @aws-sdk/client-s3); cross-process Go↔TS interop test in `packages/sdk/test/datastore-client.test.ts`. Local FS carries a content-type sidecar convention shared by both languages.

### WS-4 — TS Step Runner (language adapter)  *(depends: WS-0.4, WS-3.4)*

New `packages/sdk/src/runner/` shipping a `massive-step-runner` bin ([ir-and-datastore.md](ir-and-datastore.md#language-runtime-adapters)). Can start against WS-0 descriptor **fixtures** before WS-3.4 lands, then wire real datastore reads.

- **WS-4.1 — Descriptor parsing isolated from execution.** Parse `StepInvocationDescriptor` JSON; keep parsing separate from execution so a future transport swaps in cleanly.
  - AC: parses all `conformance/fixtures/descriptors/`; rejects a malformed descriptor with a clear error.
  - Status: implemented (`packages/sdk/src/runner/descriptor.ts`), Ajv-validated with typed decode; parsing isolated from execution.
- **WS-4.2 — Source package fetch + symbol resolution.** Fetch/unpack the referenced source package from the datastore; import the module/export symbol.
  - AC: given a descriptor pointing at a fixture package, resolves and loads the exported step function.
  - Status: implemented (`packages/sdk/src/runner/source.ts`) with hash verification; real `.tar.zst` unpacking is a documented v0 follow-up (directory-artifact shape covers v0 tests).
- **WS-4.3 — Execute one step with schema validation at the boundary.** Read canonical JSON input artifact, validate against input schema ref, execute, validate output against output schema ref, write canonical JSON output artifact.
  - AC: executes a real fixture step end to end against a temp local datastore; a schema mismatch on input or output produces a precise diagnostic.
  - Status: implemented (`packages/sdk/src/runner/execute.ts`); schema refs resolve from `blobs/sha256:<digest>` keys — WS-5 descriptor emission must write schema blobs there.
- **WS-4.4 — Runner diagnostics.** Clear errors for module resolution, schema validation, and step failure, distinguishable by exit behavior for the orchestrator.
  - AC: three failure modes produce three distinguishable, documented outcomes.
  - Status: implemented (`packages/sdk/src/runner/outcomes.ts`): exit 0 success / 64 descriptor / 65 schema validation / 66 step execution.

### WS-5 — Go Local Orchestrator + Run Manifests  *(depends: WS-2, WS-3, WS-4)*

- **WS-5.1 — Local run manifest + descriptor emission.** From a `WorkflowPlan`, create a run (run ID), emit per-node `StepInvocationDescriptor`s, and record a run manifest in the datastore.
  - AC: descriptors validate against WS-0.4 and match golden fixtures for the linear case; run manifest and result keys per `conformance/schema/datastore-layout.md`.
- **WS-5.2 — Schedule + invoke the TS runner as an external process.** Walk the WS-2.4 order, spawn the step runner per node, thread step outputs to downstream inputs, handle fan-in (`mergeInputs`) and channels.
  - AC: linear + diamond + multi-stage fan-in execute real steps; outputs land at the WS-0.5 datastore keys.
- **WS-5.3 — Validate runtime artifacts.** After each step, validate artifact presence, schema refs, and content hashes.
  - AC: a tampered output artifact hash fails the run loudly.
- **WS-5.4 — Warm runner invocation.** Design the orchestrator↔runner protocol so a long-lived runner process can handle many descriptors; per-step Node cold-start must not define local DX. Per-step spawn is acceptable for M1 bring-up, but the descriptor handoff (WS-5.2) must not bake in one-process-per-step assumptions.
  - AC: a local run of a 20-step linear fixture executes with a single warm runner process producing artifacts identical to cold mode; per-step overhead is measured and recorded.

### WS-6 — CLI `massive run` (integration → M1)  *(depends: WS-1, WS-5)*

New `packages/cli`. This is the glue that makes M1 real.

- **WS-6.1 — `massive run workflow.ts` orchestration.** Discover entrypoint → invoke SDK emitter → write `WorkflowSpec` to datastore → invoke Go compiler → run Go local orchestrator → invoke TS runner per step → surface concise status + final result location.
  - AC: `massive run` on a fixture workflow produces real datastore outputs via the full path; **no in-memory execution**.
- **WS-6.2 — Caching by hash.** Skip emit if source unchanged (source-package hash), skip compile if spec unchanged (`specHash`), skip nothing semantically. Cache keyed in the datastore.
  - AC: a second identical run reuses cached spec + plan (observable via verbose logs) and is materially faster.
- **WS-6.3 — Verbose/inspect surface.** Hide hashes/paths by default; expose them under `--verbose` / an `inspect` subcommand ([overview.md](overview.md#developer-experience)).
  - AC: default output is author-facing; verbose reveals artifact paths + hashes.

### WS-7 — Argo `WorkflowTemplate` Compiler (→ M2 wedge)  *(depends: WS-2, WS-3, WS-4)*

Implement only the executable wedge from [argo-backend.md](argo-backend.md#v0-executable-wedge): plan → materialize tree → validate structure → minimal invariants → emit bundle. Presets/plugins/patches/mediation are WS-10.

- **WS-7.1 — Container-only env gate.** Accept `env.container(...)`; reject `env.node(...)` for Argo with a target-compatibility diagnostic until WS-9 lands Kubernetes materialization.
  - AC: a node-env workflow targeting Argo fails with the documented diagnostic; a container-env workflow proceeds.
  - Status: implemented in `internal/target/argo` (`gateEnvironments` + typed `TargetCompatibilityError`). The compiled plan now carries the materialized container image (`internal/plan` populates `MaterializedEnvironment.container`), so the gate is plan-driven. Node/empty-image/non-container kinds are rejected naming the step and kind; covered by `gate_test.go`.
- **WS-7.2 — `WorkflowTemplate` generation.** Emit a `WorkflowTemplate` (not a one-off `Workflow`) with a DAG mirroring the plan topology; each step template runs the fixed Massive runtime image contract (fetch source package → resolve symbol → read inputs → write outputs) per [environment-materialization.md](environment-materialization.md#container-escape-hatch).
  - AC: generated template's DAG dependencies equal the plan edges; step container is the runtime image, not a placeholder `echo`.
  - Status: implemented in `internal/target/argo` (`template.go`). DAG task dependencies equal the plan's step-to-step edges (sentinels skipped). Each step container runs the **step driver** (`massive-orchestrator step`), not an embedded descriptor: the runtime image ships the driver + TS runner, and the driver loads the plan from the datastore, materializes the node input (incl. `mergeInputs`) from upstream outputs, and builds a schema-valid `StepInvocationDescriptor` at run time — identical to a local run's — so descriptors are never invalid at compile time. Args are the node id (static), `--run-id {{workflow.uid}}`, and `--plan-hash` (static); datastore/project config comes from container env vars bound to `spec.arguments.parameters` (WS-8 supplies values). Contract cpu/memory map to container resources; `serviceAccountName`/`namespace` come from the target config. The step-driver pod contract is proven mock-free (`TestArgoStepDriverMatchesLocalRun`): running a plan node-by-node through the `step` CLI yields step outputs byte-identical to a full local run for linear-chain and diamond.
- **WS-7.3 — Structure validation.** Validate generated YAML against Kubernetes + Argo CRD schemas (pick the CRD version per [open-questions.md](open-questions.md#argo-compiler)).
  - AC: invalid generated YAML is caught in an offline test; valid YAML passes.
  - Status: implemented in `internal/target/argo/validate.go` against Argo `v3.7.16`, vendored at `conformance/schema/argo-workflows-v3.7.16.schema.json` (reusing the `santhosh-tekuri/jsonschema/v6` library `internal/spec` uses). Offline, no cluster/network. `validate_test.go` covers a valid template plus tampered (missing spec, wrong-typed entrypoint/templates) failures.
- **WS-7.4 — Minimal invariants.** Enforce `dag-integrity`, `plan-provenance`, `identity-set` ([argo-backend.md](argo-backend.md#invariants)).
  - AC: each invariant has a passing case and a deliberately-broken failing case.
  - Status: implemented in `internal/target/argo/invariants.go` and recorded in the bundle manifest. Each has a passing and a deliberately-broken failing test (`invariants_test.go`); a hard failure aborts the compile with a diagnostic and emits no bundle.
- **WS-7.5 — Bundle emission.** Emit canonical `workflow-template.yaml`, `massive-plan.json`, `bundle-manifest.json` into `dist/argo/<workflow>/`; bundle manifest records all artifacts.
  - AC: bundle matches `conformance/fixtures/bundles/`; deterministic (no timestamps, sorted keys).
  - Status: implemented. Backend-neutral emission (`internal/target/emit.go`, `BuildBundle`/`WriteBundle`) writes sorted, timestamp-free artifacts plus a canonical `bundle-manifest.json` recording each file's sha256; `cmd/massive-compiler compile-target` defaults output to `dist/<target>/<workflow>/`. The `internal/target` interface is backend-neutral: `CompileInput` carries `TargetKind` + canonical `TargetConfig` bytes (opaque to the package), each backend decodes/validates its own config, and the bundle hash covers the config wholesale so future backends are hash-covered without touching the neutral package. `Registry.Compile` verifies `PlanJSON`/`PlanHash`/`Plan` agree before dispatch (`VerifyPlanConsistency`). Golden fixtures for `linear-chain` and `diamond` (both declare an argo target) live under `conformance/fixtures/bundles/<case>/argo/` with byte-stability and golden-match tests (`golden_test.go`); they are produced through the supported CLI path and a functional test drives `massive-compiler compile-target` end-to-end and asserts the emitted bundle matches the goldens (`TestCompileTargetCLIMatchesGoldenBundles`). Checked by `pnpm check:conformance`.

### WS-8 — Argo Cluster Test Harness (→ M2)  *(depends: WS-7)*

- **WS-8.1 — Cluster harness.** Namespace-per-run: verify/install Argo CRDs+controller, apply the generated template, `argo submit --from workflowtemplate/<name>`, wait for terminal status, collect status/logs, inspect datastore artifacts, delete namespace ([testing-strategy.md](testing-strategy.md#argo-compiler-tests)).
  - AC: `pnpm test:argo-cluster` runs against OrbStack (`orbstack` context, `argo` namespace) and asserts Succeeded + expected datastore artifacts.
- **WS-8.2 — Pod-reachable datastore wiring.** Wire MinIO (in-cluster or reachable S3-compatible endpoint) so step pods read/write real artifacts.
  - AC: a run's step outputs are present in the datastore after completion, produced by real pods.

### WS-9 — Node Environment Materialization (→ M3; off critical path)  *(depends: WS-0, WS-3)*

Build against contracts; do **not** block the M2 wedge. See [environment-materialization.md](environment-materialization.md).

- **WS-9.1 — Env key calculation.** Compute the Node env key from env kind, Node version, package manager (+version), lockfile hash, manifest hash, workspace package hashes, platform, arch, materializer version, build args.
  - AC: two workflows with identical dependency inputs share a key; a lockfile change changes the key; a resource-limit change does not.
- **WS-9.2 — Materialization artifact shape.** Decide + implement tarball vs OCI layer vs backend-specific ([open-questions.md](open-questions.md)); produce `envs/<env-key>/manifest.json` + `runtime.tar.zst` (or chosen shape).
  - AC: real materialization from a small committed fixture package with a lockfile; manifest records source hashes, platform, digests, entrypoint.
- **WS-9.3 — Local target consumption.** Local runner uses materialized env; reuse local package-manager caches where possible; record manifest + hashes.
  - AC: local run of a node-env workflow uses the materialized env, not the ambient environment.
- **WS-9.4 — Kubernetes materialization + Argo `env.node` unblock.** Materialize to object storage; pods fetch/mount the artifact; flip WS-7.1 to accept `env.node(...)` for Argo.
  - AC: an Argo run of a node-env workflow executes with the materialized dependencies; cache hit/miss diagnostics surfaced.

### WS-10 — Determinism, Provenance, Invariants, S3 Parity (→ M4)  *(depends: WS-2, WS-7)*

- **WS-10.1 — Determinism guarantees.** Stable ordering, canonical YAML/JSON, no timestamps, sorted keys, bundle hash covering IR+config+patches+provider identity+compiler version+materialization refs ([argo-backend.md](argo-backend.md#determinism)).
  - AC: byte-identical bundle across two runs on two machines for the same inputs (golden test).
- **WS-10.2 — Field-level provenance.** Emit the provenance sidecar (name, source layer, scope, target path, old/new value hashes) and basic `massive compile --explain <path>`.
  - AC: `--explain` resolves a generated field to its source layer for one worked example.
- **WS-10.3 — Presets/plugins/typed-config/patches pipeline.** Implement the full Argo pipeline stages 3–7 from [argo-backend.md](argo-backend.md#compiler-pipeline) (patches as ordered, named, provenance-carrying; `onMiss: error` default).
  - AC: a strategic-merge and a JSON patch both apply with recorded provenance; missing selector errors by default.
- **WS-10.4 — Full invariant set.** Add `entrypoint-resolves`, `artifact-wiring`, `reserved-names`, `name-uniqueness`, `secret-binding`, `egress-representable`, with severities (hard / soft / forceable).
  - AC: each invariant has passing + failing cases; reserved-names is non-forceable.
- **WS-10.5 — S3/MinIO parity gate.** Run the full local + Argo suites against the S3-compatible datastore, not just local FS.
  - AC: the datastore contract suite and at least one end-to-end run pass on MinIO.

---

## Cross-Cutting Requirements

**Hashing (owned by WS-0.6, consumed everywhere).** All hashes are over canonical field trees canonicalized per **RFC 8785 (JCS)** with a v0 safe-integer number restriction — see `conformance/schema/hashing.md`. Never over JSON whitespace or binary wire encodings. The `stable.ts` implementation is the normative baseline. Documented coverage:

- `specHash`: the full `WorkflowSpec` field tree.
- `sourcePackageHash`: exact file list + per-file content hashes.
- env key: environment-relevant inputs only (WS-9.1) — never resources/secrets/network/scheduling.
- `planHash`: spec hash + GraphIR + ExecutionContract + symbol table + target config + patches + compiler version + env materialization refs + datastore manifest refs + mediation provider identity ([ir-and-datastore.md](ir-and-datastore.md#plan-hash)).
- Keys use full `sha256:<hex>`; manifests record the algorithm; UI may shorten for display only.
- Wall-clock timestamps are not part of canonical compiled artifacts, bundle manifests, hash coverage, or JSON artifacts. If audit timing is needed later, store it in side metadata outside `WorkflowPlan` and `TargetBundleManifest`.

**Security / policy (org requirements — flag in review).** Anything touching secrets, network egress, service accounts, or the datastore credential path warrants explicit review. Never hardcode credentials — env vars or AWS Managed Secrets only. Don't log customer code/PII/tokens. New package-manager manifests (`go.mod`, any new `package.json`, lockfiles) must carry the org-mandated cooldown settings; GitHub Actions must be pinned to full commit SHAs with a version comment. The sidecar/secret-mediation model in [argo-backend.md](argo-backend.md#runtime-mediation) is reserved but out of v0 scope.

**Testing (repo + org policy).** Mock-free everywhere: real FS datastores, real MinIO, real generated manifests, schema validation, real local clusters. `node scripts/check-no-test-mocks.mjs` gates every change; enable the pre-commit hook with `git config core.hooksPath .githooks`. Kubernetes tests are opt-in/tagged.

**AI-generated code.** Every line is reviewed; production merges need a PR with +1. Surface conflicts with these specs rather than silently diverging.

---

## Handoff Checklist

To pick up a task:

1. **Read the contract, not the neighbors.** Your inputs/outputs are files in `conformance/`. Build against those fixtures; do not wait on another workstream's live code.
2. **Confirm your milestone gate.** Check the [Workstreams](#workstreams) table for `Depends on` — if a dependency's fixtures aren't in `conformance/` yet, that's the real blocker to raise.
3. **Write the functional test first.** It should consume/produce the relevant `conformance/` fixture. No mocks.
4. **If you must change a contract**, bump it in `conformance/` with a version note and open the change to dependents asynchronously — do not fork the contract inside your workstream.
5. **Run** `node scripts/check-no-test-mocks.mjs` and (for TS) the deno test command in [testing-strategy.md](testing-strategy.md) before marking done.
6. **Flag** any secret/network/auth/datastore-credential surface for explicit security review in your PR.

**Recommended first wave to staff in parallel:** WS-0 (unblocks all) → then immediately WS-1, WS-2, WS-3, WS-4, WS-9 concurrently. WS-5/WS-6 converge those into M1; WS-7/WS-8 deliver M2.
