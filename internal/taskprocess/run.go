// Package taskprocess owns the operating-system lifetime of a task invocation.
package taskprocess

import (
	"bytes"
	"context"
	"errors"
	"fmt"
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
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = directory
	// Bound inherited-pipe drainage even if a descendant leaves the ownership
	// group. Local author code is trusted execution, not an OS sandbox.
	cmd.WaitDelay = pipeDrainLimit
	owner, err := own(cmd)
	if err != nil {
		return "", err
	}
	defer owner.close()
	cmd.Cancel = owner.kill
	var output boundedOutput
	cmd.Stdout, cmd.Stderr = &output, &output
	if err := owner.start(ctx); err != nil {
		if cmd.Process != nil {
			_ = owner.kill()
			_ = cmd.Wait()
		}
		if ctx.Err() != nil {
			return output.String(), ctx.Err()
		}
		return output.String(), err
	}
	err = cmd.Wait()
	// A successful wait is authoritative even if a sibling cancels the shared
	// context immediately afterward. Preserve its exit status and output.
	if err != nil && ctx.Err() != nil {
		return output.String(), ctx.Err()
	}
	if errors.Is(err, exec.ErrWaitDelay) {
		err = fmt.Errorf("task descendants kept output open after the adapter exited; ensure tasks await child processes: %w", err)
	}
	return output.String(), err
}

type boundedOutput struct {
	buffer    bytes.Buffer
	truncated bool
}

func (output *boundedOutput) Write(data []byte) (int, error) {
	remaining := outputLimit - output.buffer.Len()
	kept := min(len(data), remaining)
	_, _ = output.buffer.Write(data[:kept])
	output.truncated = output.truncated || kept < len(data)
	return len(data), nil
}

func (output *boundedOutput) String() string {
	if output.truncated {
		return output.buffer.String() + fmt.Sprintf("\n[task output truncated at %d bytes]\n", outputLimit)
	}
	return output.buffer.String()
}
