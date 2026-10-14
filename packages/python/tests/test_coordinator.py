from __future__ import annotations

import json
import os
import shutil
import signal
import subprocess
import sys
import time
from datetime import datetime
from pathlib import Path
from typing import Any

import pytest

ROOT = Path(__file__).resolve().parents[3]
PROJECT = "massive/coordinator-functional"


@pytest.fixture(scope="module")
def coordinator_cli(tmp_path_factory: pytest.TempPathFactory) -> Path:
    binary = tmp_path_factory.mktemp("coordinator-cli") / "massive"
    subprocess.run(["go", "build", "-o", str(binary), "./cmd/massive"], cwd=ROOT, check=True)
    return binary


def setup(
    tmp_path: Path, *, blocked: list[str] | None = None, delays: dict[str, float] | None = None
) -> tuple[Path, Path, Path, list[dict[str, Any]]]:
    source = tmp_path / "source"
    source.mkdir()
    shutil.copy(Path(__file__).parent / "fixtures/coordinator_workflow.py", source / "workflow.py")
    control = tmp_path / "control"
    control.mkdir()
    (control / "settings.json").write_text(
        json.dumps({"blocked": blocked or [], "delays": delays or {}})
    )
    store = tmp_path / "store"
    jobs = [
        {"name": name, "control": str(control), "value": index}
        for index, name in enumerate(("one", "two", "three"))
    ]
    return source, control, store, jobs


def argv(
    binary: Path,
    source: Path,
    store: Path,
    jobs: list[dict[str, Any]],
    run: str,
    *,
    resume: bool = False,
    workers: int = 3,
) -> list[str]:
    args = [
        str(binary),
        "run",
        "--store",
        str(store),
        "--project",
        PROJECT,
        "--json",
        "--progress",
        "none",
        "--workers",
        str(workers),
    ]
    return (
        [*args, "--resume", run, "--workers-quiescent"]
        if resume
        else [*args, str(source / "workflow.py"), "--input", json.dumps(jobs), "--run-id", run]
    )


def env(**extra: str) -> dict[str, str]:
    return {**os.environ, "MASSIVE_PYTHON": sys.executable, **extra}


def run(args: list[str], **extra: str) -> subprocess.CompletedProcess[str]:
    return subprocess.run(
        args, env=env(**extra), check=False, capture_output=True, text=True, timeout=40
    )


def journal(store: Path, run: str) -> dict[str, Any]:
    paths = list(store.glob(f"projects/*/runs/{run}/manifest.json"))
    if not paths:
        paths = list(store.glob(f"projects/*/runs/{run}/run-manifest.json"))
    return json.loads(paths[0].read_text())


def record(store: Path, run: str) -> dict[str, Any]:
    return json.loads((store / journal(store, run)["coordinatorRecord"]["key"]).read_text())


def wait_for(predicate: Any, process: subprocess.Popen[str], *, seconds: float = 20) -> Any:
    deadline = time.monotonic() + seconds
    while time.monotonic() < deadline:
        if process.poll() is not None:
            stdout, stderr = process.communicate()
            pytest.fail(f"CLI stopped before handshake: {stdout}\n{stderr}")
        try:
            value = predicate()
        except (IndexError, FileNotFoundError, json.JSONDecodeError):
            value = None
        if value:
            return value
        time.sleep(0.01)
    pytest.fail("coordinator handshake timed out")


def kill_and_quiesce(process: subprocess.Popen[str], control: Path) -> None:
    process.kill()  # Real abrupt SIGKILL of the owning CLI.
    process.communicate(timeout=10)
    assert process.returncode == -signal.SIGKILL
    groups = {json.loads(path.read_text())["pgid"] for path in control.glob("started-*.json")}
    for group in groups:
        try:
            os.killpg(group, signal.SIGKILL)
        except ProcessLookupError:
            pass
    deadline = time.monotonic() + 5
    while time.monotonic() < deadline:
        active = []
        for path in Path("/proc").glob("[0-9]*/stat"):
            try:
                fields = path.read_text().rsplit(")", 1)[1].split()
                if int(fields[2]) in groups and fields[0] != "Z":
                    active.append(path)
            except (OSError, IndexError, ValueError):
                pass
        if not active:
            return
        time.sleep(0.01)
    pytest.fail("owned worker groups did not quiesce")


