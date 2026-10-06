package meter

import (
	"fmt"
	"net/url"
	"path"
	"regexp"
	"strings"
)

const (
	CredentialHeaderAuthorization = "Authorization"
	CredentialHeaderXAPIKey       = "X-Api-Key"
)

const (
	// CredentialModeStatic is the default credential mode (also what an
	// empty mode resolves to): the route's API key is sent in its credential
	// header on every request.
	CredentialModeStatic = "static"
	// CredentialModeGitHubCopilot authenticates with a short-lived GitHub
	// Copilot API token exchanged from the operator's GitHub OAuth token.
	CredentialModeGitHubCopilot = "github-copilot"
	// CredentialModeChatGPTCodex authenticates with the operator's ChatGPT
	// OAuth access token as "Authorization: Bearer <token>" and the account
	// id as the "chatgpt-account-id" header. Nothing here exchanges or
	// refreshes the token: the ChatGPT OAuth refresh token rotates on use,
	// so refreshing it would log the host `codex` CLI out. The host resolves
	// a fresh access token from ~/.codex/auth.json before each launch (see
	// cmd/factoryd/relay_codex.go).
	CredentialModeChatGPTCodex = "chatgpt-codex"
)

const (
	// ChatGPTCodexAPIBase is the upstream ChatGPT backend Codex CLI talks
	// to when authenticated via ChatGPT OAuth login, reverse-engineered
	// from a live proxy capture (2026-09-23):
	// `codex exec` sends only POST <base>/responses, never GET /models. A
	// chatgpt-codex routes: entry defaults its upstream to it.
	ChatGPTCodexAPIBase = "https://chatgpt.com/backend-api/codex"
	// ChatGPTCodexResponsesPath is the only path codex's own provider
	// (wire_api="responses") ever requests.
	ChatGPTCodexResponsesPath = "/responses"
)

// The GitHub Copilot wire contract below is reverse-engineered from pi's own
// github-copilot provider, not GitHub's own public documentation (there is
// none for this internal API): badlogic/pi-mono, commit
// 2176b9dd8f0020bfb383bcf074f78a5f30efc29e --
//   - packages/ai/src/auth/oauth/github-copilot.ts (token exchange:
//     refreshGitHubCopilotAccessToken/getUrls)
//   - packages/ai/src/providers/github-copilot.ts (base URL)
const (
	// CopilotAPIBase is githubCopilotProvider()'s own baseUrl in
	// github-copilot.ts, and the value every Individual-plan Copilot
	// account resolves to in practice. A github-copilot routes: entry
	// defaults its upstream to it.
	CopilotAPIBase = "https://api.individual.githubcopilot.com"
	// CopilotChatCompletionsPath is the OpenAI-compatible chat/completions
	// path every Copilot model request in pi's own openai-completions
	// client hits.
	CopilotChatCompletionsPath = "/chat/completions"
	// CopilotResponsesPath is the Responses API path used by models whose
	// catalog entry names Pi's openai-responses client.
	CopilotResponsesPath = "/responses"
)

// CopilotAllowedPathPrefixFor is the allowed path prefix a github-copilot
// route defaults to for workerAPI, and the path that API sends every request
// to -- the one place that maps a model's api to a Copilot path, shared by
// that defaulting and CopilotModelUsable/sandbox.
// ValidateGitHubCopilotWorkerAPI's own mismatch check.
func CopilotAllowedPathPrefixFor(workerAPI string) string {
	if workerAPI == RequestFormatOpenAIResponses {
		return CopilotResponsesPath
	}
	return CopilotChatCompletionsPath
}

// HasPathPrefix reports whether requestPath is allowed under allowedPrefix:
// allowedPrefix's own trailing "/" is not significant (the caller need not
// trim it), "" or "/" allows every absolute path, and otherwise requestPath
// must equal allowedPrefix or start with allowedPrefix+"/".
func HasPathPrefix(requestPath, allowedPrefix string) bool {
	cleaned := path.Clean(requestPath)
	if cleaned != requestPath && cleaned+"/" != requestPath {
		return false
	}
	allowedPrefix = strings.TrimRight(allowedPrefix, "/")
	if allowedPrefix == "" {
		return strings.HasPrefix(requestPath, "/")
	}
	return requestPath == allowedPrefix || strings.HasPrefix(requestPath, allowedPrefix+"/")
}

// CredentialHeaderValue formats apiKey for the outbound credential header.
// An Authorization header means Bearer-scheme auth by convention on every
// OpenAI-compatible endpoint a route targets (OpenRouter, a self-hosted
// gateway, ...) -- found via review (Codex, PR #59): without this, the
// configured key was sent verbatim under Authorization with no scheme,
// which such an endpoint rejects outright. X-Api-Key carries the raw key
// with no scheme, matching Anthropic's own convention, so it is left
// untouched. An apiKey that already declares its own scheme (rare, but an
// operator might configure the full "Bearer <token>" as their credential
// directly) is not double-prefixed.
func CredentialHeaderValue(header, apiKey string) string {
	if header != CredentialHeaderAuthorization {
		return apiKey
	}
	if _, _, found := strings.Cut(apiKey, " "); found {
		return apiKey
	}
	return "Bearer " + apiKey
}

