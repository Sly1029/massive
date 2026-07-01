# IR And Datastore

Status: draft

Massive's canonical compiled artifact is a Cap'n Proto `WorkflowPlan`. The v0 schema lives at [`../../conformance/schema/workflow-plan.capnp`](../../conformance/schema/workflow-plan.capnp), with target bundle output described by [`../../conformance/schema/bundle-manifest.capnp`](../../conformance/schema/bundle-manifest.capnp).

The TypeScript SDK is not the source of truth. It is the first authoring frontend. Future Python, Rust, or Go SDKs should emit the same shared schema.

For v0, frontend SDKs emit deterministic `WorkflowSpec` JSON that conforms to the shared schema. The Go compiler is the first required Cap'n Proto binary writer. This avoids making every SDK responsible for byte-stable Cap'n Proto encoding while the schema is still moving.

## WorkflowSpec

`WorkflowSpec` is the frontend-emitted, pre-materialization artifact. It is not a runnable plan and should not be accepted directly by backend runners.

The v0 machine contract is the draft 2020-12 JSON Schema at [`../../conformance/schema/workflow-spec.schema.json`](../../conformance/schema/workflow-spec.schema.json). Conformance fixtures live under [`../../conformance/fixtures/specs`](../../conformance/fixtures/specs). The schema validates portable artifact shape; the Go compiler still owns cross-reference validation, DAG validation, target compatibility, and strict type/contract checks.

It includes:

- `GraphIR`: portable graph topology and typed dataflow.
- schema table: portable JSON Schema values keyed by content hash.
- symbol table: stable executable references that point at source package IDs.
- source package table: one or more source package manifests keyed by source package ID and content hash.
- environment table: environment specs keyed by environment spec hash.
- execution contract table: effective workflow and step contracts keyed by content hash.
- target requests: local, Argo, and future backend planning inputs.

Execution contracts reference environment specs by hash. Environment specs must not be duplicated inside each contract. This keeps environment materialization deduped by the effective dependency environment rather than by resource, secret, or network settings.

The SDK resolves workflow defaults and step overrides before emitting the spec. Graph nodes reference effective execution contracts by content hash. The spec may retain workflow-level defaults and override provenance for explainability, but Go target compilers consume the effective `contractRef` on each node rather than re-running frontend merge semantics.

Target requests are part of the `WorkflowSpec` rather than only CLI arguments. This makes deployment intent portable and content-addressed with the workflow. The Go compiler may compile one requested target, a subset of requested targets, or all requested targets, but it must not silently invent target configuration outside the spec. CLI flags may select requested targets and output locations; they should not mutate target semantics.

Targets are allowed to support different feature subsets. The Go compiler owns target compatibility checks and should produce explicit diagnostics when a target cannot represent a requested graph shape, execution contract, environment, secret mode, network intent, or storage requirement. Unsupported target features are compile-time errors unless the target has a documented degraded mode and the spec explicitly allows that degradation.

`WorkflowSpec` is content-addressed by a `specHash` over its canonical field tree. The hash is not computed over JSON whitespace and is not computed over Cap'n Proto wire bytes.

The emitting SDK is responsible for language-specific validation before it writes a `WorkflowSpec`. For TypeScript, that includes resolving module/export symbols against the source package and checking that the authoring-time step declarations can be lowered into portable schemas and contracts. A future Python SDK must perform the equivalent Python-specific checks before emitting the same portable spec shape. The Go compiler validates the emitted spec as a portable artifact: schema conformance, graph integrity, contract references, target requests, datastore references, and backend-specific invariants. It should not need to understand each frontend language's import or reflection rules.

## Source Packages

Source packages are separate content-addressed artifacts. A `WorkflowSpec` references them by package ID and content hash. Symbols point at a package ID plus language-specific entrypoint metadata.

Example:

