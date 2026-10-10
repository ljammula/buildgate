package requestdrivertest

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"buildgate/internal/request"
	"buildgate/internal/requestdriver"
	"buildgate/internal/run"
	"buildgate/internal/testfixture"
	wsisolation "buildgate/internal/workspace"
)

func TestIsolationMarker(t *testing.T, repoDir, dataDir, runID, mode string) wsisolation.IsolationMarker {
	t.Helper()
	base, err := exec.Command("git", "-C", repoDir, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("resolve HEAD: %v", err)
	}
	parentDir := filepath.Join(dataDir, "workspaces")
	worktreePath, branch, err := wsisolation.Prepare(repoDir, parentDir, runID, strings.TrimSpace(string(base)))
	if err != nil {
		t.Fatalf("prepare isolated worktree: %v", err)
	}
	commonDir, err := wsisolation.GitCommonDir(repoDir)
	if err != nil {
		t.Fatalf("resolve common dir: %v", err)
	}
	marker := wsisolation.IsolationMarker{
		Version: wsisolation.IsolationMarkerVersion, RunID: runID, Mode: mode,
		WorktreeID: runID,
		RepoDir:    repoDir, CommonDir: commonDir, DataDir: dataDir,
		ParentDir: parentDir, WorktreePath: worktreePath, Branch: branch,
		Prepared: true, WorkflowID: "workflow-" + runID,
		CheckpointDir: filepath.Join(dataDir, "temporal-checkpoints", runID),
	}
	if err := wsisolation.WriteIsolationMarker(wsisolation.IsolationMarkerPath(dataDir, runID), marker); err != nil {
		t.Fatalf("write isolation marker: %v", err)
	}
	return marker
}

// FakeReclaimDocker writes a docker stand-in that lists the containers in
// the returned psFile (one "name<TAB>runid" per line) for every `ps`, logs
// every invocation to the returned logFile, and succeeds at everything else.
func FakeReclaimDocker(t *testing.T) (docker, psFile, logFile string) {
	t.Helper()
	dir := t.TempDir()
	docker = filepath.Join(dir, "docker")
	psFile = filepath.Join(dir, "ps.txt")
	logFile = filepath.Join(dir, "calls.log")
	script := "#!/bin/sh\necho \"$*\" >> " + logFile + "\n" +
		"if [ \"$1\" = ps ]; then cat " + psFile + "; fi\nexit 0\n"
	if err := os.WriteFile(docker, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake docker: %v", err)
	}
	return docker, psFile, logFile
}

// SeedLostStep saves a request whose step in state was lost, waiting in
// resume_review; lostRunID is the run a lost build was running.
func SeedLostStep(t *testing.T, dataDir, id string, state request.State, lostRunID string) {
	t.Helper()
	NewApprovableDriverRequest(t, dataDir, id, state)
	r, err := request.Load(dataDir, id)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.EnterResumeReview(state, lostRunID, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}
}

// FakeJobContainers reports ids as the containers labelled for a request's
// drafting job, and records the request ids it was asked about.
type FakeJobContainers struct {
	IDs   []string
	Asked []string
}

func (f *FakeJobContainers) RunContainerIDs(_ context.Context, _, _, runID string) ([]string, error) {
	f.Asked = append(f.Asked, runID)
	return f.IDs, nil
}

// FakeResumePreconditions stands in for the Docker and git checks of a
// resume. It records the run ids it was asked about and the round limit it
// saw; err, when set, is what the check returns.
type FakeResumePreconditions struct {
	T         *testing.T
	OK        bool
	Reasons   []string
	Err       error
	Asked     []string
	MaxRounds int
}

func (f *FakeResumePreconditions) CheckResumePreconditions(_ context.Context, _, _, haltedRunID, specSHA256 string, rounds int) (bool, []string, error) {
	if specSHA256 == "" {
		f.T.Error("the resume check got no ticket spec hash")
	}
	f.Asked = append(f.Asked, haltedRunID)
	f.MaxRounds = rounds
	return f.OK, f.Reasons, f.Err
}

