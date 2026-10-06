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

func TestIntegrationDaemonRefusesOverrideTokenInEnvironment(t *testing.T) {
	t.Parallel()
	cmd := factorydCommand(t, "daemon",
		"-temporal-address", "localhost:7233",
		"-repository", "fixture/repo-refuses-token",
		"-data-dir", t.TempDir(),
	)
	cmd.Env = append(os.Environ(), "FACTORYD_API_OVERRIDE_TOKEN=leaked-secret-value")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("factoryd daemon exited 0 with the override token set in its own environment; output:\n%s", out)
	}
	if !strings.Contains(string(out), "FACTORYD_API_OVERRIDE_TOKEN") {
		t.Fatalf("output = %q, want it to name the misconfigured variable", out)
	}
}

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
	r := runFactorydWithSpecFlagsAndDataDir(t, ws, "commit", "true", "# fixture spec\nTests-Required: no -- integration fixture doesn't exercise tests_added\n", "30s", nil, nil, dataDir)
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

	denied := runFactorydWithSpecFlagsAndDataDir(t, engagedWorkspace, "commit", "true", "# fixture spec\nTests-Required: no -- integration fixture doesn't exercise tests_added\n", "30s", nil, nil, dataDir)
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
	after := runFactorydWithSpecFlagsAndDataDir(t, engagedWorkspace, "commit", "true", "# fixture spec\nTests-Required: no -- integration fixture doesn't exercise tests_added\n", "30s", nil, nil, dataDir)
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
	clean := runFactorydWithSpecFlagsAndDataDir(t, newFixtureRepo(t), "commit", "true", "# fixture spec\nTests-Required: no -- integration fixture doesn't exercise tests_added\n", "30s", nil, nil, dataDir)
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

// fixtureCheckProjectArtifacts writes a well-formed spec.md,
// architecture.md, contract.md, and ticket-001.md into a fresh temp dir
// and returns their paths — the "everything should pass" baseline for
// `factoryd check-project` tests, following exactly the section names/
// order each of policy.ProductSpecFrozen/ArchitectureStructure/
// ProgramDesignStructure/TicketStructure requires.
func fixtureCheckProjectArtifacts(t *testing.T) (specPath, architecturePath, contractPath, ticketPath string) {
	t.Helper()
	dir := t.TempDir()
	write := func(name, content string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		return path
	}
	specPath = write("spec.md", "STATUS: FROZEN -- reviewed 2026-08-29\n\n# Fixture spec\n")
	architecturePath = write("architecture.md", "# Architecture\n\n## Repo layout\nstuff\n\n## Verification\nrun make verify\n\n## Known deviations\nnone\n")
	contractPath = write("contract.md", "# Contract\n\n## Conventions\nJSON everywhere\n\n## Endpoint: Add\nPOST /add\n")
	ticketPath = write("ticket-001.md", "This is a brand-new, empty, already-git-init-ed repo.\n\n"+
		"## Goal\nbuild it\n\n"+
		"## Required changes\nUpdate `ARCHITECTURE.md` and append a `PROGRESS.md` entry before finishing.\n\n"+
		"## Verification\nrun make verify\n\n"+
		"## Commit\nticket(001): x\n")
	return specPath, architecturePath, contractPath, ticketPath
}

// TestIntegrationCheckProjectAcceptanceSuiteWiredPassesWhenStaged proves
// the acceptance_suite_wired check (policy.AcceptanceSuiteWired) is
// actually reachable through the CLI: a Makefile that references every
// drafted spec/acceptance/<NNN> slice passes.
func TestIntegrationCheckProjectAcceptanceSuiteWiredPassesWhenStaged(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "spec", "acceptance", "001"), 0o755); err != nil {
		t.Fatalf("mkdir acceptance slice: %v", err)
	}
	makefilePath := filepath.Join(dir, "Makefile")
	makefile := "verify-full: verify\n\t(cd 'spec/acceptance/001' && go test ./...)\n"
	if err := os.WriteFile(makefilePath, []byte(makefile), 0o644); err != nil {
		t.Fatalf("write Makefile: %v", err)
	}
	dataDir := t.TempDir()

	cmd := factorydCommand(t, "check-project",
		"-project", "fixture-project",
		"-makefile", makefilePath,
		"-data-dir", dataDir,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("factoryd check-project: %v: %s", err, out)
	}
	if !strings.Contains(string(out), "acceptance_suite_wired: PASS") {
		t.Errorf("output = %q, want it to contain %q", out, "acceptance_suite_wired: PASS")
	}
}

// TestIntegrationCheckProjectAcceptanceSuiteWiredFailsWhenUnstaged proves
// the inverse: a slice drafted on disk but never mentioned in the
// Makefile fails the check, exits nonzero, and names the unstaged slice.
func TestIntegrationCheckProjectAcceptanceSuiteWiredFailsWhenUnstaged(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "spec", "acceptance", "001"), 0o755); err != nil {
		t.Fatalf("mkdir acceptance slice: %v", err)
	}
	makefilePath := filepath.Join(dir, "Makefile")
	if err := os.WriteFile(makefilePath, []byte("verify:\n\tgo test ./...\n"), 0o644); err != nil {
		t.Fatalf("write Makefile: %v", err)
	}
	dataDir := t.TempDir()

	cmd := factorydCommand(t, "check-project",
		"-project", "fixture-project",
		"-makefile", makefilePath,
		"-data-dir", dataDir,
	)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("factoryd check-project: want a nonzero exit for an unstaged slice, got success: %s", out)
	}
	if !strings.Contains(string(out), "acceptance_suite_wired: FAIL") {
		t.Errorf("output = %q, want it to contain %q", out, "acceptance_suite_wired: FAIL")
	}
	if !strings.Contains(string(out), filepath.Join("spec", "acceptance", "001")) {
		t.Errorf("output = %q, want it to name the unstaged slice", out)
	}
}

// TestIntegrationCheckProjectAcceptanceSuiteWiredPassesWithNoSliceDrafted
// proves the check is opt-in-per-artifact the same way every other check
// is: a project with no spec/acceptance directory at all (nothing
// drafted yet) passes rather than erroring on a missing directory.
func TestIntegrationCheckProjectAcceptanceSuiteWiredPassesWithNoSliceDrafted(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	makefilePath := filepath.Join(dir, "Makefile")
	if err := os.WriteFile(makefilePath, []byte("verify:\n\tgo test ./...\n"), 0o644); err != nil {
		t.Fatalf("write Makefile: %v", err)
	}
	dataDir := t.TempDir()

	cmd := factorydCommand(t, "check-project",
		"-project", "fixture-project",
		"-makefile", makefilePath,
		"-data-dir", dataDir,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("factoryd check-project: %v: %s", err, out)
	}
	if !strings.Contains(string(out), "acceptance_suite_wired: PASS") {
		t.Errorf("output = %q, want it to contain %q", out, "acceptance_suite_wired: PASS")
	}
}

