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


class MultiTagged(BaseModel):
    kind: Literal["approved", "manual-review"]
    value: int


MultiTagRoute = Annotated[MultiTagged | Rejected, Field(discriminator="kind")]


class FastTrack(BaseModel):
    kind: Literal["fast"]
    value: int


class ManualReview(BaseModel):
    kind: Literal["manual"]
    value: int


InnerRoute = Annotated[FastTrack | ManualReview, Field(discriminator="kind")]


def classify(context: StepContext[None, Request]) -> Route:
    return Approved(kind="approved", value=context.inputs.value)


def classify_without_discriminator(context: StepContext[None, Request]) -> Approved | Rejected:
    return Approved(kind="approved", value=context.inputs.value)


def classify_unicode(context: StepContext[None, Request]) -> UnicodeRoute:
    return Astral(kind="\U00010000")


def classify_multi_tag(context: StepContext[None, Request]) -> MultiTagRoute:
    return MultiTagged(kind="approved", value=context.inputs.value)


def approve(context: StepContext[None, Approved]) -> Result:
    return Result(value=context.inputs.value)


def reject(context: StepContext[None, Rejected]) -> Result:
    return Result(value=0)


def astral(context: StepContext[None, Astral]) -> Result:
    return Result(value=1)


def private_use(context: StepContext[None, PrivateUse]) -> Result:
    return Result(value=2)


def review(context: StepContext[None, Approved]) -> InnerRoute:
    return FastTrack(kind="fast", value=context.inputs.value)


def fast_track(context: StepContext[None, FastTrack]) -> Result:
    return Result(value=context.inputs.value)


def manual_review(context: StepContext[None, ManualReview]) -> Result:
    return Result(value=context.inputs.value)


def approved_items(context: StepContext[None, Approved]) -> list[Approved]:
    return [context.inputs]


def map_approved(context: StepContext[None, Approved]) -> Result:
    return Result(value=context.inputs.value + 1)


def collect_approved_results(context: StepContext[None, list[Result]]) -> Result:
    return context.inputs[0]


def rejected_results(context: StepContext[None, Rejected]) -> list[Result]:
    return [Result(value=0)]


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
    approved_input = route.case(Approved)
    rejected_input = route.case(Rejected)
    selected = route.select(Result, approved=approved, rejected=rejected)
    graph.edge_from(graph.start).to(classified)
    graph.edge_from(approved_input).to(approved)
    graph.edge_from(rejected_input).to(rejected)
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


def test_map_can_follow_a_decision_branch_and_select_its_downstream_result() -> None:
    graph = GraphBuilder(
        name="decision-map-workflow",
        input_type=Request,
        output_type=Result,
        defaults=_defaults(),
    )
    classified = graph.add(graph.step()(classify))
    approved_source = graph.add(graph.step()(approved_items))
    approved_map = graph.map(approved_source, graph.step()(map_approved), id="map-approved")
    approved_result = graph.add(graph.step()(collect_approved_results))
    rejected_result = graph.add(graph.step()(reject))
    route = graph.decision(classified, on="kind", id="review-route")
    approved_input = route.case(Approved)
    rejected_input = route.case(Rejected)
    selected = route.select(Result, approved=approved_result, rejected=rejected_result)
    graph.edge_from(graph.start).to(classified)
    graph.edge_from(approved_input).to(approved_source)
    graph.edge_from(approved_map).to(approved_result)
    graph.edge_from(rejected_input).to(rejected_result)
    graph.edge_from(selected).to(graph.end)

    graph_ir = _emit(graph).value["graph"]

    assert graph_ir["irVersion"] == "0.3"
    assert {"from": "approved_items", "to": "map-approved"} in graph_ir["edges"]
    assert {"from": "map-approved", "to": "collect_approved_results"} in graph_ir["edges"]


