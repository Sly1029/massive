from __future__ import annotations

import pytest

from massive import container


def test_container_recipe_normalizes_composed_build_inputs_into_a_canonical_plan() -> None:
    recipe = container(
        "registry.example/python@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
        runtime=("python", "3.12.3"),
        packages={"zeta": "1.0.0", "alpha": "2.0.0"},
        build_args={"UV_COMPILE_BYTECODE": "1", "PIP_DISABLE_PIP_VERSION_CHECK": "1"},
    )

    plan = recipe.plan()

    assert recipe.as_json() == {
        "kind": "container",
        "image": "registry.example/python@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
        "platform": "linux/amd64",
        "runtime": {"kind": "python", "version": "3.12.3"},
        "packages": [
            {"name": "alpha", "version": "2.0.0"},
            {"name": "zeta", "version": "1.0.0"},
        ],
        "buildArgs": [
            {"name": "PIP_DISABLE_PIP_VERSION_CHECK", "value": "1"},
            {"name": "UV_COMPILE_BYTECODE", "value": "1"},
        ],
    }
    assert plan.identity == "sha256:54269435b3020035d98af1a140f31b9d484df4e1652e9f7c035b1f6604de6539"
    assert "kind" not in plan.value


def test_container_recipe_composes_immutable_overlays() -> None:
    base = container(
        "registry.example/python@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
    )

    composed = base.extend(packages={"pydantic": "2.10.6"}).extend(
        build_args={"UV_LINK_MODE": "copy"}
    )

    assert base.as_json() == {
        "kind": "container",
        "image": "registry.example/python@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
        "platform": "linux/amd64",
    }
    assert composed.plan().value["packages"] == [{"name": "pydantic", "version": "2.10.6"}]
    assert composed.plan().identity != base.plan().identity


@pytest.mark.parametrize(
    "image",
    [
        "registry.example/python:3.12",
        "registry.example/python",
        "registry.example/python@sha256:upperCASE0123456789abcdef0123456789abcdef0123456789abcdef",
    ],
)
def test_container_recipe_rejects_mutable_or_malformed_image_references(image: str) -> None:
    with pytest.raises(ValueError, match="immutable image digest"):
        container(image, platform="linux/amd64")
