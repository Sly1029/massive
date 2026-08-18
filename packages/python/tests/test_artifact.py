from __future__ import annotations

import json
import os
import uuid
from collections.abc import Generator
from concurrent.futures import ThreadPoolExecutor
from contextlib import contextmanager
from pathlib import Path

import boto3
import pytest

from massive.artifact import (
    ArtifactBodyConflictError,
    ArtifactIntegrityError,
    ArtifactManifestConflictError,
    ArtifactNotFoundError,
    ArtifactRuntime,
    ArtifactValidationError,
    Destination,
    Producer,
)
from massive.canonical import sha256_ref
from massive.datastore import DatastoreConflictError, LocalDatastore, S3Datastore

BODY = b'{"value":42}'
BODY_HASH = "sha256:dc60e632a90329ccfd34fbe904d94704dbbb6669575185e26389854ff64139c3"
SCHEMA = b'{"additionalProperties":false,"properties":{"value":{"type":"integer"}},"required":["value"],"type":"object"}'
SCHEMA_HASH = "sha256:cc6d2156c280bb3efad77622be3c070cf9a18fbf7ddaf4db6a7c6988a417048a"
PLAN_HASH = "sha256:" + "a" * 64
PROJECT_KEY = "sha256-" + "b" * 64


def test_publish_resolve_and_retry_use_the_go_compatible_manifest(tmp_path: Path) -> None:
    runtime, store = _runtime(tmp_path)

    first = runtime.publish_json(_destination(), _producer(), BODY)
    second = runtime.publish_json(_destination(), _producer(), BODY)
    publication, resolved = runtime.resolve_json(_destination(), _producer())

    assert first == second == publication
    assert resolved == BODY
    assert first.body.hash == BODY_HASH
    assert store.get(_destination().manifest_key).body == (
        b'{"body":{"contentType":"application/json","hash":"sha256:dc60e632a90329ccfd34fbe904d94704dbbb6669575185e26389854ff64139c3","key":"blobs/sha256/dc60e632a90329ccfd34fbe904d94704dbbb6669575185e26389854ff64139c3","size":12},"encoding":"canonical-json-v0","kind":"DataArtifactManifest","producer":{"attempt":1,"nodeId":"task","planHash":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","projectKey":"sha256-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","runId":"run-1"},"schema":"sha256:cc6d2156c280bb3efad77622be3c070cf9a18fbf7ddaf4db6a7c6988a417048a","schemaVersion":0}'
    )


def test_publish_completes_a_body_only_interrupted_publication(tmp_path: Path) -> None:
    runtime, store = _runtime(tmp_path)
    store.put(
        "blobs/sha256/dc60e632a90329ccfd34fbe904d94704dbbb6669575185e26389854ff64139c3",
        BODY,
        content_type="application/json",
        if_absent=True,
    )

    publication = runtime.publish_json(_destination(), _producer(), BODY)

    assert publication.body.hash == BODY_HASH
    assert store.get(_destination().manifest_key).body


def test_concurrent_identical_publications_converge_on_one_artifact(tmp_path: Path) -> None:
    runtime, _store = _runtime(tmp_path)

    with ThreadPoolExecutor(max_workers=8) as executor:
        publications = list(
            executor.map(
                lambda _ignored: runtime.publish_json(_destination(), _producer(), BODY), range(8)
            )
        )

    assert publications == [publications[0]] * 8
    _publication, resolved = runtime.resolve_json(_destination(), _producer())
    assert resolved == BODY


def test_concurrent_different_outputs_have_one_manifest_winner_without_overwrite(
    tmp_path: Path,
) -> None:
    runtime, _store = _runtime(tmp_path)
    candidates = (BODY, b'{"value":43}')

    with ThreadPoolExecutor(max_workers=2) as executor:
        futures = [
            executor.submit(runtime.publish_json, _destination(), _producer(), candidate)
            for candidate in candidates
        ]
    results = [future.exception() for future in futures]

    assert sum(result is None for result in results) == 1
    assert sum(isinstance(result, ArtifactManifestConflictError) for result in results) == 1
    _publication, resolved = runtime.resolve_json(_destination(), _producer())
    assert resolved in candidates


