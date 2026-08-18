from __future__ import annotations

import importlib.resources
import json
import re
from dataclasses import dataclass
from functools import cache
from pathlib import Path
from typing import Protocol, cast

from jsonschema import Draft202012Validator
from jsonschema.exceptions import SchemaError, ValidationError
from referencing.exceptions import Unresolvable

from .canonical import JsonValue, canonical_json, sha256_ref
from .datastore import Datastore, DatastoreConflictError, DatastoreNotFoundError

JSON_CONTENT_TYPE = "application/json"
MANIFEST_CONTENT_TYPE = "application/vnd.massive.data-artifact-manifest+json"


class ArtifactError(Exception):
    pass


class ArtifactValidationError(ArtifactError):
    pass


class ArtifactIntegrityError(ArtifactError):
    pass


class ArtifactNotFoundError(ArtifactError):
    pass


class ArtifactBodyConflictError(ArtifactError):
    pass


class ArtifactManifestConflictError(ArtifactError):
    pass


class SchemaValidator(Protocol):
    def validate(self, value: object) -> None: ...


@dataclass(frozen=True, slots=True)
class Destination:
    manifest_key: str
    schema: str


@dataclass(frozen=True, slots=True)
class Producer:
    project_key: str
    plan_hash: str
    run_id: str
    node_id: str
    attempt: int

    def json(self) -> dict[str, JsonValue]:
        return {
            "projectKey": self.project_key,
            "planHash": self.plan_hash,
            "runId": self.run_id,
            "nodeId": self.node_id,
            "attempt": self.attempt,
        }


@dataclass(frozen=True, slots=True)
class ArtifactRef:
    key: str
    hash: str
    size: int
    content_type: str

    def json(self) -> dict[str, JsonValue]:
        return {
            "key": self.key,
            "hash": self.hash,
            "size": self.size,
            "contentType": self.content_type,
        }


@dataclass(frozen=True, slots=True)
class PublishedJSON:
    manifest: ArtifactRef
    body: ArtifactRef
    schema: str


class ArtifactRuntime:
    """Publishes JSON through an immutable body followed by a manifest commit point."""

    def __init__(self, datastore: Datastore) -> None:
        self._datastore = datastore

    def publish_json(
        self, destination: Destination, producer: Producer, body: bytes
    ) -> PublishedJSON:
        _validate_destination(destination, producer)
        _validate_canonical_json(self._datastore, destination.schema, body)
        body_hash = sha256_ref(body)
        body_ref = ArtifactRef(
            key=_blob_key(body_hash),
            hash=body_hash,
            size=len(body),
            content_type=JSON_CONTENT_TYPE,
        )
        manifest = cast(
            dict[str, JsonValue],
            {
                "kind": "DataArtifactManifest",
                "schemaVersion": 0,
                "encoding": "canonical-json-v0",
                "producer": producer.json(),
                "schema": destination.schema,
                "body": body_ref.json(),
            },
        )
        manifest_body = _canonical_manifest(manifest)
        _put_immutable(
            self._datastore, body_ref.key, body, JSON_CONTENT_TYPE, ArtifactBodyConflictError
        )
        _put_immutable(
            self._datastore,
            destination.manifest_key,
            manifest_body,
            MANIFEST_CONTENT_TYPE,
            ArtifactManifestConflictError,
        )
        return PublishedJSON(
            manifest=ArtifactRef(
                key=destination.manifest_key,
                hash=sha256_ref(manifest_body),
                size=len(manifest_body),
                content_type=MANIFEST_CONTENT_TYPE,
            ),
            body=body_ref,
            schema=destination.schema,
        )

    def resolve_json(
        self, destination: Destination, producer: Producer
    ) -> tuple[PublishedJSON, bytes]:
        _validate_destination(destination, producer)
        try:
            manifest_object = self._datastore.get(destination.manifest_key)
        except DatastoreNotFoundError as error:
            raise ArtifactNotFoundError(
                f"artifact manifest {destination.manifest_key} is missing"
            ) from error
        if manifest_object.info.content_type != MANIFEST_CONTENT_TYPE:
            raise ArtifactIntegrityError(
                f"manifest {destination.manifest_key} has unexpected content type"
            )
        manifest = _parse_canonical_json(manifest_object.body, "manifest", ArtifactIntegrityError)
        try:
            _manifest_validator().validate(manifest)
        except ValidationError as error:
            raise ArtifactIntegrityError("manifest does not satisfy its schema") from error
        if not isinstance(manifest, dict):
            raise ArtifactIntegrityError("manifest must be an object")
        expected_producer = producer.json()
        if (
            manifest.get("producer") != expected_producer
            or manifest.get("schema") != destination.schema
        ):
            raise ArtifactIntegrityError("manifest does not match its expected producer and schema")
        body = manifest["body"]
        if not isinstance(body, dict):
            raise ArtifactIntegrityError("manifest body must be an object")
        body_ref = ArtifactRef(
            key=cast(str, body["key"]),
            hash=cast(str, body["hash"]),
            size=cast(int, body["size"]),
            content_type=cast(str, body["contentType"]),
        )
        if body_ref.key != _blob_key(body_ref.hash):
            raise ArtifactIntegrityError("manifest body key does not match its digest")
        try:
            body_object = self._datastore.get(body_ref.key)
        except DatastoreNotFoundError as error:
            raise ArtifactIntegrityError(f"manifest body {body_ref.key} is missing") from error
        if (
            body_object.info.content_type != JSON_CONTENT_TYPE
            or len(body_object.body) != body_ref.size
            or sha256_ref(body_object.body) != body_ref.hash
        ):
            raise ArtifactIntegrityError("manifest body does not match its reference")
        try:
            _validate_canonical_json(self._datastore, destination.schema, body_object.body)
        except ArtifactValidationError as error:
            raise ArtifactIntegrityError("manifest body does not satisfy its schema") from error
        return (
            PublishedJSON(
                manifest=ArtifactRef(
                    key=destination.manifest_key,
                    hash=sha256_ref(manifest_object.body),
                    size=len(manifest_object.body),
                    content_type=MANIFEST_CONTENT_TYPE,
                ),
                body=body_ref,
                schema=destination.schema,
            ),
            body_object.body,
        )


