from __future__ import annotations

import inspect
import sys
from collections.abc import Callable
from dataclasses import dataclass
from pathlib import Path
from types import ModuleType
from typing import (
    Any,
    Generic,
    TypeVar,
    cast,
    get_args,
    get_origin,
    get_type_hints,
    overload,
)

from pydantic import TypeAdapter

from .canonical import JsonValue, canonical_json, sha256_ref
from .context import DepsT, InputT, StepContext
from .contracts import ExecutionContract
from .source_package import SourcePackage

OutputT = TypeVar("OutputT")
WorkflowInputT = TypeVar("WorkflowInputT")
WorkflowOutputT = TypeVar("WorkflowOutputT")

_START = "__start"
_END = "__end"
# Graph IR versioning is independent from the outer WorkflowSpec transport
# schema so graph evolution remains an explicit compiler contract.
GRAPH_IR_VERSION = "0.1"


@dataclass(frozen=True, slots=True)
class StepDefinition(Generic[DepsT, InputT, OutputT]):
    function: Callable[[StepContext[DepsT, InputT]], OutputT]
    input_type: Any
    output_type: Any
    deps_type: Any
    contract: ExecutionContract | None


@dataclass(frozen=True, slots=True)
class NodeHandle(Generic[OutputT]):
    node_id: str
    input_type: Any
    output_type: Any


@dataclass(frozen=True, slots=True)
class _StartHandle(Generic[WorkflowInputT]):
    output_type: Any


@dataclass(frozen=True, slots=True)
class _EndHandle(Generic[WorkflowOutputT]):
    input_type: Any


class EdgePath(Generic[OutputT]):
    def __init__(self, add_edge: Callable[[str, str], None], source: str, output_type: Any) -> None:
        self._add_edge = add_edge
        self._source = source
        self._output_type = output_type

    @overload
    def to(self, target: NodeHandle[Any]) -> EdgePath[Any]: ...

    @overload
    def to(self, target: _EndHandle[Any]) -> None: ...

    def to(self, target: NodeHandle[Any] | _EndHandle[Any]) -> EdgePath[Any] | None:
        expected = target.input_type
        if self._output_type != expected:
            raise TypeError(f"edge from {self._source!r} has incompatible input type")
        target_id = target.node_id if isinstance(target, NodeHandle) else _END
        self._add_edge(self._source, target_id)
        if isinstance(target, NodeHandle):
            return EdgePath(self._add_edge, target_id, target.output_type)
        return None


@dataclass(frozen=True, slots=True)
class WorkflowSpec:
    value: dict[str, JsonValue]
    spec_hash: str
    graph_version: str = GRAPH_IR_VERSION

    def to_json(self) -> str:
        return canonical_json(self.value)


