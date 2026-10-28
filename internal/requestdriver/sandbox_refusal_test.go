package requestdriver_test

import (
	"context"
	"strings"
	"testing"

	"buildgate/internal/projectconfig"
	"buildgate/internal/request"
	"buildgate/internal/requestdriver"
	"buildgate/internal/requestdriver/requestdrivertest"
	"buildgate/internal/run"
)

// A ticket build refused for an unapproved sandbox block halts the request
// with the refusal's own sentence, which names the approve command.
func TestAdvanceBuildingHaltsWithTheSandboxApprovalRefusal(t *testing.T) {
	dp := newFakeDeps(t)
	dataDir, id := requestdrivertest.BuildingFixture(dp, t, 1)
	refusal := &projectconfig.SandboxNotApprovedError{Hash: strings.Repeat("ab", 32), Commit: strings.Repeat("c", 40), Repo: "/work/repo", Store: "/data/sandbox-approvals/repo.json"}
	runner := func(ctx context.Context, args []string, onReady func(*run.Run)) error { return refusal }

	if err := requestdrivertest.DriveRequests(dp, context.Background(), dataDir, requestdriver.WorkerConfig{}, requestdrivertest.FailingSpecDraftRunner(t), requestdrivertest.FailingPlanTicketsRunner(t), runner); err != nil {
		t.Fatalf("driveRequests: %v", err)
	}
	loaded, err := request.Load(dataDir, id)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.State != request.StateHalted || !strings.Contains(loaded.Error, refusal.Error()) || !strings.Contains(loaded.Error, "factoryd sandbox approve -repo /work/repo") {
		t.Errorf("State = %q, Error = %q; want a halt carrying %q", loaded.State, loaded.Error, refusal.Error())
	}
}
