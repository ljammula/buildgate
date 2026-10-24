package meter

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// openAIResponsesMessageUsage parses a non-streaming OpenAI Responses API
// response. Unlike Chat Completions, Responses names the authoritative
// counts input_tokens/output_tokens. Like Chat Completions' prompt_tokens,
// input_tokens already INCLUDES any cached tokens (input_tokens_details.
// cached_tokens is a reported subset), so costInputTokens and
// meterInputTokens are the same value here too.
func openAIResponsesMessageUsage(body []byte) (costInputTokens, meterInputTokens, cachedTokens, cacheWriteTokens, outputTokens int, err error) {
	var response struct {
		Usage *struct {
			InputTokens        *int `json:"input_tokens"`
			OutputTokens       *int `json:"output_tokens"`
			InputTokensDetails *struct {
				CachedTokens *int `json:"cached_tokens"`
			} `json:"input_tokens_details"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		return 0, 0, 0, 0, 0, fmt.Errorf("parse upstream usage: %w", err)
	}
	if response.Usage == nil || response.Usage.InputTokens == nil || response.Usage.OutputTokens == nil {
		return 0, 0, 0, 0, 0, fmt.Errorf("parse upstream usage: missing input_tokens or output_tokens")
	}
	input := *response.Usage.InputTokens
	output := *response.Usage.OutputTokens
	if input < 0 || output < 0 {
		return 0, 0, 0, 0, 0, fmt.Errorf("parse upstream usage: token counts must not be negative")
	}
	var cached int
	if response.Usage.InputTokensDetails != nil {
		cached, err = nonNegativeIntField(response.Usage.InputTokensDetails.CachedTokens)
		if err != nil {
			return 0, 0, 0, 0, 0, fmt.Errorf("parse upstream usage: token counts must not be negative")
		}
	}
	maxInt := int(^uint(0) >> 1)
	if input > maxInt-output {
		return 0, 0, 0, 0, 0, fmt.Errorf("parse upstream usage: token count overflow")
	}
	cached = min(cached, input)
	return input - cached, input, cached, 0, output, nil
}

// errFailedBeforeAnyWork is what Usage returns for a stream that ended in
// response.failed with no usage after only response.created and
// response.in_progress events: the upstream did no work, so nothing is owed.
var errFailedBeforeAnyWork = errors.New("upstream response failed before any work")

// openAIResponsesSSEUsage observes the Responses API event stream. The
// authoritative usage object is carried by the terminal response.completed,
// response.incomplete or response.failed event (a failure may carry none); intermediate output deltas do not contain a
// complete token count and must never be metered on their own.
type openAIResponsesSSEUsage struct {
	buffer           []byte
	dataLines        []string
	eventBytes       int
	inputTokens      *int
	cachedTokens     int
	outputTokens     *int
	terminalResponse bool
	// sawWork is set by any event before the terminal one that is not
	// response.created or response.in_progress.
	sawWork bool
	// failed is set when the terminal event is response.failed.
	failed bool
	err    error
}

func (u *openAIResponsesSSEUsage) Write(p []byte) (int, error) {
	if u.err != nil || u.terminalResponse {
		return len(p), nil
	}
	u.buffer = append(u.buffer, p...)
	for {
		newline := bytes.IndexByte(u.buffer, '\n')
		if newline < 0 {
			if u.eventBytes+len(u.buffer) > MaxSSEEventBytes {
				u.fail(fmt.Errorf("parse upstream event stream usage: event exceeds %d bytes", MaxSSEEventBytes))
			}
			return len(p), nil
		}
		line := strings.TrimSuffix(string(u.buffer[:newline]), "\r")
		u.buffer = u.buffer[newline+1:]
		u.eventBytes += newline + 1
		if u.eventBytes > MaxSSEEventBytes {
			u.fail(fmt.Errorf("parse upstream event stream usage: event exceeds %d bytes", MaxSSEEventBytes))
			return len(p), nil
		}
		if line == "" {
			u.parseEvent()
			if u.err != nil || u.terminalResponse {
				return len(p), nil
			}
			continue
		}
		if strings.HasPrefix(line, "data:") {
			data := strings.TrimPrefix(line, "data:")
			data = strings.TrimPrefix(data, " ")
			u.dataLines = append(u.dataLines, data)
		}
	}
}

func (u *openAIResponsesSSEUsage) parseEvent() {
	u.eventBytes = 0
	if len(u.dataLines) == 0 {
		return
	}
	data := strings.Join(u.dataLines, "\n")
	u.dataLines = u.dataLines[:0]
	if data == "[DONE]" {
		u.buffer = nil
		return
	}
	var event struct {
		Type     string `json:"type"`
		Response *struct {
			Usage *struct {
				InputTokens        *int `json:"input_tokens"`
				OutputTokens       *int `json:"output_tokens"`
				InputTokensDetails *struct {
					CachedTokens *int `json:"cached_tokens"`
				} `json:"input_tokens_details"`
			} `json:"usage"`
		} `json:"response"`
	}
	if err := json.Unmarshal([]byte(data), &event); err != nil {
		u.fail(fmt.Errorf("parse upstream event stream usage: %w", err))
		return
	}
	switch event.Type {
	case "response.completed", "response.incomplete":
		if u.terminalResponse {
			u.fail(errors.New("parse upstream event stream usage: duplicate terminal response event"))
			return
		}
		u.terminalResponse = true
		if event.Response == nil || event.Response.Usage == nil ||
			event.Response.Usage.InputTokens == nil || event.Response.Usage.OutputTokens == nil {
			u.fail(fmt.Errorf("parse upstream event stream usage: terminal response missing input_tokens or output_tokens"))
			return
		}
		u.readUsage(event.Response.Usage.InputTokens, event.Response.Usage.OutputTokens, event.Response.Usage.InputTokensDetails)
	case "response.failed":
		u.terminalResponse = true
		u.failed = true
		if event.Response == nil || event.Response.Usage == nil {
			return
		}
		// A usage object that is there but incomplete is unreadable usage
		// (charged its estimate), never "no usage" (released).
		if event.Response.Usage.InputTokens == nil || event.Response.Usage.OutputTokens == nil {
			u.fail(fmt.Errorf("parse upstream event stream usage: terminal response missing input_tokens or output_tokens"))
			return
		}
		u.readUsage(event.Response.Usage.InputTokens, event.Response.Usage.OutputTokens, event.Response.Usage.InputTokensDetails)
	case "response.created", "response.in_progress":
	default:
		u.sawWork = true
	}
}

// readUsage validates and records a terminal event's usage object.
func (u *openAIResponsesSSEUsage) readUsage(input, output *int, details *struct {
	CachedTokens *int `json:"cached_tokens"`
}) {
	if *input < 0 || *output < 0 {
		u.fail(fmt.Errorf("parse upstream event stream usage: token counts must not be negative"))
		return
	}
	var cached int
	if details != nil {
		cachedValue, cachedErr := nonNegativeIntField(details.CachedTokens)
		if cachedErr != nil {
			u.fail(fmt.Errorf("parse upstream event stream usage: token counts must not be negative"))
			return
		}
		cached = cachedValue
	}
	inputTokens, outputTokens := *input, *output
	u.inputTokens = &inputTokens
	u.cachedTokens = cached
	u.outputTokens = &outputTokens
}

// Failed reports that the terminal event was response.failed.
func (u *openAIResponsesSSEUsage) Failed() bool { return u.failed }

func (u *openAIResponsesSSEUsage) fail(err error) {
	u.err = err
	u.buffer = nil
	u.dataLines = nil
	u.eventBytes = 0
}

func (u *openAIResponsesSSEUsage) Usage() (costInputTokens, meterInputTokens, cachedTokens, cacheWriteTokens, outputTokens int, err error) {
	if u.err != nil {
		return 0, 0, 0, 0, 0, u.err
	}
	if !u.terminalResponse {
		return 0, 0, 0, 0, 0, fmt.Errorf("parse upstream event stream usage: missing terminal response event")
	}
	if u.inputTokens == nil || u.outputTokens == nil {
		if u.failed && !u.sawWork {
			return 0, 0, 0, 0, 0, errFailedBeforeAnyWork
		}
		return 0, 0, 0, 0, 0, fmt.Errorf("parse upstream event stream usage: no usage object observed")
	}
	maxInt := int(^uint(0) >> 1)
	if *u.inputTokens > maxInt-*u.outputTokens {
		return 0, 0, 0, 0, 0, fmt.Errorf("parse upstream event stream usage: token count overflow")
	}
	cached := min(u.cachedTokens, *u.inputTokens)
	return *u.inputTokens - cached, *u.inputTokens, cached, 0, *u.outputTokens, nil
}
