from __future__ import annotations

import pytest

from massive import container


def test_container_records_invocation_requirements() -> None:
    environment = container(
        "registry.example/python@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
        command=("python", "-m", "worker"),
        working_directory="app",
    )
    assert environment.as_json() == {
        "kind": "container",
        "image": "registry.example/python@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
        "platform": "linux/amd64",
        "command": ["python", "-m", "worker"],
        "workingDirectory": "app",
    }


@pytest.mark.parametrize("image", ["registry.example/python:3.12", "registry.example/python"])
def test_container_recipe_rejects_mutable_image_references(image: str) -> None:
    with pytest.raises(ValueError, match="immutable image digest"):
        container(image)
