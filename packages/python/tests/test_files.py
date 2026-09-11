from __future__ import annotations

import json
from pathlib import Path

import pytest
from pydantic import BaseModel, TypeAdapter, ValidationError
from pydantic_core import PydanticSerializationError

from massive import ArtifactFiles, Blob, Tree
from massive.artifact import ArtifactIntegrityError
from massive.canonical import canonical_json, sha256_ref
from massive.datastore import LocalDatastore


class Payload(BaseModel):
    trees: list[Tree]
    report: Blob


def test_nested_handles_publish_hydrate_and_isolate_working_copies(tmp_path: Path) -> None:
    source = tmp_path / "source"
    source.mkdir()
    (source / "empty").mkdir()
    (source / "run.sh").write_bytes(b"#!/bin/sh\necho hello\n")
    (source / "run.sh").chmod(0o751)
    store = LocalDatastore(tmp_path / "store")
    writer = ArtifactFiles(store, tmp_path / "writer")
    payload = Payload(trees=[Tree.from_path(source)], report=Blob.from_path(source / "run.sh"))
    body = payload.model_dump_json(context=writer)
    assert str(source) not in body
    first = Payload.model_validate_json(body, context=ArtifactFiles(store, tmp_path / "first"))
    second = Payload.model_validate_json(body, context=ArtifactFiles(store, tmp_path / "second"))
    root = first.trees[0].path()
    assert (root / "empty").is_dir()
    assert (root / "run.sh").stat().st_mode & 0o777 == 0o755
    assert Tree.from_path(root) == payload.trees[0]
    assert first.trees[0] == second.trees[0]
    assert hash(first.trees[0]) == hash(second.trees[0])
    (root / "run.sh").write_text("changed")
    first.report.path().write_text("also changed")
    assert (second.trees[0].path() / "run.sh").read_bytes() == b"#!/bin/sh\necho hello\n"
    assert second.report.path().read_bytes() == b"#!/bin/sh\necho hello\n"
    # Forwarding handles ignores scratch mutations; explicit snapshots publish them.
    assert first.model_dump_json(context=writer) == body
    updated = Tree.from_path(root)
    assert updated.hash != first.trees[0].hash
    updated_body = updated.model_dump_json(context=writer)
    restored = Tree.model_validate_json(updated_body, context=writer)
    assert (restored.path() / "run.sh").read_text() == "changed"


def test_snapshot_identity_ignores_creation_order_mtime_and_nonexec_modes(tmp_path: Path) -> None:
    for name, order in [("a", ["z", "a"]), ("b", ["a", "z"])]:
        root = tmp_path / name
        root.mkdir()
        for entry in order:
            (root / entry).write_text(entry)
            (root / entry).chmod(0o600 if name == "a" else 0o644)
    assert Tree.from_path(tmp_path / "a") == Tree.from_path(tmp_path / "b")
    (tmp_path / "b" / "a").chmod(0o700)
    assert Tree.from_path(tmp_path / "a") != Tree.from_path(tmp_path / "b")


def test_empty_tree_roundtrips(tmp_path: Path) -> None:
    root = tmp_path / "empty"
    root.mkdir()
    files = ArtifactFiles(LocalDatastore(tmp_path / "store"), tmp_path / "scratch")
    tree = Tree.from_path(root)
    body = tree.model_dump_json(context=files)
    assert list(Tree.model_validate_json(body, context=files).path().iterdir()) == []
    assert tree.hash == sha256_ref(b'{"directories":[],"files":[],"version":1}')


@pytest.mark.parametrize("directory", [False, True])
def test_tree_rejects_symlinks(tmp_path: Path, directory: bool) -> None:
    root = tmp_path / "root"
    root.mkdir()
    outside = tmp_path / "outside"
    outside.mkdir() if directory else outside.write_text("private")
    (root / "link").symlink_to(outside, target_is_directory=directory)
    with pytest.raises(ValueError, match="regular files or directories"):
        Tree.from_path(root)


def test_blob_rejects_changed_source_before_publication(tmp_path: Path) -> None:
    source = tmp_path / "file"
    source.write_text("before")
    blob = Blob.from_path(source)
    source.write_text("after")
    files = ArtifactFiles(LocalDatastore(tmp_path / "store"), tmp_path / "scratch")
    with pytest.raises(PydanticSerializationError, match="source changed"):
        blob.model_dump_json(context=files)


def test_unbound_references_are_inert_and_unknown_versions_fail(tmp_path: Path) -> None:
    source = tmp_path / "file"
    source.write_text("hello")
    reference = Blob.from_path(source).model_dump(mode="json")
    with pytest.raises(ValueError, match="binding"):
        Blob.model_validate(reference).path()
    reference["kind"] = "massive.blob.v0"
    with pytest.raises(ValidationError):
        Blob.model_validate(reference)


