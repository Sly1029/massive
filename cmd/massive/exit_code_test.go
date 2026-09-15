package main

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/Sly1029/massive/internal/orchestrator"
)

func TestExitCodeForSurfacesRunnerFailureToTargetScheduler(t *testing.T) {
	for _, testCase := range []struct {
		err  error
		want int
	}{
		{errors.New("usage"), 1},
		{&orchestrator.InvocationFailure{ExitCode: 66}, 66},
		{fmt.Errorf("wrapped: %w", &orchestrator.InvocationFailure{ExitCode: 67}), 67},
		{&orchestrator.InvocationFailure{ExitCode: -1, TimedOutAfter: time.Minute}, runtimeExitTimeout},
		{&orchestrator.InvocationFailure{ExitCode: -1}, 1},
	} {
		if got := exitCodeFor(testCase.err); got != testCase.want {
			t.Fatalf("exitCodeFor(%v) = %d, want %d", testCase.err, got, testCase.want)
		}
	}
}
