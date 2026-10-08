# Local execution

Execution cancellation and durable outcome publication have different lifetimes.
Stop dispatch using the outer run context. Reconcile successful invocation
outcomes and verify their artifacts with a bounded detached context, then write
the terminal journal with a separate bounded context. Do not adopt orphan output
manifests from invocations that did not report success.

An invoker can cancel sibling workers after an infrastructure failure. Classify
the root using the outer run context and error cause; sibling cancellation must
not turn an infrastructure failure into an operator cancellation. Preserve
source-indexed completed map items while terminalizing started and queued work.

Use real processes and stores for cancellation tests, with filesystem or socket
handshakes to prove author code started. Fuzz partial outcome identity/order and
journal invariants without replacing the invoker with a mock API.
`FuzzGeneratedGraphExecution` runs generated graphs with an in-process executor
that implements the runner side of the descriptor contract against the real
datastore and artifact runtime. Its oracle must not assume a node order: the
run-wide worker budget may execute independent branches concurrently. When a
rare fault combination matters, commit a named seed under `testdata/fuzz`.

Never persist raw runner output or arbitrary context cancellation causes in the
root journal diagnostic. Return details to the caller; shared artifacts contain
safe lifecycle summaries.

Apply verification deadlines per invocation, not once per arbitrarily large map.
Reconcile all reported dispatches before verification can fail. Publish the map
journal after reconciliation instead of rewriting the full item list for every
item, which makes publication quadratic in map cardinality.

Source snapshots must remain read-only after installation. macOS requires the
staging directory itself to be writable during rename; restore its read-only
mode before reporting installation success. Check snapshot containment before
reuse, staging, installation, or removal.
Resolve symlinks in existing ancestors even when the target does not exist.

Retries append attempts; never rewrite an earlier attempt or reuse its output
slot. Only failed outcomes with a retryable exit schedule another attempt, and
only while attempts remain. Cancellation, infrastructure errors, and output
verification failures end the step. Apply `timeoutSeconds` per attempt as an
invoker deadline that yields a retryable failure, not a run cancellation. The
backoff wait must observe the run context.

Python tasks run as `python -I -m massive.runner`. Local runs pin the interpreter
that passed dependency preflight; do not resolve `MASSIVE_PYTHON` again per task.
Isolated Python attempts extract the verified source archive and run the full
dependency preflight with the executor's interpreter before invoking the runner.
Only findings and `environment.ProjectError` become a `PreflightError` (CLI
exit 68, never retried); return context cancellation and other infrastructure
errors unchanged so a terminated pod is retried. A
passing attempt writes `environment.json` beside its output manifest.
