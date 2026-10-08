"""Static fan-in: merge/gather construction, emission, and call expansion."""

from __future__ import annotations

from pathlib import Path
from typing import Annotated, Any, Literal, cast

import pytest
from pydantic import BaseModel, Field

from massive import GraphBuilder, StepContext, container, execution, source_package

DEFAULTS = execution(
    environment=container(
        "example.invalid/fan-in@sha256:"
        "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
    )
)


class Value(BaseModel):
    value: int


class Left(BaseModel):
    kind: Literal["left"]
    value: int


class Right(BaseModel):
    kind: Literal["right"]
    value: int


class Middle(BaseModel):
    kind: Literal["middle"]
    value: int


Side = Annotated[Left | Right, Field(discriminator="kind")]
AnySide = Annotated[Left | Middle | Right, Field(discriminator="kind")]


def identity(context: StepContext[Value]) -> Value:
    return context.inputs


def to_left(context: StepContext[Value]) -> Left:
    return Left(kind="left", value=context.inputs.value)


def to_right(context: StepContext[Value]) -> Right:
    return Right(kind="right", value=context.inputs.value)


def classify(context: StepContext[Value]) -> Side:
    return Left(kind="left", value=context.inputs.value)


def to_number(context: StepContext[Value]) -> int:
    return context.inputs.value


def pair(context: StepContext[tuple[Value, Value]]) -> Value:
    return context.inputs[0]


def sides(context: StepContext[tuple[Left, Right]]) -> Value:
    return Value(value=context.inputs[0].value)


def values(context: StepContext[list[Value]]) -> Value:
    return context.inputs[0]


def gathered_sides(context: StepContext[list[Side]]) -> Value:
    return Value(value=context.inputs[0].value)


def undiscriminated_sides(context: StepContext[list[Left | Right]]) -> Value:
    return Value(value=context.inputs[0].value)


def any_sides(context: StepContext[list[AnySide]]) -> Value:
    return Value(value=context.inputs[0].value)


def items(context: StepContext[Value]) -> list[Value]:
    return [context.inputs]


def graph(name: str = "fan-in", input_type: Any = Value) -> GraphBuilder[Any, Value]:
    return GraphBuilder(name=name, input_type=input_type, output_type=Value, defaults=DEFAULTS)


def emit(built: GraphBuilder[Any, Any]) -> dict[str, Any]:
    source = source_package(
        root=Path(__file__).parent, include=[Path(__file__).name], package_id="fan-in"
    )
    return cast(dict[str, Any], built.emit(source=source).value)


def nodes(spec: dict[str, Any]) -> dict[str, dict[str, Any]]:
    return {node["id"]: node for node in spec["graph"]["nodes"]}


def diamond(built: GraphBuilder[Any, Value]) -> tuple[Any, Any]:
    split = built.add(identity, id="split")
    left = built.add(identity, id="a")
    right = built.add(identity, id="b")
    built.edge_from(built.start).to(split).to(left)
    built.edge_from(split).to(right)
    return left, right


def test_merge_emits_merge_inputs_in_argument_order() -> None:
    built = graph()
    left, right = diamond(built)
    built.merge(right, left).transform(pair).to_end(built.end)

    emitted = nodes(emit(built))

    assert emitted["pair"]["mergeInputs"] == ["b", "a"]
    assert "mergeInputs" not in emitted["a"]


def test_merge_requires_the_consumer_to_accept_the_positional_tuple() -> None:
    built = graph()
    split = built.add(identity, id="split")
    left = built.add(to_left)
    right = built.add(to_right)
    built.edge_from(built.start).to(split).to(left)
    built.edge_from(split).to(right)

    path = cast(Any, built.merge(right, left))
    with pytest.raises(TypeError, match="edge from 'to_right', 'to_left' has incompatible input"):
        path.to(built.add(sides))
    built.merge(left, right).to(built.add(sides, id="ordered"))


def test_gather_of_one_output_type_is_a_list_of_it() -> None:
    built = graph()
    left, right = diamond(built)
    built.gather(left, right).transform(values).to_end(built.end)

    assert nodes(emit(built))["values"]["mergeInputs"] == ["a", "b"]


def test_gather_of_different_models_requires_a_discriminated_consumer() -> None:
    built = graph()
    split = built.add(identity, id="split")
    left = built.add(to_left)
    right = built.add(to_right)
    built.edge_from(built.start).to(split).to(left)
    built.edge_from(split).to(right)

    with pytest.raises(TypeError, match="must be a Pydantic discriminated union"):
        cast(Any, built.gather(left, right)).to(built.add(undiscriminated_sides))
    with pytest.raises(TypeError, match="must decode exactly the gathered models"):
        cast(Any, built.gather(left, right)).to(built.add(any_sides))
    built.gather(left, right).transform(gathered_sides).to_end(built.end)

    assert nodes(emit(built))["gathered_sides"]["mergeInputs"] == ["to_left", "to_right"]


def test_gather_rejects_different_outputs_that_are_not_models() -> None:
    built = graph()
    split = built.add(identity, id="split")
    number = built.add(to_number)
    built.edge_from(built.start).to(split).to(number)

    with pytest.raises(TypeError, match="each source to produce a Pydantic model"):
        cast(Any, built).gather(split, number)


