# Shipped CLI

This is the shipped Go-backed CLI for both language adapters. Put reusable
behavior in `../../internal/controlplane`; keep command parsing and rendering here.
The compiler and spec-runner commands elsewhere in `cmd/` are conformance tools.

Kong owns argument validation. Parse errors exit 2 and write only to stderr.
Machine-readable output must remain parseable even when author code logs.
Adapter dispatch follows the entrypoint language, including under the Python
wheel launcher. Exercise `../../scripts/test-python-distribution.sh` after
changing language dispatch, packaging, or runtime command selection.
