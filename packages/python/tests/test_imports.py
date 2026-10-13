from __future__ import annotations

import json
import os
import subprocess
import sys
import venv
from pathlib import Path

import pytest


@pytest.mark.parametrize(
    ("module", "deferred"),
    [
        (
            "massive.cli",
            ("pydantic", "jsonschema", "botocore", "massive.builder", "massive.files"),
        ),
        (
            "massive.frontend",
            ("jsonschema", "referencing", "botocore", "massive.files", "massive.runner"),
        ),
        (
            "massive.runner",
            (
                "pydantic",
                "pydantic_core",
                "jsonschema",
                "referencing",
                "botocore",
                "massive.builder",
                "massive.files",
                "massive.hashing",
                "massive._source_config",
                "asyncio",
                "tarfile",
            ),
        ),
    ],
)
def test_entrypoint_imports_defer_unused_dependencies(
    module: str, deferred: tuple[str, ...]
) -> None:
    result = subprocess.run(
        [
            sys.executable,
            "-X",
            "importtime",
            "-c",
            (
                "import importlib, json, sys; "
                "importlib.import_module(sys.argv[1]); print(json.dumps(sorted(sys.modules)))"
            ),
            module,
        ],
        capture_output=True,
        text=True,
        check=True,
    )
    loaded = json.loads(result.stdout)
    assert module in loaded
    for name in deferred:
        assert not any(item == name or item.startswith(name + ".") for item in loaded), name


def test_frontend_emits_without_storage_or_artifact_runtime_imports(tmp_path: Path) -> None:
    repository = Path(__file__).resolve().parents[3]
    output = tmp_path / "spec.json"
    workflow = repository / "conformance/workflows/python-linear/workflow.py"
    result = subprocess.run(
        [
            sys.executable,
            "-c",
            (
                "import json, sys; from massive.frontend import main; "
                "assert main(sys.argv[1:]) == 0; print(json.dumps(sorted(sys.modules)))"
            ),
            "emit",
            "--output",
            str(output),
            str(workflow),
        ],
        capture_output=True,
        text=True,
        check=True,
    )
    assert json.loads(output.read_text())["workflow"]["name"] == "python-linear"
    loaded = json.loads(result.stdout)
    for name in (
        "botocore",
        "jsonschema",
        "massive.datastore",
        "massive.artifact",
        "massive.files",
    ):
        assert name not in loaded, name


@pytest.mark.parametrize(
    "first_import",
    [
        "import massive",
        "import massive.frontend",
        "import massive.runner",
        "import massive.source_package",
        "from massive import GraphBuilder",
        "from massive import Blob",
    ],
)
def test_public_exports_keep_identity_and_source_factory_across_import_orders(
    first_import: str,
) -> None:
    result = subprocess.run(
        [
            sys.executable,
            "-c",
            first_import
            + "\n"
            + """
import importlib
from pathlib import Path
import massive

assert set(massive.__all__) <= set(dir(massive))
namespace = {}
exec('from massive import *', namespace)
for name in massive.__all__:
    exported = namespace[name]
    assert exported is getattr(massive, name)
    module_name = getattr(exported, '__module__', 'massive.builder')
    assert exported is getattr(importlib.import_module(module_name), name)
package = massive.source_package(root=Path('.'), include=['*.py'], package_id='test')
assert isinstance(package, massive.SourcePackage)
assert package.include == ('*.py',)
try:
    massive.missing_export
except AttributeError:
    pass
else:
    raise AssertionError('unknown exports must raise AttributeError')
""",
        ],
        capture_output=True,
        text=True,
        check=False,
    )
    assert result.returncode == 0, result.stderr


def test_local_datastore_works_without_cloud_dependencies(tmp_path: Path) -> None:
    environment = tmp_path / "environment"
    venv.EnvBuilder(with_pip=False).create(environment)
    python = environment / ("Scripts/python.exe" if os.name == "nt" else "bin/python")
    sdk_source = Path(__file__).resolve().parents[1] / "src"
    result = subprocess.run(
        [
            str(python),
            "-I",
            "-c",
            """
import sys
from pathlib import Path
sys.path.insert(0, sys.argv[1])
from massive import StepContext
from massive.datastore import LocalDatastore, S3Datastore

store = LocalDatastore(Path(sys.argv[2]))
store.put('local.txt', b'local body', content_type='text/plain')
assert store.get('local.txt').body == b'local body'
try:
    S3Datastore({'kind': 's3', 'bucket': 'test', 'region': 'us-east-1'})
except ModuleNotFoundError as error:
    assert error.name == 'botocore', error
else:
    raise AssertionError('S3 use must require botocore')
""",
            str(sdk_source),
            str(tmp_path / "store"),
        ],
        capture_output=True,
        text=True,
        check=False,
    )
    assert result.returncode == 0, result.stderr
