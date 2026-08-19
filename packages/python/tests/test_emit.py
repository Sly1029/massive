from __future__ import annotations

import importlib.util
import json
import subprocess
import sys
from pathlib import Path
from types import ModuleType
from typing import Any, cast

from massive import source_package


def test_emits_a_canonical_python_workflow_spec_accepted_by_go_compiler(tmp_path: Path) -> None:
    repository = Path(__file__).resolve().parents[3]
    fixture = Path(__file__).parent / "fixtures/emission_workflow.py"
    module = _load_fixture(fixture)

    specification = module.graph.emit(
        source=source_package(
            root=fixture.parent,
            include=[fixture.name],
            package_id="python-main",
        )
    )
    spec_path = tmp_path / "workflow-spec.json"
    spec_path.write_text(specification.to_json() + "\n")

    result = subprocess.run(
        [
            "go",
            "run",
            "./cmd/massive-compiler",
            "compile",
            "--spec",
            str(spec_path),
            "--out",
            str(tmp_path / "compiled"),
        ],
        cwd=repository,
        check=False,
        capture_output=True,
        text=True,
    )

    assert result.returncode == 0, result.stderr
    emitted = json.loads(spec_path.read_text())
    assert emitted["graph"]["irVersion"] == "0.1"
    step = next(node for node in emitted["graph"]["nodes"] if node["kind"] == "step")
    catalog = json.loads((repository / "conformance/graph-catalog.json").read_text())
    graph_case = next(item for item in catalog["cases"] if item["id"] == "single-step")
    assert (
        len([node for node in emitted["graph"]["nodes"] if node["kind"] == "step"])
        == (graph_case["executableSteps"])
    )
    assert len(emitted["graph"]["edges"]) == graph_case["directedEdges"]
    assert graph_case["mergeInputs"] == []
    assert emitted["specHash"] == specification.spec_hash
    assert emitted["symbols"][step["symbolRef"]] == {
        "packageId": "python-main",
        "language": "python",
        "module": "emission_workflow",
        "export": "increment",
    }
    compiled = json.loads((tmp_path / "compiled/workflow-plan.json").read_text())
    assert compiled["graph"]["workflowName"] == "python-emission"
    environment_identity = module.graph.defaults.environment.plan().identity
    assert compiled["environments"] == [
        {
            "envRef": environment_identity,
            "kind": "container-plan",
            "specHash": environment_identity,
            "container": {
                "image": "example.invalid/python@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
                "platform": "linux/amd64",
                "command": ["python", "-m", "massive"],
                "workingDirectory": "app",
            },
        }
    ]
    assert compiled["contracts"][0]["environmentRef"] == environment_identity


def test_python_model_map_emits_a_spec_accepted_by_go_compiler(tmp_path: Path) -> None:
    repository = Path(__file__).resolve().parents[3]
    fixture = Path(__file__).parent / "fixtures/map_emission_workflow.py"
    module = _load_fixture(fixture)

    specification = module.graph.emit(
        source=source_package(
            root=fixture.parent,
            include=[fixture.name],
            package_id="python-main",
        )
    )
    spec_path = tmp_path / "workflow-spec.json"
    spec_path.write_text(specification.to_json() + "\n")

    result = subprocess.run(
        [
            "go",
            "run",
            "./cmd/massive-compiler",
            "compile",
            "--spec",
            str(spec_path),
            "--out",
            str(tmp_path / "compiled"),
        ],
        cwd=repository,
        check=False,
        capture_output=True,
        text=True,
    )

    assert result.returncode == 0, result.stderr
    emitted = json.loads(spec_path.read_text())
    map_node = next(node for node in emitted["graph"]["nodes"] if node["kind"] == "map")
    assert emitted["graph"]["irVersion"] == "0.3"
    assert emitted["schemas"][map_node["inputSchema"]]["items"] == {"$ref": "#/$defs/Item"}
    assert "Detail" in emitted["schemas"][map_node["inputSchema"]]["$defs"]
    assert emitted["schemas"][map_node["outputSchema"]]["items"] == {"$ref": "#/$defs/Result"}
    compiled = json.loads((tmp_path / "compiled/workflow-plan.json").read_text())
    compiled_map = next(node for node in compiled["graph"]["nodes"] if node["kind"] == "map")
    assert compiled_map["maxConcurrency"] == 3


def test_source_checkout_location_does_not_change_workflow_identity(tmp_path: Path) -> None:
    fixture = Path(__file__).parent / "fixtures/emission_workflow.py"
    emitted = []
    for checkout in (tmp_path / "first", tmp_path / "second/nested"):
        checkout.mkdir(parents=True)
        source = checkout / fixture.name
        source.write_bytes(fixture.read_bytes())
        module = _load_fixture(source)
        emitted.append(
            module.graph.emit(
                source=source_package(
                    root=checkout,
                    include=[source.name],
                    package_id="python-main",
                )
            )
        )

    assert emitted[0].spec_hash == emitted[1].spec_hash
    assert emitted[0].to_json() == emitted[1].to_json()
    source_packages = cast(dict[str, Any], emitted[0].value["sourcePackages"])
    package = cast(dict[str, Any], source_packages["python-main"])
    assert "root" not in package
    assert "include" not in package


def test_decision_reuses_the_persisted_output_schema_when_pydantic_modes_differ(
    tmp_path: Path,
) -> None:
    repository = Path(__file__).resolve().parents[3]
    fixture = Path(__file__).parent / "fixtures/decimal_decision_workflow.py"
    module = _load_fixture(fixture)
    specification = module.graph.emit(
        source=source_package(
            root=fixture.parent,
            include=[fixture.name],
            package_id="python-main",
        )
    )
    spec_path = tmp_path / "workflow-spec.json"
    spec_path.write_text(specification.to_json() + "\n")

    nodes = {
        node["id"]: node for node in cast(dict[str, Any], specification.value["graph"])["nodes"]
    }
    assert nodes["classify"]["outputSchema"] == nodes["route"]["inputSchema"]
    assert nodes["classify"]["outputSchema"] != nodes["accept"]["inputSchema"]
    accepted_case = next(
        decision_case
        for decision_case in nodes["route"]["cases"]
        if decision_case["tag"] == "accepted"
    )
    assert accepted_case["schema"] == nodes["accept"]["inputSchema"]

    result = subprocess.run(
        [
            "go",
            "run",
            "./cmd/massive-compiler",
            "compile",
            "--spec",
            str(spec_path),
            "--out",
            str(tmp_path / "compiled"),
        ],
        cwd=repository,
        check=False,
        capture_output=True,
        text=True,
    )
    assert result.returncode == 0, result.stderr


def _load_fixture(path: Path) -> ModuleType:
    specification = importlib.util.spec_from_file_location("emission_workflow", path)
    assert specification is not None
    assert specification.loader is not None
    module = importlib.util.module_from_spec(specification)
    sys.modules[specification.name] = module
    specification.loader.exec_module(module)
    return module
