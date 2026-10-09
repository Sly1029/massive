"""Maps that collect item failures, from emission through real execution."""

from __future__ import annotations

import json
import os
import subprocess
import sys
from hashlib import sha256
from pathlib import Path
from typing import Any

import pytest
from pydantic import BaseModel, TypeAdapter, ValidationError

from massive import (
    GraphBuilder,
    MapItemFailed,
    MapItemFailure,
    MapItemOutcome,
    MapItemSucceeded,
    StepContext,
    container,
    execution,
    source_package,
)

REPOSITORY = Path(__file__).resolve().parents[3]
WORKFLOW = REPOSITORY / "conformance/workflows/python-map-outcomes"
SPEC = REPOSITORY / "conformance/fixtures/specs/python-map-outcomes/workflow-spec.json"
DEFAULTS = execution(
    environment=container(
        "example.invalid/map-outcomes@sha256:"
        "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
    )
)


class Result(BaseModel):
    value: int


def load(context: StepContext[Result]) -> list[Result]:
    return [context.inputs]


def increment(context: StepContext[Result]) -> Result:
    return Result(value=context.inputs.value + 1)


def count_outcomes(context: StepContext[list[MapItemOutcome[Result]]]) -> int:
    return len(context.inputs)


def total(context: StepContext[list[Result]]) -> int:
    return sum(result.value for result in context.inputs)


def _collecting_graph() -> tuple[GraphBuilder[Result, int], Any]:
    graph = GraphBuilder(name="collecting", input_type=Result, output_type=int, defaults=DEFAULTS)
    items = graph.add(load)
    outcomes = graph.map(items, increment, id="increment", item_failures="collect")
    graph.edge_from(graph.start).to(items)
    return graph, outcomes


def _emit(graph: GraphBuilder[Any, Any]) -> dict[str, Any]:
    return graph.emit(
        source=source_package(
            root=Path(__file__).parent, include=[Path(__file__).name], package_id="python-tests"
        )
    ).value  # pyright: ignore[reportReturnType]


def test_collecting_map_emits_the_policy_and_an_outcome_list_schema() -> None:
    graph, outcomes = _collecting_graph()
    graph.edge_from(outcomes).to(graph.add(count_outcomes)).to_end(graph.end)

    emitted = _emit(graph)
    node = next(node for node in emitted["graph"]["nodes"] if node["kind"] == "map")
    consumer = next(node for node in emitted["graph"]["nodes"] if node["id"] == "count_outcomes")

    assert node["itemFailures"] == "collect"
    assert consumer["inputSchema"] == node["outputSchema"]
    output = emitted["schemas"][node["outputSchema"]]
    assert output["items"] == {"$ref": "#/$defs/MapItemOutcome_Result_"}
    assert output["$defs"]["MapItemSucceeded_Result_"]["properties"]["value"] == {
        "$ref": "#/$defs/Result"
    }
    assert output["$defs"]["MapItemFailure"]["properties"]["kind"]["enum"] == [
        "error",
        "killed",
        "non-retryable",
        "timeout",
    ]


def test_default_map_omits_the_policy_from_the_ir() -> None:
    graph = GraphBuilder(name="failing", input_type=Result, output_type=int, defaults=DEFAULTS)
    items = graph.add(load)
    results = graph.map(items, increment, id="increment")
    graph.edge_from(graph.start).to(items)
    graph.edge_from(results).to(graph.add(total)).to_end(graph.end)

    node = next(node for node in _emit(graph)["graph"]["nodes"] if node["kind"] == "map")

    assert "itemFailures" not in node


def test_collecting_map_output_cannot_feed_a_plain_list_consumer() -> None:
    graph, outcomes = _collecting_graph()

    with pytest.raises(TypeError, match="incompatible input type"):
        graph.edge_from(outcomes).to(graph.add(total))  # pyright: ignore[reportArgumentType]


