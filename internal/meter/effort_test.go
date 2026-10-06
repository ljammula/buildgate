package meter

import (
	"testing"
)

// TestParseRequestedReasoningEffort covers every shape
// parseRequestedReasoningEffort recognizes per request format, the
// exact-key decoding boundary (a mixed-case duplicate must never win),
// format-awareness (a field belonging to a different format must be
// ignored), and the "present with the wrong type is an anomaly, not
// absence" rule.
func TestParseRequestedReasoningEffort(t *testing.T) {
	for _, tc := range []struct {
		name   string
		format string
		body   string
		want   string
	}{
		{
			name:   "OpenAI Responses shape",
			format: RequestFormatOpenAIResponses,
			body:   `{"model":"gpt-5.6-luna","reasoning":{"effort":"medium"}}`,
			want:   "medium",
		},
		{
			name:   "OpenAI Chat Completions shape",
			format: RequestFormatOpenAICompletions,
			body:   `{"model":"gpt-5.6-luna","reasoning_effort":"high"}`,
			want:   "high",
		},
		{
			name:   "Anthropic Messages thinking budget",
			format: RequestFormatAnthropic,
			body:   `{"model":"claude","thinking":{"type":"enabled","budget_tokens":2048}}`,
			want:   "budget:2048",
		},
		{
			name:   "Anthropic Messages thinking adaptive with no budget",
			format: RequestFormatAnthropic,
			body:   `{"model":"claude","thinking":{"type":"adaptive"}}`,
			want:   "adaptive",
		},
		{
			name:   "Anthropic budget_tokens takes precedence over adaptive type",
			format: RequestFormatAnthropic,
			body:   `{"thinking":{"type":"adaptive","budget_tokens":512}}`,
			want:   "budget:512",
		},
		{
			name:   "Anthropic thinking disabled records empty regardless of budget_tokens",
			format: RequestFormatAnthropic,
			body:   `{"thinking":{"type":"disabled","budget_tokens":4096}}`,
			want:   "",
		},
		{
			name:   "Anthropic output_config.effort",
			format: RequestFormatAnthropic,
			body:   `{"model":"claude","output_config":{"effort":"high"}}`,
			want:   "high",
		},
		{
			name:   "Anthropic output_config.effort takes precedence over thinking",
			format: RequestFormatAnthropic,
			body:   `{"output_config":{"effort":"low"},"thinking":{"type":"adaptive"}}`,
			want:   "low",
		},
		{
			name:   "absent",
			format: RequestFormatAnthropic,
			body:   `{"model":"gpt-5.6-luna","messages":[]}`,
			want:   "",
		},
		{
			name:   "malformed JSON",
			format: RequestFormatOpenAIResponses,
			body:   `{not valid json`,
			want:   "",
		},
		{
			name:   "unknown word falls back to other",
			format: RequestFormatOpenAICompletions,
			body:   `{"reasoning_effort":"extreme"}`,
			want:   "other",
		},
		{
			name:   "mixed-case word falls back to other (not in the closed set)",
			format: RequestFormatOpenAICompletions,
			body:   `{"reasoning_effort":"Medium"}`,
			want:   "other",
		},
		{
			name:   "non-allowlisted punctuation falls back to other",
			format: RequestFormatOpenAIResponses,
			body:   `{"reasoning":{"effort":"medium; DROP TABLE"}}`,
			want:   "other",
		},
		{
			name:   "exact-key decoding: a mixed-case duplicate sibling never wins",
			format: RequestFormatOpenAIResponses,
			body:   `{"reasoning":{"effort":"xhigh"},"REASONING":{"effort":"low"}}`,
			want:   "xhigh",
		},
		{
			name:   "exact-key decoding: Chat Completions duplicate case variant ignored",
			format: RequestFormatOpenAICompletions,
			body:   `{"reasoning_effort":"low","Reasoning_Effort":"max"}`,
			want:   "low",
		},
		{
			name:   "format mismatch on a Chat-Completions-configured route ignores a Responses-shaped field",
			format: RequestFormatOpenAICompletions,
			body:   `{"reasoning":{"effort":"max"}}`,
			want:   "",
		},
		{
			name:   "format mismatch on a Responses-configured route ignores a Chat-Completions-shaped field",
			format: RequestFormatOpenAIResponses,
			body:   `{"reasoning_effort":"max"}`,
			want:   "",
		},
		{
			name:   "sibling type mismatch: parent is not an object",
			format: RequestFormatOpenAIResponses,
			body:   `{"reasoning":"medium"}`,
			want:   "other",
		},
		{
			name:   "sibling type mismatch: leaf is not a string",
			format: RequestFormatOpenAIResponses,
			body:   `{"reasoning":{"effort":5}}`,
			want:   "other",
		},
		{
			name:   "sibling type mismatch: Chat Completions leaf is not a string",
			format: RequestFormatOpenAICompletions,
			body:   `{"reasoning_effort":5}`,
			want:   "other",
		},
		{
			name:   "sibling type mismatch: Anthropic thinking is not an object",
			format: RequestFormatAnthropic,
			body:   `{"thinking":"enabled"}`,
			want:   "other",
		},
		{
			name:   "sibling type mismatch: Anthropic thinking.type is not a string",
			format: RequestFormatAnthropic,
			body:   `{"thinking":{"type":5}}`,
			want:   "other",
		},
		{
			name:   "sibling type mismatch: Anthropic thinking.budget_tokens is not a number",
			format: RequestFormatAnthropic,
			body:   `{"thinking":{"budget_tokens":"a lot"}}`,
			want:   "other",
		},
		{
			name:   "negative budget_tokens is an anomaly, not a real budget",
			format: RequestFormatAnthropic,
			body:   `{"thinking":{"budget_tokens":-5}}`,
			want:   "other",
		},
		{
			name:   "sibling type mismatch: Anthropic output_config is not an object",
			format: RequestFormatAnthropic,
			body:   `{"output_config":"high"}`,
			want:   "other",
		},
		{
			name:   "unknown thinking.type is an anomaly",
			format: RequestFormatAnthropic,
			body:   `{"thinking":{"type":"turbo"}}`,
			want:   "other",
		},
		{
			name:   "budget_tokens overflowing int64 is an anomaly",
			format: RequestFormatAnthropic,
			body:   `{"thinking":{"budget_tokens":99999999999999999999}}`,
			want:   "other",
		},
		{
			name:   "budget_tokens zero parses to budget:0 (ranked as none, but still a real parsed value)",
			format: RequestFormatAnthropic,
			body:   `{"thinking":{"budget_tokens":0}}`,
			want:   "budget:0",
		},
		// --- JSON null is treated exactly like absent, at every accessor. ---
		{
			name:   "Chat Completions reasoning_effort: null is absent, not other",
			format: RequestFormatOpenAICompletions,
			body:   `{"reasoning_effort":null}`,
			want:   "",
		},
		{
			name:   "Responses reasoning.effort: null is absent, not other",
			format: RequestFormatOpenAIResponses,
			body:   `{"reasoning":{"effort":null}}`,
			want:   "",
		},
		{
			name:   "Responses reasoning: null (the whole parent) is absent",
			format: RequestFormatOpenAIResponses,
			body:   `{"reasoning":null}`,
			want:   "",
		},
		{
			name:   "Anthropic output_config.effort: null falls through to thinking",
			format: RequestFormatAnthropic,
			body:   `{"output_config":{"effort":null},"thinking":{"type":"adaptive"}}`,
			want:   "adaptive",
		},
		{
			name:   "Anthropic output_config: null (the whole object) falls through to thinking",
			format: RequestFormatAnthropic,
			body:   `{"output_config":null,"thinking":{"budget_tokens":10}}`,
			want:   "budget:10",
		},
		{
			name:   "Anthropic thinking: null (the whole object) is absent",
			format: RequestFormatAnthropic,
			body:   `{"thinking":null}`,
			want:   "",
		},
		{
			name:   "Anthropic thinking.budget_tokens: null is absent, not budget:0",
			format: RequestFormatAnthropic,
			body:   `{"thinking":{"type":"adaptive","budget_tokens":null}}`,
			want:   "adaptive",
		},
		{
			name:   "Anthropic thinking.type: null is absent, not an anomaly",
			format: RequestFormatAnthropic,
			body:   `{"thinking":{"type":null,"budget_tokens":5}}`,
			want:   "budget:5",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ParseRequestedReasoningEffort([]byte(tc.body), tc.format); got != tc.want {
				t.Errorf("ParseRequestedReasoningEffort(%q, %q) = %q, want %q", tc.body, tc.format, got, tc.want)
			}
		})
	}
}

