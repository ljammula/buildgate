package workflow

import (
	"context"
	"fmt"

	"buildgate/internal/changesize"
	"buildgate/internal/run"
	"buildgate/internal/runner"
)

// MeasureChangeSizeActivity reads only git objects through the host-owned
// attributes view, after the result commit exists and before code review.
func (a *Activities) MeasureChangeSizeActivity(ctx context.Context, input MeasureChangeSizeInput) (*run.ChangeSize, error) {
	git, err := runner.NewSizeDiff(ctx, input.WorkspacePath)
	if err != nil {
		return nil, fmt.Errorf("prepare source-size diff: %w", err)
	}
	defer git.Close()
	size, err := changesize.Measure(ctx, git, input.BaseSHA, input.ResultSHA, input.TestPatterns)
	if err != nil {
		return nil, fmt.Errorf("measure source-size diff: %w", err)
	}
	return size, nil
}