def start(
    binary: Path, source: Path, store: Path, jobs: list[dict[str, Any]], name: str, **extra: str
) -> subprocess.Popen[str]:
    return subprocess.Popen(
        argv(binary, source, store, jobs, name),
        env=env(**extra),
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        text=True,
    )


def completed(journal: dict[str, Any]) -> dict[str, Any]:
    return {step["nodeId"]: step for step in journal["steps"] if step["status"] == "succeeded"}


@pytest.mark.skipif(sys.platform != "linux", reason="SIGKILL quiescence evidence uses Linux /proc")
@pytest.mark.parametrize("pattern", ["normal", "as_completed"])
@pytest.mark.parametrize("first", ["one", "three"])
def test_sigkill_resume_delivery_turns_preserve_publications(
    coordinator_cli: Path, tmp_path: Path, pattern: str, first: str
) -> None:
    source, control, store, jobs = setup(
        tmp_path,
        blocked=[*(f"a-{name}" for name in ("one", "two", "three") if name != first), f"b-{first}"],
    )
    process = start(coordinator_cli, source, store, jobs, "crash", COORDINATOR_VARIANT=pattern)
    try:
        wait_for(
            lambda: (
                completed(journal(store, "crash")) and list(control.glob(f"started-b-{first}-*.json"))
            ),
            process,
        )
        before = completed(journal(store, "crash"))
        old_history = record(store, "crash")
        # Another coordinator cannot acquire the run, even when asserting quiescence.
        owned = run(argv(coordinator_cli, source, store, jobs, "crash", resume=True))
        assert owned.returncode != 0 and "already owned" in owned.stderr
        kill_and_quiesce(process, control)
    finally:
        if process.poll() is None:
            kill_and_quiesce(process, control)
    # The API forces operators to acknowledge the documented fencing precondition.
    precondition = run(
        [
            arg
            for arg in argv(coordinator_cli, source, store, jobs, "crash", resume=True)
            if arg != "--workers-quiescent"
        ]
    )
    assert precondition.returncode != 0 and "workers-quiescent" in precondition.stderr
    # Actual live completion timing changes; cached turns still use their history.
    (control / "settings.json").write_text(
        json.dumps({"delays": {"a-two": 0.25, "a-three": 0, "b-one": 0.15}})
    )
    resumed = run(
        argv(coordinator_cli, source, store, jobs, "crash", resume=True),
        COORDINATOR_VARIANT=pattern,
    )
    assert resumed.returncode == 0, resumed.stderr
    after = journal(store, "crash")
    for key, step in before.items():
        assert next(entry for entry in after["steps"] if entry["nodeId"] == key) == step
    history = record(store, "crash")
    assert history["deliveries"][: len(old_history["deliveries"])] == old_history["deliveries"]
    assert history["turns"][: len(old_history["turns"]) - 1] == old_history["turns"][:-1]
    reference = run(
        argv(coordinator_cli, source, store, jobs, "reference"), COORDINATOR_VARIANT=pattern
    )
    assert reference.returncode == 0, reference.stderr
    actual, expected = json.loads(resumed.stdout)["result"], json.loads(reference.stdout)["result"]
    assert actual == expected if pattern == "normal" else sorted(actual) == sorted(expected)
    assert sorted(actual) == [11, 12, 13]
    # Completed activity one was published exactly once, with the same attempt.
    assert len(list(control.glob(f"effect-a-{first}-*"))) == 1
    assert history["deliveries"][0].endswith(f"@a-{first}")


