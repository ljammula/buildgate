package main

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	"buildgate/internal/run"
)

const baselineFixtureSpec = "# fixture spec\nTests-Required: no -- integration fixture doesn't exercise tests_added\n"

// TestIntegrationBaselineVerifyHaltsBeforeTheBuild runs the real binary on
// a verify command that fails on the base commit with a test the ticket
// does not name: the run halts with the test named, the build never starts,
// and the worktree and branch are discarded.
func TestIntegrationBaselineVerifyHaltsBeforeTheBuild(t *testing.T) {
	ws := newFixtureRepo(t)
	r := runFactorydWithSpec(t, ws, "commit", "echo '--- FAIL: TestPager (0.00s)'; echo leftover > left.txt; exit 1", baselineFixtureSpec, "30s")

	if r.State != run.StateHalted || r.HaltReasonCode != run.HaltReasonBaselineVerifyFailed {
		t.Fatalf("state = %q, halt reason = %q; want halted with %q", r.State, r.HaltReasonCode, run.HaltReasonBaselineVerifyFailed)
	}
	if len(r.Attempts) != 1 || r.Attempts[0].Kind != run.BaselineVerifyAttemptKind || r.Attempts[0].ExitCode != 1 {
		t.Errorf("Attempts = %+v, want only the failed baseline verify: no build ran", r.Attempts)
	}
	const summary = "failed: TestPager; the ticket does not name it"
	if r.BaselineVerify == nil || r.BaselineVerify.Summary() != summary || r.BaselineVerify.BaseSHA != r.BaseSHA {
		t.Errorf("BaselineVerify = %+v, want %q on base %s", r.BaselineVerify, summary, r.BaseSHA)
	}
	if want := "halted before the build: baseline verify " + summary; r.Triage != want {
		t.Errorf("Triage = %q, want %q", r.Triage, want)
	}
	if !strings.HasPrefix(r.HaltError, "No model call was made") {
		t.Errorf("HaltError = %q, want what to do about it", r.HaltError)
	}
	if r.AgentEvidence != nil || len(r.GateResults) != 0 {
		t.Errorf("a run halted on its baseline has build evidence or gate results: %+v %+v", r.AgentEvidence, r.GateResults)
	}
	if _, err := os.Stat(r.WorkspacePath); !os.IsNotExist(err) {
		t.Errorf("the halted run's worktree %s still exists (%v)", r.WorkspacePath, err)
	}
	if out, _ := exec.Command("git", "-C", ws, "status", "--porcelain").Output(); len(out) != 0 {
		t.Errorf("the operator's checkout was touched: %s", out)
	}
}

// TestIntegrationBaselineVerifyFailureTheTicketNamesBuilds: the same failure,
// named by the ticket, lets the build run and gives it the note; the run is
// then judged by canonical verification as any other.
func TestIntegrationBaselineVerifyFailureTheTicketNamesBuilds(t *testing.T) {
	ws := newFixtureRepo(t)
	r := runFactorydWithSpec(t, ws, "commit", afterBaselineFailing(t, "echo '--- FAIL: TestPager (0.00s)'; echo leftover > left.txt; exit 1"), "# fixture spec\n\nMake `TestPager` pass.\n\nTests-Required: no -- integration fixture doesn't exercise tests_added\n", "30s")

	if r.State != run.StateAccepted {
		t.Fatalf("state = %q, want accepted (triage %q, halt %q)", r.State, r.Triage, r.HaltError)
	}
	if r.BaselineVerify == nil || !r.BaselineVerify.Expected || r.BaselineVerify.Summary() != "failed as the ticket expects: TestPager" {
		t.Fatalf("BaselineVerify = %+v, want a failure the ticket expects", r.BaselineVerify)
	}
	if len(r.Attempts) < 2 || r.Attempts[0].Kind != run.BaselineVerifyAttemptKind || r.Attempts[0].ExitCode != 1 || r.Attempts[1].Kind != "build" {
		t.Fatalf("Attempts = %+v, want the failed baseline, then the build", r.Attempts)
	}
	log, err := os.ReadFile(r.Attempts[1].LogPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(log), "fake_build_app: baseline_failure=Before this build, the verify command was run on the untouched repository and failed (exit 1).") || !strings.Contains(string(log), "|- TestPager|") {
		t.Errorf("the build was not given the baseline note:\n%s", log)
	}
	// What the baseline's command wrote is not part of the result.
	for _, f := range r.ChangedFiles {
		if f == "left.txt" {
			t.Errorf("ChangedFiles = %v: the baseline's leftover reached the build's result", r.ChangedFiles)
		}
	}
}

// TestIntegrationBaselineVerifyThatPassesIsOnTheAcceptedRun is the common
// case: the result is recorded and the run goes on as before.
func TestIntegrationBaselineVerifyThatPassesIsOnTheAcceptedRun(t *testing.T) {
	ws := newFixtureRepo(t)
	r := runFactorydWithSpec(t, ws, "commit", "true", baselineFixtureSpec, "30s")
	if r.State != run.StateAccepted {
		t.Fatalf("state = %q, want accepted", r.State)
	}
	if r.BaselineVerify == nil || !r.BaselineVerify.Passed || r.BaselineVerify.Command != "true" {
		t.Errorf("BaselineVerify = %+v, want passed", r.BaselineVerify)
	}
	afterBaselineAttempt(t, r.Attempts)
}

// TestIntegrationBaselineVerifyThatLeavesAFileOutsideAllowedFilesHalts: the
// command passes but writes a file the repository does not ignore and the
// ticket does not allow, which every build's commit would carry into a
// diff_scope quarantine: the run halts before the build, the file is not in
// the worktree and the operator's checkout is untouched.
func TestIntegrationBaselineVerifyThatLeavesAFileOutsideAllowedFilesHalts(t *testing.T) {
	ws := newFixtureRepo(t)
	spec := "# fixture spec\n\nAllowed-Files: content.txt\nTests-Required: no -- integration fixture doesn't exercise tests_added\n"
	r := runFactorydWithSpec(t, ws, "commit", "echo bytes > left.pyc", spec, "30s")

	if r.State != run.StateHalted || r.HaltReasonCode != run.HaltReasonBaselineVerifyFailed {
		t.Fatalf("state = %q, halt reason = %q (triage %q); want halted with %q", r.State, r.HaltReasonCode, r.Triage, run.HaltReasonBaselineVerifyFailed)
	}
	if len(r.Attempts) != 1 || r.Attempts[0].Kind != run.BaselineVerifyAttemptKind {
		t.Errorf("Attempts = %+v, want only the baseline verify: no build ran", r.Attempts)
	}
	const summary = "passed, but the command leaves left.pyc outside the ticket's Allowed-Files"
	if r.BaselineVerify == nil || r.BaselineVerify.Summary() != summary {
		t.Errorf("BaselineVerify = %+v, want %q", r.BaselineVerify, summary)
	}
	if want := "halted before the build: baseline verify " + summary; r.Triage != want {
		t.Errorf("Triage = %q, want %q", r.Triage, want)
	}
	if !strings.Contains(r.HaltError, ".gitignore") {
		t.Errorf("HaltError = %q, want the .gitignore advice", r.HaltError)
	}
	if out, _ := exec.Command("git", "-C", ws, "status", "--porcelain").Output(); len(out) != 0 {
		t.Errorf("the operator's checkout was touched: %s", out)
	}
}
