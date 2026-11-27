package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"buildgate/internal/release"
	"buildgate/internal/run"
)

func TestIntegrationAgentCommitsItself(t *testing.T) {
	// not parallel-safe: newFixtureRepo calls testfixture.NewGitRepo, which uses t.Setenv (GIT_CONFIG_GLOBAL/GIT_CONFIG_SYSTEM); t.Setenv forbids t.Parallel on the same test.
	ws := newFixtureRepo(t)
	r := runFactoryd(t, ws, "commit", "true")

	if r.State != run.StateAccepted {
		t.Fatalf("state = %q, want %q", r.State, run.StateAccepted)
	}
	if r.CommittedByFactoryd {
		t.Error("CommittedByFactoryd = true, want false: the agent committed its own work")
	}
	assertClean(t, ws)
}

// TestIntegrationAcceptedRunRecordsReleaseDecision is the regression test
// for the 2026-09-03 Opus factory-pipeline review's item C4:
// internal/release's EvaluateDecision/SaveDecision/MergePolicyCheck existed
// and were correctly unit-tested, but had zero call sites anywhere in this
// repo, so no accepted run ever accumulated the Phase 7 evidence-window
// decision record the plan's own design calls for. Proves an accepted run
// produces a real, durable release-decisions/<run-id>.json -- groundwork
// only: this asserts Allowed is false with the default-restrictive policy
// (no merge/push side effect exists to gate), not that the decision permits
// anything.
func TestIntegrationAcceptedRunRecordsReleaseDecision(t *testing.T) {
	// not parallel-safe: newFixtureRepo calls testfixture.NewGitRepo, which uses t.Setenv (GIT_CONFIG_GLOBAL/GIT_CONFIG_SYSTEM); t.Setenv forbids t.Parallel on the same test.
	ws := newFixtureRepo(t)
	dataDir := t.TempDir()
	r := runFactorydWithSpecFlagsAndDataDir(t, ws, "commit", "true", "# fixture spec\nTests-Required: no -- integration fixture doesn't exercise tests_red_on_base\n", "30s", nil, nil, dataDir)
	if r.State != run.StateAccepted {
		t.Fatalf("state = %q, want %q", r.State, run.StateAccepted)
	}

	project := release.ProjectFromWorkspace(ws)
	decisionPath := filepath.Join(dataDir, "projects", project, "release-decisions", r.ID+".json")
	b, err := os.ReadFile(decisionPath)
	if err != nil {
		t.Fatalf("read release decision %s: %v", decisionPath, err)
	}
	var decision release.Decision
	if err := json.Unmarshal(b, &decision); err != nil {
		t.Fatalf("unmarshal release decision: %v", err)
	}
	if decision.RunID != r.ID {
		t.Errorf("decision.RunID = %q, want %q", decision.RunID, r.ID)
	}
	if decision.Allowed {
		t.Error("decision.Allowed = true, want false: the default MergePolicy (no -release-rollback-plan declared) must deny, not silently permit")
	}
	found := false
	for _, reason := range decision.Reasons {
		if strings.Contains(reason, "rollback plan") {
			found = true
		}
	}
	if !found {
		t.Errorf("decision.Reasons = %v, want one mentioning the missing rollback plan", decision.Reasons)
	}
}

