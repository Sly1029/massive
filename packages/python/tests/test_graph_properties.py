"""Shrinking graph generators exercise authoring, the Go compiler, and local execution."""

from __future__ import annotations

import importlib.util
import json
import os
import string
import subprocess
import sys
from dataclasses import dataclass, field
from pathlib import Path
from tempfile import TemporaryDirectory
from types import ModuleType
from typing import Annotated, Any, Literal

import pytest
from hypothesis import HealthCheck, event, example, given, settings
from hypothesis import strategies as st
from pydantic import BaseModel, Field

from massive import GraphBuilder, StepContext, container, execution, source_package

REPOSITORY = Path(__file__).resolve().parents[3]
GENERATED = Path(__file__).parent / "fixtures/generated_graph"
NIGHTLY = os.environ.get("MASSIVE_HYPOTHESIS_PROFILE") == "nightly"
DEFAULTS = execution(
    environment=container("example.invalid/python@sha256:" + "0" * 64, platform="linux/amd64")
)


def scaled(*, ci: int, nightly: int, **options: Any) -> settings:
    """Set an example budget per property for the ci and nightly profiles (conftest.py)."""
    return settings(max_examples=nightly if NIGHTLY else ci, deadline=None, **options)


class Value(BaseModel):
    value: int


class Left(BaseModel):
    kind: Literal["left"]
    value: int


class Right(BaseModel):
    kind: Literal["right"]
    value: int


Route = Annotated[Left | Right, Field(discriminator="kind")]


def classify(ctx: StepContext[Value]) -> Route:
    return Left(kind="left", value=ctx.inputs.value)


def left(ctx: StepContext[Left]) -> Value:
    return Value(value=ctx.inputs.value)


def right(ctx: StepContext[Right]) -> Value:
    return Value(value=ctx.inputs.value)


def identity(ctx: StepContext[Value]) -> Value:
    return ctx.inputs


def new_graph():
    return GraphBuilder(name="generated", input_type=Value, output_type=Value, defaults=DEFAULTS)


def _go_binary(tmp_path_factory: pytest.TempPathFactory, command: str) -> Path:
    path = tmp_path_factory.mktemp(command) / command
    subprocess.run(
        ["go", "build", "-o", str(path), f"./cmd/{command}"],
        cwd=REPOSITORY,
        check=True,
        capture_output=True,
    )
    return path


@pytest.fixture(scope="module")
def compiler(tmp_path_factory: pytest.TempPathFactory) -> Path:
    return _go_binary(tmp_path_factory, "massive-compiler")


@pytest.fixture(scope="module")
def massive_cli(tmp_path_factory: pytest.TempPathFactory) -> Path:
    return _go_binary(tmp_path_factory, "massive")


def _load_shapes() -> ModuleType:
    """Import the fixture under the module name its emitted symbols reference."""
    specification = importlib.util.spec_from_file_location(
        "generated_shapes", GENERATED / "generated_shapes.py"
    )
    assert specification is not None and specification.loader is not None
    module = importlib.util.module_from_spec(specification)
    sys.modules[specification.name] = module
    specification.loader.exec_module(module)
    return module


shapes = _load_shapes()
GENERATED_SOURCE = source_package(root=GENERATED, include=["*.py"], package_id="python-main")
TEST_SOURCE = source_package(
    root=Path(__file__).parent, include=[Path(__file__).name], package_id="generated"
)

ID_CHARS = string.ascii_letters + string.digits + "_.@:#-"
MAX_ID = 128
SAFE_INTEGERS = st.integers(min_value=-(2**53) + 1, max_value=2**53 - 1)


def _unambiguous(value: str) -> bool:
    """Generated IDs avoid reserved IDs, the '--' scope separator, and the '-select' suffix.

    Then a scoped '<call>--<child>' ID parses back to one call path, so globally
    unique raw IDs give globally unique emitted IDs and the graph must be valid.
    """
    return (
        value not in {".", "..", "__start", "__end"}
        and "--" not in value
        and not value.startswith("-")
        and not value.endswith("-")
        and not value.endswith("-select")
    )


