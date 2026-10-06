package progress

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func writeEmptyFile(path string) error {
	return os.WriteFile(path, nil, 0o640)
}

func writeLines(t *testing.T, path string, lines []Event) {
	t.Helper()
	for _, e := range lines {
		if err := Append(path, e); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
}

func TestSummary(t *testing.T) {
	cases := []struct {
		name  string
		lines []Event
		want  RunSummary
	}{
		{
			name:  "missing file",
			lines: nil,
			want:  RunSummary{},
		},
		{
			name: "stage in progress",
			lines: []Event{
				{Ts: "t1", Source: "factory", Stage: "build", Event: "start"},
			},
			want: RunSummary{LastAt: "t1", CurrentStage: "build"},
		},
		{
			name: "stage ended clears current stage",
			lines: []Event{
				{Ts: "t1", Source: "factory", Stage: "build", Event: "start"},
				{Ts: "t2", Source: "factory", Stage: "build", Event: "end", Outcome: "pass"},
			},
			want: RunSummary{LastAt: "t2", CurrentStage: ""},
		},
		{
			name: "finished end leaves no current stage",
			lines: []Event{
				{Ts: "t1", Source: "factory", Stage: "verify", Event: "start"},
				{Ts: "t2", Source: "factory", Stage: "verify", Event: "end", Outcome: "pass"},
				{Ts: "t3", Source: "factory", Stage: "finished", Event: "end", Outcome: "accepted"},
			},
			want: RunSummary{LastAt: "t3", CurrentStage: ""},
		},
		{
			name: "worker round in progress",
			lines: []Event{
				{Ts: "t1", Source: "factory", Stage: "build", Event: "start"},
				{Ts: "t2", Source: "worker", Stage: "round", Event: "start", Round: 2, MaxRounds: 6},
			},
			want: RunSummary{LastAt: "t2", CurrentStage: "build", CurrentRound: 2, MaxRounds: 6},
		},
		{
			name: "worker round ended resets round",
			lines: []Event{
				{Ts: "t1", Source: "worker", Stage: "round", Event: "start", Round: 2, MaxRounds: 6},
				{Ts: "t2", Source: "worker", Stage: "round", Event: "end", Round: 2, MaxRounds: 6, Outcome: "fail"},
			},
			want: RunSummary{LastAt: "t2", CurrentRound: 0, MaxRounds: 0},
		},
		{
			name: "queued note sets waiting reason",
			lines: []Event{
				{Ts: "t1", Source: "factory", Stage: "queued", Event: "note", Detail: "behind 1 run(s) on foo/bar"},
			},
			want: RunSummary{LastAt: "t1", WaitingReason: "behind 1 run(s) on foo/bar"},
		},
		{
			name: "start after queued clears waiting reason",
			lines: []Event{
				{Ts: "t1", Source: "factory", Stage: "queued", Event: "note", Detail: "behind 1 run(s) on foo/bar"},
				{Ts: "t2", Source: "factory", Stage: "prepare_workspace", Event: "start"},
			},
			want: RunSummary{LastAt: "t2", CurrentStage: "prepare_workspace", WaitingReason: ""},
		},
		{
			name: "model host lock wait sets waiting reason",
			lines: []Event{
				{Ts: "t1", Source: "factory", Stage: "model_host_lock", Event: "waiting", Detail: "queued behind run-a"},
			},
			want: RunSummary{LastAt: "t1", WaitingReason: "queued behind run-a"},
		},
		{
			name: "model host lock acquired clears waiting reason",
			lines: []Event{
				{Ts: "t1", Source: "factory", Stage: "model_host_lock", Event: "waiting", Detail: "queued behind run-a"},
				{Ts: "t2", Source: "factory", Stage: "model_host_lock", Event: "acquired"},
			},
			want: RunSummary{LastAt: "t2", WaitingReason: ""},
		},
		{
			name: "compose services lock wait sets, then acquired clears, waiting reason",
			lines: []Event{
				{Ts: "t1", Source: "factory", Stage: "compose_services_lock", Event: "waiting", Detail: "queued behind run-a (pid 7)"},
			},
			want: RunSummary{LastAt: "t1", WaitingReason: "queued behind run-a (pid 7)"},
		},
		{
			name: "compose services lock acquired clears waiting reason",
			lines: []Event{
				{Ts: "t1", Source: "factory", Stage: "compose_services_lock", Event: "waiting", Detail: "queued behind run-a"},
				{Ts: "t2", Source: "factory", Stage: "compose_services_lock", Event: "acquired"},
			},
			want: RunSummary{LastAt: "t2", WaitingReason: ""},
		},
		{
			name: "later queued note updates waiting reason",
			lines: []Event{
				{Ts: "t1", Source: "factory", Stage: "queued", Event: "note", Detail: "behind 1 run(s) on foo/bar"},
				{Ts: "t2", Source: "factory", Stage: "queued", Event: "note", Detail: "behind 2 run(s) on foo/bar"},
			},
			want: RunSummary{LastAt: "t2", WaitingReason: "behind 2 run(s) on foo/bar"},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var path string
			if c.lines != nil {
				path = filepath.Join(t.TempDir(), FileName)
				writeLines(t, path, c.lines)
			} else {
				path = filepath.Join(t.TempDir(), "does-not-exist.jsonl")
			}

			got, err := Summary(path)
			if err != nil {
				t.Fatalf("Summary: %v", err)
			}
			if got != c.want {
				t.Errorf("Summary = %+v, want %+v", got, c.want)
			}
		})
	}
}

