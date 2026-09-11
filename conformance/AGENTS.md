# Shared contracts and conformance

Schema sources in `schema/` define shared wire contracts. Regenerate derived
bindings, emitted specs, and golden plans using the repository generation
scripts; do not hand-edit generated hashes to make a failing fixture pass.

Contract migrations must update both language emitters and Go readers together.
Keep only the current transport/version; obsolete inputs should explain that
they need rebuilding. Test rehashed but semantically invalid plans as well as
malformed bytes. Graph fuzzing should cover topology and exhaustive decisions,
not only the parser's tolerance of random JSON.

Fixtures describe real executable requirements. Change a fixture at its source
when its environment or network intent changes; tests must not silently rewrite
those requirements just to get through a target compiler.
