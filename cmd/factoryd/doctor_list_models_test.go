package main

import (
	"bytes"
	"context"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"buildgate/internal/meter"
	"buildgate/internal/modelrole"
	"buildgate/internal/sessionconfig"
)

// doctorInputsForRoute builds a doctorInputs{settings: ...} whose
// roles.execution resolves through one routes:/models: (route, model)
// pair built from route and modelID -- the shape every doctorListModels
// test below needs now that routes:/models:/roles: is the only session-
// config schema and doctorListModels always resolves roles.execution via
// modelrole.SelectRoute (never reading relay* fields directly off the
// doctorInputs an operator or flag would have set them from).
func doctorInputsForRoute(route sessionconfig.Route, modelID string) doctorInputs {
	settings := sessionconfig.DefaultSettings()
	settings.Routes = map[string]sessionconfig.Route{"r": route}
	settings.Models = map[string]sessionconfig.Model{"m": {ID: modelID, Routes: []string{"r"}}}
	settings.Roles = &sessionconfig.Roles{Execution: &sessionconfig.RoleConfig{Model: "m"}}
	return doctorInputs{settings: settings}
}

// stubListGitHubCopilotModels overrides listGitHubCopilotModelsFn for the
// duration of the test, restoring the real meter.ListGitHubCopilotModels
// on cleanup -- so a doctor/quickstart test can assert on the
// model-listing behavior without ever making a real network call to
// api.individual.githubcopilot.com/api.github.com.
func stubListGitHubCopilotModels(t *testing.T, fn func(ctx context.Context, caBundlePath, githubToken, upstream string) ([]meter.CopilotModel, error)) {
	t.Helper()
	orig := listGitHubCopilotModelsFn
	listGitHubCopilotModelsFn = fn
	t.Cleanup(func() { listGitHubCopilotModelsFn = orig })
}

// TestDoctorListModelsChatGPTCodexNamesConfiguredModelNoNetworkCall proves
// -list-models for chatgpt-codex never attempts a network call (there is
// no listing endpoint -- codex exec speaks only POST /responses) and
// instead reports the configured (or default) model id plainly.
func TestDoctorListModelsChatGPTCodexNamesConfiguredModelNoNetworkCall(t *testing.T) {
	authFile := writeCodexAuthFile(t, t.TempDir(), map[string]any{
		"auth_mode": "chatgpt",
		"tokens": map[string]any{
			"access_token": fakeCodexJWT(t, time.Now().Add(48*time.Hour)),
			"account_id":   "acct-123",
		},
	})
	var out bytes.Buffer
	in := doctorInputsForRoute(sessionconfig.Route{CredentialMode: meter.CredentialModeChatGPTCodex, CodexAuthFile: authFile}, "gpt-5.6-luna")
	if err := doctorListModels(context.Background(), &out, in); err != nil {
		t.Fatalf("doctorListModels: %v", err)
	}
	if !strings.Contains(out.String(), "no model-listing endpoint") {
		t.Errorf("output = %q, want it to say there's no listing endpoint", out.String())
	}
	if !strings.Contains(out.String(), "gpt-5.6-luna") {
		t.Errorf("output = %q, want the configured model id", out.String())
	}
}

// TestDoctorListModelsChatGPTCodexDefaultsWhenUnconfigured used to prove
// an unconfigured model id reports chatGPTCodexDefaultModelID as the
// default. That branch (doctorListModels' own "configured == \"\""
// fallback) is now unreachable through any schema-valid routes:/models:
// config: sandbox.RoutePolicy.Validate itself requires a non-empty
// worker model id whenever auth mode is chatgpt-codex, and
// doctorListModels always resolves roles.execution through
// modelrole.SelectRoute (which builds and validates that same policy)
// before ever reaching its own branch -- so an empty configured model id
// can no longer reach this message live, only via a direct, synthetic
// doctorInputs{} call the way this test used to build one. Left
// unexercised rather than kept as a test of dead code.

// TestDoctorListModelsCopilotFailsClosedWithNoTokenSource proves
// -list-models fails when roles.execution's own github-copilot route has
// no GitHub OAuth token source configured, rather than attempting a real
// network call with an empty token: modelrole.SelectRoute's own
// credential probe (resolveRouteCredentials) skips the route before
// doctorListModels ever dispatches to its github-copilot branch, so the
// route is reported unusable (modelrole.ErrNoRouteAvailable), not the
// resolver's own error text (SelectRoute never echoes that -- see
// skipReasonCredentialUnavailable's own doc comment).
func TestDoctorListModelsCopilotFailsClosedWithNoTokenSource(t *testing.T) {
	t.Setenv("GITHUB_COPILOT_TOKEN", "")
	t.Setenv("HOME", t.TempDir())
	var out bytes.Buffer
	in := doctorInputsForRoute(sessionconfig.Route{CredentialMode: meter.CredentialModeGitHubCopilot}, "gpt-5.6-luna")
	err := doctorListModels(context.Background(), &out, in)
	if !errors.Is(err, modelrole.ErrNoRouteAvailable) {
		t.Fatalf("doctorListModels with no token source: err = %v, want modelrole.ErrNoRouteAvailable", err)
	}
}

