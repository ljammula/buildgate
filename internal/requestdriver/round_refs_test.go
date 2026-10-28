package requestdriver_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"buildgate/internal/forge"
	"buildgate/internal/request"
	"buildgate/internal/requestdriver"
	"buildgate/internal/requestdriver/requestdrivertest"
	"buildgate/internal/run"
	"buildgate/internal/workspace"
)

func sha40(n int) string { return fmt.Sprintf("%040x", n) }

// quarantinedWithCommit is a build that quarantined on failedCheck after
// committing resultSHA on top of baseSHA.
func quarantinedWithCommit(t *testing.T, dataDir string, args []string, baseSHA, resultSHA, failedCheck string) error {
	t.Helper()
	return quarantinedRun(t, dataDir, requestdrivertest.ArgValue(args, "-ticket"), baseSHA, resultSHA, failedCheck)
}

func quarantinedRun(t *testing.T, dataDir, runID, baseSHA, resultSHA, failedCheck string) error {
	t.Helper()
	rr := &run.Run{
		ID: runID, State: run.StateQuarantined, HaltError: "gate failed: " + failedCheck,
		Branch: "factoryd/" + runID, BaseSHA: baseSHA, ResultSHA: resultSHA,
		GateResults: []run.GateResult{{Check: "canonical_verify", Passed: true}, {Check: failedCheck, Passed: false}},
	}
	return rr.Save(dataDir)
}

// refRecorder is the fake's CreateRef and CreateBranchAt: the refs written and
// the branches created, in order.
type refRecorder struct {
	refs     map[string]string
	order    []string
	branches map[string]string
}

func recordRefs(dp *fakeDeps) *refRecorder {
	rec := &refRecorder{refs: map[string]string{}, branches: map[string]string{}}
	dp.createRefFn = func(dir, ref, sha string) error {
		if have, ok := rec.refs[ref]; ok {
			if have == sha {
				return nil
			}
			return workspace.ErrRefExists
		}
		rec.refs[ref] = sha
		rec.order = append(rec.order, ref)
		return nil
	}
	dp.createBranchAtFn = func(dir, branch, sha string) error {
		if have, ok := rec.branches[branch]; ok && have != sha {
			return workspace.ErrBranchExists
		}
		rec.branches[branch] = sha
		return nil
	}
	return rec
}

func TestEachTicketRoundGetsARefAndLedgerEntry(t *testing.T) {
	dp := newFakeDeps(t)
	rec := recordRefs(dp)
	dataDir, id := requestdrivertest.BuildingFixture(dp, t, 1)
	base, first, second := sha40(1), sha40(0xa1), sha40(0xa2)
	buildRunner := func(ctx context.Context, args []string, onReady func(*run.Run)) error {
		runID := requestdrivertest.ArgValue(args, "-ticket")
		onReady(&run.Run{ID: runID})
		return requestdrivertest.ReproductionRefusedOn(t, dataDir, runID, "factoryd/"+runID, base, first).Save(dataDir)
	}
	requestdrivertest.StubReviewCorrectiveRunner(t, dataDir, func(dataDir, roundRunID string) *run.Run {
		return &run.Run{
			ID: roundRunID, State: run.StateQuarantined, HaltError: "gate failed: canonical_verify", Branch: "factoryd/x",
			BaseSHA: first, DiffBaseSHA: base, ResultSHA: second,
			GateResults: []run.GateResult{{Check: "canonical_verify", Passed: false}},
		}
	})
	cfg := requestdriver.WorkerConfig{ReviewCorrectiveRounds: 1}
	if err := requestdrivertest.DriveRequests(dp, context.Background(), dataDir, cfg, requestdrivertest.FailingSpecDraftRunner(t), requestdrivertest.FailingPlanTicketsRunner(t), buildRunner); err != nil {
		t.Fatalf("driveRequests: %v", err)
	}
	loaded := requestdrivertest.LoadRequest(t, dataDir, id)
	tags := loaded.Tickets[0].RoundTags
	if len(tags) != 2 {
		t.Fatalf("RoundTags = %+v, want the build and the corrective round", tags)
	}
	want := []struct {
		kind, sha, outcome string
		failed             []string
	}{
		{request.RoundTagBuild, first, "quarantined", []string{"review_reproduction"}},
		{request.RoundTagCorrective, second, "quarantined", []string{"canonical_verify"}},
	}
	for i, w := range want {
		got := tags[i]
		wantRef := fmt.Sprintf("refs/buildgate/rounds/%s/t1/r%d", id, i+1)
		if got.N != i+1 || got.Ref != wantRef || got.Kind != w.kind || got.SHA != w.sha || got.Outcome != w.outcome || !reflect.DeepEqual(got.FailedChecks, w.failed) {
			t.Errorf("RoundTags[%d] = %+v, want n=%d ref=%s kind=%s sha=%s outcome=%s failed=%v", i, got, i+1, wantRef, w.kind, w.sha, w.outcome, w.failed)
		}
		if rec.refs[wantRef] != w.sha {
			t.Errorf("ref %s = %q, want %s", wantRef, rec.refs[wantRef], w.sha)
		}
	}
	if tags[1].DiffBaseSHA != base || tags[1].BaseSHA != first {
		t.Errorf("round 2 bases = %q/%q, want %q/%q", tags[1].BaseSHA, tags[1].DiffBaseSHA, first, base)
	}
}

