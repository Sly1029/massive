from __future__ import annotations

from collections.abc import Callable
from datetime import datetime
from decimal import Decimal
from enum import Enum
from pathlib import Path
from typing import Any, Literal
from uuid import UUID

import pytest
from pydantic import BaseModel, ConfigDict, ValidationError

from massive import (
    DEFAULT_MAP_CONCURRENCY,
    MAX_MAP_CONCURRENCY,
    GraphBuilder,
    StepContext,
    container,
    execution,
    source_package,
)
from massive.builder import _assert_canonical_json_schema


class Request(BaseModel):
    value: int


class Result(BaseModel):
    value: int


class DecimalResult(BaseModel):
    value: Decimal


class DecimalInput(BaseModel):
    value: Decimal


class NestedDecimalInput(BaseModel):
    result: DecimalInput


class NestedDecimalResult(BaseModel):
    values: list[float]


class DecimalMeasurement(BaseModel):
    value: float


class ReferencedDecimalResult(BaseModel):
    measurement: DecimalMeasurement


class FloatEnumResult(BaseModel):
    value: Literal[1, 1.5]


class EventKind(Enum):
    CREATED = "created"


class NestedIntegerShape(BaseModel):
    value: int


class CanonicalShape(BaseModel):
    allowed: bool
    integer: int
    optional_integer: int | None
    integers: list[int]
    integer_mapping: dict[str, int]
    literal_integer: Literal[1]
    identifier: UUID
    occurred_at: datetime
    kind: EventKind
    nested: NestedIntegerShape


class AnyResult(BaseModel):
    value: Any


class AnyMappingResult(BaseModel):
    values: dict[str, Any]


class AnyListResult(BaseModel):
    values: list[Any]


class ExtraAllowedResult(BaseModel):
    model_config = ConfigDict(extra="allow")

    value: int


class SharedChild(BaseModel):
    value: int


class RecursiveItem(BaseModel):
    left: SharedChild
    right: SharedChild
    children: list[RecursiveItem]


def needs_services(context: StepContext[dict[str, str], Request]) -> Result:
    return Result(value=context.inputs.value)


def identity(context: StepContext[None, Request]) -> Result:
    return Result(value=context.inputs.value)


def decimal_result(context: StepContext[None, Request]) -> DecimalResult:
    return DecimalResult(value=Decimal(context.inputs.value) / Decimal(2))


def decimal_echo(context: StepContext[None, DecimalResult]) -> DecimalResult:
    return context.inputs


def nested_decimal_echo(context: StepContext[None, NestedDecimalInput]) -> NestedDecimalInput:
    return context.inputs


def float_input(context: StepContext[None, float]) -> Request:
    return Request(value=int(context.inputs))


def nested_decimal_result(context: StepContext[None, Request]) -> NestedDecimalResult:
    return NestedDecimalResult(values=[float(context.inputs.value)])


def referenced_decimal_result(context: StepContext[None, Request]) -> ReferencedDecimalResult:
    return ReferencedDecimalResult(measurement=DecimalMeasurement(value=context.inputs.value))


def float_enum_result(context: StepContext[None, Request]) -> FloatEnumResult:
    return FloatEnumResult(value=1.5)


def canonical_shape_identity(context: StepContext[None, CanonicalShape]) -> CanonicalShape:
    return context.inputs


def any_result(context: StepContext[None, Request]) -> AnyResult:
    return AnyResult(value=context.inputs.value)


def any_mapping_result(context: StepContext[None, Request]) -> AnyMappingResult:
    return AnyMappingResult(values={"value": context.inputs.value})


def any_list_result(context: StepContext[None, Request]) -> AnyListResult:
    return AnyListResult(values=[context.inputs.value])


def extra_allowed_result(context: StepContext[None, Request]) -> ExtraAllowedResult:
    return ExtraAllowedResult(value=context.inputs.value)


def load_requests(context: StepContext[None, Request]) -> list[Request]:
    return [context.inputs]


def increment_request(context: StepContext[None, Request]) -> Result:
    return Result(value=context.inputs.value + 1)


def load_any_requests(context: StepContext[None, Request]) -> list[Any]:
    return [context.inputs]


def result_identity(context: StepContext[None, Result]) -> Result:
    return context.inputs


def list_result_identity(context: StepContext[None, list[Result]]) -> list[Result]:
    return context.inputs


def load_results(context: StepContext[None, Request]) -> list[Result]:
    return [Result(value=context.inputs.value)]


