package meter

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// openAIMessageUsage parses an OpenAI-compatible chat/completions (or
// completions) response's top-level usage object -- prompt_tokens/
// completion_tokens, the counterpart to anthropicMessageUsage's
// input_tokens/output_tokens above. Unlike Anthropic, prompt_tokens
// already INCLUDES any cached tokens (prompt_tokens_details.cached_tokens
// is a reported subset of it, not additive), so costInputTokens and
// meterInputTokens are simply the same value here -- only cachedTokens
// (optional; 0 when the upstream/model doesn't report it) changes what
// WeightedInputTokens (see Account.AddUsage) does with it.
func openAIMessageUsage(body []byte) (costInputTokens, meterInputTokens, cachedTokens, cacheWriteTokens, outputTokens int, err error) {
	var response struct {
		Usage *struct {
			PromptTokens        *int `json:"prompt_tokens"`
			CompletionTokens    *int `json:"completion_tokens"`
			PromptTokensDetails *struct {
				CachedTokens *int `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		return 0, 0, 0, 0, 0, fmt.Errorf("parse upstream usage: %w", err)
	}
	if response.Usage == nil || response.Usage.PromptTokens == nil || response.Usage.CompletionTokens == nil {
		return 0, 0, 0, 0, 0, fmt.Errorf("parse upstream usage: missing prompt_tokens or completion_tokens")
	}
	input := *response.Usage.PromptTokens
	output := *response.Usage.CompletionTokens
	if input < 0 || output < 0 {
		return 0, 0, 0, 0, 0, fmt.Errorf("parse upstream usage: token counts must not be negative")
	}
	var cached int
	if response.Usage.PromptTokensDetails != nil {
		cached, err = nonNegativeIntField(response.Usage.PromptTokensDetails.CachedTokens)
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

// openAISSEUsage is the OpenAI-compatible counterpart to anthropicSSEUsage
// above, and shares its io.Writer/io.TeeReader role (see ServeHTTP) and its
// bounded-single-event buffering (see anthropicSSEUsage.Write's own doc
// comment -- the same reasoning applies verbatim here). It is simpler than
// the Anthropic parser: an OpenAI-compatible stream that requested
// stream_options.include_usage carries the usage object, already summed,
// on exactly one chunk (conventionally the last, immediately before the
// terminal "data: [DONE]"), not as a running input/cumulative-output split
// across multiple events -- so this only needs to remember the last usage
// object observed, not reconstruct one from several.
type openAISSEUsage struct {
	buffer       []byte
	dataLines    []string
	eventBytes   int
	inputTokens  *int
	cachedTokens int
	outputTokens *int
	done         bool
	err          error
}

func (u *openAISSEUsage) Write(p []byte) (int, error) {
	if u.err != nil || u.done {
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
			if u.err != nil || u.done {
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

func (u *openAISSEUsage) parseEvent() {
	u.eventBytes = 0
	if len(u.dataLines) == 0 {
		return
	}
	data := strings.Join(u.dataLines, "\n")
	u.dataLines = u.dataLines[:0]
	if data == "[DONE]" {
		u.done = true
		u.buffer = nil
		return
	}
	var event struct {
		Usage *struct {
			PromptTokens        *int `json:"prompt_tokens"`
			CompletionTokens    *int `json:"completion_tokens"`
			PromptTokensDetails *struct {
				CachedTokens *int `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
		} `json:"usage"`
	}
	if err := json.Unmarshal([]byte(data), &event); err != nil {
		u.fail(fmt.Errorf("parse upstream event stream usage: %w", err))
		return
	}
	// Most chunks carry no usage object at all (only the final one does,
	// and only when the request asked for it) -- absence here is not an
	// error, it just leaves the last-seen value in place.
	if event.Usage == nil || event.Usage.PromptTokens == nil || event.Usage.CompletionTokens == nil {
		return
	}
	if *event.Usage.PromptTokens < 0 || *event.Usage.CompletionTokens < 0 {
		u.fail(fmt.Errorf("parse upstream event stream usage: token counts must not be negative"))
		return
	}
	var cached int
	if event.Usage.PromptTokensDetails != nil {
		cachedValue, cachedErr := nonNegativeIntField(event.Usage.PromptTokensDetails.CachedTokens)
		if cachedErr != nil {
			u.fail(fmt.Errorf("parse upstream event stream usage: token counts must not be negative"))
			return
		}
		cached = cachedValue
	}
	promptTokens := *event.Usage.PromptTokens
	completionTokens := *event.Usage.CompletionTokens
	u.inputTokens = &promptTokens
	u.cachedTokens = cached
	u.outputTokens = &completionTokens
}

func (u *openAISSEUsage) fail(err error) {
	u.err = err
	u.buffer = nil
	u.dataLines = nil
	u.eventBytes = 0
}

// Usage requires the terminal "data: [DONE]" event (see parseEvent), the
// same completeness guard anthropicSSEUsage.usage applies via its own
// message_stop requirement: a stream truncated by a dropped connection must
// not book a partial/absent usage observation as if it were authoritative.
func (u *openAISSEUsage) Usage() (costInputTokens, meterInputTokens, cachedTokens, cacheWriteTokens, outputTokens int, err error) {
	if u.err != nil {
		return 0, 0, 0, 0, 0, u.err
	}
	if !u.done {
		return 0, 0, 0, 0, 0, fmt.Errorf("parse upstream event stream usage: missing terminal [DONE] event")
	}
	if u.inputTokens == nil || u.outputTokens == nil {
		return 0, 0, 0, 0, 0, fmt.Errorf("parse upstream event stream usage: no usage object observed (the request may not have set stream_options.include_usage)")
	}
	maxInt := int(^uint(0) >> 1)
	if *u.inputTokens > maxInt-*u.outputTokens {
		return 0, 0, 0, 0, 0, fmt.Errorf("parse upstream event stream usage: token count overflow")
	}
	cached := min(u.cachedTokens, *u.inputTokens)
	return *u.inputTokens - cached, *u.inputTokens, cached, 0, *u.outputTokens, nil
}
