package observation

import (
	"reflect"
	"strings"
	"testing"

	"buildgate/internal/run"
)

func boolPtr(b bool) *bool { return &b }

func failed(index int, signature string, blockers ...string) run.AgentEvidenceRound {
	if len(blockers) == 0 {
		blockers = []string{"canonical verification failed"}
	}
	return run.AgentEvidenceRound{Index: index, VerifyPassed: boolPtr(false), Blockers: blockers, FailureSignature: signature, ChangedFiles: []string{"sum.go"}}
}

func passed(index int, files ...string) run.AgentEvidenceRound {
	return run.AgentEvidenceRound{Index: index, VerifyPassed: boolPtr(true), Blockers: []string{}, ChangedFiles: files}
}

func finished(id string, state run.State, rounds ...run.AgentEvidenceRound) *run.Run {
	r := &run.Run{ID: id, Ticket: "ticket-" + id, State: state, UpdatedAt: "2026-10-08T10:00:00Z"}
	if rounds != nil {
		r.AgentEvidence = &run.AgentEvidence{Rounds: rounds}
	}
	return r
}

func kinds(observations []Observation) []string {
	out := make([]string, 0, len(observations))
	for _, o := range observations {
		out = append(out, o.Kind)
	}
	return out
}

func TestFromRunsAFailureThenAPassIsOneFixedObservationWithItsOutput(t *testing.T) {
	r := finished("r1", run.StateAccepted, failed(1, "aaaa"), passed(2, "sum.go", "sum_test.go"))
	report := FromRuns("app", []*run.Run{r}, func(runID string, round int) (string, string) {
		if runID != "r1" || round != 1 {
			t.Errorf("round log asked for %s round %d, want r1 round 1", runID, round)
		}
		return "round-logs/round-1/verify.log", "ok  pkg/a\n--- FAIL: TestSum (0.00s)\n    sum_test.go:9: got 3, want 9\nFAIL\n"
	})
	got := report.Observations
	if len(got) != 1 {
		t.Fatalf("observations = %+v, want one", got)
	}
	want := Observation{
		Kind: KindFixedAfterFailure, RunID: "r1", Ticket: "ticket-r1", At: "2026-10-08T10:00:00Z",
		What:         "Round 1 failed (canonical verification failed); round 2 passed after changing sum.go, sum_test.go.",
		Rounds:       []int{1, 2},
		Blockers:     []string{"canonical verification failed"},
		ChangedFiles: []string{"sum.go", "sum_test.go"},
		Log:          "round-logs/round-1/verify.log",
		Excerpt:      "--- FAIL: TestSum (0.00s)\n    sum_test.go:9: got 3, want 9\nFAIL",
		logRound:     1,
	}
	if !reflect.DeepEqual(got[0], want) {
		t.Errorf("observation =\n%+v\nwant\n%+v", got[0], want)
	}
}

// Each observation speaks for the rounds it names: a repeat or an idle
// round carries its own blockers and output, not those of a later round
// that failed differently, and every repeat in a streak is reported.
func TestFromRunAttributesBlockersAndOutputToTheRoundsNamed(t *testing.T) {
	r := finished("r1", run.StateAccepted,
		failed(1, "a"),
		failed(2, "a", "no changes made to the workspace", "canonical verification failed"),
		failed(3, "b", "pi invocation timed out"),
		failed(4, "c", "reference oracle failed"),
		failed(5, "c", "reference oracle failed"),
		passed(6, "a.go"),
	)
	got := FromRun(r)
	if want := []string{KindRepeatedFailure, KindRepeatedFailure, KindRoundChangedNothing, KindFixedAfterFailure}; !reflect.DeepEqual(kinds(got), want) {
		t.Fatalf("kinds = %v, want %v", kinds(got), want)
	}
	first, second, idle, fixed := got[0], got[1], got[2], got[3]
	if !reflect.DeepEqual(first.Rounds, []int{1, 2}) || first.logRound != 2 || !strings.Contains(first.What, "canonical verification failed") || strings.Contains(first.What, "timed out") {
		t.Errorf("first repeat = %+v, want rounds 1 and 2 with round 2's blockers and output", first)
	}
	if !reflect.DeepEqual(second.Rounds, []int{4, 5}) || second.logRound != 5 || !reflect.DeepEqual(second.Blockers, []string{"reference oracle failed"}) {
		t.Errorf("second repeat = %+v, want rounds 4 and 5", second)
	}
	if !reflect.DeepEqual(idle.Rounds, []int{2}) || idle.logRound != 2 || idle.What != "Round 2 ended without changing any file." || len(idle.Blockers) != 2 {
		t.Errorf("idle = %+v, want round 2 alone, with its own blockers", idle)
	}
	if fixed.logRound != 5 || !reflect.DeepEqual(fixed.Rounds, []int{1, 2, 3, 4, 5, 6}) {
		t.Errorf("fixed = %+v, want the streak's last failure illustrated", fixed)
	}
}

