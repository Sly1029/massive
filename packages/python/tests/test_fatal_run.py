"""Fatal signals across real runners, sibling processes, journals and exit hooks."""

from __future__ import annotations

import json
import os
import socket
import subprocess
import sys
from hashlib import sha256
from pathlib import Path

import pytest

from massive import FatalRunError, NonRetryableError

ROOT = Path(__file__).resolve().parents[3]


@pytest.fixture(scope="module")
def fatal_cli(tmp_path_factory: pytest.TempPathFactory) -> Path:
    binary = tmp_path_factory.mktemp("fatal-cli") / "massive"
    subprocess.run(["go", "build", "-o", str(binary), "./cmd/massive"], cwd=ROOT, check=True)
    return binary


def test_fatal_constructor_bounds_text_without_throwing() -> None:
    error = FatalRunError("🚀" * 1025, "t" * 65)
    assert isinstance(error, NonRetryableError)
    assert str(error) == "🚀" * 1024
    assert error.type == "t" * 64
    assert FatalRunError("Safe message").type is None

    class Unprintable:
        def __str__(self) -> str:
            raise RuntimeError("cannot format")

    for value in [None, 7, Unprintable()]:
        error = FatalRunError(value, value)
        assert str(error) == "Fatal run failure"
        assert error.type is None


@pytest.mark.parametrize("collect,group", [(False, False), (True, False), (True, True)])
def test_fatal_aborts_retries_collection_and_real_siblings(
    tmp_path: Path, fatal_cli: Path, collect: bool, group: bool
) -> None:
    with socket.socket() as listener:
        listener.bind(("127.0.0.1", 0))
        listener.listen()
        listener.settimeout(30)
        port = listener.getsockname()[1]
        child = (
            "import socket,time; from pathlib import Path; "
            f"s=socket.create_connection(('127.0.0.1',{port})); "
            "s.sendall(b'ready'); "
            f"Path({str(tmp_path / 'ready')!r}).touch(); time.sleep(60)"
        )
        workflow = tmp_path / "workflow.py"
        workflow.write_text(
            f"""from pathlib import Path
import subprocess, sys, time
from massive import FatalRunError, GraphBuilder, MapItemOutcome, RunOutcome, StepContext, container, execution, retry
STATE = Path({str(tmp_path)!r})

def stop(ctx: StepContext[int]) -> int:
    (STATE / ('attempt-' + str(ctx.inputs) + '-' + str(ctx.invocation.attempt))).touch()
    if ctx.inputs == 0:
        if {collect!r}:
            deadline = time.monotonic() + 20
            while not (STATE / 'ready').exists():
                if time.monotonic() > deadline: raise RuntimeError('handshake deadline')
                time.sleep(.01)
        fatal = FatalRunError('Input is invalid', 'input.invalid')
        if {group!r}:
            raise ExceptionGroup('SECRET group', [RuntimeError('SECRET sibling'), ExceptionGroup('SECRET nested', [fatal])])
        raise fatal from RuntimeError('SECRET chained cause')
    subprocess.Popen([sys.executable, '-c', {child!r}])
    time.sleep(60)
    (STATE / 'sibling-completed').touch()
    return ctx.inputs

def after(ctx: StepContext[list[MapItemOutcome[int]]]) -> int:
    (STATE / 'downstream').touch()
    return len(ctx.inputs)

def after_step(ctx: StepContext[int]) -> int:
    (STATE / 'downstream').touch()
    return ctx.inputs

def notify(ctx: StepContext[RunOutcome]) -> None:
    (STATE / 'outcome.json').write_text(ctx.inputs.model_dump_json())

def items(ctx: StepContext[list[int]]) -> list[int]: return ctx.inputs

defaults = execution(environment=container('example.invalid/runner@sha256:' + '0'*64), retry=retry(3))
plain = GraphBuilder(name='fatal-step', input_type=int, output_type=int, defaults=defaults)
plain.edge_from(plain.start).to(plain.add(stop)).to(plain.add(after_step)).to_end(plain.end)
plain.on_exit(notify)
graph = GraphBuilder(name='fatal-map', input_type=list[int], output_type=int, defaults=defaults)
source = graph.add(items)
graph.edge_from(graph.start).to(source)
outcomes = graph.map(source, stop, id='stop', item_failures='collect', concurrency=2)
graph.edge_from(outcomes).to(graph.add(after)).to_end(graph.end)
graph.on_exit(notify)
"""
        )
        command = [
            str(fatal_cli),
            "run",
            str(workflow) + ("#graph" if collect else "#plain"),
            "--store",
            str(tmp_path / "store"),
            "--project",
            "fatal-conformance",
            "--run-id",
            "fatal-test",
            "--input",
            json.dumps(list(range(12)) if collect else 0),
            "--json",
        ]
        with subprocess.Popen(
            command,
            cwd=ROOT,
            env={**os.environ, "MASSIVE_PYTHON": sys.executable},
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            text=True,
        ) as process:
            connection = listener.accept()[0] if collect else None
            try:
                stdout, stderr = process.communicate(timeout=30)
                assert process.returncode == 69, stderr
                if connection is not None:
                    connection.settimeout(5)
                    assert connection.recv(5) == b"ready"
                    assert connection.recv(1) == b""  # The real descendant was cancelled.
            finally:
                if connection is not None:
                    connection.close()
        expected = {
            "type": "input.invalid",
            "message": "Input is invalid",
            "nodeId": "stop",
        }
        run = json.loads(stdout)
        assert run["status"] == "failed"
        assert run["failure"] == expected
        project = "sha256-" + sha256(b"fatal-conformance").hexdigest()
        journal_text = (
            tmp_path / "store" / "projects" / project / "runs/fatal-test/run-manifest.json"
        ).read_text()
        journal = json.loads(journal_text)
        assert journal["failure"] == expected
        assert "SECRET" not in journal_text
        assert "SECRET" not in stderr
        assert len(list(tmp_path.glob("attempt-0-*"))) == 1
        assert not list(tmp_path.glob("attempt-2-*"))
        assert not (tmp_path / "sibling-completed").exists()
        assert not (tmp_path / "downstream").exists()
        outcome = json.loads((tmp_path / "outcome.json").read_text())
        assert outcome["status"] == "failed"
        assert outcome["failed_node"] == "stop"
        assert outcome["fatal"] == {"message": "Input is invalid", "type": "input.invalid"}
        if collect:
            mapper = next(step for step in journal["steps"] if step["nodeId"] == "stop")
            assert mapper["status"] == "failed"
            assert mapper["items"][0]["status"] == "failed"
            assert mapper["items"][1]["status"] == "cancelled"
            assert all(item["status"] == "not-started" for item in mapper["items"][2:])


