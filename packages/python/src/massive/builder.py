from __future__ import annotations

import inspect
import sys
from collections.abc import Awaitable, Callable
from dataclasses import dataclass, field, replace
from datetime import timedelta
from enum import Enum
from functools import reduce
from pathlib import Path
from types import GenericAlias, ModuleType
from typing import (
    Annotated,
    Any,
    Generic,
    Never,
    TypeVar,
    cast,
    get_args,
    get_origin,
    overload,
)

from pydantic import (
    BaseModel,
    ConfigDict,
    Field,
    StrictInt,
    TypeAdapter,
    ValidationError,
    model_validator,
)
from typing_extensions import TypeForm

from ._step import StepDefinition as _Step
from .canonical import JsonValue, canonical_json, sha256_ref
from .context import InputT, StepContext
from .contracts import ExecutionContract, Retry
from .hashing import SOURCE_PACKAGE_HASHING, WORKFLOW_SPEC_HASHING
from .identity import SAFE_PATH_SEGMENT, SafePathSegment
from .source_package import SourcePackage

OutputT = TypeVar("OutputT")
# A node's input is invariant because edges require exact schema equality; its
# output is covariant so heterogeneous handles solve to a union of outputs.
NodeInputT = TypeVar("NodeInputT")
NodeOutputT_co = TypeVar("NodeOutputT_co", covariant=True)
ValueT = TypeVar("ValueT")
ItemT = TypeVar("ItemT")
ResultT = TypeVar("ResultT")
WorkflowInputT = TypeVar("WorkflowInputT")
WorkflowOutputT = TypeVar("WorkflowOutputT")
CaseT = TypeVar("CaseT", bound=BaseModel)
SelectT = TypeVar("SelectT")
Merge1T = TypeVar("Merge1T")
Merge2T = TypeVar("Merge2T")
Merge3T = TypeVar("Merge3T")
Merge4T = TypeVar("Merge4T")
Merge5T = TypeVar("Merge5T")
Merge6T = TypeVar("Merge6T")
Merge7T = TypeVar("Merge7T")
Merge8T = TypeVar("Merge8T")

_START = "__start"
_END = "__end"
_NODE_KIND_ORDER = ("start", "step", "map", "decision", "select", "end")
# Graph IR versioning is independent from the outer WorkflowSpec transport
# schema so graph evolution remains an explicit compiler contract.
GRAPH_IR_VERSION = "0.3"
DEFAULT_MAP_CONCURRENCY = 20
MAX_MAP_CONCURRENCY = 2**32 - 1
_MAP_CONCURRENCY: TypeAdapter[int] = TypeAdapter[int](
    Annotated[StrictInt, Field(ge=1, le=MAX_MAP_CONCURRENCY)]
)


class SchemaPurpose(str, Enum):
    INPUT = "validation"
    OUTPUT = "serialization"


class _WorkflowIdentity(BaseModel):
    model_config = ConfigDict(frozen=True)

    name: SafePathSegment


class _DecisionIdentity(BaseModel):
    model_config = ConfigDict(frozen=True)

    id: SafePathSegment

    @model_validator(mode="after")
    def _reserve_select_suffix(self) -> _DecisionIdentity:
        if len(self.id) + len("-select") > 128:
            raise ValueError("must leave room for the derived '-select' node id")
        return self


class _MapIdentity(BaseModel):
    model_config = ConfigDict(frozen=True)

    id: SafePathSegment
    concurrency: Annotated[StrictInt, Field(ge=1, le=MAX_MAP_CONCURRENCY)]


@dataclass(frozen=True, slots=True)
class NodeHandle(Generic[NodeInputT, NodeOutputT_co]):
    """A graph node that consumes ``NodeInputT`` and produces ``NodeOutputT_co``.

    Only steps and calls accept edges. Start, map, select, and decision-case
    handles are value sources typed ``NodeHandle[Never, Output]``.
    """

    graph_token: object = field(repr=False, compare=False)
    node_id: str
    input_type: Any
    output_type: Any


@dataclass(frozen=True, slots=True)
class CaseHandle(NodeHandle[Never, CaseT], Generic[CaseT]):
    decision_id: str
    tag: str


@dataclass(slots=True)
class _DecisionDefinition:
    id: str
    source: NodeHandle[Any, Any]
    selector: str
    cases: dict[str, type[BaseModel]]
    claimed_cases: set[str]
    branch_roots: dict[str, str]


@dataclass(frozen=True, slots=True)
class _SelectDefinition:
    id: str
    decision_id: str
    output_type: Any
    inputs: dict[str, NodeHandle[Any, Any]]


@dataclass(frozen=True, slots=True)
class _MapDefinition:
    id: str
    source: NodeHandle[Any, Any]
    mapper: _Step[Any, Any]
    handle: NodeHandle[Any, Any]
    concurrency: int


class DecisionHandle(Generic[OutputT]):
    def __init__(self, graph: GraphBuilder[Any, Any], definition: _DecisionDefinition) -> None:
        self._graph = graph
        self._definition = definition

    def case(self, case_type: type[CaseT]) -> CaseHandle[CaseT]:
        return self._graph._claim_decision_case(  # pyright: ignore[reportPrivateUsage]
            self._definition.id, case_type
        )

    def select(
        self, output_type: TypeForm[SelectT], **inputs: NodeHandle[Any, SelectT]
    ) -> NodeHandle[Never, SelectT]:
        return self._graph._select_decision(  # pyright: ignore[reportPrivateUsage]
            self._definition.id, output_type, inputs
        )


@dataclass(frozen=True, slots=True)
class _EndHandle(Generic[WorkflowOutputT]):
    input_type: Any
    graph_token: object = field(repr=False, compare=False)


@dataclass(frozen=True, slots=True)
class _PathValue:
    """What a path delivers: one source's output, or a fan-in of several sources.

    A fan-in consumer receives the sources' values as one ordered JSON array, so
    it must accept exactly ``annotation``. A gather of different outputs is
    instead decoded through a discriminated union of exactly ``gathered_models``.
    """

    sources: tuple[NodeHandle[Any, Any], ...]
    annotation: Any
    gathered_models: frozenset[type[BaseModel]] = frozenset()

    def require_input(self, target_id: str, input_type: Any) -> None:
        origin = ", ".join(repr(source.node_id) for source in self.sources)
        if not self.gathered_models:
            if self.annotation != input_type:
                raise TypeError(f"edge from {origin} has incompatible input type")
            return
        if get_origin(input_type) is not list:
            raise TypeError(f"fan-in from {origin} into {target_id!r} must be consumed as a list")
        (item_type,) = get_args(input_type)
        _, cases = _tagged_union_cases(
            item_type, f"the gather consumer {target_id!r} item type for different models"
        )
        if frozenset(cases.values()) != self.gathered_models:
            raise TypeError(
                f"fan-in from {origin} into {target_id!r} must decode exactly the gathered models"
            )