// TestIntegrationCheckProjectAcceptanceSuiteWiredFailsOnUnreadableDirectory
// is the regression test for a real Codex review finding (PR #59): an
// unreadable (not missing) spec/acceptance directory used to be treated
// identically to a missing one -- silently recording PASS with no slices,
// never actually inspecting whatever suite really exists there. Only
// os.IsNotExist should mean "nothing drafted yet"; any other error (here,
// a permission denial) must fail the check instead.
func TestIntegrationCheckProjectAcceptanceSuiteWiredFailsOnUnreadableDirectory(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("running as root: chmod 0 does not deny root read access, so this test's premise doesn't hold")
	}
	dir := t.TempDir()
	makefilePath := filepath.Join(dir, "Makefile")
	if err := os.WriteFile(makefilePath, []byte("verify:\n\tgo test ./...\n"), 0o644); err != nil {
		t.Fatalf("write Makefile: %v", err)
	}
	acceptanceRoot := filepath.Join(dir, "spec", "acceptance")
	if err := os.MkdirAll(acceptanceRoot, 0o750); err != nil {
		t.Fatalf("mkdir spec/acceptance: %v", err)
	}
	if err := os.Chmod(acceptanceRoot, 0o000); err != nil {
		t.Fatalf("chmod spec/acceptance unreadable: %v", err)
	}
	defer os.Chmod(acceptanceRoot, 0o750) // restore so t.TempDir() cleanup can remove it
	dataDir := t.TempDir()

	cmd := factorydCommand(t, "check-project",
		"-project", "fixture-project",
		"-makefile", makefilePath,
		"-data-dir", dataDir,
	)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("factoryd check-project unexpectedly succeeded against an unreadable spec/acceptance: %s", out)
	}
	if !strings.Contains(string(out), "acceptance_suite_wired: FAIL") {
		t.Errorf("output = %q, want it to contain %q", out, "acceptance_suite_wired: FAIL")
	}
	if !strings.Contains(string(out), "could not read") {
		t.Errorf("output = %q, want it to name the unreadable directory, not silently pass", out)
	}
}

// TestIntegrationCheckProjectAllChecksPass is the happy path for
// `factoryd check-project`: well-formed spec/architecture/contract/ticket
// artifacts all pass their own structural check, the process exits 0, and
// the durable ProjectCheckRecord reflects all four as passed — this is
// the invocation point policy.ProductSpecFrozen/TicketStructure/
// ArchitectureStructure/ProgramDesignStructure were written for but never
// had (see each function's own "factoryd does not invoke it yet" doc
// comment).
func TestIntegrationCheckProjectAllChecksPass(t *testing.T) {
	t.Parallel()
	specPath, architecturePath, contractPath, ticketPath := fixtureCheckProjectArtifacts(t)
	dataDir := t.TempDir()

	cmd := factorydCommand(t, "check-project",
		"-project", "fixture-project",
		"-spec", specPath,
		"-architecture", architecturePath,
		"-contract", contractPath,
		"-ticket", ticketPath,
		"-ticket-number", "1",
		"-data-dir", dataDir,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("factoryd check-project: %v: %s", err, out)
	}
	for _, want := range []string{
		"product_spec_frozen: PASS",
		"program_design_structure: PASS",
		"architecture_structure: PASS",
		"ticket_structure: PASS",
	} {
		if !strings.Contains(string(out), want) {
			t.Errorf("output = %q, want it to contain %q", out, want)
		}
	}

	entries, err := os.ReadDir(filepath.Join(dataDir, "project-checks"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("expected exactly 1 project-check record: entries=%v err=%v", entries, err)
	}
	b, err := os.ReadFile(filepath.Join(dataDir, "project-checks", entries[0].Name()))
	if err != nil {
		t.Fatalf("read project-check record: %v", err)
	}
	var record ProjectCheckRecord
	if err := json.Unmarshal(b, &record); err != nil {
		t.Fatalf("unmarshal project-check record: %v", err)
	}
	if !record.Passed {
		t.Errorf("record.Passed = false, want true: %+v", record)
	}
	if record.Project != "fixture-project" {
		t.Errorf("record.Project = %q, want %q", record.Project, "fixture-project")
	}
	if len(record.Results) != 4 {
		t.Errorf("record.Results = %+v, want exactly 4 (one per declared artifact)", record.Results)
	}
	for _, r := range record.Results {
		if !r.Passed {
			t.Errorf("check %q did not pass: %+v", r.Check, r)
		}
	}
}

// TestIntegrationCheckProjectBrownfieldProfileSkipsSpecAndContractAdvisoryArchitecture
// is the regression test for the gap live validation found, 2026-09-08:
// -preflight-profile was wired into factoryd <run>'s own mandatory
// preflight and the API's /projects/check dry-run, but check-project --
// the standalone CLI preview command factoryd onboard's own output tells
// an operator to run -- never got the flag at all, so an operator
// following that exact advice on a real brownfield repo (a DRAFT spec.md,
// no contract.md yet) hit "flag provided but not defined" instead of the
// brownfield behavior the other two entry points already have.
func TestIntegrationCheckProjectBrownfieldProfileSkipsSpecAndContractAdvisoryArchitecture(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	write := func(name, content string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		return path
	}
	// Deliberately still-DRAFT and missing every required section -- both
	// would fail a strict check-project run; brownfield must skip the
	// first entirely and only advisory-fail the second.
	specPath := write("spec.md", "STATUS: DRAFT -- pending human review\n\n# Fixture spec\n")
	architecturePath := write("architecture.md", "# Architecture\n\nnothing here yet\n")
	dataDir := t.TempDir()

	cmd := factorydCommand(t, "check-project",
		"-project", "brownfield-project",
		"-spec", specPath,
		"-architecture", architecturePath,
		"-data-dir", dataDir,
		"-preflight-profile", "brownfield",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("factoryd check-project -preflight-profile brownfield: %v: %s", err, out)
	}
	if strings.Contains(string(out), "product_spec_frozen") {
		t.Errorf("output = %q, brownfield must skip product_spec_frozen entirely", out)
	}
	if !strings.Contains(string(out), "architecture_structure: FAIL (advisory)") {
		t.Errorf("output = %q, want architecture_structure reported FAIL but marked advisory", out)
	}

	entries, err := os.ReadDir(filepath.Join(dataDir, "project-checks"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("expected exactly 1 project-check record: entries=%v err=%v", entries, err)
	}
	b, err := os.ReadFile(filepath.Join(dataDir, "project-checks", entries[0].Name()))
	if err != nil {
		t.Fatalf("read project-check record: %v", err)
	}
	var record ProjectCheckRecord
	if err := json.Unmarshal(b, &record); err != nil {
		t.Fatalf("unmarshal project-check record: %v", err)
	}
	if !record.Passed {
		t.Errorf("record.Passed = false, want true -- an advisory-only failure must not fail the overall check: %+v", record)
	}
	if len(record.Results) != 1 {
		t.Errorf("record.Results = %+v, want exactly 1 (architecture_structure only; product_spec_frozen skipped entirely)", record.Results)
	}
}

// TestIntegrationCheckProjectRejectsUnknownPreflightProfile proves an
// unrecognized -preflight-profile value fails closed with a message naming
// both the flag and the one accepted non-empty value, rather than silently
// falling back to strict or brownfield.
func TestIntegrationCheckProjectRejectsUnknownPreflightProfile(t *testing.T) {
	t.Parallel()
	specPath, _, _, _ := fixtureCheckProjectArtifacts(t)
	out, err := factorydCommand(t, "check-project",
		"-project", "fixture-project",
		"-spec", specPath,
		"-data-dir", t.TempDir(),
		"-preflight-profile", "nonsense",
	).CombinedOutput()
	if err == nil {
		t.Fatalf("expected a nonzero exit for an unrecognized -preflight-profile, got success: %s", out)
	}
	if !strings.Contains(string(out), `-preflight-profile must be "" or "brownfield"`) {
		t.Errorf("output = %q, want it to name the accepted values", out)
	}
}

// TestIntegrationCheckProjectReportsFailingChecks proves a genuinely
// malformed ticket (missing the required ARCHITECTURE.md/PROGRESS.md
// update instruction) is caught: the process exits nonzero, the failure
// reason is printed, and the durable record marks that specific check
// failed with its reason while a well-formed spec passed alongside it —
// this run declares only -spec and -ticket, proving each check is
// independently opt-in per declared artifact.
func TestIntegrationCheckProjectReportsFailingChecks(t *testing.T) {
	t.Parallel()
	specPath, _, _, _ := fixtureCheckProjectArtifacts(t)
	dir := t.TempDir()
	badTicketPath := filepath.Join(dir, "ticket-001.md")
	// Missing the required ARCHITECTURE.md/PROGRESS.md instruction under
	// "## Required changes".
	badTicket := "This is a brand-new, empty, already-git-init-ed repo.\n\n" +
		"## Goal\nbuild it\n\n" +
		"## Required changes\ndo the thing\n\n" +
		"## Verification\nrun make verify\n\n" +
		"## Commit\nticket(001): x\n"
	if err := os.WriteFile(badTicketPath, []byte(badTicket), 0o644); err != nil {
		t.Fatalf("write bad ticket: %v", err)
	}
	dataDir := t.TempDir()

	cmd := factorydCommand(t, "check-project",
		"-project", "fixture-project",
		"-spec", specPath,
		"-ticket", badTicketPath,
		"-ticket-number", "1",
		"-data-dir", dataDir,
	)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected a nonzero exit for a malformed ticket, got success: %s", out)
	}
	if !strings.Contains(string(out), "product_spec_frozen: PASS") {
		t.Errorf("output = %q, want the well-formed spec to still pass", out)
	}
	if !strings.Contains(string(out), "ticket_structure: FAIL") ||
		!strings.Contains(string(out), "ARCHITECTURE.md/PROGRESS.md update instruction") {
		t.Errorf("output = %q, want ticket_structure to fail with its specific reason", out)
	}

	entries, err := os.ReadDir(filepath.Join(dataDir, "project-checks"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("expected exactly 1 project-check record: entries=%v err=%v", entries, err)
	}
	b, err := os.ReadFile(filepath.Join(dataDir, "project-checks", entries[0].Name()))
	if err != nil {
		t.Fatalf("read project-check record: %v", err)
	}
	var record ProjectCheckRecord
	if err := json.Unmarshal(b, &record); err != nil {
		t.Fatalf("unmarshal project-check record: %v", err)
	}
	if record.Passed {
		t.Errorf("record.Passed = true, want false: %+v", record)
	}
	if len(record.Results) != 2 {
		t.Fatalf("record.Results = %+v, want exactly 2 (spec + ticket only — architecture/contract were never declared)", record.Results)
	}
}

// TestIntegrationCheckProjectRequiresAtLeastOneArtifact confirms
// check-project refuses to run (and writes no durable record) when the
// caller declares none of -spec/-contract/-architecture/-ticket — there
// is nothing to check.
func TestIntegrationCheckProjectRequiresAtLeastOneArtifact(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	cmd := factorydCommand(t, "check-project", "-project", "fixture-project", "-data-dir", dataDir)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected a nonzero exit with no artifacts declared, got success: %s", out)
	}
	if !strings.Contains(string(out), "at least one of -spec, -contract, -architecture, -ticket, -makefile is required") {
		t.Errorf("output = %q, want it to name the missing declaration", out)
	}
	if entries, err := os.ReadDir(filepath.Join(dataDir, "project-checks")); err == nil && len(entries) != 0 {
		t.Errorf("project-checks dir = %v, want no record written when nothing was checked", entries)
	}
}

// TestIntegrationCheckProjectRequiresProject confirms -project is
// required even when an artifact is declared — the durable record's
// filename is keyed by it, and "no project identifier" is exactly the
// same class of missing-required-flag error as -ticket/-workspace/-spec
// on the ordinary run path.
func TestIntegrationCheckProjectRequiresProject(t *testing.T) {
	t.Parallel()
	specPath, _, _, _ := fixtureCheckProjectArtifacts(t)
	dataDir := t.TempDir()
	cmd := factorydCommand(t, "check-project", "-spec", specPath, "-data-dir", dataDir)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected a nonzero exit with no -project, got success: %s", out)
	}
	if !strings.Contains(string(out), "-project is required") {
		t.Errorf("output = %q, want it to name the missing -project flag", out)
	}
}