```text
sourcePackages:
  ts-main:
    language: typescript
    packageHash: sha256:...
    artifact: packages/<package-key>/source.tar.zst

symbols:
  math/double:
    packageId: ts-main
    module: ./workflow.ts
    export: double
```

V0 TypeScript workflows will usually have one source package, but the schema must not assume one package per workflow. This keeps future Python support, mixed-language workflows, reusable packages, and monorepo package boundaries possible without changing the graph IR.

Source packages are not environments. They identify executable source content. Environment specs identify dependency/runtime requirements. Environment materialization may read source package metadata when calculating package or workspace hashes, but dependency environment keys are still derived from environment-relevant inputs, not from resource limits, secret bindings, or target-specific scheduling settings.

Package roots are explicit. A file entrypoint such as `massive run workflow.ts` may infer the package root from the nearest workflow/package config. A directory entrypoint such as `massive run workflow/` should read that directory's workflow/package config. The package root controls:

- workflow entrypoint export,
- included source files,
- local utility modules,
- package manager manifests and lockfiles,
- environment defaults,
- target requests.

The compiler should avoid broad implicit packaging. For v0, packaging should be driven by explicit include patterns plus required manifests and lockfiles. Future SDKs may add dependency-graph-assisted suggestions, but the emitted source package manifest must list exact files and content hashes.

For TypeScript v0, the package config file is `massive.config.ts`.

Example:

```ts
import { defineWorkflowPackage, env, target } from "@massive/sdk";

export default defineWorkflowPackage({
  projectId: "acme/security-workflows",
  entrypoint: "./src/workflow.ts#default",
  include: ["src/**/*.ts", "package.json", "pnpm-lock.yaml"],
  environment: env.node({
    version: "22.12.0",
    packageManager: "pnpm",
    lockfile: "pnpm-lock.yaml",
  }),
  targets: [
    target.local({}),
    target.argo({ namespace: "workflows", serviceAccountName: "massive-runner" }),
  ],
});
```

Future language SDKs may use native package configuration surfaces, such as `pyproject.toml` for Python, as long as they emit the same portable `WorkflowSpec` structure.

Zero-config single-file TypeScript workflows are allowed for local development only. If no `massive.config.ts` is found, the CLI may synthesize an ephemeral package config:

```text
entrypoint: <workflow-file>#default
packageRoot: dirname(<workflow-file>)
include:
  - <workflow-file>
  - package.json if present
  - recognized lockfile if present
targets:
  - local
```

If the file has multiple exported workflows, the CLI requires an explicit selector such as `workflow.ts#name`. Zero-config specs must not request deployable targets such as Argo.

## WorkflowPlan

The compiled plan joins three surfaces:

- `GraphIR`: portable graph topology and typed dataflow.
- `ExecutionContract`: portable execution requirements.
- backend/materialization references: code packages, environment artifacts, datastore paths, target metadata, provenance, and compiler version.

The Go compiler consumes a `WorkflowSpec`, validates it, resolves target inputs, materializes or records environments, writes package and datastore references, and emits a Cap'n Proto `WorkflowPlan`.

The plan should be content-addressed and hashable. Same source inputs, compiler version, target config, patches, environment inputs, and materializer settings should produce the same canonical plan hash. Hashes are computed over canonical field trees, not raw Cap'n Proto segment bytes, because valid Cap'n Proto messages can have different byte layouts.

Human-diffable fixtures should use the deterministic JSON projection documented in [`../../conformance/schema/workflow-plan-json-projection.md`](../../conformance/schema/workflow-plan-json-projection.md). That projection is a conformance aid only; runners consume the persisted Cap'n Proto plan and target manifests.

Canonical plans and target bundle manifests must not include wall-clock timestamps. Compiler identity, compiler version, source/spec hashes, materialized artifact refs, and validation results belong in canonical provenance; compile time and bundle emission time are side metadata if they are needed later.

## GraphIR

The v0 `WorkflowSpec` graph IR is intentionally narrow: DAG step nodes, start/end nodes, directed edges, and explicit `mergeInputs` fan-in.