func TestFromRunRepeatsAndIdleRoundsWithinOneStreak(t *testing.T) {
	r := finished("r1", run.StateQuarantined,
		failed(1, "aaaa"),
		failed(2, "bbbb", "no changes made to the workspace", "canonical verification failed"),
		failed(3, "bbbb", "no changes made to the workspace", "canonical verification failed"),
		failed(4, ""),
		failed(5, ""),
	)
	r.GateResults = []run.GateResult{{Check: "canonical_verify", Passed: false, ExitCode: 1}}
	got := FromRun(r)
	if want := []string{KindRepeatedFailure, KindRoundChangedNothing, KindCheckFailed}; !reflect.DeepEqual(kinds(got), want) {
		t.Fatalf("kinds = %v, want %v", kinds(got), want)
	}
	if !reflect.DeepEqual(got[0].Rounds, []int{2, 3}) || got[0].What != "Rounds 2 and 3 failed the same way (no changes made to the workspace; canonical verification failed)." {
		t.Errorf("repeat = %+v", got[0])
	}
	if !reflect.DeepEqual(got[1].Rounds, []int{2, 3}) || got[1].What != "Rounds 2 and 3 ended without changing any file." {
		t.Errorf("idle = %+v", got[1])
	}
	if got[2].Check != "canonical_verify" || got[2].ExitCode != 1 || got[2].What != "The run was quarantined; its canonical_verify check failed (exit 1)." {
		t.Errorf("check = %+v", got[2])
	}
	// The build's last round failed: the check's observation shows why.
	if got[2].logRound != 5 || !reflect.DeepEqual(got[2].Blockers, []string{"canonical verification failed"}) {
		t.Errorf("check = %+v, want the last round's blockers and output", got[2])
	}
}

// Two rounds with no signature are not "the same failure": an older record
// has none, and an empty value says nothing.
func TestFromRunRoundsWithoutASignatureAreNeverARepeat(t *testing.T) {
	old := run.AgentEvidenceRound{Index: 1, VerifyPassed: boolPtr(false)}
	older := run.AgentEvidenceRound{Index: 2, VerifyPassed: boolPtr(false)}
	got := FromRun(finished("r1", run.StateAccepted, old, older, run.AgentEvidenceRound{Index: 3, VerifyPassed: boolPtr(true)}))
	if want := []string{KindFixedAfterFailure}; !reflect.DeepEqual(kinds(got), want) {
		t.Fatalf("kinds = %v, want %v", kinds(got), want)
	}
	if got[0].What != "Rounds 1 and 2 failed; round 3 passed." {
		t.Errorf("What = %q", got[0].What)
	}
}

func TestFromRunTwoStreaksAreTwoObservations(t *testing.T) {
	r := finished("r1", run.StateAccepted, failed(1, "a"), passed(2, "a.go"), failed(3, "b"), passed(4, "b.go"))
	got := FromRun(r)
	if len(got) != 2 || !reflect.DeepEqual(got[0].Rounds, []int{1, 2}) || !reflect.DeepEqual(got[1].Rounds, []int{3, 4}) {
		t.Errorf("observations = %+v, want one per streak", got)
	}
}

func TestFromRunQuarantineAndHaltWithoutDetail(t *testing.T) {
	q := finished("q", run.StateQuarantined)
	q.Triage = "The verify command failed on TestKey in every round.\nmore"
	q.GateResults = []run.GateResult{{Check: "lint", Passed: true}, {Check: "code_review", Passed: false, ExitCode: 3}, {Check: "code_review", Passed: false, ExitCode: 3}}
	got := FromRun(q)
	if len(got) != 1 || got[0].Check != "code_review" {
		t.Errorf("a check failing twice = %+v, want one observation", got)
	}
	q.GateResults = nil
	if got := FromRun(q); len(got) != 1 || got[0].What != "The run was quarantined: The verify command failed on TestKey in every round." {
		t.Errorf("quarantine with no failed check = %+v", got)
	}
	h := finished("h", run.StateHalted)
	h.HaltReasonCode, h.Triage = "relay_ceiling_exceeded", "halted: spend ceiling"
	if got := FromRun(h); len(got) != 1 || got[0].Kind != KindRunHalted || got[0].What != "The run halted: relay_ceiling_exceeded." {
		t.Errorf("halt = %+v", got)
	}
	h.HaltReasonCode = ""
	if got := FromRun(h); got[0].What != "The run halted: spend ceiling." {
		t.Errorf("halt with a triage sentence only = %+v", got)
	}
	h.Triage = ""
	if got := FromRun(h); got[0].What != "The run halted: no reason recorded." {
		t.Errorf("halt with nothing = %+v", got)
	}
}

