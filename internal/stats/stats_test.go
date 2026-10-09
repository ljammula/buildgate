package stats

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"buildgate/internal/run"
)

func ptr(b bool) *bool { return &b }

// passRound is a round that passed; failRound one that failed with the given
// signature and blockers.
func passRound(i int) run.AgentEvidenceRound {
	return run.AgentEvidenceRound{Index: i, VerifyPassed: ptr(true)}
}

func failRound(i int, signature string, blockers ...string) run.AgentEvidenceRound {
	if len(blockers) == 0 {
		blockers = []string{"canonical verification failed"}
	}
	return run.AgentEvidenceRound{Index: i, VerifyPassed: ptr(false), Blockers: blockers, FailureSignature: signature}
}

func mk(id string, state run.State, created string, rounds ...run.AgentEvidenceRound) *run.Run {
	r := &run.Run{ID: id, Ticket: id, State: state, CreatedAt: created, UpdatedAt: created}
	if rounds != nil {
		r.AgentEvidence = &run.AgentEvidence{Rounds: rounds}
	}
	return r
}

func spent(r *run.Run, in, out, cost int64) *run.Run {
	r.Attempts = append(r.Attempts, run.Attempt{RelayConsumedInputTokens: in, RelayConsumedOutputTokens: out, RelayConsumedCostMicroUSD: cost})
	return r
}

