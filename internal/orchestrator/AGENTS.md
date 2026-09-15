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

Never persist raw runner output or arbitrary context cancellation causes in the
root journal diagnostic. Return details to the caller; shared artifacts contain
safe lifecycle summaries.

Apply verification deadlines per invocation, not once per arbitrarily large map.
Reconcile all reported dispatches before verification can fail. Publish the map
journal after reconciliation instead of rewriting the full item list for every
item, which makes publication quadratic in map cardinality.

Retries append attempts; never rewrite an earlier attempt or reuse its output
slot. Only failed outcomes with a retryable exit schedule another attempt, and
only while attempts remain. Cancellation, infrastructure errors, and output
verification failures end the step. Apply `timeoutSeconds` per attempt as an
invoker deadline that yields a retryable failure, not a run cancellation. The
backoff wait must observe the run context.
