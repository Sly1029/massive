from __future__ import annotations

from pathlib import Path
from typing import Any

from hatchling.builders.hooks.plugin.interface import BuildHookInterface

_SCHEMA = Path("conformance/schema/step-invocation-descriptor.schema.json")


class CustomBuildHook(BuildHookInterface):
    """Package the repository's canonical descriptor schema without copying it."""

    def initialize(self, version: str, build_data: dict[str, Any]) -> None:
        root = Path(self.root)
        candidates = (root.parents[1] / _SCHEMA, root / _SCHEMA)
        schema = next((candidate for candidate in candidates if candidate.is_file()), None)
        if schema is None:
            raise FileNotFoundError("canonical step invocation descriptor schema is unavailable")
        destination = (
            _SCHEMA.as_posix()
            if self.target_name == "sdist"
            else "massive/schemas/step-invocation-descriptor.schema.json"
        )
        build_data["force_include"][str(schema)] = destination
