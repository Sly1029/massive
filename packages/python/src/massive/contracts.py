from __future__ import annotations

import re
from collections.abc import Mapping
from dataclasses import dataclass
from typing import Literal

from .canonical import JsonValue, canonical_json, sha256_ref

_IMAGE_DIGEST = re.compile(r"^[^@\s]+@sha256:[0-9a-f]{64}$")
_PLATFORM = re.compile(r"^[a-z0-9][a-z0-9._-]*/[a-z0-9][a-z0-9._-]*$")


@dataclass(frozen=True, slots=True)
class ContainerInvocationPlan:
    """Canonical runtime selection for an already-built container image."""

    identity: str
    canonical_json: str


@dataclass(frozen=True, slots=True)
class ContainerRecipe:
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

    def plan(self) -> ContainerInvocationPlan:
        value = {key: item for key, item in self.as_json().items() if key != "kind"}
        encoded = canonical_json(value)
        return ContainerInvocationPlan(identity=sha256_ref(encoded), canonical_json=encoded)

    def extend(
        self,
        *,
        command: tuple[str, ...] | None = None,
        working_directory: str | None = None,
    ) -> ContainerRecipe:
        """Compose deterministic invocation overlays without policy fields."""
        return _container_recipe(
            image=self.image,
            platform=self.platform,
            command=self.command if command is None else command,
            working_directory=(
                self.working_directory if working_directory is None else working_directory
            ),
        )


@dataclass(frozen=True, slots=True)
class ExecutionContract:
    environment: ContainerRecipe
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
) -> ContainerRecipe:
    return _container_recipe(
        image=image,
        platform=platform,
        command=command,
        working_directory=working_directory,
    )


def _container_recipe(
    *,
    image: str,
    platform: str,
    command: tuple[str, ...] | None,
    working_directory: str | None,
) -> ContainerRecipe:
    if not _IMAGE_DIGEST.fullmatch(image):
        raise ValueError("container image must be an immutable image digest reference")
    if not _PLATFORM.fullmatch(platform):
        raise ValueError("container platform must be an os/architecture pair")
    if command is not None and not all(command):
        raise ValueError("container command values must be non-empty strings")
    if working_directory is not None and not working_directory:
        raise ValueError("container working directory must not be empty")
    return ContainerRecipe(
        image=image,
        platform=platform,
        command=command,
        working_directory=working_directory,
    )


def execution(
    *,
    environment: ContainerRecipe,
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


ContainerEnvironment = ContainerRecipe
