package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"buildgate/internal/consolelink"
	"buildgate/internal/progress"
	"buildgate/internal/request"
	"buildgate/internal/run"
)

var unnamedBaseline = &run.BaselineVerify{
	Command: "python -m pytest -q tests", BaseSHA: "2247b35ea1c4aaaa", ExitCode: 1,
	FailingTests: []string{"tests/test_pager.py::test_less", "tests/test_pager.py::test_more"}, FailingCount: 2,
	Unnamed: []string{"tests/test_pager.py::test_less", "tests/test_pager.py::test_more"}, UnnamedCount: 2,
}

const unnamedBaselineSummary = "failed: tests/test_pager.py::test_less and 1 more; the ticket names none of them"

// TestStatusShowsARunsBaselineVerify: a row is followed by the result, in
// the table and in -json, for a run that passed it and one that did not.
func TestStatusShowsARunsBaselineVerify(t *testing.T) {
	now := time.Now()
	created := now.Add(-time.Minute).Format(time.RFC3339)
	passed := buildStatusEntry(&run.Run{ID: "r1", State: run.StateAccepted, CreatedAt: created, BaselineVerify: &run.BaselineVerify{Passed: true}}, now)
	if passed.Baseline != "passed" {
		t.Errorf("Baseline = %q, want passed", passed.Baseline)
	}
	failed := buildStatusEntry(&run.Run{ID: "r2", State: run.StateHalted, CreatedAt: created, BaselineVerify: unnamedBaseline}, now)
	if failed.Baseline != unnamedBaselineSummary {
		t.Errorf("Baseline = %q, want %q", failed.Baseline, unnamedBaselineSummary)
	}
	if lines := statusRunDetailLines(failed); len(lines) != 1 || lines[0] != "baseline verify: "+unnamedBaselineSummary {
		t.Errorf("detail lines = %q", lines)
	}
	none := buildStatusEntry(&run.Run{ID: "r3", State: run.StateSliceRunning, CreatedAt: created}, now)
	if none.Baseline != "" || len(statusRunDetailLines(none)) != 0 {
		t.Errorf("a run with no baseline yet shows one: %+v", none)
	}
}

func TestWatchShowsTheBaselineVerifyResultLiveAndInTheRecap(t *testing.T) {
	t0 := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	events := []progress.Event{
		{Ts: t0.Format(time.RFC3339Nano), Source: "factory", Stage: run.BaselineVerifyAttemptKind, Event: "start"},
		{Ts: t0.Add(21 * time.Second).Format(time.RFC3339Nano), Source: "factory", Stage: run.BaselineVerifyAttemptKind, Event: "end", Outcome: "fail", Detail: unnamedBaselineSummary},
	}
	lines := renderRunEvents(events, t0)
	if len(lines) != 2 || !strings.Contains(lines[1], "baseline_verify") || !strings.HasSuffix(lines[1], unnamedBaselineSummary+" (21s)") {
		t.Errorf("live lines = %q, want the end line to name the failing test", lines)
	}

	var recap bytes.Buffer
	printRunRecap(&recap, &run.Run{ID: "r2", Ticket: "t", State: run.StateHalted, BaselineVerify: unnamedBaseline}, t.TempDir())
	if want := "  baseline verify: " + unnamedBaselineSummary + "\n"; !strings.Contains(recap.String(), want) {
		t.Errorf("recap lacks %q:\n%s", want, recap.String())
	}
	recap.Reset()
	printRunRecap(&recap, &run.Run{ID: "r1", Ticket: "t", State: run.StateAccepted, BaselineVerify: &run.BaselineVerify{Passed: true}}, t.TempDir())
	if !strings.Contains(recap.String(), "  baseline verify: passed\n") {
		t.Errorf("recap of a run that passed its baseline:\n%s", recap.String())
	}
}

// TestInboxShowsTheBaselineVerifyOfEachBuiltTicket: read from the ticket's
// run, whose record is beside its run.json until the run's last save.
func TestInboxShowsTheBaselineVerifyOfEachBuiltTicket(t *testing.T) {
	f := newProfileFixture(t, "default")
	t.Setenv(consolelink.EnvVar, "")
	dataDir := f.roots["default"]
	for id, b := range map[string]*run.BaselineVerify{"run-1": {Passed: true}, "run-2": unnamedBaseline} {
		if err := (&run.Run{ID: id, State: run.StateHalted}).Save(dataDir); err != nil {
			t.Fatal(err)
		}
		if err := run.SaveBaselineVerify(run.Dir(dataDir, id), b); err != nil {
			t.Fatal(err)
		}
	}
	saveWaitingRequest(t, dataDir, "halt-a", "Colour output", request.StateHalted, time.Hour, func(r *request.Request) {
		r.Error = "ticket 2/3 halted"
		r.Tickets = []request.Ticket{{Index: 1, RunID: "run-1"}, {Index: 2, RunID: "run-2"}, {Index: 3}}
	})
	var out, warn bytes.Buffer
	if err := inboxRun(nil, &out, &warn); err != nil {
		t.Fatalf("inboxRun: %v", err)
	}
	if want := "  baseline verify: ticket 1 passed; ticket 2 " + unnamedBaselineSummary + "\n"; !strings.Contains(out.String(), want) {
		t.Errorf("inbox lacks %q:\n%s", want, out.String())
	}
}

// TestSaveWordsABaselineHaltForTheOperator: the halt reaches save with
// Temporal's error chain; the record replaces it with what to do.
func TestSaveWordsABaselineHaltForTheOperator(t *testing.T) {
	dataDir := t.TempDir()
	r := &run.Run{ID: "r2", State: run.StateSliceRunning, CreatedAt: time.Now().Format(time.RFC3339)}
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	if err := run.SaveBaselineVerify(run.Dir(dataDir, "r2"), unnamedBaseline); err != nil {
		t.Fatal(err)
	}
	r.State = run.StateHalted
	r.HaltConfirmed = true
	r.HaltReasonCode = run.HaltReasonBaselineVerifyFailed
	r.HaltError = "temporal workflow did not complete: workflow execution error (type: RunWorkflow): baseline verify activity: activity error"
	if err := save(r, dataDir); err != nil {
		t.Fatal(err)
	}
	loaded, err := run.Load(dataDir, "r2")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(loaded.HaltError, "No model call was made: the verify command fails on the untouched repository (2247b35ea1c4)") {
		t.Errorf("HaltError = %q", loaded.HaltError)
	}
	if want := "halted before the build: baseline verify " + unnamedBaselineSummary; loaded.Triage != want {
		t.Errorf("Triage = %q, want %q", loaded.Triage, want)
	}
	if loaded.BaselineVerify == nil || loaded.BaselineVerify.FailingCount != 2 {
		t.Errorf("the saved run lost its baseline: %+v", loaded.BaselineVerify)
	}
}
