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
from pydantic import AliasChoices, BaseModel, ConfigDict, Field, StrictStr
from referencing.exceptions import Unresolvable

from .canonical import (
    CanonicalJsonError,
    JsonValue,
    canonical_json,
    parse_canonical_json,
    sha256_ref,
)
from .datastore import Datastore, DatastoreConflictError, DatastoreNotFoundError
from .identity import (
    ExecutionScope,
    PositiveAttempt,
    ProjectKey,
    SafePathSegment,
    Sha256Reference,
)

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


class Destination(BaseModel):
    model_config = ConfigDict(frozen=True, extra="forbid")

    manifest_key: StrictStr = Field(
        validation_alias=AliasChoices("manifest_key", "manifestKey"),
        serialization_alias="manifestKey",
        min_length=1,
    )
    schema_ref: Sha256Reference = Field(
        validation_alias=AliasChoices("schema_ref", "schema"), serialization_alias="schema"
    )


class Producer(BaseModel):
    model_config = ConfigDict(frozen=True, extra="forbid")

    project_key: ProjectKey = Field(
        validation_alias=AliasChoices("project_key", "projectKey"), serialization_alias="projectKey"
    )
    plan_hash: Sha256Reference = Field(
        validation_alias=AliasChoices("plan_hash", "planHash"), serialization_alias="planHash"
    )
    run_id: SafePathSegment = Field(
        validation_alias=AliasChoices("run_id", "runId"), serialization_alias="runId"
    )
    node_id: SafePathSegment = Field(
        validation_alias=AliasChoices("node_id", "nodeId"), serialization_alias="nodeId"
    )
    attempt: PositiveAttempt
    scope: ExecutionScope | None = None

    def identity_json(self) -> dict[str, JsonValue]:
        return cast(
            dict[str, JsonValue], self.model_dump(mode="json", by_alias=True, exclude_none=True)
        )


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

    def validate_destination(self, destination: Destination, producer: Producer) -> None:
        """Validate an immutable producer slot before invoking user code."""
        _validate_destination(destination, producer)

    def publish_json(
        self, destination: Destination, producer: Producer, body: bytes
    ) -> PublishedJSON:
        _validate_destination(destination, producer)
        _validate_canonical_json(self._datastore, destination.schema_ref, body)
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
                "schemaVersion": 1,
                "encoding": "canonical-json-v0",
                "producer": producer.identity_json(),
                "schema": destination.schema_ref,
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
            schema=destination.schema_ref,
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
        expected_producer = producer.identity_json()
        if (
            manifest.get("producer") != expected_producer
            or manifest.get("schema") != destination.schema_ref
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
            _validate_canonical_json(self._datastore, destination.schema_ref, body_object.body)
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
                schema=destination.schema_ref,
            ),
            body_object.body,
        )


def _validate_destination(destination: Destination, producer: Producer) -> None:
    expected_key = (
        f"projects/{producer.project_key}/runs/{producer.run_id}/steps/"
        f"{producer.node_id}{_scope_key_suffix(producer.scope)}/{producer.attempt}/output-manifest.json"
    )
    if destination.manifest_key != expected_key:
        raise ArtifactValidationError(
            f"manifest destination {destination.manifest_key!r} does not match producer slot"
        )
    try:
        _blob_key(destination.schema_ref)
    except ArtifactValidationError as error:
        raise ArtifactValidationError("schema reference must be a SHA-256 reference") from error


def _scope_key_suffix(scope: ExecutionScope | None) -> str:
    if scope is None:
        return ""
    return "/scopes" + "".join(
        f"/maps/{frame.map_id}/items/{frame.index}" for frame in scope.frames
    )


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
        validator = cast(SchemaValidator, Draft202012Validator(schema))
    except (SchemaError, re.error, Unresolvable) as error:
        raise ArtifactValidationError(f"schema {schema_ref} cannot be used") from error
    try:
        validator.validate(document)
    except ValidationError as error:
        raise ArtifactValidationError(f"value does not satisfy schema {schema_ref}") from error
    except (re.error, Unresolvable) as error:
        raise ArtifactValidationError(f"schema {schema_ref} cannot be used") from error


def _parse_canonical_json(body: bytes, label: str, error_type: type[ArtifactError]) -> JsonValue:
    try:
        return parse_canonical_json(body)
    except CanonicalJsonError as error:
        raise error_type(f"{label} is not canonical JSON") from error


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
