from __future__ import annotations

from datetime import timedelta
from typing import Annotated, Literal, Never

from pydantic import BaseModel, Field

from massive import (
    EdgePath,
    GraphBuilder,
    JsonValue,
    NodeHandle,
    StepContext,
    container,
    execution,
    retry,
)


class Request(BaseModel):
    value: int


class Result(BaseModel):
    value: int


class BatchRequest(BaseModel):
    values: list[Request]


class Metadata(BaseModel):
    values: dict[str, JsonValue]


class Approved(BaseModel):
    kind: Literal["approved"]
    value: int


class Rejected(BaseModel):
    kind: Literal["rejected"]
    reason: str


Route = Annotated[Approved | Rejected, Field(discriminator="kind")]


graph: GraphBuilder[Request, Result] = GraphBuilder(
    name="typed-authoring",
    input_type=Request,
    output_type=Result,
    defaults=execution(
        environment=container(
            "example.invalid/typed@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
            platform="linux/amd64",
        )
    ),
)


def increment(context: StepContext[Request]) -> Result:
    return Result(value=context.inputs.value + 1)


async def increment_async(context: StepContext[Request]) -> Result:
    return Result(value=context.inputs.value + 1)


sync_node: NodeHandle[Request, Result] = graph.add(increment)
async_node: NodeHandle[Request, Result] = graph.add(increment_async)
graph.edge_from(graph.start).to(sync_node).to_end(graph.end)

child_graph: GraphBuilder[Request, Result] = GraphBuilder(
    name="typed-child",
    input_type=Request,
    output_type=Result,
    defaults=graph.defaults,
)
child_node: NodeHandle[Request, Result] = child_graph.add(increment)
child_graph.edge_from(child_graph.start).to(child_node).to_end(child_graph.end)
parent_graph: GraphBuilder[Request, Result] = GraphBuilder(
    name="typed-parent",
    input_type=Request,
    output_type=Result,
    defaults=graph.defaults,
)
called: NodeHandle[Request, Result] = parent_graph.call(child_graph, id="child")
parent_graph.edge_from(parent_graph.start).to(called).to_end(parent_graph.end)


map_graph: GraphBuilder[BatchRequest, list[Result]] = GraphBuilder(
    name="typed-map",
    input_type=BatchRequest,
    output_type=list[Result],
    defaults=graph.defaults,
)


def unpack(context: StepContext[BatchRequest]) -> list[Request]:
    return context.inputs.values


def increment_item(context: StepContext[Request]) -> Result:
    return Result(value=context.inputs.value + 1)


requests: NodeHandle[BatchRequest, list[Request]] = map_graph.add(unpack)
mapped: NodeHandle[Never, list[Result]] = map_graph.map(
    requests,
    increment_item,
    id="increment-items",
    retry=retry(3, delay=timedelta(seconds=5)),
    timeout=timedelta(minutes=5),
)
map_graph.edge_from(map_graph.start).to(requests)
map_graph.edge_from(mapped).to_end(map_graph.end)


decision_graph: GraphBuilder[Request, Result] = GraphBuilder(
    name="typed-decisions",
    input_type=Request,
    output_type=Result,
    defaults=execution(
        environment=container(
            "example.invalid/typed-decisions@sha256:"
            "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
        )
    ),
)


def classify(context: StepContext[Request]) -> Route:
    return Approved(kind="approved", value=context.inputs.value)


def approve(context: StepContext[Approved]) -> Result:
    return Result(value=context.inputs.value)


def reject(context: StepContext[Rejected]) -> Result:
    return Result(value=0)


classified = decision_graph.add(classify)
approved_result = decision_graph.add(approve)
rejected_result = decision_graph.add(reject)
route = decision_graph.decision(classified, on="kind", id="route")
approved_input: NodeHandle[Never, Approved] = route.case(Approved)
rejected_input: NodeHandle[Never, Rejected] = route.case(Rejected)
decision_graph.edge_from(decision_graph.start).to(classified)
decision_graph.edge_from(approved_input).to(approved_result)
decision_graph.edge_from(rejected_input).to(rejected_result)
selected: NodeHandle[Never, Result] = route.select(
    Result, approved=approved_result, rejected=rejected_result
)
decision_graph.edge_from(selected).to_end(decision_graph.end)


map_select_graph: GraphBuilder[Request, list[Result]] = GraphBuilder(
    name="typed-map-select",
    input_type=Request,
    output_type=list[Result],
    defaults=graph.defaults,
)


def classify_for_map(context: StepContext[Request]) -> Route:
    return Approved(kind="approved", value=context.inputs.value)


def approved_items(context: StepContext[Approved]) -> list[Approved]:
    return [context.inputs]


def increment_approved(context: StepContext[Approved]) -> Result:
    return Result(value=context.inputs.value + 1)


def rejected_items(context: StepContext[Rejected]) -> list[Result]:
    return [Result(value=0)]


classified_for_map = map_select_graph.add(classify_for_map)
approved_source = map_select_graph.add(approved_items)
rejected_source = map_select_graph.add(rejected_items)
map_route = map_select_graph.decision(classified_for_map, on="kind", id="map-route")
approved_input = map_route.case(Approved)
rejected_input = map_route.case(Rejected)
approved_mapped: NodeHandle[Never, list[Result]] = map_select_graph.map(
    approved_source, increment_approved, id="increment-approved"
)
selected_map: NodeHandle[Never, list[Result]] = map_route.select(
    list[Result], approved=approved_mapped, rejected=rejected_source
)
map_select_graph.edge_from(map_select_graph.start).to(classified_for_map)
map_select_graph.edge_from(approved_input).to(approved_source)
map_select_graph.edge_from(rejected_input).to(rejected_source)
map_select_graph.edge_from(selected_map).to_end(map_select_graph.end)



