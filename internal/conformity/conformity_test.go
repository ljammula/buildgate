package conformity

import (
	"testing"
	"time"

	"buildgate/internal/sandbox"
)

func TestArgsIncludesReviewBaseSHA(t *testing.T) {
	got := Args("/path/conformity_review.py", "/ws", "/ws/criteria.md", "required", "deadbeef", "", "pi")

	want := []string{
		"/path/conformity_review.py",
		"--workspace", "/ws",
		"--spec-acceptance-criteria", "/ws/criteria.md",
		"--conformity-policy", "required",
		"--review-base-sha", "deadbeef",
		"--harness", "pi",
	}
	if len(got) != len(want) {
		t.Fatalf("Args() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Args()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestArgsOmitsReviewBaseSHAWhenEmpty(t *testing.T) {
	got := Args("/path/conformity_review.py", "/ws", "/ws/criteria.md", "required", "", "", "pi")
	for i, arg := range got {
		if arg == "--review-base-sha" {
			t.Fatalf("Args() included --review-base-sha with an empty value at index %d: %v", i, got)
		}
	}
}

func TestArgsIncludesThinkingWhenSet(t *testing.T) {
	got := Args("/path/conformity_review.py", "/ws", "/ws/criteria.md", "required", "deadbeef", "max", "pi")

	want := []string{
		"/path/conformity_review.py",
		"--workspace", "/ws",
		"--spec-acceptance-criteria", "/ws/criteria.md",
		"--conformity-policy", "required",
		"--review-base-sha", "deadbeef",
		"--thinking", "max",
		"--harness", "pi",
	}
	if len(got) != len(want) {
		t.Fatalf("Args() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Args()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestArgsOmitsThinkingWhenEmpty(t *testing.T) {
	got := Args("/path/conformity_review.py", "/ws", "/ws/criteria.md", "required", "deadbeef", "", "pi")
	for i, arg := range got {
		if arg == "--thinking" {
			t.Fatalf("Args() included --thinking with an empty value at index %d: %v", i, got)
		}
	}
}

func relaySpecFixture() sandbox.RouteSpec {
	policy := sandbox.RoutePolicy{
		Route:                      "primary",
		Upstream:                   "https://api.anthropic.com",
		MaxRequestBytes:            1 << 20,
		RequestsPerMinute:          60,
		TokenBudget:                1000,
		TokenBudgetWindow:          time.Hour,
		CostBudgetMicroUSD:         1000,
		CostBudgetWindow:           time.Hour,
		InputMicroUSDPerMTok:       1000000,
		CachedInputMicroUSDPerMTok: 1000000,
		CacheWriteMicroUSDPerMTok:  1000000,
		OutputMicroUSDPerMTok:      1000000,
		TokenCeiling:               5000,
		CostCeilingMicroUSD:        5000,
	}
	return policy.Spec(sandbox.RouteSecret{}, sandbox.RouteSecret{}, sandbox.RouteSecret{}, sandbox.RouteSecret{}, "run-1", "/data")
}

func TestPhaseRelaySpecCarriesOverRemainingBudget(t *testing.T) {
	base := relaySpecFixture()
	got := PhaseRelaySpec(base, nil, 800, 800)

	if got.TokenCeiling != 4200 {
		t.Errorf("TokenCeiling = %d, want 4200 (5000 - 800)", got.TokenCeiling)
	}
	if got.CostCeilingMicroUSD != 4200 {
		t.Errorf("CostCeilingMicroUSD = %d, want 4200 (5000 - 800)", got.CostCeilingMicroUSD)
	}
	if got.TokenBudget != base.TokenBudget {
		t.Errorf("TokenBudget = %d, want unchanged %d", got.TokenBudget, base.TokenBudget)
	}
	if err := got.Validate(); err != nil {
		t.Errorf("Validate() = %v, want nil", err)
	}
}

func TestPhaseRelaySpecNeverExceedsTrueRemainingCeiling(t *testing.T) {
	base := relaySpecFixture() // TokenCeiling=5000, TokenBudget=1000
	got := PhaseRelaySpec(base, nil, 4900, 4900)

	if got.TokenCeiling != 100 {
		t.Fatalf("TokenCeiling = %d, want 100 (the true remaining amount, never inflated up to TokenBudget)", got.TokenCeiling)
	}
	if got.TokenBudget > got.TokenCeiling {
		t.Errorf("TokenBudget = %d > TokenCeiling = %d: total spend could now exceed the true remaining ceiling", got.TokenBudget, got.TokenCeiling)
	}
	if err := got.Validate(); err != nil {
		t.Errorf("Validate() = %v, want nil", err)
	}
}

func TestPhaseRelaySpecAppliesReviewModelFields(t *testing.T) {
	base := relaySpecFixture()
	base.WorkerModelID = "build-model"
	base.WorkerModelAPI = "openai-completions"
	base.WorkerBasePath = "/v1"
	base.WorkerModelExtraJSON = `{"reasoning":true}`

	got := PhaseRelaySpec(base, &ReviewRoute{
		WorkerModelID:        "review-model",
		WorkerModelAPI:       "openai-responses",
		WorkerBasePath:       "/v2",
		WorkerModelExtraJSON: `{"reasoning":true,"thinkingLevelMap":{"max":"xhigh"}}`,
		UsageFormat:          "openai-responses",
		AllowedPathPrefix:    "/responses",
	}, 800, 800)

	if got.WorkerModelID != "review-model" {
		t.Errorf("WorkerModelID = %q, want %q (the review role's alias, not the build's)", got.WorkerModelID, "review-model")
	}
	if got.WorkerModelAPI != "openai-responses" {
		t.Errorf("WorkerModelAPI = %q, want %q", got.WorkerModelAPI, "openai-responses")
	}
	if got.WorkerBasePath != "/v2" {
		t.Errorf("WorkerBasePath = %q, want %q", got.WorkerBasePath, "/v2")
	}
	if got.WorkerModelExtraJSON != `{"reasoning":true,"thinkingLevelMap":{"max":"xhigh"}}` {
		t.Errorf("WorkerModelExtraJSON = %q, want the review alias's own", got.WorkerModelExtraJSON)
	}
	if got.UsageFormat != "openai-responses" {
		t.Errorf("UsageFormat = %q, want %q (the review role's own, not the build's)", got.UsageFormat, "openai-responses")
	}
	if got.AllowedPathPrefix != "/responses" {
		t.Errorf("AllowedPathPrefix = %q, want %q", got.AllowedPathPrefix, "/responses")
	}
	// Ceiling arithmetic is unaffected by the model-field override --
	// same remaining-budget math as TestPhaseRelaySpecCarriesOverRemainingBudget.
	if got.TokenCeiling != 4200 {
		t.Errorf("TokenCeiling = %d, want 4200 (5000 - 800)", got.TokenCeiling)
	}
	if got.CostCeilingMicroUSD != 4200 {
		t.Errorf("CostCeilingMicroUSD = %d, want 4200 (5000 - 800)", got.CostCeilingMicroUSD)
	}
	// Per-token prices stay the build's own -- per-role pricing is a
	// later phase (see PhaseRelaySpec's own doc comment).
	if got.InputMicroUSDPerMTok != base.InputMicroUSDPerMTok {
		t.Errorf("InputMicroUSDPerMTok = %d, want unchanged %d", got.InputMicroUSDPerMTok, base.InputMicroUSDPerMTok)
	}
	if err := got.Validate(); err != nil {
		t.Errorf("Validate() = %v, want nil", err)
	}
}

// TestPhaseRelaySpecUsesReviewPolicyAndRemainingCeiling proves a routes:
// mode ReviewRoute (Upstream set) replaces the build's own full relay
// identity -- upstream, auth mode, header, route, billing, and
// credentials -- not just its worker-model fields, while the remaining-
// ceiling arithmetic still derives from base's own ceilings exactly as
// the legacy (nil-review, or a ReviewRoute with Upstream=="") path
// already proves in TestPhaseRelaySpecCarriesOverRemainingBudget/
// TestPhaseRelaySpecAppliesReviewModelFields above.
func TestPhaseRelaySpecUsesReviewPolicyAndRemainingCeiling(t *testing.T) {
	base := relaySpecFixture()
	base.Upstream = "https://build.example.invalid"
	base.AuthMode = "static"
	base.Route = "build-route"
	base.Billing = "metered"
	base.InputMicroUSDPerMTok = 3000000
	base.OutputMicroUSDPerMTok = 15000000
	base.APIKey = sandbox.NewRouteSecret("build-secret")

	// The review route is a plaintext local model with no real
	// credential to protect, at different prices than the build's own
	// route -- every one of these must come from the review policy, not
	// leak over from base.
	got := PhaseRelaySpec(base, &ReviewRoute{
		WorkerModelID:                "review-model",
		Upstream:                     "http://127.0.0.1:8080",
		AuthMode:                     "static",
		UpstreamAuthHeader:           "Authorization",
		Route:                        "review-route",
		Billing:                      "metered",
		AllowPlaintextUpstream:       true,
		AllowUnauthenticatedUpstream: true,
		InputMicroUSDPerMTok:         1000000,
		CachedInputMicroUSDPerMTok:   1000000,
		CacheWriteMicroUSDPerMTok:    1000000,
		OutputMicroUSDPerMTok:        2000000,
	}, 800, 800)

	if got.Upstream != "http://127.0.0.1:8080" {
		t.Errorf("Upstream = %q, want the review route's own upstream", got.Upstream)
	}
	if got.UpstreamAuthHeader != "Authorization" {
		t.Errorf("UpstreamAuthHeader = %q, want the review route's own header", got.UpstreamAuthHeader)
	}
	if got.Route != "review-route" {
		t.Errorf("Route = %q, want %q", got.Route, "review-route")
	}
	if got.Billing != "metered" {
		t.Errorf("Billing = %q, want %q", got.Billing, "metered")
	}
	if !got.AllowPlaintextUpstream {
		t.Error("AllowPlaintextUpstream = false, want the review route's own true")
	}
	if !got.AllowUnauthenticatedUpstream {
		t.Error("AllowUnauthenticatedUpstream = false, want the review route's own true")
	}
	if got.InputMicroUSDPerMTok != 1000000 || got.OutputMicroUSDPerMTok != 2000000 {
		t.Errorf("CostPer{Input,Output}TokenMicroUSD = %d/%d, want the review route's own 1/2, not the build's 3/15", got.InputMicroUSDPerMTok, got.OutputMicroUSDPerMTok)
	}
	if got.APIKey.Configured() {
		t.Error("APIKey configured, want it cleared -- the review route carries no credential (AllowUnauthenticatedUpstream) and must not leak the build's own")
	}
	// Ceiling arithmetic still derives from base's own ceilings (5000
	// each), unaffected by the review route's different upstream/prices.
	if got.TokenCeiling != 4200 {
		t.Errorf("TokenCeiling = %d, want 4200 (5000 - 800)", got.TokenCeiling)
	}
	if got.CostCeilingMicroUSD != 4200 {
		t.Errorf("CostCeilingMicroUSD = %d, want 4200 (5000 - 800)", got.CostCeilingMicroUSD)
	}
	if err := got.ValidateUpstreamScheme(); err != nil {
		t.Errorf("ValidateUpstreamScheme() = %v, want nil", err)
	}
	if err := got.Validate(); err != nil {
		t.Errorf("Validate() = %v, want nil", err)
	}
}

// TestPhaseRelaySpecLegacyReviewNeverTouchesUpstreamOrCredentials proves
// a legacy ReviewRoute (Upstream left "") never changes base's own
// Upstream/AuthMode/Route/Billing/credentials -- only its worker-model
// fields, exactly as before this design's routes: mode fields existed.
func TestPhaseRelaySpecLegacyReviewNeverTouchesUpstreamOrCredentials(t *testing.T) {
	base := relaySpecFixture()
	base.Upstream = "https://build.example.invalid"
	base.AuthMode = "static"
	base.Route = "build-route"
	base.Billing = "metered"

	got := PhaseRelaySpec(base, &ReviewRoute{WorkerModelID: "review-model"}, 800, 800)

	if got.Upstream != base.Upstream {
		t.Errorf("Upstream = %q, want unchanged %q (legacy ReviewRoute carries no upstream)", got.Upstream, base.Upstream)
	}
	if got.AuthMode != base.AuthMode {
		t.Errorf("AuthMode = %q, want unchanged %q", got.AuthMode, base.AuthMode)
	}
	if got.Route != base.Route {
		t.Errorf("Route = %q, want unchanged %q", got.Route, base.Route)
	}
	if got.Billing != base.Billing {
		t.Errorf("Billing = %q, want unchanged %q", got.Billing, base.Billing)
	}
	if got.WorkerModelID != "review-model" {
		t.Errorf("WorkerModelID = %q, want the review alias's own", got.WorkerModelID)
	}
}

// TestReviewRelayFromPolicyCopiesEveryField proves ReviewRelayFromPolicy
// carries every one of RoutePolicy's routes:-mode fields into the
// returned ReviewRoute, plus the given credentials -- independently of
// PhaseRelaySpec's own field-copy tests above, since this is the
// assembly step that runs before PhaseRelaySpec ever sees the result.
func TestReviewRelayFromPolicyCopiesEveryField(t *testing.T) {
	policy := sandbox.RoutePolicy{
		WorkerModelID:                "gpt-5.6-luna",
		WorkerModelAPI:               "openai-responses",
		WorkerBasePath:               "/v2",
		WorkerModelExtraJSON:         `{"reasoning":true}`,
		UsageFormat:                  "openai-responses",
		AllowedPathPrefix:            "/responses",
		Upstream:                     "https://review.example.invalid",
		AuthMode:                     "chatgpt-codex",
		UpstreamAuthHeader:           "Authorization",
		Route:                        "review-route",
		Billing:                      "subscription",
		AllowPlaintextUpstream:       true,
		AllowUnauthenticatedUpstream: true,
		InputMicroUSDPerMTok:         11000000,
		CachedInputMicroUSDPerMTok:   1100000,
		CacheWriteMicroUSDPerMTok:    13750000,
		OutputMicroUSDPerMTok:        22000000,
	}
	apiKey := sandbox.NewRouteSecret("api-key-secret")
	githubToken := sandbox.NewRouteSecret("github-token-secret")
	chatGPTToken := sandbox.NewRouteSecret("chatgpt-token-secret")
	chatGPTAccountID := sandbox.NewRouteSecret("chatgpt-account-id")

	got := ReviewRelayFromPolicy(policy, apiKey, githubToken, chatGPTToken, chatGPTAccountID)

	want := &ReviewRoute{
		WorkerModelID:                policy.WorkerModelID,
		WorkerModelAPI:               policy.WorkerModelAPI,
		WorkerBasePath:               policy.WorkerBasePath,
		WorkerModelExtraJSON:         policy.WorkerModelExtraJSON,
		UsageFormat:                  policy.UsageFormat,
		AllowedPathPrefix:            policy.AllowedPathPrefix,
		Upstream:                     policy.Upstream,
		AuthMode:                     policy.AuthMode,
		UpstreamAuthHeader:           policy.UpstreamAuthHeader,
		Route:                        policy.Route,
		Billing:                      policy.Billing,
		APIKey:                       apiKey,
		GitHubToken:                  githubToken,
		ChatGPTToken:                 chatGPTToken,
		ChatGPTAccountID:             chatGPTAccountID,
		AllowPlaintextUpstream:       policy.AllowPlaintextUpstream,
		AllowUnauthenticatedUpstream: policy.AllowUnauthenticatedUpstream,
		InputMicroUSDPerMTok:         policy.InputMicroUSDPerMTok,
		CachedInputMicroUSDPerMTok:   policy.CachedInputMicroUSDPerMTok,
		CacheWriteMicroUSDPerMTok:    policy.CacheWriteMicroUSDPerMTok,
		OutputMicroUSDPerMTok:        policy.OutputMicroUSDPerMTok,
	}
	if *got != *want {
		t.Errorf("ReviewRelayFromPolicy() = %+v, want %+v", *got, *want)
	}
}

func TestPhaseRelaySpecFloorsAtOneWhenCeilingFullyConsumed(t *testing.T) {
	base := relaySpecFixture()
	got := PhaseRelaySpec(base, nil, 10000, 10000) // far more than the 5000 ceiling

	if got.TokenCeiling != 1 || got.TokenBudget != 1 {
		t.Errorf("TokenCeiling/TokenBudget = %d/%d, want 1/1", got.TokenCeiling, got.TokenBudget)
	}
	if got.CostCeilingMicroUSD != 1 || got.CostBudgetMicroUSD != 1 {
		t.Errorf("CostCeilingMicroUSD/CostBudgetMicroUSD = %d/%d, want 1/1", got.CostCeilingMicroUSD, got.CostBudgetMicroUSD)
	}
	if err := got.Validate(); err != nil {
		t.Errorf("Validate() = %v, want nil (a minimal but still valid spec)", err)
	}
}

func TestArgsPassesHarnessWhenSet(t *testing.T) {
	got := Args("/path/conformity_review.py", "/ws", "/ws/criteria.md", "required", "", "", "pifork")
	if n := len(got); n < 2 || got[n-2] != "--harness" || got[n-1] != "pifork" {
		t.Fatalf("Args() = %v, want it to end with --harness pifork", got)
	}
}
