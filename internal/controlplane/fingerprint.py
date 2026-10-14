"""Best-effort cache eligibility and fingerprints; no author imports or third-party libraries.

Executed by the control plane with python -I -c. Failure disables caching, leaving
all diagnostics and validation to the normal source reader and preflight.
"""

import hashlib
import json
import os
import shutil
import sys
from pathlib import Path

import tomllib
from massive.source_package import selected_files


def digest(data):
    return "sha256:" + hashlib.sha256(data).hexdigest()


def stamp(path):
    info = path.stat()
    return [
        str(path),
        str(path.resolve()),
        info.st_mode,
        info.st_size,
        info.st_mtime_ns,
        info.st_ctime_ns,
        info.st_dev,
        info.st_ino,
    ]


def fingerprint(root):
    # System sites, editable installs, custom .pth hooks and external lock sources
    # can change import resolution outside the inspected trees. Do not cache them.
    prefix = Path(sys.prefix)
    config = prefix / "pyvenv.cfg"
    if sys.prefix == sys.base_prefix or not config.is_file():
        return None
    if "include-system-site-packages = true" in config.read_text().lower():
        return None
    project_file = root / "pyproject.toml"
    project = tomllib.loads(project_file.read_text()) if project_file.is_file() else {}
    include = tuple(
        project.get("tool", {})
        .get("massive", {})
        .get("source", {})
        .get("include", ["*.py"])
    )
    files = selected_files(root, (*include, "pyproject.toml", "uv.lock"))
    source = [
        {"path": name, "hash": digest(path.read_bytes())}
        for name, path in sorted(
            files.items(), key=lambda item: item[0].encode("utf-16-be")
        )
    ]
    lock_file = root / "uv.lock"
    if lock_file.is_file():
        lock = tomllib.loads(lock_file.read_text())
        for package in lock.get("package", []):
            location = package.get("source", {})
            if any(key in location for key in ("directory", "editable", "path")):
                return None
            if location.get("registry", "https:").startswith("file:"):
                return None
            if "virtual" in location and location["virtual"] != ".":
                return None

    state = [
        digest(lock_file.read_bytes()) if lock_file.is_file() else None,
        sys.version,
        sys.implementation.cache_tag,
        sys.prefix,
        sys.base_prefix,
        sys.path,
        stamp(Path(sys.executable)),
        stamp(Path(sys._base_executable)),
        digest(config.read_bytes()),
    ]
    seen = set()
    for entry in sys.path:
        path = Path(entry)
        if not path.exists():
            state.append([entry, None])
            continue
        if not path.is_dir():
            state.append([stamp(path), digest(path.read_bytes())])
            continue
        # No path injected from an external editable source or startup hook.
        if not path.is_relative_to(prefix) and not path.is_relative_to(
            Path(sys.base_prefix)
        ):
            return None
        pending = [str(path)]
        while pending:
            directory = pending.pop()
            if directory in seen:
                continue
            seen.add(directory)
            entries = sorted(os.scandir(directory), key=lambda item: item.name)
            if any(
                item.name.endswith((".pyc", ".pyo"))
                and not Path(item.path).with_suffix(".py").is_file()
                for item in entries
            ):
                return None
            entries = [
                item
                for item in entries
                if item.name != "__pycache__"
                and not item.name.endswith((".pyc", ".pyo"))
            ]
            state.append([directory, [item.name for item in entries]])
            for item in entries:
                if item.is_symlink():
                    return None
                if item.is_dir():
                    pending.append(item.path)
                    continue
                if item.name in (
                    "sitecustomize.py",
                    "usercustomize.py",
                ) or item.name.endswith(".egg-link"):
                    return None
                if item.name.endswith(".pth") and (
                    item.name != "_virtualenv.pth"
                    or Path(item.path).read_text().strip() != "import _virtualenv"
                ):
                    return None
                info = item.stat()
                state.append(
                    [
                        item.path,
                        info.st_mode,
                        info.st_size,
                        info.st_mtime_ns,
                        info.st_ctime_ns,
                        info.st_dev,
                        info.st_ino,
                    ]
                )
                if directory.endswith((".dist-info", ".egg-info")):
                    content = Path(item.path).read_bytes()
                    state.append(digest(content))
                    if item.name == "direct_url.json" and json.loads(content).get(
                        "dir_info", {}
                    ).get("editable"):
                        return None
    # The probe checks unselected root modules for shadowing and ancestor
    # pyprojects for uv workspaces. uv also reads project/user configuration.
    state.append(
        sorted(
            [path.name, path.is_file(), (path / "__init__.py").is_file()]
            for path in root.iterdir()
            if path.name != "__pycache__"
        )
    )
    configs = []
    for ancestor in (root, *root.parents):
        for name in ("pyproject.toml", "uv.toml"):
            path = ancestor / name
            content = path.read_bytes() if path.is_file() else None
            state.append([str(path), digest(content) if content is not None else None])
            if content is not None:
                parsed = tomllib.loads(content.decode())
                configs.append(
                    parsed.get("tool", {}).get("uv", {})
                    if name == "pyproject.toml"
                    else parsed
                )
    if sys.platform == "win32":
        config_paths = [
            Path(os.environ[variable]) / "uv" / "uv.toml"
            for variable in ("APPDATA", "PROGRAMDATA")
            if variable in os.environ
        ]
    else:
        config_paths = [
            Path(os.environ.get("XDG_CONFIG_HOME", str(Path.home() / ".config")))
            / "uv"
            / "uv.toml",
            Path("/etc/uv/uv.toml"),
        ]
        config_paths.extend(
            Path(directory) / "uv" / "uv.toml"
            for directory in os.environ.get("XDG_CONFIG_DIRS", "/etc/xdg").split(
                os.pathsep
            )
        )
    if config_file := os.environ.get("UV_CONFIG_FILE"):
        config_paths.append(Path(config_file))
    for path in config_paths:
        content = path.read_bytes() if path.is_file() else None
        state.append([str(path), digest(content) if content is not None else None])
        if content is not None:
            configs.append(tomllib.loads(content.decode()))
    for config in configs:
        if (
            config.get("find-links")
            or config.get("keyring-provider", "disabled") != "disabled"
        ):
            return None
        if any(
            item.get("url", "https:").startswith("file:")
            for item in config.get("index", [])
        ):
            return None
        sources = config.get("sources", {}).values()
        if any("path" in item for item in sources):
            return None
    # Directory indexes and find-links may consult mutable external files that
    # are outside the inspected interpreter/project. Leave their checks uncached.
    if (
        os.environ.get("UV_FIND_LINKS")
        or os.environ.get("UV_KEYRING_PROVIDER", "disabled") != "disabled"
    ):
        return None
    for name, value in os.environ.items():
        if (
            name.startswith("UV_")
            and (
                name in ("UV_INDEX", "UV_DEFAULT_INDEX", "UV_EXTRA_INDEX_URL")
                or name.startswith("UV_INDEX_")
                and name != "UV_INDEX_STRATEGY"
            )
            and (value.startswith("file:") or "://" not in value)
        ):
            return None
    uv = shutil.which("uv")
    state.append(stamp(Path(uv)) if uv else None)
    return {
        "files": source,
        "environment": digest(json.dumps(state, separators=(",", ":")).encode()),
    }


try:
    result = fingerprint(Path(sys.argv[1]).resolve(strict=True))
except (OSError, ValueError, TypeError, KeyError):
    result = None
print(json.dumps(result, separators=(",", ":")))
