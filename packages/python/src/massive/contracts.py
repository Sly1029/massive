from __future__ import annotations

from dataclasses import dataclass
from typing import Literal


@dataclass(frozen=True, slots=True)
class ContainerEnvironment:
    image: str
    command: tuple[str, ...] | None = None
    working_directory: str | None = None

    def as_json(self) -> dict[str, object]:
        value: dict[str, object] = {"kind": "container", "image": self.image}
        if self.command is not None:
            value["command"] = list(self.command)
        if self.working_directory is not None:
            value["workingDirectory"] = self.working_directory
        return value


@dataclass(frozen=True, slots=True)
class ExecutionContract:
    environment: ContainerEnvironment
    cpu: str | None = None
    memory: str | None = None
    network: Literal["none", "any"] | None = None

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
        return value


def container(image: str, *, command: tuple[str, ...] | None = None) -> ContainerEnvironment:
    if not image:
        raise ValueError("container image must not be empty")
    return ContainerEnvironment(image=image, command=command)


def execution(
    *,
    environment: ContainerEnvironment,
    cpu: str | None = None,
    memory: str | None = None,
    network: Literal["none", "any"] | None = None,
) -> ExecutionContract:
    return ExecutionContract(environment=environment, cpu=cpu, memory=memory, network=network)
