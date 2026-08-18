from __future__ import annotations

import hashlib
import json

type JsonValue = None | bool | int | str | list["JsonValue"] | dict[str, "JsonValue"]

_MAX_SAFE_INTEGER = (1 << 53) - 1


class CanonicalJsonError(ValueError):
    """Bytes are not a valid canonical JSON v0 value."""


def canonical_json(value: JsonValue) -> str:
    """Encode a v0 Massive field tree with the shared canonical JSON rules."""
    return _encode(value)


def parse_canonical_json(body: bytes) -> JsonValue:
    """Decode canonical JSON v0 bytes, rejecting malformed or noncanonical input."""
    try:
        value = json.loads(body)
        if canonical_json(value).encode() != body:
            raise ValueError("JSON body is not canonical")
    except (UnicodeDecodeError, json.JSONDecodeError, TypeError, ValueError) as error:
        raise CanonicalJsonError("JSON body is not canonical") from error
    return value


def sha256_ref(value: str | bytes) -> str:
    payload = value.encode("utf-8") if isinstance(value, str) else value
    return f"sha256:{hashlib.sha256(payload).hexdigest()}"


def _encode(value: JsonValue) -> str:
    if value is None:
        return "null"
    if value is True:
        return "true"
    if value is False:
        return "false"
    if isinstance(value, int):
        if not -_MAX_SAFE_INTEGER <= value <= _MAX_SAFE_INTEGER:
            raise ValueError("canonical JSON integers must be within JavaScript's safe range")
        return str(value)
    if isinstance(value, float):
        raise TypeError("canonical JSON v0 does not support floating-point numbers")
    if isinstance(value, str):
        _assert_well_formed_unicode(value)
        return json.dumps(value, ensure_ascii=False, separators=(",", ":"))
    if isinstance(value, list):
        return "[" + ",".join(_encode(item) for item in value) + "]"
    encoded: list[str] = []
    for key in sorted(value, key=lambda item: item.encode("utf-16-be")):
        _assert_well_formed_unicode(key)
        encoded.append(f"{_encode(key)}:{_encode(value[key])}")
    return "{" + ",".join(encoded) + "}"


def _assert_well_formed_unicode(value: str) -> None:
    if any(0xD800 <= ord(character) <= 0xDFFF for character in value):
        raise ValueError("canonical JSON strings must not contain lone surrogates")
