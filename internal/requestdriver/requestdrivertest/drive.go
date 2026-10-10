// Package requestdrivertest holds the fixtures the tests of requestdriver, and
// of the commands that call it, share: request and run records on disk,
// stub runners for the model jobs and the ticket build, and stand-ins for the
// resume checks. A fixture that needs a requestdriver.Deps takes the caller's
// own fake.
package requestdrivertest

import (
	"context"
	"fmt"

	"buildgate/internal/request"
	"buildgate/internal/requestdriver"
)

// DriveRequests is one pass over the requests in dataDir: every pr_review
// request is polled, and only the oldest request in a state that runs a job
// is advanced. Production advances each request through its own workflow;
// the runners are stubs.
func DriveRequests(dp requestdriver.Deps, ctx context.Context, dataDir string, cfg requestdriver.WorkerConfig, specRunner requestdriver.SpecDraftRunner, planRunner requestdriver.PlanTicketsRunner, oracleRunner requestdriver.OracleDraftRunner, buildRunner requestdriver.TicketRunner) error {
	requests, err := request.List(dataDir)
	if err != nil {
		return fmt.Errorf("list requests: %w", err)
	}
	var firstErr error
	advancedJob := false
	for _, r := range requests {
		if !requestdriver.RequestDriverOwnsState(r.State) {
			continue
		}
		if r.State != request.StatePRReview {
			if advancedJob {
				continue
			}
			advancedJob = true
		}
		err := requestdriver.AdvanceRequest(dp, ctx, dataDir, r, cfg, specRunner, planRunner, oracleRunner, buildRunner)
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