// LostBuildFixture is a two-ticket request in resume_review whose ticket-2
// build, run "lost-run", was lost; the run's worktree is kept. decision, when
// not "", is applied through ResumeDecide, returning the request to building.
func LostBuildFixture(dp requestdriver.Deps, t *testing.T, decision string) (dataDir, id string, marker wsisolation.IsolationMarker) {
	t.Helper()
	dataDir, id = BuildingFixture(dp, t, 2)
	r, err := request.Load(dataDir, id)
	if err != nil {
		t.Fatal(err)
	}
	r.TicketIndex = 2
	r.Tickets[0].RunID = "prev-run"
	if err := (&run.Run{ID: "prev-run", BaseSHA: strings.Repeat("1", 40)}).Save(dataDir); err != nil {
		t.Fatal(err)
	}
	r.Tickets[1].RunID = "lost-run"
	if err := r.EnterResumeReview(request.StateBuilding, "lost-run", time.Now()); err != nil {
		t.Fatal(err)
	}
	if decision != "" {
		if _, err := r.ResumeDecide(decision, "alice", time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	repoDir := testfixture.NewGitRepo(t)
	marker = TestIsolationMarker(t, repoDir, dataDir, "lost-run", "temporal")
	if err := (&run.Run{
		ID: "lost-run", State: run.StateHalted, HaltConfirmed: true, KeptForResume: true, RequestID: id,
		ProjectPath: repoDir, WorkspacePath: marker.WorktreePath, Branch: marker.Branch,
	}).Save(dataDir); err != nil {
		t.Fatal(err)
	}
	return dataDir, id, marker
}

func AdvanceBuildingOnce(dp requestdriver.Deps, t *testing.T, dataDir, id string, gate requestdriver.ResumeGate, runner requestdriver.TicketRunner) *request.Request {
	t.Helper()
	r, err := request.Load(dataDir, id)
	if err != nil {
		t.Fatal(err)
	}
	if err := requestdriver.AdvanceBuilding(dp, context.Background(), dataDir, r, requestdriver.WorkerConfig{Resume: gate}, runner, time.Now()); err != nil {
		t.Fatalf("advanceBuilding: %v", err)
	}
	got, err := request.Load(dataDir, id)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// KeptRun builds a halted, KeptForResume run over a real isolated worktree:
// a repository, a temporal-mode isolation marker and worktree, and the run
// record. It returns the data dir, the repo, the marker and the base commit.
func KeptRun(t *testing.T, id string) (dataDir, repoDir string, marker wsisolation.IsolationMarker, base string) {
	t.Helper()
	repoDir = testfixture.NewGitRepo(t)
	dataDir = t.TempDir()
	marker = TestIsolationMarker(t, repoDir, dataDir, id, "temporal")
	out, err := exec.Command("git", "-C", repoDir, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("resolve base: %v", err)
	}
	base = strings.TrimSpace(string(out))
	r := &run.Run{
		ID: id, State: run.StateHalted, HaltConfirmed: true, KeptForResume: true,
		ProjectPath: repoDir, WorkspacePath: marker.WorktreePath, Branch: marker.Branch, BaseSHA: base,
	}
	if err := r.Save(dataDir); err != nil {
		t.Fatalf("save kept run: %v", err)
	}
	return dataDir, repoDir, marker, base
}

func WorktreeGitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	full := append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com", "-C", dir}, args...)
	out, err := exec.Command("git", full...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func CommitInWorktree(t *testing.T, dir, name string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(name), 0o644); err != nil {
		t.Fatal(err)
	}
	WorktreeGitOut(t, dir, "add", name)
	WorktreeGitOut(t, dir, "commit", "-m", name)
	return WorktreeGitOut(t, dir, "rev-parse", "HEAD")
}

func WriteRoundStateHead(t *testing.T, dir, head string) {
	t.Helper()
	body := `{"version":1,"last_completed_round":1,"head":"` + head + `"}`
	if err := os.WriteFile(filepath.Join(dir, ".pi-build-round-state.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func NoContainersDocker(t *testing.T) string {
	t.Helper()
	docker, psFile, _ := FakeReclaimDocker(t)
	if err := os.WriteFile(psFile, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	return docker
}
