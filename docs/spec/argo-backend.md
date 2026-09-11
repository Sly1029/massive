# Argo Backend

Status: executable DAGs, decisions/selects, and finite maps

The current implementation emits an executable DAG and validates its
`WorkflowTemplate` offline against Argo Workflows v3.7.16. The bundle mounts an
immutable ConfigMap containing the verified plan and source archives; every pod
runs one isolated proto-described step through the same language runner used by
local execution. Argo output parameters carry canonical JSON values between
tasks. The template is annotated
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
    source-sha256-<digest>.tar  # verified transport-neutral source package
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
  --artifact-store massive-artifacts
```

The lower-level `massive-compiler bundle-argo` command additionally requires a
`--runtime-assets` directory and `--materialization materialization-spec.json`.
Plan and deployment schema v1 are required; older artifacts must be rebuilt. See the
[portable materialization contract](materialization-contract.md).
`massive build` is the public path: it verifies
the authoring source manifest, creates deterministic archives, and binds them
to the exact canonical plan. Verified source archives are exposed as standalone
runtime assets and recorded in `bundle-manifest.json`; the 0.1 embedded
ConfigMap contains the same bytes. This deliberate duplication keeps the first
Argo wedge self-contained while giving an object-store transport a stable pack
to upload without decoding Kubernetes resources.

## Runtime Transport

Runtime packing and runtime transport are separate concerns. Compilation
produces one immutable pack containing the canonical plan, schemas, and
content-addressed source archives. A transport adapter then materializes that
pack:

- `embedded-v0` stores the plan and archives in an immutable ConfigMap;
- `object-store` will publish the same pack to the datastore and place only
  verified artifact references in the generated template.

The embedded adapter remains intentionally size-bounded. S3 support should be
implemented by adding the second adapter at this seam, not by teaching Python
workflow authors about uploads or adding S3 conditionals to graph compilation.

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

Portable execution contracts should not contain raw Kubernetes `PodSpec`
fields. They should carry an opaque placement class such as `sandboxed`,
`sandboxed-netraw`, or `large-ephemeral-disk`. The Argo deployment profile
resolves each class to target-owned pod settings such as runtime class,
affinity, tolerations, priority, and ephemeral-storage requests. A future
backend can resolve the same class differently or reject it precisely.

This is the intended replacement for workflow-local copies of affinity and
toleration trees. Raw named patches remain the escape hatch for genuinely
one-off Kubernetes behavior, but they are applied after placement resolution
and validated against the pinned Argo/Kubernetes schema. Massive-owned volume,
identity, output, and runtime fields remain reserved.

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
Control tasks do not receive storage credentials or an author network contract.

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
branches, arbitrary string tags, empty maps, and selected-item failures. It
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

Source archives still use the bounded embedded transport, and ordinary values
still pass through Argo parameters. All Argo invocations use the shared datastore
for artifact publication and Blob/Tree hydration; there is no pod-local fallback.

## Shared invocation datastore

`--artifact-store` is required and names a Kubernetes ConfigMap containing
`datastore.json`. It is independent of the embedded **source** transport:

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
and optional `AWS_SESSION_TOKEN` from that Secret to user invocation containers.
Control-only tasks never receive those storage credentials. No credential bytes
are included in plans, descriptors, source archives, or deploy bundles.

An `egress: none` execution contract cannot reach remote storage, so the Argo
compiler rejects it. A future storage mediation implementation must provide a
real enforcement mechanism before that combination can be supported.

Rebuild existing Argo bundles: `massive runtime step` and `runtime map item` now
require `--datastore-config <file>` instead of `--store`. There is no compatibility
flag or default private store. The standalone isolated invocation primitive can
also accept an explicit local descriptor for filesystem integration tests;
normal `massive run --store` continues to select the local backend's datastore.

Storage bindings must be scoped to the application trust boundary: use a dedicated
bucket or IAM permissions restricted to its object prefix. Invocation code is
trusted with its bound storage credentials. The Go gateway accepts explicit AWS
keys or configured web/container workload identity and does not discover EC2 node
credentials. Credential acquisition failures stop invocation before artifact IO.
Automatic pod retries remain disabled until shared publication retry behavior is
covered by live failure-injection tests.


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
A deployment mapping can contain refs unused by a particular graph; they are
never injected unless a task declares them.

This is native environment binding only. Local execution continues to inherit
the caller's environment without selective binding or secret preflight. There
is no `.env` loader, secret-manager lookup, optional-secret fallback, or secret
value serialization. The live Argo gate checks a real Secret through a mapped
invocation and verifies that other pods never receive its environment entry.
