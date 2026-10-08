"""Report how the running interpreter realizes a workflow project's requirements.

Run as ``python -I -m massive_environment probe <workflow-directory>``. The probe
reads project metadata and installed distribution metadata only; it never imports
workflow modules or the ``massive`` SDK, so a broken SDK dependency is reported
by the lock check instead of crashing the probe. Isolated mode keeps the
workflow directory, ``PYTHONPATH``, and user site-packages off ``sys.path``.
"""

from __future__ import annotations

import json
import platform
import shlex
import sys
import sysconfig
import tomllib
from collections.abc import Sequence
from fnmatch import fnmatchcase
from importlib.metadata import Distribution as InstalledDistribution
from importlib.metadata import distributions, packages_distributions
from pathlib import Path
from typing import Annotated, Literal
from urllib.parse import urlsplit

from packaging.requirements import Requirement
from packaging.specifiers import SpecifierSet
from packaging.utils import canonicalize_name
from pydantic import (
    AfterValidator,
    BaseModel,
    ConfigDict,
    Field,
    StringConstraints,
    ValidationError,
)

SDK_DISTRIBUTION = "massive-workflows"
_ERROR_EXIT = 2
_OPERATING_SYSTEMS = {"linux": "linux", "darwin": "darwin", "win32": "windows"}
_ARCHITECTURES = {"x86_64": "amd64", "amd64": "amd64", "aarch64": "arm64", "arm64": "arm64"}

NonEmpty = Annotated[str, StringConstraints(min_length=1)]
CanonicalName = Annotated[str, StringConstraints(pattern=r"^[a-z0-9]+(-[a-z0-9]+)*$")]
FindingCode = Literal[
    "REQUIRES_PYTHON",
    "MISSING_REQUIREMENT",
    "REQUIREMENT_VERSION",
    "DUPLICATE_DISTRIBUTION",
    "SHADOWED_MODULE",
    "WORKSPACE_LOCK",
    "LOCKED_SDK_VERSION",
]


class ProjectError(Exception):
    """Project metadata that cannot be read; reported as one line, exit 2."""


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


class _Workspace(BaseModel):
    model_config = ConfigDict(extra="ignore")

    members: tuple[str, ...] = ()
    exclude: tuple[str, ...] = ()


class _UVTool(BaseModel):
    model_config = ConfigDict(extra="ignore")

    workspace: _Workspace | None = None


class _Tools(BaseModel):
    model_config = ConfigDict(extra="ignore")

    uv: _UVTool = Field(default_factory=_UVTool)


class _Pyproject(BaseModel):
    model_config = ConfigDict(extra="ignore")

    project: _Project | None = None
    tool: _Tools = Field(default_factory=_Tools)


class _WorkspaceCandidate(BaseModel):
    """An ancestor pyproject.toml; only its uv workspace table matters."""

    model_config = ConfigDict(extra="ignore")

    tool: _Tools = Field(default_factory=_Tools)


class _LockedPackage(BaseModel):
    model_config = ConfigDict(extra="ignore")

    name: str
    version: str | None = None


class _Lock(BaseModel):
    model_config = ConfigDict(extra="ignore")

    package: tuple[_LockedPackage, ...] = ()


class _Output(BaseModel):
    model_config = ConfigDict(extra="forbid", frozen=True, serialize_by_alias=True)


class Interpreter(_Output):
    """The probed interpreter. Paths pin execution; they are not identity."""

    executable: NonEmpty
    prefix: NonEmpty
    implementation: NonEmpty
    version: NonEmpty
    cache_tag: NonEmpty | None = Field(alias="cacheTag")
    platform: NonEmpty
    os: NonEmpty
    arch: NonEmpty


class ProjectRequirements(_Output):
    """[project] dependency inputs; dependencies use canonical names, sorted."""

    requires_python: NonEmpty | None = Field(alias="requiresPython")
    dependencies: tuple[NonEmpty, ...]


class Distribution(_Output):
    """The first installed copy of a distribution on sys.path."""

    name: CanonicalName
    version: NonEmpty
    direct: bool
    editable: bool


class Finding(_Output):
    code: FindingCode
    message: NonEmpty
    fix: NonEmpty


class Probe(_Output):
    """Facts reported before any workflow module is imported."""

    model_config = ConfigDict(
        title="Massive Python environment probe",
        json_schema_extra={
            "$schema": "https://json-schema.org/draft/2020-12/schema",
            "$id": "https://massive.dev/conformance/schema/environment-probe.schema.json",
        },
    )

    schema_version: Literal[1] = Field(alias="schemaVersion")
    interpreter: Interpreter
    project: ProjectRequirements | None
    distributions: tuple[Distribution, ...]
    findings: tuple[Finding, ...]


