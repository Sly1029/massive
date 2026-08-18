from __future__ import annotations

import asyncio
import hashlib
import importlib
import importlib.resources
import inspect
import json
import re
import sys
import tarfile
from collections.abc import Awaitable, Callable, Generator, Mapping
from contextlib import contextmanager
from functools import cache
from io import BytesIO
from pathlib import Path
from tempfile import TemporaryDirectory
from typing import Literal, NotRequired, Protocol, TypedDict, cast

from jsonschema import Draft202012Validator
from jsonschema.exceptions import SchemaError as JsonSchemaError
from jsonschema.exceptions import ValidationError as JsonSchemaValidationError
from pydantic import TypeAdapter
from pydantic import ValidationError as PydanticValidationError
from pydantic_core import PydanticSerializationError
from referencing.exceptions import Unresolvable

from .artifact import ArtifactError, ArtifactRuntime, Destination, Producer
from .builder import StepDefinition
from .canonical import (
    CanonicalJsonError,
    JsonValue,
    canonical_json,
    parse_canonical_json,
    sha256_ref,
)
from .context import InvocationContext, StepContext
from .datastore import (
    Datastore,
    DatastoreDescriptor,
    DatastoreNotFoundError,
    LocalDatastore,
    S3Datastore,
)
from .identity import SHA256_REFERENCE

_SOURCE_ARCHIVE_CONTENT_TYPE = "application/vnd.massive.source-tar"
_MAX_SOURCE_FILES = 1024
_MAX_SOURCE_BYTES = 50 * 1024 * 1024
_DESCRIPTOR_EXIT = 64
_SCHEMA_EXIT = 65
_STEP_EXIT = 66

type StepFunction = Callable[[StepContext[None, object]], object | Awaitable[object]]


class ArtifactRef(TypedDict):
    key: str
    hash: str
    contentType: str


class DataArtifactRef(TypedDict):
    artifact: ArtifactRef
    schema: str


class DataArtifactManifestDestination(TypedDict):
    manifestKey: str
    schema: str


class SymbolDescriptor(TypedDict):
    packageId: str
    language: Literal["typescript", "python"]
    module: str
    export: str


class SourcePackageDescriptor(TypedDict):
    packageId: str
    language: Literal["typescript", "python"]
    packageHash: str
    sourceArchive: ArtifactRef
    manifest: NotRequired[ArtifactRef]


class StepInvocationDescriptor(TypedDict):
    kind: Literal["StepInvocationDescriptor"]
    schemaVersion: Literal[1]
    encoding: Literal["json-v1"]
    planHash: str
    projectKey: str
    runId: str
    nodeId: str
    attempt: int
    symbol: SymbolDescriptor
    sourcePackage: SourcePackageDescriptor
    environmentRef: str
    input: DataArtifactRef
    output: DataArtifactManifestDestination
    datastore: DatastoreDescriptor


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


@cache
def _descriptor_validator() -> SchemaValidator:
    packaged_schema = importlib.resources.files("massive").joinpath(
        "schemas", "step-invocation-descriptor.schema.json"
    )
    if packaged_schema.is_file():
        schema_text = packaged_schema.read_text(encoding="utf-8")
    else:
        # Editable installs use the repository's canonical schema directly.
        schema_text = (
            Path(__file__).resolve().parents[4]
            / "conformance/schema/step-invocation-descriptor.schema.json"
        ).read_text()
    schema = cast(dict[str, object], json.loads(schema_text))
    Draft202012Validator.check_schema(schema)
    return cast(SchemaValidator, Draft202012Validator(schema))


def _load_descriptor(path: Path) -> StepInvocationDescriptor:
    try:
        descriptor: object = json.loads(path.read_text())
    except (OSError, json.JSONDecodeError) as error:
        raise DescriptorError(f"cannot read descriptor {path}: {error}") from error
    try:
        _descriptor_validator().validate(descriptor)
    except JsonSchemaValidationError as error:
        raise DescriptorError(f"descriptor does not satisfy its JSON Schema: {error}") from error
    return cast(StepInvocationDescriptor, descriptor)