def test_map_rejects_an_unknown_item_failure_policy() -> None:
    graph = GraphBuilder(name="unknown", input_type=Result, output_type=int, defaults=DEFAULTS)
    items = graph.add(load)

    with pytest.raises(ValidationError, match="item_failures"):
        graph.map(items, increment, id="increment", item_failures="ignore")  # pyright: ignore[reportCallIssue, reportArgumentType]


def test_outcomes_decode_as_a_discriminated_generic() -> None:
    adapter = TypeAdapter(list[MapItemOutcome[Result]])

    decoded = adapter.validate_json(
        '[{"status":"succeeded","value":{"value":2}},'
        '{"failure":{"attempts":3,"diagnostic":"step-timeout (timed out after 2s)","kind":"timeout"},'
        '"status":"failed"}]'
    )

    assert decoded == [
        MapItemSucceeded[Result](status="succeeded", value=Result(value=2)),
        MapItemFailed(
            status="failed",
            failure=MapItemFailure(
                attempts=3, diagnostic="step-timeout (timed out after 2s)", kind="timeout"
            ),
        ),
    ]
    with pytest.raises(ValidationError):
        adapter.validate_json(
            '[{"status":"failed","failure":{"attempts":0,"diagnostic":"","kind":"error"}}]'
        )
    with pytest.raises(ValidationError):
        adapter.validate_json('[{"status":"succeeded","value":{"value":2},"failure":null}]')


def test_go_compiler_accepts_the_emitted_outcome_schema_and_rejects_a_mismatch(
    tmp_path: Path,
) -> None:
    accepted = json.loads(SPEC.read_text())
    result = _compile(accepted, tmp_path / "accepted")
    assert result.returncode == 0, result.stderr
    plan = json.loads((tmp_path / "accepted/workflow-plan.json").read_text())
    assert (
        next(node for node in plan["graph"]["nodes"] if node["kind"] == "map")["itemFailures"]
        == "collect"
    )

    # Dropping the policy leaves an outcome list that is not a list of items.
    rejected = json.loads(SPEC.read_text())
    for node in rejected["graph"]["nodes"]:
        node.pop("itemFailures", None)
    result = _compile(rejected, tmp_path / "rejected")
    assert result.returncode != 0
    assert "map outputSchema must be an array whose items exactly equal itemOutputSchema" in (
        result.stderr
    )


def test_collecting_map_keeps_every_item_outcome_through_real_failures(tmp_path: Path) -> None:
    store = tmp_path / "store"
    tasks = [
        {"name": "ok", "behavior": "ok"},
        {"name": "flaky", "behavior": "flaky"},
        {"name": "raise", "behavior": "raise"},
        {"name": "refuse", "behavior": "refuse"},
        {"name": "segfault", "behavior": "segfault"},
        {"name": "sigkill", "behavior": "sigkill"},
        {"name": "hang", "behavior": "hang"},
    ]

    run = _run(store, "outcomes", {"tasks": tasks})

    assert run["status"] == "succeeded"
    assert json.loads((store / run["resultKey"]).read_text()) == {
        "scanned": [{"attempt": 1, "name": "ok"}, {"attempt": 2, "name": "flaky"}],
        "failed": [
            {
                "attempts": 2,
                "diagnostic": "step-execution-failure (exit 66): raise cannot be parsed",
                "kind": "error",
                "name": "raise",
            },
            {
                "attempts": 1,
                "diagnostic": "non-retryable-step-failure (exit 67): refuse is not supported",
                "kind": "non-retryable",
                "name": "refuse",
            },
            {
                "attempts": 2,
                "diagnostic": "runner-killed (signal)",
                "kind": "killed",
                "name": "segfault",
            },
            {
                "attempts": 2,
                "diagnostic": "runner-killed (signal)",
                "kind": "killed",
                "name": "sigkill",
            },
            {
                "attempts": 2,
                "diagnostic": "step-timeout (timed out after 2s)",
                "kind": "timeout",
                "name": "hang",
            },
        ],
    }
    journal = _journal(store, "outcomes")
    scan = next(step for step in journal["steps"] if step["nodeId"] == "scan")
    assert scan["status"] == "succeeded"
    assert scan["itemFailures"] == "collect"
    assert [item["status"] for item in scan["items"]] == [
        "succeeded",
        "succeeded",
        "failed",
        "failed",
        "failed",
        "failed",
        "failed",
    ]
    # The journal keeps durable summaries; exception messages stay in the outcome value.
    assert [attempt["diagnostic"] for attempt in scan["items"][2]["attempts"]] == [
        "step-execution-failure (exit 66)",
        "step-execution-failure (exit 66)",
    ]
    assert "cannot be parsed" not in json.dumps(journal)


