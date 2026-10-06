package sandbox

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"buildgate/internal/meter"
)

// Environment names a route's provider supplies to the worker. Each holds a
// placeholder the supervisor replaces with the secret on a request to the
// route's endpoint; the worker never holds the secret.
const (
	EnvChatGPTToken   = "BG_CHATGPT_TOKEN"
	EnvChatGPTAccount = "BG_CHATGPT_ACCOUNT"
	EnvModelKey       = "BG_MODEL_KEY"
)

// Worker environment that names the placeholders above for the harness
// adapter (agent/pi/scripts/harness_adapters.py).
const (
	envModelKeyEnv      = "FACTORY_MODEL_KEY_ENV"
	envModelHeadersJSON = "FACTORY_MODEL_HEADERS_JSON"
)

// Worker-side model route facts, harness-neutral: every harness adapter in
// the worker (agent/pi/scripts/harness_adapters.py) renders its own CLI's
// configuration from these. No credential travels here.
const (
	envModelBaseURL   = "FACTORY_MODEL_BASE_URL"
	envModelID        = "FACTORY_MODEL_ID"
	envModelAPI       = "FACTORY_MODEL_API"
	envModelExtraJSON = "FACTORY_MODEL_EXTRA_JSON"
)

const chatGPTAccountHeader = "chatgpt-account-id"

// RouteAccess is how a worker launched through a Runtime reaches its model
// route: the provider holding the credential, the one endpoint the sandbox's
// network policy admits, and the worker environment describing both.
type RouteAccess struct {
	// Provider is the gateway provider holding the route's credential, empty
	// for a route with none (allow_no_credential).
	Provider string
	// CredentialEnv names the placeholders the provider supplies.
	CredentialEnv []string
	Endpoint      RouteEndpoint
	// Environment is the FACTORY_MODEL_* entries the worker gets.
	Environment []string
}

// RouteEndpoint is the only destination the worker may reach for its model.
type RouteEndpoint struct {
	Host string
	Port int
	// TLS is false for a plaintext (http) upstream.
	TLS    bool
	Method string
	// Path is the one request path admitted, or with PathIsPrefix the prefix
	// every admitted path starts with.
	Path         string
	PathIsPrefix bool
	// Binaries are the worker executables allowed to connect.
	Binaries []string
}

const maxProviderNameLength = 63

// ProviderName is the gateway provider of route for this data directory.
// The gateway is shared by every profile on the machine, so the name carries
// the data directory's label: two profiles never push to one provider.
func ProviderName(dataDir, route string) (string, error) {
	absDataDir, err := filepath.Abs(dataDir)
	if err != nil {
		return "", fmt.Errorf("resolve sandbox data directory: %w", err)
	}
	clean := strings.Trim(sandboxNameDisallowed.ReplaceAllString(strings.ToLower(route), "-"), "-")
	if clean == "" {
		return "", fmt.Errorf("route provider: route name %q holds no letter or digit", route)
	}
	name := "bg-" + dataDirLabel(absDataDir)[:8] + "-" + clean
	if len(name) > maxProviderNameLength {
		return "", fmt.Errorf("route provider: route name %q is too long", route)
	}
	return name, nil
}

// RouteAccess derives the worker's access to the route p describes. binaries
// are the harness's executables that call the model. It refuses a route the
// sandbox runtime cannot serve: one with no worker model id (an
// Anthropic-shaped worker, whose key the harness reads from a name the
// factory does not let a worker hold). A github-copilot route's key is the
// operator's GitHub login token, which the Copilot API accepts as a bearer
// token and which does not expire during a step.
func (p RoutePolicy) RouteAccess(dataDir string, binaries []string) (RouteAccess, error) {
	if err := p.Validate(); err != nil {
		return RouteAccess{}, err
	}
	if err := p.ValidateUpstreamScheme(); err != nil {
		return RouteAccess{}, err
	}
	if len(binaries) == 0 {
		return RouteAccess{}, errors.New("sandbox runtime: the harness names no executable that may reach the model")
	}
	if p.WorkerModelID == "" {
		return RouteAccess{}, fmt.Errorf("sandbox runtime: route %q has no worker model id; an Anthropic-shaped worker is not supported", p.Route)
	}
	endpoint, baseURL, err := p.routeEndpoint()
	if err != nil {
		return RouteAccess{}, err
	}
	endpoint.Binaries = append([]string(nil), binaries...)
	access := RouteAccess{Endpoint: endpoint}

	headers := map[string]string{}
	switch {
	case p.AuthMode == meter.CredentialModeChatGPTCodex:
		access.CredentialEnv = []string{EnvChatGPTToken, EnvChatGPTAccount}
		headers[chatGPTAccountHeader] = EnvChatGPTAccount
	case !p.AllowUnauthenticatedUpstream:
		access.CredentialEnv = []string{EnvModelKey}
	}
	if len(access.CredentialEnv) > 0 {
		if !endpoint.TLS {
			return RouteAccess{}, fmt.Errorf("sandbox runtime: route %q sends a credential to a plaintext upstream", p.Route)
		}
		if access.Provider, err = ProviderName(dataDir, p.Route); err != nil {
			return RouteAccess{}, err
		}
	}

	api := p.WorkerModelAPI
	if api == "" {
		api = meter.RequestFormatOpenAICompletions
	}
	access.Environment = []string{
		envModelBaseURL + "=" + baseURL,
		envModelID + "=" + p.WorkerModelID,
		envModelAPI + "=" + api,
	}
	extra, err := compactJSONObject(p.WorkerModelExtraJSON)
	if err != nil {
		return RouteAccess{}, fmt.Errorf("worker model extra JSON: %w", err)
	}
	if extra != "" {
		access.Environment = append(access.Environment, envModelExtraJSON+"="+extra)
	}
	if len(access.CredentialEnv) > 0 {
		access.Environment = append(access.Environment, envModelKeyEnv+"="+access.CredentialEnv[0])
	}
	if len(headers) > 0 {
		encoded, err := json.Marshal(headers)
		if err != nil {
			return RouteAccess{}, err
		}
		access.Environment = append(access.Environment, envModelHeadersJSON+"="+string(encoded))
	}
	return access, nil
}

