# Argo Backend

Status: executable DAGs, decisions/selects, and finite maps

The current implementation emits an executable DAG and validates its
`WorkflowTemplate` offline against Argo Workflows v3.7.16. The bundle mounts an
immutable ConfigMap containing the verified plan and, for the `embedded-v0`
runtime transport, its source archives; `object-store-v0` pods fetch source
archives from the shared datastore instead. Every pod runs one isolated
proto-described step through the same language runner used by local execution. Argo parameters carry
[value parameters](#value-parameters) between tasks: small canonical JSON
values inline, larger ones as datastore references. The template is annotated
`massive.dev/execution-status: executable-dag`.

Argo is the first non-local backend. The Argo compiler emits a deploy bundle, not only a single WorkflowTemplate.

The Argo backend proves the main product thesis: a portable typed graph plus execution contract can lower to real infrastructure with pods, resources, object storage, environment artifacts, network policy, secret binding, observability, and generated deployment artifacts.

## Deploy Bundle

An Argo compile target emits a deployable template bundle, not an ad hoc run object. The primary Kubernetes artifact is a `WorkflowTemplate`.

```text
dist/argo/<workflow-name>/
  workflow-template.yaml
  workflow-template.json      # canonical machine-readable projection
  runtime-configmap.json
  runtime-assets/
    source-sha256-<digest>.tar  # verified source package (embedded or published)
  massive-plan.json
  bundle-manifest.json
  deployment-spec.json
  materialization-spec.json
  materialization-manifest.json
  workflow-spec.json
```

The exact files vary by target config. The bundle manifest is canonical and
records the plan, deployment, bundle, and emitted-file body hashes. The current
command is:

```sh
massive build workflow.py \
  --output dist/argo \
  --namespace workflows \
  --service-account massive-runner \
  --artifact-store massive-artifacts \
  --runtime-transport embedded-v0   # or object-store-v0
```

The lower-level `massive-compiler bundle-argo` command additionally requires a
`--runtime-assets` directory and `--materialization materialization-spec.json`.
Plan and deployment schema v1 are required; older artifacts must be rebuilt. See the
[portable materialization contract](materialization-contract.md).
`massive build` is the public path: it verifies
the authoring source manifest, creates deterministic archives, and binds them
to the exact canonical plan. Verified source archives are exposed as standalone
runtime assets and recorded in `bundle-manifest.json` for both transports, so
the pack can be inspected or published without decoding Kubernetes resources.

## Runtime Transport

Runtime packing and runtime transport are separate concerns. Compilation
produces one immutable pack containing the canonical plan, schemas, and
content-addressed source archives. The deployment's required
`target.runtimeTransport` binding selects how pods receive that pack. It is
part of `DeploymentSpec` and therefore of the deployment hash and runtime
ConfigMap name; the plan hash does not change. `bundle-manifest.json` records
the same value as `runtimeTransport`, and the WorkflowTemplate carries it in
the `massive.dev/runtime-transport` annotation.

- `embedded-v0` (the `massive build` default) stores the plan and archives in
  one immutable ConfigMap. Kubernetes limits a ConfigMap to 1 MiB and
  `binaryData` is base64-encoded, so the decoded plan plus archives must not
  exceed 700 KiB. A larger pack fails the build with a diagnostic naming
  `--runtime-transport object-store-v0`; files are never dropped.
- `object-store-v0` mounts only the plan (still bounded by 700 KiB). Each
  runner template pins every source package as
  `--source-archive=<package-hash>=<archive-digest>`. The language runner
  streams `packages/sha256-<package>/archives/sha256-<archive>.tar` from the
  bound datastore to private scratch and rejects it unless its SHA-256 equals
  the pinned digest, before reading any entry or importing code. Python attempts
  first fetch and verify the same object for dependency preflight. Like the
  runner, the pod checks the object's declared size against the largest valid
  archive before reading it, then streams it to scratch through its digest
  and extracts from disk, so pod memory does not grow with the package. A
  missing, denied, oversized, or digest-mismatched archive exits 68 there; a
  datastore outage or interrupted read stays retryable. Otherwise a missing object, an object above the largest
  valid archive size, or a digest mismatch is a descriptor failure (exit 64).
  Argo's retry expression retries neither, and the missing-object diagnostic
  names `massive publish`. With
  least-privilege credentials that lack `s3:ListBucket`, S3 reports a missing
  key as 403 AccessDenied; the runner treats a denied archive read the same
  way and names both causes: an unpublished archive or missing read
  permission. Control pods
  execute no author code and receive no source.

Selection is explicit rather than automatic by size. `object-store-v0` adds a
deploy step that needs datastore write credentials, so a growing workflow
should not silently change how it must be deployed. Upload the archives before
the first run:

```sh
massive publish .massive/argo --datastore-config datastore.json
```

`massive publish` reads `bundle-manifest.json`, checks each archive against the
digest recorded there and pinned by `materialization-manifest.json`, re-derives
its source-package identity, and writes it with an atomic if-absent put.
Republishing is idempotent; an existing object with different bytes is a
conflict, never overwritten. The descriptor uses the
shared datastore schema and may differ from the in-cluster one only in its
endpoint (for example, a port-forward). Credentials come from the standard
AWS environment. A bundle built with `embedded-v0` has nothing to publish and is
rejected.

The archive key is content-addressed by both the package identity and the
exact archive digest. Archive bytes are not uniquely determined by package
identity (tar metadata and end padding can vary), so keying by package alone
would let one differently encoded archive occupy the key every pod reads. Local
runs and `embedded-v0` pods install archives under the same layout.

Source packages may hold up to 16,384 files and 256 MiB of file bodies. The Go
verifier and the Python runner share these bounds, recorded with shared
at-limit and over-limit archives in `conformance/fixtures/source-limits`. The
largest archive they accept is 16,384 × 1 KiB of headers and padding, plus
256 MiB, plus one 10 KiB tar record of end padding. The Python runner refuses
an object above that size from its declared length (S3 `ContentLength` or file
size) and stops copying once it passes the bound, so an object planted by any
holder of store write credentials cannot fill pod disk. The TypeScript runner
buffers archives in memory and keeps a 1,024-file, 50 MiB cap; `massive build`
rejects a larger TypeScript package, and the runner rejects one with a
diagnostic naming the Python runner.

Build and publish hold each archive in memory. Pods do not: the Go runtime never
reads the archive, and the Python runner streams the download and each
extracted file, then deletes its archive copy before user code runs. Peak pod
scratch is therefore the archive plus the extracted tree, up to about twice the
package size, during extraction.

**Size budget.** Every runner pod, including each map item, downloads and
extracts the full archive before invoking user code; it needs up to twice the
package size in scratch while extracting and the extracted size afterwards. Packages of a few tens of MB suit fan-outs of about 1,000
items. Above that, account for transfer volume (item count × archive size) and
per-pod ephemeral storage. A node-level cache keyed by archive digest is a
planned follow-up, not current behavior.

## Target Config

Argo customization uses typed target config, compiler plugins, and ordered raw patches.

Typed config is not a separate privileged path. It lowers into the same internal patch representation as user patches.

Argo configuration is declared in a separately hashed `DeploymentSpec` that references one `WorkflowPlan`. It carries profile settings such as namespace, service account, template name, and opaque artifact-store binding; it never carries raw credentials. The target compiler is responsible for rejecting plan features that Argo cannot represent or that the selected profile does not enable.

```ts
deployment.argo({
  name: "argo-staging",
  artifactStoreBinding: "staging-artifacts",
  namespace: "workflows",
  serviceAccountName: "massive-runner",

  workflowTemplate: {
    parallelism: 100,
    podGC: { strategy: "OnPodSuccess" },
    ttlStrategy: { secondsAfterCompletion: 86400 },
  },

  podDefaults: {
    priorityClassName: "high",
    nodeSelector: { "karpenter.sh/nodepool": "gvisor" },
    tolerations: [gvisorToleration],
    securityContext: { runAsNonRoot: true },
  },

  artifactRepository: argo.s3Artifacts({
    bucket: "workflow-artifacts",
    prefix: "massive/",
  }),

  networkPolicy: argo.networkPolicy({
    mode: "kubernetes",
  }),

  runtime: argo.runtime({
    secretMode: "native",
    networkMode: "native",
  }),

  presets: [
    argo.presets.gvisorPool(),
  ],

  plugins: [
    argo.plugins.datadogOtel(),
    argo.plugins.podLabels({ team: "security" }),
  ],

  patches: [
    argo.patch.workflowTemplate("owner-annotation", {
      metadata: { annotations: { owner: "appsec" } },
    }),
    argo.patch.stepPod("scan-priority", "scan", {
      priorityClassName: "urgent",
    }),
  ],
});
```

## Compiler Pipeline

```text
1. Plan
   Consume WorkflowPlan and Argo target planning inputs.

2. Materialize
   Produce an internal typed Argo bundle tree.

3. Apply Presets
   Named bundles such as gpuPool, gvisorPool, datadog, restrictedNet.

4. Apply Typed Config
   workflow-wide -> template defaults -> per-template -> per-step.

5. Apply Raw User Patches
   strategic merge patches first, then JSON patches, declared order.

6. Validate Structure
   Argo/Kubernetes schema validation.

7. Apply System Mediation
   v0 DirectMediationProvider. Future SidecarMediationProvider.

8. Validate Invariants
   DAG edges, artifact wiring, plan hash, reserved names, service account,
   secrets, egress.

9. Emit Bundle
   Canonical YAML, workflow.json, provenance sidecar, bundle manifest.
```

System mediation runs after user patches. Users can customize generated YAML freely, then the compiler reasserts secret/network/runtime wiring in a controlled stage.

### Pod Placement Seam

Portable execution contracts do not contain raw Kubernetes `PodSpec` fields.
[Pod placement](#pod-placement) is deployment configuration keyed by node id,
so it never enters plan identity. An opaque placement class carried by the
contract (such as `sandboxed` or `large-ephemeral-disk`) would let several
nodes share one setting, but it puts a deployment vocabulary into the plan
hash; add one only when node-keyed overrides prove too repetitive.

## V0 Executable Wedge

The full pipeline above is the target architecture, not the first implementation slice. The first Go Argo compiler should only implement:

```text
1. Plan
   Consume WorkflowSpec target inputs and the compiled WorkflowPlan.

2. Materialize Tree
   Produce the minimal Argo WorkflowTemplate tree.

3. Validate Structure
   Validate generated YAML against the selected Kubernetes and Argo schemas.

4. Validate Minimal Invariants
   Enforce dag-integrity, plan-provenance, and identity-set.

5. Emit Bundle
   Emit canonical YAML, workflow.json, and bundle-manifest.json.
```

Presets, plugins, user patches, and field-level provenance explanations are deferred.
Application secrets bind logical refs to native Kubernetes Secret keys. Storage credentials can bind to a named Secret
or workload identity. Egress restrictions that prevent storage access fail the build.

The v0 Argo step image contract is the same as the container environment
contract: an immutable image contains the matching `massive-workflows` release.
The mounted runtime bundle supplies verified source, and `massive runtime step`
resolves the requested symbol, validates one canonical JSON input, invokes the
language runner, and writes one canonical JSON output parameter.

Graph IR 0.3 finite maps use the same runner seam. A nested DAG expands the
crystallized list into indexed values, invokes `massive runtime map item` with
Argo `withParam`, and collects indexed outputs in source order. Template
`parallelism` enforces `maxConcurrency`. A singleton internal marker carries an
empty list through Argo's loop-output aggregation without invoking user code;
the collector publishes canonical `[]`.

The compiler does not emit a one-off `Workflow`. Apply the source ConfigMap, datastore ConfigMap, and WorkflowTemplate, then submit it with
`argo submit --from workflowtemplate/<name> -p 'input=<json>'`.

The first executable Argo wedge supports `env.container(...)` only. `env.node(...)` should be rejected for Argo with a clear target compatibility diagnostic until Node dependency environment materialization exists for Kubernetes.

## Patches

Patches are named and ordered. Compiler plugins return patches; they do not mutate the bundle directly.

```ts
argo.patch.strategic("scan-priority", {
  scope: { kind: "task", nodeId: "scan" },
  value: { priorityClassName: "high" },
  onMiss: "error",
});

argo.patch.json("remove-default-label", {
  scope: { kind: "workflow" },
  ops: [{ op: "remove", path: "/metadata/labels/foo" }],
});
```

Patch rules:

- semantic selectors are preferred over array indexes,
- `onMiss: "error"` by default,
- patches carry provenance,
- strategic merge is the common path,
- JSON Patch is the surgical escape hatch,
- raw patches are applied before system mediation.

## Provenance

V0 includes a basic field-level provenance map.

Every patch op records:

- name,
- source layer,
- scope,
- target path,
- old value hash when known,
- new value hash,
- timestamp-free deterministic metadata.

The emitted provenance sidecar should support rough explanations such as:

```text
massive compile --explain spec.templates.scan.priorityClassName
```

The CLI can be basic in v0, but the data must exist.

## Invariants

Final validation runs after all user patches and system mediation.

Starter invariant set:

- `dag-integrity`: every IR node maps to a reachable Argo template and every IR edge survives as an Argo dependency.
- `entrypoint-resolves`: generated entrypoint exists.
- `artifact-wiring`: steps can read/write the configured object store and plan artifacts.
- `identity-set`: each pod has a service account.
- `plan-provenance`: compiled plan hash annotation exists and matches.
- `reserved-names`: user resources cannot collide with `wf-system-*` reserved names.
- `name-uniqueness`: generated names are valid and unique.
- `secret-binding`: native secret mode binds all declared secrets.
- `egress-representable`: selected network mode can represent declared egress intents or explicitly marks them unenforced.

Invariants should have severities:

- hard,
- soft warning,
- forceable hard with explicit unsafe acknowledgement.

Some invariants, such as reserved system names, should not be forceable.

## Runtime Mediation

V0 uses direct/native mediation:

```ts
runtime: {
  secretMode: "native",
  networkMode: "native",
}
```

Future:

```ts
runtime: {
  secretMode: "sidecar",
  networkMode: "sidecar-proxy",
}
```

The future sidecar/proxy model is reserved now through:

- IR secret refs and egress intents,
- a mediation provider interface,
- reserved names such as `wf-system-*`,
- reserved proxy port ranges,
- reserved annotations for internal secret/egress wiring,
- target config slots for sidecars and proxy config.

Provider interface sketch:

```ts
interface MediationProvider {
  injectSecretAccess(tree: ArgoBundleTree, reqs: SecretRequirement[]): Patch[];
  injectEgressMediation(
    tree: ArgoBundleTree,
    reqs: EgressIntent[],
    mode: NetworkPolicyMode,
  ): Patch[];
}
```

V0 ships `DirectMediationProvider`. Future support adds `SidecarMediationProvider` without changing workflow authoring or the core IR.

## Determinism

The Argo compiler must be deterministic:

- stable ordering,
- canonical YAML/JSON serialization,
- no timestamps in emitted artifacts,
- sorted map keys where possible,
- bundle hash covers IR, target config, patches, provider identities, compiler version, and materialized artifact references.

## Decisions and selects

Argo lowers all supported graph node kinds. A decision runs `massive runtime
control`, validates the selected case with the same schema validator as local
execution, and emits a numeric case index. User tags never become controller
expressions. Decision and select tasks reuse an upstream container environment
without invoking author code or inheriting that step's resources or secrets.
Control tasks receive the shared datastore binding, including bound storage
credentials, so they can resolve and publish value references; they never
receive application secrets, resources, or an author network contract.

Every ordinary dependency requires `.Succeeded`. Selects wait for all branch
sources to finish or become inactive, require at least one successful source,
and require their decision to succeed. A single lazy expression reads the chosen
output. This preserves nested inactive regions and prevents failed branches from
turning into successful empty results. The Argo node outputs persist the route
index; Argo currently owns the execution journal rather than emitting the local
`RunManifest` format.

The main container does not automount the Kubernetes API token. The explicit
`executor.serviceAccountName` supplies the Argo executor's identity; configure
that account as described in Argo's service-account documentation, including the
executor token and workflow-task-result permissions.

## Live conformance and runner image

`./scripts/test-argo.sh` builds the current wheel and a non-root runner image,
creates a disposable kind cluster, installs Argo **v3.7.16**, and runs nested
branches, arbitrary string tags, empty maps, and selected-item failures. It also
builds a generated source package of more than 2,000 files and 2 MiB with
`object-store-v0`, publishes it through a port-forward to the cluster's MinIO,
and requires the Argo result to equal a local run of the same workflow. It
cleans up its cluster and retains diagnostics under `dist/argo-test-logs` on
failure. Prerequisites: Docker, kind, kubectl, curl, Go, and uv. With snap Docker,
set `TMPDIR` to a writable directory under your home so Docker can read the build
context and image archive. CI runs the same script.

`packages/python/Dockerfile` consumes a platform wheel and `requirements.txt`
exported from the SDK's `uv.lock`. Its base image is digest-pinned, dependencies
are hash-verified, and the runtime runs as UID 65532. Publish the resulting image
and use its registry digest in `container(...)`. Application packages can extend
this recipe with their own locked dependencies; the generic image contains only
Massive and its runtime requirements.

Source archives use the selected runtime transport, and JSON values use value
parameters. All Argo invocations use the shared datastore for artifact
publication, value references, and Blob/Tree hydration; there is no pod-local
fallback.

## Value parameters

Argo passes values between tasks as parameters. It copies them into pod
arguments and its template environment, which Linux limits to 128 KiB per
string, and into the workflow status. A merge step or a map collector receives
several values at once. So Massive runtime commands exchange **value
parameters**, and between tasks each value has exactly one valid spelling:

- canonical JSON text when the canonical body is at most **4,096 bytes**; or
- `@` followed by the canonical JSON of a reference,
  `{"hash":"sha256:<hex>","size":<bytes>}`, to the canonical body stored at
  `blobs/sha256/<hex>` with content type `application/json`.

`@` cannot begin JSON text, so the forms are unambiguous for every value.
Runtimes reject whitespace, reordered or duplicate keys, inline values above
4 KiB, and references to bodies small enough to be inline, with a
non-retryable exit 64. The reference schema is
`conformance/schema/value-reference.schema.json`; referenced bodies are at most
256 MiB. A reference's hash is the SHA-256 of the same canonical bytes recorded
in artifact manifests and step descriptors, so carrying a value by reference
never changes its identity, input hashes, output manifests, or idempotency
keys. Readers check the object's declared size against the reference before
reading, never read past it, and verify hash, content type, and canonical
encoding before use. Vectors, with hashes computed independently of the Go
implementation, live in `conformance/fixtures/value-parameters`.

**Workflow inputs** are inline JSON only. A submitted reference is rejected,
because it could point pods at any body in the shared store, including another
project's. A step fed by the workflow input canonicalizes it itself
(`--workflow-input`). When a decision or map consumes the workflow input, an
inserted `workflow-entry` task normalizes it once, publishing it when it exceeds
4 KiB, so those control tasks only read. A workflow result above 4 KiB is
returned as a reference; read its body from the datastore.

**Maps.** Expansion never writes. Items of an inline list travel inline. Items
of a referenced list carry that list's reference as
`{"index":i,"listRef":{...}}`, and each item pod reads the list and selects its
own index, so every item pod downloads the whole list. Mapper results travel as
`{"index":i,"value":v}` when the result is at most **100 bytes**, otherwise as
`{"index":i,"ref":{...}}`, which costs about 120 bytes. Argo concatenates every
result envelope into the collector's single parameter, and that aggregate
appears twice, JSON-escaped, in the collector's template environment. Expansion
therefore rejects maps wider than **341 items** with a clear exit-64 error
rather than letting the collector fail with "argument list too long". Split
wider work into fewer, larger items.

**Merges.** A merge step receives one parameter per source (`--merge-input`, in
`mergeInputs` order) rather than a concatenated array, because a reference is
not JSON text.

Typed step inputs are hydrated by the Go runtime before the language runner
starts, so runners and author code see ordinary values in both targets. The
local target already passes every value as a datastore artifact and never needs
references.

**Datastore access by template.** All pods mount the same datastore descriptor
and, when `--artifact-credentials-secret` is set, receive the same storage
credentials. The minimum S3 permissions each template needs are:

| Template | Reads | Writes |
| --- | --- | --- |
| step, map item (author code) | run inputs, schemas, source archives, Blob/Tree bodies, `blobs/sha256/*` | run inputs and output manifests, `blobs/sha256/*`, file artifacts |
| decision, select | `blobs/sha256/*` (referenced inputs) | none |
| map expand | `blobs/sha256/*` (referenced list) | none |
| map collect | `blobs/sha256/*` (referenced results) | `blobs/sha256/*` (collected list) |
| `workflow-entry` (only before a decision or map) | none | `blobs/sha256/*` (normalized input) |

Control tasks never receive application secrets, resources, or an author
network contract. Binding control templates to a separate, narrower credential
(for example a control-pod service account with object read on
`blobs/sha256/*` and put only for collect and entry) is a follow-up; today one
binding serves every pod.

## Shared invocation datastore

`--artifact-store` is required and names a Kubernetes ConfigMap containing
`datastore.json`. `object-store-v0` reads source archives from this same
store; `embedded-v0` does not:

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: massive-artifacts
  namespace: workflows
data:
  datastore.json: |
    {"kind":"s3","bucket":"workflow-artifacts","region":"us-east-1","prefix":"massive"}
```

For an S3-compatible service, add `endpoint` (an HTTP(S) origin) and
`forcePathStyle: true`. The bucket must already exist. Both Go and Python use
this same descriptor; unknown fields and credential values in the descriptor
are rejected. File bodies, tree manifests, schemas, source packages, and step
output manifests are published to this store. Consumers hydrate files into
private invocation scratch directories. A forwarded reference preserves its
original bytes even if a working copy was edited.

Without `--artifact-credentials-secret`, the runtime resolves standard AWS
credentials from the execution environment, including workload identity. The
Go provider refreshes IAM credentials; Python uses its SDK credential chain.
With the flag, Massive binds only `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`,
and optional `AWS_SESSION_TOKEN` from that Secret to runtime containers. Control
tasks receive them only to read value references; map collection and the
workflow entry task also publish them. No credential bytes
are included in plans, descriptors, source archives, or deploy bundles.

An `egress: none` execution contract cannot reach remote storage, so the Argo
compiler rejects it. A future storage mediation implementation must provide a
real enforcement mechanism before that combination can be supported.

Rebuild existing Argo bundles: `massive runtime step` and `runtime map item`
require `--datastore-config <file>` and exactly one of `--bundle-dir`
(`embedded-v0`) or repeated `--source-archive` (`object-store-v0`). There is no compatibility
flag or default private store. The standalone isolated invocation primitive can
also accept an explicit local descriptor for filesystem integration tests;
normal `massive run --store` continues to select the local backend's datastore.

Storage bindings must be scoped to the application trust boundary: use a dedicated
bucket or IAM permissions restricted to its object prefix. Invocation code is
trusted with its bound storage credentials. The Go gateway accepts explicit AWS
keys or configured web/container workload identity and does not discover EC2 node
credentials. Credential acquisition failures stop invocation before artifact IO.
## Retries and timeouts

A contract's `retry` lowers to a `retryStrategy` on runner templates only
(static steps and map items); decision, expansion, and collection control pods
never retry. The strategy uses `limit: maxAttempts - 1`, `retryPolicy: Always`,
and `expression: "!(lastRetry.exitCode in ['64', '65', '67', '68'])"`, so descriptor,
schema, author non-retryable, and dependency preflight (68) failures stop
immediately. Before each Python attempt, `massive runtime step` and
`runtime map item` extract the verified source archive and run dependency
preflight with the image's interpreter. An attempt that passes writes
`environment.json` (requirement and realization hashes, plus a reference to the
content-addressed `RealizedEnvironment`) beside its output manifest. The runner
then runs that interpreter with `-I`. A nonzero
`delaySeconds` adds `backoff{duration, factor, cap}`. The runtime receives
`--retry-count={{retries}}` and derives `attempt = retries + 1`, so every retry
publishes to its own attempt slot and keeps the same idempotency key.

`timeoutSeconds` is enforced inside the runtime process rather than through
`activeDeadlineSeconds`, so a timed-out attempt exits with 124 and stays
retryable.

A plan's run deadline (`graph.deadlineSeconds`, from the WorkflowSpec's
`workflow.deadlineSeconds`) is the only `activeDeadlineSeconds` Massive emits,
on the WorkflowTemplate spec. Argo measures it from the workflow's start, stops
running pods, and fails the workflow. It is plan data rather than deployment
data because it changes what a run means (an unfinished run fails), so it must
behave the same on the local target, where the orchestrator cancels execution
through its ordinary cancellation path and records a failed journal naming the
deadline. The live Argo gate stops a 600-second step with a 30-second deadline. Argo retries map items independently; the local orchestrator stops
scheduling retries once any item fails terminally.


## Pod placement

Deployments choose where pods run with `massive build ... --placement
placement.json`. The file becomes `target.placement` in `DeploymentSpec`, so
changing it changes the deployment hash and leaves the plan hash unchanged:

```json
{
  "defaults": {
    "labels": {"cost-center": "research"},
    "annotations": {"example.com/owner": "data-platform"},
    "tolerations": [{"key": "spot", "operator": "Exists", "effect": "NoSchedule"}],
    "priorityClassName": "batch"
  },
  "nodes": {
    "scan": {
      "nodeSelector": {"pool": "gpu"},
      "tolerations": [{"key": "nvidia.com/gpu", "operator": "Exists", "effect": "NoSchedule"}],
      "runtimeClassName": "gvisor",
      "podSpecPatch": {"containers": [{"name": "main", "resources": {"limits": {"nvidia.com/gpu": "1"}}}]}
    }
  }
}
```

Each entry may set `nodeSelector`, `affinity`, `tolerations`,
`runtimeClassName`, `priorityClassName`, `labels`, `annotations`, and
`podSpecPatch`. `defaults` applies to every pod. A `nodes` entry, keyed by the
plan's step or map node id (a called child's steps use their scoped
`call--step` ids), overrides the defaults for the pods that run that node's
author code: the step pod, or every item pod of a map. `nodeSelector`,
`labels`, and `annotations` merge by key with the override winning; any other
field set by the override replaces the default. An override naming a missing
node, a decision, or a select fails the build.

Control pods (decision, select, map expansion and collection, and the
`workflow-entry` task) receive only the defaults. They run Massive's own code,
never author code, so accelerator pools and sandboxed runtimes would only
waste capacity on them; they still need the defaults because a cluster whose
nodes are all tainted, or a cost report keyed by label, must see every pod. Put
pool-specific settings in node overrides, not defaults, if control pods should
not land there.

Kubernetes values are validated at build time against the definitions in the
pinned Argo schema (`io.k8s.api.core.v1.Affinity`, `Toleration`, and so on)
with unknown fields rejected, like the API server's strict field validation, so
a misspelled field is a diagnostic rather than a value the controller drops.
The deployment schema adds what the OpenAPI types omit: toleration operators
and effects, label key and value syntax, and DNS names. Placement cannot
override the plan's platform keys (`kubernetes.io/os`, `kubernetes.io/arch`),
and label or annotation keys under `massive.dev/` or `argoproj.io/` are
reserved.

Each pod template receives its resolved settings directly rather than through
Argo's workflow-level defaults, so Argo's own merge rules never apply. Argo
templates have no `runtimeClassName` field; it is lowered into the template's
`podSpecPatch` together with the deployment's patch.

`podSpecPatch` is the escape hatch: a strategic merge patch over the generated
pod spec, limited to `securityContext`, `dnsPolicy`, `dnsConfig`,
`hostAliases`, `imagePullSecrets`, `schedulerName`,
`terminationGracePeriodSeconds`, and one `containers` entry named `main` that
may set `securityContext` and extended `resources` such as GPUs. Identity,
volumes, the main container's image, command, and environment, plan-owned CPU
and memory, and every field with a typed setting are not patchable; patch
directives such as `$patch` are unknown fields.

The live Argo gate runs a map whose item pods carry a node selector, a
RuntimeClass, and a patch, and checks that every pod the controller created
carries the default labels, annotations, tolerations, and an admitted
PriorityClass, while only the item pods carry the override.

## Application secret bindings

A task declares portable environment names and logical refs, for example
`execution(environment=environment, secrets={"SERVICE_TOKEN": "catalog-api"})`.
Deploy with `massive build ... --secret-bindings bindings.json`, where the file is:

```json
{"catalog-api": {"name": "catalog-credentials", "key": "token"}}
```

Bindings are names only. They enter DeploymentSpec and its hash, while plan and
environment identities stay unchanged. The compiler never fetches credentials.
Each user step or map item receives only its declared refs as required
`valueFrom.secretKeyRef` entries. Control pods receive none. Missing logical
bindings fail compilation; missing Kubernetes Secrets or keys prevent the pod
from starting. Keys resolve in the workflow's deployment namespace. Kubernetes
owns rotation and authorization; changing a Secret does not alter an already
running container's environment.

Names and keys follow [Kubernetes Secret constraints](https://kubernetes.io/docs/concepts/configuration/secret/#constraints-on-secret-names-and-data).
Environment names must use the portable `[A-Za-z_][A-Za-z0-9_]*` form, with no
repeated names in one contract. The `AWS_` and `MASSIVE_` prefixes are reserved for
storage and runtime configuration, including when workload identity is selected.
`PATH`, `HOME`, `PYTHONPATH`, `PYTHONHOME`, `TMPDIR`, `TMP`, `TEMP`, `LD_PRELOAD`,
`LD_LIBRARY_PATH`, `LD_AUDIT`, and `NODE_OPTIONS` are also reserved. Use application
names and explicitly configured clients for credentials outside that boundary.
Reserved names protect runtime configuration; they are not a security boundary
against author code. A deployment mapping can contain refs unused by a particular graph; they are
never injected unless a task declares them.

This is native environment binding only. Local execution continues to inherit
the caller's environment without selective binding or secret preflight. There
is no `.env` loader, secret-manager lookup, optional-secret fallback, or secret
value serialization. The live Argo gate checks a real Secret through a mapped
invocation and verifies that other pods never receive its environment entry.