@pytest.mark.parametrize("variant", ["changed", "reordered", "new", "early"])
def test_replay_divergence_stops_before_new_activity_effects(
    coordinator_cli: Path, tmp_path: Path, variant: str
) -> None:
    source, control, store, jobs = setup(tmp_path)
    initial = run(argv(coordinator_cli, source, store, jobs, "diverge"))
    assert initial.returncode == 0, initial.stderr
    effects = {path.name: path.read_bytes() for path in control.glob("effect-*")}
    resumed = run(
        argv(coordinator_cli, source, store, jobs, "diverge", resume=True),
        COORDINATOR_VARIANT=variant,
    )
    assert resumed.returncode != 0 and "divergence" in resumed.stderr, resumed.stderr
    assert effects == {path.name: path.read_bytes() for path in control.glob("effect-*")}


@pytest.mark.parametrize("damage", ["missing", "corrupt"])
@pytest.mark.parametrize("target", ["activity", "final", "result", "input"])
def test_completed_artifact_damage_is_never_a_cache_miss(
    coordinator_cli: Path, tmp_path: Path, damage: str, target: str
) -> None:
    source, control, store, jobs = setup(tmp_path)
    assert run(argv(coordinator_cli, source, store, jobs, "damage")).returncode == 0
    before = list(control.glob("effect-*"))
    state = journal(store, "damage")
    history = record(store, "damage")
    keys = {
        "activity": state["steps"][0]["attempts"][0]["output"]["body"]["key"],
        "final": json.loads(
            (store / history["final"]["output"]["key"]).read_text()
        )["body"]["key"],
        "result": state["result"]["key"],
        "input": history["start"]["descriptor"]["input"]["artifact"]["key"],
    }
    body = store / keys[target]
    body.unlink() if damage == "missing" else body.write_bytes(b"999999")
    resumed = run(argv(coordinator_cli, source, store, jobs, "damage", resume=True))
    diagnostic = "input artifact" if target == "input" else "verification"
    assert resumed.returncode != 0 and diagnostic in resumed.stderr
    assert list(control.glob("effect-*")) == before


@pytest.mark.parametrize("changed", ["source", "input", "store"])
def test_resume_identity_changes_stop_before_effects(
    coordinator_cli: Path, tmp_path: Path, changed: str
) -> None:
    source, control, store, jobs = setup(tmp_path)
    assert run(argv(coordinator_cli, source, store, jobs, "identity")).returncode == 0
    effects = {path.name: path.read_bytes() for path in control.glob("effect-*")}
    args = argv(coordinator_cli, source, store, jobs, "identity", resume=True)
    diagnostic = "identity changed"
    if changed == "source":
        with (source / "workflow.py").open("a") as file:
            file.write("\n# Different source identity.\n")
        args.append(str(source / "workflow.py"))
    elif changed == "input":
        args.extend(["--input", json.dumps([{**jobs[0], "value": 99}])])
    else:
        moved = tmp_path / "moved-store"
        shutil.copytree(store, moved)
        args[args.index(str(store))] = str(moved)
        diagnostic = "store location changed"
    rejected = run(args)
    assert rejected.returncode != 0 and diagnostic in rejected.stderr, rejected.stderr
    assert effects == {path.name: path.read_bytes() for path in control.glob("effect-*")}


@pytest.mark.parametrize("behavior", ["orphan", "fail", "fatal", "hang", "invalid"])
def test_activity_failure_and_deadline_paths(
    coordinator_cli: Path, tmp_path: Path, behavior: str
) -> None:
    source, _control, store, jobs = setup(tmp_path)
    jobs[0]["behavior"] = behavior
    extra = {"COORDINATOR_DEADLINE": "2"} if behavior == "hang" else {}
    result = run(argv(coordinator_cli, source, store, jobs, "failures"), **extra)
    state = journal(store, "failures")
    first = next(step for step in state["steps"] if step["nodeId"].endswith("@a-one"))
    if behavior in ("orphan", "fail"):
        assert result.returncode == 0, result.stderr
        assert json.loads(result.stdout)["result"][0] == (11 if behavior == "orphan" else 10)
        assert len(first["attempts"]) == (2 if behavior == "orphan" else 3)
        assert first["attempts"][0]["status"] == "failed"
        assert "output" not in first["attempts"][0]
        if behavior == "orphan":
            assert (
                store
                / f"projects/{state['projectKey']}/runs/failures/steps/{first['nodeId']}/1/output-manifest.json"
            ).exists()
        replay = run(argv(coordinator_cli, source, store, jobs, "failures", resume=True))
        assert replay.returncode == 0, replay.stderr
        assert journal(store, "failures")["steps"] == state["steps"]
    else:
        assert result.returncode != 0, result.stderr
        assert state["status"] == "failed"
        assert len(first["attempts"]) == 1
        if behavior == "fatal":
            assert state["failure"]["message"] == "safe coordinator test fatal"
        if behavior == "hang":
            assert "deadline" in result.stderr


