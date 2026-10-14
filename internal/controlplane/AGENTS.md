# Workflow preparation

For local cache changes, read `../../docs/spec/environment-materialization.md`
section “Local preparation caches”. Keep cache addresses separate from portable
source/spec/plan and realized-environment identity recipes. Use the frontend's
shared source selection so new glob matches and project/lock inputs invalidate.

Cache hits require a fresh supported interpreter fingerprint and the normal spec
and plan validators. Unsupported mutable environments take full preflight and
emission. Publish only unchanged snapshots through the local datastore's atomic
write; `--no-cache` bypasses both cache reads and publication. Exercise installed
wheels as well as editable development environments.
