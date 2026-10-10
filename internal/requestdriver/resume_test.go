package requestdriver_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"buildgate/internal/request"
	"buildgate/internal/requestdriver"
	"buildgate/internal/requestdriver/requestdrivertest"
	"buildgate/internal/run"
	"buildgate/internal/sandbox"
)

func TestValidateResumeWorktreeFlags(t *testing.T) {
	cases := []struct {
		name                  string
		of                    string
		onBranch, repo, prior string
		wantErr               string
	}{
		{name: "unset is always fine"},
		{name: "ok", of: "r1"},
		{name: "on-branch", of: "r1", onBranch: "x", wantErr: "-on-branch"},
		{name: "repository", of: "r1", repo: "o/r", wantErr: "-repository"},
		{name: "prior-run", of: "r1", prior: "p", wantErr: "-prior-run"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := requestdriver.ValidateResumeWorktreeFlags(tc.of, tc.onBranch, tc.repo, tc.prior)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %v, want one containing %q", err, tc.wantErr)
			}
		})
	}
}

// TestRequestResumeRefusalsRefusesADraftingStepWhileItsContainerLives covers
// the drafting branch of the resume preflight: a container labelled for the
// request blocks the resume, naming it; none lets it through.
func TestRequestResumeRefusalsRefusesADraftingStepWhileItsContainerLives(t *testing.T) {
	dataDir := t.TempDir()
	requestdrivertest.SeedLostStep(t, dataDir, "req-draft", request.StateSpecDrafting, "")
	r, err := request.Load(dataDir, "req-draft")
	if err != nil {
		t.Fatal(err)
	}

	containers := &requestdrivertest.FakeJobContainers{IDs: []string{"abc123"}}
	reasons, err := requestdriver.ResumeGate{Containers: containers}.RequestResumeRefusals(context.Background(), dataDir, "docker", r)
	if err != nil {
		t.Fatal(err)
	}
	if len(reasons) != 1 || !strings.Contains(reasons[0], "spec_drafting step is still alive") || !strings.Contains(reasons[0], "abc123") {
		t.Fatalf("reasons = %v, want one naming the live spec_drafting container", reasons)
	}
	if len(containers.Asked) != 1 || containers.Asked[0] != "req-draft" {
		t.Errorf("listed containers of %v, want the request id req-draft", containers.Asked)
	}

	if reasons, err = (requestdriver.ResumeGate{Containers: &requestdrivertest.FakeJobContainers{}}).RequestResumeRefusals(context.Background(), dataDir, "docker", r); err != nil || len(reasons) != 0 {
		t.Fatalf("with no container: reasons = %v, err = %v, want none", reasons, err)
	}
}

func capturingBuildRunner(t *testing.T, dataDir string, calls *[][]string) requestdriver.TicketRunner {
	base := requestdrivertest.AcceptingBuildRunner(t, dataDir)
	return func(ctx context.Context, args []string, onReady func(*run.Run)) error {
		*calls = append(*calls, append([]string(nil), args...))
		return base(ctx, args, onReady)
	}
}

func TestAdvanceBuildingResumeRoundAdoptsTheLostWorktreeAndKeepsIt(t *testing.T) {
	dp := newFakeDeps(t)
	dataDir, id, marker := requestdrivertest.LostBuildFixture(dp, t, request.ResumeRound)
	pre := &requestdrivertest.FakeResumePreconditions{T: t, OK: true}
	var calls [][]string

	got := requestdrivertest.AdvanceBuildingOnce(dp, t, dataDir, id, requestdriver.ResumeGate{Preconditions: pre}, capturingBuildRunner(t, dataDir, &calls))

	if len(calls) != 1 {
		t.Fatalf("build runner called %d times, want 1", len(calls))
	}
	if v := requestdrivertest.ArgValue(calls[0], "-resume-worktree-of"); v != "lost-run" {
		t.Errorf("-resume-worktree-of = %q, want the lost run", v)
	}
	if requestdrivertest.HasFlag(calls[0], "-prior-run") {
		t.Errorf("a resume passed -prior-run (%q); -resume-worktree-of refuses it and the worktree already holds the chain", requestdrivertest.ArgValue(calls[0], "-prior-run"))
	}
	if len(pre.Asked) != 1 || (pre.Asked)[0] != "lost-run" || pre.MaxRounds != requestdriver.DefaultMaxRounds {
		t.Errorf("preconditions asked %v with max rounds %d, want lost-run with %d", pre.Asked, pre.MaxRounds, requestdriver.DefaultMaxRounds)
	}
	if _, err := os.Stat(marker.WorktreePath); err != nil {
		t.Errorf("the kept worktree was cleared before it was adopted: %v", err)
	}
	if kept, _ := run.Load(dataDir, "lost-run"); !kept.KeptForResume {
		t.Error("the lost run lost KeptForResume: only the adopting run may clear it")
	}
	if got.State != request.StatePRReview || got.ResumeDecision != nil {
		t.Errorf("state %s decision %+v, want pr_review and the consumed decision dropped", got.State, got.ResumeDecision)
	}
}

