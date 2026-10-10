package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"buildgate/internal/evidence"
	"buildgate/internal/handoff"
	"buildgate/internal/request"
	"buildgate/internal/requestdriver"
	"buildgate/internal/requestdriver/requestdrivertest"
	"buildgate/internal/run"
)

func TestValidateEarlierAttemptFile(t *testing.T) {
	dir := t.TempDir()
	good := dir + "/earlier-attempt.md"
	if err := os.WriteFile(good, []byte("# record\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	big := dir + "/big.md"
	if err := os.WriteFile(big, make([]byte, maxEarlierAttemptBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	link := dir + "/link.md"
	if err := os.Symlink(good, link); err != nil {
		t.Fatal(err)
	}
	for path, wantErr := range map[string]bool{"": false, good: false, big: true, link: true, dir: true, dir + "/missing.md": true} {
		if err := validateEarlierAttemptFile(path); (err != nil) != wantErr {
			t.Errorf("validateEarlierAttemptFile(%q) = %v, want an error: %v", path, err, wantErr)
		}
	}
	if err := validateFollowUpRunInputs("not-a-sha", "", ""); err == nil {
		t.Error("a malformed -diff-base was accepted")
	}
	if err := validateFollowUpRunInputs("", "abc123", ""); err == nil {
		t.Error("a malformed -instruction-base was accepted")
	}
}

// TestRetryGivesTheRebuildTheRecordOfTheFailedAttempt: after `factoryd
// retry`, the ticket's rebuild is told what the quarantined attempt failed
// on, when a build may be told, and then continues from that attempt's
// commit on its branch. Any other rebuild, and every `retry -from scratch`,
// starts from the base. It is not a corrective round and uses none of that
// budget.
func TestRetryGivesTheRebuildTheRecordOfTheFailedAttempt(t *testing.T) {
	base, result := fmt.Sprintf("%040d", 1), fmt.Sprintf("%040d", 2)
	const fromBase = "This build starts again from the base commit: none of that attempt's changes are in the workspace"
	const onBranch = "This build continues on that attempt's branch: what it committed is in the workspace."
	flagged := func(rr *run.Run) {
		rr.GateResults = []run.GateResult{{Check: "spec_conformity", Passed: false}}
		rr.SpecConformityVerdicts = []run.ReviewVerdict{{Criterion: "2. rejects a negative amount", Verdict: "flagged", Detail: "no test covers it"}}
	}
	for name, tc := range map[string]struct {
		failed []string
		// shape changes the quarantined run after it is built as an
		// ordinary one of this request, built from the ticket's spec.
		shape       func(rr *run.Run)
		fromScratch bool
		wantRecord  string
		// wantOpens is how the record opens; wantDiffBase is the
		// -diff-base of a rebuild that continues on the attempt's branch,
		// "" for one that starts from the base.
		wantOpens, wantDiffBase string
	}{
		"a gate a build can fix":                            {failed: []string{"lint"}, wantRecord: "- `lint` failed (exit 2)", wantOpens: onBranch, wantDiffBase: base},
		"only a review failed, with a verdict":              {shape: flagged, wantRecord: "rejects a negative amount", wantOpens: onBranch, wantDiffBase: base},
		"the attempt ran on a branch with an earlier base":  {failed: []string{"lint"}, shape: func(rr *run.Run) { rr.DiffBaseSHA = fmt.Sprintf("%040d", 9) }, wantRecord: "- `lint` failed (exit 2)", wantOpens: onBranch, wantDiffBase: fmt.Sprintf("%040d", 9)},
		"-from scratch":                                     {failed: []string{"lint"}, fromScratch: true, wantRecord: "- `lint` failed (exit 2)", wantOpens: fromBase},
		"the attempt committed nothing":                     {failed: []string{"lint"}, shape: func(rr *run.Run) { rr.ResultSHA = rr.BaseSHA }, wantRecord: "- `lint` failed (exit 2)", wantOpens: fromBase},
		"the attempt recorded no branch":                    {failed: []string{"lint"}, shape: func(rr *run.Run) { rr.Branch = "" }, wantRecord: "- `lint` failed (exit 2)", wantOpens: fromBase},
		"a check a build is never told of":                  {failed: []string{"lint", "tests_added"}},
		"the run is another request's":                      {failed: []string{"lint"}, shape: func(rr *run.Run) { rr.RequestID = "some-other-request" }},
		"the ticket's spec changed since":                   {failed: []string{"lint"}, shape: func(rr *run.Run) { rr.SpecSHA256 = strings.Repeat("0", 64) }},
		"the handoff was changed after the run recorded it": {failed: []string{"lint"}, shape: func(rr *run.Run) { rr.HandoffSHA256 = strings.Repeat("0", 64) }},
	} {
		t.Run(name, func(t *testing.T) {
			dp := newTestDeps(t)
			dataDir, id := requestdrivertest.BuildingFixture(dp, t, 1)
			branch := "factoryd/" + id + "-001"
			var builds [][]string
			buildRunner := quarantinedThenAcceptedBuildRunner(t, dataDir, id, [2]string{base, result}, tc.failed, tc.shape, &builds)
			// No corrective budget: the first quarantine stands until a human retries.
			cfg := requestdriver.WorkerConfig{ReviewCorrectiveRounds: 0}
			drive := func() {
				t.Helper()
				if err := driveRequests(dp, context.Background(), dataDir, cfg, requestdrivertest.FailingSpecDraftRunner(t), requestdrivertest.FailingPlanTicketsRunner(t), requestdrivertest.FailingOracleDraftRunner(t), buildRunner); err != nil {
					t.Fatalf("driveRequests: %v", err)
				}
			}
			drive()
			quarantined, err := request.Load(dataDir, id)
			if err != nil || quarantined.State != request.StateQuarantined {
				t.Fatalf("after the first build: %v, %v, want quarantined", quarantined.State, err)
			}
			if got := requestdrivertest.ArgValue(builds[0], "-earlier-attempt"); got != "" {
				t.Fatalf("the ticket's first build was given a record: %q", got)
			}
			if handled, err := retryRequestFrom(dp, dataDir, quarantined, "", tc.fromScratch, time.Now()); err != nil || !handled {
				t.Fatalf("retryRequest: handled %v, err %v", handled, err)
			}
			drive()
			if len(builds) != 2 {
				t.Fatalf("builds = %d, want the first and the retry's", len(builds))
			}
			rebuild := builds[1]
			wantBranch := ""
			if tc.wantDiffBase != "" {
				wantBranch = branch
			}
			if got := requestdrivertest.ArgValue(rebuild, "-on-branch"); got != wantBranch {
				t.Errorf("the rebuild runs -on-branch %q, want %q", got, wantBranch)
			}
			if got := requestdrivertest.ArgValue(rebuild, "-diff-base"); got != tc.wantDiffBase {
				t.Errorf("the rebuild's -diff-base = %q, want %q", got, tc.wantDiffBase)
			}
			recordPath := requestdrivertest.ArgValue(rebuild, "-earlier-attempt")
			if (recordPath != "") != (tc.wantRecord != "") {
				t.Fatalf("-earlier-attempt = %q, want a record: %v", recordPath, tc.wantRecord != "")
			}
			if tc.wantRecord != "" {
				record, err := os.ReadFile(recordPath)
				if err != nil || !strings.Contains(string(record), tc.wantRecord) {
					t.Errorf("the record = %q, %v, want %q", record, err, tc.wantRecord)
				}
				if !strings.HasPrefix(string(record), tc.wantOpens) {
					t.Errorf("the record opens:\n%.200s\nwant: %s", record, tc.wantOpens)
				}
			}
			loaded, err := request.Load(dataDir, id)
			if err != nil {
				t.Fatal(err)
			}
			if len(loaded.Tickets[0].Rounds) != 0 {
				t.Errorf("Rounds = %+v, want none: a retry is not a corrective round", loaded.Tickets[0].Rounds)
			}
			if loaded.RetryFromScratch {
				t.Error("the retry's -from scratch outlived the build state it was made for")
			}
		})
	}
}

// TestAResumeFromScratchAfterALostRetryStartsFromTheBase: a retry that was
// to continue on the attempt's branch is lost before its run exists; the
// only decision left is `resume -from scratch`, and the build it starts is
// from the base (told so by its record), never on that branch.
func TestAResumeFromScratchAfterALostRetryStartsFromTheBase(t *testing.T) {
	dp := newTestDeps(t)
	dataDir, id := requestdrivertest.BuildingFixture(dp, t, 1)
	var builds [][]string
	buildRunner := quarantinedThenAcceptedBuildRunner(t, dataDir, id, [2]string{fmt.Sprintf("%040d", 1), fmt.Sprintf("%040d", 2)}, []string{"lint"}, nil, &builds)
	cfg := requestdriver.WorkerConfig{Resume: requestdriver.ResumeGate{Preconditions: &requestdrivertest.FakeResumePreconditions{T: t, OK: true}}}
	drive := func() {
		t.Helper()
		if err := driveRequests(dp, context.Background(), dataDir, cfg, requestdrivertest.FailingSpecDraftRunner(t), requestdrivertest.FailingPlanTicketsRunner(t), requestdrivertest.FailingOracleDraftRunner(t), buildRunner); err != nil {
			t.Fatalf("driveRequests: %v", err)
		}
	}
	drive()
	quarantined, err := request.Load(dataDir, id)
	if err != nil || quarantined.State != request.StateQuarantined {
		t.Fatalf("after the first build: %v, %v, want quarantined", quarantined.State, err)
	}
	if handled, err := retryRequest(dp, dataDir, quarantined, "", time.Now()); err != nil || !handled {
		t.Fatalf("retryRequest: handled %v, err %v", handled, err)
	}
	// The worker is lost before the rebuild's run exists: no kept run.
	lost, err := request.Load(dataDir, id)
	if err != nil {
		t.Fatal(err)
	}
	if err := lost.EnterResumeReview(request.StateBuilding, "", time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := lost.ResumeDecide(request.ResumeScratch, "alice", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := lost.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	drive()
	if len(builds) != 2 {
		t.Fatalf("builds = %d, want the first and the one the resume started", len(builds))
	}
	if requestdrivertest.HasFlag(builds[1], "-on-branch") || requestdrivertest.HasFlag(builds[1], "-diff-base") {
		t.Fatalf("resume -from scratch built on the attempt's branch: %v", builds[1])
	}
	assertRecordOf(t, requestdrivertest.ArgValue(builds[1], "-earlier-attempt"), "This build starts again from the base commit: none of that attempt's changes are in the workspace", id+"-001")
}

// TestARetryOfALaterTicketContinuesOnItsBranchWithoutThePriorRun: ticket 2
// of a request is quarantined and retried; its rebuild continues on that
// attempt's branch, which already holds ticket 1's work, so it names no
// -prior-run (the two are refused together).
func TestARetryOfALaterTicketContinuesOnItsBranchWithoutThePriorRun(t *testing.T) {
	dp := newTestDeps(t)
	dataDir, id := requestdrivertest.BuildingFixture(dp, t, 2)
	base, result := fmt.Sprintf("%040d", 1), fmt.Sprintf("%040d", 2)
	branch2 := "factoryd/" + id + "-002"
	var builds [][]string
	buildRunner := func(ctx context.Context, args []string, onReady func(*run.Run)) error {
		builds = append(builds, args)
		ticket := requestdrivertest.ArgValue(args, "-ticket")
		onReady(&run.Run{ID: ticket})
		if ticket == id+"-001" || len(builds) > 2 {
			return (&run.Run{ID: ticket, Ticket: ticket, State: run.StateAccepted, BaseSHA: base, Branch: "factoryd/" + ticket, RequestID: id}).Save(dataDir)
		}
		rr := requestdrivertest.QuarantinedOn(t, dataDir, ticket, branch2, base, result, "lint")
		rr.RequestID = id
		sum, err := evidence.SHA256File(requestdrivertest.ArgValue(args, "-spec"))
		if err != nil {
			t.Fatal(err)
		}
		rr.SpecSHA256 = sum
		if err := handoff.Sync(rr, dataDir); err != nil {
			t.Fatal(err)
		}
		return rr.Save(dataDir)
	}
	drive := func() *request.Request {
		t.Helper()
		if err := driveRequests(dp, context.Background(), dataDir, requestdriver.WorkerConfig{}, requestdrivertest.FailingSpecDraftRunner(t), requestdrivertest.FailingPlanTicketsRunner(t), requestdrivertest.FailingOracleDraftRunner(t), buildRunner); err != nil {
			t.Fatalf("driveRequests: %v", err)
		}
		r, err := request.Load(dataDir, id)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	r := drive()
	for i := 0; i < 3 && r.State == request.StateBuilding; i++ {
		r = drive()
	}
	if r.State != request.StateQuarantined || len(builds) != 2 {
		t.Fatalf("state %s after %d builds, want ticket 1 accepted and ticket 2 quarantined", r.State, len(builds))
	}
	if got := requestdrivertest.ArgValue(builds[1], "-prior-run"); got != id+"-001" {
		t.Fatalf("ticket 2's first build: -prior-run = %q, want ticket 1's run", got)
	}
	if handled, err := retryRequest(dp, dataDir, r, "", time.Now()); err != nil || !handled {
		t.Fatalf("retryRequest: handled %v, err %v", handled, err)
	}
	drive()
	if len(builds) != 3 {
		t.Fatalf("builds = %d, want the retry's rebuild", len(builds))
	}
	if got := requestdrivertest.ArgValue(builds[2], "-on-branch"); got != branch2 {
		t.Errorf("-on-branch = %q, want %q", got, branch2)
	}
	if requestdrivertest.HasFlag(builds[2], "-prior-run") {
		t.Errorf("the rebuild names -prior-run %q beside -on-branch", requestdrivertest.ArgValue(builds[2], "-prior-run"))
	}
}

// quarantinedThenAcceptedBuildRunner builds a ticket twice: the first run
// is quarantined on failed (with a handoff, unless shape set a hash that
// disowns it) and shaped by shape; the second is accepted. shas are the
// first build's base and result commits.
func quarantinedThenAcceptedBuildRunner(t *testing.T, dataDir, id string, shas [2]string, failed []string, shape func(*run.Run), builds *[][]string) requestdriver.TicketRunner {
	t.Helper()
	branch := "factoryd/" + id + "-001"
	return func(ctx context.Context, args []string, onReady func(*run.Run)) error {
		*builds = append(*builds, args)
		ticket := requestdrivertest.ArgValue(args, "-ticket")
		if onReady != nil {
			onReady(&run.Run{ID: ticket})
		}
		if len(*builds) > 1 {
			return (&run.Run{ID: ticket, Ticket: ticket, State: run.StateAccepted, Branch: branch, RequestID: id}).Save(dataDir)
		}
		rr := requestdrivertest.QuarantinedOn(t, dataDir, ticket, branch, shas[0], shas[1], failed...)
		rr.RequestID = id
		sum, err := evidence.SHA256File(requestdrivertest.ArgValue(args, "-spec"))
		if err != nil {
			t.Fatal(err)
		}
		rr.SpecSHA256 = sum
		if shape != nil {
			shape(rr)
		}
		if rr.HandoffSHA256 != strings.Repeat("0", 64) {
			if err := handoff.Sync(rr, dataDir); err != nil {
				t.Fatal(err)
			}
		}
		return rr.Save(dataDir)
	}
}

// TestABuildThatFollowsAnUnfinishedOneIsGivenTheSameRecord: a retry's
// rebuild is given the record of the quarantined attempt and is then lost.
// The build that follows it, resumed in its kept worktree or started again
// from the base, is given the record of that same attempt, loaded again
// from that attempt's handoff, and each run carries which run its record is
// of. A lost build that was given no record passes none on.
func TestABuildThatFollowsAnUnfinishedOneIsGivenTheSameRecord(t *testing.T) {
	base, result := fmt.Sprintf("%040d", 1), fmt.Sprintf("%040d", 2)
	for name, tc := range map[string]struct {
		verb string
		// onAttemptsBranch: the lost build ran on the quarantined
		// attempt's branch, not on a new one from the base.
		onAttemptsBranch bool
		// nothingCommitted: the quarantined attempt's result commit is its
		// base, as when its verification never passed.
		nothingCommitted bool
		// withdrawn removes the quarantined attempt's handoff before the
		// build that follows the lost one starts.
		withdrawn  bool
		wantOpens  string
		wantResume bool
	}{
		"resumed in the kept worktree, which started from the base": {
			verb: request.ResumeRound, wantResume: true,
			wantOpens: "This build resumes a later build of this ticket that was interrupted. The record below is of the attempt before that one, which finished and failed its checks. The interrupted build had started again from the base commit: that attempt's commit is not in the workspace",
		},
		"resumed in the kept worktree, which is on the attempt's branch": {
			verb: request.ResumeRound, onAttemptsBranch: true, wantResume: true,
			wantOpens: "This build resumes a later build of this ticket that was interrupted. The record below is of the attempt before that one, which finished and failed its checks. The interrupted build ran on that attempt's branch: what that attempt committed is in the workspace",
		},
		"resumed in the kept worktree, after an attempt that committed nothing": {
			verb: request.ResumeRound, nothingCommitted: true, wantResume: true,
			wantOpens: "This build resumes a later build of this ticket that was interrupted. The record below is of the attempt before that one, which finished and failed its checks. The interrupted build had started again from the base commit: that attempt's commit is not in the workspace",
		},
		"started again from the base": {
			verb:      request.ResumeScratch,
			wantOpens: "This build starts again from the base commit: none of that attempt's changes are in the workspace",
		},
		"the attempt's handoff is gone by the time of the resume": {
			verb: request.ResumeRound, wantResume: true, withdrawn: true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			dp := newTestDeps(t)
			dataDir, id := requestdrivertest.BuildingFixture(dp, t, 1)
			repoDir := newFixtureRepo(t)
			var builds [][]string
			var started []*run.Run
			attemptResult := result
			if tc.nothingCommitted {
				attemptResult = base
			}
			buildRunner := quarantinedThenLostBuildRunner(t, dataDir, id, repoDir, [2]string{base, attemptResult}, tc.onAttemptsBranch, &builds, &started)
			cfg := requestdriver.WorkerConfig{Resume: requestdriver.ResumeGate{Preconditions: &requestdrivertest.FakeResumePreconditions{T: t, OK: true}}}
			drive := func() *request.Request {
				t.Helper()
				if err := driveRequests(dp, context.Background(), dataDir, cfg, requestdrivertest.FailingSpecDraftRunner(t), requestdrivertest.FailingPlanTicketsRunner(t), requestdrivertest.FailingOracleDraftRunner(t), buildRunner); err != nil {
					t.Fatalf("driveRequests: %v", err)
				}
				r, err := request.Load(dataDir, id)
				if err != nil {
					t.Fatal(err)
				}
				return r
			}
			quarantined := drive()
			if quarantined.State != request.StateQuarantined {
				t.Fatalf("after the first build: %s, want quarantined", quarantined.State)
			}
			attemptID := started[0].ID
			if started[0].EarlierAttemptOf != "" {
				t.Fatalf("the first build carries a record's source: %q", started[0].EarlierAttemptOf)
			}
			if handled, err := retryRequest(dp, dataDir, quarantined, "", time.Now()); err != nil || !handled {
				t.Fatalf("retryRequest: handled %v, err %v", handled, err)
			}
			lost := drive()
			if lost.State != request.StateResumeReview {
				t.Fatalf("after the lost rebuild: %s, want resume_review", lost.State)
			}
			if requestdrivertest.ArgValue(builds[1], "-earlier-attempt") == "" || started[1].EarlierAttemptOf != attemptID {
				t.Fatalf("the rebuild: -earlier-attempt %q, carries %q, want the record of %s", requestdrivertest.ArgValue(builds[1], "-earlier-attempt"), started[1].EarlierAttemptOf, attemptID)
			}
			if tc.withdrawn {
				if err := os.Remove(filepath.Join(run.Dir(dataDir, attemptID), "handoff.json")); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := lost.ResumeDecide(tc.verb, "alice", time.Now()); err != nil {
				t.Fatal(err)
			}
			if err := lost.Save(dataDir); err != nil {
				t.Fatal(err)
			}
			drive()
			if len(builds) != 3 {
				t.Fatalf("builds = %d, want three", len(builds))
			}
			following := builds[2]
			if got := requestdrivertest.ArgValue(following, "-resume-worktree-of"); (got != "") != tc.wantResume {
				t.Fatalf("-resume-worktree-of = %q, want a resume: %v", got, tc.wantResume)
			}
			recordPath := requestdrivertest.ArgValue(following, "-earlier-attempt")
			if tc.wantOpens == "" {
				if recordPath != "" || started[2].EarlierAttemptOf != "" {
					t.Fatalf("-earlier-attempt = %q, carries %q, want no record: the attempt no longer vouches for one", recordPath, started[2].EarlierAttemptOf)
				}
				return
			}
			if recordPath == requestdrivertest.ArgValue(builds[1], "-earlier-attempt") && tc.wantResume {
				t.Errorf("the resumed build was handed the lost build's own record file %q, want one written for it", recordPath)
			}
			assertRecordOf(t, recordPath, tc.wantOpens, attemptID)
			if started[2].EarlierAttemptOf != attemptID {
				t.Errorf("the following build carries %q, want %s", started[2].EarlierAttemptOf, attemptID)
			}
		})
	}
}

// quarantinedThenLostBuildRunner builds a ticket three times, each as its
// own run (<ticket>-build<n>): the first is quarantined by lint with a
// handoff, the second is lost with its worktree kept, the third accepted.
// shas are the first build's base and result commits. The first build
// committed nothing new when they are equal. The lost build is on the first
// one's branch when onAttemptsBranch, else on its own. Each call's argv and
// the run the driver was shown are appended.
func quarantinedThenLostBuildRunner(t *testing.T, dataDir, id, repoDir string, shas [2]string, onAttemptsBranch bool, builds *[][]string, started *[]*run.Run) requestdriver.TicketRunner {
	t.Helper()
	branch := "factoryd/" + id + "-001"
	return func(ctx context.Context, args []string, onReady func(*run.Run)) error {
		*builds = append(*builds, args)
		n := len(*builds)
		runID := fmt.Sprintf("%s-build%d", requestdrivertest.ArgValue(args, "-ticket"), n)
		current := &run.Run{ID: runID}
		onReady(current)
		*started = append(*started, current)
		switch n {
		case 1:
			rr := requestdrivertest.QuarantinedOn(t, dataDir, runID, branch, shas[0], shas[1], "lint")
			rr.RequestID = id
			sum, err := evidence.SHA256File(requestdrivertest.ArgValue(args, "-spec"))
			if err != nil {
				t.Fatal(err)
			}
			rr.SpecSHA256 = sum
			if err := handoff.Sync(rr, dataDir); err != nil {
				t.Fatal(err)
			}
			return rr.Save(dataDir)
		case 2:
			marker := requestdrivertest.IsolationMarker(t, repoDir, dataDir, runID, "temporal")
			lostBranch := marker.Branch
			if onAttemptsBranch {
				lostBranch = branch
			}
			// A rebuild from the base has the first build's base as its
			// own, which is also that build's result when it committed
			// nothing: the two cases must not be told apart by commit id.
			return (&run.Run{
				ID: runID, Ticket: runID, State: run.StateHalted, HaltConfirmed: true, KeptForResume: true,
				RequestID: id, EarlierAttemptOf: current.EarlierAttemptOf, BaseSHA: shas[0],
				ProjectPath: repoDir, WorkspacePath: marker.WorktreePath, Branch: lostBranch,
			}).Save(dataDir)
		default:
			return (&run.Run{ID: runID, Ticket: runID, State: run.StateAccepted, Branch: branch, RequestID: id}).Save(dataDir)
		}
	}
}

// assertRecordOf fails unless the file at path opens with opens and is the
// record of attemptID's failed lint.
func assertRecordOf(t *testing.T, path, opens, attemptID string) {
	t.Helper()
	record, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the record: %v", err)
	}
	if !strings.HasPrefix(string(record), opens) {
		t.Errorf("the record opens:\n%.300s\nwant it to open: %s", record, opens)
	}
	if !strings.Contains(string(record), "(run "+attemptID+")") || !strings.Contains(string(record), "- `lint` failed (exit 2)") {
		t.Errorf("the record is not of the quarantined attempt %s:\n%s", attemptID, record)
	}
}
