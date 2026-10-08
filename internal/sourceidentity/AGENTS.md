# Source package identity

`VerifyArchive` (build-time, in memory) and `ExtractArchive` (pods, streamed
from scratch files) share one archive walk, so they accept exactly the same
archives. Keep every archive rule in that walk; never add a second parser.
Extraction writes through an `os.Root` before identity is known, so callers
must extract into a fresh directory and discard it on error.
