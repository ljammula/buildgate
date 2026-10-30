package workflow

import (
	"testing"

	"go.temporal.io/sdk/testsuite"
	temporalworkflow "go.temporal.io/sdk/workflow"

	"buildgate/internal/run"
)

// A new execution asks for referenceOracleRemovedChange once, where the
// removed oracle commit used to run.
func TestANewExecutionRecordsTheReferenceOracleRemovedChange(t *testing.T) {
	pin := func(env *testsuite.TestWorkflowEnvironment) {
		env.OnGetVersion(referenceOracleRemovedChange, temporalworkflow.DefaultVersion, 1).Return(temporalworkflow.Version(1)).Times(1)
		t.Cleanup(func() { env.AssertExpectations(t) })
	}
	err, result, _ := runIsolationGuardFixture(t, fixtureInput(), pin)
	if err != nil || result.State != run.StateAccepted {
		t.Fatalf("err=%v state=%v, want accepted", err, result.State)
	}
}

// A history recorded before the reference oracle was removed has no such
// marker and replays as DefaultVersion: the run it recorded made no oracle
// call (it named no oracle directory), and replays to the same verdict.
func TestHistoryRecordedBeforeTheReferenceOracleRemovalReplaysUnchanged(t *testing.T) {
	pin := func(env *testsuite.TestWorkflowEnvironment) {
		env.OnGetVersion(referenceOracleRemovedChange, temporalworkflow.DefaultVersion, 1).Return(temporalworkflow.DefaultVersion)
	}
	err, result, _ := runIsolationGuardFixture(t, fixtureInput(), pin)
	if err != nil || result.State != run.StateAccepted {
		t.Errorf("replay of a pre-removal history: err=%v state=%v, want accepted", err, result.State)
	}
}