def test_local_conditional_put_keeps_the_first_content_type_during_a_race(tmp_path: Path) -> None:
    store = LocalDatastore(tmp_path / "store")
    key = "objects/race.json"
    body = b'{"value":42}'

    def put(content_type: str) -> str:
        try:
            store.put(key, body, content_type=content_type, if_absent=True)
            return content_type
        except DatastoreConflictError:
            return "conflict"

    with ThreadPoolExecutor(max_workers=2) as executor:
        results = list(executor.map(put, ("application/json", "application/x-race")))

    winners = [result for result in results if result != "conflict"]
    assert len(winners) == 1
    object_ = store.get(key)
    assert object_.body == body
    assert object_.info.content_type == winners[0]


def test_local_conditional_put_never_overwrites_a_same_type_race(tmp_path: Path) -> None:
    store = LocalDatastore(tmp_path / "store")
    key = "objects/same-type-race.json"
    candidates = (b'{"value":42}', b'{"value":43}')

    def put(body: bytes) -> bytes | None:
        try:
            store.put(key, body, content_type="application/json", if_absent=True)
            return body
        except DatastoreConflictError:
            return None

    with ThreadPoolExecutor(max_workers=2) as executor:
        results = list(executor.map(put, candidates))

    winners = [result for result in results if result is not None]
    assert len(winners) == 1
    assert store.get(key).body == winners[0]


def test_local_conditional_put_recovers_a_metadata_only_crash_state(tmp_path: Path) -> None:
    store = LocalDatastore(tmp_path / "store")
    key = "objects/recover.json"
    metadata = store.root / ".massive-datastore-metadata" / f"{sha256_ref(key).removeprefix('sha256:')}.json"
    metadata.parent.mkdir(parents=True)
    metadata.write_text('{"contentType":"application/json"}')

    store.put(key, b'{"recovered":true}', content_type="application/json", if_absent=True)

    object_ = store.get(key)
    assert object_.body == b'{"recovered":true}'
    assert object_.info.content_type == "application/json"


def test_publish_rejects_a_body_collision_and_an_existing_different_manifest(
    tmp_path: Path,
) -> None:
    runtime, store = _runtime(tmp_path)
    body_path = store.path_for_key(
        "blobs/sha256/dc60e632a90329ccfd34fbe904d94704dbbb6669575185e26389854ff64139c3"
    )
    body_path.parent.mkdir(parents=True, exist_ok=True)
    body_path.write_bytes(b'{"value":0}')

    with pytest.raises(ArtifactBodyConflictError):
        runtime.publish_json(_destination(), _producer(), BODY)

    clean_runtime, _clean_store = _runtime(tmp_path / "manifest")
    clean_runtime.publish_json(_destination(), _producer(), BODY)

    with pytest.raises(ArtifactManifestConflictError):
        clean_runtime.publish_json(_destination(), _producer(), b'{"value":43}')


@pytest.mark.parametrize("tamper", ["manifest", "body", "content-type"])
def test_resolve_rejects_tampered_publications(tmp_path: Path, tamper: str) -> None:
    runtime, store = _runtime(tmp_path)
    runtime.publish_json(_destination(), _producer(), BODY)

    if tamper == "manifest":
        store.path_for_key(_destination().manifest_key).write_bytes(b"{}")
    elif tamper == "body":
        store.path_for_key(
            "blobs/sha256/dc60e632a90329ccfd34fbe904d94704dbbb6669575185e26389854ff64139c3"
        ).write_bytes(b'{"value":0}')
    else:
        _write_content_type(store, _destination().manifest_key, "application/json")

    with pytest.raises(ArtifactIntegrityError):
        runtime.resolve_json(_destination(), _producer())