class EdgePath(Generic[ValueT]):
    """A ``ValueT`` flowing out of a node, wired onward with ``to``/``to_end``.

    The methods are deliberately not overloaded so a type checker reports one
    precise mismatch at the offending edge.
    """

    def __init__(self, graph: GraphBuilder[Any, Any], value: _PathValue) -> None:
        self._graph = graph
        self._value = value

    def to(self, target: NodeHandle[ValueT, OutputT]) -> EdgePath[OutputT]:
        if isinstance(target, _EndHandle):  # Untyped workflows written for the old overload.
            raise TypeError("close a path with .to_end(graph.end) instead of .to(graph.end)")
        self._graph._connect(self._value, target)  # pyright: ignore[reportPrivateUsage]
        return EdgePath(self._graph, _PathValue((target,), target.output_type))

    def to_end(self, end: _EndHandle[ValueT]) -> None:
        self._graph._connect(self._value, end)  # pyright: ignore[reportPrivateUsage]

    # Awaitable-first overloads let every checker solve an async step's output
    # to the awaited value; ``OutputT | Awaitable[OutputT]`` is ambiguous to ty.
    @overload
    def transform(
        self,
        function: Callable[[StepContext[ValueT]], Awaitable[OutputT]],
        *,
        id: str | None = None,
        contract: ExecutionContract | None = None,
        retry: Retry | None = None,
        timeout: timedelta | None = None,
    ) -> EdgePath[OutputT]: ...

    @overload
    def transform(
        self,
        function: Callable[[StepContext[ValueT]], OutputT],
        *,
        id: str | None = None,
        contract: ExecutionContract | None = None,
        retry: Retry | None = None,
        timeout: timedelta | None = None,
    ) -> EdgePath[OutputT]: ...

    def transform(
        self,
        function: Callable[[StepContext[ValueT]], Any],
        *,
        id: str | None = None,
        contract: ExecutionContract | None = None,
        retry: Retry | None = None,
        timeout: timedelta | None = None,
    ) -> EdgePath[Any]:
        """Register ``function`` as an ordinary named step and wire this path into it."""
        step = self._graph.add(function, id=id, contract=contract, retry=retry, timeout=timeout)
        return self.to(step)


@dataclass(frozen=True, slots=True)
class WorkflowSpec:
    value: dict[str, JsonValue]
    spec_hash: str

    def to_json(self) -> str:
        return canonical_json(self.value)


