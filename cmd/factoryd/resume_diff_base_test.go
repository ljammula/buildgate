package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"buildgate/internal/requestdriver"
	"buildgate/internal/run"
	"buildgate/internal/workflow"
)

// A run that continues an attempt's branch (a corrective build, a retry on the
// attempt's commit, a PR-review round) starts from that attempt's commit and
// names the ticket's base as its diff base. Lost, it is not kept, and a resume
// of it is refused: no resumed run can today have such a run's base, the
// attempt's commit, as its own with the diff base dropped.
func TestALostRunThatContinuedABranchCannotBeResumed(t *testing.T) {
	repoDir := newFixtureRepo(t)
	dataDir := t.TempDir()
	testIsolationMarker(t, repoDir, dataDir, "req-1-001-corrective1", "temporal")
	seedOwnedRun(t, dataDir, "req-1-001-corrective1", run.StateSliceRunning, deadPID(t), "")
	r, _ := run.Load(dataDir, "req-1-001-corrective1")
	r.RequestID, r.OnBranch, r.DiffBaseSHA = "req-1", "factoryd/req-1-001", strings.Repeat("1", 40)
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	if halted, err := haltDeadOwnerRun(dataDir, "req-1-001-corrective1"); err != nil || !halted {
		t.Fatalf("haltDeadOwnerRun = %v, %v", halted, err)
	}
	if got, _ := run.Load(dataDir, "req-1-001-corrective1"); got.KeptForResume {
		t.Fatal("a lost run that continued a branch was kept for resume")
	}
	if _, _, err := requestdriver.ResolveResumeFrom(context.Background(), dataDir, noContainersDocker(t), "req-1-001-corrective1", "", 0); err == nil || !strings.Contains(err.Error(), "not kept for a resume") {
		t.Errorf("resume of a lost run that continued a branch: %v, want refused as not kept", err)
	}
}

// Should a run with a diff base ever be resumed, the resumed run's record and
// workflow input carry that diff base: the ticket's base is never replaced by
// the commit the lost run started from, so the resumed run's diff gates and
// every later round judge the ticket's whole change, never less.
func TestAResumedRunCarriesTheLostRunsDiffBase(t *testing.T) {
	repo, sha := instructionBaseRepo(t)
	dataDir := t.TempDir()
	// The lost run: started from an attempt's commit (sha[2]) with the
	// ticket's base (sha[0]) as its diff base.
	lost := &run.Run{ID: "lost", BaseSHA: sha[2], DiffBaseSHA: sha[0], InstructionBaseSHA: sha[0]}
	encoded, err := json.Marshal(workflow.NewResumeFrom(lost))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"diff_base_sha":"`+sha[0]+`"`) {
		t.Errorf("the resume input does not carry the lost run's diff base %s: %s", sha[0], encoded)
	}
	var resumeFrom workflow.ResumeFrom
	if err := json.Unmarshal(encoded, &resumeFrom); err != nil {
		t.Fatal(err)
	}
	workspace, none := repo, ""
	tr := &ticketRun{id: "resumed", workspace: &workspace, dataDir: &dataDir, diffBase: &none, instructionBase: &none, baseSHA: sha[2], resumeFrom: &resumeFrom, r: &run.Run{ID: "resumed"}}
	if err := tr.recordAncestorInputs(); err != nil {
		t.Fatal(err)
	}
	if tr.r.DiffBaseSHA != sha[0] {
		t.Errorf("the resumed run records diff base %q, want the lost run's %s: its diff gates and the next round's -diff-base would measure from the attempt's commit %s", tr.r.DiffBaseSHA, sha[0], sha[2])
	}
	// A resumed first build had no diff base and gets none.
	first := workflow.NewResumeFrom(&run.Run{ID: "lost-first", BaseSHA: sha[0]})
	tr = &ticketRun{id: "resumed-first", workspace: &workspace, dataDir: &dataDir, diffBase: &none, instructionBase: &none, baseSHA: sha[0], resumeFrom: first, r: &run.Run{ID: "resumed-first"}}
	if err := tr.recordAncestorInputs(); err != nil || tr.r.DiffBaseSHA != "" {
		t.Errorf("a resumed first build records diff base %q, %v; want none", tr.r.DiffBaseSHA, err)
	}
}
