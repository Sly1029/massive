# Generated graphs

Test support for the compiler, executor, and target fuzzers. Production
packages must not import it.

Every byte sequence must decode into a spec that `spec.Parse` accepts. When the
generator produces a rejected spec, decide whether the generator or the
validator is wrong; do not narrow the generator to hide a validator bug.

The interpreter states the execution contract independently: activation from
each node's enclosing decision cases, values from symbol behaviors, attempts
from the retry contract, and map rounds where a terminal item failure stops
sibling retries. Do not import orchestrator or plan code into it. When Graph IR
or retry semantics change, extend the generator and interpreter together.

Child graphs decode once and expand at each call site with `call--node`
scoped IDs, as frontends emit them, so instances share symbols and behavior.
Keep IDs inside the 128-character safe segment.

Map item-failure policies are read after the fault script and interrupt, then
the workflow is emitted again, so older seeds keep their meaning; node IDs must
not depend on the policy. A collecting map is always followed by a step that
reads its outcomes, which keeps region output types independent of the policy.
Its outcome schema is hand-spelled in Pydantic's `$ref` form so the compiler's
annotation-insensitive comparison is exercised, not only exact equality.
