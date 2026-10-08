from __future__ import annotations

import json
import subprocess
import tempfile
from dataclasses import dataclass
from pathlib import Path

import pytest

from massive import canonical_json


def test_emit_writes_a_canonical_spec_for_the_single_exported_graph(tmp_path: Path) -> None:
    workflow = tmp_path / "workflow.py"
    workflow.write_text(_workflow_source("graph"))

    result = _emit(workflow)

    assert result.returncode == 0, result.stderr
    assert result.stderr == ""
    assert result.spec == canonical_json(json.loads(result.spec))
    assert not result.spec.endswith("\n")
    assert json.loads(result.spec)["workflow"]["name"] == "frontend-graph"


def test_emit_writes_only_the_spec_to_its_output_whatever_author_code_prints(tmp_path: Path) -> None:
    workflow = tmp_path / "workflow.py"
    workflow.write_text(
        "import os\nimport subprocess\nimport sys\n\n"
        'print("from print")\n'
        'os.write(1, b"from file descriptor\\n")\n'
        'subprocess.run([sys.executable, "-c", "print(\'from subprocess\')"], check=True)\n\n'
        + _workflow_source("graph")
    )

    result = _emit(workflow)

    assert result.returncode == 0, result.stderr
    assert result.spec == canonical_json(json.loads(result.spec))
    for line in ("from print", "from file descriptor", "from subprocess"):
        assert line in result.stdout


@pytest.mark.parametrize("fixture", ["python-linear", "python-fan-in"])
def test_checked_python_workflow_matches_shared_conformance_fixture(fixture: str) -> None:
    repository = Path(__file__).resolve().parents[3]
    workflow = repository / f"conformance/workflows/{fixture}/workflow.py"
    expected = (repository / f"conformance/fixtures/specs/{fixture}/workflow-spec.json").read_text()

    result = _emit(workflow, "graph")

    assert result.returncode == 0, result.stderr
    assert result.stderr == ""
    assert result.spec == expected.rstrip("\n")


def test_emit_selects_the_requested_named_graph(tmp_path: Path) -> None:
    workflow = tmp_path / "workflow.py"
    workflow.write_text(_workflow_source("first") + "\n" + _workflow_source("second"))

    result = _emit(workflow, "second")

    assert result.returncode == 0, result.stderr
    assert result.stderr == ""
    assert json.loads(result.spec)["workflow"]["name"] == "frontend-second"


def test_emit_rejects_an_explicit_selector_that_is_not_a_graph(tmp_path: Path) -> None:
    workflow = tmp_path / "workflow.py"
    workflow.write_text(_workflow_source("graph"))

    result = _emit(workflow, "Request")

    assert result.returncode == 2
    assert result.spec == ""
    assert result.stderr == (
        f'massive-python-frontend: workflow entrypoint "{workflow}#Request" '
        "does not export a GraphBuilder\n"
    )


def test_emit_reports_sorted_candidates_for_ambiguous_graph_exports(tmp_path: Path) -> None:
    workflow = tmp_path / "workflow.py"
    workflow.write_text(_workflow_source("second") + "\n" + _workflow_source("first"))

    result = _emit(workflow)

    assert result.returncode == 2
    assert result.spec == ""
    assert result.stderr == (
        f'massive-python-frontend: workflow entrypoint "{workflow}" is ambiguous; '
        "GraphBuilder candidates: first, second\n"
    )


def test_emit_reports_when_the_module_exports_no_graphs(tmp_path: Path) -> None:
    workflow = tmp_path / "workflow.py"
    workflow.write_text("value = 1\n")

    result = _emit(workflow)

    assert result.returncode == 2
    assert result.spec == ""
    assert result.stderr == (
        f'massive-python-frontend: workflow entrypoint "{workflow}" '
        "exports no GraphBuilder values (candidates: none)\n"
    )


def test_emit_includes_the_entry_and_root_level_sibling_python_files(tmp_path: Path) -> None:
    workflow = tmp_path / "workflow.py"
    helper = tmp_path / "helper.py"
    helper.write_text("increment = 1\n")
    (tmp_path / "ignored.txt").write_text("not source\n")
    workflow.write_text(
        _workflow_source("graph").replace(
            "from massive import GraphBuilder, StepContext, container, execution\n",
            "from helper import increment\n\n"
            "from massive import GraphBuilder, StepContext, container, execution\n",
        ).replace(
            "return Result(value=context.inputs.value)",
            "return Result(value=context.inputs.value + increment)",
        )
    )

    result = _emit(workflow)

    assert result.returncode == 0, result.stderr
    package = json.loads(result.spec)["sourcePackages"]["python-main"]
    assert package["packageId"] == "python-main"
    assert [file["path"] for file in package["files"]] == ["helper.py", "workflow.py"]


