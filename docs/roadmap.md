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

Next:

- Exercise installation and execution on clean Linux CI runners.
- Add dependency preflight and record the realized environment identity. Use
  standard Python project metadata and lockfiles, not a second dependency language.
- Add object-store source transport beyond Argo's 700 KiB embedded limit, and
  artifact references for values too large for Argo parameters.

Acceptance: a clean checkout runs a linear workflow, a resource-bearing fan-out,
and a conditional workflow without manually repairing the environment.

See [environment materialization](spec/environment-materialization.md) for the
requirements/realization distinction and [the Python guide](../packages/python/README.md)
for what works today.

## 2. Make execution predictable

- Expose a run-wide worker budget. Local maps already use parallel subprocesses;
  independent DAG branches should share the same budget rather than multiply it.
- Add per-task timeout, explicit retry policy, attempt accounting, and cancellation
  of child-process trees.
- Persist bounded task logs and structured lifecycle events; bring inspection into
  the shipped Go CLI before retiring the Deno CLI.
- Treat external side effects separately from immutable artifact publication:
  retrying an output write does not make a task's API calls exactly-once.

Acceptance: a deliberate item failure, timeout, and cancellation produce useful
diagnostics and terminal journals without leaked workers or ambiguous outputs.

## 3. Recover expensive work

Add selective resume when representative run cost warrants it. Require matching
plan/input identity, verify completed artifacts, and rerun only eligible incomplete
work. This is not a general metadata database or cross-run result cache.

## 4. Complete Argo for the supported graph model

Static DAGs and finite single-step maps lower to Argo today. Remaining work:

- decisions/selects;
- deployment-bound secret references;
- larger source/value transport;
- real-cluster conformance runs, including empty maps and failed items.

Reject unsupported requirements instead of silently weakening them. Schema
validation and isolated runner tests do not replace a live cluster execution gate.

## Keep the core small

- Preserve explicit typed dataflow, immutable artifacts, and stable task/item identity.
- Keep dependency realization separate from secrets, network policy, and scheduling.
- Use Pydantic Graph's explicit fan-out/join DX as inspiration, not its in-memory
  state or runtime. Ordered map collection followed by a normal typed step is the
  current reduction model.
- Keep domain models, tools, billing, registries, triggers, and UI policy outside core.
- Maintain one supported Python CLI path. Do not add features to the legacy CLI
  merely to keep two implementations in sync.
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
