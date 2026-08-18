from __future__ import annotations

from collections.abc import Callable
from pathlib import Path
from typing import Any

import pytest
from pydantic import BaseModel

from massive import GraphBuilder, StepContext, container, execution, source_package


class Request(BaseModel):
    value: int


class Result(BaseModel):
    value: int


class DecimalRequest(BaseModel):
    value: float


class NestedDecimalResult(BaseModel):
    values: list[float]


class DecimalMeasurement(BaseModel):
    value: float


class ReferencedDecimalResult(BaseModel):
    measurement: DecimalMeasurement


def needs_services(context: StepContext[dict[str, str], Request]) -> Result:
    return Result(value=context.inputs.value)


def identity(context: StepContext[None, Request]) -> Result:
    return Result(value=context.inputs.value)


def decimal_identity(context: StepContext[None, DecimalRequest]) -> Result:
    return Result(value=int(context.inputs.value))


def nested_decimal_result(context: StepContext[None, Request]) -> NestedDecimalResult:
    return NestedDecimalResult(values=[float(context.inputs.value)])


def referenced_decimal_result(context: StepContext[None, Request]) -> ReferencedDecimalResult:
    return ReferencedDecimalResult(measurement=DecimalMeasurement(value=context.inputs.value))


def test_graph_without_dependencies_rejects_a_step_that_declares_them() -> None:
    graph = GraphBuilder(
        name="no-deps",
        input_type=Request,
        output_type=Result,
        defaults=execution(
            environment=container(
                "example.invalid/no-deps@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
                platform="linux/amd64",
            )
        ),
    )

    with pytest.raises(TypeError, match="does not permit dependencies"):
        graph.step()(needs_services)


def test_end_is_terminal() -> None:
    graph = GraphBuilder(
        name="terminal-end",
        input_type=Request,
        output_type=Result,
        defaults=execution(
            environment=container(
                "example.invalid/terminal@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
                platform="linux/amd64",
            )
        ),
    )

    node = graph.add(graph.step()(identity))

    assert graph.edge_from(graph.start).to(node).to(graph.end) is None


@pytest.mark.parametrize(
    ("input_type", "output_type", "step", "message"),
    [
        (DecimalRequest, Result, decimal_identity, "workflow input schema"),
        (Request, NestedDecimalResult, nested_decimal_result, "workflow output schema"),
        (Request, ReferencedDecimalResult, referenced_decimal_result, "workflow output schema"),
    ],
)
def test_emit_rejects_schemas_that_admit_non_integer_json_numbers(
    input_type: type[BaseModel],
    output_type: type[BaseModel],
    step: Callable[..., Any],
    message: str,
) -> None:
    graph = GraphBuilder(
        name="integer-only",
        input_type=input_type,
        output_type=output_type,
        defaults=execution(
            environment=container(
                "example.invalid/integer-only@sha256:"
                "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
                platform="linux/amd64",
            )
        ),
    )
    node = graph.add(graph.step()(step))
    graph.edge_from(graph.start).to(node).to(graph.end)

    with pytest.raises(ValueError, match=message + ".*integer-only"):
        graph.emit(
            source=source_package(
                root=Path(__file__).parent,
                include=[Path(__file__).name],
                package_id="python-tests",
            )
        )