// TestIntegrationKillSwitchEngagedDeniesReleaseDecision pins the
// `factoryd kill-switch` subcommand against the thing it exists to control.
// internal/release's Engage/Disengage/LoadKillSwitch were durable and
// unit-tested but had no caller outside their own tests, so the Phase 7
// precondition "a kill switch exists that a human can hit" was unmet in
// practice even though every accepted run's release decision already
// consults the switch. This proves the operator surface really reaches that
// consultation, in both directions: engaged denies with a reason naming the
// switch, disengaged does not.
func TestIntegrationKillSwitchEngagedDeniesReleaseDecision(t *testing.T) {
	// not parallel-safe: newFixtureRepo calls testfixture.NewGitRepo, which uses t.Setenv (GIT_CONFIG_GLOBAL/GIT_CONFIG_SYSTEM); t.Setenv forbids t.Parallel on the same test.
	dataDir := t.TempDir()
	engagedWorkspace := newFixtureRepo(t)
	project := release.ProjectFromWorkspace(engagedWorkspace)

	runKillSwitch := func(args ...string) string {
		t.Helper()
		out, err := factorydCommand(t, append([]string{"kill-switch", "-data-dir", dataDir, "-project", project}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("factoryd kill-switch %v: %v\n%s", args, err, out)
		}
		return string(out)
	}

	engaged := runKillSwitch("-state", "engaged", "-by", "operator", "-reason", "halting releases while the oracle is under review")
	if !strings.Contains(engaged, "engaged=true") {
		t.Fatalf("kill-switch engage output = %q, want it to report engaged=true", engaged)
	}

	denied := runFactorydWithSpecFlagsAndDataDir(t, engagedWorkspace, "commit", "true", "# fixture spec\nTests-Required: no -- integration fixture doesn't exercise tests_red_on_base\n", "30s", nil, nil, dataDir)
	if denied.State != run.StateAccepted {
		t.Fatalf("state = %q, want %q", denied.State, run.StateAccepted)
	}
	deniedDecision := readReleaseDecision(t, dataDir, project, denied.ID)
	if deniedDecision.Allowed {
		t.Error("decision.Allowed = true, want false while the kill switch is engaged")
	}
	if !slices.ContainsFunc(deniedDecision.Reasons, func(reason string) bool { return strings.Contains(reason, "kill switch is engaged") }) {
		t.Errorf("decision.Reasons = %v, want one naming the engaged kill switch", deniedDecision.Reasons)
	}

	// Disengaging is the same operator surface resolved the other way: the
	// next run's decision must no longer carry the kill-switch reason. It
	// stays denied on the default-restrictive policy's own grounds (no
	// rollback plan declared), which is exactly what this asserts around.
	runKillSwitch("-state", "disengaged", "-by", "operator", "-reason", "oracle review complete")
	// The same repository again (a fresh run id): the project id is the
	// repository's own, so a sibling checkout under the same parent would
	// be a different project, not "the next run" of this one.
	after := runFactorydWithSpecFlagsAndDataDir(t, engagedWorkspace, "commit", "true", "# fixture spec\nTests-Required: no -- integration fixture doesn't exercise tests_red_on_base\n", "30s", nil, nil, dataDir)
	if after.State != run.StateAccepted {
		t.Fatalf("state after disengage = %q, want %q", after.State, run.StateAccepted)
	}
	afterDecision := readReleaseDecision(t, dataDir, project, after.ID)
	if slices.ContainsFunc(afterDecision.Reasons, func(reason string) bool { return strings.Contains(reason, "kill switch") }) {
		t.Errorf("decision.Reasons = %v, want none mentioning the kill switch after disengaging", afterDecision.Reasons)
	}
}

// TestIntegrationKillSwitchRequiresAttributionForATransition proves the
// operator surface fails closed on an unattributable transition -- a safety
// action with no operator and no reason is not auditable evidence, the same
// requirement run.ApplyOverride enforces -- while still allowing an
// unattributed read of the current state.
func TestIntegrationKillSwitchRequiresAttributionForATransition(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	out, err := factorydCommand(t, "kill-switch", "-data-dir", dataDir, "-project", "fixture", "-state", "engaged").CombinedOutput()
	if err == nil {
		t.Fatalf("factoryd kill-switch -state engaged with no -by/-reason: want an error, got success\n%s", out)
	}
	if !strings.Contains(string(out), "-by and -reason are required") {
		t.Fatalf("output = %q, want it to name the missing attribution flags", out)
	}

	status, err := factorydCommand(t, "kill-switch", "-data-dir", dataDir, "-project", "fixture").CombinedOutput()
	if err != nil {
		t.Fatalf("factoryd kill-switch status read: %v\n%s", err, status)
	}
	if !strings.Contains(string(status), "engaged=false") {
		t.Fatalf("status output = %q, want it to report the refused transition never landed", status)
	}
}

// TestIntegrationKillSwitchTransitionsSerializeAcrossProcesses is the
// regression test for a real P1 from Codex review of PR #46: adding the
// `factoryd kill-switch` CLI made kill-switch transitions reachable from
// separate processes for the first time, but release.transition protected
// its load-modify-save with only a process-local mutex. Two concurrent
// invocations could each load the same record, each append one transition,
// and the later save would silently drop the earlier operator's
// attributable entry — an emergency engage plausibly being the loser.
//
// Proving that by racing subprocesses would be probabilistic (the
// load-modify-save window is microseconds, process startup is
// milliseconds), so this proves the guarantee that makes the race
// impossible instead, deterministically: this test process holds the
// project's own advisory lock, and a real `factoryd kill-switch`
// subprocess must actually wait for it rather than proceed on a record it
// could still be reading stale. Red before the fix, where nothing consults
// a lock file at all and the subprocess completes immediately.
func TestIntegrationKillSwitchTransitionsSerializeAcrossProcesses(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	project := "serialized-fixture"
	killSwitch := func(args ...string) *exec.Cmd {
		return factorydCommand(t, append([]string{"kill-switch", "-data-dir", dataDir, "-project", project}, args...)...)
	}

	// One completed transition first, so the record and its history exist
	// and the second transition below has something it could clobber.
	if out, err := killSwitch("-state", "engaged", "-by", "first-operator", "-reason", "halt releases").CombinedOutput(); err != nil {
		t.Fatalf("first kill-switch transition: %v\n%s", err, out)
	}

	lockPath, err := release.KillSwitchLockPath(dataDir, project)
	if err != nil {
		t.Fatalf("resolve kill switch lock path: %v", err)
	}
	held, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatalf("open kill switch lock: %v", err)
	}
	defer held.Close()
	if err := syscall.Flock(int(held.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatalf("hold kill switch lock: %v", err)
	}

	blocked := killSwitch("-state", "disengaged", "-by", "second-operator", "-reason", "review complete")
	var blockedOutput bytes.Buffer
	blocked.Stdout, blocked.Stderr = &blockedOutput, &blockedOutput
	if err := blocked.Start(); err != nil {
		t.Fatalf("start blocked kill-switch invocation: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- blocked.Wait() }()

	select {
	case err := <-done:
		_ = syscall.Flock(int(held.Fd()), syscall.LOCK_UN)
		t.Fatalf("kill-switch invocation completed (err=%v) while another process held the project lock -- transitions are not serialized across processes\n%s", err, blockedOutput.String())
	case <-time.After(750 * time.Millisecond):
	}

	if err := syscall.Flock(int(held.Fd()), syscall.LOCK_UN); err != nil {
		t.Fatalf("release kill switch lock: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("kill-switch invocation after lock release: %v\n%s", err, blockedOutput.String())
		}
	case <-time.After(10 * time.Second):
		_ = blocked.Process.Kill()
		t.Fatalf("kill-switch invocation never completed after the lock was released\n%s", blockedOutput.String())
	}

	record, err := release.LoadKillSwitch(dataDir, project)
	if err != nil {
		t.Fatalf("load kill switch: %v", err)
	}
	if record.Engaged {
		t.Errorf("record.Engaged = true, want false after the second transition")
	}
	var attributions []string
	for _, transition := range record.History {
		attributions = append(attributions, transition.By)
	}
	if !slices.Equal(attributions, []string{"first-operator", "second-operator"}) {
		t.Errorf("history attributions = %v, want both operators' transitions retained in order", attributions)
	}
}

func readReleaseDecision(t *testing.T, dataDir, project, runID string) release.Decision {
	t.Helper()
	path := filepath.Join(dataDir, "projects", project, "release-decisions", runID+".json")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read release decision %s: %v", path, err)
	}
	var decision release.Decision
	if err := json.Unmarshal(b, &decision); err != nil {
		t.Fatalf("unmarshal release decision: %v", err)
	}
	return decision
}

// TestIntegrationOverrideRateReportsAcceptedAndOverriddenCounts pins the
// `factoryd override-rate` subcommand added per the plan's 2026-08-28
// Opus review: it must count one plainly-accepted run and one
// accepted-via-override run correctly, and its output must name both.
func TestIntegrationOverrideRateReportsAcceptedAndOverriddenCounts(t *testing.T) {
	// not parallel-safe: newFixtureRepo calls testfixture.NewGitRepo, which uses t.Setenv (GIT_CONFIG_GLOBAL/GIT_CONFIG_SYSTEM); t.Setenv forbids t.Parallel on the same test.
	dataDir := t.TempDir()

	// A plain, unattended acceptance.
	clean := runFactorydWithSpecFlagsAndDataDir(t, newFixtureRepo(t), "commit", "true", "# fixture spec\nTests-Required: no -- integration fixture doesn't exercise tests_red_on_base\n", "30s", nil, nil, dataDir)
	if clean.State != run.StateAccepted {
		t.Fatalf("clean run state = %q, want %q", clean.State, run.StateAccepted)
	}

	// A run that quarantines on scope, then is overridden to accepted.
	specContent := "# Ticket: fixture\n\n## Out of scope\n\nAllowed-Files: content.txt\n"
	quarantined := runFactorydWithSpecFlagsAndDataDir(t, newFixtureRepo(t), "commit_extra", "true", specContent, "30s", nil, nil, dataDir)
	if quarantined.State != run.StateQuarantined {
		t.Fatalf("quarantined run state = %q, want %q", quarantined.State, run.StateQuarantined)
	}
	overrideCmd := factorydCommand(t, "override",
		"-run", quarantined.ID,
		"-data-dir", dataDir,
		"-by", "operator",
		"-reason", "reviewed manually: out-of-scope file was benign",
		"-state", "accepted",
	)
	if out, err := overrideCmd.CombinedOutput(); err != nil {
		t.Fatalf("factoryd override: %v: %s", err, out)
	}

	rateCmd := factorydCommand(t, "override-rate", "-data-dir", dataDir)
	out, err := rateCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("factoryd override-rate: %v: %s", err, out)
	}
	got := string(out)
	for _, want := range []string{
		"runs recorded: 2",
		"reached accepted: 2",
		"accepted via human override: 1",
		"any override recorded (accepted or not): 1",
		"override rate among accepted runs: 50%",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("override-rate output = %q, want it to contain %q", got, want)
		}
	}
}