def test_resolve_reports_a_missing_manifest_separately_from_integrity_failures(tmp_path: Path) -> None:
    runtime, _store = _runtime(tmp_path)

    with pytest.raises(ArtifactNotFoundError):
        runtime.resolve_json(_destination(), _producer())


def test_publish_requires_a_normalized_project_key(tmp_path: Path) -> None:
    runtime, _store = _runtime(tmp_path)
    destination = Destination(
        manifest_key="projects/project/runs/run-1/steps/task/1/output-manifest.json",
        schema=SCHEMA_HASH,
    )
    producer = Producer(
        project_key="project",
        plan_hash=PLAN_HASH,
        run_id="run-1",
        node_id="task",
        attempt=1,
    )

    with pytest.raises(ArtifactValidationError, match="normalized SHA-256 identity"):
        runtime.publish_json(destination, producer, BODY)


def test_publish_reports_an_invalid_json_schema_as_an_artifact_validation_error(
    tmp_path: Path,
) -> None:
    runtime, store = _runtime(tmp_path)
    invalid_schema = b'{"type":"not-a-json-schema-type"}'
    invalid_schema_hash = sha256_ref(invalid_schema)
    store.put(
        f"blobs/sha256/{invalid_schema_hash.removeprefix('sha256:')}",
        invalid_schema,
        content_type="application/json",
        if_absent=True,
    )
    destination = Destination(
        manifest_key=f"projects/{PROJECT_KEY}/runs/run-1/steps/task/1/output-manifest.json",
        schema=invalid_schema_hash,
    )

    with pytest.raises(ArtifactValidationError, match="does not satisfy schema"):
        runtime.publish_json(destination, _producer(), BODY)


def test_immutable_conflict_preserves_an_unrelated_datastore_read_failure(tmp_path: Path) -> None:
    runtime, store = _runtime(tmp_path)
    store.put(
        "blobs/sha256/dc60e632a90329ccfd34fbe904d94704dbbb6669575185e26389854ff64139c3",
        BODY,
        content_type="application/json",
        if_absent=True,
    )
    metadata = (
        store.root
        / ".massive-datastore-metadata"
        / f"{sha256_ref('blobs/sha256/dc60e632a90329ccfd34fbe904d94704dbbb6669575185e26389854ff64139c3').removeprefix('sha256:')}.json"
    )
    metadata.write_text("not JSON")

    with pytest.raises(json.JSONDecodeError):
        runtime.publish_json(_destination(), _producer(), BODY)


@pytest.mark.skipif(
    not os.environ.get("MASSIVE_TEST_S3_ENDPOINT"),
    reason="requires a configured MinIO/S3 endpoint",
)
def test_publish_and_resolve_against_a_real_s3_store() -> None:
    endpoint = os.environ["MASSIVE_TEST_S3_ENDPOINT"]
    access_key = os.environ.get("MASSIVE_TEST_S3_ACCESS_KEY")
    secret_key = os.environ.get("MASSIVE_TEST_S3_SECRET_KEY")
    if access_key is None or secret_key is None:
        pytest.skip("MASSIVE_TEST_S3_ENDPOINT requires test access credentials")
    bucket = f"massive-python-artifact-{uuid.uuid4().hex}"
    setup_client = boto3.client(
        "s3",
        endpoint_url=endpoint,
        region_name="us-east-1",
        aws_access_key_id=access_key,
        aws_secret_access_key=secret_key,
    )
    setup_client.create_bucket(Bucket=bucket)
    with _ambient_test_credentials(access_key, secret_key):
        store = S3Datastore(
            {
                "kind": "s3",
                "bucket": bucket,
                "region": "us-east-1",
                "endpoint": endpoint,
                "forcePathStyle": True,
            }
        )
        store.put(
            f"blobs/sha256/{SCHEMA_HASH.removeprefix('sha256:')}",
            SCHEMA,
            content_type="application/json",
            if_absent=True,
        )
        runtime = ArtifactRuntime(store)

        published = runtime.publish_json(_destination(), _producer(), BODY)
        resolved, body = runtime.resolve_json(_destination(), _producer())

    assert published == resolved
    assert body == BODY


