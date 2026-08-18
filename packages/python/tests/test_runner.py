from __future__ import annotations

import subprocess
import sys
from hashlib import sha256
from pathlib import Path
from typing import Any

import pytest

from massive import canonical_json, sha256_ref


@pytest.mark.parametrize(
    ("export", "expected"), [("double", {"value": 42}), ("increment", {"value": 22})]
)
def test_runner_executes_sync_and_async_python_steps_via_descriptor(
    tmp_path: Path, export: str, expected: dict[str, int]
) -> None:
    descriptor_path, descriptor, store = _descriptor(tmp_path, export=export)

    result = _run(descriptor_path)

    assert result.returncode == 0, result.stderr
    output = (store / descriptor["output"]["artifact"]["key"]).read_text()
    assert output == canonical_json(expected)
    metadata = (
        store
        / ".massive-datastore-metadata"
        / f"{sha256(descriptor['output']['artifact']['key'].encode()).hexdigest()}.json"
    )
    assert metadata.read_text() == '{"contentType":"application/json"}'


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
    input_text = canonical_json(input_value or {"value": 21})
    pointer_text = canonical_json({"sourceFetch": str(source_root)})
    package_hash = "sha256:" + "d" * 64
    descriptor: dict[str, Any] = {
        "kind": "StepInvocationDescriptor",
        "schemaVersion": 0,
        "encoding": "json-v0",
        "planHash": "sha256:" + "a" * 64,
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
                "key": f"packages/{package_hash.replace(':', '-')}/source.tar.zst",
                "hash": sha256_ref(pointer_text),
                "contentType": "application/vnd.massive.source-directory+json",
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
            "artifact": {
                "key": "runs/python-runner-test/outputs/task.json",
                "contentType": "application/json",
            },
            "schema": schema_hash,
        },
        "datastore": {"kind": "local", "path": str(store)},
    }
    _write(store, descriptor["sourcePackage"]["sourceArchive"]["key"], pointer_text)
    _write(store, f"blobs/sha256/{schema_hash.removeprefix('sha256:')}", schema_text)
    _write(store, descriptor["input"]["artifact"]["key"], input_text)
    descriptor_path = tmp_path / "descriptor.json"
    descriptor_path.write_text(canonical_json(descriptor))
    return descriptor_path, descriptor, store


def _run(descriptor_path: Path) -> subprocess.CompletedProcess[str]:
    repository = Path(__file__).resolve().parents[3]
    return subprocess.run(
        [sys.executable, "-m", "massive.runner", str(descriptor_path)],
        cwd=repository,
        check=False,
        capture_output=True,
        text=True,
    )


def _write(root: Path, key: str, body: str) -> None:
    path = root / key
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(body)