def _execute(descriptor: StepInvocationDescriptor) -> None:
    symbol = descriptor["symbol"]
    source_package = descriptor["sourcePackage"]
    if symbol["language"] != "python" or source_package["language"] != "python":
        raise DescriptorError("Python runner requires Python symbol and source package")
    if symbol["packageId"] != source_package["packageId"]:
        raise DescriptorError("symbol package does not match source package")
    datastore = _datastore(descriptor["datastore"])
    input_value, _input_schema = _read_input(descriptor, datastore)
    with _source_root(source_package, datastore) as source_root:
        function, input_adapter, output_adapter = _resolve_step(symbol, source_root)
        _execute_source(descriptor, datastore, function, input_adapter, output_adapter, input_value)


def _execute_source(
    descriptor: StepInvocationDescriptor,
    datastore: Datastore,
    function: StepFunction,
    input_adapter: TypeAdapter[object] | None,
    output_adapter: TypeAdapter[object] | None,
    input_value: object,
) -> None:
    if input_adapter is not None:
        try:
            input_value = input_adapter.validate_python(input_value)
        except PydanticValidationError as error:
            raise SchemaError(f"input does not satisfy the step input type: {error}") from error
    context = StepContext[None, object](
        inputs=input_value,
        deps=None,
        invocation=InvocationContext(
            run_id=descriptor["runId"],
            step_id=descriptor["nodeId"],
            idempotency_key=f"{descriptor['runId']}:{descriptor['nodeId']}",
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
            validated_output = output_adapter.validate_python(output)
        except PydanticValidationError as error:
            raise SchemaError(f"output does not satisfy the step output type: {error}") from error
        try:
            output = output_adapter.dump_python(validated_output, mode="json")
        except PydanticSerializationError as error:
            raise SchemaError(f"output cannot be serialized as JSON: {error}") from error
    try:
        output_text = canonical_json(cast(JsonValue, output))
    except (TypeError, ValueError) as error:
        raise SchemaError(f"output is not canonical JSON: {error}") from error
    output_descriptor = descriptor["output"]
    try:
        ArtifactRuntime(datastore).publish_json(
            Destination(
                manifest_key=output_descriptor["manifestKey"],
                schema_ref=output_descriptor["schema"],
            ),
            Producer(
                project_key=descriptor["projectKey"],
                plan_hash=descriptor["planHash"],
                run_id=descriptor["runId"],
                node_id=descriptor["nodeId"],
                attempt=descriptor["attempt"],
            ),
            output_text.encode(),
        )
    except (ArtifactError, PydanticValidationError) as error:
        raise SchemaError(f"output artifact publication failed: {error}") from error


async def _await_output(value: Awaitable[object]) -> object:
    return await value


def _read_input(
    descriptor: StepInvocationDescriptor, datastore: Datastore
) -> tuple[JsonValue, dict[str, object]]:
    input_descriptor = descriptor["input"]
    artifact = input_descriptor["artifact"]
    expected_hash = artifact["hash"]
    body = _read(datastore, artifact["key"])
    if sha256_ref(body) != expected_hash:
        raise SchemaError("input artifact hash mismatch")
    value = _parse_canonical_json(body, "input artifact")
    schema = _schema(datastore, input_descriptor["schema"])
    _validate(schema, value, "input")
    return value, schema


def _schema(datastore: Datastore, reference: str) -> dict[str, object]:
    try:
        SHA256_REFERENCE.validate_python(reference)
    except PydanticValidationError:
        raise SchemaError("schema reference is not a SHA-256 reference")
    body = _read(datastore, f"blobs/sha256/{reference.removeprefix('sha256:')}")
    schema = _parse_canonical_json(body, "schema")
    if not isinstance(schema, dict):
        raise SchemaError("schema must be an object")
    if sha256_ref(body) != reference:
        raise SchemaError("schema hash mismatch")
    return cast(dict[str, object], schema)


def _validate(schema: Mapping[str, object], value: object, role: str) -> None:
    try:
        Draft202012Validator.check_schema(schema)
        validator = cast(SchemaValidator, Draft202012Validator(schema))
    except (JsonSchemaError, re.error, Unresolvable) as error:
        raise SchemaError(f"{role} JSON Schema cannot be used: {error}") from error
    try:
        validator.validate(value)
    except JsonSchemaValidationError as error:
        raise SchemaError(f"{role} does not satisfy its JSON Schema: {error}") from error
    except (re.error, Unresolvable) as error:
        raise SchemaError(f"{role} JSON Schema cannot be used: {error}") from error


def _parse_canonical_json(body: bytes, label: str) -> JsonValue:
    try:
        return parse_canonical_json(body)
    except CanonicalJsonError as error:
        raise SchemaError(f"{label} is not canonical JSON: {error}") from error


@contextmanager
def _source_root(
    source_package: SourcePackageDescriptor, datastore: Datastore
) -> Generator[Path, None, None]:
    archive = source_package["sourceArchive"]
    if archive["contentType"] != _SOURCE_ARCHIVE_CONTENT_TYPE:
        raise DescriptorError("Python runner requires application/vnd.massive.source-tar")
    body = datastore.get(archive["key"]).body
    if _sha256_ref_bytes(body) != archive["hash"]:
        raise DescriptorError("source archive hash mismatch")
    with TemporaryDirectory(prefix="massive-source-") as temporary:
        root = Path(temporary)
        try:
            with tarfile.open(fileobj=BytesIO(body), mode="r:") as archive_file:
                names: set[str] = set()
                total_size = 0
                for member in archive_file:
                    if (
                        not member.isfile()
                        or not _safe_archive_path(member.name)
                        or member.name in names
                    ):
                        raise DescriptorError(
                            f"source archive contains unsafe entry {member.name!r}"
                        )
                    total_size += member.size
                    if len(names) >= _MAX_SOURCE_FILES or total_size > _MAX_SOURCE_BYTES:
                        raise DescriptorError("source archive exceeds source package limits")
                    source = archive_file.extractfile(member)
                    if source is None:
                        raise DescriptorError(
                            f"source archive entry {member.name!r} cannot be read"
                        )
                    target = root / member.name
                    target.parent.mkdir(parents=True, exist_ok=True)
                    target.write_bytes(source.read())
                    target.chmod(0o444)
                    names.add(member.name)
        except (tarfile.TarError, OSError) as error:
            raise DescriptorError(f"source archive is invalid: {error}") from error
        yield root


def _resolve_step(
    symbol: SymbolDescriptor, source_root: Path
) -> tuple[StepFunction, TypeAdapter[object] | None, TypeAdapter[object] | None]:
    module_name = symbol["module"]
    if not module_name or any(not part.isidentifier() for part in module_name.split(".")):
        raise DescriptorError("Python module must be a dotted identifier")
    export = symbol["export"]
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


def _read(store: Datastore, key: str) -> bytes:
    try:
        return store.get(key).body
    except DatastoreNotFoundError as error:
        raise SchemaError(f"required datastore object is missing: {key}") from error


def _datastore(descriptor: DatastoreDescriptor) -> Datastore:
    if descriptor["kind"] == "local":
        return LocalDatastore(Path(descriptor["path"]))
    if descriptor["kind"] == "s3":
        return S3Datastore(descriptor)
    raise AssertionError("unreachable datastore kind")


def _sha256_ref_bytes(body: bytes) -> str:
    return "sha256:" + hashlib.sha256(body).hexdigest()


def _safe_archive_path(path: str) -> bool:
    return (
        bool(path)
        and not path.startswith("/")
        and "\\" not in path
        and all(part not in {"", ".", ".."} for part in path.split("/"))
    )


def main() -> int:
    if len(sys.argv) != 2:
        print("descriptor-resolution-failure: expected descriptor path", file=sys.stderr)
        return _DESCRIPTOR_EXIT
    return run_descriptor_path(Path(sys.argv[1]))


if __name__ == "__main__":
    raise SystemExit(main())
