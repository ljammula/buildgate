package main

import (
	"strings"
	"testing"
	"time"

	"buildgate/internal/progress"
)

// TestWatchShowsTheStartupStepsAndTheFooterNamesTheCurrentOne: a start-up step
// prints as a nested started/ended line, and until the agent's first round the
// footer reads "start-up n/6: <step> (running <time>)".
func TestWatchShowsTheStartupStepsAndTheFooterNamesTheCurrentOne(t *testing.T) {
	t0, _ := time.Parse(time.RFC3339, "2026-10-20T10:00:00Z")
	at := func(sec int) string {
		return t0.Add(time.Duration(sec) * time.Second).Format("2006-01-02T15:04:05.000Z07:00")
	}
	events := []progress.Event{
		{Ts: at(0), Source: "factory", Stage: "image", Event: "start"},
		{Ts: at(1), Source: "factory", Stage: "image", Event: "end", Outcome: "pass", Detail: "worker@sha256:aaaa (configured)"},
		{Ts: at(2), Source: "factory", Stage: "prepare_workspace", Event: "start"},
		{Ts: at(3), Source: "factory", Stage: "prepare_workspace", Event: "end", Outcome: "pass"},
		{Ts: at(4), Source: "factory", Stage: "install", Event: progress.StepStart},
	}
	lines := renderRunEvents(append(events, progress.Event{Ts: at(7), Source: "factory", Stage: "install", Event: progress.StepEnd, Outcome: "pass", Detail: "registry proxy"}), t0)
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "install") || !strings.Contains(joined, "started") || !strings.Contains(joined, "pass (3s)") {
		t.Errorf("transcript lacks the install step's start and timed end:\n%s", joined)
	}

	footer := withStartup("⏱ 46s  ·  prepare_workspace", progress.StartupFooter(progress.StartupStepsFrom(events), t0.Add(46*time.Second)))
	if want := "start-up 3/6: install (running 42s)  ·  ⏱ 46s  ·  prepare_workspace"; footer != want {
		t.Errorf("footer = %q, want %q", footer, want)
	}
	launched := append(events, progress.Event{Ts: at(8), Source: "factory", Stage: "agent", Event: progress.StepStart})
	if got := progress.StartupFooter(progress.StartupStepsFrom(launched), t0.Add(50*time.Second)); !strings.HasPrefix(got, "start-up 3/6: install") {
		t.Errorf("footer before the first round = %q", got)
	}
	started := append(launched, progress.Event{Ts: at(9), Source: "worker", Stage: "round", Event: "start", Round: 1, MaxRounds: 6})
	if got := progress.StartupFooter(progress.StartupStepsFrom(started), t0.Add(50*time.Second)); got != "" {
		t.Errorf("footer after the agent started = %q, want none", got)
	}
}