def test_select_accepts_a_direct_map_result_with_a_synthesized_list_output_type() -> None:
    graph = GraphBuilder(
        name="decision-map-select",
        input_type=Request,
        output_type=list[Result],
        defaults=_defaults(),
    )
    classified = graph.add(graph.step()(classify))
    approved_source = graph.add(graph.step()(approved_items))
    approved_map = graph.map(approved_source, graph.step()(map_approved), id="map-approved")
    rejected_result = graph.add(graph.step()(rejected_results))
    route = graph.decision(classified, on="kind", id="review-route")
    approved_input = route.case(Approved)
    rejected_input = route.case(Rejected)
    selected = route.select(list[Result], approved=approved_map, rejected=rejected_result)
    graph.edge_from(graph.start).to(classified)
    graph.edge_from(approved_input).to(approved_source)
    graph.edge_from(rejected_input).to(rejected_result)
    graph.edge_from(selected).to(graph.end)

    graph_ir = _emit(graph).value["graph"]

    assert graph_ir["irVersion"] == "0.3"
    assert {"from": "map-approved", "to": "review-route-select"} in graph_ir["edges"]


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


def test_decision_rejects_a_model_with_multiple_discriminator_tags() -> None:
    graph = GraphBuilder(
        name="multi-tag-decision",
        input_type=Request,
        output_type=Result,
        defaults=_defaults(),
    )
    classified = graph.add(graph.step()(classify_multi_tag))

    with pytest.raises(
        TypeError,
        match=(
            "decision case MultiTagged declares multiple discriminator tags "
            "'approved', 'manual-review'; split it into one Pydantic model per tag"
        ),
    ):
        graph.decision(classified, on="kind", id="multi-tag-route")


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


def test_emit_allows_an_outer_select_to_choose_a_nested_select() -> None:
    graph = GraphBuilder(
        name="nested-decision",
        input_type=Request,
        output_type=Result,
        defaults=_defaults(),
    )
    classified = graph.add(graph.step()(classify))
    reviewed = graph.add(graph.step()(review))
    fast_result = graph.add(graph.step()(fast_track))
    manual_result = graph.add(graph.step()(manual_review))
    rejected_result = graph.add(graph.step()(reject))

    outer = graph.decision(classified, on="kind", id="outer")
    graph.edge_from(graph.start).to(classified)
    graph.edge_from(outer.case(Approved)).to(reviewed)
    graph.edge_from(outer.case(Rejected)).to(rejected_result)

    inner = graph.decision(reviewed, on="kind", id="inner")
    graph.edge_from(inner.case(FastTrack)).to(fast_result)
    graph.edge_from(inner.case(ManualReview)).to(manual_result)
    selected_inner = inner.select(Result, fast=fast_result, manual=manual_result)

    selected_outer = outer.select(
        Result,
        approved=selected_inner,
        rejected=rejected_result,
    )
    graph.edge_from(selected_outer).to(graph.end)

    nodes = {node["id"]: node for node in _emit(graph).value["graph"]["nodes"]}
    assert nodes["outer-select"]["selectInputs"] == [
        {"case": "approved", "source": "inner-select"},
        {"case": "rejected", "source": "reject"},
    ]


def test_outer_select_rejects_a_source_from_only_one_nested_case() -> None:
    graph = GraphBuilder(
        name="invalid-nested-decision",
        input_type=Request,
        output_type=Result,
        defaults=_defaults(),
    )
    classified = graph.add(graph.step()(classify))
    reviewed = graph.add(graph.step()(review))
    fast_result = graph.add(graph.step()(fast_track))
    manual_result = graph.add(graph.step()(manual_review))
    rejected_result = graph.add(graph.step()(reject))

    outer = graph.decision(classified, on="kind", id="outer")
    graph.edge_from(graph.start).to(classified)
    graph.edge_from(outer.case(Approved)).to(reviewed)
    graph.edge_from(outer.case(Rejected)).to(rejected_result)

    inner = graph.decision(reviewed, on="kind", id="inner")
    graph.edge_from(inner.case(FastTrack)).to(fast_result)
    graph.edge_from(inner.case(ManualReview)).to(manual_result)
    inner.select(Result, fast=fast_result, manual=manual_result)

    with pytest.raises(
        ValueError,
        match="source 'fast_track' has unresolved nested decision requirement inner='fast'",
    ):
        outer.select(Result, approved=fast_result, rejected=rejected_result)


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
