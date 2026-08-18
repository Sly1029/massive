# Canonical JSON v0 corpus

Each document under `valid/` is both valid JSON and already in its
`canonical-json-v0` byte representation. Each document under `invalid/` is
valid JSON source that must be rejected as a canonical-json-v0 payload: parse
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