class GraphBuilder(Generic[WorkflowInputT, WorkflowOutputT]):
    def __init__(
        self,
        *,
        name: str,
        input_type: type[WorkflowInputT],
        output_type: type[WorkflowOutputT],
        defaults: ExecutionContract,
    ) -> None:
        self.name = _WorkflowIdentity(name=name).name
        self.input_type = input_type
        self.output_type = output_type
        self.defaults = defaults
        self._graph_token = object()
        self.start = NodeHandle[Never, WorkflowInputT](
            graph_token=self._graph_token,
            node_id=_START,
            input_type=Never,
            output_type=input_type,
        )
        self.end = _EndHandle[WorkflowOutputT](
            input_type=output_type, graph_token=self._graph_token
        )
        self._nodes: dict[str, tuple[_Step[Any, Any], NodeHandle[Any, Any]]] = {}
        self._handles: dict[str, NodeHandle[Any, Any]] = {}
        self._edges: set[tuple[str, str]] = set()
        self._conditional_edges: set[tuple[str, str, str]] = set()
        self._decisions: dict[str, _DecisionDefinition] = {}
        self._selects: dict[str, _SelectDefinition] = {}
        self._maps: dict[str, _MapDefinition] = {}
        self._calls: dict[str, GraphBuilder[Any, Any]] = {}
        self._merges: dict[str, tuple[str, ...]] = {}
        self._emitted = False
        self._emitting = False
        self._cached_spec: WorkflowSpec | None = None
        self._cached_source: tuple[str, str] | None = None

    def call(
        self,
        graph: GraphBuilder[InputT, OutputT],
        *,
        id: str,
    ) -> NodeHandle[InputT, OutputT]:
        if self._emitted:
            raise RuntimeError("graph has already been emitted")
        node_id = SAFE_PATH_SEGMENT.validate_python(id)
        if node_id in self._known_node_ids():
            raise ValueError(f"duplicate or reserved call id {node_id!r}")
        handle = NodeHandle[InputT, OutputT](
            graph_token=self._graph_token,
            node_id=node_id,
            input_type=graph.input_type,
            output_type=graph.output_type,
        )
        self._calls[node_id] = graph
        self._handles[node_id] = handle
        return handle

    @overload
    def add(
        self,
        function: Callable[[StepContext[InputT]], Awaitable[OutputT]],
        *,
        id: str | None = None,
        contract: ExecutionContract | None = None,
        retry: Retry | None = None,
        timeout: timedelta | None = None,
    ) -> NodeHandle[InputT, OutputT]: ...

    @overload
    def add(
        self,
        function: Callable[[StepContext[InputT]], OutputT],
        *,
        id: str | None = None,
        contract: ExecutionContract | None = None,
        retry: Retry | None = None,
        timeout: timedelta | None = None,
    ) -> NodeHandle[InputT, OutputT]: ...

    def add(
        self,
        function: Callable[[StepContext[Any]], Any],
        *,
        id: str | None = None,
        contract: ExecutionContract | None = None,
        retry: Retry | None = None,
        timeout: timedelta | None = None,
    ) -> NodeHandle[Any, Any]:
        if self._emitted:
            raise RuntimeError("graph has already been emitted")
        item = _Step[Any, Any].from_callable(
            function, contract=self._registration_contract(contract, retry, timeout)
        )
        node_id = SAFE_PATH_SEGMENT.validate_python(id or item.function.__name__)
        if node_id in self._known_node_ids():
            raise ValueError(f"duplicate or reserved step id {node_id!r}")
        handle = NodeHandle[Any, Any](
            graph_token=self._graph_token,
            node_id=node_id,
            input_type=item.input_type,
            output_type=item.output_type,
        )
        self._nodes[node_id] = (item, handle)
        self._handles[node_id] = handle
        return handle

    @overload
    def map(
        self,
        source: NodeHandle[Any, list[ItemT]],
        mapper: Callable[[StepContext[ItemT]], Awaitable[ResultT]],
        *,
        id: str,
        concurrency: int = DEFAULT_MAP_CONCURRENCY,
        contract: ExecutionContract | None = None,
        retry: Retry | None = None,
        timeout: timedelta | None = None,
    ) -> NodeHandle[Never, list[ResultT]]: ...

    @overload
    def map(
        self,
        source: NodeHandle[Any, list[ItemT]],
        mapper: Callable[[StepContext[ItemT]], ResultT],
        *,
        id: str,
        concurrency: int = DEFAULT_MAP_CONCURRENCY,
        contract: ExecutionContract | None = None,
        retry: Retry | None = None,
        timeout: timedelta | None = None,
    ) -> NodeHandle[Never, list[ResultT]]: ...

    def map(
        self,
        source: NodeHandle[Any, Any],
        mapper: Callable[[StepContext[Any]], Any],
        *,
        id: str,
        concurrency: int = DEFAULT_MAP_CONCURRENCY,
        contract: ExecutionContract | None = None,
        retry: Retry | None = None,
        timeout: timedelta | None = None,
    ) -> NodeHandle[Never, Any]:
        if self._emitted:
            raise RuntimeError("graph has already been emitted")
        step = _Step[Any, Any].from_callable(
            mapper, contract=self._registration_contract(contract, retry, timeout)
        )
        source_id = source.node_id
        if source.graph_token is not self._graph_token:
            raise ValueError(f"map source {source_id!r} belongs to a different graph")
        source_item_schema = _direct_list_item_schema(
            source.output_type,
            f"map source {source_id!r}",
        )
        if source_item_schema != _normalized_core_schema(step.input_type):
            raise TypeError(f"map source {source_id!r} item type does not match mapper input type")
        identity = _MapIdentity(
            id=id,
            concurrency=_MAP_CONCURRENCY.validate_python(concurrency),
        )
        map_id = identity.id
        if map_id in self._known_node_ids():
            raise ValueError(f"duplicate or reserved map id {map_id!r}")
        output_type = list[step.output_type]
        handle = NodeHandle[Never, Any](
            graph_token=self._graph_token,
            node_id=map_id,
            input_type=Never,
            output_type=output_type,
        )
        self._maps[map_id] = _MapDefinition(
            id=map_id,
            source=source,
            mapper=step,
            handle=handle,
            concurrency=identity.concurrency,
        )
        self._handles[map_id] = handle
        self._add_edge(source_id, map_id)
        return handle

    def edge_from(self, source: NodeHandle[Any, OutputT]) -> EdgePath[OutputT]:
        if source.graph_token is not self._graph_token:
            raise ValueError("edge source belongs to a different graph")
        return EdgePath(self, _PathValue((source,), source.output_type))

    @overload
    def merge(
        self, first: NodeHandle[Any, Merge1T], second: NodeHandle[Any, Merge2T], /
    ) -> EdgePath[tuple[Merge1T, Merge2T]]: ...

    @overload
    def merge(
        self,
        first: NodeHandle[Any, Merge1T],
        second: NodeHandle[Any, Merge2T],
        third: NodeHandle[Any, Merge3T],
        /,
    ) -> EdgePath[tuple[Merge1T, Merge2T, Merge3T]]: ...

    @overload
    def merge(
        self,
        first: NodeHandle[Any, Merge1T],
        second: NodeHandle[Any, Merge2T],
        third: NodeHandle[Any, Merge3T],
        fourth: NodeHandle[Any, Merge4T],
        /,
    ) -> EdgePath[tuple[Merge1T, Merge2T, Merge3T, Merge4T]]: ...

    @overload
    def merge(
        self,
        first: NodeHandle[Any, Merge1T],
        second: NodeHandle[Any, Merge2T],
        third: NodeHandle[Any, Merge3T],
        fourth: NodeHandle[Any, Merge4T],
        fifth: NodeHandle[Any, Merge5T],
        /,
    ) -> EdgePath[tuple[Merge1T, Merge2T, Merge3T, Merge4T, Merge5T]]: ...

    @overload
    def merge(
        self,
        first: NodeHandle[Any, Merge1T],
        second: NodeHandle[Any, Merge2T],
        third: NodeHandle[Any, Merge3T],
        fourth: NodeHandle[Any, Merge4T],
        fifth: NodeHandle[Any, Merge5T],
        sixth: NodeHandle[Any, Merge6T],
        /,
    ) -> EdgePath[tuple[Merge1T, Merge2T, Merge3T, Merge4T, Merge5T, Merge6T]]: ...

    @overload
    def merge(
        self,
        first: NodeHandle[Any, Merge1T],
        second: NodeHandle[Any, Merge2T],
        third: NodeHandle[Any, Merge3T],
        fourth: NodeHandle[Any, Merge4T],
        fifth: NodeHandle[Any, Merge5T],
        sixth: NodeHandle[Any, Merge6T],
        seventh: NodeHandle[Any, Merge7T],
        /,
    ) -> EdgePath[tuple[Merge1T, Merge2T, Merge3T, Merge4T, Merge5T, Merge6T, Merge7T]]: ...

    @overload
    def merge(
        self,
        first: NodeHandle[Any, Merge1T],
        second: NodeHandle[Any, Merge2T],
        third: NodeHandle[Any, Merge3T],
        fourth: NodeHandle[Any, Merge4T],
        fifth: NodeHandle[Any, Merge5T],
        sixth: NodeHandle[Any, Merge6T],
        seventh: NodeHandle[Any, Merge7T],
        eighth: NodeHandle[Any, Merge8T],
        /,
    ) -> EdgePath[
        tuple[Merge1T, Merge2T, Merge3T, Merge4T, Merge5T, Merge6T, Merge7T, Merge8T]
    ]: ...

    def merge(
        self,
        first: NodeHandle[Any, Any],
        second: NodeHandle[Any, Any],
        /,
        *rest: NodeHandle[Any, Any],
    ) -> EdgePath[Any]:
        """Join sources into one positional tuple, in argument order, for a step or call."""
        sources = (first, second, *rest)
        annotation = GenericAlias(tuple, tuple(source.output_type for source in sources))
        return EdgePath(self, self._fan_in(sources, annotation))

    def gather(
        self,
        first: NodeHandle[Any, ValueT],
        second: NodeHandle[Any, ValueT],
        /,
        *rest: NodeHandle[Any, ValueT],
    ) -> EdgePath[list[ValueT]]:
        """Join sources into one list, in argument order, for a step or call.

        Sources with different outputs must each produce a Pydantic model, and
        the consumer must decode them through a discriminated union.
        """
        sources = (first, second, *rest)
        outputs: list[Any] = []
        for source in sources:
            if source.output_type not in outputs:
                outputs.append(source.output_type)
        if len(outputs) == 1:
            return EdgePath(self, self._fan_in(sources, GenericAlias(list, (outputs[0],))))
        models = frozenset(
            output
            for output in outputs
            if isinstance(output, type) and issubclass(output, BaseModel)
        )
        if len(models) != len(outputs):
            raise TypeError(
                "gathering different outputs requires each source to produce a Pydantic model "
                "with a Literal discriminator tag"
            )
        annotation = GenericAlias(list, (reduce(lambda union, model: union | model, outputs),))
        return EdgePath(self, self._fan_in(sources, annotation, models))

    def _fan_in(
        self,
        sources: tuple[NodeHandle[Any, Any], ...],
        annotation: Any,
        gathered_models: frozenset[type[BaseModel]] = frozenset(),
    ) -> _PathValue:
        if self._emitted:
            raise RuntimeError("graph has already been emitted")
        producers = {*self._nodes, *self._maps, *self._selects, *self._calls}
        for source in sources:
            if source.graph_token is not self._graph_token:
                raise ValueError("fan-in source belongs to a different graph")
            # Case handles carry their decision's id, so they are not producers.
            if source.node_id not in producers or isinstance(source, CaseHandle):
                raise TypeError(
                    f"fan-in source {source.node_id!r} must be a step, map, select, or call"
                )
        if len({source.node_id for source in sources}) != len(sources):
            raise ValueError("fan-in sources must be distinct")
        return _PathValue(sources, annotation, gathered_models)

    def _connect(self, value: _PathValue, target: NodeHandle[Any, Any] | _EndHandle[Any]) -> None:
        if target.graph_token is not self._graph_token:
            raise ValueError("edge target belongs to a different graph")
        target_id = target.node_id if isinstance(target, NodeHandle) else _END
        if target_id != _END and target_id not in self._nodes and target_id not in self._calls:
            raise TypeError(f"edge target {target_id!r} is not a step or call")
        fan_in = len(value.sources) > 1
        if fan_in and target_id == _END:
            raise TypeError("a fan-in must target a step or call, not the graph end")
        if fan_in and target_id in self._merges:
            raise ValueError(f"{target_id!r} already receives a fan-in")
        value.require_input(target_id, target.input_type)
        for source in value.sources:
            case = source.tag if isinstance(source, CaseHandle) else None
            self._add_edge(source.node_id, target_id, case)
        if fan_in:
            self._merges[target_id] = tuple(source.node_id for source in value.sources)

    def decision(
        self, source: NodeHandle[Any, OutputT], *, on: str, id: str
    ) -> DecisionHandle[OutputT]:
        if self._emitted:
            raise RuntimeError("graph has already been emitted")
        if source.graph_token is not self._graph_token:
            raise ValueError("decision source belongs to a different graph")
        if source.node_id == _START:
            raise TypeError("decision source must be a step, map, select, or call")
        identity = _DecisionIdentity(id=id)
        if identity.id in self._known_node_ids():
            raise ValueError(f"duplicate or reserved decision id {identity.id!r}")
        discriminator, cases = _tagged_union_cases(source.output_type, "decision input")
        if discriminator != on:
            raise TypeError(f"decision selector {on!r} does not match the Pydantic discriminator")
        definition = _DecisionDefinition(
            id=identity.id,
            source=source,
            selector=on,
            cases=cases,
            claimed_cases=set(),
            branch_roots={},
        )
        self._decisions[identity.id] = definition
        self._add_edge(source.node_id, identity.id, None)
        return DecisionHandle(self, definition)

    def _claim_decision_case(self, decision_id: str, case_type: type[CaseT]) -> CaseHandle[CaseT]:
        if self._emitted:
            raise RuntimeError("graph has already been emitted")
        definition = self._decisions[decision_id]
        matching_tags = [tag for tag, model in definition.cases.items() if model is case_type]
        if not matching_tags:
            raise TypeError(f"{case_type.__name__} is not a case for decision {decision_id!r}")
        tag = matching_tags[0]
        if tag in definition.claimed_cases:
            raise ValueError(f"decision {decision_id!r} case {tag!r} is already connected")
        definition.claimed_cases.add(tag)
        return CaseHandle(
            graph_token=self._graph_token,
            node_id=decision_id,
            input_type=Never,
            output_type=case_type,
            decision_id=decision_id,
            tag=tag,
        )

    def _select_decision(
        self,
        decision_id: str,
        output_type: TypeForm[SelectT],
        inputs: dict[str, NodeHandle[Any, SelectT]],
    ) -> NodeHandle[Never, SelectT]:
        if self._emitted:
            raise RuntimeError("graph has already been emitted")
        definition = self._decisions[decision_id]
        expected_tags = set(definition.cases)
        if definition.claimed_cases != expected_tags:
            missing = sorted(expected_tags - definition.claimed_cases, key=_canonical_sort_key)
            raise ValueError(
                f"decision {decision_id!r} has unconnected cases: {', '.join(missing)}"
            )
        if set(inputs) != expected_tags:
            missing = sorted(expected_tags - set(inputs), key=_canonical_sort_key)
            extra = sorted(set(inputs) - expected_tags, key=_canonical_sort_key)
            details = [
                *(f"missing {tag!r}" for tag in missing),
                *(f"unknown {tag!r}" for tag in extra),
            ]
            raise ValueError(
                f"decision {decision_id!r} select cases must match exactly: {', '.join(details)}"
            )
        lineages = self._activation_lineages()
        for tag, source in inputs.items():
            if source.graph_token is not self._graph_token:
                raise ValueError("decision select source belongs to a different graph")
            if _normalized_core_schema(source.output_type) != _normalized_core_schema(output_type):
                raise TypeError(
                    f"decision {decision_id!r} case {tag!r} output type does not match select output"
                )
            root = definition.branch_roots.get(tag)
            if root is not None and not self._is_reachable(root, source.node_id):
                raise ValueError(
                    f"decision {decision_id!r} case {tag!r} select source is not in that branch"
                )
            if root is not None:
                self._validate_select_source_lineage(
                    definition,
                    tag,
                    source,
                    lineages,
                )
        select_id = f"{decision_id}-select"
        if select_id in self._known_node_ids():
            raise ValueError(f"duplicate derived select id {select_id!r}")
        self._selects[select_id] = _SelectDefinition(
            id=select_id,
            decision_id=decision_id,
            output_type=output_type,
            inputs=dict(inputs),
        )
        for source in inputs.values():
            self._add_edge(source.node_id, select_id, None)
        handle = NodeHandle[Never, SelectT](
            graph_token=self._graph_token,
            node_id=select_id,
            input_type=Never,
            output_type=output_type,
        )
        self._handles[select_id] = handle
        return handle

    def _add_edge(self, source: str, target: str, case: str | None = None) -> None:
        if self._emitted:
            raise RuntimeError("graph has already been emitted")
        if source not in self._known_node_ids():
            raise ValueError(f"unknown edge source {source!r}")
        if target not in self._known_node_ids():
            raise ValueError(f"unknown edge target {target!r}")
        if case is not None:
            definition = self._decisions.get(source)
            if definition is None or case not in definition.cases:
                raise ValueError(f"unknown conditional decision edge {source!r}:{case!r}")
            existing_root = definition.branch_roots.get(case)
            if existing_root is not None:
                raise ValueError(
                    f"decision {source!r} case {case!r} already has branch root {existing_root!r}"
                )
            definition.branch_roots[case] = target
            self._conditional_edges.add((source, target, case))
            return
        self._edges.add((source, target))

    def _registration_contract(
        self,
        contract: ExecutionContract | None,
        retry: Retry | None,
        timeout: timedelta | None,
    ) -> ExecutionContract | None:
        """Override only the given retry/timeout fields of the selected contract."""
        if retry is None and timeout is None:
            return contract
        base = contract or self.defaults
        return replace(
            base,
            retry=base.retry if retry is None else retry,
            timeout=base.timeout if timeout is None else timeout,
        )

    def _known_node_ids(self) -> set[str]:
        return {
            _START,
            _END,
            *self._nodes,
            *self._decisions,
            *self._selects,
            *self._maps,
            *self._calls,
        }

    def _is_reachable(self, source: str, target: str) -> bool:
        pending = [source]
        seen: set[str] = set()
        adjacency: dict[str, list[str]] = {}
        for edge_source, edge_target in self._edges:
            adjacency.setdefault(edge_source, []).append(edge_target)
        for edge_source, edge_target, _ in self._conditional_edges:
            adjacency.setdefault(edge_source, []).append(edge_target)
        while pending:
            current = pending.pop()
            if current == target:
                return True
            if current in seen:
                continue
            seen.add(current)
            pending.extend(adjacency.get(current, ()))
        return False

    def _activation_lineages(self) -> dict[str, dict[str, str]]:
        inbound: dict[str, list[tuple[str, str | None]]] = {}
        for source, target in self._edges:
            inbound.setdefault(target, []).append((source, None))
        for source, target, tag in self._conditional_edges:
            inbound.setdefault(target, []).append((source, tag))

        pending = self._known_node_ids()
        lineages: dict[str, dict[str, str]] = {}
        while pending:
            progressed = False
            for node_id in sorted(pending, key=_canonical_sort_key):
                select = self._selects.get(node_id)
                if select is not None:
                    enclosing = lineages.get(select.decision_id)
                    if enclosing is None:
                        continue
                    lineages[node_id] = dict(enclosing)
                    pending.remove(node_id)
                    progressed = True
                    break

                dependencies = inbound.get(node_id, [])
                if any(source not in lineages for source, _ in dependencies):
                    continue
                lineage: dict[str, str] = {}
                for source, case in dependencies:
                    for decision_id, tag in lineages[source].items():
                        existing = lineage.get(decision_id)
                        if existing is not None and existing != tag:
                            raise ValueError(
                                f"node {node_id!r} requires incompatible cases of decision "
                                f"{decision_id!r}"
                            )
                        lineage[decision_id] = tag
                    if case is not None:
                        lineage[source] = case
                lineages[node_id] = lineage
                pending.remove(node_id)
                progressed = True
                break
            if not progressed:
                break
        return lineages

    def _validate_select_source_lineage(
        self,
        definition: _DecisionDefinition,
        tag: str,
        source: NodeHandle[Any, Any],
        lineages: dict[str, dict[str, str]],
    ) -> None:
        enclosing = lineages.get(definition.id)
        actual = lineages.get(source.node_id)
        if enclosing is None or actual is None:
            return
        expected = {**enclosing, definition.id: tag}
        if actual == expected:
            return
        extras = [
            f"{decision_id}={actual_tag!r}"
            for decision_id, actual_tag in sorted(
                actual.items(), key=lambda item: _canonical_sort_key(item[0])
            )
            if expected.get(decision_id) != actual_tag
        ]
        if extras:
            raise ValueError(
                f"decision {definition.id!r} case {tag!r} select source "
                f"{source.node_id!r} has unresolved nested decision requirement "
                f"{', '.join(extras)}; select nested decision outputs before using them "
                "in an enclosing select"
            )
        raise ValueError(
            f"decision {definition.id!r} case {tag!r} select source "
            f"{source.node_id!r} is not active for the whole decision case"
        )

    def emit(self, *, source: SourcePackage) -> WorkflowSpec:
        if self._emitted:
            raise RuntimeError("graph has already been emitted")
        if self._emitting:
            raise ValueError("recursive workflow calls are not supported")
        self._emitting = True
        try:
            return self._emit(source=source)
        finally:
            self._emitting = False

    def _emit(self, *, source: SourcePackage) -> WorkflowSpec:
        if not self._nodes and not self._maps and not self._calls:
            raise ValueError("workflow must contain at least one step")
        if not any(source_id == _START for source_id, _ in self._edges):
            raise ValueError("workflow start has no edge")
        if not any(target_id == _END for _, target_id in self._edges):
            raise ValueError("workflow end has no edge")

        for map_id in self._maps:
            if not any(edge_source == map_id for edge_source, _ in self._edges):
                raise ValueError(f"map {map_id!r} must have an outgoing edge")

        inbound: dict[str, set[str]] = {}
        for edge_source, edge_target in self._edges:
            inbound.setdefault(edge_target, set()).add(edge_source)
        for edge_source, edge_target, _ in self._conditional_edges:
            inbound.setdefault(edge_target, set()).add(edge_source)
        for node_id in (*self._nodes, *self._calls):
            kind = "call" if node_id in self._calls else "step"
            sources = inbound.get(node_id, set())
            merged = self._merges.get(node_id)
            if merged is not None and sources != set(merged):
                raise ValueError(f"{kind} {node_id!r} receives edges outside its fan-in")
            if merged is None and len(sources) > 1:
                raise ValueError(
                    f"{kind} {node_id!r} has {len(sources)} incoming edges; join them with "
                    "graph.merge(...) or graph.gather(...)"
                )
        for call_id in self._calls:
            if call_id not in inbound:
                raise ValueError(f"call {call_id!r} must have an incoming edge")
            if not any(source == call_id for source, _ in self._edges):
                raise ValueError(f"call {call_id!r} must have an outgoing edge")

        lineages = self._activation_lineages()
        for select in self._selects.values():
            decision = self._decisions[select.decision_id]
            for tag, selected_source in select.inputs.items():
                root = decision.branch_roots.get(tag)
                if root is None:
                    raise ValueError(f"decision {decision.id!r} case {tag!r} has no branch edge")
                if not self._is_reachable(root, selected_source.node_id):
                    raise ValueError(
                        f"decision {decision.id!r} case {tag!r} select source is not in that branch"
                    )
                self._validate_select_source_lineage(
                    decision,
                    tag,
                    selected_source,
                    lineages,
                )

        source_files, package_hash = source.manifest()
        schema_table: dict[str, JsonValue] = {}

        def schema_ref(annotation: Any, role: str, purpose: SchemaPurpose) -> str:
            adapter = TypeAdapter(annotation)
            schema = cast(JsonValue, adapter.json_schema(mode=purpose.value))
            canonical_shape = cast(
                JsonValue,
                adapter.json_schema(
                    mode=(
                        SchemaPurpose.OUTPUT.value
                        if purpose is SchemaPurpose.INPUT
                        else purpose.value
                    )
                ),
            )
            _assert_canonical_json_schema(canonical_shape, role)
            reference = sha256_ref(canonical_json(schema))
            schema_table[reference] = schema
            return reference

        input_schema = schema_ref(self.input_type, "workflow input schema", SchemaPurpose.INPUT)
        output_schema = schema_ref(self.output_type, "workflow output schema", SchemaPurpose.OUTPUT)
        environments: dict[str, JsonValue] = {}
        contracts: dict[str, JsonValue] = {}

        def contract_ref(contract: ExecutionContract) -> str:
            environment = cast(JsonValue, contract.environment.as_json())
            environment_ref = sha256_ref(canonical_json(environment))
            environments[environment_ref] = environment
            contract_value: dict[str, JsonValue] = {"environmentRef": environment_ref}
            for key, item in contract.as_json().items():
                if key != "environment":
                    contract_value[key] = cast(JsonValue, item)
            value = cast(JsonValue, contract_value)
            reference = sha256_ref(canonical_json(value))
            contracts[reference] = value
            return reference

        nodes: list[dict[str, JsonValue]] = [{"id": _START, "kind": "start"}]
        symbols: dict[str, JsonValue] = {}
        for node_id, (step, _) in sorted(self._nodes.items()):
            module = _symbol_module(step.function, source.root)
            symbol_ref = f"{source.package_id}:{module}#{step.function.__name__}"
            symbols[symbol_ref] = {
                "packageId": source.package_id,
                "language": "python",
                "module": module,
                "export": step.function.__name__,
            }
            merge_inputs: dict[str, JsonValue] = (
                {"mergeInputs": list(self._merges[node_id])} if node_id in self._merges else {}
            )
            nodes.append(
                {
                    **merge_inputs,
                    "id": node_id,
                    "kind": "step",
                    "inputSchema": schema_ref(
                        step.input_type, f"step {node_id!r} input schema", SchemaPurpose.INPUT
                    ),
                    "outputSchema": schema_ref(
                        step.output_type, f"step {node_id!r} output schema", SchemaPurpose.OUTPUT
                    ),
                    "symbolRef": symbol_ref,
                    "contractRef": contract_ref(step.contract or self.defaults),
                }
            )
        for map_id, definition in sorted(
            self._maps.items(), key=lambda item: _canonical_sort_key(item[0])
        ):
            module = _symbol_module(definition.mapper.function, source.root)
            symbol_ref = f"{source.package_id}:{module}#{definition.mapper.function.__name__}"
            symbols[symbol_ref] = {
                "packageId": source.package_id,
                "language": "python",
                "module": module,
                "export": definition.mapper.function.__name__,
            }
            nodes.append(
                {
                    "id": map_id,
                    "kind": "map",
                    "inputSchema": schema_ref(
                        definition.source.output_type,
                        f"map {map_id!r} input schema",
                        SchemaPurpose.INPUT,
                    ),
                    "itemInputSchema": schema_ref(
                        definition.mapper.input_type,
                        f"map {map_id!r} item input schema",
                        SchemaPurpose.INPUT,
                    ),
                    "itemOutputSchema": schema_ref(
                        definition.mapper.output_type,
                        f"map {map_id!r} item output schema",
                        SchemaPurpose.OUTPUT,
                    ),
                    "outputSchema": schema_ref(
                        definition.handle.output_type,
                        f"map {map_id!r} output schema",
                        SchemaPurpose.OUTPUT,
                    ),
                    "symbolRef": symbol_ref,
                    "contractRef": contract_ref(definition.mapper.contract or self.defaults),
                    "maxConcurrency": definition.concurrency,
                }
            )
        for call_id in sorted(self._calls, key=_canonical_sort_key):
            call_node: dict[str, JsonValue] = {"id": call_id, "kind": "call"}
            if call_id in self._merges:
                call_node["mergeInputs"] = list(self._merges[call_id])
            nodes.append(call_node)
        for decision_id, decision in sorted(
            self._decisions.items(), key=lambda item: _canonical_sort_key(item[0])
        ):
            nodes.append(
                {
                    "id": decision_id,
                    "kind": "decision",
                    "inputSchema": schema_ref(
                        decision.source.output_type,
                        f"decision {decision_id!r} input schema",
                        SchemaPurpose.OUTPUT,
                    ),
                    "selector": decision.selector,
                    "cases": [
                        {
                            "tag": tag,
                            "schema": schema_ref(
                                case_type,
                                f"decision {decision_id!r} case {tag!r} schema",
                                SchemaPurpose.INPUT,
                            ),
                        }
                        for tag, case_type in sorted(
                            decision.cases.items(), key=lambda item: _canonical_sort_key(item[0])
                        )
                    ],
                }
            )
        for select_id, select in sorted(
            self._selects.items(), key=lambda item: _canonical_sort_key(item[0])
        ):
            nodes.append(
                {
                    "id": select_id,
                    "kind": "select",
                    "decisionRef": select.decision_id,
                    "outputSchema": schema_ref(
                        select.output_type,
                        f"select {select_id!r} output schema",
                        SchemaPurpose.OUTPUT,
                    ),
                    "selectInputs": [
                        {"case": tag, "source": source.node_id}
                        for tag, source in sorted(
                            select.inputs.items(), key=lambda item: _canonical_sort_key(item[0])
                        )
                    ],
                }
            )
        nodes.append({"id": _END, "kind": "end"})
        edges: list[dict[str, JsonValue]] = [
            {"from": edge_source, "to": edge_target} for edge_source, edge_target in self._edges
        ]
        edges.extend(
            {"from": edge_source, "to": edge_target, "case": case}
            for edge_source, edge_target, case in self._conditional_edges
        )
        edges.sort(
            key=lambda edge: tuple(
                _canonical_sort_key(cast(str, edge[key]))
                for key in ("from", "to", "case")
                if key in edge
            )
        )
        for call_id, child in sorted(
            self._calls.items(), key=lambda item: _canonical_sort_key(item[0])
        ):
            if child._emitting:
                raise ValueError(f"recursive workflow call at {call_id!r}")
            if child._cached_spec is None:
                child_spec = child.emit(source=source)
            else:
                child_spec = child._cached_spec
                if child._cached_source != (source.package_id, package_hash):
                    raise ValueError(
                        f"called workflow {child.name!r} was emitted from a different source package"
                    )
            child_value = child_spec.value
            for table, entries in (
                (schema_table, cast(dict[str, JsonValue], child_value["schemas"])),
                (symbols, cast(dict[str, JsonValue], child_value["symbols"])),
                (environments, cast(dict[str, JsonValue], child_value["environments"])),
                (contracts, cast(dict[str, JsonValue], child_value["contracts"])),
            ):
                table.update(entries)
            child_graph = cast(dict[str, JsonValue], child_value["graph"])
            child_nodes = cast(list[dict[str, JsonValue]], child_graph["nodes"])
            child_edges = cast(list[dict[str, JsonValue]], child_graph["edges"])
            entries = [cast(str, edge["to"]) for edge in child_edges if edge["from"] == _START]
            exits = [cast(str, edge["from"]) for edge in child_edges if edge["to"] == _END]
            if len(entries) != 1 or len(exits) != 1 or entries[0] == _END or exits[0] == _START:
                raise ValueError(
                    f"call {call_id!r} requires child {child.name!r} to have exactly one "
                    "start successor and one end predecessor that are child nodes"
                )
            first, last = entries[0], exits[0]
            call_node = next(node for node in nodes if node["id"] == call_id)
            scoped: dict[str, str] = {}
            for node in child_nodes:
                child_id = cast(str, node["id"])
                if child_id in (_START, _END):
                    continue
                scoped_id = f"{call_id}--{child_id}"
                try:
                    scoped[child_id] = SAFE_PATH_SEGMENT.validate_python(scoped_id)
                except ValidationError as error:
                    raise ValueError(
                        f"call {call_id!r} scopes child node {child_id!r} as a "
                        f"{len(scoped_id)}-character id; '<call id>--<child node id>' must "
                        "fit the 128-character node id limit"
                    ) from error
            occupied = {cast(str, node["id"]) for node in nodes}
            if occupied.intersection(scoped.values()):
                raise ValueError(f"call {call_id!r} produces a duplicate scoped node id")
            for node in child_nodes:
                if node["id"] in (_START, _END):
                    continue
                expanded = dict(node)
                expanded["id"] = scoped[cast(str, node["id"])]
                if "decisionRef" in expanded:
                    expanded["decisionRef"] = scoped[cast(str, expanded["decisionRef"])]
                if "mergeInputs" in expanded:
                    expanded["mergeInputs"] = [
                        scoped[item] for item in cast(list[str], expanded["mergeInputs"])
                    ]
                if "selectInputs" in expanded:
                    expanded["selectInputs"] = [
                        {**item, "source": scoped[cast(str, item["source"])]}
                        for item in cast(list[dict[str, JsonValue]], expanded["selectInputs"])
                    ]
                if node["id"] == first and "mergeInputs" in call_node:
                    # The call's fan-in becomes its child's entry step's fan-in.
                    if expanded["kind"] != "step":
                        raise ValueError(
                            f"call {call_id!r} receives a fan-in, so child {child.name!r} "
                            "must start with a step"
                        )
                    expanded["mergeInputs"] = call_node["mergeInputs"]
                nodes.append(expanded)
            expanded_edges: list[dict[str, JsonValue]] = []
            for edge in edges:
                if edge["to"] == call_id:
                    expanded_edges.append({**edge, "to": scoped[first]})
                elif edge["from"] == call_id:
                    expanded_edges.append({**edge, "from": scoped[last]})
                else:
                    expanded_edges.append(edge)
            for edge in child_edges:
                if edge["from"] == _START or edge["to"] == _END:
                    continue
                expanded_edges.append(
                    {
                        **edge,
                        "from": scoped[cast(str, edge["from"])],
                        "to": scoped[cast(str, edge["to"])],
                    }
                )
            edges = expanded_edges
            for node in nodes:
                if node.get("kind") == "select" and "selectInputs" in node:
                    node["selectInputs"] = [
                        {
                            **item,
                            "source": scoped[last] if item["source"] == call_id else item["source"],
                        }
                        for item in cast(list[dict[str, JsonValue]], node["selectInputs"])
                    ]
                if "mergeInputs" in node:
                    node["mergeInputs"] = [
                        scoped[last] if item == call_id else item
                        for item in cast(list[str], node["mergeInputs"])
                    ]
            nodes = [node for node in nodes if node["id"] != call_id]
        # Expanded child nodes join the parent's canonical order, so a call emits
        # the same spec as the equivalent hand-inlined graph.
        nodes.sort(
            key=lambda node: (
                _NODE_KIND_ORDER.index(cast(str, node["kind"])),
                _canonical_sort_key(cast(str, node["id"])),
            )
        )
        edges.sort(
            key=lambda edge: tuple(
                _canonical_sort_key(cast(str, edge[key]))
                for key in ("from", "to", "case")
                if key in edge
            )
        )
        value = cast(
            JsonValue,
            {
                "kind": "WorkflowSpec",
                "schemaVersion": 0,
                "encoding": "json-v0",
                "hashing": WORKFLOW_SPEC_HASHING.as_json(),
                "workflow": {
                    "name": self.name,
                    "inputSchema": input_schema,
                    "outputSchema": output_schema,
                },
                "graph": {
                    "irVersion": GRAPH_IR_VERSION,
                    "start": _START,
                    "end": _END,
                    "nodes": nodes,
                    "edges": edges,
                },
                "schemas": schema_table,
                "symbols": symbols,
                "sourcePackages": {
                    source.package_id: {
                        "packageId": source.package_id,
                        "language": "python",
                        "packageHash": package_hash,
                        "hashing": SOURCE_PACKAGE_HASHING.as_json(),
                        "files": source_files,
                    }
                },
                "environments": environments,
                "contracts": contracts,
            },
        )
        spec_hash = sha256_ref(canonical_json(value))
        emitted = {**cast(dict[str, JsonValue], value), "specHash": spec_hash}
        self._emitted = True
        result = WorkflowSpec(value=emitted, spec_hash=spec_hash)
        self._cached_spec = result
        self._cached_source = (source.package_id, package_hash)
        return result


