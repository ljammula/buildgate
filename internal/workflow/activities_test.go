package workflow

import (
	"fmt"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"buildgate/internal/reviewstep"
	"buildgate/internal/runner"
)

// TestSandboxUserForForcesHostIdentityWhenNotIsolated is the regression
// test for a real P1 finding from GitHub Codex App review of PR #62:
// PrepareIsolatedWorkspaceActivity's own wsisolation.EnableWorkerGroupWrite
// grant (the thing that makes -sandbox-worker-uid's dedicated identity
// able to write at all) only ever runs when input.IsolateWorkspace is
// true. Leaving sandboxUserFor empty for a non-isolated run let
// sandbox.ResolveDefaultWorkerIdentity resolve the separated identity
// anyway -- a UID with no write access to the ordinary, non-isolated
// checkout at all, silently failing every build/verify/full-suite command
// that needs to write there. sandboxUserFor must instead force the
// pre-Phase-6 host-UID:GID identity explicitly in that case.
func TestSandboxUserForForcesHostIdentityWhenNotIsolated(t *testing.T) {
	a := &Activities{}
	want := fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid())
	if got := a.sandboxUserFor(RunWorkflowInput{IsolateWorkspace: false}); got != want {
		t.Errorf("sandboxUserFor(IsolateWorkspace: false) = %q, want the host UID:GID %q", got, want)
	}
	if got := a.sandboxUserFor(RunWorkflowInput{IsolateWorkspace: true}); got != "" {
		t.Errorf("sandboxUserFor(IsolateWorkspace: true) = %q, want empty (let the default separated identity resolve)", got)
	}
	// An explicit choice -- the request's own SandboxUser, or the Worker's
	// own static SandboxUser -- must still win regardless of isolation.
	if got := (&Activities{}).sandboxUserFor(RunWorkflowInput{IsolateWorkspace: false, SandboxUser: "1000:1000"}); got != "1000:1000" {
		t.Errorf("sandboxUserFor with an explicit request SandboxUser = %q, want it unchanged", got)
	}
	if got := (&Activities{SandboxUser: "2000:2000"}).sandboxUserFor(RunWorkflowInput{IsolateWorkspace: false}); got != "2000:2000" {
		t.Errorf("sandboxUserFor with an explicit Worker-static SandboxUser = %q, want it unchanged", got)
	}
}

// TestHeartbeatWhileRunningRecordsHeartbeatsUntilFnReturns proves the
// mechanism RunBuildActivity/RunVerifyActivity now rely on to let a
// server-side cancellation/termination actually interrupt their
// subprocess (see activityHeartbeatInterval's doc comment): heartbeat
// fires repeatedly for as long as fn is still running, stops once it
// returns, and fn's own result/error still come back unchanged.
func TestHeartbeatWhileRunningRecordsHeartbeatsUntilFnReturns(t *testing.T) {
	var heartbeats atomic.Int32
	wantResult := runner.Result{ExitCode: 7}
	result, err := heartbeatWhileRunning(5*time.Millisecond, func() { heartbeats.Add(1) }, func() (runner.Result, error) {
		time.Sleep(60 * time.Millisecond)
		return wantResult, nil
	})
	if err != nil {
		t.Fatalf("heartbeatWhileRunning: %v", err)
	}
	if result.ExitCode != wantResult.ExitCode {
		t.Fatalf("result = %+v, want %+v", result, wantResult)
	}
	if got := heartbeats.Load(); got < 3 {
		t.Fatalf("heartbeats recorded = %d, want at least 3 over a 60ms run with a 5ms interval", got)
	}
}

// TestHeartbeatWhileRunningStopsAfterFnReturns proves the heartbeat
// goroutine is actually torn down once fn returns, not left running (which
// would otherwise call heartbeat against a since-completed Activity
// execution's ctx).
func TestHeartbeatWhileRunningStopsAfterFnReturns(t *testing.T) {
	var heartbeats atomic.Int32
	if _, err := heartbeatWhileRunning(2*time.Millisecond, func() { heartbeats.Add(1) }, func() (runner.Result, error) {
		return runner.Result{}, nil
	}); err != nil {
		t.Fatalf("heartbeatWhileRunning: %v", err)
	}
	afterReturn := heartbeats.Load()
	time.Sleep(30 * time.Millisecond)
	if got := heartbeats.Load(); got != afterReturn {
		t.Fatalf("heartbeats recorded after fn returned = %d -> %d, want no further heartbeats once fn is done", afterReturn, got)
	}
}

