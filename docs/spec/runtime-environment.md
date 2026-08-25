# Runtime Environment Composition

Status: draft

[Environment Materialization](environment-materialization.md) covers the
*dependency* environment: which runtime, packages, and platform a step's code
needs. This document covers everything else a step needs at runtime — secrets,
network egress, placement, and lifecycle instrumentation — and proposes one
compositional model for all of it, across backends.

The motivating observation: secrets, egress allowlists, placement, and
per-step middleware keep arriving as separate feature requests, but they are
all the same shape. Each is a **declarative facet** that the author or platform
states abstractly, and a **provider** that a deployment profile selects to
realize it on a concrete backend. Massive core should own the facet vocabulary
and the provider seams; consumer platform SDKs own concrete providers.

## Facets

A step's effective runtime environment is the composition of:

| Facet | Declared as | Realized by |
| --- | --- | --- |
| Dependency environment | `env.container(...)` / future `env.uv(...)` | materializer (existing) |
| Secrets | logical secret refs in the execution contract | secret provider |
| Network | egress intent (`none` / `declared(hosts)` / `any`) | egress renderer |
| Placement | opaque placement class name | profile resolution table |
| Lifecycle | middleware chain + typed deps | runner composition |

Two invariants carry over unchanged from environment materialization:

- Policy facets never enter the dependency environment key. Secrets, network,
  placement, and middleware do not change which bytes execute.
- Declarations live in the portable plan; realizations live in the separately
  hashed `DeploymentSpec`. The same plan deploys against different profiles
  without changing `planHash`.

Composition follows the existing contract-merge rules: workflow defaults, then
step overrides, merged by the frontend SDK into one effective contract per
node. Profiles compose the same way on the deployment side: an org base
profile provides defaults (placement table, default secret provider, egress
renderer), and a per-workflow profile overrides them.

## Secrets as materialized environment

The contract already declares secrets abstractly:

```py
execution(
    environment=container("ghcr.io/acme/runner@sha256:..."),
    secrets={"GITHUB_TOKEN": "github/app-token"},
    network="declared",
)
```

The declaration is a map from the name the step code sees to a logical
reference the platform understands. The plan records only this map. What is
missing today is the realization seam; Argo lowering currently fails the build
on any declared secret.

A **secret provider** is selected by the deployment profile, per secret class
or as a default. Its interface, conceptually:

```text
realize(declared secrets, target kind, profile)
  -> runtime wiring (env sources, mounts, sidecars, bindings)
  -> realization manifest (provider kind, refs — never values)
```

Reference providers that Massive core should ship:

- `env-passthrough` (local): the named variables must exist in the invoking
  environment; the runner passes them through. Nothing is persisted.
- `k8s-secret-ref` (Argo): each logical ref resolves to a `secretKeyRef` via a
  profile-owned mapping; the pod spec gains `valueFrom` entries.

Consumer-provided providers conform to the same seam:

- `platform-binding` (future Cloudflare/Vercel): logical refs map to the
  platform's binding/secret-store names; no env injection at all.
- `mediated-proxy`: see below.

Provider realizations are recorded in the bundle manifest like any other
generated artifact, so a deploy bundle states verifiably *how* every secret is
delivered — without containing any secret material. Profiles carry binding
names only; raw credentials never appear in any Massive artifact.

## Mediated credentials: the sentinel-token pattern

The strongest provider does not deliver the real credential to the step at
all. The step process receives a **sentinel token** — a syntactically valid
but worthless placeholder — and all egress flows through a mediation proxy
that recognizes sentinels, substitutes the real credential, re-signs the
request, and forwards it. This pattern is production-proven (Semgrep runs a Go
sidecar runtime proxy doing exactly this for workflow pods); Massive should
support it as a provider contract, not as a hardcoded integration.

Why it earns first-class support:

- The real credential never enters the step process. Untrusted or
  LLM-generated workflow code cannot exfiltrate what it never holds, and
  accidental logging leaks a worthless sentinel.
- Substitution requires interception, so the proxy is necessarily also an
  egress chokepoint. Secrets and network policy stop being separate
  enforcement problems: one component enforces the host allowlist and injects
  credentials, and the two policies can be checked against each other at
  compile time (a credential scoped to `api.github.com` on a step whose egress
  denies that host is a build error, not a runtime surprise).

The provider contract Massive core defines:

- **Sentinel format.** Namespaced and unmistakable, e.g.
  `massive-sentinel:<secret-name>:<nonce>`, generated per run. Runners treat
  the sentinel exactly like an env var; step code is unaware of mediation.
- **Substitution capabilities.** A mediation implementation declares which
  credential schemes it can rewrite and re-sign: static header/bearer
  substitution, AWS SigV4 re-signing, or scheme plugins. Compile fails loudly
  when a declared secret's scheme has no capable mediator in the profile —
  same philosophy as unsupported graph semantics on a target.