// routeEndpoint reads p.Upstream into the endpoint the policy admits and the
// base URL the worker is given. The relay forwards a worker's request path
// onto the upstream's own path, so the admitted path is the two joined: one
// exact path for chatgpt-codex and github-copilot (each serves one API path,
// and other paths on the same host the worker has no business with), a
// prefix for a static route, whose API may have several. An exact path also
// leaves the worker no path segment to put a credential placeholder in,
// which the supervisor would resolve.
func (p RoutePolicy) routeEndpoint() (RouteEndpoint, string, error) {
	upstream, err := url.Parse(p.Upstream)
	if err != nil {
		return RouteEndpoint{}, "", fmt.Errorf("sandbox runtime: route %q upstream: %w", p.Route, err)
	}
	if upstream.Hostname() == "" || upstream.User != nil || upstream.RawQuery != "" || upstream.Fragment != "" {
		return RouteEndpoint{}, "", fmt.Errorf("sandbox runtime: route %q upstream must be scheme://host[:port][/path] with no user, query or fragment", p.Route)
	}
	endpoint := RouteEndpoint{Host: upstream.Hostname(), Method: "POST"}
	switch upstream.Scheme {
	case "https":
		endpoint.TLS, endpoint.Port = true, 443
	case "http":
		endpoint.Port = 80
	default:
		return RouteEndpoint{}, "", fmt.Errorf("sandbox runtime: route %q upstream scheme must be http or https", p.Route)
	}
	if port := upstream.Port(); port != "" {
		if endpoint.Port, err = strconv.Atoi(port); err != nil || endpoint.Port < 1 || endpoint.Port > 65535 {
			return RouteEndpoint{}, "", fmt.Errorf("sandbox runtime: route %q upstream port %q is invalid", p.Route, port)
		}
	}
	prefix := p.AllowedPathPrefix
	if prefix == "" {
		prefix = "/v1/messages"
	}
	upstreamPath := strings.TrimRight(upstream.Path, "/")
	endpoint.Path = upstreamPath + prefix
	endpoint.PathIsPrefix = p.AuthMode != meter.CredentialModeChatGPTCodex && p.AuthMode != meter.CredentialModeGitHubCopilot

	baseURL := upstream.Scheme + "://" + upstream.Host + upstreamPath
	if trimmed := strings.Trim(p.WorkerBasePath, "/"); trimmed != "" {
		baseURL += "/" + trimmed
	}
	return endpoint, baseURL, nil
}

// compactJSONObject returns value compacted, or "" when it is blank. Anything
// but a JSON object is an error; `null` decodes without one and is refused.
func compactJSONObject(value string) (string, error) {
	if strings.TrimSpace(value) == "" {
		return "", nil
	}
	var object map[string]any
	if err := json.Unmarshal([]byte(value), &object); err != nil {
		return "", fmt.Errorf("must be a JSON object: %w", err)
	}
	if object == nil {
		return "", errors.New("must be a JSON object, got null")
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, []byte(value)); err != nil {
		return "", err
	}
	return compact.String(), nil
}

