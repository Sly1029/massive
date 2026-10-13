"""The Python twin of conformance/workflows/ts-composed.

Both emit the graph in conformance/fixtures/composition/composed-graph.json.
"""

from __future__ import annotations

from massive import GraphBuilder, StepContext, container, execution

DEFAULTS = execution(
    environment=container(
        "example.invalid/python-runner@sha256:"
        "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
    )
)


def prepare(context: StepContext[int]) -> int:
    return context.inputs * 10


def side(context: StepContext[int]) -> int:
    return context.inputs + 3


def normalize(context: StepContext[int]) -> int:
    return context.inputs + 1


def total(context: StepContext[list[int]]) -> int:
    return sum(context.inputs)


def label(context: StepContext[int]) -> str:
    return f"value:{context.inputs}"


normalizer = GraphBuilder(
    name="normalizer", input_type=int, output_type=int, defaults=DEFAULTS
)
normalizer.edge_from(normalizer.start).transform(normalize).to_end(normalizer.end)

# Its entry step receives the parent's fan-in, and it calls another workflow.
totals = GraphBuilder(
    name="totals", input_type=list[int], output_type=int, defaults=DEFAULTS
)
totals.edge_from(totals.start).transform(total).to(
    totals.call(normalizer, id="again")
).to_end(totals.end)

graph = GraphBuilder(
    name="composed", input_type=int, output_type=str, defaults=DEFAULTS
)
prepared = graph.add(prepare)
upstream = graph.call(normalizer, id="upstream")
side_step = graph.add(side)
graph.edge_from(graph.start).to(prepared).to(upstream)
graph.edge_from(prepared).to(side_step)
# "join" sorts before "upstream", so the consumer expands before its source.
graph.gather(upstream, side_step).to(graph.call(totals, id="join")).transform(
    label
).to_end(graph.end)
