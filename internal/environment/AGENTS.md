# Dependency preflight

This package checks an existing Python environment against a workflow project.
It never installs packages. Read `../../docs/spec/environment-materialization.md`
before changing findings, verification levels, or the probe contract.

The Python probe (`massive.environment`) must run with `-I` and read metadata
only. Never import workflow modules before every finding is reported. Validate
probe output against `conformance/schema/environment-probe.schema.json`, and
update that schema, the Pydantic models, and the Go structs together. The probe
imports the installed SDK. A broken SDK installation is an error with an
installation command, not a finding.

uv checks run offline with `UV_PROJECT_ENVIRONMENT` set to the probed prefix.
Exit 1 means "not current"; any other failure is an error. Findings may carry
only package names and versions parsed from uv's planned-change lines. Raw uv
output can contain index URLs with credentials, so never report or persist it.
Lock checks keep exact `uv sync --check` semantics: extra packages fail. A
missing `uv` with a lock present fails with a fix line; never downgrade the
verification level silently.

Test against real environments built with `uv venv` and `uv sync --locked`.
`conformance/workflows/python-locked/uv.lock` is generated with `uv lock`;
regenerate it when the SDK's dependencies or version change, and never edit it by hand.
