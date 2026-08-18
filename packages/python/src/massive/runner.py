from __future__ import annotations

import asyncio
import hashlib
import importlib
import inspect
import json
import sys
import tarfile
from collections.abc import Awaitable, Callable, Generator, Mapping
from contextlib import contextmanager
from io import BytesIO
from pathlib import Path
from tempfile import TemporaryDirectory
from typing import TYPE_CHECKING, Protocol, cast

from botocore.config import Config
from botocore.session import get_session
from jsonschema import Draft202012Validator
from pydantic import TypeAdapter

if TYPE_CHECKING:
    from mypy_boto3_s3 import S3Client

from .builder import StepDefinition
from .canonical import JsonValue, canonical_json, sha256_ref
from .context import InvocationContext, StepContext

_SOURCE_ARCHIVE_CONTENT_TYPE = "application/vnd.massive.source-tar"
_MAX_SOURCE_FILES = 1024
_MAX_SOURCE_BYTES = 50 * 1024 * 1024
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


class Datastore(Protocol):
    def read(self, key: str) -> bytes: ...

    def write(self, key: str, body: bytes) -> None: ...


class LocalDatastore:
    def __init__(self, root: Path) -> None:
        self.root = root.resolve()

    def read(self, key: str) -> bytes:
        return _path(self.root, key).read_bytes()

    def write(self, key: str, body: bytes) -> None:
        target = _path(self.root, key)
        target.parent.mkdir(parents=True, exist_ok=True)
        temporary = target.with_name(f".tmp-{target.name}")
        temporary.write_bytes(body)
        temporary.replace(target)
        metadata = (
            self.root
            / ".massive-datastore-metadata"
            / f"{hashlib.sha256(key.encode()).hexdigest()}.json"
        )
        metadata.parent.mkdir(parents=True, exist_ok=True)
        metadata.write_text('{"contentType":"application/json"}')


class S3Datastore:
    def __init__(self, descriptor: Descriptor) -> None:
        endpoint_value = descriptor.get("endpoint")
        endpoint = endpoint_value if isinstance(endpoint_value, str) else None
        config = (
            Config(s3={"addressing_style": "path"})
            if descriptor.get("forcePathStyle") is True
            else None
        )
        self.bucket = _string(descriptor, "bucket")
        prefix = descriptor.get("prefix", "")
        if not isinstance(prefix, str):
            raise DescriptorError("S3 datastore prefix must be a string")
        self.prefix = prefix.strip("/")
        self.client = cast(
            "S3Client",
            get_session().create_client(
                "s3",
                region_name=_string(descriptor, "region"),
                endpoint_url=endpoint,
                config=config,
            ),
        )

    def read(self, key: str) -> bytes:
        return self.client.get_object(Bucket=self.bucket, Key=self._key(key))["Body"].read()

    def write(self, key: str, body: bytes) -> None:
        self.client.put_object(
            Bucket=self.bucket, Key=self._key(key), Body=body, ContentType="application/json"
        )

    def _key(self, key: str) -> str:
        return key if self.prefix == "" else f"{self.prefix}/{key}"


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
    if datastore.get("kind") == "local":
        _string(datastore, "path")
    elif datastore.get("kind") == "s3":
        _string(datastore, "bucket")
        _string(datastore, "region")
    else:
        raise DescriptorError("datastore kind must be local or s3")
    return typed_descriptor


def _execute(descriptor: Descriptor) -> None:
    symbol = _require_mapping(descriptor, "symbol")
    source_package = _require_mapping(descriptor, "sourcePackage")
    if symbol.get("language") != "python" or source_package.get("language") != "python":
        raise DescriptorError("Python runner requires Python symbol and source package")
    if symbol.get("packageId") != source_package.get("packageId"):
        raise DescriptorError("symbol package does not match source package")
    datastore = _datastore(_require_mapping(descriptor, "datastore"))
    input_value, _input_schema = _read_input(descriptor, datastore)
    with _source_root(source_package, datastore) as source_root:
        function, input_adapter, output_adapter = _resolve_step(symbol, source_root)
        _execute_source(descriptor, datastore, function, input_adapter, output_adapter, input_value)


def _execute_source(
    descriptor: Descriptor,
    datastore: Datastore,
    function: StepFunction,
    input_adapter: TypeAdapter[object] | None,
    output_adapter: TypeAdapter[object] | None,
    input_value: object,
) -> None:
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
    datastore.write(_string(output_artifact, "key"), output_text.encode())


async def _await_output(value: Awaitable[object]) -> object:
    return await value


def _read_input(
    descriptor: Descriptor, datastore: Datastore
) -> tuple[JsonValue, dict[str, object]]:
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


def _schema(datastore: Datastore, reference: str) -> dict[str, object]:
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


@contextmanager
def _source_root(source_package: Descriptor, datastore: Datastore) -> Generator[Path, None, None]:
    archive = _require_mapping(source_package, "sourceArchive")
    if archive.get("contentType") != _SOURCE_ARCHIVE_CONTENT_TYPE:
        raise DescriptorError("Python runner requires application/vnd.massive.source-tar")
    body = datastore.read(_string(archive, "key"))
    if _sha256_ref_bytes(body) != _string(archive, "hash"):
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


def _read(store: Datastore, key: str) -> str:
    return store.read(key).decode()


def _datastore(descriptor: Descriptor) -> Datastore:
    if descriptor.get("kind") == "local":
        return LocalDatastore(Path(_string(descriptor, "path")))
    if descriptor.get("kind") == "s3":
        return S3Datastore(descriptor)
    raise DescriptorError("datastore kind must be local or s3")


def _sha256_ref_bytes(body: bytes) -> str:
    return "sha256:" + hashlib.sha256(body).hexdigest()


def _safe_archive_path(path: str) -> bool:
    return (
        bool(path)
        and not path.startswith("/")
        and "\\" not in path
        and all(part not in {"", ".", ".."} for part in path.split("/"))
    )


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