// TestIntegrationOverrideRecordsReleaseDecision is the regression test for
// a real Codex review finding on PR #45: an override to accepted is
// exactly the internal/release.MergePolicy.AllowOverrides=true case, but
// neither `factoryd override` nor the initial (quarantined) result path
// ever called recordReleaseDecision -- an overridden run had no
// release-decision file at all.
func TestIntegrationOverrideRecordsReleaseDecision(t *testing.T) {
	// not parallel-safe: newFixtureRepo calls testfixture.NewGitRepo, which uses t.Setenv (GIT_CONFIG_GLOBAL/GIT_CONFIG_SYSTEM); t.Setenv forbids t.Parallel on the same test.
	dataDir := t.TempDir()
	ws := newFixtureRepo(t)
	specContent := "# Ticket: fixture\n\n## Out of scope\n\nAllowed-Files: content.txt\n"
	quarantined := runFactorydWithSpecFlagsAndDataDir(t, ws, "commit_extra", "true", specContent, "30s", nil, nil, dataDir)
	if quarantined.State != run.StateQuarantined {
		t.Fatalf("quarantined run state = %q, want %q", quarantined.State, run.StateQuarantined)
	}

	project := release.ProjectFromWorkspace(ws)
	decisionPath := filepath.Join(dataDir, "projects", project, "release-decisions", quarantined.ID+".json")
	if _, err := os.Stat(decisionPath); !os.IsNotExist(err) {
		t.Fatalf("release decision %s exists before override, err=%v -- want none recorded for a run that never reached accepted", decisionPath, err)
	}

	overrideCmd := factorydCommand(t, "override",
		"-run", quarantined.ID,
		"-data-dir", dataDir,
		"-by", "operator",
		"-reason", "reviewed manually: out-of-scope file was benign",
		"-state", "accepted",
		"-release-allow-overrides",
	)
	if out, err := overrideCmd.CombinedOutput(); err != nil {
		t.Fatalf("factoryd override: %v: %s", err, out)
	}

	b, err := os.ReadFile(decisionPath)
	if err != nil {
		t.Fatalf("read release decision %s after override: %v", decisionPath, err)
	}
	var decision release.Decision
	if err := json.Unmarshal(b, &decision); err != nil {
		t.Fatalf("unmarshal release decision: %v", err)
	}
	if decision.RunID != quarantined.ID {
		t.Errorf("decision.RunID = %q, want %q", decision.RunID, quarantined.ID)
	}
}
