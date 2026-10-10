package main

import (
	"os"
	"strings"
	"testing"
	"time"

	"buildgate/internal/request"
	"buildgate/internal/requestdriver"
	"buildgate/internal/requestdriver/requestdrivertest"
	"buildgate/internal/run"
)

func TestResumeMainDefaultsToRoundAndChecksTheKeptWorktree(t *testing.T) {
	dp := newTestDeps(t)
	isolateSessionConfig(t)
	woken := stubWake(dp, t, nil)
	pre := &requestdrivertest.FakeResumePreconditions{T: t, OK: true}
	dataDir, id, _ := requestdrivertest.LostBuildFixture(dp, t, "")

	if err := resumeMainWith(dp, []string{"-data-dir", dataDir, id}, requestdriver.ResumeGate{Preconditions: pre}); err != nil {
		t.Fatalf("resumeMain: %v", err)
	}
	got, _ := request.Load(dataDir, id)
	if got.State != request.StateBuilding || got.PendingResumeVerb() != request.ResumeRound {
		t.Errorf("state %s verb %q, want building with the default round decision", got.State, got.PendingResumeVerb())
	}
	if len(pre.Asked) != 1 || (pre.Asked)[0] != "lost-run" {
		t.Errorf("preconditions asked %v, want one check of lost-run", pre.Asked)
	}
	if len(*woken) != 1 || (*woken)[0] != id {
		t.Errorf("woken %v, want one wake", *woken)
	}
}

func TestResumeMainRefusesUpFrontWhenThePreconditionsFail(t *testing.T) {
	dp := newTestDeps(t)
	isolateSessionConfig(t)
	woken := stubWake(dp, t, nil)
	pre := &requestdrivertest.FakeResumePreconditions{T: t, Reasons: []string{"a container of the lost run is alive"}}
	dataDir, id, _ := requestdrivertest.LostBuildFixture(dp, t, "")

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
	pre := &requestdrivertest.FakeResumePreconditions{T: t, Reasons: []string{"never consulted"}}
	dataDir, id, _ := requestdrivertest.LostBuildFixture(dp, t, "")

	if err := resumeMainWith(dp, []string{"-data-dir", dataDir, "-from", "bogus", id}, requestdriver.ResumeGate{Preconditions: pre}); err == nil || !strings.Contains(err.Error(), "-from") {
		t.Errorf("unknown -from: err = %v", err)
	}
	if err := resumeMainWith(dp, []string{"-data-dir", dataDir, "-from", "scratch", id}, requestdriver.ResumeGate{Preconditions: pre}); err != nil {
		t.Fatalf("resumeMain -from scratch: %v", err)
	}
	got, _ := request.Load(dataDir, id)
	if got.PendingResumeVerb() != request.ResumeScratch || len(pre.Asked) != 0 || len(*woken) != 1 {
		t.Errorf("verb %q, asked %v, woken %v; want scratch, no check, one wake", got.PendingResumeVerb(), pre.Asked, *woken)
	}
}

func TestRetryMainRefusesAResumeReviewRequestWithTheResumeHint(t *testing.T) {
	dp := newTestDeps(t)
	woken := stubWake(dp, t, nil)
	dataDir := t.TempDir()
	requestdrivertest.SeedLostStep(t, dataDir, "req-1", request.StatePlanning, "")
	err := retryMain(dp, []string{"-data-dir", dataDir, "req-1"})
	if err == nil || !strings.Contains(err.Error(), "factoryd resume req-1") {
		t.Fatalf("retryMain err = %v, want the `factoryd resume` hint", err)
	}
	if len(*woken) != 0 {
		t.Errorf("woken %v, want none", *woken)
	}
}

// The lost run is recorded only when its record exists, was kept for a resume
// and is not accepted: retry keeps the ticket's previous RunID, which a step
// lost before the new run existed would otherwise name.
func TestLostRunOfNamesOnlyAKeptNonAcceptedRun(t *testing.T) {
	dp := newTestDeps(t)
	dataDir, id := requestdrivertest.BuildingFixture(dp, t, 1)
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
	pre := &requestdrivertest.FakeResumePreconditions{T: t, OK: true}
	dataDir, id := requestdrivertest.BuildingFixture(dp, t, 1)
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
	if len(*woken) != 0 || len(pre.Asked) != 0 {
		t.Errorf("woken %v asked %v, want neither", *woken, pre.Asked)
	}

	// At the build: a decision that got through anyway returns to resume_review.
	r, _ = request.Load(dataDir, id)
	if _, err := r.ResumeDecide(request.ResumeRound, "alice", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	got := requestdrivertest.AdvanceBuildingOnce(dp, t, dataDir, id, requestdriver.ResumeGate{Preconditions: pre}, requestdrivertest.FailingBuildRunner(t))
	if got.State != request.StateResumeReview || len(got.Resume.Refused) != 1 || !strings.Contains(got.Resume.Refused[0], "use -from scratch") {
		t.Errorf("state %s resume %+v, want a refusal naming -from scratch", got.State, got.Resume)
	}
}

// A decision that continues no kept build ends the wait of any kept worktree.
func TestResumeMainReapsKeptRunsWhenNoKeptBuildIsContinued(t *testing.T) {
	dp := newTestDeps(t)
	isolateSessionConfig(t)
	stubWake(dp, t, nil)
	pre := &requestdrivertest.FakeResumePreconditions{T: t, OK: true}
	gate := requestdriver.ResumeGate{Preconditions: pre, Containers: &requestdrivertest.FakeJobContainers{}}
	for _, from := range []request.State{request.StatePlanning, request.StatePRReview} {
		dataDir := t.TempDir()
		requestdrivertest.SeedLostStep(t, dataDir, "req-1", from, "")
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
	dataDir, id, _ := requestdrivertest.LostBuildFixture(dp, t, "")
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
	requestdrivertest.SeedLostStep(t, dataDir, "req-1", request.StateBuilding, "run-lost")
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
	requestdrivertest.SeedLostStep(t, dataDir, "req-1", request.StatePlanning, "")
	if !requestLogsTarget(dataDir, "req-1").terminal() {
		t.Error("logs -f would keep following a request waiting in resume_review")
	}
}
