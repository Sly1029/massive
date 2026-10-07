# Local datastore

Use one short-lived `os.Root` per operation for object and metadata access,
including directory creation, links, renames, and listing. Lexical path checks
alone do not contain symlinks or concurrent ancestor replacement. Read operations
must preserve a missing store without creating directories.