// TestDoctorListModelsCopilotPrintsListingWithTagsAndAdvisory proves the
// github-copilot branch prints each model's id, context window, and
// preview/picker-default/premium tags, plus the weak-model advisory
// warning when a listed id is on quickstartWeakModelAdvisories.
func TestDoctorListModelsCopilotPrintsListingWithTagsAndAdvisory(t *testing.T) {
	t.Setenv("GITHUB_COPILOT_TOKEN", "gho_test_token")
	t.Setenv("HOME", t.TempDir())
	stubListGitHubCopilotModels(t, func(ctx context.Context, caBundlePath, githubToken, upstream string) ([]meter.CopilotModel, error) {
		if githubToken != "gho_test_token" {
			t.Errorf("githubToken = %q, want the resolved GITHUB_COPILOT_TOKEN", githubToken)
		}
		return []meter.CopilotModel{
			{ID: "gpt-5.6-luna", ContextWindow: 272000, ModelPickerEnabled: true, IsPremium: true, Multiplier: 1},
			{ID: "gpt-4.1", Preview: true},
		}, nil
	})

	var out bytes.Buffer
	in := doctorInputsForRoute(sessionconfig.Route{CredentialMode: meter.CredentialModeGitHubCopilot}, "gpt-5.6-luna")
	if err := doctorListModels(context.Background(), &out, in); err != nil {
		t.Fatalf("doctorListModels: %v", err)
	}
	got := out.String()
	for _, want := range []string{"gpt-5.6-luna", "context window 272000", "picker default", "premium x1", "gpt-4.1", "preview", "too weak for the build loop"} {
		if !strings.Contains(got, want) {
			t.Errorf("output = %q, want it to contain %q", got, want)
		}
	}
}

// TestDoctorListModelsCopilotAcceptsResponsesOnlyModelAndLabelsEndpoints
// is the regression test for the Responses-route fix: `-list-models`
// must honour the configured relay_worker_api (a /responses-only model
// is usable, not tagged "not usable"), label each model's own endpoints
// with their own "endpoints: ..." tag, and never repeat that same list
// inside a "not usable" reason.
func TestDoctorListModelsCopilotAcceptsResponsesOnlyModelAndLabelsEndpoints(t *testing.T) {
	t.Setenv("GITHUB_COPILOT_TOKEN", "gho_test_token")
	t.Setenv("HOME", t.TempDir())
	stubListGitHubCopilotModels(t, func(ctx context.Context, caBundlePath, githubToken, upstream string) ([]meter.CopilotModel, error) {
		return []meter.CopilotModel{
			{ID: "gpt-5.6-luna", ContextWindow: 272000, SupportedEndpoints: []string{"/responses"}},
			{ID: "gpt-4.1", ContextWindow: 128000, SupportedEndpoints: []string{"/chat/completions"}},
		}, nil
	})

	var out bytes.Buffer
	settings := sessionconfig.DefaultSettings()
	settings.Routes = map[string]sessionconfig.Route{"r": {CredentialMode: meter.CredentialModeGitHubCopilot}}
	settings.Models = map[string]sessionconfig.Model{"m": {ID: "gpt-5.6-luna", API: meter.RequestFormatOpenAIResponses, Routes: []string{"r"}}}
	settings.Roles = &sessionconfig.Roles{Execution: &sessionconfig.RoleConfig{Model: "m"}}
	in := doctorInputs{settings: settings}
	if err := doctorListModels(context.Background(), &out, in); err != nil {
		t.Fatalf("doctorListModels: %v", err)
	}
	got := out.String()
	luna := ""
	gpt41 := ""
	for _, line := range strings.Split(got, "\n") {
		switch {
		case strings.HasPrefix(line, "  gpt-5.6-luna"):
			luna = line
		case strings.HasPrefix(line, "  gpt-4.1"):
			gpt41 = line
		}
	}
	if !strings.Contains(luna, "[endpoints: /responses]") || strings.Contains(luna, "not usable") {
		t.Errorf("gpt-5.6-luna line = %q, want it endpoint-labeled and usable under relay_worker_api openai-responses", luna)
	}
	if !strings.Contains(gpt41, "[endpoints: /chat/completions]") || !strings.Contains(gpt41, "not usable") {
		t.Errorf("gpt-4.1 line = %q, want it endpoint-labeled and not usable under relay_worker_api openai-responses", gpt41)
	}
	if strings.Count(gpt41, "/chat/completions") != 1 {
		t.Errorf("gpt-4.1 line = %q, want /chat/completions named once (its own endpoints tag only, not repeated in the not-usable reason)", gpt41)
	}
}