func TestAdvanceBuildingConsumesTheResumeDecisionOnceTheRunStarts(t *testing.T) {
	dp := newFakeDeps(t)
	dataDir, id, _ := requestdrivertest.LostBuildFixture(dp, t, request.ResumeRound)
	gate := requestdriver.ResumeGate{Preconditions: &requestdrivertest.FakeResumePreconditions{T: t, OK: true}}
	runner := func(_ context.Context, args []string, onReady func(*run.Run)) error {
		onReady(&run.Run{ID: requestdrivertest.ArgValue(args, "-ticket"), State: run.StateReady})
		onDisk, err := request.Load(dataDir, id)
		if err != nil {
			t.Fatal(err)
		}
		if onDisk.ResumeDecision != nil {
			t.Errorf("decision %+v still on disk once the run started; a crash now would reuse it for the next build", onDisk.ResumeDecision)
		}
		if onDisk.Tickets[1].RunID != requestdrivertest.ArgValue(args, "-ticket") {
			t.Errorf("ticket run id %q, want the new run", onDisk.Tickets[1].RunID)
		}
		return (&run.Run{ID: requestdrivertest.ArgValue(args, "-ticket"), State: run.StateHalted, HaltError: "boom"}).Save(dataDir)
	}
	got := requestdrivertest.AdvanceBuildingOnce(dp, t, dataDir, id, gate, runner)
	if got.State != request.StateHalted {
		t.Fatalf("state %s, want halted", got.State)
	}

	// A later retry of the same ticket rebuilds: the spent decision is not reused.
	pre := &requestdrivertest.FakeResumePreconditions{T: t, OK: true}
	if _, err := request.Retry(dataDir, id, "alice", "", time.Now(), nil); err != nil {
		t.Fatal(err)
	}
	var calls [][]string
	requestdrivertest.AdvanceBuildingOnce(dp, t, dataDir, id, requestdriver.ResumeGate{Preconditions: pre}, capturingBuildRunner(t, dataDir, &calls))
	if len(calls) != 1 || requestdrivertest.HasFlag(calls[0], "-resume-worktree-of") || len(pre.Asked) != 0 {
		t.Errorf("rebuild after a consumed decision: calls %v, preconditions asked %v; want a fresh build", calls, pre.Asked)
	}
}

func TestAdvanceBuildingRefusedPreconditionsReturnToResumeReviewWithReasons(t *testing.T) {
	dp := newFakeDeps(t)
	dataDir, id, marker := requestdrivertest.LostBuildFixture(dp, t, request.ResumeRound)
	gate := requestdriver.ResumeGate{Preconditions: &requestdrivertest.FakeResumePreconditions{T: t, Reasons: []string{"1 sandbox container(s) labelled for run lost-run still exist", "the history was rewritten"}}}

	got := requestdrivertest.AdvanceBuildingOnce(dp, t, dataDir, id, gate, requestdrivertest.FailingBuildRunner(t))

	if got.State != request.StateResumeReview || got.Resume == nil || got.Resume.Generation != 2 || got.ResumeDecision != nil {
		t.Fatalf("state %s resume %+v decision %+v, want resume_review at generation 2 with no decision", got.State, got.Resume, got.ResumeDecision)
	}
	if len(got.Resume.Refused) != 2 || got.Resume.LostRunID != "lost-run" || got.Resume.FromState != request.StateBuilding {
		t.Errorf("Resume = %+v, want both reasons and the lost build", got.Resume)
	}
	for _, want := range []string{"still exist", "history was rewritten", "-from scratch", "factoryd cancel " + id} {
		if !strings.Contains(got.Error, want) {
			t.Errorf("reason %q lacks %q", got.Error, want)
		}
	}
	if strings.Contains(got.NextAction(), "factoryd resume "+id+"`") {
		t.Errorf("next action %q offers a plain resume after a refusal", got.NextAction())
	}
	if _, err := os.Stat(marker.WorktreePath); err != nil {
		t.Errorf("a refused resume discarded the kept worktree: %v", err)
	}
	if got.NotifyCount != 1 {
		t.Errorf("NotifyCount = %d, want the reminder sent on re-entering resume_review", got.NotifyCount)
	}
}

