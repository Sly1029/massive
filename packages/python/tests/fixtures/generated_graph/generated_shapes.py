"""Steps and a builder for graphs described by generated property-test data.

A ``GraphDescription`` is plain data, so the property tests can build it both
with ``GraphBuilder.call()`` and hand-inlined, pass it to ``massive run`` through
``workflow.py``, and predict its execution with an independent model.
"""

from __future__ import annotations

from collections.abc import Callable
from typing import Annotated, Any, Literal

from pydantic import BaseModel, ConfigDict, Field

from massive import GraphBuilder, StepContext, container, execution

DEFAULTS = execution(
    environment=container("example.invalid/python@sha256:" + "0" * 64, platform="linux/amd64")
)


class Leaf(BaseModel):
    model_config = ConfigDict(validate_by_name=True, validate_by_alias=True)

    label: str = Field(alias="ラベル")
    note: str | None = None
    weights: list[int]


class Payload(BaseModel):
    title: str = ""
    leaves: list[Leaf] = []
    counts: dict[str, int] = {}
    optional: int | None = None


class Token(BaseModel):
    """Workflow state: the step trace, routing bits, and map widths still to use."""

    trace: list[str]
    route: int
    widths: list[int]
    payload: Payload


class Left(BaseModel):
    kind: Literal["left"]
    token: Token


class Right(BaseModel):
    kind: Literal["right"]
    token: Token


Route = Annotated[Left | Right, Field(discriminator="kind")]


class Item(BaseModel):
    index: int
    token: Token
    mapped_by: str | None = None


def _visit(token: Token, step_id: str) -> Token:
    return token.model_copy(update={"trace": [*token.trace, step_id]})


def advance(ctx: StepContext[Token]) -> Token:
    return _visit(ctx.inputs, ctx.invocation.step_id)


async def advance_async(ctx: StepContext[Token]) -> Token:
    return _visit(ctx.inputs, ctx.invocation.step_id)


def classify(ctx: StepContext[Token]) -> Route:
    token = _visit(ctx.inputs, ctx.invocation.step_id)
    token = token.model_copy(update={"route": token.route >> 1})
    if ctx.inputs.route & 1:
        return Right(kind="right", token=token)
    return Left(kind="left", token=token)


def enter_left(ctx: StepContext[Left]) -> Token:
    return _visit(ctx.inputs.token, ctx.invocation.step_id)


def enter_right(ctx: StepContext[Right]) -> Token:
    return _visit(ctx.inputs.token, ctx.invocation.step_id)


def explode(ctx: StepContext[Token]) -> list[Item]:
    token = _visit(ctx.inputs, ctx.invocation.step_id)
    width, *rest = token.widths or [0]
    token = token.model_copy(update={"widths": rest})
    return [Item(index=index, token=token) for index in range(width)]


def mark(ctx: StepContext[Item]) -> Item:
    return ctx.inputs.model_copy(update={"mapped_by": ctx.invocation.step_id})


async def mark_async(ctx: StepContext[Item]) -> Item:
    return ctx.inputs.model_copy(update={"mapped_by": ctx.invocation.step_id})


def collect(ctx: StepContext[list[Item]]) -> Token:
    """Record item order; an empty map has no token to carry, so state restarts."""
    items = ctx.inputs
    entry = (
        f"{ctx.invocation.step_id}<"
        + ",".join(str(item.index) for item in items)
        + "|"
        + ",".join(sorted({str(item.mapped_by) for item in items}))
    )
    if not items:
        return Token(trace=[entry], route=0, widths=[], payload=Payload())
    token = items[0].token
    return token.model_copy(update={"trace": [*token.trace, entry]})


class StepNode(BaseModel):
    model_config = ConfigDict(frozen=True)

    kind: Literal["step"] = "step"
    id: str
    asynchronous: bool = False


class CallNode(BaseModel):
    """Sequential calls of one shared child graph, one per ID."""

    model_config = ConfigDict(frozen=True)

    kind: Literal["call"] = "call"
    ids: tuple[str, ...] = Field(min_length=1)
    body: tuple[Node, ...] = Field(min_length=1)


class Arm(BaseModel):
    """A case's entry step and body; ``via`` wraps both in a call (a case target)."""

    model_config = ConfigDict(frozen=True)

    entry: str
    via: str | None = None
    body: tuple[Node, ...] = ()