// TestValidReasoningEffort covers the re-validation boundary
// internal/sandbox applies to a "reasoning_effort" value read back out of
// this relay's own log/ledger output before trusting it: the closed set
// of string levels, plus "other"/"adaptive"/"budget:<n>".
func TestValidReasoningEffort(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  bool
	}{
		{"none", true},
		{"minimal", true},
		{"low", true},
		{"medium", true},
		{"high", true},
		{"xhigh", true},
		{"max", true},
		{"other", true},
		{"adaptive", true},
		{"budget:2048", true},
		{"budget:0", true},
		{"budget:9223372036854775807", true},  // 19 digits, exactly int64 max
		{"budget:9223372036854775808", false}, // 19 digits, but ONE OVER int64 max -- must be caught by ParseInt, not just the digit-count check
		{"", false},
		{"budget:", false},
		{"budget:abc", false},
		{"budget:-5", false},                   // no sign allowed
		{"budget:007", false},                  // leading zero (not exactly "0")
		{"budget:12345678901234567890", false}, // 20 digits, over the cap
		{"Medium", false},
		{"extreme", false},
		{"medium; DROP TABLE", false},
	} {
		if got := ValidReasoningEffort(tc.value); got != tc.want {
			t.Errorf("ValidReasoningEffort(%q) = %t, want %t", tc.value, got, tc.want)
		}
	}
}

