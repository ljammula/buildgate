package sandbox

import (
	"fmt"
	"testing"

	"buildgate/internal/meter"
	"buildgate/internal/run"
)

// routePolicyViolations is one edit per rule of RoutePolicy.Validate, in the
// order Validate reports them. fields names what the edit sets: two edits
// that set the same field cannot be combined.
var routePolicyViolations = []struct {
	name   string
	fields []string
	edit   func(*RoutePolicy)
}{
	{"bad upstream", []string{"Upstream"}, func(p *RoutePolicy) { p.Upstream = "not a url" }},
	{"relative path prefix", []string{"AllowedPathPrefix"}, func(p *RoutePolicy) { p.AllowedPathPrefix = "v1" }},
	{"unknown usage format", []string{"UsageFormat"}, func(p *RoutePolicy) { p.UsageFormat = "other" }},
	{"unknown worker API", []string{"WorkerModelAPI"}, func(p *RoutePolicy) { p.WorkerModelAPI = "other" }},
	{"responses API without model id", []string{"WorkerModelAPI", "WorkerModelID"}, func(p *RoutePolicy) {
		p.WorkerModelAPI = meter.RequestFormatOpenAIResponses
	}},
	{"unknown credential header", []string{"UpstreamAuthHeader"}, func(p *RoutePolicy) { p.UpstreamAuthHeader = "Cookie" }},
	{"unknown auth mode", []string{"AuthMode"}, func(p *RoutePolicy) { p.AuthMode = "other" }},
	{"missing route", []string{"Route"}, func(p *RoutePolicy) { p.Route = "" }},
	{"route with control character", []string{"Route"}, func(p *RoutePolicy) { p.Route = "a\x01b" }},
	{"unknown billing", []string{"Billing"}, func(p *RoutePolicy) { p.Billing = "other" }},
	{"subscription billing on a static route", []string{"Billing", "AuthMode"}, func(p *RoutePolicy) {
		p.Billing = run.BillingSubscription
	}},
	{"copilot without model id", []string{"AuthMode", "WorkerModelID"}, func(p *RoutePolicy) {
		p.AuthMode = meter.CredentialModeGitHubCopilot
	}},
	{"copilot with another host", []string{"AuthMode", "WorkerModelID", "Upstream"}, func(p *RoutePolicy) {
		p.AuthMode, p.WorkerModelID, p.Upstream = meter.CredentialModeGitHubCopilot, "m", "https://models.example/v1"
	}},
	{"copilot with another path prefix", []string{"AuthMode", "WorkerModelID", "Upstream", "AllowedPathPrefix"}, func(p *RoutePolicy) {
		p.AuthMode, p.WorkerModelID, p.Upstream = meter.CredentialModeGitHubCopilot, "m", "https://api.githubcopilot.com"
		p.AllowedPathPrefix = "/other"
	}},
	{"chatgpt with another host", []string{"AuthMode", "Upstream"}, func(p *RoutePolicy) {
		p.AuthMode = meter.CredentialModeChatGPTCodex
	}},
	{"chatgpt without model id", []string{"AuthMode", "Upstream", "AllowedPathPrefix", "WorkerModelID"}, func(p *RoutePolicy) {
		p.AuthMode, p.Upstream = meter.CredentialModeChatGPTCodex, meter.ChatGPTCodexAPIBase
		p.AllowedPathPrefix = meter.ChatGPTCodexResponsesPath
	}},
	{"model id with tab", []string{"WorkerModelID"}, func(p *RoutePolicy) { p.WorkerModelID = "a\tb" }},
	{"model id with anthropic usage", []string{"WorkerModelID", "UsageFormat"}, func(p *RoutePolicy) {
		p.WorkerModelID, p.UsageFormat = "m", meter.UsageFormatAnthropic
	}},
	{"responses API with completions usage", []string{"WorkerModelID", "WorkerModelAPI", "UsageFormat"}, func(p *RoutePolicy) {
		p.WorkerModelID, p.WorkerModelAPI, p.UsageFormat = "m", meter.RequestFormatOpenAIResponses, meter.UsageFormatOpenAI
	}},
	{"completions API with responses usage", []string{"WorkerModelID", "WorkerModelAPI", "UsageFormat"}, func(p *RoutePolicy) {
		p.WorkerModelID, p.UsageFormat = "m", meter.UsageFormatOpenAIResponses
	}},
	{"model id with slash", []string{"WorkerModelID"}, func(p *RoutePolicy) { p.WorkerModelID = "a/b" }},
	{"zero request size", []string{"MaxRequestBytes"}, func(p *RoutePolicy) { p.MaxRequestBytes = 0 }},
	{"zero request rate", []string{"RequestsPerMinute"}, func(p *RoutePolicy) { p.RequestsPerMinute = 0 }},
	{"zero token window", []string{"TokenBudgetWindow"}, func(p *RoutePolicy) { p.TokenBudgetWindow = 0 }},
	{"zero cost window", []string{"CostBudgetWindow"}, func(p *RoutePolicy) { p.CostBudgetWindow = 0 }},
	{"negative output price", []string{"OutputMicroUSDPerMTok"}, func(p *RoutePolicy) { p.OutputMicroUSDPerMTok = -1 }},
	{"cached price above input price", []string{"CachedInputMicroUSDPerMTok"}, func(p *RoutePolicy) {
		p.CachedInputMicroUSDPerMTok = p.InputMicroUSDPerMTok + 1
	}},
	{"zero token ceiling", []string{"TokenCeiling"}, func(p *RoutePolicy) { p.TokenCeiling = 0 }},
	{"zero cost ceiling", []string{"CostCeilingMicroUSD"}, func(p *RoutePolicy) { p.CostCeilingMicroUSD = 0 }},
	{"token ceiling below budget", []string{"TokenCeiling"}, func(p *RoutePolicy) { p.TokenCeiling = 1 }},
	{"cost ceiling below budget", []string{"CostCeilingMicroUSD"}, func(p *RoutePolicy) { p.CostCeilingMicroUSD = 1 }},
}

