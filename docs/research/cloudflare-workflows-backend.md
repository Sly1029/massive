# Cloudflare Workflows as a Massive Compile Target

Status: research note
Date: 2026-08-25

This note evaluates Cloudflare Workflows (durable execution on Cloudflare
Workers) as a candidate backend for Massive's static plan compiler, alongside
the existing Argo Workflows backend. It uses primary sources only: Cloudflare
developer documentation, Cloudflare blog posts and changelogs, and the
Cloudflare TypeScript SDK source (generated from Cloudflare's OpenAPI spec).
Factual observations are separated from inferences. Dates matter: Workflows
went GA on 2025-04-07; Python Workflows entered open beta 2025-08-22;
Containers went GA 2026-04-13.

## Executive summary

Cloudflare Workflows is a replay-based durable execution engine, not a static
DAG scheduler. The "graph" is whatever an imperative `run()` function does at
runtime; durability comes from memoizing `step.do()` results keyed by step
name and re-running the function against that cache. A Massive lowering is
feasible and reasonably clean as a **generated (or generic) TypeScript
interpreter that walks the compiled protobuf plan inside `run()`**, because a
static plan is deterministic by construction — exactly what the replay model
requires. Steps, retries/backoff, static fan-out, and event waits map
directly. The hard mismatches are compute shape (isolate steps have a 128 MB /
≤5 min CPU envelope; Python-in-container work must be pushed to Cloudflare
Containers behind Durable Object bindings), payload passing (1 MiB per step
result forces artifact-reference passing through R2 from day one), and
resource contracts (no CPU/memory requests on steps). This validates the
orchestrator/executor split already recorded in
[metaflow-replacement.md](metaflow-replacement.md): a Worker is a coordinator,
not a general Linux process host.

## Sourced facts

### 1. Execution model

- A Workflow is a class extending `WorkflowEntrypoint` with an async
  `run(event, step)` method containing at least one step call. `run` receives
  a `WorkflowEvent<T>` (`payload`, `timestamp`, `instanceId`, `workflowName`,
  optional cron `schedule`) and may return output retrievable via status
  queries. ([Workers API reference](https://developers.cloudflare.com/workflows/build/workers-api/))
- `step.do(name, config?, callback, rollbackOptions?)` runs a retryable step.
  Step names are up to 256 characters; callbacks must return serializable
  state (primitives, structured-cloneable objects, or
  `ReadableStream<Uint8Array>` for larger binary output). Optional
  `rollbackOptions` register compensating actions executed on failure or on
  `terminate({ rollback: true })`. ([Workers API reference](https://developers.cloudflare.com/workflows/build/workers-api/))
- Per-step config: `retries: { limit, delay, backoff }` and `timeout`.
  Defaults: 5 retries, 10 second delay, `exponential` backoff, 10 minute
  timeout per attempt. Backoff is one of `constant | linear | exponential`;
  `delay` may be a function of `(ctx, error)` including `ctx.attempt` for
  dynamic delays. Durations are milliseconds or strings ("30 seconds",
  "1 hour"). ([Sleeping and retrying](https://developers.cloudflare.com/workflows/build/sleeping-and-retrying/))
  Up to 10,000 retries per step are allowed. ([Limits](https://developers.cloudflare.com/workflows/reference/limits/))
- `step.sleep(name, duration)` (up to 365 days) and
  `step.sleepUntil(name, timestamp)` hibernate the instance; sleeps do not
  count toward the step limit. `step.waitForEvent(name, { type, timeout })`
  pauses until a matching event arrives via `instance.sendEvent({ type,
  payload })` or the REST events endpoint; default timeout is 24 hours.
  ([Workers API reference](https://developers.cloudflare.com/workflows/build/workers-api/))
- `NonRetryableError` thrown inside `step.do()` stops retries and propagates
  to `run`, failing the instance if unhandled. ([Workers API reference](https://developers.cloudflare.com/workflows/build/workers-api/))
- **Determinism/replay rules.** Only state returned from `step.do()` is
  persisted; instances hibernate and lose all in-memory state, so variables
  outside steps reset. Code outside `step.do()` may execute multiple times
  across engine restarts, so side effects and non-deterministic calls
  (randomness, timestamps) must live inside steps; conditional logic must be
  based only on deterministic values (event payload or prior step outputs).
  Steps should ideally be idempotent. Step names are the cache keys and must
  be derived deterministically. Mutations to the incoming event are not
  persisted across steps. `Promise.race()`/`Promise.any()` must be wrapped in
  a `step.do()`; every step must be awaited. ([Rules of Workflows](https://developers.cloudflare.com/workflows/build/rules-of-workflows/))
- **Engine internals.** Each instance runs one-to-one with an Engine that is a
  SQLite-backed Durable Object; a per-account controller Durable Object
  manages instances. The engine is a loop that re-executes `run()` returning
  memoized results for steps that already ran ("We return the cache if it
  exists. Else we run the user callback"), with Durable Object alarms driving
  wakeups after eviction/hibernation. ([Cloudflare blog, 2024-10-24](https://blog.cloudflare.com/building-workflows-durable-execution-on-workers/))
- **Instance lifecycle.** Binding methods: `create({ id?, params?, retention? })`
  (unique IDs up to 100 chars within retention), idempotent `createBatch`
  (≤100 instances), `get(id)`. Instance methods: `status()`, `pause()`,
  `resume()`, `restart(options?)` (restart from the beginning or from a named
  step by `{ name, count, type }`, reusing cached results of earlier steps),
  `terminate({ rollback? })`, `sendEvent()`. Status values: `queued | running
  | paused | errored | terminated | complete | waiting | waitingForPause |
  unknown` (the REST status-edit response also includes `rollingBack`).
  ([Workers API reference](https://developers.cloudflare.com/workflows/build/workers-api/),
  [SDK status.ts](https://github.com/cloudflare/cloudflare-typescript/blob/main/src/resources/workflows/instances/status.ts))

### 2. Limits

All from the [Workflows limits page](https://developers.cloudflare.com/workflows/reference/limits/):

| Limit | Workers Free | Workers Paid |
| --- | --- | --- |
| Steps per instance | 1,024 | 10,000 default, configurable to 25,000 (`step.sleep` excluded) |
| Persisted state per instance | 100 MB | 1 GB (includes streamed bytes) |
| Non-stream step return | 1 MiB | 1 MiB (use `ReadableStream` for larger binary) |
| Event/params payload | 1 MiB | 1 MiB |
| CPU time per step | 10 ms | 30 s default, configurable to 5 min |
| Wall-clock per step | unlimited | unlimited (I/O waits don't consume CPU) |
| Concurrent (running) instances | 100 | 50,000 (`waiting` excluded; queued beyond that: 100k / 2M) |
| Instance creation rate | 100/s | 300/s account, 100/s per workflow (HTTP 429 beyond) |
| Completed-state retention | 3 days | 30 days |
| Max `step.sleep` | 365 days | 365 days |
| Retries per step | 10,000 | 10,000 |
| Subrequests per invocation | 50 | 10,000 default, up to 10M |
| Workflows per account | 100 | 500 (shares Worker script limits) |
| Script size | 3 MB | 10 MB |

Workflow names ≤64 chars matching `^[a-zA-Z0-9_][a-zA-Z0-9-_]*$`; instance IDs
≤100 chars. Cron-triggered instances on Paid may run up to one hour per firing
without consuming concurrency slots; ≤100 schedules per account. The rules
page separately advises keeping step timeouts ≤30 minutes.
([Limits](https://developers.cloudflare.com/workflows/reference/limits/),
[Rules of Workflows](https://developers.cloudflare.com/workflows/build/rules-of-workflows/))

Underlying Workers platform limits also apply to step callbacks: 128 MB memory
per isolate, 6 simultaneous connections awaiting response headers, bundle 3 MB
(Free) / 10 MB (Paid) gzipped and 64 MB uncompressed.
([Workers platform limits](https://developers.cloudflare.com/workers/platform/limits/))

### 3. Language and runtime constraints

- Workflows is GA (2025-04-07) on Free and Paid plans; the first-class SDK is
  JS/TS. ([Changelog](https://developers.cloudflare.com/workflows/reference/changelog/),
  [GA blog](https://blog.cloudflare.com/workflows-ga-production-ready-durable-execution/))
- **Python Workers are open beta** (`python_workers` compatibility flag
  required). They run Python in the Workers runtime with an FFI to JavaScript
  objects and can access KV, D1, Durable Objects, Workflows, and Queues via
  bindings. ([Python Workers](https://developers.cloudflare.com/workers/languages/python/))
  Packages are declared in `pyproject.toml` and deployed with `pywrangler`;
  pure-Python PyPI packages and Emscripten/Pyodide wheels are supported, only
  async HTTP libraries (aiohttp, httpx) work, and "WebAssembly support for
  Python packages is still in early stages."
  ([Python packages](https://developers.cloudflare.com/workers/languages/python/packages/))
  This is a Pyodide/WASM environment, not CPython-on-Linux: no native wheels
  outside the curated set, no subprocesses.
- **Python Workflows SDK is open beta** (since 2025-08-22; requires
  `python_workers` + `python_workflows` flags). It mirrors the JS API in
  snake_case with a decorator form (`@step.do("name")`), `step.sleep`,
  `step.sleep_until`, `step.wait_for_event`, `asyncio.gather` for
  concurrency, and — notably — a **declarative DAG API**: steps declare
  dependencies via parameter names (or legacy `depends=[...]`) with
  `concurrent=True`, and "if a dependency has already completed, it will be
  skipped and its return value will be reused."
  ([Python Workflows SDK](https://developers.cloudflare.com/workflows/python/),
  [Python DAG API](https://developers.cloudflare.com/workflows/python/dag/),
  [Changelog](https://developers.cloudflare.com/workflows/reference/changelog/),
  [Python Workflows blog, 2025-11-10](https://blog.cloudflare.com/python-workflows/))
- **Cloudflare Containers are GA** as of 2026-04-13 on Workers Paid, with
  active-CPU pricing, Docker Hub/registry support, and "easy connections to
  Workers and other bindings." ([Containers GA changelog](https://developers.cloudflare.com/changelog/post/2026-04-13-containers-sandbox-ga/))
  Container classes are Durable Objects: they are configured under
  `durable_objects.bindings`, instances "are spun up on-demand and controlled
  by code you write in your Worker" via
  `getContainer(env.MY_CONTAINER, id).fetch(request)`, and stop after an
  inactivity window (`sleepAfter`). ([Containers overview](https://developers.cloudflare.com/containers/))
  Because a Workflow step can use any binding on `this.env`, a step can drive
  a container through its Durable Object binding — i.e., a step becomes
  "call container over HTTP/RPC and await the result."
- Container instance types range from `lite` (1/16 vCPU, 256 MiB, 2 GB disk)
  to `standard-4` (4 vCPU, 12 GiB, 20 GB disk); custom types up to 4 vCPU /
  12 GiB / 20 GB. Account defaults: 1,500 concurrent vCPU, 6 TiB concurrent
  memory, 30 TB concurrent disk, 50 GB total image storage, image size bounded
  by instance disk. ([Container limits](https://developers.cloudflare.com/containers/platform-details/limits/))

### 4. Dynamic vs static control flow

- The step graph is **data-dependent by construction**: it is whatever the
  imperative `run()` executes. There is no declarative graph artifact in JS;
  the engine discovers steps by replaying the function.
  ([Workers API reference](https://developers.cloudflare.com/workflows/build/workers-api/),
  [engine blog](https://blog.cloudflare.com/building-workflows-durable-execution-on-workers/))
- Parallel fan-out is `Promise.all([...step.do(...)])`; the rules page shows
  this as the recommended pattern, with state "exclusively comprised of step
  returns." `Promise.race`/`Promise.any` must themselves be wrapped in a
  `step.do` for cache consistency. ([Rules of Workflows](https://developers.cloudflare.com/workflows/build/rules-of-workflows/))
- There is **no native map/batch primitive within an instance** in the JS SDK;
  fan-out is manual promise composition (the Python SDK's `concurrent=True`
  DAG helper is the closest declarative construct).
  ([Python DAG API](https://developers.cloudflare.com/workflows/python/dag/))
  Docs publish no per-instance concurrent-step cap; practical ceilings are
  the step limit (1,024–25,000), the 6-simultaneous-connection Worker limit,
  and subrequest limits. ([Limits](https://developers.cloudflare.com/workflows/reference/limits/),
  [Workers platform limits](https://developers.cloudflare.com/workers/platform/limits/))
  Cross-instance fan-out exists via idempotent `createBatch` (≤100 per call).
  ([Workers API reference](https://developers.cloudflare.com/workflows/build/workers-api/))
- The dashboard visualizer reconstructs a diagram from execution: unawaited
  promises get entry numbers (`starts`) and resolution points (`resolves`) to
  infer which steps ran concurrently — i.e., the graph is derived from a
  trace, not from a definition. ([Visualizer](https://developers.cloudflare.com/workflows/build/visualizer/))

### 5. Deployment and packaging

- Workflows are declared in `wrangler.jsonc` under `workflows`: `name`,
  `binding` (JS variable), `class_name` (exported class), optional
  `script_name` for cross-script bindings ("including Workflows defined in
  other Workers projects within your account"), and `schedules` (cron
  expressions). ([Trigger Workflows](https://developers.cloudflare.com/workflows/build/trigger-workflows/))
- Triggering: Workers bindings (from fetch handlers, queue consumers,
  scheduled handlers, Durable Objects), the REST API
  (`POST /accounts/{a}/workflows/{name}/instances`, plus `/batch`), `wrangler
  workflows trigger`, and cron schedules. There is no direct
  queue-to-workflow binding; queues trigger via a consumer Worker calling the
  binding. ([Trigger Workflows](https://developers.cloudflare.com/workflows/build/trigger-workflows/),
  [SDK api.md](https://github.com/cloudflare/cloudflare-typescript/blob/main/src/resources/workflows/api.md))
- Code ships as a Worker bundle (3/10 MB gzipped, 64 MB uncompressed); a
  workflow shares the account's Worker script limits.
  ([Limits](https://developers.cloudflare.com/workflows/reference/limits/),
  [Workers platform limits](https://developers.cloudflare.com/workers/platform/limits/))
- **Versioning.** The REST API has a per-workflow `versions` resource
  (`list`, `get`, and a `graph` endpoint per version), and every instance
  carries a `version_id` and `trigger_source` (`api | binding | event |
  cron`). ([SDK api.md](https://github.com/cloudflare/cloudflare-typescript/blob/main/src/resources/workflows/api.md),
  [SDK instances.ts](https://github.com/cloudflare/cloudflare-typescript/blob/main/src/resources/workflows/instances/instances.ts))
  The docs do **not** publish semantics for what code an in-flight instance
  executes after a redeploy (no pinning guarantee is documented); the launch
  blog mentions only gradual engine upgrades. Since step results are cached
  by name and `run()` is re-executed against that cache, changing step names,
  order, or branching logic mid-flight can mis-match the cache — the same
  hazard class as Temporal workflow versioning, but without a documented
  patching mechanism. (Inference from
  [Rules of Workflows](https://developers.cloudflare.com/workflows/build/rules-of-workflows/) and
  [engine blog](https://blog.cloudflare.com/building-workflows-durable-execution-on-workers/);
  treat as a risk requiring empirical validation.)

### 6. Secrets and configuration

- Per-Worker secrets and vars are available on `this.env` inside the workflow
  class, alongside all other bindings (KV, R2, D1, DO, service bindings), so
  steps access secrets the same way any Worker does.
  ([Workers API reference](https://developers.cloudflare.com/workflows/build/workers-api/),
  [Rules of Workflows](https://developers.cloudflare.com/workflows/build/rules-of-workflows/))
- **Secrets Store** (account-level, centralized secrets) is in **open beta**
  (docs updated 2026-08-14). Workers bind via `secrets_store_secrets`
  (binding, `store_id`, `secret_name`) and read with
  `await env.BINDING.get()`; deployment requires Super Administrator or
  Secrets Store Deployer roles. It differs from classic per-Worker
  `wrangler secret put` secrets by being account-scoped and centrally
  managed. ([Secrets Store](https://developers.cloudflare.com/secrets-store/),
  [Workers integration](https://developers.cloudflare.com/secrets-store/integrations/workers/))

### 7. Storage

- **R2** exposes an S3-compatible API: Head/Get/Put/Delete/Copy object,
  ListObjectsV2, and full multipart upload (Create/UploadPart/Complete/Abort,
  UploadPartCopy). Region is `auto` (`us-east-1` aliases to it). Unsupported:
  ACL grants, object lock/retention, KMS server-side encryption, object
  tagging. Checksums: CRC-64/NVME full-object; CRC-32/CRC-32C/SHA-1/SHA-256
  composite. ([R2 S3 API](https://developers.cloudflare.com/r2/api/s3/api/))
  This is the natural fit for a content-addressed artifact store: Massive can
  reuse its existing S3-oriented artifact protocol against R2 with digest
  verification via SHA-256 checksums, and the same store is reachable both
  from Workers (R2 binding) and from external runners (S3 API).
- KV is eventually consistent edge cache storage; D1 is serverless SQLite;
  Durable Objects provide per-object strongly consistent SQLite-backed state
  (and are what Workflows itself is built on).
  ([Engine blog](https://blog.cloudflare.com/building-workflows-durable-execution-on-workers/),
  [Python Workers bindings list](https://developers.cloudflare.com/workers/languages/python/))
  None of these fit bulk artifacts; DO/D1 suit run metadata if Cloudflare-side
  metadata were ever needed.

### 8. Observability

- `wrangler workflows` CLI: `list`, `describe`, `trigger`, `instances list`
  (filter by status), `instances describe` ("see its logs, retries and
  errors", with step-level output, truncated at 5,000 chars by default),
  `instances pause/resume/terminate [--rollback]/delete`, all with `--local`
  for dev. ([Wrangler workflows commands](https://developers.cloudflare.com/workers/wrangler/commands/workflows/))
- REST: instance `get` returns `status`, `params`, `output`, `success`,
  `start`/`end`, `version_id`, `trigger`, and a `steps` array where each step
  entry has `name`, `start`/`end`, `attempts` (each with start/end, success,
  error), `output`, `success`, and `type` (`step | rollback | sleep |
  termination | waitForEvent`); a separate `/step` endpoint returns full
  untruncated output per attempt. Step config supports `sensitive: 'output'`
  to redact outputs from logs and step-output APIs.
  ([SDK instances.ts](https://github.com/cloudflare/cloudflare-typescript/blob/main/src/resources/workflows/instances/instances.ts),
  [SDK api.md](https://github.com/cloudflare/cloudflare-typescript/blob/main/src/resources/workflows/api.md))
- Metrics: dashboard per-Workflow/per-instance analytics plus the
  `workflowsAdaptiveGroups` GraphQL dataset (31-day retention) with
  `stepName`/`stepCount` dimensions and event types `STEP_START`,
  `STEP_SUCCESS`, `STEP_FAILURE`, `SLEEP_START`, `SLEEP_COMPLETE`, retry and
  rollback events, and `wallTime`. ([Metrics and analytics](https://developers.cloudflare.com/workflows/observability/metrics-analytics/))
- The dashboard renders an execution-derived diagram of an instance,
  including concurrency inferred from promise start/resolve ordering.
  ([Visualizer](https://developers.cloudflare.com/workflows/build/visualizer/))

## Implications for a static-plan compiler backend

Everything below is inference from the sourced facts above.

### The models are inverted, and that inversion is favorable

Argo consumes a static graph and schedules containers against it; Cloudflare
consumes imperative code and derives durability by replaying it. A Massive
plan sits upstream of both: it is a deterministic data structure. Replaying a
**fixed interpreter over a fixed plan** trivially satisfies Cloudflare's
determinism rules — branching on plan structure and on persisted step outputs
is exactly the "conditional logic based only on deterministic values" the
rules page mandates. Massive does not need to solve the hard general problem
(making arbitrary user code replay-safe) because user code never runs in the
`run()` function; only the interpreter does.

### Lowering shape: interpreter over the plan, not codegen

Two options:

1. **Generic TS interpreter Worker** — one published Worker containing a plan
   interpreter; the compiled protobuf plan is embedded in the bundle (or
   fetched from R2 in step 0 and re-fetched deterministically by digest).
   Node IDs become step names (`{node_id}#{shard}` for map elements), which
   satisfies "deterministic step names" and keeps names stable across plan
   versions.
2. **Codegen** — emit a bespoke `run()` per plan.

The interpreter is strictly better here: the plan is already the IR, codegen
adds a second artifact to version, and Cloudflare's undocumented
redeploy-vs-in-flight semantics (§5) make *minimizing code churn per plan
change* valuable — a stable interpreter version plus a content-addressed plan
blob means the Worker code only changes when the interpreter does. This is
the same "small, versioned, deterministic plan interpreter" shape the
Metaflow note already proposed for a future Temporal target; Cloudflare is
effectively the first buyer of that abstraction.

### What maps cleanly

- **Steps** → `step.do` with name = plan node ID.
- **Retry/backoff/timeout contracts** → `WorkflowStepConfig` maps directly
  (limit/delay/constant-linear-exponential backoff/timeout), with
  `NonRetryableError` for plan-declared non-retryable failure classes.
  Massive's retry vocabulary should stay within this intersection (it is also
  expressible in Argo `retryStrategy`).
- **Static fan-out / map** → `Promise.all` over `step.do` calls generated
  from the plan's map node; width is data-dependent but derived from a
  persisted upstream step output, which is replay-safe. Caps: step budget
  (10k–25k paid) bounds total map elements per instance; very wide maps
  should lower to `createBatch` child instances (≤100/call, idempotent)
  instead — a decision the compiler can make from plan metadata.
- **Decisions** → replay-safe branching in the interpreter on persisted step
  outputs.
- **Waits/timers/human-in-the-loop** → `step.sleep`, `step.sleepUntil`,
  `step.waitForEvent` + `sendEvent`, which have **no Argo equivalent of
  comparable quality** — this is the feature that would motivate the backend.
- **Introspection** → instance/steps REST shape (per-step attempts, timings,
  outputs, `version_id`) is rich enough to back Massive's run-metadata reads
  without extra bookkeeping.

### What does not map

- **Python steps / containers as executors.** Worker steps are 128 MB
  isolates with ≤5 min CPU; Python Workers are Pyodide-on-WASM (beta, no
  native wheels, no subprocesses) — not a substitute for Massive's
  container-based Python step contract. The viable pattern is: workflow step
  = thin driver that calls a Cloudflare Container (GA, ≤4 vCPU/12 GiB/20 GB,
  1,500 concurrent vCPU per account) via its Durable Object binding, or any
  external executor over HTTP. Steps have unlimited wall-clock while awaiting
  I/O, so "await the container" works, but the compiler must emit an
  invocation protocol (start, poll/await, collect exit status) rather than
  Argo's "the step *is* the container." Container resource limits are far
  below Kubernetes-scale nodes; heavy steps stay on Argo.
- **Payload transport.** 1 MiB per non-stream step return and per event
  payload, 1 GB persisted state per instance. Massive must pass artifact
  references (R2 keys + digests) between steps, never values — which is
  already the plan's artifact-protocol posture, so this constraint mostly
  confirms the design rather than fighting it.
- **Resource contracts.** Plans that declare CPU/memory/GPU requirements have
  no Workflows expression. Lowering must either map resource classes to
  Container instance types (lite…standard-4) or reject the plan; GPU
  contracts are unloweragble on this target today.
- **Source/code transport.** Argo lowering ships an image reference;
  Cloudflare lowering ships (a) the interpreter Worker, (b) the plan blob,
  and (c) container images pushed to a registry with 50 GB account image
  storage. That is a third packaging pipeline the compiler must own.
- **Definition versioning for in-flight runs.** Argo `WorkflowTemplate`
  versions are explicit resources; Cloudflare exposes `version_id` per
  instance but documents no pinning semantics. Until validated empirically,
  Massive should treat plans as immutable per workflow name (e.g., encode
  plan digest into the Workflow name or only route new instances to new
  deployments) rather than relying on safe hot-upgrades.

### Abstraction choices this forces now vs later

**Now** (cheap to adopt, expensive to retrofit):

1. **Reference-passing only between steps** — required by the 1 MiB limit,
   harmless on Argo. Values live in the content-addressed store; the plan
   carries references.
2. **Backend-neutral retry vocabulary** limited to
   {limit, delay, constant/linear/exponential backoff, timeout,
   retryable-vs-not} — the intersection of `WorkflowStepConfig` and Argo
   `retryStrategy`.
3. **Executor as a first-class plan concept separate from orchestrator** —
   each step names an execution contract (isolate-inline vs container vs
   external), because on Cloudflare "step" and "compute" are different
   machines. This is the split the v2 direction already endorses.
4. **Deterministic, stable node IDs** in the protobuf plan (they become both
   Argo template names and Cloudflare step-cache keys).

**Later** (defer until a Cloudflare backend is actually built):

- The interpreter itself, event/`waitForEvent` plan nodes (Argo has no
  counterpart; adding them to the IR before a consumer exists is
  speculative), rollback/compensation nodes (Cloudflare-only today), the
  child-instance sharding strategy for very wide maps, and any reliance on
  Secrets Store (open beta) over per-Worker secrets.

**Net assessment.** Cloudflare Workflows is a credible second orchestrator
target whose replay model is *easier*, not harder, to compile to than it
first appears — precisely because Massive's plans are static. The gating
dependencies are practical, not conceptual: container-backed step execution
(GA since 2026-04-13, resource-constrained), artifact-reference transport
(already planned), and clarity on redeploy semantics for in-flight instances
(undocumented; must be tested before production use).
