package meter

const (
	UsageFormatAnthropic       = "anthropic"
	UsageFormatOpenAI          = "openai"
	UsageFormatOpenAIResponses = "openai-responses"

	RequestFormatAnthropic         = "anthropic"
	RequestFormatOpenAICompletions = "openai-completions"
	RequestFormatOpenAIResponses   = "openai-responses"
)

// WorkerAPIAnthropicMessages is a model's api for Anthropic's native Messages
// API (models.<m>.api). The meter calls the same wire shape
// RequestFormatAnthropic and UsageFormatAnthropic.
const WorkerAPIAnthropicMessages = "anthropic-messages"

// RequestFormatForWorkerAPI is the meter's request format for a model's api:
// anthropic-messages is RequestFormatAnthropic, an empty api is
// RequestFormatOpenAICompletions, and every other value is its own name.
func RequestFormatForWorkerAPI(api string) string {
	switch api {
	case "":
		return RequestFormatOpenAICompletions
	case WorkerAPIAnthropicMessages:
		return RequestFormatAnthropic
	}
	return api
}

// UsageFormatForWorkerAPI is the response shape the meter parses for a
// model's api.
func UsageFormatForWorkerAPI(api string) string {
	switch api {
	case WorkerAPIAnthropicMessages:
		return UsageFormatAnthropic
	case RequestFormatOpenAIResponses:
		return UsageFormatOpenAIResponses
	}
	return UsageFormatOpenAI
}
