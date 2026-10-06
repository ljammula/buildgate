package meter

import (
	"fmt"
	"math"
	"regexp"

	"google.golang.org/protobuf/types/known/structpb"
)

// RewriteGitHubCopilot is the Policy.Rewrite value that applies the Copilot
// request-body rewrites of rewrite_copilot.go.
const RewriteGitHubCopilot = "github-copilot"

// RouteChatGPTCodex is the Policy.Route value whose 401/403 responses are
// withheld from the sandbox (the relay does the same for its chatgpt-codex
// credential mode).
const RouteChatGPTCodex = "chatgpt-codex"

const (
	// maxPolicyInteger is the largest integer a protobuf Struct (float64)
	// number carries exactly.
	maxPolicyInteger = 1 << 53
	// DefaultMaxRequestBytes is Policy.MaxRequestBytes when unset: the
	// binding's own payload cap.
	DefaultMaxRequestBytes = 4 << 20
)

var runNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// Policy is the per-sandbox configuration the gateway passes with every
// evaluation, decoded strictly from a google.protobuf.Struct: an unknown key
// is an error. Ceilings are the REMAINING ceilings for this sandbox (0
// disables one); every integer is non-negative.
type Policy struct {
	Run           string // names the ledger directory
	Sandbox       string // must equal the request's sandbox name
	Route         string
	UsageFormat   string
	RequestFormat string
	Rewrite       string

	TokenCeiling        int64
	CostCeilingMicroUSD int64
	Prices              Prices

	TokenBudget        int64
	TokenWindowSeconds int64
	CostBudgetMicroUSD int64
	CostWindowSeconds  int64
	RequestsPerMinute  int64
	MaxRequestBytes    int64
}

// DecodePolicy decodes and validates a Struct. A nil Struct is an empty one
// and fails on the missing run.
func DecodePolicy(config *structpb.Struct) (Policy, error) {
	var p Policy
	for key, value := range config.GetFields() {
		if err := p.set(key, value); err != nil {
			return Policy{}, err
		}
	}
	if err := p.finish(); err != nil {
		return Policy{}, err
	}
	return p, nil
}

func (p *Policy) set(key string, value *structpb.Value) error {
	if field := p.stringField(key); field != nil {
		s, ok := value.GetKind().(*structpb.Value_StringValue)
		if !ok {
			return fmt.Errorf("config key %q must be a string", key)
		}
		*field = s.StringValue
		return nil
	}
	if field := p.intField(key); field != nil {
		n, err := integerValue(key, value)
		if err != nil {
			return err
		}
		*field = n
		return nil
	}
	return fmt.Errorf("unknown config key %q", key)
}

func (p *Policy) stringField(key string) *string {
	switch key {
	case "run":
		return &p.Run
	case "sandbox":
		return &p.Sandbox
	case "route":
		return &p.Route
	case "usage_format":
		return &p.UsageFormat
	case "request_format":
		return &p.RequestFormat
	case "rewrite":
		return &p.Rewrite
	}
	return nil
}

func (p *Policy) intField(key string) *int64 {
	switch key {
	case "token_ceiling":
		return &p.TokenCeiling
	case "cost_ceiling_micro_usd":
		return &p.CostCeilingMicroUSD
	case "input_price_micro_usd_per_mtok":
		return &p.Prices.InputMicroUSDPerMTok
	case "cached_input_price_micro_usd_per_mtok":
		return &p.Prices.CachedInputMicroUSDPerMTok
	case "cache_write_price_micro_usd_per_mtok":
		return &p.Prices.CacheWriteMicroUSDPerMTok
	case "output_price_micro_usd_per_mtok":
		return &p.Prices.OutputMicroUSDPerMTok
	case "token_budget":
		return &p.TokenBudget
	case "token_window_seconds":
		return &p.TokenWindowSeconds
	case "cost_budget_micro_usd":
		return &p.CostBudgetMicroUSD
	case "cost_window_seconds":
		return &p.CostWindowSeconds
	case "requests_per_minute":
		return &p.RequestsPerMinute
	case "max_request_bytes":
		return &p.MaxRequestBytes
	}
	return nil
}

// integerValue reads a Struct number as a non-negative integer no larger
// than 2^53.
func integerValue(key string, value *structpb.Value) (int64, error) {
	number, ok := value.GetKind().(*structpb.Value_NumberValue)
	if !ok {
		return 0, fmt.Errorf("config key %q must be a number", key)
	}
	v := number.NumberValue
	switch {
	case math.IsNaN(v) || v != math.Trunc(v):
		return 0, fmt.Errorf("config key %q must be an integer, got %v", key, v)
	case v < 0:
		return 0, fmt.Errorf("config key %q must not be negative, got %v", key, v)
	case v > maxPolicyInteger:
		return 0, fmt.Errorf("config key %q exceeds 2^53, got %v", key, v)
	}
	return int64(v), nil
}

// finish applies defaults and checks the cross-field rules.
func (p *Policy) finish() error {
	if !runNamePattern.MatchString(p.Run) {
		return fmt.Errorf("config key %q must match %s, got %q", "run", runNamePattern, p.Run)
	}
	if p.Sandbox == "" {
		return fmt.Errorf("config key %q is required", "sandbox")
	}
	if err := p.finishFormats(); err != nil {
		return err
	}
	if p.MaxRequestBytes == 0 {
		p.MaxRequestBytes = DefaultMaxRequestBytes
	}
	if p.MaxRequestBytes > DefaultMaxRequestBytes {
		return fmt.Errorf("config key %q exceeds the %d byte payload cap", "max_request_bytes", DefaultMaxRequestBytes)
	}
	if p.TokenBudget > 0 && p.TokenWindowSeconds == 0 {
		return fmt.Errorf("token_budget needs token_window_seconds")
	}
	if p.CostBudgetMicroUSD > 0 && p.CostWindowSeconds == 0 {
		return fmt.Errorf("cost_budget_micro_usd needs cost_window_seconds")
	}
	return nil
}

func (p *Policy) finishFormats() error {
	if p.UsageFormat == "" {
		p.UsageFormat = UsageFormatAnthropic
	}
	if p.RequestFormat == "" {
		p.RequestFormat = RequestFormatAnthropic
	}
	switch p.UsageFormat {
	case UsageFormatAnthropic, UsageFormatOpenAI, UsageFormatOpenAIResponses:
	default:
		return fmt.Errorf("config key %q has unknown value %q", "usage_format", p.UsageFormat)
	}
	switch p.RequestFormat {
	case RequestFormatAnthropic, RequestFormatOpenAICompletions, RequestFormatOpenAIResponses:
	default:
		return fmt.Errorf("config key %q has unknown value %q", "request_format", p.RequestFormat)
	}
	if p.Rewrite != "" && p.Rewrite != RewriteGitHubCopilot {
		return fmt.Errorf("config key %q has unknown value %q", "rewrite", p.Rewrite)
	}
	return nil
}

// RewriteBody applies the policy's request-body rewrite, or returns body
// unchanged when none is configured.
func (p Policy) RewriteBody(body []byte) []byte {
	if p.Rewrite != RewriteGitHubCopilot {
		return body
	}
	if p.RequestFormat == RequestFormatOpenAIResponses {
		return NormalizeCopilotResponsesRequest(body)
	}
	return NormalizeCopilotRequest(body)
}
