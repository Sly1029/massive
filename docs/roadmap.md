# Roadmap

Status: living document (last revised 2026-08-25)

This is the active prioritization for Massive after the 0.1 platform wheel and
the auditable Argo runtime packs (PRs #27–#31). It replaces the archived
[implementation roadmap](spec/archive/implementation-roadmap.md); the normative
architecture direction remains
[Workflow Platform v2 Direction](spec/workflow-platform-v2.md).

Priorities here are informed by a concrete consumer integration: the
`semgrep/workflows` fleet (142 workflows on a Metaflow-backed internal SDK) and
its side-by-side Massive prototype
([workflows#2173](https://github.com/semgrep/workflows/pull/2173)). Massive is
not built for that one consumer, but the fleet is a good forcing function: the
features it needs are the features any serious platform consumer needs.

## Layering rule

Every requested capability gets assigned to exactly one layer before it gets
built:

- **Massive core** — generic graph semantics, execution contracts, plan/bundle
  identity, backend lowerings, and the seams other layers plug into. Core never
  contains consumer policy: no org nodepools, no billing, no product-specific
  metadata.
- **Consumer platform SDK** — a consumer-owned package (for Semgrep, a future
  `wf_sdk` v2) that composes core seams into org policy: middleware packs,
  secret providers, placement class definitions, registry/UI metadata,
  automations.
- **Consumer migration** — changes inside consumer repos (domain model
  adjustments, image rebasing, porting workflows).

The recurring failure mode this rule prevents: a consumer need arrives shaped
like "add X to Massive" when the durable answer is "add a seam to Massive and
implement X against it in the consumer SDK." Each workstream below is labeled
with its layer split.

## Workstream A — graph semantics on Argo (core)

The Argo lowering accepts `start`/`step`/`map`/`end` and still rejects Graph IR
0.2 decisions/selects. This is the single largest remaining gap between
what the SDK can express and what deploys: in the reference fleet, 87 of 142
workflows use dynamic fan-out and the conditional-routing pattern is mandated
by its style guide.

1. **Complete (2026-08-25):** lower finite maps to Argo native fan-out
   (`withParam` over an indexed crystallization of the item list), preserving
   `maxConcurrency` via `parallelism`, duplicate-item identity, empty-map
   behavior, and ordered collection at the fan-in.
2. Lower decisions/selects to `when` expressions over the persisted decision
   artifact's discriminant tag.
3. Extend the conformance graph catalog with decision and map shapes so Graph
   IR 0.2/0.3 carries a catalog-level obligation for every backend, and add the
   missing `conformance/fixtures/bundles/`.

Layer: entirely core. No consumer code changes; the authoring surface already
exists.

## Workstream B — failure policy in the contract (core, small SDK surface)

Nothing in the plan proto, contract, or orchestrator expresses retries or
timeouts today; the invocation descriptor's `attempt` field is never advanced.
The reference fleet auto-injects retries on every non-terminal step and uses
timeouts in 83 files.

1. Add `retry` (max attempts, backoff) and `timeout` to `ExecutionContract` and
   the plan proto. Argo lowering is nearly free (`retryStrategy`,
   `activeDeadlineSeconds`); the local orchestrator honors the same fields and
   finally increments `attempt`. Idempotent artifact publication already makes
   retries convergent — that groundwork is done.
2. Do not clone Metaflow's `@catch`. A fallible step that degrades instead of
   failing is a step whose output type is a discriminated success/failure
   union feeding a decision — machinery that already exists and stays typed.
   Ship this as a documented pattern plus (optionally) a small authoring helper.

Layer: core for the contract fields and lowerings. The catch-equivalent
convention can be wrapped ergonomically by consumer SDKs if they want
Metaflow-flavored sugar.

## Workstream C — real source transport (core)

`embedded-v0` caps plan + source archives at 700 KiB of ConfigMap. Real
workflows ship rule/prompt trees; the reference fleet's compiled templates
already brush Argo's etcd ceiling. PR #31 deliberately made the runtime pack
transport-agnostic: the standalone source tars are byte-identical to the
embedded ones.

1. Add an object-store transport (`RuntimeTransport: s3-v0` or similar): the
   bundle step uploads the same content-addressed packs to the configured
   artifact store; pods fetch and verify by digest at startup.
2. Keep `embedded-v0` as the zero-infrastructure default for small workflows.

Layer: core. The seam was designed for exactly this; it is mostly execution.

## Workstream D — numeric portability decision (core decision, consumer migration)

canonical-json-v0 rejects floats at `emit()`. The reference fleet has 33 model
files with `confidence_score: float`-style fields. This decision must precede
any real detector port, because it is a domain-model migration on the consumer
side.

1. Core: document the blessed representations (Pydantic `Decimal` crossing as
   canonical string; scaled integers for fixed-precision scores) and the
   rationale, with a migration recipe. Extending canonical JSON itself with
   floats is rejected — deterministic cross-language float serialization is
   exactly the swamp canonical-json-v0 exists to avoid.
2. Consumer migration: convert score fields model-by-model as workflows port.

## Workstream E — composable runtime environment (core seam, consumer providers)

Secrets, egress allowlists, placement, and per-step middleware are all
instances of one missing abstraction: the composition of a step's *runtime
environment* out of declarative facets, each realized per backend. This is
specified in [Runtime Environment Composition](spec/runtime-environment.md)
(new). Highlights:

- Secrets become part of environment materialization: the contract declares
  logical secret names; a deployment-profile *secret provider* realizes them.
  Providers range from local env passthrough to a mediating token proxy that
  substitutes sentinel tokens for real credentials outside the step process —
  the pattern proven by Semgrep's Go sidecar runtime proxy, generalized into a
  seam instead of hardcoded.
- `network="declared"` (host allowlists) gets lowered by a profile-selected
  policy renderer (vanilla `NetworkPolicy` vs Cilium FQDN policies).
- The placement-class seam proposed in
  [argo-backend.md](spec/argo-backend.md) is implemented: contracts select an
  opaque class; the deployment profile resolves it to runtime class, affinity,
  tolerations, priority, and ephemeral storage.
- A step lifecycle/middleware protocol (plus `deps_type` injection) lets a
  consumer SDK attach tracing, usage metering, budget guards, and status
  dispatch to every step without forking the runner.

Layer: core owns the facet model, the provider/renderer interfaces, the
lifecycle protocol, and reference implementations (env-passthrough secrets,
vanilla NetworkPolicy renderer). Consumer SDKs own concrete providers (token
proxy, Cilium renderer, gvisor placement classes, billing middleware).
Security-sensitive seams (secret mediation, egress) get a security review
before implementation.

## Workstream F — second-backend seam probe (core, research first)

Argo alone cannot validate that the backend interface generalizes. Before
committing to more abstraction, run a design-level probe of a maximally
*different* target: Cloudflare Workflows — dynamic durable execution with
replay, JS-first runtime, bindings-based secrets, no pod model. Findings live
in [Cloudflare Workflows research](research/cloudflare-workflows-backend.md);
the deliverable is a seam verdict:

- which plan features lower cleanly (steps, retries, maps, decisions),
- which need per-backend capability declarations instead of hard errors
  (containers, resources, placement),
- which abstractions must be chosen now (capability negotiation, runtime
  transport per target, secret provider interface) versus safely deferred
  (durable resume semantics, non-DAG extensions).

An executable Cloudflare backend is *not* scheduled; the probe exists to keep
today's seams honest. A formal `Backend` capability table (declared supported
node kinds and contract features, checked at compile with a single diagnostic
shape) should fall out of this regardless of which backend comes second.

## Workstream G — consumer unblockers (consumer, with one core release task)

To merge workflows#2173 and make a first live Argo submission:

1. Core: publish `massive-workflows==0.1.0` to a real index (the consumer's
   private CodeArtifact index is already wired; lockfiles need linux/amd64 and
   arm64 platform-wheel variants).
2. Consumer: build a runner image on the existing fleet base with the pinned
   wheel, replacing the placeholder digest.
3. Consumer: extract engine-neutral domain models (`WorkflowOutput`, finding
   types) into a package that does not depend on the legacy engine, so a
   Massive workflow stops dragging Metaflow into the runner image.

## Cleanup (core, cheap, parallel to everything)

- Close stale PR #15 (superseded by #28–#31).
- Collapse the two-CLI split: port `inspect` and `--store-prefix` to the Go
  CLI; decide the fate of the Deno CLI and the TS SDK's unemittable
  channel/state surface, then delete or clearly quarantine it.
- Move the examples walkthrough onto the shipped toolchain (examples 01–04 are
  TypeScript driven by `deno task`; the wheel installs `massive`).
- Add CI. `pnpm check` (including the mock ban and the clean-wheel
  distribution test) is currently enforced only by convention and a
  one-check pre-commit hook.
- Parallelize the local orchestrator across independent DAG branches; the
  current sequential topological loop undercuts the local≡Argo parity story.
- Loosen project identity: `run` fails on any git origin that is not
  GitHub/GitLab unless `--project` is passed.
- Docs staleness: fixed alongside this roadmap (index statuses, archived
  roadmap moved, market-positioning note corrected).

## Deliberately later

- Durable retry/resume across process loss (required by the v1 acceptance gate
  in workflow-platform-v2, but nothing in the reference fleet uses resume).
- Cross-run named/tagged artifacts (needed eventually to replace consumer
  artifact stores; per-run content-addressed artifacts suffice for porting).
- Registry, trigger, and UI-input metadata — consumer SDK territory; core may
  eventually grow an opaque plan-annotation seam, not typed product metadata.
- Multi-step map bodies, broadcast/gather, reducer-backed joins (specified in
  workflow-platform-v2; sequenced after Workstream A proves single-step maps
  on Argo).
- Observability contracts (structured event log, metrics) — after the
  middleware seam exists, since consumer middleware covers the near-term need.