// TestIntegrationCheckProjectRejectsTraversalProjectID is the same
// traversal guard convention -ticket/-prior-run already get in
// runMainWithReady, applied to -project: it becomes part of the durable
// record's own filename in saveProjectCheckRecord.
func TestIntegrationCheckProjectRejectsTraversalProjectID(t *testing.T) {
	t.Parallel()
	specPath, _, _, _ := fixtureCheckProjectArtifacts(t)
	dataDir := t.TempDir()
	cmd := factorydCommand(t, "check-project", "-project", "../evil", "-spec", specPath, "-data-dir", dataDir)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected a nonzero exit for a traversal -project, got success: %s", out)
	}
	if !strings.Contains(string(out), "-project must be a single path component") {
		t.Errorf("output = %q, want it to name the rejected -project value", out)
	}
	if entries, err := os.ReadDir(dataDir); err == nil && len(entries) != 0 {
		t.Errorf("data dir = %v, want nothing created outside it for a rejected -project", entries)
	}
}

// TestIntegrationInitScaffoldsPassingBootstrap proves `factoryd init`'s
// scaffold actually satisfies the structural checks it's meant to pre-seed:
// spec/contract.md and ARCHITECTURE.md pass their checks as scaffolded
// (structure only, no "frozen" concept), and spec/spec.md needs only its
// STATUS line flipped from DRAFT to FROZEN, not any other edit, to pass
// too -- proven by running the real `factoryd check-project` command
// against the scaffold's own output, not by re-implementing the checks
// here.
func TestIntegrationInitScaffoldsPassingBootstrap(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	cmd := factorydCommand(t, "init", "-skip-doctor", "-project", "calculator-live-test", "-root", root)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("factoryd init failed: %v: %s", err, out)
	}

	specPath := filepath.Join(root, "spec", "spec.md")
	contractPath := filepath.Join(root, "spec", "contract.md")
	architecturePath := filepath.Join(root, "ARCHITECTURE.md")
	for _, path := range []string{specPath, contractPath, architecturePath} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("stat %s: %v", path, err)
		}
	}

	spec, err := os.ReadFile(specPath)
	if err != nil {
		t.Fatalf("read scaffolded spec: %v", err)
	}
	frozen := strings.Replace(string(spec), "STATUS: DRAFT -- pending human review", "STATUS: FROZEN -- reviewed", 1)
	if frozen == string(spec) {
		t.Fatalf("scaffolded spec did not contain the expected DRAFT status line: %q", spec)
	}
	if err := os.WriteFile(specPath, []byte(frozen), 0o644); err != nil {
		t.Fatalf("freeze scaffolded spec: %v", err)
	}

	dataDir := t.TempDir()
	checkCmd := factorydCommand(t, "check-project",
		"-project", "calculator-live-test",
		"-spec", specPath,
		"-contract", contractPath,
		"-architecture", architecturePath,
		"-data-dir", dataDir,
	)
	checkOut, checkErr := checkCmd.CombinedOutput()
	if checkErr != nil {
		t.Fatalf("factoryd check-project against the scaffold (frozen) failed: %v: %s", checkErr, checkOut)
	}
}