def load_recursive_items(context: StepContext[None, Request]) -> list[RecursiveItem]:
    leaf = RecursiveItem(
        left=SharedChild(value=context.inputs.value),
        right=SharedChild(value=context.inputs.value),
        children=[],
    )
    return [leaf]


def recursive_item_identity(context: StepContext[None, RecursiveItem]) -> RecursiveItem:
    return context.inputs


def test_graph_without_dependencies_rejects_a_step_that_declares_them() -> None:
    graph = GraphBuilder(
        name="no-deps",
        input_type=Request,
        output_type=Result,
        defaults=execution(
            environment=container(
                "example.invalid/no-deps@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
                platform="linux/amd64",
            )
        ),
    )

    with pytest.raises(TypeError, match="does not permit dependencies"):
        graph.step()(needs_services)


def test_end_is_terminal() -> None:
    graph = GraphBuilder(
        name="terminal-end",
        input_type=Request,
        output_type=Result,
        defaults=execution(
            environment=container(
                "example.invalid/terminal@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
                platform="linux/amd64",
            )
        ),
    )

    node = graph.add(graph.step()(identity))

    assert graph.edge_from(graph.start).to(node).to(graph.end) is None


def test_map_emits_one_ordered_collection_node_with_its_registered_mapper() -> None:
    graph = GraphBuilder(
        name="map-requests",
        input_type=Request,
        output_type=list[Result],
        defaults=_defaults(),
    )
    requests = graph.add(graph.step()(load_requests))
    mapped = graph.map(requests, graph.step()(increment_request), id="increment-all", concurrency=3)
    graph.edge_from(graph.start).to(requests)
    graph.edge_from(mapped).to(graph.end)

    specification = _emit(graph)
    graph_ir = specification.value["graph"]
    nodes = {node["id"]: node for node in graph_ir["nodes"]}
    mapped_node = nodes["increment-all"]
    schemas = specification.value["schemas"]

    assert graph_ir["irVersion"] == "0.3"
    assert mapped_node["kind"] == "map"
    assert mapped_node["maxConcurrency"] == 3
    assert schemas[mapped_node["inputSchema"]] == {
        "$defs": {
            "Request": {
                "properties": {"value": {"title": "Value", "type": "integer"}},
                "required": ["value"],
                "title": "Request",
                "type": "object",
            }
        },
        "items": {"$ref": "#/$defs/Request"},
        "type": "array",
    }
    assert schemas[mapped_node["itemInputSchema"]] == {
        "properties": {"value": {"title": "Value", "type": "integer"}},
        "required": ["value"],
        "title": "Request",
        "type": "object",
    }
    assert schemas[mapped_node["itemOutputSchema"]] == {
        "properties": {"value": {"title": "Value", "type": "integer"}},
        "required": ["value"],
        "title": "Result",
        "type": "object",
    }
    assert schemas[mapped_node["outputSchema"]] == {
        "$defs": {
            "Result": {
                "properties": {"value": {"title": "Value", "type": "integer"}},
                "required": ["value"],
                "title": "Result",
                "type": "object",
            }
        },
        "items": {"$ref": "#/$defs/Result"},
        "type": "array",
    }
    assert {"from": "load_requests", "to": "increment-all"} in graph_ir["edges"]
    assert {"from": "increment-all", "to": "__end"} in graph_ir["edges"]
    assert "increment_request" not in nodes


def test_map_can_be_the_only_node_and_uses_the_shared_default_concurrency() -> None:
    graph = GraphBuilder(
        name="map-start",
        input_type=list[Request],
        output_type=list[Result],
        defaults=_defaults(),
    )
    mapped = graph.map(graph.start, graph.step()(increment_request), id="increment-items")
    graph.edge_from(mapped).to(graph.end)

    nodes = {node["id"]: node for node in _emit(graph).value["graph"]["nodes"]}

    assert nodes["increment-items"]["maxConcurrency"] == DEFAULT_MAP_CONCURRENCY


def test_map_allows_fan_out_to_multiple_list_consumers() -> None:
    graph = GraphBuilder(
        name="map-fan-out",
        input_type=list[Result],
        output_type=list[Result],
        defaults=_defaults(),
    )
    mapped = graph.map(graph.start, graph.step()(result_identity), id="copy-items")
    first = graph.add(graph.step()(list_result_identity), id="first")
    second = graph.add(graph.step()(list_result_identity), id="second")
    graph.edge_from(mapped).to(first).to(graph.end)
    graph.edge_from(mapped).to(second).to(graph.end)

    graph_ir = _emit(graph).value["graph"]

    assert {"from": "copy-items", "to": "first"} in graph_ir["edges"]
    assert {"from": "copy-items", "to": "second"} in graph_ir["edges"]


