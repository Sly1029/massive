from __future__ import annotations

from pydantic import BaseModel

from massive import GraphBuilder, JsonValue, StepContext, container, execution


class Result(BaseModel):
    metadata: dict[str, JsonValue]
    identifier: int


def produce(context: StepContext[float]) -> Result:
    return Result(metadata={"x": context.inputs}, identifier=int(context.inputs))


def echo(context: StepContext[Result]) -> Result:
    # Pydantic restores both integral wire tokens as ints, including JsonValue.
    assert isinstance(context.inputs.metadata["x"], int)
    assert isinstance(context.inputs.identifier, int)
    return context.inputs


graph = GraphBuilder(
    name="integral-doubles",
    input_type=float,
    output_type=Result,
    defaults=execution(
        environment=container(
            "example.invalid/python@sha256:"
            "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
        )
    ),
)
produced = graph.add(produce)
echoed = graph.add(echo)
graph.edge_from(graph.start).to(produced).to(echoed).to_end(graph.end)
