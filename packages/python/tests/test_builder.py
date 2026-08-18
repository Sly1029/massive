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

from massive import GraphBuilder, StepContext, container, execution, source_package
from massive.builder import SchemaPurpose, _assert_canonical_json_schema


class Request(BaseModel):
    value: int


class Result(BaseModel):
    value: int


class DecimalResult(BaseModel):
    value: Decimal


class DecimalInput(BaseModel):
    value: Decimal


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


def needs_services(context: StepContext[dict[str, str], Request]) -> Result:
    return Result(value=context.inputs.value)


def identity(context: StepContext[None, Request]) -> Result:
    return Result(value=context.inputs.value)


def decimal_result(context: StepContext[None, Request]) -> DecimalResult:
    return DecimalResult(value=Decimal(context.inputs.value) / Decimal(2))


def decimal_echo(context: StepContext[None, DecimalResult]) -> DecimalResult:
    return context.inputs


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
            {"const": 1.5}, "step 'float-constant' output schema", SchemaPurpose.OUTPUT
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
        _assert_canonical_json_schema(schema, "step 'unsafe' output schema", SchemaPurpose.OUTPUT)


def test_schema_validation_escapes_json_pointer_tokens_in_diagnostics() -> None:
    with pytest.raises(ValueError, match=r"#/properties/name~1with~0tokens"):
        _assert_canonical_json_schema(
            {"properties": {"name/with~tokens": {"type": "number"}}},
            "step 'pointer' output schema",
            SchemaPurpose.OUTPUT,
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
        _assert_canonical_json_schema(schema, "step 'metadata' output schema", SchemaPurpose.OUTPUT)


@pytest.mark.parametrize(
    "schema",
    [
        {"properties": {"value": True}, "type": "object"},
        {"items": True, "type": "array"},
    ],
)
def test_schema_validation_rejects_true_boolean_child_schemas(schema: dict[str, Any]) -> None:
    with pytest.raises(ValueError, match="step 'any-child' output schema.*unconstrained"):
        _assert_canonical_json_schema(schema, "step 'any-child' output schema", SchemaPurpose.OUTPUT)


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
    _assert_canonical_json_schema(schema, "step 'safe' output schema", SchemaPurpose.OUTPUT)


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
