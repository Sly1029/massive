from __future__ import annotations

import asyncio
import hashlib
import importlib
import inspect
import json
import sys
from collections.abc import Awaitable, Callable, Generator, Mapping
from contextlib import contextmanager
from pathlib import Path
from typing import Protocol, cast

from jsonschema import Draft202012Validator
from pydantic import TypeAdapter

from .builder import StepDefinition
from .canonical import JsonValue, canonical_json, sha256_ref
from .context import InvocationContext, StepContext

_SOURCE_DIRECTORY_CONTENT_TYPE = "application/vnd.massive.source-directory+json"
_DESCRIPTOR_EXIT = 64
_SCHEMA_EXIT = 65
_STEP_EXIT = 66

type Descriptor = dict[str, object]
type StepFunction = Callable[[StepContext[None, object]], object | Awaitable[object]]


class SchemaValidator(Protocol):
    def validate(self, value: object) -> None: ...


class DescriptorError(Exception):
    pass


class SchemaError(Exception):
    pass


class StepError(Exception):
    pass


def run_descriptor_path(path: Path) -> int:
    try:
        descriptor = _load_descriptor(path)
        _execute(descriptor)
        return 0
    except DescriptorError as error:
        print(f"descriptor-resolution-failure: {error}", file=sys.stderr)
        return _DESCRIPTOR_EXIT
    except SchemaError as error:
        print(f"schema-validation-failure: {error}", file=sys.stderr)
        return _SCHEMA_EXIT
    except StepError as error:
        print(f"step-execution-failure: {error}", file=sys.stderr)
        return _STEP_EXIT


def _load_descriptor(path: Path) -> Descriptor:
    try:
        descriptor: object = json.loads(path.read_text())
    except (OSError, json.JSONDecodeError) as error:
        raise DescriptorError(f"cannot read descriptor {path}: {error}") from error
    if not isinstance(descriptor, dict):
        raise DescriptorError("descriptor must be a JSON object")
    typed_descriptor = cast(Descriptor, descriptor)
    _require(typed_descriptor, "kind", "StepInvocationDescriptor")
    _require(typed_descriptor, "schemaVersion", 0)
    _require(typed_descriptor, "encoding", "json-v0")
    _require_mapping(typed_descriptor, "symbol")
    _require_mapping(typed_descriptor, "sourcePackage")
    _require_mapping(typed_descriptor, "input")
    _require_mapping(typed_descriptor, "output")
    datastore = _require_mapping(typed_descriptor, "datastore")
    _require(datastore, "kind", "local")
    if not isinstance(datastore.get("path"), str) or not datastore["path"]:
        raise DescriptorError("local datastore requires a path")
    return typed_descriptor


def _execute(descriptor: Descriptor) -> None:
    symbol = _require_mapping(descriptor, "symbol")
    source_package = _require_mapping(descriptor, "sourcePackage")
    if symbol.get("language") != "python" or source_package.get("language") != "python":
        raise DescriptorError("Python runner requires Python symbol and source package")
    if symbol.get("packageId") != source_package.get("packageId"):
        raise DescriptorError("symbol package does not match source package")
    datastore = Path(_string(_require_mapping(descriptor, "datastore"), "path")).resolve()
    input_value, _input_schema = _read_input(descriptor, datastore)
    source_root = _source_root(source_package, datastore)
    function, input_adapter, output_adapter = _resolve_step(symbol, source_root)
    if input_adapter is not None:
        try:
            input_value = input_adapter.validate_python(input_value)
        except Exception as error:
            raise SchemaError(f"input does not satisfy the step input type: {error}") from error
    context = StepContext[None, object](
        inputs=input_value,
        deps=None,
        invocation=InvocationContext(
            run_id=_string(descriptor, "runId"),
            step_id=_string(descriptor, "nodeId"),
            idempotency_key=f"{_string(descriptor, 'runId')}:{_string(descriptor, 'nodeId')}",
        ),
    )
    try:
        output = function(context)
        if inspect.isawaitable(output):
            output = asyncio.run(_await_output(output))
    except Exception as error:
        raise StepError(str(error)) from error
    if output_adapter is not None:
        try:
            output = output_adapter.dump_python(output_adapter.validate_python(output), mode="json")
        except Exception as error:
            raise SchemaError(f"output does not satisfy the step output type: {error}") from error
    output_descriptor = _require_mapping(descriptor, "output")
    output_schema = _schema(datastore, _string(output_descriptor, "schema"))
    _validate(output_schema, output, "output")
    try:
        output_text = canonical_json(cast(JsonValue, output))
    except (TypeError, ValueError) as error:
        raise SchemaError(f"output is not canonical JSON: {error}") from error
    output_artifact = _require_mapping(output_descriptor, "artifact")
    _write(datastore, _string(output_artifact, "key"), output_text)


async def _await_output(value: Awaitable[object]) -> object:
    return await value


def _read_input(descriptor: Descriptor, datastore: Path) -> tuple[JsonValue, dict[str, object]]:
    input_descriptor = _require_mapping(descriptor, "input")
    artifact = _require_mapping(input_descriptor, "artifact")
    expected_hash = _string(artifact, "hash")
    text = _read(datastore, _string(artifact, "key"))
    if sha256_ref(text) != expected_hash:
        raise SchemaError("input artifact hash mismatch")
    try:
        value = cast(JsonValue, json.loads(text))
    except json.JSONDecodeError as error:
        raise SchemaError(f"input artifact is not JSON: {error}") from error
    if canonical_json(value) != text:
        raise SchemaError("input artifact is not canonical JSON")
    schema = _schema(datastore, _string(input_descriptor, "schema"))
    _validate(schema, value, "input")
    return value, schema