def _direct_list_item_schema(annotation: Any, role: str) -> object:
    """Return a normalized Pydantic core schema for a direct ``list[T]`` item."""
    root, definitions = _core_schema_parts(annotation)
    if root.get("type") != "list":
        raise TypeError(f"{role} must be a direct concrete list[T]")
    item_schema = _schema_mapping(root.get("items_schema"))
    if item_schema is None or item_schema.get("type") == "any":
        raise TypeError(f"{role} must be a direct concrete list[T]")
    return _normalize_core_schema_node(item_schema, definitions, set())


def _normalized_core_schema(annotation: Any) -> object:
    root, definitions = _core_schema_parts(annotation)
    return _normalize_core_schema_node(root, definitions, set())


def _core_schema_parts(annotation: Any) -> tuple[dict[str, object], dict[str, dict[str, object]]]:
    adapter: TypeAdapter[object] = TypeAdapter(annotation)
    core_schema = cast(dict[str, object], adapter.core_schema)
    definitions: dict[str, dict[str, object]] = {}

    def collect(value: object) -> None:
        schema = _schema_mapping(value)
        if schema is not None:
            if schema.get("type") == "definitions":
                raw_definitions = _schema_list(schema.get("definitions"))
                if raw_definitions is not None:
                    for raw_definition in raw_definitions:
                        definition = _schema_mapping(raw_definition)
                        if definition is not None:
                            reference = definition.get("ref")
                            if isinstance(reference, str):
                                definitions[reference] = definition
            for child in schema.values():
                collect(child)
        else:
            items = _schema_list(value)
            if items is None:
                return
            for child in items:
                collect(child)

    collect(core_schema)
    root = core_schema
    while root.get("type") == "definitions":
        wrapped = root.get("schema")
        wrapped_schema = _schema_mapping(wrapped)
        if wrapped_schema is None:
            break
        root = wrapped_schema
    return root, definitions


