package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"buildgate/internal/run"
	"buildgate/internal/stats"
)

func statsPtr(b bool) *bool { return &b }

func statsPass(i int) run.AgentEvidenceRound {
	return run.AgentEvidenceRound{Index: i, VerifyPassed: statsPtr(true)}
}

func statsFail(i int, sig string) run.AgentEvidenceRound {
	return run.AgentEvidenceRound{Index: i, VerifyPassed: statsPtr(false), Blockers: []string{"canonical verification failed"}, FailureSignature: sig}
}

// statsFixtureDataDir writes, under two projects:
//
//	app:   t-001 accepted in one round; t-002 quarantined on tests_added then a
//	       corrective round accepted (two rounds, failed the same way twice before);
//	       t-003 halted; a live-smoke run; one run still in progress.
//	other: o-001 accepted in one round.
func statsFixtureDataDir(t *testing.T) string {
	t.Helper()
	dataDir := t.TempDir()
	save := func(r run.Run) {
		t.Helper()
		if err := r.Save(dataDir); err != nil {
			t.Fatalf("save %s: %v", r.ID, err)
		}
	}
	round := func(rs ...run.AgentEvidenceRound) *run.AgentEvidence { return &run.AgentEvidence{Rounds: rs} }
	save(run.Run{ID: "t-001", Ticket: "t-001", Project: "app", State: run.StateAccepted, CreatedAt: "2026-10-01T10:00:00Z", AgentEvidence: round(statsPass(1))})
	save(run.Run{ID: "t-002", Ticket: "t-002", Project: "app", State: run.StateQuarantined, CreatedAt: "2026-10-02T10:00:00Z",
		AgentEvidence: round(statsFail(1, "aa"), statsFail(2, "aa")), GateResults: []run.GateResult{{Check: "tests_added"}}})
	save(run.Run{ID: "t-002-corrective1", Ticket: "t-002-corrective1", Project: "app", State: run.StateAccepted, CreatedAt: "2026-10-02T12:00:00Z", AgentEvidence: round(statsPass(1))})
	save(run.Run{ID: "t-003", Ticket: "t-003", Project: "app", State: run.StateHalted, CreatedAt: "2026-10-09T10:00:00Z", HaltReasonCode: "relay_ceiling_exceeded"})
	save(run.Run{ID: "live-smoke-m-1", Ticket: "live-smoke-m-1", Project: "app", State: run.StateAccepted, CreatedAt: "2026-10-09T11:00:00Z"})
	save(run.Run{ID: "t-004", Ticket: "t-004", Project: "app", State: run.StateSliceRunning, CreatedAt: "2026-10-09T12:00:00Z"})
	save(run.Run{ID: "o-001", Ticket: "o-001", Project: "other", State: run.StateAccepted, CreatedAt: "2026-10-05T10:00:00Z", AgentEvidence: round(statsPass(1))})
	return dataDir
}

var statsNow = time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)

func runStats(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := statsRun(args, &out, statsNow)
	return out.String(), err
}

func TestStatsSummaryTablePrintsOneRowPerProjectAndAnOverallRow(t *testing.T) {
	dir := statsFixtureDataDir(t)
	got, err := runStats(t, "-data-dir", dir)
	if err != nil {
		t.Fatal(err)
	}
	want := `PROJECT  TICKETS  ONE-SHOT   ACCEPTED   MEDIAN ROUNDS  TOP QUARANTINE CHECK
app      3        1/3 (33%)  2/3 (67%)  2              tests_added (1)
other    1        1/1 (100%) 1/1 (100%) 1              -
overall  4        2/4 (50%)  3/4 (75%)  1              tests_added (1)

1 live-smoke run(s) not counted; -all counts them.
1 run(s) still in progress not counted.
`
	if normalizeSpaces(got) != normalizeSpaces(want) {
		t.Errorf("summary =\n%s\nwant\n%s", got, want)
	}
	all, err := runStats(t, "-data-dir", dir, "-all")
	if err != nil || !strings.Contains(all, "\napp ") || strings.Contains(all, "live-smoke run(s) not counted") || !strings.Contains(normalizeSpaces(all), "app 4 ") {
		t.Errorf("-all = %q (%v)", all, err)
	}
}

// normalizeSpaces makes the comparison independent of column padding.
func normalizeSpaces(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = strings.Join(strings.Fields(l), " ")
	}
	return strings.Join(lines, "\n")
}

