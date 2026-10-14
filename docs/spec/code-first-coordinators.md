# Code-first coordinators

Status: implemented local spike, based on `1e07ae0`; validation evidence is
recorded below as each gate passes. The protocol, CPython asyncio runner,
replay record, functional fixtures, and typing fixtures are implemented.
Adoption and merge require the owner's review. Argo consumes builder GraphIR.

The hypothesis is that ordinary typed Python can replace costly coordination
wiring while Massive retains immutable artifacts and isolated activities.
The motivation supplied for this spike is Samuel Colvin's “The code IS the
graph” (2026-10-13): join outputs rather than mutable shared state, and recover
coordination by replaying recorded activity results. This is consistent with
[keeping the core small](../roadmap.md#keep-the-core-small), but replay adds a
correctness obligation that a key-to-result cache alone does not satisfy.

## Three contracts

| Contract | Responsibility | Existing foundation |
| --- | --- | --- |
| Activity | Typed input/output, effects, isolation, retry and timeout policy | Plain `StepContext[I] -> O` functions, `ExecutionContract`, invocation descriptor v3, manifest-last publication |
| Coordination | Choose activities and join their immutable outputs | Builder GraphIR today; an async Python function in this spike |
| Record | Bind source/input identity, invocation commands, attempts, verified results, and replay order | Run journal v7 and immutable coordinator replay snapshots in the datastore |

The Go orchestrator owns execution and publication. The coordinator requests
activities; it does not become a second step executor. Keep the implementation
additive: one coordinator runner mode, one replay record, and reuse the existing
invoker, process owner, retry classification, artifact verifier, and events.
Do not serialize coordinator objects, closures, clients, or interpreter memory.

## Python interface

```python
async def coordinator(wf: Workflow, inputs: Request) -> Report:
    doubled, squared = await asyncio.gather(
        wf.run(double, inputs),
        wf.run(square, inputs),
    )
    return await wf.run(pair, (doubled, squared))
```

`Workflow` exposes one activity operation:

```python
run[I, O](
    fn: Callable[[StepContext[I]], O | Awaitable[O]],
    input: I,
    *,
    contract: ExecutionContract | None = None,
    key: str | None = None,
) -> Awaitable[O]
```

The runner constructs the handle with immutable defaults and typed entry
metadata. Resolve an omitted contract to those defaults before recording the
command. Register ordinary module-exported functions using `_step.py`'s existing
signature inspection; never decorate or transparently intercept calls. Ordinary
calls remain direct calls and have no durability. Lazy exports should follow
the existing package convention.

Publish Awaitable-first and synchronous overloads, as `GraphBuilder.add()`
already does: the union alone is ambiguous to ty. Both checkers must reject a
wrong input and prove awaited synchronous/asynchronous outputs, heterogeneous
`gather` tuple positions, and homogeneous gathered list elements. Runtime
input/output schemas still need validation; static inference does not replace it.

After policy exhaustion, raise `StepFailed` with the existing item-failure
classification, attempt count, and bounded diagnostic. Recoverable kinds remain
`error`, `killed`, `non-retryable`, and `timeout`. Contract, infrastructure,
verification, and cancellation failures end execution; converting these to
item results would weaken the existing contract. `FatalRunError` fails the run
and cancels siblings even if Python catches an exception. Raw logs and exception
chains are never root diagnostics.

Fan-out uses `asyncio.gather`. For collected outcomes, catch `StepFailed` inside
an item helper and return an existing typed `MapItemOutcome[O]`; avoid
`return_exceptions=True`, which loses the useful closed result type. A Go-owned
run-wide worker budget bounds all commands together; creating a separate
one-step invoker batch must not multiply its process limit. An optional Python
semaphore can limit submission, but cannot establish the executor's budget.

## Protocol and transport

Add `conformance/schema/coordinator.proto`, its frozen JSON Schema, generated
Go bindings, and a JSON projection description. Import the plan's
`ExecutionContract`, `ArtifactRef`, source/symbol/schema records rather than
copying their definitions. There is currently no protobuf item-failure message:
promote the existing `map-item-failure.schema.json` record into a shared proto
type and keep its fields and taxonomy unchanged. Do not invent a second
failure classifier for coordinators.

The essential messages are:

| Message | Required meaning |
| --- | --- |
| `CoordinatorStart` | Protocol version, run/plan identity, pinned coordinator symbol/source, input artifact and schemas, resolved defaults and environment, run deadline |
| `InvokeCommand` | `invocation_key`, `symbol_ref`, full resolved `ExecutionContract`, input `ArtifactRef` |
| `InvokeResult` | `invocation_key`, exactly one of verified output `ArtifactRef` or shared recoverable failure record |
| `CoordinatorComplete` | Typed, verified final output artifact; no outstanding activities |
| `CoordinatorFail` | Bounded safe lifecycle diagnostic and existing fatal cause when explicitly supplied |
| `CoordinatorQuiescent` | Close the current turn's ordered command sequence before another result is delivered |

Resolve schema hashes from the pinned symbol table or explicit schema records;
an untyped `ArtifactRef` by itself does not establish the activity's input or
output type. A result references the verified data-artifact manifest, so its
body, schema, and producer identity can be checked through the existing artifact
runtime. The orchestrator supplies attempt/output destinations through the
ordinary v3 descriptor. Coordinator loops use explicit keys and leave
`ExecutionScope` absent; do not fabricate GraphIR map frames. Accordingly
`context.invocation.item` is `None` for these calls.

Use bounded UTF-8 NDJSON over dedicated inherited request/response pipes; keep
stdout/stderr for author logs. If the local process abstraction cannot pass
extra descriptors portably, reserve stdin/stdout and redirect author stdout
before importing the entry module. Arbitrary prints must not become protocol
frames. Serialize concurrent writes, validate each complete frame against the
schema, bound lengths, reject unknown versions/variants, and treat EOF before a
terminal message as an interrupted coordinator. The entire subprocess belongs
to the existing process owner. No datastore credentials travel in log frames.

Projection follows the plan convention: optional proto scalars preserve
presence, camelCase JSON, explicit variant `kind`, canonical sorted keys,
omitted empty repeated fields, and exactly-one alternatives validated by schema.
Use the existing canonical JSON/hash recipe for numbers and artifact bodies.
Framing order is separate from invocation identity.

## Identity and replay

Use the current Python symbol reference, `<package>:<module>#<export>`.
An unkeyed one-use invocation uses that symbol; a keyed invocation appends
`@<key>`. Restrict explicit keys to `[A-Za-z0-9_.-]+`; reserving `@`, `:`,
and `#` avoids collisions with source/symbol delimiters.
Validate the complete identifier against the existing safe-segment rules,
including the 128-character limit; reject unsupported identifiers rather than
silently truncating or sanitizing them. Keys must be nonempty and stable within
the coordinator scope. Repeating a symbol requires explicit keys on **all** its
uses; mixing an unkeyed first call with a keyed later call fails before the
later dispatch. A duplicate full key is an author error, even with equal input.

For example, `scan` over a recorded list uses `key=f"item-{index}"`. A stable
domain item identifier is also suitable if validated and unique. Attempts do
not change the invocation key or the existing run-scoped idempotency key.
Positional counters would rename every subsequent activity when an unrelated
call is inserted. Explicit names keep attribution and external deduplication
readable. They do **not** permit resuming edited source: source identity and
the recorded command prefix must still match.

Define a versioned coordinator execution fingerprint from the selected source
package identity, coordinator symbol, and canonical typed inputs. Record the
schema/protocol recipe and resolved default policy as part of the immutable
start configuration. Reuse `planHash` as this recipe's execution fingerprint
only with an explicit coordinator discriminator; do not claim a GraphIR plan
was compiled. A run ID names one execution, while the fingerprint checks resume
eligibility. The fingerprint recipe v2 names `cpython-asyncio-delivery-v1`;
earlier spike recipes fail with a rebuild diagnostic. Freeze the checked
interpreter/environment, archived source, and
original absolute deadline. A resume must not buy a new deadline or reset retries.

Claim exclusive ownership before replay or journal mutation. A local file lock
can exclude another coordinator, but does not fence orphan activity processes.
The current Unix process owner creates independent groups; SIGKILL of the CLI
does not run its cleanup. Establish worker quiescence before restarting an
unfinished invocation, or explicitly document this resume precondition. Do not
claim exactly-once effects or automatic orphan fencing from process groups.

At minimum, persist an ordered acceptance log and a key-indexed result record:

1. Before resumed dispatch, validate identity and record consistency and verify
   all completed artifacts, with a separate bounded verification budget per
   invocation. Validate and durably record a new command before dispatch.
2. Append attempts, using fresh output slots; never reset completed attempts.
3. Verify publication, including referenced Blob/Tree closure, then commit the
   terminal result before releasing it to Python. Record exhausted failures too.
4. On resume, consume the recorded command prefix in order. Compare the entire
   canonical command: key, symbol, resolved contract, and input reference.
5. A matching completed invocation returns its verified recorded result with
   no execution. A matching unfinished invocation retains its attempt history;
   an interrupted attempt is accounted for before any eligible new attempt.
6. Permit a new command only after every prior accepted command has been
   reissued in order. Early completion, a missing/reordered/duplicate key, or a
   changed command is divergence. Stop dispatch and publish a safe diagnostic.

Never skip a corrupt/missing result and rerun it as a cache miss. Never adopt
an orphan output from a runner that did not report success. Death after an
external effect but before a durable verified result can cause re-execution;
the activity must use idempotency. Retry-disabled work interrupted after its
only attempt is not automatically safe to execute again.

### Parallel replay is the decisive risk

Ordered commands alone suffice for a restricted pattern: submit a deterministic
fan-out, collect each item's failure into a value, await the complete ordered
`gather`, then issue further commands. They do not fully describe arbitrary
async coordinators. If B finishes before A and B's continuation invokes C,
releasing cached A first on replay can change the next command without any
clock, random call, or source change.

The implemented scheduler supports independent per-item continuations through
deterministic delivery turns, including ordinary `asyncio.gather`. Go durably
records one result delivery before releasing it to CPython. The runner uses a
CPython selector event loop and drains every FIFO ready callback to quiescence
before reading another result, even when all response frames are already buffered.
It then emits the turn boundary. Replay re-delivers the recorded order and
compares the full canonical commands and command count for every closed turn
before allowing new activity dispatch. The unfinished final turn may extend its
accepted prefix; live execution begins after all recorded history is consumed.

This is delivery-order scheduling: a fast later activity can release its
continuation while an earlier activity remains blocked. Go settlement order
chooses live delivery order; replay preserves that order. Changing future
completion order after the recorded prefix is consumed may change `as_completed`
output order, as it does in a fresh execution. A complete record replays the exact
terminal output and commands without new attempts.

`asyncio.as_completed` must receive tasks created in deterministic input order
with `asyncio.create_task`; its bare-coroutine scheduling passes through a set.
This avoids identity-dependent command order inside the standard library.
Timers/duration waits are rejected, including aliased calls that evade the cheap
source checks. Coordinator I/O and background threads remain unsupported.
The runner depends on CPython's private ready queue to recognize quiescence;
the checked interpreter realization is pinned and verified before replay. This
is an explicit spike dependency, not a guarantee for alternate event loops or
arbitrary Python libraries. Functional tests change completion timing and NDJSON
batching while comparing every command and turn boundary.
Temporal explicitly replays an ordered history of commands and events; it is
more than an activity-result dictionary.
[Temporal replay](https://docs.temporal.io/workflows).

No coordinator I/O, clock, randomness, environment-dependent decisions,
background threads, or duration-based waits. Put these observations in
activities and branch on their outputs. Avoid order from sets and unordered
external state. Imports and validators must also obey this rule. Detect cheap
violations with explicit checks for common forbidden operations, plus command
and terminal-result comparisons; inspection cannot prove CPython determinism.
An audit hook catches some I/O, not every clock, random call, or native extension.
This remains trusted execution, not a Python sandbox.

## Journals, lifecycle, and exit behavior

The v6 reader assumes a static planned step set and rejects failed steps in a
successful run. Coordinator recovery deliberately violates that second
assumption. Do not disguise a recovered activity as a successful attempt.
If extending the existing manifest, bump its transport version and migrate
writers/readers together with the repository's rebuild diagnostic.

Keep changes small: a coordinator discriminator/start identity and a reference
to the versioned replay record; dynamic step entries reuse existing attempts
and publications. Contracts live with their recorded commands. Only a
coordinator variant may succeed with terminal recoverable failed activities
after its final output verifies. Graph runs retain their current invariants.
An independently versioned sidecar can hold the transcript, but adding its
reference or changing root success rules still requires the manifest bump.
Inspection must work without importing coordinator code.

Use existing run/attempt/step events with `nodeId` equal to the invocation key.
Do not emit execution attempts for cached results, promise static map totals,
or use observational run events as the replay log. Emit terminal events after
durable settlement. Resume event sequencing must remain monotonic within the
run if appending to an existing event stream.

`try/finally` is replayed coordinator control flow, not an exit hook guarantee.
Effects in a finalizer still need `wf.run` and explicit keys. SIGKILL never
executes Python finalizers; fatal-run and deadline cancellation stop ordinary
dispatch, including cleanup activities. Builder `graph.on_exit` remains the
post-settlement hook with a detached, finite lifetime and its own recorded
attempt. The spike need not add a coordinator hook interface. Do not weaken
fatal-run behavior to let Python swallow it or promise exactly-once cleanup.
Run-wide cancellation must reconcile successful in-flight outcomes under the
existing bounded detached lifetime before writing the terminal journal.

## What becomes ordinary code

| Existing coordination surface | Coordinator equivalent | What remains in core |
| --- | --- | --- |
| `decision` / `case` / `select` | `if` or `match` on typed recorded outputs | Output schema validation and replay checking |
| `merge` / `gather` | Typed tuple/list construction and `asyncio.gather` | Activity isolation, output verification, supported scheduling semantics |
| Map `item_failures="collect"` | Item helper catches `StepFailed`, returns typed outcomes, ordered gather | Retry policy, shared failure taxonomy, sibling fatal cancellation |
| Single-step recovery (G4) | `try` / `except StepFailed`, then a keyed recovery activity | Terminal failure recording and replay |
| Much of selective resume (G7) | Replay the coordinator with recorded invocation results | Identity, artifact closure, interrupted-attempt policy, ownership |

This joins immutable outputs. Mutable shared branch state is still fragile and
must not become an artifact contract. Unexecuted branches cannot be listed as
skipped static nodes because there is no exhaustive graph. An observed graph
can be reconstructed from history, but it is not a preflight execution plan.
GraphIR constructs remain necessary for builders and Argo; remove them only
if those authoring/target requirements are separately retired. G7's recovery
and retention obligations do not disappear just because its scheduler does.

## Future replay targets

These are design mappings, not implemented adapters. Share semantic contracts;
let each target own its native history and scheduling rather than nesting the
local replay engine inside it.

| Massive | Temporal mapping | Gap or qualification |
| --- | --- | --- |
| Coordinator entry / run ID | Workflow execution / workflow identity | Pin code and SDK; native Workflow ID and Run ID have different lifetimes |
| `InvokeCommand` / invocation key | Activity scheduling / explicit Activity ID | Adapter resolves symbols and artifacts; no GraphIR expansion |
| Contract retry / attempt timeout | Activity retry policy / start-to-close timeout | Translate policies exactly or reject; native defaults differ |
| `InvokeResult` | Recorded activity completion/failure payload | Carry artifact refs and shared classifications |
| Fatal run / run deadline | Workflow failure plus sibling cancellation / execution timeout | Activity failure alone does not express Massive's sticky whole-run abort |

Temporal compares workflow commands with recorded history and requires
deterministic workflow code. Pinning source is the spike's versioning strategy;
live source migration is deferred.
[Temporal determinism](https://docs.temporal.io/workflow-definition).

| Massive | Cloudflare Workflows mapping | Gap or qualification |
| --- | --- | --- |
| Coordinator / run inputs | Entrypoint `run` / event payload | Runtime adapter required; CPython subprocess is a local implementation |
| `wf.run(fn, input, key=...)` | `step.do(invocation_key, config, callback)` | Callback resolves the activity and immutable input |
| Stable explicit key | Deterministic step name | Massive invokes each full name once; repeated-name counters are not identities |
| Retry / timeout contract | Step retry configuration / timeout | Translate bounds and attempt counting; reject unsupported resources/policy |
| Artifact result / terminal failure | Persisted callback result / classified step exception | Keep body refs portable; implement sticky fatal handling separately |

Cloudflare exposes named steps and retry/timeout configuration. Its step context
also counts repeated calls with the same name; explicit per-item names avoid
depending on that positional count.
[Workers API](https://developers.cloudflare.com/workflows/build/workers-api/),
[step context](https://developers.cloudflare.com/workflows/build/step-context/).
Its guidance requires deterministic names/branches and cautions about races.
The local delivery history has no implemented mapping to that target; validate
concurrent behavior there before asserting equivalence.
[Workflows rules](https://developers.cloudflare.com/workflows/build/rules-of-workflows/).

## Argo and Monty

**(a) Builder-only Argo — recommended.** Keep today's compiled GraphIR, static
validation, placement, pod visibility, and distributed artifact transport.
Code-first input to Argo should fail with a capability diagnostic. This spike
does not run kind or establish new Argo conformance.

**(b) Restricted AST to GraphIR — paper design only.** A plausible subset has
direct top-level activity symbols, constant contracts/keys, straight-line
awaits, fixed tuple gather, typed tuple/list joins, discriminated-union branches,
and a finite list fan-out whose body is one activity. These map to existing
step, merge, decision/select, and map nodes. General predicates/transforms
must become explicit steps; GraphIR cannot evaluate arbitrary Python expressions.
Reject arbitrary loops, dynamic function lookup, recursion, races, and recovery
exceptions/finalizers whose semantics GraphIR cannot express. Supporting
single-step recovery would itself require a graph contract change. Without a
representative workflow demanding one source for both targets, the compiler
looks like a second authoring language with two failure semantics. Defer it.

**(c) Coordinator pod.** Python could replay in a control pod and submit
activities as pods or child workflows. This requires submission permissions,
durable ownership/fencing across pod restarts, scheduling and cancellation,
new observability, and reconciliation of Kubernetes events with the record.
A subprocess-only pod loses independent placement and pod-level activity
isolation. Either design is substantially more than existing Argo lowering;
do not advertise it as a trivial backend.

Monty can suspend at external calls/futures and serialize interpreter state for
later restoration. A future adapter could translate `wf.run` into that same
invocation protocol and persist snapshots at suspensions instead of replaying
all coordination. Snapshot provenance, runtime compatibility, pending activity
ownership, and atomic publication still matter. Python language/library and
type-model compatibility need separate tests; no CPython drop-in claim is
justified.
[Monty snapshots](https://github.com/pydantic/monty/blob/main/docs/snapshots.md).

## Evidence required and recommendation

Use the fan-in and map-outcomes conformance functions as real activities in a
code-first example: keyed fan-out, typed join, branch, exhausted-retry outcomes,
and a recovery invocation after the join. Functional acceptance must show:

- Kill the owning CLI after a journal-confirmed completion while another
  invocation is active; establish quiescence of its owned workers and resume
  the same pinned source/input. Completed
  keys retain exactly their old attempt counts and publications, and the final
  typed output equals an uninterrupted run. Include abrupt SIGKILL, not only
  graceful cancellation.
- Reissue a changed command for the same key, reorder a recorded pair, add a
  new key ahead of the recorded suffix, or finish early. Each fails before new
  effects, with a safe divergence diagnostic. Vary nondeterminism without
  changing archived source; a source edit tests identity rejection instead.
- Corrupt/remove a completed artifact, retain an orphan output, interrupt a
  retry/backoff, recover an exhausted failure, and exercise fatal/deadline paths.
  Verify artifacts before returning any cached output and preserve attempts.
- Change parallel completion order and response batching. State whether only
  barrier-style joins or full independent continuations are supported.
- Pass pyright and ty positive/negative fixtures; protocol/schema conformance;
  the mock ban; and the repository check through `massive-heavy`. Installed-wheel
  execution must cover the new runner mode. Completed spike gates are listed
  below; unrun targets do not inherit local evidence.

Retain builders and keep this PR draft for owner review. The delivery scheduler
addresses the command-only replay risk for the tested asyncio patterns. Adoption
should weigh the authoring benefit against a private CPython scheduling dependency,
whole-snapshot journal cost, and the explicit orphan-worker quiescence precondition.
The evidence does not establish arbitrary durable Python, automatic effect
fencing, or a distributed coordinator target.

If adopted, separate activity execution from coordination scheduling at the
existing invoker seam; add one typed coordinator/run-kind contract and one
versioned record. Keep artifacts, source identity, environment checks, policies,
safe errors, and events shared. Move graph-specific coordination into the
builder/GraphIR scheduler while replay targets consume invocations directly.
Do not remove GraphIR operators, introduce a general engine/plugin framework,
or grow hosted policy until a second real target demonstrates the need.

## Spike evidence

Functional evidence is produced by `packages/python/tests/test_coordinator.py`
against real CLI/runner processes and filesystem stores, through `massive-heavy`.

1. **Abrupt recovery:** both fan-out/per-item-chain patterns pass CLI SIGKILL,
   worker quiescence, exclusive ownership, and resume. Completed step records
   retain exact attempts and publications; gathered output equals an uninterrupted
   run. Resume events have strictly increasing sequences and cached keys emit
   no new attempt-start events. Event sequence reservations may leave crash gaps.
2. **Divergence:** unchanged archived source with changed input-to-command,
   reordered keys, a new key ahead of the recorded suffix, or early completion
   fails with a safe divergence diagnostic. The four cases preserve every
   external activity-effect file byte for byte.
3. **Artifacts and lifecycle:** 21 cases pass missing/corrupt activity, final,
   run-result, and pinned-input artifacts; changed source/input/store identity;
   orphan publication; recorded retry/backoff and interrupted non-retryable work;
   recovered exhaustion; fatal, deadline, sticky coordinator fatal, and unsupported
   operations. Backoff retains its original timestamp and retries retain the
   idempotency key. Only coordinator journals admit recovered failures; v6 is
   rejected with a rebuild diagnostic. These cases run through the CPython
   asyncio driver, with ordinary standard-library joins.
