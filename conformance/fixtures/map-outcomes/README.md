# Map outcome schemas

`outcome-schemas.json` lists `(itemOutputSchema, outputSchema)` pairs for a map
with `itemFailures: "collect"` and whether the Go compiler must accept the
output as the list of item outcomes wrapping the item schema
(`docs/spec/ir-and-datastore.md`, "Item failure policy").

The `pydantic-*` cases are the Python SDK's emission of `T` and
`list[MapItemOutcome[T]]`; `packages/python/tests/test_map_outcomes.py` checks
that the current SDK still emits them exactly. `inline-without-annotations`
spells the contract without references or annotations. Every rejected case
changes one validating keyword of that inline form, or is not an outcome list.
Change a case only together with the contract.