def _schema(datastore: Path, reference: str) -> dict[str, object]:
    if not reference.startswith("sha256:"):
        raise SchemaError("schema reference is not a SHA-256 reference")
    text = _read(datastore, f"blobs/sha256/{reference.removeprefix('sha256:')}")
    try:
        schema: object = json.loads(text)
    except json.JSONDecodeError as error:
        raise SchemaError(f"schema is not JSON: {error}") from error
    if not isinstance(schema, dict):
        raise SchemaError("schema must be an object")
    if sha256_ref(canonical_json(cast(JsonValue, schema))) != reference:
        raise SchemaError("schema hash mismatch")
    return cast(dict[str, object], schema)


def _validate(schema: Mapping[str, object], value: object, role: str) -> None:
    try:
        Draft202012Validator.check_schema(schema)
        validator = cast(SchemaValidator, Draft202012Validator(schema))
        validator.validate(value)
    except Exception as error:
        raise SchemaError(f"{role} does not satisfy its JSON Schema: {error}") from error


def _source_root(source_package: Descriptor, datastore: Path) -> Path:
    archive = _require_mapping(source_package, "sourceArchive")
    if archive.get("contentType") != _SOURCE_DIRECTORY_CONTENT_TYPE:
        raise DescriptorError("Python runner currently requires a local source-directory package")
    text = _read(datastore, _string(archive, "key"))
    if sha256_ref(text) != _string(archive, "hash"):
        raise DescriptorError("source archive hash mismatch")
    try:
        pointer: object = json.loads(text)
    except json.JSONDecodeError as error:
        raise DescriptorError(f"source pointer is not JSON: {error}") from error
    if not isinstance(pointer, dict):
        raise DescriptorError("source pointer must contain a sourceFetch path")
    root = Path(_string(cast(Descriptor, pointer), "sourceFetch")).resolve()
    if not root.is_dir():
        raise DescriptorError("source package root does not exist")
    return root


def _resolve_step(
    symbol: Descriptor, source_root: Path
) -> tuple[StepFunction, TypeAdapter[object] | None, TypeAdapter[object] | None]:
    module_name = _string(symbol, "module")
    if not module_name or any(not part.isidentifier() for part in module_name.split(".")):
        raise DescriptorError("Python module must be a dotted identifier")
    export = _string(symbol, "export")
    if not export.isidentifier():
        raise DescriptorError("Python export must be an identifier")
    with _source_import_path(source_root):
        try:
            module = importlib.import_module(module_name)
        except Exception as error:
            raise DescriptorError(f"cannot import {module_name}: {error}") from error
    exported = getattr(module, export, None)
    if isinstance(exported, StepDefinition):
        step = cast(StepDefinition[object, object, object], exported)
        return (
            cast(StepFunction, step.function),
            TypeAdapter[object](step.input_type),
            TypeAdapter[object](step.output_type),
        )
    if callable(exported):
        return cast(StepFunction, exported), None, None
    raise DescriptorError(f"export {export!r} is not a step function")


@contextmanager
def _source_import_path(root: Path) -> Generator[None, None, None]:
    root_text = str(root)
    sys.path.insert(0, root_text)
    try:
        yield
    finally:
        sys.path.remove(root_text)


def _read(root: Path, key: str) -> str:
    return _path(root, key).read_text()


def _write(root: Path, key: str, body: str) -> None:
    target = _path(root, key)
    target.parent.mkdir(parents=True, exist_ok=True)
    temporary = target.with_name(f".tmp-{target.name}")
    temporary.write_text(body)
    temporary.replace(target)
    metadata = (
        root / ".massive-datastore-metadata" / f"{hashlib.sha256(key.encode()).hexdigest()}.json"
    )
    metadata.parent.mkdir(parents=True, exist_ok=True)
    metadata.write_text('{"contentType":"application/json"}')


def _path(root: Path, key: str) -> Path:
    candidate = (root / key).resolve()
    if root not in candidate.parents:
        raise DescriptorError("datastore key escapes the local datastore root")
    return candidate


def _require(value: Mapping[str, object], key: str, expected: object) -> None:
    if value.get(key) != expected:
        raise DescriptorError(f"descriptor field {key!r} must equal {expected!r}")


def _require_mapping(value: Mapping[str, object], key: str) -> Descriptor:
    child = value.get(key)
    if not isinstance(child, dict):
        raise DescriptorError(f"descriptor field {key!r} must be an object")
    return cast(Descriptor, child)


def _string(value: Mapping[str, object], key: str) -> str:
    child = value.get(key)
    if not isinstance(child, str) or not child:
        raise DescriptorError(f"descriptor field {key!r} must be a non-empty string")
    return child


def main() -> int:
    if len(sys.argv) != 2:
        print("descriptor-resolution-failure: expected descriptor path", file=sys.stderr)
        return _DESCRIPTOR_EXIT
    return run_descriptor_path(Path(sys.argv[1]))


if __name__ == "__main__":
    raise SystemExit(main())