// TestIntegrationOnboardDetectsRealVerifyCommandFromExistingRepo is the
// regression test for a fable adoption review's recommendation:
// `factoryd onboard`, unlike `factoryd init`, points at an existing repo
// that already has real code in it, and must pre-fill ARCHITECTURE.md's
// own Verification section with that
// repo's real command -- proven here against real build tooling on disk,
// not synthetic, and against the real subprocess (not detectVerifyCommand
// called directly), so a regression in flag wiring would fail this test
// too. The real Makefile lives at root, -workspace's own parent, matching
// -workspace's real role as an empty placeholder subdirectory (see
// onboardMain's own doc comment); a decoy go.mod inside -workspace itself
// -- which would win as "go test ./..." if detection ever again read the
// wrong directory -- proves detection reads from root, not -workspace
// (Codex review of PR #91: an earlier version of this test put its only
// build file inside -workspace, which happened to still pass only
// because detectVerifyCommand had the exact bug this decoy now catches).
func TestIntegrationOnboardDetectsRealVerifyCommandFromExistingRepo(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	if err := os.MkdirAll(workspace, 0o750); err != nil {
		t.Fatalf("mkdir workspace: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "Makefile"), []byte("verify:\n\tgo test ./...\n"), 0o644); err != nil {
		t.Fatalf("write Makefile: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "go.mod"), []byte("module example.com/decoy-should-not-be-read\n\ngo 1.22\n"), 0o644); err != nil {
		t.Fatalf("write decoy go.mod: %v", err)
	}

	cmd := factorydCommand(t, "onboard", "-skip-doctor", "-project", "existing-go-repo", "-workspace", workspace)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("factoryd onboard failed: %v: %s", err, out)
	}
	if !strings.Contains(string(out), `"make verify"`) {
		t.Errorf("output = %q, want it to report the detected verify command from root's own Makefile, not the decoy go.mod inside -workspace", out)
	}

	architecture, err := os.ReadFile(filepath.Join(root, "ARCHITECTURE.md"))
	if err != nil {
		t.Fatalf("read scaffolded ARCHITECTURE.md: %v", err)
	}
	if !strings.Contains(string(architecture), "`make verify`") {
		t.Errorf("ARCHITECTURE.md = %q, want the detected verify command pre-filled into its Verification section", architecture)
	}

	// spec.md/contract.md stay exactly as generic as `factoryd init`'s own
	// -- onboard does not attempt to infer real product intent from source
	// code, matching init's own documented judgment call.
	specPath := filepath.Join(root, "spec", "spec.md")
	if _, err := os.Stat(specPath); err != nil {
		t.Errorf("stat scaffolded spec.md: %v", err)
	}
}

// TestIntegrationOnboardCreatesMissingWorkspacePlaceholder is the
// regression test for the friction found onboarding a brand-new repo
// (2026-09-14): -workspace used to hard-fail with a bare "no such file or
// directory" unless the operator had already `mkdir`'d the empty
// placeholder by hand. onboard now creates it itself, and keeps it out of
// `git status` via the repo's own untracked info/exclude rather than a
// tracked .gitignore edit.
func TestIntegrationOnboardCreatesMissingWorkspacePlaceholder(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if out, err := exec.Command("git", "-C", root, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	workspace := filepath.Join(root, "workspace")
	if _, err := os.Stat(workspace); err == nil {
		t.Fatal("workspace already exists before onboard runs -- test setup is wrong")
	}

	cmd := factorydCommand(t, "onboard", "-skip-doctor", "-project", "brand-new-repo", "-workspace", workspace)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("factoryd onboard failed: %v: %s", err, out)
	}
	if !strings.Contains(string(out), "Created empty placeholder directory") {
		t.Errorf("output = %q, want it to report creating the missing -workspace placeholder", out)
	}
	if info, err := os.Stat(workspace); err != nil || !info.IsDir() {
		t.Fatalf("workspace %q was not created as a directory: %v", workspace, err)
	}
	if entries, err := os.ReadDir(workspace); err != nil || len(entries) != 0 {
		t.Errorf("workspace = %v entries, want it created empty", entries)
	}
	if err := exec.Command("git", "-C", root, "check-ignore", "-q", "workspace/probe").Run(); err != nil {
		t.Error("workspace/ was not added to the repo's own git exclude")
	}
	if out := gitStatusPorcelain(t, root); strings.Contains(out, "workspace") {
		t.Errorf("git status = %q, want the auto-created workspace/ to not show as untracked", out)
	}
	if _, err := os.Stat(filepath.Join(root, ".gitignore")); err == nil {
		t.Error("onboard must not write a tracked .gitignore for the workspace placeholder")
	}
	if _, err := os.Stat(filepath.Join(root, "spec", "spec.md")); err != nil {
		t.Errorf("stat scaffolded spec.md: %v", err)
	}
}

// TestIntegrationOnboardRollsBackCreatedWorkspaceOnScaffoldRefusal is the
// regression test for Codex's "delay creating the placeholder until
// validation passes" finding (this PR, 2026-09-14): a failed scaffold
// used to leave the freshly created -workspace placeholder and its
// info/exclude mutation behind despite onboarding returning an error,
// contradicting writeScaffoldFiles' own all-or-nothing refusal. onboard
// must now roll both back. A pre-existing ARCHITECTURE.md alone no longer
// triggers a refusal (onboard now scaffolds only whichever of the three
// docs are missing -- see TestIntegrationOnboardScaffoldsOnlyMissingArtifacts),
// so this uses a pre-existing .factory.yml with -write-factory-yml
// instead, which still keeps its own hard "refuses to overwrite" contract.
func TestIntegrationOnboardRollsBackCreatedWorkspaceOnScaffoldRefusal(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if out, err := exec.Command("git", "-C", root, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	if err := os.WriteFile(filepath.Join(root, ".factory.yml"), []byte("verify_command: \"echo hi\"\n"), 0o644); err != nil {
		t.Fatalf("seed existing .factory.yml: %v", err)
	}
	workspace := filepath.Join(root, "workspace")

	cmd := factorydCommand(t, "onboard", "-skip-doctor", "-project", "existing-go-repo", "-workspace", workspace, "-write-factory-yml")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected a nonzero exit when .factory.yml already exists, got success: %s", out)
	}
	if _, err := os.Stat(workspace); err == nil {
		t.Errorf("workspace %q was left behind after a refused scaffold, want it rolled back", workspace)
	} else if !os.IsNotExist(err) {
		t.Fatalf("stat workspace after refusal: %v", err)
	}
	if err := exec.Command("git", "-C", root, "check-ignore", "-q", "workspace/probe").Run(); err == nil {
		t.Error("workspace/ was left in the repo's own git exclude after a refused scaffold")
	}
}

// TestIntegrationOnboardRequiresRepositoryParentToExist is the regression
// test for Codex's "require the repository parent before creating the
// placeholder" finding (this PR, 2026-09-14): os.MkdirAll used to create a
// misspelled/missing repository path too, so a typo'd -workspace silently
// scaffolded into a brand-new empty directory tree and could report
// success while leaving the real, intended repository untouched.
func TestIntegrationOnboardRequiresRepositoryParentToExist(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	nonexistentRepo := filepath.Join(root, "my-ap") // typo for "my-app"
	workspace := filepath.Join(nonexistentRepo, "workspace")

	cmd := factorydCommand(t, "onboard", "-skip-doctor", "-project", "typo-repo", "-workspace", workspace)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected a nonzero exit when -workspace's parent does not exist, got success: %s", out)
	}
	if !strings.Contains(string(out), "does not exist") {
		t.Errorf("output = %q, want it to name the missing repository parent", out)
	}
	if _, err := os.Stat(nonexistentRepo); err == nil {
		t.Errorf("%q was silently created, want onboard to require it to already exist", nonexistentRepo)
	}
}

func gitStatusPorcelain(t *testing.T, dir string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", dir, "status", "--porcelain").CombinedOutput()
	if err != nil {
		t.Fatalf("git status: %v: %s", err, out)
	}
	return string(out)
}

