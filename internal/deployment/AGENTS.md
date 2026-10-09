# Deployment bindings

Plan identity describes portable requirements; deployment identity includes the
chosen namespace, storage, and logical-secret-to-Kubernetes-key bindings. Changing
a binding must change the deployment hash while leaving the plan hash unchanged.

Validate binding JSON against the shared deployment schema before decoding it.
Pod placement is deployment data: it references Kubernetes definitions in the
pinned Argo schema, which this package loads with unknown fields rejected
(`rejectUnknownFields`). Keep Kubernetes objects as raw JSON rather than
re-modeling them, and express reserved fields as `false` property schemas so
diagnostics name the offending key.
Bindings contain Secret names and keys, never credential values. The Argo target
owns environment-variable restrictions and injects only refs declared by a user
invocation; Kubernetes resolves their values in the deployment namespace.