def test_installed_conformance_shape(coordinator_cli: Path, tmp_path: Path) -> None:
    source = ROOT / "conformance/workflows/python-coordinator/workflow.py"
    result = run(
        [
            str(coordinator_cli),
            "run",
            str(source),
            "--input",
            '{"value":3}',
            "--store",
            str(tmp_path),
            "--project",
            PROJECT,
            "--run-id",
            "conformance",
            "--json",
            "--progress",
            "none",
        ]
    )
    assert result.returncode == 0, result.stderr
    value = json.loads(result.stdout)["result"]
    assert value["report"] == {"parts": ["Doubled=6", "Squared=9"]}
    assert value["outcomes"][1]["failure"]["attempts"] == 2
    assert value["recovery"] == {"kind": "doubled", "value": 6}


@pytest.mark.parametrize("pattern", ["normal", "as_completed"])
def test_real_runner_response_batching_preserves_every_turn(
    coordinator_cli: Path, tmp_path: Path, pattern: str
) -> None:
    source, _, store, jobs = setup(tmp_path, delays={"a-one": 0.15, "a-two": 0, "a-three": 0.05})
    original = run(argv(coordinator_cli, source, store, jobs, "batch"), COORDINATOR_VARIANT=pattern)
    assert original.returncode == 0, original.stderr
    history = record(store, "batch")
    responses = []
    for turn, key in enumerate(history["deliveries"], 1):
        responses.append({**history["results"][key], "turn": turn})
    frames = [history["start"], *responses]
    batch = subprocess.run(
        [sys.executable, "-I", "-m", "massive.runner", "--coordinator"],
        input="".join(json.dumps(frame) + "\n" for frame in frames),
        capture_output=True,
        check=False,
        text=True,
        timeout=20,
        env=env(COORDINATOR_VARIANT=pattern),
    )
    assert batch.returncode == 0, batch.stderr
    batch_output = [json.loads(line) for line in batch.stdout.splitlines()]
    from jsonschema import Draft202012Validator

    validator = Draft202012Validator(
        json.loads((ROOT / "conformance/schema/coordinator.schema.json").read_text())
    )
    for frame in [*frames, *batch_output]:
        validator.validate(frame)
    assert not validator.is_valid({**responses[0], "protocolVersion": 2})
    assert not validator.is_valid({**responses[0], "unknown": True})
    assert not validator.is_valid(
        {**responses[0], "failure": {"attempts": 1, "kind": "error", "diagnostic": "bounded"}}
    )
    streamed = subprocess.Popen(
        [sys.executable, "-I", "-m", "massive.runner", "--coordinator"],
        stdin=subprocess.PIPE,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        text=True,
        env=env(COORDINATOR_VARIANT=pattern),
    )
    assert streamed.stdin is not None and streamed.stdout is not None
    streamed.stdin.write(json.dumps(history["start"]) + "\n")
    streamed.stdin.flush()
    streamed_output = []
    response_index = 0
    while True:
        line = streamed.stdout.readline()
        assert line, "runner stopped before terminal frame"
        frame = json.loads(line)
        streamed_output.append(frame)
        if frame["kind"] == "CoordinatorQuiescent":
            streamed.stdin.write(json.dumps(responses[response_index]) + "\n")
            streamed.stdin.flush()
            response_index += 1
        if frame["kind"] == "CoordinatorComplete":
            break
    streamed.stdin.close()
    assert streamed.wait(timeout=10) == 0
    assert batch_output == streamed_output
    commands = [frame for frame in batch_output if frame["kind"] == "InvokeCommand"]
    assert commands == [command for turn in history["turns"] for command in turn["commands"]]
    # A succeeded replay also proves terminal-result comparison and zero new attempts.
    resumed = run(
        argv(coordinator_cli, source, store, jobs, "batch", resume=True), COORDINATOR_VARIANT=pattern
    )
    assert resumed.returncode == 0, resumed.stderr
    assert json.loads(resumed.stdout)["result"] == json.loads(original.stdout)["result"]