func at(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

func equalRate(t *testing.T, name string, got *float64, want float64) {
	t.Helper()
	if got == nil || *got != want {
		t.Errorf("%s = %v, want %v", name, got, want)
	}
}

// TestComputeCountsByHand: every number below is worked out from the records.
func TestComputeCountsByHand(t *testing.T) {
	a := spent(mk("a-001", run.StateAccepted, "2026-10-01T00:00:00Z", passRound(1)), 100, 50, 3000)
	// b: accepted at the first run, after two failed rounds that failed the same
	// way, the second changing nothing.
	b := spent(mk("b-001", run.StateAccepted, "2026-10-01T01:00:00Z",
		failRound(1, "s1"), failRound(2, "s1", "no changes made to the workspace"), passRound(3)), 200, 0, 5000)
	// c: quarantined on two checks after two failed rounds with different
	// signatures, then a corrective round accepted after one failed round.
	c1 := mk("c-001", run.StateQuarantined, "2026-10-01T02:00:00Z", failRound(1, "x"), failRound(2, "y"))
	c1.GateResults = []run.GateResult{{Check: "canonical_verify"}, {Check: "tests_added"}, {Check: "tests_added"}, {Check: "lint", Passed: true}}
	c2 := mk("c-001-corrective1", run.StateAccepted, "2026-10-01T03:00:00Z", failRound(1, ""), passRound(2))
	d := mk("d-001", run.StateHalted, "2026-10-01T04:00:00Z")
	d.HaltReasonCode = "relay_ceiling_exceeded"
	e := mk("e-001", run.StateQuarantined, "2026-10-01T05:00:00Z")
	e.HaltReasonCode = "verify_failed"
	f := mk("f-001", run.StateAccepted, "2026-10-01T06:00:00Z", passRound(1))
	f.Overrides = []run.Override{{By: "operator"}}
	g := mk("g-001", run.StateSliceRunning, "2026-10-01T07:00:00Z")
	smoke := mk("live-smoke-mathops-20261001", run.StateAccepted, "2026-10-01T08:00:00Z", passRound(1))

	rep := Compute([]*run.Run{a, b, c1, c2, d, e, f, g, smoke, nil}, Options{Project: "app", ExcludeTicketPrefixes: []string{SmokePrefix}})
	if rep.Project != "app" || rep.ExcludedRuns != 1 || rep.Unfinished != 1 || rep.Runs != 7 {
		t.Fatalf("header = %+v", rep)
	}
	m := rep.Overall
	if m.Tickets != 6 || m.OneShot != 1 || m.Accepted != 4 {
		t.Errorf("tickets/one-shot/accepted = %d/%d/%d, want 6/1/4", m.Tickets, m.OneShot, m.Accepted)
	}
	equalRate(t, "one-shot rate", m.OneShotRate, 0.1667)
	equalRate(t, "accepted rate", m.AcceptedRate, 0.6667)
	// Accepted series rounds: a 1, b 3, c 2+2 = 4, f 1 -> 1,1,3,4.
	if want := (Spread{Series: 4, Median: 2, P90: 4}); m.RoundsToGreen != want {
		t.Errorf("rounds to green = %+v, want %+v", m.RoundsToGreen, want)
	}
	// Pairs: b (1,2) same and no-change; c1 (1,2) different; c2 round 1 is alone.
	if m.FailedRoundPairs != 2 || m.ComparablePairs != 2 || m.SameFailurePairs != 1 || m.NoChangePairs != 1 {
		t.Errorf("pairs = %d/%d/%d/%d, want 2/2/1/1", m.FailedRoundPairs, m.ComparablePairs, m.SameFailurePairs, m.NoChangePairs)
	}
	wantQ := []Count{{"(verify_failed)", 1}, {"canonical_verify", 1}, {"tests_added", 1}}
	if !equalCounts(m.QuarantinedBy, wantQ) {
		t.Errorf("quarantined by = %v, want %v", m.QuarantinedBy, wantQ)
	}
	if !equalCounts(m.HaltedBy, []Count{{"relay_ceiling_exceeded", 1}}) {
		t.Errorf("halted by = %v", m.HaltedBy)
	}
	if m.CorrectiveBuilds != (Corrective{Ran: 1, Accepted: 1}) {
		t.Errorf("corrective = %+v", m.CorrectiveBuilds)
	}
	if want := (Spend{Tokens: 350, CostMicroUSD: 8000, RunsWithSpend: 2, PerAcceptedTicketMicroUSD: 2000}); m.Spend != want {
		t.Errorf("spend = %+v, want %+v", m.Spend, want)
	}
}

func equalCounts(a, b []Count) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestRetryAndFollowUpRunsJoinTheirTicketAndAreNotOneShot(t *testing.T) {
	runs := []*run.Run{
		mk("r-001", run.StateQuarantined, "2026-10-01T00:00:00Z"),
		// A retry keeps the ticket name and carries the request id.
		mk("r-001", run.StateAccepted, "2026-10-01T01:00:00Z"),
		mk("s-001", run.StateAccepted, "2026-10-01T00:00:00Z", passRound(1)),
		mk("s-001-review2-fix1", run.StateAccepted, "2026-10-01T02:00:00Z", passRound(1)),
		// Names that only look like a follow-up are tickets of their own.
		mk("api-review1", run.StateAccepted, "2026-10-01T00:00:00Z", passRound(1)),
		mk("api-review2", run.StateQuarantined, "2026-10-01T00:00:00Z"),
	}
	runs[1].ID = "r-001-retry"
	m := Compute(runs, Options{}).Overall
	if m.Tickets != 4 || m.OneShot != 1 || m.Accepted != 3 {
		t.Errorf("tickets/one-shot/accepted = %d/%d/%d, want 4/1/3", m.Tickets, m.OneShot, m.Accepted)
	}
	if m.CorrectiveBuilds != (Corrective{Ran: 2, Accepted: 2}) {
		t.Errorf("corrective = %+v, want 2 ran, 2 accepted", m.CorrectiveBuilds)
	}
}

func TestSameTicketNameInTwoRequestsIsTwoTickets(t *testing.T) {
	x, y := mk("t-001", run.StateAccepted, "2026-10-01T00:00:00Z", passRound(1)), mk("t-001-b", run.StateAccepted, "2026-10-01T00:00:00Z", passRound(1))
	x.Ticket, y.Ticket = "t-001", "t-001"
	x.RequestID, y.RequestID = "req-1", "req-2"
	if got := Compute([]*run.Run{x, y}, Options{}).Overall; got.Tickets != 2 || got.OneShot != 2 {
		t.Errorf("tickets/one-shot = %d/%d, want 2/2", got.Tickets, got.OneShot)
	}
}

func TestATicketWhoseFirstRunIsInProgressIsNotCounted(t *testing.T) {
	runs := []*run.Run{
		mk("e-001-a", run.StateSliceRunning, "2026-10-01T00:00:00Z"),
		mk("e-001-b", run.StateAccepted, "2026-10-01T02:00:00Z", passRound(1)),
	}
	runs[0].Ticket, runs[1].Ticket = "e-001", "e-001"
	rep := Compute(runs, Options{})
	if rep.Overall.Tickets != 0 || rep.Unfinished != 1 || rep.Runs != 0 {
		t.Errorf("report = tickets %d unfinished %d runs %d", rep.Overall.Tickets, rep.Unfinished, rep.Runs)
	}
}

func TestSpreadOddAndEven(t *testing.T) {
	cases := []struct {
		in   []int
		want Spread
	}{
		{nil, Spread{}},
		{[]int{4}, Spread{Series: 1, Median: 4, P90: 4}},
		{[]int{3, 1, 2}, Spread{Series: 3, Median: 2, P90: 3}},
		{[]int{4, 1, 2, 3}, Spread{Series: 4, Median: 2.5, P90: 4}},
		{[]int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}, Spread{Series: 10, Median: 5.5, P90: 9}},
		{[]int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11}, Spread{Series: 11, Median: 6, P90: 10}},
	}
	for _, c := range cases {
		if got := spread(c.in); got != c.want {
			t.Errorf("spread(%v) = %+v, want %+v", c.in, got, c.want)
		}
	}
}

