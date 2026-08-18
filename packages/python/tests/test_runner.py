from __future__ import annotations

import json
import os
import subprocess
import sys
import tarfile
import uuid
from hashlib import sha256
from io import BytesIO
from pathlib import Path
from typing import Any, cast

import boto3
import pytest

from massive import canonical_json, sha256_ref
from massive.artifact import ArtifactRuntime, Destination, Producer
from massive.canonical import JsonValue
from massive.datastore import LocalDatastore


@pytest.mark.parametrize(
    ("export", "expected"), [("double", {"value": 42}), ("increment", {"value": 22})]
)
def test_runner_executes_sync_and_async_python_steps_via_descriptor(
    tmp_path: Path, export: str, expected: dict[str, int]
) -> None:
    descriptor_path, descriptor, store = _descriptor(tmp_path, export=export)

    result = _run(descriptor_path)

    assert result.returncode == 0, result.stderr
    output = descriptor["output"]
    publication, body = ArtifactRuntime(LocalDatastore(store)).resolve_json(
        Destination(manifest_key=output["manifestKey"], schema=output["schema"]),
        Producer(
            project_key=descriptor["projectKey"],
            plan_hash=descriptor["planHash"],
            run_id=descriptor["runId"],
            node_id=descriptor["nodeId"],
            attempt=descriptor["attempt"],
        ),
    )
    assert body == canonical_json(cast(JsonValue, expected)).encode()
    assert publication.manifest.content_type == "application/vnd.massive.data-artifact-manifest+json"
    assert publication.body.content_type == "application/json"


@pytest.mark.parametrize(
    ("export", "input_value", "expected_exit"),
    [
        ("double", {"value": "not-an-integer"}, 65),
        ("invalid_output", {"value": 21}, 65),
        ("explode", {"value": 21}, 66),
    ],
)
def test_runner_uses_protocol_exit_codes_for_schema_and_step_failures(
    tmp_path: Path, export: str, input_value: dict[str, object], expected_exit: int
) -> None:
    descriptor_path, _descriptor_value, _store = _descriptor(
        tmp_path, export=export, input_value=input_value
    )

    result = _run(descriptor_path)

    assert result.returncode == expected_exit


def test_runner_uses_descriptor_exit_code_for_invalid_descriptor(tmp_path: Path) -> None:
    descriptor_path = tmp_path / "descriptor.json"
    descriptor_path.write_text("{}")

    result = _run(descriptor_path)

    assert result.returncode == 64


def test_runner_reports_malformed_schema_as_schema_failure(tmp_path: Path) -> None:
    descriptor_path, descriptor, store = _descriptor(tmp_path, export="double")
    schema_ref = descriptor["input"]["schema"]
    _write(store, f"blobs/sha256/{schema_ref.removeprefix('sha256:')}", "{")

    result = _run(descriptor_path)

    assert result.returncode == 65


def test_runner_reports_an_invalid_immutable_output_slot_as_schema_failure(
    tmp_path: Path,
) -> None:
    descriptor_path, descriptor, _store = _descriptor(tmp_path, export="double")
    descriptor["output"]["manifestKey"] = (
        "projects/project/runs/python-runner-test/steps/other/1/output-manifest.json"
    )
    descriptor_path.write_text(canonical_json(cast(JsonValue, descriptor)))

    result = _run(descriptor_path)

    assert result.returncode == 65


def test_runner_rejects_a_verified_traversal_archive(tmp_path: Path) -> None:
    descriptor_path, descriptor, store = _descriptor(tmp_path, export="double")
    body = _archive_entry("../escape.py", b"raise RuntimeError('unsafe')\n")
    archive = descriptor["sourcePackage"]["sourceArchive"]
    archive["hash"] = "sha256:" + sha256(body).hexdigest()
    path = store / archive["key"]
    path.write_bytes(body)
    descriptor_path.write_text(canonical_json(descriptor))

    result = _run(descriptor_path)

    assert result.returncode == 64
    assert not (tmp_path / "escape.py").exists()


