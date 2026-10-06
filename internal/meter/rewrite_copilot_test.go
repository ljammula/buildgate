package meter

import (
	"testing"
)

func TestCopilotResponsesInitiatorUsesLastInputRole(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{name: "user input", body: `{"input":[{"role":"assistant"},{"role":"user"}]}`, want: "user"},
		{name: "tool follow-up", body: `{"input":[{"role":"user"},{"role":"tool"}]}`, want: "agent"},
		{name: "empty input", body: `{"input":[]}`, want: "user"},
		{name: "malformed input", body: `{not json`, want: "user"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := copilotInitiator([]byte(tc.body), RequestFormatOpenAIResponses); got != tc.want {
				t.Fatalf("copilotInitiator(%s) = %q, want %q", tc.body, got, tc.want)
			}
		})
	}
}
