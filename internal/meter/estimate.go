package meter

import "encoding/json"

// UnmeterableFallbackOutputTokens is the assumed output-token cost of an
// unmeterable request whose own body carries no parseable max_tokens /
// max_completion_tokens -- a generous, deliberately round ceiling
// (matching the common informal default a number of model APIs and
// client libraries use when a caller omits an explicit cap), not a
// measurement. Only Anthropic Messages requests are required by that API
// to always carry max_tokens; an OpenAI-shaped request may omit it
// entirely and rely on the upstream's own server-side default, which this
// relay cannot see.
const UnmeterableFallbackOutputTokens = 4096

// requestedMaxTokens extracts a request body's own declared max_tokens
// (Anthropic Messages; also the older OpenAI Chat Completions field name)
// or max_completion_tokens (current OpenAI Chat Completions field name), or
// max_output_tokens (OpenAI Responses), preserving that precedence and
// falling back to UnmeterableFallbackOutputTokens when none parses.
// Malformed JSON is treated the same as an absent field -- this is a
// best-effort conservative estimate for an already-abnormal request, not a
// second validation pass the normal request path should depend on.
func requestedMaxTokens(requestBody []byte) int {
	var fields struct {
		MaxTokens           *int `json:"max_tokens"`
		MaxCompletionTokens *int `json:"max_completion_tokens"`
		MaxOutputTokens     *int `json:"max_output_tokens"`
	}
	if err := json.Unmarshal(requestBody, &fields); err != nil {
		return UnmeterableFallbackOutputTokens
	}
	if fields.MaxTokens != nil && *fields.MaxTokens > 0 {
		return *fields.MaxTokens
	}
	if fields.MaxCompletionTokens != nil && *fields.MaxCompletionTokens > 0 {
		return *fields.MaxCompletionTokens
	}
	if fields.MaxOutputTokens != nil && *fields.MaxOutputTokens > 0 {
		return *fields.MaxOutputTokens
	}
	return UnmeterableFallbackOutputTokens
}

// estimateTokens is the worst-case usage of a request whose real usage is
// unknown: its body length as the input tokens and its declared (or fallback)
// output cap as the output tokens. Account.ChargeEstimate and the meter
// service's reservations both use it, so a reservation equals the charge it
// stands for.
func estimateTokens(requestBody []byte) (inputTokens, outputTokens int) {
	return len(requestBody), requestedMaxTokens(requestBody)
}
