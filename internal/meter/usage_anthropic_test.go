package meter

import (
	"testing"
)

func TestAnthropicSSEUsageParsesCRLFAcrossArbitraryWrites(t *testing.T) {
	const streamBody = "event: message_start\r\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":4}}}\r\n\r\nevent: message_delta\r\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":9}}\r\n\r\nevent: message_stop\r\ndata: {\"type\":\"message_stop\"}\r\n\r\n"
	usage := &anthropicSSEUsage{}
	for _, b := range []byte(streamBody) {
		if _, err := usage.Write([]byte{b}); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	_, inputTokens, _, _, outputTokens, err := usage.Usage()
	if err != nil {
		t.Fatalf("usage: %v", err)
	}
	if inputTokens != 4 || outputTokens != 9 {
		t.Fatalf("usage = (%d, %d), want (4, 9)", inputTokens, outputTokens)
	}
}