// TestIntegrationOnboardScaffoldsOnlyMissingArtifacts is a regression
// test: onboard used to refuse to scaffold anything at all if even one
// of its three
// target artifacts (spec/spec.md, spec/contract.md, ARCHITECTURE.md)
// already existed -- exactly the shape of a typical real repo, which
// already has a hand-written ARCHITECTURE.md but no spec/ yet. onboard now
// scaffolds only whichever of the three are actually missing, and leaves
// any that already exist byte-identical -- not even re-timestamped.
func TestIntegrationOnboardScaffoldsOnlyMissingArtifacts(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	if err := os.MkdirAll(workspace, 0o750); err != nil {
		t.Fatalf("mkdir workspace: %v", err)
	}
	architecturePath := filepath.Join(root, "ARCHITECTURE.md")
	architectureContent := []byte("# Real, already-written docs\n")
	if err := os.WriteFile(architecturePath, architectureContent, 0o644); err != nil {
		t.Fatalf("seed existing ARCHITECTURE.md: %v", err)
	}
	origInfo, err := os.Stat(architecturePath)
	if err != nil {
		t.Fatalf("stat seeded ARCHITECTURE.md: %v", err)
	}

	cmd := factorydCommand(t, "onboard", "-skip-doctor", "-project", "existing-go-repo", "-workspace", workspace)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("factoryd onboard failed: %v: %s", err, out)
	}
	if strings.Contains(string(out), "refusing to overwrite") {
		t.Errorf("output = %q, want no refusal when only ARCHITECTURE.md pre-exists", out)
	}

	for _, p := range []string{
		filepath.Join(root, "spec", "spec.md"),
		filepath.Join(root, "spec", "contract.md"),
	} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("stat %s: %v, want it scaffolded since it was missing", p, err)
		}
	}

	b, err := os.ReadFile(architecturePath)
	if err != nil {
		t.Fatalf("read ARCHITECTURE.md: %v", err)
	}
	if string(b) != string(architectureContent) {
		t.Errorf("ARCHITECTURE.md content changed: got %q, want unchanged %q", b, architectureContent)
	}
	newInfo, err := os.Stat(architecturePath)
	if err != nil {
		t.Fatalf("stat ARCHITECTURE.md after onboard: %v", err)
	}
	if !newInfo.ModTime().Equal(origInfo.ModTime()) {
		t.Errorf("ARCHITECTURE.md was touched: mtime changed from %v to %v, want untouched", origInfo.ModTime(), newInfo.ModTime())
	}
}

// TestIntegrationIntakeDraftsTicketsDespiteExpectedCheckpointRefusal is the
// regression test for the 2026-09-03 Opus factory-pipeline review's item
// C5: factoryd's entry precondition was "a ticket file already exists on
// disk," with no automated path to producing one. Proves `factoryd
// intake`'s own designed success path: goal_pilot.py's real
// --non-interactive mode always refuses the spec-freeze checkpoint (exits
// non-zero) after step 2 already drafted spec.md/tickets/ARCHITECTURE.md
// to disk -- intakeMain must treat that refusal as this command's own
// success, judged by the ticket files actually on disk, not by the
// wrapped process's exit code.
func TestIntegrationIntakeDraftsTicketsDespiteExpectedCheckpointRefusal(t *testing.T) {
	t.Parallel()
	pilotDir := filepath.Join(t.TempDir(), "pilot")
	script, err := filepath.Abs("testdata/fake_goal_pilot.sh")
	if err != nil {
		t.Fatalf("resolve fake goal_pilot script: %v", err)
	}
	cmd := factorydCommand(t, "intake",
		"-spec-input", "a calculator app",
		"-pilot-dir", pilotDir,
		"-goal-pilot-interpreter", "/bin/sh",
		"-goal-pilot-script", script,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("factoryd intake failed: %v: %s", err, out)
	}
	if !strings.Contains(string(out), "001-fixture-ticket.md") {
		t.Errorf("intake output = %q, want it to list the drafted ticket", out)
	}
	if !strings.Contains(string(out), "spec-freeze checkpoint is never auto-approved") {
		t.Errorf("intake output = %q, want it to explain the deliberate human checkpoint", out)
	}
	// Regression coverage for a real Codex review finding on this same PR:
	// intake's own advertised -workspace must actually satisfy
	// projectBootstrapArtifactPaths/resolvePiTicketPath, which both search
	// under filepath.Dir(-workspace)/spec -- not pilotDir itself, which
	// intake used to (wrongly) tell the caller to use directly.
	recommendedWorkspace := filepath.Join(pilotDir, "workspace")
	if !strings.Contains(string(out), recommendedWorkspace) {
		t.Errorf("intake output = %q, want it to recommend -workspace %s", out, recommendedWorkspace)
	}
	specPath, _, architecturePath := projectBootstrapArtifactPaths(recommendedWorkspace)
	if specPath != filepath.Join(pilotDir, "spec", "spec.md") {
		t.Errorf("projectBootstrapArtifactPaths(%s) spec path = %s, want it to resolve to where intake actually wrote spec.md", recommendedWorkspace, specPath)
	}
	if architecturePath != filepath.Join(pilotDir, "ARCHITECTURE.md") {
		t.Errorf("projectBootstrapArtifactPaths(%s) architecture path = %s, want it to resolve to where intake actually wrote ARCHITECTURE.md", recommendedWorkspace, architecturePath)
	}
	for _, path := range []string{
		filepath.Join(pilotDir, "spec", "spec.md"),
		filepath.Join(pilotDir, "spec", "tickets", "001-fixture-ticket.md"),
		filepath.Join(pilotDir, "ARCHITECTURE.md"),
		recommendedWorkspace,
	} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("stat %s: %v", path, err)
		}
	}
}

// TestIntegrationIntakeLockBesideRelativeDotPilotDir guards a real Codex
// review finding on PR #56: withIntakeLock used to derive its lock path from
// -pilot-dir's own filepath.Dir/Base without canonicalizing first, so
// -pilot-dir "." (both Dir and Base of which are also ".") placed the lock
// file back inside the pilot dir itself -- recreating the exact
// non-empty-pilot-dir scaffold failure the sibling-lock-file fix exists to
// close. Runs the real binary with its working directory set to the pilot
// dir and "." as -pilot-dir, then confirms the lock file landed in the
// parent, not inside the (still-empty, from goal_pilot.py's perspective)
// pilot dir.
// TestIntegrationIntakeFallsBackToPilotDirsOwnFactoryYML guards a real
// adversarial-review finding (2026-09-11): -verify-command must be
// supplied by hand every time, unlike run_ticket.go/submit.go which fall
// back to -workspace's own already-recorded .factory.yml verify_command
// via applyProjectConfigDefaults -- an operator who already ran `factoryd
// onboard`/`init -write-factory-yml` against -pilot-dir (the documented
// common case: -pilot-dir and a later -workspace name the same directory)
// but forgot the separate -verify-command flag on `intake` would
// otherwise still fall through to goal_pilot.py's unsatisfiable hardcoded
// default.
func TestIntegrationIntakeFallsBackToPilotDirsOwnFactoryYML(t *testing.T) {
	t.Parallel()
	pilotDir := filepath.Join(t.TempDir(), "pilot")
	if err := os.MkdirAll(pilotDir, 0o750); err != nil {
		t.Fatalf("mkdir pilot dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(pilotDir, ".factory.yml"), []byte("verify_command: \"cd backend && go test ./...\"\n"), 0o644); err != nil {
		t.Fatalf("write .factory.yml: %v", err)
	}
	script, err := filepath.Abs("testdata/fake_goal_pilot.sh")
	if err != nil {
		t.Fatalf("resolve fake goal_pilot script: %v", err)
	}
	cmd := factorydCommand(t, "intake",
		"-spec-input", "a calculator app",
		"-pilot-dir", pilotDir,
		"-goal-pilot-interpreter", "/bin/sh",
		"-goal-pilot-script", script,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("factoryd intake failed: %v: %s", err, out)
	}
	if !strings.Contains(string(out), `using "cd backend && go test ./..." from `+pilotDir+"/.factory.yml") {
		t.Errorf("intake output = %q, want it to report falling back to .factory.yml's own verify_command", out)
	}
	if !strings.Contains(string(out), "verify_command=cd backend && go test ./...") {
		t.Errorf("intake output = %q, want the fixture to have actually received --verify-command", out)
	}
}