func TestPairsNeedBothRoundsFailedAndSignaturesToBeComparable(t *testing.T) {
	r := mk("p-001", run.StateQuarantined, "2026-10-01T00:00:00Z",
		failRound(1, ""), failRound(2, "s"), failRound(3, "s"), passRound(4), failRound(5, "s"))
	m := Compute([]*run.Run{r}, Options{}).Overall
	// (1,2) has an empty signature, (2,3) is the same failure, (3,4) and (4,5)
	// have a passed round.
	if m.FailedRoundPairs != 2 || m.ComparablePairs != 1 || m.SameFailurePairs != 1 || m.NoChangePairs != 0 {
		t.Errorf("pairs = %d/%d/%d/%d, want 2/1/1/0", m.FailedRoundPairs, m.ComparablePairs, m.SameFailurePairs, m.NoChangePairs)
	}
}

func TestTopListsAreCappedAndOrdered(t *testing.T) {
	var runs []*run.Run
	for i := 0; i < 12; i++ {
		r := mk(string(rune('a'+i))+"-001", run.StateHalted, "2026-10-01T00:00:00Z")
		r.HaltReasonCode = "code_" + string(rune('a'+i))
		runs = append(runs, r)
	}
	// Two more runs halting on code_l make it the largest; the rest tie on one.
	for _, id := range []string{"x-001", "y-001"} {
		r := mk(id, run.StateHalted, "2026-10-01T00:00:00Z")
		r.HaltReasonCode = "code_l"
		runs = append(runs, r)
	}
	h := Compute(runs, Options{}).Overall.HaltedBy
	if len(h) != TopN || h[0] != (Count{"code_l", 3}) || h[1] != (Count{"code_a", 1}) || h[9] != (Count{"code_i", 1}) {
		t.Errorf("halted by = %v", h)
	}
}

func TestHaltReasonFallsBackToTheTriageSentence(t *testing.T) {
	a := mk("a-001", run.StateHalted, "2026-10-01T00:00:00Z")
	a.Triage = "halted: sandbox attempts exhausted\nmore"
	b := mk("b-001", run.StateHalted, "2026-10-01T00:00:00Z")
	c := mk("c-001", run.StateHalted, "2026-10-01T00:00:00Z")
	c.Triage = "halted: " + strings.Repeat("x", 200)
	h := Compute([]*run.Run{a, b, c}, Options{}).Overall.HaltedBy
	want := []Count{{noHaltReason, 1}, {"sandbox attempts exhausted", 1}, {strings.Repeat("x", haltReasonLimit), 1}}
	if !equalCounts(h, want) {
		t.Errorf("halted by = %v, want %v", h, want)
	}
}

func seriesAt(id, created string) *run.Run {
	return mk(id, run.StateAccepted, created, passRound(1))
}

