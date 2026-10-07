# Python tests

`test_graph_properties.py` generates `GraphDescription` data
(`fixtures/generated_graph/generated_shapes.py`) and checks it three ways:
`call()` and hand-inlined builds emit byte-identical specs that compile; local
`massive run` results and run journals match the independent `Expectation`
model; and invalid composition (scoped ID collisions, IDs over 128 characters)
fails at emission. When adding a graph feature, extend the description, the
builder, and the model together rather than asserting only that it compiles.

Generated IDs exclude `--` and the `-select` suffix so scoped IDs stay
unambiguous; collision properties construct those cases deliberately. Python has
no merge API and the TypeScript builder has no decisions, maps, or calls, so
cross-language IR parity would cover only linear chains with differing schema
references; it is intentionally not generated here.

Hypothesis profiles live in `conftest.py`: `ci` (derandomized, run by
`pnpm check`) and `nightly` (randomized, larger maps, cached example database in
the fuzz workflow). Select one with `MASSIVE_HYPOTHESIS_PROFILE`; budget
execution-heavy properties with `scaled()`.