@pytest.mark.skipif(
    not os.environ.get("MASSIVE_TEST_S3_ENDPOINT"),
    reason="requires a configured MinIO/S3 endpoint",
)
def test_concurrent_publication_against_a_real_s3_store() -> None:
    endpoint = os.environ["MASSIVE_TEST_S3_ENDPOINT"]
    access_key = os.environ.get("MASSIVE_TEST_S3_ACCESS_KEY")
    secret_key = os.environ.get("MASSIVE_TEST_S3_SECRET_KEY")
    if access_key is None or secret_key is None:
        pytest.skip("MASSIVE_TEST_S3_ENDPOINT requires test access credentials")
    bucket = f"massive-python-artifact-race-{uuid.uuid4().hex}"
    setup_client = boto3.client(
        "s3",
        endpoint_url=endpoint,
        region_name="us-east-1",
        aws_access_key_id=access_key,
        aws_secret_access_key=secret_key,
    )
    setup_client.create_bucket(Bucket=bucket)
    with _ambient_test_credentials(access_key, secret_key):
        store = S3Datastore(
            {
                "kind": "s3",
                "bucket": bucket,
                "region": "us-east-1",
                "endpoint": endpoint,
                "forcePathStyle": True,
            }
        )
        store.put(
            f"blobs/sha256/{SCHEMA_HASH.removeprefix('sha256:')}",
            SCHEMA,
            content_type="application/json",
            if_absent=True,
        )
        runtime = ArtifactRuntime(store)
        with ThreadPoolExecutor(max_workers=8) as executor:
            publications = list(
                executor.map(
                    lambda _ignored: runtime.publish_json(_destination(), _producer(), BODY),
                    range(8),
                )
            )

        assert publications == [publications[0]] * 8

        race_destination = _destination(run_id="race")
        race_producer = _producer(run_id="race")
        with ThreadPoolExecutor(max_workers=2) as executor:
            futures = [
                executor.submit(runtime.publish_json, race_destination, race_producer, candidate)
                for candidate in (BODY, b'{"value":43}')
            ]
        results = [future.exception() for future in futures]

    assert sum(result is None for result in results) == 1
    assert sum(isinstance(result, ArtifactManifestConflictError) for result in results) == 1


def _runtime(tmp_path: Path) -> tuple[ArtifactRuntime, LocalDatastore]:
    store = LocalDatastore(tmp_path / "store")
    store.put(
        f"blobs/sha256/{SCHEMA_HASH.removeprefix('sha256:')}",
        SCHEMA,
        content_type="application/json",
        if_absent=True,
    )
    return ArtifactRuntime(store), store


def _destination(run_id: str = "run-1") -> Destination:
    return Destination(
        manifest_key=f"projects/{PROJECT_KEY}/runs/{run_id}/steps/task/1/output-manifest.json",
        schema=SCHEMA_HASH,
    )


def _producer(run_id: str = "run-1") -> Producer:
    return Producer(
        project_key=PROJECT_KEY,
        plan_hash=PLAN_HASH,
        run_id=run_id,
        node_id="task",
        attempt=1,
    )


def _write_content_type(store: LocalDatastore, key: str, content_type: str) -> None:
    metadata = (
        store.root
        / ".massive-datastore-metadata"
        / f"{sha256_ref(key).removeprefix('sha256:')}.json"
    )
    metadata.write_text(json.dumps({"contentType": content_type}))


@contextmanager
def _ambient_test_credentials(access_key: str, secret_key: str) -> Generator[None, None, None]:
    names = ("AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN")
    previous = {name: os.environ.get(name) for name in names}
    os.environ["AWS_ACCESS_KEY_ID"] = access_key
    os.environ["AWS_SECRET_ACCESS_KEY"] = secret_key
    os.environ.pop("AWS_SESSION_TOKEN", None)
    try:
        yield
    finally:
        for name, value in previous.items():
            if value is None:
                os.environ.pop(name, None)
            else:
                os.environ[name] = value
