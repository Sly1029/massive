"""Entrypoint for `massive run`: builds the description passed by the property tests."""

from __future__ import annotations

import os

from generated_shapes import GraphDescription, build_graph

graph = build_graph(
    GraphDescription.model_validate_json(os.environ["MASSIVE_GENERATED_GRAPH"]),
    inline=os.environ.get("MASSIVE_GENERATED_GRAPH_INLINE") == "1",
)