@pytest.mark.parametrize("source", ["start", "case", "duplicate", "foreign"])
def test_fan_in_sources_must_be_distinct_value_producers_of_this_graph(source: str) -> None:
    built = graph()
    left, _ = diamond(built)
    if source == "start":
        sources, error = (built.start, left), "'__start' must be a step, map, select, or call"
    elif source == "case":
        route = built.decision(built.add(classify), on="kind", id="route")
        sources, error = (route.case(Left), left), "'route' must be a step"
    elif source == "duplicate":
        sources, error = (left, left), "fan-in sources must be distinct"
    else:
        sources, error = (graph("other").add(identity, id="a"), left), "different graph"
    with pytest.raises((TypeError, ValueError), match=error):
        cast(Any, built).merge(*sources)


def test_a_fan_in_targets_one_step_or_call() -> None:
    built = graph()
    left, right = diamond(built)
    with pytest.raises(TypeError, match="fan-in must target a step or call"):
        cast(Any, built.gather(left, right)).to_end(built.end)

    consumer = built.add(values)
    built.gather(left, right).to(consumer)
    with pytest.raises(ValueError, match="'values' already receives a fan-in"):
        built.gather(right, left).to(consumer)


def test_emit_rejects_incoming_edges_that_bypass_a_fan_in() -> None:
    built = graph()
    left, right = diamond(built)
    joined = built.add(identity, id="joined")
    built.edge_from(left).to(joined).to_end(built.end)
    built.edge_from(right).to(joined)
    with pytest.raises(ValueError, match="step 'joined' has 2 incoming edges; join them"):
        emit(built)

    built = graph()
    left, right = diamond(built)
    extra = built.add(items)
    built.edge_from(left).to(extra)
    consumer = built.add(values)
    built.gather(left, right).to(consumer).to_end(built.end)
    built.edge_from(extra).to(consumer)
    with pytest.raises(ValueError, match="step 'values' receives edges outside its fan-in"):
        emit(built)


def _child(name: str, input_type: Any, populate: Any) -> GraphBuilder[Any, Value]:
    child = graph(name, input_type)
    populate(child).to_end(child.end)
    return child


def test_a_called_graph_can_consume_a_fan_in_and_matches_its_inlined_twin() -> None:
    def build(*, inline: bool) -> GraphBuilder[Any, Value]:
        built = graph()
        left, right = diamond(built)
        if inline:
            consumer = built.add(values, id="join--values")
        else:
            child = _child("join", list[Value], lambda g: g.edge_from(g.start).transform(values))
            consumer = built.call(child, id="join")
        built.gather(left, right).to(consumer).to_end(built.end)
        return built

    composed = emit(build(inline=False))

    assert composed == emit(build(inline=True))
    assert nodes(composed)["join--values"]["mergeInputs"] == ["a", "b"]


def test_a_call_receiving_a_fan_in_needs_a_child_that_starts_with_a_step() -> None:
    child = graph("mapped", list[Value])
    mapped = child.map(child.start, identity, id="each")
    child.edge_from(mapped).to(child.add(values)).to_end(child.end)
    built = graph()
    left, right = diamond(built)
    built.gather(left, right).to(built.call(child, id="join")).to_end(built.end)

    with pytest.raises(ValueError, match="call 'join' receives a fan-in, so child 'mapped' must"):
        emit(built)


def test_fan_in_sources_and_child_exits_can_be_calls() -> None:
    """A called graph may end in a merge, and its exit may feed a parent merge."""

    def build(*, inline: bool) -> GraphBuilder[Any, Value]:
        built = graph()
        split = built.add(identity, id="split")
        built.edge_from(built.start).to(split)
        if inline:
            first = built.add(identity, id="inner--first")
            second = built.add(identity, id="inner--second")
            built.edge_from(split).to(first).to(second)
            built.edge_from(first).to(other := built.add(identity, id="inner--other"))
            source = built.add(pair, id="inner--pair")
            built.merge(second, other).to(source)
        else:

            def populate(child: GraphBuilder[Any, Value]) -> Any:
                first = child.add(identity, id="first")
                second = child.add(identity, id="second")
                other = child.add(identity, id="other")
                child.edge_from(child.start).to(first).to(second)
                child.edge_from(first).to(other)
                return child.merge(second, other).transform(pair)

            source = built.call(_child("inner", Value, populate), id="inner")
            built.edge_from(split).to(source)
        sibling = built.add(identity, id="sibling")
        built.edge_from(split).to(sibling)
        built.merge(source, sibling).transform(pair, id="outer").to_end(built.end)
        return built

    composed = emit(build(inline=False))

    assert composed == emit(build(inline=True))
    emitted = nodes(composed)
    assert emitted["inner--pair"]["mergeInputs"] == ["inner--second", "inner--other"]
    assert emitted["outer"]["mergeInputs"] == ["inner--pair", "sibling"]


def test_call_still_requires_an_incoming_edge() -> None:
    child = _child("child", Value, lambda g: g.edge_from(g.start).transform(identity))
    built = graph()
    called = built.call(child, id="child")
    built.edge_from(built.start).to(built.add(identity)).to_end(built.end)
    built.edge_from(called).to(built.add(identity, id="after"))
    with pytest.raises(ValueError, match="call 'child' must have an incoming edge"):
        emit(built)
