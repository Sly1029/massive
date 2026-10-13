# Map schema boundaries

Map list/item comparisons reconstruct the standalone item document with exactly
its transitive local `$defs`, including references inside inline containers.
Reuse the local definition closure helpers and preserve exact canonical JSON
comparison, including annotations. `ValidateControlFlow` also checks this rule
when validating compiled plans. Collected outcomes use their separately defined
annotation-insensitive comparison in `outcome.go`; keep that contract distinct.
