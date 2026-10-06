package meter

import (
	"testing"
)

// TestRequestedMaxTokens covers requestedMaxTokens directly: max_tokens
// wins when present (Anthropic Messages and older OpenAI Chat Completions
// requests both use this field name), max_completion_tokens (current
// OpenAI Chat Completions) is used when max_tokens is absent,
// max_output_tokens (OpenAI Responses) is used when both are absent, and
// anything unparseable -- malformed JSON, an empty body, a non-positive
// value -- falls back to UnmeterableFallbackOutputTokens rather than zero,
// which would make the conservative estimate charge nothing at all for an
// unmeterable request whose own declared cap can't be trusted.
func TestRequestedMaxTokens(t *testing.T) {
	cases := []struct {
		name string
		body string
		want int
	}{
		{name: "max_tokens present", body: `{"max_tokens":500}`, want: 500},
		{name: "max_completion_tokens present", body: `{"max_completion_tokens":700}`, want: 700},
		{name: "max_output_tokens present", body: `{"max_output_tokens":900}`, want: 900},
		{name: "max_tokens wins over max_completion_tokens", body: `{"max_tokens":500,"max_completion_tokens":700}`, want: 500},
		{name: "max_completion_tokens wins over max_output_tokens", body: `{"max_completion_tokens":700,"max_output_tokens":900}`, want: 700},
		{name: "malformed JSON falls back", body: `{not json`, want: UnmeterableFallbackOutputTokens},
		{name: "empty body falls back", body: "", want: UnmeterableFallbackOutputTokens},
		{name: "neither field present falls back", body: `{"model":"x"}`, want: UnmeterableFallbackOutputTokens},
		{name: "non-positive max_tokens falls back", body: `{"max_tokens":0}`, want: UnmeterableFallbackOutputTokens},
		{name: "non-positive max_output_tokens falls back", body: `{"max_output_tokens":-1}`, want: UnmeterableFallbackOutputTokens},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := requestedMaxTokens([]byte(tc.body)); got != tc.want {
				t.Errorf("requestedMaxTokens(%q) = %d, want %d", tc.body, got, tc.want)
			}
		})
	}
}
