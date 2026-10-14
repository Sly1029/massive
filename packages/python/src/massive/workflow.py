# pyright: reportPrivateUsage=false
"""Explicit activities and deterministic CPython asyncio delivery turns."""

from __future__ import annotations

import ast
import asyncio
import inspect
import re
import sys
from collections import deque
from collections.abc import Awaitable, Callable, Generator
from contextlib import contextmanager
from pathlib import Path
from typing import Any, TypeVar, cast, overload

from pydantic import TypeAdapter

from ._step import StepDefinition
from .canonical import JsonValue, canonical_json, sha256_ref
from .context import StepContext
from .contracts import ExecutionContract
from .outcomes import MapItemFailure

I = TypeVar("I")
O = TypeVar("O")


class StepFailed(Exception):
    def __init__(self, failure: MapItemFailure) -> None:
        self.failure = failure
        super().__init__(failure.diagnostic)


class _Scheduler:
    def __init__(self) -> None:
        if sys.implementation.name != "cpython":
            raise RuntimeError("coordinator capability: delivery turns require pinned CPython")
        self.loop = asyncio.SelectorEventLoop()
        self.author_turn = False
        self.fatal: BaseException | None = None

    @contextmanager
    def runtime(self) -> Generator[None, None, None]:
        author_turn = self.author_turn
        self.author_turn = False
        try:
            yield
        finally:
            self.author_turn = author_turn

    def drain(self) -> None:
        # CPython's FIFO ready queue defines quiescence. run_forever installs the
        # normal running-loop context; a stop callback bounds each ready batch.
        # The interpreter realization is pinned and checked before every replay.
        ready = cast(deque[asyncio.Handle], getattr(self.loop, "_ready"))  # noqa: B009 -- absent in typeshed
        while ready:
            self.loop.call_soon(self.loop.stop)
            self.author_turn = True
            try:
                self.loop.run_forever()
            finally:
                self.author_turn = False
            if getattr(self.loop, "_scheduled"):  # noqa: B009 -- absent in typeshed
                raise RuntimeError("coordinator capability: duration waits must run inside an activity")

    def close(self) -> None:
        # Cancellation never drains another author turn or dispatches finalizers.
        for task in asyncio.all_tasks(self.loop):
            task.cancel()
        self.loop.close()


class Workflow:
    """Runner-owned handle; activities are ordinary exported step functions."""

    def __init__(
        self,
        *,
        defaults: ExecutionContract,
        _scheduler: _Scheduler,
        _issue: Callable[[Callable[..., Any], Any, ExecutionContract, str], asyncio.Future[Any]],
        _root: Path,
        _package_id: str,
    ) -> None:
        self.defaults = defaults
        self._scheduler, self._issue = _scheduler, _issue
        self._root, self._package_id = _root, _package_id
        self._keys: set[str] = set()
        self._symbols: dict[str, bool] = {}

    @overload
    async def run(
        self,
        fn: Callable[[StepContext[I]], Awaitable[O]],
        input: I,
        *,
        contract: ExecutionContract | None = None,
        key: str | None = None,
    ) -> O: ...
    @overload
    async def run(
        self,
        fn: Callable[[StepContext[I]], O],
        input: I,
        *,
        contract: ExecutionContract | None = None,
        key: str | None = None,
    ) -> O: ...
    async def run(
        self,
        fn: Callable[[StepContext[I]], O | Awaitable[O]],
        input: I,
        *,
        contract: ExecutionContract | None = None,
        key: str | None = None,
    ) -> O:
        with self._scheduler.runtime():
            from .builder import _symbol_module

            StepDefinition[I, O].from_callable(fn)
            symbol = f"{self._package_id}:{_symbol_module(fn, self._root)}#{fn.__name__}"
            if key is not None and re.fullmatch(r"[A-Za-z0-9_.-]+", key) is None:
                raise ValueError("invocation key must match [A-Za-z0-9_.-]+")
            invocation_key = symbol if key is None else f"{symbol}@{key}"
            if (
                len(invocation_key) > 128
                or re.fullmatch(r"[A-Za-z0-9_.@:#-]+", invocation_key) is None
            ):
                raise ValueError(
                    "invocation identity must be a safe segment of at most 128 characters"
                )
            if symbol in self._symbols and (key is None or not self._symbols[symbol]):
                raise ValueError("repeated symbols require explicit keys on all uses")
            if invocation_key in self._keys:
                raise ValueError("duplicate invocation key")
            self._symbols[symbol] = key is not None
            self._keys.add(invocation_key)
            future = self._issue(fn, input, contract or self.defaults, invocation_key)
        return cast(O, await future)

def resolved_contract(contract: ExecutionContract) -> tuple[dict[str, Any], dict[str, Any]]:
    environment = contract.environment.as_json()
    environment_ref = sha256_ref(canonical_json(environment))
    value = {name: item for name, item in contract.as_json().items() if name != "environment"}
    value["environmentRef"] = environment_ref
    return (
        {"contractRef": sha256_ref(canonical_json(cast(JsonValue, value))), **value},
        {
            "envRef": environment_ref,
            "container": {name: item for name, item in environment.items() if name != "kind"},
        },
    )


def schema_record(annotation: Any, *, mode: str) -> dict[str, str]:
    body = canonical_json(
        cast(JsonValue, TypeAdapter(annotation).json_schema(mode=cast(Any, mode)))
    )
    return {"hash": sha256_ref(body), "canonicalJson": body}


def entry_types(fn: Callable[..., Any]) -> tuple[Any, Any]:
    from typing import get_type_hints

    hints = get_type_hints(fn)
    parameters = list(inspect.signature(fn).parameters)
    if (
        not inspect.iscoroutinefunction(fn)
        or len(parameters) != 2
        or hints.get(parameters[0]) is not Workflow
    ):
        raise TypeError(
            "coordinator requires async def coordinator(wf: Workflow, inputs: Input) -> Output"
        )
    return hints[parameters[1]], hints["return"]


def check_coordinator_operations(fn: Callable[..., Any]) -> None:
    """Cheap authoring checks; replay is still the correctness boundary."""
    tree = ast.parse(inspect.getsource(fn))
    forbidden = {
        "open",
        "input",
        "time",
        "sleep",
        "wait_for",
        "timeout",
        "call_later",
        "call_at",
        "monotonic",
        "perf_counter",
        "now",
        "today",
        "random",
        "randint",
        "randrange",
        "choice",
        "uuid4",
        "urandom",
        "getenv",
        "read_text",
        "read_bytes",
        "write_text",
        "write_bytes",
        "Popen",
        "Thread",
    }
    for node in ast.walk(tree):
        if isinstance(node, ast.Call):
            name = (
                node.func.id
                if isinstance(node.func, ast.Name)
                else (node.func.attr if isinstance(node.func, ast.Attribute) else "")
            )
            if name in forbidden:
                raise TypeError(f"coordinator capability: {name} must run inside an activity")
        if isinstance(node, ast.Attribute) and node.attr == "environ":
            raise TypeError("coordinator capability: environment reads must run inside an activity")