def _normalize_core_schema_node(
    value: object,
    definitions: dict[str, dict[str, object]],
    resolving: set[str],
) -> object:
    schema = _schema_mapping(value)
    if schema is not None:
        if schema.get("type") == "definition-ref":
            reference = schema.get("schema_ref")
            if isinstance(reference, str) and reference in definitions:
                if reference in resolving:
                    return {"type": "recursive-reference"}
                return _normalize_core_schema_node(
                    definitions[reference], definitions, {*resolving, reference}
                )
        normalized: dict[str, object] = {}
        for key, child in schema.items():
            if key in {"definitions", "metadata", "ref"}:
                continue
            if (key == "cls" and isinstance(child, type)) or callable(child):
                normalized[key] = f"{child.__module__}.{child.__qualname__}"
            else:
                normalized[key] = _normalize_core_schema_node(child, definitions, resolving)
        return normalized
    items = _schema_list(value)
    if items is not None:
        return [_normalize_core_schema_node(child, definitions, resolving) for child in items]
    if isinstance(value, tuple):
        tuple_items = cast(tuple[object, ...], value)
        return tuple(
            _normalize_core_schema_node(child, definitions, resolving) for child in tuple_items
        )
    return value


def _schema_mapping(value: object) -> dict[str, object] | None:
    if not isinstance(value, dict):
        return None
    return cast(dict[str, object], value)


