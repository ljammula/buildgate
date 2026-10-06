package meter

import (
	"testing"
)

func TestWeightedInputTokensDiscountsCachedTokens(t *testing.T) {
	tests := []struct {
		name         string
		inputTokens  int
		cachedTokens int
		want         int
	}{
		{"no cache field reported (cachedTokens=0) is full weight", 1000, 0, 1000},
		{"entirely cached rounds the 10% share up", 1000, 1000, 100},
		{"partially cached: 100 uncached + ceil(900*10/100)", 1000, 900, 100 + 90},
		{"cached count above input is clamped to input", 1000, 1500, 100},
		{"negative cached count is clamped to zero", 1000, -5, 1000},
		{"a fractional cached share rounds up, never down", 1000, 999, 1 + 100},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := WeightedInputTokens(tc.inputTokens, tc.cachedTokens); got != tc.want {
				t.Fatalf("WeightedInputTokens(%d, %d) = %d, want %d", tc.inputTokens, tc.cachedTokens, got, tc.want)
			}
		})
	}
}

// TestOpenAIMessageUsageParsesCachedTokens proves the Chat Completions
// parser reads prompt_tokens_details.cached_tokens as cachedTokens while
// leaving costInputTokens/meterInputTokens both equal to the unchanged
// prompt_tokens figure -- OpenAI's prompt_tokens already includes any
// cached tokens, unlike Anthropic's input_tokens.
func TestOpenAIMessageUsageParsesCachedTokens(t *testing.T) {
	body := `{"usage":{"prompt_tokens":1000,"completion_tokens":50,"prompt_tokens_details":{"cached_tokens":900}}}`
	costInput, meterInput, cached, cacheWrite, output, err := openAIMessageUsage([]byte(body))
	if err != nil {
		t.Fatalf("openAIMessageUsage: %v", err)
	}
	if costInput != 100 || meterInput != 1000 || cached != 900 || cacheWrite != 0 || output != 50 {
		t.Fatalf("openAIMessageUsage = (cost=%d meter=%d cached=%d cacheWrite=%d output=%d), want (100, 1000, 900, 0, 50)", costInput, meterInput, cached, cacheWrite, output)
	}
}

// TestOpenAIMessageUsageNoCachedFieldIsFullWeight proves a response with no
// prompt_tokens_details field at all (the shape before this change, and
// what every non-caching upstream/model still sends) parses cachedTokens
// as 0 -- WeightedInputTokens then leaves the figure unchanged, exactly
// today's behavior.
func TestOpenAIMessageUsageNoCachedFieldIsFullWeight(t *testing.T) {
	body := `{"usage":{"prompt_tokens":1000,"completion_tokens":50}}`
	costInput, meterInput, cached, cacheWrite, output, err := openAIMessageUsage([]byte(body))
	if err != nil {
		t.Fatalf("openAIMessageUsage: %v", err)
	}
	if costInput != 1000 || meterInput != 1000 || cached != 0 || cacheWrite != 0 || output != 50 {
		t.Fatalf("openAIMessageUsage = (cost=%d meter=%d cached=%d cacheWrite=%d output=%d), want (1000, 1000, 0, 0, 50)", costInput, meterInput, cached, cacheWrite, output)
	}
}

// TestOpenAIResponsesMessageUsageParsesCachedTokens is
// TestOpenAIMessageUsageParsesCachedTokens's Responses-API counterpart:
// input_tokens_details.cached_tokens, not prompt_tokens_details.
func TestOpenAIResponsesMessageUsageParsesCachedTokens(t *testing.T) {
	body := `{"usage":{"input_tokens":1000,"output_tokens":50,"input_tokens_details":{"cached_tokens":900}}}`
	costInput, meterInput, cached, cacheWrite, output, err := openAIResponsesMessageUsage([]byte(body))
	if err != nil {
		t.Fatalf("openAIResponsesMessageUsage: %v", err)
	}
	if costInput != 100 || meterInput != 1000 || cached != 900 || cacheWrite != 0 || output != 50 {
		t.Fatalf("openAIResponsesMessageUsage = (cost=%d meter=%d cached=%d cacheWrite=%d output=%d), want (100, 1000, 900, 0, 50)", costInput, meterInput, cached, cacheWrite, output)
	}
}

