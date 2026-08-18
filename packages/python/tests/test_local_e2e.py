from __future__ import annotations

import importlib.util
import json
import subprocess
import sys
from pathlib import Path
from types import ModuleType

from massive import source_package


def test_python_workflow_runs_through_go_orchestrator(tmp_path: Path) -> None:
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
    store = tmp_path / "store"

    result = subprocess.run(
        [
            "go",
            "run",
            "./cmd/massive-orchestrator",
            "run",
            "--spec",
            str(spec_path),
            "--store",
            str(store),
            "--project",
            "example/python-e2e",
            "--run-id",
            "python-e2e",
            "--input",
            '{"value":21}',
            "--json",
        ],
        cwd=repository,
        check=False,
        capture_output=True,
        text=True,
    )

    assert result.returncode == 0, result.stderr
    run = json.loads(result.stdout)
    assert run["status"] == "succeeded"
    assert run["steps"] == [{"nodeId": "increment", "status": "succeeded"}]
    assert json.loads((store / run["resultKey"]).read_text()) == {"value": 22}


def _load_fixture(path: Path) -> ModuleType:
    specification = importlib.util.spec_from_file_location("python_e2e_workflow", path)
    assert specification is not None
    assert specification.loader is not None
    module = importlib.util.module_from_spec(specification)
    sys.modules[specification.name] = module
    specification.loader.exec_module(module)
    return module