// TestDoctorListModelsOpenAIListsFromUpstream proves the default
// (static/openai) branch builds <upstream><base-path>/models and prints
// each id, against an httptest server standing in for the upstream --
// never a real model host.
func TestDoctorListModelsOpenAIListsFromUpstream(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	var gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"model-a"},{"id":"model-b"}]}`))
	}))
	defer server.Close()

	var out bytes.Buffer
	// relayAllowPlaintextUpstream: true -- an httptest server is a
	// private (127.0.0.1) http:// endpoint, exactly the case
	// sandbox.RoutePolicy.ValidateUpstreamScheme (a round-3 review's
	// plaintext-upstream rule) requires this set for; no credential is
	// configured here, so CredentialSafeForUpstream's own coexistence
	// rule never fires.
	basePath := "/v1"
	in := doctorInputsForRoute(sessionconfig.Route{Upstream: server.URL, WorkerBasePath: &basePath, AllowPlaintextUpstream: true, AllowNoCredential: true}, "model-a")
	if err := doctorListModels(context.Background(), &out, in); err != nil {
		t.Fatalf("doctorListModels: %v", err)
	}
	if gotPath != "/v1/models" {
		t.Errorf("request path = %q, want /v1/models", gotPath)
	}
	for _, want := range []string{"model-a", "model-b"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output = %q, want it to list %q", out.String(), want)
		}
	}
}

// TestDoctorListModelsOpenAISendsConfiguredCredentialHeader is the
// regression test for a round-2 review's configured-credential-header
// requirement: when ANTHROPIC_API_KEY is set,
// the listing request must carry it under the configured credential
// header (relay_credential_header, defaulting to X-Api-Key), the same
// header a real forwarded request would use -- not an unauthenticated
// bare probe that could see a different (or no) catalog than a real run
// does.
func TestDoctorListModelsOpenAISendsConfiguredCredentialHeader(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-test-key")
	var gotHeader string
	// A real credential requires https:// -- sandbox.RoutePolicy.
	// ValidateUpstreamScheme (the same round-3 review plaintext-upstream
	// rule) refuses to send one over plaintext even to a private host,
	// matching what a real relay launch would refuse too. An httptest
	// TLS server's self-signed cert is trusted via
	// meter.OutboundTransport's own -egress-ca-bundle support, the same
	// mechanism a real corporate-CA operator uses.
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeader = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"model-a"}]}`))
	}))
	defer server.Close()
	caBundle := writeTestServerCABundle(t, server)

	var out bytes.Buffer
	basePath := "/v1"
	in := doctorInputsForRoute(sessionconfig.Route{Upstream: server.URL, WorkerBasePath: &basePath, CredentialHeader: "Authorization"}, "model-a")
	in.egressCABundle = caBundle
	if err := doctorListModels(context.Background(), &out, in); err != nil {
		t.Fatalf("doctorListModels: %v", err)
	}
	if gotHeader != "Bearer sk-test-key" {
		t.Errorf("Authorization header = %q, want Bearer sk-test-key", gotHeader)
	}
}

// TestDoctorListModelsUsesSelectedRouteCredentialEnv is the regression
// test for a real finding: doctorListModels' own static-mode branch read
// a hard-coded ANTHROPIC_API_KEY regardless of the resolved route's own
// credential_env, so a route naming a different variable (e.g. a
// non-Anthropic OpenAI-compatible endpoint keyed on its own API key) had
// that other key silently ignored and, worse, could leak whatever
// ANTHROPIC_API_KEY happened to be set to in the operator's shell to an
// unrelated host. ANTHROPIC_API_KEY is deliberately set to a DIFFERENT
// value here than the route's own OTHER_KEY, so the assertion fails if
// the Anthropic one is ever the one actually sent.
func TestDoctorListModelsUsesSelectedRouteCredentialEnv(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-wrong-anthropic-key")
	t.Setenv("OTHER_KEY", "sk-other-key")
	var gotHeader string
	// https:// (not a plaintext + AllowPlaintextUpstream combination):
	// resolveRouteCredentials' own CredentialSafeForUpstream check
	// refuses to resolve a real credential over a plaintext upstream even
	// with AllowPlaintextUpstream set, an unrelated concern this test
	// doesn't mean to exercise (see TestDoctorListModelsOpenAINeverSendsCredentialWhenAllowNoCredentialSet's
	// own doc comment).
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeader = r.Header.Get("X-Api-Key")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"model-a"}]}`))
	}))
	defer server.Close()
	caBundle := writeTestServerCABundle(t, server)

	var out bytes.Buffer
	basePath := "/v1"
	in := doctorInputsForRoute(sessionconfig.Route{Upstream: server.URL, WorkerBasePath: &basePath, CredentialEnv: "OTHER_KEY"}, "model-a")
	in.egressCABundle = caBundle
	if err := doctorListModels(context.Background(), &out, in); err != nil {
		t.Fatalf("doctorListModels: %v", err)
	}
	if gotHeader != "sk-other-key" {
		t.Errorf("X-Api-Key header = %q, want the route's own OTHER_KEY value %q, never ANTHROPIC_API_KEY", gotHeader, "sk-other-key")
	}
}

// TestDoctorListModelsOpenAIRefusesCredentialOverPlaintext is the same
// round-3 review's regression test for the plaintext-upstream rule: an
// http:// upstream (with no allow_plaintext_upstream opt-in) with
// ANTHROPIC_API_KEY set must never have that key sent -- the request
// must not even be attempted. modelrole.SelectRoute's own
// ValidateUpstreamScheme call refuses this route before doctorListModels
// ever dispatches to its own branch, so it comes back as
// modelrole.ErrNoRouteAvailable, not a printed "Not listing models"
// message.
func TestDoctorListModelsOpenAIRefusesCredentialOverPlaintext(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-test-key")
	requestReached := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestReached = true
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"model-a"}]}`))
	}))
	defer server.Close()

	var out bytes.Buffer
	basePath := "/v1"
	in := doctorInputsForRoute(sessionconfig.Route{Upstream: server.URL, WorkerBasePath: &basePath}, "model-a")
	if err := doctorListModels(context.Background(), &out, in); !errors.Is(err, modelrole.ErrNoRouteAvailable) {
		t.Fatalf("doctorListModels = %v, want modelrole.ErrNoRouteAvailable (a plaintext upstream with no allow_plaintext_upstream opt-in is unusable)", err)
	}
	if requestReached {
		t.Fatal("the listing request reached the upstream despite a plaintext URL and a real credential -- the key was sent (or could have been) in cleartext")
	}
}

