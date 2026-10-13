# Canonical identities

Reject malformed UTF-8, unpaired surrogate escapes, and duplicate object names
before decoding can replace or discard data. Validate the complete input before
excluding a self-hash member. Preserve existing safe-integer bytes and the established JSON shape of typed
Go values when changing JSON libraries. Numbers use finite IEEE-754 doubles
and RFC 8785 formatting; canonical byte readers must compare re-encoded bytes
to reject rounded or noncanonical source spellings.
