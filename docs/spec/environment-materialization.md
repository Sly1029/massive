# Workflow Packaging and Environment Materialization

Status: accepted direction; implementation status is explicit below.

## Three things, not one overloaded environment

| Concern | Question | Owner |
| --- | --- | --- |
| Source package | Which workflow modules and resource bytes execute? | Workflow author |
| Dependency environment | Which Python, distributions, native tools, and platform are needed? | Workflow author; materializer realizes them |
| Deployment bindings | Where does it run, and how are credentials and policy supplied? | CI or platform operator |

An environment is **not** an Argo spec with a local emulation. Nor should an Argo
secret change a dependency build-cache key. A deployment selects how to realize
requirements without changing the graph's dataflow.

## What works today

The Python frontend reads `pyproject.toml` adjacent to the workflow entrypoint.
It does not search parent directories. A directory is the package root; multiple
workflow exports in it share its packaging configuration.

```toml
[project]
name = "example-analysis"
version = "0.1.0"
requires-python = ">=3.12"
dependencies = ["massive-workflows==0.1.0", "httpx>=0.28,<1"]

[tool.massive.source]
include = ["*.py", "analysis/**/*.py", "analysis/prompts/*.txt", "rules/*.yaml"]
```

The default include is `["*.py"]`. Custom includes replace that default and must
include the entrypoint. `pyproject.toml` and `uv.lock`, when present, are always
included so dependency/configuration edits change source identity. The source
archive contains regular files only; selected symlinks and path escapes fail.
Use explicit allowlists rather than `**/*`: credentials, `.env`, virtual
environments, datasets, and caches do not belong in source archives.

Read packaged resources relative to `__file__` or with `importlib.resources`.
Do not depend on the original checkout path or the runner's working directory.
Third-party distributions are installed in the environment, not copied into source.

For a workflow directory with a committed `uv.lock`, run from that directory:

```sh
uv sync --locked
uv run --locked massive run workflow.py --project example/analysis --input '{}'
```

Create/update the lock deliberately with `uv lock`, not as an implicit side effect
of compilation. Each workflow can have its own directory, project metadata,
lockfile, and virtual environment. `massive` uses the launching Python interpreter
for both emission and task execution. It does not install dependencies itself.

For Argo, build an image with the same locked dependencies and Massive version,
then reference its immutable digest using `container(...)`. Argo currently executes
that image; local execution uses the active Python environment, **not** the declared
container. They are not yet verified-equivalent realizations.

## The next small contract

The first portable slice is implemented for **existing immutable containers**.
See [the materialization wire contract](materialization-contract.md). Public
`massive build` now packages portable inputs, independently compiles those inputs,
and emits a separate materialization manifest bound by DeploymentSpec v1.
It verifies source bytes but does not contact an image registry or inspect
installed dependencies. `PINNED_REFERENCE_ONLY` records that limit explicitly.

Reuse `[project]` and `uv.lock` as dependency sources of truth. Do not introduce
another `packages={"httpx": ...}` DSL or copy package resolution into Massive.

An environment requirement should identify:

- Python compatibility and dependency project/lock inputs;
- native tool requirements when a real workflow needs them;
- supported OS/architecture when dependencies constrain them.

A realization should record:

- mode (`existing-python` or an immutable container initially);
- actual interpreter/platform and dependency input digests;
- materializer identity/version;
- verified output identity, such as an OCI digest, where one exists.

An existing-environment check is not a hermetic build and must say so. Missing or
incompatible dependencies should fail before scheduling work, with an actionable
installation command. A container materializer must commit an actual image before
recording an artifact; a recipe is not a built artifact.

Dependency preflight should eventually happen before importing author code. That
requires reading project metadata without graph evaluation. Optional Docker-based
local execution can then reuse an existing image rather than invent a new scheduler.
Neither dependency preflight nor local Docker execution is implemented by this change.

## Identity and sharing

Source and dependency identities serve different purposes:

- A source hash includes workflow code/resources and selected project inputs.
- An environment key includes only dependency/build inputs, platform, and materializer
  version, so unrelated workflows can reuse the same environment.
- Deployment identity includes bindings and execution policy, never secret values.

Including `uv.lock` in a source archive records intended dependencies. It does not
prove that the installed environment matches them, and it is not result-cache safety.
Do not claim reproducibility until realization is verified.

## Secrets are runtime bindings, not packages

See [runtime environment bindings](runtime-environment.md). CI owns obtaining
credentials; Massive should declare requirements, validate that bindings exist,
and expose only the binding needed by the task. It should not become a vault.

Private package-index credentials are build/install-time credentials. CI supplies
them to `uv` or a container build's secret mechanism. Never put them in project URLs,
lockfiles, build arguments, source archives, specs, or image layers.

## Non-goals for this milestone

No generic materializer registry, Nix integration, environment solver, automatic
network installation in the runner, or backend-specific Python dependency syntax.
Prove one locked Python path and one immutable image path before adding adapters.
