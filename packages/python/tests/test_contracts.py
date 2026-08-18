from __future__ import annotations

import pytest

from massive import container, execution


def test_container_recipe_composes_invocation_fields_into_a_canonical_plan() -> None:
    base = container(
        "registry.example/python@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
    )

    recipe = base.extend(command=("python", "-m", "worker"), working_directory="app")
    plan = recipe.plan()

    assert base.as_json() == {
        "kind": "container",
        "image": "registry.example/python@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
        "platform": "linux/amd64",
    }
    assert plan.canonical_json == (
        '{"command":["python","-m","worker"],'
        '"image":"registry.example/python@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",'
        '"platform":"linux/amd64","workingDirectory":"app"}'
    )
    assert plan.identity == "sha256:955ff79a6c9999658eaa09f710b629834d0556f70506d79c71deddf302bf4e4e"


def test_resources_and_secret_refs_do_not_change_container_plan_identity() -> None:
    recipe = container(
        "registry.example/python@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
    )

    first = execution(environment=recipe, cpu="1", secrets={"TOKEN": "secret/first"})
    second = execution(environment=recipe, memory="4Gi", secrets={"TOKEN": "secret/second"})

    assert first.as_json() != second.as_json()
    assert first.environment.plan().identity == second.environment.plan().identity


@pytest.mark.parametrize("image", ["registry.example/python:3.12", "registry.example/python"])
def test_container_recipe_rejects_mutable_image_references(image: str) -> None:
    with pytest.raises(ValueError, match="immutable image digest"):
        container(image)
