# Target Backends

Status: v0 contract

A **target backend** lowers a compiled `WorkflowPlan` into a deployable bundle
for one execution platform. Argo is the first backend; Cloudflare Workers,
Temporal, and Vercel are the motivating future backends
([open-questions.md](open-questions.md)). The backend contract is deliberately
small and **plan-driven**: a backend reads the compiled plan and the requested
target config, and emits artifacts. It never sees the `WorkflowSpec`.

The contract lives in `internal/target` and is backend-neutral — nothing Argo-
or Kubernetes-specific appears there.

## Pipeline position

```text
WorkflowSpec (SDK)
  -> Go compiler: validate + materialize -> WorkflowPlan (canonical JSON)   [internal/plan]
  -> target.Backend.Compile(plan, target request) -> Bundle                 [internal/target/<backend>]
  -> target.WriteBundle -> dist/<target>/<workflow>/                        [internal/target]
```

The plan is the compatibility boundary. Environment materialization results (for
the container escape hatch, the runtime image) are recorded **on the plan**, so a
backend does not reach back into the spec to learn how to run a step.

## The `Backend` interface

```go
type Backend interface {
    Kind() string                       // e.g. "argo"
    Compile(CompileInput) (*Bundle, error)
}

type CompileInput struct {
    Plan     *planpb.WorkflowPlan  // typed compiled plan
    PlanJSON []byte                // its canonical JSON body (datastore-identical bytes)
    PlanHash string                // its self-excluded digest
    Target   spec.Target           // the resolved target request for this kind
}
```

`CompileInput` is everything a backend needs. It carries both the typed plan and
its canonical bytes/hash so a backend never re-marshals or re-hashes the plan
(and cannot accidentally diverge from what was written to the datastore).

## Emitting a bundle

A backend builds its content artifacts and invariant results, then routes them
through `target.BuildBundle`, which assembles the canonical bundle manifest, and
`target.WriteBundle`, which writes the bundle deterministically. Every backend
gets identical manifest construction, content hashing, bundle hashing, and disk
emission for free.

```go
func (b *Backend) Compile(in target.CompileInput) (*target.Bundle, error) {
    // 1. Gate: reject plan features this backend cannot represent, with a clear
    //    diagnostic naming the step/feature.
    // 2. Materialize the backend artifacts from the plan.
    // 3. Validate structure (schema) and enforce invariants; a hard failure
    //    returns an error and emits no bundle.
    artifacts := []target.Artifact{ /* {Path, Bytes, ContentType, Role} ... */ }
    validations := []target.Validation{ /* {Name, Passed, Diagnostic} ... */ }
    return target.BuildBundle(in, artifacts, validations)
}
```

- `Artifact.Path` is bundle-relative (forward slashes, no leading `/`, no
  `.`/`..` segments — enforced on write).
- The manifest (`bundle-manifest.json`, typed by
  [`bundle-manifest.proto`](../../conformance/schema/bundle-manifest.proto)) is
  reserved and written by `target`; a backend must not emit it.
- The bundle hash covers the plan hash, target request, compiler identity, and
  the ordered `(path, sha256)` list of content artifacts — so identical inputs
  yield a byte-identical bundle. It excludes validation outcomes (diagnostics,
  not identity).

## Registration

Backends are registered explicitly — no `init()` side effects:

```go
registry := target.NewRegistry()
registry.Register(argo.New())
bundle, err := registry.Compile("argo", input)   // unknown kind -> UnknownTargetError listing supported kinds
```

## Determinism requirements

Every backend must be deterministic ([argo-backend.md](argo-backend.md#determinism)):
stable ordering, canonical serialization, **no wall-clock timestamps** in emitted
artifacts, sorted keys where possible. Add a byte-stability test (compile twice,
compare) and golden fixtures under `conformance/fixtures/bundles/<case>/<target>/`.

## What is *not* a bundle backend

The `local` target is orchestrator-driven: it executes a plan directly against
the datastore ([internal/orchestrator](../../internal/orchestrator)) and emits no
deploy bundle. It is intentionally **not** registered as a `Backend`.

## Worked example: a new `temporal` backend

1. `internal/target/temporal`: implement `Backend` with `Kind() == "temporal"`.
2. In `Compile`, gate plan features Temporal cannot represent; materialize a
   Temporal workflow/worker definition from `input.Plan`; validate it; enforce
   the invariants that apply.
3. Return `target.BuildBundle(input, artifacts, validations)`.
4. Register it (`registry.Register(temporal.New())`) and add a `--target temporal`
   path — `cmd/massive-compiler compile-target` already resolves the target
   request from the spec and writes to `dist/temporal/<workflow>/`.
5. Add golden fixtures + a byte-stability test; wire them into
   `pnpm check:conformance`.

No change to `internal/target`, the plan schema, or the SDK is required.
