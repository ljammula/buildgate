package meter

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// anthropicMessageUsage returns the input/output token counts separately —
// callers combine them differently (a simple sum for the token budget, a
// price-weighted sum for the cost budget), so this stops short of adding
// them itself the way its predecessor (anthropicMessageTokens) did.
//
// costInputTokens is exactly usage.input_tokens, unchanged from before
// cachedInputWeightPercent existed -- Anthropic's Messages API documents
// input_tokens as covering only tokens after the last cache breakpoint,
// excluding cache_creation_input_tokens and cache_read_input_tokens
// entirely, so this relay's cost accounting (which uses costInputTokens,
// never meterInputTokens) is byte-for-byte unaffected by this function's
// own cache-field parsing. meterInputTokens folds both cache fields in --
// cache_creation_input_tokens at full weight (it is a write, not a hit),
// cache_read_input_tokens as cachedTokens, the subset WeightedInputTokens
// (see addUsage) discounts. Both fields are optional: an upstream/model
// that doesn't report prompt caching at all simply omits them, read here
// as 0 by nonNegativeIntField.
func anthropicMessageUsage(body []byte) (costInputTokens, meterInputTokens, cachedTokens, cacheWriteTokens, outputTokens int, err error) {
	var message struct {
		Usage *struct {
			InputTokens              *int `json:"input_tokens"`
			OutputTokens             *int `json:"output_tokens"`
			CacheCreationInputTokens *int `json:"cache_creation_input_tokens"`
			CacheReadInputTokens     *int `json:"cache_read_input_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &message); err != nil {
		return 0, 0, 0, 0, 0, fmt.Errorf("parse upstream usage: %w", err)
	}
	if message.Usage == nil || message.Usage.InputTokens == nil || message.Usage.OutputTokens == nil {
		return 0, 0, 0, 0, 0, fmt.Errorf("parse upstream usage: missing input_tokens or output_tokens")
	}
	input := *message.Usage.InputTokens
	output := *message.Usage.OutputTokens
	if input < 0 || output < 0 {
		return 0, 0, 0, 0, 0, fmt.Errorf("parse upstream usage: token counts must not be negative")
	}
	cacheCreation, creationErr := nonNegativeIntField(message.Usage.CacheCreationInputTokens)
	cacheRead, readErr := nonNegativeIntField(message.Usage.CacheReadInputTokens)
	if creationErr != nil || readErr != nil {
		return 0, 0, 0, 0, 0, fmt.Errorf("parse upstream usage: token counts must not be negative")
	}
	maxInt := int64(^uint(0) >> 1)
	meterInput := int64(input) + int64(cacheCreation) + int64(cacheRead)
	if meterInput > maxInt-int64(output) {
		return 0, 0, 0, 0, 0, fmt.Errorf("parse upstream usage: token count overflow")
	}
	return input, int(meterInput), cacheRead, cacheCreation, output, nil
}

// nonNegativeIntField returns 0 for a nil optional integer usage field (the
// upstream/model doesn't report it at all -- absence is normal, not an
// anomaly), or an error if the field is present but negative. Shared by
// every per-format cache-token field (Anthropic's cache_creation_input_
// tokens/cache_read_input_tokens, the OpenAI-shaped formats' cached_tokens)
// so the same non-negative validation applies uniformly.
func nonNegativeIntField(field *int) (int, error) {
	if field == nil {
		return 0, nil
	}
	if *field < 0 {
		return 0, fmt.Errorf("token counts must not be negative")
	}
	return *field, nil
}

const MaxSSEEventBytes = 1 << 20

// anthropicSSEUsage is an io.Writer so it can observe the exact byte flow being
// copied to the client without holding the full response. It retains only one
// bounded SSE event and the authoritative counts needed after a complete copy.
type anthropicSSEUsage struct {
	buffer              []byte
	dataLines           []string
	eventBytes          int
	inputTokens         *int
	cacheCreationTokens int
	cacheReadTokens     int
	maxOutputTokens     *int
	stopped             bool
	err                 error
}

// Write incrementally parses complete SSE lines while io.TeeReader forwards the
// same bytes unchanged. Parsing is abandoned if one event exceeds a bounded
// size, so a malformed upstream cannot turn metering into unbounded buffering;
// the client passthrough continues because Write still accepts every byte.
func (u *anthropicSSEUsage) Write(p []byte) (int, error) {
	if u.err != nil || u.stopped {
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
			if u.err != nil || u.stopped {
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

func (u *anthropicSSEUsage) parseEvent() {
	u.eventBytes = 0
	if len(u.dataLines) == 0 {
		return
	}
	data := strings.Join(u.dataLines, "\n")
	u.dataLines = u.dataLines[:0]
	// "[DONE]" is the OpenAI Chat Completions terminal marker; an Anthropic
	// stream has no such sentinel (it ends with a typed message_stop event).
	// Seeing one here is therefore not a malformed Anthropic stream, it is
	// proof this relay was pointed at an OpenAI-compatible upstream while
	// still configured with UsageFormatAnthropic -- so say that, rather than
	// letting json.Unmarshal report "invalid character 'D' looking for
	// beginning of value" ('[' opens an array, 'D' cannot start a value).
	//
	// Worth a dedicated branch because that generic message is what this
	// mismatch actually looks like in production, and it is deeply
	// misleading (found live, 2026-09-08, debugging a notes app repo ticket
	// build): every OpenAI content chunk unmarshals *cleanly* into the
	// struct below -- unknown fields are ignored, Type is "", and the switch
	// on it matches no case -- so a wholly wrong-format stream is
	// indistinguishable from a healthy one until its very last event, and
	// then fails pointing at the one line that is not the problem.
	//
	// Deliberately not a `default:` case on Type instead: a well-formed
	// Anthropic stream legitimately carries ping/content_block_* events this
	// parser has no case for, so an unrecognized type is normal and only
	// this sentinel is unambiguous.
	if data == "[DONE]" || data == "DONE" {
		u.fail(errors.New("parse upstream event stream usage: upstream sent an OpenAI-compatible terminal event while usage format is \"anthropic\" -- set the relay's usage format to \"openai\" for this upstream"))
		return
	}
	var event struct {
		Type    string `json:"type"`
		Message *struct {
			Usage *struct {
				InputTokens              *int `json:"input_tokens"`
				CacheCreationInputTokens *int `json:"cache_creation_input_tokens"`
				CacheReadInputTokens     *int `json:"cache_read_input_tokens"`
			} `json:"usage"`
		} `json:"message"`
		Usage *struct {
			OutputTokens *int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal([]byte(data), &event); err != nil {
		u.fail(fmt.Errorf("parse upstream event stream usage: %w", err))
		return
	}
	switch event.Type {
	case "message_start":
		if u.inputTokens != nil {
			u.fail(fmt.Errorf("parse upstream event stream usage: duplicate message_start"))
			return
		}
		if event.Message == nil || event.Message.Usage == nil || event.Message.Usage.InputTokens == nil {
			u.fail(fmt.Errorf("parse upstream event stream usage: message_start missing input_tokens"))
			return
		}
		if *event.Message.Usage.InputTokens < 0 {
			u.fail(fmt.Errorf("parse upstream event stream usage: token counts must not be negative"))
			return
		}
		inputTokens := *event.Message.Usage.InputTokens
		u.inputTokens = &inputTokens
		// See anthropicMessageUsage's own doc comment: both fields are
		// optional (0 when the upstream/model doesn't report prompt
		// caching), cache_creation_input_tokens counts at full weight,
		// cache_read_input_tokens is the cached subset WeightedInputTokens
		// discounts.
		cacheCreation, creationErr := nonNegativeIntField(event.Message.Usage.CacheCreationInputTokens)
		cacheRead, readErr := nonNegativeIntField(event.Message.Usage.CacheReadInputTokens)
		if creationErr != nil || readErr != nil {
			u.fail(fmt.Errorf("parse upstream event stream usage: token counts must not be negative"))
			return
		}
		u.cacheCreationTokens = cacheCreation
		u.cacheReadTokens = cacheRead
	case "message_delta":
		if u.inputTokens == nil {
			u.fail(fmt.Errorf("parse upstream event stream usage: message_delta before message_start"))
			return
		}
		if event.Usage == nil || event.Usage.OutputTokens == nil {
			u.fail(fmt.Errorf("parse upstream event stream usage: message_delta missing output_tokens"))
			return
		}
		if *event.Usage.OutputTokens < 0 {
			u.fail(fmt.Errorf("parse upstream event stream usage: token counts must not be negative"))
			return
		}
		// Anthropic documents output_tokens as cumulative. Keep the maximum
		// observed value so a regressing or reordered delta cannot undercount.
		if u.maxOutputTokens == nil || *event.Usage.OutputTokens > *u.maxOutputTokens {
			outputTokens := *event.Usage.OutputTokens
			u.maxOutputTokens = &outputTokens
		}
	case "message_stop":
		u.stopped = true
		u.buffer = nil
		u.dataLines = nil
	}
}

func (u *anthropicSSEUsage) fail(err error) {
	u.err = err
	u.buffer = nil
	u.dataLines = nil
	u.eventBytes = 0
}

// Usage returns counts only for a complete, well-formed Anthropic stream. A
// message_stop is required so EOF from a truncated upstream cannot book a
// partial response, and the sum is checked before addUsage feeds the integer
// window limiter so malformed counts cannot overflow its accounting.
func (u *anthropicSSEUsage) Usage() (costInputTokens, meterInputTokens, cachedTokens, cacheWriteTokens, outputTokens int, err error) {
	if u.err != nil {
		return 0, 0, 0, 0, 0, u.err
	}
	if !u.stopped {
		return 0, 0, 0, 0, 0, fmt.Errorf("parse upstream event stream usage: missing message_stop")
	}
	if u.inputTokens == nil || u.maxOutputTokens == nil {
		return 0, 0, 0, 0, 0, fmt.Errorf("parse upstream event stream usage: missing input_tokens or output_tokens")
	}
	maxInt := int64(^uint(0) >> 1)
	meterInput := int64(*u.inputTokens) + int64(u.cacheCreationTokens) + int64(u.cacheReadTokens)
	if meterInput > maxInt-int64(*u.maxOutputTokens) {
		return 0, 0, 0, 0, 0, fmt.Errorf("parse upstream event stream usage: token count overflow")
	}
	return *u.inputTokens, int(meterInput), u.cacheReadTokens, u.cacheCreationTokens, *u.maxOutputTokens, nil
}
