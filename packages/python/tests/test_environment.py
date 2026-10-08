from __future__ import annotations

import json
import subprocess
import sys
from pathlib import Path
from typing import Any

from jsonschema import Draft202012Validator

from massive_environment import Probe

_SCHEMA_PATH = (
    Path(__file__).resolve().parents[3] / "conformance/schema/environment-probe.schema.json"
)
_SCHEMA = json.loads(_SCHEMA_PATH.read_text())


def test_committed_schema_is_generated_from_the_probe_model() -> None:
    # Regenerate with:
    # python -I -c "import json, massive_environment as m;
    #   print(json.dumps(m.Probe.model_json_schema(mode='serialization'), indent=2))"
    assert Probe.model_json_schema(mode="serialization") == _SCHEMA


def _probe(root: Path) -> subprocess.CompletedProcess[str]:
    return subprocess.run(
        [sys.executable, "-I", "-m", "massive_environment", "probe", str(root)],
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
    prefix = f"invalid {tmp_path / 'pyproject.toml'}: project.dependencies.0: Value error, "
    assert result.stderr.startswith(prefix)
    assert result.stderr.count("\n") == 1
    assert "errors.pydantic.dev" not in result.stderr


def test_direct_references_never_carry_credentials_or_local_paths(tmp_path: Path) -> None:
    secret = "user:s3cr3t-token"
    (tmp_path / "pyproject.toml").write_text(
        "[project]\n"
        'name = "direct"\n'
        'version = "0.1.0"\n'
        "dependencies = [\n"
        f'  "Private_Pkg @ https://{secret}@packages.example.invalid:8443/p/pkg-1.0.tar.gz?t=1#sha256=0",\n'
        f'  "local-pkg @ file://{tmp_path}/wheels/local_pkg-1.0-py3-none-any.whl",\n'
        "]\n"
    )
    facts = _facts(tmp_path)
    assert facts["project"]["dependencies"] == [
        "local-pkg @ file:",
        "private-pkg @ https://packages.example.invalid:8443/p/pkg-1.0.tar.gz",
    ]
    reported = json.dumps(facts["findings"])
    assert [finding["code"] for finding in facts["findings"]] == ["MISSING_REQUIREMENT"] * 2
    assert "s3cr3t" not in reported and str(tmp_path / "wheels") not in reported


def test_locked_sdk_version_must_match_the_installed_sdk(tmp_path: Path) -> None:
    (tmp_path / "pyproject.toml").write_text('[project]\nname = "locked"\nversion = "0.1.0"\n')
    (tmp_path / "uv.lock").write_text(
        'version = 1\n[[package]]\nname = "massive-workflows"\nversion = "9.9.9"\n'
    )
    findings = _facts(tmp_path)["findings"]
    assert [finding["code"] for finding in findings] == ["LOCKED_SDK_VERSION"]
    assert "uv.lock pins massive-workflows 9.9.9" in findings[0]["message"]