// TestIntegrationIntakeExplicitVerifyCommandWinsOverFactoryYML confirms an
// explicit -verify-command is never silently overridden by -pilot-dir's
// own .factory.yml -- the fallback above must only apply when the flag was
// left at its default.
func TestIntegrationIntakeExplicitVerifyCommandWinsOverFactoryYML(t *testing.T) {
	t.Parallel()
	pilotDir := filepath.Join(t.TempDir(), "pilot")
	if err := os.MkdirAll(pilotDir, 0o750); err != nil {
		t.Fatalf("mkdir pilot dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(pilotDir, ".factory.yml"), []byte("verify_command: \"from .factory.yml, should be ignored\"\n"), 0o644); err != nil {
		t.Fatalf("write .factory.yml: %v", err)
	}
	script, err := filepath.Abs("testdata/fake_goal_pilot.sh")
	if err != nil {
		t.Fatalf("resolve fake goal_pilot script: %v", err)
	}
	cmd := factorydCommand(t, "intake",
		"-spec-input", "a calculator app",
		"-pilot-dir", pilotDir,
		"-goal-pilot-interpreter", "/bin/sh",
		"-goal-pilot-script", script,
		"-verify-command", "explicit wins",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("factoryd intake failed: %v: %s", err, out)
	}
	if strings.Contains(string(out), ".factory.yml") {
		t.Errorf("intake output = %q, want no mention of falling back to .factory.yml when -verify-command was given explicitly", out)
	}
	if !strings.Contains(string(out), "verify_command=explicit wins") {
		t.Errorf("intake output = %q, want the fixture to have received the explicit -verify-command, not .factory.yml's", out)
	}
}

func TestIntegrationIntakeLockBesideRelativeDotPilotDir(t *testing.T) {
	t.Parallel()
	parent := t.TempDir()
	pilotDir := filepath.Join(parent, "pilot")
	if err := os.MkdirAll(pilotDir, 0o750); err != nil {
		t.Fatalf("mkdir pilot dir: %v", err)
	}
	script, err := filepath.Abs("testdata/fake_goal_pilot.sh")
	if err != nil {
		t.Fatalf("resolve fake goal_pilot script: %v", err)
	}
	cmd := factorydCommand(t, "intake",
		"-spec-input", "a calculator app",
		"-pilot-dir", ".",
		"-goal-pilot-interpreter", "/bin/sh",
		"-goal-pilot-script", script,
	)
	cmd.Dir = pilotDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("factoryd intake failed: %v: %s", err, out)
	}
	wantLock := filepath.Join(parent, ".pilot.intake.lock")
	if _, statErr := os.Stat(wantLock); statErr != nil {
		t.Errorf("stat %s: %v (lock file was not placed beside a relative \".\" pilot dir)", wantLock, statErr)
	}
	if _, statErr := os.Stat(filepath.Join(pilotDir, "..intake.lock")); statErr == nil {
		t.Errorf("lock file landed inside the pilot dir itself (..intake.lock) -- reproduces the scaffold-check regression this test guards")
	}
}

// TestIntegrationIntakeFailsOnNoTicketsDrafted proves intakeMain reports a
// real error (not a silent success) when goal_pilot.py's own drafting
// step produced no ticket files -- the one thing intakeMain's success
// actually depends on, independent of the wrapped process's exit code.
func TestIntegrationIntakeFailsOnNoTicketsDrafted(t *testing.T) {
	t.Parallel()
	pilotDir := filepath.Join(t.TempDir(), "pilot")
	script, err := filepath.Abs("testdata/fake_goal_pilot.sh")
	if err != nil {
		t.Fatalf("resolve fake goal_pilot script: %v", err)
	}
	cmd := factorydCommand(t, "intake",
		"-spec-input", "a calculator app",
		"-pilot-dir", pilotDir,
		"-goal-pilot-interpreter", "/bin/sh",
		"-goal-pilot-script", script,
	)
	cmd.Env = append(os.Environ(), "FAKE_GOAL_PILOT_MODE=no_tickets")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("factoryd intake unexpectedly succeeded with no tickets drafted: %s", out)
	}
	if !strings.Contains(string(out), "drafted no new ticket files") {
		t.Errorf("intake output = %q, want it to explain no tickets were drafted", out)
	}
}

// TestIntegrationIntakeDoesNotReportStaleTicketsWhenOnlySpecIsRewritten is
// the regression test for a real Codex review finding on this same fix's
// own first attempt (PR #45): goal_pilot.py's step 2 always rewrites
// spec.md on success, but a *partial* failure can rewrite spec.md while
// drafting no new tickets at all (reproduced here by
// FAKE_GOAL_PILOT_MODE=no_tickets) -- checking spec.md's own freshness
// alone was not enough to vouch for the ticket list: with only that
// check, a second invocation's fresh spec.md would make intake report the
// *first* invocation's now-stale tickets as if this one had drafted them,
// for whatever the second invocation's own (possibly different)
// -spec-input was. Unlike
// TestIntegrationIntakeDoesNotReportStaleTicketsAsFreshlyDrafted (whose
// second invocation writes nothing at all), this specifically covers the
// case where spec.md itself does advance.
func TestIntegrationIntakeDoesNotReportStaleTicketsWhenOnlySpecIsRewritten(t *testing.T) {
	t.Parallel()
	pilotDir := filepath.Join(t.TempDir(), "pilot")
	script, err := filepath.Abs("testdata/fake_goal_pilot.sh")
	if err != nil {
		t.Fatalf("resolve fake goal_pilot script: %v", err)
	}
	args := []string{"intake",
		"-spec-input", "a calculator app",
		"-pilot-dir", pilotDir,
		"-goal-pilot-interpreter", "/bin/sh",
		"-goal-pilot-script", script,
	}
	if out, err := factorydCommand(t, args...).CombinedOutput(); err != nil {
		t.Fatalf("first (successful) factoryd intake failed: %v: %s", err, out)
	}
	firstTicket, err := os.ReadFile(filepath.Join(pilotDir, "spec", "tickets", "001-fixture-ticket.md"))
	if err != nil {
		t.Fatalf("read ticket drafted by first intake: %v", err)
	}

	cmd := factorydCommand(t, args...)
	cmd.Env = append(os.Environ(), "FAKE_GOAL_PILOT_MODE=no_tickets")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("second factoryd intake unexpectedly succeeded despite drafting no new tickets: %s", out)
	}
	if !strings.Contains(string(out), "drafted no new ticket files") {
		t.Errorf("intake output = %q, want it to explain no new tickets were drafted (not silently report the first run's stale one)", out)
	}
	secondTicket, err := os.ReadFile(filepath.Join(pilotDir, "spec", "tickets", "001-fixture-ticket.md"))
	if err != nil {
		t.Fatalf("read ticket after second intake: %v", err)
	}
	if !bytes.Equal(firstTicket, secondTicket) {
		t.Fatalf("ticket file changed despite FAKE_GOAL_PILOT_MODE=no_tickets, want the fixture to have left it untouched (test setup assumption violated)")
	}
}

// TestIntegrationIntakeFailsOnDraftingFailure is
// TestIntegrationIntakeFailsOnNoTicketsDrafted's counterpart for a harder
// failure: goal_pilot.py's own step 2 never even ran successfully, so the
// pilot dir has no spec.md at all. Caught by intakeMain's checkpoint-signal
// check (the phrase goal_pilot.py's own step3_freeze_checkpoint prints
// never appears at all here), not its later file-freshness checks.
func TestIntegrationIntakeFailsOnDraftingFailure(t *testing.T) {
	t.Parallel()
	pilotDir := filepath.Join(t.TempDir(), "pilot")
	script, err := filepath.Abs("testdata/fake_goal_pilot.sh")
	if err != nil {
		t.Fatalf("resolve fake goal_pilot script: %v", err)
	}
	cmd := factorydCommand(t, "intake",
		"-spec-input", "a calculator app",
		"-pilot-dir", pilotDir,
		"-goal-pilot-interpreter", "/bin/sh",
		"-goal-pilot-script", script,
	)
	cmd.Env = append(os.Environ(), "FAKE_GOAL_PILOT_MODE=fail")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("factoryd intake unexpectedly succeeded despite goal_pilot.py's own drafting failure: %s", out)
	}
	if !strings.Contains(string(out), "did not reach either the spec-freeze checkpoint") {
		t.Errorf("intake output = %q, want it to explain the missing checkpoint signal", out)
	}
}

