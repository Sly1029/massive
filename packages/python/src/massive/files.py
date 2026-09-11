"""Typed file references with invocation-scoped hydration and immutable CAS bodies."""

from __future__ import annotations

import os
import stat
from pathlib import Path, PurePosixPath
from typing import Annotated, Literal, Self
from uuid import uuid4

from pydantic import (
    BaseModel,
    ConfigDict,
    Field,
    PrivateAttr,
    SerializationInfo,
    SerializerFunctionWrapHandler,
    StrictInt,
    ValidationInfo,
    model_serializer,
    model_validator,
)

from .artifact import ArtifactBodyConflictError, ArtifactIntegrityError, put_immutable
from .canonical import canonical_json, parse_canonical_json, sha256_ref
from .datastore import Datastore
from .identity import Sha256Reference

_BYTES = "application/octet-stream"
_TREE = "application/vnd.massive.tree+json"
type ByteSize = Annotated[StrictInt, Field(ge=0, le=(1 << 53) - 1)]


class _FileReference(BaseModel):
    model_config = ConfigDict(frozen=True, extra="forbid")

    hash: Sha256Reference
    size: ByteSize
    _source: Path | None = PrivateAttr(default=None)
    _local: Path | None = PrivateAttr(default=None)
    _files: ArtifactFiles | None = PrivateAttr(default=None)

    def __eq__(self, other: object) -> bool:
        return (
            type(self) is type(other)
            and isinstance(other, _FileReference)
            and self.hash == other.hash
            and self.size == other.size
        )

    @model_validator(mode="after")
    def _bind(self, info: ValidationInfo) -> Self:
        if isinstance(info.context, ArtifactFiles) and self._files is not info.context:
            bound = self.model_copy()
            bound._files = info.context
            bound._local = None
            return bound
        return self

    @model_serializer(mode="wrap")
    def _serialize(self, handler: SerializerFunctionWrapHandler, info: SerializationInfo):
        if isinstance(info.context, ArtifactFiles):
            self.publish(info.context)
        return handler(self)

    def publish(self, files: ArtifactFiles) -> None:
        raise NotImplementedError


class Blob(_FileReference):
    """An opaque immutable file. ``path()`` hydrates it within the current invocation."""

    kind: Literal["massive.blob.v1"]

    @classmethod
    def from_path(cls, path: Path | str) -> Self:
        source = Path(path).absolute()
        if not stat.S_ISREG(source.lstat().st_mode):
            raise ValueError("Blob source must be a regular file, not a symlink")
        body = _read_regular_file(source)
        result = cls(kind="massive.blob.v1", hash=sha256_ref(body), size=len(body))
        result._source = source
        return result

    def publish(self, files: ArtifactFiles) -> None:
        if self._source is None:
            files.read(self, _BYTES)
            return
        if not stat.S_ISREG(self._source.lstat().st_mode):
            raise ArtifactIntegrityError("Blob source is no longer a regular file")
        files.commit(self, _read_regular_file(self._source), _BYTES)

    def path(self) -> Path:
        if self._source is not None:
            return self._source
        if self._local is None:
            if self._files is None:
                raise ValueError("Blob.path() requires an invocation or ArtifactFiles binding")
            self._local = self._files.hydrate_blob(self)
        return self._local


class _TreeFile(BaseModel):
    model_config = ConfigDict(frozen=True, extra="forbid", strict=True)
    path: str
    blob: Blob
    executable: bool


class _TreeManifest(BaseModel):
    model_config = ConfigDict(frozen=True, extra="forbid", strict=True)
    version: Annotated[StrictInt, Field(ge=1, le=1)] = 1
    directories: list[str]
    files: list[_TreeFile]

    @model_validator(mode="after")
    def _validate_paths(self) -> Self:
        directories = set(self.directories)
        paths = self.directories + [entry.path for entry in self.files]
        if len(set(paths)) != len(paths):
            raise ValueError("tree paths must be unique")
        if self.directories != sorted(self.directories) or [f.path for f in self.files] != sorted(
            f.path for f in self.files
        ):
            raise ValueError("tree entries must be sorted by Unicode code point")
        for path in paths:
            parsed = PurePosixPath(path)
            if (
                not path
                or path == "."
                or parsed.is_absolute()
                or parsed.as_posix() != path
                or any(part in {".", ".."} for part in parsed.parts)
                or "\\" in path
                or "\x00" in path
            ):
                raise ValueError(f"unsafe tree path: {path!r}")
            if parsed.parent != PurePosixPath(".") and str(parsed.parent) not in directories:
                raise ValueError(f"tree parent directory is missing: {path!r}")
        return self


