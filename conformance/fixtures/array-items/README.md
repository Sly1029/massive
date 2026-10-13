# Array item schemas

`schemas.json` records separately emitted item and array schemas. The Python
SDK emission is pinned by `test_nested_map_schemas.py`; Go applies every pair
at both map boundaries, including compiled-plan control-flow validation.

Standalone items carry exactly their transitive local `$defs`, even when the
item root is an inline container. Unrelated outer definitions are excluded.
Field types, annotations, and extra standalone definitions still compare by
exact canonical JSON. Outcome vectors separately pin nested collected values
in `../map-outcomes/outcome-schemas.json`.
