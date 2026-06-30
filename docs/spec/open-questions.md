# Open Questions

Status: draft

This document tracks decisions that are intentionally unsettled or likely to change after building real workflows.

## Tentative Decisions

### Global State Channel Declaration

V0 channels are globally declared in `stateSchema(...)`.

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

## Environment Materialization

Open questions:

- Should the Node materializer output a tarball, OCI layer, or backend-specific artifact?
- How should source packages be separated from dependency environments in monorepos?
- Should lockfile strictness be mandatory for local development?
- How should materialization cache hits and misses be exposed?
- Should `env.container(...)` skip materialization entirely or still emit a manifest artifact?

## Argo Compiler

Open questions:

- Should strategic merge use vendored Kubernetes OpenAPI patch metadata, or a small hardcoded merge-key table for v0?
- Should v0 include only a path lock-list for policy authority, or start with a richer policy engine?
- Should runtime `podSpecPatch` passthrough exist as an explicit escape hatch, or should everything resolve at compile time?
- Which Argo CRD schema version is the v0 validation target?
- How much of `compile --explain` should be user-facing in v0 versus just emitted as provenance data?

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

## Market Positioning

Open questions:

- Is the first public wedge "portable workflow compiler" or "typed deployable workflow plans"?
- How much should Massive compare itself to Metaflow in docs versus staying TypeScript-native?
- Is Argo the primary production story long-term, or only the first serious target?
- Which future backend should follow Argo: Cloudflare, Vercel, Temporal, or a Python SDK?
