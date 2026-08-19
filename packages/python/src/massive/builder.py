from __future__ import annotations

import inspect
import sys
from collections.abc import Awaitable, Callable
from dataclasses import dataclass
from enum import Enum
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

from pydantic import BaseModel, ConfigDict, TypeAdapter, model_validator

from .canonical import JsonValue, canonical_json, sha256_ref
from .context import DepsT, InputT, StepContext
from .contracts import ExecutionContract
from .identity import SAFE_PATH_SEGMENT, SafePathSegment
from .source_package import SourcePackage

OutputT = TypeVar("OutputT")
WorkflowInputT = TypeVar("WorkflowInputT")
WorkflowOutputT = TypeVar("WorkflowOutputT")
CaseT = TypeVar("CaseT", bound=BaseModel)
SelectT = TypeVar("SelectT")

_START = "__start"
_END = "__end"
# Graph IR versioning is independent from the outer WorkflowSpec transport
# schema so graph evolution remains an explicit compiler contract.
GRAPH_IR_VERSION = "0.1"
_DECISION_GRAPH_IR_VERSION = "0.2"


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


@dataclass(frozen=True, slots=True)
class StepDefinition(Generic[DepsT, InputT, OutputT]):
    function: Callable[[StepContext[DepsT, InputT]], OutputT | Awaitable[OutputT]]
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
class CaseHandle(NodeHandle[CaseT], Generic[CaseT]):
    decision_id: str
    tag: str


@dataclass(slots=True)
class _DecisionDefinition:
    id: str
    source: NodeHandle[Any]
    selector: str
    cases: dict[str, type[BaseModel]]
    claimed_cases: set[str]
    branch_roots: dict[str, str]


@dataclass(frozen=True, slots=True)
class _SelectDefinition:
    id: str
    decision_id: str
    output_type: Any
    inputs: dict[str, NodeHandle[Any]]


class DecisionHandle(Generic[OutputT]):
    def __init__(self, graph: GraphBuilder[Any, Any, Any], definition: _DecisionDefinition) -> None:
        self._graph = graph
        self._definition = definition

    def case(self, case_type: type[CaseT]) -> CaseHandle[CaseT]:
        return self._graph._claim_decision_case(  # pyright: ignore[reportPrivateUsage]
            self._definition.id, case_type
        )

    def select(
        self, output_type: type[SelectT], **inputs: NodeHandle[SelectT]
    ) -> NodeHandle[SelectT]:
        return self._graph._select_decision(  # pyright: ignore[reportPrivateUsage]
            self._definition.id, output_type, inputs
        )


@dataclass(frozen=True, slots=True)
class _StartHandle(Generic[WorkflowInputT]):
    output_type: Any


@dataclass(frozen=True, slots=True)
class _EndHandle(Generic[WorkflowOutputT]):
    input_type: Any


