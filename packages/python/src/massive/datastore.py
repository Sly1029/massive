from __future__ import annotations

import hashlib
import json
import os
from collections.abc import Iterator
from dataclasses import dataclass
from pathlib import Path
from tempfile import NamedTemporaryFile
from typing import TYPE_CHECKING, BinaryIO, Literal, NotRequired, Protocol, TypedDict, cast
from uuid import uuid4

from botocore.config import Config
from botocore.exceptions import ClientError
from botocore.session import get_session

if TYPE_CHECKING:
    from mypy_boto3_s3 import S3Client
    from mypy_boto3_s3.type_defs import GetObjectOutputTypeDef

_CHUNK_BYTES = 1024 * 1024


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


class DatastoreObjectTooLargeError(Exception):
    pass


class DatastoreAccessDeniedError(Exception):
    """The store refused a read. Without list permission, S3 reports a missing
    key this way too, so callers cannot tell the two apart."""


class Datastore(Protocol):
    def put(
        self, key: str, body: bytes, *, content_type: str, if_absent: bool = False
    ) -> ObjectInfo: ...

    def get(self, key: str) -> DatastoreObject: ...

    def download(self, key: str, destination: BinaryIO, *, max_bytes: int) -> ObjectInfo:
        """Stream one object into an open file without buffering its body.

        Raises DatastoreObjectTooLargeError, without writing past max_bytes,
        when the object is larger.
        """
        ...


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

    def download(self, key: str, destination: BinaryIO, *, max_bytes: int) -> ObjectInfo:
        try:
            with self.path_for_key(key).open("rb") as source:
                _check_size(key, os.fstat(source.fileno()).st_size, max_bytes)
                _copy_bounded(
                    key, iter(lambda: source.read(_CHUNK_BYTES), b""), destination, max_bytes
                )
        except FileNotFoundError as error:
            raise DatastoreNotFoundError(f"datastore object not found: {key}") from error
        return ObjectInfo(
            key=key, size=destination.tell(), content_type=self._read_content_type(key)
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
        config = None
        if "forcePathStyle" in descriptor:
            config = Config(
                s3={"addressing_style": "path" if descriptor["forcePathStyle"] else "virtual"}
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
        result = self._get_object(key)
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

    def download(self, key: str, destination: BinaryIO, *, max_bytes: int) -> ObjectInfo:
        result = self._get_object(key)
        try:
            _check_size(key, result["ContentLength"], max_bytes)
            _copy_bounded(key, result["Body"].iter_chunks(_CHUNK_BYTES), destination, max_bytes)
        finally:
            result["Body"].close()
        return ObjectInfo(
            key=key,
            size=destination.tell(),
            content_type=result.get("ContentType") or "application/octet-stream",
        )

    def _get_object(self, key: str) -> GetObjectOutputTypeDef:
        _validate_key(key)
        try:
            return self.client.get_object(Bucket=self.bucket, Key=self._key(key))
        except ClientError as error:
            if _s3_status(error) == 404 or _s3_code(error) in {
                "NoSuchKey",
                "NoSuchBucket",
                "NotFound",
            }:
                raise DatastoreNotFoundError(f"datastore object not found: {key}") from error
            if _s3_status(error) == 403 or _s3_code(error) == "AccessDenied":
                raise DatastoreAccessDeniedError(f"datastore read denied: {key}") from error
            raise

    def _key(self, key: str) -> str:
        return key if self.prefix == "" else f"{self.prefix}/{key}"


def datastore_from_descriptor(descriptor: DatastoreDescriptor) -> Datastore:
    if descriptor["kind"] == "local":
        return LocalDatastore(Path(descriptor["path"]))
    if descriptor["kind"] == "s3":
        return S3Datastore(descriptor)
    raise AssertionError("unreachable datastore kind")


def _check_size(key: str, size: int, max_bytes: int) -> None:
    if size > max_bytes:
        raise DatastoreObjectTooLargeError(
            f"datastore object {key} is {size} bytes, above the {max_bytes}-byte limit"
        )


def _copy_bounded(key: str, chunks: Iterator[bytes], destination: BinaryIO, max_bytes: int) -> None:
    # The declared size is checked first; counting again guards a store that
    # returns more bytes than it declared.
    written = 0
    for chunk in chunks:
        written += len(chunk)
        _check_size(key, written, max_bytes)
        destination.write(chunk)


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