func TestEachPRReviewRoundGetsARefAndLedgerEntry(t *testing.T) {
	dp := newFakeDeps(t)
	rec := recordRefs(dp)
	threads := []forge.Thread{{ID: "thread-1", Path: "a.go", Line: 1, Author: "dave", Body: "needs work", CommentID: 444}}
	r, dataDir := requestdrivertest.StubPRReviewTestFixture(t, 1)
	stubPRReviewDeps(dp, t, forge.ReviewState{State: "OPEN", BlocksReadyThreads: threads, ActionableThreads: threads}, nil)
	roundSHA := sha40(0xbeef)
	requestdriver.PrReviewCorrectiveRunner = func(ctx context.Context, args []string, onReady func(*run.Run)) error {
		runID := requestdrivertest.ArgValue(args, "-ticket")
		rr := &run.Run{ID: runID, State: run.StateQuarantined, BaseSHA: sha40(7), ResultSHA: roundSHA, GateResults: []run.GateResult{{Check: "code_review", Passed: false}}}
		if err := rr.Save(dataDir); err != nil {
			return err
		}
		return errors.New("run quarantined: code_review")
	}
	dp.replyToReviewCommentFn = func(context.Context, string, int64, string) error { return nil }
	cfg := requestdriver.WorkerConfig{PrPollInterval: time.Minute, MaxReviewRounds: 3}
	if err := requestdriver.AdvancePRReview(dp, context.Background(), dataDir, r, cfg, time.Now()); err != nil {
		t.Fatalf("AdvancePRReview: %v", err)
	}
	tags := r.Tickets[0].RoundTags
	if len(tags) != 1 || tags[0].Kind != request.RoundTagPRReview || tags[0].SHA != roundSHA || tags[0].Outcome != "quarantined" || !reflect.DeepEqual(tags[0].FailedChecks, []string{"code_review"}) {
		t.Fatalf("RoundTags = %+v, want one quarantined pr_review round at %s failing code_review", tags, roundSHA)
	}
	if rec.refs[tags[0].Ref] != roundSHA || !strings.HasPrefix(tags[0].Ref, "refs/buildgate/rounds/req-1/t1/r") {
		t.Errorf("refs = %v, want %s under refs/buildgate/rounds/req-1/t1/", rec.refs, tags[0].Ref)
	}
}

