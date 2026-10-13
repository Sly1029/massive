// Package taskprocess owns the operating-system lifetime of a task invocation.
package taskprocess

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"time"
)

const (
	outputLimit    = 1 << 20
	pipeDrainLimit = 250 * time.Millisecond
)

// Run captures bounded combined output. A task owns its descendants: completing
// or cancelling the adapter terminates processes still in its OS ownership group.
// Tasks must await children whose work contributes to their result.
func Run(ctx context.Context, argv []string, directory string) (string, error) {
	var output boundedOutput
	err := RunTo(ctx, argv, directory, nil, &output, &output)
	return output.String(), err
}

// RunTo runs an adapter with separate output streams and owns its descendants.
// Callers choose their output policy; frontend specifications must remain intact
// rather than share the bounded combined log capture used by task invocations.
// A nil environment inherits this process's environment.
func RunTo(ctx context.Context, argv []string, directory string, environment []string, stdout, stderr io.Writer) error {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir, cmd.Env = directory, environment
	// Bound inherited-pipe drainage even if a descendant leaves the ownership
	// group. Local author code is trusted execution, not an OS sandbox.
	cmd.WaitDelay = pipeDrainLimit
	owner, err := own(cmd)
	if err != nil {
		return err
	}
	defer owner.close()
	cmd.Cancel = owner.kill
	cmd.Stdout, cmd.Stderr = stdout, stderr
	if err := owner.start(ctx); err != nil {
		if cmd.Process != nil {
			_ = owner.kill()
			_ = cmd.Wait()
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return err
	}
	err = cmd.Wait()
	// A successful wait is authoritative even if a sibling cancels the shared
	// context immediately afterward. Preserve its exit status and output.
	if err != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(err, exec.ErrWaitDelay) {
		err = fmt.Errorf("descendants kept output open after the parent process exited; ensure child processes are awaited: %w", err)
	}
	return err
}

// boundedOutput is a ring containing the most recent output, so failures
// retain their final diagnostic even after very noisy author logs.
type boundedOutput struct {
	buffer    []byte
	offset    int
	truncated bool
}

func (output *boundedOutput) Write(data []byte) (int, error) {
	length := len(data)
	if len(data) >= outputLimit {
		output.truncated = output.truncated || len(output.buffer)+len(data) > outputLimit
		output.buffer = append(output.buffer[:0], data[len(data)-outputLimit:]...)
		output.offset = 0
		return length, nil
	}
	if len(output.buffer) < outputLimit {
		kept := min(len(data), outputLimit-len(output.buffer))
		output.buffer = append(output.buffer, data[:kept]...)
		data = data[kept:]
	}
	if len(data) > 0 {
		output.truncated = true
		copied := copy(output.buffer[output.offset:], data)
		copy(output.buffer, data[copied:])
		output.offset = (output.offset + len(data)) % outputLimit
	}
	return length, nil
}

func (output *boundedOutput) String() string {
	if output.truncated {
		return fmt.Sprintf("[task output truncated; showing last %d bytes]\n", outputLimit) + string(output.buffer[output.offset:]) + string(output.buffer[:output.offset])
	}
	return string(output.buffer)
}
