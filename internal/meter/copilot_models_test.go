package meter

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// newCopilotTokenServer stands in for api.github.com's
// /copilot_internal/v2/token endpoint: it hands out a fresh token good for
// validForSeconds, counting how many times it was actually hit, and
// recording the last request's headers for assertions.
func newCopilotTokenServer(t *testing.T, validFor time.Duration) (server *httptest.Server, hits *atomic.Int32, lastAuth *atomic.Value) {
	t.Helper()
	hits = &atomic.Int32{}
	lastAuth = &atomic.Value{}
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		lastAuth.Store(r.Header.Get("Authorization"))
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token":      fmt.Sprintf("copilot-token-%d", hits.Load()),
			"expires_at": time.Now().Add(validFor).Unix(),
		})
	}))
	t.Cleanup(server.Close)
	return server, hits, lastAuth
}

// TestListGitHubCopilotModelsParsesEntitledModels confirms the models
// listing exchanges the GitHub OAuth token first, sends the same
// identifying headers a forwarded chat request gets, and parses the
// documented response shape (capabilities.limits, billing) into
// CopilotModel.
// TestCopilotModelsURLDefaultsAndHonoursExplicitUpstream is the
// regression test from a round-2 review for the models-URL host
// selection: an empty upstream (Individual-plan,
// the historic behavior) resolves to CopilotAPIBase, while an explicit
// one (a Business/Enterprise Copilot host) is honoured verbatim -- the
// listing must come from the SAME host a real forwarded request would
// use (see applyGitHubCopilotRelayDefaults), not always the individual
// default.
func TestCopilotModelsURLDefaultsAndHonoursExplicitUpstream(t *testing.T) {
	if got, want := copilotModelsURL(""), CopilotAPIBase+"/models"; got != want {
		t.Errorf("copilotModelsURL(\"\") = %q, want %q", got, want)
	}
	if got, want := copilotModelsURL("https://api.business.githubcopilot.com"), "https://api.business.githubcopilot.com/models"; got != want {
		t.Errorf("copilotModelsURL(business) = %q, want %q", got, want)
	}
	// A trailing slash on the configured upstream must not produce a
	// double slash before /models.
	if got, want := copilotModelsURL("https://api.business.githubcopilot.com/"), "https://api.business.githubcopilot.com/models"; got != want {
		t.Errorf("copilotModelsURL(business, trailing slash) = %q, want %q", got, want)
	}
}

