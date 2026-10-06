package request

import (
	"reflect"
	"testing"

	"buildgate/internal/testfixture"
)

// TestRequestCannotCarryACredential reflect-walks Request the same way
// internal/workflow's TestRunWorkflowInputCannotCarryACredential walks
// RunWorkflowInput (via the same shared testfixture.FindCredentialLeak):
// Request is the durable, on-disk record this package's own Save/Load
// persist (request.json under a request's directory, long-lived and
// readable by anyone with filesystem access to it, and synced onward by
// requestsubmit/notify), so nothing reachable from it should ever be, or
// become, a raw credential. The walker's own detector-catches-a-violation
// self-tests live in internal/testfixture/credentialwalk_test.go, not
// here.
func TestRequestCannotCarryACredential(t *testing.T) {
	// SpecEvidence.Usage/PlanEvidence.Usage are map[string]any: best-effort
	// token-usage evidence recorded verbatim from the spec/plan-drafting
	// job's own --evidence JSON (see SpecEvidence/PlanEvidence's doc
	// comments), display-only and never consulted by any gate. Their
	// interface-kind map values are exempted here, not by weakening the
	// walk's default interface handling for anyone else.
	exempt := map[string]reflect.Kind{
		"Request.SpecEvidence[].Usage[]": reflect.Interface,
		"Request.PlanEvidence[].Usage[]": reflect.Interface,
	}
	if err := testfixture.FindCredentialLeak(reflect.TypeOf(Request{}), nil, exempt); err != nil {
		t.Fatal(err)
	}
}
