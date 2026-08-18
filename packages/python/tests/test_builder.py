from __future__ import annotations

from collections.abc import Callable
from datetime import datetime
from enum import Enum
from pathlib import Path
from typing import Any, Literal
from uuid import UUID

import pytest
from pydantic import BaseModel

from massive import GraphBuilder, StepContext, container, execution, source_package
from massive.builder import _assert_canonical_json_schema


class Request(BaseModel):
    value: int


class Result(BaseModel):
    value: int


class DecimalRequest(BaseModel):
    value: float


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


class CanonicalShape(BaseModel):
    integer: int
    optional_integer: int | None
    integers: list[int]
    integer_mapping: dict[str, int]
    literal_integer: Literal[1]
    identifier: UUID
    occurred_at: datetime
    kind: EventKind


class AnyResult(BaseModel):
    value: Any


class AnyMappingResult(BaseModel):
    values: dict[str, Any]


class AnyListResult(BaseModel):
    values: list[Any]


def needs_services(context: StepContext[dict[str, str], Request]) -> Result:
    return Result(value=context.inputs.value)


def identity(context: StepContext[None, Request]) -> Result:
    return Result(value=context.inputs.value)


def decimal_identity(context: StepContext[None, DecimalRequest]) -> Result:
    return Result(value=int(context.inputs.value))


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
        (DecimalRequest, Result, decimal_identity, "workflow input schema"),
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


@pytest.mark.parametrize(
    ("step", "message"),
    [
        (float_enum_result, "step 'float-enum' output schema.*enum"),
    ],
)
def test_emit_rejects_step_schemas_with_float_constants_or_enums(
    step: Callable[..., Any], message: str
) -> None:
    graph = _integer_graph()
    identity_node = graph.add(graph.step()(identity))
    graph.add(graph.step()(step), id=step.__name__.removesuffix("_result").replace("_", "-"))
    graph.edge_from(graph.start).to(identity_node).to(graph.end)

    with pytest.raises(TypeError, match=message):
        _emit(graph)


@pytest.mark.parametrize(
    ("step", "step_id"),
    [
        (any_result, "any-result"),
        (any_mapping_result, "any-mapping-result"),
        (any_list_result, "any-list-result"),
    ],
)
def test_emit_rejects_an_unconstrained_pydantic_any_schema(
    step: Callable[..., Any], step_id: str
) -> None:
    graph = _integer_graph()
    identity_node = graph.add(graph.step()(identity))
    graph.add(graph.step()(step), id=step_id)
    graph.edge_from(graph.start).to(identity_node).to(graph.end)

    with pytest.raises(ValueError, match=f"step '{step_id}' output schema.*unconstrained"):
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
    with pytest.raises(TypeError, match="step 'float-constant' output schema.*const"):
        _assert_canonical_json_schema(
            {"const": 1.5}, "step 'float-constant' output schema"
        )


@pytest.mark.parametrize(
    ("schema", "message"),
    [
        ({"const": 1 << 53}, "safe-integer.*const"),
        ({"enum": [1, 1 << 53]}, "safe-integer.*enum"),
    ],
)
def test_schema_validation_rejects_unsafe_integer_constants_and_enums(
    schema: dict[str, Any], message: str
) -> None:
    with pytest.raises(ValueError, match=message):
        _assert_canonical_json_schema(schema, "step 'unsafe' output schema")


def test_schema_validation_escapes_json_pointer_tokens_in_diagnostics() -> None:
    with pytest.raises(ValueError, match=r"#/properties/name~1with~0tokens"):
        _assert_canonical_json_schema(
            {"properties": {"name/with~tokens": {"type": "number"}}},
            "step 'pointer' output schema",
        )


def _integer_graph() -> GraphBuilder[None, Request, Result]:
    return GraphBuilder(
        name="integer-only",
        input_type=Request,
        output_type=Result,
        defaults=_defaults(),
    )


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
