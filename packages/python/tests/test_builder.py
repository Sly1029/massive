from __future__ import annotations

import pytest
from pydantic import BaseModel

from massive import GraphBuilder, StepContext, container, execution


class Request(BaseModel):
    value: int


class Result(BaseModel):
    value: int


def needs_services(context: StepContext[dict[str, str], Request]) -> Result:
    return Result(value=context.inputs.value)


def identity(context: StepContext[None, Request]) -> Result:
    return Result(value=context.inputs.value)


def test_graph_without_dependencies_rejects_a_step_that_declares_them() -> None:
    graph = GraphBuilder(
        name="no-deps",
        input_type=Request,
        output_type=Result,
        defaults=execution(environment=container("example.invalid/no-deps:latest")),
    )

    with pytest.raises(TypeError, match="does not permit dependencies"):
        graph.step()(needs_services)


def test_end_is_terminal() -> None:
    graph = GraphBuilder(
        name="terminal-end",
        input_type=Request,
        output_type=Result,
        defaults=execution(environment=container("example.invalid/terminal:latest")),
    )

    node = graph.add(graph.step()(identity))

    assert graph.edge_from(graph.start).to(node).to(graph.end) is None
