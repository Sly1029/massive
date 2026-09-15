from __future__ import annotations

from dataclasses import replace
from datetime import timedelta
from typing import Any

import pytest

from massive import Retry, container, execution, retry

_ENVIRONMENT = container("registry.example/python@sha256:" + "0123456789abcdef" * 4)


def test_retry_defaults_and_emission() -> None:
    assert retry(1).as_json() == {
        "maxAttempts": 1,
        "delaySeconds": 10,
        "backoffFactor": 2,
        "maxDelaySeconds": 600,
    }
    assert retry(5, delay=timedelta(0), backoff=1, max_delay=timedelta(days=1)).as_json() == {
        "maxAttempts": 5,
        "delaySeconds": 0,
        "backoffFactor": 1,
        "maxDelaySeconds": 86400,
    }


def test_contract_emits_retry_and_timeout_only_when_set() -> None:
    plain = execution(environment=_ENVIRONMENT)
    assert "retry" not in plain.as_json()
    assert "timeoutSeconds" not in plain.as_json()
    contract = execution(
        environment=_ENVIRONMENT,
        retry=retry(3, delay=timedelta(seconds=30)),
        timeout=timedelta(hours=2),
    )
    assert contract.as_json()["retry"] == {
        "maxAttempts": 3,
        "delaySeconds": 30,
        "backoffFactor": 2,
        "maxDelaySeconds": 600,
    }
    assert contract.as_json()["timeoutSeconds"] == 7200


@pytest.mark.parametrize(
    ("arguments", "message"),
    [
        ({"attempts": 0}, "retry attempts must be an integer from 1 to 100"),
        ({"attempts": 101}, "retry attempts must be an integer from 1 to 100"),
        ({"attempts": True}, "retry attempts must be an integer from 1 to 100"),
        ({"attempts": 2.0}, "retry attempts must be an integer from 1 to 100"),
        ({"attempts": 2, "backoff": 0}, "retry backoff must be an integer from 1 to 10"),
        ({"attempts": 2, "backoff": 11}, "retry backoff must be an integer from 1 to 10"),
        (
            {"attempts": 2, "delay": timedelta(seconds=-1)},
            "retry delay must be whole seconds from 0 to 86400",
        ),
        (
            {"attempts": 2, "delay": timedelta(milliseconds=1500)},
            "retry delay must be whole seconds from 0 to 86400",
        ),
        ({"attempts": 2, "delay": 10}, "retry delay must be whole seconds from 0 to 86400"),
        (
            {"attempts": 2, "max_delay": timedelta(days=1, seconds=1)},
            "retry max_delay must be whole seconds from 0 to 86400",
        ),
        (
            {"attempts": 2, "delay": timedelta(minutes=5), "max_delay": timedelta(minutes=1)},
            "retry max_delay must not be shorter than delay",
        ),
    ],
)
def test_retry_rejects_values_outside_the_contract(arguments: dict[str, Any], message: str) -> None:
    with pytest.raises(ValueError, match=message):
        retry(**arguments)


def test_direct_and_derived_construction_is_validated() -> None:
    with pytest.raises(ValueError, match="retry attempts"):
        Retry(attempts=0, delay=timedelta(0), backoff=1, max_delay=timedelta(0))
    with pytest.raises(ValueError, match="retry max_delay"):
        replace(retry(3), max_delay=timedelta(seconds=1))


@pytest.mark.parametrize(
    "timeout",
    [timedelta(0), timedelta(milliseconds=500), timedelta(days=7, seconds=1), 60],
)
def test_contract_rejects_timeouts_outside_the_contract(timeout: Any) -> None:
    with pytest.raises(ValueError, match="step timeout must be whole seconds from 1 to 604800"):
        execution(environment=_ENVIRONMENT, timeout=timeout)
    with pytest.raises(ValueError, match="step timeout"):
        replace(execution(environment=_ENVIRONMENT), timeout=timeout)


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