def probe(root: Path) -> Probe:
    """Compare the running interpreter with the project at ``root`` without importing it."""
    project = _read(root / "pyproject.toml", _Pyproject).project
    locked = (root / "uv.lock").is_file()
    installed: dict[str, list[InstalledDistribution]] = {}
    for distribution in distributions():
        installed.setdefault(canonicalize_name(distribution.name), []).append(distribution)
    effective = {name: copies[0] for name, copies in sorted(installed.items())}

    findings: list[Finding] = []
    applicable = [] if project is None else _applicable(project)
    if project is not None:
        findings.extend(_requirement_findings(project, applicable, effective, root, locked))
    # Only copies that can change what the workflow imports matter; system
    # interpreters may carry unrelated duplicates in their own site-packages.
    relevant = {canonicalize_name(item.name) for item in applicable} | {SDK_DISTRIBUTION}
    for name in sorted(relevant):
        copies = installed.get(name, [])
        if len(copies) > 1:
            locations = ", ".join(f"{copy.version} in {copy.locate_file('')}" for copy in copies)
            findings.append(
                Finding(
                    code="DUPLICATE_DISTRIBUTION",
                    message=f"{name} is installed {len(copies)} times on sys.path "
                    f"({locations}); the first copy shadows the others",
                    fix=f"remove the shadowed {name} copies from sys.path, or recreate the "
                    f"environment: {_sync_command(root)}",
                )
            )
    # uv resolves a workspace member with the workspace's lock, even when the
    # member has a uv.lock of its own, so neither lock would really be checked.
    if (workspace := _workspace_root(root)) is not None:
        findings.append(
            Finding(
                code="WORKSPACE_LOCK",
                message=f"{root} is a member of the uv workspace at {workspace}; uv resolves "
                "it with the workspace's uv.lock, which is neither archived nor checked",
                fix=f"give the workflow its own lock: exclude it from [tool.uv.workspace] in "
                f"{workspace / 'pyproject.toml'} and run `uv lock` in {root}",
            )
        )
    if locked and (finding := _locked_sdk_finding(root, effective.get(SDK_DISTRIBUTION))):
        findings.append(finding)
    findings.extend(_shadowing_findings(root, project))

    return Probe(
        schemaVersion=1,
        interpreter=_interpreter(),
        project=None
        if project is None
        else ProjectRequirements(
            requiresPython=project.requires_python,
            dependencies=tuple(
                sorted(_normalized(Requirement(text)) for text in project.dependencies)
            ),
        ),
        distributions=tuple(
            _distribution_facts(name, distribution) for name, distribution in effective.items()
        ),
        findings=tuple(findings),
    )


def _read[Model: BaseModel](path: Path, model: type[Model]) -> Model:
    if not path.is_file():
        return model()
    try:
        return model.model_validate(tomllib.loads(path.read_text(encoding="utf-8")))
    except OSError as error:
        raise ProjectError(f"cannot read {path}: {error.strerror}") from error
    except tomllib.TOMLDecodeError as error:
        raise ProjectError(f"invalid TOML in {path}: {error}") from error
    except ValidationError as error:
        summary = "; ".join(
            f"{'.'.join(str(part) for part in item['loc'])}: {item['msg'].splitlines()[0]}"
            for item in error.errors(include_url=False)
        )
        raise ProjectError(f"invalid {path}: {summary}") from error


def _applicable(project: _Project) -> list[Requirement]:
    requirements = [Requirement(text) for text in project.dependencies]
    return [
        requirement
        for requirement in requirements
        if requirement.marker is None or requirement.marker.evaluate({"extra": ""})
    ]


def _sync_command(root: Path, *options: str) -> str:
    """The command that repairs this interpreter's environment from uv.lock."""
    flags = "".join(f" {option}" for option in options)
    if Path(sys.prefix).resolve() == (root / ".venv").resolve():
        return f"run `uv sync --locked{flags}` in {root}"
    return (
        f"run `UV_PROJECT_ENVIRONMENT={shlex.quote(sys.prefix)} uv sync --locked --no-dev{flags} "
        f"--project {shlex.quote(str(root))}`, or launch via `uv run --locked massive …` "
        f"from {root}"
    )


