package meter

import (
	"fmt"
	"testing"
)

func TestOpenAIResponsesUsageParsersRejectUntrustworthyCounts(t *testing.T) {
	maxInt := int(^uint(0) >> 1)
	tests := []struct {
		name string
		body string
	}{
		{name: "missing usage", body: `{}`},
		{name: "missing input", body: `{"usage":{"output_tokens":1}}`},
		{name: "missing output", body: `{"usage":{"input_tokens":1}}`},
		{name: "malformed JSON", body: `{not json`},
		{name: "negative input", body: `{"usage":{"input_tokens":-1,"output_tokens":1}}`},
		{name: "negative output", body: `{"usage":{"input_tokens":1,"output_tokens":-1}}`},
		{name: "overflow", body: fmt.Sprintf(`{"usage":{"input_tokens":%d,"output_tokens":1}}`, maxInt)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, _, _, _, err := openAIResponsesMessageUsage([]byte(tc.body)); err == nil {
				t.Fatalf("openAIResponsesMessageUsage(%s) = nil error, want rejection", tc.body)
			}
		})
	}
}

func TestOpenAIResponsesSSEUsageRequiresTerminalUsageEvent(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		wantInput  int
		wantOutput int
	}{
		{
			name: "completed",
			body: "event: response.output_text.delta\n" +
				"data: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\n" +
				"data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":7,\"output_tokens\":11}}}\n\n",
			wantInput:  7,
			wantOutput: 11,
		},
		{
			name:       "incomplete",
			body:       "data: {\"type\":\"response.incomplete\",\"response\":{\"usage\":{\"input_tokens\":3,\"output_tokens\":4}}}\n\n",
			wantInput:  3,
			wantOutput: 4,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			parser := &openAIResponsesSSEUsage{}
			if _, err := parser.Write([]byte(tc.body)); err != nil {
				t.Fatalf("Write: %v", err)
			}
			_, inputTokens, _, _, outputTokens, err := parser.Usage()
			if err != nil {
				t.Fatalf("usage: %v", err)
			}
			if inputTokens != tc.wantInput || outputTokens != tc.wantOutput {
				t.Fatalf("usage = %d+%d, want %d+%d", inputTokens, outputTokens, tc.wantInput, tc.wantOutput)
			}
		})
	}

	for _, tc := range []struct {
		name string
		body string
	}{
		{name: "missing terminal", body: "data: {\"type\":\"response.output_text.delta\"}\n\n"},
		{name: "malformed JSON", body: "data: {not json\n\n"},
		{name: "missing usage", body: "data: {\"type\":\"response.completed\",\"response\":{}}\n\n"},
		{name: "negative usage", body: "data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":-1,\"output_tokens\":1}}}\n\n"},
		{name: "overflow", body: fmt.Sprintf("data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":%d,\"output_tokens\":1}}}\n\n", int(^uint(0)>>1))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parser := &openAIResponsesSSEUsage{}
			_, _ = parser.Write([]byte(tc.body))
			if _, _, _, _, _, err := parser.Usage(); err == nil {
				t.Fatalf("usage() = nil error, want rejection")
			}
		})
	}
}