func shareAField(a, b []string) bool {
	for _, x := range a {
		for _, y := range b {
			if x == y {
				return true
			}
		}
	}
	return false
}

// TestRoutePolicyValidateReportsTheFirstViolatedRule pins the precedence of
// RoutePolicy.Validate: a policy that breaks two rules is refused with the
// error of the rule listed first in routePolicyViolations, the same error
// that rule gives alone.
func TestRoutePolicyValidateReportsTheFirstViolatedRule(t *testing.T) {
	if err := validRelayPolicy().Validate(); err != nil {
		t.Fatalf("valid policy refused: %v", err)
	}
	alone := make([]string, len(routePolicyViolations))
	seen := map[string]string{}
	for i, v := range routePolicyViolations {
		policy := validRelayPolicy()
		v.edit(&policy)
		err := policy.Validate()
		if err == nil {
			t.Fatalf("%s: accepted", v.name)
		}
		alone[i] = err.Error()
		if other, ok := seen[alone[i]]; ok {
			t.Fatalf("%s and %s give the same error %q, so the pairs below could not tell them apart", v.name, other, alone[i])
		}
		seen[alone[i]] = v.name
	}
	for i, first := range routePolicyViolations {
		for j := i + 1; j < len(routePolicyViolations); j++ {
			second := routePolicyViolations[j]
			if shareAField(first.fields, second.fields) {
				continue
			}
			t.Run(fmt.Sprintf("%s+%s", first.name, second.name), func(t *testing.T) {
				policy := validRelayPolicy()
				first.edit(&policy)
				second.edit(&policy)
				err := policy.Validate()
				if err == nil || err.Error() != alone[i] {
					t.Fatalf("got %v, want the first rule's error %q", err, alone[i])
				}
			})
		}
	}
}
