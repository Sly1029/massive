# Open Questions

Status: draft

This document tracks decisions that are intentionally unsettled or likely to change after building real workflows.

## Tentative Decisions

### Global State Channel Declaration

Channels are post-M2 for the portable `WorkflowSpec` schema. When they enter the schema, the current tentative direction is to declare them globally in `stateSchema(...)`.

Why this is the current choice:

- the compiler can validate schemas and reducers up front,
- branch discriminants are easy to inspect,
- join and foreach collection behavior is explicit,
- final output projection is easier to compile.

Why it may change:

- local channel declarations may make small workflows more ergonomic,
- reusable workflow modules may want to expose channels alongside their steps,
- a future SDK may infer safe channels from typed publish declarations.

### DAG-Only IR

The v0 portable IR rejects arbitrary cycles.

Why this is the current choice:

- Argo maps naturally to DAGs,
- arbitrary loops push Massive toward owning durable execution,
- DAG-only keeps backend compilers simpler and honest.

Potential future:

- bounded loop primitive with max iteration count and state schema,
- backend-specific cyclic/state-machine target for systems that support it,
- explicit durable runtime backend.

### Closure Boundary

The portable IR must not depend on serializing TypeScript closures.

Why this is the current choice:

- Cap'n Proto must work across languages,
- backend compilers need stable symbols,
- closures are not portable or diffable.

Open issue:

- decide exactly how the TypeScript SDK generates and registers symbol IDs for step functions, reducers, projections, and advanced conditions.

Current validation split:

- each frontend SDK validates language-specific symbol resolvability before emitting `WorkflowSpec`,
- Go validates the emitted portable spec and target compilation invariants,
- runners still fail loudly if deployed source or environment artifacts do not match the validated spec.

### WorkflowSpec Boundary

V0 frontend SDKs emit deterministic `WorkflowSpec` JSON conforming to the shared schema. They do not need to emit Cap'n Proto binary directly.

Why this is the current choice:

- the Cap'n Proto schema remains the language-neutral contract,
- Go can be the first byte-stable Cap'n Proto writer,
- frontend SDKs avoid dependency on immature or divergent Cap'n Proto encoders,
- plan hashes can be defined over canonical field trees rather than raw wire bytes.

Potential future:

- frontend SDKs may emit Cap'n Proto binary once encoder determinism and toolchain stability are proven.

### Target Requests In Specs

Target requests live inside `WorkflowSpec`.

Why this is the current choice:

- deployment intent is portable across machines and frontend SDKs,
- target configuration participates in `specHash`,
- Go can compile a selected requested target without accepting hidden semantic mutations from CLI flags,
- target compatibility checks stay centralized in the Go compiler.

Rules:

- CLI flags may select requested targets and output locations,
- CLI flags should not mutate target semantics,
- unsupported target features are compile-time diagnostics unless an explicit degraded mode is requested in the spec.

### Source Package Boundary

`WorkflowSpec` references one or more source packages by package ID and content hash. Symbols point at package IDs plus language-specific entrypoint metadata.

Why this is the current choice:

- source identity stays separate from dependency environment materialization,
- future Python and mixed-language workflows do not require a schema rewrite,
- monorepo package boundaries can be modeled explicitly,
- source packages can be reused across targets and environments.

V0 TypeScript workflows usually emit one source package, but that is an SDK convenience rather than a schema invariant.

### Workflow Entrypoints And Package Roots

V0 workflow entrypoints are explicit exports.

Current TypeScript behavior:

- `massive run workflow.ts` uses the default export when present,
- `massive run workflow.ts#name` selects a named export,
- if a file has multiple exported workflows and no selector, the CLI reports an ambiguity,
- `massive run workflow/` resolves through package configuration in that directory.

Package roots are explicit and define included source files, local utilities, package manifests, lockfiles, environment defaults, and target requests. V0 packaging is driven by include patterns and required manifests rather than broad implicit source scanning.

For TypeScript v0, package roots use `massive.config.ts`. Future language SDKs may use native configuration surfaces, such as `pyproject.toml` for Python, provided they emit the same portable `WorkflowSpec`.

Zero-config single-file TypeScript workflows are allowed only for local development. They synthesize an ephemeral package config with the selected workflow file, nearby package manifests and lockfiles when present, and the `local` target only. Deployable targets require explicit package configuration.

