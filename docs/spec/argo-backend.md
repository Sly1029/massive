# Argo Backend

Status: draft

Argo is the first non-local backend. The Argo compiler emits a deploy bundle, not only a single WorkflowTemplate.

It is implemented as the first `target.Backend` behind the backend-neutral
interface in [target-backends.md](target-backends.md); the plan-driven `Backend`
contract, bundle manifest, and deterministic emission described there are shared
by every future backend (Cloudflare Workers, Temporal, Vercel).

The Argo backend proves the main product thesis: a portable typed graph plus execution contract can lower to real infrastructure with pods, resources, object storage, environment artifacts, network policy, secret binding, observability, and generated deployment artifacts.

## Deploy Bundle

An Argo compile target emits a deployable template bundle, not an ad hoc run object. The primary Kubernetes artifact is a `WorkflowTemplate`.

```text
dist/argo/<workflow-name>/
  workflow-template.yaml
  network-policy.yaml
  service-account.patch.yaml
  massive-plan.json
  provenance.json
  bundle-manifest.json
  values.schema.json
```

The exact files vary by target config. The bundle manifest is canonical and records all emitted artifacts.

## Target Config

Argo customization uses typed target config, compiler plugins, and ordered raw patches.

Typed config is not a separate privileged path. It lowers into the same internal patch representation as user patches.

Argo target config is declared inside `WorkflowSpec` as a target request. The Go compiler may be asked to compile only the Argo request from a multi-target spec, but Argo semantics come from the spec, not ad hoc CLI flags. The compiler is responsible for rejecting workflow features that Argo cannot represent or that the selected Argo target configuration does not enable.

```ts
argoTarget({
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

Presets, plugins, user patches, system mediation, field-level provenance explanations, secret binding, and network policy enforcement are deferred until after the SDK -> spec -> Go -> Argo execution path works end to end.

### Step pod contract (step driver)

The generated `WorkflowTemplate` does **not** embed a `StepInvocationDescriptor`. Embedding one at compile time is unfixable: the input artifact's content hash and the pod-reachable datastore endpoint are only known at run time, so a compile-time descriptor is never schema-valid. Instead, each step template's container runs the **Massive step driver** — `massive-orchestrator step` — which executes exactly one plan node:

- It loads the compiled plan from the datastore (`plans/<plan-key>/workflow.json`) and verifies it against the plan hash.
- It materializes the node's input from upstream step outputs (including `mergeInputs` fan-in), reusing the local orchestrator's logic, so the input artifact carries a real content hash.
- It builds a schema-valid `StepInvocationDescriptor` — byte-for-byte the same shape a local run builds — and invokes the TS runner.
- It records the node's run-manifest entry. Data flows step-to-step through the datastore in DAG order; the DAG dependencies provide ordering only.

The **runtime image contract** is that the container-env image ships the step driver **and** the TS runner (which fetches the source package from the datastore, resolves the symbol, reads inputs, writes outputs). The step template's container is:

```text
command: [massive-orchestrator, step]
args:    [--node <id>, --run-id {{workflow.uid}}, --plan-hash <plan-hash>]
```

Node id and plan hash are static template values; the run id is Argo's per-run `{{workflow.uid}}`, so one reusable `WorkflowTemplate` serves every run.

Datastore location and project identity come from **container environment variables** (`MASSIVE_DATASTORE_KIND`, `MASSIVE_DATASTORE_PATH`, `MASSIVE_PROJECT_ID`), each bound to a **workflow parameter** (`spec.arguments.parameters`, declared with names and no defaults). WS-8 supplies real values at submit time (`argo submit -p datastore-kind=... -p datastore-path=... -p project-id=...`). The generated YAML therefore contains only env var **names** and workflow-parameter **references** — never datastore coordinates and never credentials. S3/MinIO credentials remain env-sourced (from a Kubernetes secret wired by WS-8), never baked into the template and never logged, per org policy. The v0 step driver supports the local datastore; the pod-reachable S3/MinIO datastore is WS-8.

The compiler should not emit a one-off `Workflow` as the primary bundle artifact. Actual submission and execution are left to users or test harnesses. Local cluster tests can submit the generated template through the Argo CLI, for example with `argo submit --from workflowtemplate/<name>`, then wait for completion and inspect datastore artifacts.

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