// TestAnthropicMessageUsageFoldsCacheFieldsAtCorrectWeights is the
// Anthropic counterpart, and the one format where costInputTokens diverges
// from meterInputTokens: the Messages API's input_tokens field excludes
// cache_creation_input_tokens and cache_read_input_tokens entirely (they
// are additive, not a subset), so meterInputTokens folds both in --
// cache_creation_input_tokens at full weight (a write, not a hit),
// cache_read_input_tokens as cachedTokens -- while costInputTokens stays
// exactly input_tokens, unchanged from before this feature existed.
func TestAnthropicMessageUsageFoldsCacheFieldsAtCorrectWeights(t *testing.T) {
	body := `{"usage":{"input_tokens":50,"output_tokens":10,"cache_creation_input_tokens":200,"cache_read_input_tokens":100000}}`
	costInput, meterInput, cached, cacheWrite, output, err := anthropicMessageUsage([]byte(body))
	if err != nil {
		t.Fatalf("anthropicMessageUsage: %v", err)
	}
	if costInput != 50 {
		t.Fatalf("costInputTokens = %d, want 50 (Anthropic's input_tokens already excludes both cache fields)", costInput)
	}
	wantMeterInput := 50 + 200 + 100000
	if meterInput != wantMeterInput || cached != 100000 || cacheWrite != 200 || output != 10 {
		t.Fatalf("anthropicMessageUsage = (meter=%d cached=%d cacheWrite=%d output=%d), want (%d, 100000, 200, 10)", meterInput, cached, cacheWrite, output, wantMeterInput)
	}
	if got := WeightedInputTokens(meterInput, cached); got != 50+200+10000 {
		t.Fatalf("WeightedInputTokens(meter, cached) = %d, want %d (input+cache_creation at full weight, cache_read at 10%%)", got, 50+200+10000)
	}
}

// TestAnthropicMessageUsageNoCacheFieldsIsFullWeight proves a response
// carrying neither cache field (the shape every Anthropic response had
// before prompt caching existed, and what a non-caching request still
// gets) leaves meterInputTokens equal to costInputTokens/input_tokens and
// cachedTokens at 0 -- full weight, unchanged from today.
func TestAnthropicMessageUsageNoCacheFieldsIsFullWeight(t *testing.T) {
	body := `{"usage":{"input_tokens":50,"output_tokens":10}}`
	costInput, meterInput, cached, cacheWrite, output, err := anthropicMessageUsage([]byte(body))
	if err != nil {
		t.Fatalf("anthropicMessageUsage: %v", err)
	}
	if costInput != 50 || meterInput != 50 || cached != 0 || cacheWrite != 0 || output != 10 {
		t.Fatalf("anthropicMessageUsage = (cost=%d meter=%d cached=%d cacheWrite=%d output=%d), want (50, 50, 0, 0, 10)", costInput, meterInput, cached, cacheWrite, output)
	}
}

// TestOpenAIUsageClampsCachedToInput: an upstream reporting more cached
// tokens than input tokens must not yield a negative uncached count (a
// negative cost term would let the cost ceiling under-count).
func TestOpenAIUsageClampsCachedToInput(t *testing.T) {
	body := `{"usage":{"input_tokens":100,"output_tokens":5,"input_tokens_details":{"cached_tokens":500}}}`
	costInput, meterInput, cached, _, _, err := openAIResponsesMessageUsage([]byte(body))
	if err != nil {
		t.Fatalf("openAIResponsesMessageUsage: %v", err)
	}
	if costInput != 0 || meterInput != 100 || cached != 100 {
		t.Fatalf("got (uncached=%d meter=%d cached=%d), want (0, 100, 100)", costInput, meterInput, cached)
	}
	chat := `{"usage":{"prompt_tokens":100,"completion_tokens":5,"prompt_tokens_details":{"cached_tokens":500}}}`
	costInput, _, cached, _, _, err = openAIMessageUsage([]byte(chat))
	if err != nil {
		t.Fatalf("openAIMessageUsage: %v", err)
	}
	if costInput != 0 || cached != 100 {
		t.Fatalf("chat got (uncached=%d cached=%d), want (0, 100)", costInput, cached)
	}
}