### Execution Contract Merging

Frontend SDKs merge workflow-level defaults and step-level overrides before emitting `WorkflowSpec`.

Why this is the current choice:

- frontend SDKs own authoring semantics,
- Go target compilers consume effective contracts without duplicating merge logic,
- target compatibility checks are deterministic because every executable graph node has an explicit `contractRef`,
- defaults can still be retained as provenance for diagnostics and explain output.

### Local Execution Path

Local execution uses the same compiler boundary as deployable targets.

Rules:

- SDKs emit `WorkflowSpec`,
- Go compiles the local target and writes `WorkflowPlan`,
- local runners load persisted plans and datastore artifacts,
- steps execute through language runtime adapters,
- frontend builder state and in-memory runtime registries are not a supported execution path.

The developer experience can still be a single command that automatically emits, compiles, and runs locally.

Common local commands should look like `massive run workflow.ts` or `massive run workflow/`. Those commands discover the workflow entrypoint, invoke the language SDK emitter, compile the local target through Go, run the Go local orchestrator, and invoke language adapters. Authors should see concise run status and diagnostics by default; artifact hashes and generated files should be exposed through verbose or inspect commands.

Open risk: per-step runner process spawning. The same-path discipline means the orchestrator invokes an external runner per step, and Node cold-start per step could make local iteration feel sluggish — which is exactly the pressure that pushes users back to in-memory runners. The orchestrator↔runner protocol must support a warm, long-lived runner process handling multiple descriptors (see roadmap WS-5.4). Per-step spawn is acceptable for M1 bring-up only.

### Language Runtime Adapters

Language runtime adapters are external runner processes with a stable invocation protocol. They are not embedded interpreters inside Go.

Current TypeScript decision:

- the TypeScript SDK package ships the TypeScript step runner,
- Go emits compiled plans and step invocation descriptors, not a second frontend spec,
- the TypeScript runner consumes those invocation descriptors and executes module/export symbols from source packages,
- the same TypeScript runner shape should work for local execution and containerized Argo steps.

Why this is the current choice:

- frontend SDKs stay responsible for language-specific runtime behavior,
- Go remains strict about portable specs, plans, and target compilation,
- a future second-language SDK (none scheduled) could add its own runner without changing Go orchestration semantics,
- local development can be smooth without creating a separate in-memory execution model.

### Step Invocation Descriptor

V0 uses JSON step invocation descriptors between Go orchestration and language runtime adapters.

Rules:

- the descriptor is defined as a shared schema message,
- JSON is only the v0 transport encoding,
- the logical fields must be compatible with future Cap'n Proto serialization,
- adapters should keep descriptor parsing separate from step execution.

Required fields include plan hash, run ID, node ID, attempt, symbol reference, source package reference, environment reference, input artifacts, output destinations, schema refs, and datastore configuration.

### Runtime Data Artifacts

V0 stores step inputs, step outputs, channel values, and final run results as canonical JSON artifacts.

Why this is the current choice:

- language adapters can validate JSON-shaped values directly,
- portable schemas already lower to JSON Schema,
- the plan can still carry schema refs, artifact refs, and content hashes,
- v0 avoids prematurely mapping arbitrary portable schemas to generated Cap'n Proto data messages.

Future work may add Cap'n Proto value artifacts for compatible schemas.

### Plan Artifact Encoding

Current decision: the compiled `WorkflowPlan` and bundle manifests are Cap'n Proto (`workflow-plan.capnp`, `bundle-manifest.capnp`), with a documented JSON projection for human-diffable fixtures.

Tension to re-evaluate at M1:

- `planHash` is computed over the canonical field tree (RFC 8785), never over Cap'n Proto wire bytes — the binary encoding contributes nothing to artifact identity.
- The JSON projection currently carries all conformance and review weight; the Cap'n Proto artifact has no consumer yet.
- Cap'n Proto is a whole extra toolchain (codegen, CLI, Go bindings) in a v0 that already spans TypeScript and Go.

Checkpoint at M1: if no consumer needs the binary encoding by then (the local orchestrator could read canonical JSON just as well), make canonical JSON the plan artifact and drop the Cap'n Proto toolchain before M4 hardening bakes it in. Keep the decision reversible until then by keeping all plan semantics in the shared field tree, not in encoding-specific behavior.

