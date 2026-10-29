package requestdriver_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"buildgate/internal/release"
	"buildgate/internal/request"
	"buildgate/internal/requestdriver"
	"buildgate/internal/requestdriver/requestdrivertest"
	"buildgate/internal/run"
)

const heldContent = "package sum\n\n// CONTENT-NEVER-TOLD\nfunc TestEmpty(t *testing.T) {}\n"

// redChallenge is the record of a first build whose held file failed.
func redChallenge(t *testing.T, dataDir, runID string) *run.ChallengeRecord {
	t.Helper()
	host := filepath.Join(run.Dir(dataDir, runID), run.ChallengeDirName, "0", "internal", "sum", "sum_edge_test.go")
	if err := os.MkdirAll(filepath.Dir(host), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(host, []byte(heldContent), 0o444); err != nil {
		t.Fatal(err)
	}
	return &run.ChallengeRecord{Verdict: run.ChallengeRed, Files: []run.ChallengeFile{{
		Path: "internal/sum/sum_edge_test.go", SHA256: strings.Repeat("a", 64), HostPath: host, Criterion: "AC-2", Verdict: run.ChallengeRed,
		Failures: []run.ChallengeFailure{{Test: "TestEmptySum", Assertion: "fail"}},
	}}}
}

// challengeFixture is an advice fixture whose first build records a red
// challenge and whose request has an approved spec.
func challengeFixture(t *testing.T, verdict string) *adviceFixture {
	f := newAdviceFixture(t, nil)
	f.cfg.Settings.ReviewAdviceRounds = 0
	r := requestdrivertest.LoadRequest(t, f.dataDir, f.id)
	sum, err := request.HashFile(f.dataDir, r.ID, "spec.md")
	if err != nil {
		t.Fatal(err)
	}
	if r.ApprovedSHA256 == nil {
		r.ApprovedSHA256 = map[string]string{}
	}
	r.ApprovedSHA256["spec.md"] = sum
	if err := r.Save(f.dataDir); err != nil {
		t.Fatal(err)
	}
	first := f.buildRunner
	f.buildRunner = func(ctx context.Context, args []string, onReady func(*run.Run)) error {
		if err := first(ctx, args, onReady); err != nil {
			return err
		}
		id := requestdrivertest.ArgValue(args, "-ticket")
		rec, err := run.Load(f.dataDir, id)
		if err != nil {
			return err
		}
		rec.Challenge = redChallenge(t, f.dataDir, id)
		rec.Challenge.Verdict = verdict
		return rec.Save(f.dataDir)
	}
	return f
}

// committedFor is the commit a green challenge round records: the files are
// committed only when it is green.
func committedFor(verdict string) string {
	if verdict == run.ChallengeGreen {
		return roundSHA(2)
	}
	return ""
}

// challengeRoundRun is the challenge round's run as its stand-in saves it:
// accepted on the first run's branch with an allowed decision and the given
// challenge verdict.
func (f *adviceFixture) challengeRoundRun(t *testing.T, id, verdict string) *run.Run {
	t.Helper()
	rr := &run.Run{
		ID: id, Ticket: id, Project: "app", State: run.StateAccepted, Branch: f.branch(), BaseSHA: f.resultSHA, DiffBaseSHA: f.baseSHA, ResultSHA: roundSHA(1),
		WorkspacePath: t.TempDir(), Attempts: []run.Attempt{{}},
		CodeReview: &run.CodeReviewResult{Policy: "required", Available: true},
		Challenge:  &run.ChallengeRecord{Verdict: verdict, Rerun: true, CommittedSHA: committedFor(verdict), Files: []run.ChallengeFile{{Path: "internal/sum/sum_edge_test.go", Criterion: "AC-2", Verdict: verdict}}},
	}
	if err := release.SaveDecision(f.dataDir, release.Decision{RunID: id, Project: "app", Allowed: true, Evaluated: time.Now().UTC().Format(time.RFC3339)}); err != nil {
		t.Fatal(err)
	}
	return rr
}

// SC-022: the first build of a request's ticket is given the challenge inputs
// (the approved spec with the answers, the counts a header may cite); a red
// challenge starts one challenge round, given the failing files' paths,
// criteria and failing tests as its record and the held files read-only, never
// a file's content; an accepted round with a green challenge is pushed and
// becomes the ticket's run, with one comment.
func TestRedChallengeStartsOneRoundWhichWhenGreenIsPushed(t *testing.T) {
	f := challengeFixture(t, run.ChallengeRed)
	var roundArgs [][]string
	calls, lastArgs := requestdrivertest.StubReviewCorrectiveRunner(t, f.dataDir, func(dataDir, id string) *run.Run {
		return f.challengeRoundRun(t, id, run.ChallengeGreen)
	})
	if err := f.drive(t, context.Background(), f.buildRunner); err != nil {
		t.Fatalf("drive: %v", err)
	}
	roundArgs = append(roundArgs, *lastArgs)
	first := f.buildArgs[0]
	if !requestdrivertest.HasFlag(first, "-challenge") || requestdrivertest.ArgValue(first, "-challenge-spec") == "" || requestdrivertest.ArgValue(first, "-challenge-criteria") == "" {
		t.Errorf("the first build = %v, want -challenge with its spec and counts", first)
	}
	if *calls != 1 {
		t.Fatalf("challenge round builds = %d, want exactly 1", *calls)
	}
	args := roundArgs[0]
	if !requestdrivertest.HasFlag(args, "-challenge-round") || requestdrivertest.ArgValue(args, "-challenge-held") == "" || requestdrivertest.HasFlag(args, "-challenge") {
		t.Errorf("round args = %v, want -challenge-round with the held manifest and no new job", args)
	}
	record := earlierAttemptOf(t, args)
	for _, want := range []string{"internal/sum/sum_edge_test.go", "AC-2", "TestEmptySum"} {
		if !strings.Contains(record, want) {
			t.Errorf("the record lacks %q:\n%s", want, record)
		}
	}
	for _, input := range []string{record, requestdrivertest.ArgValue(args, "-challenge-held")} {
		body := input
		if !strings.Contains(input, "\n") {
			body = string(mustReadFile(t, input))
		}
		if strings.Contains(body, "CONTENT-NEVER-TOLD") {
			t.Errorf("a held file's content reached the round's input:\n%s", body)
		}
	}
	loaded := requestdrivertest.LoadRequest(t, f.dataDir, f.id)
	tk := loaded.Tickets[0]
	roundRunID := requestdrivertest.TicketRunID(f.id, 1) + "-challenge1"
	if len(tk.Rounds) != 1 || tk.Rounds[0].Kind != request.ChallengeRoundKind || !tk.Rounds[0].Pushed || tk.RunID != roundRunID || !tk.ChallengeRounded || !tk.Challenged || tk.Advice != nil {
		t.Fatalf("ticket = %+v, want one pushed challenge round that is the ticket's run", tk)
	}
	if len(f.comments) != 1 || !strings.Contains(f.comments[0], "internal/sum/sum_edge_test.go") {
		t.Errorf("comments = %v, want one naming the committed file", f.comments)
	}
}

// A round that does not turn the challenge green leaves the run it followed,
// puts the branch back, posts the advice and starts no second round.
func TestChallengeRoundThatStaysRedIsAdviceAndTheRunStands(t *testing.T) {
	f := challengeFixture(t, run.ChallengeRed)
	calls, _ := requestdrivertest.StubReviewCorrectiveRunner(t, f.dataDir, func(dataDir, id string) *run.Run {
		return f.challengeRoundRun(t, id, run.ChallengeRed)
	})
	if err := f.drive(t, context.Background(), f.buildRunner); err != nil {
		t.Fatalf("drive: %v", err)
	}
	if *calls != 1 {
		t.Fatalf("round builds = %d, want 1", *calls)
	}
	if len(f.pushed) != 0 || len(f.restored) != 1 {
		t.Errorf("pushed %v, restored %v; want nothing pushed and the branch put back", f.pushed, f.restored)
	}
	tk := requestdrivertest.LoadRequest(t, f.dataDir, f.id).Tickets[0]
	if len(tk.Rounds) != 1 || tk.Rounds[0].Pushed || tk.RunID != requestdrivertest.TicketRunID(f.id, 1) || tk.Advice != nil {
		t.Errorf("ticket = %+v, want the first run to stand with one unpushed round", tk)
	}
	if len(f.comments) != 1 || !strings.Contains(f.comments[0], "internal/sum/sum_edge_test.go") || !strings.Contains(f.comments[0], "AC-2") || strings.Contains(f.comments[0], "CONTENT-NEVER-TOLD") {
		t.Errorf("comments = %v, want the advice naming the file and criterion only", f.comments)
	}
}

// No round for a green challenge, and a later build of the ticket runs the
// held files again (-challenge-held) and never starts another job.
func TestGreenChallengeStartsNoRound(t *testing.T) {
	f := challengeFixture(t, run.ChallengeGreen)
	calls, _ := requestdrivertest.StubReviewCorrectiveRunner(t, f.dataDir, func(dataDir, id string) *run.Run {
		return f.challengeRoundRun(t, id, run.ChallengeGreen)
	})
	if err := f.drive(t, context.Background(), f.buildRunner); err != nil {
		t.Fatalf("drive: %v", err)
	}
	if *calls != 0 {
		t.Errorf("round builds = %d, want none for a green challenge", *calls)
	}
	loaded := requestdrivertest.LoadRequest(t, f.dataDir, f.id)
	if tk := loaded.Tickets[0]; !tk.Challenged || tk.ChallengeRunID != requestdrivertest.TicketRunID(f.id, 1) {
		t.Errorf("ticket = %+v, want it recorded as challenged by its first run", tk)
	}
	later, err := requestdriver.BuildRequestBuildArgs(f.dataDir, loaded, loaded.Tickets[0], f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if requestdrivertest.HasFlag(later, "-challenge") || requestdrivertest.ArgValue(later, "-challenge-held") == "" || requestdrivertest.HasFlag(later, "-challenge-round") {
		t.Errorf("a later build = %v, want the held files to run again and no new job or round", later)
	}
}

// A round whose green files did not commit does not replace the run it
// followed: nothing is pushed and the first run stands.
func TestChallengeRoundWithUncommittedGreenFilesDoesNotReplaceTheRun(t *testing.T) {
	f := challengeFixture(t, run.ChallengeRed)
	requestdrivertest.StubReviewCorrectiveRunner(t, f.dataDir, func(dataDir, id string) *run.Run {
		rr := f.challengeRoundRun(t, id, run.ChallengeGreen)
		rr.Challenge.CommittedSHA = ""
		return rr
	})
	if err := f.drive(t, context.Background(), f.buildRunner); err != nil {
		t.Fatalf("drive: %v", err)
	}
	tk := requestdrivertest.LoadRequest(t, f.dataDir, f.id).Tickets[0]
	if len(f.pushed) != 0 || tk.RunID != requestdrivertest.TicketRunID(f.id, 1) {
		t.Errorf("pushed %v, ticket run %s; want nothing pushed and the first run to stand", f.pushed, tk.RunID)
	}
}
