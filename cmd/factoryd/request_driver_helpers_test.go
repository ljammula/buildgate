package main

import (
	"context"
	"fmt"

	"buildgate/internal/request"
	"buildgate/internal/requestdriver"
)

// driveRequests advances the oldest request in a state the request
// driver knows how to progress by exactly one step, and saves it. It is a
// no-op (returns nil) when no request is in such a state.
//
// The states driven here are submitted, spec_drafting, planning, and
// building (ticket sequencing) -- approval into building/pr_review is
// the `approve` verb, and PR polling out of pr_review is the PR-review
// driver's responsibility; a request sitting in pr_review is simply left
// alone by this function.
//
// Test-only: one pass of the retired poll loop. Production advances each
// request through its own Temporal workflow (worker_cmd.go). The runners are
// injectable stubs.
func driveRequests(dp *deps, ctx context.Context, dataDir string, cfg requestdriver.WorkerConfig, specRunner requestdriver.SpecDraftRunner, planRunner requestdriver.PlanTicketsRunner, oracleRunner requestdriver.OracleDraftRunner, buildRunner requestdriver.TicketRunner) error {
	requests, err := request.List(dataDir)
	if err != nil {
		return fmt.Errorf("list requests: %w", err)
	}
	// Every pr_review request is polled each pass, and only the oldest
	// request in any other state is advanced. pr_review is a human-wait
	// state (its step is a rate-limited PR read that runs no job), so
	// letting the oldest request park there blocked every newer request
	// forever: a request waiting on PR approval starved all later
	// submissions (found live, 2026-09-19 -- a newer request sat in
	// "submitted" for 10 minutes behind a request awaiting PR review).
	// States that run a model job stay strictly one-per-pass, oldest first,
	// so at most one job is ever in flight.
	var firstErr error
	advancedJob := false
	for _, r := range requests {
		if !requestdriver.RequestDriverOwnsState(r.State) {
			continue
		}
		isJob := r.State != request.StatePRReview
		if isJob {
			if advancedJob {
				continue
			}
			advancedJob = true
			// Published through the worker heartbeat so status and the
			// console can say what other requests are queued behind.
			addActiveRequest(r.ID)
		}
		err := requestdriver.AdvanceRequest(dp, ctx, dataDir, r, cfg, specRunner, planRunner, oracleRunner, buildRunner)
		if isJob {
			removeActiveRequest(r.ID)
		}
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
