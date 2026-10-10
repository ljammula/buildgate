package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"buildgate/internal/handoff"
	"buildgate/internal/run"
)

// gateBaseFixture is a fixture repository whose committed .factory.yml
// defines one repository gate, and the commit a run of it starts from.
func gateBaseFixture(t *testing.T, gateCommand string) (ws, base string) {
	t.Helper()
	ws = newFixtureRepo(t)
	commitFactoryYML(t, ws, "gates:\n  - id: house_rule\n    command: '"+gateCommand+"'\n")
	out, err := runGit(t, ws, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("git rev-parse: %v: %s", err, out)
	}
	return ws, strings.TrimSpace(out)
}

// recordedBaseCheck reads the gate's base_check object from the run record as
// the API serves it, nil when the gate has none.
func recordedBaseCheck(t *testing.T, dataDir, runID, check string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(run.Dir(dataDir, runID), "run.json"))
	if err != nil {
		t.Fatalf("read run.json: %v", err)
	}
	var record struct {
		GateResults []map[string]any `json:"gate_results"`
	}
	if err := json.Unmarshal(raw, &record); err != nil {
		t.Fatalf("decode run.json: %v", err)
	}
	for _, g := range record.GateResults {
		if g["check"] == check {
			baseCheck, _ := g["base_check"].(map[string]any)
			return baseCheck
		}
	}
	t.Fatalf("run.json has no %s gate result: %s", check, raw)
	return nil
}

// handoffCheck loads the run's handoff as a corrective round does and returns
// it with its entry for check.
func handoffCheck(t *testing.T, dataDir string, r *run.Run, check string) (handoff.Document, handoff.Check) {
	t.Helper()
	doc, err := handoff.Load(run.Dir(dataDir, r.ID), r.HandoffSHA256, r.State)
	if err != nil {
		t.Fatalf("load the handoff: %v", err)
	}
	for _, c := range doc.Checks {
		if c.Check == check {
			return doc, c
		}
	}
	t.Fatalf("the handoff has no %s check: %+v", check, doc.Checks)
	return doc, handoff.Check{}
}

// assertNoGateBaseWorktreeLeft: the scratch worktree of the rerun is gone
// from disk and from the repository's worktree list.
func assertNoGateBaseWorktreeLeft(t *testing.T, ws, dataDir, runID string) {
	t.Helper()
	if entries, err := os.ReadDir(filepath.Join(run.Dir(dataDir, runID), "gate-base")); err == nil && len(entries) > 0 {
		t.Errorf("the run directory still holds %d scratch worktree(s) of the base rerun", len(entries))
	}
	list, err := runGit(t, ws, "worktree", "list", "--porcelain")
	if err != nil {
		t.Fatalf("git worktree list: %v: %s", err, list)
	}
	if strings.Contains(list, "gate-base") {
		t.Errorf("the repository still lists a scratch worktree of the base rerun:\n%s", list)
	}
}

