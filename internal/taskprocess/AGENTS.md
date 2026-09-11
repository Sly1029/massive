# Task process ownership

This module owns process startup, descendant termination, pipe drainage, and the
captured output budget. Keep platform handling here; the orchestrator translates
errors into invocation outcomes. Signal ingress and journal state belong to the
CLI and orchestrator, respectively.

On Windows, start suspended and assign the job before resuming the initial
thread. Assignment after startup permits early children to escape. Serialize
assignment with cancellation so a closed job handle cannot be reused during
startup. Failed assignment must terminate and reap the suspended process.
On Linux and macOS, use a separate process group. This is trusted execution;
processes deliberately leaving their group are not sandboxed.

Exercise real descendants that retain stdout after their parent exits and after
cancellation. The socket fixture supplies both liveness evidence and cleanup for
regressions. Changes to ownership need the Linux, macOS, and Windows CI matrix;
cross-compilation alone does not validate native process lifetime behavior.