def _validate_destination(destination: Destination, producer: Producer) -> None:
    expected_key = (
        f"projects/{producer.project_key}/runs/{producer.run_id}/steps/"
        f"{producer.node_id}/{producer.attempt}/output-manifest.json"
    )
    if destination.manifest_key != expected_key:
        raise ArtifactValidationError(
            f"manifest destination {destination.manifest_key!r} does not match producer slot"
        )
    if producer.attempt < 1 or not all(
        (
            producer.project_key,
            producer.plan_hash,
            producer.run_id,
            producer.node_id,
            destination.schema,
        )
    ):
        raise ArtifactValidationError("producer and schema identity must be present")
    if re.fullmatch(r"sha256-[0-9a-f]{64}", producer.project_key) is None:
        raise ArtifactValidationError("project key must be a normalized SHA-256 identity")
    try:
        _blob_key(destination.schema)
    except ArtifactValidationError as error:
        raise ArtifactValidationError("schema reference must be a SHA-256 reference") from error


def _validate_canonical_json(datastore: Datastore, schema_ref: str, body: bytes) -> None:
    document = _parse_canonical_json(body, "value", ArtifactValidationError)
    schema_key = _blob_key(schema_ref)
    try:
        schema_body = datastore.get(schema_key).body
    except DatastoreNotFoundError as error:
        raise ArtifactValidationError(f"schema {schema_ref} is missing") from error
    schema = _parse_canonical_json(schema_body, "schema", ArtifactValidationError)
    if sha256_ref(schema_body) != schema_ref:
        raise ArtifactValidationError(f"schema {schema_ref} does not match its digest")
    if not isinstance(schema, dict):
        raise ArtifactValidationError("schema must be an object")
    try:
        Draft202012Validator.check_schema(schema)
        cast(SchemaValidator, Draft202012Validator(schema)).validate(document)
    except (
        SchemaError,
        ValidationError,
        re.error,
        Unresolvable,
    ) as error:
        raise ArtifactValidationError(f"value does not satisfy schema {schema_ref}") from error


def _parse_canonical_json(body: bytes, label: str, error_type: type[ArtifactError]) -> JsonValue:
    try:
        value = cast(JsonValue, json.loads(body))
        if canonical_json(value).encode() != body:
            raise ValueError("JSON body is not canonical")
    except (UnicodeDecodeError, json.JSONDecodeError, TypeError, ValueError) as error:
        raise error_type(f"{label} is not canonical JSON") from error
    return value


def _canonical_manifest(manifest: dict[str, JsonValue]) -> bytes:
    try:
        _manifest_validator().validate(manifest)
        return canonical_json(manifest).encode()
    except (TypeError, ValueError, ValidationError) as error:
        raise ArtifactValidationError("artifact manifest does not satisfy its schema") from error


def _put_immutable(
    datastore: Datastore,
    key: str,
    body: bytes,
    content_type: str,
    conflict_error: type[ArtifactBodyConflictError | ArtifactManifestConflictError],
) -> None:
    try:
        datastore.put(key, body, content_type=content_type, if_absent=True)
        return
    except DatastoreConflictError:
        pass
    try:
        existing = datastore.get(key)
    except DatastoreNotFoundError as error:
        raise conflict_error(f"cannot inspect existing immutable object {key}") from error
    if existing.info.content_type != content_type or existing.body != body:
        raise conflict_error(f"existing immutable object {key} differs")


def _blob_key(hash_ref: str) -> str:
    prefix = "sha256:"
    digest = hash_ref.removeprefix(prefix)
    if (
        not hash_ref.startswith(prefix)
        or len(digest) != 64
        or any(char not in "0123456789abcdef" for char in digest)
    ):
        raise ArtifactValidationError(f"invalid SHA-256 reference {hash_ref!r}")
    return f"blobs/sha256/{digest}"


@cache
def _manifest_validator() -> SchemaValidator:
    source = importlib.resources.files("massive").joinpath(
        "schemas", "data-artifact-manifest.schema.json"
    )
    if source.is_file():
        document = json.loads(source.read_text(encoding="utf-8"))
    else:
        document = json.loads(
            (
                Path(__file__).resolve().parents[4]
                / "conformance/schema/data-artifact-manifest.schema.json"
            ).read_text()
        )
    Draft202012Validator.check_schema(document)
    return cast(SchemaValidator, Draft202012Validator(document))
