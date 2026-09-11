# Validation and packaging scripts

Resolve tools relative to the active checkout. Pin infrastructure/tool versions
and immutable images used by reproducible cluster gates.

The Argo gate owns a disposable cluster and captures workflow, controller, and
pod diagnostics before cleanup on failure. Preserve that evidence path when
changing the script. Cleanup must target only resources created by that run.

Use the actual built wheel in distribution and container tests, and exercise
both language entrypoints. Editable SDK imports do not prove packaging.
