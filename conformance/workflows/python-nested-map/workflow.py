"""Nested model containers through maps, collected outcomes, and fan-in."""

from __future__ import annotations

from collections.abc import Callable
from typing import TypeVar

from massive import (
    GraphBuilder,
    MapItemOutcome,
    MapItemSucceeded,
    NonRetryableError,
    StepContext,
    container,
    execution,
)
from pydantic import BaseModel

DEFAULTS = execution(
    environment=container(
        "example.invalid/python-runner@sha256:"
        "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
    )
)


class Report(BaseModel):
    value: str


class Detail(BaseModel):
    report: Report


class Nested(BaseModel):
    detail: Detail


class Recursive(BaseModel):
    value: str
    children: list[Recursive]


def seed(ctx: StepContext[list[int]]) -> list[int]:
    return ctx.inputs


def per_item(ctx: StepContext[int]) -> list[Report]:
    return [Report(value=str(ctx.inputs)), Report(value=f"{ctx.inputs}!")]


def per_tuple(ctx: StepContext[int]) -> tuple[Report, ...]:
    return tuple(per_item(ctx))


def per_nested(ctx: StepContext[int]) -> list[Nested]:
    return [Nested(detail=Detail(report=report)) for report in per_item(ctx)]


def echo_reports(ctx: StepContext[list[Report]]) -> list[Report]:
    return ctx.inputs


def flatten(ctx: StepContext[list[list[Report]]]) -> list[str]:
    return [report.value for reports in ctx.inputs for report in reports]


def flatten_tuples(ctx: StepContext[list[tuple[Report, ...]]]) -> list[str]:
    return [report.value for reports in ctx.inputs for report in reports]


def flatten_nested(ctx: StepContext[list[list[Nested]]]) -> list[str]:
    return [nested.detail.report.value for results in ctx.inputs for nested in results]


ResultT = TypeVar("ResultT")


def mapped(
    name: str,
    mapper: Callable[[StepContext[int]], ResultT],
    reducer: Callable[[StepContext[list[ResultT]]], list[str]],
) -> GraphBuilder[list[int], list[str]]:
    graph = GraphBuilder(
        name=name, input_type=list[int], output_type=list[str], defaults=DEFAULTS
    )
    items = graph.add(seed)
    graph.edge_from(graph.start).to(items)
    results = graph.map(items, mapper, id="per-item")
    graph.edge_from(results).transform(reducer).to_end(graph.end)
    return graph


lists = mapped("nested-lists", per_item, flatten)
tuples = mapped("nested-tuples", per_tuple, flatten_tuples)
nested = mapped("nested-models", per_nested, flatten_nested)

inputs = GraphBuilder(
    name="nested-inputs", input_type=list[int], output_type=list[str], defaults=DEFAULTS
)
items = inputs.add(seed)
inputs.edge_from(inputs.start).to(items)
reports = inputs.map(items, per_item, id="reports")
echoed = inputs.map(reports, echo_reports, id="echo")
inputs.edge_from(echoed).transform(flatten).to_end(inputs.end)


def sometimes(ctx: StepContext[int]) -> list[Report]:
    if ctx.inputs % 2:
        raise NonRetryableError("odd item")
    return per_item(ctx)


def collect_values(ctx: StepContext[list[MapItemOutcome[list[Report]]]]) -> list[str]:
    values: list[str] = []
    for outcome in ctx.inputs:
        if isinstance(outcome, MapItemSucceeded):
            values.extend(report.value for report in outcome.value)
        else:
            values.append("failed")
    return values


collect = GraphBuilder(
    name="nested-outcomes",
    input_type=list[int],
    output_type=list[str],
    defaults=DEFAULTS,
)
items = collect.add(seed)
collect.edge_from(collect.start).to(items)
outcomes = collect.map(items, sometimes, id="per-item", item_failures="collect")
collect.edge_from(outcomes).transform(collect_values).to_end(collect.end)


def batch(ctx: StepContext[list[int]]) -> list[Report]:
    return [Report(value=str(value)) for value in ctx.inputs]


def merged(ctx: StepContext[tuple[list[Report], list[Report]]]) -> list[str]:
    return [report.value for reports in ctx.inputs for report in reports]


def fan_in(name: str, gather: bool) -> GraphBuilder[list[int], list[str]]:
    graph = GraphBuilder(
        name=name, input_type=list[int], output_type=list[str], defaults=DEFAULTS
    )
    split = graph.add(seed)
    left, right = graph.add(batch, id="left"), graph.add(batch, id="right")
    graph.edge_from(graph.start).to(split).to(left)
    graph.edge_from(split).to(right)
    if gather:
        graph.gather(left, right).transform(flatten).to_end(graph.end)
    else:
        graph.merge(left, right).transform(merged).to_end(graph.end)
    return graph


gather = fan_in("nested-gather", True)
merge = fan_in("nested-merge", False)