@pytest.mark.skipif(
    not os.environ.get("MASSIVE_TEST_S3_ENDPOINT"),
    reason="requires a configured MinIO/S3 endpoint",
)
def test_runner_executes_against_a_real_s3_descriptor(tmp_path: Path) -> None:
    endpoint = os.environ["MASSIVE_TEST_S3_ENDPOINT"]
    access_key = os.environ.get("MASSIVE_TEST_S3_ACCESS_KEY")
    secret_key = os.environ.get("MASSIVE_TEST_S3_SECRET_KEY")
    if access_key is None or secret_key is None:
        pytest.skip("MASSIVE_TEST_S3_ENDPOINT requires test access credentials")
    descriptor_path, descriptor, store = _descriptor(tmp_path, export="double")
    bucket = f"massive-python-{uuid.uuid4().hex}"
    client = boto3.client(
        "s3", endpoint_url=endpoint, region_name="us-east-1", aws_access_key_id=access_key,
        aws_secret_access_key=secret_key,
    )
    client.create_bucket(Bucket=bucket)
    for path in store.rglob("*"):
        if path.is_file() and ".massive-datastore-metadata" not in path.parts:
            client.put_object(Bucket=bucket, Key=str(path.relative_to(store)), Body=path.read_bytes())
    descriptor["datastore"] = {
        "kind": "s3", "bucket": bucket, "region": "us-east-1", "endpoint": endpoint,
        "forcePathStyle": True,
    }
    serialized = canonical_json(cast(JsonValue, descriptor))
    assert access_key not in serialized
    assert secret_key not in serialized
    descriptor_path.write_text(serialized)
    environment = {**os.environ, "AWS_ACCESS_KEY_ID": access_key, "AWS_SECRET_ACCESS_KEY": secret_key}

    result = _run(descriptor_path, environment)

    assert result.returncode == 0, result.stderr
    manifest = json.loads(
        client.get_object(Bucket=bucket, Key=descriptor["output"]["manifestKey"])["Body"].read()
    )
    output = client.get_object(Bucket=bucket, Key=manifest["body"]["key"])["Body"].read()
    assert manifest["kind"] == "DataArtifactManifest"
    assert output == canonical_json(cast(JsonValue, {"value": 42})).encode()


def _descriptor(
    tmp_path: Path, *, export: str, input_value: dict[str, object] | None = None
) -> tuple[Path, dict[str, Any], Path]:
    source_root = Path(__file__).parent / "fixtures"
    store = tmp_path / "store"
    schema = {
        "type": "object",
        "additionalProperties": False,
        "required": ["value"],
        "properties": {"value": {"type": "integer"}},
    }
    schema_text = canonical_json(schema)
    schema_hash = sha256_ref(schema_text)
    input_text = canonical_json(cast(JsonValue, input_value or {"value": 21}))
    archive_body = _source_archive(source_root)
    package_hash = "sha256:" + "d" * 64
    descriptor: dict[str, Any] = {
        "kind": "StepInvocationDescriptor",
        "schemaVersion": 1,
        "encoding": "json-v1",
        "planHash": "sha256:" + "a" * 64,
        "projectKey": "project",
        "runId": "python-runner-test",
        "nodeId": export,
        "attempt": 1,
        "symbol": {
            "packageId": "python-main",
            "language": "python",
            "module": "runner_workflow",
            "export": export,
        },
        "sourcePackage": {
            "packageId": "python-main",
            "language": "python",
            "packageHash": package_hash,
            "sourceArchive": {
                "key": f"packages/{package_hash.replace(':', '-')}/source.tar",
                "hash": "sha256:" + sha256(archive_body).hexdigest(),
                "contentType": "application/vnd.massive.source-tar",
            },
        },
        "environmentRef": "sha256:" + "b" * 64,
        "input": {
            "artifact": {
                "key": "runs/python-runner-test/inputs/task.json",
                "hash": sha256_ref(input_text),
                "contentType": "application/json",
            },
            "schema": schema_hash,
        },
        "output": {
            "manifestKey": f"projects/project/runs/python-runner-test/steps/{export}/1/output-manifest.json",
            "schema": schema_hash,
        },
        "datastore": {"kind": "local", "path": str(store)},
    }
    archive_path = store / descriptor["sourcePackage"]["sourceArchive"]["key"]
    archive_path.parent.mkdir(parents=True, exist_ok=True)
    archive_path.write_bytes(archive_body)
    _write(store, f"blobs/sha256/{schema_hash.removeprefix('sha256:')}", schema_text)
    _write(store, descriptor["input"]["artifact"]["key"], input_text)
    descriptor_path = tmp_path / "descriptor.json"
    descriptor_path.write_text(canonical_json(descriptor))
    return descriptor_path, descriptor, store


def _run(descriptor_path: Path, environment: dict[str, str] | None = None) -> subprocess.CompletedProcess[str]:
    repository = Path(__file__).resolve().parents[3]
    return subprocess.run(
        [sys.executable, "-m", "massive.runner", str(descriptor_path)],
        cwd=repository,
        check=False,
        capture_output=True,
        env=environment,
        text=True,
    )


def _write(root: Path, key: str, body: str) -> None:
    path = root / key
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(body)


def _source_archive(source_root: Path, entries: tuple[str, ...] = ("runner_workflow.py",)) -> bytes:
    buffer = BytesIO()
    with tarfile.open(fileobj=buffer, mode="w", format=tarfile.USTAR_FORMAT) as archive:
        for name in entries:
            body = (source_root / name).read_bytes()
            info = tarfile.TarInfo(name)
            info.mode = 0o644
            info.size = len(body)
            info.mtime = 0
            archive.addfile(info, BytesIO(body))
    return buffer.getvalue()


def _archive_entry(name: str, body: bytes) -> bytes:
    buffer = BytesIO()
    with tarfile.open(fileobj=buffer, mode="w", format=tarfile.USTAR_FORMAT) as archive:
        info = tarfile.TarInfo(name)
        info.mode = 0o644
        info.size = len(body)
        info.mtime = 0
        archive.addfile(info, BytesIO(body))
    return buffer.getvalue()
