package meter

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// copilotTokenExchangeURL is exactly getUrls("github.com").copilotTokenUrl
// in pi's github-copilot.ts (see routes.go's source citation). A GET (no
// method override in pi's own fetchJson call), not a POST.
const copilotTokenExchangeURL = "https://api.github.com/copilot_internal/v2/token"

// OutboundTransport returns http.DefaultTransport when caBundlePath is
// empty, or a Transport whose TLS RootCAs is the platform's default system
// cert pool PLUS caBundlePath's certificate(s) when set, for cmd/factoryd's
// host-side HTTP callers (doctor's and quickstart's model listing) behind a
// corporate TLS-interception proxy. Deliberately additive, not
// SSL_CERT_FILE, which Go's crypto/x509 on Linux treats as REPLACING the
// default trust file: a public upstream must still verify.
func OutboundTransport(caBundlePath string) (http.RoundTripper, error) {
	if caBundlePath == "" {
		return http.DefaultTransport, nil
	}
	pem, err := os.ReadFile(caBundlePath)
	if err != nil {
		return nil, fmt.Errorf("read CA bundle: %w", err)
	}
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("CA bundle %s: no PEM certificate found", caBundlePath)
	}
	// Clone http.DefaultTransport rather than starting from a bare
	// &http.Transport{}: a bare struct silently drops Proxy (HTTP(S)_PROXY
	// support), the dial/idle/handshake timeouts, and ForceAttemptHTTP2 --
	// found via adversarial review, corporate-ca branch.
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.TLSClientConfig = &tls.Config{RootCAs: pool}
	return t, nil
}

// NoRedirectCheckRedirect is an http.Client.CheckRedirect that stops at the
// first redirect and hands the caller that 3xx response as-is, instead of
// following it. Go's default http.Client forwards most request headers
// -- a credential header included -- to a same-scheme redirect target,
// which could leak that credential to a different host or port than the
// one it was actually configured for. Every host-side caller that sends a
// real credential over HTTP from this repository (cmd/factoryd's own
// model-listing calls) uses it.
func NoRedirectCheckRedirect(*http.Request, []*http.Request) error {
	return http.ErrUseLastResponse
}

// ExchangeGitHubCopilotToken performs one real Copilot token-exchange round
// trip and returns only the resulting token's own expiry, for cmd/factoryd's
// doctor check that a configured GitHub OAuth token actually works.
// caBundlePath is OutboundTransport's.
func ExchangeGitHubCopilotToken(ctx context.Context, caBundlePath, githubToken string) (expiresAt time.Time, err error) {
	transport, err := OutboundTransport(caBundlePath)
	if err != nil {
		return time.Time{}, err
	}
	client := &http.Client{Transport: transport, Timeout: 10 * time.Second, CheckRedirect: NoRedirectCheckRedirect}
	_, expiresAt, err = exchangeGitHubCopilotToken(ctx, client, copilotTokenExchangeURL, githubToken)
	return expiresAt, err
}