func TestAdvanceBuildingResumeScratchClearsKeptRunsAndBuildsFresh(t *testing.T) {
	dp := newFakeDeps(t)
	dataDir, id, marker := requestdrivertest.LostBuildFixture(dp, t, request.ResumeScratch)
	pre := &requestdrivertest.FakeResumePreconditions{T: t, OK: true}
	var calls [][]string

	got := requestdrivertest.AdvanceBuildingOnce(dp, t, dataDir, id, requestdriver.ResumeGate{Preconditions: pre}, capturingBuildRunner(t, dataDir, &calls))

	if len(calls) != 1 || requestdrivertest.HasFlag(calls[0], "-resume-worktree-of") {
		t.Fatalf("calls %v, want one fresh build without -resume-worktree-of", calls)
	}
	if requestdrivertest.ArgValue(calls[0], "-prior-run") != "prev-run" {
		t.Errorf("-prior-run = %q, want the chain from ticket 1", requestdrivertest.ArgValue(calls[0], "-prior-run"))
	}
	if len(pre.Asked) != 0 {
		t.Errorf("preconditions asked %v for a scratch rebuild, want none", pre.Asked)
	}
	if _, err := os.Stat(marker.WorktreePath); !os.IsNotExist(err) {
		t.Errorf("the lost run's kept worktree survived a rebuild from scratch (stat err = %v)", err)
	}
	if kept, _ := run.Load(dataDir, "lost-run"); kept.KeptForResume {
		t.Error("the lost run is still KeptForResume after a rebuild from scratch")
	}
	if got.State != request.StatePRReview || got.ResumeDecision != nil {
		t.Errorf("state %s decision %+v, want pr_review and no decision", got.State, got.ResumeDecision)
	}
}

// A decision that answers an earlier resume_review (stale Generation) is
// ignored: the build is a fresh one and checks nothing.
func TestAdvanceBuildingIgnoresAResumeDecisionOfAStaleGeneration(t *testing.T) {
	dp := newFakeDeps(t)
	dataDir, id, marker := requestdrivertest.LostBuildFixture(dp, t, request.ResumeRound)
	r, err := request.Load(dataDir, id)
	if err != nil {
		t.Fatal(err)
	}
	r.Resume.Generation = 2 // a second loss since the decision was made
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	pre := &requestdrivertest.FakeResumePreconditions{T: t, OK: true}
	var calls [][]string

	requestdrivertest.AdvanceBuildingOnce(dp, t, dataDir, id, requestdriver.ResumeGate{Preconditions: pre}, capturingBuildRunner(t, dataDir, &calls))

	if len(calls) != 1 || requestdrivertest.HasFlag(calls[0], "-resume-worktree-of") || len(pre.Asked) != 0 {
		t.Errorf("calls %v, preconditions asked %v; want a fresh build for a stale decision", calls, pre.Asked)
	}
	if _, err := os.Stat(marker.WorktreePath); !os.IsNotExist(err) {
		t.Errorf("a stale decision kept the old worktree (stat err = %v)", err)
	}
}

// A heartbeat lost while the process stays up (laptop sleep) halts the run
// with its worktree kept: the request waits for a human like any lost step,
// instead of going to halted, where `retry` would delete the worktree.
func TestAdvanceBuildingHaltedRunKeptForResumeEntersResumeReview(t *testing.T) {
	dp := newFakeDeps(t)
	for _, kept := range []bool{true, false} {
		dataDir, id := requestdrivertest.BuildingFixture(dp, t, 1)
		runner := func(_ context.Context, args []string, onReady func(*run.Run)) error {
			rid := requestdrivertest.ArgValue(args, "-ticket")
			onReady(&run.Run{ID: rid, State: run.StateReady})
			return (&run.Run{ID: rid, State: run.StateHalted, HaltError: "heartbeat lost", KeptForResume: kept}).Save(dataDir)
		}
		got := requestdrivertest.AdvanceBuildingOnce(dp, t, dataDir, id, requestdriver.ResumeGate{}, runner)
		if !kept {
			if got.State != request.StateHalted {
				t.Errorf("a halted run not kept: state %s, want halted", got.State)
			}
			continue
		}
		if got.State != request.StateResumeReview || got.Resume == nil || got.Resume.LostRunID != requestdrivertest.TicketRunID(id, 1) || got.Resume.FromState != request.StateBuilding {
			t.Errorf("state %s resume %+v, want resume_review of the kept run", got.State, got.Resume)
		}
		if got.NotifyCount != 1 {
			t.Errorf("NotifyCount = %d, want the reminder sent", got.NotifyCount)
		}
	}
}

