from __future__ import annotations

import json
import math
import struct
from pathlib import Path
from typing import cast

import pytest

from massive import canonical_json, sha256_ref
from massive.canonical import CanonicalJsonError, JsonValue, parse_canonical_json


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
    valid_directory = corpus / "valid"
    invalid_directory = corpus / "invalid"
    assert valid_directory.is_dir()
    assert invalid_directory.is_dir()
    valid_paths = sorted(valid_directory.glob("*.json"))
    invalid_paths = sorted(invalid_directory.glob("*.json"))
    hashes = json.loads((corpus / "hashes.json").read_text())
    valid_names = {path.name for path in valid_paths}
    invalid_names = {path.name for path in invalid_paths}
    assert {
        "escaping.json",
        "integer-like-key-order.json",
        "integers.json",
        "prototype-keys.json",
        "utf16-key-order.json",
    } <= valid_names
    assert {
        "exponent.json",
        "lone-surrogate-key.json",
        "lone-surrogate-value.json",
        "negative-zero.json",
        "whitespace.json",
    } <= invalid_names
    assert set(hashes) == valid_names

    for path in valid_paths:
        source = canonical_fixture_payload(path)
        value = parse_canonical_json(source)

        assert canonical_json(value).encode() == source, path.name
        assert sha256_ref(source) == hashes[path.name], path.name

    for path in invalid_paths:
        source = canonical_fixture_payload(path)
        try:
            canonical = canonical_json(parse_canonical_json(source)).encode()
        except (TypeError, ValueError):
            continue
        else:
            assert canonical != source, path.name


def canonical_fixture_payload(path: Path) -> bytes:
    fixture = path.read_bytes()
    assert fixture.endswith(b"\n")
    assert not fixture.endswith(b"\r\n")
    return fixture[:-1]


@pytest.mark.parametrize("value", [float("nan"), float("inf"), -float("inf"), 1 << 53, -(1 << 53)])
def test_canonical_json_rejects_values_outside_v0_number_contract(value: float) -> None:
    with pytest.raises((TypeError, ValueError)):
        canonical_json(cast(JsonValue, value))


@pytest.mark.parametrize("body", [b"\x80", b"1.0", b'{"value":42 }'])
def test_parse_canonical_json_rejects_malformed_utf8_and_noncanonical_bytes(body: bytes) -> None:
    with pytest.raises(CanonicalJsonError):
        parse_canonical_json(body)


def test_shared_ieee_double_vectors() -> None:
    repository = Path(__file__).resolve().parents[3]
    vectors = json.loads(
        (repository / "conformance/fixtures/canonical-json-v0/numbers.json").read_text()
    )
    for case in vectors["cases"]:
        value = struct.unpack(">d", bytes.fromhex(case["bits"]))[0]
        if case["canonical"] is None:
            with pytest.raises(ValueError):
                canonical_json(value)
            continue
        encoded = canonical_json(value)
        assert encoded == case["canonical"], case["bits"]
        assert sha256_ref(encoded) == case["hash"], case["bits"]
        decoded = parse_canonical_json(encoded.encode())
        assert float(decoded) == value, case["bits"]
        if value != 0:
            assert struct.pack(">d", float(decoded)).hex() == case["bits"]
        else:
            assert math.copysign(1, decoded) == 1
    for case in vectors["sources"]:
        if case["canonical"] is None or case["source"] != case["canonical"]:
            with pytest.raises(CanonicalJsonError):
                parse_canonical_json(case["source"].encode())
        if case["canonical"] is not None:
            assert canonical_json(json.loads(case["source"], parse_int=float)) == case["canonical"]


def test_integral_float_and_integer_share_identity() -> None:
    assert canonical_json(3.0) == canonical_json(3) == "3"
    assert sha256_ref(canonical_json(3.0)) == sha256_ref(canonical_json(3))
    assert canonical_json(float(1 << 53)) == "9007199254740992"
    assert isinstance(parse_canonical_json(b"9007199254740992"), float)
