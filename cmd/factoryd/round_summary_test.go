package main

import (
	"testing"

	"buildgate/internal/run"
)

func boolPtr(b bool) *bool { return &b }

// TestRoundSummary is table-driven over RoundSummary's shape: nil/empty
// evidence, mixed pass/fail rounds, token formatting, cost presence, and
// the >8-round collapse.
func TestRoundSummary(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		ev      *run.AgentEvidence
		costUSD string
		want    string
	}{
		{
			name: "nil evidence",
			ev:   nil,
			want: "",
		},
		{
			name: "no rounds",
			ev:   &run.AgentEvidence{},
			want: "",
		},
		{
			name: "fail then pass with tokens and cost",
			ev: &run.AgentEvidence{Rounds: []run.AgentEvidenceRound{
				{Index: 1, VerifyPassed: boolPtr(false), Usage: map[string]any{"input": float64(20000), "output": float64(1200)}},
				{Index: 2, VerifyPassed: boolPtr(false), Usage: map[string]any{"input": float64(15000), "output": float64(1000)}},
				{Index: 3, VerifyPassed: boolPtr(true), Usage: map[string]any{"input": float64(3800), "output": float64(200)}},
			}},
			costUSD: "0.12",
			want:    "3 rounds · r1 fail (verify) · r2 fail (verify) · r3 pass · 41.2k tokens · $0.12",
		},
		{
			// C8 (operator demo, 2026-09-26): totalTokens absent, so the
			// fallback must add cacheRead/cacheWrite alongside input/output
			// rather than only the two uncached fields.
			name: "no totalTokens, falls back to input+output+cache",
			ev: &run.AgentEvidence{Rounds: []run.AgentEvidenceRound{
				{Index: 1, VerifyPassed: boolPtr(true), Usage: map[string]any{
					"input": float64(1000), "output": float64(200),
					"cacheRead": float64(500), "cacheWrite": float64(300),
				}},
			}},
			want: "1 round · r1 pass · 2.0k tokens",
		},
		{
			name: "no cost, no tokens",
			ev: &run.AgentEvidence{Rounds: []run.AgentEvidenceRound{
				{Index: 1, VerifyPassed: boolPtr(true)},
			}},
			want: "1 round · r1 pass",
		},
		{
			name: "timed out",
			ev: &run.AgentEvidence{Rounds: []run.AgentEvidenceRound{
				{Index: 1, AgentTimedOut: true},
			}},
			want: "1 round · r1 fail (timed out)",
		},
		{
			name: "pi returncode nonzero",
			ev: &run.AgentEvidence{Rounds: []run.AgentEvidenceRound{
				{Index: 1, AgentReturnCode: 1},
			}},
			want: "1 round · r1 fail (error)",
		},
		{
			name: "fast check failed short-circuits before verify",
			ev: &run.AgentEvidence{Rounds: []run.AgentEvidenceRound{
				{Index: 1, FastCheckRan: true, FastCheckPassed: boolPtr(false)},
			}},
			want: "1 round · r1 fail (verify)",
		},
		{
			name: "verify passed nil (no canonical command resolvable)",
			ev: &run.AgentEvidence{Rounds: []run.AgentEvidenceRound{
				{Index: 1},
			}},
			want: "1 round · r1 fail (error)",
		},
		{
			name: "collapses beyond 8 rounds",
			ev: &run.AgentEvidence{Rounds: []run.AgentEvidenceRound{
				{Index: 1, VerifyPassed: boolPtr(false)},
				{Index: 2, VerifyPassed: boolPtr(false)},
				{Index: 3, VerifyPassed: boolPtr(false)},
				{Index: 4, VerifyPassed: boolPtr(false)},
				{Index: 5, VerifyPassed: boolPtr(false)},
				{Index: 6, VerifyPassed: boolPtr(false)},
				{Index: 7, VerifyPassed: boolPtr(false)},
				{Index: 8, VerifyPassed: boolPtr(false)},
				{Index: 9, VerifyPassed: boolPtr(true)},
			}},
			want: "9 rounds · r1 fail (verify) · r2 fail (verify) · r3 fail (verify) · r4 fail (verify) · r5 fail (verify) · r6 fail (verify) · r7 fail (verify) · r8 fail (verify) · … r9",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := RoundSummary(tt.ev, tt.costUSD); got != tt.want {
				t.Errorf("RoundSummary() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestFormatTokenCount(t *testing.T) {
	t.Parallel()
	tests := []struct {
		n    int64
		want string
	}{
		{0, "0"},
		{823, "823"},
		{41234, "41.2k"},
		{1234567, "1.2M"},
	}
	for _, tt := range tests {
		if got := formatTokenCount(tt.n); got != tt.want {
			t.Errorf("formatTokenCount(%d) = %q, want %q", tt.n, got, tt.want)
		}
	}
}
