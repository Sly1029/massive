# WorkflowSpec Shape Fixtures

These fixtures prove the `WorkflowSpec` JSON Schema accepts and rejects the intended v0 artifact shapes.

They are not hash fixtures. Digest values use valid-looking placeholders so schema validation can check field shape, required hash prefixes, and hash length. `WS-0.6` owns canonical hash vectors and must define which fields each digest covers. In particular, `specHash` should cover the `WorkflowSpec` field tree excluding the `specHash` field itself.

Symbol modules use the explicit `./workflow.ts` convention. Frontend SDKs should emit normalized relative module specifiers for deterministic symbol resolution.
