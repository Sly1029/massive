# Local datastore

Use one short-lived `os.Root` per operation for object and metadata access,
including directory creation, links, renames, and listing. Lexical path checks
alone do not contain symlinks or concurrent ancestor replacement. Read operations
must preserve a missing store without creating directories.

Read objects that can be large, such as source archives, with `Open` and bound
them by its declared size; `Get` buffers the whole body. Denied S3 reads in both
map to `ErrAccessDenied` and keep the S3 code (for example
`SignatureDoesNotMatch` or `RequestTimeTooSkewed`), which is the diagnosis.
