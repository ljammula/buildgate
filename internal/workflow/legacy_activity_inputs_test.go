package workflow

import (
	"context"
	"time"

	"buildgate/internal/runner"
)

// deadlineRecorder wraps an Activities fake runner, recording the time left on
// each command's context when it starts.
func deadlineRecorder(a *Activities) *[]time.Duration {
	var left []time.Duration
	inner := a.runWithRetriesChecked
	a.runWithRetriesChecked = func(ctx context.Context, ws string, logPath func(int) string, n int, before func(int) error, after func(int, runner.Result, error) error, name string, args ...string) (runner.Result, error) {
		if d, ok := ctx.Deadline(); ok {
			left = append(left, time.Until(d))
		}
		return inner(ctx, ws, logPath, n, before, after, name, args...)
	}
	return &left
}
