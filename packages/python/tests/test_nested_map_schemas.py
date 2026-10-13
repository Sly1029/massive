"""Self-contained item schemas and execution of nested model containers."""

from __future__ import annotations

import importlib.util
import json
import os
import subprocess
import sys
from pathlib import Path
from typing import Any

import pytest
from pydantic import TypeAdapter

from massive import MapItemOutcome

REPOSITORY = Path(__file__).resolve().parents[3]
WORKFLOW = REPOSITORY / "conformance/workflows/python-nested-map/workflow.py"


@pytest.fixture(scope="module")
def massive_cli(tmp_path_factory: pytest.TempPathFactory) -> Path:
    binary = tmp_path_factory.mktemp("nested-map-cli") / "massive"
    subprocess.run(
        ["go", "build", "-o", str(binary), "./cmd/massive"],
        cwd=REPOSITORY,
        check=True,
        capture_output=True,
    )
    return binary


@pytest.mark.parametrize("values", [[0, 1, 2], []])
@pytest.mark.parametrize(
    "graph", ["lists", "tuples", "nested", "inputs", "collect", "gather", "merge"]
)
def test_nested_model_containers_run(
    massive_cli: Path, tmp_path: Path, graph: str, values: list[int]
) -> None:
    store = tmp_path / "store"
    result = subprocess.run(
        [
            str(massive_cli),
            "run",
            f"{WORKFLOW}#{graph}",
            "--store",
            str(store),
            "--project",
            "example/nested-map",
            "--run-id",
            "nested-map",
            "--input",
            json.dumps(values),
            "--json",
        ],
        cwd=REPOSITORY,
        env={**os.environ, "MASSIVE_PYTHON": sys.executable},
        check=False,
        capture_output=True,
        text=True,
    )
    assert result.returncode == 0, result.stderr
    run = json.loads(result.stdout)
    assert run["status"] == "succeeded"
    if graph in {"gather", "merge"}:
        expected = [str(value) for value in values] * 2
    elif graph == "collect":
        expected = [
            part
            for value in values
            for part in (["failed"] if value % 2 else [str(value), f"{value}!"])
        ]
    else:
        expected = [part for value in values for part in [str(value), f"{value}!"]]
    assert run["result"] == expected


def test_python_emits_nested_array_schema_vectors() -> None:
    specification = importlib.util.spec_from_file_location("workflow", WORKFLOW)
    assert specification is not None and specification.loader is not None
    module = importlib.util.module_from_spec(specification)
    sys.modules[specification.name] = module
    try:
        specification.loader.exec_module(module)
        types: dict[str, Any] = {
            "model-list": list[module.Report],
            "model-tuple": tuple[module.Report, ...],
            "nested-model-list": list[module.Nested],
            "recursive-model-list": list[module.Recursive],
            "nullable-model-list": list[module.Report] | None,
            "fixed-model-tuple": tuple[module.Report, module.Nested],
            "model-dict": dict[str, module.Report],
        }
        vectors = json.loads(
            (REPOSITORY / "conformance/fixtures/array-items/schemas.json").read_text()
        )
        for vector in vectors["cases"]:
            if vector["name"] not in types:
                continue
            annotation = types[vector["name"]]
            assert vector["accepted"] is True
            assert vector["itemSchema"] == TypeAdapter(annotation).json_schema(mode="serialization")
            assert vector["arraySchema"] == TypeAdapter(list[annotation]).json_schema(
                mode="serialization"
            )
        outcomes = json.loads(
            (REPOSITORY / "conformance/fixtures/map-outcomes/outcome-schemas.json").read_text()
        )
        for vector in outcomes["cases"]:
            name = vector["name"].removeprefix("pydantic-")
            if name not in types:
                continue
            annotation = types[name]
            assert vector["accepted"] is True
            assert vector["itemOutputSchema"] == TypeAdapter(annotation).json_schema(
                mode="serialization"
            )
            assert vector["outputSchema"] == TypeAdapter(
                list[MapItemOutcome[annotation]]
            ).json_schema(mode="serialization")
    finally:
        del sys.modules[specification.name]
