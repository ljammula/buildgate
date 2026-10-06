package meter

import (
	"testing"
)

// TestResponseIsEventStreamWithoutContentType: the ChatGPT Codex backend
// streams with no Content-Type header, so a streaming request's missing
// Content-Type must select the event-stream usage parser; a declared
// Content-Type always wins.
func TestResponseIsEventStreamWithoutContentType(t *testing.T) {
	streaming := []byte(`{"model":"m","stream":true}`)
	for _, tc := range []struct {
		contentType string
		body        []byte
		want        bool
	}{
		{"", streaming, true},
		{"", []byte(`{"model":"m"}`), false},
		{"", []byte(`not json`), false},
		{"application/json", streaming, false},
		{"text/event-stream; charset=utf-8", []byte(`{}`), true},
	} {
		if got := ResponseIsEventStream(tc.contentType, tc.body); got != tc.want {
			t.Errorf("ResponseIsEventStream(%q, %s) = %v, want %v", tc.contentType, tc.body, got, tc.want)
		}
	}
}