def test_emit_rejects_imported_workflow_modules_outside_the_source_package(
    tmp_path: Path,
) -> None:
    workflow = tmp_path / "workflow.py"
    steps = tmp_path / "steps"
    steps.mkdir()
    (steps / "__init__.py").write_text("")
    (steps / "work.py").write_text(
        "from massive import StepContext\n\n"
        "from models import Request, Result\n\n\n"
        "def identity(context: StepContext[Request]) -> Result:\n"
        "    return Result(value=context.inputs.value)\n"
    )
    (tmp_path / "models.py").write_text(
        "from pydantic import BaseModel\n\n\n"
        "class Request(BaseModel):\n    value: int\n\n\n"
        "class Result(BaseModel):\n    value: int\n"
    )
    workflow.write_text(
        "from massive import GraphBuilder, container, execution\n"
        "from models import Request, Result\n"
        "from steps.work import identity\n\n"
        "graph = GraphBuilder(\n"
        '    name="frontend-modules",\n'
        "    input_type=Request,\n"
        "    output_type=Result,\n"
        "    defaults=execution(environment=container(\n"
        '        "example.invalid/python@sha256:" + "0" * 64\n'
        "    )),\n"
        ")\n"
        "graph.edge_from(graph.start).to(graph.add(identity)).to_end(graph.end)\n"
    )

    rejected = _emit(workflow)

    assert rejected.returncode == 2
    assert rejected.spec == ""
    assert rejected.stderr == (
        "massive-python-frontend: source include does not select imported workflow "
        "modules: steps/__init__.py, steps/work.py; add them to "
        "[tool.massive.source].include\n"
    )

    (tmp_path / "pyproject.toml").write_text(
        '[tool.massive.source]\ninclude = ["*.py", "steps/*.py"]\n'
    )
    accepted = _emit(workflow)

    assert accepted.returncode == 0, accepted.stderr
    package = json.loads(accepted.spec)["sourcePackages"]["python-main"]
    assert [file["path"] for file in package["files"]] == [
        "models.py",
        "pyproject.toml",
        "steps/__init__.py",
        "steps/work.py",
        "workflow.py",
    ]


def test_emit_errors_have_a_stable_nonzero_exit(tmp_path: Path) -> None:
    workflow = tmp_path / "workflow.py"
    workflow.write_text("value = 1\n")

    first = _emit(workflow)
    second = _emit(workflow)

    assert first.returncode == second.returncode == 2
    assert first.spec == second.spec == ""
    assert first.stderr == second.stderr


def test_missing_entrypoint_in_source_fails_before_import(tmp_path: Path) -> None:
    workflow = tmp_path / "workflow.py"
    workflow.write_text('raise RuntimeError("must not import")\n')
    (tmp_path / "pyproject.toml").write_text(
        '[tool.massive.source]\ninclude = ["pyproject.toml"]\n'
    )
    result = _emit(workflow)
    assert result.returncode == 2
    assert result.spec == ""
    assert "source include must select the workflow entrypoint" in result.stderr
    assert "must not import" not in result.stderr


def test_malformed_packaging_fails_before_import(tmp_path: Path) -> None:
    workflow = tmp_path / "workflow.py"
    workflow.write_text('raise RuntimeError("must not import")\n')
    (tmp_path / "pyproject.toml").write_text("[invalid toml")
    result = _emit(workflow)
    assert result.returncode == 2
    assert result.spec == ""
    assert "must not import" not in result.stderr


@dataclass(frozen=True)
class _Emitted:
    returncode: int
    stdout: str
    stderr: str
    spec: str


def _emit(workflow: Path, selector: str | None = None) -> _Emitted:
    package = Path(__file__).resolve().parents[1]
    entry = str(workflow) if selector is None else f"{workflow}#{selector}"
    with tempfile.TemporaryDirectory() as directory:
        output = Path(directory) / "workflow-spec.json"
        result = subprocess.run(
            [
                str(package / ".venv/bin/massive-python-frontend"),
                "emit",
                "--output",
                str(output),
                entry,
            ],
            cwd=package,
            check=False,
            capture_output=True,
            text=True,
        )
        spec = output.read_text() if output.exists() else ""
    return _Emitted(result.returncode, result.stdout, result.stderr, spec)


def _workflow_source(export: str) -> str:
    return f'''\
from pydantic import BaseModel

from massive import GraphBuilder, StepContext, container, execution


class Request(BaseModel):
    value: int


class Result(BaseModel):
    value: int


{export} = GraphBuilder(
    name="frontend-{export}",
    input_type=Request,
    output_type=Result,
    defaults=execution(
        environment=container(
            "example.invalid/python@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
        )
    ),
)


def identity(context: StepContext[Request]) -> Result:
    return Result(value=context.inputs.value)


identity_node = {export}.add(identity)
{export}.edge_from({export}.start).to(identity_node).to_end({export}.end)
'''