@pytest.mark.skipif(sys.platform != "linux", reason="SIGKILL quiescence evidence uses Linux /proc")
@pytest.mark.parametrize("retry_enabled", [True, False])
def test_sigkill_retry_backoff_and_disabled_attempts(
    coordinator_cli: Path, tmp_path: Path, retry_enabled: bool
) -> None:
    source, control, store, jobs = setup(tmp_path, blocked=[] if retry_enabled else ["a-one"])
    fixture = source / "workflow.py"
    text = fixture.read_text().replace(
        "retry(3, delay=timedelta(0))",
        "retry(3, delay=timedelta(seconds=2))" if retry_enabled else "retry(1, delay=timedelta(0))",
    )
    fixture.write_text(text)
    jobs = jobs[:1]
    jobs[0]["behavior"] = "flaky" if retry_enabled else "ok"
    process = start(coordinator_cli, source, store, jobs, "retry")
    try:
        if retry_enabled:
            wait_for(lambda: record(store, "retry")["retryAt"], process)
        else:
            wait_for(lambda: list(control.glob("started-a-one-*.json")), process)
        old = journal(store, "retry")["steps"][0]["attempts"]
        backoff = record(store, "retry")["retryAt"]
        kill_and_quiesce(process, control)
    finally:
        if process.poll() is None:
            kill_and_quiesce(process, control)
    (control / "settings.json").write_text("{}")
    resumed = run(argv(coordinator_cli, source, store, jobs, "retry", resume=True))
    assert resumed.returncode == 0, resumed.stderr
    step = journal(store, "retry")["steps"][0]
    assert len(step["attempts"]) == (2 if retry_enabled else 1)
    assert step["attempts"][0]["attempt"] == old[0]["attempt"] == 1
    assert step["attempts"][0]["status"] == "failed"
    assert "output" not in step["attempts"][0]
    assert json.loads(resumed.stdout)["result"] == ([11] if retry_enabled else [10])
    if retry_enabled:
        assert step["attempts"][0] == old[0]
        first = json.loads((control / "started-a-one-1.json").read_text())
        second = json.loads((control / "started-a-one-2.json").read_text())
        assert first["idempotencyKey"] == second["idempotencyKey"]
        assert second["startedAt"] >= datetime.fromisoformat(next(iter(backoff.values()))).timestamp()
    else:
        assert not list(control.glob("started-a-one-2.json"))


def test_argo_capability_diagnostic(coordinator_cli: Path, tmp_path: Path) -> None:
    source, _, _, _ = setup(tmp_path)
    result = run(
        [
            str(coordinator_cli),
            "build",
            str(source / "workflow.py"),
            "--output",
            str(tmp_path / "bundle"),
            "--namespace",
            "workflows",
            "--service-account",
            "massive",
            "--artifact-store",
            "store",
        ]
    )
    assert result.returncode != 0 and "Argo capability" in result.stderr
    assert not (tmp_path / "bundle").exists()