- **Attachment point per backend.** On Kubernetes: a profile-specified sidecar
  (or node-level proxy) plus pod wiring that routes step egress through it.
  Locally: an optional loopback proxy process the orchestrator can start, so
  the mediated path is testable in development rather than cloud-only. On
  platforms with no sidecar concept, the provider is simply unavailable and
  the profile must choose another.
- **Policy surfacing.** Substitutions and denials appear in the run manifest
  as structured events (secret name and host, never values), answering the
  open question of how policy violations surface in step logs.

Contracts may demand this level: a step (or workflow default) can declare
`secrets` with `mediation: required`, making the plan refuse profiles that
would deliver raw credentials into the process. That keeps the security
posture in the portable, reviewable artifact instead of in deployment
convention.

This section resolves the direction of the "Sidecar Runtime" questions in
[open-questions.md](open-questions.md): the proxy protocol is provider-defined
behind a core seam; owning object-store credentials and re-signing datastore
access is a natural second use of the same mediator; local development runs
the same mediator as a loopback process.

## Egress renderers

`network` intents stay as they are (`none`, `declared` + hosts, `any`), but
`declared` gets implemented via profile-selected renderers rather than one
hardcoded lowering:

- `k8s-networkpolicy`: vanilla `NetworkPolicy`. Honest limitation: it cannot
  express FQDN rules, so it can render `none`/`any` and CIDR-shaped
  declarations only; FQDN declarations fail compile under this renderer.
- `cilium-fqdn` (consumer-provided): renders `toFQDNs` rules, plus
  profile-supplied static denies (metadata endpoints and the like).
- `mediated-proxy`: the mediation sidecar enforces the allowlist itself;
  rendering degrades the pod to proxy-only egress.

The plan records the intent; the bundle manifest records which renderer
realized it and validation results.

## Placement classes

Implemented as proposed in [argo-backend.md](argo-backend.md): contracts
select an opaque class (`sandboxed`, `large-ephemeral-disk`, ...); the profile
resolves each class to runtime class, node affinity, tolerations, priority,
and ephemeral storage. Class *names* are portable strings in the plan; class
*definitions* are profile data. Kubernetes PodSpec trees never enter the
portable SDK or proto. Raw named patches remain the schema-validated escape
hatch. A profile that cannot resolve a class fails compile with the standard
unsupported-capability diagnostic.

## Lifecycle middleware and typed deps

Platform teams need code around every step — tracing, usage metering, budget
guards, status dispatch — without forking the runner. Two seams:

- **Middleware chain.** An ordered list of named middlewares wrapping step
  invocation inside the language runner: before-invoke, after-publish,
  on-failure. Declared per profile (platform policy, applies fleet-wide) with
  contract-level opt-outs, and resolved as symbols from source packages —
  the same symbol discipline as steps, no closures.
- **Typed deps.** `deps_type` (already reserved in the `GraphBuilder`
  signature) becomes real: a deps provider constructs the typed handle a step
  receives, composed from the realized environment (e.g. a GitHub client that
  is already sentinel-wired). Deps providers are consumer code; core owns the
  construction protocol and injection point.

Middleware identity participates in the deployment hash, not the plan hash:
attaching a usage tracker does not change what the workflow *is*, only how a
profile runs it.

## Per-backend realization summary

| Facet | Local | Argo | Platform runtimes (future) |
| --- | --- | --- | --- |
| Secrets | env passthrough or loopback mediator | secret-ref, or mediation sidecar | native bindings |
| Egress | mediator or unenforced (declared intent recorded) | NetworkPolicy / Cilium / mediator | platform egress config or reject |
| Placement | n/a (recorded, ignored) | class → profile resolution | reject or map to placement hints |
| Middleware | runner chain | runner chain (same runner) | runner chain where runner exists |

Facets a backend cannot realize fail at compile against that profile, with
the same explicit diagnostics the Argo lowering uses for unsupported graph
semantics. Capability is declared, never implied.

## Sequencing

1. Contract fields and plan/proto additions: `mediation` requirement flag,
   placement class name, structured `declared` hosts (already in proto).
2. Provider/renderer interfaces in the Go control plane; reference providers
   (`env-passthrough`, `k8s-secret-ref`, `k8s-networkpolicy`) and placement
   resolution; bundle-manifest realization records.
3. Mediation provider contract + local loopback mediator (the testable core).
4. Middleware chain + deps provider protocol in the Python runner.
5. Consumer SDK adopts: token-proxy mediator, Cilium renderer, org placement
   classes, platform middleware pack.

Secret mediation and egress enforcement are security-load-bearing; their
design review should include the security team before implementation starts.

## Open issues

- Exact sentinel wire format and per-run nonce derivation.
- Whether the mediator also fronts datastore access (re-signing object-store
  requests) in the same deployment, and what that does to runner credentials.
- Scheme-plugin packaging for mediators: compiled-in only, or a declared
  capability list in the profile?
- Middleware ordering semantics when org profile and workflow profile both
  contribute middlewares.
- Whether deps providers may perform I/O at construction time or must be
  lazy handles.