It includes:

- workflow name and schema version,
- graph input schema and final output schema,
- step nodes,
- start/end nodes,
- directed edges,
- merge fan-in through `mergeInputs`,
- step input/output schema references,
- stable symbol references for executable code,
- retry metadata,
- artifact dependencies.

It does not include arbitrary closures. Channels, branch nodes, foreach/map nodes, joins/reducers, and channel publish/read declarations are post-M2 `WorkflowSpec` features. The authoring model may describe those user-facing APIs before they are admitted to the portable v0 schema; frontend SDKs must not emit them into `schemaVersion: 0`.

## ExecutionContract

Execution contracts describe how graph nodes are allowed to run.

They include:

- environment spec hash references,
- resource requests/limits,
- secrets,
- network/egress intents,
- storage/artifact requirements,
- observability requirements,
- runtime mediation mode.

Contracts are merged from workflow defaults and step overrides. Effective contracts are deduped in the compiled plan by content hash.

Frontend SDKs perform that merge before emitting `WorkflowSpec`. Go validates that every executable graph node has a `contractRef`, that every referenced contract exists, and that the selected target can represent each effective contract.

Resource and network differences do not make two environments different. Environment materialization keys should include only environment-relevant inputs.

## Object Store Datastore

The v0 datastore model is object-store-first with a local filesystem implementation for testing and local development.

Supported v0 store classes:

```ts
datastore.local({
  path: "~/.massive/store",
});

datastore.s3({
  bucket: "my-workflows",
  prefix: "massive/",
  region: "us-west-2",
});
```

S3-compatible stores, such as R2 or MinIO, should be supportable through endpoint configuration.

The default local datastore root is user-level: `~/.massive/store`. This avoids creating project-local `.massive` directories during normal development. Project-local datastore paths are still useful for tests, temporary isolation, and explicit user configuration.

## No Metadata Database In V0

V0 does not require a database or hosted metadata service. The object store holds manifests and artifacts. A richer metadata service can be added later without changing the compiled-plan invariant.

## Storage Layout

Indicative layout:

```text
/blobs/sha256/<digest>
/specs/<spec-key>/workflow-spec.json
/envs/<env-key>/manifest.capnp
/envs/<env-key>/runtime.tar.zst
/packages/<package-key>/source-manifest.capnp
/packages/<package-key>/source.tar.zst
/plans/<plan-key>/workflow.capnp
/plans/<plan-key>/provenance.capnp
/targets/<plan-key>/<target>/bundle-manifest.capnp
/projects/<project-key>/runs/<run-id>/steps/<step-id>/<attempt>/output.json
/projects/<project-key>/runs/<run-id>/channels/<channel-name>/value.json
```

The exact path format should be specified in the Cap'n Proto manifest, not hardcoded by backend runners.

Compiled artifacts are globally content-addressed inside the configured datastore. Run metadata and run outputs are namespaced by project key so local run history stays organized without losing deduplication for source packages, specs, plans, and environment artifacts.

Project keys come from explicit project identity. `massive.config.ts` may set a project ID. If it does not, v0 should derive the project identity from the Git `origin` remote owner/repository name, such as `user/repo`. V0 only needs to handle common GitHub- and GitLab-style SSH or HTTPS remotes. If that simple derivation fails, commands that need project-scoped run metadata should fail loudly and ask the user to configure a project ID.

The project key stored in the datastore should be a normalized and hashed representation of that project identity, not raw arbitrary text. It should not include transient run data.

Content-addressed keys should use collision-resistant full digests, such as `sha256:<hex>`, and manifests should record the hash algorithm. UI and CLI output may display shortened prefixes, but datastore keys and manifest references should use full hashes.

## Compile Before Run

Every run consumes a compiled plan. Local development may auto-compile, but the local runner still loads `WorkflowPlan` from the local datastore.

