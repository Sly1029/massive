# Roadmap

Massive's next milestone is **reliable typed Python workflows on a laptop or in
customer-owned CI, without a hosted control plane**. Argo remains the distributed
target. Native GitHub Actions compilation and additional targets are deferred.

This narrows the sequencing in [Workflow Platform v2](spec/workflow-platform-v2.md),
not its immutable dataflow or portable invocation contracts. Python is the priority;
the existing TypeScript frontend remains supported, but feature parity is not a
release gate.

## 1. Package a real workflow

Implemented:

- The platform wheel includes the matching Go CLI and Python runner.
- A workflow directory may select nested Python modules and resource files with
  `[tool.massive.source].include` in its own `pyproject.toml`.
- The source identity includes the selected files, `pyproject.toml`, and `uv.lock`
  when present. The runner loads the same archived files locally and remotely.
- Source packages reject selected symlinks and path escapes.
- Argo's `object-store-v0` runtime transport carries source packages beyond the
  700 KiB embedded ConfigMap limit: `massive publish` uploads verified archives
  and pods verify them by pinned digest. Source packages may hold 16,384 files
  and 256 MiB.
- JSON values above 4 KiB cross Argo parameters as content-addressed datastore
  references, hydrated before typed step inputs; local execution passes the
  same values as artifacts. Multi-MB values flow through steps, decisions,
  selects, and maps on both targets.
- Python `Blob` and `Tree` references transport files through the existing
  filesystem/S3 datastore, including ordered subprocess maps. Hydration is
  invocation-local; explicit snapshots publish mutations.
- Top-level typed functions register directly through `add()` and `map()`.
  `StepContext[Input]` exposes only implemented invocation capabilities.
- Shrinking Python graph generators exercise nested decisions through the Go
  compiler; continuous Go fuzzing executes generated graphs (nested decisions,
  maps, merges, child-graph calls, retries, timeouts, and cancellation) against
  a reference interpreter, rejects rehashed semantic mutations, checks plan
  identity metamorphically, and lowers every generated plan to Argo.
- Dependency preflight checks the launching interpreter before importing author
  code: `requires-python`, direct requirements, shadowed distributions and
  modules, the SDK release, and with `uv.lock` an offline check that the locked
  runtime set is installed. Local run journals (v5) bind the content-addressed
  `RealizedEnvironment` and its requirement/realization identities, shown by
  `massive inspect --environment`. Each Argo attempt checks its image against the
  archived project before author code runs (exit 68, not retried) and records
  its realization; `massive build` checks only emission and lock freshness. `massive env check --json` exposes it to CI. The
  checked interpreter runs emission and every local task in isolated mode.

Next:

- Exercise installation and execution on clean Linux CI runners.

Acceptance: a clean checkout runs a linear workflow, a resource-bearing fan-out,
and a conditional workflow without manually repairing the environment.

See [environment materialization](spec/environment-materialization.md) for the
requirements/realization distinction and [the Python guide](../packages/python/README.md)
for what works today.

## 2. Make execution predictable

- Expose a run-wide worker budget. Local maps already use parallel subprocesses;
  independent DAG branches should share the same budget rather than multiply it.
- Local task subprocesses now own ordinary descendants through OS process groups
  or Windows jobs, with bounded pipe drainage and captured output. Context
  cancellation persists terminal journals and retains verified completed map
  outputs. SIGINT and SIGTERM to the CLI cancel the run through the same path;
  SIGKILL still leaves the journal unterminated.
- Per-task timeout, bounded retry with backoff, a non-retryable author signal,
  and per-attempt journal accounting are part of `ExecutionContract` and run on
  both the local and Argo targets.
- A map may collect item failures (`item_failures="collect"`): after retries,
  an item's exception, timeout, `NonRetryableError`, or death by signal (an
  out-of-memory kill or a native crash) becomes a typed outcome instead of
  failing the map, so siblings' results survive and a downstream step decides.
  The local executor journals each failed item; Argo lowering is next.