// A repository gate that fails on the build's result and on the commit the
// build started from cannot be fixed by a build: the gate result records it,
// the handoff sorts it as the operator's with a sentence that says so, and
// what the failures allow next is therefore not a corrective build.
func TestIntegrationGateThatAlsoFailsOnTheBaseCommitIsTheOperators(t *testing.T) {
	ws, base := gateBaseFixture(t, "false")
	dataDir := t.TempDir()
	r := runFactorydWithSpecFlagsAndDataDir(t, ws, "commit", "true", "# fixture spec\n", "60s", nil, nil, dataDir)

	if r.State != run.StateQuarantined {
		t.Fatalf("state = %q, want %q (gates: %+v)", r.State, run.StateQuarantined, r.GateResults)
	}
	if passed, found := gatePassed(r, "repo-house_rule"); !found || passed {
		t.Fatalf("repo-house_rule found=%v passed=%v, want a failed gate", found, passed)
	}
	if r.ResultSHA == "" || r.ResultSHA == base {
		t.Errorf("result_sha = %q, want the build's own commit, not the base %s", r.ResultSHA, base)
	}

	baseCheck := recordedBaseCheck(t, dataDir, r.ID, "repo-house_rule")
	if baseCheck["outcome"] != "fails" || baseCheck["base_sha"] != base {
		t.Errorf("base_check = %v, want outcome fails on base_sha %s", baseCheck, base)
	}

	doc, check := handoffCheck(t, dataDir, r, "repo-house_rule")
	if check.Bin != handoff.BinOperator {
		t.Errorf("repo-house_rule is sorted %q, want %q", check.Bin, handoff.BinOperator)
	}
	if doc.Next != handoff.BinOperator {
		t.Errorf("next = %q, want %q: a corrective build would be started for a gate no build can fix", doc.Next, handoff.BinOperator)
	}
	for _, want := range []string{"fails on the base commit " + base[:12], "fix the gate command or the repository", "No corrective build is started"} {
		if !strings.Contains(check.Finding, want) {
			t.Errorf("the finding lacks %q: %q", want, check.Finding)
		}
		if !strings.Contains(r.Triage, want) {
			t.Errorf("the run's triage sentence lacks %q: %q", want, r.Triage)
		}
	}
	assertNoGateBaseWorktreeLeft(t, ws, dataDir, r.ID)
}

// A gate that fails on the result and passes on the base commit was broken by
// the build: it stays a failure a corrective build is given, and the record
// says the base passes.
func TestIntegrationGateThatFailsOnlyOnTheResultStaysCorrective(t *testing.T) {
	// The fixture's build appends a line naming itself to content.txt.
	ws, base := gateBaseFixture(t, "! grep -q fake_build_app content.txt")
	dataDir := t.TempDir()
	r := runFactorydWithSpecFlagsAndDataDir(t, ws, "commit", "true", "# fixture spec\n", "60s", nil, nil, dataDir)

	if r.State != run.StateQuarantined {
		t.Fatalf("state = %q, want %q (gates: %+v)", r.State, run.StateQuarantined, r.GateResults)
	}
	baseCheck := recordedBaseCheck(t, dataDir, r.ID, "repo-house_rule")
	if baseCheck["outcome"] != "passes" || baseCheck["base_sha"] != base {
		t.Errorf("base_check = %v, want outcome passes on base_sha %s", baseCheck, base)
	}
	doc, check := handoffCheck(t, dataDir, r, "repo-house_rule")
	if check.Bin != handoff.BinCorrective || doc.Next != handoff.BinCorrective {
		t.Errorf("repo-house_rule is sorted %q, next %q; want both %q", check.Bin, doc.Next, handoff.BinCorrective)
	}
	if strings.Contains(check.Finding, "base commit") {
		t.Errorf("the finding speaks of the base commit for a gate that passes there: %q", check.Finding)
	}
	assertNoGateBaseWorktreeLeft(t, ws, dataDir, r.ID)
}

// A gate that passes is never rerun: the record carries no base check.
func TestIntegrationPassingGateIsNotRerunOnTheBaseCommit(t *testing.T) {
	ws, _ := gateBaseFixture(t, "true")
	dataDir := t.TempDir()
	r := runFactorydWithSpecFlagsAndDataDir(t, ws, "commit", "true", "# fixture spec\n", "60s", nil, nil, dataDir)

	if r.State != run.StateAccepted {
		t.Fatalf("state = %q, want %q (gates: %+v)", r.State, run.StateAccepted, r.GateResults)
	}
	if baseCheck := recordedBaseCheck(t, dataDir, r.ID, "repo-house_rule"); baseCheck != nil {
		t.Errorf("a passing gate recorded a base check: %v", baseCheck)
	}
	if _, err := os.Lstat(filepath.Join(run.Dir(dataDir, r.ID), "gate-base")); err == nil {
		t.Errorf("a passing gate left a gate-base directory in the run directory")
	}
}
