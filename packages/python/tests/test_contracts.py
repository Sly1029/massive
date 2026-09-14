from __future__ import annotations

from dataclasses import replace

import pytest

from massive import container, execution


def test_derived_contract_accepts_secret_mappings_without_mutating_its_base() -> None:
    base = execution(
        environment=container("registry.example/python@sha256:" + "0123456789abcdef" * 4),
        cpu="2",
        secrets={"BASE_TOKEN": "base"},
    )
    refs = {"Z_TOKEN": "last", "A_TOKEN": "first"}
    derived = replace(base, secrets=refs)
    refs["A_TOKEN"] = "changed"
    assert derived.as_json()["secrets"] == [
        {"name": "A_TOKEN", "ref": "first"},
        {"name": "Z_TOKEN", "ref": "last"},
    ]
    assert base.as_json()["secrets"] == [{"name": "BASE_TOKEN", "ref": "base"}]
    assert derived.as_json()["resources"] == {"cpu": "2"}
    with pytest.raises(ValueError, match="secret names and refs"):
        replace(base, secrets={"TOKEN": ""})


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