def test_map_accepts_recursive_and_reused_model_item_schemas() -> None:
    graph = GraphBuilder(
        name="recursive-map",
        input_type=Request,
        output_type=list[RecursiveItem],
        defaults=_defaults(),
    )
    source = graph.add(graph.step()(load_recursive_items))
    mapped = graph.map(source, graph.step()(recursive_item_identity), id="copy-recursive")
    graph.edge_from(graph.start).to(source)
    graph.edge_from(mapped).to(graph.end)

    assert _emit(graph).value["graph"]["irVersion"] == "0.3"


def test_map_uses_pydantic_validation_for_concurrency() -> None:
    graph = GraphBuilder(
        name="map-invalid-concurrency",
        input_type=list[Request],
        output_type=list[Result],
        defaults=_defaults(),
    )

    with pytest.raises(ValidationError, match="greater than or equal to 1"):
        graph.map(graph.start, graph.step()(increment_request), id="invalid-concurrency", concurrency=0)

    with pytest.raises(ValidationError, match="valid integer"):
        graph.map(graph.start, graph.step()(increment_request), id="boolean-concurrency", concurrency=True)

    graph.map(
        graph.start,
        graph.step()(increment_request),
        id="maximum-concurrency",
        concurrency=MAX_MAP_CONCURRENCY,
    )

    with pytest.raises(ValidationError, match="less than or equal to 4294967295"):
        graph.map(
            graph.start,
            graph.step()(increment_request),
            id="overflow-concurrency",
            concurrency=MAX_MAP_CONCURRENCY + 1,
        )


def test_map_rejects_duplicate_ids_and_cross_graph_sources() -> None:
    graph = GraphBuilder(
        name="map-identities",
        input_type=Request,
        output_type=list[Result],
        defaults=_defaults(),
    )
    source = graph.add(graph.step()(load_requests))
    graph.map(source, graph.step()(increment_request), id="duplicate")

    with pytest.raises(ValueError, match="duplicate or reserved map id 'duplicate'"):
        graph.map(source, graph.step()(increment_request), id="duplicate")

    other_graph = GraphBuilder(
        name="other-map-identities",
        input_type=Request,
        output_type=list[Result],
        defaults=_defaults(),
    )
    other_source = other_graph.add(other_graph.step()(load_requests))

    with pytest.raises(ValueError, match="map source 'load_requests' belongs to a different graph"):
        graph.map(other_source, graph.step()(increment_request), id="foreign")


def test_map_uses_the_mapper_symbol_and_contract_override() -> None:
    graph = GraphBuilder(
        name="map-contract",
        input_type=Request,
        output_type=list[Result],
        defaults=_defaults(),
    )
    source = graph.add(graph.step()(load_requests))
    mapper = graph.step(
        contract=execution(
            environment=container(
                "example.invalid/map-override@sha256:"
                "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
                platform="linux/amd64",
            )
        )
    )(increment_request)
    mapped = graph.map(source, mapper, id="overridden")
    graph.edge_from(graph.start).to(source)
    graph.edge_from(mapped).to(graph.end)

    specification = _emit(graph).value
    map_node = next(node for node in specification["graph"]["nodes"] if node["id"] == "overridden")
    contract = specification["contracts"][map_node["contractRef"]]
    environment = specification["environments"][contract["environmentRef"]]

    assert map_node["symbolRef"] == "python-tests:test_builder#increment_request"
    assert environment["image"].startswith("example.invalid/map-override@sha256:")


def test_map_emit_requires_a_direct_concrete_list_source() -> None:
    graph = GraphBuilder(
        name="map-non-list-source",
        input_type=Request,
        output_type=list[Result],
        defaults=_defaults(),
    )
    source = graph.add(graph.step()(identity))
    with pytest.raises(
        TypeError, match=r"map source 'identity' must be a direct concrete list\[T\]"
    ):
        graph.map(source, graph.step()(increment_request), id="not-a-list")


def test_map_emit_rejects_unconstrained_list_items() -> None:
    graph = GraphBuilder(
        name="map-any-source",
        input_type=Request,
        output_type=list[Result],
        defaults=_defaults(),
    )
    source = graph.add(graph.step()(load_any_requests))
    with pytest.raises(
        TypeError, match=r"map source 'load_any_requests' must be a direct concrete list\[T\]"
    ):
        graph.map(source, graph.step()(increment_request), id="any-items")


