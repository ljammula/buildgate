package progress

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fe(ts, stage, event, outcome, detail, log string) Event {
	return Event{Ts: ts, Source: "factory", Stage: stage, Event: event, Outcome: outcome, Detail: detail, Log: log}
}

func statusesOf(steps []StartupStep) string {
	var parts []string
	for _, s := range steps {
		parts = append(parts, s.Name+"="+s.Status)
	}
	return strings.Join(parts, " ")
}

// TestStartupStepsInOrderWithSkippedAndFailed: the six steps come out in
// order from the factory's lines; a step no line began is skipped once a later
// step began, pending before; the first occurrence of a step wins; a failed
// end is failed; the agent is done at the worker's first round.
func TestStartupStepsInOrderWithSkippedAndFailed(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	writeLines(t, path, []Event{
		fe("t1", "image", "start", "", "", ""),
		fe("t2", "image", "end", "pass", "localhost:5050/x@sha256:1 (configured)", ""),
		fe("t3", "prepare_workspace", "start", "", "", ""),
		fe("t4", "prepare_workspace", "end", "pass", "", ""),
		fe("t5", "baseline_verify", "start", "", "", ""),
		// The baseline command runs repository code that can print a round
		// start: before the build launch it is nobody's agent.
		{Ts: "t5b", Source: "worker", Stage: "round", Event: "start", Round: 1, MaxRounds: 6},
		fe("t9", "baseline_verify", "end", "fail", "failed: TestX", "verify-baseline.log"),
		fe("t6", "sandbox", StepStart, "", "", ""),
		fe("t7", "sandbox", StepEnd, "pass", "", "launches/a.log"),
		fe("t7b", "agent", StepStart, "", "", ""),
		fe("t8", "sandbox", StepStart, "", "second launch", ""),
		{Ts: "t10", Source: "worker", Stage: "round", Event: "start", Round: 1, MaxRounds: 6},
		fe("t11", "install", StepStart, "", "after the agent", ""),
	})
	steps, err := StartupSteps(path)
	if err != nil {
		t.Fatal(err)
	}
	want := "image=done workspace=done install=skipped sandbox=done verify=failed agent=done"
	if got := statusesOf(steps); got != want {
		t.Fatalf("steps = %s, want %s", got, want)
	}
	if steps[0].Detail != "localhost:5050/x@sha256:1 (configured)" || steps[0].StartedAt != "t1" || steps[0].EndedAt != "t2" {
		t.Errorf("image step = %+v", steps[0])
	}
	if steps[5].StartedAt != "t7b" || steps[5].EndedAt != "t10" {
		t.Errorf("agent step = %+v, want the factory's start and the first round after it", steps[5])
	}
	if steps[3].Log != "launches/a.log" || steps[3].EndedAt != "t7" {
		t.Errorf("sandbox step = %+v, want the first launch's log and end", steps[3])
	}
	if steps[4].Log != "verify-baseline.log" || steps[4].Detail != "failed: TestX" {
		t.Errorf("verify step = %+v", steps[4])
	}

	// Before anything: all pending; a missing feed is no error.
	none, err := StartupSteps(filepath.Join(t.TempDir(), "absent.jsonl"))
	if err != nil || none != nil {
		t.Errorf("missing feed: %v, %v, want none", none, err)
	}
	// A feed from before the steps existed has none either, whatever it holds.
	old := filepath.Join(t.TempDir(), FileName)
	writeLines(t, old, []Event{fe("t1", "build", "start", "", "", ""), {Ts: "t2", Source: "worker", Stage: "round", Event: "start", Round: 1}})
	if got, err := StartupSteps(old); err != nil || got != nil {
		t.Errorf("historical feed: %v, %v, want none", got, err)
	}

	// Running, with the current wait on it; the run finishing before the agent fails the agent.
	running := StartupStepsFrom([]Event{
		fe("t1", "image", "start", "", "", ""),
		fe("t2", "image", "end", "pass", "", ""),
		fe("t3", "model_host_lock", "waiting", "", "queued behind run-a", ""),
		fe("t4", "prepare_workspace", "start", "", "", ""),
		fe("t5", "model_host_lock", "waiting", "", "queued behind run-b", ""),
	})
	if got, want := statusesOf(running), "image=done workspace=running install=pending sandbox=pending verify=pending agent=pending"; got != want {
		t.Errorf("running = %s, want %s", got, want)
	}
	if running[1].Wait != "queued behind run-b" {
		t.Errorf("wait on the running step = %q", running[1].Wait)
	}
	failed := StartupStepsFrom([]Event{
		fe("t0", "agent", StepStart, "", "", ""),
		fe("t1", "prepare_workspace", "start", "", "", ""),
		fe("t2", "prepare_workspace", "end", "pass", "", ""),
		fe("t3", "finished", "end", "halted", "preflight failed", ""),
	})
	if got := statusesOf(failed); !strings.HasSuffix(got, "agent=failed") || !strings.Contains(got, "image=skipped") {
		t.Errorf("finished before the agent: %s", got)
	}
}