def test_coordinator_runtime_capabilities_and_sticky_fatal(
    coordinator_cli: Path, tmp_path: Path
) -> None:
    source, control, store, jobs = setup(tmp_path)
    fixture = source / "workflow.py"
    original = fixture.read_text()
    for name, body, diagnostic in [
        ("clock", "return [int(time.time())]", "capability"),
        ("io", "return [int(Path(inputs[0].control).read_text())]", "capability"),
        ("asyncio", "import asyncio\n    await asyncio.sleep(0)\n    return []", "capability"),
        (
            "timer",
            "from asyncio import sleep as pause\n    await pause(0.01)\n    return []",
            "capability",
        ),
        (
            "sticky",
            'try:\n        raise FatalRunError("sticky safe cause")\n    except FatalRunError:\n        pass\n    return []',
            "sticky safe cause",
        ),
    ]:
        fixture.write_text(
            original[: original.index("async def coordinator")]
            + "async def coordinator(wf: Workflow, inputs: list[Job]) -> list[int]:\n    "
            + body
            + "\n"
        )
        result = run(argv(coordinator_cli, source, store, jobs, name))
        assert result.returncode != 0 and diagnostic in result.stderr, result.stderr
        assert not list(control.glob("effect-*"))
        if name == "sticky":
            assert journal(store, name)["failure"]["message"] == "sticky safe cause"


@pytest.mark.parametrize("bad_key", ["", "bad:key", "bad@key", "bad#key", "x" * 129])
def test_explicit_key_validation_before_dispatch(
    coordinator_cli: Path, tmp_path: Path, bad_key: str
) -> None:
    source, control, store, jobs = setup(tmp_path)
    fixture = source / "workflow.py"
    original = fixture.read_text()
    fixture.write_text(
        original[: original.index("async def coordinator")]
        + "async def coordinator(wf: Workflow, inputs: list[Job]) -> list[int]:\n"
        + f"    return [await wf.run(stage_a, inputs[0], key={bad_key!r})]\n"
    )
    result = run(argv(coordinator_cli, source, store, jobs, "bad-key"))
    assert result.returncode != 0
    assert not list(control.glob("started-*"))


@pytest.mark.parametrize("keys", [(None, "second"), ("first", None), ("same", "same")])
def test_all_or_nothing_and_duplicate_keys(
    coordinator_cli: Path, tmp_path: Path, keys: tuple[str | None, str | None]
) -> None:
    source, control, store, jobs = setup(tmp_path)
    fixture = source / "workflow.py"
    original = fixture.read_text()
    fixture.write_text(
        original[: original.index("async def coordinator")]
        + "async def coordinator(wf: Workflow, inputs: list[Job]) -> list[int]:\n"
        + f"    first = await wf.run(stage_a, inputs[0], key={keys[0]!r})\n"
        + f"    return [first, await wf.run(stage_a, inputs[1], key={keys[1]!r})]\n"
    )
    result = run(argv(coordinator_cli, source, store, jobs, "keys"))
    assert result.returncode != 0
    assert len(list(control.glob("started-*"))) == 1


@pytest.mark.skipif(sys.platform != "linux", reason="SIGKILL quiescence evidence uses Linux /proc")
def test_resume_retains_original_deadline(coordinator_cli: Path, tmp_path: Path) -> None:
    source, control, store, jobs = setup(tmp_path, blocked=["a-one"])
    process = start(coordinator_cli, source, store, jobs[:1], "deadline", COORDINATOR_DEADLINE="2")
    try:
        wait_for(lambda: list(control.glob("started-a-one-*.json")), process)
        old_deadline = record(store, "deadline")["deadline"]
        kill_and_quiesce(process, control)
    finally:
        if process.poll() is None:
            kill_and_quiesce(process, control)
    time.sleep(2)
    (control / "settings.json").write_text("{}")
    resumed = run(argv(coordinator_cli, source, store, jobs, "deadline", resume=True))
    assert resumed.returncode != 0 and "deadline" in resumed.stderr
    assert record(store, "deadline")["deadline"] == old_deadline
    assert not list(control.glob("started-a-one-2.json"))