class Tree(_FileReference):
    """A directory snapshot. Hydrated mutations persist only via a new ``from_path()``."""

    kind: Literal["massive.tree.v1"]
    _manifest: _TreeManifest | None = PrivateAttr(default=None)

    @classmethod
    def from_path(cls, path: Path | str) -> Self:
        root = Path(path).absolute()
        if not stat.S_ISDIR(root.lstat().st_mode):
            raise ValueError("Tree source must be a directory, not a symlink")
        directories: list[str] = []
        files: list[_TreeFile] = []
        pending = [root]
        while pending:
            directory = pending.pop()
            # scandir propagates read failures: an unreadable directory must never
            # silently disappear from a supposedly complete snapshot.
            with os.scandir(directory) as entries:
                for entry in entries:
                    mode = entry.stat(follow_symlinks=False).st_mode
                    path = Path(entry.path)
                    relative = path.relative_to(root).as_posix()
                    if stat.S_ISDIR(mode):
                        directories.append(relative)
                        pending.append(path)
                    elif stat.S_ISREG(mode):
                        files.append(
                            _TreeFile(
                                path=relative,
                                blob=Blob.from_path(path),
                                executable=bool(mode & 0o111),
                            )
                        )
                    else:
                        raise ValueError(
                            f"Tree entries must be regular files or directories: {relative}"
                        )
        directories.sort()
        files.sort(key=lambda entry: entry.path)
        manifest = _TreeManifest(directories=directories, files=files)
        body = canonical_json(manifest.model_dump(mode="json")).encode()
        result = cls(kind="massive.tree.v1", hash=sha256_ref(body), size=len(body))
        result._source = root
        result._manifest = manifest
        return result

    def publish(self, files: ArtifactFiles) -> None:
        if self._manifest is None:
            manifest = _TreeManifest.model_validate(parse_canonical_json(files.read(self, _TREE)))
            for entry in manifest.files:
                files.read(entry.blob, _BYTES)
            return
        for entry in self._manifest.files:
            entry.blob.publish(files)
        files.commit(self, canonical_json(self._manifest.model_dump(mode="json")).encode(), _TREE)

    def path(self) -> Path:
        if self._source is not None:
            return self._source
        if self._local is None:
            if self._files is None:
                raise ValueError("Tree.path() requires an invocation or ArtifactFiles binding")
            self._local = self._files.hydrate_tree(self)
        return self._local


class ArtifactFiles:
    """Bind typed handles to a datastore and caller-owned, isolated scratch directory.

    Pass this object as Pydantic's validation/serialization ``context`` when reading
    or publishing handles outside a runner. The caller owns scratch cleanup.
    """

    def __init__(self, datastore: Datastore, scratch: Path) -> None:
        self._datastore = datastore
        self._scratch = scratch

    def read(self, reference: _FileReference, content_type: str) -> bytes:
        obj = self._datastore.get(_file_key(reference))
        if (
            len(obj.body) != reference.size
            or sha256_ref(obj.body) != reference.hash
            or obj.info.content_type != content_type
        ):
            raise ArtifactIntegrityError("file artifact body does not match its reference")
        return obj.body

    def commit(self, reference: _FileReference, body: bytes, content_type: str) -> None:
        if len(body) != reference.size or sha256_ref(body) != reference.hash:
            raise ArtifactIntegrityError(
                "artifact source changed after from_path(); snapshot it again"
            )
        put_immutable(
            self._datastore,
            _file_key(reference),
            body,
            content_type,
            ArtifactBodyConflictError,
        )

    def hydrate_blob(self, reference: Blob) -> Path:
        body = self.read(reference, _BYTES)
        self._scratch.mkdir(parents=True, exist_ok=True)
        target = self._scratch / uuid4().hex
        target.write_bytes(body)
        return target

    def hydrate_tree(self, reference: Tree) -> Path:
        manifest = _TreeManifest.model_validate(parse_canonical_json(self.read(reference, _TREE)))
        target = self._scratch / uuid4().hex
        target.mkdir(parents=True)
        for directory in manifest.directories:
            (target / directory).mkdir()
        for entry in manifest.files:
            path = target / entry.path
            path.write_bytes(self.read(entry.blob, _BYTES))
            path.chmod(0o755 if entry.executable else 0o644)
        return target


def _file_key(reference: _FileReference) -> str:
    kind = "trees" if isinstance(reference, Tree) else "blobs"
    return f"file-artifacts/{kind}/sha256/{reference.hash.removeprefix('sha256:')}"


def _read_regular_file(path: Path) -> bytes:
    # O_NONBLOCK prevents a concurrent replacement with a FIFO from hanging;
    # O_NOFOLLOW closes the lstat/open race for a replaced leaf symlink.
    descriptor = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    with os.fdopen(descriptor, "rb") as stream:
        if not stat.S_ISREG(os.fstat(stream.fileno()).st_mode):
            raise ValueError("artifact source must be a regular file")
        return stream.read()