class EdgePath(Generic[OutputT]):
    def __init__(
        self,
        add_edge: Callable[[str, str, str | None], None],
        source: str,
        output_type: Any,
        case: str | None = None,
    ) -> None:
        self._add_edge = add_edge
        self._source = source
        self._output_type = output_type
        self._case = case

    @overload
    def to(self, target: NodeHandle[Any]) -> EdgePath[Any]: ...

    @overload
    def to(self, target: _EndHandle[Any]) -> None: ...

    def to(self, target: NodeHandle[Any] | _EndHandle[Any]) -> EdgePath[Any] | None:
        expected = target.input_type
        if self._output_type != expected:
            raise TypeError(f"edge from {self._source!r} has incompatible input type")
        target_id = target.node_id if isinstance(target, NodeHandle) else _END
        self._add_edge(self._source, target_id, self._case)
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
        self.name = _WorkflowIdentity(name=name).name
        self.input_type = input_type
        self.output_type = output_type
        self.deps_type = deps_type
        self.defaults = defaults
        self.start = _StartHandle[WorkflowInputT](output_type=input_type)
        self.end = _EndHandle[WorkflowOutputT](input_type=output_type)
        self._nodes: dict[str, tuple[StepDefinition[Any, Any, Any], NodeHandle[Any]]] = {}
        self._edges: set[tuple[str, str]] = set()
        self._conditional_edges: set[tuple[str, str, str]] = set()
        self._decisions: dict[str, _DecisionDefinition] = {}
        self._selects: dict[str, _SelectDefinition] = {}
        self._emitted = False

    def step(
        self, *, contract: ExecutionContract | None = None
    ) -> Callable[
        [Callable[[StepContext[DepsT, InputT]], OutputT | Awaitable[OutputT]]],
        StepDefinition[DepsT, InputT, OutputT],
    ]:
        def register(
            function: Callable[[StepContext[DepsT, InputT]], OutputT | Awaitable[OutputT]],
        ) -> StepDefinition[DepsT, InputT, OutputT]:
            if "<locals>" in function.__qualname__ or function.__name__ == "<lambda>":
                raise TypeError("workflow steps must be top-level named functions")
            hints = get_type_hints(function, include_extras=True)
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
        node_id = SAFE_PATH_SEGMENT.validate_python(id or item.function.__name__)
        if node_id in self._known_node_ids():
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
        if node_id != _START and node_id not in self._known_node_ids():
            raise ValueError("edge source belongs to a different graph")
        case = source.tag if isinstance(source, CaseHandle) else None
        return EdgePath(self._add_edge, node_id, source.output_type, case)

    def decision(self, source: NodeHandle[OutputT], *, on: str, id: str) -> DecisionHandle[OutputT]:
        if self._emitted:
            raise RuntimeError("graph has already been emitted")
        if source.node_id not in self._known_node_ids():
            raise ValueError("decision source belongs to a different graph")
        identity = _DecisionIdentity(id=id)
        if identity.id in self._known_node_ids():
            raise ValueError(f"duplicate or reserved decision id {identity.id!r}")
        cases = _decision_cases(source.output_type, on)
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
            node_id=decision_id,
            input_type=case_type,
            output_type=case_type,
            decision_id=decision_id,
            tag=tag,
        )

    def _select_decision(
        self,
        decision_id: str,
        output_type: type[SelectT],
        inputs: dict[str, NodeHandle[SelectT]],
    ) -> NodeHandle[SelectT]:
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
        for tag, source in inputs.items():
            if source.node_id not in self._known_node_ids():
                raise ValueError("decision select source belongs to a different graph")
            if source.output_type is not output_type:
                raise TypeError(
                    f"decision {decision_id!r} case {tag!r} output type does not match select output"
                )
            root = definition.branch_roots.get(tag)
            if root is not None and not self._is_reachable(root, source.node_id):
                raise ValueError(
                    f"decision {decision_id!r} case {tag!r} select source is not in that branch"
                )
        select_id = f"{decision_id}-select"
        if select_id in self._known_node_ids():
            raise ValueError(f"duplicate derived select id {select_id!r}")
        self._selects[select_id] = _SelectDefinition(
            id=select_id,
            decision_id=decision_id,
            output_type=output_type,
            inputs=cast(dict[str, NodeHandle[Any]], inputs),
        )
        for source in inputs.values():
            self._add_edge(source.node_id, select_id, None)
        return NodeHandle(node_id=select_id, input_type=output_type, output_type=output_type)

    def _add_edge(self, source: str, target: str, case: str | None = None) -> None:
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

    def _known_node_ids(self) -> set[str]:
        return {_START, _END, *self._nodes, *self._decisions, *self._selects}

    def _is_reachable(self, source: str, target: str) -> bool:
        pending = [source]
        seen: set[str] = set()
        adjacency: dict[str, list[str]] = {}
        for edge_source, edge_target in self._edges:
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
            nodes.append(
                {
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
                        SchemaPurpose.INPUT,
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
        ir_version = _DECISION_GRAPH_IR_VERSION if self._decisions else GRAPH_IR_VERSION
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
                    "irVersion": ir_version,
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
        return WorkflowSpec(value=emitted, spec_hash=spec_hash, graph_version=ir_version)


def _decision_cases(annotation: Any, selector: str) -> dict[str, type[BaseModel]]:
    """Read a Pydantic tagged-union's declared cases from its core schema."""
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
        raise TypeError(
            "decision input must be a Pydantic discriminated union with string Literal tags"
        )
    if core_schema.get("discriminator") != selector:
        raise TypeError(f"decision selector {selector!r} does not match the Pydantic discriminator")
    raw_choices = core_schema.get("choices")
    if not isinstance(raw_choices, dict) or not raw_choices:
        raise TypeError("Pydantic discriminated union must declare one or more cases")
    choices = cast(dict[object, object], raw_choices)

    cases: dict[str, type[BaseModel]] = {}
    tags_by_model: dict[type[BaseModel], list[str]] = {}
    for raw_tag, raw_choice in choices.items():
        if not isinstance(raw_tag, str):
            raise TypeError("decision tags must be string Literal values")
        if not isinstance(raw_choice, dict):
            raise TypeError("decision cases must be direct Pydantic model alternatives")
        choice = cast(dict[str, object], raw_choice)
        choice_type = choice.get("type")
        if choice_type == "model":
            model = choice.get("cls")
        elif choice_type == "definition-ref":
            reference = choice.get("schema_ref")
            model = definitions.get(reference) if isinstance(reference, str) else None
        else:
            raise TypeError("decision cases must be direct Pydantic model alternatives")
        if not isinstance(model, type) or not issubclass(model, BaseModel):
            raise TypeError("decision cases must be Pydantic models")
        cases[raw_tag] = model
        tags_by_model.setdefault(model, []).append(raw_tag)

    for model, tags in tags_by_model.items():
        if len(tags) > 1:
            ordered_tags = sorted(tags, key=_canonical_sort_key)
            rendered_tags = ", ".join(repr(tag) for tag in ordered_tags)
            raise TypeError(
                f"decision case {model.__name__} declares multiple discriminator tags "
                f"{rendered_tags}; split it into one Pydantic model per tag"
            )
    return cases


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
                "model fractional values as strings."
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