def test_map_emit_requires_the_source_item_type_to_match_the_mapper_input() -> None:
    graph = GraphBuilder(
        name="map-item-mismatch",
        input_type=Request,
        output_type=list[Result],
        defaults=_defaults(),
    )
    source = graph.add(graph.step()(load_requests))
    with pytest.raises(
        TypeError,
        match="map source 'load_requests' item type does not match mapper input type",
    ):
        graph.map(source, graph.step()(result_identity), id="wrong-item")


def test_map_allows_sequential_composition() -> None:
    graph = GraphBuilder(
        name="nested-map",
        input_type=Request,
        output_type=list[Result],
        defaults=_defaults(),
    )
    source = graph.add(graph.step()(load_requests))
    mapped = graph.map(source, graph.step()(increment_request), id="first-map")
    remapped = graph.map(mapped, graph.step()(result_identity), id="second-map")
    graph.edge_from(graph.start).to(source)
    graph.edge_from(remapped).to(graph.end)

    graph_ir = _emit(graph).value["graph"]

    assert {"from": "first-map", "to": "second-map"} in graph_ir["edges"]


def test_map_emit_requires_an_outgoing_edge() -> None:
    graph = GraphBuilder(
        name="map-multiple-consumers",
        input_type=Request,
        output_type=list[Result],
        defaults=_defaults(),
    )
    source = graph.add(graph.step()(load_requests))
    graph.map(source, graph.step()(increment_request), id="increment-all")
    alternate_result = graph.add(graph.step()(load_results))
    graph.edge_from(graph.start).to(source)
    graph.edge_from(graph.start).to(alternate_result).to(graph.end)

    with pytest.raises(ValueError, match="map 'increment-all' must have an outgoing edge"):
        _emit(graph)


def test_map_emit_requires_exactly_one_incoming_edge() -> None:
    graph = GraphBuilder(
        name="map-multiple-sources",
        input_type=Request,
        output_type=list[Result],
        defaults=_defaults(),
    )
    source = graph.add(graph.step()(load_requests), id="first-source")
    additional_source = graph.add(graph.step()(load_requests), id="second-source")
    mapped = graph.map(source, graph.step()(increment_request), id="increment-all")
    graph.edge_from(graph.start).to(source)
    graph.edge_from(graph.start).to(additional_source)
    graph.edge_from(additional_source).to(mapped)
    graph.edge_from(mapped).to(graph.end)

    with pytest.raises(ValueError, match="map 'increment-all' must have exactly one incoming edge"):
        _emit(graph)


@pytest.mark.parametrize(
    ("input_type", "output_type", "step", "message"),
    [
        (Request, NestedDecimalResult, nested_decimal_result, "workflow output schema"),
        (Request, ReferencedDecimalResult, referenced_decimal_result, "workflow output schema"),
    ],
)
def test_emit_rejects_schemas_that_admit_non_integer_json_numbers(
    input_type: type[BaseModel],
    output_type: type[BaseModel],
    step: Callable[..., Any],
    message: str,
) -> None:
    graph = GraphBuilder(
        name="integer-only",
        input_type=input_type,
        output_type=output_type,
        defaults=execution(
            environment=container(
                "example.invalid/integer-only@sha256:"
                "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
                platform="linux/amd64",
            )
        ),
    )
    node = graph.add(graph.step()(step))
    graph.edge_from(graph.start).to(node).to(graph.end)

    with pytest.raises(ValueError, match=message + ".*integer-only"):
        graph.emit(
            source=source_package(
                root=Path(__file__).parent,
                include=[Path(__file__).name],
                package_id="python-tests",
            )
        )


def test_emit_uses_validation_schemas_for_inputs_and_serialization_schemas_for_outputs() -> None:
    graph = GraphBuilder(
        name="decimal-transport",
        input_type=Request,
        output_type=DecimalResult,
        defaults=_defaults(),
    )
    node = graph.add(graph.step()(decimal_result))
    graph.edge_from(graph.start).to(node).to(graph.end)

    specification = _emit(graph)
    workflow = specification.value["workflow"]
    schemas = specification.value["schemas"]
    input_schema = schemas[workflow["inputSchema"]]
    output_schema = schemas[workflow["outputSchema"]]

    assert input_schema == {
        "properties": {"value": {"title": "Value", "type": "integer"}},
        "required": ["value"],
        "title": "Request",
        "type": "object",
    }
    assert output_schema == {
        "properties": {
            "value": {
                "pattern": "^(?!^[-+.]*$)[+-]?0*\\d*\\.?\\d*$",
                "title": "Value",
                "type": "string",
            }
        },
        "required": ["value"],
        "title": "DecimalResult",
        "type": "object",
    }


