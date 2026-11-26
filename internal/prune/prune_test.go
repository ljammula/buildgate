package prune

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"buildgate/internal/request"
	"buildgate/internal/run"
	"buildgate/internal/sandbox"
	"buildgate/internal/testfixture"
	"buildgate/internal/workspace"
)

var (
	pruneNow  = time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	longAgo   = pruneNow.Add(-30 * 24 * time.Hour)
	twoWeeks  = 14 * 24 * time.Hour
	yesterday = pruneNow.Add(-24 * time.Hour)
)

// gitRemover removes worktrees and round refs with the real git and records
// the round-ref deletions. onRemove, when set, runs inside each worktree
// removal.
type gitRemover struct {
	refsPruned []string
	onRemove   func()
}

func (g *gitRemover) RemoveWorktree(repoDir, worktreePath string) error {
	if g.onRemove != nil {
		g.onRemove()
	}
	return workspace.RemoveWorktreeOnly(repoDir, worktreePath)
}
func (g *gitRemover) DeleteRoundRefs(repoDir, id string) error {
	g.refsPruned = append(g.refsPruned, id)
	return workspace.DeleteRoundRefs(repoDir, id)
}

type fixture struct {
	t       *testing.T
	dataDir string
	repo    string
	head    string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	repo := testfixture.NewGitRepo(t)
	out, err := exec.Command("git", "-C", repo, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{t: t, dataDir: t.TempDir(), repo: repo, head: strings.TrimSpace(string(out))}
}

// request saves a request in state, entered at.
func (f *fixture) request(id string, state request.State, at time.Time) {
	f.t.Helper()
	r := request.New(id, f.repo, "app", request.Source{Kind: request.SourceText}, at)
	r.State = state
	r.EnteredAt = at.UTC().Format(time.RFC3339Nano)
	if err := r.Save(f.dataDir); err != nil {
		f.t.Fatal(err)
	}
}

// run saves an isolated run of requestID (none: "") with a real worktree, a
// scratch directory, a build session and the records a reader uses, and
// returns it.
func (f *fixture) run(id, requestID string, state run.State, at time.Time) *run.Run {
	f.t.Helper()
	path, branch, err := workspace.Prepare(f.repo, filepath.Join(f.dataDir, "workspaces"), id, f.head)
	if err != nil {
		f.t.Fatal(err)
	}
	writeFile(f.t, filepath.Join(path, "big.bin"), strings.Repeat("x", 4096))
	r := &run.Run{ID: id, Ticket: id, RequestID: requestID, ProjectPath: f.repo, WorkspacePath: path, Branch: branch,
		State: state, CreatedAt: at.Format(time.RFC3339), UpdatedAt: at.Format(time.RFC3339)}
	if err := r.Save(f.dataDir); err != nil {
		f.t.Fatal(err)
	}
	runDir := run.Dir(f.dataDir, id)
	writeFile(f.t, filepath.Join(runDir, "abc.attempt-1-build_app.log"), "build output")
	writeFile(f.t, filepath.Join(runDir, "progress.jsonl"), "{}\n")
	writeFile(f.t, filepath.Join(runDir, "prompts", "build-1", "build.md"), "the prompt")
	writeFile(f.t, filepath.Join(runDir, run.BuildSessionDirName, "session.jsonl"), strings.Repeat("s", 100))
	writeFile(f.t, filepath.Join(sandbox.RunScratchDir(f.dataDir, id), "cache", "obj"), strings.Repeat("c", 200))
	return r
}

// roundTag gives request id a ticket with one round tag in the fixture's
// repository, and creates its round ref and the unpushed retry-from branch
// `retry -from` makes, both at the repository's head.
func (f *fixture) roundTag(id, runID string) {
	f.t.Helper()
	r, err := request.Load(f.dataDir, id)
	if err != nil {
		f.t.Fatal(err)
	}
	r.Tickets = []request.Ticket{{Index: 1, RunID: runID, RoundTags: []request.RoundTag{{N: 1, RunID: runID, Repo: f.repo}}}}
	if err := r.Save(f.dataDir); err != nil {
		f.t.Fatal(err)
	}
	for _, args := range [][]string{
		{"update-ref", "refs/buildgate/rounds/" + id + "/t1/r1", f.head},
		{"branch", "buildgate/" + id + "/t1-from-r1", f.head},
	} {
		if out, err := exec.Command("git", append([]string{"-C", f.repo}, args...)...).CombinedOutput(); err != nil {
			f.t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
}

func (f *fixture) refExists(ref string) bool {
	return exec.Command("git", "-C", f.repo, "rev-parse", "--verify", "--quiet", ref).Run() == nil
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// records is every file under runs/ and requests/ except the build sessions,
// with its bytes.
func records(t *testing.T, dataDir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, top := range []string{"runs", "requests"} {
		_ = filepath.WalkDir(filepath.Join(dataDir, top), func(p string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() && d.Name() == run.BuildSessionDirName {
				return filepath.SkipDir
			}
			if d.Type().IsRegular() && d.Name() != ".lock" {
				b, _ := os.ReadFile(p)
				out[p] = string(b)
			}
			return nil
		})
	}
	return out
}

func kinds(p Plan) map[string]int {
	out := map[string]int{}
	for _, it := range p.Items {
		out[it.Owner+" "+it.Kind]++
	}
	return out
}

func TestPruneRemovesAFinishedRequestsWorktreeScratchAndSessionAndKeepsEveryRecord(t *testing.T) {
	f := newFixture(t)
	f.request("req-done", request.StateDone, longAgo)
	rec := f.run("req-done-001", "req-done", run.StateAccepted, longAgo)
	f.roundTag("req-done", "req-done-001")
	before := records(t, f.dataDir)

	plan, err := PlanDataDir(f.dataDir, twoWeeks, pruneNow)
	if err != nil {
		t.Fatal(err)
	}
	if got := kinds(plan); got["req-done worktree"] != 1 || got["req-done scratch"] != 1 || got["req-done build-session"] != 1 || len(plan.Items) != 3 {
		t.Fatalf("plan items = %v, want the worktree, scratch and build session of req-done", got)
	}
	if plan.Bytes() < 4096+200+100 {
		t.Errorf("plan bytes = %d, want at least the files written", plan.Bytes())
	}
	rm := &gitRemover{}
	freed, err := plan.Apply(rm)
	if err != nil {
		t.Fatal(err)
	}
	if freed != plan.Bytes() {
		t.Errorf("freed = %d, want %d", freed, plan.Bytes())
	}
	for _, it := range plan.Items {
		if _, err := os.Lstat(it.Path); !os.IsNotExist(err) {
			t.Errorf("%s %s still exists", it.Kind, it.Path)
		}
	}
	if len(rm.refsPruned) != 1 || rm.refsPruned[0] != "req-done" {
		t.Errorf("round refs pruned for %v, want [req-done]", rm.refsPruned)
	}
	after := records(t, f.dataDir)
	for p, body := range before {
		if after[p] != body {
			t.Errorf("record %s changed or went", p)
		}
	}
	if out, err := exec.Command("git", "-C", f.repo, "rev-parse", "--verify", "refs/heads/"+rec.Branch).CombinedOutput(); err != nil {
		t.Errorf("the run's branch went with its worktree: %v %s", err, out)
	}
	again, err := PlanDataDir(f.dataDir, twoWeeks, pruneNow)
	if err != nil || len(again.Items) != 0 {
		t.Errorf("a second plan = %v, %v; want nothing left", again.Items, err)
	}
}

func TestPruneLeavesEveryRequestThatCanStillMoveOrIsTooRecent(t *testing.T) {
	f := newFixture(t)
	for _, s := range []request.State{request.StateQuarantined, request.StateHalted, request.StatePRReview, request.StateBuilding, request.StateResumeReview} {
		id := "req-" + string(s)
		f.request(id, s, longAgo)
		f.run(id+"-001", id, run.StateAccepted, longAgo)
	}
	f.request("req-recent", request.StateDone, yesterday)
	f.run("req-recent-001", "req-recent", run.StateAccepted, yesterday)
	// A finished request one of whose runs is not confirmed terminal.
	f.request("req-unconfirmed", request.StateCancelled, longAgo)
	f.run("req-unconfirmed-001", "req-unconfirmed", run.StateHalted, longAgo)
	// A run with no request that an override can still move.
	f.run("solo-quarantined", "", run.StateQuarantined, longAgo)

	plan, err := PlanDataDir(f.dataDir, twoWeeks, pruneNow)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Items) != 0 || len(plan.Requests) != 0 {
		t.Errorf("plan = %v requests %v, want nothing", kinds(plan), plan.Requests)
	}
}

func TestPruneKeepsARunWhoseOwnerIsUnknownAndJudgesAReplayRunByItsRequest(t *testing.T) {
	f := newFixture(t)
	// A run no record ties to a request: old and accepted, but nothing says
	// it is not a request's ticket still to come back to.
	f.run("solo-accepted", "", run.StateAccepted, longAgo)
	// A restack replay run with no RequestID of its own, whose replay names
	// its request, and a run only a ticket names: both old and accepted
	// while their request waits in pr_review.
	f.request("req-pr", request.StatePRReview, longAgo)
	replay := f.run("req-pr-replay", "", run.StateAccepted, longAgo)
	replay.Replay = &run.Replay{RequestID: "req-pr", TicketIndex: 1, OfRunID: "req-pr-001"}
	if err := replay.Save(f.dataDir); err != nil {
		t.Fatal(err)
	}
	f.run("req-pr-ticket", "", run.StateAccepted, longAgo)
	r, err := request.Load(f.dataDir, "req-pr")
	if err != nil {
		t.Fatal(err)
	}
	r.Tickets = []request.Ticket{{Index: 1, RunID: "req-pr-ticket"}}
	if err := r.Save(f.dataDir); err != nil {
		t.Fatal(err)
	}
	plan, err := PlanDataDir(f.dataDir, twoWeeks, pruneNow)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Items) != 0 || len(plan.Requests) != 0 {
		t.Fatalf("plan = %v, want nothing: no run is judged on its own age", kinds(plan))
	}

	// Once the request is done, the run only its ticket names goes with it.
	r.State = request.StateDone
	r.EnteredAt = longAgo.Format(time.RFC3339Nano)
	if err := r.Save(f.dataDir); err != nil {
		t.Fatal(err)
	}
	plan, err = PlanDataDir(f.dataDir, twoWeeks, pruneNow)
	if err != nil {
		t.Fatal(err)
	}
	got := kinds(plan)
	if got["req-pr worktree"] != 2 || got["solo-accepted worktree"] != 0 {
		t.Fatalf("plan = %v, want the two worktrees of req-pr's runs and none of the unowned run", got)
	}
}

// savedRun saves an accepted run of req-done with the given workspace.
func (f *fixture) savedRun(id, workspacePath string) {
	f.t.Helper()
	r := &run.Run{ID: id, RequestID: "req-done", ProjectPath: f.repo, WorkspacePath: workspacePath, Branch: "factoryd/" + id,
		State: run.StateAccepted, UpdatedAt: longAgo.Format(time.RFC3339)}
	if err := r.Save(f.dataDir); err != nil {
		f.t.Fatal(err)
	}
}

func TestPruneListsAWorktreeOnlyWhenTheFactoryMadeItAndNothingElseClaimsIt(t *testing.T) {
	f := newFixture(t)
	f.request("req-done", request.StateDone, longAgo)
	// Positive control: the factory's own worktree is pruned.
	f.run("req-done-ok", "req-done", run.StateAccepted, longAgo)
	// The operator's own checkout as the workspace: not the project's own
	// path check alone, so give it a branch.
	f.savedRun("req-done-own", f.repo)
	// A linked worktree, but outside <data dir>/workspaces: only the parent
	// directory check protects it.
	outside, _, err := workspace.Prepare(f.repo, t.TempDir(), "req-done-outside", f.head)
	if err != nil {
		t.Fatal(err)
	}
	f.savedRun("req-done-outside", outside)
	// Inside workspaces, but a repository of its own (.git a directory):
	// only the .git file check protects it.
	clone := filepath.Join(f.dataDir, "workspaces", "req-done-clone")
	if out, err := exec.Command("git", "clone", "-q", f.repo, clone).CombinedOutput(); err != nil {
		t.Fatalf("git clone: %v %s", err, out)
	}
	f.savedRun("req-done-clone", clone)
	// A worktree a later run adopted (its marker names it).
	adopted := f.run("req-done-adopted", "req-done", run.StateAccepted, longAgo)
	marker := workspace.IsolationMarker{RunID: "other-run", WorktreePath: adopted.WorkspacePath, RepoDir: f.repo, DataDir: f.dataDir}
	if err := workspace.WriteIsolationMarker(workspace.IsolationMarkerPath(f.dataDir, "other-run"), marker); err != nil {
		t.Fatal(err)
	}
	plan, err := PlanDataDir(f.dataDir, twoWeeks, pruneNow)
	if err != nil {
		t.Fatal(err)
	}
	var listed []string
	for _, it := range plan.Items {
		if it.Kind == KindWorktree {
			listed = append(listed, it.RunID)
		}
	}
	if len(listed) != 1 || listed[0] != "req-done-ok" {
		t.Errorf("listed worktrees of %v, want only req-done-ok", listed)
	}
}

func TestPruneNeverListsAWorktreeANonPrunedRunRecordNames(t *testing.T) {
	f := newFixture(t)
	f.request("req-done", request.StateDone, longAgo)
	shared := f.run("req-done-001", "req-done", run.StateAccepted, longAgo)
	// A run of another, still moving request records the same workspace.
	f.request("req-live", request.StateBuilding, longAgo)
	other := &run.Run{ID: "req-live-001", RequestID: "req-live", ProjectPath: f.repo, WorkspacePath: shared.WorkspacePath,
		State: run.StateAccepted, UpdatedAt: longAgo.Format(time.RFC3339)}
	if err := other.Save(f.dataDir); err != nil {
		t.Fatal(err)
	}
	plan, err := PlanDataDir(f.dataDir, twoWeeks, pruneNow)
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range plan.Items {
		if it.Kind == KindWorktree {
			t.Errorf("listed worktree %s that req-live-001's record names", it.Path)
		}
	}
	if kinds(plan)["req-done scratch"] != 1 {
		t.Errorf("the rest of req-done is still pruned: %v", kinds(plan))
	}
}

func TestPruneKeepsEveryBranchAndDeletesOnlyTheRoundRefs(t *testing.T) {
	f := newFixture(t)
	f.request("req-done", request.StateDone, longAgo)
	rec := f.run("req-done-001", "req-done", run.StateAccepted, longAgo)
	f.roundTag("req-done", "req-done-001")
	plan, err := PlanDataDir(f.dataDir, twoWeeks, pruneNow)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := plan.Apply(&gitRemover{}); err != nil {
		t.Fatal(err)
	}
	if f.refExists("refs/buildgate/rounds/req-done/t1/r1") {
		t.Error("the round ref is still there")
	}
	for _, branch := range []string{"buildgate/req-done/t1-from-r1", rec.Branch} {
		if !f.refExists("refs/heads/" + branch) {
			t.Errorf("branch %s was deleted", branch)
		}
	}
}

func TestPruneSkipsARequestWhoseFileNamesAnotherRequest(t *testing.T) {
	f := newFixture(t)
	f.request("req-real", request.StateDone, longAgo)
	f.run("req-real-001", "req-real", run.StateAccepted, longAgo)
	// requests/req-copy/request.json is a copy of req-real's.
	b, err := os.ReadFile(request.Path(f.dataDir, "req-real"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(request.Dir(f.dataDir, "req-copy"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(request.Path(f.dataDir, "req-copy"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	plan, err := PlanDataDir(f.dataDir, twoWeeks, pruneNow)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Skipped) != 1 || !strings.Contains(plan.Skipped[0], "req-copy") {
		t.Errorf("skipped = %v, want req-copy named", plan.Skipped)
	}
	if len(plan.Requests) != 1 || plan.Requests[0] != "req-real" {
		t.Errorf("requests = %v, want only req-real", plan.Requests)
	}
}

func TestPruneApplySkipsARequestThatIsNoLongerFinished(t *testing.T) {
	f := newFixture(t)
	f.request("req-done", request.StateDone, longAgo)
	f.run("req-done-001", "req-done", run.StateAccepted, longAgo)
	plan, err := PlanDataDir(f.dataDir, twoWeeks, pruneNow)
	if err != nil || len(plan.Items) == 0 {
		t.Fatalf("plan = %v, %v", plan.Items, err)
	}
	r, err := request.Load(f.dataDir, "req-done")
	if err != nil {
		t.Fatal(err)
	}
	r.State = request.StateBuilding
	if err := r.Save(f.dataDir); err != nil {
		t.Fatal(err)
	}
	freed, err := plan.Apply(&gitRemover{})
	if err == nil || freed != 0 {
		t.Errorf("Apply = %d, %v; want nothing freed and the refusal", freed, err)
	}
	for _, it := range plan.Items {
		if _, statErr := os.Lstat(it.Path); statErr != nil {
			t.Errorf("%s %s was removed: %v", it.Kind, it.Path, statErr)
		}
	}
}

func TestPruneApplySkipsWhenARunIsNotTerminalAnymore(t *testing.T) {
	f := newFixture(t)
	f.request("req-done", request.StateDone, longAgo)
	f.run("req-done-001", "req-done", run.StateAccepted, longAgo)
	plan, err := PlanDataDir(f.dataDir, twoWeeks, pruneNow)
	if err != nil || len(plan.Items) == 0 {
		t.Fatalf("plan = %v, %v", plan.Items, err)
	}
	// A run of the request that came after the listing and is moving.
	late := &run.Run{ID: "req-done-002", RequestID: "req-done", ProjectPath: f.repo, State: run.StateSliceRunning, UpdatedAt: pruneNow.Format(time.RFC3339)}
	if err := late.Save(f.dataDir); err != nil {
		t.Fatal(err)
	}
	freed, err := plan.Apply(&gitRemover{})
	if err == nil || freed != 0 {
		t.Errorf("Apply = %d, %v; want nothing freed and the refusal", freed, err)
	}
	for _, it := range plan.Items {
		if _, statErr := os.Lstat(it.Path); statErr != nil {
			t.Errorf("%s %s was removed: %v", it.Kind, it.Path, statErr)
		}
	}
}

func TestPruneApplyHoldsTheRequestLockWhileItRemoves(t *testing.T) {
	f := newFixture(t)
	f.request("req-done", request.StateDone, longAgo)
	f.run("req-done-001", "req-done", run.StateAccepted, longAgo)
	plan, err := PlanDataDir(f.dataDir, twoWeeks, pruneNow)
	if err != nil {
		t.Fatal(err)
	}
	var tookLock chan struct{}
	rm := &gitRemover{onRemove: func() {
		tookLock = make(chan struct{})
		go func() {
			if unlock, err := request.Lock(f.dataDir, "req-done"); err == nil {
				unlock()
			}
			close(tookLock)
		}()
		select {
		case <-tookLock:
			t.Error("the request's lock was free while a worktree was being removed")
		case <-time.After(300 * time.Millisecond):
		}
	}}
	if _, err := plan.Apply(rm); err != nil {
		t.Fatal(err)
	}
	if tookLock == nil {
		t.Fatal("no worktree was removed")
	}
	<-tookLock
}
