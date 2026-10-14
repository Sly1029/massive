"""Typed, portable workflow authoring for Massive."""

from importlib import import_module
from typing import TYPE_CHECKING, Any

# This lightweight module is eager because its factory shares the module name.
# Python binds imported submodules on their parent; keep the public factory callable.
from .source_package import SourcePackage, source_package

if TYPE_CHECKING:
    from .workflow import StepFailed, Workflow
    from .builder import (
        DEFAULT_MAP_CONCURRENCY,
        MAX_MAP_CONCURRENCY,
        CaseHandle,
        DecisionHandle,
        EdgePath,
        GraphBuilder,
        NodeHandle,
        WorkflowSpec,
    )
    from .canonical import JsonValue, canonical_json, sha256_ref
    from .context import FatalRunError, InvocationContext, NonRetryableError, StepContext
    from .contracts import (
        Container,
        ExecutionContract,
        Retry,
        container,
        execution,
        retry,
    )
    from .files import ArtifactFiles, Blob, Tree
    from .identity import MapItemIdentity, MapItemScopeFrame
    from .outcome import RunOutcome
    from .outcomes import MapItemFailed, MapItemFailure, MapItemOutcome, MapItemSucceeded

__all__ = [
    "DEFAULT_MAP_CONCURRENCY",
    "MAX_MAP_CONCURRENCY",
    "ArtifactFiles",
    "Blob",
    "CaseHandle",
    "Container",
    "DecisionHandle",
    "EdgePath",
    "ExecutionContract",
    "FatalRunError",
    "GraphBuilder",
    "InvocationContext",
    "JsonValue",
    "MapItemFailed",
    "MapItemFailure",
    "MapItemIdentity",
    "MapItemOutcome",
    "MapItemScopeFrame",
    "MapItemSucceeded",
    "NodeHandle",
    "NonRetryableError",
    "Retry",
    "RunOutcome",
    "SourcePackage",
    "StepContext",
    "Tree",
    "WorkflowSpec",
    "Workflow",
    "StepFailed",
    "canonical_json",
    "container",
    "execution",
    "retry",
    "sha256_ref",
    "source_package",
]

_EXPORT_MODULES = {
    "Workflow": "workflow",
    "StepFailed": "workflow",
    "DEFAULT_MAP_CONCURRENCY": "builder",
    "MAX_MAP_CONCURRENCY": "builder",
    "CaseHandle": "builder",
    "DecisionHandle": "builder",
    "EdgePath": "builder",
    "GraphBuilder": "builder",
    "NodeHandle": "builder",
    "WorkflowSpec": "builder",
    "JsonValue": "canonical",
    "canonical_json": "canonical",
    "sha256_ref": "canonical",
    "InvocationContext": "context",
    "MapItemIdentity": "identity",
    "MapItemScopeFrame": "identity",
    "FatalRunError": "context",
    "NonRetryableError": "context",
    "StepContext": "context",
    "Container": "contracts",
    "ExecutionContract": "contracts",
    "Retry": "contracts",
    "container": "contracts",
    "execution": "contracts",
    "retry": "contracts",
    "ArtifactFiles": "files",
    "Blob": "files",
    "Tree": "files",
    "RunOutcome": "outcome",
    "MapItemFailed": "outcomes",
    "MapItemFailure": "outcomes",
    "MapItemOutcome": "outcomes",
    "MapItemSucceeded": "outcomes",
}


def __getattr__(name: str) -> Any:
    module_name = _EXPORT_MODULES.get(name)
    if module_name is None:
        raise AttributeError(f"module {__name__!r} has no attribute {name!r}")
    module = import_module(f".{module_name}", __name__)
    value = getattr(module, name)
    globals()[name] = value
    return value


def __dir__() -> list[str]:
    return sorted(set(globals()) | set(__all__))
