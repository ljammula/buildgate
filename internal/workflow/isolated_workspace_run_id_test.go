package workflow

import (
	"strings"
	"testing"
)

// TestIsolatedWorkspaceRunIDIsHumanLegible is the regression test for
// the legible-isolated-workspace-run-id finding: before this fix, a
// -repository-routed run's own isolated worktree/branch name was a bare
// SHA-256 digest (isolatedWorkspaceRunID took only workflowID), so a real
// PR branch read "factoryd/<64 hex chars>" with nothing an operator could
// recognize it by. The durable run id's own slug must now be a visible
// prefix.
func TestIsolatedWorkspaceRunIDIsHumanLegible(t *testing.T) {
	got := isolatedWorkspaceRunID("add-worstweekday-req-20260925-073712-001", "repo-owner-abc-run-add-worstweekday-req-20260925-073712")
	if !strings.HasPrefix(got, "add-worstweekday-req-20260925-073712-001-") {
		t.Errorf("isolatedWorkspaceRunID = %q, want a readable slug prefix", got)
	}
	if strings.Contains(got, "/") || strings.Contains(got, "..") {
		t.Errorf("isolatedWorkspaceRunID = %q, not git-ref/filesystem safe", got)
	}
}

// TestIsolatedWorkspaceRunIDBoundedAndDeterministic covers the two
// properties this value's own doc comment promises to preserve from
// before #10: bounded length regardless of how long durableRunID or
// workflowID are (a -repository-routed workflowID can be ~250 chars), and
// a deterministic result for the same inputs (Prepare and Rollback must
// agree across separate Activity invocations for the same run).
func TestIsolatedWorkspaceRunIDBoundedAndDeterministic(t *testing.T) {
	longRunID := strings.Repeat("x", 300)
	longWorkflowID := strings.Repeat("y", 300)
	got1 := isolatedWorkspaceRunID(longRunID, longWorkflowID)
	got2 := isolatedWorkspaceRunID(longRunID, longWorkflowID)
	if got1 != got2 {
		t.Fatalf("isolatedWorkspaceRunID is not deterministic: %q != %q", got1, got2)
	}
	// isolatedWorkspaceRunIDSlugMaxLen (40) + "-" + isolatedWorkspaceRunIDHashLen (12).
	const maxLen = isolatedWorkspaceRunIDSlugMaxLen + 1 + isolatedWorkspaceRunIDHashLen
	if len(got1) > maxLen {
		t.Errorf("isolatedWorkspaceRunID length = %d, want <= %d", len(got1), maxLen)
	}
}

// TestIsolatedWorkspaceRunIDFallsBackToHashAlone covers a durableRunID
// with nothing sanitize.Slug can keep (empty, or entirely non-[a-z0-9]):
// the pre-#10 behavior, a bare hash, still applies rather than producing
// a name starting or ending with "-" or being empty.
func TestIsolatedWorkspaceRunIDFallsBackToHashAlone(t *testing.T) {
	for _, durableRunID := range []string{"", "***", "   "} {
		got := isolatedWorkspaceRunID(durableRunID, "workflow-1")
		if got == "" {
			t.Errorf("isolatedWorkspaceRunID(%q, ...) is empty", durableRunID)
		}
		if strings.HasPrefix(got, "-") || strings.HasSuffix(got, "-") {
			t.Errorf("isolatedWorkspaceRunID(%q, ...) = %q, want no leading/trailing '-'", durableRunID, got)
		}
	}
}

// TestIsolatedWorkspaceRunIDDifferentWorkflowIDsDiffer guards against a
// regression that ties the whole result to durableRunID alone (two
// concurrent corrective rounds or reclaimed runs sharing one durable run
// id, but different workflow executions, must still get different
// worktrees/branches).
func TestIsolatedWorkspaceRunIDDifferentWorkflowIDsDiffer(t *testing.T) {
	a := isolatedWorkspaceRunID("req-1-001", "workflow-a")
	b := isolatedWorkspaceRunID("req-1-001", "workflow-b")
	if a == b {
		t.Errorf("isolatedWorkspaceRunID gave the same result for two different workflow ids: %q", a)
	}
}
