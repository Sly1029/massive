from __future__ import annotations

import hashlib
import json
import os
from dataclasses import dataclass
from pathlib import Path
from tempfile import NamedTemporaryFile
from typing import TYPE_CHECKING, Literal, NotRequired, Protocol, TypedDict, cast
from uuid import uuid4

from botocore.config import Config
from botocore.exceptions import ClientError
from botocore.session import get_session

if TYPE_CHECKING:
    from mypy_boto3_s3 import S3Client


class LocalDatastoreDescriptor(TypedDict):
    kind: Literal["local"]
    path: str


class S3DatastoreDescriptor(TypedDict):
    kind: Literal["s3"]
    bucket: str
    region: str
    prefix: NotRequired[str]
    endpoint: NotRequired[str]
    forcePathStyle: NotRequired[bool]


type DatastoreDescriptor = LocalDatastoreDescriptor | S3DatastoreDescriptor


@dataclass(frozen=True, slots=True)
class ObjectInfo:
    key: str
    size: int
    content_type: str


@dataclass(frozen=True, slots=True)
class DatastoreObject:
    info: ObjectInfo
    body: bytes


class DatastoreConflictError(Exception):
    pass


class DatastoreNotFoundError(Exception):
    pass


class Datastore(Protocol):
    def put(
        self, key: str, body: bytes, *, content_type: str, if_absent: bool = False
    ) -> ObjectInfo: ...

    def get(self, key: str) -> DatastoreObject: ...


class LocalDatastore:
    def __init__(self, root: Path) -> None:
        self.root = root.resolve()

    def put(
        self, key: str, body: bytes, *, content_type: str, if_absent: bool = False
    ) -> ObjectInfo:
        target = self.path_for_key(key)
        target.parent.mkdir(parents=True, exist_ok=True)
        if if_absent:
            self._ensure_immutable_metadata(key, content_type)
        temporary = target.with_name(f".tmp-{target.name}-{uuid4()}")
        installed = False
        try:
            temporary.write_bytes(body)
            if if_absent:
                try:
                    os.link(temporary, target)
                except FileExistsError as error:
                    raise DatastoreConflictError(
                        f"datastore object already exists: {key}"
                    ) from error
                temporary.unlink()
            else:
                temporary.replace(target)
            installed = True
        finally:
            if not installed:
                temporary.unlink(missing_ok=True)
        if not if_absent:
            self._write_content_type(key, content_type)
        return ObjectInfo(key=key, size=len(body), content_type=content_type)

    def get(self, key: str) -> DatastoreObject:
        try:
            body = self.path_for_key(key).read_bytes()
        except FileNotFoundError as error:
            raise DatastoreNotFoundError(f"datastore object not found: {key}") from error
        return DatastoreObject(
            info=ObjectInfo(key=key, size=len(body), content_type=self._read_content_type(key)),
            body=body,
        )

    def path_for_key(self, key: str) -> Path:
        _validate_key(key)
        target = (self.root / key).resolve()
        if self.root not in target.parents:
            raise ValueError("datastore key escapes the local datastore root")
        return target

    def _metadata_path(self, key: str) -> Path:
        digest = hashlib.sha256(key.encode()).hexdigest()
        return self.root / ".massive-datastore-metadata" / f"{digest}.json"

    def _write_content_type(self, key: str, content_type: str) -> None:
        metadata = self._metadata_path(key)
        metadata.parent.mkdir(parents=True, exist_ok=True)
        with NamedTemporaryFile(
            mode="w", encoding="utf-8", dir=metadata.parent, delete=False
        ) as file:
            file.write(json.dumps({"contentType": content_type}, separators=(",", ":")))
            temporary = Path(file.name)
        temporary.replace(metadata)

    # An IfAbsent object publishes its immutable metadata before its body. That
    # prevents a concurrent reader from observing an installed body with the
    # default content type. A metadata-only crash is recoverable: an identical
    # retry installs the absent body, while a differing content type cannot
    # change the record established by the first publisher.
    def _ensure_immutable_metadata(self, key: str, content_type: str) -> None:
        metadata = self._metadata_path(key)
        metadata.parent.mkdir(parents=True, exist_ok=True)
        expected = json.dumps({"contentType": content_type}, separators=(",", ":")).encode()
        temporary = metadata.with_name(f".tmp-{metadata.name}-{uuid4()}")
        try:
            temporary.write_bytes(expected)
            try:
                os.link(temporary, metadata)
                return
            except FileExistsError:
                pass
            if metadata.read_bytes() != expected:
                raise DatastoreConflictError(f"datastore object already exists: {key}")
        finally:
            temporary.unlink(missing_ok=True)

    def _read_content_type(self, key: str) -> str:
        try:
            value: object = json.loads(self._metadata_path(key).read_text())
        except FileNotFoundError:
            return "application/octet-stream"
        if not isinstance(value, dict):
            raise TypeError(f"invalid datastore metadata for {key}")
        content_type = cast(dict[str, object], value).get("contentType")
        if not isinstance(content_type, str):
            raise TypeError(f"invalid datastore metadata for {key}")
        return content_type