// TestIntegrationIntakeFailsOnPartialCrashDespiteFreshFiles is the
// regression test for a real finding from a local codex review pass on
// this same PR: the file-freshness checks alone prove content changed,
// not that goal_pilot.py's own step 2 (drafting) actually ran to
// completion -- its step 3 (the spec-freeze checkpoint) only ever runs
// *after* step 2 finishes in full. A crash partway through step 2 (after
// spec.md and at least one ticket, but before, say,
// write_architecture_stub) would leave freshly changed files on disk
// without ever reaching that checkpoint -- FAKE_GOAL_PILOT_MODE=
// partial_crash reproduces exactly that shape (fresh spec.md, fresh
// ticket, but no checkpoint-refusal phrase in the output). intakeMain
// must reject this as a real failure, not present the partial output as
// a completed draft.
func TestIntegrationIntakeFailsOnPartialCrashDespiteFreshFiles(t *testing.T) {
	t.Parallel()
	pilotDir := filepath.Join(t.TempDir(), "pilot")
	script, err := filepath.Abs("testdata/fake_goal_pilot.sh")
	if err != nil {
		t.Fatalf("resolve fake goal_pilot script: %v", err)
	}
	cmd := factorydCommand(t, "intake",
		"-spec-input", "a calculator app",
		"-pilot-dir", pilotDir,
		"-goal-pilot-interpreter", "/bin/sh",
		"-goal-pilot-script", script,
	)
	cmd.Env = append(os.Environ(), "FAKE_GOAL_PILOT_MODE=partial_crash")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("factoryd intake unexpectedly succeeded despite a partial crash before the spec-freeze checkpoint: %s", out)
	}
	if !strings.Contains(string(out), "did not reach either the spec-freeze checkpoint") {
		t.Errorf("intake output = %q, want it to explain the missing checkpoint signal despite fresh files on disk", out)
	}
	// The fixture really did write fresh files -- confirming this test
	// actually exercises the checkpoint-signal check, not a check that
	// would have failed anyway for a more mundane reason (e.g. no files
	// at all, which TestIntegrationIntakeFailsOnDraftingFailure already
	// covers).
	if _, statErr := os.Stat(filepath.Join(pilotDir, "spec", "spec.md")); statErr != nil {
		t.Errorf("stat spec.md: %v -- want the fixture to have written it before crashing (test setup assumption violated)", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(pilotDir, "spec", "tickets", "001-fixture-ticket.md")); statErr != nil {
		t.Errorf("stat ticket: %v -- want the fixture to have written it before crashing (test setup assumption violated)", statErr)
	}
}

// TestIntegrationIntakeDoesNotReportStaleTicketsAsFreshlyDrafted is the
// regression test for a real Codex review finding on PR #45: a
// before/after ticket-directory existence diff would find zero "new"
// files on a failed re-run against a -pilot-dir a prior, successful
// intake already populated -- silently handing off stale tickets, drafted
// for a completely different -spec-input, as if this invocation had just
// drafted them. Runs intake twice against the same -pilot-dir: once
// successfully (leaving real spec.md/tickets on disk), then again in
// FAKE_GOAL_PILOT_MODE=fail (which touches nothing) -- the second
// invocation must fail, not silently report the first run's now-stale
// output as its own. (The checkpoint-signal check added later catches
// this same case too -- the fixture never reaches it in "fail" mode
// either -- but this test's own value is proving that holds even with
// real, stale files already sitting in -pilot-dir from a genuine prior
// success, not just an empty directory.)
func TestIntegrationIntakeDoesNotReportStaleTicketsAsFreshlyDrafted(t *testing.T) {
	t.Parallel()
	pilotDir := filepath.Join(t.TempDir(), "pilot")
	script, err := filepath.Abs("testdata/fake_goal_pilot.sh")
	if err != nil {
		t.Fatalf("resolve fake goal_pilot script: %v", err)
	}
	firstArgs := []string{"intake",
		"-spec-input", "a calculator app",
		"-pilot-dir", pilotDir,
		"-goal-pilot-interpreter", "/bin/sh",
		"-goal-pilot-script", script,
	}
	if out, err := factorydCommand(t, firstArgs...).CombinedOutput(); err != nil {
		t.Fatalf("first (successful) factoryd intake failed: %v: %s", err, out)
	}

	cmd := factorydCommand(t, firstArgs...)
	cmd.Env = append(os.Environ(), "FAKE_GOAL_PILOT_MODE=fail")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("second factoryd intake unexpectedly succeeded despite goal_pilot.py's own drafting failure: %s", out)
	}
	if !strings.Contains(string(out), "did not reach either the spec-freeze checkpoint") {
		t.Errorf("intake output = %q, want it to refuse the first run's now-stale spec.md/tickets rather than report them as freshly drafted", out)
	}
}

// TestIntegrationIntakeResumesPastFreezeToDraftContract is the regression
// test for the gap a user hit live, 2026-09-03: after intake's first
// invocation halted at the spec-freeze checkpoint and a human froze
// spec.md by hand (flipped its STATUS line to FROZEN, exactly what real
// review does), a second `factoryd intake` invocation against the same
// -pilot-dir used to be reported as a failure -- goal_pilot.py itself
// happily skips straight to step 4 and drafts spec/contract.md, halting
// at the *next* checkpoint instead, but intakeMain only ever recognized
// the first checkpoint's refusal phrase as success. Proves the fix: a
// second invocation after a manual freeze is now itself a success,
// spec/contract.md exists and is freshly drafted, and -checkpoint=review
// (not goal_pilot.py's own default of "skip", which would not have halted
// non-interactively at all) was actually passed through.
func TestIntegrationIntakeResumesPastFreezeToDraftContract(t *testing.T) {
	t.Parallel()
	pilotDir := filepath.Join(t.TempDir(), "pilot")
	script, err := filepath.Abs("testdata/fake_goal_pilot.sh")
	if err != nil {
		t.Fatalf("resolve fake goal_pilot script: %v", err)
	}
	args := []string{"intake",
		"-spec-input", "a calculator app",
		"-pilot-dir", pilotDir,
		"-goal-pilot-interpreter", "/bin/sh",
		"-goal-pilot-script", script,
	}
	if out, err := factorydCommand(t, args...).CombinedOutput(); err != nil {
		t.Fatalf("first (successful) factoryd intake failed: %v: %s", err, out)
	}

	specPath := filepath.Join(pilotDir, "spec", "spec.md")
	spec, err := os.ReadFile(specPath)
	if err != nil {
		t.Fatalf("read drafted spec: %v", err)
	}
	frozen := strings.Replace(string(spec), "STATUS: DRAFT", "STATUS: FROZEN", 1)
	if frozen == string(spec) {
		t.Fatalf("drafted spec did not contain the expected DRAFT status line: %q", spec)
	}
	if err := os.WriteFile(specPath, []byte(frozen), 0o644); err != nil {
		t.Fatalf("freeze drafted spec: %v", err)
	}

	out, err := factorydCommand(t, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("second (resumed) factoryd intake failed: %v: %s", err, out)
	}
	if !strings.Contains(string(out), "checkpoint=review") {
		t.Errorf("intake output = %q, want it to have passed --checkpoint review through to goal_pilot.py rather than its own unsafe default", out)
	}
	contractPath := filepath.Join(pilotDir, "spec", "contract.md")
	if _, err := os.Stat(contractPath); err != nil {
		t.Errorf("stat %s: %v -- want the resumed invocation to have drafted it", contractPath, err)
	}
	if !strings.Contains(string(out), contractPath) {
		t.Errorf("intake output = %q, want it to name the drafted contract path", out)
	}
	if !strings.Contains(string(out), "acceptance-suite") {
		t.Errorf("intake output = %q, want it to explain the acceptance-suite checkpoint it halted at", out)
	}
}

