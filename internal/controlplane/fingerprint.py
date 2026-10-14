"""Best-effort cache eligibility and fingerprints; no author imports or third-party libraries.

Executed by the control plane with python -I -c. Failure disables caching, leaving
all diagnostics and validation to the normal source reader and preflight.
"""

import hashlib
import json
import os
from pathlib import Path
import shutil
import sys
import tomllib

from massive.source_package import selected_files


def digest(data):
    return "sha256:" + hashlib.sha256(data).hexdigest()


def stamp(path):
    info = path.stat()
    return [str(path), str(path.resolve()), info.st_mode, info.st_size,
            info.st_mtime_ns, info.st_ctime_ns, info.st_dev, info.st_ino]


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
    include = tuple(project.get("tool", {}).get("massive", {}).get("source", {}).get("include", ["*.py"]))
    files = selected_files(root, (*include, "pyproject.toml", "uv.lock"))
    source = [{"path": name, "hash": digest(path.read_bytes())}
              for name, path in sorted(files.items(), key=lambda item: item[0].encode("utf-16-be"))]
    lock_file = root / "uv.lock"
    if lock_file.is_file():
        lock = tomllib.loads(lock_file.read_text())
        for package in lock.get("package", []):
            location = package.get("source", {})
            if any(key in location for key in ("directory", "editable", "path")):
                return None
            if "virtual" in location and location["virtual"] != ".":
                return None
    if any("path" in item for item in project.get("tool", {}).get("uv", {}).get("sources", {}).values()):
        return None

    state = [digest(lock_file.read_bytes()) if lock_file.is_file() else None, sys.version, sys.implementation.cache_tag, sys.prefix, sys.base_prefix,
             sys.path, stamp(Path(sys.executable)), stamp(Path(sys._base_executable)),
             digest(config.read_bytes())]
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
        if not path.is_relative_to(prefix) and not path.is_relative_to(Path(sys.base_prefix)):
            return None
        for directory, names, filenames in os.walk(path, followlinks=False):
            names[:] = sorted(name for name in names if name != "__pycache__")
            directory_path = Path(directory)
            if directory_path in seen:
                names[:] = []
                continue
            seen.add(directory_path)
            # Directory mtimes change when Python writes bytecode; the sorted
            # listing detects additions/removals without false bytecode misses.
            state.append([str(directory_path), names, sorted(name for name in filenames
                          if not name.endswith((".pyc", ".pyo")))])
            for name in names:
                if (directory_path / name).is_symlink():
                    return None
            for name in sorted(filenames):
                if name.endswith((".pyc", ".pyo")):
                    continue
                file = directory_path / name
                if file.is_symlink():
                    return None
                if name.endswith(".egg-link"):
                    return None
                if name.endswith(".pth") and (name != "_virtualenv.pth" or file.read_text().strip() != "import _virtualenv"):
                    return None
                state.append(stamp(file))
                if directory_path.name.endswith((".dist-info", ".egg-info")):
                    content = file.read_bytes()
                    state.append(digest(content))
                    if name == "direct_url.json" and json.loads(content).get("dir_info", {}).get("editable"):
                        return None
    # The probe checks unselected root modules for shadowing and ancestor
    # pyprojects for uv workspaces. uv also reads project/user configuration.
    state.append(sorted([path.name, path.is_file(), (path / "__init__.py").is_file()]
                        for path in root.iterdir() if path.name != "__pycache__"))
    for ancestor in (root, *root.parents):
        for name in ("pyproject.toml", "uv.toml"):
            path = ancestor / name
            state.append([str(path), digest(path.read_bytes()) if path.is_file() else None])
    user_config = Path(os.environ.get("XDG_CONFIG_HOME", str(Path.home() / ".config"))) / "uv" / "uv.toml"
    state.append([str(user_config), digest(user_config.read_bytes()) if user_config.is_file() else None])
    uv = shutil.which("uv")
    state.append(stamp(Path(uv)) if uv else None)
    return {"files": source, "environment": digest(json.dumps(state, separators=(",", ":")).encode())}


try:
    result = fingerprint(Path(sys.argv[1]).resolve(strict=True))
except (OSError, ValueError, TypeError, KeyError):
    result = None
print(json.dumps(result, separators=(",", ":")))
