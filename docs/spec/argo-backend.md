# Argo Backend

Status: draft

Argo is the first non-local backend. The Argo compiler emits a deploy bundle, not only a single WorkflowTemplate.

The Argo backend proves the main product thesis: a portable typed graph plus execution contract can lower to real infrastructure with pods, resources, object storage, environment artifacts, network policy, secret binding, observability, and generated deployment artifacts.

## Deploy Bundle

An Argo compile target emits a directory or archive like:

```text
dist/argo/<workflow-name>/
  workflow-template.yaml
  network-policy.yaml
  service-account.patch.yaml
  massive-plan.capnp
  provenance.capnp
  bundle-manifest.capnp
  values.schema.json
```

The exact files vary by target config. The bundle manifest is canonical and records all emitted artifacts.

## Target Config

Argo customization uses typed target config, compiler plugins, and ordered raw patches.

Typed config is not a separate privileged path. It lowers into the same internal patch representation as user patches.

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
   Canonical YAML, workflow.capnp, provenance sidecar, bundle manifest.
```

System mediation runs after user patches. Users can customize generated YAML freely, then the compiler reasserts secret/network/runtime wiring in a controlled stage.

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