func TestBucketsKeepEmptyOnesAndPutABoundaryInTheLaterBucket(t *testing.T) {
	runs := []*run.Run{
		seriesAt("s1-001", "2026-09-30T10:00:00Z"),
		seriesAt("s2-001", "2026-10-01T10:00:00Z"),
		seriesAt("s3-001", "2026-10-10T10:00:00Z"),
		seriesAt("s4-001", "2026-10-09T00:00:00Z"),
	}
	rep := Compute(runs, Options{Now: at("2026-10-15T12:00:00Z")})
	if rep.BucketDays != 7 || len(rep.Buckets) != 3 {
		t.Fatalf("buckets = %d (days %d), want 3", len(rep.Buckets), rep.BucketDays)
	}
	starts := []string{"2026-09-25T00:00:00Z", "2026-10-02T00:00:00Z", "2026-10-09T00:00:00Z"}
	tickets := []int{2, 0, 2}
	for i, b := range rep.Buckets {
		if b.Start != starts[i] || b.Metrics.Tickets != tickets[i] {
			t.Errorf("bucket %d = %s with %d tickets, want %s with %d", i, b.Start, b.Metrics.Tickets, starts[i], tickets[i])
		}
	}
	if rep.Buckets[0].End != starts[1] || rep.Buckets[1].Metrics.OneShotRate != nil || rep.Buckets[1].Metrics.QuarantinedBy == nil {
		t.Errorf("empty bucket = %+v", rep.Buckets[1])
	}
	equalRate(t, "bucket 0 one-shot", rep.Buckets[0].Metrics.OneShotRate, 1)
	if rep.Overall.Tickets != 4 {
		t.Errorf("overall tickets = %d, want 4", rep.Overall.Tickets)
	}
}

func TestBucketsWithoutNowEndAtTheNewestTicketAndWidthIsConfigurable(t *testing.T) {
	runs := []*run.Run{seriesAt("a-001", "2026-10-01T10:00:00Z"), seriesAt("b-001", "2026-10-04T10:00:00Z")}
	rep := Compute(runs, Options{BucketDays: 2})
	// End day 10-05; two-day buckets back from it: 10-01, 10-03 -> 2 buckets
	// would start 10-01; ceil(4/2) = 2.
	if len(rep.Buckets) != 2 || rep.Buckets[0].Start != "2026-10-01T00:00:00Z" || rep.Buckets[0].Metrics.Tickets != 1 || rep.Buckets[1].Metrics.Tickets != 1 {
		t.Errorf("buckets = %+v", rep.Buckets)
	}
}

func TestAtMostTwentySixBucketsAreKeptNewestLast(t *testing.T) {
	var runs []*run.Run
	day := at("2026-01-01T12:00:00Z")
	for i := 0; i < 30; i++ {
		runs = append(runs, seriesAt("w"+string(rune('a'+i))+"-001", day.AddDate(0, 0, 7*i).Format(time.RFC3339)))
	}
	rep := Compute(runs, Options{})
	if len(rep.Buckets) != MaxBuckets {
		t.Fatalf("buckets = %d, want %d", len(rep.Buckets), MaxBuckets)
	}
	if rep.Buckets[0].Start != "2026-01-23T00:00:00Z" || rep.Buckets[MaxBuckets-1].End != "2026-07-24T00:00:00Z" {
		t.Errorf("first %s, last end %s", rep.Buckets[0].Start, rep.Buckets[MaxBuckets-1].End)
	}
	for i, b := range rep.Buckets {
		if b.Metrics.Tickets != 1 {
			t.Errorf("bucket %d holds %d tickets, want 1", i, b.Metrics.Tickets)
		}
	}
	// The four oldest tickets are in the overall numbers only.
	if rep.Overall.Tickets != 30 {
		t.Errorf("overall tickets = %d, want 30", rep.Overall.Tickets)
	}
}

