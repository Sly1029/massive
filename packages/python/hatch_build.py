from __future__ import annotations

import os
import platform
import subprocess
from pathlib import Path
from typing import Any

from hatchling.builders.hooks.plugin.interface import BuildHookInterface

_SCHEMAS = (
    Path("conformance/schema/step-invocation-descriptor.schema.json"),
    Path("conformance/schema/data-artifact-manifest.schema.json"),
    Path("conformance/schema/workflow-spec.schema.json"),
)

_GO_SOURCE_PATHS = (
    Path("go.mod"),
    Path("go.sum"),
    Path("cmd"),
    Path("internal"),
    Path("conformance/schema"),
)

_PLATFORM_TAGS = {
    ("linux", "amd64"): "manylinux_2_17_x86_64",
    ("linux", "arm64"): "manylinux_2_17_aarch64",
    ("darwin", "amd64"): "macosx_10_15_x86_64",
    ("darwin", "arm64"): "macosx_11_0_arm64",
    ("windows", "amd64"): "win_amd64",
    ("windows", "arm64"): "win_arm64",
}

_MACHINE_TO_GO_ARCH = {
    "aarch64": "arm64",
    "amd64": "amd64",
    "arm64": "arm64",
    "x86_64": "amd64",
}


class CustomBuildHook(BuildHookInterface):
    """Build the native control plane and package its canonical contracts."""

    def initialize(self, version: str, build_data: dict[str, Any]) -> None:
        root = Path(self.root)
        repository = _go_repository(root)
        for schema_path in _SCHEMAS:
            candidates = (repository / schema_path, root / schema_path)
            schema = next((candidate for candidate in candidates if candidate.is_file()), None)
            if schema is None:
                raise FileNotFoundError(f"canonical schema is unavailable: {schema_path}")
            destination = (
                schema_path.as_posix()
                if self.target_name == "sdist"
                else f"massive/schemas/{schema_path.name}"
            )
            build_data["force_include"][str(schema)] = destination

        if self.target_name == "sdist":
            for relative_path in _GO_SOURCE_PATHS:
                source = repository / relative_path
                if not source.exists():
                    raise FileNotFoundError(f"Go build source is unavailable: {relative_path}")
                build_data["force_include"][str(source)] = relative_path.as_posix()
            return

        if self.target_name != "wheel":
            return

        goos, goarch, platform_tag = _target_platform()
        package_version = self.metadata.version
        executable_name = "massive.exe" if goos == "windows" else "massive"
        artifact = root / ".massive-build" / f"{goos}-{goarch}" / executable_name
        artifact.parent.mkdir(parents=True, exist_ok=True)
        environment = os.environ.copy()
        environment.update({"CGO_ENABLED": "0", "GOOS": goos, "GOARCH": goarch})
        subprocess.run(
            [
                "go",
                "build",
                "-buildvcs=false",
                "-trimpath",
                "-ldflags",
                (
                    "-s -w -X "
                    "github.com/Sly1029/massive/internal/controlplane.Version="
                    f"{package_version}"
                ),
                "-o",
                str(artifact),
                "./cmd/massive",
            ],
            cwd=repository,
            env=environment,
            check=True,
        )
        build_data["force_include"][str(artifact)] = f"massive/_bin/{executable_name}"
        build_data["pure_python"] = False
        build_data["tag"] = f"py3-none-{platform_tag}"


def _go_repository(root: Path) -> Path:
    for candidate in (root.parents[1], root):
        if (candidate / "go.mod").is_file() and (candidate / "cmd/massive").is_dir():
            return candidate
    raise FileNotFoundError("Massive Go source tree is unavailable")


def _target_platform() -> tuple[str, str, str]:
    goos = os.environ.get("MASSIVE_BUILD_GOOS", platform.system().lower())
    host_machine = platform.machine().lower()
    goarch = os.environ.get("MASSIVE_BUILD_GOARCH", _MACHINE_TO_GO_ARCH.get(host_machine, ""))
    try:
        platform_tag = _PLATFORM_TAGS[(goos, goarch)]
    except KeyError as error:
        raise RuntimeError(f"unsupported Massive wheel target: {goos}/{goarch}") from error
    return goos, goarch, platform_tag
