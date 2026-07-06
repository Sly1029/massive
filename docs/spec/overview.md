# Massive Workflow Compiler Spec

Status: draft

Massive is a portable workflow compiler. It is not, at least initially, a durable execution runtime. Authors define typed workflows in TypeScript, the SDK lowers them into a language-neutral workflow specification, and backend compilers render that specification into runnable artifacts such as a local async plan or an Argo deploy bundle.

Two audiences anchor the design. Workflow authors get a typed TypeScript SDK and a one-command local loop. Platform teams get the differentiating layer: compiled, deterministic, provenance-carrying deploy bundles whose execution contracts (environment, resources, secrets, network) are verifiable artifacts rather than runtime configuration. When scope decisions are close, the compile/verify/provenance layer wins.

The core bet is that workflow authoring, graph analysis, execution requirements, environment materialization, and backend-specific deployment can be separated cleanly:

```text
TypeScript authoring source
  -> Graphology-backed graph model
  -> canonical WorkflowSpec JSON conforming to the shared schema
  -> Go backend compiler
  -> Cap'n Proto WorkflowPlan + TargetBundleManifest
  -> object-store datastore
  -> backend runner
```

All workflows are compiled before running. This includes local development. Local runs may auto-compile by default, but they still run a compiled plan from the local datastore.

There is no separate in-memory local execution contract. A good local developer experience may hide the emit/compile/run steps behind one command, but it must still use the same `WorkflowSpec`, Go compiler, persisted `WorkflowPlan`, datastore artifacts, and language runtime adapter path as deployable targets.

Language SDKs may ship their own runtime adapters. For TypeScript, the SDK should include the local step runner used by the local target and by containerized Argo steps. Go still owns graph orchestration, target compilation, and artifact validation; the TypeScript runner owns TypeScript module loading, function invocation, and schema validation at the step boundary.

## Developer Experience

Workflow authors should not need to understand the full compiler round trip for local development. The CLI should provide a simple command such as:

```sh
massive run workflow.ts
massive run workflow/
```

Workflow entrypoints are explicit exports. For TypeScript v0, a file entrypoint may use a default export or a selected named export such as `workflow.ts#mathWorkflow`. A directory entrypoint should resolve through package configuration rather than recursive inference.

Single-file workflows may run locally without `massive.config.ts`. That zero-config mode creates an ephemeral package config with strict defaults and `local` as the only target. Deployable targets require explicit package configuration.

Internally, that command still executes the full modular path:

```text
discover workflow entrypoint
  -> invoke the appropriate SDK emitter
  -> write WorkflowSpec to the local datastore
  -> invoke the Go compiler for the local target
  -> write WorkflowPlan and local run manifest
  -> run the Go local orchestrator
  -> invoke language runtime adapters for each step
  -> write run artifacts to the datastore
```

The default output should focus on author-facing status, diagnostics, and final result locations. Artifact paths, hashes, generated specs, and compiled plans should be available through verbose flags or explicit inspect commands, not required knowledge for the common local path.

For v0, the Cap'n Proto schema is the shared contract, but the TypeScript SDK does not need to emit Cap'n Proto binary bytes directly. The SDK emits deterministic `WorkflowSpec` JSON that conforms to the shared schema. The Go compiler is the first writer of canonical Cap'n Proto `WorkflowPlan` artifacts.

## Goals

- Provide a TypeScript-first workflow SDK with a declarative, functional authoring style inspired by `pydantic-graph`.
- Use native graph libraries instead of reimplementing graph algorithms. TypeScript uses Graphology internally. The IR stays language-neutral by design, but TypeScript/JavaScript is the only planned authoring language for now.
- Keep the canonical compiled workflow representation language-neutral with Cap'n Proto.
- Treat execution requirements as first-class. A compiled workflow includes graph topology plus environment, resources, secrets, storage, network, and observability contracts.
- Support local async execution and Argo as the first production backend.
- Use object storage for compiled plans, code packages, environments, step outputs, channel values, and run artifacts.
- Keep future runtime sidecar/proxy support reserved in the model without requiring it for v0.

## Non-Goals For V0

- Owning a durable execution runtime.
- Supporting arbitrary cyclic workflows. The portable v0 IR is DAG-only.
- Supporting Cloudflare Workers/Workflows or Vercel Workflows as v0 backends.
- Supporting authoring languages other than TypeScript/JavaScript. The IR remains language-neutral by design, but no second-language SDK is scheduled.
- Implementing uv, Nix, or container image builders beyond the TypeScript/Node path and container escape hatch.
- Requiring a metadata database or hosted control plane.
- Implementing the full runtime sidecar/proxy security model in v0.

## Core Architecture

Massive has two separate compiler boundary artifacts:

- `WorkflowSpec`: the frontend-emitted, pre-materialization workflow specification. It contains `GraphIR`, schema tables, symbol tables, source package manifests, normalized execution contracts, normalized environment specs, and target requests.
- `WorkflowPlan`: the backend-compiled unit. It joins the spec with materialized artifact references, datastore paths, backend target metadata, compiler version, validation results, and provenance.

The compiled plan still contains three joined surfaces:

- `GraphIR`: computation topology. For the first v0 wedge this means DAG step nodes, start/end nodes, directed edges, `mergeInputs` fan-in, step symbols, input/output schemas, retry metadata, and artifact edges. Branches, foreach/map, channel declarations, and reducer-backed joins are post-M2 portable-schema work.
- `ExecutionContract`: how the computation is allowed to run. Contracts reference environment specs by content hash and include resources, secrets, network intents, storage requirements, observability, and runtime mediation mode.
- `WorkflowPlan`: the compiled unit that joins `GraphIR`, `ExecutionContract`, symbol tables, materialized artifact references, backend target metadata, and provenance.

Backends consume `WorkflowPlan`. They should not need to inspect TypeScript source.

## V0 Scope

V0 should include:

- TypeScript SDK.
- Graphology-backed builder model.
- Cap'n Proto schema for the shared IR and compiled plan.
- Deterministic `WorkflowSpec` JSON emission from the TypeScript SDK.
- Go backend compiler that validates specs and writes Cap'n Proto `WorkflowPlan` artifacts.
- Local filesystem datastore.
- S3-compatible object-store datastore.
- Local async runner.
- Argo compiler that emits a deploy bundle.
- Node environment materialization from package manager lockfiles.
- Container environment escape hatch.
- Basic field-level provenance map for generated backend artifacts.
- Functional test harnesses for SDK compilation, datastore behavior, environment materialization, and Argo bundle validation. Mocking APIs are banned.

## References

Design was informed by:

- Semgrep Workflows: Python/Metaflow authoring, Argo compilation, S3-backed code packages and artifacts, runtime network/secret isolation work.
- Metaflow: static graph extraction, datastore model, Argo support, and environment plugins such as uv and PyPI.
- Pydantic Graph: builder API with typed state, typed dependencies, typed step input/output, map/spread, join, and reducers.
- Graphology and NetworkX: mature graph libraries that should own graph operations in their respective languages.
- Argo Workflows: the first non-local backend target.
- Cloudflare Workflows, Vercel Workflows, Temporal, Dagster, Inngest, and Mastra: adjacent systems that make the market positioning clearer.
