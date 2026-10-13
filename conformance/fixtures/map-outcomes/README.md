# Map outcome schemas

`outcome-schemas.json` lists `(itemOutputSchema, outputSchema)` pairs for a map
with `itemFailures: "collect"` and whether the Go compiler must accept the
output as the list of item outcomes wrapping the item schema
(`docs/spec/ir-and-datastore.md`, "Item failure policy").

The `pydantic-*` cases are the Python SDK's emission of `T` and
`list[MapItemOutcome[T]]`; `packages/python/tests/test_map_outcomes.py` and
`test_nested_map_schemas.py` check that the current SDK still emits them exactly,
including lists of nested and recursive models. `inline-without-annotations`
spells the contract without references or annotations. Rejected cases change a
validating keyword, including a nested model field, or are not outcome lists.
Change a case only together with the contract.
