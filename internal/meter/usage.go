package meter

import (
	"encoding/json"
	"io"
	"strings"
)

// ResponseIsEventStream reports whether an upstream response body is a
// server-sent event stream: its Content-Type says so, or -- when the
// upstream sends no Content-Type at all -- the request itself asked to
// stream. The ChatGPT Codex backend (CredentialModeChatGPTCodex) streams
// Responses events with no Content-Type header (found live, 2026-09-23:
// every request fell through to the JSON usage parser, failed on the
// leading "event:" line, and was charged exhaustUsageBudgets' conservative
// estimate -- roughly 3-6x real usage). A declared non-stream Content-Type
// is always believed.
func ResponseIsEventStream(contentType string, requestBody []byte) bool {
	if strings.TrimSpace(contentType) != "" {
		return isEventStream(contentType)
	}
	var request struct {
		Stream bool `json:"stream"`
	}
	return json.Unmarshal(requestBody, &request) == nil && request.Stream
}

func isEventStream(contentType string) bool {
	mediaType, _, _ := strings.Cut(contentType, ";")
	return strings.EqualFold(strings.TrimSpace(mediaType), "text/event-stream")
}

// SSEUsageParser is the common shape NewSSEUsage returns: an io.Writer that
// observes a streamed response body via io.TeeReader (see relay.ServeHTTP) without
// altering it, plus a Usage() accessor called once the copy is complete.
// costInputTokens/meterInputTokens/cachedTokens match Account.AddUsage's own
// parameters -- see its doc comment.
type SSEUsageParser interface {
	io.Writer
	Usage() (costInputTokens, meterInputTokens, cachedTokens, cacheWriteTokens, outputTokens int, err error)
}

// ParseMessageUsage dispatches to the non-streaming usage parser matching
// usageFormat. Return values match Account.AddUsage's own parameters -- see its
// doc comment.
func ParseMessageUsage(usageFormat string, body []byte) (costInputTokens, meterInputTokens, cachedTokens, cacheWriteTokens, outputTokens int, err error) {
	if usageFormat == UsageFormatOpenAI {
		return openAIMessageUsage(body)
	}
	if usageFormat == UsageFormatOpenAIResponses {
		return openAIResponsesMessageUsage(body)
	}
	return anthropicMessageUsage(body)
}

// NewSSEUsage dispatches to the streaming usage parser matching
// usageFormat.
func NewSSEUsage(usageFormat string) SSEUsageParser {
	if usageFormat == UsageFormatOpenAI {
		return &openAISSEUsage{}
	}
	if usageFormat == UsageFormatOpenAIResponses {
		return &openAIResponsesSSEUsage{}
	}
	return &anthropicSSEUsage{}
}