// TestDoctorListModelsOpenAIRefusesCredentialOverPlaintextEvenWhenAllowed
// proves that AllowPlaintextUpstream's own private-host exemption does
// NOT extend to a route that also carries a real credential -- matching
// sandbox.RouteSpec.Validate's own stricter rule (found via review, Codex,
// PR #59): the exemption exists for a credential-FREE local endpoint, not
// a credentialed one that merely happens to be private. This is now
// modelrole.SelectRoute's own sandbox.CredentialSafeForUpstream check
// (roles.execution resolves before doctorListModels' own branch ever
// runs), so the route is skipped as unusable rather than reaching
// doctorListModels' own later "Not listing models" diagnostic -- either
// way, the listing request must never reach the upstream.
func TestDoctorListModelsOpenAIRefusesCredentialOverPlaintextEvenWhenAllowed(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-test-key")
	requestReached := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestReached = true
	}))
	defer server.Close()

	var out bytes.Buffer
	basePath := "/v1"
	in := doctorInputsForRoute(sessionconfig.Route{Upstream: server.URL, WorkerBasePath: &basePath, AllowPlaintextUpstream: true}, "model-a")
	if err := doctorListModels(context.Background(), &out, in); !errors.Is(err, modelrole.ErrNoRouteAvailable) {
		t.Fatalf("doctorListModels = %v, want modelrole.ErrNoRouteAvailable (a real credential over a plaintext upstream makes the route unusable)", err)
	}
	if requestReached {
		t.Fatal("the listing request reached the upstream despite carrying a real credential over an AllowPlaintextUpstream-exempted (but still plaintext) endpoint")
	}
}