@st.composite
def descriptions(draw: st.DrawFn, *, max_depth: int, max_block: int) -> Any:
    used: set[str] = set()

    def ident(room: int) -> str:
        size = draw(st.one_of(st.integers(1, max(1, min(6, room))), st.just(room)))
        value = draw(
            st.text(ID_CHARS, min_size=size, max_size=size).filter(
                lambda value: _unambiguous(value) and value not in used
            )
        )
        used.add(value)
        return value

    def scope_id(room: int) -> str:
        # A third of the remaining room keeps several nested scopes within MAX_ID.
        return ident(max(1, (room - 2) // 3))

    def via(room: int, depth: int) -> str | None:
        return scope_id(room) if depth > 0 and draw(st.booleans()) else None

    def block(prefix_length: int, depth: int, min_size: int) -> tuple[Any, ...]:
        size = draw(st.integers(min_size, max_block))
        return tuple(node(prefix_length, depth) for _ in range(size))

    def scoped(prefix_length: int, scope: str | None) -> int:
        return prefix_length if scope is None else prefix_length + len(scope) + 2

    def node(prefix_length: int, depth: int) -> Any:
        room = MAX_ID - prefix_length
        kinds = ["step", "fan"] + (["call", "decide"] if depth > 0 else [])
        kind = draw(st.sampled_from(kinds))
        if kind == "step":
            return shapes.StepNode(id=ident(room), asynchronous=draw(st.booleans()))
        if kind == "call":
            ids = tuple(scope_id(room) for _ in range(draw(st.integers(1, 2))))
            longest = max(len(call_id) for call_id in ids)
            return shapes.CallNode(
                ids=ids, body=block(prefix_length + longest + 2, depth - 1, 1)
            )
        if kind == "fan":
            scope = via(room, depth)
            return shapes.FanNode(
                explode=ident(MAX_ID - scoped(prefix_length, scope)),
                via=scope,
                map=ident(room),
                collect=ident(room),
                concurrency=draw(st.integers(1, 4)),
                asynchronous=draw(st.booleans()),
            )
        scope = via(room, depth)
        classifier = ident(MAX_ID - scoped(prefix_length, scope))
        arms = []
        for _ in range(2):
            arm_scope = via(room, depth)
            arm_length = scoped(prefix_length, arm_scope)
            arms.append(
                shapes.Arm(
                    entry=ident(MAX_ID - arm_length),
                    via=arm_scope,
                    body=block(arm_length, depth - 1, 0),
                )
            )
        return shapes.DecideNode(
            classifier=classifier,
            via=scope,
            decision=ident(room - len("-select")),
            left=arms[0],
            right=arms[1],
        )

    return shapes.GraphDescription(body=block(0, max_depth, 1))


leaves = st.builds(
    shapes.Leaf,
    label=st.text(max_size=8),
    note=st.none() | st.text(max_size=8),
    weights=st.one_of(
        st.lists(SAFE_INTEGERS, max_size=4),
        st.lists(SAFE_INTEGERS, min_size=200, max_size=1000),
    ),
)
payloads = st.builds(
    shapes.Payload,
    title=st.text(max_size=12),
    leaves=st.lists(leaves, max_size=3),
    counts=st.dictionaries(st.text(max_size=6), SAFE_INTEGERS, max_size=4),
    optional=st.none() | SAFE_INTEGERS,
)


@dataclass
class Expectation:
    """An independent model of a description's run on the local target."""

    steps: dict[str, tuple[str, int]] = field(default_factory=dict)
    decisions: dict[str, str | None] = field(default_factory=dict)

    def run(self, description: Any, token: dict[str, Any] | None) -> dict[str, Any] | None:
        return self._block(description.body, "", token)

    def _visit(self, node_id: str, token: dict[str, Any] | None) -> dict[str, Any] | None:
        self.steps[node_id] = ("skipped" if token is None else "succeeded", 0)
        return None if token is None else {**token, "trace": [*token["trace"], node_id]}

    def _block(
        self, nodes: tuple[Any, ...], prefix: str, token: dict[str, Any] | None
    ) -> dict[str, Any] | None:
        for node in nodes:
            token = self._node(node, prefix, token)
        return token

    def _node(self, node: Any, prefix: str, token: dict[str, Any] | None) -> dict[str, Any] | None:
        if isinstance(node, shapes.StepNode):
            return self._visit(prefix + node.id, token)
        if isinstance(node, shapes.CallNode):
            for call_id in node.ids:
                token = self._block(node.body, f"{prefix}{call_id}--", token)
            return token
        scope = prefix if node.via is None else f"{prefix}{node.via}--"
        if isinstance(node, shapes.DecideNode):
            routed = self._visit(scope + node.classifier, token)
            tag = None if token is None else ("right" if token["route"] & 1 else "left")
            self.decisions[prefix + node.decision] = tag
            if routed is not None:
                routed = {**routed, "route": routed["route"] >> 1}
            outcomes = {}
            for arm_tag, arm in (("left", node.left), ("right", node.right)):
                arm_scope = prefix if arm.via is None else f"{prefix}{arm.via}--"
                entered = self._visit(arm_scope + arm.entry, routed if tag == arm_tag else None)
                outcomes[arm_tag] = self._block(arm.body, arm_scope, entered)
            return None if tag is None else outcomes[tag]
        exploded = self._visit(scope + node.explode, token)
        map_id, collect_id = prefix + node.map, prefix + node.collect
        if exploded is None:
            self.steps[map_id] = ("skipped", 0)
            self.steps[collect_id] = ("skipped", 0)
            return None
        width, *rest = exploded["widths"] or [0]
        self.steps[map_id] = ("succeeded", width)
        self._visit(collect_id, exploded)
        entry = f"{collect_id}<{','.join(map(str, range(width)))}|{map_id if width else ''}"
        if width == 0:
            empty = shapes.Payload().model_dump(mode="json", by_alias=True)
            return {"trace": [entry], "route": 0, "widths": [], "payload": empty}
        return {**exploded, "trace": [*exploded["trace"], entry], "widths": rest}


def _emit(description: Any, *, inline: bool) -> str:
    return shapes.build_graph(description, inline=inline).emit(source=GENERATED_SOURCE).to_json()


@scaled(ci=40, nightly=1500)
@given(description=descriptions(max_depth=3, max_block=3))
def test_composition_emits_the_hand_inlined_spec_and_compiles(compiler: Path, description) -> None:
    composed = _emit(description, inline=False)
    assert composed == _emit(description, inline=True)

    expectation = Expectation()
    expectation.run(description, None)
    graph = json.loads(composed)["graph"]
    emitted = {node["id"]: node["kind"] for node in graph["nodes"]}
    assert {i for i, kind in emitted.items() if kind in {"step", "map"}} == set(expectation.steps)
    assert {i for i, kind in emitted.items() if kind == "decision"} == set(expectation.decisions)
    assert all(len(node_id) <= MAX_ID for node_id in emitted)
    with TemporaryDirectory() as directory:
        path = Path(directory) / "spec.json"
        path.write_text(composed)
        result = subprocess.run(
            [str(compiler), "compile", "--spec", str(path), "--out", f"{directory}/compiled"],
            capture_output=True,
            text=True,
            check=False,
        )
    assert result.returncode == 0, result.stderr


def _run_generated(
    massive_cli: Path, store: Path, description: Any, token: dict[str, Any], run_id: str
) -> tuple[dict[str, Any], dict[str, Any]]:
    environment = {
        **os.environ,
        "MASSIVE_GENERATED_GRAPH": description.model_dump_json(),
        "MASSIVE_PYTHON": sys.executable,
    }
    common = ["--store", str(store), "--project", "generated/graphs", "--json"]
    result = subprocess.run(
        [
            str(massive_cli),
            "run",
            f"{GENERATED / 'workflow.py'}#graph",
            "--run-id",
            run_id,
            "--input",
            json.dumps(token),
            *common,
        ],
        cwd=REPOSITORY,
        env=environment,
        capture_output=True,
        text=True,
        check=False,
    )
    assert result.returncode == 0, result.stderr
    journal = subprocess.run(
        [str(massive_cli), "inspect", run_id, *common],
        cwd=REPOSITORY,
        capture_output=True,
        text=True,
        check=True,
    )
    return json.loads(result.stdout), json.loads(journal.stdout)


def _assert_matches_model(
    expectation: Expectation, run: dict[str, Any], journal: dict[str, Any], expected: Any
) -> None:
    assert run["status"] == "succeeded"
    assert run["result"] == expected
    observed = {}
    for step in journal["steps"]:
        items = step.get("items") or []
        assert [item["index"] for item in items] == list(range(len(items)))
        assert {item["status"] for item in items} <= {"succeeded"}
        observed[step["nodeId"]] = (step["status"], len(items))
    assert observed == expectation.steps
    assert {
        decision["nodeId"]: decision.get("selectedCase") for decision in journal["decisions"]
    } == expectation.decisions


SMOKE_DESCRIPTION = shapes.GraphDescription(
    body=(
        shapes.StepNode(id="a"),
        shapes.CallNode(ids=("c@1", "c:2"), body=(shapes.StepNode(id="s", asynchronous=True),)),
        shapes.DecideNode(
            classifier="cl",
            via="v#",
            decision="d",
            left=shapes.Arm(
                entry="L",
                via="lv",
                body=(
                    shapes.FanNode(explode="e", via="ev", map="m", collect="k", concurrency=2),
                ),
            ),
            right=shapes.Arm(entry="R"),
        ),
        shapes.FanNode(explode="e2", map="m2", collect="k2", concurrency=3, asynchronous=True),
    )
)


SMOKE_PAYLOAD = shapes.Payload(
    title="ζ\u2028",
    leaves=[shapes.Leaf(label="ラベル", note=None, weights=list(range(-500, 500)))],
    counts={"𝔘": 1, "\ufffd": 2, "": 3},
    optional=2**53 - 1,
)


@scaled(
    ci=6,
    nightly=120,
    suppress_health_check=[HealthCheck.too_slow, HealthCheck.data_too_large],
)
@example(description=SMOKE_DESCRIPTION, route=0, widths=[3, 0], payload=SMOKE_PAYLOAD)
@example(description=SMOKE_DESCRIPTION, route=1, widths=[0, 2], payload=SMOKE_PAYLOAD)
@given(
    description=descriptions(max_depth=2, max_block=2),
    route=st.integers(min_value=0, max_value=2**53 - 1),
    widths=st.lists(st.integers(0, 40 if NIGHTLY else 3), max_size=6),
    payload=payloads,
)
def test_generated_graphs_run_locally_as_the_model_predicts(
    massive_cli: Path, description, route: int, widths: list[int], payload
) -> None:
    token = {
        "trace": [],
        "route": route,
        "widths": widths,
        "payload": payload.model_dump(mode="json", by_alias=True),
    }
    expectation = Expectation()
    expected = expectation.run(description, token)
    event(f"nodes: {len(expectation.steps) + len(expectation.decisions)}")
    event(f"active steps: {sum(status == 'succeeded' for status, _ in expectation.steps.values())}")
    event(f"composition: {'call' if '\"call\"' in description.model_dump_json() else 'flat'}")
    with TemporaryDirectory() as directory:
        run, journal = _run_generated(massive_cli, Path(directory), description, token, "generated")
    _assert_matches_model(expectation, run, journal, expected)


def test_large_map_collects_items_in_source_order(massive_cli: Path, tmp_path: Path) -> None:
    width = 1000 if NIGHTLY else 24
    description = shapes.GraphDescription(
        body=(shapes.FanNode(explode="explode", map="items", collect="collect", concurrency=8),)
    )
    empty = shapes.Payload().model_dump(mode="json", by_alias=True)
    token = {"trace": [], "route": 0, "widths": [width], "payload": empty}
    expectation = Expectation()
    expected = expectation.run(description, token)
    run, journal = _run_generated(massive_cli, tmp_path, description, token, "large-map")
    _assert_matches_model(expectation, run, journal, expected)
    assert expected is not None
    assert expected["trace"][-1].startswith("collect<0,1,2,")


@given(
    call_id=st.text(ID_CHARS, min_size=1, max_size=30).filter(_unambiguous),
    middle=st.text(ID_CHARS, min_size=1, max_size=30).filter(_unambiguous),
    child_id=st.text(ID_CHARS, min_size=1, max_size=30).filter(_unambiguous),
    shape=st.sampled_from(["parent-literal", "two-calls", "nested-call"]),
    call_first=st.booleans(),
)
def test_scoped_id_collisions_are_rejected_in_either_declaration_order(
    call_id: str, middle: str, child_id: str, shape: str, call_first: bool
) -> None:
    """A call's scoped child ID must not reuse an ID already present in the parent."""

    def child(name: str, step_id: str):
        graph = GraphBuilder(name=name, input_type=Value, output_type=Value, defaults=DEFAULTS)
        graph.edge_from(graph.start).to(graph.add(identity, id=step_id)).to(graph.end)
        return graph

    parent = new_graph()
    if shape == "parent-literal":
        collision = child("child", child_id)
        first = parent.call(collision, id=call_id)
        second = parent.add(identity, id=f"{call_id}--{child_id}")
    elif shape == "two-calls":
        first = parent.call(child("outer", f"{middle}--{child_id}"), id=call_id)
        second = parent.call(child("inner", child_id), id=f"{call_id}--{middle}")
    else:
        nested = GraphBuilder(name="nested", input_type=Value, output_type=Value, defaults=DEFAULTS)
        nested.edge_from(nested.start).to(nested.call(child("leaf", child_id), id=middle)).to(
            nested.end
        )
        first = parent.call(nested, id=call_id)
        second = parent.add(identity, id=f"{call_id}--{middle}--{child_id}")
    ordered = (first, second) if call_first else (second, first)
    parent.edge_from(parent.start).to(ordered[0]).to(ordered[1]).to(parent.end)
    with pytest.raises(ValueError, match="duplicate scoped node id"):
        parent.emit(source=TEST_SOURCE)


@example(scopes=[26], leaf=100)
@example(scopes=[27], leaf=100)
@given(
    scopes=st.lists(st.integers(1, 60), min_size=1, max_size=5),
    leaf=st.integers(1, MAX_ID),
)
def test_nested_calls_fail_clearly_beyond_the_id_limit(scopes: list[int], leaf: int) -> None:
    graph = GraphBuilder(name="leaf", input_type=Value, output_type=Value, defaults=DEFAULTS)
    graph.edge_from(graph.start).to(graph.add(identity, id="s" * leaf)).to(graph.end)
    for depth, length in enumerate(scopes):
        parent = GraphBuilder(
            name=f"level{depth}", input_type=Value, output_type=Value, defaults=DEFAULTS
        )
        parent.edge_from(parent.start).to(parent.call(graph, id="c" * length)).to(parent.end)
        graph = parent
    scoped_id = "--".join(["c" * length for length in reversed(scopes)] + ["s" * leaf])
    if len(scoped_id) <= MAX_ID:
        nodes = json.loads(graph.emit(source=TEST_SOURCE).to_json())["graph"]["nodes"]
        assert scoped_id in {node["id"] for node in nodes}
        return
    with pytest.raises(ValueError, match=r"call 'c+' scopes child node '[cs-]+' as a \d+-character"):
        graph.emit(source=TEST_SOURCE)


@given(
    node_id=st.from_regex(r"[a-z][a-z0-9]{0,20}", fullmatch=True),
    endpoint=st.sampled_from(["source", "target", "start", "end"]),
)
def test_handles_cannot_cross_graphs_with_colliding_ids(node_id: str, endpoint: str) -> None:
    a, b = new_graph(), new_graph()
    local = a.add(identity, id=node_id)
    foreign = b.add(identity, id=node_id)
    with pytest.raises(ValueError, match="different graph"):
        if endpoint == "source":
            a.edge_from(foreign)
        elif endpoint == "target":
            a.edge_from(a.start).to(foreign)
        elif endpoint == "start":
            a.edge_from(b.start)
        else:
            a.edge_from(local).to(b.end)


@given(
    paths=st.lists(
        st.lists(st.text(alphabet="abXY-_é中", min_size=1, max_size=5), min_size=1, max_size=3),
        max_size=15,
    )
)
@settings(max_examples=40, deadline=None)
def test_generated_tree_shapes_roundtrip(paths) -> None:
    from massive import ArtifactFiles, Tree
    from massive.datastore import LocalDatastore

    with TemporaryDirectory() as directory:
        root = Path(directory)
        source = root / "source"
        source.mkdir()
        for components in paths:
            target = source.joinpath(*components)
            target.mkdir(parents=True, exist_ok=True)
            (target / "payload.bin").write_text("/".join(components))
        tree = Tree.from_path(source)
        files = ArtifactFiles(LocalDatastore(root / "store"), root / "scratch")
        encoded = tree.model_dump_json(context=files)
        restored = Tree.model_validate_json(encoded, context=files)
        assert Tree.from_path(restored.path()) == tree


def test_decisions_and_selects_reject_foreign_handles_with_matching_ids() -> None:
    a, b = new_graph(), new_graph()
    sources = [g.add(classify, id="classifier") for g in (a, b)]
    with pytest.raises(ValueError, match="different graph"):
        a.decision(sources[1], on="kind", id="foreign")
    routes = [g.decision(source, on="kind", id="route") for g, source in zip((a, b), sources)]
    lefts, rights, cases = [], [], []
    for graph, route in zip((a, b), routes):
        first = graph.add(left, id="left")
        second = graph.add(right, id="right")
        case = route.case(Left)
        graph.edge_from(case).to(first)
        graph.edge_from(route.case(Right)).to(second)
        lefts.append(first)
        rights.append(second)
        cases.append(case)
    with pytest.raises(ValueError, match="different graph"):
        a.edge_from(cases[1])
    with pytest.raises(ValueError, match="different graph"):
        routes[0].select(Value, left=lefts[1], right=rights[0])


def test_emission_freezes_previously_created_edge_paths() -> None:
    graph = new_graph()
    node = graph.add(identity)
    path = graph.edge_from(graph.start)
    path.to(node).to(graph.end)
    graph.emit(
        source=source_package(
            root=Path(__file__).parent, include=[Path(__file__).name], package_id="frozen"
        )
    )
    with pytest.raises(RuntimeError, match="already been emitted"):
        path.to(node)
