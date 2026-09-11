# Live Argo evidence

Controller status includes DAG and TaskGroup nodes as well as pods. A TaskGroup
can carry the same templateName as its map items; filter `type == "Pod"` when
counting physical invocations. Assert item cardinality and selected/inactive
routes independently of the final value, which can match after an empty route.

Credential checks should inspect the actual pod's main container as well as
exercise code that requires the credential. Assert that pods were observed so
a pod-GC policy cannot turn a scope check into an empty passing loop.