func TestSinceAndUntilBoundATicketByItsFirstRun(t *testing.T) {
	runs := []*run.Run{
		seriesAt("s1-001", "2026-09-30T10:00:00Z"),
		seriesAt("s2-001", "2026-10-01T00:00:00Z"),
		seriesAt("s4-001", "2026-10-09T00:00:00Z"),
		seriesAt("s3-001", "2026-10-10T00:00:00Z"),
	}
	rep := Compute(runs, Options{Since: at("2026-10-01T00:00:00Z"), Until: at("2026-10-10T00:00:00Z")})
	if rep.Overall.Tickets != 2 || rep.Runs != 2 || rep.Since != "2026-10-01T00:00:00Z" || rep.Until != "2026-10-10T00:00:00Z" {
		t.Errorf("report = tickets %d runs %d since %q until %q", rep.Overall.Tickets, rep.Runs, rep.Since, rep.Until)
	}
	total := 0
	for _, b := range rep.Buckets {
		total += b.Metrics.Tickets
	}
	if len(rep.Buckets) != 2 || total != 2 {
		t.Errorf("buckets = %d holding %d tickets, want 2 holding 2", len(rep.Buckets), total)
	}
}

func TestNoRunsGivesAnEmptyReportThatEncodesAsArraysAndNulls(t *testing.T) {
	rep := Compute(nil, Options{Project: "app"})
	if rep.Overall.Tickets != 0 || rep.Overall.OneShotRate != nil || rep.Overall.AcceptedRate != nil || rep.Runs != 0 {
		t.Errorf("report = %+v", rep)
	}
	b, err := json.Marshal(rep)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"buckets":[]`, `"quarantined_by":[]`, `"halted_by":[]`, `"one_shot_rate":null`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("JSON lacks %s: %s", want, b)
		}
	}
}

func TestExcludedPrefixesMatchTicketOrID(t *testing.T) {
	a := mk("live-smoke-a-20261001", run.StateAccepted, "2026-10-01T00:00:00Z", passRound(1))
	b := mk("x-001", run.StateAccepted, "2026-10-01T00:00:00Z", passRound(1))
	b.Ticket = "live-smoke-b"
	c := mk("keep-001", run.StateAccepted, "2026-10-01T00:00:00Z", passRound(1))
	rep := Compute([]*run.Run{a, b, c}, Options{ExcludeTicketPrefixes: []string{SmokePrefix, ""}})
	if rep.ExcludedRuns != 2 || rep.Overall.Tickets != 1 {
		t.Errorf("excluded %d, tickets %d, want 2 and 1", rep.ExcludedRuns, rep.Overall.Tickets)
	}
}

// The next two tests copy the fixtures of scripts/tests/test_baseline.py and
// assert its documented expected numbers for the fields both define.

func TestParityBaselineOneShotFixture(t *testing.T) {
	runs := []*run.Run{
		mk("z-a-1", run.StateQuarantined, "2026-10-01T00:00:00Z"),
		mk("a-2", run.StateAccepted, "2026-10-01T01:00:00Z"),
		mk("b-1", run.StateAccepted, "2026-10-01T00:00:00Z"),
		mk("c-1", run.StateAccepted, "2026-10-01T00:00:00Z"),
		mk("d-1", run.StateAccepted, "2026-10-01T00:00:00Z"),
		mk("e-1", run.StateSliceRunning, "2026-10-01T00:00:00Z"),
		mk("e-2", run.StateAccepted, "2026-10-01T02:00:00Z"),
	}
	runs[0].Ticket, runs[1].Ticket = "a", "a"
	runs[4].Overrides = []run.Override{{By: "operator"}}
	runs[3].Rescues = []run.Rescue{{By: "operator"}}
	runs[5].Ticket, runs[6].Ticket = "e", "e"
	rep := Compute(runs, Options{})
	// baseline: one_shot 1 of tickets 4, runs 7, finished 6, unfinished 1.
	if rep.Overall.OneShot != 1 || rep.Overall.Tickets != 4 || rep.Unfinished != 1 {
		t.Errorf("one-shot %d of %d, unfinished %d; baseline expects 1 of 4 and 1", rep.Overall.OneShot, rep.Overall.Tickets, rep.Unfinished)
	}
	equalRate(t, "one-shot rate", rep.Overall.OneShotRate, 0.25)
}

func TestParityBaselineRecordedSignatureAndStopFixtures(t *testing.T) {
	rounds := []run.AgentEvidenceRound{
		failRound(1, "aaaa"), failRound(2, "aaaa"), failRound(3, "bbbb"), passRound(4),
		failRound(5, "cccc", "no changes made to the workspace"), failRound(6, "cccc", "no changes made to the workspace"),
	}
	r := mk("r", run.StateQuarantined, "2026-10-01T00:00:00Z", rounds...)
	m := Compute([]*run.Run{r}, Options{}).Overall
	// baseline: pairs 3, second changed nothing 1, recorded same 2, different 1.
	if m.FailedRoundPairs != 3 || m.NoChangePairs != 1 || m.SameFailurePairs != 2 || m.ComparablePairs-m.SameFailurePairs != 1 {
		t.Errorf("pairs = %+v", m)
	}

	q1 := mk("q1", run.StateQuarantined, "2026-10-01T00:00:00Z")
	q1.GateResults = []run.GateResult{{Check: "canonical_verify", Passed: true}, {Check: "code_review"}, {Check: "tests_added"}, {Check: "code_review"}}
	q2 := mk("q2", run.StateQuarantined, "2026-10-01T00:00:00Z")
	q2.GateResults = []run.GateResult{{Check: "code_review"}}
	q3 := mk("q3", run.StateQuarantined, "2026-10-01T00:00:00Z")
	q3.HaltReasonCode = "verify_failed"
	q4 := mk("q4", run.StateQuarantined, "2026-10-01T00:00:00Z")
	h1 := mk("h1", run.StateHalted, "2026-10-01T00:00:00Z")
	h1.HaltReasonCode, h1.Triage = "relay_ceiling_exceeded", "halted: spend ceiling"
	h2 := mk("h2", run.StateHalted, "2026-10-01T00:00:00Z")
	h2.Triage = "halted: sandbox attempts exhausted\nmore"
	h3 := mk("h3", run.StateHalted, "2026-10-01T00:00:00Z")
	a1 := mk("a1", run.StateAccepted, "2026-10-01T00:00:00Z")
	a1.GateResults = []run.GateResult{{Check: "lint"}}
	got := Compute([]*run.Run{q1, q2, q3, q4, h1, h2, h3, a1}, Options{}).Overall
	// baseline: quarantine_checks {code_review 2, tests_added 1, (verify_failed) 1, none 1};
	// halt_reasons {relay_ceiling_exceeded 1, sandbox attempts exhausted 1, none 1}.
	wantQ := []Count{{"code_review", 2}, {noFailedCheck, 1}, {"(verify_failed)", 1}, {"tests_added", 1}}
	wantH := []Count{{noHaltReason, 1}, {"relay_ceiling_exceeded", 1}, {"sandbox attempts exhausted", 1}}
	if !equalCounts(got.QuarantinedBy, wantQ) || !equalCounts(got.HaltedBy, wantH) {
		t.Errorf("quarantined by %v, halted by %v", got.QuarantinedBy, got.HaltedBy)
	}
}

func TestParseSinceAndBucketDays(t *testing.T) {
	now := at("2026-10-09T12:00:00Z")
	for text, want := range map[string]string{"": "", "30d": "2026-09-09T12:00:00Z", "2026-09-01": "2026-09-01T00:00:00Z"} {
		got, err := ParseSince(text, now)
		if err != nil || formatTime(got) != want {
			t.Errorf("ParseSince(%q) = %v, %v; want %q", text, got, err, want)
		}
	}
	for _, bad := range []string{"0d", "-3d", "xd", "2026-13-01", "yesterday", "99999d"} {
		if _, err := ParseSince(bad, now); err == nil {
			t.Errorf("ParseSince(%q) accepted", bad)
		}
	}
	for text, want := range map[string]int{"": 7, "1": 1, "14": 14} {
		if got, err := ParseBucketDays(text); err != nil || got != want {
			t.Errorf("ParseBucketDays(%q) = %d, %v", text, got, err)
		}
	}
	for _, bad := range []string{"0", "-1", "x", "366"} {
		if _, err := ParseBucketDays(bad); err == nil {
			t.Errorf("ParseBucketDays(%q) accepted", bad)
		}
	}
}
