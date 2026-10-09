package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"buildgate/internal/forge"
	"buildgate/internal/memory"
	"buildgate/internal/release"
	"buildgate/internal/run"
)

// memRepo is a real git repository in a temp dir.
type memRepo struct {
	t   *testing.T
	dir string
}

func newMemRepo(t *testing.T) *memRepo {
	t.Helper()
	m := &memRepo{t: t, dir: t.TempDir()}
	m.git("init", "-q", "-b", "main")
	return m
}

func (m *memRepo) git(args ...string) string {
	m.t.Helper()
	cmd := exec.Command("git", append([]string{"-C", m.dir, "-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false", "-c", "core.hooksPath=/dev/null"}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	out, err := cmd.CombinedOutput()
	if err != nil {
		m.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// commit writes files (a nil value removes the file), commits everything and
// returns the new commit.
func (m *memRepo) commit(files map[string]*string) string {
	m.t.Helper()
	for name, body := range files {
		path := filepath.Join(m.dir, name)
		if body == nil {
			_ = os.Remove(path)
			continue
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			m.t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(*body), 0o644); err != nil {
			m.t.Fatal(err)
		}
	}
	m.git("add", "-A")
	m.git("commit", "-q", "--allow-empty", "-m", "c")
	return m.git("rev-parse", "HEAD")
}

func (m *memRepo) symlinkCommit(name, target string) string {
	m.t.Helper()
	_ = os.Remove(filepath.Join(m.dir, name))
	if err := os.Symlink(target, filepath.Join(m.dir, name)); err != nil {
		m.t.Fatal(err)
	}
	m.git("add", "-A")
	m.git("commit", "-q", "-m", "link")
	return m.git("rev-parse", "HEAD")
}

func (m *memRepo) changed(base, result string) []string {
	out := m.git("diff", "--name-only", base, result)
	if out == "" {
		return []string{}
	}
	return strings.Split(out, "\n")
}

func sp(s string) *string { return &s }

func fenced(prefix, suffix string, lines ...string) string {
	return string(memory.Section{Before: prefix, After: suffix}.Render(lines))
}

// memoryRun is an accepted run from base to result, its inventory taken from git.
func (m *memRepo) memoryRun(base, result, requestID string) *run.Run {
	return &run.Run{ID: "run-1", Project: "widget", RequestID: requestID, State: run.StateAccepted, BaseSHA: base, ResultSHA: result, ChangedFiles: m.changed(base, result)}
}

func saveTestProposal(t *testing.T, dataDir, requestID, expected string) {
	t.Helper()
	path, err := memory.ProposalPath(dataDir, "widget", requestID)
	if err != nil {
		t.Fatal(err)
	}
	if err := memory.SaveProposal(path, memory.Proposal{RequestID: requestID, Expected: expected}); err != nil {
		t.Fatal(err)
	}
}

// countingHost counts blobAtCommit calls on dp's fake host.
func countingHost(dp *deps) *int {
	n := new(int)
	host := fakeHostOf(dp)
	real := host.blobAtCommitFn
	host.blobAtCommitFn = func(ctx context.Context, repoDir, commit, path string) ([]byte, bool, error) {
		*n++
		return real(ctx, repoDir, commit, path)
	}
	return n
}

// memoryDenial is the memory reasons MergePolicyCheck gives a run carrying edit.
func memoryDenial(t *testing.T, r *run.Run) []string {
	t.Helper()
	r.DiffStat = &run.DiffStat{FilesChanged: 1, Insertions: 1}
	r.GateResults = []run.GateResult{{Check: "verify", Passed: true}}
	r.DependencyLockfilesTouched = []string{}
	_, reasons := release.MergePolicyCheck(*r, *allowingMergePolicyForTest())
	var out []string
	for _, reason := range reasons {
		if strings.Contains(reason, "memory") {
			out = append(out, reason)
		}
	}
	return out
}

func evaluate(t *testing.T, dp *deps, r *run.Run, dataDir, repo string) *run.MemoryEdit {
	t.Helper()
	r.MemoryEdit = computeMemoryEdit(context.Background(), dp, r, dataDir, repo)
	return r.MemoryEdit
}

func TestRunThatEditsMemorySectionWithoutAProposalIsDenied(t *testing.T) {
	const prose = "# Guide\n\nSome prose.\n\n"
	baseFile := fenced(prose, "\n## Tail\n", "- run make verify before pushing")
	cases := []struct {
		name       string
		base, edit *string // AGENTS.md at base (nil: absent) and at result
		denied     bool
	}{
		{"no AGENTS.md on either side", nil, nil, false},
		{"edited outside the fence", sp(baseFile), sp(strings.Replace(baseFile, "Some prose.", "Other prose.", 1)), false},
		{"file added with no fence", nil, sp("# Guide\n"), false},
		{"a line added inside the fence", sp(baseFile), sp(strings.Replace(baseFile, memory.EndMarker, "- a new lesson\n"+memory.EndMarker, 1)), true},
		{"section created", sp("# Guide\n"), sp(fenced("# Guide\n", "", "- new")), true},
		{"markers removed", sp(baseFile), sp(strings.NewReplacer(memory.BeginMarker+"\n", "", memory.EndMarker+"\n", "").Replace(baseFile)), true},
		{"a second begin marker added", sp(baseFile), sp(baseFile + memory.BeginMarker + "\n"), true},
		{"the section moved", sp(baseFile), sp(prose + "\n## Tail\n" + strings.TrimSuffix(strings.TrimPrefix(baseFile, prose), "\n## Tail\n")), true},
		{"CRLF inside the block", sp(baseFile), sp(strings.Replace(baseFile, "- run make verify before pushing\n", "- run make verify before pushing\r\n", 1)), true},
		{"file deleted with a section", sp(baseFile), nil, true},
		{"already damaged fence, edited elsewhere", sp(baseFile + memory.BeginMarker + "\n"), sp(baseFile + memory.BeginMarker + "\nmore\n"), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := newMemRepo(t)
			base := m.commit(map[string]*string{"README.md": sp("r\n"), "AGENTS.md": c.base})
			result := m.commit(map[string]*string{"AGENTS.md": c.edit})
			r := m.memoryRun(base, result, "req-1")
			edit := evaluate(t, newTestDeps(t), r, t.TempDir(), m.dir)
			reasons := memoryDenial(t, r)
			if !c.denied {
				if len(reasons) != 0 || (edit != nil && edit.SectionChanged) {
					t.Fatalf("edit = %+v reasons = %v, want no denial", edit, reasons)
				}
				return
			}
			if edit == nil || !edit.SectionChanged || edit.Proposal || len(reasons) != 1 || reasons[0] != release.ReasonMemorySectionNotMemoryChange {
				t.Fatalf("edit = %+v reasons = %v, want the not-a-memory-change denial", edit, reasons)
			}
		})
	}
}

func TestMemoryEditSymlinkAndCaseVariantAreSectionChanges(t *testing.T) {
	m := newMemRepo(t)
	base := m.commit(map[string]*string{"AGENTS.md": sp(fenced("# G\n", "", "- one")), "docs.md": sp("x\n")})
	link := m.symlinkCommit("AGENTS.md", "docs.md")
	r := m.memoryRun(base, link, "req-1")
	if edit := evaluate(t, newTestDeps(t), r, t.TempDir(), m.dir); edit == nil || !edit.SectionChanged || edit.ResultSHA256 != "" {
		t.Fatalf("symlink: edit = %+v, want a section change with no hash", edit)
	}

	m = newMemRepo(t)
	base = m.commit(map[string]*string{"README.md": sp("r\n")})
	variant := m.commit(map[string]*string{"agents.md": sp(fenced("", "", "- sneaky"))})
	r = m.memoryRun(base, variant, "")
	if edit := evaluate(t, newTestDeps(t), r, t.TempDir(), m.dir); edit == nil || !edit.SectionChanged {
		t.Fatalf("case variant carrying markers: edit = %+v, want a section change", edit)
	}
	if reasons := memoryDenial(t, r); len(reasons) != 1 || reasons[0] != release.ReasonMemorySectionNotMemoryChange {
		t.Fatalf("reasons = %v", reasons)
	}

	m = newMemRepo(t)
	base = m.commit(map[string]*string{"README.md": sp("r\n")})
	plain := m.commit(map[string]*string{"Agents.MD": sp("# no markers\n")})
	r = m.memoryRun(base, plain, "")
	if edit := evaluate(t, newTestDeps(t), r, t.TempDir(), m.dir); edit == nil || edit.SectionChanged {
		t.Fatalf("case variant without markers: edit = %+v, want evidence but no section change", edit)
	}
	if reasons := memoryDenial(t, r); len(reasons) != 0 {
		t.Fatalf("reasons = %v, want none", reasons)
	}
}

func TestMemoryRunDeniedWhenAgentsFileDiffersFromApprovedText(t *testing.T) {
	baseFile := "# Guide\n"
	expected := fenced(baseFile, "", "- run make verify before pushing")
	cases := []struct {
		name   string
		files  map[string]*string
		denied string // "" means released
	}{
		{"exactly the approved text", map[string]*string{"AGENTS.md": sp(expected)}, ""},
		{"approved text plus another file", map[string]*string{"AGENTS.md": sp(expected), "README.md": sp("changed\n")}, "README.md"},
		{"one byte different", map[string]*string{"AGENTS.md": sp(strings.Replace(expected, "verify", "verifY", 1))}, release.ReasonMemoryChangeNotApproved},
		{"trailing newline added", map[string]*string{"AGENTS.md": sp(expected + "\n")}, release.ReasonMemoryChangeNotApproved},
		{"build did nothing", map[string]*string{}, release.ReasonMemoryChangeNotApproved},
		{"file deleted", map[string]*string{"AGENTS.md": nil}, release.ReasonMemoryChangeNotApproved},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := newMemRepo(t)
			base := m.commit(map[string]*string{"README.md": sp("r\n"), "AGENTS.md": sp(baseFile)})
			result := m.commit(c.files)
			dataDir := t.TempDir()
			saveTestProposal(t, dataDir, "req-1", expected)
			r := m.memoryRun(base, result, "req-1")
			edit := evaluate(t, newTestDeps(t), r, dataDir, m.dir)
			reasons := memoryDenial(t, r)
			if edit == nil || !edit.Proposal {
				t.Fatalf("edit = %+v, want a proposal on record", edit)
			}
			if c.denied == "" {
				if len(reasons) != 0 || !edit.Matches {
					t.Fatalf("edit = %+v reasons = %v, want released", edit, reasons)
				}
				return
			}
			if len(reasons) != 1 || !strings.Contains(reasons[0], c.denied) {
				t.Fatalf("edit = %+v reasons = %v, want one containing %q", edit, reasons, c.denied)
			}
		})
	}
}

// The proposal was rendered from another base than the run's: the run is
// judged on the expected hash alone.
func TestMemoryRunJudgedOnExpectedHashNotOnProposalBase(t *testing.T) {
	m := newMemRepo(t)
	expected := fenced("# Guide\n", "", "- one")
	base := m.commit(map[string]*string{"AGENTS.md": sp("# Something newer\n")})
	result := m.commit(map[string]*string{"AGENTS.md": sp(expected)})
	dataDir := t.TempDir()
	path, _ := memory.ProposalPath(dataDir, "widget", "req-1")
	if err := memory.SaveProposal(path, memory.Proposal{RequestID: "req-1", BaseBlobSHA256: memory.HashHex([]byte("# Guide\n")), Expected: expected}); err != nil {
		t.Fatal(err)
	}
	r := m.memoryRun(base, result, "req-1")
	if edit := evaluate(t, newTestDeps(t), r, dataDir, m.dir); edit == nil || !edit.Matches || len(memoryDenial(t, r)) != 0 {
		t.Fatalf("edit = %+v, want a match", edit)
	}
}

func TestMemoryEditUsesDiffBaseWhenSet(t *testing.T) {
	m := newMemRepo(t)
	original := fenced("# G\n", "", "- one")
	diffBase := m.commit(map[string]*string{"AGENTS.md": sp(original)})
	checkout := m.commit(map[string]*string{"AGENTS.md": sp(fenced("# G\n", "", "- one", "- two"))}) // an earlier round's change
	result := m.commit(map[string]*string{"README.md": sp("x\n"), "AGENTS.md": sp(fenced("# G\n", "", "- one", "- two"))})
	r := m.memoryRun(diffBase, result, "")
	r.BaseSHA, r.DiffBaseSHA = checkout, diffBase
	if edit := evaluate(t, newTestDeps(t), r, t.TempDir(), m.dir); edit == nil || !edit.SectionChanged {
		t.Fatalf("edit = %+v, want the section change measured from the diff base", edit)
	}
}

func TestMemoryEditMakesNoGitCallForARunThatDidNotTouchAgentsFile(t *testing.T) {
	m := newMemRepo(t)
	base := m.commit(map[string]*string{"AGENTS.md": sp(fenced("# G\n", "", "- one"))})
	result := m.commit(map[string]*string{"README.md": sp("x\n")})
	dp := newTestDeps(t)
	calls := countingHost(dp)
	r := m.memoryRun(base, result, "req-1")
	if edit := evaluate(t, dp, r, t.TempDir(), m.dir); edit != nil || *calls != 0 {
		t.Fatalf("edit = %+v after %d git reads, want nil and none", edit, *calls)
	}
	// A nested AGENTS.md is not the root file.
	result = m.commit(map[string]*string{"sub/AGENTS.md": sp(fenced("", "", "- x"))})
	r = m.memoryRun(base, result, "")
	if edit := evaluate(t, dp, r, t.TempDir(), m.dir); edit != nil || *calls != 0 {
		t.Fatalf("nested: edit = %+v after %d git reads, want nil and none", edit, *calls)
	}
}

func TestMemoryEditGitFailureFailsClosedOnlyWhereGoverned(t *testing.T) {
	boom := func(context.Context, string, string, string) ([]byte, bool, error) {
		return nil, false, os.ErrPermission
	}
	m := newMemRepo(t)
	base := m.commit(map[string]*string{"AGENTS.md": sp("# G\n"), "README.md": sp("r\n")})
	result := m.commit(map[string]*string{"AGENTS.md": sp("# Guide edited\n")})

	dp := newTestDeps(t)
	fakeHostOf(dp).blobAtCommitFn = boom
	r := m.memoryRun(base, result, "")
	edit := evaluate(t, dp, r, t.TempDir(), m.dir)
	if edit == nil || edit.Error == "" || !edit.FailClosed {
		t.Fatalf("a run that changed AGENTS.md: edit = %+v, want a fail-closed error", edit)
	}
	if reasons := memoryDenial(t, r); len(reasons) != 1 || !strings.Contains(reasons[0], "could not be completed") {
		t.Fatalf("reasons = %v", reasons)
	}

	dataDir := t.TempDir()
	saveTestProposal(t, dataDir, "req-1", "x")
	other := m.commit(map[string]*string{"README.md": sp("changed\n")})
	r = m.memoryRun(result, other, "req-1")
	if edit := evaluate(t, dp, r, dataDir, m.dir); edit == nil || !edit.FailClosed || len(memoryDenial(t, r)) != 1 {
		t.Fatalf("a memory run: edit = %+v, want fail closed", edit)
	}

	// An unusable proposal file fails closed for its request too.
	bad, _ := memory.ProposalPath(dataDir, "widget", "req-2")
	if err := os.WriteFile(bad, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	r = m.memoryRun(result, other, "req-2")
	if edit := evaluate(t, dp, r, dataDir, m.dir); edit == nil || !edit.FailClosed {
		t.Fatalf("broken proposal: edit = %+v, want fail closed", edit)
	}
}

// The tail of the fixture run: apply the result and read what was recorded.
func applyMemoryRun(t *testing.T, m *memRepo, dataDir, base, result, requestID string) (*run.Run, release.Decision) {
	t.Helper()
	r := &run.Run{ID: "run-1", Ticket: "fixture-ticket", Project: "widget", ProjectPath: m.dir, RequestID: requestID,
		State: run.StateSliceRunning, CreatedAt: time.Now().Format(time.RFC3339Nano)}
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	res := allowingRunWorkflowResultForTest(run.StateAccepted, result)
	res.ChangedFiles = m.changed(base, result)
	if err := applyRunWorkflowResult(newTestDeps(t), r, dataDir, r.ID, r.Ticket, m.dir, base, "task-queue", res, false, allowingMergePolicyForTest(), forge.GHPullRequestOpener{}, false); err != nil {
		t.Fatalf("applyRunWorkflowResult: %v", err)
	}
	return r, readReleaseDecisionFile(t, dataDir, "widget", r.ID)
}

func readReleaseDecisionFile(t *testing.T, dataDir, project, runID string) release.Decision {
	t.Helper()
	d, err := release.LoadDecision(dataDir, project, runID)
	if err != nil || d == nil {
		t.Fatalf("LoadDecision = %v, %v", d, err)
	}
	return *d
}

func TestApplyRunWorkflowResultRecordsAndEnforcesMemoryEdit(t *testing.T) {
	baseFile := "# Guide\n"
	expected := fenced(baseFile, "", "- run make verify before pushing")
	edited := strings.Replace(expected, "verify", "verifY", 1)

	m := newMemRepo(t)
	base := m.commit(map[string]*string{"AGENTS.md": sp(baseFile)})
	good := m.commit(map[string]*string{"AGENTS.md": sp(expected)})
	dataDir := t.TempDir()
	saveTestProposal(t, dataDir, "req-1", expected)
	r, decision := applyMemoryRun(t, m, dataDir, base, good, "req-1")
	if r.MemoryEdit == nil || !r.MemoryEdit.Matches || !decision.Allowed {
		t.Fatalf("memory run with the approved text: edit = %+v decision = %+v, want released", r.MemoryEdit, decision)
	}
	if saved, err := run.Load(dataDir, r.ID); err != nil || saved.MemoryEdit == nil || !saved.MemoryEdit.SectionChanged {
		t.Fatalf("saved run = %+v, %v, want the evidence persisted", saved, err)
	}

	m = newMemRepo(t)
	base = m.commit(map[string]*string{"AGENTS.md": sp(baseFile)})
	bad := m.commit(map[string]*string{"AGENTS.md": sp(edited)})
	dataDir = t.TempDir()
	saveTestProposal(t, dataDir, "req-1", expected)
	if _, decision = applyMemoryRun(t, m, dataDir, base, bad, "req-1"); decision.Allowed || !strings.Contains(strings.Join(decision.Reasons, ";"), release.ReasonMemoryChangeNotApproved) {
		t.Fatalf("decision = %+v, want refused for not matching the approved text", decision)
	}

	m = newMemRepo(t)
	base = m.commit(map[string]*string{"AGENTS.md": sp(baseFile)})
	sneaky := m.commit(map[string]*string{"AGENTS.md": sp(expected)})
	if _, decision = applyMemoryRun(t, m, t.TempDir(), base, sneaky, ""); decision.Allowed || !strings.Contains(strings.Join(decision.Reasons, ";"), release.ReasonMemorySectionNotMemoryChange) {
		t.Fatalf("decision = %+v, want refused: the run has no proposal", decision)
	}
}

// A run recorded before the field existed gets the evidence on a retry that
// opens its pull request, before the decision is evaluated again.
func TestEnsureMemoryEditComputesAndPersistsForARunWithoutIt(t *testing.T) {
	m := newMemRepo(t)
	base := m.commit(map[string]*string{"AGENTS.md": sp("# G\n")})
	result := m.commit(map[string]*string{"AGENTS.md": sp(fenced("# G\n", "", "- one"))})
	dataDir := t.TempDir()
	r := m.memoryRun(base, result, "")
	r.WorkspacePath, r.ProjectPath = m.dir, m.dir
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	loaded, err := run.Load(dataDir, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	ensureMemoryEdit(newTestDeps(t), dataDir, r.ID, loaded)
	if loaded.MemoryEdit == nil || !loaded.MemoryEdit.SectionChanged {
		t.Fatalf("in memory: %+v", loaded.MemoryEdit)
	}
	if again, err := run.Load(dataDir, r.ID); err != nil || again.MemoryEdit == nil {
		t.Fatalf("persisted: %+v, %v", again, err)
	}
}

func TestBlobAtCommitReadsGitObjectsOnly(t *testing.T) {
	m := newMemRepo(t)
	c := m.commit(map[string]*string{"AGENTS.md": sp("committed\n"), "big.md": sp(strings.Repeat("x", maxAgentsBlobBytes+1))})
	if err := os.WriteFile(filepath.Join(m.dir, "AGENTS.md"), []byte("dirty worktree\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	host := realHost{}
	got, ok, err := host.blobAtCommit(context.Background(), m.dir, c, "AGENTS.md")
	if err != nil || !ok || string(got) != "committed\n" {
		t.Fatalf("blobAtCommit = %q, %v, %v, want the committed bytes", got, ok, err)
	}
	if _, ok, err := host.blobAtCommit(context.Background(), m.dir, c, "nothing.md"); ok || err != nil {
		t.Fatalf("absent: %v, %v", ok, err)
	}
	if _, _, err := host.blobAtCommit(context.Background(), m.dir, c, "big.md"); err != errBlobTooLarge {
		t.Fatalf("big: %v", err)
	}
	if _, _, err := host.blobAtCommit(context.Background(), m.dir, "--output=x", "AGENTS.md"); err == nil {
		t.Fatal("a commit argument that is not a full id was accepted")
	}
}