def ordinary_increment(context: StepContext[Request]) -> Result:
    return Result(value=context.inputs.value + 1)


ordinary_node: NodeHandle[Request, Result] = graph.add(ordinary_increment)


class Document(BaseModel):
    text: str


class Summary(BaseModel):
    summary: str


class Prompt(BaseModel):
    prompt: str


class Answer(BaseModel):
    answer: str


def summarize(context: StepContext[Document]) -> Summary:
    return Summary(summary=context.inputs.text)


def to_prompt(context: StepContext[Summary]) -> Prompt:
    return Prompt(prompt=context.inputs.summary)


async def to_prompt_async(context: StepContext[Summary]) -> Prompt:
    return Prompt(prompt=context.inputs.summary)


def to_combined(context: StepContext[tuple[Prompt, Prompt]]) -> Prompt:
    weighed, tallied = context.inputs
    return Prompt(prompt=f"{weighed.prompt} ({tallied.prompt})")


def to_answer(context: StepContext[Prompt]) -> Answer:
    return Answer(answer=context.inputs.prompt)


summaries: GraphBuilder[Document, Summary] = GraphBuilder(
    name="summaries", input_type=Document, output_type=Summary, defaults=graph.defaults
)
summaries.edge_from(summaries.start).to(summaries.add(summarize)).to_end(summaries.end)
answers: GraphBuilder[Prompt, Answer] = GraphBuilder(
    name="answers", input_type=Prompt, output_type=Answer, defaults=graph.defaults
)
answers.edge_from(answers.start).to(answers.add(to_answer)).to_end(answers.end)

# A transform adapts one graph's output to the next graph's input.
pipeline: GraphBuilder[Document, Answer] = GraphBuilder(
    name="typed-pipeline", input_type=Document, output_type=Answer, defaults=graph.defaults
)
summary_call: NodeHandle[Document, Summary] = pipeline.call(summaries, id="summaries")
prompt_path: EdgePath[Prompt] = pipeline.edge_from(pipeline.start).to(summary_call).transform(
    to_prompt
)
prompt_path.to(pipeline.call(answers, id="answers")).to_end(pipeline.end)
async_prompt: EdgePath[Prompt] = pipeline.edge_from(summary_call).transform(
    to_prompt_async, id="to-prompt-async"
)


class Approval(BaseModel):
    kind: Literal["approval"]
    summary: str


class Objection(BaseModel):
    kind: Literal["objection"]
    reason: str


Review = Annotated[Approval | Objection, Field(discriminator="kind")]


def approve_summary(context: StepContext[Summary]) -> Approval:
    return Approval(kind="approval", summary=context.inputs.summary)


def object_to_summary(context: StepContext[Summary]) -> Objection:
    return Objection(kind="objection", reason=context.inputs.summary)


def weigh(context: StepContext[tuple[Approval, Objection]]) -> Prompt:
    approval, objection = context.inputs
    return Prompt(prompt=f"{approval.summary}: {objection.reason}")


def tally(context: StepContext[list[Review]]) -> Prompt:
    return Prompt(prompt=str(len(context.inputs)))


# Static fan-in: merge delivers a positional tuple, gather a list of the union.
reviews: GraphBuilder[Document, Answer] = GraphBuilder(
    name="typed-fan-in", input_type=Document, output_type=Answer, defaults=graph.defaults
)
reviewed = reviews.call(summaries, id="summaries")
reviews.edge_from(reviews.start).to(reviewed)
approval = reviews.add(approve_summary)
objection = reviews.add(object_to_summary)
reviews.edge_from(reviewed).to(approval)
reviews.edge_from(reviewed).to(objection)
weighed: EdgePath[tuple[Approval, Objection]] = reviews.merge(approval, objection)
gathered: EdgePath[list[Approval | Objection]] = reviews.gather(approval, objection)
weigh_node: NodeHandle[tuple[Approval, Objection], Prompt] = reviews.add(weigh)
tally_node: NodeHandle[list[Review], Prompt] = reviews.add(tally)
weighed.to(weigh_node)
gathered.to(tally_node)
reviews.merge(weigh_node, tally_node).transform(to_combined).to(
    reviews.call(answers, id="answers")
).to_end(reviews.end)


# Two fan-ins type-check but are rejected when the graph is built, so they stay
# valid here; tests/test_fan_in.py pins their build-time rejection.
class Base(BaseModel):
    value: int


class Sub(Base):
    extra: int


def to_base(context: StepContext[Document]) -> Base:
    return Base(value=len(context.inputs.text))


def to_sub(context: StepContext[Document]) -> Sub:
    return Sub(value=len(context.inputs.text), extra=0)


def bases(context: StepContext[list[Base]]) -> Answer:
    return Answer(answer=str(len(context.inputs)))


def first_document(context: StepContext[Document]) -> Document:
    return context.inputs


unchecked: GraphBuilder[Document, tuple[Document, Document]] = GraphBuilder(
    name="build-time-fan-in",
    input_type=Document,
    output_type=tuple[Document, Document],
    defaults=graph.defaults,
)
# Covariant outputs solve a model and its subclass to list[Base]; the build
# sees two different models and requires a discriminated consumer.
unchecked.gather(unchecked.add(to_base), unchecked.add(to_sub)).to(unchecked.add(bases))
# A tuple workflow output matches a merge's type, but the graph end takes one edge.
unchecked.merge(
    unchecked.add(first_document, id="first"), unchecked.add(first_document, id="second")
).to_end(unchecked.end)
