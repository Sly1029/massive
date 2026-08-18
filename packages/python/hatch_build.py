from __future__ import annotations

from pathlib import Path
from typing import Any

from hatchling.builders.hooks.plugin.interface import BuildHookInterface

_SCHEMAS = (
    Path("conformance/schema/step-invocation-descriptor.schema.json"),
    Path("conformance/schema/data-artifact-manifest.schema.json"),
)


class CustomBuildHook(BuildHookInterface):
    """Package the repository's canonical descriptor schema without copying it."""

    def initialize(self, version: str, build_data: dict[str, Any]) -> None:
        root = Path(self.root)
        for schema_path in _SCHEMAS:
            candidates = (root.parents[1] / schema_path, root / schema_path)
            schema = next((candidate for candidate in candidates if candidate.is_file()), None)
            if schema is None:
                raise FileNotFoundError(f"canonical schema {schema_path} is unavailable")
            destination = (
                schema_path.as_posix()
                if self.target_name == "sdist"
                else f"massive/schemas/{schema_path.name}"
            )
            build_data["force_include"][str(schema)] = destination
