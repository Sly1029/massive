"""Wiring that must fail type checking.

Each bad line carries one ignore per checker. pyright's
``reportUnnecessaryTypeIgnoreComment`` and ty's ``unused-ignore-comment`` fail
the build if a line stops being an error, so the suite proves the rejection.
"""

from __future__ import annotations

from pydantic import BaseModel

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