// TestSummaryReadsOnlyTail confirms Summary bounds its read to tailBudget
// bytes regardless of how large the file is -- a long-running run's feed
// must not make every run-list poll linear in its total history size.
func TestSummaryReadsOnlyTail(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)

	// Pad the file with enough old lines to exceed tailBudget, each
	// carrying a distinct stage so a summary that (incorrectly) saw them
	// would report the wrong current stage.
	pad := strings.Repeat("a", 200)
	for i := 0; i < 500; i++ {
		if err := Append(path, Event{Source: "factory", Stage: "build", Event: "start", Detail: pad}); err != nil {
			t.Fatalf("Append: %v", err)
		}
		if err := Append(path, Event{Source: "factory", Stage: "build", Event: "end", Outcome: "pass"}); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	if err := Append(path, Event{Ts: "last", Source: "factory", Stage: "verify", Event: "start"}); err != nil {
		t.Fatalf("Append: %v", err)
	}

	got, err := Summary(path)
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	if got.CurrentStage != "verify" || got.LastAt != "last" {
		t.Errorf("Summary = %+v, want current_stage=verify last_at=last", got)
	}
}

// TestSummaryFallsBackWhenTailHasNoFactoryLine covers the one case the
// tail-only read gets wrong on its own: a stage whose relayed worker notes
// alone exceed tailBudget, pushing its factory "start" line out of the
// window. Summary must still report that stage as current.
func TestSummaryFallsBackWhenTailHasNoFactoryLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	if err := Append(path, Event{Source: "factory", Stage: "build", Event: "start"}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := Append(path, Event{Source: "worker", Stage: "round", Event: "start", Round: 2, MaxRounds: 6}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	pad := strings.Repeat("n", 400)
	for i := 0; i < 300; i++ {
		if err := Append(path, Event{Source: "worker", Stage: "agent", Event: "note", Round: 2, Detail: pad}); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}

	got, err := Summary(path)
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	if got.CurrentStage != "build" || got.CurrentRound != 2 || got.MaxRounds != 6 {
		t.Errorf("Summary = %+v, want current_stage=build round 2/6", got)
	}
}

// TestSummarize confirms Summarize (the in-memory core Summary itself
// calls after tailing/parsing progress.jsonl) agrees with Summary on the
// same events -- cmd/factoryd's watchRun calls it directly against its
// own already-parsed event list rather than re-reading the file.
func TestSummarize(t *testing.T) {
	events := []Event{
		{Ts: "t1", Source: "factory", Stage: "queued", Event: "note", Detail: "behind 2 run(s) on calc-app"},
		{Ts: "t2", Source: "factory", Stage: "build", Event: "start"},
		{Ts: "t3", Source: "worker", Stage: "round", Event: "start", Round: 1, MaxRounds: 6},
	}
	want := RunSummary{LastAt: "t3", CurrentStage: "build", CurrentRound: 1, MaxRounds: 6, WaitingReason: ""}
	if got := Summarize(events); got != want {
		t.Errorf("Summarize = %+v, want %+v", got, want)
	}
}

func TestSummaryEmptyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	if err := writeEmptyFile(path); err != nil {
		t.Fatalf("writeEmptyFile: %v", err)
	}
	got, err := Summary(path)
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	if got != (RunSummary{}) {
		t.Errorf("Summary = %+v, want zero value", got)
	}
}

