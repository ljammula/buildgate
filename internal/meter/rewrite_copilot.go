package meter

import "encoding/json"

// The four identifying values below are the static headers pi's own
// github-copilot provider sends (see routes.go's
// source citation); exported so the relay's token exchange uses the same bytes.
const (
	CopilotUserAgent           = "GitHubCopilotChat/0.35.0"
	CopilotEditorVersion       = "vscode/1.107.0"
	CopilotEditorPluginVersion = "copilot-chat/0.35.0"
	CopilotIntegrationID       = "vscode-chat"
)

// CopilotExtraHeaders is every header pi's own github-copilot provider adds
// to an OpenAI-compatible request, forced onto every request this relay
// forwards in CredentialModeGitHubCopilot -- see routes.go's
// source citation. requestBody is the worker's own (already-read) request
// body, consulted only to infer X-Initiator the same way pi's
// inferCopilotInitiator does.
func CopilotExtraHeaders(requestBody []byte, requestFormat string) map[string]string {
	return map[string]string{
		"User-Agent":             CopilotUserAgent,
		"Editor-Version":         CopilotEditorVersion,
		"Editor-Plugin-Version":  CopilotEditorPluginVersion,
		"Copilot-Integration-Id": CopilotIntegrationID,
		"Openai-Intent":          "conversation-edits",
		"X-Initiator":            copilotInitiator(requestBody, requestFormat),
	}
}

// copilotInitiator mirrors inferCopilotInitiator in pi's own
// github-copilot-headers.ts: "agent" when the request's last input/message is
// not from the user (e.g. a follow-up after a tool result), "user" otherwise
// -- including when requestBody does not parse as the expected shape at all,
// matching that function's own fallback for an empty input/message list.
func copilotInitiator(requestBody []byte, requestFormat string) string {
	var parsed struct {
		Messages []struct {
			Role string `json:"role"`
		} `json:"messages"`
		Input []struct {
			Role string `json:"role"`
		} `json:"input"`
	}

	if err := json.Unmarshal(requestBody, &parsed); err != nil || len(parsed.Messages) == 0 {
		if err != nil || requestFormat != RequestFormatOpenAIResponses || len(parsed.Input) == 0 {
			return "user"
		}
		if parsed.Input[len(parsed.Input)-1].Role != "user" {
			return "agent"
		}
		return "user"
	}
	if requestFormat == RequestFormatOpenAIResponses {
		if len(parsed.Input) == 0 || parsed.Input[len(parsed.Input)-1].Role == "user" {
			return "user"
		}
		return "agent"
	}
	if parsed.Messages[len(parsed.Messages)-1].Role != "user" {
		return "agent"
	}
	return "user"
}

// NormalizeCopilotRequest strips request fields GitHub's Copilot Chat
// Completions route does not accept, for its two current callers on
// this route -- Pi and pifork, whichever engine relay_credential_mode:
// github-copilot is configured under (server.go's request path applies
// this unconditionally in that mode, not gated on a specific engine):
// private BYOK metadata ("snippy"), explicit sampling/reasoning controls
// ("temperature", "reasoning_effort"), and non-function custom tools.
//
// The "temperature"/"reasoning_effort" strip is deliberate and has a real
// consequence worth stating plainly: a session-config role's configured
// thinking/reasoning effort (internal/sessionconfig's own roles: block --
// see relay/effort.go, which is what sets reasoning_effort on the
// request in the first place) never reaches Copilot on this route. The
// field is removed here, silently, before the request leaves this
// process, not merely ignored upstream. This is kept as-is rather than
// changed: whether Copilot's Chat Completions endpoint would actually
// accept either field is exactly what the live Copilot walk validates,
// and this relay has not yet been re-verified against a build that
// leaves them in.
//
// Malformed JSON is left for the upstream to reject.
func NormalizeCopilotRequest(requestBody []byte) []byte {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(requestBody, &fields); err != nil {
		return requestBody
	}
	_, hasSnippy := fields["snippy"]
	_, hasTemperature := fields["temperature"]
	_, hasReasoningEffort := fields["reasoning_effort"]
	changed := hasSnippy || hasTemperature || hasReasoningEffort
	if toolsJSON, present := fields["tools"]; present {
		var tools []json.RawMessage
		if err := json.Unmarshal(toolsJSON, &tools); err == nil {
			functionTools := make([]json.RawMessage, 0, len(tools))
			for _, toolJSON := range tools {
				var tool struct {
					Type string `json:"type"`
				}
				if json.Unmarshal(toolJSON, &tool) == nil && tool.Type == "function" {
					functionTools = append(functionTools, toolJSON)
				} else {
					changed = true
				}
			}
			if len(functionTools) != len(tools) {
				normalizedTools, err := json.Marshal(functionTools)
				if err == nil {
					fields["tools"] = normalizedTools
				}
			}
		}
	}
	if !changed {
		return requestBody
	}
	delete(fields, "snippy")
	delete(fields, "temperature")
	delete(fields, "reasoning_effort")
	normalized, err := json.Marshal(fields)
	if err != nil {
		return requestBody
	}
	return normalized
}

// NormalizeCopilotResponsesRequest removes only the Copilot CLI metadata
// field that is not part of the Responses API contract. Responses-specific
// fields such as input, reasoning, previous_response_id, and custom tools
// are intentionally preserved: unlike the Chat Completions compatibility
// path, they are part of the API shape used by entitled GPT-5.6 models.
func NormalizeCopilotResponsesRequest(requestBody []byte) []byte {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(requestBody, &fields); err != nil {
		return requestBody
	}
	if _, present := fields["snippy"]; !present {
		return requestBody
	}
	delete(fields, "snippy")
	normalized, err := json.Marshal(fields)
	if err != nil {
		return requestBody
	}
	return normalized
}