// exchangeGitHubCopilotToken performs one token-exchange HTTP round trip
// against tokenURL. Its returned error never embeds the upstream response
// body -- only the status code or a parse-shape complaint -- since the raw
// body of a GitHub API error response can echo request metadata.
func exchangeGitHubCopilotToken(ctx context.Context, client *http.Client, tokenURL, githubToken string) (token string, expiresAt time.Time, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, tokenURL, nil)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("build copilot token exchange request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	// Bearer, not the "token <oauth-token>" scheme some other GitHub API
	// clients use for a personal access token -- confirmed against pi's own
	// refreshGitHubCopilotAccessToken, which sends exactly
	// `Authorization: Bearer ${refreshToken}` here.
	req.Header.Set("Authorization", "Bearer "+githubToken)
	req.Header.Set("User-Agent", CopilotUserAgent)
	req.Header.Set("Editor-Version", CopilotEditorVersion)
	req.Header.Set("Editor-Plugin-Version", CopilotEditorPluginVersion)
	req.Header.Set("Copilot-Integration-Id", CopilotIntegrationID)

	resp, err := client.Do(req)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("copilot token exchange request: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", time.Time{}, fmt.Errorf("read copilot token exchange response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", time.Time{}, fmt.Errorf("copilot token exchange: upstream returned status %d", resp.StatusCode)
	}
	var parsed struct {
		Token     string `json:"token"`
		ExpiresAt int64  `json:"expires_at"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", time.Time{}, fmt.Errorf("parse copilot token exchange response: %w", err)
	}
	if parsed.Token == "" || parsed.ExpiresAt <= 0 {
		return "", time.Time{}, errors.New("copilot token exchange: response missing token or expires_at")
	}
	return parsed.Token, time.Unix(parsed.ExpiresAt, 0), nil
}

// copilotModelsPath is GitHub Copilot's own model-listing endpoint, hit
// with the same base URL and identifying headers as a chat request. The
// response shape modeled in CopilotModel below is this package's own
// best-effort understanding of GitHub's Copilot model-listing response (the
// same shape third-party Copilot proxies such as copilot-api parse), not a
// line-by-line port of a cited pi-mono source file. ListGitHubCopilotModels
// is never called from a live model turn in this repository's own test
// suite (no live model calls -- AGENTS.md); doctorCheckCopilotModelListed
// and quickstart's own model picker are the real live proof points.
const copilotModelsPath = "/models"

// CopilotModel is one entry from GET <upstream>/models. Every field is
// best-effort: a zero value means "not reported by this account/plan",
// never "false" or "free" -- Preview/ModelPickerEnabled/IsPremium default
// false and ContextWindow/MaxPromptTokens/MaxOutputTokens/Multiplier
// default 0 when GitHub omits them, so callers must not treat an absent
// field as a confirmed negative. ContextWindow is the value this package
// recommends configuring as relay_worker_model_extra_json.contextWindow
// -- listGitHubCopilotModels prefers max_prompt_tokens over
// max_context_window_tokens for it (Codex review, round 2: Copilot's own
// per-request admission limit is max_prompt_tokens, smaller than
// max_context_window_tokens for several models -- configuring the larger
// figure understates how much of the model's own context a real request
// can actually use before Copilot itself rejects it, the same class of
// bug -relay-worker-model-extra-json's own doc comment already documents
// for a local-model route). MaxPromptTokens is also exposed on its own
// for a caller that wants the raw, unselected value.
type CopilotModel struct {
	ID                 string
	Name               string
	Vendor             string
	Preview            bool
	ModelPickerEnabled bool
	ContextWindow      int
	MaxPromptTokens    int
	MaxOutputTokens    int
	IsPremium          bool
	Multiplier         float64
	// SupportedEndpoints is the listing's own supported_endpoints (e.g.
	// "/chat/completions", "/responses", "/v1/messages"); empty when the
	// listing omits it. This relay forwards worker requests to either
	// CopilotChatCompletionsPath or CopilotResponsesPath, chosen by the
	// configured relay_worker_api (see CopilotModelUsable/
	// CopilotWorkerAPIFor) -- a model whose supported_endpoints exclude
	// both (e.g. a Claude model listed for Copilot's own /v1/messages
	// only) cannot serve a run under either.
	SupportedEndpoints []string
	// PolicyState is the listing's own policy.state ("enabled",
	// "unconfigured", ...), empty when the listing carries no policy. A
	// model whose policy is not "enabled" is listed but rejected with 400
	// model_not_supported until the user or org enables it in GitHub's
	// Copilot settings -- found live in the 2026-09-25 closing walk
	// (claude-sonnet-5).
	PolicyState string
}

// CopilotModelUsable reports whether this relay can run m under workerAPI
// (RequestFormatOpenAICompletions/"" or RequestFormatOpenAIResponses -- see
// CopilotAllowedPathPrefixFor), and why not, split into reason (what's
// wrong; empty when ok) and fix (the specific relay_worker_api change
// that would make it usable, or "" when ok, or when no such setting
// exists -- a policy refusal, or m serving neither path this relay
// forwards to). Kept as two strings, not one combined message, so a
// caller that already shows m's own endpoints elsewhere (doctor
// -list-models's own "endpoints: ..." tag) can print just fix without
// repeating them; a caller building one self-contained message
// (doctorCheckCopilotModelListed's own Err) joins reason and fix itself.
//
// Two ways a listed model is unusable: its supported_endpoints exclude
// the path workerAPI would forward to (found live in the 2026-09-25 closing
// walk: gpt-5.6-luna serves /responses only, so it was unusable under the
// then-only-supported chat/completions path; under
// relay_worker_api: openai-responses it is usable), or its policy.state is
// not "enabled" (claude-sonnet-5 was listed with policy "disabled" and
// rejected with 400 model_not_supported). A listing that omits either field
// is assumed usable.
func CopilotModelUsable(m CopilotModel, workerAPI string) (ok bool, reason string, fix string) {
	if m.PolicyState != "" && m.PolicyState != "enabled" {
		return false, fmt.Sprintf("its Copilot policy is %q -- enable it in your GitHub Copilot settings (or ask your org admin)", m.PolicyState), ""
	}
	if len(m.SupportedEndpoints) == 0 {
		return true, "", ""
	}
	wantPath := CopilotAllowedPathPrefixFor(workerAPI)
	var servesResponses, servesChatCompletions bool
	for _, e := range m.SupportedEndpoints {
		switch copilotNormalizeEndpoint(e) {
		case wantPath:
			return true, "", ""
		case CopilotResponsesPath:
			servesResponses = true
		case CopilotChatCompletionsPath:
			servesChatCompletions = true
		}
	}
	reason = fmt.Sprintf("it serves only %s", strings.Join(m.SupportedEndpoints, ", "))
	switch {
	case servesResponses && workerAPI != RequestFormatOpenAIResponses:
		fix = fmt.Sprintf("set models.<name>.api: %s to use it", RequestFormatOpenAIResponses)
	case servesChatCompletions && workerAPI == RequestFormatOpenAIResponses:
		fix = "unset models.<name>.api (or set it to \"" + RequestFormatOpenAICompletions + "\") to use it"
	default:
		// Serves neither path this relay forwards to (e.g. a Claude model
		// listed for Copilot's own /v1/messages only) -- no models.<name>.api
		// setting fixes this, unlike the two cases above.
		fix = fmt.Sprintf("this relay forwards %s or %s only, and no models.<name>.api setting makes it usable", CopilotChatCompletionsPath, CopilotResponsesPath)
	}
	return false, reason, fix
}

// copilotNormalizeEndpoint normalises one supported_endpoints entry so
// "chat/completions", "/chat/completions/", "/v1/chat/completions" (and
// the same three spellings for "responses") all compare equal to
// CopilotChatCompletionsPath/CopilotResponsesPath.
func copilotNormalizeEndpoint(e string) string {
	return "/" + strings.TrimPrefix(strings.Trim(e, "/"), "v1/")
}

// CopilotModelUsableAnyAPI reports whether m is usable under SOME
// relay_worker_api (preferring RequestFormatOpenAICompletions, today's
// route default, when m serves both paths) and, when so, which one --
// folding together the endpoint check (which path m serves) and the
// policy check (m.PolicyState) CopilotModelUsable applies for one fixed
// workerAPI. The single predicate quickstart's own Copilot model picker,
// config writer, and suggestion list all use (unlike doctor, which always
// checks against one already-configured relay_worker_api and so keeps
// using CopilotModelUsable directly): quickstart can simply write
// whichever relay_worker_api a chosen model needs, so there is no reason
// to check "usable under completions" and "usable under responses"
// separately and combine the results itself.
func CopilotModelUsableAnyAPI(m CopilotModel) (api string, ok bool, why string) {
	if m.PolicyState != "" && m.PolicyState != "enabled" {
		return "", false, fmt.Sprintf("its Copilot policy is %q -- enable it in your GitHub Copilot settings (or ask your org admin)", m.PolicyState)
	}
	if len(m.SupportedEndpoints) == 0 {
		return RequestFormatOpenAICompletions, true, ""
	}
	var servesResponses bool
	for _, e := range m.SupportedEndpoints {
		switch copilotNormalizeEndpoint(e) {
		case CopilotChatCompletionsPath:
			return RequestFormatOpenAICompletions, true, ""
		case CopilotResponsesPath:
			servesResponses = true
		}
	}
	if servesResponses {
		return RequestFormatOpenAIResponses, true, ""
	}
	return "", false, fmt.Sprintf("this relay forwards %s or %s only, and it serves only %s; no models.<name>.api setting makes it usable", CopilotChatCompletionsPath, CopilotResponsesPath, strings.Join(m.SupportedEndpoints, ", "))
}

// ListGitHubCopilotModels exchanges githubToken for a short-lived Copilot
// API token (the same exchange every forwarded worker request performs --
// see copilotCredentialSource.exchange) and lists the models that token is
// entitled to. Host-side only, like ExchangeGitHubCopilotToken (whose own
// doc comment explains caBundlePath) -- called by cmd/factoryd's `doctor
// -list-models` and `quickstart`'s own model picker, never from inside the
// sandbox. Its returned error never includes the upstream response body
// (matching exchange's own discipline): a 401/403 body from GitHub can
// echo request metadata, and neither githubToken nor the exchanged
// Copilot token is ever logged or returned.
//
// upstream is the API base the listing is fetched from -- CopilotAPIBase
// (the Individual-plan default) when empty, or the caller's own
// effective relay_upstream otherwise, so a Business/Enterprise account
// configured with a different Copilot API host (see
// applyGitHubCopilotRelayDefaults's own doc comment: the relay forwards
// chat requests to exactly this same upstream) gets a listing from the
// SAME host its real requests use, not always the individual-plan one
// (Codex review, round 2).
func ListGitHubCopilotModels(ctx context.Context, caBundlePath, githubToken, upstream string) ([]CopilotModel, error) {
	transport, err := OutboundTransport(caBundlePath)
	if err != nil {
		return nil, err
	}
	client := &http.Client{Transport: transport, Timeout: 10 * time.Second, CheckRedirect: NoRedirectCheckRedirect}
	return listGitHubCopilotModels(ctx, client, copilotTokenExchangeURL, copilotModelsURL(upstream), githubToken)
}

// copilotModelsURL resolves upstream (empty defaults to CopilotAPIBase,
// the Individual-plan host) to the full GET .../models URL -- factored
// out of ListGitHubCopilotModels so this resolution is unit-testable on
// its own, without a real token exchange.
func copilotModelsURL(upstream string) string {
	if upstream == "" {
		upstream = CopilotAPIBase
	}
	return strings.TrimRight(upstream, "/") + copilotModelsPath
}

// listGitHubCopilotModels is ListGitHubCopilotModels' own logic with the
// token-exchange and models-listing URLs overridable, so tests can point
// both at an httptest.Server instead of the real api.github.com/
// api.individual.githubcopilot.com.
func listGitHubCopilotModels(ctx context.Context, client *http.Client, tokenURL, modelsURL, githubToken string) ([]CopilotModel, error) {
	token, _, err := exchangeGitHubCopilotToken(ctx, client, tokenURL, githubToken)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, modelsURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build copilot models request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("User-Agent", CopilotUserAgent)
	req.Header.Set("Editor-Version", CopilotEditorVersion)
	req.Header.Set("Editor-Plugin-Version", CopilotEditorPluginVersion)
	req.Header.Set("Copilot-Integration-Id", CopilotIntegrationID)

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("copilot models request: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read copilot models response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("copilot models request: upstream returned status %d", resp.StatusCode)
	}
	var parsed struct {
		Data []struct {
			ID                 string `json:"id"`
			Name               string `json:"name"`
			Vendor             string `json:"vendor"`
			Preview            bool   `json:"preview"`
			ModelPickerEnabled bool   `json:"model_picker_enabled"`
			// Raw, decoded leniently below: an unexpected shape for either
			// field must not fail the whole listing (both were ignored
			// before they were parsed at all).
			SupportedEndpoints json.RawMessage `json:"supported_endpoints"`
			Policy             json.RawMessage `json:"policy"`
			Capabilities       struct {
				Limits struct {
					MaxContextWindowTokens int `json:"max_context_window_tokens"`
					MaxPromptTokens        int `json:"max_prompt_tokens"`
					MaxOutputTokens        int `json:"max_output_tokens"`
				} `json:"limits"`
			} `json:"capabilities"`
			Billing struct {
				IsPremium  bool    `json:"is_premium"`
				Multiplier float64 `json:"multiplier"`
			} `json:"billing"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("parse copilot models response: %w", err)
	}
	models := make([]CopilotModel, 0, len(parsed.Data))
	for _, m := range parsed.Data {
		if m.ID == "" {
			continue
		}
		// Prefer max_prompt_tokens (Copilot's own real per-request
		// admission limit) over max_context_window_tokens for
		// ContextWindow -- see CopilotModel's own doc comment.
		var endpoints []string
		_ = json.Unmarshal(m.SupportedEndpoints, &endpoints)
		var policyObj struct {
			State string `json:"state"`
		}
		_ = json.Unmarshal(m.Policy, &policyObj)
		policy := policyObj.State
		contextWindow := m.Capabilities.Limits.MaxPromptTokens
		if contextWindow == 0 {
			contextWindow = m.Capabilities.Limits.MaxContextWindowTokens
		}
		models = append(models, CopilotModel{
			ID:                 m.ID,
			Name:               m.Name,
			Vendor:             m.Vendor,
			Preview:            m.Preview,
			ModelPickerEnabled: m.ModelPickerEnabled,
			ContextWindow:      contextWindow,
			MaxPromptTokens:    m.Capabilities.Limits.MaxPromptTokens,
			MaxOutputTokens:    m.Capabilities.Limits.MaxOutputTokens,
			IsPremium:          m.Billing.IsPremium,
			Multiplier:         m.Billing.Multiplier,
			SupportedEndpoints: endpoints,
			PolicyState:        policy,
		})
	}
	return models, nil
}
