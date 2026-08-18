# Massive Workflow Compiler Spec

Status: draft

> This document describes the implemented v0 substrate. The accepted
> next-version product and architecture direction is
> [Workflow Platform v2 Direction](workflow-platform-v2.md). It makes Python the
> priority frontend for v2 and replaces mutable-state/channel proposals
> with explicit immutable dataflow.

Massive is a portable workflow compiler. It is not, at least initially, a durable execution runtime. Authors define typed workflows in Python or TypeScript, the SDK lowers them into a language-neutral workflow specification, and backend compilers render that specification into runnable artifacts such as a local async plan or an Argo deploy bundle.

Python is the primary authoring SDK for v2. It uses Pydantic validation schemas
for values entering a workflow or step and serialization schemas for values
leaving one. Runtimes validate inputs with `TypeAdapter`, serialize outputs with
`dump_python(mode="json")`, then publish canonical immutable JSON artifacts.
This lets Pydantic `Decimal` cross the wire as its serialized string form while
keeping v0's prohibition on floating-point JSON numbers. TypeScript remains a
supported frontend and consumes the same versioned graph and artifact contracts.
The Python emitter emits validation schemas for inputs, but checks each input
type's serialization schema before emission; this rejects a bare `float` input
while retaining Decimal's canonical string transport.

Two audiences anchor the design. Workflow authors get a typed Python SDK and a one-command local loop, with TypeScript also supported. Platform teams get the differentiating layer: compiled, deterministic, provenance-carrying deploy bundles whose execution contracts (environment, resources, secrets, network) are verifiable artifacts rather than runtime configuration. When scope decisions are close, the compile/verify/provenance layer wins.

The core bet is that workflow authoring, graph analysis, execution requirements, environment materialization, and backend-specific deployment can be separated cleanly:

```text
Python or TypeScript authoring source
  -> typed SDK graph model
  -> canonical WorkflowSpec JSON conforming to the shared schema
  -> Go backend compiler
  -> canonical WorkflowPlan JSON + TargetBundleManifest JSON typed by proto schemas
  -> object-store datastore
  -> backend runner
```

All workflows are compiled before running. This includes local development. Local runs may auto-compile by default, but they still run a compiled plan from the local datastore.

There is no separate in-memory local execution contract. A good local developer experience may hide the emit/compile/run steps behind one command, but it must still use the same `WorkflowSpec`, Go compiler, persisted `WorkflowPlan`, datastore artifacts, and language runtime adapter path as deployable targets.

Language SDKs may ship their own runtime adapters. Python and TypeScript SDKs
include local step runners that are also suitable for containerized Argo steps.
Go still owns graph orchestration, target compilation, and artifact validation;
each language runner owns module loading, function invocation, and schema
validation at its step boundary.

## Developer Experience

Workflow authors should not need to understand the full compiler round trip for local development. The CLI should provide a simple command such as:

```sh
massive run workflow.ts
massive run workflow/
```

Workflow entrypoints are explicit exports. For TypeScript v0, a file entrypoint may use a default export or a selected named export such as `workflow.ts#mathWorkflow`. A directory entrypoint should resolve through package configuration rather than recursive inference.

Single-file workflows may run locally without `massive.config.ts`. That zero-config mode creates an ephemeral package config with strict defaults. Deployable profiles require explicit package configuration.

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

For v0, `.proto` schemas define the typed compiled-plan and manifest contracts, and canonical JSON is the artifact body. The SDK emits deterministic `WorkflowSpec` JSON that conforms to the shared schema. The Go compiler writes canonical `WorkflowPlan` JSON artifacts typed by the proto schemas.

## Goals

- Provide a Python-first workflow SDK with a declarative, functional authoring style inspired by `pydantic-graph`, while retaining a supported TypeScript SDK.
- Use native graph libraries instead of reimplementing graph algorithms. TypeScript uses Graphology internally. The IR stays language-neutral by design, but TypeScript/JavaScript is the only planned authoring language for now.
- Keep the canonical compiled workflow representation language-neutral with proto-typed JSON artifacts.
- Treat execution requirements as first-class. A compiled workflow includes graph topology plus environment, resources, secrets, storage, network, and observability contracts.
- Support local async execution and Argo as the first production backend.
- Use object storage for compiled plans, code packages, environments, step outputs, channel values, and run artifacts.
- Keep future runtime sidecar/proxy support reserved in the model without requiring it for v0.

## Non-Goals For V0

- Owning a durable execution runtime.
- Supporting arbitrary cyclic workflows. The portable v0 IR is DAG-only.
- Supporting Cloudflare Workers/Workflows or Vercel Workflows as v0 backends.
- Treating every language runtime as equally mature. Python is the priority SDK;
  TypeScript is supported through the shared IR and artifact contracts.
- Implementing uv, Nix, or container image builders beyond the TypeScript/Node path and container escape hatch.
- Requiring a metadata database or hosted control plane.
- Implementing the full runtime sidecar/proxy security model in v0.

## Core Architecture

Massive has two separate compiler boundary artifacts:

- `WorkflowSpec`: the frontend-emitted, pre-materialization workflow specification. It contains `GraphIR`, schema tables, symbol tables, source package manifests, normalized execution contracts, and normalized environment specs.
- `WorkflowPlan`: the backend-compiled unit. It joins the spec with materialized artifact references, datastore paths, compiler version, validation results, and provenance.
- `DeploymentSpec`: a separately hashed binding from one plan to a target profile. It carries target settings and opaque credential/secret or artifact-store bindings, never raw credentials.

The compiled plan still contains three joined surfaces:

- `GraphIR`: computation topology. For the first v0 wedge this means DAG step nodes, start/end nodes, directed edges, `mergeInputs` fan-in, step symbols, input/output schemas, retry metadata, and artifact edges. Branches, foreach/map, channel declarations, and reducer-backed joins are post-M2 portable-schema work.
- `ExecutionContract`: how the computation is allowed to run. Contracts reference environment specs by content hash and include resources, secrets, network intents, storage requirements, observability, and runtime mediation mode.
- `WorkflowPlan`: the compiled unit that joins `GraphIR`, `ExecutionContract`, symbol tables, materialized artifact references, and provenance.

Backends consume `WorkflowPlan`. They should not need to inspect authoring source.

## V0 Scope

V0 should include:

- Python SDK and supported TypeScript SDK.
- Typed SDK builder models (Pydantic-backed in Python; Graphology-backed in TypeScript).
- Proto schemas for the compiled plan and target manifests.
- Deterministic `WorkflowSpec` JSON emission from Python and TypeScript SDKs.
- Go backend compiler that validates specs and writes canonical JSON `WorkflowPlan` artifacts.
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

- Existing workflow systems: Python authoring, Argo compilation, object-store-backed code packages and artifacts, and runtime network/secret isolation.
- Metaflow: static graph extraction, datastore model, Argo support, and environment plugins such as uv and PyPI.
- Pydantic Graph: builder API with typed state, typed dependencies, typed step input/output, map/spread, join, and reducers.
- Graphology and NetworkX: mature graph libraries that should own graph operations in their respective languages.
- Argo Workflows: the first non-local backend target.
- Cloudflare Workflows, Vercel Workflows, Temporal, Dagster, Inngest, and Mastra: adjacent systems that make the market positioning clearer.
