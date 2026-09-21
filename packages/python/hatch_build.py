from __future__ import annotations

import os
import shutil
import subprocess
from pathlib import Path
from typing import Any

from hatchling.builders.hooks.plugin.interface import BuildHookInterface
from packaging.tags import sys_tags

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

# Release cross-builds. The binaries are static; Go 1.27 requires macOS 13.
_RELEASE_PLATFORM_TAGS = {
    ("linux", "amd64"): "manylinux_2_17_x86_64",
    ("linux", "arm64"): "manylinux_2_17_aarch64",
    ("darwin", "amd64"): "macosx_13_0_x86_64",
    ("darwin", "arm64"): "macosx_13_0_arm64",
    ("windows", "amd64"): "win_amd64",
    ("windows", "arm64"): "win_arm64",
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

        go = shutil.which("go")
        if go is None:
            raise RuntimeError(
                "building massive-workflows from source requires the Go toolchain "
                "declared in go.mod on PATH; prebuilt wheels cover Linux (glibc), "
                "macOS 13+, and Windows on amd64 and arm64"
            )
        goos, goarch, platform_tag = _target_platform(go, repository)
        package_version = self.metadata.version
        executable_name = "massive.exe" if goos == "windows" else "massive"
        artifact = root / ".massive-build" / f"{goos}-{goarch}" / executable_name
        artifact.parent.mkdir(parents=True, exist_ok=True)
        environment = os.environ.copy()
        environment.update({"CGO_ENABLED": "0", "GOOS": goos, "GOARCH": goarch})
        subprocess.run(
            [
                go,
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
    # An extracted sdist carries go.mod at its root; a checkout keeps it two levels up.
    for candidate in (root, root.parent.parent):
        if (candidate / "go.mod").is_file() and (candidate / "cmd/massive").is_dir():
            return candidate
    raise FileNotFoundError("Massive Go source tree is unavailable")


def _target_platform(go: str, repository: Path) -> tuple[str, str, str]:
    if "MASSIVE_BUILD_GOOS" in os.environ or "MASSIVE_BUILD_GOARCH" in os.environ:
        goos = os.environ.get("MASSIVE_BUILD_GOOS", "")
        goarch = os.environ.get("MASSIVE_BUILD_GOARCH", "")
        try:
            return goos, goarch, _RELEASE_PLATFORM_TAGS[(goos, goarch)]
        except KeyError as error:
            raise RuntimeError(f"unsupported Massive release target: {goos}/{goarch}") from error
    # A source build targets the installing interpreter, including platforms
    # without a release wheel such as musl Linux.
    goos, goarch = subprocess.run(
        [go, "env", "GOOS", "GOARCH"],
        cwd=repository,
        capture_output=True,
        text=True,
        check=True,
    ).stdout.split()
    return goos, goarch, next(iter(sys_tags())).platform
