from __future__ import annotations

import re
from collections.abc import Mapping
from dataclasses import dataclass, field
from datetime import timedelta
from types import MappingProxyType
from typing import Literal

from .canonical import JsonValue

_IMAGE_DIGEST = re.compile(r"^[^@\s]+@sha256:[0-9a-f]{64}$")
_PLATFORM = re.compile(r"^[a-z0-9][a-z0-9._-]*/[a-z0-9][a-z0-9._-]*$")
_SECOND = timedelta(seconds=1)


@dataclass(frozen=True, slots=True)
class Container:
    """An immutable image selection and invocation overlay, not a build recipe."""

    image: str
    platform: str
    command: tuple[str, ...] | None = None
    working_directory: str | None = None

    def as_json(self) -> dict[str, JsonValue]:
        value: dict[str, JsonValue] = {
            "kind": "container",
            "image": self.image,
            "platform": self.platform,
        }
        if self.command is not None:
            value["command"] = list(self.command)
        if self.working_directory is not None:
            value["workingDirectory"] = self.working_directory
        return value


@dataclass(frozen=True, slots=True)
class Retry:
    """Run a step up to ``attempts`` times.

    The delay before attempt ``n >= 2`` is ``min(delay * backoff ** (n - 2), max_delay)``.
    """

    attempts: int
    delay: timedelta
    backoff: int
    max_delay: timedelta

    def __post_init__(self) -> None:
        if type(self.attempts) is not int or not 1 <= self.attempts <= 100:
            raise ValueError("retry attempts must be an integer from 1 to 100")
        if type(self.backoff) is not int or not 1 <= self.backoff <= 10:
            raise ValueError("retry backoff must be an integer from 1 to 10")
        for name, delay in (("delay", self.delay), ("max_delay", self.max_delay)):
            if not _whole_seconds(delay, 0, 86400):
                raise ValueError(f"retry {name} must be whole seconds from 0 to 86400")
        if self.max_delay < self.delay:
            raise ValueError("retry max_delay must not be shorter than delay")

    def as_json(self) -> dict[str, JsonValue]:
        return {
            "maxAttempts": self.attempts,
            "delaySeconds": self.delay // _SECOND,
            "backoffFactor": self.backoff,
            "maxDelaySeconds": self.max_delay // _SECOND,
        }


@dataclass(frozen=True, slots=True)
class ExecutionContract:
    environment: Container
    cpu: str | None = None
    memory: str | None = None
    network: Literal["none", "any"] | None = None
    secrets: Mapping[str, str] = field(default_factory=dict[str, str])
    retry: Retry | None = None
    timeout: timedelta | None = None

    def __post_init__(self) -> None:
        if any(not name or not ref for name, ref in self.secrets.items()):
            raise ValueError("secret names and refs must be non-empty strings")
        if self.timeout is not None and not _whole_seconds(self.timeout, 1, 604800):
            raise ValueError("step timeout must be whole seconds from 1 to 604800")
        refs = dict(
            sorted(
                self.secrets.items(),
                key=lambda pair: (pair[0].encode("utf-16-be"), pair[1].encode("utf-16-be")),
            )
        )
        object.__setattr__(self, "secrets", MappingProxyType(refs))

    def as_json(self) -> dict[str, object]:
        value: dict[str, object] = {"environment": self.environment.as_json()}
        resources = {
            key: resource
            for key, resource in (("cpu", self.cpu), ("memory", self.memory))
            if resource is not None
        }
        if resources:
            value["resources"] = resources
        if self.network is not None:
            value["network"] = {"egress": self.network}
        if self.secrets:
            value["secrets"] = [{"name": name, "ref": ref} for name, ref in self.secrets.items()]
        if self.retry is not None:
            value["retry"] = self.retry.as_json()
        if self.timeout is not None:
            value["timeoutSeconds"] = self.timeout // _SECOND
        return value


def container(
    image: str,
    *,
    platform: str = "linux/amd64",
    command: tuple[str, ...] | None = None,
    working_directory: str | None = None,
) -> Container:
    if not _IMAGE_DIGEST.fullmatch(image):
        raise ValueError("container image must be an immutable image digest reference")
    if not _PLATFORM.fullmatch(platform):
        raise ValueError("container platform must be an os/architecture pair")
    if command is not None and not all(command):
        raise ValueError("container command values must be non-empty strings")
    if working_directory is not None and not working_directory:
        raise ValueError("container working directory must not be empty")
    return Container(
        image=image,
        platform=platform,
        command=command,
        working_directory=working_directory,
    )


def execution(
    *,
    environment: Container,
    cpu: str | None = None,
    memory: str | None = None,
    network: Literal["none", "any"] | None = None,
    secrets: Mapping[str, str] | None = None,
    retry: Retry | None = None,
    timeout: timedelta | None = None,
) -> ExecutionContract:
    return ExecutionContract(
        environment=environment,
        cpu=cpu,
        memory=memory,
        network=network,
        secrets={} if secrets is None else secrets,
        retry=retry,
        timeout=timeout,
    )


def retry(
    attempts: int,
    *,
    delay: timedelta = timedelta(seconds=10),
    backoff: int = 2,
    max_delay: timedelta = timedelta(minutes=10),
) -> Retry:
    """Retry a failed step attempt; ``retry(1)`` runs the step once without retries."""
    return Retry(attempts=attempts, delay=delay, backoff=backoff, max_delay=max_delay)


def _whole_seconds(value: timedelta, minimum: int, maximum: int) -> bool:
    return (
        type(value) is timedelta and not value % _SECOND and minimum <= value // _SECOND <= maximum
    )