// TestHasFinished covers the single-terminal-line rule: a feed reports
// finished only once the factory's own "finished" end line is present,
// never for a still-running run or a missing file.
func TestHasFinished(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	if HasFinished(path) {
		t.Fatal("HasFinished(missing file) = true, want false")
	}
	writeLines(t, path, []Event{{Source: "factory", Stage: "build", Event: "start"}})
	if HasFinished(path) {
		t.Fatal("HasFinished(running) = true, want false")
	}
	writeLines(t, path, []Event{{Source: "factory", Stage: "finished", Event: "end", Outcome: "quarantined"}})
	if !HasFinished(path) {
		t.Fatal("HasFinished(finished) = false, want true")
	}
}

func TestTruncateDetailKeepsRuneBoundary(t *testing.T) {
	s := strings.Repeat("·", maxDetailLen) // 2 bytes each, so the byte cap lands mid-rune
	got := truncateDetail(s)
	if len(got) > maxDetailLen || !utf8.ValidString(got) {
		t.Fatalf("truncateDetail produced %d bytes, valid=%v", len(got), utf8.ValidString(got))
	}
}

// TestStalled covers Stalled's cases: recent activity, quiet past
// StallAfter measured from the progress feed's last line, quiet measured
// from createdAt when there is no progress feed yet, and an unparseable
// timestamp reporting not-stalled rather than guessing. Mirrors
// cmd/factoryd's former stallState test exactly -- this is now the single
// implementation both cmd/factoryd and the console's server-computed
// fields rely on.
func TestStalled(t *testing.T) {
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	const layout = "2006-01-02T15:04:05.000Z07:00"

	cases := []struct {
		name        string
		summary     RunSummary
		createdAt   string
		wantStalled bool
		wantSince   time.Duration
	}{
		{
			name:        "recent activity is not stalled",
			summary:     RunSummary{LastAt: now.Add(-1 * time.Minute).Format(layout)},
			createdAt:   now.Add(-10 * time.Minute).Format(time.RFC3339),
			wantStalled: false,
			wantSince:   time.Minute,
		},
		{
			name:        "quiet past StallAfter since last progress line",
			summary:     RunSummary{LastAt: now.Add(-6 * time.Minute).Format(layout)},
			createdAt:   now.Add(-20 * time.Minute).Format(time.RFC3339),
			wantStalled: true,
			wantSince:   6 * time.Minute,
		},
		{
			name:        "no progress lines falls back to createdAt",
			summary:     RunSummary{},
			createdAt:   now.Add(-6 * time.Minute).Format(time.RFC3339),
			wantStalled: true,
			wantSince:   6 * time.Minute,
		},
		{
			name:        "no progress lines, fresh run is not stalled",
			summary:     RunSummary{},
			createdAt:   now.Add(-1 * time.Minute).Format(time.RFC3339),
			wantStalled: false,
			wantSince:   time.Minute,
		},
		{
			name:        "unparseable timestamp reports not stalled",
			summary:     RunSummary{LastAt: "not-a-timestamp"},
			createdAt:   now.Add(-20 * time.Minute).Format(time.RFC3339),
			wantStalled: false,
			wantSince:   0,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			stalled, since := Stalled(c.summary, c.createdAt, now)
			if stalled != c.wantStalled || since != c.wantSince {
				t.Errorf("Stalled = (%v, %v), want (%v, %v)", stalled, since, c.wantStalled, c.wantSince)
			}
		})
	}
}
