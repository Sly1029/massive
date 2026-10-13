from __future__ import annotations

from massive import GraphBuilder, MapItemIdentity, StepContext, container, execution


def load_items(ctx: StepContext[list[int]]) -> list[int]:
    assert ctx.invocation.item is None
    return ctx.inputs


def capture_item(ctx: StepContext[int]) -> MapItemIdentity:
    assert ctx.invocation.item is not None
    return ctx.invocation.item


defaults = execution(
    environment=container("example.invalid/python@sha256:" + "0" * 64, platform="linux/amd64")
)
graph = GraphBuilder(
    name="item-identity", input_type=list[int], output_type=list[MapItemIdentity], defaults=defaults
)
items = graph.add(load_items)
results = graph.map(items, capture_item, id="capture", concurrency=3)
graph.edge_from(graph.start).to(items)
graph.edge_from(results).to_end(graph.end)

composed = GraphBuilder(
    name="composed-item-identity",
    input_type=list[int],
    output_type=list[MapItemIdentity],
    defaults=defaults,
)
composed.edge_from(composed.start).to(composed.call(graph, id="child")).to_end(composed.end)
