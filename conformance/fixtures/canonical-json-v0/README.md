# Canonical JSON v0 corpus

Each document under `valid/` is both valid JSON and already in its
`canonical-json-v0` byte representation. Each document under `invalid/` is
valid JSON source that must be rejected as a canonical-json-v0 payload: parse
it, canonicalize its field tree, and require byte-for-byte equality with the
source. The invalid corpus deliberately includes numeric lexical forms which
lose their spelling during JSON parsing.

The final newline used by repository text files is not part of a payload; test
readers may strip that one transport newline before comparison.

`hashes.json` pins the SHA-256 digest of each final-newline-stripped valid
payload. It makes the corpus a byte-level cross-language contract, rather than
only a parser round-trip fixture.

Language SDK tests must consume both directories. In-memory values which JSON
cannot express (such as `undefined`, sparse arrays, functions, symbols,
bigints, and cyclic values) are covered by language-local tests at the
canonicalizer boundary.