// TestStepLinesDoNotCloseTheOpenStage: a step_start or step_end nested in a
// stage is no stage boundary, so the open stage stays current.
func TestStepLinesDoNotCloseTheOpenStage(t *testing.T) {
	sum := Summarize([]Event{
		fe("t1", "build", "start", "", "", ""),
		fe("t2", "install", StepStart, "", "", ""),
		fe("t3", "install", StepEnd, "pass", "", ""),
		fe("t4", "sandbox", StepStart, "", "", ""),
		fe("t5", "sandbox", StepEnd, "pass", "", ""),
	})
	if sum.CurrentStage != "build" || sum.LastAt != "t5" {
		t.Errorf("summary = %+v, want build still open, last line t5", sum)
	}
	sum = Summarize([]Event{
		fe("t1", "build", "start", "", "", ""),
		fe("t2", "install", StepStart, "", "", ""),
		fe("t3", "build", "end", "pass", "", ""),
	})
	if sum.CurrentStage != "" {
		t.Errorf("stage after its own end = %q, want none", sum.CurrentStage)
	}
}

// TestAWorkerLineCannotCarryAStepLog: a worker line naming a log, or a step,
// never becomes a factory step or supplies a log; the parser does not set a
// log, and the feed drops one from any source but the factory.
func TestAWorkerLineCannotCarryAStepLog(t *testing.T) {
	for _, line := range []string{
		`FACTORY_PROGRESS {"stage":"round","event":"start","round":1,"max_rounds":2,"log":"../../etc/passwd"}`,
		`FACTORY_PROGRESS {"stage":"agent","event":"note","detail":"x","log":"/etc/passwd"}`,
	} {
		e, ok := ParseWorkerLine(line)
		if !ok || e.Log != "" || e.Source != "worker" {
			t.Errorf("ParseWorkerLine(%q) = %+v, %v: want a worker event with no log", line, e, ok)
		}
	}
	for _, line := range []string{
		`FACTORY_PROGRESS {"stage":"sandbox","event":"step_start","log":"/etc/passwd"}`,
		`FACTORY_PROGRESS {"stage":"install","event":"step_end","log":"x"}`,
		`FACTORY_PROGRESS {"stage":"image","event":"start","log":"x"}`,
	} {
		if e, ok := ParseWorkerLine(line); ok {
			t.Errorf("ParseWorkerLine(%q) relayed %+v, want it dropped", line, e)
		}
	}

	// Even a worker event built by hand and appended loses its log.
	path := filepath.Join(t.TempDir(), FileName)
	if err := Append(path, Event{Source: "worker", Stage: "round", Event: "start", Round: 1, Log: "/etc/passwd"}); err != nil {
		t.Fatal(err)
	}
	events, err := Read(path)
	if err != nil || len(events) != 1 || events[0].Log != "" {
		t.Fatalf("appended worker line = %+v, %v, want no log", events, err)
	}
	// And a worker-sourced step line in the feed starts nothing.
	steps := StartupStepsFrom([]Event{
		{Ts: "t1", Source: "worker", Stage: "sandbox", Event: StepStart, Log: "/etc/passwd"},
		{Ts: "t2", Source: "worker", Stage: "baseline_verify", Event: "start", Log: "/etc/passwd"},
	})
	for _, s := range steps {
		if s.Status != StepPending || s.Log != "" {
			t.Errorf("step %+v started by a worker line", s)
		}
	}
	// The factory's own log is kept.
	if err := MarkLog(path, "sandbox", StepEnd, "pass", "", "launches/a.log"); err != nil {
		t.Fatal(err)
	}
	events, _ = Read(path)
	if events[len(events)-1].Log != "launches/a.log" {
		t.Errorf("factory log dropped: %+v", events[len(events)-1])
	}
}

