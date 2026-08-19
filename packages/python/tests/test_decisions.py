from __future__ import annotations

from pathlib import Path
from typing import Annotated, Any, Literal

import pytest
from pydantic import BaseModel, Field, ValidationError

from massive import GraphBuilder, StepContext, container, execution, source_package


class Request(BaseModel):
    value: int


class Result(BaseModel):
    value: int


class OtherResult(BaseModel):
    reason: str


class Approved(BaseModel):
    kind: Literal["approved"]
    value: int


class Rejected(BaseModel):
    kind: Literal["rejected"]
    reason: str


Route = Annotated[Approved | Rejected, Field(discriminator="kind")]


class Astral(BaseModel):
    kind: Literal["\U00010000"]


class PrivateUse(BaseModel):
    kind: Literal["\ue000"]


UnicodeRoute = Annotated[Astral | PrivateUse, Field(discriminator="kind")]


def classify(context: StepContext[None, Request]) -> Route:
    return Approved(kind="approved", value=context.inputs.value)


def classify_without_discriminator(context: StepContext[None, Request]) -> Approved | Rejected:
    return Approved(kind="approved", value=context.inputs.value)


def classify_unicode(context: StepContext[None, Request]) -> UnicodeRoute:
    return Astral(kind="\U00010000")


def approve(context: StepContext[None, Approved]) -> Result:
    return Result(value=context.inputs.value)


def reject(context: StepContext[None, Rejected]) -> Result:
    return Result(value=0)


def astral(context: StepContext[None, Astral]) -> Result:
    return Result(value=1)


def private_use(context: StepContext[None, PrivateUse]) -> Result:
    return Result(value=2)


def test_emit_serializes_an_exhaustive_pydantic_decision_as_data_only_ir() -> None:
    graph = GraphBuilder(
        name="decision-workflow",
        input_type=Request,
        output_type=Result,
        defaults=_defaults(),
    )
    classified = graph.add(graph.step()(classify))
    approved = graph.add(graph.step()(approve))
    rejected = graph.add(graph.step()(reject))

    route = graph.decision(classified, on="kind", id="review-route")
    graph.edge_from(graph.start).to(classified)
    graph.edge_from(route.case(Approved)).to(approved)
    graph.edge_from(route.case(Rejected)).to(rejected)
    selected = route.select(Result, approved=approved, rejected=rejected)
    graph.edge_from(selected).to(graph.end)

    specification = _emit(graph)
    graph_ir = specification.value["graph"]
    nodes = {node["id"]: node for node in graph_ir["nodes"]}
    decision = nodes["review-route"]
    select = nodes["review-route-select"]

    assert graph_ir["irVersion"] == "0.2"
    assert decision["kind"] == "decision"
    assert decision["selector"] == "kind"
    assert [case["tag"] for case in decision["cases"]] == ["approved", "rejected"]
    assert select["kind"] == "select"
    assert select["decisionRef"] == "review-route"
    assert select["selectInputs"] == [
        {"case": "approved", "source": "approve"},
        {"case": "rejected", "source": "reject"},
    ]
    assert {"from": "review-route", "to": "approve", "case": "approved"} in graph_ir["edges"]
    assert {"from": "review-route", "to": "reject", "case": "rejected"} in graph_ir["edges"]
    assert {"from": "approve", "to": "review-route-select"} in graph_ir["edges"]
    assert {"from": "reject", "to": "review-route-select"} in graph_ir["edges"]


def test_decision_rejects_nonportable_or_ambiguous_authoring_forms() -> None:
    graph = GraphBuilder(
        name="decision-errors",
        input_type=Request,
        output_type=Result,
        defaults=_defaults(),
    )
    undecorated = graph.add(graph.step()(classify_without_discriminator))

    with pytest.raises(TypeError, match="Pydantic discriminated union"):
        graph.decision(undecorated, on="kind", id="undecorated")

    classified = graph.add(graph.step()(classify))
    with pytest.raises(TypeError, match="does not match"):
        graph.decision(classified, on="route", id="wrong-selector")

    with pytest.raises(ValidationError, match="derived '-select'"):
        graph.decision(classified, on="kind", id="x" * 122)


def test_decision_requires_exactly_one_connected_case_and_matching_selected_outputs() -> None:
    graph = GraphBuilder(
        name="decision-case-errors",
        input_type=Request,
        output_type=Result,
        defaults=_defaults(),
    )
    classified = graph.add(graph.step()(classify))
    approved = graph.add(graph.step()(approve))
    rejected = graph.add(graph.step()(reject))
    route = graph.decision(classified, on="kind", id="case-errors")

    approved_input = route.case(Approved)
    with pytest.raises(ValueError, match="already connected"):
        route.case(Approved)
    graph.edge_from(approved_input).to(approved)

    with pytest.raises(ValueError, match="unconnected cases: rejected"):
        route.select(Result, approved=approved)

    graph.edge_from(route.case(Rejected)).to(rejected)
    with pytest.raises(ValueError, match="missing 'rejected'.*unknown 'unknown'"):
        route.select(Result, approved=approved, unknown=rejected)
    with pytest.raises(TypeError, match="does not match select output"):
        route.select(OtherResult, approved=approved, rejected=rejected)
    with pytest.raises(ValueError, match="not in that branch"):
        route.select(Result, approved=rejected, rejected=approved)


def test_decision_cases_and_select_inputs_use_utf16_ordering() -> None:
    graph = GraphBuilder(
        name="unicode-decision",
        input_type=Request,
        output_type=Result,
        defaults=_defaults(),
    )
    classified = graph.add(graph.step()(classify_unicode))
    astral_result = graph.add(graph.step()(astral))
    private_use_result = graph.add(graph.step()(private_use))
    route = graph.decision(classified, on="kind", id="unicode-route")
    graph.edge_from(graph.start).to(classified)
    graph.edge_from(route.case(Astral)).to(astral_result)
    graph.edge_from(route.case(PrivateUse)).to(private_use_result)
    selected = route.select(
        Result,
        **{
            "\ue000": private_use_result,
            "\U00010000": astral_result,
        },
    )
    graph.edge_from(selected).to(graph.end)

    graph_ir = _emit(graph).value["graph"]
    nodes = {node["id"]: node for node in graph_ir["nodes"]}

    assert [case["tag"] for case in nodes["unicode-route"]["cases"]] == [
        "\U00010000",
        "\ue000",
    ]
    assert nodes["unicode-route-select"]["selectInputs"] == [
        {"case": "\U00010000", "source": "astral"},
        {"case": "\ue000", "source": "private_use"},
    ]


def _defaults():
    return execution(
        environment=container(
            "example.invalid/decisions@sha256:"
            "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
            platform="linux/amd64",
        )
    )


def _emit(graph: GraphBuilder[Any, Any, Any]):
    return graph.emit(
        source=source_package(
            root=Path(__file__).parent,
            include=[Path(__file__).name],
            package_id="python-tests",
        )
    )
