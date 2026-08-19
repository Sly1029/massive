from __future__ import annotations

import hashlib
import importlib.util
import sys
from collections.abc import Generator, Mapping, Sequence
from contextlib import contextmanager, redirect_stdout
from dataclasses import dataclass
from pathlib import Path
from types import ModuleType
from typing import Any, cast

from .builder import GraphBuilder, WorkflowSpec
from .source_package import source_package

_PACKAGE_ID = "python-main"
_SOURCE_INCLUDE = ["*.py"]
_ERROR_EXIT = 2


class FrontendError(Exception):
    """A user-facing frontend diagnostic without an implementation traceback."""


@dataclass(frozen=True, slots=True)
class EntrypointRequest:
    path: Path
    selector: str | None

    @classmethod
    def parse(cls, value: str) -> EntrypointRequest:
        raw_path, separator, raw_selector = value.rpartition("#")
        path_text = raw_path if separator else value
        selector = raw_selector if separator else None
        if not path_text:
            raise FrontendError("workflow entrypoint path must not be empty")
        if selector == "":
            raise FrontendError("workflow export selector must not be empty")
        path = Path(path_text).expanduser().resolve()
        if not path.is_file():
            raise FrontendError(f'workflow entrypoint "{path}" is not a file')
        if path.suffix != ".py":
            raise FrontendError(f'workflow entrypoint "{path}" must be a Python file')
        return cls(path=path, selector=selector)

    @property
    def display(self) -> str:
        return str(self.path) if self.selector is None else f"{self.path}#{self.selector}"


@dataclass(frozen=True, slots=True)
class GraphSelection:
    graph: GraphBuilder[Any, Any, Any]
    export_name: str


@dataclass(frozen=True, slots=True)
class EmitResult:
    specification: WorkflowSpec

    @property
    def canonical_json(self) -> str:
        return self.specification.to_json()


def emit(request: EntrypointRequest) -> EmitResult:
    """Load exactly one exported graph and emit its portable workflow specification."""
    with redirect_stdout(sys.stderr), _import_workflow(request) as module:
        selection = _select_graph(request, module)
        source = source_package(
            root=request.path.parent,
            include=_SOURCE_INCLUDE,
            package_id=_PACKAGE_ID,
        )
        specification = selection.graph.emit(source=source)
    return EmitResult(specification=specification)


def main(argv: Sequence[str] | None = None) -> int:
    arguments = tuple(sys.argv[1:] if argv is None else argv)
    if len(arguments) != 2 or arguments[0] != "emit":
        _write_diagnostic("usage: massive-python-frontend emit path/to/workflow.py[#export]")
        return _ERROR_EXIT
    try:
        result = emit(EntrypointRequest.parse(arguments[1]))
    except FrontendError as error:
        _write_diagnostic(str(error))
        return _ERROR_EXIT
    except Exception as error:  # noqa: BLE001 -- this is the process boundary.
        _write_diagnostic(str(error))
        return _ERROR_EXIT
    sys.stdout.write(result.canonical_json)
    return 0


def _write_diagnostic(message: str) -> None:
    sys.stderr.write(f"massive-python-frontend: {message}\n")


def _select_graph(request: EntrypointRequest, module: ModuleType) -> GraphSelection:
    graphs: dict[str, GraphBuilder[Any, Any, Any]] = {}
    for name, value in vars(module).items():
        if isinstance(value, GraphBuilder):
            graphs[name] = cast(GraphBuilder[Any, Any, Any], value)
    if request.selector is not None:
        selected = graphs.get(request.selector)
        if selected is None:
            raise FrontendError(f'workflow entrypoint "{request.display}" does not export a GraphBuilder')
        return GraphSelection(graph=selected, export_name=request.selector)
    candidates = sorted(graphs)
    if len(candidates) == 1:
        export_name = candidates[0]
        return GraphSelection(graph=graphs[export_name], export_name=export_name)
    if not candidates:
        raise FrontendError(
            f'workflow entrypoint "{request.path}" exports no GraphBuilder values '
            "(candidates: none)"
        )
    raise FrontendError(
        f'workflow entrypoint "{request.path}" is ambiguous; GraphBuilder candidates: '
        + ", ".join(candidates)
    )


@contextmanager
def _import_workflow(request: EntrypointRequest) -> Generator[ModuleType, None, None]:
    module_name = _isolated_module_name(request.path)
    specification = importlib.util.spec_from_file_location(module_name, request.path)
    if specification is None or specification.loader is None:
        raise FrontendError(f'could not load workflow entrypoint "{request.path}"')
    module = importlib.util.module_from_spec(specification)
    original_path = sys.path.copy()
    original_modules = sys.modules.copy()
    try:
        sys.path.insert(0, str(request.path.parent))
        sys.modules[module_name] = module
        try:
            specification.loader.exec_module(module)
        except Exception as error:
            raise FrontendError(
                f'could not import workflow entrypoint "{request.path}": {error}'
            ) from error
        yield module
    finally:
        sys.path[:] = original_path
        _restore_modules(original_modules, request.path.parent, module_name)


def _isolated_module_name(path: Path) -> str:
    digest = hashlib.sha256(str(path).encode("utf-8")).hexdigest()
    return f"_massive_workflow_{digest}"


def _restore_modules(
    original_modules: Mapping[str, ModuleType], root: Path, module_name: str
) -> None:
    for name, module in tuple(sys.modules.items()):
        if name == module_name or _module_is_within(module, root):
            previous = original_modules.get(name)
            if previous is None:
                sys.modules.pop(name, None)
            else:
                sys.modules[name] = previous


def _module_is_within(module: ModuleType | None, root: Path) -> bool:
    file_name = getattr(module, "__file__", None)
    if file_name is None:
        return False
    try:
        Path(file_name).resolve().relative_to(root.resolve())
    except ValueError:
        return False
    return True