class S3Datastore:
    def __init__(self, descriptor: S3DatastoreDescriptor) -> None:
        self.bucket = descriptor["bucket"]
        prefix = descriptor.get("prefix", "")
        self.prefix = _normalize_prefix(prefix)
        config = (
            Config(s3={"addressing_style": "path"}) if descriptor.get("forcePathStyle") else None
        )
        self.client = cast(
            "S3Client",
            get_session().create_client(
                "s3",
                region_name=descriptor["region"],
                endpoint_url=descriptor.get("endpoint"),
                config=config,
            ),
        )

    def put(
        self, key: str, body: bytes, *, content_type: str, if_absent: bool = False
    ) -> ObjectInfo:
        _validate_key(key)
        try:
            if if_absent:
                self.client.put_object(
                    Bucket=self.bucket,
                    Key=self._key(key),
                    Body=body,
                    ContentType=content_type,
                    IfNoneMatch="*",
                )
            else:
                self.client.put_object(
                    Bucket=self.bucket,
                    Key=self._key(key),
                    Body=body,
                    ContentType=content_type,
                )
        except ClientError as error:
            if _s3_status(error) == 412 or _s3_code(error) == "PreconditionFailed":
                raise DatastoreConflictError(f"datastore object already exists: {key}") from error
            raise
        return ObjectInfo(key=key, size=len(body), content_type=content_type)

    def get(self, key: str) -> DatastoreObject:
        _validate_key(key)
        try:
            result = self.client.get_object(Bucket=self.bucket, Key=self._key(key))
        except ClientError as error:
            if _s3_status(error) == 404 or _s3_code(error) in {
                "NoSuchKey",
                "NoSuchBucket",
                "NotFound",
            }:
                raise DatastoreNotFoundError(f"datastore object not found: {key}") from error
            raise
        stream = result["Body"]
        body = stream.read()
        return DatastoreObject(
            info=ObjectInfo(
                key=key,
                size=len(body),
                content_type=result.get("ContentType") or "application/octet-stream",
            ),
            body=body,
        )

    def _key(self, key: str) -> str:
        return key if self.prefix == "" else f"{self.prefix}/{key}"


def datastore_from_descriptor(descriptor: DatastoreDescriptor) -> Datastore:
    if descriptor["kind"] == "local":
        return LocalDatastore(Path(descriptor["path"]))
    if descriptor["kind"] == "s3":
        return S3Datastore(descriptor)
    raise AssertionError("unreachable datastore kind")


def _validate_key(key: str) -> None:
    if not key or key.startswith("/") or "\\" in key:
        raise ValueError(f"invalid datastore key {key!r}")
    segments = key.split("/")
    if any(segment in {"", ".", ".."} for segment in segments):
        raise ValueError(f"invalid datastore key {key!r}")


def _normalize_prefix(prefix: str) -> str:
    if prefix == "":
        return ""
    trimmed = prefix.rstrip("/")
    _validate_key(trimmed)
    return trimmed


def _s3_code(error: ClientError) -> str:
    return error.response.get("Error", {}).get("Code", "")


def _s3_status(error: ClientError) -> int | None:
    value = error.response.get("ResponseMetadata", {}).get("HTTPStatusCode")
    return value if isinstance(value, int) else None