def test_run_wide_worker_budget(coordinator_cli: Path, tmp_path: Path) -> None:
    source, control, store, jobs = setup(tmp_path, blocked=["a-one"])
    process = subprocess.Popen(
        argv(coordinator_cli, source, store, jobs, "budget", workers=1),
        env=env(),
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        text=True,
    )
    try:
        wait_for(lambda: list(control.glob("started-a-one-*.json")), process)
        wait_for(lambda: len(journal(store, "budget")["steps"]) == 3, process)
        assert len(list(control.glob("started-*"))) == 1
        assert sum(bool(step["attempts"]) for step in journal(store, "budget")["steps"]) == 1
        (control / "release-a-one").touch()
        stdout, stderr = process.communicate(timeout=20)
        assert process.returncode == 0, stderr
        assert json.loads(stdout)["result"] == [11, 12, 13]
    finally:
        if process.poll() is None:
            kill_and_quiesce(process, control)


def test_recovered_failures_require_coordinator_journal_kind(
    coordinator_cli: Path, tmp_path: Path
) -> None:
    source = ROOT / "conformance/workflows/python-coordinator/workflow.py"
    result = run(
        [
            str(coordinator_cli),
            "run",
            str(source),
            "--input",
            '{"value":3}',
            "--store",
            str(tmp_path),
            "--project",
            PROJECT,
            "--run-id",
            "kind",
            "--json",
            "--progress",
            "none",
        ]
    )
    assert result.returncode == 0, result.stderr
    inspect_args = [
        str(coordinator_cli),
        "inspect",
        "kind",
        "--store",
        str(tmp_path),
        "--project",
        PROJECT,
        "--json",
    ]
    assert run(inspect_args).returncode == 0
    state = journal(tmp_path, "kind")
    state["runKind"] = "graph"
    del state["coordinatorRecord"]
    path = next(tmp_path.glob("projects/*/runs/kind/run-manifest.json"))
    path.write_text(json.dumps(state))
    rejected = run(inspect_args)
    assert rejected.returncode != 0 and "successful run has unsuccessful step" in rejected.stderr
    state["schemaVersion"], state["encoding"] = 6, "json-v6"
    path.write_text(json.dumps(state))
    obsolete = run(inspect_args)
    assert obsolete.returncode != 0 and "rebuild" in obsolete.stderr


@pytest.mark.skipif(sys.platform != "linux", reason="SIGKILL quiescence evidence uses Linux /proc")
def test_resume_event_sequences_are_monotonic(coordinator_cli: Path, tmp_path: Path) -> None:
    source, control, store, jobs = setup(tmp_path, blocked=["a-two", "a-three", "b-one"])
    original_events, resumed_events = tmp_path / "original.ndjson", tmp_path / "resumed.ndjson"
    process = subprocess.Popen(
        [*argv(coordinator_cli, source, store, jobs, "events"), "--events", str(original_events)],
        env=env(),
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        text=True,
    )
    try:
        wait_for(
            lambda: (
                completed(journal(store, "events")) and list(control.glob("started-b-one-*.json"))
            ),
            process,
        )
        kill_and_quiesce(process, control)
    finally:
        if process.poll() is None:
            kill_and_quiesce(process, control)
    (control / "settings.json").write_text("{}")
    resumed = run(
        [
            *argv(coordinator_cli, source, store, jobs, "events", resume=True),
            "--events",
            str(resumed_events),
        ]
    )
    assert resumed.returncode == 0, resumed.stderr
    before = [json.loads(line) for line in original_events.read_text().splitlines()]
    after = [json.loads(line) for line in resumed_events.read_text().splitlines()]
    sequences = [event["sequence"] for event in [*before, *after]]
    assert sequences == sorted(set(sequences))
    # Find the actual completed key from the original durable delivery sequence.
    cached_key = record(store, "events")["deliveries"][0]
    assert not [
        event
        for event in after
        if event.get("nodeId") == cached_key and event["type"] == "step.attempt.started"
    ]
    assert after[-1]["type"] == "run.finished"