func TestStatsProjectFormPrintsTheBlockTheBucketTableAndTheLists(t *testing.T) {
	dir := statsFixtureDataDir(t)
	got, err := runStats(t, "-data-dir", dir, "-project", "app")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"project app: 3 ticket(s) over 4 finished run(s)",
		"one-shot 1/3 (33%)",
		"accepted 2/3 (67%)",
		"rounds to green median 2, p90 3 over 2 accepted ticket(s)",
		"same failure twice 1/1 (100%) of failed-round pairs that recorded a signature",
		"round changed no file 0/1 (0%) of failed-round pairs",
		"corrective builds 1 ran, 1 accepted",
		"WEEK OF TICKETS ONE-SHOT % ACCEPTED % MEDIAN ROUNDS SAME-FAILURE %",
		"2026-09-27 2 1/2 (50%) 2/2 (100%) 2 1/1 (100%)",
		"2026-10-04 1 0/1 (0%) 0/1 (0%) - -",
		"quarantined by:\ntests_added 1",
		"halted by:\nrelay_ceiling_exceeded 1",
	} {
		if !strings.Contains(normalizeSpaces(got), want) {
			t.Errorf("project output lacks %q:\n%s", want, got)
		}
	}
	empty, err := runStats(t, "-data-dir", dir, "-project", "nothing")
	if err != nil || !strings.Contains(empty, "0 ticket(s)") || !strings.Contains(normalizeSpaces(empty), "one-shot -") || !strings.Contains(empty, "(none)") {
		t.Errorf("an unknown project = %q (%v)", empty, err)
	}
}

func TestStatsSinceAndBucketNarrowTheReport(t *testing.T) {
	dir := statsFixtureDataDir(t)
	got, err := runStats(t, "-data-dir", dir, "-project", "app", "-since", "2026-10-05", "-bucket", "3")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "1 ticket(s)") || !strings.Contains(got, "3 DAYS FROM") || !strings.Contains(got, "on or after 2026-10-05") {
		t.Errorf("output = %s", got)
	}
}

func TestStatsJSONForBothForms(t *testing.T) {
	dir := statsFixtureDataDir(t)
	out, err := runStats(t, "-data-dir", dir, "-json")
	if err != nil {
		t.Fatal(err)
	}
	var overview statsOverview
	if err := json.Unmarshal([]byte(out), &overview); err != nil {
		t.Fatalf("decode: %v: %s", err, out)
	}
	if len(overview.Projects) != 2 || overview.Projects[0].Project != "app" || overview.Overall.Overall.Tickets != 4 || overview.Overall.ExcludedRuns != 1 {
		t.Errorf("overview = %+v", overview)
	}
	one, err := runStats(t, "-data-dir", dir, "-json", "-project", "app", "-all")
	if err != nil {
		t.Fatal(err)
	}
	var report stats.Report
	if err := json.Unmarshal([]byte(one), &report); err != nil || report.Project != "app" || report.Overall.Tickets != 4 || report.ExcludedRuns != 0 {
		t.Errorf("report = %+v (%v)", report, err)
	}
}

func TestStatsFlagErrors(t *testing.T) {
	dir := statsFixtureDataDir(t)
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"-data-dir", dir, "-since", "soon"}, `since "soon"`},
		{[]string{"-data-dir", dir, "-since", "0d"}, `since "0d"`},
		{[]string{"-data-dir", dir, "-bucket", "0"}, "-bucket 0"},
		{[]string{"-data-dir", dir, "extra"}, `takes no arguments, got "extra"`},
		{[]string{"-nope"}, "flag provided but not defined"},
	} {
		_, err := runStats(t, c.args...)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%v: err = %v, want %q", c.args, err, c.want)
		}
	}
}

func TestStatsOnADataDirWithoutRuns(t *testing.T) {
	got, err := runStats(t, "-data-dir", t.TempDir())
	if err != nil || !strings.Contains(normalizeSpaces(got), "overall 0 - - - -") {
		t.Errorf("empty data dir = %q (%v)", got, err)
	}
}

func TestShare(t *testing.T) {
	for _, c := range []struct {
		part, whole int
		want        string
	}{{3, 8, "3/8 (38%)"}, {0, 0, "-"}, {0, 4, "0/4 (0%)"}, {4, 4, "4/4 (100%)"}} {
		if got := share(c.part, c.whole); got != c.want {
			t.Errorf("share(%d, %d) = %q, want %q", c.part, c.whole, got, c.want)
		}
	}
}