// TestActivitiesRunIDForFallsBackToLogDirBaseName is the regression for a
// codex finding: a RunWorkflowInput serialized by a binary before RunID
// existed has no such field, and Temporal workflow inputs persist across
// worker upgrades, so runIDFor must recover the same durable run id from
// LogDir (always run.Dir(dataDir, id), for every submission path both
// before and after RunID was added) rather than construct an empty
// sandbox.LaunchSpec.RunID that Validate would then reject.
func TestActivitiesRunIDForFallsBackToLogDirBaseName(t *testing.T) {
	a := &Activities{}
	input := RunWorkflowInput{LogDir: "/data/runs/pre-upgrade-run-42"}
	if got := a.runIDFor(input); got != "pre-upgrade-run-42" {
		t.Errorf("runIDFor (no RunID) = %q, want %q", got, "pre-upgrade-run-42")
	}

	input.RunID = "explicit-run-id"
	if got := a.runIDFor(input); got != "explicit-run-id" {
		t.Errorf("runIDFor (RunID set) = %q, want the explicit value, got %q", got, got)
	}
}

// TestActivitiesSandboxResourceLimitsDefaultToPriorHardcodedLiterals proves
// an Activities built without SandboxMemory/SandboxCPUs/
// SandboxTmpfsSize set (every pre-existing caller and test) resolves to
// exactly the literals every sandboxed LaunchSpec here used before these
// became operator-configurable (PR #50's deferred item, closed
// 2026-09-05) -- so leaving cmd/factoryd's new flags at their defaults
// changes no run's actual resource ceiling.
func TestActivitiesSandboxResourceLimitsDefaultToPriorHardcodedLiterals(t *testing.T) {
	a := &Activities{}
	if got := a.sandboxMemory(); got != "4g" {
		t.Errorf("sandboxMemory() = %q, want the prior hardcoded 4g", got)
	}
	if got := a.sandboxCPUs(); got != "2" {
		t.Errorf("sandboxCPUs() = %q, want the prior hardcoded 2", got)
	}
	if got := a.sandboxTmpfsSize(); got != "1g" {
		t.Errorf("sandboxTmpfsSize() = %q, want 1g", got)
	}
}

// TestActivitiesSandboxResourceLimitsUseConfiguredValues proves a
// configured ceiling is actually honored, not silently overridden by the
// prior hardcoded default.
func TestActivitiesSandboxResourceLimitsUseConfiguredValues(t *testing.T) {
	a := &Activities{SandboxMemory: "8g", SandboxCPUs: "4", SandboxTmpfsSize: "512m"}
	if got := a.sandboxMemory(); got != "8g" {
		t.Errorf("sandboxMemory() = %q, want the configured 8g", got)
	}
	if got := a.sandboxCPUs(); got != "4" {
		t.Errorf("sandboxCPUs() = %q, want the configured 4", got)
	}
	if got := a.sandboxTmpfsSize(); got != "512m" {
		t.Errorf("sandboxTmpfsSize() = %q, want the configured 512m", got)
	}
}

// TestReviewStepPassedDecodesCombinedExit: a passing combined review exits
// 40, not 0, and must mark its progress stage passed.
func TestReviewStepPassedDecodesCombinedExit(t *testing.T) {
	for _, tc := range []struct {
		step string
		exit int
		want bool
	}{
		{reviewstep.Combined, reviewstep.CombinedExitBase, true},
		{reviewstep.Combined, reviewstep.CombinedExitBase + 1, false},
		{reviewstep.Combined, reviewstep.CombinedExitBase + 2, false},
		{reviewstep.Combined, 0, false}, // outside 40..43: a crash, never a pass
		{reviewstep.Combined, 1, false},
		{reviewstep.CodeReview, 0, true},
		{reviewstep.CodeReview, 40, false},
	} {
		if got := reviewStepPassed(tc.step, tc.exit); got != tc.want {
			t.Errorf("reviewStepPassed(%q, %d) = %v, want %v", tc.step, tc.exit, got, tc.want)
		}
	}
}
