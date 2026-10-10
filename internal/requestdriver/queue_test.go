package requestdriver_test

import (
	"reflect"
	"testing"

	"buildgate/internal/requestdriver"
	"buildgate/internal/testfixture"
)

// TestQueueEntryCannotCarryACredential reflect-walks QueueEntry the same
// way internal/workflow's TestRunWorkflowInputCannotCarryACredential walks
// RunWorkflowInput (via the same shared testfixture.FindCredentialLeak):
// QueueEntry is the durable, on-disk record `submit` writes and
// `worker` reads back (<data-dir>/queue/<id>/entry.json), so nothing
// reachable from it should ever be, or become, a raw credential. The
// walker's own detector-catches-a-violation self-tests live in
// internal/testfixture/credentialwalk_test.go, not here.
func TestQueueEntryCannotCarryACredential(t *testing.T) {
	t.Parallel()
	if err := testfixture.FindCredentialLeak(reflect.TypeOf(requestdriver.QueueEntry{}), nil, nil); err != nil {
		t.Fatal(err)
	}
}