// RouteCredential builds what the Runtime pushes to access.Provider before a
// launch. expiresAt is the credential's own expiry, zero when it has none. A
// credential that expires before now plus stepTimeout is refused: a sandbox
// keeps the value it started with, so a step that outlives it would fail on
// its next model call.
func (p RoutePolicy) RouteCredential(access RouteAccess, apiKey, githubToken, chatGPTToken, chatGPTAccountID RouteSecret, expiresAt, now time.Time, stepTimeout time.Duration) (RouteCredential, error) {
	if access.Provider == "" {
		return RouteCredential{}, fmt.Errorf("sandbox runtime: route %q has no provider to push a credential to", p.Route)
	}
	values := map[string]string{}
	switch p.AuthMode {
	case meter.CredentialModeChatGPTCodex:
		values[EnvChatGPTToken] = chatGPTToken.reveal()
		values[EnvChatGPTAccount] = chatGPTAccountID.reveal()
	case meter.CredentialModeGitHubCopilot:
		values[EnvModelKey] = githubToken.reveal()
	default:
		values[EnvModelKey] = apiKey.reveal()
	}
	for _, name := range access.CredentialEnv {
		if values[name] == "" {
			return RouteCredential{}, fmt.Errorf("sandbox runtime: route %q has no value for %s", p.Route, name)
		}
	}
	if len(values) != len(access.CredentialEnv) {
		return RouteCredential{}, fmt.Errorf("sandbox runtime: route %q: credential does not match the route's access", p.Route)
	}
	if !expiresAt.IsZero() && expiresAt.Before(now.Add(stepTimeout)) {
		return RouteCredential{}, fmt.Errorf("sandbox runtime: route %q credential expires at %s, before this step's budget of %s ends; refresh it and retry",
			p.Route, expiresAt.UTC().Format(time.RFC3339), stepTimeout)
	}
	return RouteCredential{Provider: access.Provider, ExpiresAt: expiresAt, values: values}, nil
}

// MeterMiddleware is the name the gateway registers factoryd-meter under.
const MeterMiddleware = "factoryd-meter"

// maxMeterRunNameLength is the meter's own limit on a run name.
const maxMeterRunNameLength = 63

// MeterRunName names the run's directory under the meter's ledger root, which
// every profile on the machine shares: the data directory's label, the run
// id in the meter's alphabet, and a hash of the exact run id so two ids that
// read alike after cleaning or shortening never share a directory.
func MeterRunName(dataDir, runID string) (string, error) {
	absDataDir, err := filepath.Abs(dataDir)
	if err != nil {
		return "", fmt.Errorf("resolve sandbox data directory: %w", err)
	}
	if runID == "" {
		return "", errors.New("meter run name: run id is required")
	}
	sum := sha256.Sum256([]byte(runID))
	prefix := dataDirLabel(absDataDir)[:8] + "-"
	suffix := "-" + hex.EncodeToString(sum[:])[:8]
	clean := strings.Trim(sandboxNameDisallowed.ReplaceAllString(strings.ToLower(runID), "-"), "-")
	if room := maxMeterRunNameLength - len(prefix) - len(suffix); len(clean) > room {
		clean = strings.TrimRight(clean[:room], "-")
	}
	if clean == "" {
		return prefix + suffix[1:], nil
	}
	return prefix + clean + suffix, nil
}

// MeterConfig is the per-sandbox configuration the gateway hands the meter
// with every request of this sandbox (meter.DecodePolicy reads it). The
// ceilings are p's, which the caller has already cut to what the run has
// left; the meter counts this sandbox from zero.
func (p RoutePolicy) MeterConfig(dataDir, runID, sandboxName string) (map[string]any, error) {
	runName, err := MeterRunName(dataDir, runID)
	if err != nil {
		return nil, err
	}
	requestFormat := p.WorkerModelAPI
	if requestFormat == "" {
		requestFormat = meter.RequestFormatOpenAICompletions
	}
	config := map[string]any{
		"run":                                   runName,
		"sandbox":                               sandboxName,
		"route":                                 p.AuthMode,
		"usage_format":                          p.UsageFormat,
		"request_format":                        requestFormat,
		"token_ceiling":                         int64(p.TokenCeiling),
		"cost_ceiling_micro_usd":                p.CostCeilingMicroUSD,
		"input_price_micro_usd_per_mtok":        p.InputMicroUSDPerMTok,
		"cached_input_price_micro_usd_per_mtok": p.CachedInputMicroUSDPerMTok,
		"cache_write_price_micro_usd_per_mtok":  p.CacheWriteMicroUSDPerMTok,
		"output_price_micro_usd_per_mtok":       p.OutputMicroUSDPerMTok,
		"token_budget":                          int64(p.TokenBudget),
		"token_window_seconds":                  int64(p.TokenBudgetWindow / time.Second),
		"cost_budget_micro_usd":                 p.CostBudgetMicroUSD,
		"cost_window_seconds":                   int64(p.CostBudgetWindow / time.Second),
		"requests_per_minute":                   int64(p.RequestsPerMinute),
		"max_request_bytes":                     p.MaxRequestBytes,
	}
	if p.AuthMode == meter.CredentialModeGitHubCopilot {
		config["rewrite"] = meter.RewriteGitHubCopilot
	}
	return config, nil
}