- Persist bounded task logs and structured lifecycle events. Inspection is available
  in the shipped Go CLI; the Deno CLI has been removed.
- Treat external side effects separately from immutable artifact publication:
  retrying an output write does not make a task's API calls exactly-once.

Acceptance: a deliberate item failure, timeout, and cancellation produce useful
diagnostics and terminal journals without leaked workers or ambiguous outputs.

## 3. Recover expensive work

Add selective resume when representative run cost warrants it. Require matching
plan/input identity, verify completed artifacts, and rerun only eligible incomplete
work. This is not a general metadata database or cross-run result cache.

## 4. Complete Argo for the supported graph model

DAGs, exhaustive decisions/selects, and finite single-step maps lower to Argo.
Live conformance covers nested inactive branches, empty maps, failed items, and
deployment-bound native Secret references. Cloud workload identity remains a
separate infrastructure gate.
Source packages beyond the embedded limit use `object-store-v0`.
Remaining work:
- representative application images, and map fan-outs wider than 341 items
  (one Argo parameter of collected item envelopes).
- A narrower credential binding for control pods; today one storage binding
  serves every pod.

Reject unsupported requirements instead of silently weakening them. Schema
validation and isolated runner tests do not replace a live cluster execution gate.

## Artifact release gates

- Blob/Tree transport is exercised across live Argo pods against MinIO. Validate
  workload identity in a cloud cluster as a separate infrastructure gate.
- Add streaming transfer and scratch budgets for repository-sized trees; current
  file operations buffer one file at a time.
- Cache extracted source archives per node, keyed by archive digest. Today every
  Argo runner pod downloads and extracts its full source package.
- Preserve reference closure before adding any retention or selective resume.
- Keep repository fetching, revision metadata, service clients, and domain policy
  in application packages composed over typed inputs and reusable contracts.

## Keep the core small

- Preserve explicit typed dataflow, immutable artifacts, and stable task/item identity.
- Keep dependency realization separate from secrets, network policy, and scheduling.
- Use Pydantic Graph's explicit fan-out/join DX as inspiration, not its in-memory
  state or runtime. Ordered map collection followed by a normal typed step is the
  current reduction model.
- Keep domain models, tools, billing, registries, triggers, and UI policy outside core.
- Maintain one shipped Go-backed CLI for both language adapters. The Deno CLI and
  its emit/toolchain caches are removed; add behavior at the shared control plane.
- Remove unemittable public surface rather than advertising placeholder behavior:
  TypeScript channels, publication fields, and mutable step state are removed.

## Deliberately deferred

- Native GitHub Actions and other new backend targets.
- Generalized secret-provider, mediation, placement, and middleware frameworks.
- Multiple dependency builders: prove a locked Python environment and an immutable
  container recipe first.
- Multi-step/nested map scopes, first-class reducers, races, streaming, and cycles.
- Hosted scheduling, dashboards, named artifact catalogs, and legacy-engine compatibility.

Revisit a deferred feature only with a concrete workflow and a functional acceptance
test. A normal CI job invoking `massive run` does not need a new compiler target.

Graph artifacts use only current IR 0.3. Both frontends emit it for all graph
shapes; obsolete specs and plans must be rebuilt, with no compatibility reader.

CLI retirement removes the Deno-only emit cache, binary build cache, store-prefix
flags/environment aliases, and bare `-` input spelling. Use an explicit `--store`
root and `--input` or `--input-file`; no compatibility aliases remain. Local
execution in both languages is trusted application execution, not a sandbox.

Python authoring keeps Pydantic-style explicit typed edges, decisions, and maps.
Registration preserves ordinary functions; execution contracts belong to their
uses in a graph. Dependency generics, step decorators, and container-plan aliases
are removed. Invocation descriptors use only v3/json-v3, without channel fields;
old descriptors must be rebuilt. Materialization selections have one supported
container field rather than a single-variant union, preserving their JSON shape.