func TestStartupFooter(t *testing.T) {
	now, _ := time.Parse(tsLayout, "2026-10-20T10:00:42.000Z")
	steps := StartupStepsFrom([]Event{
		fe("2026-10-20T10:00:00.000Z", "image", "start", "", "", ""),
		fe("2026-10-20T10:00:01.000Z", "image", "end", "pass", "", ""),
		fe("2026-10-20T10:00:02.000Z", "prepare_workspace", "start", "", "", ""),
		fe("2026-10-20T10:00:03.000Z", "prepare_workspace", "end", "pass", "", ""),
		fe("2026-10-20T10:00:00.000Z", "install", StepStart, "", "", ""),
	})
	if got, want := StartupFooter(steps, now), "start-up 3/6: install (running 42s)"; got != want {
		t.Errorf("footer = %q, want %q", got, want)
	}
	done := append(steps[:0:0], steps...)
	for i := range done {
		done[i].Status = StepDone
	}
	if got := StartupFooter(done, now); got != "" {
		t.Errorf("footer with the agent started = %q, want none", got)
	}
}

// TestAForgedRoundDuringVerifyDoesNotHideLaterLines: a round-start line printed
// before the factory started the build launch neither completes the agent step
// nor stops the read, so the factory's later lines still count.
func TestAForgedRoundDuringVerifyDoesNotHideLaterLines(t *testing.T) {
	steps := StartupStepsFrom([]Event{
		fe("t1", "baseline_verify", "start", "", "", ""),
		{Ts: "t2", Source: "worker", Stage: "round", Event: "start", Round: 1, MaxRounds: 3},
		fe("t3", "baseline_verify", "end", "pass", "passed", ""),
		fe("t4", "sandbox", StepStart, "", "", ""),
	})
	if got, want := statusesOf(steps), "image=skipped workspace=skipped install=skipped sandbox=running verify=done agent=pending"; got != want {
		t.Errorf("steps = %s, want %s", got, want)
	}
}

// TestStartupStepsSkipOverlongLines: a line past the parse limit is skipped,
// not an error, and later lines still count.
func TestStartupStepsSkipOverlongLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	writeLines(t, path, []Event{fe("t1", "image", "start", "", "", "")})
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(strings.Repeat("x", 2*startupMaxLine) + "\n")
	_ = f.Close()
	if err := Append(path, fe("t2", "image", "end", "pass", "", "")); err != nil {
		t.Fatal(err)
	}
	steps, err := StartupSteps(path)
	if err != nil || steps[0].Status != StepDone {
		t.Fatalf("steps = %+v, %v; want the image done past an overlong line", steps, err)
	}
}

// TestFactoryDetailsAreOneSanitizedLine: a detail built from error text loses
// control characters and line breaks when written.
func TestFactoryDetailsAreOneSanitizedLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	if err := Mark(path, "image", "end", "fail", "boom\x1b[31m red\nsecond line\x00"); err != nil {
		t.Fatal(err)
	}
	events, _ := Read(path)
	if d := events[0].Detail; strings.ContainsAny(d, "\n\x1b\x00") {
		t.Errorf("detail = %q, want one clean line", d)
	}
}

// TestSummaryDoesNotReportAShortenedWait: a wait that began before the tail
// window is looked for further back rather than reported from the window's
// first line.
func TestSummaryDoesNotReportAShortenedWait(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	writeLines(t, path, []Event{{Ts: "2026-10-20T10:00:00.000Z", Source: "factory", Stage: "model_host_lock", Event: "waiting", Detail: "queued behind a"}})
	for i := 0; i < 700; i++ {
		_ = Append(path, Event{Ts: "2026-10-20T10:30:00.000Z", Source: "worker", Stage: "agent", Event: "note", Detail: strings.Repeat("n", 120)})
	}
	if err := Append(path, Event{Ts: "2026-10-20T10:31:00.000Z", Source: "factory", Stage: "model_host_lock", Event: "waiting", Detail: "queued behind b"}); err != nil {
		t.Fatal(err)
	}
	sum, err := Summary(path)
	if err != nil {
		t.Fatal(err)
	}
	if sum.WaitingSince != "2026-10-20T10:00:00.000Z" {
		t.Errorf("WaitingSince = %q, want the first line of the wait, found beyond the tail window", sum.WaitingSince)
	}
}