// TestDoctorListModelsOpenAINeverSendsCredentialWhenAllowNoCredentialSet
// is the regression test for another round-3 review finding:
// relay_allow_no_credential means a real run sends no credential at all
// (quickstart's own -route openai "no credential" path even scrubs
// ANTHROPIC_API_KEY from the daemon's environment); the listing must
// match, even though the operator's own shell still has
// ANTHROPIC_API_KEY set.
func TestDoctorListModelsOpenAINeverSendsCredentialWhenAllowNoCredentialSet(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-test-key")
	var gotHeader string
	sawHeader := false
	// An https:// server, not a plaintext one with AllowPlaintextUpstream:
	// resolveRouteCredentials' own CredentialSafeForUpstream check now
	// refuses to resolve a real credential (ANTHROPIC_API_KEY is set) for
	// any route that also sets AllowPlaintextUpstream, regardless of
	// AllowNoCredential -- a real credential must never coexist with a
	// plaintext upstream. Using https:// here isolates the concern this
	// test actually covers (AllowNoCredential scrubbing the credential
	// from the request despite a real one being available) from that
	// unrelated, already-covered plaintext-vs-credential refusal.
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeader = r.Header.Get("X-Api-Key")
		sawHeader = gotHeader != ""
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"model-a"}]}`))
	}))
	defer server.Close()
	caBundle := writeTestServerCABundle(t, server)

	var out bytes.Buffer
	basePath := "/v1"
	in := doctorInputsForRoute(sessionconfig.Route{Upstream: server.URL, WorkerBasePath: &basePath, AllowNoCredential: true}, "model-a")
	in.egressCABundle = caBundle
	if err := doctorListModels(context.Background(), &out, in); err != nil {
		t.Fatalf("doctorListModels: %v", err)
	}
	if sawHeader {
		t.Errorf("X-Api-Key header = %q, want none sent when relay_allow_no_credential is set", gotHeader)
	}
}

// writeTestServerCABundle PEM-encodes server's own TLS certificate to a
// temp file, standing in for a real -egress-ca-bundle -- so a test that
// needs a real, validation-passing https:// upstream can trust an
// httptest.NewTLSServer's self-signed certificate the same way a real
// operator's corporate-CA bundle gets trusted (meter.OutboundTransport).
func writeTestServerCABundle(t *testing.T, server *httptest.Server) string {
	t.Helper()
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	path := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(path, pemBytes, 0o600); err != nil {
		t.Fatalf("write test CA bundle: %v", err)
	}
	return path
}

// TestDoctorListModelsRedactsUserinfoInOutput used to prove a
// -relay-upstream with embedded userinfo (https://user:pass@host/..)
// never appeared with those credentials intact in what's printed. That
// scenario can no longer be built through routes:/models: config:
// sandbox.RoutePolicy's own validRelayUpstream rejects a Route.Upstream
// carrying userinfo outright (`u.User != nil`), so modelrole.SelectRoute
// would skip such a route as unusable before doctorListModels' own
// request logic ever ran. redactUserinfo itself is still directly
// covered by TestRedactUserinfoStripsCredentials below.
func TestRedactUserinfoStripsCredentials(t *testing.T) {
	got := redactUserinfo("https://user:pass@example.com/v1/models")
	if strings.Contains(got, "user") || strings.Contains(got, "pass") {
		t.Errorf("redactUserinfo = %q, want userinfo stripped", got)
	}
	if got != "https://example.com/v1/models" {
		t.Errorf("redactUserinfo = %q, want https://example.com/v1/models", got)
	}
	// A URL with no userinfo passes through unchanged.
	if got := redactUserinfo("https://example.com/v1/models"); got != "https://example.com/v1/models" {
		t.Errorf("redactUserinfo (no userinfo) = %q, want unchanged", got)
	}
}

// TestDoctorListModelsExplicitAnthropicUpstreamAlsoSkipsListing proves
// doctorListModels reports "listing not supported" for a route whose
// resolved upstream is Anthropic's own host, rather than building a
// bogus "/models" URL and failing with a generic missing-upstream error,
// and never makes a network call doing so. Unlike the legacy default
// (-relay-upstream's own bare-default-to-Anthropic case, since removed
// alongside routes:/models: becoming the only session-config schema), a
// static route's own Upstream must be explicit -- an empty one is simply
// unusable (sandbox.RoutePolicy.Validate's own required-upstream check),
// never a silent fallback to Anthropic.
func TestDoctorListModelsExplicitAnthropicUpstreamAlsoSkipsListing(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-test-key")
	var out bytes.Buffer
	in := doctorInputsForRoute(sessionconfig.Route{Upstream: "https://api.anthropic.com"}, "model-a")
	if err := doctorListModels(context.Background(), &out, in); err != nil {
		t.Fatalf("doctorListModels: %v", err)
	}
	if !strings.Contains(out.String(), "listing not supported for this route") {
		t.Errorf("output = %q, want it to say listing isn't supported for anthropic", out.String())
	}
}

// TestDoctorCheckCopilotModelListedPassesWhenModelPresent and
// TestDoctorCheckCopilotModelListedFailsWithFixNamingRealIDs are the
// regression tests proving that the onboarding walk found `doctor` passed
// with relay_worker_model_id: placeholder-model under github-copilot
// mode, because the only copilot check that existed
// (doctorCheckCopilotTokenExchange) proves the token is valid, never that
// the configured id is one this account actually serves.
func TestDoctorCheckCopilotModelListedPassesWhenModelPresent(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	stubListGitHubCopilotModels(t, func(ctx context.Context, caBundlePath, githubToken, upstream string) ([]meter.CopilotModel, error) {
		return []meter.CopilotModel{{ID: "gpt-5.6-luna", ContextWindow: 200000}, {ID: "gpt-4.1", ContextWindow: 128000}}, nil
	})
	tokenFile := newCopilotAuthFixturePath(t, "gho_test_token")

	check := doctorCheckCopilotModelListed(context.Background(), tokenFile, "", "", "gpt-5.6-luna", "", "", false, doctorRoutesModeRouteKeys("route", "model"))
	if check.Err != nil {
		t.Fatalf("doctorCheckCopilotModelListed: Err = %v, want nil (the model is in the listing)", check.Err)
	}
}

func TestDoctorCheckCopilotModelListedFailsWithFixNamingRealIDs(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	stubListGitHubCopilotModels(t, func(ctx context.Context, caBundlePath, githubToken, upstream string) ([]meter.CopilotModel, error) {
		return []meter.CopilotModel{{ID: "gpt-5.6-luna", ContextWindow: 200000}, {ID: "gpt-4.1", ContextWindow: 128000}}, nil
	})
	tokenFile := newCopilotAuthFixturePath(t, "gho_test_token")

	check := doctorCheckCopilotModelListed(context.Background(), tokenFile, "", "", "placeholder-model", "", "", false, doctorRoutesModeRouteKeys("route", "model"))
	if check.Err == nil {
		t.Fatal("doctorCheckCopilotModelListed with an unentitled id: Err = nil, want a failure")
	}
	if !strings.Contains(check.Err.Error(), "placeholder-model") {
		t.Errorf("Err = %v, want it to name the bad id", check.Err)
	}
	for _, wantID := range []string{"gpt-5.6-luna", "gpt-4.1"} {
		if !strings.Contains(check.Fix, wantID) {
			t.Errorf("Fix = %q, want it to name real entitled id %q", check.Fix, wantID)
		}
	}
}

// TestDoctorCheckCopilotModelListedFailsClosedOnListingError proves a
// listing failure (e.g. the token exchange itself failing) surfaces as a
// doctorCheck failure naming that error, not a silent pass.
func TestDoctorCheckCopilotModelListedFailsClosedOnListingError(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	stubListGitHubCopilotModels(t, func(ctx context.Context, caBundlePath, githubToken, upstream string) ([]meter.CopilotModel, error) {
		return nil, errors.New("token exchange: upstream returned status 401")
	})
	tokenFile := newCopilotAuthFixturePath(t, "gho_test_token")

	check := doctorCheckCopilotModelListed(context.Background(), tokenFile, "", "", "gpt-5.6-luna", "", "", false, doctorRoutesModeRouteKeys("route", "model"))
	if check.Err == nil || !strings.Contains(check.Err.Error(), "401") {
		t.Fatalf("doctorCheckCopilotModelListed with a listing error: Err = %v, want it to surface the listing failure", check.Err)
	}
}

// TestDoctorCheckCopilotModelListedForwardsConfiguredUpstream is the
// regression test for a round-2 review finding: the listing must be
// fetched from the SAME host a real request would use, not always the
// hardcoded Individual-plan default -- a Business/Enterprise account
// configured with relay_upstream pointing at a different Copilot API
// host must have its listing derived from that same host.
func TestDoctorCheckCopilotModelListedForwardsConfiguredUpstream(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	const businessUpstream = "https://api.business.githubcopilot.com"
	var gotUpstream string
	stubListGitHubCopilotModels(t, func(ctx context.Context, caBundlePath, githubToken, upstream string) ([]meter.CopilotModel, error) {
		gotUpstream = upstream
		return []meter.CopilotModel{{ID: "gpt-5.6-luna"}}, nil
	})
	tokenFile := newCopilotAuthFixturePath(t, "gho_test_token")

	doctorCheckCopilotModelListed(context.Background(), tokenFile, "", "", "gpt-5.6-luna", businessUpstream, "", false, doctorRoutesModeRouteKeys("route", "model"))
	if gotUpstream != businessUpstream {
		t.Errorf("upstream forwarded to the listing = %q, want %q", gotUpstream, businessUpstream)
	}
}

// TestDoctorCheckCopilotModelListedDowngradesToAdvisoryOnNonDefaultUpstream
// proves a "not entitled" verdict on an explicitly-configured, non-default
// relay_upstream (a Business/Enterprise host this relay has never live-
// verified the /models shape of) is a WARN (Advisory), not a hard FAIL --
// the same round-2 review finding's documented fallback for when the
// derived URL can't be trusted as authoritative.
func TestDoctorCheckCopilotModelListedDowngradesToAdvisoryOnNonDefaultUpstream(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	stubListGitHubCopilotModels(t, func(ctx context.Context, caBundlePath, githubToken, upstream string) ([]meter.CopilotModel, error) {
		return []meter.CopilotModel{{ID: "gpt-5.6-luna"}}, nil
	})
	tokenFile := newCopilotAuthFixturePath(t, "gho_test_token")

	check := doctorCheckCopilotModelListed(context.Background(), tokenFile, "", "", "placeholder-model", "https://api.business.githubcopilot.com", "", false, doctorRoutesModeRouteKeys("route", "model"))
	if check.Err == nil {
		t.Fatal("check.Err = nil, want a failure naming the unentitled id")
	}
	if !check.Advisory {
		t.Error("check.Advisory = false, want true (a non-default relay_upstream downgrades this to a WARN)")
	}
}

// TestDoctorCheckCopilotModelListedStaysHardFailOnDefaultUpstream proves
// the default (empty/individual-plan) upstream keeps the original hard
// FAIL behavior -- only an explicitly non-default upstream downgrades.
func TestDoctorCheckCopilotModelListedStaysHardFailOnDefaultUpstream(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	stubListGitHubCopilotModels(t, func(ctx context.Context, caBundlePath, githubToken, upstream string) ([]meter.CopilotModel, error) {
		return []meter.CopilotModel{{ID: "gpt-5.6-luna"}}, nil
	})
	tokenFile := newCopilotAuthFixturePath(t, "gho_test_token")

	check := doctorCheckCopilotModelListed(context.Background(), tokenFile, "", "", "placeholder-model", "", "", false, doctorRoutesModeRouteKeys("route", "model"))
	if check.Advisory {
		t.Error("check.Advisory = true, want false for the default upstream (a hard FAIL)")
	}
}

// TestDoctorCheckCopilotModelListedRefusesPlaintextUpstreamBeforeExchange
// is the regression test for another round-3 review finding: a stale
// relay_upstream (e.g. left over from a local-model route after
// switching to github-copilot) must never receive the exchanged Copilot
// API token in cleartext -- the check must FAIL, naming the reason, and
// must never even call listGitHubCopilotModelsFn (which itself performs
// the real GitHub token exchange), since the plaintext refusal happens
// BEFORE that exchange, not just before using its result.
func TestDoctorCheckCopilotModelListedRefusesPlaintextUpstreamBeforeExchange(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	called := false
	stubListGitHubCopilotModels(t, func(ctx context.Context, caBundlePath, githubToken, upstream string) ([]meter.CopilotModel, error) {
		called = true
		return nil, nil
	})
	tokenFile := newCopilotAuthFixturePath(t, "gho_test_token")

	check := doctorCheckCopilotModelListed(context.Background(), tokenFile, "", "", "gpt-5.6-luna", "http://local-model:8080", "", false, doctorRoutesModeRouteKeys("route", "model"))
	if check.Err == nil {
		t.Fatal("check.Err = nil, want a failure naming the invalid upstream")
	}
	if !strings.Contains(check.Err.Error(), "route's upstream") || !strings.Contains(check.Err.Error(), "not valid for github-copilot") {
		t.Errorf("check.Err = %v, want it to name the route's upstream as not valid for github-copilot", check.Err)
	}
	if called {
		t.Fatal("listGitHubCopilotModelsFn (which itself exchanges the GitHub OAuth token) was called despite an invalid plaintext upstream")
	}
}

// TestDoctorListModelsCopilotRefusesPlaintextUpstreamBeforeExchange is
// TestDoctorCheckCopilotModelListedRefusesPlaintextUpstreamBeforeExchange's
// own counterpart for `doctor -list-models` itself: an invalid github-
// copilot route (a plaintext, non-Copilot upstream) fails
// sandbox.RoutePolicy.Validate's own meter.ValidateGitHubCopilotRoute
// check inside modelrole.SelectRoute before doctorListModels' branch
// ever runs, so it comes back as modelrole.ErrNoRouteAvailable rather
// than doctorListModels' own (now unreachable for this case) named
// relay_upstream diagnostic -- either way, the token exchange must never
// be attempted.
func TestDoctorListModelsCopilotRefusesPlaintextUpstreamBeforeExchange(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	called := false
	stubListGitHubCopilotModels(t, func(ctx context.Context, caBundlePath, githubToken, upstream string) ([]meter.CopilotModel, error) {
		called = true
		return nil, nil
	})

	var out bytes.Buffer
	in := doctorInputsForRoute(sessionconfig.Route{CredentialMode: meter.CredentialModeGitHubCopilot, Upstream: "http://local-model:8080"}, "gpt-5.6-luna")
	err := doctorListModels(context.Background(), &out, in)
	if !errors.Is(err, modelrole.ErrNoRouteAvailable) {
		t.Fatalf("doctorListModels = %v, want modelrole.ErrNoRouteAvailable (an invalid plaintext upstream makes the route unusable)", err)
	}
	if called {
		t.Fatal("listGitHubCopilotModelsFn was called despite an invalid plaintext upstream")
	}
}

// TestDoctorCheckCopilotModelListedRefusesNonCopilotHostBeforeExchange is
// TestDoctorCheckCopilotModelListedRefusesPlaintextUpstreamBeforeExchange's
// own counterpart for a stale, genuinely-https but non-Copilot
// relay_upstream (review round 1: a config left over from switching off a
// local-model route, e.g. https://openrouter.ai/api, satisfies
// ValidateUpstreamScheme and so previously sailed straight through to a
// real GitHub token exchange and Copilot bearer token being sent to that
// host) -- doctorValidateCopilotUpstream's new
// meter.ValidateGitHubCopilotRoute call must refuse it before either.
func TestDoctorCheckCopilotModelListedRefusesNonCopilotHostBeforeExchange(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	called := false
	stubListGitHubCopilotModels(t, func(ctx context.Context, caBundlePath, githubToken, upstream string) ([]meter.CopilotModel, error) {
		called = true
		return nil, nil
	})
	tokenFile := newCopilotAuthFixturePath(t, "gho_test_token")

	check := doctorCheckCopilotModelListed(context.Background(), tokenFile, "", "", "gpt-5.6-luna", "https://openrouter.ai/api", "", false, doctorRoutesModeRouteKeys("route", "model"))
	if check.Err == nil {
		t.Fatal("check.Err = nil, want a failure naming the non-Copilot upstream")
	}
	if !strings.Contains(check.Err.Error(), "route's upstream") || !strings.Contains(check.Err.Error(), "not valid for github-copilot") {
		t.Errorf("check.Err = %v, want it to name the route's upstream as not valid for github-copilot", check.Err)
	}
	if called {
		t.Fatal("listGitHubCopilotModelsFn (which itself exchanges the GitHub OAuth token) was called despite a non-Copilot https upstream")
	}
}

// TestDoctorListModelsCopilotRefusesNonCopilotHostBeforeExchange is
// TestDoctorCheckCopilotModelListedRefusesNonCopilotHostBeforeExchange's
// own counterpart for `doctor -list-models` itself: a non-Copilot https
// upstream fails the identical meter.ValidateGitHubCopilotRoute check
// inside modelrole.SelectRoute before doctorListModels' branch ever
// runs (see TestDoctorListModelsCopilotRefusesPlaintextUpstreamBeforeExchange's
// own doc comment).
func TestDoctorListModelsCopilotRefusesNonCopilotHostBeforeExchange(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	called := false
	stubListGitHubCopilotModels(t, func(ctx context.Context, caBundlePath, githubToken, upstream string) ([]meter.CopilotModel, error) {
		called = true
		return nil, nil
	})

	var out bytes.Buffer
	in := doctorInputsForRoute(sessionconfig.Route{CredentialMode: meter.CredentialModeGitHubCopilot, Upstream: "https://openrouter.ai/api"}, "gpt-5.6-luna")
	err := doctorListModels(context.Background(), &out, in)
	if !errors.Is(err, modelrole.ErrNoRouteAvailable) {
		t.Fatalf("doctorListModels = %v, want modelrole.ErrNoRouteAvailable (a non-Copilot upstream makes the route unusable)", err)
	}
	if called {
		t.Fatal("listGitHubCopilotModelsFn was called despite a non-Copilot https upstream")
	}
}

// newCopilotAuthFixturePath writes a minimal pi-shaped auth.json (via
// relay_copilot_flags_test.go's own writeCopilotAuthFixture helper, the
// "github-copilot" oauth entry resolveGitHubCopilotToken reads) to a temp
// file and returns its path.
func newCopilotAuthFixturePath(t *testing.T, refreshToken string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "auth.json")
	writeCopilotAuthFixture(t, path, refreshToken)
	return path
}

// TestDoctorValidateCopilotUpstreamRefusesPlaintextEvenWhenAllowed: found
// live in the 2026-09-25 closing walk -- a private http:// relay_upstream
// left in the config under relay_allow_plaintext_upstream passed
// ValidateUpstreamScheme, and doctor -list-models sent the exchanged
// Copilot token to it in plaintext.
func TestDoctorValidateCopilotUpstreamRefusesPlaintextEvenWhenAllowed(t *testing.T) {
	if err := doctorValidateCopilotUpstream("http://100.101.1.2:8080", true); err == nil {
		t.Fatal("doctorValidateCopilotUpstream accepted a plaintext upstream for a credentialed route")
	}
	if err := doctorValidateCopilotUpstream(effectiveCopilotUpstream(""), false); err != nil {
		t.Fatalf("default Copilot upstream rejected: %v", err)
	}
}

// TestDoctorCheckCopilotModelListedFailsListedButUnservableModel: found
// live in the 2026-09-25 closing walk -- gpt-5.6-luna is listed on Copilot
// but serves only /responses, so it passed this check and then failed spec
// drafting with zero successful relay calls. The fix line must suggest
// only models the relay can serve.
func TestDoctorCheckCopilotModelListedFailsListedButUnservableModel(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	stubListGitHubCopilotModels(t, func(ctx context.Context, caBundlePath, githubToken, upstream string) ([]meter.CopilotModel, error) {
		return []meter.CopilotModel{
			{ID: "gpt-5.6-luna", SupportedEndpoints: []string{"/responses", "ws:/responses"}},
			{ID: "claude-sonnet-5", ContextWindow: 200000, SupportedEndpoints: []string{"/chat/completions", "/v1/messages"}},
		}, nil
	})
	tokenFile := newCopilotAuthFixturePath(t, "gho_test_token")

	check := doctorCheckCopilotModelListed(context.Background(), tokenFile, "", "", "gpt-5.6-luna", "", "", false, doctorRoutesModeRouteKeys("route", "model"))
	if check.Err == nil || check.Advisory {
		t.Fatalf("check = %+v, want a hard failure for a /responses-only model", check)
	}
	if !strings.Contains(check.Fix, "claude-sonnet-5") || strings.Contains(check.Fix, "gpt-5.6-luna") {
		t.Errorf("Fix = %q, want only servable models suggested", check.Fix)
	}
	if ok := doctorCheckCopilotModelListed(context.Background(), tokenFile, "", "", "claude-sonnet-5", "", "", false, doctorRoutesModeRouteKeys("route", "model")); ok.Err != nil {
		t.Errorf("servable model: Err = %v, want nil", ok.Err)
	}
}

// TestDoctorCheckCopilotModelListedFailsPolicyDisabledModel: found live in
// the 2026-09-25 closing walk -- claude-sonnet-5 was listed with policy
// "disabled" and rejected at run time with 400 model_not_supported.
func TestDoctorCheckCopilotModelListedFailsPolicyDisabledModel(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	stubListGitHubCopilotModels(t, func(ctx context.Context, caBundlePath, githubToken, upstream string) ([]meter.CopilotModel, error) {
		return []meter.CopilotModel{
			{ID: "claude-sonnet-5", SupportedEndpoints: []string{"/chat/completions"}, PolicyState: "disabled"},
			{ID: "kimi-k3-copilot", ContextWindow: 917504, SupportedEndpoints: []string{"/chat/completions"}, PolicyState: "enabled"},
		}, nil
	})
	tokenFile := newCopilotAuthFixturePath(t, "gho_test_token")

	check := doctorCheckCopilotModelListed(context.Background(), tokenFile, "", "", "claude-sonnet-5", "", "", false, doctorRoutesModeRouteKeys("route", "model"))
	if check.Err == nil || !strings.Contains(check.Err.Error(), "disabled") {
		t.Fatalf("check = %+v, want a failure naming the disabled policy", check)
	}
	if !strings.Contains(check.Fix, "kimi-k3-copilot") || strings.Contains(check.Fix, "claude-sonnet-5") {
		t.Errorf("Fix = %q, want only enabled models suggested", check.Fix)
	}
}

// TestDoctorCheckCopilotModelListedAcceptsResponsesOnlyModelUnderResponsesAPI
// is the regression test for the Responses-route fix: relay_worker_api:
// openai-responses must make a /responses-only model (e.g. gpt-5.6-luna)
// pass this check, not just meter.CopilotModelUsable in isolation.
func TestDoctorCheckCopilotModelListedAcceptsResponsesOnlyModelUnderResponsesAPI(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	stubListGitHubCopilotModels(t, func(ctx context.Context, caBundlePath, githubToken, upstream string) ([]meter.CopilotModel, error) {
		return []meter.CopilotModel{
			{ID: "gpt-5.6-luna", ContextWindow: 200000, SupportedEndpoints: []string{"/responses"}},
		}, nil
	})
	tokenFile := newCopilotAuthFixturePath(t, "gho_test_token")

	check := doctorCheckCopilotModelListed(context.Background(), tokenFile, "", "", "gpt-5.6-luna", "", meter.RequestFormatOpenAIResponses, false, doctorRoutesModeRouteKeys("route", "model"))
	if check.Err != nil {
		t.Fatalf("doctorCheckCopilotModelListed with relay_worker_api openai-responses: Err = %v, want nil", check.Err)
	}
}

func TestCopilotServableModelIDsSkipsPreviewAndEmbeddingsAndPrefersPicker(t *testing.T) {
	got := copilotServableModelIDs([]meter.CopilotModel{
		{ID: "copilot-search-a", Preview: true, ContextWindow: 244000},
		{ID: "text-embedding-3-small"},
		{ID: "gpt-4o", ContextWindow: 64000},
		{ID: "kimi-k3-copilot", ContextWindow: 917504, ModelPickerEnabled: true},
		{ID: "claude-sonnet-5", ContextWindow: 200000, PolicyState: "disabled", ModelPickerEnabled: true},
	}, "", 5)
	if want := []string{"kimi-k3-copilot", "gpt-4o"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("copilotServableModelIDs = %v, want %v", got, want)
	}
}

func TestCopilotServableModelIDsFallsBackWhenFilterEmptiesIt(t *testing.T) {
	got := copilotServableModelIDs([]meter.CopilotModel{{ID: "only-preview", Preview: true}}, "", 5)
	if want := []string{"only-preview"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("copilotServableModelIDs = %v, want %v", got, want)
	}
	if clause := copilotSuggestionClause(nil); clause != "" {
		t.Fatalf("copilotSuggestionClause(nil) = %q, want empty", clause)
	}
}
