# Validation and packaging scripts

Use the actual built wheel in distribution tests and container conformance.
Resolve tools relative to the active checkout. Pin infrastructure/tool versions
and immutable images used by reproducible cluster gates.

The Argo gate owns a disposable cluster and captures workflow, controller, and
pod diagnostics before cleanup on failure. Preserve that evidence path when
changing the script. Cleanup must target only resources created by that run.

Test the real public entrypoints. A language adapter succeeding from an editable
checkout is not evidence that the installed wheel dispatches or ships it correctly.