def _schema_list(value: object) -> list[object] | None:
    if not isinstance(value, list):
        return None
    return cast(list[object], value)


def _tagged_union_cases(annotation: Any, role: str) -> tuple[object, dict[str, type[BaseModel]]]:
    """Read a Pydantic tagged union's discriminator and one model per string tag.

    Decisions route on these tags, and a gather of different models decodes
    through them: an undiscriminated union would let Pydantic's smart-union
    matching choose a different model than the producer returned.
    """
    core_schema = cast(dict[str, object], TypeAdapter(annotation).core_schema)
    definitions: dict[str, type[BaseModel]] = {}
    if core_schema.get("type") == "definitions":
        raw_definitions = core_schema.get("definitions")
        if isinstance(raw_definitions, list):
            for raw_definition in cast(list[object], raw_definitions):
                if not isinstance(raw_definition, dict):
                    continue
                definition = cast(dict[str, object], raw_definition)
                reference = definition.get("ref")
                model = definition.get("cls")
                if (
                    isinstance(reference, str)
                    and isinstance(model, type)
                    and issubclass(model, BaseModel)
                ):
                    definitions[reference] = model
        raw_root_schema = core_schema.get("schema")
        if isinstance(raw_root_schema, dict):
            core_schema = cast(dict[str, object], raw_root_schema)

    if core_schema.get("type") != "tagged-union":
        raise TypeError(f"{role} must be a Pydantic discriminated union with string Literal tags")
    raw_choices = core_schema.get("choices")
    if not isinstance(raw_choices, dict) or not raw_choices:
        raise TypeError("Pydantic discriminated union must declare one or more cases")
    choices = cast(dict[object, object], raw_choices)

    cases: dict[str, type[BaseModel]] = {}
    tags_by_model: dict[type[BaseModel], list[str]] = {}
    for raw_tag, raw_choice in choices.items():
        if not isinstance(raw_tag, str):
            raise TypeError(f"{role} tags must be string Literal values")
        if not isinstance(raw_choice, dict):
            raise TypeError(f"{role} cases must be direct Pydantic model alternatives")
        choice = cast(dict[str, object], raw_choice)
        choice_type = choice.get("type")
        if choice_type == "model":
            model = choice.get("cls")
        elif choice_type == "definition-ref":
            reference = choice.get("schema_ref")
            model = definitions.get(reference) if isinstance(reference, str) else None
        else:
            raise TypeError(f"{role} cases must be direct Pydantic model alternatives")
        if not isinstance(model, type) or not issubclass(model, BaseModel):
            raise TypeError(f"{role} cases must be Pydantic models")
        cases[raw_tag] = model
        tags_by_model.setdefault(model, []).append(raw_tag)

    for model, tags in tags_by_model.items():
        if len(tags) > 1:
            ordered_tags = sorted(tags, key=_canonical_sort_key)
            rendered_tags = ", ".join(repr(tag) for tag in ordered_tags)
            raise TypeError(
                f"{role} case {model.__name__} declares multiple discriminator tags "
                f"{rendered_tags}; split it into one Pydantic model per tag"
            )
    return core_schema.get("discriminator"), cases


