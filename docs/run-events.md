# Local run events

`massive run --json --events run-events.ndjson` keeps the existing single stdout
result and writes a live, versioned event stream to a separate new file. Progress
uses stderr: `--progress auto|plain|none`, default `auto`. Auto updates one compact
line on a TTY and emits plain lines otherwise. Plain and auto finish with step
durations and total wall time. None retains the previous summary behavior.

An event file is created exclusively, with owner-only permissions. Existing
paths, special descriptors, and `--events -` are rejected. Parent directories must
exist. Each complete JSON object and newline is written immediately without an
application buffer; per-line fsync is unnecessary. A wrapper may tail the file
while the run is active. Delivery errors make the CLI exit unsuccessfully while
preserving the actual workflow status and stdout result. Progress can be disabled
independently of events.

The [JSON Schema](../conformance/schema/run-event.schema.json) defines the only
accepted version (`schemaVersion: 1`). Every event has `type`, `sequence`, `runId`,
`time` (UTC RFC 3339), and `elapsedMs` from the run's monotonic clock. Sequence
numbers start at one and increase across concurrent item callbacks. Durations
are monotonic milliseconds and include output verification; step durations also
include retry backoff. Total wall time begins when the orchestrator enters the
run, includes source preparation and the exit hook, and excludes frontend emission.

| Event | Additional fields |
| --- | --- |
| `run.started` | `planHash`, `nodeCount` (planned graph steps/maps, excluding exit hook) |
| `step.started` | `nodeId` |
| `step.attempt.started` | `nodeId`, optional `itemIndex`, `attempt`, `maxAttempts` |
| `step.attempt.finished` | Same identity, `durationMs`, `status`, `classification`, optional `diagnostic`, `logTail` |
| `map.progress` | `nodeId`, `completed`, `failed`, `total` |
| `step.finished` | `nodeId`, `status`, `durationMs`, optional `diagnostic` |
| `run.finished` | `status`, `durationMs`, optional `failedNode`, `diagnostic`, `exitHook` |

An item's identity is `(runId, nodeId, itemIndex)`: the index is zero-based and
stable in source order. Author item values are never serialized as identities.
Attempts are dense and one-based per step/item. A failed attempt finishes before
its retry starts; retries append journal attempts. The map's unscoped attempt
represents collection and always has `maxAttempts: 1`; scoped item attempts use
the map's retry policy. The exit hook uses ordinary step/attempt events and its
settled status is repeated in `run.finished.exitHook`; hook failure leaves the
run's status unchanged. `persistenceDiagnostic` reports a failed hook journal
rewrite separately.

Events observe the same records that are published to the journal. Live map
callbacks reconcile verified results as workers finish, without waiting for the
whole batch or rewriting all item records per completion. `completed` counts
resolved terminal items, including cancelled and undispatched items when a map
ends early; `failed` counts items with journal status `failed`. Collected failures
increase the failure count while the map and run may succeed. An expanded empty
map emits `0/0`. Skipped and undispatched steps emit `step.finished` with zero
duration and no start/attempt events. Decisions, selects, and graph markers have
no executable step events.

Failure classifications distinguish author error, non-retryable error, signal
termination, timeout, contract error, infrastructure error, cancellation, and
output verification failure. Diagnostics are bounded to 2,048 characters. Log
tails retain the last 20 lines, bounded to 8,192 characters. These local files
can contain author output; the durable root journal retains safe lifecycle
summaries. Stderr replaces control characters to keep author output from
controlling the terminal.

Ordinary failures, deadlines, and interrupts close dispatched attempts and emit
`run.finished` after the durable outcome and exit hook. Abrupt process death
(such as SIGKILL), event-file delivery failure, or an app restart can leave an
incomplete stream. Treat a missing terminal event or an incomplete final line as
interruption; inspect the journal for durable state. A run rejected before its
journal is claimed has no `run.started` event. Consumers validate each line
against the schema and reject obsolete versions instead of inferring a migration.