func TestARoundWithNoCommitOfItsOwnIsNotTagged(t *testing.T) {
	dp := newFakeDeps(t)
	rec := recordRefs(dp)
	dataDir, id := requestdrivertest.BuildingFixture(dp, t, 1)
	buildRunner := func(ctx context.Context, args []string, onReady func(*run.Run)) error {
		onReady(&run.Run{ID: requestdrivertest.ArgValue(args, "-ticket")})
		return quarantinedWithCommit(t, dataDir, args, sha40(1), sha40(1), "canonical_verify")
	}
	if err := requestdrivertest.DriveRequests(dp, context.Background(), dataDir, requestdriver.WorkerConfig{}, requestdrivertest.FailingSpecDraftRunner(t), requestdrivertest.FailingPlanTicketsRunner(t), buildRunner); err != nil {
		t.Fatalf("driveRequests: %v", err)
	}
	if tags := requestdrivertest.LoadRequest(t, dataDir, id).Tickets[0].RoundTags; len(tags) != 0 || len(rec.refs) != 0 {
		t.Errorf("RoundTags = %+v, refs = %v; want none for a round whose result equals its base", tags, rec.refs)
	}
}

// quarantinedTwoRounds drives a building request to a quarantine after its first
// build (round 1) and returns its ids and the recorder.
func quarantinedAfterOneRound(t *testing.T, dp *fakeDeps) (dataDir, id string, rec *refRecorder) {
	t.Helper()
	rec = recordRefs(dp)
	dataDir, id = requestdrivertest.BuildingFixture(dp, t, 1)
	buildRunner := func(ctx context.Context, args []string, onReady func(*run.Run)) error {
		onReady(&run.Run{ID: requestdrivertest.ArgValue(args, "-ticket")})
		return quarantinedWithCommit(t, dataDir, args, sha40(1), sha40(0xa1), "canonical_verify")
	}
	if err := requestdrivertest.DriveRequests(dp, context.Background(), dataDir, requestdriver.WorkerConfig{}, requestdrivertest.FailingSpecDraftRunner(t), requestdrivertest.FailingPlanTicketsRunner(t), buildRunner); err != nil {
		t.Fatalf("driveRequests: %v", err)
	}
	if got := requestdrivertest.LoadRequest(t, dataDir, id); got.State != request.StateQuarantined || len(got.Tickets[0].RoundTags) != 1 {
		t.Fatalf("fixture: state %q with %d round tags, want quarantined with 1", got.State, len(got.Tickets[0].RoundTags))
	}
	return dataDir, id, rec
}

func TestRetryFromRoundNBuildsOnABranchAtThatRound(t *testing.T) {
	dp := newFakeDeps(t)
	dataDir, id, rec := quarantinedAfterOneRound(t, dp)
	if _, err := request.RetryFromRound(dataDir, id, "alice", "", 1, time.Now(), nil); err != nil {
		t.Fatalf("RetryFromRound: %v", err)
	}
	var args []string
	buildRunner := func(ctx context.Context, a []string, onReady func(*run.Run)) error {
		args = a
		runID := requestdrivertest.ArgValue(a, "-ticket") + "-rebuilt"
		onReady(&run.Run{ID: runID})
		return quarantinedRun(t, dataDir, runID, sha40(0xa1), sha40(0xa3), "canonical_verify")
	}
	if err := requestdrivertest.DriveRequests(dp, context.Background(), dataDir, requestdriver.WorkerConfig{}, requestdrivertest.FailingSpecDraftRunner(t), requestdrivertest.FailingPlanTicketsRunner(t), buildRunner); err != nil {
		t.Fatalf("driveRequests: %v", err)
	}
	wantBranch := "buildgate/" + id + "/t1-from-r1"
	if got := requestdrivertest.ArgValue(args, "-on-branch"); got != wantBranch {
		t.Errorf("-on-branch = %q, want %q", got, wantBranch)
	}
	if got := requestdrivertest.ArgValue(args, "-diff-base"); got != sha40(1) {
		t.Errorf("-diff-base = %q, want the round's base %q", got, sha40(1))
	}
	if rec.branches[wantBranch] != sha40(0xa1) {
		t.Errorf("branches = %v, want %s at round 1's commit %s", rec.branches, wantBranch, sha40(0xa1))
	}
	if requestdrivertest.HasFlag(args, "-prior-run") {
		t.Errorf("argv %v carries -prior-run: the round-1 branch already holds the earlier tickets' work", args)
	}
	loaded := requestdrivertest.LoadRequest(t, dataDir, id)
	if loaded.RetryFromRound != 0 {
		t.Errorf("RetryFromRound = %d after the rebuild ended, want it dropped", loaded.RetryFromRound)
	}
	if tags := loaded.Tickets[0].RoundTags; len(tags) != 2 || tags[1].SHA != sha40(0xa3) {
		t.Errorf("RoundTags = %+v, want the rebuild recorded as round 2", tags)
	}
}

