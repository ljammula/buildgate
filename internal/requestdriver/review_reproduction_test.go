package requestdriver_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"buildgate/internal/evidence"
	"buildgate/internal/request"
	"buildgate/internal/requestdriver"
	"buildgate/internal/requestdriver/requestdrivertest"
	"buildgate/internal/run"
	"buildgate/internal/unresolved"
)

// reproducedLine is the one line a later build is told of
// requestdrivertest.ReproductionRefusedOn's blocking finding.
var reproducedLine = fmt.Sprintf("- \"high\" %q (spec line %d): the factory's run of `%s` failed on the result: %q fail\n",
	requestdrivertest.ReproductionSummary, requestdrivertest.ReproductionSpecLine, requestdrivertest.ReproductionPath, requestdrivertest.ReproductionTestID)

// neverTold is what the reviewer wrote that no build is ever given (SC-018):
// the blocking finding's failure scenario, the reproduction's content, and
// a finding that is advice.
var neverTold = []string{"SCENARIO-NEVER-TOLD", "CONTENT-NEVER-TOLD", "ADVICE-NEVER-TOLD", "nothing ran it"}

// readRecheckManifest reads the -recheck-reproductions manifest of args.
func readRecheckManifest(t *testing.T, args []string) run.RecheckManifest {
	t.Helper()
	path := requestdrivertest.ArgValue(args, "-recheck-reproductions")
	if path == "" {
		t.Fatalf("the build has no -recheck-reproductions: %v", args)
	}
	var m run.RecheckManifest
	if err := json.Unmarshal(mustReadFile(t, path), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// assertToldOnlyTheLine checks a build's inputs: its -earlier-attempt record
// (when wantRecord) holds the finding's one line, and no file the build is
// given (-spec, the record) holds anything the reviewer wrote beyond it; no
// argument names the reproduction's held copy, which the build never mounts.
func assertToldOnlyTheLine(t *testing.T, dataDir, quarantinedID string, args []string, wantRecord bool) {
	t.Helper()
	record := requestdrivertest.ArgValue(args, "-earlier-attempt")
	if wantRecord {
		if got := string(mustReadFile(t, record)); !strings.Contains(got, "## Reproduced review findings\n\n"+reproducedLine) {
			t.Errorf("the record lacks the finding's one line %q:\n%s", reproducedLine, got)
		}
	}
	for _, input := range []string{requestdrivertest.ArgValue(args, "-spec"), record} {
		if input == "" {
			continue
		}
		got := string(mustReadFile(t, input))
		for _, leaked := range neverTold {
			if strings.Contains(got, leaked) {
				t.Errorf("%s carries %q:\n%s", input, leaked, got)
			}
		}
	}
	held := run.ReviewReproductionDir(run.Dir(dataDir, quarantinedID))
	for _, a := range args {
		if strings.Contains(a, held) || strings.Contains(a, requestdrivertest.ReproductionContent) {
			t.Errorf("an argument of the build names the held reproduction: %q", a)
		}
	}
}

// SC-022, P2.4: a ticket run a reviewer's reproduction refused is followed
// by one corrective build on its branch, told the finding in one line and
// given the held copy to recheck; accepted, it is the ticket's accepted run.
func TestReviewReproductionCorrectiveRoundAccepted(t *testing.T) {
	dp := newFakeDeps(t)
	dataDir, id := requestdrivertest.BuildingFixture(dp, t, 1)
	branch, baseSHA := "factoryd/"+id+"-001", fmt.Sprintf("%040d", 1)
	buildRunner := requestdrivertest.ReviewQuarantinedBuildRunner(t, dataDir, branch, baseSHA, "")
	calls, lastArgs := requestdrivertest.StubReviewCorrectiveRunner(t, dataDir, func(dataDir, roundRunID string) *run.Run {
		return &run.Run{ID: roundRunID, State: run.StateAccepted, Branch: branch, PullRequestURL: "https://github.com/acme/app/pull/999"}
	})
	cfg := requestdriver.WorkerConfig{ReviewCorrectiveRounds: 1, OpenPullRequest: true}
	if err := requestdrivertest.DriveRequests(dp, context.Background(), dataDir, cfg, requestdrivertest.FailingSpecDraftRunner(t), requestdrivertest.FailingPlanTicketsRunner(t), requestdrivertest.FailingOracleDraftRunner(t), buildRunner); err != nil {
		t.Fatalf("driveRequests: %v", err)
	}
	if *calls != 1 {
		t.Fatalf("corrective runner calls = %d, want 1", *calls)
	}
	args := *lastArgs
	for flag, want := range map[string]string{"-ticket": id + "-001-corrective1", "-on-branch": branch, "-diff-base": baseSHA} {
		if got := requestdrivertest.ArgValue(args, flag); got != want {
			t.Errorf("%s = %q, want %q", flag, got, want)
		}
	}
	for _, flag := range []string{"-review-reproduction", "-open-pull-request"} {
		if !requestdrivertest.HasFlag(args, flag) {
			t.Errorf("the corrective build lacks %s: %v", flag, args)
		}
	}
	if m := readRecheckManifest(t, args); len(m.Reproductions) != 1 || m.Reproductions[0].RunID != id+"-001" {
		t.Errorf("manifest = %+v, want the quarantined run's one held reproduction", m)
	}
	assertToldOnlyTheLine(t, dataDir, id+"-001", args, true)

	loaded := requestdrivertest.LoadRequest(t, dataDir, id)
	if loaded.State != request.StatePRReview {
		t.Fatalf("State = %q, want %q", loaded.State, request.StatePRReview)
	}
	tk := loaded.Tickets[0]
	if len(tk.Rounds) != 1 || tk.Rounds[0].Kind != request.CorrectiveRoundKind || tk.Rounds[0].Outcome != request.RoundAccepted {
		t.Fatalf("Rounds = %+v, want one accepted corrective round", tk.Rounds)
	}
	if tk.RunID != tk.Rounds[0].RunID || tk.PRURL != "https://github.com/acme/app/pull/999" {
		t.Errorf("ticket = run %q, PR %q, want the corrective round's", tk.RunID, tk.PRURL)
	}
}

// The corrective build's -recheck-reproductions manifest is the ticket's
// list: for the quarantined run's blocking finding, its held copy under that
// run's review-repro directory, with the hash, runner, test IDs and cited
// line the run recorded. A held copy that is gone or no longer matches its
// hash gives no corrective build: the request halts (HaltReproductionUnheld)
// with the operator's way out.
func TestReviewReproductionCorrectiveCarriesTheRecheckManifest(t *testing.T) {
	dp := newFakeDeps(t)
	dataDir, id := requestdrivertest.BuildingFixture(dp, t, 1)
	quarantined := requestdrivertest.ReproductionRefusedOn(t, dataDir, id+"-001", "factoryd/"+id+"-001", fmt.Sprintf("%040d", 1), fmt.Sprintf("%040d", 2))
	if err := quarantined.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	r := requestdrivertest.LoadRequest(t, dataDir, id)
	r.TicketIndex = 1
	// What the request driver records when it observes the refusal.
	r.Tickets[0].NoteReproductionRefusal(requestdrivertest.HeldEntry(t, dataDir, quarantined.ID), quarantined.ID)
	calls, lastArgs := requestdrivertest.StubReviewCorrectiveRunner(t, dataDir, func(dataDir, roundRunID string) *run.Run {
		return &run.Run{ID: roundRunID, State: run.StateAccepted, Branch: quarantined.Branch}
	})
	handled, err := requestdriver.TryCheckCorrectiveRound(dp, context.Background(), dataDir, r, &r.Tickets[0], quarantined, requestdriver.WorkerConfig{ReviewCorrectiveRounds: 1}, time.Now())
	if !handled || err != nil || *calls != 1 {
		t.Fatalf("handled=%v err=%v calls=%d, want one corrective build", handled, err, *calls)
	}
	m := readRecheckManifest(t, *lastArgs)
	if filepath.Dir(requestdrivertest.ArgValue(*lastArgs, "-recheck-reproductions")) != filepath.Join(request.Dir(dataDir, id), "rounds", id+"-001-corrective1") {
		t.Errorf("the manifest is not in the build's directory: %s", requestdrivertest.ArgValue(*lastArgs, "-recheck-reproductions"))
	}
	if want := requestdrivertest.HeldEntry(t, dataDir, quarantined.ID); len(m.Reproductions) != 1 || fmt.Sprint(m.Reproductions[0]) != fmt.Sprint(want) {
		t.Errorf("manifest = %+v, want %+v", m, want)
	}

	for name, spoil := range map[string]func(host string){
		"the held copy is gone":     func(host string) { _ = os.Remove(host) },
		"the held copy was changed": func(host string) { _ = os.Chmod(host, 0o600); _ = os.WriteFile(host, []byte("package sum\n"), 0o600) },
	} {
		t.Run(name, func(t *testing.T) {
			dp := newFakeDeps(t)
			dataDir, id := requestdrivertest.BuildingFixture(dp, t, 1)
			quarantined := requestdrivertest.ReproductionRefusedOn(t, dataDir, id+"-001", "factoryd/"+id+"-001", fmt.Sprintf("%040d", 1), fmt.Sprintf("%040d", 2))
			spoil(filepath.Join(run.ReviewReproductionDir(run.Dir(dataDir, quarantined.ID)), "0", filepath.FromSlash(requestdrivertest.ReproductionPath)))
			if err := quarantined.Save(dataDir); err != nil {
				t.Fatal(err)
			}
			r := requestdrivertest.LoadRequest(t, dataDir, id)
			r.TicketIndex = 1
			r.Tickets[0].NoteReproductionRefusal(requestdrivertest.HeldEntry(t, dataDir, quarantined.ID), quarantined.ID)
			calls, _ := requestdrivertest.StubReviewCorrectiveRunner(t, dataDir, func(dataDir, roundRunID string) *run.Run {
				return &run.Run{ID: roundRunID, State: run.StateAccepted}
			})
			handled, err := requestdriver.TryCheckCorrectiveRound(dp, context.Background(), dataDir, r, &r.Tickets[0], quarantined, requestdriver.WorkerConfig{ReviewCorrectiveRounds: 1}, time.Now())
			if !handled || err != nil || *calls != 0 {
				t.Errorf("handled=%v err=%v calls=%d, want the request halted and no corrective build", handled, err, *calls)
			}
			if r.State != request.StateHalted || r.HaltKind != request.HaltReproductionUnheld || !r.Tickets[0].Reproductions[0].Unheld || !strings.Contains(r.Error, "factoryd retry "+id) {
				t.Errorf("state %s kind %q error %q list %+v; want halted with the unheld kind, the entry marked and the retry named", r.State, r.HaltKind, r.Error, r.Tickets[0].Reproductions)
			}
		})
	}
}

// SC-022, P2.4: a corrective build whose recheck still fails on its result
// is refused again; the recheck runs once, so the request is quarantined
// with the review_reproduction check even with budget left, and the reason
// names the check and the reproduction's path.
func TestReviewReproductionStillFailingQuarantines(t *testing.T) {
	dp := newFakeDeps(t)
	dataDir, id := requestdrivertest.BuildingFixture(dp, t, 1)
	branch, baseSHA := "factoryd/"+id+"-001", fmt.Sprintf("%040d", 1)
	buildRunner := requestdrivertest.ReviewQuarantinedBuildRunner(t, dataDir, branch, baseSHA, "")
	calls, _ := requestdrivertest.StubReviewCorrectiveRunner(t, dataDir, func(dataDir, roundRunID string) *run.Run {
		return requestdrivertest.StillFailingAfterRecheck(t, dataDir, roundRunID, id+"-001", branch, fmt.Sprintf("%040d", 2), fmt.Sprintf("%040d", 3))
	})
	cfg := requestdriver.WorkerConfig{ReviewCorrectiveRounds: 2}
	if err := requestdrivertest.DriveRequests(dp, context.Background(), dataDir, cfg, requestdrivertest.FailingSpecDraftRunner(t), requestdrivertest.FailingPlanTicketsRunner(t), requestdrivertest.FailingOracleDraftRunner(t), buildRunner); err != nil {
		t.Fatalf("driveRequests: %v", err)
	}
	if *calls != 1 {
		t.Fatalf("corrective runner calls = %d, want 1: the recheck runs once", *calls)
	}
	loaded := requestdrivertest.LoadRequest(t, dataDir, id)
	if loaded.State != request.StateQuarantined || loaded.QuarantineCheck != request.QuarantineCheckReviewReproduction {
		t.Fatalf("State %q, QuarantineCheck %q, want quarantined on %q", loaded.State, loaded.QuarantineCheck, request.QuarantineCheckReviewReproduction)
	}
	for _, want := range []string{"review_reproduction", requestdrivertest.ReproductionPath, "still fails on the result commit after the corrective build"} {
		if !strings.Contains(loaded.Error, want) {
			t.Errorf("Error = %q, want it to name %q", loaded.Error, want)
		}
	}
	if next := loaded.NextAction(); !strings.Contains(next, "review_reproduction quarantined") || !strings.Contains(next, "factoryd override") {
		t.Errorf("NextAction() = %q, want the review_reproduction remedy", next)
	}
	if rounds := loaded.Tickets[0].Rounds; len(rounds) != 1 || rounds[0].Outcome != request.RoundQuarantined {
		t.Errorf("Rounds = %+v, want the one quarantined round", rounds)
	}
}

// With budget left, a corrective build its own review refused through a new
// reproduction is followed by another, which rechecks that build's held
// copy and is measured against the ticket's original base.
func TestReviewReproductionNewFindingInARoundRunsTheNextRound(t *testing.T) {
	dp := newFakeDeps(t)
	dataDir, id := requestdrivertest.BuildingFixture(dp, t, 1)
	branch, originalBase, round1Tip := "factoryd/"+id+"-001", fmt.Sprintf("%040d", 1), fmt.Sprintf("%040d", 2)
	buildRunner := requestdrivertest.ReviewQuarantinedBuildRunner(t, dataDir, branch, originalBase, "")
	var round2Args []string
	callCount := 0
	orig := requestdriver.ReviewCorrectiveRunner
	t.Cleanup(func() { requestdriver.ReviewCorrectiveRunner = orig })
	requestdriver.ReviewCorrectiveRunner = func(ctx context.Context, args []string, onReady func(*run.Run)) error {
		callCount++
		roundRunID := requestdrivertest.ArgValue(args, "-ticket")
		if onReady != nil {
			onReady(&run.Run{ID: roundRunID})
		}
		if callCount == 1 {
			rr := requestdrivertest.ReproductionRefusedOn(t, dataDir, roundRunID, branch, round1Tip, fmt.Sprintf("%040d", 3))
			rr.DiffBaseSHA = originalBase
			return rr.Save(dataDir)
		}
		round2Args = args
		return (&run.Run{ID: roundRunID, State: run.StateAccepted, Branch: branch, PullRequestURL: "https://github.com/acme/app/pull/9"}).Save(dataDir)
	}
	cfg := requestdriver.WorkerConfig{ReviewCorrectiveRounds: 2, OpenPullRequest: true}
	if err := requestdrivertest.DriveRequests(dp, context.Background(), dataDir, cfg, requestdrivertest.FailingSpecDraftRunner(t), requestdrivertest.FailingPlanTicketsRunner(t), requestdrivertest.FailingOracleDraftRunner(t), buildRunner); err != nil {
		t.Fatalf("driveRequests: %v", err)
	}
	if callCount != 2 {
		t.Fatalf("corrective runner calls = %d, want 2", callCount)
	}
	if got := requestdrivertest.ArgValue(round2Args, "-diff-base"); got != originalBase {
		t.Errorf("round 2's -diff-base = %q, want the original base %q", got, originalBase)
	}
	// Round 1's own finding has the first build's path and hash: the
	// ticket's list holds it once, from the run that first held it.
	if m := readRecheckManifest(t, round2Args); len(m.Reproductions) != 1 || m.Reproductions[0].RunID != id+"-001" || m.Reproductions[0].Path != requestdrivertest.ReproductionPath {
		t.Errorf("round 2 rechecks %+v, want the ticket's one reproduction", m.Reproductions)
	}
	if list := requestdrivertest.LoadRequest(t, dataDir, id).Tickets[0].Reproductions; len(list) != 1 || !slices.Equal(list[0].RefusedRuns, []string{id + "-001", id + "-001-corrective1"}) {
		t.Errorf("ticket list = %+v, want one reproduction that refused both runs", list)
	}
	if loaded := requestdrivertest.LoadRequest(t, dataDir, id); loaded.State != request.StatePRReview {
		t.Fatalf("State = %q, want %q", loaded.State, request.StatePRReview)
	}
}

// SC-022: severity never refuses. A high finding with no reproduction the
// factory ran starts no corrective build: an accepted run stays accepted,
// and a run whose code_review failed with a verdict (a record from a worker
// whose findings still failed it) is not handed to a build.
func TestHighSeverityFindingWithoutReproductionStartsNoCorrectiveRound(t *testing.T) {
	high := &run.CodeReviewResult{Policy: "required", Available: true, Findings: []run.CodeReviewFinding{
		{Severity: "high", File: "a.go", Line: 1, Summary: "nil deref", FailureScenario: "crash", Disposition: run.DispositionAdvice},
	}}
	for name, tc := range map[string]struct {
		state     run.State
		failed    []string
		wantState request.State
	}{
		"accepted with high advice":         {run.StateAccepted, nil, request.StatePRReview},
		"code_review failed with a verdict": {run.StateQuarantined, []string{"code_review"}, request.StateQuarantined},
	} {
		t.Run(name, func(t *testing.T) {
			dp := newFakeDeps(t)
			dataDir, id := requestdrivertest.BuildingFixture(dp, t, 1)
			buildRunner := func(ctx context.Context, args []string, onReady func(*run.Run)) error {
				ticket := requestdrivertest.ArgValue(args, "-ticket")
				if onReady != nil {
					onReady(&run.Run{ID: ticket})
				}
				rr := requestdrivertest.QuarantinedOn(t, dataDir, ticket, "factoryd/"+id+"-001", fmt.Sprintf("%040d", 1), fmt.Sprintf("%040d", 2), tc.failed...)
				rr.CodeReview, rr.State = high, tc.state
				if tc.state == run.StateAccepted {
					rr.HandoffSHA256, rr.PullRequestURL = "", "https://github.com/acme/app/pull/1"
				}
				return rr.Save(dataDir)
			}
			calls, _ := requestdrivertest.StubReviewCorrectiveRunner(t, dataDir, func(dataDir, roundRunID string) *run.Run {
				return &run.Run{ID: roundRunID, State: run.StateAccepted}
			})
			cfg := requestdriver.WorkerConfig{ReviewCorrectiveRounds: 1, OpenPullRequest: true}
			if err := requestdrivertest.DriveRequests(dp, context.Background(), dataDir, cfg, requestdrivertest.FailingSpecDraftRunner(t), requestdrivertest.FailingPlanTicketsRunner(t), requestdrivertest.FailingOracleDraftRunner(t), buildRunner); err != nil {
				t.Fatalf("driveRequests: %v", err)
			}
			if *calls != 0 {
				t.Errorf("corrective runner calls = %d, want 0", *calls)
			}
			loaded := requestdrivertest.LoadRequest(t, dataDir, id)
			if loaded.State != tc.wantState || loaded.QuarantineCheck != "" || len(loaded.Tickets[0].Rounds) != 0 {
				t.Errorf("State %q, QuarantineCheck %q, Rounds %+v, want %q with no check and no round", loaded.State, loaded.QuarantineCheck, loaded.Tickets[0].Rounds, tc.wantState)
			}
		})
	}
}

// SC-018, SC-022 on the request path: from the ticket's quarantined build to
// the corrective build's argv, a blocking finding reaches the build only as
// its one cleaned line in the -earlier-attempt record; the spec, the record
// and the argv never carry the reviewer's failure scenario, the
// reproduction's content, a finding that is advice, or the held copy.
func TestBlockingFindingReachesBuildAsOneLineWithoutContent(t *testing.T) {
	dp := newFakeDeps(t)
	dataDir, id := requestdrivertest.BuildingFixture(dp, t, 1)
	branch := "factoryd/" + id + "-001"
	buildRunner := requestdrivertest.ReviewQuarantinedBuildRunner(t, dataDir, branch, fmt.Sprintf("%040d", 1), "")
	_, lastArgs := requestdrivertest.StubReviewCorrectiveRunner(t, dataDir, func(dataDir, roundRunID string) *run.Run {
		return &run.Run{ID: roundRunID, State: run.StateAccepted, Branch: branch}
	})
	cfg := requestdriver.WorkerConfig{ReviewCorrectiveRounds: 1}
	if err := requestdrivertest.DriveRequests(dp, context.Background(), dataDir, cfg, requestdrivertest.FailingSpecDraftRunner(t), requestdrivertest.FailingPlanTicketsRunner(t), requestdrivertest.FailingOracleDraftRunner(t), buildRunner); err != nil {
		t.Fatalf("driveRequests: %v", err)
	}
	assertToldOnlyTheLine(t, dataDir, id+"-001", *lastArgs, true)
	record := string(mustReadFile(t, requestdrivertest.ArgValue(*lastArgs, "-earlier-attempt")))
	if got := strings.Count(record, requestdrivertest.ReproductionSummary); got != 1 {
		t.Errorf("the record quotes the finding %d times, want once:\n%s", got, record)
	}
}

// A retry of a ticket a reviewer's reproduction refused (the next build of
// the ticket, given the attempt's record) is told the finding in one line
// and rechecks the held copy too, like a corrective round.
func TestARetryAfterAReproductionRechecksIt(t *testing.T) {
	dp := newFakeDeps(t)
	dataDir, id := requestdrivertest.BuildingFixture(dp, t, 1)
	var builds [][]string
	buildRunner := func(ctx context.Context, args []string, onReady func(*run.Run)) error {
		builds = append(builds, args)
		ticket := requestdrivertest.ArgValue(args, "-ticket")
		if onReady != nil {
			onReady(&run.Run{ID: ticket})
		}
		rr := requestdrivertest.ReproductionRefusedOn(t, dataDir, ticket, "factoryd/"+ticket, fmt.Sprintf("%040d", 1), fmt.Sprintf("%040d", 2))
		rr.RequestID = id
		// The run records the spec it was built from, as a real build does.
		sum, err := evidence.SHA256File(requestdrivertest.ArgValue(args, "-spec"))
		if err != nil {
			return err
		}
		rr.SpecSHA256 = sum
		return rr.Save(dataDir)
	}
	// No corrective budget: the first build quarantines the request.
	if err := requestdrivertest.DriveRequests(dp, context.Background(), dataDir, requestdriver.WorkerConfig{}, requestdrivertest.FailingSpecDraftRunner(t), requestdrivertest.FailingPlanTicketsRunner(t), requestdrivertest.FailingOracleDraftRunner(t), buildRunner); err != nil {
		t.Fatalf("driveRequests: %v", err)
	}
	if _, err := request.Retry(dataDir, id, "alice", "", time.Now(), nil); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if err := requestdrivertest.DriveRequests(dp, context.Background(), dataDir, requestdriver.WorkerConfig{}, requestdrivertest.FailingSpecDraftRunner(t), requestdrivertest.FailingPlanTicketsRunner(t), requestdrivertest.FailingOracleDraftRunner(t), buildRunner); err != nil {
		t.Fatalf("driveRequests after retry: %v", err)
	}
	if len(builds) != 2 {
		t.Fatalf("builds = %d, want the first and the retry", len(builds))
	}
	if requestdrivertest.HasFlag(builds[0], "-recheck-reproductions") {
		t.Errorf("the first build rechecks with nothing before it: %v", builds[0])
	}
	if m := readRecheckManifest(t, builds[1]); len(m.Reproductions) != 1 || m.Reproductions[0].RunID != id+"-001" {
		t.Errorf("manifest = %+v, want the quarantined run's held reproduction", m)
	}
	assertToldOnlyTheLine(t, dataDir, id+"-001", builds[1], true)
}

// refusedBuildRunner is a ticket build a reviewer's reproduction refuses
// (ReproductionRefusedOn), recording the request and the spec it was built
// from as a real build does; each build's args are appended to builds.
func refusedBuildRunner(t *testing.T, dataDir, id string, builds *[][]string) requestdriver.TicketRunner {
	return func(ctx context.Context, args []string, onReady func(*run.Run)) error {
		*builds = append(*builds, args)
		ticket := requestdrivertest.ArgValue(args, "-ticket")
		if onReady != nil {
			onReady(&run.Run{ID: ticket})
		}
		rr := requestdrivertest.ReproductionRefusedOn(t, dataDir, ticket, "factoryd/"+ticket, fmt.Sprintf("%040d", 1), fmt.Sprintf("%040d", 2))
		return saveAsBuiltFrom(t, dataDir, id, args, rr)
	}
}

// saveAsBuiltFrom saves rr with the request id and the hash of the -spec it
// was built from, as a real build records them.
func saveAsBuiltFrom(t *testing.T, dataDir, id string, args []string, rr *run.Run) error {
	t.Helper()
	sum, err := evidence.SHA256File(requestdrivertest.ArgValue(args, "-spec"))
	if err != nil {
		return err
	}
	rr.RequestID, rr.SpecSHA256 = id, sum
	return rr.Save(dataDir)
}

// assertRechecksTheFirstRunsCopy checks that args' manifest runs the held
// copy of the first build (origin), from under origin's own review-repro
// directory.
func assertRechecksTheFirstRunsCopy(t *testing.T, dataDir, origin string, args []string) {
	t.Helper()
	m := readRecheckManifest(t, args)
	want := requestdrivertest.HeldEntry(t, dataDir, origin)
	if len(m.Reproductions) != 1 || fmt.Sprint(m.Reproductions[0]) != fmt.Sprint(want) {
		t.Errorf("manifest = %+v, want the first run's held copy %+v", m.Reproductions, want)
	}
}

// SC-022: after a corrective build whose recheck is still failing, the
// automatic rounds stop, but an operator's retry, which builds on that
// build's commits, still rechecks the first run's reproduction.
func TestARetryAfterAStillFailingRecheckRechecksTheFirstRunsCopy(t *testing.T) {
	dp := newFakeDeps(t)
	dataDir, id := requestdrivertest.BuildingFixture(dp, t, 1)
	var builds, rounds [][]string
	buildRunner := refusedBuildRunner(t, dataDir, id, &builds)
	orig := requestdriver.ReviewCorrectiveRunner
	t.Cleanup(func() { requestdriver.ReviewCorrectiveRunner = orig })
	requestdriver.ReviewCorrectiveRunner = func(ctx context.Context, args []string, onReady func(*run.Run)) error {
		rounds = append(rounds, args)
		roundRunID := requestdrivertest.ArgValue(args, "-ticket")
		if onReady != nil {
			onReady(&run.Run{ID: roundRunID})
		}
		rr := requestdrivertest.StillFailingAfterRecheck(t, dataDir, roundRunID, id+"-001", "factoryd/"+id+"-001", fmt.Sprintf("%040d", 1), fmt.Sprintf("%040d", 3))
		return saveAsBuiltFrom(t, dataDir, id, args, rr)
	}
	cfg := requestdriver.WorkerConfig{ReviewCorrectiveRounds: 2}
	drive := func() {
		t.Helper()
		if err := requestdrivertest.DriveRequests(dp, context.Background(), dataDir, cfg, requestdrivertest.FailingSpecDraftRunner(t), requestdrivertest.FailingPlanTicketsRunner(t), requestdrivertest.FailingOracleDraftRunner(t), buildRunner); err != nil {
			t.Fatalf("driveRequests: %v", err)
		}
	}
	drive()
	if len(builds) != 1 || len(rounds) != 1 {
		t.Fatalf("builds %d, rounds %d; want the first build and one corrective round, then no automatic round after the still-failing recheck", len(builds), len(rounds))
	}
	if _, err := request.Retry(dataDir, id, "alice", "", time.Now(), nil); err != nil {
		t.Fatalf("retry: %v", err)
	}
	drive()
	if len(builds) != 2 {
		t.Fatalf("builds = %d, want the retry", len(builds))
	}
	assertRechecksTheFirstRunsCopy(t, dataDir, id+"-001", builds[1])
}

// SC-022: a corrective build whose recheck passed but which another check
// quarantined is followed by a round that still rechecks the reproduction:
// it stays on the ticket's list, so a later round cannot undo the fix.
func TestARoundAfterAPassingRecheckStillRechecksIt(t *testing.T) {
	dp := newFakeDeps(t)
	dataDir, id := requestdrivertest.BuildingFixture(dp, t, 1)
	branch := "factoryd/" + id + "-001"
	buildRunner := requestdrivertest.ReviewQuarantinedBuildRunner(t, dataDir, branch, fmt.Sprintf("%040d", 1), "")
	var rounds [][]string
	orig := requestdriver.ReviewCorrectiveRunner
	t.Cleanup(func() { requestdriver.ReviewCorrectiveRunner = orig })
	requestdriver.ReviewCorrectiveRunner = func(ctx context.Context, args []string, onReady func(*run.Run)) error {
		rounds = append(rounds, args)
		roundRunID := requestdrivertest.ArgValue(args, "-ticket")
		if onReady != nil {
			onReady(&run.Run{ID: roundRunID})
		}
		if len(rounds) == 1 {
			return requestdrivertest.AfterRecheck(t, dataDir, roundRunID, id+"-001", branch, fmt.Sprintf("%040d", 1), fmt.Sprintf("%040d", 3), "passes", "lint").Save(dataDir)
		}
		return (&run.Run{ID: roundRunID, State: run.StateAccepted, Branch: branch, PullRequestURL: "https://github.com/acme/app/pull/9"}).Save(dataDir)
	}
	cfg := requestdriver.WorkerConfig{ReviewCorrectiveRounds: 2, OpenPullRequest: true}
	if err := requestdrivertest.DriveRequests(dp, context.Background(), dataDir, cfg, requestdrivertest.FailingSpecDraftRunner(t), requestdrivertest.FailingPlanTicketsRunner(t), requestdrivertest.FailingOracleDraftRunner(t), buildRunner); err != nil {
		t.Fatalf("driveRequests: %v", err)
	}
	if len(rounds) != 2 {
		t.Fatalf("corrective rounds = %d, want 2", len(rounds))
	}
	assertRechecksTheFirstRunsCopy(t, dataDir, id+"-001", rounds[1])
}

// SC-022: a build that would continue on the commits of a run a reviewer's
// reproduction refused, when that reproduction's held copy is gone, is not
// built: the request halts with the reason.
func TestARetryWithTheHeldCopyGoneIsRefused(t *testing.T) {
	dp := newFakeDeps(t)
	dataDir, id := requestdrivertest.BuildingFixture(dp, t, 1)
	var builds [][]string
	buildRunner := refusedBuildRunner(t, dataDir, id, &builds)
	drive := func() {
		t.Helper()
		if err := requestdrivertest.DriveRequests(dp, context.Background(), dataDir, requestdriver.WorkerConfig{}, requestdrivertest.FailingSpecDraftRunner(t), requestdrivertest.FailingPlanTicketsRunner(t), requestdrivertest.FailingOracleDraftRunner(t), buildRunner); err != nil {
			t.Fatalf("driveRequests: %v", err)
		}
	}
	drive()
	if err := os.Remove(requestdrivertest.HeldEntry(t, dataDir, id+"-001").HostPath); err != nil {
		t.Fatal(err)
	}
	if _, err := request.Retry(dataDir, id, "alice", "", time.Now(), nil); err != nil {
		t.Fatalf("retry: %v", err)
	}
	drive()
	if len(builds) != 1 {
		t.Fatalf("builds = %d, want no build after the retry", len(builds))
	}
	loaded := requestdrivertest.LoadRequest(t, dataDir, id)
	if loaded.State != request.StateHalted || loaded.HaltKind != request.HaltReproductionUnheld || !strings.Contains(loaded.Error, "not built without a recheck") || !strings.Contains(loaded.Error, "factoryd retry "+id) {
		t.Errorf("State %q, kind %q, Error %q; want halted with the recheck's reason and the operator's way out", loaded.State, loaded.HaltKind, loaded.Error)
	}
}

// A ticket run with only a review that gave no verdict is not judged: no
// corrective build, and the request waits in resume_review with the cause.
func TestCodeReviewWithNoVerdictStartsNoRoundAndWaitsInResumeReview(t *testing.T) {
	dp := newFakeDeps(t)
	dataDir, id := requestdrivertest.BuildingFixture(dp, t, 1)
	buildRunner := func(ctx context.Context, args []string, onReady func(*run.Run)) error {
		ticket := requestdrivertest.ArgValue(args, "-ticket")
		if onReady != nil {
			onReady(&run.Run{ID: ticket})
		}
		rr := requestdrivertest.QuarantinedOn(t, dataDir, ticket, "factoryd/"+id+"-001", fmt.Sprintf("%040d", 1), fmt.Sprintf("%040d", 2), "code_review")
		rr.CodeReview = &run.CodeReviewResult{Policy: "required", Available: false}
		return rr.Save(dataDir)
	}
	calls, _ := requestdrivertest.StubReviewCorrectiveRunner(t, dataDir, func(dataDir, roundRunID string) *run.Run {
		return &run.Run{ID: roundRunID, State: run.StateAccepted}
	})
	cfg := requestdriver.WorkerConfig{ReviewCorrectiveRounds: 1}
	if err := requestdrivertest.DriveRequests(dp, context.Background(), dataDir, cfg, requestdrivertest.FailingSpecDraftRunner(t), requestdrivertest.FailingPlanTicketsRunner(t), requestdrivertest.FailingOracleDraftRunner(t), buildRunner); err != nil {
		t.Fatalf("driveRequests: %v", err)
	}
	if *calls != 0 {
		t.Fatalf("corrective runner calls = %d, want 0", *calls)
	}
	loaded := requestdrivertest.LoadRequest(t, dataDir, id)
	if loaded.State != request.StateResumeReview || loaded.Resume == nil || loaded.Resume.Cause != unresolved.NoVerdict {
		t.Fatalf("state %s resume %+v, want resume_review with cause %s", loaded.State, loaded.Resume, unresolved.NoVerdict)
	}
}

// Budget zero: no corrective build; the quarantine names review_reproduction.
func TestReviewReproductionWithNoBudgetQuarantinesOnTheCheck(t *testing.T) {
	dp := newFakeDeps(t)
	dataDir, id := requestdrivertest.BuildingFixture(dp, t, 1)
	buildRunner := requestdrivertest.ReviewQuarantinedBuildRunner(t, dataDir, "factoryd/"+id+"-001", fmt.Sprintf("%040d", 1), "")
	calls, _ := requestdrivertest.StubReviewCorrectiveRunner(t, dataDir, func(dataDir, roundRunID string) *run.Run {
		return &run.Run{ID: roundRunID, State: run.StateAccepted}
	})
	if err := requestdrivertest.DriveRequests(dp, context.Background(), dataDir, requestdriver.WorkerConfig{}, requestdrivertest.FailingSpecDraftRunner(t), requestdrivertest.FailingPlanTicketsRunner(t), requestdrivertest.FailingOracleDraftRunner(t), buildRunner); err != nil {
		t.Fatalf("driveRequests: %v", err)
	}
	if *calls != 0 {
		t.Fatalf("corrective runner calls = %d, want 0", *calls)
	}
	loaded := requestdrivertest.LoadRequest(t, dataDir, id)
	if loaded.State != request.StateQuarantined || loaded.QuarantineCheck != request.QuarantineCheckReviewReproduction {
		t.Fatalf("State %q, QuarantineCheck %q, want quarantined on review_reproduction", loaded.State, loaded.QuarantineCheck)
	}
}

// A ticket that already spent its budget gets no further round.
func TestReviewReproductionAfterTheBudgetIsSpentStartsNoRound(t *testing.T) {
	dp := newFakeDeps(t)
	dataDir, id := requestdrivertest.BuildingFixture(dp, t, 1)
	r := requestdrivertest.LoadRequest(t, dataDir, id)
	r.TicketIndex = 1
	ticket := &r.Tickets[0]
	ticket.Rounds = append(ticket.Rounds, request.Round{Index: 1, Kind: request.CorrectiveRoundKind, Outcome: request.RoundQuarantined, RunID: "prior", At: time.Now().UTC().Format(time.RFC3339Nano)})
	runRecord := requestdrivertest.ReproductionRefusedOn(t, dataDir, "run-under-test", "factoryd/"+id+"-001", fmt.Sprintf("%040d", 1), fmt.Sprintf("%040d", 2))
	calls, _ := requestdrivertest.StubReviewCorrectiveRunner(t, dataDir, func(dataDir, roundRunID string) *run.Run {
		return &run.Run{ID: roundRunID, State: run.StateAccepted}
	})
	handled, err := requestdriver.TryCheckCorrectiveRound(dp, context.Background(), dataDir, r, ticket, runRecord, requestdriver.WorkerConfig{ReviewCorrectiveRounds: 1}, time.Now())
	if handled || err != nil || *calls != 0 || len(ticket.Rounds) != 1 {
		t.Fatalf("handled=%v err=%v calls=%d rounds=%d, want nothing started", handled, err, *calls, len(ticket.Rounds))
	}
}

// A cancel that lands while the round builds survives the round's own save.
func TestCorrectiveRoundCancelledMidRoundDoesNotResurrectRequest(t *testing.T) {
	dp := newFakeDeps(t)
	dataDir, id := requestdrivertest.BuildingFixture(dp, t, 1)
	branch := "factoryd/" + id + "-001"
	buildRunner := requestdrivertest.ReviewQuarantinedBuildRunner(t, dataDir, branch, fmt.Sprintf("%040d", 1), "")
	calls, _ := requestdrivertest.StubReviewCorrectiveRunner(t, dataDir, func(dataDir, roundRunID string) *run.Run {
		cancelRequestForTest(t, dataDir, id)
		return &run.Run{ID: roundRunID, State: run.StateAccepted, Branch: branch, PullRequestURL: "https://github.com/acme/app/pull/999"}
	})
	cfg := requestdriver.WorkerConfig{ReviewCorrectiveRounds: 1, OpenPullRequest: true}
	if err := requestdrivertest.DriveRequests(dp, context.Background(), dataDir, cfg, requestdrivertest.FailingSpecDraftRunner(t), requestdrivertest.FailingPlanTicketsRunner(t), requestdrivertest.FailingOracleDraftRunner(t), buildRunner); err != nil {
		t.Fatalf("driveRequests: %v", err)
	}
	loaded := requestdrivertest.LoadRequest(t, dataDir, id)
	if *calls != 1 || loaded.State != request.StateCancelled || len(loaded.Tickets[0].Rounds) != 0 {
		t.Fatalf("calls %d, State %q, Rounds %+v, want the cancel kept and no round recorded", *calls, loaded.State, loaded.Tickets[0].Rounds)
	}
}

// A shutdown mid-round leaves the request building with no round recorded
// and no budget spent.
func TestCorrectiveRoundCtxCancelledLeavesRequestBuilding(t *testing.T) {
	dp := newFakeDeps(t)
	dataDir, id := requestdrivertest.BuildingFixture(dp, t, 1)
	buildRunner := requestdrivertest.ReviewQuarantinedBuildRunner(t, dataDir, "factoryd/"+id+"-001", fmt.Sprintf("%040d", 1), "")
	ctx, cancel := context.WithCancel(context.Background())
	orig := requestdriver.ReviewCorrectiveRunner
	t.Cleanup(func() { requestdriver.ReviewCorrectiveRunner = orig })
	requestdriver.ReviewCorrectiveRunner = func(rctx context.Context, args []string, onReady func(*run.Run)) error {
		cancel()
		return errors.New("context canceled")
	}
	cfg := requestdriver.WorkerConfig{ReviewCorrectiveRounds: 1}
	if err := requestdrivertest.DriveRequests(dp, ctx, dataDir, cfg, requestdrivertest.FailingSpecDraftRunner(t), requestdrivertest.FailingPlanTicketsRunner(t), requestdrivertest.FailingOracleDraftRunner(t), buildRunner); err == nil {
		t.Fatal("driveRequests: want the cancelled round's own error propagated")
	}
	loaded := requestdrivertest.LoadRequest(t, dataDir, id)
	if loaded.State != request.StateBuilding || len(loaded.Tickets[0].Rounds) != 0 {
		t.Fatalf("State %q, Rounds %+v, want building with no round", loaded.State, loaded.Tickets[0].Rounds)
	}
}

// While the round runs, the request on disk already names the round's run.
func TestCorrectiveRoundRecordsItsRunWhileRunning(t *testing.T) {
	dp := newFakeDeps(t)
	dataDir, id := requestdrivertest.BuildingFixture(dp, t, 1)
	branch := "factoryd/" + id + "-001"
	buildRunner := requestdrivertest.ReviewQuarantinedBuildRunner(t, dataDir, branch, fmt.Sprintf("%040d", 1), "")
	var onDisk, requestIDOnRun string
	requestdrivertest.StubReviewCorrectiveRunner(t, dataDir, func(dataDir, roundRunID string) *run.Run {
		onDisk = requestdrivertest.LoadRequest(t, dataDir, id).Tickets[0].RunID
		if rr, err := run.Load(dataDir, roundRunID); err == nil {
			requestIDOnRun = rr.RequestID
		}
		return &run.Run{ID: roundRunID, State: run.StateAccepted, Branch: branch, RequestID: id, PullRequestURL: "https://github.com/acme/app/pull/1"}
	})
	cfg := requestdriver.WorkerConfig{ReviewCorrectiveRounds: 1, OpenPullRequest: true}
	if err := requestdrivertest.DriveRequests(dp, context.Background(), dataDir, cfg, requestdrivertest.FailingSpecDraftRunner(t), requestdrivertest.FailingPlanTicketsRunner(t), requestdrivertest.FailingOracleDraftRunner(t), buildRunner); err != nil {
		t.Fatalf("driveRequests: %v", err)
	}
	if want := id + "-001-corrective1"; onDisk != want || requestIDOnRun != id {
		t.Errorf("during the round: ticket RunID %q, run RequestID %q; want %q, %q", onDisk, requestIDOnRun, want, id)
	}
}