def test_emit_accepts_decimal_string_transport_between_steps() -> None:
    graph = GraphBuilder(
        name="decimal-pipeline",
        input_type=Request,
        output_type=DecimalResult,
        defaults=_defaults(),
    )
    first = graph.add(graph.step()(decimal_result))
    second = graph.add(graph.step()(decimal_echo))
    graph.edge_from(graph.start).to(first).to(second).to(graph.end)

    specification = _emit(graph)
    schemas = specification.value["schemas"]
    step = next(node for node in specification.value["graph"]["nodes"] if node["id"] == "decimal_echo")

    assert schemas[step["inputSchema"]]["properties"]["value"]["anyOf"] == [
        {"type": "number"},
        {"pattern": "^(?!^[-+.]*$)[+-]?0*\\d*\\.?\\d*$", "type": "string"},
    ]
    assert schemas[step["outputSchema"]]["properties"]["value"] == {
        "pattern": "^(?!^[-+.]*$)[+-]?0*\\d*\\.?\\d*$",
        "title": "Value",
        "type": "string",
    }


def test_emit_accepts_nested_decimal_validation_inputs_by_their_serialized_shape() -> None:
    graph = GraphBuilder(
        name="nested-decimal-input",
        input_type=NestedDecimalInput,
        output_type=NestedDecimalInput,
        defaults=_defaults(),
    )
    node = graph.add(graph.step()(nested_decimal_echo))
    graph.edge_from(graph.start).to(node).to(graph.end)

    specification = _emit(graph)
    schemas = specification.value["schemas"]
    workflow = specification.value["workflow"]

    assert schemas[workflow["inputSchema"]]["$defs"]["DecimalInput"]["properties"]["value"][
        "anyOf"
    ][1]["type"] == "string"
    assert schemas[workflow["outputSchema"]]["$defs"]["DecimalInput"]["properties"]["value"][
        "type"
    ] == "string"


def test_emit_rejects_bare_float_workflow_and_registered_step_inputs() -> None:
    workflow_float = GraphBuilder(
        name="float-workflow-input",
        input_type=float,
        output_type=Request,
        defaults=_defaults(),
    )
    workflow_float_node = workflow_float.add(workflow_float.step()(float_input))
    workflow_float.edge_from(workflow_float.start).to(workflow_float_node).to(workflow_float.end)

    with pytest.raises(ValueError, match="workflow input schema.*integer-only"):
        _emit(workflow_float)

    step_float = GraphBuilder(
        name="float-step-input",
        input_type=Request,
        output_type=Request,
        defaults=_defaults(),
    )
    # Keep the float step disconnected so a safe workflow input cannot mask its
    # role-specific diagnostic. Emission validates every registered step; the
    # compiler separately owns reachability validation.
    step_float.add(step_float.step()(float_input))
    assert step_float.edge_from(step_float.start).to(step_float.end) is None

    with pytest.raises(ValueError, match="step 'float_input' input schema.*integer-only"):
        _emit(step_float)


@pytest.mark.parametrize("identifier", ["a/b", ".", "..", "x" * 129])
def test_graph_rejects_workflow_names_that_cannot_be_used_as_safe_wire_segments(
    identifier: str,
) -> None:
    with pytest.raises(ValidationError):
        GraphBuilder(
            name=identifier,
            input_type=Request,
            output_type=Result,
            defaults=_defaults(),
        )


@pytest.mark.parametrize("identifier", ["a/b", ".", "..", "x" * 129])
def test_graph_rejects_node_ids_that_cannot_be_used_as_safe_wire_segments(identifier: str) -> None:
    graph = GraphBuilder(
        name="safe-id-test",
        input_type=Request,
        output_type=Result,
        defaults=_defaults(),
    )

    with pytest.raises(ValidationError):
        graph.add(graph.step()(identity), id=identifier)


@pytest.mark.parametrize("identifier", ["_step", ".hidden"])
def test_graph_accepts_safe_wire_segment_identifiers(identifier: str) -> None:
    graph = GraphBuilder(
        name=identifier,
        input_type=Request,
        output_type=Result,
        defaults=_defaults(),
    )

    node = graph.add(graph.step()(identity), id=identifier)

    assert node.node_id == identifier


