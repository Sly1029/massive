"""Report how the running interpreter realizes a workflow project's requirements.

Run as ``python -I -m massive.environment probe <workflow-directory>``. The probe
reads project metadata and installed distribution metadata only; it never imports
workflow modules. Isolated mode keeps the workflow directory, ``PYTHONPATH``, and
user site-packages off ``sys.path``, so a planted module cannot run here.
"""

from __future__ import annotations

import json
import platform
import sys
import sysconfig
import tomllib
from collections.abc import Sequence
from importlib.metadata import Distribution as InstalledDistribution
from importlib.metadata import distributions, packages_distributions
from pathlib import Path
from typing import Annotated, Literal

from packaging.requirements import Requirement
from packaging.specifiers import SpecifierSet
from packaging.utils import canonicalize_name
from pydantic import AfterValidator, BaseModel, ConfigDict, Field, ValidationError

PROBE_SCHEMA_VERSION = 1
_ERROR_EXIT = 2
_OPERATING_SYSTEMS = {"linux": "linux", "darwin": "darwin", "win32": "windows"}
_ARCHITECTURES = {"x86_64": "amd64", "amd64": "amd64", "aarch64": "arm64", "arm64": "arm64"}

FindingCode = Literal[
    "REQUIRES_PYTHON",
    "MISSING_REQUIREMENT",
    "REQUIREMENT_VERSION",
    "DUPLICATE_DISTRIBUTION",
    "SHADOWED_MODULE",
]


def _specifier(value: str) -> str:
    SpecifierSet(value)
    return value


def _requirement(value: str) -> str:
    Requirement(value)
    return value


class _Project(BaseModel):
    model_config = ConfigDict(extra="ignore")

    name: str | None = None
    requires_python: Annotated[str, AfterValidator(_specifier)] | None = Field(
        default=None, alias="requires-python"
    )
    dependencies: tuple[Annotated[str, AfterValidator(_requirement)], ...] = ()


class _Pyproject(BaseModel):
    model_config = ConfigDict(extra="ignore")

    project: _Project | None = None


class _Output(BaseModel):
    model_config = ConfigDict(extra="forbid", frozen=True, serialize_by_alias=True)


class Interpreter(_Output):
    executable: str
    prefix: str
    implementation: str
    version: str
    cache_tag: str | None = Field(alias="cacheTag")
    platform: str
    os: str
    arch: str


class ProjectRequirements(_Output):
    requires_python: str | None = Field(alias="requiresPython")
    dependencies: tuple[str, ...]


class InstalledDistributionFacts(_Output):
    name: str
    version: str
    direct: bool
    editable: bool


class Finding(_Output):
    code: FindingCode
    message: str
    fix: str


class Probe(_Output):
    schema_version: Literal[1] = Field(alias="schemaVersion")
    interpreter: Interpreter
    project: ProjectRequirements | None
    distributions: tuple[InstalledDistributionFacts, ...]
    findings: tuple[Finding, ...]


def probe(root: Path) -> Probe:
    """Compare the running interpreter with the project at ``root`` without importing it."""
    project_file = root / "pyproject.toml"
    project = (
        _Pyproject.model_validate(tomllib.loads(project_file.read_text(encoding="utf-8"))).project
        if project_file.is_file()
        else None
    )
    installed: dict[str, list[InstalledDistribution]] = {}
    for distribution in distributions():
        installed.setdefault(canonicalize_name(distribution.name), []).append(distribution)
    effective = {name: copies[0] for name, copies in sorted(installed.items())}

    findings: list[Finding] = []
    for name, copies in installed.items():
        if len(copies) > 1:
            locations = ", ".join(
                f"{copy.version} in {copy.locate_file('')}" for copy in copies
            )
            findings.append(
                Finding(
                    code="DUPLICATE_DISTRIBUTION",
                    message=f"{name} is installed {len(copies)} times on sys.path ({locations}); "
                    "the first copy shadows the others",
                    fix=f"remove the extra {name} installations, or recreate the environment "
                    "with `uv sync --locked`",
                )
            )
    if project is not None:
        findings.extend(_requirement_findings(project, effective, root))
    findings.extend(_shadowing_findings(root, project))

    return Probe(
        schemaVersion=PROBE_SCHEMA_VERSION,
        interpreter=_interpreter(),
        project=None
        if project is None
        else ProjectRequirements(
            requiresPython=project.requires_python,
            dependencies=tuple(sorted(_normalized(text) for text in project.dependencies)),
        ),
        distributions=tuple(
            _distribution_facts(name, distribution) for name, distribution in effective.items()
        ),
        findings=tuple(findings),
    )


