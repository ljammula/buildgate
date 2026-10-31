package meter

import "testing"

// TestWorkerAPIFormats: each model api maps to the request and usage format
// the meter parses for it.
func TestWorkerAPIFormats(t *testing.T) {
	for _, tc := range []struct{ api, request, usage string }{
		{"", RequestFormatOpenAICompletions, UsageFormatOpenAI},
		{RequestFormatOpenAICompletions, RequestFormatOpenAICompletions, UsageFormatOpenAI},
		{RequestFormatOpenAIResponses, RequestFormatOpenAIResponses, UsageFormatOpenAIResponses},
		{WorkerAPIAnthropicMessages, RequestFormatAnthropic, UsageFormatAnthropic},
	} {
		if got := RequestFormatForWorkerAPI(tc.api); got != tc.request {
			t.Errorf("RequestFormatForWorkerAPI(%q) = %q, want %q", tc.api, got, tc.request)
		}
		if got := UsageFormatForWorkerAPI(tc.api); got != tc.usage {
			t.Errorf("UsageFormatForWorkerAPI(%q) = %q, want %q", tc.api, got, tc.usage)
		}
	}
}