def test_fatal_exit_hook_does_not_change_a_successful_run(tmp_path: Path, fatal_cli: Path) -> None:
    workflow = tmp_path / "workflow.py"
    workflow.write_text(
        """from massive import FatalRunError, GraphBuilder, RunOutcome, StepContext, container, execution

def good(ctx: StepContext[int]) -> int: return ctx.inputs

def notify(ctx: StepContext[RunOutcome]) -> None:
    raise FatalRunError('Upstream source unavailable', 'source.unavailable')

graph = GraphBuilder(name='fatal-hook', input_type=int, output_type=int,
    defaults=execution(environment=container('example.invalid/runner@sha256:' + '0'*64)))
graph.edge_from(graph.start).to(graph.add(good)).to_end(graph.end)
graph.on_exit(notify)
"""
    )
    result = subprocess.run(
        [
            str(fatal_cli),
            "run",
            str(workflow),
            "--store",
            str(tmp_path / "store"),
            "--project",
            "fatal-conformance",
            "--run-id",
            "hook-test",
            "--input",
            "42",
            "--json",
        ],
        cwd=ROOT,
        env={**os.environ, "MASSIVE_PYTHON": sys.executable},
        check=False,
        capture_output=True,
        text=True,
        timeout=30,
    )
    assert result.returncode == 0, result.stderr
    run = json.loads(result.stdout)
    assert run["status"] == "succeeded"
    assert run["result"] == 42
    assert "failure" not in run
    assert run["exitHook"]["status"] == "failed"