def _requirement_findings(
    project: _Project,
    applicable: list[Requirement],
    effective: dict[str, InstalledDistribution],
    root: Path,
    locked: bool,
) -> list[Finding]:
    findings: list[Finding] = []
    python_version = platform.python_version()
    if project.requires_python is not None and not SpecifierSet(project.requires_python).contains(
        python_version, prereleases=True
    ):
        spec = shlex.quote(project.requires_python)
        findings.append(
            Finding(
                code="REQUIRES_PYTHON",
                message=f"Python {python_version} at {sys.executable} does not satisfy "
                f"requires-python {project.requires_python!r}",
                fix=_sync_command(root, f"--python {spec}")
                if locked
                else f"create an environment with `uv venv --python {spec}` and install the "
                "workflow's dependencies into it",
            )
        )
    for requirement in applicable:
        installed = effective.get(canonicalize_name(requirement.name))
        shown = _normalized(requirement)
        if locked:
            install = _sync_command(root)
        elif requirement.url is not None:
            install = f"install {requirement.name} from its direct reference in [project].dependencies"
        else:
            install = (
                f"run `uv pip install --python {shlex.quote(sys.executable)} {shlex.quote(shown)}`"
            )
        if installed is None:
            findings.append(
                Finding(
                    code="MISSING_REQUIREMENT",
                    message=f"{shown!r} is declared in [project].dependencies but "
                    f"not installed in {sys.prefix}",
                    fix=install,
                )
            )
        elif not requirement.specifier.contains(installed.version, prereleases=True):
            findings.append(
                Finding(
                    code="REQUIREMENT_VERSION",
                    message=f"{requirement.name} {installed.version} is installed, but "
                    f"[project].dependencies requires {shown!r}",
                    fix=install,
                )
            )
    return findings


def _workspace_root(root: Path) -> Path | None:
    for parent in root.parents:
        workspace = _read(parent / "pyproject.toml", _WorkspaceCandidate).tool.uv.workspace
        if workspace is None:
            continue
        member = root.relative_to(parent).as_posix()
        if any(fnmatchcase(member, pattern) for pattern in workspace.members) and not any(
            fnmatchcase(member, pattern) for pattern in workspace.exclude
        ):
            return parent
    return None


def _locked_sdk_finding(root: Path, installed: InstalledDistribution | None) -> Finding | None:
    """The uv sync check excludes the SDK, whose image install records a local
    wheel rather than the locked source; its locked version must still match."""
    try:
        lock = _Lock.model_validate(tomllib.loads((root / "uv.lock").read_text(encoding="utf-8")))
    except (tomllib.TOMLDecodeError, ValidationError):
        return None  # the uv lock check reports a malformed lock
    locked = next(
        (item.version for item in lock.package if canonicalize_name(item.name) == SDK_DISTRIBUTION),
        None,
    )
    if locked is None or (installed is not None and installed.version == locked):
        return None
    return Finding(
        code="LOCKED_SDK_VERSION",
        message=f"uv.lock pins {SDK_DISTRIBUTION} {locked}, but "
        f"{'none' if installed is None else installed.version} is installed in {sys.prefix}",
        fix=_sync_command(root),
    )


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


def _normalized(requirement: Requirement) -> str:
    """A requirement safe to report and hash: a canonical name, and a direct
    reference without credentials, query, fragment, or local path."""
    normalized = Requirement(str(requirement))
    normalized.name = canonicalize_name(normalized.name)
    if normalized.url is not None:
        url = urlsplit(normalized.url)
        if url.scheme == "file" or not url.hostname:
            normalized.url = "file:"
        else:
            port = f":{url.port}" if url.port is not None else ""
            normalized.url = f"{url.scheme}://{url.hostname}{port}{url.path}"
    return str(normalized)


def _distribution_facts(name: str, distribution: InstalledDistribution) -> Distribution:
    direct_url = distribution.read_text("direct_url.json")
    editable = False
    if direct_url is not None:
        dir_info = json.loads(direct_url).get("dir_info", {})
        editable = dir_info.get("editable", False) is True
    return Distribution(
        name=name, version=distribution.version, direct=direct_url is not None, editable=editable
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
        sys.stderr.write("usage: python -I -m massive_environment probe <workflow-directory>\n")
        return _ERROR_EXIT
    try:
        result = probe(Path(arguments[1]).resolve())
    except ProjectError as error:
        sys.stderr.write(f"{error}\n")
        return _ERROR_EXIT
    sys.stdout.write(result.model_dump_json())
    return 0
