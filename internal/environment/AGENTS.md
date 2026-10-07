# Dependency preflight

This package checks an existing Python environment against a workflow project.
It never installs packages. Read `../../docs/spec/environment-materialization.md`
before changing findings, verification levels, or the probe contract.

The Python probe (top-level `massive_environment`) must run with `-I` and read
metadata only. It must not import workflow modules or the `massive` SDK, which
keeps it fast and lets a broken SDK dependency surface as a lock finding.
`conformance/schema/environment-probe.schema.json` is generated from the probe's
Pydantic model; a Python test enforces this. Update the Go structs with it.
Exit 2 from the probe means unreadable project metadata: surface its one line.

uv checks run offline with `UV_PROJECT_ENVIRONMENT` set to the probed prefix and
`--python` set to the probed executable. Pass only allowlisted `UV_*` variables
(cache, index, and resolution inputs); variables such as `UV_FROZEN`, `UV_NO_DEV`,
or `UV_PYTHON` would change the check's meaning. Exit 1 means "not current"; any
other failure is a `UV_FAILED` finding that keeps the probe's findings. Findings
may carry only package names and versions parsed from uv's planned-change lines.
Raw uv output can contain index URLs with credentials, so never report or persist it.
The lock check is `--inexact --no-default-groups --no-install-project
--no-install-package massive-workflows`: the locked runtime set must be
installed, while groups, extras, or tools beside it are allowed. The project
ships as an archive and is never installed. The SDK's recorded image source (a
local wheel) never matches its lock source, so the probe compares only its
locked version. Name direct-reference packages without their URL. Fix lines must
name the checked environment unless it is `<root>/.venv`. A missing `uv` with a
lock, or membership in a parent uv workspace (whether or not the member has its
own lock), is a finding. Requirements leave the probe without URL credentials,
queries, fragments, or local paths. Never downgrade the verification level
silently; a project without `[project]` metadata is `UNDECLARED`.

`identity.go` owns the `python-requirement` and `existing-python` recipes. Only
a report without findings has a `Record`. Never add interpreter paths, install
locations, or RECORD hashes to the realization identity. Changing a recipe means
bumping its recipeVersion and regenerating the hashing vectors from the
implementation; `conformance/schema` re-derives them independently.

Scope `Execution` is the full check for an interpreter that runs tasks.
`Emission` (`massive build`) drops installed-requirement findings and the
`uv sync` check, keeps interpreter safety, the SDK release, workspace locks,
and `uv lock --check`, and never yields a record. The container is checked
per attempt instead.

Test against real environments built with `uv venv` and `uv sync --locked`.
`conformance/workflows/python-locked/uv.lock` is generated with `uv lock`;
regenerate it when the SDK's dependencies or version change, and never edit it by hand.

Launch the probe and uv through `taskprocess.RunTo` so descendants are owned and
inherited-pipe drainage is bounded, as for the frontend. A cancelled context is
returned as the error, never reported as a finding.