def _requirement_findings(
    project: _Project, effective: dict[str, InstalledDistribution], root: Path
) -> list[Finding]:
    findings: list[Finding] = []
    python_version = platform.python_version()
    if project.requires_python is not None and not SpecifierSet(project.requires_python).contains(
        python_version, prereleases=True
    ):
        findings.append(
            Finding(
                code="REQUIRES_PYTHON",
                message=f"Python {python_version} does not satisfy requires-python "
                f"{project.requires_python!r}",
                fix=f"create the environment with a compatible interpreter: "
                f"`uv sync --locked --python '{project.requires_python}'` in {root}",
            )
        )
    for text in project.dependencies:
        requirement = Requirement(text)
        if requirement.marker is not None and not requirement.marker.evaluate({"extra": ""}):
            continue
        installed = effective.get(canonicalize_name(requirement.name))
        if installed is None:
            findings.append(
                Finding(
                    code="MISSING_REQUIREMENT",
                    message=f"{text!r} is declared in [project].dependencies but not installed",
                    fix=f"install the project's dependencies with `uv sync --locked` in {root}",
                )
            )
        elif not requirement.specifier.contains(installed.version, prereleases=True):
            findings.append(
                Finding(
                    code="REQUIREMENT_VERSION",
                    message=f"{requirement.name} {installed.version} is installed, but "
                    f"[project].dependencies requires {text!r}",
                    fix=f"install the declared version with `uv sync --locked` in {root}",
                )
            )
    return findings


def _shadowing_findings(root: Path, project: _Project | None) -> list[Finding]:
    """Find workflow-directory modules that would replace an interpreter module.

    The frontend and runner put the workflow directory first on ``sys.path``.
    """
    own_distribution = (
        canonicalize_name(project.name) if project is not None and project.name else None
    )
    installed_modules = {
        module: owners
        for module, names in packages_distributions().items()
        if (owners := sorted({canonicalize_name(name) for name in names} - {own_distribution}))
    }
    findings: list[Finding] = []
    for path in sorted(root.iterdir()):
        if path.is_file() and path.suffix == ".py":
            module = path.stem
        elif (path / "__init__.py").is_file():
            module = path.name
        else:
            continue
        if module in sys.stdlib_module_names:
            shadowed = f"the standard-library module {module!r}"
        elif module in installed_modules:
            shadowed = f"module {module!r} from {', '.join(installed_modules[module])}"
        else:
            continue
        findings.append(
            Finding(
                code="SHADOWED_MODULE",
                message=f"{path.name} in {root} shadows {shadowed}",
                fix=f"rename {path.name} and update the imports that use it",
            )
        )
    return findings


def _normalized(text: str) -> str:
    requirement = Requirement(text)
    requirement.name = canonicalize_name(requirement.name)
    return str(requirement)


def _distribution_facts(
    name: str, distribution: InstalledDistribution
) -> InstalledDistributionFacts:
    direct_url = distribution.read_text("direct_url.json")
    editable = False
    if direct_url is not None:
        dir_info = json.loads(direct_url).get("dir_info", {})
        editable = dir_info.get("editable", False) is True
    return InstalledDistributionFacts(
        name=name,
        version=distribution.version,
        direct=direct_url is not None,
        editable=editable,
    )


def _interpreter() -> Interpreter:
    machine = platform.machine().lower()
    return Interpreter(
        executable=sys.executable,
        prefix=sys.prefix,
        implementation=sys.implementation.name,
        version=platform.python_version(),
        cacheTag=sys.implementation.cache_tag,
        platform=sysconfig.get_platform(),
        os=_OPERATING_SYSTEMS.get(sys.platform, sys.platform),
        arch=_ARCHITECTURES.get(machine, machine),
    )


def main(argv: Sequence[str] | None = None) -> int:
    arguments = tuple(sys.argv[1:] if argv is None else argv)
    if len(arguments) != 2 or arguments[0] != "probe":
        sys.stderr.write("usage: python -I -m massive.environment probe <workflow-directory>\n")
        return _ERROR_EXIT
    root = Path(arguments[1]).resolve()
    try:
        result = probe(root)
    except (OSError, tomllib.TOMLDecodeError, ValidationError) as error:
        sys.stderr.write(f"massive.environment: cannot read {root / 'pyproject.toml'}: {error}\n")
        return _ERROR_EXIT
    sys.stdout.write(result.model_dump_json())
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
