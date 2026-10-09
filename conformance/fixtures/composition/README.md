# Composition vector

`composed-graph.json` is the Graph IR both SDKs must emit for the equivalent
composed workflows in `conformance/workflows/ts-composed` and
`conformance/workflows/python-composed`: a child workflow called twice, a call
that consumes a fan-in and expands before the call that feeds it, a nested
call, and a transform step.

It records node ids, kinds, each step's resolved export, `mergeInputs`, and
edges in emitted order. Schema, symbol, and contract references are omitted
because Zod and Pydantic lower equivalent types to different JSON Schemas.
Each SDK's tests emit its workflow and compare the projection to this file;
`internal/controlplane` runs both workflows locally to the same result.
