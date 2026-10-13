# Run journals

This package owns the current journal model and reader. The writer in
`../orchestrator` uses these types; keep one model for both directions.
The structural contract is `../../conformance/schema/run-manifest.schema.json`.

Preserve semantic validation after schema validation: terminal attempt status,
dense source-indexed map items, and failed-map not-started items are related
invariants. Exercise records emitted by real successful and failed executions.
Inspection must read existing artifacts without importing author code, writing
store bytes, or scanning other projects. Reject obsolete journal transports.

The only transport is v6/json-v6. `environment` is required: null when the
frontend has no dependency preflight, otherwise both identity hashes and the
content-addressed record. Older journals are rejected; rebuild with the current SDK and create a new run. Failed and cancelled runs require a root
diagnostic and no unfinished steps or map items. Undispatched work has no
attempts. Termination preserves completed publications and decision records.
A terminal journal may carry `exitHook`: one attempt, `succeeded` or `failed`,
written after the run's own terminal state. It never changes the run status.
Fuzz parsing and terminalization together: accepted records must round trip,
termination must be idempotent, and successful artifact references must survive.

A map step that collects item failures carries `itemFailures: "collect"`;
only such a successful map may contain `failed` items. Keep the field on every
map-step status variant of the schema.

Only failed runs may carry the optional typed `failure` cause. A fatal cause's
node must name a failed dispatched step or map; preserve its safe message and
optional type independently of sibling cancellation diagnostics.
