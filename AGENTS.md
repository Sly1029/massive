# Repository Instructions

## Code Quality

- Think like a senior engineer. Optimize for maintainability and clear control flow.
- Inline small logic until extracting a function would enclose meaningful behavior.
- Avoid passthrough functions and abstractions that only rename behavior.
- Prefer library primitives for validation and parsing. Use Zod, Cap'n Proto schemas, Kubernetes schema validation, and typecheckers instead of ad hoc defensive checks.
- Avoid broad defensive code. Make invalid states unrepresentable where the language and validation libraries allow it.

## Testing Policy

- Tests must be functional. Do not use mock APIs, spies, patching, or MagicMock-style substitutes.
- Banned examples include `vi.mock`, `vi.fn`, `vi.spyOn`, `jest.mock`, `jest.fn`, `jest.spyOn`, `sinon`, `MagicMock`, `AsyncMock`, `unittest.mock`, and Python `patch`.
- Prefer real filesystem datastores, local object-store-compatible services, local Kubernetes clusters, real generated manifests, and schema validation.
- Use test helpers only when they create real functional fixtures, such as temporary object stores, compiled plans, Argo bundles, or local runner invocations.
- Before finishing code changes, run:

```sh
node scripts/check-no-test-mocks.mjs
```

