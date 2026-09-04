# Portable Materialization Contract

Status: implemented for externally supplied immutable containers; offline.

## Boundaries

`WorkflowSpec` remains the frontend JSON contract. The pure Go compiler emits the
protobuf-owned **WorkflowPlan schema v1**. `EnvironmentRequirement` uses a typed
oneof for container or Node requirements, preserving requirements rather than
recording an environment as "skipped". There are no realization outputs in that
message. Argo accepts only container requirements in this milestone.

Old plan and deployment artifacts are rejected, not adapted. Rebuild them with
the matching compiler/runtime version. No backward-compatibility branch is kept.

`conformance/schema/materialization.proto` adds two separate, protobuf-owned
contracts:

- **MaterializationSpec** selects an existing immutable image/platform for each
  compiled environment reference and records each source package's archive digest.
  Its `existingContainer` oneof is the only supported mode. The selection must
  exactly match the workflow's pinned container requirement; it cannot override
  the workflow image, platform, command, or working directory.
- **MaterializationManifest** records the independently checked inputs,
  per-environment realization identities, and materializer name/version.
  A caller-supplied manifest is never accepted as proof: compilation derives it
  again from the spec, plan, and actual source bytes.

Source package hashes authenticate normalized file paths and their bytes.
Archive hashes authenticate the exact tar bytes. Both must match; an archive
with different metadata can retain source identity but changes materialization
identity. Missing, extra, corrupt, unsafe, or mismatched source archives fail
before a bundle is produced.

**Image verification is `PINNED_REFERENCE_ONLY`.** The offline materializer checks
the immutable reference and declared platform, not registry availability,
registry authorization, the image's actual architecture, installed dependencies,
or runner compatibility. It does not build or pull an image. Kubernetes still
resolves/pulls it when running a pod. This manifest is not a reproducibility
attestation or authorization grant.

## Local and receiving-side compilation

`controlplane.PrepareArgo` evaluates no code: given an already-emitted frontend
result, it packages real source files and derives portable materialization inputs.
It compiles locally to derive requirement references, but sends no trusted plan.

`controlplane.CompileArgo` accepts only:

1. WorkflowSpec bytes;
2. MaterializationSpec bytes;
3. source archive bytes keyed by source-package identity;
4. a deployment profile.

It revalidates/recompiles the workflow, resolves materialization, constructs the
deployment binding, and lowers to Argo. It has no checkout path, interpreter,
output directory, Kubernetes client, or image builder. `massive build` composes
these same operations and then writes the result to disk.

A future `wf deploy`/`massive deploy` transport can upload the source bytes,
send the two specs plus a profile selection, and invoke this same boundary on a
server. The server must authenticate the request, authorize the chosen profile
and bindings, and bound uploads before calling the compiler. A separate
publisher applies the generated WorkflowTemplate. Neither transport nor
publication is implemented here.

The existing embedded ConfigMap transport and size limit remain unchanged.
Object-store upload/retrieval is a later adapter; local filesystem paths are not
part of either materialization message.

## Identity and versions

Materialization schema version 0 fixes the canonical-json-v0 hash recipes:

- `specHash`: canonical protobuf JSON with only root `specHash` absent.
- `manifestHash`: canonical protobuf JSON with only root `manifestHash` absent.
- `realizationHash`: canonical JSON of `recipe: "existing-container"`,
  `recipeVersion: 1`, image, platform, materializer name/version, and
  `verification: "PINNED_REFERENCE_ONLY"`.

Environment selections sort by environment reference, source entries by package
hash, using UTF-16 code-unit order. Scalar presence is explicit, unknown fields
fail, and JSON uses protobuf's lowerCamelCase projection. Whitespace does not
affect identity. Duplicate selections are invalid; the compiler deduplicates
identical frontend requirement aliases before producing the plan.

The realization hash excludes source, workflow, execution command, namespace,
service account, and secret bindings. Unrelated workflows can therefore identify
the same image/platform realization. The manifest includes the source inputs;
deployment identity binds the plan and full manifest.

**DeploymentSpec v1 requires `materializationHash`.** Its hash continues to cover
all fields except root `deploymentHash`, so a different realization cannot be
silently substituted. There is one target-compilation path: materialization inputs
are mandatory, source archives are verified, and the resulting manifest must match
the deployment binding. Runtime verifies source again before execution.

The plan environment reference hashes the canonical protobuf JSON requirement
with root `envRef` absent. It includes its selected variant (`container` or
`node`) and invocation requirements. The separate realization identity excludes
invocation details so the same dependency environment remains reusable.

New public builds emit `materialization-spec.json` and
`materialization-manifest.json`, both covered by the target bundle manifest.
The standalone compiler can rebuild those artifacts without a checkout:

```sh
massive-compiler bundle-argo \
  --plan .massive/argo/massive-plan.json \
  --deployment .massive/argo/deployment-spec.json \
  --materialization .massive/argo/materialization-spec.json \
  --runtime-assets .massive/argo/runtime-assets \
  --out .massive/rebuilt
```

No automatic installation, dependency preflight, registry verification, server
build workers, provider registry, or local Docker execution is included.
