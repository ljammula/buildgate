package triage

import (
	"strings"
	"testing"

	"buildgate/internal/run"
	"buildgate/internal/unresolved"
)

// A run halted because its model route's credential could not be used says
// so and names the fix, never the long workflow error it carries.
func TestTriageRouteCredentialHalt(t *testing.T) {
	r := &run.Run{ID: "run-c", State: run.StateHalted, HaltReasonCode: string(unresolved.RouteCredential), HaltError: "workflow execution error (type: RunWorkflow, ...)"}
	got := Run(r, t.TempDir())
	if !strings.Contains(got, "credential") || !strings.Contains(got, "codex login") || strings.Contains(got, "workflow execution error") {
		t.Errorf("triage = %q", got)
	}
}
