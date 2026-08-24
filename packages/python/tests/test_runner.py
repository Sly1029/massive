from __future__ import annotations

import json
import os
import subprocess
import sys
import tarfile
import uuid
from decimal import Decimal
from hashlib import sha256
from io import BytesIO
from pathlib import Path
from typing import Any, cast

import boto3
import pytest
from pydantic import BaseModel, TypeAdapter

from massive import canonical_json, sha256_ref
from massive.artifact import ArtifactRuntime, Destination, Producer
from massive.canonical import JsonValue
from massive.datastore import LocalDatastore

PROJECT_KEY = "sha256-" + "b" * 64


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
        Destination(manifest_key=output["manifestKey"], schema_ref=output["schema"]),
        Producer(
            project_key=descriptor["projectKey"],
            plan_hash=descriptor["planHash"],
            run_id=descriptor["runId"],
            node_id=descriptor["nodeId"],
            attempt=descriptor["attempt"],
        ),
    )
    assert body == canonical_json(cast(JsonValue, expected)).encode()
    assert (
        publication.manifest.content_type == "application/vnd.massive.data-artifact-manifest+json"
    )
    assert publication.body.content_type == "application/json"


@pytest.mark.parametrize(
    ("export", "scope", "attempt", "expected_key"),
    [
        (
            "capture_sync_invocation",
            {"frames": [{"kind": "map-item", "mapId": "fanout", "index": 0}]},
            1,
            "massive-invocation-v1/python-runner-test/capture_sync_invocation/scope/maps/fanout/items/0/attempt/1",
        ),
        (
            "capture_sync_invocation",
            {"frames": [{"kind": "map-item", "mapId": "fanout", "index": 1}]},
            1,
            "massive-invocation-v1/python-runner-test/capture_sync_invocation/scope/maps/fanout/items/1/attempt/1",
        ),
        (
            "capture_async_invocation",
            {
                "frames": [
                    {"kind": "map-item", "mapId": "outer", "index": 0},
                    {"kind": "map-item", "mapId": "inner", "index": 3},
                ]
            },
            2,
            "massive-invocation-v1/python-runner-test/capture_async_invocation/scope/maps/outer/items/0/maps/inner/items/3/attempt/2",
        ),
        (
            "capture_async_invocation",
            {
                "frames": [
                    {"kind": "map-item", "mapId": "inner", "index": 3},
                    {"kind": "map-item", "mapId": "outer", "index": 0},
                ]
            },
            2,
            "massive-invocation-v1/python-runner-test/capture_async_invocation/scope/maps/inner/items/3/maps/outer/items/0/attempt/2",
        ),
    ],
)
def test_runner_exposes_collision_free_scoped_idempotency_keys(
    tmp_path: Path, export: str, scope: dict[str, object], attempt: int, expected_key: str
) -> None:
    output_schema = {
        "type": "object",
        "additionalProperties": False,
        "required": ["idempotency_key"],
        "properties": {"idempotency_key": {"type": "string"}},
    }
    descriptor_path, descriptor, store = _descriptor(
        tmp_path, export=export, output_schema=output_schema
    )
    descriptor["scope"] = scope
    descriptor["attempt"] = attempt
    scope_path = "/scopes" + "".join(
        f"/maps/{frame['mapId']}/items/{frame['index']}" for frame in scope["frames"]
    )
    descriptor["output"]["manifestKey"] = (
        f"projects/{PROJECT_KEY}/runs/python-runner-test/steps/{export}{scope_path}/"
        f"{attempt}/output-manifest.json"
    )
    descriptor_path.write_text(canonical_json(cast(JsonValue, descriptor)))

    result = _run(descriptor_path)

    assert result.returncode == 0, result.stderr
    publication, body = ArtifactRuntime(LocalDatastore(store)).resolve_json(
        Destination(
            manifest_key=descriptor["output"]["manifestKey"],
            schema_ref=descriptor["output"]["schema"],
        ),
        Producer.model_validate(
            {
                "projectKey": descriptor["projectKey"],
                "planHash": descriptor["planHash"],
                "runId": descriptor["runId"],
                "nodeId": descriptor["nodeId"],
                "attempt": descriptor["attempt"],
                "scope": descriptor["scope"],
            }
        ),
    )
    assert publication.manifest.key == descriptor["output"]["manifestKey"]
    assert body == canonical_json({"idempotency_key": expected_key}).encode()


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