func TestFromRunsCountsOrdersAndSkipsUnfinishedRuns(t *testing.T) {
	clean := finished("clean", run.StateAccepted, passed(1, "a.go"))
	rescued := finished("rescued", run.StateAccepted, passed(1, "a.go"))
	rescued.Rescues = []run.Rescue{{}}
	early := finished("early", run.StateAccepted, failed(1, "a"), passed(2, "a.go"))
	early.UpdatedAt = "2026-10-08T01:50:00-05:00" // 06:50Z
	late := finished("late", run.StateHalted)
	late.UpdatedAt = "2026-10-08T01:10:00-06:00" // 07:10Z, though its text sorts first
	running := finished("running", run.StateSliceRunning, failed(1, "a"))

	report := FromRuns("app", []*run.Run{clean, early, nil, running, late, rescued}, nil)
	if report.Project != "app" || report.Runs != 4 || report.AcceptedFirstRound != 1 || report.Truncated {
		t.Errorf("report = %+v, want 4 finished runs, one accepted in its first round", report)
	}
	if got, want := kinds(report.Observations), []string{KindRunHalted, KindFixedAfterFailure}; !reflect.DeepEqual(got, want) {
		t.Errorf("order = %v, want the later run first: %v", got, want)
	}
	wantCounts := map[string]int{KindFixedAfterFailure: 1, KindRepeatedFailure: 0, KindRoundChangedNothing: 0, KindCheckFailed: 0, KindRunHalted: 1}
	if !reflect.DeepEqual(report.Counts, wantCounts) {
		t.Errorf("Counts = %v, want %v", report.Counts, wantCounts)
	}
	if empty := FromRuns("none", nil, nil); empty.Observations == nil || len(empty.Counts) != len(Kinds) {
		t.Errorf("empty report = %+v, want an empty list and a zero for every kind", empty)
	}
}

// A long history is counted whole and listed up to the cap, and a round's
// output is read only for what is listed: a trailing failed round that
// yields no observation, and everything past the cap, cost no read.
func TestFromRunsCutsTheListKeepsTheCountsAndReadsOnlyWhatItLists(t *testing.T) {
	var runs []*run.Run
	for i := 0; i < MaxObservations+7; i++ {
		runs = append(runs, finished("f"+strings.Repeat("x", i), run.StateAccepted, failed(1, "a"), passed(2, "a.go"), failed(3, "b")))
	}
	reads := 0
	report := FromRuns("app", runs, func(string, int) (string, string) {
		reads++
		return "round-logs/round-1/verify.log", "FAIL"
	})
	if len(report.Observations) != MaxObservations || !report.Truncated || report.Counts[KindFixedAfterFailure] != MaxObservations+7 {
		t.Errorf("list %d, truncated %v, count %d", len(report.Observations), report.Truncated, report.Counts[KindFixedAfterFailure])
	}
	if reads != MaxObservations {
		t.Errorf("read %d round logs, want one per listed observation (%d)", reads, MaxObservations)
	}
}

func TestBecauseNamesAFewBlockers(t *testing.T) {
	got := because([]string{"a", "b", "c", "d", "e", "f"})
	if got != " (a; b; c; d; and 2 more)" {
		t.Errorf("because = %q", got)
	}
}

func TestExcerptPicksFailureLinesCleansAndCuts(t *testing.T) {
	text := "go build ./...\nok  \tpkg/a\t0.01s\n\x1b[31m--- FAIL: TestSum (0.00s)\x1b[0m\n    sum_test.go:9: got 3, want 9\nAuthorization: Bearer abcdef0123456789 error\nFAIL\tpkg/b\n"
	got := Excerpt(text)
	if strings.Contains(got, "\x1b") || strings.Contains(got, "abcdef0123456789") || strings.Contains(got, "go build") {
		t.Errorf("Excerpt = %q, want failure lines only, no escapes, no credential", got)
	}
	if !strings.HasPrefix(got, "--- FAIL: TestSum (0.00s)\n    sum_test.go:9: got 3, want 9\n") || !strings.HasSuffix(got, "FAIL\tpkg/b") {
		t.Errorf("Excerpt = %q", got)
	}
	if got := Excerpt("ok\n\x1b[31merror: coloured\x1b[0m\nok\n"); got != "error: coloured" {
		t.Errorf("a coloured failure line: %q, want it picked", got)
	}
	if got := Excerpt("line one\nline two\n"); got != "line one\nline two" {
		t.Errorf("no failure line: %q, want the last lines", got)
	}
	long := strings.Repeat("error: "+strings.Repeat("é", 200)+"\n", 40)
	cut := Excerpt(long)
	if len(cut) > maxExcerptBytes || strings.Count(cut, "\n") >= maxExcerptLines || !strings.HasPrefix(cut, "error: é") {
		t.Errorf("long excerpt: %d bytes, %d lines", len(cut), strings.Count(cut, "\n")+1)
	}
	if Excerpt("") != "" {
		t.Error("empty text gave an excerpt")
	}
}
