# pyright: reportPrivateUsage=false
"""Explicit activity calls driven by Massive's deterministic delivery turns.

The local spike supports these coroutine joins, not an asyncio event loop.
"""

from __future__ import annotations

import inspect
import re
from collections import deque
from collections.abc import AsyncIterator, Awaitable, Callable, Generator, Sequence
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any, Generic, TypeVar, cast, overload

from pydantic import TypeAdapter

from ._step import StepDefinition
from .canonical import JsonValue, canonical_json, sha256_ref
from .context import StepContext
from .contracts import ExecutionContract
from .outcomes import MapItemFailure

I = TypeVar("I")
O = TypeVar("O")
A = TypeVar("A")
B = TypeVar("B")
C = TypeVar("C")


class StepFailed(Exception):
    def __init__(self, failure: MapItemFailure) -> None:
        self.failure = failure
        super().__init__(failure.diagnostic)


@dataclass(eq=False)
class _Future(Generic[O]):
    done: bool = False
    value: O | None = None
    error: BaseException | None = None
    waiters: list[_Task] = field(default_factory=list)
    listeners: list[_Future[None]] = field(default_factory=list)

    def __await__(self) -> Generator[_Future[O], None, O]:
        if not self.done:
            yield self
        if self.error is not None:
            raise self.error
        return cast(O, self.value)


@dataclass(eq=False)
class _Task:
    iterator: Generator[Any, None, Any]
    result: _Future[Any]


class _Scheduler:
    def __init__(self) -> None:
        self.ready: deque[_Task] = deque()
        self.tasks: list[_Task] = []

    def spawn(self, value: Awaitable[O]) -> _Future[O]:
        result: _Future[O] = _Future()
        task = _Task(value.__await__(), result)
        self.tasks.append(task)
        self.ready.append(task)
        return result

    def settle(
        self, future: _Future[Any], value: Any = None, error: BaseException | None = None
    ) -> None:
        if future.done:
            raise RuntimeError("result delivered twice")
        future.done, future.value, future.error = True, value, error
        self.ready.extend(future.waiters)
        future.waiters.clear()
        for listener in future.listeners:
            if not listener.done:
                self.settle(listener)

    def drain(self) -> None:
        while self.ready:
            task = self.ready.popleft()
            try:
                blocked = next(task.iterator)
            except StopIteration as result:
                self.settle(task.result, result.value)
            except BaseException as error:  # noqa: BLE001 -- coroutine boundary
                self.settle(task.result, error=error)
            else:
                if not isinstance(blocked, _Future):
                    raise TypeError(
                        "coordinator capability: await only wf.run, wf.gather, "
                        "or wf.as_completed; asyncio futures and duration waits are unsupported"
                    )
                if blocked.done:
                    self.ready.append(task)
                else:
                    blocked.waiters.append(task)

    def close(self) -> None:
        for task in self.tasks:
            task.iterator.close()


class Workflow:
    """Runner-owned handle; activities are ordinary exported step functions."""

    def __init__(
        self,
        *,
        defaults: ExecutionContract,
        _scheduler: _Scheduler,
        _issue: Callable[[Callable[..., Any], Any, ExecutionContract, str], _Future[Any]],
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
        from .builder import _symbol_module

        StepDefinition[I, O].from_callable(fn)
        symbol = f"{self._package_id}:{_symbol_module(fn, self._root)}#{fn.__name__}"
        if key is not None and re.fullmatch(r"[A-Za-z0-9_.-]+", key) is None:
            raise ValueError("invocation key must match [A-Za-z0-9_.-]+")
        invocation_key = symbol if key is None else f"{symbol}@{key}"
        if len(invocation_key) > 128 or re.fullmatch(r"[A-Za-z0-9_.@:#-]+", invocation_key) is None:
            raise ValueError("invocation identity must be a safe segment of at most 128 characters")
        if symbol in self._symbols and (key is None or not self._symbols[symbol]):
            raise ValueError("repeated symbols require explicit keys on all uses")
        if invocation_key in self._keys:
            raise ValueError("duplicate invocation key")
        self._symbols[symbol] = key is not None
        self._keys.add(invocation_key)
        return cast(O, await self._issue(fn, input, contract or self.defaults, invocation_key))

    @overload
    async def gather(self, items: list[Awaitable[O]], /) -> list[O]: ...
    @overload
    async def gather(self, a: Awaitable[A], /) -> tuple[A]: ...
    @overload
    async def gather(self, a: Awaitable[A], b: Awaitable[B], /) -> tuple[A, B]: ...
    @overload
    async def gather(
        self, a: Awaitable[A], b: Awaitable[B], c: Awaitable[C], /
    ) -> tuple[A, B, C]: ...
    async def gather(self, *items: Any) -> Any:
        as_list = len(items) == 1 and isinstance(items[0], list)
        selected = cast(list[Awaitable[Any]], items[0]) if as_list else list(items)
        futures = [self._scheduler.spawn(item) for item in selected]
        values = [await future for future in futures]
        return values if as_list else tuple(values)

    async def as_completed(self, items: Sequence[Awaitable[O]]) -> AsyncIterator[O]:
        futures = [self._scheduler.spawn(item) for item in items]
        while futures:
            ready = next((future for future in futures if future.done), None)
            if ready is None:
                wake: _Future[None] = _Future()
                for future in futures:
                    future.listeners.append(wake)
                await wake
                for future in futures:
                    future.listeners.remove(wake)
                continue
            futures.remove(ready)
            yield await ready


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
