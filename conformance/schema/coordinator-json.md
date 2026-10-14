# Coordinator v1 projection

`coordinator.proto` imports plan v1 identity, schema, contract and artifact records,
runner v3 start descriptors, and the exact shared `ItemFailure` record. All
scalars preserve presence. JSON uses camelCase and an explicit `kind`;
`protocolVersion` is always 1. The frozen schema rejects unknown fields,
versions and variants, and enforces exactly one result alternative. Frames use
canonical-json-v0, UTF-8 NDJSON, and a 1 MiB limit including the newline. Empty
repeated fields are omitted (commands always contain their two schema records).

Turn 0 runs the entry to quiescence. `CoordinatorQuiescent` closes its command
sequence. Each `InvokeResult` increments the turn by one, and the coordinator
runs all ready continuations to quiescence before another result is delivered.
Full canonical command bytes, including resolved schema/environment records,
are compared on replay. Logs travel on stderr; stdin/stdout are reserved before
loading author modules, including redirection of native writes to fd 1.

Run journal v7 carries `runKind`, and coordinator journals reference an immutable
content-addressed replay snapshot. Publishing that reference is the atomic commit
point for attempts, commands, settled results, and delivery order. A snapshot is
independently versioned and inspection never imports coordinator code.