@pytest.mark.parametrize("target", ["input", "schema"])
def test_runner_reports_a_missing_required_input_object_as_schema_failure(
    tmp_path: Path, target: str
) -> None:
    descriptor_path, descriptor, store = _descriptor(tmp_path, export="double")
    if target == "input":
        key = descriptor["input"]["artifact"]["key"]
    else:
        schema_ref = descriptor["input"]["schema"]
        key = f"blobs/sha256/{schema_ref.removeprefix('sha256:')}"
    store.joinpath(key).unlink()

    result = _run(descriptor_path)

    assert result.returncode == 65


@pytest.mark.parametrize("body", [b"\x80", b"1.5", b'{"value":21 }'])
def test_runner_reports_invalid_utf8_float_and_noncanonical_input_as_schema_failures(
    tmp_path: Path, body: bytes
) -> None:
    descriptor_path, descriptor, store = _descriptor(tmp_path, export="double")
    store.joinpath(descriptor["input"]["artifact"]["key"]).write_bytes(body)

    result = _run(descriptor_path)

    assert result.returncode == 65


@pytest.mark.parametrize("body", [b"\x80", b'{"type":"object" }'])
def test_runner_reports_invalid_utf8_and_noncanonical_schema_as_schema_failures(
    tmp_path: Path, body: bytes
) -> None:
    descriptor_path, descriptor, store = _descriptor(tmp_path, export="double")
    schema_ref = descriptor["input"]["schema"]
    store.joinpath(f"blobs/sha256/{schema_ref.removeprefix('sha256:')}").write_bytes(body)

    result = _run(descriptor_path)

    assert result.returncode == 65


class DecimalResult(BaseModel):
    value: Decimal


def test_runner_uses_serialized_decimal_output_as_valid_downstream_input(tmp_path: Path) -> None:
    output_schema = TypeAdapter(DecimalResult).json_schema(mode="serialization")
    input_schema = TypeAdapter(DecimalResult).json_schema(mode="validation")
    first_path, first, store = _descriptor(tmp_path, export="decimal_result")
    output_schema_text = canonical_json(cast(JsonValue, output_schema))
    output_schema_ref = sha256_ref(output_schema_text)
    _write(store, f"blobs/sha256/{output_schema_ref.removeprefix('sha256:')}", output_schema_text)
    first["output"]["schema"] = output_schema_ref
    first_path.write_text(canonical_json(cast(JsonValue, first)))

    first_result = _run(first_path)

    assert first_result.returncode == 0, first_result.stderr
    publication, first_body = ArtifactRuntime(LocalDatastore(store)).resolve_json(
        Destination(manifest_key=first["output"]["manifestKey"], schema_ref=output_schema_ref),
        Producer(
            project_key=first["projectKey"],
            plan_hash=first["planHash"],
            run_id=first["runId"],
            node_id=first["nodeId"],
            attempt=first["attempt"],
        ),
    )
    assert first_body == b'{"value":"10.5"}'

    validation_schema_text = canonical_json(cast(JsonValue, input_schema))
    validation_schema_ref = sha256_ref(validation_schema_text)
    _write(
        store,
        f"blobs/sha256/{validation_schema_ref.removeprefix('sha256:')}",
        validation_schema_text,
    )
    second_path, second, _unused_store = _descriptor(tmp_path / "second", export="decimal_echo")
    second["input"] = {
        "artifact": {
            "key": publication.body.key,
            "hash": publication.body.hash,
            "contentType": publication.body.content_type,
        },
        "schema": validation_schema_ref,
    }
    second["output"]["schema"] = output_schema_ref
    second["datastore"] = {"kind": "local", "path": str(store)}
    second_path.write_text(canonical_json(cast(JsonValue, second)))

    second_result = _run(second_path)

    assert second_result.returncode == 0, second_result.stderr


