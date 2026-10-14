# Run events v1

`massive run --json --events run-events.ndjson` preserves the single stdout result
and writes versioned NDJSON to a separate new file. Progress uses stderr:
`--progress auto|plain|none`, default `auto`. Auto shows a compact live view on a
TTY and only the final duration summary otherwise. Plain opts into event lines
and the summary. None disables rendering; with no event path it installs no
observer at all.

Event files are created only after input validation and frontend emission succeed,
exclusively with mode 0600. Existing paths are rejected; parent directories must
exist. `-` is an ordinary new filename. A single writer
drains the ordered queue and flushes each batch of complete newline-terminated
JSON records. Workers only enqueue in memory. The lossless queue is unbounded
(bounded by the run's total event count), so it can grow to the run size if the events file
stalls. Stderr has a separate bounded, best-effort channel: slow rendering can drop live lines without losing file
events or final summary rows. Normal completion and cancellation drain and close
the stream. File delivery errors make the CLI exit unsuccessfully while
preserving the workflow outcome and stdout result.

The [target-neutral JSON Schema](../conformance/schema/run-event.v1.schema.json)
defines `schemaVersion: 1`. Every event includes `type`, `sequence`, `projectKey`,
`runId`, `time` (UTC RFC 3339), and `elapsedMs`. Sequence starts at one per project
run. `elapsedMs` is monotonic time since the producer's run start, including
source preparation and exit-hook execution, excluding frontend emission. Attempt
and step durations are monotonic milliseconds. Process attempt duration is measured
by the invoker around process execution, excluding sibling execution and output
verification. Step duration includes settlement, output verification, and retry
backoff.

The vocabulary is:

- `run.started`: `planHash`, optional `mapTotals` keyed by node ID when cardinality
  is known when the run starts. This describes cardinality, not whether a map
  will execute; a known map can belong to an inactive branch.
- `step.attempt.started`: `nodeId`, `attempt`, optional zero-based `itemIndex`.
- `step.attempt.finished`: the same identity, `durationMs`, `status`,
  `classification` required for non-success outcomes, optional safe `diagnostic`
  bounded to 2,048 characters.
- `step.finished`: `nodeId`, `status`, `durationMs`.
- `run.finished`: `status`.

An item identity is `(projectKey, runId, nodeId, itemIndex)`, using stable source
order rather than author values. Attempts are dense and one-based. An attempt
finishes before its retry starts. The map's unscoped attempt represents expansion
and collection; item attempts are scoped by index. Skipped and undispatched steps
finish with zero duration and no attempts. Graph markers, decisions, and selects
have no executable step events. Exit hooks use ordinary step/attempt events and
never change the run outcome.

Local execution emits at dispatch, observed execution completion, and terminal
transitions. `step.attempt.finished` reports the attempt's execution outcome;
`step.finished` reports the settled step outcome. Process success can therefore
be followed by step failure if output verification or settlement fails. The
map's unscoped attempt covers expansion and collection; exit-hook preparation
failures also finish their control-plane attempt.

Map correctness uses the invoker's returned outcomes, validated before journal
mutation. All reported dispatches are reconciled before verification, then item
settlement follows source-index order. Item attempt finishes are live and ordered
by observation, without waiting for siblings or batch settlement. Observation-only
callbacks enqueue measured duration and process outcome; they do no I/O or
verification, run outside invoker locks, and never mutate journal entries.
Returned outcomes also supply events for invokers without callbacks. Open attempts
without a reported outcome are closed before step and run completion. Events have
no independent execution state model.

Non-success classifications are `error`, `non-retryable`, `fatal`, `terminated`, `timeout`,
`contract`, `infrastructure`, `cancelled`, and `verification`. Diagnostics contain
only safe lifecycle summaries. Author messages and logs stay out of this
contract. Fatal attempts use `fatal`; the human progress summary shows the
settled run's explicitly safe fatal cause separately. Terminal controls are sanitized by the CLI renderer. Production writes
events without validating itself; functional tests and consumers validate the
versioned schema.

`run.finished` follows terminal journal publication and the exit hook's record
publication attempt. Abrupt process death or event-file failure can leave an
incomplete stream. Treat a missing terminal event or incomplete final line as
interruption and inspect the journal for durable state. Runs rejected before
claiming their journal emit no `run.started`. Consumers reject obsolete versions
rather than inferring a migration.