func TestListGitHubCopilotModelsParsesEntitledModels(t *testing.T) {
	tokenServer, hits, lastAuth := newCopilotTokenServer(t, time.Hour)

	var gotAuth, gotIntegration string
	modelsServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotIntegration = r.Header.Get("Copilot-Integration-Id")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"data":[
			{"id":"gpt-5.6-luna","name":"GPT-5.6 Luna","vendor":"openai","preview":false,"model_picker_enabled":true,
			 "supported_endpoints":["/responses","ws:/responses"],
			 "capabilities":{"limits":{"max_context_window_tokens":272000,"max_prompt_tokens":200000,"max_output_tokens":32000}},
			 "billing":{"is_premium":true,"multiplier":1}},
			{"id":"gpt-4.1","name":"GPT-4.1","vendor":"openai","preview":true,"model_picker_enabled":false,
			 "capabilities":{"limits":{"max_context_window_tokens":128000}},
			 "billing":{"is_premium":false,"multiplier":0}},
			{"id":""}
		]}`)
	}))
	defer modelsServer.Close()

	models, err := listGitHubCopilotModels(t.Context(), http.DefaultClient, tokenServer.URL, modelsServer.URL, "gho_test_token")
	if err != nil {
		t.Fatalf("listGitHubCopilotModels: %v", err)
	}
	if hits.Load() != 1 {
		t.Fatalf("token exchange hits = %d, want 1", hits.Load())
	}
	if got := lastAuth.Load().(string); got != "Bearer gho_test_token" {
		t.Fatalf("token exchange Authorization = %q, want Bearer gho_test_token", got)
	}
	if gotAuth != "Bearer copilot-token-1" {
		t.Fatalf("models request Authorization = %q, want the exchanged Copilot token", gotAuth)
	}
	if gotIntegration != CopilotIntegrationID {
		t.Fatalf("models request Copilot-Integration-Id = %q, want %q", gotIntegration, CopilotIntegrationID)
	}
	// The id:"" entry is dropped, not returned as a blank model.
	if len(models) != 2 {
		t.Fatalf("models = %+v, want exactly 2 entries", models)
	}
	// ContextWindow prefers max_prompt_tokens (200000, Copilot's own real
	// per-request admission limit) over max_context_window_tokens
	// (272000) -- a round-2 review found: configuring the larger figure
	// understates how much of the model's own context a real request can
	// actually use before Copilot itself rejects it.
	want := CopilotModel{
		ID: "gpt-5.6-luna", Name: "GPT-5.6 Luna", Vendor: "openai",
		Preview: false, ModelPickerEnabled: true,
		ContextWindow: 200000, MaxPromptTokens: 200000, MaxOutputTokens: 32000,
		IsPremium: true, Multiplier: 1,
		SupportedEndpoints: []string{"/responses", "ws:/responses"},
	}
	if !reflect.DeepEqual(models[0], want) {
		t.Fatalf("models[0] = %+v, want %+v", models[0], want)
	}
	if models[1].ID != "gpt-4.1" || !models[1].Preview || models[1].ModelPickerEnabled {
		t.Fatalf("models[1] = %+v, want gpt-4.1 marked preview, not model-picker-enabled", models[1])
	}
	// gpt-4.1's fixture has no max_prompt_tokens at all -- ContextWindow
	// must fall back to max_context_window_tokens (128000), not silently
	// end up 0.
	if models[1].ContextWindow != 128000 {
		t.Errorf("models[1].ContextWindow = %d, want 128000 (fallback to max_context_window_tokens)", models[1].ContextWindow)
	}
	if models[1].MaxPromptTokens != 0 {
		t.Errorf("models[1].MaxPromptTokens = %d, want 0 (not reported)", models[1].MaxPromptTokens)
	}
}

// TestListGitHubCopilotModelsNeverLeaksTokenOnError confirms a failed
// models request never echoes the upstream's response body (which could
// reflect back the exchanged Copilot token or other request metadata) --
// the same discipline copilotCredentialSource.exchange already follows
// for the token-exchange call itself. The 401 body below deliberately
// contains what look like both the GitHub OAuth token and the exchanged
// Copilot token, standing in for an upstream error response that echoes
// request details back to the caller.
func TestListGitHubCopilotModelsNeverLeaksTokenOnError(t *testing.T) {
	tokenServer, _, _ := newCopilotTokenServer(t, time.Hour)
	const secretGithubToken = "gho_super_secret_token"
	const leakedBody = `{"error":"unauthorized","authorization":"Bearer copilot-token-1","token":"` + secretGithubToken + `"}`
	modelsServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, leakedBody)
	}))
	defer modelsServer.Close()

	_, err := listGitHubCopilotModels(t.Context(), http.DefaultClient, tokenServer.URL, modelsServer.URL, secretGithubToken)
	if err == nil {
		t.Fatal("listGitHubCopilotModels with a 401 response = nil error, want one naming the failure")
	}
	if strings.Contains(err.Error(), secretGithubToken) {
		t.Fatalf("error %q leaks the GitHub OAuth token", err.Error())
	}
	if strings.Contains(err.Error(), "copilot-token-1") {
		t.Fatalf("error %q leaks the exchanged Copilot token", err.Error())
	}
	if strings.Contains(err.Error(), leakedBody) {
		t.Fatalf("error %q echoes the raw upstream response body", err.Error())
	}
}

// TestListGitHubCopilotModelsNeverFollowsARedirect is a regression test:
// the exchanged Copilot API token travels in the models request's own
// Authorization header (see listGitHubCopilotModels), and
// Go's default http.Client forwards that header when it follows a
// same-scheme redirect -- so a models endpoint (compromised, misconfigured,
// or simply redirecting https to http) that answers with a 301 to a
// second host must never have that redirect followed at all. The client
// built here mirrors ListGitHubCopilotModels' own
// CheckRedirect: NoRedirectCheckRedirect exactly (this lower-level
// listGitHubCopilotModels takes any *http.Client the caller supplies, so
// the exported wrapper's own real client can't be pointed at httptest
// servers).
func TestListGitHubCopilotModelsNeverFollowsARedirect(t *testing.T) {
	tokenServer, _, _ := newCopilotTokenServer(t, time.Hour)

	var leakServerHit bool
	leakServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		leakServerHit = true
		if auth := r.Header.Get("Authorization"); auth != "" {
			t.Errorf("leak server received Authorization header %q -- the redirect was followed", auth)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"data":[]}`)
	}))
	defer leakServer.Close()

	modelsServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, leakServer.URL+"/models", http.StatusMovedPermanently)
	}))
	defer modelsServer.Close()

	client := &http.Client{CheckRedirect: NoRedirectCheckRedirect}
	_, err := listGitHubCopilotModels(t.Context(), client, tokenServer.URL, modelsServer.URL, "gho_test_token")
	if err == nil {
		t.Fatal("listGitHubCopilotModels with a redirecting models endpoint = nil error, want one naming the unfollowed 301")
	}
	if leakServerHit {
		t.Fatal("the redirect target was reached -- the exchanged Copilot token could have been forwarded to it")
	}
}