@pytest.mark.parametrize(
    "step",
    [
        float_enum_result,
    ],
)
def test_emit_rejects_step_schemas_with_float_constants_or_enums(
    step: Callable[..., Any],
) -> None:
    graph = GraphBuilder(
        name="float-enum",
        input_type=Request,
        output_type=FloatEnumResult,
        defaults=_defaults(),
    )
    node = graph.add(graph.step()(step), id=step.__name__.removesuffix("_result").replace("_", "-"))
    graph.edge_from(graph.start).to(node).to(graph.end)

    with pytest.raises(ValueError, match="workflow output schema.*canonical-json-v0"):
        _emit(graph)


@pytest.mark.parametrize(
    ("step", "step_id", "output_type"),
    [
        (any_result, "any-result", AnyResult),
        (any_mapping_result, "any-mapping-result", AnyMappingResult),
        (any_list_result, "any-list-result", AnyListResult),
        (extra_allowed_result, "extra-allowed-result", ExtraAllowedResult),
    ],
)
def test_emit_rejects_an_unconstrained_pydantic_any_schema(
    step: Callable[..., Any], step_id: str, output_type: type[BaseModel]
) -> None:
    graph = GraphBuilder(
        name=step_id,
        input_type=Request,
        output_type=output_type,
        defaults=_defaults(),
    )
    node = graph.add(graph.step()(step), id=step_id)
    graph.edge_from(graph.start).to(node).to(graph.end)

    with pytest.raises(ValueError, match="workflow output schema.*unconstrained"):
        _emit(graph)


def test_emit_accepts_supported_integer_and_string_json_shapes() -> None:
    graph = GraphBuilder(
        name="canonical-shape",
        input_type=CanonicalShape,
        output_type=CanonicalShape,
        defaults=_defaults(),
    )
    node = graph.add(graph.step()(canonical_shape_identity))
    graph.edge_from(graph.start).to(node).to(graph.end)

    specification = _emit(graph)

    assert specification.spec_hash.startswith("sha256:")


def test_schema_validation_rejects_a_float_constant_with_step_role_diagnostics() -> None:
    with pytest.raises(ValueError, match="step 'float-constant' output schema.*canonical-json-v0"):
        _assert_canonical_json_schema(
            {"const": 1.5}, "step 'float-constant' output schema"
        )


@pytest.mark.parametrize(
    "schema",
    [
        {"const": 1 << 53},
        {"enum": [1, 1 << 53]},
    ],
)
def test_schema_validation_rejects_unsafe_integer_constants_and_enums(
    schema: dict[str, Any],
) -> None:
    with pytest.raises(ValueError, match="step 'unsafe' output schema.*canonical-json-v0"):
        _assert_canonical_json_schema(schema, "step 'unsafe' output schema")


def test_schema_validation_escapes_json_pointer_tokens_in_diagnostics() -> None:
    with pytest.raises(ValueError, match=r"#/properties/name~1with~0tokens"):
        _assert_canonical_json_schema(
            {"properties": {"name/with~tokens": {"type": "number"}}},
            "step 'pointer' output schema",
        )


@pytest.mark.parametrize(
    "schema",
    [
        {"minimum": 1.5, "type": "integer"},
        {"description": "integer", "examples": [1 << 53], "type": "integer"},
    ],
)
def test_schema_validation_rejects_noncanonical_metadata_and_bounds(schema: dict[str, Any]) -> None:
    with pytest.raises(ValueError, match="step 'metadata' output schema.*canonical-json-v0"):
        _assert_canonical_json_schema(schema, "step 'metadata' output schema")


@pytest.mark.parametrize(
    "schema",
    [
        {"properties": {"value": True}, "type": "object"},
        {"items": True, "type": "array"},
    ],
)
def test_schema_validation_rejects_true_boolean_child_schemas(schema: dict[str, Any]) -> None:
    with pytest.raises(ValueError, match="step 'any-child' output schema.*unconstrained"):
        _assert_canonical_json_schema(schema, "step 'any-child' output schema")


@pytest.mark.parametrize(
    "schema",
    [
        {"items": False, "type": "array"},
        {"if": {"type": "number"}, "type": "object"},
        {"not": {"type": "number"}, "type": "object"},
    ],
)
def test_schema_validation_keeps_false_and_polarity_sensitive_children_safe(
    schema: dict[str, Any]
) -> None:
    _assert_canonical_json_schema(schema, "step 'safe' output schema")


def _defaults():
    return execution(
        environment=container(
            "example.invalid/integer-only@sha256:"
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