def test_runner_reports_an_invalid_immutable_output_slot_as_schema_failure(
    tmp_path: Path,
) -> None:
    # explode would exit 66 if the runner reached user code. Destination
    # binding is part of descriptor validation and must fail first.
    descriptor_path, descriptor, _store = _descriptor(tmp_path, export="explode")
    descriptor["output"]["manifestKey"] = (
        f"projects/{PROJECT_KEY}/runs/python-runner-test/steps/other/1/output-manifest.json"
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


def test_runner_executes_against_a_real_s3_descriptor(
    tmp_path: Path, s3_server: Any
) -> None:
    endpoint = s3_server.endpoint
    access_key = s3_server.access_key
    secret_key = s3_server.secret_key
    descriptor_path, descriptor, store = _descriptor(tmp_path, export="double")
    bucket = f"massive-python-{uuid.uuid4().hex}"
    client = boto3.client(
        "s3",
        endpoint_url=endpoint,
        region_name="us-east-1",
        aws_access_key_id=access_key,
        aws_secret_access_key=secret_key,
    )
    client.create_bucket(Bucket=bucket)
    for path in store.rglob("*"):
        if path.is_file() and ".massive-datastore-metadata" not in path.parts:
            client.put_object(
                Bucket=bucket, Key=str(path.relative_to(store)), Body=path.read_bytes()
            )
    descriptor["datastore"] = {
        "kind": "s3",
        "bucket": bucket,
        "region": "us-east-1",
        "endpoint": endpoint,
        "forcePathStyle": True,
    }
    serialized = canonical_json(cast(JsonValue, descriptor))
    assert access_key not in serialized
    assert secret_key not in serialized
    descriptor_path.write_text(serialized)
    environment = {
        **os.environ,
        "AWS_ACCESS_KEY_ID": access_key,
        "AWS_SECRET_ACCESS_KEY": secret_key,
    }

    result = _run(descriptor_path, environment)

    assert result.returncode == 0, result.stderr
    manifest = json.loads(
        client.get_object(Bucket=bucket, Key=descriptor["output"]["manifestKey"])["Body"].read()
    )
    output = client.get_object(Bucket=bucket, Key=manifest["body"]["key"])["Body"].read()
    assert manifest["kind"] == "DataArtifactManifest"
    assert output == canonical_json(cast(JsonValue, {"value": 42})).encode()


def _descriptor(
    tmp_path: Path,
    *,
    export: str,
    input_value: dict[str, object] | None = None,
    output_schema: dict[str, JsonValue] | None = None,
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
    output_schema_text = canonical_json(output_schema or schema)
    output_schema_hash = sha256_ref(output_schema_text)
    input_text = canonical_json(cast(JsonValue, input_value or {"value": 21}))
    archive_body = _source_archive(source_root)
    package_hash = "sha256:" + "d" * 64
    descriptor: dict[str, Any] = {
        "kind": "StepInvocationDescriptor",
        "schemaVersion": 2,
        "encoding": "json-v2",
        "planHash": "sha256:" + "a" * 64,
        "projectKey": PROJECT_KEY,
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
            "manifestKey": f"projects/{PROJECT_KEY}/runs/python-runner-test/steps/{export}/1/output-manifest.json",
            "schema": output_schema_hash,
        },
        "datastore": {"kind": "local", "path": str(store)},
    }
    archive_path = store / descriptor["sourcePackage"]["sourceArchive"]["key"]
    archive_path.parent.mkdir(parents=True, exist_ok=True)
    archive_path.write_bytes(archive_body)
    _write(store, f"blobs/sha256/{schema_hash.removeprefix('sha256:')}", schema_text)
    _write(store, f"blobs/sha256/{output_schema_hash.removeprefix('sha256:')}", output_schema_text)
    _write(store, descriptor["input"]["artifact"]["key"], input_text)
    descriptor_path = tmp_path / "descriptor.json"
    descriptor_path.write_text(canonical_json(descriptor))
    return descriptor_path, descriptor, store


def _run(
    descriptor_path: Path, environment: dict[str, str] | None = None
) -> subprocess.CompletedProcess[str]:
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