def _canonical_sort_key(value: str) -> bytes:
    return value.encode("utf-16-be")


def _symbol_module(function: Callable[..., Any], root: Path) -> str:
    source_file = inspect.getsourcefile(function)
    if source_file is None:
        raise TypeError("workflow step source file cannot be resolved")
    relative = Path(source_file).resolve().relative_to(root.resolve())
    if relative.suffix != ".py":
        raise TypeError("workflow steps must be defined in a Python source file")
    module = relative.with_suffix("").as_posix().replace("/", ".")
    if not module or any(not part.isidentifier() for part in module.split(".")):
        raise TypeError("workflow step module is not a stable Python module name")
    loaded = sys.modules.get(function.__module__)
    if not isinstance(loaded, ModuleType) or getattr(loaded, function.__name__, None) is None:
        raise TypeError("workflow step must remain exported from its module")
    return module


def _assert_canonical_json_schema(schema: JsonValue, role: str) -> None:
    """Reject Pydantic schemas that cannot describe canonical JSON v0 values.

    This follows the schema containers Pydantic emits, including local
    definitions and references. It is deliberately conservative for Pydantic
    output rather than a general JSON Schema satisfiability checker.
    """

    try:
        canonical_json(schema)
    except (TypeError, ValueError) as error:
        raise ValueError(
            f"{role} contains a schema value canonical-json-v0 cannot encode; "
            "use safe integers and strings instead of floats or unsafe integers."
        ) from error

    def pointer(path: str, token: str | int) -> str:
        escaped = str(token).replace("~", "~0").replace("/", "~1")
        return f"{path}/{escaped}"

    metadata = {
        "$comment",
        "default",
        "deprecated",
        "description",
        "examples",
        "readOnly",
        "title",
        "writeOnly",
    }
    mappings = {"$defs", "dependentSchemas", "patternProperties", "properties"}
    single_schemas = {
        "contains",
        "contentSchema",
        "else",
        "items",
        "propertyNames",
        "then",
        "unevaluatedItems",
        "unevaluatedProperties",
    }
    schema_arrays = {"allOf", "anyOf", "oneOf", "prefixItems"}

    def visit(value: JsonValue, path: str) -> None:
        if value is True:
            raise ValueError(
                f"{role} is unconstrained at {path}; canonical-json-v0 cannot represent "
                "an Any value. Use an explicit integer, string, object, or collection schema."
            )
        if not isinstance(value, dict):
            return
        non_metadata = set(value) - metadata
        if not non_metadata:
            raise ValueError(
                f"{role} is unconstrained at {path}; canonical-json-v0 cannot represent "
                "an Any value. Use an explicit integer, string, object, or collection schema."
            )
        type_value = value.get("type")
        if type_value == "number" or (isinstance(type_value, list) and "number" in type_value):
            raise ValueError(
                f"{role} uses JSON Schema type 'number' at {path}; "
                "canonical-json-v0 is integer-only. Use an integer field or "
                "model fractional values as strings. For JSON escape hatches, "
                "annotate values with massive.JsonValue."
            )
        for key in mappings:
            child = value.get(key)
            if isinstance(child, dict):
                for name, definition in child.items():
                    if isinstance(definition, (bool, dict)):
                        visit(definition, pointer(pointer(path, key), name))
        for key in single_schemas:
            child = value.get(key)
            if isinstance(child, (bool, dict)):
                visit(child, pointer(path, key))
        # `if` and `not` are polarity-sensitive: a nested number may be
        # conditionally constrained or forbidden, so neither is traversed here.
        additional_properties = value.get("additionalProperties")
        if additional_properties is True:
            raise ValueError(
                f"{role} permits unconstrained object values at "
                f"{pointer(path, 'additionalProperties')}; canonical-json-v0 cannot "
                "represent an Any value. Use dict[str, int] or dict[str, str]."
            )
        if isinstance(additional_properties, dict):
            visit(cast(JsonValue, additional_properties), pointer(path, "additionalProperties"))
        for key in schema_arrays:
            child = value.get(key)
            if isinstance(child, list):
                for index, item in enumerate(child):
                    visit(item, pointer(pointer(path, key), index))

    visit(schema, "#")