```text
massive run --local
  -> emit WorkflowSpec if source changed
  -> Go compile if spec changed
  -> write plan to ~/.massive/store/plans/<plan-key>/workflow.capnp
  -> execute compiled plan
```

Production targets should require explicit compile/deploy steps.

Local execution is not allowed to bypass the compiler by using TypeScript builder state, in-memory runtime registries, or frontend-only schema registries. The local target is a real target compiled by Go. It reads the same persisted plan and datastore artifacts as other runners, then invokes the appropriate language runtime adapter for each step.

The CLI may hide this sequence behind `massive run workflow.ts` or `massive run workflow/`. That command is orchestration over the same artifacts, not a separate execution mode. It should cache by source package hash, spec hash, plan hash, and target config in the configured datastore so repeated local runs are fast without weakening the compiler boundary.

The Go compiler does not emit a second frontend spec for language runners. It emits `WorkflowPlan` and target/run manifests. Language adapters consume step invocation descriptors derived from that compiled plan: plan hash, run ID, node ID, input artifact references, output artifact destinations, schema refs, symbol refs, source package refs, and environment refs.

For TypeScript v0, the adapter should live with the TypeScript SDK package. That keeps TypeScript import rules, module loading, and author-facing diagnostics close to the authoring surface while preserving Go as the strict compiler for portable inputs and target behavior.

## Step Invocation Descriptor

The step invocation descriptor is the narrow runtime protocol between Go orchestration and language adapters.

V0 serializes this descriptor as JSON for ease of implementation in TypeScript and future Python. The descriptor must still be defined as a shared schema message, not as an adapter-private JSON shape, so it can later be serialized as Cap'n Proto without changing its semantics.

It includes:

- schema version,
- plan hash,
- run ID,
- node ID,
- attempt number,
- symbol reference,
- source package reference,
- environment reference,
- input artifact references and schema refs,
- output artifact destinations and schema refs,
- channel read/write artifact refs when applicable,
- datastore configuration needed by the adapter.

Example:

```json
{
  "schemaVersion": 0,
  "planHash": "sha256:...",
  "runId": "run-...",
  "nodeId": "double",
  "attempt": 1,
  "symbol": {
    "packageId": "ts-main",
    "module": "./workflow.ts",
    "export": "double"
  },
  "sourcePackage": {
    "artifact": "packages/.../source.tar.zst",
    "hash": "sha256:..."
  },
  "input": {
    "artifact": "runs/.../inputs/double.json",
    "schema": "sha256:..."
  },
  "output": {
    "artifact": "runs/.../steps/double/attempts/1/output.json",
    "schema": "sha256:..."
  }
}
```

Future Cap'n Proto transport should reuse the same logical fields. Runtime adapters should isolate descriptor parsing from step execution so the JSON transport can be replaced without rewriting symbol loading or step invocation logic.

## Runtime Data Artifacts

V0 step inputs, step outputs, channel values, and final run results are stored as canonical JSON data artifacts.

Each artifact records:

- schema hash,
- artifact hash,
- content type,
- datastore key,
- producing run, node, and attempt when applicable.

The plan, environment manifests, source manifests, and bundle manifests may use Cap'n Proto where Go writes them. User data values remain JSON in v0 because portable authoring schemas lower to JSON Schema and language adapters can validate JSON-shaped values directly.

This is a runtime data decision, not a compiler boundary retreat. The compiled plan still records the schema refs, artifact refs, and hashes needed to validate and reproduce execution. Future work may add Cap'n Proto value artifacts for compatible schemas, but v0 does not require mapping arbitrary portable JSON Schema values into generated Cap'n Proto data schemas.

## Plan Hash

The plan hash should cover:

- spec hash,
- GraphIR,
- ExecutionContract,
- symbol table,
- target config,
- patches,
- compiler version,
- environment materialization references,
- datastore manifest references,
- mediation provider identity.

The plan hash should be annotated into generated deploy artifacts. Backend validation should reject runs where the plan hash annotation is missing or mismatched.