def test_corrupt_body_fails_before_hydration(tmp_path: Path) -> None:
    source = tmp_path / "file"
    source.write_text("hello")
    store = LocalDatastore(tmp_path / "store")
    files = ArtifactFiles(store, tmp_path / "scratch")
    body = Blob.from_path(source).model_dump_json(context=files)
    blob = Blob.model_validate_json(body, context=files)
    key = f"file-artifacts/blobs/sha256/{blob.hash.removeprefix('sha256:')}"
    store.put(key, b"wrong", content_type="application/octet-stream")
    with pytest.raises(ArtifactIntegrityError):
        blob.path()
    assert not (tmp_path / "scratch").exists()


@pytest.mark.parametrize("path", ["../escape", "/absolute", "a/../escape", "a//b", "a\\b", "."])
def test_tree_manifest_rejects_unsafe_paths_before_writing(tmp_path: Path, path: str) -> None:
    store = LocalDatastore(tmp_path / "store")
    body = canonical_json({"version": 1, "directories": [path], "files": []}).encode()
    digest = sha256_ref(body)
    store.put(
        f"file-artifacts/trees/sha256/{digest[7:]}",
        body,
        content_type="application/vnd.massive.tree+json",
    )
    tree = Tree.model_validate(
        {"kind": "massive.tree.v1", "hash": digest, "size": len(body)},
        context=ArtifactFiles(store, tmp_path / "scratch"),
    )
    with pytest.raises(ValidationError):
        tree.path()
    assert not (tmp_path / "scratch").exists()


def test_handle_schema_matches_wire_value(tmp_path: Path) -> None:
    from jsonschema import Draft202012Validator

    path = tmp_path / "file"
    path.write_text("value")
    adapter = TypeAdapter(Blob)
    Draft202012Validator(adapter.json_schema()).validate(
        json.loads(adapter.dump_json(Blob.from_path(path)))
    )


def test_rebinding_a_handle_does_not_reuse_or_mutate_prior_scratch(tmp_path: Path) -> None:
    path = tmp_path / "source"
    path.write_text("original")
    store = LocalDatastore(tmp_path / "store")
    a = ArtifactFiles(store, tmp_path / "a")
    b = ArtifactFiles(store, tmp_path / "b")
    body = Blob.from_path(path).model_dump_json(context=a)
    first = Blob.model_validate_json(body, context=a)
    first.path().write_text("changed")
    rebound = Blob.model_validate(first, context=b)
    assert rebound is not first
    assert first.path().read_text() == "changed"
    assert rebound.path().read_text() == "original"


def test_tree_path_order_uses_manifest_strings(tmp_path: Path) -> None:
    # Path ordering compares components, which differs from wire string ordering
    # for punctuation adjacent to a directory separator.
    (tmp_path / "a" / "x").mkdir(parents=True)
    (tmp_path / "a-").mkdir()
    (tmp_path / "a" / "x" / "f").write_text("nested")
    (tmp_path / "a.txt").write_text("root")
    tree = Tree.from_path(tmp_path)
    assert tree.size > 0


@pytest.mark.parametrize(
    "manifest",
    [
        {"version": True, "directories": [], "files": []},
        {"version": 2, "directories": [], "files": []},
        {"version": 1, "directories": ["a", "a"], "files": []},
        {"version": 1, "directories": ["a/b"], "files": []},
        {"version": 1, "directories": ["z", "a"], "files": []},
    ],
)
def test_tree_manifest_rejects_invalid_version_and_structure(tmp_path: Path, manifest) -> None:
    store = LocalDatastore(tmp_path / "store")
    body = canonical_json(manifest).encode()
    digest = sha256_ref(body)
    store.put(
        f"file-artifacts/trees/sha256/{digest[7:]}",
        body,
        content_type="application/vnd.massive.tree+json",
    )
    tree = Tree.model_validate(
        {"kind": "massive.tree.v1", "hash": digest, "size": len(body)},
        context=ArtifactFiles(store, tmp_path / "scratch"),
    )
    with pytest.raises(ValidationError):
        tree.path()
    assert not (tmp_path / "scratch").exists()


def test_unreadable_directory_cannot_silently_disappear_from_snapshot(tmp_path: Path) -> None:
    import os

    if os.geteuid() == 0:
        pytest.skip("root bypasses directory read permissions")
    directory = tmp_path / "restricted"
    directory.mkdir()
    (directory / "file").write_text("must not be omitted")
    directory.chmod(0)
    try:
        with pytest.raises(PermissionError):
            Tree.from_path(tmp_path)
    finally:
        directory.chmod(0o755)
