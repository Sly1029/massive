package taskprocess

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

func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "process-fixture" {
		runFixture(os.Args[2], os.Args[3])
		return
	}
	os.Exit(m.Run())
}

func runFixture(mode, ready string) {
	if mode == "output" {
		fmt.Print(strings.Repeat("x", outputLimit*2))
		return
	}
	if mode == "failure" {
		fmt.Fprintln(os.Stderr, "author diagnostic")
		os.Exit(42)
	}

	if mode == "child" {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			panic(err)
		}
		defer listener.Close()
		if err := os.WriteFile(ready, []byte(listener.Addr().String()), 0600); err != nil {
			panic(err)
		}
		fmt.Println("child ready")
		for {
			connection, err := listener.Accept()
			if err != nil {
				return
			}
			var message [1]byte
			_, _ = connection.Read(message[:])
			_ = connection.Close()
			if message[0] == 'x' {
				return
			}
		}
	}
	executable, err := os.Executable()
	if err != nil {
		panic(err)
	}
	child := exec.Command(executable, "process-fixture", "child", ready)
	child.Stdout, child.Stderr = os.Stdout, os.Stderr
	if err := child.Start(); err != nil {
		panic(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			panic("child did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if mode == "return" {
		return
	}
	_ = child.Wait()
}

func TestRunOwnsDescendantsAndInheritedOutput(t *testing.T) {
	for _, mode := range []string{"cancel", "return"} {
		t.Run(mode, func(t *testing.T) {
			ready := filepath.Join(t.TempDir(), "ready")
			executable, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				_, err := Run(ctx, []string{executable, "process-fixture", mode, ready}, "")
				done <- err
			}()
			var address string
			deadline := time.Now().Add(10 * time.Second)
			for address == "" {
				data, err := os.ReadFile(ready)
				if err == nil {
					address = string(data)
				}
				if time.Now().After(deadline) {
					t.Fatal("fixture did not become ready")
				}
				time.Sleep(10 * time.Millisecond)
			}
			// A real socket provides both a liveness check and cleanup for the failing
			// pre-fix implementation, without leaving a descendant after the test.
			t.Cleanup(func() {
				connection, err := net.DialTimeout("tcp", address, time.Second)
				if err == nil {
					_, _ = connection.Write([]byte("x"))
					_ = connection.Close()
				}
			})
			if mode == "cancel" {
				cancel()
			}
			select {
			case err := <-done:
				if mode == "cancel" && !errors.Is(err, context.Canceled) {
					t.Fatalf("cancel error = %v", err)
				}
				if mode == "return" && !errors.Is(err, exec.ErrWaitDelay) {
					t.Fatalf("unawaited descendant error = %v", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("task completion hung on a descendant's inherited output pipe")
			}
			deadline = time.Now().Add(time.Second)
			for {
				connection, err := net.DialTimeout("tcp", address, 100*time.Millisecond)
				if err != nil {
					break
				}
				_ = connection.Close()
				if time.Now().After(deadline) {
					t.Fatal("descendant is still serving after the task completed")
				}
				time.Sleep(10 * time.Millisecond)
			}

		})
	}
}

func TestRunBoundsOutputAndPreservesAuthorExit(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	output, err := Run(t.Context(), []string{executable, "process-fixture", "output", "unused"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(output, "x") != outputLimit || !strings.HasSuffix(output, "[task output truncated at 1048576 bytes]\n") {
		t.Fatalf("output was not bounded and marked: length %d", len(output))
	}
	output, err = Run(t.Context(), []string{executable, "process-fixture", "failure", "unused"}, "")
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 42 || output != "author diagnostic\n" {
		t.Fatalf("author result = %q, %v", output, err)
	}
}

func TestRunRejectsMissingExecutableAndCancelledContext(t *testing.T) {
	if _, err := Run(t.Context(), []string{filepath.Join(t.TempDir(), "missing")}, ""); err == nil {
		t.Fatal("missing executable succeeded")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Run(ctx, []string{executable, "process-fixture", "failure", "unused"}, ""); !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-cancelled invocation = %v", err)
	}
}