class GraphBuilder(Generic[DepsT, WorkflowInputT, WorkflowOutputT]):
    def __init__(
        self,
        *,
        name: str,
        input_type: type[WorkflowInputT],
        output_type: type[WorkflowOutputT],
        defaults: ExecutionContract,
        deps_type: type[DepsT] | None = None,
    ) -> None:
        if not name:
            raise ValueError("workflow name must not be empty")
        self.name = name
        self.input_type = input_type
        self.output_type = output_type
        self.deps_type = deps_type
        self.defaults = defaults
        self.start = _StartHandle[WorkflowInputT](output_type=input_type)
        self.end = _EndHandle[WorkflowOutputT](input_type=output_type)
        self._nodes: dict[str, tuple[StepDefinition[Any, Any, Any], NodeHandle[Any]]] = {}
        self._edges: set[tuple[str, str]] = set()
        self._emitted = False

    def step(
        self, *, contract: ExecutionContract | None = None
    ) -> Callable[
        [Callable[[StepContext[DepsT, InputT]], OutputT]], StepDefinition[DepsT, InputT, OutputT]
    ]:
        def register(
            function: Callable[[StepContext[DepsT, InputT]], OutputT],
        ) -> StepDefinition[DepsT, InputT, OutputT]:
            if "<locals>" in function.__qualname__ or function.__name__ == "<lambda>":
                raise TypeError("workflow steps must be top-level named functions")
            hints = get_type_hints(function)
            parameters = list(inspect.signature(function).parameters)
            if len(parameters) != 1 or parameters[0] not in hints:
                raise TypeError("a workflow step requires one annotated StepContext parameter")
            context_type = hints[parameters[0]]
            if get_origin(context_type) is not StepContext:
                raise TypeError("a workflow step parameter must be StepContext[Deps, Input]")
            declared_deps, input_type = get_args(context_type)
            if hints.get("return", inspect.Signature.empty) is inspect.Signature.empty:
                raise TypeError("a workflow step requires a return annotation")
            if self.deps_type is None and declared_deps is not type(None):
                raise TypeError("this graph does not permit dependencies")
            if self.deps_type is not None and declared_deps != self.deps_type:
                raise TypeError("step dependency type differs from the graph dependency type")
            return StepDefinition(
                function=function,
                input_type=input_type,
                output_type=hints["return"],
                deps_type=declared_deps,
                contract=contract,
            )

        return register

    @overload
    def add(
        self, item: StepDefinition[Any, Any, OutputT], *, id: str | None = None
    ) -> NodeHandle[OutputT]: ...

    @overload
    def add(self, item: EdgePath[Any], *, id: None = None) -> None: ...

    def add(
        self, item: StepDefinition[Any, Any, OutputT] | EdgePath[Any], *, id: str | None = None
    ) -> NodeHandle[OutputT] | None:
        if isinstance(item, EdgePath):
            if id is not None:
                raise TypeError("an edge path does not accept an id")
            return None
        if self._emitted:
            raise RuntimeError("graph has already been emitted")
        node_id = id or item.function.__name__
        if node_id in {_START, _END} or node_id in self._nodes:
            raise ValueError(f"duplicate or reserved step id {node_id!r}")
        handle = NodeHandle[OutputT](
            node_id=node_id,
            input_type=item.input_type,
            output_type=item.output_type,
        )
        self._nodes[node_id] = (item, handle)
        return handle

    @overload
    def edge_from(self, source: _StartHandle[WorkflowInputT]) -> EdgePath[WorkflowInputT]: ...

    @overload
    def edge_from(self, source: NodeHandle[OutputT]) -> EdgePath[OutputT]: ...

    def edge_from(self, source: _StartHandle[Any] | NodeHandle[Any]) -> EdgePath[Any]:
        node_id = _START if isinstance(source, _StartHandle) else source.node_id
        if node_id != _START and node_id not in self._nodes:
            raise ValueError("edge source belongs to a different graph")
        return EdgePath(self._add_edge, node_id, source.output_type)

    def _add_edge(self, source: str, target: str) -> None:
        if source not in {_START, *self._nodes}:
            raise ValueError(f"unknown edge source {source!r}")
        if target not in {_END, *self._nodes}:
            raise ValueError(f"unknown edge target {target!r}")
        self._edges.add((source, target))

    def emit(self, *, source: SourcePackage) -> WorkflowSpec:
        if self._emitted:
            raise RuntimeError("graph has already been emitted")
        if self.deps_type is not None:
            raise ValueError("dependency providers are not part of the v0 invocation protocol")
        if not self._nodes:
            raise ValueError("workflow must contain at least one step")
        if not any(source_id == _START for source_id, _ in self._edges):
            raise ValueError("workflow start has no edge")
        if not any(target_id == _END for _, target_id in self._edges):
            raise ValueError("workflow end has no edge")

        source_files, package_hash = source.manifest()
        schema_table: dict[str, JsonValue] = {}

        def schema_ref(annotation: Any) -> str:
            schema = cast(JsonValue, TypeAdapter(annotation).json_schema(mode="validation"))
            reference = sha256_ref(canonical_json(schema))
            schema_table[reference] = schema
            return reference

        input_schema = schema_ref(self.input_type)
        output_schema = schema_ref(self.output_type)
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
            nodes.append(
                {
                    "id": node_id,
                    "kind": "step",
                    "inputSchema": schema_ref(step.input_type),
                    "outputSchema": schema_ref(step.output_type),
                    "symbolRef": symbol_ref,
                    "contractRef": contract_ref(step.contract or self.defaults),
                }
            )
        nodes.append({"id": _END, "kind": "end"})
        value = cast(
            JsonValue,
            {
                "kind": "WorkflowSpec",
                "schemaVersion": 0,
                "encoding": "json-v0",
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
                    "edges": [{"from": edge[0], "to": edge[1]} for edge in sorted(self._edges)],
                },
                "schemas": schema_table,
                "symbols": symbols,
                "sourcePackages": {
                    source.package_id: {
                        "packageId": source.package_id,
                        "language": "python",
                        "packageHash": package_hash,
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
        return WorkflowSpec(value=emitted, spec_hash=spec_hash)


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
