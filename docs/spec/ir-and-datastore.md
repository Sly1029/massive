# IR And Datastore

Status: draft

Massive's canonical compiled artifact is a Cap'n Proto `WorkflowPlan`.

The TypeScript SDK is not the source of truth. It is the first authoring frontend. Future Python, Rust, or Go SDKs should emit the same Cap'n Proto schema.

## WorkflowPlan

The compiled plan joins three surfaces:

- `GraphIR`: portable graph topology and typed dataflow.
- `ExecutionContract`: portable execution requirements.
- backend/materialization references: code packages, environment artifacts, datastore paths, target metadata, provenance, and compiler version.

The plan should be content-addressed and hashable. Same source inputs, compiler version, target config, patches, environment inputs, and materializer settings should produce byte-identical plan artifacts.

## GraphIR

The v0 graph IR is DAG-only.

It includes:

- workflow name and schema version,
- graph input schema and final output schema,
- step nodes,
- start/end nodes,
- directed edges,
- branches,
- foreach/map nodes,
- join/reducer references,
- step input/output schema references,
- state channel declarations,
- stable symbol references for executable code,
- retry metadata,
- artifact dependencies.

It does not include arbitrary closures.

## ExecutionContract

Execution contracts describe how graph nodes are allowed to run.

They include:

- environment specs,
- resource requests/limits,
- secrets,
- network/egress intents,
- storage/artifact requirements,
- observability requirements,
- runtime mediation mode.

Contracts are merged from workflow defaults and step overrides. Effective contracts are deduped in the compiled plan by content hash.

Resource and network differences do not make two environments different. Environment materialization keys should include only environment-relevant inputs.

## Object Store Datastore

The v0 datastore model is object-store-first with a local filesystem implementation for testing and local development.

Supported v0 store classes:

```ts
datastore.local({
  path: ".massive/store",
});

datastore.s3({
  bucket: "my-workflows",
  prefix: "massive/",
  region: "us-west-2",
});
```

S3-compatible stores, such as R2 or MinIO, should be supportable through endpoint configuration.

## No Metadata Database In V0

V0 does not require a database or hosted metadata service. The object store holds manifests and artifacts. A richer metadata service can be added later without changing the compiled-plan invariant.

## Storage Layout

Indicative layout:

```text
/envs/<env-key>/manifest.capnp
/envs/<env-key>/runtime.tar.zst
/packages/<package-key>/source.tar.zst
/plans/<plan-key>/workflow.capnp
/plans/<plan-key>/provenance.capnp
/targets/<plan-key>/<target>/bundle-manifest.capnp
/runs/<run-id>/steps/<step-id>/<attempt>/output.capnp
/runs/<run-id>/channels/<channel-name>/value.capnp
```

The exact path format should be specified in the Cap'n Proto manifest, not hardcoded by backend runners.

## Compile Before Run

Every run consumes a compiled plan. Local development may auto-compile, but the local runner still loads `WorkflowPlan` from the local datastore.

```text
massive run --local
  -> compile if source changed
  -> write plan to .massive/store/plans/<plan-key>/workflow.capnp
  -> execute compiled plan
```

Production targets should require explicit compile/deploy steps.

## Plan Hash

The plan hash should cover:

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