// TestIntegrationIntakeResumeFailsWhenContractNotProduced is
// TestIntegrationIntakeResumesPastFreezeToDraftContract's counterpart for
// a resumed invocation that crashes before ever reaching the
// acceptance-suite checkpoint: intakeMain must reject this as a real
// failure (via its "did not reach either checkpoint" error), not treat
// the exit code or partial output as a completed contract draft.
func TestIntegrationIntakeResumeFailsWhenContractNotProduced(t *testing.T) {
	t.Parallel()
	pilotDir := filepath.Join(t.TempDir(), "pilot")
	script, err := filepath.Abs("testdata/fake_goal_pilot.sh")
	if err != nil {
		t.Fatalf("resolve fake goal_pilot script: %v", err)
	}
	args := []string{"intake",
		"-spec-input", "a calculator app",
		"-pilot-dir", pilotDir,
		"-goal-pilot-interpreter", "/bin/sh",
		"-goal-pilot-script", script,
	}
	if out, err := factorydCommand(t, args...).CombinedOutput(); err != nil {
		t.Fatalf("first (successful) factoryd intake failed: %v: %s", err, out)
	}
	specPath := filepath.Join(pilotDir, "spec", "spec.md")
	spec, err := os.ReadFile(specPath)
	if err != nil {
		t.Fatalf("read drafted spec: %v", err)
	}
	frozen := strings.Replace(string(spec), "STATUS: DRAFT", "STATUS: FROZEN", 1)
	if err := os.WriteFile(specPath, []byte(frozen), 0o644); err != nil {
		t.Fatalf("freeze drafted spec: %v", err)
	}

	cmd := factorydCommand(t, args...)
	cmd.Env = append(os.Environ(), "FAKE_GOAL_PILOT_MODE=no_contract")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("resumed factoryd intake unexpectedly succeeded despite goal_pilot.py never reaching the acceptance-suite checkpoint: %s", out)
	}
	if !strings.Contains(string(out), "did not reach either the spec-freeze checkpoint") {
		t.Errorf("intake output = %q, want it to explain the missing checkpoint signal", out)
	}
	contractPath := filepath.Join(pilotDir, "spec", "contract.md")
	if _, statErr := os.Stat(contractPath); statErr == nil {
		t.Errorf("stat %s unexpectedly succeeded -- want the fixture to have crashed before writing it (test setup assumption violated)", contractPath)
	}
}

// TestIntegrationIntakeWarnsRatherThanSilentlyRedraftingOnMisplacedFreeze is
// the regression test for a real Opus review finding, 2026-09-04 (A1):
// goal_pilot.py's own read_spec_status() parses ONLY line 1 of spec.md
// (`^STATUS:\s*(\S+)`, case-sensitive) -- a human who "freezes" the spec
// by writing STATUS: FROZEN anywhere else (e.g. after adding their own
// heading or review notes above it, a completely natural edit) gets no
// error: goal_pilot.py just falls into its own `if status != "FROZEN":`
// branch and silently re-drafts spec.md via /spec-plan, discarding
// whatever the human actually reviewed. A fixture that detected "frozen"
// via a bare substring grep couldn't reproduce this at all (that gap is
// what let the resume tests above pass despite this). Proves: given a
// spec frozen this exact wrong way, factoryd (a) still runs to completion
// (this is not a hard error -- a genuinely not-yet-frozen spec must still
// draft normally) but (b) prints an explicit, visible warning naming the
// line-1 requirement before it overwrites anything, and (c) does NOT
// treat this as the resume-to-contract success path.
func TestIntegrationIntakeWarnsRatherThanSilentlyRedraftingOnMisplacedFreeze(t *testing.T) {
	t.Parallel()
	pilotDir := filepath.Join(t.TempDir(), "pilot")
	script, err := filepath.Abs("testdata/fake_goal_pilot.sh")
	if err != nil {
		t.Fatalf("resolve fake goal_pilot script: %v", err)
	}
	args := []string{"intake",
		"-spec-input", "a calculator app",
		"-pilot-dir", pilotDir,
		"-goal-pilot-interpreter", "/bin/sh",
		"-goal-pilot-script", script,
	}
	if out, err := factorydCommand(t, args...).CombinedOutput(); err != nil {
		t.Fatalf("first (successful) factoryd intake failed: %v: %s", err, out)
	}

	specPath := filepath.Join(pilotDir, "spec", "spec.md")
	spec, err := os.ReadFile(specPath)
	if err != nil {
		t.Fatalf("read drafted spec: %v", err)
	}
	// The exact human mistake this guards against: STATUS: FROZEN is
	// genuinely present in the file, just not on line 1 -- e.g. a review
	// note added above it. A bare substring search would call this
	// frozen; goal_pilot.py's own line-1-anchored parser does not.
	misplaced := "Reviewed and approved, 2026-09-04.\n" + strings.Replace(string(spec), "STATUS: DRAFT", "STATUS: FROZEN", 1)
	if err := os.WriteFile(specPath, []byte(misplaced), 0o644); err != nil {
		t.Fatalf("write misplaced-freeze spec: %v", err)
	}

	out, err := factorydCommand(t, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("second factoryd intake failed: %v: %s", err, out)
	}
	if !strings.Contains(string(out), "does not read exactly \"STATUS: FROZEN\"") {
		t.Errorf("intake output = %q, want an explicit warning that line 1 isn't the frozen status line", out)
	}
	if strings.Contains(string(out), "acceptance-suite") {
		t.Errorf("intake output = %q, want the fresh-draft path (misplaced freeze reads as not-yet-frozen to goal_pilot.py itself), not the contract-resume path", out)
	}
	contractPath := filepath.Join(pilotDir, "spec", "contract.md")
	if _, statErr := os.Stat(contractPath); statErr == nil {
		t.Errorf("stat %s unexpectedly succeeded -- want no contract drafted, since goal_pilot.py itself never saw this spec as frozen", contractPath)
	}

	// The cheapest mitigation the same Opus review pass suggested as a
	// follow-up, 2026-09-04: back up spec.md before it can be overwritten,
	// regardless of whether this specific invocation turns out to be the
	// benign "still draft" case or the destructive "human edited this and
	// lost it" case -- no false positives, makes the bad case recoverable.
	backupPath := specPath + ".pre-intake"
	backup, err := os.ReadFile(backupPath)
	if err != nil {
		t.Fatalf("read backup %s: %v -- want the misplaced-freeze spec backed up before being overwritten", backupPath, err)
	}
	if string(backup) != misplaced {
		t.Errorf("backup content = %q, want the exact misplaced-freeze spec this invocation was about to overwrite", backup)
	}
	if !strings.Contains(string(out), backupPath) {
		t.Errorf("intake output = %q, want it to name the backup path", out)
	}
}

// TestIntegrationIntakeSerializesAcrossProcesses is the regression test
// for a real Opus review finding, 2026-09-04 (A2): intakeMain took no
// lock at all, so two concurrent `factoryd intake` invocations against
// the same -pilot-dir could interleave two goal_pilot.py subprocesses
// writing the same spec/ tree -- both take their content snapshots before
// either writes, so both would report success, each possibly attributing
// the other's output to itself. Same proof shape as
// TestIntegrationKillSwitchTransitionsSerializeAcrossProcesses (racing
// subprocesses directly would be probabilistic): this test process holds
// the pilot dir's own advisory lock directly, and a real `factoryd
// intake` subprocess must actually wait for it rather than proceed. Red
// before the fix, where nothing consults a lock file at all and the
// subprocess completes immediately.