## Environment Materialization

Open questions:

- Should the Node materializer output a tarball, OCI layer, or backend-specific artifact?
- How should source packages be separated from dependency environments in monorepos?
- Should lockfile strictness be mandatory for local development?
- How should materialization cache hits and misses be exposed?

Current decisions:

- `env.container(...)` emits a lightweight environment manifest even when dependency materialization is skipped.
- Argo v0 executable support accepts `env.container(...)` and rejects `env.node(...)` until Node dependency materialization for Kubernetes exists.

## Argo Compiler

Open questions:

- Should strategic merge use vendored Kubernetes OpenAPI patch metadata, or a small hardcoded merge-key table for v0?
- Should v0 include only a path lock-list for policy authority, or start with a richer policy engine?
- Should runtime `podSpecPatch` passthrough exist as an explicit escape hatch, or should everything resolve at compile time?
- Which Argo CRD schema version is the v0 validation target?
- How much of `compile --explain` should be user-facing in v0 versus just emitted as provenance data?

Current v0 wedge:

- implement only plan, minimal WorkflowTemplate generation, schema validation, `dag-integrity`, `plan-provenance`, `identity-set`, and bundle emission before adding presets, plugins, patches, or system mediation.
- the compiler emits `WorkflowTemplate`; user commands and test harnesses submit actual `Workflow` runs from the template.

## Sidecar Runtime

The sidecar/proxy runtime is future architecture, not v0 required.

Open questions:

- What exact proxy protocol should be used for secrets and egress mediation?
- Should the sidecar own object-store credentials and re-sign requests?
- How should local development emulate the sidecar?
- Which reserved ports and names should be standardized now?
- How should policy violations be surfaced in step logs and run artifacts?

## Datastore

Open questions:

- Should S3-compatible stores be modeled as `datastore.s3({ endpoint })` or separate `r2`, `minio`, etc. helpers?
- Should run metadata be a manifest object only, or should v0 include an append-only event log in object storage?
- What retention and garbage collection semantics should exist for plans, environments, packages, and runs?

Current decisions:

- local development defaults to `~/.massive/store`,
- compiled artifacts are globally content-addressed in the configured datastore,
- run metadata and outputs are namespaced under project keys,
- project identity comes from explicit config or a simply parsed Git `origin` owner/repository name; missing or unsupported identity is a loud configuration error,
- content-addressed keys use full collision-resistant digests with the hash algorithm recorded,
- tests should use explicit temporary local datastore roots,
- Argo execution requires a pod-reachable object-store-compatible datastore such as MinIO, S3, or R2.

## Testing Infrastructure

Current direction:

- Ban mock functions, spies, monkeypatches, and MagicMock-style substitutes.
- Use functional tests against real local filesystem datastores, S3-compatible stores, generated plans, generated Argo bundles, and local Kubernetes clusters.
- Keep Kubernetes execution tests opt-in or separately tagged.

Open questions:

- Should MinIO be mandatory for CI, or should S3-compatible tests be optional in v0?
- Should Argo cluster tests run against OrbStack/minikube locally only, or also in CI through kind?
- Should conformance fixtures be checked in as Cap'n Proto binary plans, text dumps, or both?
- How strict should golden bundle tests be before provenance and deterministic ordering are fully stable?

Current v0 direction:

- use canonical `WorkflowSpec` JSON fixtures at the SDK boundary,
- use Go deterministic JSON dumps of parsed specs for conformance assertions,
- add Cap'n Proto binary `WorkflowPlan` fixtures after Go compilation is implemented.

## Market Positioning

Current stance (July 2026): TypeScript/JavaScript is the only authoring language for now — no second-language SDK is scheduled, though the IR stays language-neutral by design. The near-term wedge leans toward platform teams that want compiled, deterministic, provenance-carrying deploy bundles with verifiable execution contracts; author-facing DX is the adoption surface, not the differentiator.

Open questions:

- Is the first public wedge "portable workflow compiler" or "typed deployable workflow plans"?
- How much should Massive compare itself to Metaflow in docs versus staying TypeScript-native?
- Is Argo the primary production story long-term, or only the first serious target?
- Which future backend should follow Argo: Cloudflare, Vercel, or Temporal?
