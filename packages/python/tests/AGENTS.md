# Python tests

`test_graph_properties.py` generates `GraphDescription` data
(`fixtures/generated_graph/generated_shapes.py`) and checks it three ways:
`call()` and hand-inlined builds emit byte-identical specs that compile; local
`massive run` results and run journals match the independent `Expectation`
model; and invalid composition (scoped ID collisions, IDs over 128 characters)
fails at emission. When adding a graph feature, extend the description, the
builder, and the model together rather than asserting only that it compiles.

Generated IDs exclude `--` and the `-select` suffix so scoped IDs stay
unambiguous; collision properties construct those cases deliberately. `JoinNode`
generates static fan-in (merge tuples or gathered lists, optionally consumed by a
call); its join step records each input's last trace entry in merge order, so
the model checks ordering as well as activity. The TypeScript builder has no
decisions, maps, or calls, so cross-language IR parity would cover only linear
chains and diamonds with differing schema references; it is intentionally not
generated here.

`tests/typecheck/` is checked by pyright and ty, not executed. Known-bad wiring
in `rejected_wiring.py` carries one ignore per checker, and both unused-ignore
rules are errors, so a line that stops failing breaks `pnpm check:fast`.
Wiring that type-checks but fails at build stays in `authoring.py`, paired with
a `test_fan_in.py` test that pins the build-time rejection.

Hypothesis profiles live in `conftest.py`: `ci` (derandomized, run by
`pnpm check`) and `nightly` (randomized, larger maps, cached example database in
the fuzz workflow). Select one with `MASSIVE_HYPOTHESIS_PROFILE`; budget
execution-heavy properties with `scaled()`.