def test_collecting_map_with_no_failures_wraps_every_value(tmp_path: Path) -> None:
    store = tmp_path / "store"

    run = _run(store, "clean", {"tasks": [{"name": "a", "behavior": "ok"}]})

    assert run["status"] == "succeeded"
    assert json.loads((store / run["resultKey"]).read_text()) == {
        "scanned": [{"attempt": 1, "name": "a"}],
        "failed": [],
    }
    collected = _journal(store, "clean")["steps"][1]["attempts"][0]["output"]["body"]["key"]
    assert json.loads((store / collected).read_text()) == [
        {"status": "succeeded", "value": {"attempt": 1, "name": "a"}}
    ]


def _compile(spec: dict[str, Any], out: Path) -> subprocess.CompletedProcess[str]:
    out.mkdir(parents=True)
    spec_path = out / "workflow-spec.json"
    spec_path.write_text(json.dumps(spec))
    return subprocess.run(
        [
            "go",
            "run",
            "./cmd/massive-compiler",
            "compile",
            "--spec",
            str(spec_path),
            "--out",
            str(out),
        ],
        cwd=REPOSITORY,
        check=False,
        capture_output=True,
        text=True,
    )


def _run(store: Path, run_id: str, workflow_input: dict[str, Any]) -> dict[str, Any]:
    result = subprocess.run(
        [
            "go",
            "run",
            "./cmd/massive-orchestrator",
            "run",
            "--spec",
            str(SPEC),
            "--source-root",
            str(WORKFLOW),
            "--store",
            str(store),
            "--project",
            "example/python-map-outcomes",
            "--run-id",
            run_id,
            "--input",
            json.dumps(workflow_input),
            "--json",
        ],
        cwd=REPOSITORY,
        env={**os.environ, "MASSIVE_PYTHON": sys.executable},
        check=False,
        capture_output=True,
        text=True,
    )
    assert result.returncode == 0, result.stderr
    return json.loads(result.stdout)


def _journal(store: Path, run_id: str) -> dict[str, Any]:
    project_key = f"sha256-{sha256(b'example/python-map-outcomes').hexdigest()}"
    return json.loads(
        (store / f"projects/{project_key}/runs/{run_id}/run-manifest.json").read_text()
    )


class Node(BaseModel):
    name: str
    children: list[Node]


@pytest.mark.parametrize(
    ("case", "item_type"),
    [("pydantic-integer", int), ("pydantic-model", Result), ("pydantic-recursive-model", Node)],
)
def test_sdk_emits_the_outcome_schema_conformance_vectors(case: str, item_type: Any) -> None:
    vectors = json.loads(
        (REPOSITORY / "conformance/fixtures/map-outcomes/outcome-schemas.json").read_text()
    )
    vector = next(vector for vector in vectors["cases"] if vector["name"] == case)

    def schema(annotation: Any) -> Any:
        return TypeAdapter(annotation).json_schema(mode="serialization")

    emitted = [schema(item_type), schema(list[MapItemOutcome[item_type]])]
    assert vector["accepted"] is True
    assert emitted == [vector["itemOutputSchema"], vector["outputSchema"]]
