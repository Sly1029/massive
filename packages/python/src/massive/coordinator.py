# pyright: reportPrivateUsage=false
"""Isolated CPython coordinator runner: one durable result per scheduler turn."""

from __future__ import annotations

import importlib
import importlib.resources
import json
import os
import sys
from collections.abc import Callable
from datetime import timedelta
from pathlib import Path
from tempfile import TemporaryDirectory
from types import FrameType
from typing import Any, BinaryIO, cast

from jsonschema import Draft202012Validator
from pydantic import TypeAdapter

from ._step import StepDefinition
from .artifact import ArtifactBodyConflictError, ArtifactRuntime, blob_key, put_immutable
from .canonical import JsonValue, canonical_json, sha256_ref
from .context import FatalRunError
from .contracts import Container, ExecutionContract, Retry
from .files import ArtifactFiles
from .outcomes import MapItemFailure
from .runner import (
    SchemaValidator,
    StepInvocationDescriptor,
    _datastore,
    _publication_identity,
    _read_input,
    _source_import_path,
    _source_root,
)
from .workflow import (
    StepFailed,
    Workflow,
    _Future,
    _Scheduler,
    check_coordinator_operations,
    entry_types,
    resolved_contract,
    schema_record,
)

MAX_FRAME = 1024 * 1024


def _validator() -> SchemaValidator:
    resource = importlib.resources.files("massive").joinpath("schemas/coordinator.schema.json")
    if resource.is_file():
        body = resource.read_text()
    else:
        body = (
            Path(__file__).resolve().parents[4] / "conformance/schema/coordinator.schema.json"
        ).read_text()
    return cast(SchemaValidator, Draft202012Validator(json.loads(body)))


def contract_from_json(value: dict[str, Any], environment: dict[str, Any]) -> ExecutionContract:
    container = environment["container"]
    policy = value.get("retry")
    return ExecutionContract(
        environment=Container(
            image=container["image"],
            platform=container["platform"],
            command=tuple(container["command"]) if "command" in container else None,
            working_directory=container.get("workingDirectory"),
        ),
        cpu=value.get("resources", {}).get("cpu"),
        memory=value.get("resources", {}).get("memory"),
        network=value.get("network", {}).get("egress"),
        secrets={item["name"]: item["ref"] for item in value.get("secrets", [])},
        retry=None
        if policy is None
        else Retry(
            attempts=policy["maxAttempts"],
            delay=timedelta(seconds=policy["delaySeconds"]),
            backoff=policy["backoffFactor"],
            max_delay=timedelta(seconds=policy["maxDelaySeconds"]),
        ),
        timeout=timedelta(seconds=value["timeoutSeconds"]) if "timeoutSeconds" in value else None,
    )


