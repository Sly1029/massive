from __future__ import annotations

import json
from pathlib import Path
from typing import cast

import pytest

from massive import canonical_json, sha256_ref
from massive.canonical import JsonValue


def test_canonical_hash_matches_shared_golden_vector() -> None:
    repository = Path(__file__).resolve().parents[3]
    value = json.loads(
        (repository / "conformance/fixtures/hashing/canonical-input.json").read_text()
    )
    expected = (
        (repository / "conformance/fixtures/hashing/canonical-input.sha256").read_text().strip()
    )

    assert sha256_ref(canonical_json(value)) == expected


def test_canonical_json_consumes_the_cross_runtime_v0_corpus() -> None:
    repository = Path(__file__).resolve().parents[3]
    corpus = repository / "conformance/fixtures/canonical-json-v0"

    for path in sorted((corpus / "valid").glob("*.json")):
        source = path.read_bytes().removesuffix(b"\n")
        value = cast(JsonValue, json.loads(source))

        assert canonical_json(value).encode() == source, path.name

    for path in sorted((corpus / "invalid").glob("*.json")):
        source = path.read_bytes().removesuffix(b"\n")
        value = cast(JsonValue, json.loads(source))
        try:
            canonical = canonical_json(value).encode()
        except (TypeError, ValueError):
            continue

        assert canonical != source, path.name


@pytest.mark.parametrize("value", [1.5, 1 << 53, -(1 << 53)])
def test_canonical_json_rejects_values_outside_v0_number_contract(value: float) -> None:
    with pytest.raises((TypeError, ValueError)):
        canonical_json(cast(JsonValue, value))
