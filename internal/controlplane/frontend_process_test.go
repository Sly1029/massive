package controlplane

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFrontendOwnsDescendantsAndInheritedPipes(t *testing.T) {
	useCancellationPython(t)
	for _, mode := range []string{"cancel", "return", "return-stderr"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			ready := filepath.Join(root, "ready")
			child := filepath.Join(root, "child.py")
			childSource := `import socket, sys
from pathlib import Path
with socket.socket() as listener:
    listener.bind(("127.0.0.1", 0))
    listener.listen()
    Path(sys.argv[1]).write_text(str(listener.getsockname()[1]))
    while True:
        connection, _ = listener.accept()
        with connection:
            if connection.recv(1) == b"x":
                break
`
			if err := os.WriteFile(child, []byte(childSource), 0o600); err != nil {
				t.Fatal(err)
			}
			entry := filepath.Join(root, "workflow.py")
			source := fmt.Sprintf(`import subprocess, sys, time
from pathlib import Path
from massive import GraphBuilder, StepContext, container, execution
subprocess.Popen([sys.executable, %q, %q])
while not Path(%q).exists():
    time.sleep(0.01)
if %q == "return-stderr":
    print("frontend diagnostic", file=sys.stderr)
if %q == "cancel":
    time.sleep(60)
def echo(ctx: StepContext[int]) -> int:
    return ctx.inputs
graph = GraphBuilder(name="frontend-owner", input_type=int, output_type=int,
    defaults=execution(environment=container("example.invalid/runner@sha256:"+"1"*64, platform="linux/amd64")))
task = graph.add(echo)
graph.edge_from(graph.start).to(task)
graph.edge_from(task).to(graph.end)
`, child, ready, ready, mode, mode)
			if err := os.WriteFile(entry, []byte(source), 0o600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				_, err := Emit(ctx, entry)
				done <- err
			}()
			address := ""
			deadline := time.Now().Add(20 * time.Second)
			for address == "" {
				if data, err := os.ReadFile(ready); err == nil && len(data) > 0 {
					address = "127.0.0.1:" + string(data)
				}
				if time.Now().After(deadline) {
					t.Fatal("frontend descendant did not reach readiness")
				}
				time.Sleep(10 * time.Millisecond)
			}
			// The socket proves liveness and lets the failing implementation clean up.
			t.Cleanup(func() {
				if connection, err := net.DialTimeout("tcp", address, time.Second); err == nil {
					_, _ = connection.Write([]byte("x"))
					_ = connection.Close()
				}
			})
			if mode == "cancel" {
				cancel()
			}
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("frontend with an unawaited descendant unexpectedly succeeded")
				}
				if mode != "cancel" && !strings.Contains(err.Error(), "descendants kept output open") {
					t.Fatalf("unawaited descendant error = %v", err)
				}
				if mode == "return-stderr" && !strings.Contains(err.Error(), "frontend diagnostic") {
					t.Fatalf("frontend diagnostic missing from error: %v", err)
				}
				if mode != "cancel" && !errors.Is(err, exec.ErrWaitDelay) {
					t.Fatalf("drain deadline error lost its cause: %v", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("frontend completion hung on its descendant's inherited pipes")
			}
			deadline = time.Now().Add(time.Second)
			for {
				connection, err := net.DialTimeout("tcp", address, 100*time.Millisecond)
				if err != nil {
					break
				}
				_ = connection.Close()
				if time.Now().After(deadline) {
					t.Fatal("frontend descendant survived completion")
				}
				time.Sleep(10 * time.Millisecond)
			}
		})
	}
}