def run() -> int:
    # Reserve stdout before importing any author module, including native writes.
    protocol = os.fdopen(os.dup(1), "wb", buffering=0)
    os.dup2(2, 1)
    sys.stdout = sys.stderr
    reader = sys.stdin.buffer
    validator = _validator()
    turn = 0
    scheduler = _Scheduler()

    def write(frame: dict[str, Any]) -> None:
        validator.validate(frame)
        body = canonical_json(cast(JsonValue, frame)).encode() + b"\n"
        if len(body) > MAX_FRAME:
            raise ValueError("coordinator frame exceeds 1 MiB")
        protocol.write(body)

    def read(stream: BinaryIO) -> dict[str, Any]:
        body = stream.readline(MAX_FRAME + 1)
        if not body or len(body) > MAX_FRAME or not body.endswith(b"\n"):
            raise ValueError("interrupted or oversized coordinator frame")
        frame = json.loads(body.decode("utf-8"))
        validator.validate(frame)
        return cast(dict[str, Any], frame)

    try:
        start = read(reader)
        if start["kind"] != "CoordinatorStart":
            raise ValueError("coordinator requires a start frame")
        descriptor = cast(StepInvocationDescriptor, start["descriptor"])
        store = _datastore(descriptor["datastore"])
        defaults = contract_from_json(start["defaults"], start["environment"])
        pending: dict[str, tuple[_Future[Any], TypeAdapter[Any]]] = {}
        with (
            _source_root(descriptor["sourcePackage"], store) as root,
            _source_import_path(root),
            TemporaryDirectory(prefix="massive-coordinator-files-") as scratch,
        ):
            files = ArtifactFiles(store, Path(scratch))
            symbol = descriptor["symbol"]
            fn = getattr(importlib.import_module(symbol["module"]), symbol["export"])
            input_type, output_type = entry_types(fn)
            check_coordinator_operations(fn)

            def audit(event: str, args: tuple[Any, ...]) -> None:
                if scheduler.author_turn and (
                    event == "open"
                    or event.startswith(("socket.", "subprocess.", "os.exec", "os.spawn"))
                ):
                    raise RuntimeError("coordinator capability: I/O must run inside an activity")

            def trace(frame: FrameType, event: str, arg: Any) -> Any:
                if (
                    event == "exception"
                    and frame.f_code.co_filename.startswith(str(root) + os.sep)
                    and isinstance(arg[1], FatalRunError)
                ):
                    scheduler.fatal = arg[1]
                return trace

            sys.addaudithook(audit)
            sys.settrace(trace)
            inputs = TypeAdapter(input_type).validate_json(
                _read_input(descriptor, store), context=files
            )

            def issue(
                fn: Callable[..., Any], value: Any, contract: ExecutionContract, key: str
            ) -> _Future[Any]:
                from .builder import _symbol_module

                step = StepDefinition[Any, Any].from_callable(fn)
                schemas = [
                    schema_record(step.input_type, mode="validation"),
                    schema_record(step.output_type, mode="serialization"),
                ]
                input_body = canonical_json(
                    cast(
                        JsonValue,
                        TypeAdapter(step.input_type).dump_python(
                            TypeAdapter(step.input_type).validate_python(value, context=files),
                            mode="json",
                            context=files,
                        ),
                    )
                ).encode()
                digest = sha256_ref(input_body)
                put_immutable(
                    store,
                    blob_key(digest),
                    input_body,
                    "application/json",
                    ArtifactBodyConflictError,
                )
                resolved, environment = resolved_contract(contract)
                module = _symbol_module(fn, root)
                ref = f"{symbol['packageId']}:{module}#{fn.__name__}"
                future: _Future[Any] = _Future()
                pending[key] = (future, TypeAdapter(step.output_type))
                write(
                    {
                        "kind": "InvokeCommand",
                        "protocolVersion": 1,
                        "turn": turn,
                        "invocationKey": key,
                        "symbolRef": ref,
                        "symbol": {
                            "symbolRef": ref,
                            "packageId": symbol["packageId"],
                            "language": "python",
                            "module": module,
                            "export": fn.__name__,
                        },
                        "contract": resolved,
                        "environment": environment,
                        "schemas": schemas,
                        "inputSchema": schemas[0]["hash"],
                        "outputSchema": schemas[1]["hash"],
                        "input": {
                            "key": blob_key(digest),
                            "hash": digest,
                            "contentType": "application/json",
                        },
                    }
                )
                return future

            wf = Workflow(
                defaults=defaults,
                _scheduler=scheduler,
                _issue=issue,
                _root=root,
                _package_id=symbol["packageId"],
            )
            terminal = scheduler.spawn(fn(wf, inputs))
            while True:
                scheduler.drain()
                if scheduler.fatal is not None:
                    raise scheduler.fatal
                if terminal.done:
                    if terminal.error is not None:
                        raise terminal.error
                    if pending:
                        raise ValueError("coordinator finished with outstanding activities")
                    adapter = TypeAdapter(output_type)
                    body = canonical_json(
                        cast(
                            JsonValue,
                            adapter.dump_python(
                                adapter.validate_python(terminal.value, context=files),
                                mode="json",
                                context=files,
                            ),
                        )
                    ).encode()
                    destination, producer = _publication_identity(descriptor)
                    published = ArtifactRuntime(store).publish_json(destination, producer, body)
                    write(
                        {
                            "kind": "CoordinatorComplete",
                            "protocolVersion": 1,
                            "turn": turn,
                            "output": {
                                "key": published.manifest.key,
                                "hash": published.manifest.hash,
                                "contentType": published.manifest.content_type,
                            },
                        }
                    )
                    return 0
                if not pending:
                    raise ValueError("coordinator quiescent without an activity")
                write({"kind": "CoordinatorQuiescent", "protocolVersion": 1, "turn": turn})
                result = read(reader)
                if result["kind"] != "InvokeResult" or result["turn"] != turn + 1:
                    raise ValueError("coordinator result has invalid delivery turn")
                turn += 1
                future, adapter = pending.pop(result["invocationKey"])
                if "failure" in result:
                    scheduler.settle(
                        future, error=StepFailed(MapItemFailure.model_validate(result["failure"]))
                    )
                else:
                    ref = result["output"]
                    value = store.get(ref["key"]).body
                    if sha256_ref(value) != ref["hash"]:
                        raise ValueError("coordinator result hash mismatch")
                    manifest = json.loads(value)
                    body = store.get(manifest["body"]["key"]).body
                    if sha256_ref(body) != manifest["body"]["hash"]:
                        raise ValueError("coordinator result body hash mismatch")
                    scheduler.settle(future, adapter.validate_json(body, context=files))
    except BaseException as error:  # noqa: BLE001 -- process boundary
        frame: dict[str, Any] = {
            "kind": "CoordinatorFail",
            "protocolVersion": 1,
            "turn": turn,
            "diagnostic": "coordinator authoring or protocol failure",
        }
        # Only explicitly safe fatal messages cross the root diagnostic boundary.
        if isinstance(error, FatalRunError):
            frame["fatalMessage"] = str(error)
            if error.type is not None:
                frame["fatalType"] = error.type
        print(f"massive coordinator: {error}", file=sys.stderr)
        write(frame)
        return 1
    finally:
        sys.settrace(None)
        scheduler.close()
        protocol.close()
