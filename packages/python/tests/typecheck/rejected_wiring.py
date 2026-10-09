"""Wiring that must fail type checking.

Each bad line carries one ignore per checker. pyright's
``reportUnnecessaryTypeIgnoreComment`` and ty's ``unused-ignore-comment`` fail
the build if a line stops being an error, so the suite proves the rejection.
"""

from __future__ import annotations

from typing import Annotated, Literal

from pydantic import BaseModel, Field

from massive import GraphBuilder, StepContext, container, execution

DEFAULTS = execution(
    environment=container(
        "example.invalid/rejected@sha256:"
        "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
    )
)


class Document(BaseModel):
    text: str


class Summary(BaseModel):
    summary: str


class Prompt(BaseModel):
    prompt: str


class Answer(BaseModel):
    answer: str


class Base(BaseModel):
    value: int


class Sub(Base):
    extra: int


def summarize(context: StepContext[Document]) -> Summary:
    return Summary(summary=context.inputs.text)


def to_prompt(context: StepContext[Summary]) -> Prompt:
    return Prompt(prompt=context.inputs.summary)


def to_answer(context: StepContext[Prompt]) -> Answer:
    return Answer(answer=context.inputs.prompt)


def to_document(context: StepContext[Summary]) -> Document:
    return Document(text=context.inputs.summary)


def make_sub(context: StepContext[Document]) -> Sub:
    return Sub(value=0, extra=0)


def wants_base(context: StepContext[Base]) -> Answer:
    return Answer(answer=str(context.inputs.value))


summaries = GraphBuilder(
    name="summaries", input_type=Document, output_type=Summary, defaults=DEFAULTS
)
summaries.edge_from(summaries.start).to(summaries.add(summarize)).to_end(summaries.end)
answers = GraphBuilder(name="answers", input_type=Prompt, output_type=Answer, defaults=DEFAULTS)
answers.edge_from(answers.start).to(answers.add(to_answer)).to_end(answers.end)

graph = GraphBuilder(name="rejected", input_type=Document, output_type=Answer, defaults=DEFAULTS)
summary = graph.call(summaries, id="summary")
answer = graph.call(answers, id="answer")

# One graph's Summary output into another graph's Prompt input.
graph.edge_from(summary).to(answer)  # pyright: ignore[reportArgumentType] # ty: ignore[invalid-argument-type]

# A path whose value is not the workflow output.
graph.edge_from(summary).to_end(graph.end)  # pyright: ignore[reportArgumentType] # ty: ignore[invalid-argument-type]

# Schemas must be equal, so a subclass output does not satisfy a base input.
graph.edge_from(graph.add(make_sub)).to(graph.add(wants_base))  # pyright: ignore[reportArgumentType] # ty: ignore[invalid-argument-type]

# A transform's return type flows on: a Document is not the Prompt `answer` needs.
graph.edge_from(summary).transform(to_document).to(answer)  # pyright: ignore[reportArgumentType] # ty: ignore[invalid-argument-type]

# A transform must accept the value on its path.
graph.edge_from(graph.start).transform(to_prompt)  # pyright: ignore[reportCallIssue, reportArgumentType] # ty: ignore[no-matching-overload]


class Left(BaseModel):
    side: Literal["left"]


class Right(BaseModel):
    side: Literal["right"]


Side = Annotated[Left | Right, Field(discriminator="side")]


def left(context: StepContext[Document]) -> Left:
    return Left(side="left")


def right(context: StepContext[Document]) -> Right:
    return Right(side="right")


def right_then_left(context: StepContext[tuple[Right, Left]]) -> Answer:
    return Answer(answer="")


def only_lefts(context: StepContext[list[Left]]) -> Answer:
    return Answer(answer="")


def sides(context: StepContext[list[Side]]) -> Answer:
    return Answer(answer="")


lefts = graph.add(left)
rights = graph.add(right)

# A merge is positional, so the consumer's tuple must list sources in order.
graph.merge(lefts, rights).to(graph.add(right_then_left))  # pyright: ignore[reportArgumentType] # ty: ignore[invalid-argument-type]

# A gather of different models is a list of their union, not of one of them.
graph.gather(lefts, rights).to(graph.add(only_lefts))  # pyright: ignore[reportArgumentType] # ty: ignore[invalid-argument-type]

# And a gather of one model is not a list of a wider union.
graph.gather(lefts, graph.add(left, id="more-lefts")).to(graph.add(sides))  # pyright: ignore[reportArgumentType] # ty: ignore[invalid-argument-type]


def left_pair(context: StepContext[tuple[Left, Left]]) -> Answer:
    return Answer(answer="")


# A merge's arity is its consumer's tuple length.
graph.merge(lefts, rights, graph.add(left, id="third-left")).to(graph.add(left_pair))  # pyright: ignore[reportArgumentType] # ty: ignore[invalid-argument-type]

# A merge joins at least two sources; a single value is an ordinary edge.
graph.merge(lefts)  # pyright: ignore[reportCallIssue] # ty: ignore[no-matching-overload]

# merge is typed per position for at most eight sources.
graph.merge(lefts, rights, lefts, rights, lefts, rights, lefts, rights, lefts)  # pyright: ignore[reportCallIssue] # ty: ignore[no-matching-overload]


def to_documents(context: StepContext[Document]) -> list[Document]:
    return [context.inputs]


def summarize_all(context: StepContext[list[Summary]]) -> Answer:
    return Answer(answer="")


# A map that collects item failures produces outcomes, not bare values.
collected = graph.map(graph.add(to_documents), summarize, id="collected", item_failures="collect")
graph.edge_from(collected).to(graph.add(summarize_all))  # pyright: ignore[reportArgumentType] # ty: ignore[invalid-argument-type]
