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
