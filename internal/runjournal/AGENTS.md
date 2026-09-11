# Run journals

This package owns the current journal model and reader. The writer in
`../orchestrator` uses these types; keep one model for both directions.
The structural contract is `../../conformance/schema/run-manifest.schema.json`.

Preserve semantic validation after schema validation: terminal attempt status,
dense source-indexed map items, and failed-map not-started items are related
invariants. Exercise records emitted by real successful and failed executions.
Inspection must read existing artifacts without importing author code, writing
store bytes, or scanning other projects. Reject obsolete journal transports.
