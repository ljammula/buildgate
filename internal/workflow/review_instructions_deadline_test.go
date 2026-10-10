package workflow

import (
	"testing"

	"buildgate/internal/sandbox"
)

// The review step takes the instruction snapshot before it starts to
// heartbeat (prepareReviewLaunch runs ahead of heartbeatWhileRunning), under
// the heartbeat timeout RunWorkflow sets on that Activity: twice
// activityHeartbeatInterval. The first heartbeat comes one interval after the
// launch begins, so a snapshot that used its whole deadline must still leave
// that interval, or the server would time the step out as lost before the
// snapshot's own refusal could reach the operator.
func TestReviewInstructionSnapshotDeadlineFitsBeforeTheReviewStepsFirstHeartbeat(t *testing.T) {
	const heartbeatTimeout = 2 * activityHeartbeatInterval
	if sandbox.ReviewInstructionTimeout+activityHeartbeatInterval >= heartbeatTimeout {
		t.Fatalf("a snapshot deadline of %v plus %v to the first heartbeat is not inside the review step's heartbeat timeout of %v", sandbox.ReviewInstructionTimeout, activityHeartbeatInterval, heartbeatTimeout)
	}
}
