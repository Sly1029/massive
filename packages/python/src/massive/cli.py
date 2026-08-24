from __future__ import annotations

import os
import subprocess
import sys
from pathlib import Path
from typing import NoReturn


def main() -> NoReturn:
    """Hand the active Python environment to the packaged Go control plane."""
    executable_name = "massive.exe" if os.name == "nt" else "massive"
    executable = Path(__file__).with_name("_bin") / executable_name
    if not executable.is_file():
        raise SystemExit(
            "massive: the native control-plane binary is missing; "
            "install a platform wheel supported by massive-workflows"
        )
    environment = os.environ.copy()
    environment.setdefault("MASSIVE_PYTHON", sys.executable)
    arguments = [str(executable), *sys.argv[1:]]
    if os.name != "nt":
        os.execve(executable, arguments, environment)
    raise SystemExit(subprocess.call(arguments, env=environment))


if __name__ == "__main__":
    main()
