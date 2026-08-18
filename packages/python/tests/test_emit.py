from __future__ import annotations

import importlib.util
import json
import subprocess
import sys
from pathlib import Path
from types import ModuleType

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
    step = next(node for node in emitted["graph"]["nodes"] if node["kind"] == "step")
    assert emitted["specHash"] == specification.spec_hash
    assert emitted["symbols"][step["symbolRef"]] == {
        "packageId": "python-main",
        "language": "python",
        "module": "emission_workflow",
        "export": "increment",
    }
    assert json.loads((tmp_path / "compiled/workflow-plan.json").read_text())["graph"][
        "workflowName"
    ] == ("python-emission")


def _load_fixture(path: Path) -> ModuleType:
    specification = importlib.util.spec_from_file_location("emission_workflow", path)
    assert specification is not None
    assert specification.loader is not None
    module = importlib.util.module_from_spec(specification)
    sys.modules[specification.name] = module
    specification.loader.exec_module(module)
    return module
