# Canonical JSON v0 corpus

Each document under `valid/` is both valid JSON and already in its
`canonical-json-v0` byte representation. Each document under `invalid/` is
JSON source (including forbidden non-finite literals) that must be rejected as a canonical-json-v0 payload: parse
it, canonicalize its field tree, and require byte-for-byte equality with the
source. The invalid corpus deliberately includes numeric lexical forms which
lose their spelling during JSON parsing.

The canonical payload ends at its final JSON token: it has **no trailing
newline**. Repository fixture files carry one final LF as text-file transport;
test readers may strip that one final LF before comparing the payload bytes.
They must not otherwise trim or re-escape the payload.

`hashes.json` pins the SHA-256 digest of each final-newline-stripped valid
payload. It makes the corpus a byte-level cross-language contract, rather than
only a parser round-trip fixture.

The `escaping.json` vector pins JSON escaping byte-for-byte: all C0 controls,
the short escapes (`\b`, `\t`, `\n`, `\f`, `\r`), raw DEL, raw `<`, `>`, `&`,
raw U+2028/U+2029, and a raw non-BMP scalar. Canonical output uses exactly the
escaping rules in `conformance/schema/hashing.md`; it does not add HTML or
JavaScript-source safety escaping.

Language SDK tests must consume both directories. In-memory values which JSON
cannot express (such as `undefined`, sparse arrays, functions, symbols,
bigints, and cyclic values) are covered by language-local tests at the
canonicalizer boundary.

Readers must hash the final-LF-stripped fixture **bytes**, not a decoded and
re-encoded string. The invalid vectors include semantically equivalent but
noncanonical JSON spellings (`\\u003c`, escaped non-ASCII scalars and
surrogate pairs, `\\u2028`, uppercase control escapes, `\\/`, and
`\\u0041`); a text round trip can accidentally erase the distinction this
corpus is intended to enforce.

`numbers.json` describes IEEE-754 doubles by their 16-digit hexadecimal bits,
with expected canonical text and SHA-256, or null expectations for non-finite
values. All three languages consume it. It includes RFC 8785 Appendix B samples,
three adjacent doubles at exponent and safe-integer boundaries, and 256
reproducible bit patterns (first eight bytes of SHA-256 of
`massive-jcs-double-<index>`, for indices 0–255). Expected text comes from
Node/V8 `JSON.stringify`, and hashes from Node's crypto SHA-256, independently
of the Go and Python encoders. `sources` exercises lexical normalization and
rejection at canonical byte boundaries. Negative zero loses its sign; every
other finite double round-trips exactly.

The preexisting `fraction.json` and `unsafe-integer.json` documents moved from
`invalid/` to `valid/` without changing their bytes: `1.5` and the integral
double `9007199254740992` are now supported. All preexisting valid corpus bytes
and pinned hashes are unchanged, as are the hashing and value-parameter vectors.
`1e3`, `-0`, and `1.0` remain noncanonical source spellings.
