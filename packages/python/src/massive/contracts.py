from __future__ import annotations

import re
from collections.abc import Mapping
from dataclasses import dataclass
from typing import Literal

from .canonical import JsonValue, canonical_json, sha256_ref

_IMAGE_DIGEST = re.compile(r"^[^@\s]+@sha256:[0-9a-f]{64}$")
_PLATFORM = re.compile(r"^[a-z0-9][a-z0-9._-]*/[a-z0-9][a-z0-9._-]*$")


@dataclass(frozen=True, slots=True)
class ContainerPlan:
    """Canonical, unbuilt environment plan derived from a container recipe.

    An artifact exists only after a materializer commits real bytes or an OCI
    image. This plan deliberately carries no artifact reference.
    """

    identity: str
    value: dict[str, JsonValue]


@dataclass(frozen=True, slots=True)
class ContainerRecipe:
    """Immutable container recipe; resources and secrets belong to ExecutionContract."""

    image: str
    platform: str
    runtime: tuple[str, str] | None = None
    packages: tuple[tuple[str, str], ...] = ()
    build_args: tuple[tuple[str, str], ...] = ()

    def as_json(self) -> dict[str, JsonValue]:
        value: dict[str, JsonValue] = {
            "kind": "container",
            "image": self.image,
            "platform": self.platform,
        }
        if self.runtime is not None:
            value["runtime"] = {"kind": self.runtime[0], "version": self.runtime[1]}
        if self.packages:
            value["packages"] = [
                {"name": name, "version": version} for name, version in self.packages
            ]
        if self.build_args:
            value["buildArgs"] = [
                {"name": name, "value": value} for name, value in self.build_args
            ]
        return value

    def plan(self) -> ContainerPlan:
        value = {
            key: item for key, item in self.as_json().items() if key != "kind"
        }
        return ContainerPlan(identity=sha256_ref(canonical_json(value)), value=value)

    def extend(
        self,
        *,
        runtime: tuple[str, str] | None = None,
        packages: Mapping[str, str] | None = None,
        build_args: Mapping[str, str] | None = None,
    ) -> ContainerRecipe:
        """Compose an overlay without admitting scheduling or secret settings."""
        merged_packages = dict(self.packages)
        if packages is not None:
            merged_packages.update(packages)
        merged_args = dict(self.build_args)
        if build_args is not None:
            merged_args.update(build_args)
        return _container_recipe(
            image=self.image,
            platform=self.platform,
            runtime=self.runtime if runtime is None else runtime,
            packages=merged_packages,
            build_args=merged_args,
        )


@dataclass(frozen=True, slots=True)
class ExecutionContract:
    environment: ContainerRecipe
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


def container(
    image: str,
    *,
    platform: str = "linux/amd64",
    runtime: tuple[str, str] | None = None,
    packages: Mapping[str, str] | None = None,
    build_args: Mapping[str, str] | None = None,
) -> ContainerRecipe:
    return _container_recipe(
        image=image,
        platform=platform,
        runtime=runtime,
        packages={} if packages is None else packages,
        build_args={} if build_args is None else build_args,
    )


def _container_recipe(
    *,
    image: str,
    platform: str,
    runtime: tuple[str, str] | None,
    packages: Mapping[str, str],
    build_args: Mapping[str, str],
) -> ContainerRecipe:
    if not _IMAGE_DIGEST.fullmatch(image):
        raise ValueError("container image must be an immutable image digest reference")
    if not _PLATFORM.fullmatch(platform):
        raise ValueError("container platform must be an os/architecture pair")
    if runtime is not None and (not runtime[0] or not runtime[1]):
        raise ValueError("container runtime must be a non-empty (kind, version) pair")
    return ContainerRecipe(
        image=image,
        platform=platform,
        runtime=runtime,
        packages=_normalized_pairs("package", packages),
        build_args=_normalized_pairs("build argument", build_args),
    )


def _normalized_pairs(label: str, values: Mapping[str, str]) -> tuple[tuple[str, str], ...]:
    normalized: list[tuple[str, str]] = []
    for name, value in values.items():
        if not name or not value:
            raise ValueError(f"container {label} names and values must be non-empty strings")
        normalized.append((name, value))
    return tuple(sorted(normalized, key=lambda pair: pair[0].encode("utf-16-be")))


def execution(
    *,
    environment: ContainerRecipe,
    cpu: str | None = None,
    memory: str | None = None,
    network: Literal["none", "any"] | None = None,
) -> ExecutionContract:
    return ExecutionContract(environment=environment, cpu=cpu, memory=memory, network=network)


# Kept as the execution-contract spelling while recipes become the public
# environment authoring vocabulary.
ContainerEnvironment = ContainerRecipe
