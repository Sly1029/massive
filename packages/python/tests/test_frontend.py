from __future__ import annotations

import json
import subprocess
from pathlib import Path

from massive import canonical_json


def test_emit_writes_a_canonical_spec_for_the_single_exported_graph(tmp_path: Path) -> None:
    workflow = tmp_path / "workflow.py"
    workflow.write_text(_workflow_source("graph"))

    result = _emit(workflow)

    assert result.returncode == 0, result.stderr
    assert result.stderr == ""
    assert result.stdout == canonical_json(json.loads(result.stdout))
    assert not result.stdout.endswith("\n")
    assert json.loads(result.stdout)["workflow"]["name"] == "frontend-graph"


def test_checked_python_workflow_matches_shared_conformance_fixture() -> None:
    repository = Path(__file__).resolve().parents[3]
    workflow = repository / "packages/cli/test/fixtures/python-linear/workflow.py"
    expected = (
        repository
        / "conformance/fixtures/specs/python-linear/workflow-spec.json"
    ).read_text()

    result = _emit(workflow, "graph")

    assert result.returncode == 0, result.stderr
    assert result.stderr == ""
    assert result.stdout == expected.rstrip("\n")


def test_emit_selects_the_requested_named_graph(tmp_path: Path) -> None:
    workflow = tmp_path / "workflow.py"
    workflow.write_text(_workflow_source("first") + "\n" + _workflow_source("second"))

    result = _emit(workflow, "second")

    assert result.returncode == 0, result.stderr
    assert result.stderr == ""
    assert json.loads(result.stdout)["workflow"]["name"] == "frontend-second"


def test_emit_rejects_an_explicit_selector_that_is_not_a_graph(tmp_path: Path) -> None:
    workflow = tmp_path / "workflow.py"
    workflow.write_text(_workflow_source("graph"))

    result = _emit(workflow, "Request")

    assert result.returncode == 2
    assert result.stdout == ""
    assert result.stderr == (
        f'massive-python-frontend: workflow entrypoint "{workflow}#Request" '
        "does not export a GraphBuilder\n"
    )


def test_emit_reports_sorted_candidates_for_ambiguous_graph_exports(tmp_path: Path) -> None:
    workflow = tmp_path / "workflow.py"
    workflow.write_text(_workflow_source("second") + "\n" + _workflow_source("first"))

    result = _emit(workflow)

    assert result.returncode == 2
    assert result.stdout == ""
    assert result.stderr == (
        f'massive-python-frontend: workflow entrypoint "{workflow}" is ambiguous; '
        "GraphBuilder candidates: first, second\n"
    )


def test_emit_reports_when_the_module_exports_no_graphs(tmp_path: Path) -> None:
    workflow = tmp_path / "workflow.py"
    workflow.write_text("value = 1\n")

    result = _emit(workflow)

    assert result.returncode == 2
    assert result.stdout == ""
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
    package = json.loads(result.stdout)["sourcePackages"]["python-main"]
    assert package["packageId"] == "python-main"
    assert [file["path"] for file in package["files"]] == ["helper.py", "workflow.py"]


def test_emit_errors_have_a_stable_nonzero_exit(tmp_path: Path) -> None:
    workflow = tmp_path / "workflow.py"
    workflow.write_text("value = 1\n")

    first = _emit(workflow)
    second = _emit(workflow)

    assert first.returncode == second.returncode == 2
    assert first.stdout == second.stdout == ""
    assert first.stderr == second.stderr


def test_missing_entrypoint_in_source_fails_before_import(tmp_path: Path) -> None:
    workflow = tmp_path / "workflow.py"
    workflow.write_text('raise RuntimeError("must not import")\n')
    (tmp_path / "pyproject.toml").write_text(
        '[tool.massive.source]\ninclude = ["pyproject.toml"]\n'
    )
    result = _emit(workflow)
    assert result.returncode == 2
    assert result.stdout == ""
    assert "source include must select the workflow entrypoint" in result.stderr
    assert "must not import" not in result.stderr


def test_malformed_packaging_fails_before_import(tmp_path: Path) -> None:
    workflow = tmp_path / "workflow.py"
    workflow.write_text('raise RuntimeError("must not import")\n')
    (tmp_path / "pyproject.toml").write_text("[invalid toml")
    result = _emit(workflow)
    assert result.returncode == 2
    assert result.stdout == ""
    assert "must not import" not in result.stderr


def _emit(workflow: Path, selector: str | None = None) -> subprocess.CompletedProcess[str]:
    package = Path(__file__).resolve().parents[1]
    entry = str(workflow) if selector is None else f"{workflow}#{selector}"
    return subprocess.run(
        [str(package / ".venv/bin/massive-python-frontend"), "emit", entry],
        cwd=package,
        check=False,
        capture_output=True,
        text=True,
    )


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


@{export}.step()
def identity(context: StepContext[None, Request]) -> Result:
    return Result(value=context.inputs.value)


identity_node = {export}.add(identity)
{export}.edge_from({export}.start).to(identity_node).to({export}.end)
'''