// A precondition check that cannot be made (Docker down, record unreadable)
// goes back to resume_review with the reason and a reminder, not a silent
// loop in building.
func TestAdvanceBuildingPreconditionCheckErrorReturnsToResumeReview(t *testing.T) {
	dp := newFakeDeps(t)
	dataDir, id, marker := requestdrivertest.LostBuildFixture(dp, t, request.ResumeRound)
	got := requestdrivertest.AdvanceBuildingOnce(dp, t, dataDir, id, requestdriver.ResumeGate{Preconditions: &requestdrivertest.FakeResumePreconditions{T: t, Err: errors.New("docker: cannot connect to the daemon")}}, requestdrivertest.FailingBuildRunner(t))
	if got.State != request.StateResumeReview || got.Resume == nil || len(got.Resume.Refused) != 1 || !strings.Contains(got.Resume.Refused[0], "cannot connect to the daemon") {
		t.Fatalf("state %s resume %+v, want resume_review naming the error", got.State, got.Resume)
	}
	if got.NotifyCount != 1 || got.ResumeDecision != nil {
		t.Errorf("NotifyCount %d decision %+v, want a reminder and no decision", got.NotifyCount, got.ResumeDecision)
	}
	if _, err := os.Stat(marker.WorktreePath); err != nil {
		t.Errorf("the kept worktree was touched: %v", err)
	}
}

func TestRemindRequestForResumeReviewNamesTheLostStepAndTheThreeCommands(t *testing.T) {
	dataDir := t.TempDir()
	requestdrivertest.SeedLostStep(t, dataDir, "req-1", request.StateBuilding, "run-lost")
	r, err := request.Load(dataDir, "req-1")
	if err != nil {
		t.Fatal(err)
	}
	if !requestdriver.ReviewState(r.State) {
		t.Fatal("resume_review is not a reminded state")
	}
	requestdriver.RemindRequest(dataDir, r, time.Now())
	log, err := os.ReadFile(requestdriver.RequestNotificationLogPath(dataDir, "req-1"))
	if err != nil {
		t.Fatalf("read notification log: %v", err)
	}
	for _, want := range []string{"resume_review", "build", "factoryd resume req-1", "-from scratch", "factoryd cancel req-1"} {
		if !strings.Contains(string(log), want) {
			t.Errorf("reminder %q lacks %q", log, want)
		}
	}
	if strings.Contains(string(log), "factoryd approve") {
		t.Errorf("reminder %q offers approve", log)
	}
	if r.NotifyCount != 1 || r.WaitingSince == "" {
		t.Errorf("reminder bookkeeping: count %d since %q", r.NotifyCount, r.WaitingSince)
	}
}

func TestResolveResumeFromCarriesTheKeptWorktree(t *testing.T) {
	dataDir, _, marker, base := requestdrivertest.KeptRun(t, "kept-run")
	head := requestdrivertest.CommitInWorktree(t, marker.WorktreePath, "round1.txt")
	requestdrivertest.WriteRoundStateHead(t, marker.WorktreePath, head)

	appendRelayLedger(t, dataDir, "kept-run", 90, 7)
	got, carried, err := requestdriver.ResolveResumeFrom(context.Background(), dataDir, requestdrivertest.NoContainersDocker(t), "kept-run", "", 0)
	if err != nil {
		t.Fatalf("resolveResumeFrom: %v", err)
	}
	if got.RunID != "kept-run" || got.WorktreePath != marker.WorktreePath || got.Branch != marker.Branch || got.BaseSHA != base {
		t.Errorf("ResumeFrom = %+v", got)
	}
	if carried == nil || *carried != (run.MeterSpend{Tokens: 90, CostMicroUSD: 7}) {
		t.Errorf("carried spend = %+v, want the halted run's ledger", carried)
	}

	requestdrivertest.WorktreeGitOut(t, marker.WorktreePath, "checkout", "-b", "elsewhere")
	if _, _, err := requestdriver.ResolveResumeFrom(context.Background(), dataDir, requestdrivertest.NoContainersDocker(t), "kept-run", "", 0); err == nil || !strings.Contains(err.Error(), "cannot resume") {
		t.Errorf("resolveResumeFrom on a wrong branch = %v, want a cannot-resume refusal", err)
	}
}