// ValidateChatGPTCodexRoute enforces CredentialModeChatGPTCodex's pinned
// route: the operator's ChatGPT OAuth token goes onto every request, so the
// upstream must be the ChatGPT Codex backend (any other host -- a leftover
// upstream from another route, or a forwarded default -- would receive the
// token), and the path prefix and worker base path must compose to exactly
// its /responses endpoint (a leftover allowed_path_prefix/worker_base_path
// would either 404 every request or widen which backend paths the worker
// can reach with that token).
func ValidateChatGPTCodexRoute(upstream, allowedPathPrefix, workerBasePath string) error {
	if strings.TrimRight(upstream, "/") != ChatGPTCodexAPIBase {
		return fmt.Errorf("relay upstream must be %q when auth mode is %q (got %q): the ChatGPT OAuth token is never sent to any other host", ChatGPTCodexAPIBase, CredentialModeChatGPTCodex, upstream)
	}
	if allowedPathPrefix != ChatGPTCodexResponsesPath || strings.Trim(workerBasePath, "/") != "" {
		return fmt.Errorf("relay allowed path prefix must be %q and worker base path empty when auth mode is %q (got %q and %q): remove allowed_path_prefix/worker_base_path from this chatgpt-codex routes: entry", ChatGPTCodexResponsesPath, CredentialModeChatGPTCodex, allowedPathPrefix, workerBasePath)
	}
	return nil
}

// copilotGHEHostPattern matches GitHub's documented Copilot inference host
// for GHE.com data-residency tenants: "copilot-api." + exactly one
// subdomain label (the tenant) + ".ghe.com" -- "copilot-api.a.b.ghe.com"
// (two labels) is deliberately NOT matched, since GitHub never documents a
// nested-tenant shape and a broader pattern would just be a wider place to
// hide a lookalike host. Matched so ValidateGitHubCopilotRoute below can
// give this specific, genuinely-Copilot-but-unsupported host its own
// "not supported yet" refusal, distinct from a lookalike-domain refusal.
var copilotGHEHostPattern = regexp.MustCompile(`^copilot-api\.[a-z0-9]([a-z0-9-]*[a-z0-9])?\.ghe\.com$`)

// ValidateGitHubCopilotRoute enforces CredentialModeGitHubCopilot's pinned
// route: the operator's Copilot API token goes onto every request, so the
// upstream must actually be a GitHub Copilot backend -- any other host (a
// leftover upstream from another route, a forwarded default, or a lookalike
// domain such as "evilgithubcopilot.com" or "githubcopilot.com.evil.com")
// would receive that token instead. Unlike ValidateChatGPTCodexRoute above,
// this is a host-suffix check, not an exact-URL match: GitHub Copilot's real
// API is served from more than one host depending on plan --
// api.individual.githubcopilot.com (CopilotAPIBase),
// api.business.githubcopilot.com, and api.enterprise.githubcopilot.com are
// all genuine upstreams for this same credential.
//
// GHE.com data-residency tenants (copilot-api.<tenant>.ghe.com) are
// deliberately REFUSED, not accepted, even though GitHub documents that
// host as a genuine Copilot inference endpoint: the OAuth token EXCHANGE
// (ExchangeGitHubCopilotToken) is hardcoded to api.github.com regardless of
// the upstream, which is the wrong identity domain for a GHE.com tenant's
// own login -- accepting the upstream pin alone here would let a GHE.com
// operator configure a route that fails at first real token exchange (or
// worse, silently exchanges against the wrong domain), not one that works
// end to end. Revisit only once the token exchange itself is GHE.com-aware
// (see USAGE_REFERENCE.md's Copilot-route section).
//
// Rejects a URL carrying userinfo (e.g.
// "https://x@api.individual.githubcopilot.com") outright, before even
// checking the host: embedding credentials in the upstream URL itself is
// never a legitimate configuration, and userinfo present alongside a
// lookalike host (e.g. "https://api.individual.githubcopilot.com@evil.com",
// where Hostname() alone would already resolve to "evil.com" and fail the
// suffix check below) must never be given the benefit of the doubt by
// silently stripping and ignoring it first.
//
// A single trailing dot on the host (the DNS root label, e.g.
// "api.individual.githubcopilot.com.") is normalized away before the
// suffix/pattern check, not rejected: it names the exact same host a
// leading resolver would reach, so treating it as a distinct, unpinned
// host would be a false rejection of a genuine upstream, not a safety
// gain.
//
// Shared by sandbox.RoutePolicy.Validate, the session config's route
// validation and factoryd doctor (doctorValidateCopilotUpstream), so all
// three fail on exactly the same upstream.
func ValidateGitHubCopilotRoute(upstream string) error {
	parsed, err := url.Parse(upstream)
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" {
		return fmt.Errorf("relay upstream must be an https URL when auth mode is %q (got %q): the Copilot API token is never sent in plaintext", CredentialModeGitHubCopilot, upstream)
	}
	if parsed.User != nil {
		return fmt.Errorf("relay upstream must not embed userinfo when auth mode is %q (got %q): the Copilot API token is never sent to a URL carrying its own embedded credentials", CredentialModeGitHubCopilot, upstream)
	}
	host := strings.ToLower(strings.TrimSuffix(parsed.Hostname(), "."))
	if host == "githubcopilot.com" || strings.HasSuffix(host, ".githubcopilot.com") {
		return nil
	}
	if copilotGHEHostPattern.MatchString(host) {
		return fmt.Errorf("relay upstream host %q is a GHE.com Copilot data-residency tenant, which is not supported yet: this repo's own OAuth token exchange (ExchangeGitHubCopilotToken) is hardcoded to api.github.com, so a GHE.com login's token would be sent to the wrong identity domain", parsed.Hostname())
	}
	return fmt.Errorf("relay upstream host must be, or end with, %q when auth mode is %q (got %q): the Copilot API token is never sent to any other host", ".githubcopilot.com", CredentialModeGitHubCopilot, parsed.Hostname())
}
