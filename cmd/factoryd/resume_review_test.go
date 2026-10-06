package main

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"buildgate/internal/request"
	"buildgate/internal/requestdriver"
	"buildgate/internal/run"
	wsisolation "buildgate/internal/workspace"
)

// seedLostStep saves a request whose step in state was lost, waiting in
// resume_review; lostRunID is the run a lost build was running.
func seedLostStep(t *testing.T, dataDir, id string, state request.State, lostRunID string) {
	t.Helper()
	newApprovableDriverRequest(t, dataDir, id, state)
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

// fakeJobContainers reports ids as the containers labelled for a request's
// drafting job, and records the request ids it was asked about.
type fakeJobContainers struct {
	ids   []string
	asked []string
}

func (f *fakeJobContainers) RunContainerIDs(_ context.Context, _, _, runID string) ([]string, error) {
	f.asked = append(f.asked, runID)
	return f.ids, nil
}

// fakeResumePreconditions stands in for the Docker and git checks of a
// resume. It records the run ids it was asked about and the round limit it
// saw; err, when set, is what the check returns.
type fakeResumePreconditions struct {
	t         *testing.T
	ok        bool
	reasons   []string
	err       error
	asked     []string
	maxRounds int
}

func (f *fakeResumePreconditions) CheckResumePreconditions(_ context.Context, _, _, haltedRunID, specSHA256 string, rounds int) (bool, []string, error) {
	if specSHA256 == "" {
		f.t.Error("the resume check got no ticket spec hash")
	}
	f.asked = append(f.asked, haltedRunID)
	f.maxRounds = rounds
	return f.ok, f.reasons, f.err
}

// lostBuildFixture is a two-ticket request in resume_review whose ticket-2
// build, run "lost-run", was lost; the run's worktree is kept. decision, when
// not "", is applied through ResumeDecide, returning the request to building.
func lostBuildFixture(dp *deps, t *testing.T, decision string) (dataDir, id string, marker wsisolation.IsolationMarker) {
	t.Helper()
	dataDir, id = buildingFixture(dp, t, 2)
	r, err := request.Load(dataDir, id)
	if err != nil {
		t.Fatal(err)
	}
	r.TicketIndex = 2
	r.Tickets[0].RunID = "prev-run"
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
	repoDir := newFixtureRepo(t)
	marker = testIsolationMarker(t, repoDir, dataDir, "lost-run", "temporal")
	if err := (&run.Run{
		ID: "lost-run", State: run.StateHalted, HaltConfirmed: true, KeptForResume: true, RequestID: id,
		ProjectPath: repoDir, WorkspacePath: marker.WorktreePath, Branch: marker.Branch,
	}).Save(dataDir); err != nil {
		t.Fatal(err)
	}
	return dataDir, id, marker
}

func advanceBuildingOnce(dp *deps, t *testing.T, dataDir, id string, gate requestdriver.ResumeGate, runner requestdriver.TicketRunner) *request.Request {
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

func capturingBuildRunner(t *testing.T, dataDir string, calls *[][]string) requestdriver.TicketRunner {
	base := acceptingBuildRunner(t, dataDir)
	return func(ctx context.Context, args []string, onReady func(*run.Run)) error {
		*calls = append(*calls, append([]string(nil), args...))
		return base(ctx, args, onReady)
	}
}

func TestAdvanceBuildingResumeRoundAdoptsTheLostWorktreeAndKeepsIt(t *testing.T) {
	dp := newTestDeps(t)
	dataDir, id, marker := lostBuildFixture(dp, t, request.ResumeRound)
	pre := &fakeResumePreconditions{t: t, ok: true}
	var calls [][]string

	got := advanceBuildingOnce(dp, t, dataDir, id, requestdriver.ResumeGate{Preconditions: pre}, capturingBuildRunner(t, dataDir, &calls))

	if len(calls) != 1 {
		t.Fatalf("build runner called %d times, want 1", len(calls))
	}
	if v := argValue(calls[0], "-resume-worktree-of"); v != "lost-run" {
		t.Errorf("-resume-worktree-of = %q, want the lost run", v)
	}
	if hasFlag(calls[0], "-prior-run") {
		t.Errorf("a resume passed -prior-run (%q); -resume-worktree-of refuses it and the worktree already holds the chain", argValue(calls[0], "-prior-run"))
	}
	if len(pre.asked) != 1 || (pre.asked)[0] != "lost-run" || pre.maxRounds != requestdriver.DefaultMaxRounds {
		t.Errorf("preconditions asked %v with max rounds %d, want lost-run with %d", pre.asked, pre.maxRounds, requestdriver.DefaultMaxRounds)
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
	dp := newTestDeps(t)
	dataDir, id, _ := lostBuildFixture(dp, t, request.ResumeRound)
	gate := requestdriver.ResumeGate{Preconditions: &fakeResumePreconditions{t: t, ok: true}}
	runner := func(_ context.Context, args []string, onReady func(*run.Run)) error {
		onReady(&run.Run{ID: argValue(args, "-ticket"), State: run.StateReady})
		onDisk, err := request.Load(dataDir, id)
		if err != nil {
			t.Fatal(err)
		}
		if onDisk.ResumeDecision != nil {
			t.Errorf("decision %+v still on disk once the run started; a crash now would reuse it for the next build", onDisk.ResumeDecision)
		}
		if onDisk.Tickets[1].RunID != argValue(args, "-ticket") {
			t.Errorf("ticket run id %q, want the new run", onDisk.Tickets[1].RunID)
		}
		return (&run.Run{ID: argValue(args, "-ticket"), State: run.StateHalted, HaltError: "boom"}).Save(dataDir)
	}
	got := advanceBuildingOnce(dp, t, dataDir, id, gate, runner)
	if got.State != request.StateHalted {
		t.Fatalf("state %s, want halted", got.State)
	}

	// A later retry of the same ticket rebuilds: the spent decision is not reused.
	pre := &fakeResumePreconditions{t: t, ok: true}
	if _, err := request.Retry(dataDir, id, "alice", "", time.Now(), nil); err != nil {
		t.Fatal(err)
	}
	var calls [][]string
	advanceBuildingOnce(dp, t, dataDir, id, requestdriver.ResumeGate{Preconditions: pre}, capturingBuildRunner(t, dataDir, &calls))
	if len(calls) != 1 || hasFlag(calls[0], "-resume-worktree-of") || len(pre.asked) != 0 {
		t.Errorf("rebuild after a consumed decision: calls %v, preconditions asked %v; want a fresh build", calls, pre.asked)
	}
}

func TestAdvanceBuildingRefusedPreconditionsReturnToResumeReviewWithReasons(t *testing.T) {
	dp := newTestDeps(t)
	dataDir, id, marker := lostBuildFixture(dp, t, request.ResumeRound)
	gate := requestdriver.ResumeGate{Preconditions: &fakeResumePreconditions{t: t, reasons: []string{"1 sandbox container(s) labelled for run lost-run still exist", "the history was rewritten"}}}

	got := advanceBuildingOnce(dp, t, dataDir, id, gate, failingBuildRunner(t))

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
	dp := newTestDeps(t)
	dataDir, id, marker := lostBuildFixture(dp, t, request.ResumeScratch)
	pre := &fakeResumePreconditions{t: t, ok: true}
	var calls [][]string

	got := advanceBuildingOnce(dp, t, dataDir, id, requestdriver.ResumeGate{Preconditions: pre}, capturingBuildRunner(t, dataDir, &calls))

	if len(calls) != 1 || hasFlag(calls[0], "-resume-worktree-of") {
		t.Fatalf("calls %v, want one fresh build without -resume-worktree-of", calls)
	}
	if argValue(calls[0], "-prior-run") != "prev-run" {
		t.Errorf("-prior-run = %q, want the chain from ticket 1", argValue(calls[0], "-prior-run"))
	}
	if len(pre.asked) != 0 {
		t.Errorf("preconditions asked %v for a scratch rebuild, want none", pre.asked)
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
	dp := newTestDeps(t)
	dataDir, id, marker := lostBuildFixture(dp, t, request.ResumeRound)
	r, err := request.Load(dataDir, id)
	if err != nil {
		t.Fatal(err)
	}
	r.Resume.Generation = 2 // a second loss since the decision was made
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	pre := &fakeResumePreconditions{t: t, ok: true}
	var calls [][]string

	advanceBuildingOnce(dp, t, dataDir, id, requestdriver.ResumeGate{Preconditions: pre}, capturingBuildRunner(t, dataDir, &calls))

	if len(calls) != 1 || hasFlag(calls[0], "-resume-worktree-of") || len(pre.asked) != 0 {
		t.Errorf("calls %v, preconditions asked %v; want a fresh build for a stale decision", calls, pre.asked)
	}
	if _, err := os.Stat(marker.WorktreePath); !os.IsNotExist(err) {
		t.Errorf("a stale decision kept the old worktree (stat err = %v)", err)
	}
}

func TestResumeMainDefaultsToRoundAndChecksTheKeptWorktree(t *testing.T) {
	dp := newTestDeps(t)
	isolateSessionConfig(t)
	woken := stubWake(dp, t, nil)
	pre := &fakeResumePreconditions{t: t, ok: true}
	dataDir, id, _ := lostBuildFixture(dp, t, "")

	if err := resumeMainWith(dp, []string{"-data-dir", dataDir, id}, requestdriver.ResumeGate{Preconditions: pre}); err != nil {
		t.Fatalf("resumeMain: %v", err)
	}
	got, _ := request.Load(dataDir, id)
	if got.State != request.StateBuilding || got.PendingResumeVerb() != request.ResumeRound {
		t.Errorf("state %s verb %q, want building with the default round decision", got.State, got.PendingResumeVerb())
	}
	if len(pre.asked) != 1 || (pre.asked)[0] != "lost-run" {
		t.Errorf("preconditions asked %v, want one check of lost-run", pre.asked)
	}
	if len(*woken) != 1 || (*woken)[0] != id {
		t.Errorf("woken %v, want one wake", *woken)
	}
}

func TestResumeMainRefusesUpFrontWhenThePreconditionsFail(t *testing.T) {
	dp := newTestDeps(t)
	isolateSessionConfig(t)
	woken := stubWake(dp, t, nil)
	pre := &fakeResumePreconditions{t: t, reasons: []string{"a container of the lost run is alive"}}
	dataDir, id, _ := lostBuildFixture(dp, t, "")

	err := resumeMainWith(dp, []string{"-data-dir", dataDir, id}, requestdriver.ResumeGate{Preconditions: pre})
	if err == nil {
		t.Fatal("resumeMain succeeded with failing preconditions")
	}
	for _, want := range []string{"a container of the lost run is alive", "-from scratch", "factoryd cancel " + id} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}
	if got, _ := request.Load(dataDir, id); got.State != request.StateResumeReview || got.ResumeDecision != nil {
		t.Errorf("a refused resume changed the request: %s %+v", got.State, got.ResumeDecision)
	}
	if len(*woken) != 0 {
		t.Errorf("woken %v, want none for a refused resume", *woken)
	}
}

func TestResumeMainScratchSkipsThePreconditionsAndRejectsAnUnknownFrom(t *testing.T) {
	dp := newTestDeps(t)
	isolateSessionConfig(t)
	woken := stubWake(dp, t, nil)
	pre := &fakeResumePreconditions{t: t, reasons: []string{"never consulted"}}
	dataDir, id, _ := lostBuildFixture(dp, t, "")

	if err := resumeMainWith(dp, []string{"-data-dir", dataDir, "-from", "bogus", id}, requestdriver.ResumeGate{Preconditions: pre}); err == nil || !strings.Contains(err.Error(), "-from") {
		t.Errorf("unknown -from: err = %v", err)
	}
	if err := resumeMainWith(dp, []string{"-data-dir", dataDir, "-from", "scratch", id}, requestdriver.ResumeGate{Preconditions: pre}); err != nil {
		t.Fatalf("resumeMain -from scratch: %v", err)
	}
	got, _ := request.Load(dataDir, id)
	if got.PendingResumeVerb() != request.ResumeScratch || len(pre.asked) != 0 || len(*woken) != 1 {
		t.Errorf("verb %q, asked %v, woken %v; want scratch, no check, one wake", got.PendingResumeVerb(), pre.asked, *woken)
	}
}

func TestRetryMainRefusesAResumeReviewRequestWithTheResumeHint(t *testing.T) {
	dp := newTestDeps(t)
	woken := stubWake(dp, t, nil)
	dataDir := t.TempDir()
	seedLostStep(t, dataDir, "req-1", request.StatePlanning, "")
	err := retryMain(dp, []string{"-data-dir", dataDir, "req-1"})
	if err == nil || !strings.Contains(err.Error(), "factoryd resume req-1") {
		t.Fatalf("retryMain err = %v, want the `factoryd resume` hint", err)
	}
	if len(*woken) != 0 {
		t.Errorf("woken %v, want none", *woken)
	}
}

// A heartbeat lost while the process stays up (laptop sleep) halts the run
// with its worktree kept: the request waits for a human like any lost step,
// instead of going to halted, where `retry` would delete the worktree.
func TestAdvanceBuildingHaltedRunKeptForResumeEntersResumeReview(t *testing.T) {
	dp := newTestDeps(t)
	for _, kept := range []bool{true, false} {
		dataDir, id := buildingFixture(dp, t, 1)
		runner := func(_ context.Context, args []string, onReady func(*run.Run)) error {
			rid := argValue(args, "-ticket")
			onReady(&run.Run{ID: rid, State: run.StateReady})
			return (&run.Run{ID: rid, State: run.StateHalted, HaltError: "heartbeat lost", KeptForResume: kept}).Save(dataDir)
		}
		got := advanceBuildingOnce(dp, t, dataDir, id, requestdriver.ResumeGate{}, runner)
		if !kept {
			if got.State != request.StateHalted {
				t.Errorf("a halted run not kept: state %s, want halted", got.State)
			}
			continue
		}
		if got.State != request.StateResumeReview || got.Resume == nil || got.Resume.LostRunID != ticketRunID(id, 1) || got.Resume.FromState != request.StateBuilding {
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
	dp := newTestDeps(t)
	dataDir, id, marker := lostBuildFixture(dp, t, request.ResumeRound)
	got := advanceBuildingOnce(dp, t, dataDir, id, requestdriver.ResumeGate{Preconditions: &fakeResumePreconditions{t: t, err: errors.New("docker: cannot connect to the daemon")}}, failingBuildRunner(t))
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

// The lost run is recorded only when its record exists, was kept for a resume
// and is not accepted: retry keeps the ticket's previous RunID, which a step
// lost before the new run existed would otherwise name.
func TestLostRunOfNamesOnlyAKeptNonAcceptedRun(t *testing.T) {
	dp := newTestDeps(t)
	dataDir, id := buildingFixture(dp, t, 1)
	r, err := request.Load(dataDir, id)
	if err != nil {
		t.Fatal(err)
	}
	r.Tickets[0].RunID = "old-run"
	if got := lostRunOf(dataDir, r); got != "" {
		t.Errorf("no run record: lostRunOf = %q, want none", got)
	}
	save := func(rec run.Run) {
		t.Helper()
		if err := rec.Save(dataDir); err != nil {
			t.Fatal(err)
		}
	}
	save(run.Run{ID: "old-run", State: run.StateHalted})
	if got := lostRunOf(dataDir, r); got != "" {
		t.Errorf("run not kept: lostRunOf = %q, want none", got)
	}
	save(run.Run{ID: "old-run", State: run.StateAccepted, KeptForResume: true})
	if got := lostRunOf(dataDir, r); got != "" {
		t.Errorf("accepted run: lostRunOf = %q, want none", got)
	}
	save(run.Run{ID: "old-run", State: run.StateHalted, KeptForResume: true})
	if got := lostRunOf(dataDir, r); got != "old-run" {
		t.Errorf("kept halted run: lostRunOf = %q, want old-run", got)
	}
}

// A round resume of a lost build with no kept run refuses with a clear
// reason, up front and again at the build.
func TestResumeRoundWithNoKeptBuildIsRefusedNamingScratch(t *testing.T) {
	dp := newTestDeps(t)
	isolateSessionConfig(t)
	woken := stubWake(dp, t, nil)
	pre := &fakeResumePreconditions{t: t, ok: true}
	dataDir, id := buildingFixture(dp, t, 1)
	r, _ := request.Load(dataDir, id)
	if err := r.EnterResumeReview(request.StateBuilding, "", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	err := resumeMainWith(dp, []string{"-data-dir", dataDir, id}, requestdriver.ResumeGate{Preconditions: pre})
	if err == nil || !strings.Contains(err.Error(), "no kept build to continue; use -from scratch") {
		t.Fatalf("resumeMain err = %v, want the no-kept-build refusal", err)
	}
	if len(*woken) != 0 || len(pre.asked) != 0 {
		t.Errorf("woken %v asked %v, want neither", *woken, pre.asked)
	}

	// At the build: a decision that got through anyway returns to resume_review.
	r, _ = request.Load(dataDir, id)
	if _, err := r.ResumeDecide(request.ResumeRound, "alice", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	got := advanceBuildingOnce(dp, t, dataDir, id, requestdriver.ResumeGate{Preconditions: pre}, failingBuildRunner(t))
	if got.State != request.StateResumeReview || len(got.Resume.Refused) != 1 || !strings.Contains(got.Resume.Refused[0], "use -from scratch") {
		t.Errorf("state %s resume %+v, want a refusal naming -from scratch", got.State, got.Resume)
	}
}

// A decision that continues no kept build ends the wait of any kept worktree.
func TestResumeMainReapsKeptRunsWhenNoKeptBuildIsContinued(t *testing.T) {
	dp := newTestDeps(t)
	isolateSessionConfig(t)
	stubWake(dp, t, nil)
	pre := &fakeResumePreconditions{t: t, ok: true}
	gate := requestdriver.ResumeGate{Preconditions: pre, Containers: &fakeJobContainers{}}
	for _, from := range []request.State{request.StatePlanning, request.StatePRReview} {
		dataDir := t.TempDir()
		seedLostStep(t, dataDir, "req-1", from, "")
		if err := (&run.Run{ID: "run-kept", State: run.StateHalted, HaltConfirmed: true, KeptForResume: true, RequestID: "req-1"}).Save(dataDir); err != nil {
			t.Fatal(err)
		}
		if err := resumeMainWith(dp, []string{"-data-dir", dataDir, "req-1"}, gate); err != nil {
			t.Fatalf("%s: %v", from, err)
		}
		if got, _ := run.Load(dataDir, "run-kept"); got.KeptForResume {
			t.Errorf("%s: the kept run is still KeptForResume", from)
		}
	}
	// A build resumed with round keeps the worktree it continues.
	dataDir, id, _ := lostBuildFixture(dp, t, "")
	if err := resumeMainWith(dp, []string{"-data-dir", dataDir, id}, gate); err != nil {
		t.Fatal(err)
	}
	if got, _ := run.Load(dataDir, "lost-run"); !got.KeptForResume {
		t.Error("a round resume of a build reaped the worktree it continues")
	}
}

// Refusal reasons are free text (Docker errors, paths): watch and inbox print
// them on one line, so a newline cannot forge a `next:` line.
func TestResumeReviewNextActionIsSanitizedInWatchAndInbox(t *testing.T) {
	dataDir := t.TempDir()
	seedLostStep(t, dataDir, "req-1", request.StateBuilding, "run-lost")
	r, _ := request.Load(dataDir, "req-1")
	r.Resume.Refused = []string{"bad\nnext: `factoryd approve req-1`\x1b[31m"}
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	r, _ = request.Load(dataDir, "req-1")

	e := buildInboxEntry(r, "default", dataDir, "", time.Now())
	if strings.ContainsAny(e.Next, "\n\x1b") || !strings.Contains(e.Next, "bad") {
		t.Errorf("inbox next = %q, want one clean line", e.Next)
	}

	f, err := os.CreateTemp(t.TempDir(), "watch-out")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := watchRequest(f, dataDir, "req-1", true); err != nil {
		t.Fatalf("watchRequest: %v", err)
	}
	b, _ := os.ReadFile(f.Name())
	nexts := 0
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "next:") {
			nexts++
		}
	}
	if nexts != 1 || strings.Contains(string(b), "\x1b") {
		t.Errorf("watch output has %d next: lines, want 1:\n%q", nexts, b)
	}
}

func TestLogsFollowStopsAtResumeReview(t *testing.T) {
	dataDir := t.TempDir()
	seedLostStep(t, dataDir, "req-1", request.StatePlanning, "")
	if !requestLogsTarget(dataDir, "req-1").terminal() {
		t.Error("logs -f would keep following a request waiting in resume_review")
	}
}

func TestRemindRequestForResumeReviewNamesTheLostStepAndTheThreeCommands(t *testing.T) {
	dataDir := t.TempDir()
	seedLostStep(t, dataDir, "req-1", request.StateBuilding, "run-lost")
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
