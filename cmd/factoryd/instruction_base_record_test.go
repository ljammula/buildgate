package main

import (
	"strings"
	"testing"

	"buildgate/internal/run"
	"buildgate/internal/workflow"
)

// instructionBaseRepo is a repository of three commits, oldest first.
func instructionBaseRepo(t *testing.T) (repo string, shas [3]string) {
	t.Helper()
	repo = commitFiles(t, map[string]string{"a.txt": "one\n"})
	shas[0] = gitHead(t, repo)
	for i := 1; i < 3; i++ {
		if out, err := runGit(t, repo, "-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "--allow-empty", "-m", "next"); err != nil {
			t.Fatalf("git commit: %v: %s", err, out)
		}
		shas[i] = gitHead(t, repo)
	}
	return repo, shas
}

func gitHead(t *testing.T, repo string) string {
	t.Helper()
	out, err := runGit(t, repo, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("rev-parse HEAD: %v: %s", err, out)
	}
	return strings.TrimSpace(out)
}

func recordedInstructionBase(t *testing.T, workspace, dataDir, base, diffBase, instructionBase string, resumeFrom *workflow.ResumeFrom) (string, error) {
	t.Helper()
	tr := &ticketRun{id: "run-1", workspace: &workspace, dataDir: &dataDir, diffBase: &diffBase, instructionBase: &instructionBase, baseSHA: base, resumeFrom: resumeFrom, r: &run.Run{ID: "run-1"}}
	err := tr.recordAncestorInputs()
	return tr.r.InstructionBaseSHA, err
}

// Every run records the instruction base its reviews read: the flag, else a
// resumed run's lost run's, else its effective diff base.
func TestRunRecordsItsResolvedInstructionBase(t *testing.T) {
	repo, sha := instructionBaseRepo(t)
	dataDir := t.TempDir()
	cases := []struct {
		name                       string
		base, diffBase, flag, want string
		resume                     *workflow.ResumeFrom
	}{
		{"a first build records its own base", sha[2], "", "", sha[2], nil},
		{"a run on a branch records its diff base", sha[2], sha[0], "", sha[0], nil},
		{"the flag outranks the diff base", sha[2], sha[1], sha[0], sha[0], nil},
		{"a resumed run inherits the lost run's base", sha[2], "", "", sha[0], &workflow.ResumeFrom{BaseSHA: sha[2], InstructionBaseSHA: sha[0]}},
		{"the flag outranks the lost run's base", sha[2], "", sha[1], sha[1], &workflow.ResumeFrom{BaseSHA: sha[2], InstructionBaseSHA: sha[0]}},
	}
	for _, c := range cases {
		got, err := recordedInstructionBase(t, repo, dataDir, c.base, c.diffBase, c.flag, c.resume)
		if err != nil || got != c.want {
			t.Errorf("%s: recorded %q, %v; want %q", c.name, got, err, c.want)
		}
	}
	// NewResumeFrom carries the lost run's resolved base, whichever field holds it.
	for lost, want := range map[*run.Run]string{
		{BaseSHA: "b", InstructionBaseSHA: "i", DiffBaseSHA: "d"}: "i",
		{BaseSHA: "b", DiffBaseSHA: "d"}:                          "d",
		{BaseSHA: "b"}:                                            "b",
	} {
		if got := workflow.NewResumeFrom(lost).InstructionBaseSHA; got != want {
			t.Errorf("NewResumeFrom(%+v).InstructionBaseSHA = %q, want %q", lost, got, want)
		}
	}
	// A base that is not an ancestor of the run's base halts the run.
	if _, err := recordedInstructionBase(t, repo, dataDir, sha[0], "", sha[2], nil); err == nil {
		t.Error("an -instruction-base that is not an ancestor of the base was accepted")
	}
}
