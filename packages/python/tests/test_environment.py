from __future__ import annotations

import json
import subprocess
import sys
from pathlib import Path
from typing import Any

from jsonschema import Draft202012Validator

_SCHEMA = json.loads(
    (Path(__file__).resolve().parents[3] / "conformance/schema/environment-probe.schema.json")
    .read_text()
)


def _probe(root: Path) -> subprocess.CompletedProcess[str]:
    return subprocess.run(
        [sys.executable, "-I", "-m", "massive.environment", "probe", str(root)],
        cwd=root,
        check=False,
        capture_output=True,
        text=True,
    )


def _facts(root: Path) -> dict[str, Any]:
    result = _probe(root)
    assert result.returncode == 0, result.stderr
    facts = json.loads(result.stdout)
    Draft202012Validator(_SCHEMA).validate(facts)
    return facts


def test_probe_reports_the_running_interpreter_without_a_project(tmp_path: Path) -> None:
    facts = _facts(tmp_path)
    assert facts["project"] is None
    assert facts["findings"] == []
    assert facts["interpreter"]["executable"] == sys.executable
    assert facts["interpreter"]["cacheTag"] == sys.implementation.cache_tag
    names = [distribution["name"] for distribution in facts["distributions"]]
    assert names == sorted(names)
    sdk = next(item for item in facts["distributions"] if item["name"] == "massive-workflows")
    assert sdk["editable"] is True and sdk["direct"] is True


def test_probe_normalizes_requirements_and_skips_inapplicable_markers(tmp_path: Path) -> None:
    (tmp_path / "pyproject.toml").write_text(
        "[project]\n"
        'name = "normalized"\n'
        'version = "0.1.0"\n'
        'requires-python = ">=3.12"\n'
        'dependencies = ["Pydantic_Core >= 2", "absent ; python_version < \'3\'"]\n'
        "[tool.massive.source]\n"
        'include = ["*.py"]\n'
    )
    facts = _facts(tmp_path)
    assert facts["project"] == {
        "requiresPython": ">=3.12",
        "dependencies": ['absent; python_version < "3"', "pydantic-core>=2"],
    }
    assert facts["findings"] == []


def test_probe_rejects_invalid_requirements_without_partial_output(tmp_path: Path) -> None:
    (tmp_path / "pyproject.toml").write_text(
        '[project]\nname = "invalid"\nversion = "0.1.0"\ndependencies = ["not a requirement!"]\n'
    )
    result = _probe(tmp_path)
    assert result.returncode == 2
    assert result.stdout == ""
    assert "pyproject.toml" in result.stderr