class DecideNode(BaseModel):
    """``via`` wraps the classifier in a call that is then the decision source."""

    model_config = ConfigDict(frozen=True)

    kind: Literal["decide"] = "decide"
    classifier: str
    via: str | None = None
    decision: str
    left: Arm
    right: Arm


class FanNode(BaseModel):
    """``via`` wraps the explode step in a call that is then the map source."""

    model_config = ConfigDict(frozen=True)

    kind: Literal["fan"] = "fan"
    explode: str
    via: str | None = None
    map: str
    collect: str
    concurrency: int = Field(ge=1)
    asynchronous: bool = False


Node = Annotated[StepNode | CallNode | DecideNode | FanNode, Field(discriminator="kind")]
CallNode.model_rebuild()
Arm.model_rebuild()


class GraphDescription(BaseModel):
    model_config = ConfigDict(frozen=True)

    body: tuple[Node, ...] = Field(min_length=1)


def build_graph(description: GraphDescription, *, inline: bool) -> GraphBuilder[Token, Token]:
    """Build with ``call()``, or inline the same nodes under '<call>--' prefixes."""
    graph = GraphBuilder(name="generated", input_type=Token, output_type=Token, defaults=DEFAULTS)
    graph.edge_from(_block(graph, graph.start, description.body, "", inline)).to(graph.end)
    return graph


Populate = Callable[[GraphBuilder[Any, Any], Any, str], Any]


def _then(graph: GraphBuilder[Any, Any], source: Any, target: Any) -> Any:
    graph.edge_from(source).to(target)
    return target


def _block(
    graph: GraphBuilder[Any, Any], source: Any, nodes: tuple[Node, ...], prefix: str, inline: bool
) -> Any:
    for node in nodes:
        source = _node(graph, source, node, prefix, inline)
    return source


def _within(
    graph: GraphBuilder[Any, Any],
    source: Any,
    via: str | None,
    prefix: str,
    inline: bool,
    types: tuple[Any, Any],
    populate: Populate,
) -> Any:
    if via is None:
        return populate(graph, source, prefix)
    if inline:
        return populate(graph, source, f"{prefix}{via}--")
    child = GraphBuilder(name=via, input_type=types[0], output_type=types[1], defaults=DEFAULTS)
    child.edge_from(populate(child, child.start, "")).to(child.end)
    return _then(graph, source, graph.call(child, id=f"{prefix}{via}"))


def _node(
    graph: GraphBuilder[Any, Any], source: Any, node: Node, prefix: str, inline: bool
) -> Any:
    if isinstance(node, StepNode):
        step = advance_async if node.asynchronous else advance
        return _then(graph, source, graph.add(step, id=prefix + node.id))
    if isinstance(node, CallNode):
        if inline:
            for call_id in node.ids:
                source = _block(graph, source, node.body, f"{prefix}{call_id}--", inline)
            return source
        child = GraphBuilder(
            name=node.ids[0], input_type=Token, output_type=Token, defaults=DEFAULTS
        )
        child.edge_from(_block(child, child.start, node.body, "", inline)).to(child.end)
        for call_id in node.ids:
            source = _then(graph, source, graph.call(child, id=prefix + call_id))
        return source
    if isinstance(node, DecideNode):
        classifier_id = node.classifier
        routed = _within(
            graph,
            source,
            node.via,
            prefix,
            inline,
            (Token, Route),
            lambda g, s, p: _then(g, s, g.add(classify, id=p + classifier_id)),
        )
        decision = graph.decision(routed, on="kind", id=prefix + node.decision)
        outcomes: dict[str, Any] = {}
        for tag, case, enter, arm in (
            ("left", Left, enter_left, node.left),
            ("right", Right, enter_right, node.right),
        ):

            def enter_arm(g: GraphBuilder[Any, Any], s: Any, p: str, enter=enter, arm=arm) -> Any:
                return _block(g, _then(g, s, g.add(enter, id=p + arm.entry)), arm.body, p, inline)

            outcomes[tag] = _within(
                graph, decision.case(case), arm.via, prefix, inline, (case, Token), enter_arm
            )
        return decision.select(Token, **outcomes)
    explode_id = node.explode
    items = _within(
        graph,
        source,
        node.via,
        prefix,
        inline,
        (Token, list[Item]),
        lambda g, s, p: _then(g, s, g.add(explode, id=p + explode_id)),
    )
    mapped = graph.map(
        items,
        mark_async if node.asynchronous else mark,
        id=prefix + node.map,
        concurrency=node.concurrency,
    )
    return _then(graph, mapped, graph.add(collect, id=prefix + node.collect))