func TestCopilotModelUsable(t *testing.T) {
	for _, tc := range []struct {
		m         CopilotModel
		workerAPI string
		want      bool
	}{
		{CopilotModel{}, "", true},
		{CopilotModel{}, RequestFormatOpenAIResponses, true},
		{CopilotModel{SupportedEndpoints: []string{"/chat/completions", "/v1/messages"}, PolicyState: "enabled"}, "", true},
		{CopilotModel{SupportedEndpoints: []string{"/responses", "ws:/responses"}}, "", false},
		{CopilotModel{SupportedEndpoints: []string{"/chat/completions"}, PolicyState: "disabled"}, "", false},
		{CopilotModel{SupportedEndpoints: []string{"chat/completions"}}, "", true},
		{CopilotModel{SupportedEndpoints: []string{"/v1/chat/completions/"}}, "", true},
		// Responses-only model: unusable under the default (completions)
		// worker API, usable under openai-responses.
		{CopilotModel{SupportedEndpoints: []string{"/responses"}}, "", false},
		{CopilotModel{SupportedEndpoints: []string{"/responses"}}, RequestFormatOpenAIResponses, true},
		{CopilotModel{SupportedEndpoints: []string{"responses"}}, RequestFormatOpenAIResponses, true},
		{CopilotModel{SupportedEndpoints: []string{"/v1/responses/"}}, RequestFormatOpenAIResponses, true},
		// Chat-only model: unusable under openai-responses.
		{CopilotModel{SupportedEndpoints: []string{"/chat/completions"}}, RequestFormatOpenAIResponses, false},
		// Both endpoints: usable under either.
		{CopilotModel{SupportedEndpoints: []string{"/chat/completions", "/responses"}}, "", true},
		{CopilotModel{SupportedEndpoints: []string{"/chat/completions", "/responses"}}, RequestFormatOpenAIResponses, true},
		// Policy disabled refuses regardless of worker API.
		{CopilotModel{SupportedEndpoints: []string{"/responses"}, PolicyState: "disabled"}, RequestFormatOpenAIResponses, false},
	} {
		got, reason, fix := CopilotModelUsable(tc.m, tc.workerAPI)
		if got != tc.want || (!got && reason == "") {
			t.Errorf("%+v workerAPI=%q: got (%v, %q, %q), want %v with a reason when false", tc.m, tc.workerAPI, got, reason, fix, tc.want)
		}
	}

	if _, reason, fix := CopilotModelUsable(CopilotModel{SupportedEndpoints: []string{"/responses"}}, ""); !strings.Contains(reason, "/responses") || !strings.Contains(fix, "models.<name>.api: openai-responses") {
		t.Errorf("responses-only model refused under completions API: reason = %q, fix = %q, want reason naming /responses and fix naming models.<name>.api: openai-responses", reason, fix)
	}
	if _, reason, _ := CopilotModelUsable(CopilotModel{SupportedEndpoints: []string{"/chat/completions"}}, RequestFormatOpenAIResponses); !strings.Contains(reason, "/chat/completions") {
		t.Errorf("chat-only model refused under responses API: reason = %q, want it to name /chat/completions", reason)
	}
	// A model serving neither path this relay forwards to (e.g. Claude via
	// Copilot's own /v1/messages): no models.<name>.api setting fixes it.
	if _, _, fix := CopilotModelUsable(CopilotModel{SupportedEndpoints: []string{"/v1/messages"}}, ""); !strings.Contains(fix, "/chat/completions") || !strings.Contains(fix, "/responses") || !strings.Contains(fix, "no models.<name>.api setting") {
		t.Errorf("model serving neither path: fix = %q, want it to name both paths and say no setting fixes it", fix)
	}
	// A policy refusal has no fix, only a reason.
	if _, reason, fix := CopilotModelUsable(CopilotModel{SupportedEndpoints: []string{"/chat/completions"}, PolicyState: "disabled"}, ""); fix != "" || !strings.Contains(reason, "policy") {
		t.Errorf("policy-disabled model: reason = %q, fix = %q, want a policy reason and no fix", reason, fix)
	}
}