// appendRelayLedger writes the run's one meter ledger and records the
// sandbox it belongs to, which is where sandbox.RunRelaySpend finds it.
func appendRelayLedger(t *testing.T, dataDir, runID string, tokens, cost int) {
	t.Helper()
	path := filepath.Join(run.Dir(dataDir, runID), "meter-ledger.jsonl")
	if err := sandbox.RecordSandbox(dataDir, runID, sandbox.SandboxRecord{Name: "sb", ID: "sb", Ledger: path}); err != nil {
		t.Fatal(err)
	}
	line := fmt.Sprintf(`{"input_tokens":%d,"output_tokens":0,"cost_micro_usd":%d}`+"\n", tokens, cost)
	if err := os.WriteFile(path, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestResumeRefusedForAChangedSpecAndForNoRoundLeft(t *testing.T) {
	dataDir, _, marker, _ := requestdrivertest.KeptRun(t, "kept-run")
	r, _ := run.Load(dataDir, "kept-run")
	r.SpecSHA256 = "aaaa"
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	docker := requestdrivertest.NoContainersDocker(t)
	if _, _, err := requestdriver.ResolveResumeFrom(context.Background(), dataDir, docker, "kept-run", "bbbb", 3); err == nil || !strings.Contains(err.Error(), "spec changed") {
		t.Errorf("a changed spec: err = %v", err)
	}
	if _, _, err := requestdriver.ResolveResumeFrom(context.Background(), dataDir, docker, "kept-run", "aaaa", 3); err != nil {
		t.Errorf("the same spec: err = %v", err)
	}
	if err := os.WriteFile(filepath.Join(marker.WorktreePath, ".pi-build-round-state.json"), []byte(`{"version":1,"last_completed_round":3}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := requestdriver.ResolveResumeFrom(context.Background(), dataDir, docker, "kept-run", "aaaa", 3); err == nil || !strings.Contains(err.Error(), "no round is left") {
		t.Errorf("no round left: err = %v", err)
	}
}

func TestKeptRunsAreClearedByARebuildAndByCancel(t *testing.T) {
	dataDir, repoDir, marker, _ := requestdrivertest.KeptRun(t, "kept-run")
	r, _ := run.Load(dataDir, "kept-run")
	r.RequestID = "req-1"
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	other := requestdrivertest.TestIsolationMarker(t, repoDir, dataDir, "kept-other", "temporal")
	if err := (&run.Run{ID: "kept-other", State: run.StateHalted, HaltConfirmed: true, KeptForResume: true, RequestID: "req-2", ProjectPath: repoDir, WorkspacePath: other.WorktreePath, Branch: other.Branch}).Save(dataDir); err != nil {
		t.Fatal(err)
	}

	// Cancel of req-1 clears only req-1's run.
	if err := requestdriver.ClearKeptRunsOfRequest(dataDir, "req-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker.WorktreePath); !os.IsNotExist(err) {
		t.Errorf("req-1's kept worktree survived cancel (stat err = %v)", err)
	}
	if got, _ := run.Load(dataDir, "kept-run"); got.KeptForResume {
		t.Error("req-1's run is still KeptForResume after cancel")
	}
	if _, err := os.Stat(other.WorktreePath); err != nil {
		t.Errorf("another request's kept worktree was touched: %v", err)
	}
}

// A is kept, B resumes A and is kept in turn: B carries A's request, so a
// retry or cancel of the request reaps B's worktree (the ticket's recorded run
// still names A, whose flag adoption cleared).
func TestAResumedRunThatIsKeptInTurnIsReapedWithItsRequest(t *testing.T) {
	dataDir, repoDir, markerA, _ := requestdrivertest.KeptRun(t, "run-a")
	a, _ := run.Load(dataDir, "run-a")
	a.RequestID = "req-1"
	if err := a.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	// Adoption cleared A's flag and moved the worktree to B.
	a.KeptForResume = false
	if err := a.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	markerB := requestdrivertest.TestIsolationMarker(t, repoDir, dataDir, "run-b", "temporal")
	if got := requestdriver.ResumedRequestID(dataDir, "run-a"); got != "req-1" {
		t.Fatalf("resumedRequestID = %q, want the halted run's request", got)
	}
	b := &run.Run{ID: "run-b", State: run.StateHalted, HaltConfirmed: true, KeptForResume: true, RequestID: requestdriver.ResumedRequestID(dataDir, "run-a"), ProjectPath: repoDir, WorkspacePath: markerB.WorktreePath, Branch: markerB.Branch}
	if err := b.Save(dataDir); err != nil {
		t.Fatal(err)
	}

	if err := requestdriver.ClearKeptRunsOfRequest(dataDir, "req-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(markerB.WorktreePath); !os.IsNotExist(err) {
		t.Errorf("the resumed run's kept worktree leaked (stat err = %v)", err)
	}
	if got, _ := run.Load(dataDir, "run-b"); got.KeptForResume {
		t.Error("the resumed run is still KeptForResume")
	}
	_ = markerA
}