// TestReasoningEffortHigherThan covers the "keep the highest seen" rank
// ordering addUsage relies on: the closed string levels in ascending
// order, "adaptive" tied with "high", every "budget:N" outranking every
// string level (compared amongst themselves by N), and "other" outranking
// everything.
// TestReasoningEffortLevelOrderHasRanks proves every entry in
// reasoningEffortLevelOrder -- the single ordered slice both
// sanitizeReasoningEffort's membership check and reasoningEffortRank's
// ordering derive from -- actually resolves to a rank, and that the ranks
// are strictly increasing in the slice's own order.
func TestReasoningEffortLevelOrderHasRanks(t *testing.T) {
	if len(reasoningEffortLevelOrder) == 0 {
		t.Fatal("reasoningEffortLevelOrder is empty")
	}
	lastTier := -1 << 30
	for _, level := range reasoningEffortLevelOrder {
		tier, _ := reasoningEffortRank(level)
		if tier <= lastTier {
			t.Errorf("reasoningEffortRank(%q) = %d, want strictly greater than the previous level's %d", level, tier, lastTier)
		}
		lastTier = tier
		if got := sanitizeReasoningEffort(level); got != level {
			t.Errorf("sanitizeReasoningEffort(%q) = %q, want it unchanged (every ordered level must be a member of its own closed set)", level, got)
		}
	}
}

// TestReasoningEffortHigherThan covers the "keep the highest effort seen"
// rank ordering addUsage relies on:
//
//	other < none < minimal < low < medium < {high, adaptive, budget:N (N>0, by N)} < xhigh < max
//
// -- "adaptive" tied with "high", "budget:0" ranked as "none" (not the
// high tier), every "budget:N" (N>0) outranking plain "high"/"adaptive"
// but never outranking "xhigh"/"max" regardless of N, and "other"
// ranking below every real level (including "none") but above "" (never
// seen anything yet).
func TestReasoningEffortHigherThan(t *testing.T) {
	// ascending order: each entry must rank higher than every earlier one.
	ascending := []string{
		"other", "none", "minimal", "low", "medium", "high", "budget:1", "budget:1000", "xhigh", "max",
	}
	for i := 1; i < len(ascending); i++ {
		if !reasoningEffortHigherThan(ascending[i], ascending[i-1]) {
			t.Errorf("reasoningEffortHigherThan(%q, %q) = false, want true", ascending[i], ascending[i-1])
		}
		if reasoningEffortHigherThan(ascending[i-1], ascending[i]) {
			t.Errorf("reasoningEffortHigherThan(%q, %q) = true, want false", ascending[i-1], ascending[i])
		}
	}
	for _, tc := range []struct{ a, b string }{
		{"adaptive", "high"}, // tied: same tier as plain "high"
		{"budget:0", "none"}, // a zero budget IS "none" effort -- a tie
	} {
		if reasoningEffortHigherThan(tc.a, tc.b) || reasoningEffortHigherThan(tc.b, tc.a) {
			t.Errorf("%q and %q must rank equally; got one ranking strictly higher", tc.a, tc.b)
		}
	}
	// budget:0 ranks at "none", strictly BELOW "high" -- not a tie.
	if !reasoningEffortHigherThan("high", "budget:0") {
		t.Error(`reasoningEffortHigherThan("high", "budget:0") = false, want true (a zero budget ranks as "none", well below "high")`)
	}
	if reasoningEffortHigherThan("budget:0", "high") {
		t.Error(`reasoningEffortHigherThan("budget:0", "high") = true, want false`)
	}
	// A budget, however large, never outranks a real "xhigh"/"max".
	if reasoningEffortHigherThan("budget:999999999999", "xhigh") {
		t.Error(`reasoningEffortHigherThan("budget:999999999999", "xhigh") = true, want false: a budget must never outrank xhigh`)
	}
	if reasoningEffortHigherThan("budget:999999999999", "max") {
		t.Error(`reasoningEffortHigherThan("budget:999999999999", "max") = true, want false: a budget must never outrank max`)
	}
	// "other" must never mask a real level, even the lowest one ("none").
	if reasoningEffortHigherThan("other", "none") {
		t.Error(`reasoningEffortHigherThan("other", "none") = true, want false: "other" must never outrank a real level`)
	}
	// But "other" IS the first-ever observation's own recorded value when
	// nothing else has ever been seen -- there is no real level yet to mask.
	if !reasoningEffortHigherThan("other", "") {
		t.Error(`reasoningEffortHigherThan("other", "") = false, want true (never seen anything yet)`)
	}
	if !reasoningEffortHigherThan("medium", "") {
		t.Error(`reasoningEffortHigherThan("medium", "") = false, want true (never seen anything yet)`)
	}
	if reasoningEffortHigherThan("", "medium") {
		t.Error(`reasoningEffortHigherThan("", "medium") = true, want false`)
	}
	if reasoningEffortHigherThan("medium", "medium") {
		t.Error("a tie must not count as higher")
	}
}
