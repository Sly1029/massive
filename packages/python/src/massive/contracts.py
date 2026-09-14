from __future__ import annotations

import re
from collections.abc import Mapping
from dataclasses import dataclass
from typing import Literal

from .canonical import JsonValue

_IMAGE_DIGEST = re.compile(r"^[^@\s]+@sha256:[0-9a-f]{64}$")
_PLATFORM = re.compile(r"^[a-z0-9][a-z0-9._-]*/[a-z0-9][a-z0-9._-]*$")


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
class ExecutionContract:
    environment: Container
    cpu: str | None = None
    memory: str | None = None
    network: Literal["none", "any"] | None = None
    secrets: tuple[tuple[str, str], ...] = ()

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
            value["secrets"] = [{"name": name, "ref": ref} for name, ref in self.secrets]
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
) -> ExecutionContract:
    secret_pairs = (
        ()
        if secrets is None
        else tuple(
            sorted(
                secrets.items(),
                key=lambda pair: (pair[0].encode("utf-16-be"), pair[1].encode("utf-16-be")),
            )
        )
    )
    if any(not name or not ref for name, ref in secret_pairs):
        raise ValueError("secret names and refs must be non-empty strings")
    return ExecutionContract(
        environment=environment,
        cpu=cpu,
        memory=memory,
        network=network,
        secrets=secret_pairs,
    )