func TestRetryFromRoundTakesANewBranchWhenAnEarlierOneMovedOn(t *testing.T) {
	dp := newFakeDeps(t)
	dataDir, id, rec := quarantinedAfterOneRound(t, dp)
	rec.branches["buildgate/"+id+"/t1-from-r1"] = sha40(0xff)
	if _, err := request.RetryFromRound(dataDir, id, "alice", "", 1, time.Now(), nil); err != nil {
		t.Fatalf("RetryFromRound: %v", err)
	}
	var args []string
	buildRunner := func(ctx context.Context, a []string, onReady func(*run.Run)) error {
		args = a
		runID := requestdrivertest.ArgValue(a, "-ticket") + "-rebuilt"
		onReady(&run.Run{ID: runID})
		return quarantinedRun(t, dataDir, runID, sha40(0xa1), sha40(0xa3), "canonical_verify")
	}
	if err := requestdrivertest.DriveRequests(dp, context.Background(), dataDir, requestdriver.WorkerConfig{}, requestdrivertest.FailingSpecDraftRunner(t), requestdrivertest.FailingPlanTicketsRunner(t), buildRunner); err != nil {
		t.Fatalf("driveRequests: %v", err)
	}
	if got, want := requestdrivertest.ArgValue(args, "-on-branch"), "buildgate/"+id+"/t1-from-r1-2"; got != want {
		t.Errorf("-on-branch = %q, want %q", got, want)
	}
}

func TestRetryFromRoundRefusedOnAPushedTicket(t *testing.T) {
	dp := newFakeDeps(t)
	dataDir, id, _ := quarantinedAfterOneRound(t, dp)
	r := requestdrivertest.LoadRequest(t, dataDir, id)
	r.Tickets[0].PRURL = "https://github.com/acme/app/pull/1"
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	_, err := request.RetryFromRound(dataDir, id, "alice", "", 1, time.Now(), nil)
	if !errors.Is(err, request.ErrIllegalTransition) || !strings.Contains(err.Error(), "force-push") || !strings.Contains(err.Error(), "close the pull request") {
		t.Fatalf("RetryFromRound on a pushed ticket = %v, want a refusal naming the force-push and closing the pull request", err)
	}
	if got := requestdrivertest.LoadRequest(t, dataDir, id); got.State != request.StateQuarantined || got.RetryFromRound != 0 {
		t.Errorf("request after the refusal: state %q, RetryFromRound %d; want it untouched", got.State, got.RetryFromRound)
	}
}

func TestRetryFromAnUnknownRoundIsRefused(t *testing.T) {
	dp := newFakeDeps(t)
	dataDir, id, _ := quarantinedAfterOneRound(t, dp)
	_, err := request.RetryFromRound(dataDir, id, "alice", "", 9, time.Now(), nil)
	if !errors.Is(err, request.ErrIllegalTransition) || !strings.Contains(err.Error(), "no round 9") || !strings.Contains(err.Error(), "rounds: 1") {
		t.Fatalf("RetryFromRound(9) = %v, want a refusal naming the rounds there are", err)
	}
}
