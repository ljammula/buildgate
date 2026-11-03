package requestdriver_test

import (
	"context"
	"strings"
	"testing"

	"buildgate/internal/request"
	"buildgate/internal/requestdriver"
	"buildgate/internal/requestdriver/requestdrivertest"
	"buildgate/internal/run"
)

// A round that diff_scope quarantined for a red held file records that file as
// out of the ticket's scope with the files the round changed outside
// Allowed-Files, says so in the declined comment, and a later pass starts no
// further round for it.
func TestDiffScopeQuarantinedRoundRecordsTheRedAsOutOfScope(t *testing.T) {
	f := challengeFixture(t, run.ChallengeRed)
	f.cfg.Settings.ReviewAdviceRounds = 3
	orig := requestdriver.ReviewCorrectiveRunner
	t.Cleanup(func() { requestdriver.ReviewCorrectiveRunner = orig })
	rounds := 0
	requestdriver.ReviewCorrectiveRunner = func(ctx context.Context, args []string, onReady func(*run.Run)) error {
		rounds++
		id := requestdrivertest.ArgValue(args, "-ticket")
		onReady(&run.Run{ID: id})
		rr := f.adviceRun(t, id, run.StateQuarantined, false)
		rr.GateResults = []run.GateResult{{Check: "diff_scope", ExitCode: 1}}
		rr.ChangedFiles = []string{"internal/payments/refunds.go", "internal/other/handler.go"}
		return rr.Save(f.dataDir)
	}
	if err := f.drive(t, context.Background(), f.buildRunner); err != nil {
		t.Fatalf("drive: %v", err)
	}
	if rounds != 1 {
		t.Errorf("rounds = %d, want 1: the red is out of scope after the first", rounds)
	}
	r := requestdrivertest.LoadRequest(t, f.dataDir, f.id)
	want := request.ChallengeRaised{Ticket: 1, Path: "internal/sum/sum_edge_test.go", Criterion: "AC-2", Files: []string{"internal/other/handler.go"}, Source: request.ChallengeRaisedDiffScope}
	if len(r.ChallengeOutOfScope) != 1 || strings.Join(r.ChallengeOutOfScope[0].Files, ",") != strings.Join(want.Files, ",") || r.ChallengeOutOfScope[0].Source != want.Source || r.ChallengeOutOfScope[0].Path != want.Path {
		t.Errorf("ChallengeOutOfScope = %+v, want [%+v]", r.ChallengeOutOfScope, want)
	}
	if len(f.comments) != 1 || !strings.Contains(f.comments[0], "out of scope: needs internal/other/handler.go") {
		t.Errorf("comments = %v, want the declined comment naming the files the fix needs", f.comments)
	}
}

// The challenge spec lists the ticket's Allowed-Files, and what an earlier
// ticket raised only when every file of it is among them.
func TestChallengeSpecListsAllowedFilesAndCoveredRaisedEntries(t *testing.T) {
	f := newAdviceFixtureOf(t, 2, nil)
	r := requestdrivertest.LoadRequest(t, f.dataDir, f.id)
	r.ChallengeOutOfScope = []request.ChallengeRaised{
		{Ticket: 1, Path: "a_test.go", Criterion: "AC-1", Files: []string{"internal/payments/refunds.go"}, Source: request.ChallengeRaisedSubject},
		{Ticket: 1, Path: "b_test.go", Criterion: "AC-2", Files: []string{"internal/elsewhere/x.go"}, Source: request.ChallengeRaisedDiffScope},
	}
	got := requestdriver.ChallengeScopeBlocksForTest(r, r.Tickets[1])
	for _, want := range []string{"# Files this ticket may change", "# Raised before by an earlier ticket's challenge", "- AC-1: internal/payments/refunds.go"} {
		if !strings.Contains(got, want) {
			t.Errorf("blocks lack %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "elsewhere") || strings.Contains(got, "AC-2") {
		t.Errorf("blocks list an entry whose files are not covered:\n%s", got)
	}
}

// A held file not run for its subject is recorded once, with the subject files
// outside Allowed-Files; a file run, or not run for another reason, is not.
func TestSubjectOutsideScopeIsRecordedOnce(t *testing.T) {
	f := newAdviceFixture(t, nil)
	r := requestdrivertest.LoadRequest(t, f.dataDir, f.id)
	rec := &run.Run{Challenge: &run.ChallengeRecord{Files: []run.ChallengeFile{
		{Path: "a_test.go", Criterion: "AC-1", Subject: []string{"internal/payments/refunds.go", "internal/x/y.go"}, NotRun: "subject internal/x/y.go is outside this ticket's Allowed-Files", OutOfScope: true},
		{Path: "b_test.go", Criterion: "AC-2", Subject: []string{"internal/x/z.go"}, NotRun: "subject internal/x/z.go is not a tracked file of the result"},
		{Path: "c_test.go", Criterion: "AC-3", Subject: []string{"internal/payments/refunds.go"}},
	}}}
	if !requestdriver.RecordSubjectOutOfScopeForTest(r, &r.Tickets[0], rec) || requestdriver.RecordSubjectOutOfScopeForTest(r, &r.Tickets[0], rec) {
		t.Fatal("want the first call to add an entry and the second none")
	}
	if len(r.ChallengeOutOfScope) != 1 || r.ChallengeOutOfScope[0].Path != "a_test.go" || strings.Join(r.ChallengeOutOfScope[0].Files, ",") != "internal/x/y.go" || r.ChallengeOutOfScope[0].Source != request.ChallengeRaisedSubject {
		t.Errorf("ChallengeOutOfScope = %+v", r.ChallengeOutOfScope)
	}
}

// A diff_scope round handed several reds records none of them: the files may
// serve only one, and the subject rule is the primary gate.
func TestDiffScopeRoundWithSeveralRedsRecordsNone(t *testing.T) {
	f := newAdviceFixture(t, nil)
	r := requestdrivertest.LoadRequest(t, f.dataDir, f.id)
	files := []string{"internal/other/handler.go"}
	one := []run.ChallengeFile{{Path: "a_test.go", Criterion: "AC-1"}}
	two := append(one, run.ChallengeFile{Path: "b_test.go", Criterion: "AC-2"})
	requestdriver.RecordRoundOutOfScopeForTest(r, &r.Tickets[0], two, files)
	if len(r.ChallengeOutOfScope) != 0 {
		t.Fatalf("recorded %+v for two reds", r.ChallengeOutOfScope)
	}
	requestdriver.RecordRoundOutOfScopeForTest(r, &r.Tickets[0], one, files)
	if len(r.ChallengeOutOfScope) != 1 {
		t.Fatalf("recorded %+v for one red, want one entry", r.ChallengeOutOfScope)
	}
}
