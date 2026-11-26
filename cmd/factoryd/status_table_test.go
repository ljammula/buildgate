package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"buildgate/internal/hostcontrol/hostcontroltest"
	"buildgate/internal/request"
	"buildgate/internal/run"
)

func TestCompactAge(t *testing.T) {
	for in, want := range map[string]string{
		"591h35m52s": "24d",
		"3h10m0s":    "3h",
		"5m12s":      "5m",
		"40s":        "40s",
		"":           "-",
		"?":          "?",
	} {
		if got := compactAge(in); got != want {
			t.Errorf("compactAge(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCutStatusDetail(t *testing.T) {
	long := strings.Repeat("é", statusDetailWidth+40)
	got, cut := cutStatusDetail(long, false)
	if !cut || len([]rune(got)) != statusDetailWidth || !strings.HasSuffix(got, "…") {
		t.Errorf("cutStatusDetail cut = %v, %d characters, want %d ending in …", cut, len([]rune(got)), statusDetailWidth)
	}
	if got, cut := cutStatusDetail(long, true); cut || got != long {
		t.Errorf("cutStatusDetail with whole set cut the text")
	}
	if got, cut := cutStatusDetail("short", false); cut || got != "short" {
		t.Errorf("cutStatusDetail(short) = %q, %v", got, cut)
	}
}

// statusTableFixture is one open request with a long reason, one finished
// request, a quarantined run of the open one and an accepted run of the
// finished one.
func statusTableFixture(t *testing.T) (dataDir, longReason string) {
	t.Helper()
	dataDir = t.TempDir()
	longReason = "ticket 2/7 halted: " + strings.Repeat("the baseline verify failed on the base commit ", 6) + "END-OF-REASON"
	saveWaitingRequest(t, dataDir, "open-req-20261101-081026", "Open", request.StateHalted, 3*time.Hour, func(r *request.Request) { r.Error = longReason })
	saveWaitingRequest(t, dataDir, "done-req-20261001-070000", "Done", request.StateDone, 30*24*time.Hour, nil)
	created := time.Now().Add(-90 * time.Minute).Format(time.RFC3339)
	for _, r := range []*run.Run{
		{ID: "open-req-20261101-081026-001-20261101-081240-1", Ticket: "open-req-20261101-081026-001", RequestID: "open-req-20261101-081026", State: run.StateQuarantined, HaltConfirmed: true, CreatedAt: created, UpdatedAt: created},
		{ID: "done-req-20261001-070000-001-20261001-070100-1", Ticket: "done-req-20261001-070000-001", RequestID: "done-req-20261001-070000", State: run.StateAccepted, HaltConfirmed: true, CreatedAt: created, UpdatedAt: created},
	} {
		if err := r.Save(dataDir); err != nil {
			t.Fatal(err)
		}
	}
	return dataDir, longReason
}

// The table has a header row, ages in one unit, reasons cut short, and only
// what is open; it logs no line of its own above the profile line.
func TestStatusTableShowsWhatIsOpenUnderAHeader(t *testing.T) {
	dataDir, longReason := statusTableFixture(t)
	logged := captureLog(t)
	out := captureStdout(t, func() {
		if err := statusMain(newTestDeps(t), []string{"-data-dir", dataDir}); err != nil {
			t.Fatalf("statusMain: %v", err)
		}
	})
	if strings.Contains(logged.String(), "data dir:") {
		t.Errorf("status without -v logged %q, want no data dir line", logged.String())
	}
	lines := strings.Split(out, "\n")
	if !strings.HasPrefix(lines[0], "profile: ") || !strings.HasPrefix(lines[1], "REQUEST") {
		t.Fatalf("status starts %q, %q; want the profile line then the header", lines[0], lines[1])
	}
	for _, column := range []string{"REQUEST", "PROJECT", "STATE", "AGE", "IN STATE", "TICKET", "DETAIL"} {
		if !strings.Contains(lines[1], column) {
			t.Errorf("request header %q lacks %s", lines[1], column)
		}
	}
	for _, want := range []string{
		"open-req-20261101-081026  app      halted  2d   3h ",
		"RUN ", "COST", "open-req-20261101-081026-001 20261101-081240-1",
		"1 finished requests and 1 finished runs not shown: factoryd status -all",
		"A reason ending in … is cut short",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("status lacks %q:\n%s", want, out)
		}
	}
	for _, unwanted := range []string{"done-req", "END-OF-REASON", "h0m", "m0s", longReason} {
		if strings.Contains(out, unwanted) {
			t.Errorf("status has %q:\n%s", unwanted, out)
		}
	}
}

func TestStatusAllShowsFinishedAndVerboseShowsWholeReasons(t *testing.T) {
	dataDir, longReason := statusTableFixture(t)
	logged := captureLog(t)
	out := captureStdout(t, func() {
		if err := statusMain(newTestDeps(t), []string{"-data-dir", dataDir, "-all", "-v"}); err != nil {
			t.Fatalf("statusMain: %v", err)
		}
	})
	for _, want := range []string{"done-req-20261001-070000  app", "done-req-20261001-070000-001 20261001-070100-1", longReason} {
		if !strings.Contains(out, want) {
			t.Errorf("status -all -v lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "not shown") || strings.Contains(out, "cut short") {
		t.Errorf("status -all -v says something is hidden or cut:\n%s", out)
	}
	if !strings.Contains(logged.String(), `data dir: "`+dataDir+`" (source: -data-dir)`) {
		t.Errorf("status -v logged %q, want the data dir and its source", logged.String())
	}

	// -json is for programs: every request, whole reasons, Go durations.
	jsonOut := captureStdout(t, func() {
		if err := statusMain(newTestDeps(t), []string{"-data-dir", dataDir, "-json"}); err != nil {
			t.Fatalf("statusMain -json: %v", err)
		}
	})
	for _, want := range []string{"done-req-20261001-070000", "END-OF-REASON"} {
		if !strings.Contains(jsonOut, want) {
			t.Errorf("status -json lacks %q", want)
		}
	}
}

func TestStatusOfAnEmptyDataDirSaysSo(t *testing.T) {
	out := captureStdout(t, func() {
		if err := statusMain(newTestDeps(t), []string{"-data-dir", t.TempDir()}); err != nil {
			t.Fatalf("statusMain: %v", err)
		}
	})
	if !strings.Contains(out, "No open requests or runs.") || strings.Contains(out, "REQUEST") {
		t.Errorf("status of an empty data dir = %q", out)
	}
}

// -all-profiles is one table of every profile's open requests, each row
// naming its profile.
func TestStatusAllProfilesListsOpenRequestsOfEveryProfile(t *testing.T) {
	f := newProfileFixture(t, "default", "work")
	saveWaitingRequest(t, f.roots["default"], "spec-a-20261101-081026", "A", request.StateSpecReview, 2*time.Hour, nil)
	saveWaitingRequest(t, f.roots["work"], "build-b-20261102-091500", "B", request.StateHalted, 10*time.Minute, nil)
	saveWaitingRequest(t, f.roots["work"], "done-c-20261001-070000", "C", request.StateDone, time.Hour, nil)

	var out, warn bytes.Buffer
	flags := newStatusFlags()
	if err := flags.set.Parse([]string{"-all-profiles"}); err != nil {
		t.Fatal(err)
	}
	if err := statusAllProfiles(&out, &warn, flags, time.Now()); err != nil {
		t.Fatalf("statusAllProfiles: %v", err)
	}
	text := out.String()
	if !strings.HasPrefix(text, "PROFILE  REQUEST") {
		t.Errorf("-all-profiles starts %q, want the header", text)
	}
	for _, want := range []string{"default  spec-a-20261101-081026", "work     build-b-20261102-091500", "1 finished requests not shown: factoryd status -all-profiles -all"} {
		if !strings.Contains(text, want) {
			t.Errorf("-all-profiles lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "done-c") {
		t.Errorf("-all-profiles lists a finished request without -all:\n%s", text)
	}

	out.Reset()
	flags = newStatusFlags()
	if err := flags.set.Parse([]string{"-all-profiles", "-all"}); err != nil {
		t.Fatal(err)
	}
	if err := statusAllProfiles(&out, &warn, flags, time.Now()); err != nil || !strings.Contains(out.String(), "work     done-c-20261001-070000") {
		t.Errorf("-all-profiles -all: err = %v, output:\n%s", err, out.String())
	}

	for _, with := range []string{"-json", "-config=work", "-data-dir=x", "-state=halted"} {
		if err := statusMain(newTestDeps(t), []string{"-all-profiles", with}); err == nil || !strings.Contains(err.Error(), "cannot be combined") {
			t.Errorf("-all-profiles %s: err = %v, want it refused", with, err)
		}
	}
}

// The closing lines count what -n cut as well as what is finished, so the
// two together account for every run.
func TestStatusSaysHowManyRunsAreLeftPastTheLimit(t *testing.T) {
	dataDir := t.TempDir()
	saveWaitingRequest(t, dataDir, "open-req-20261101-081026", "Open", request.StateHalted, time.Hour, nil)
	created := time.Now().Add(-time.Hour).Format(time.RFC3339)
	for _, suffix := range []string{"1", "2", "3"} {
		r := &run.Run{ID: "open-req-20261101-081026-001-20261101-08124" + suffix + "-1", Ticket: "open-req-20261101-081026-001", RequestID: "open-req-20261101-081026", State: run.StateQuarantined, HaltConfirmed: true, CreatedAt: created, UpdatedAt: created}
		if err := r.Save(dataDir); err != nil {
			t.Fatal(err)
		}
	}
	out := captureStdout(t, func() {
		if err := statusMain(newTestDeps(t), []string{"-data-dir", dataDir, "-n", "1"}); err != nil {
			t.Fatalf("statusMain: %v", err)
		}
	})
	if !strings.Contains(out, "2 more runs past the newest 1: factoryd status -n 3") {
		t.Errorf("status -n 1 of three runs lacks the count past the limit:\n%s", out)
	}
	out = captureStdout(t, func() {
		if err := statusMain(newTestDeps(t), []string{"-data-dir", dataDir}); err != nil {
			t.Fatalf("statusMain: %v", err)
		}
	})
	if strings.Contains(out, "more runs past") {
		t.Errorf("status with every run listed names runs past the limit:\n%s", out)
	}
}

// -json and the stale-worker notification keep the heartbeat's age as a Go
// duration; only the table's line is in one unit.
func TestStatusHeartbeatAgeIsCompactOnlyInTheTable(t *testing.T) {
	dataDir := t.TempDir()
	hostcontroltest.WriteWorkerHeartbeat(t, dataDir, 1, "")
	now := time.Now().Add(26 * time.Hour)
	raw, table := workerHeartbeatStatusLine(dataDir, now), workerHeartbeatLine(dataDir, now, humanAge)
	if !strings.Contains(raw, "last heartbeat 26h0m") {
		t.Errorf("the -json heartbeat line = %q, want a Go duration", raw)
	}
	if table != "worker not running (last heartbeat 26h ago)" {
		t.Errorf("the table's heartbeat line = %q", table)
	}
}
