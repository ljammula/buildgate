package progress

import (
	"slices"
	"testing"
	"time"
)

// TestStalledScalesWithTheLongestCompletedVerify: a verify-family stage writes
// only a start and an end line, so one open after a completed 8-minute verify
// is flagged only past 12 minutes (1.5 x 8); any other stage, and a verify with
// no completed one before it, keep the 5-minute threshold.
func TestStalledScalesWithTheLongestCompletedVerify(t *testing.T) {
	const layout = "2006-01-02T15:04:05.000Z07:00"
	at := func(hhmmss string) string { return "2026-09-18T" + hhmmss + ".000Z" }
	factory := func(ts, stage, event string) Event {
		return Event{Ts: ts, Source: "factory", Stage: stage, Event: event}
	}
	baselineDone := []Event{
		factory(at("11:00:00"), "baseline_verify", "start"),
		factory(at("11:08:00"), "baseline_verify", "end"),
	}
	cases := []struct {
		name         string
		events       []Event
		now          string
		wantLongest  time.Duration
		wantStalled  bool
		wantQuietFor time.Duration
	}{
		{
			name:         "verify open 7 min after an 8 min baseline",
			events:       append(slices.Clone(baselineDone), factory(at("11:08:00"), "verify", "start")),
			now:          at("11:15:00"),
			wantLongest:  8 * time.Minute,
			wantStalled:  false,
			wantQuietFor: 7 * time.Minute,
		},
		{
			name:         "verify open 13 min after an 8 min baseline",
			events:       append(slices.Clone(baselineDone), factory(at("11:08:00"), "verify", "start")),
			now:          at("11:21:00"),
			wantLongest:  8 * time.Minute,
			wantStalled:  true,
			wantQuietFor: 13 * time.Minute,
		},
		{
			name:         "build quiet 6 min after an 8 min baseline",
			events:       append(slices.Clone(baselineDone), factory(at("11:08:00"), "build", "start")),
			now:          at("11:14:00"),
			wantLongest:  8 * time.Minute,
			wantStalled:  true,
			wantQuietFor: 6 * time.Minute,
		},
		{
			name:         "post-oracle-commit verify open 10 min after an 8 min baseline",
			events:       append(slices.Clone(baselineDone), factory(at("11:08:00"), "post_oracle_commit_verify", "start")),
			now:          at("11:18:00"),
			wantLongest:  8 * time.Minute,
			wantStalled:  false,
			wantQuietFor: 10 * time.Minute,
		},
		{
			name:         "first verify stage quiet 6 min has no reference",
			events:       []Event{factory(at("11:00:00"), "baseline_verify", "start")},
			now:          at("11:06:00"),
			wantLongest:  0,
			wantStalled:  true,
			wantQuietFor: 6 * time.Minute,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sum := Summarize(c.events)
			if sum.LongestVerify != c.wantLongest {
				t.Errorf("LongestVerify = %v, want %v", sum.LongestVerify, c.wantLongest)
			}
			now, err := time.Parse(layout, c.now)
			if err != nil {
				t.Fatal(err)
			}
			stalled, since := Stalled(sum, "", now)
			if stalled != c.wantStalled || since != c.wantQuietFor {
				t.Errorf("Stalled = (%v, %v), want (%v, %v)", stalled, since, c.wantStalled, c.wantQuietFor)
			}
		})
	}
}