func TestCopilotModelUsableAnyAPI(t *testing.T) {
	for _, tc := range []struct {
		m       CopilotModel
		wantAPI string
		wantOK  bool
	}{
		{CopilotModel{}, RequestFormatOpenAICompletions, true},
		{CopilotModel{SupportedEndpoints: []string{"/chat/completions"}}, RequestFormatOpenAICompletions, true},
		{CopilotModel{SupportedEndpoints: []string{"/responses"}}, RequestFormatOpenAIResponses, true},
		{CopilotModel{SupportedEndpoints: []string{"responses"}}, RequestFormatOpenAIResponses, true},
		// Both served: prefer completions, today's default.
		{CopilotModel{SupportedEndpoints: []string{"/responses", "/chat/completions"}}, RequestFormatOpenAICompletions, true},
		{CopilotModel{SupportedEndpoints: []string{"/v1/messages"}}, "", false},
		// Policy disabled refuses regardless of endpoints.
		{CopilotModel{SupportedEndpoints: []string{"/responses"}, PolicyState: "disabled"}, "", false},
	} {
		api, ok, why := CopilotModelUsableAnyAPI(tc.m)
		if api != tc.wantAPI || ok != tc.wantOK || (!ok && why == "") {
			t.Errorf("CopilotModelUsableAnyAPI(%+v) = (%q, %v, %q), want (%q, %v, non-empty why when !ok)", tc.m, api, ok, why, tc.wantAPI, tc.wantOK)
		}
	}
}

// TestListGitHubCopilotModelsToleratesUnexpectedFieldShapes: a listing with
// policy as a bare string or supported_endpoints as objects must still list
// (those fields were ignored entirely before they were parsed).
func TestListGitHubCopilotModelsToleratesUnexpectedFieldShapes(t *testing.T) {
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"token":"copilot-token-1","expires_at":4102444800,"refresh_in":1500}`)
	}))
	defer tokenServer.Close()
	modelsServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"data":[{"id":"m1","policy":"enabled","supported_endpoints":[{"path":"/chat/completions"}]}]}`)
	}))
	defer modelsServer.Close()
	models, err := listGitHubCopilotModels(t.Context(), http.DefaultClient, tokenServer.URL, modelsServer.URL, "gho_test_token")
	if err != nil {
		t.Fatalf("listGitHubCopilotModels: %v", err)
	}
	if len(models) != 1 || models[0].ID != "m1" {
		t.Fatalf("models = %+v, want the one model", models)
	}
}
