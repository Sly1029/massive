"""Shrinking graph generators exercise authoring and the real Go compiler together."""

from __future__ import annotations

import subprocess
from pathlib import Path
from tempfile import TemporaryDirectory
from typing import Annotated, Literal

import pytest
from hypothesis import given, settings
from hypothesis import strategies as st
from pydantic import BaseModel, Field

from massive import GraphBuilder, StepContext, container, execution, source_package


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
    return GraphBuilder(
        name="generated",
        input_type=Value,
        output_type=Value,
        defaults=execution(
            environment=container(
                "example.invalid/python@sha256:" + "0" * 64,
                platform="linux/amd64",
            )
        ),
    )


@pytest.fixture(scope="module")
def compiler(tmp_path_factory):
    path = tmp_path_factory.mktemp("graph-compiler") / "massive-compiler"
    subprocess.run(
        ["go", "build", "-o", str(path), "./cmd/massive-compiler"],
        cwd=Path(__file__).resolve().parents[3],
        check=True,
        capture_output=True,
    )
    return path


shapes = st.recursive(
    st.integers(min_value=1, max_value=5),
    lambda children: st.tuples(children, children),
    max_leaves=8,
)


@settings(max_examples=40, deadline=None)
@given(shape=shapes)
def test_generated_nested_decisions_compile(compiler: Path, shape) -> None:
    def build(graph, source, shape, prefix):
        if isinstance(shape, int):
            for i in range(shape):
                target = graph.add(identity, id=f"{prefix}-task-{i}")
                graph.edge_from(source).to(target)
                source = target
            return source
        classifier = graph.add(classify, id=f"{prefix}-classify")
        graph.edge_from(source).to(classifier)
        route = graph.decision(classifier, on="kind", id=f"{prefix}-route")
        a = graph.add(left, id=f"{prefix}-left")
        b = graph.add(right, id=f"{prefix}-right")
        graph.edge_from(route.case(Left)).to(a)
        graph.edge_from(route.case(Right)).to(b)
        a = build(graph, a, shape[0], f"{prefix}-a")
        b = build(graph, b, shape[1], f"{prefix}-b")
        return route.select(Value, left=a, right=b)

    graph = new_graph()
    graph.edge_from(build(graph, graph.start, shape, "root")).to(graph.end)
    spec = graph.emit(
        source=source_package(
            root=Path(__file__).parent, include=[Path(__file__).name], package_id="generated"
        )
    )
    with TemporaryDirectory() as directory:
        root = Path(directory)
        path = root / "spec.json"
        path.write_text(spec.to_json())
        result = subprocess.run(
            [str(compiler), "compile", "--spec", str(path), "--out", str(root / "compiled")],
            capture_output=True,
            text=True,
            check=False,
        )
        assert result.returncode == 0, result.stderr


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
