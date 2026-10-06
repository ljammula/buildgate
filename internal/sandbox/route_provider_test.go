package sandbox

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/structpb"

	"buildgate/internal/meter"
)

func chatGPTPolicy() RoutePolicy {
	p := validRelayPolicy()
	p.Route = "chatgpt"
	p.Upstream = meter.ChatGPTCodexAPIBase
	p.AllowedPathPrefix = meter.ChatGPTCodexResponsesPath
	p.AuthMode = meter.CredentialModeChatGPTCodex
	p.UsageFormat = meter.UsageFormatOpenAIResponses
	p.WorkerModelID = "gpt-5.6-luna"
	p.WorkerModelAPI = meter.RequestFormatOpenAIResponses
	return p
}

func localModelPolicy() RoutePolicy {
	p := validRelayPolicy()
	p.Route = "Local Model"
	p.Upstream = "http://100.64.0.1:8080"
	p.AllowedPathPrefix = "/v1/chat/completions"
	p.UsageFormat = meter.UsageFormatOpenAI
	p.WorkerModelID = "qwen"
	p.WorkerBasePath = "/v1"
	p.WorkerModelExtraJSON = `{ "contextWindow": 32000 }`
	p.AllowUnauthenticatedUpstream = true
	p.AllowPlaintextUpstream = true
	return p
}

var piBinaries = []string{"/usr/local/bin/node"}

func dataDirPrefix(t *testing.T, dataDir string) string {
	t.Helper()
	abs, err := filepath.Abs(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	return dataDirLabel(abs)[:8]
}

func TestRouteAccessForChatGPTAdmitsOnlyTheResponsesPath(t *testing.T) {
	dataDir := t.TempDir()
	access, err := chatGPTPolicy().RouteAccess(dataDir, piBinaries)
	if err != nil {
		t.Fatal(err)
	}
	want := RouteAccess{
		Provider:      "bg-" + dataDirPrefix(t, dataDir) + "-chatgpt",
		CredentialEnv: []string{"BG_CHATGPT_TOKEN", "BG_CHATGPT_ACCOUNT"},
		Endpoint: RouteEndpoint{
			Host: "chatgpt.com", Port: 443, TLS: true, Method: "POST",
			Path: "/backend-api/codex/responses", Binaries: []string{"/usr/local/bin/node"},
		},
		Environment: []string{
			"FACTORY_MODEL_BASE_URL=https://chatgpt.com/backend-api/codex",
			"FACTORY_MODEL_ID=gpt-5.6-luna",
			"FACTORY_MODEL_API=openai-responses",
			"FACTORY_MODEL_KEY_ENV=BG_CHATGPT_TOKEN",
			`FACTORY_MODEL_HEADERS_JSON={"chatgpt-account-id":"BG_CHATGPT_ACCOUNT"}`,
		},
	}
	if !reflect.DeepEqual(access, want) {
		t.Fatalf("RouteAccess =\n%+v\nwant\n%+v", access, want)
	}
}

func TestRouteAccessForAnUncredentialedPlaintextRouteHasNoProvider(t *testing.T) {
	access, err := localModelPolicy().RouteAccess(t.TempDir(), piBinaries)
	if err != nil {
		t.Fatal(err)
	}
	want := RouteAccess{
		Endpoint: RouteEndpoint{
			Host: "100.64.0.1", Port: 8080, Method: "POST",
			Path: "/v1/chat/completions", PathIsPrefix: true, Binaries: []string{"/usr/local/bin/node"},
		},
		Environment: []string{
			"FACTORY_MODEL_BASE_URL=http://100.64.0.1:8080/v1",
			"FACTORY_MODEL_ID=qwen",
			"FACTORY_MODEL_API=openai-completions",
			`FACTORY_MODEL_EXTRA_JSON={"contextWindow":32000}`,
		},
	}
	if !reflect.DeepEqual(access, want) {
		t.Fatalf("RouteAccess =\n%+v\nwant\n%+v", access, want)
	}
}

func TestRouteAccessForAStaticKeyRoute(t *testing.T) {
	p := validRelayPolicy()
	p.Upstream = "https://openrouter.example:8443/api/"
	p.AllowedPathPrefix = "/v1/chat"
	p.WorkerModelID = "some-model"
	p.WorkerBasePath = "v1"
	dataDir := t.TempDir()
	access, err := p.RouteAccess(dataDir, piBinaries)
	if err != nil {
		t.Fatal(err)
	}
	if want := "bg-" + dataDirPrefix(t, dataDir) + "-test-route"; access.Provider != want {
		t.Errorf("Provider = %q, want %q", access.Provider, want)
	}
	if want := []string{"BG_MODEL_KEY"}; !reflect.DeepEqual(access.CredentialEnv, want) {
		t.Errorf("CredentialEnv = %v", access.CredentialEnv)
	}
	wantEndpoint := RouteEndpoint{Host: "openrouter.example", Port: 8443, TLS: true, Method: "POST", Path: "/api/v1/chat", PathIsPrefix: true, Binaries: piBinaries}
	if !reflect.DeepEqual(access.Endpoint, wantEndpoint) {
		t.Errorf("Endpoint = %+v, want %+v", access.Endpoint, wantEndpoint)
	}
	wantEnv := []string{
		"FACTORY_MODEL_BASE_URL=https://openrouter.example:8443/api/v1",
		"FACTORY_MODEL_ID=some-model",
		"FACTORY_MODEL_API=openai-completions",
		"FACTORY_MODEL_KEY_ENV=BG_MODEL_KEY",
	}
	if !reflect.DeepEqual(access.Environment, wantEnv) {
		t.Errorf("Environment = %v, want %v", access.Environment, wantEnv)
	}
}

func TestRouteAccessEnvironmentPassesLaunchSpecValidate(t *testing.T) {
	access, err := chatGPTPolicy().RouteAccess(t.TempDir(), piBinaries)
	if err != nil {
		t.Fatal(err)
	}
	spec := validSpec()
	spec.Environment = access.Environment
	if err := spec.Validate(); err != nil {
		t.Fatalf("Validate refused the route environment: %v", err)
	}
	for _, name := range access.CredentialEnv {
		if IsForbiddenCredentialEnvKey(name) {
			t.Errorf("%s is a forbidden credential key, so the provider could not supply it", name)
		}
	}
}

func TestRouteAccessRefuses(t *testing.T) {
	checks := []struct {
		name string
		edit func(*RoutePolicy, *[]string)
		want string
	}{
		{"an Anthropic-shaped worker", func(p *RoutePolicy, _ *[]string) {
			p.AuthMode, p.WorkerModelID, p.WorkerModelAPI = "", "", ""
		}, "no worker model id"},
		{"no harness binary", func(_ *RoutePolicy, b *[]string) { *b = nil }, "names no executable"},
		{"a policy Validate refuses", func(p *RoutePolicy, _ *[]string) { p.Route = "" }, "route is required"},
		{"a plaintext public upstream", func(p *RoutePolicy, _ *[]string) { p.Upstream = "http://models.example/v1" }, "https://"},
		{"a credential over plaintext", func(p *RoutePolicy, _ *[]string) {
			p.AuthMode = ""
			p.Upstream, p.AllowPlaintextUpstream = "http://127.0.0.1:8080", true
		}, "plaintext upstream"},
		{"extra JSON that is null", func(p *RoutePolicy, _ *[]string) { p.WorkerModelExtraJSON = "null" }, "got null"},
		{"extra JSON that is an array", func(p *RoutePolicy, _ *[]string) { p.WorkerModelExtraJSON = "[1]" }, "must be a JSON object"},
	}
	for _, check := range checks {
		t.Run(check.name, func(t *testing.T) {
			p, binaries := chatGPTPolicy(), append([]string(nil), piBinaries...)
			check.edit(&p, &binaries)
			_, err := p.RouteAccess(t.TempDir(), binaries)
			if err == nil || !strings.Contains(err.Error(), check.want) {
				t.Fatalf("err = %v, want one containing %q", err, check.want)
			}
		})
	}
}

func TestProviderNameSeparatesDataDirectories(t *testing.T) {
	a, errA := ProviderName(t.TempDir(), "chatgpt")
	b, errB := ProviderName(t.TempDir(), "chatgpt")
	if errA != nil || errB != nil || a == b {
		t.Fatalf("ProviderName = %q, %q (%v, %v); want two different names", a, b, errA, errB)
	}
	for _, route := range []string{"", "___", strings.Repeat("r", 60)} {
		if name, err := ProviderName(t.TempDir(), route); err == nil {
			t.Errorf("ProviderName(%q) = %q, want an error", route, name)
		}
	}
}

func TestRouteCredentialCarriesTheChatGPTTokenAndAccount(t *testing.T) {
	p := chatGPTPolicy()
	access, err := p.RouteAccess(t.TempDir(), piBinaries)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	expires := now.Add(2 * time.Hour)
	cred, err := p.RouteCredential(access, RouteSecret{}, RouteSecret{}, NewRouteSecret("tok-secret"), NewRouteSecret("acct-secret"), expires, now, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if cred.Provider != access.Provider || !cred.ExpiresAt.Equal(expires) {
		t.Errorf("cred = %v", cred)
	}
	if want := map[string]string{"BG_CHATGPT_TOKEN": "tok-secret", "BG_CHATGPT_ACCOUNT": "acct-secret"}; !reflect.DeepEqual(cred.Secrets(), want) {
		t.Errorf("Secrets() has the wrong names or values")
	}
}

func TestRouteCredentialNeverPrintsOrSerializesItsSecrets(t *testing.T) {
	p := chatGPTPolicy()
	access, _ := p.RouteAccess(t.TempDir(), piBinaries)
	cred, err := p.RouteCredential(access, RouteSecret{}, RouteSecret{}, NewRouteSecret("tok-secret"), NewRouteSecret("acct-secret"), time.Time{}, time.Now(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	for _, printed := range []string{fmt.Sprint(cred), fmt.Sprintf("%v %+v %#v %s", cred, cred, cred, cred), fmt.Sprintf("%+v", &cred), fmt.Sprintf("%v", []RouteCredential{cred})} {
		if strings.Contains(printed, "secret") {
			t.Errorf("printed form leaks a secret: %q", printed)
		}
	}
	if out, err := json.Marshal(cred); err == nil {
		t.Errorf("json.Marshal succeeded: %s", out)
	}
	if out, err := json.Marshal(struct{ C RouteCredential }{cred}); err == nil {
		t.Errorf("json.Marshal of a struct holding one succeeded: %s", out)
	}
}

func TestRouteCredentialRefuses(t *testing.T) {
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	token, account := NewRouteSecret("tok"), NewRouteSecret("acct")
	p := chatGPTPolicy()
	access, err := p.RouteAccess(t.TempDir(), piBinaries)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.RouteCredential(access, RouteSecret{}, RouteSecret{}, token, account, now.Add(59*time.Minute), now, time.Hour); err == nil || !strings.Contains(err.Error(), "expires at 2026-10-04T12:59:00Z") {
		t.Errorf("a credential that expires inside the step: err = %v", err)
	}
	if _, err := p.RouteCredential(access, RouteSecret{}, RouteSecret{}, token, RouteSecret{}, time.Time{}, now, time.Hour); err == nil || !strings.Contains(err.Error(), "BG_CHATGPT_ACCOUNT") {
		t.Errorf("a missing account id: err = %v", err)
	}
	if _, err := p.RouteCredential(access, NewRouteSecret("key"), RouteSecret{}, RouteSecret{}, RouteSecret{}, time.Time{}, now, time.Hour); err == nil {
		t.Error("an API key for a chatgpt route was accepted")
	}
	local := localModelPolicy()
	localAccess, err := local.RouteAccess(t.TempDir(), piBinaries)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := local.RouteCredential(localAccess, RouteSecret{}, RouteSecret{}, RouteSecret{}, RouteSecret{}, time.Time{}, now, time.Hour); err == nil || !strings.Contains(err.Error(), "no provider") {
		t.Errorf("a route with no provider: err = %v", err)
	}
}

func TestRouteCredentialForAStaticKey(t *testing.T) {
	p := validRelayPolicy()
	p.WorkerModelID = "m"
	access, err := p.RouteAccess(t.TempDir(), piBinaries)
	if err != nil {
		t.Fatal(err)
	}
	cred, err := p.RouteCredential(access, NewRouteSecret("key-secret"), RouteSecret{}, RouteSecret{}, RouteSecret{}, time.Time{}, time.Now(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]string{"BG_MODEL_KEY": "key-secret"}; !reflect.DeepEqual(cred.Secrets(), want) {
		t.Error("Secrets() has the wrong names or values")
	}
}

// copilotPolicy is a github-copilot route as modelrole resolves it for a
// chat-completions model.
func copilotPolicy() RoutePolicy {
	p := validRelayPolicy()
	p.AuthMode = meter.CredentialModeGitHubCopilot
	p.Billing = "subscription"
	p.Upstream = meter.CopilotAPIBase
	p.AllowedPathPrefix = meter.CopilotAllowedPathPrefixFor(meter.RequestFormatOpenAICompletions)
	p.WorkerBasePath = ""
	p.WorkerModelID = "gpt-5-mini"
	p.WorkerModelAPI = meter.RequestFormatOpenAICompletions
	return p
}

// TestCopilotRouteIsServedWithTheLoginTokenAndTheMeterRewrite: a
// github-copilot route launches like any credentialed route. Its key is the
// operator's GitHub login token, with no expiry to outlive, and its meter
// config turns on the Copilot rewrite.
func TestCopilotRouteIsServedWithTheLoginTokenAndTheMeterRewrite(t *testing.T) {
	p := copilotPolicy()
	dataDir := t.TempDir()
	access, err := p.RouteAccess(dataDir, piBinaries)
	if err != nil {
		t.Fatal(err)
	}
	wantEndpoint := RouteEndpoint{Host: "api.individual.githubcopilot.com", Port: 443, TLS: true, Method: "POST", Path: "/chat/completions", Binaries: piBinaries}
	if !reflect.DeepEqual(access.Endpoint, wantEndpoint) {
		t.Errorf("Endpoint = %+v, want %+v", access.Endpoint, wantEndpoint)
	}
	wantEnv := []string{
		"FACTORY_MODEL_BASE_URL=https://api.individual.githubcopilot.com",
		"FACTORY_MODEL_ID=gpt-5-mini",
		"FACTORY_MODEL_API=openai-completions",
		"FACTORY_MODEL_KEY_ENV=BG_MODEL_KEY",
	}
	if !reflect.DeepEqual(access.Environment, wantEnv) {
		t.Errorf("Environment = %v, want %v", access.Environment, wantEnv)
	}

	cred, err := p.RouteCredential(access, NewRouteSecret("api-key-secret"), NewRouteSecret("login-secret"), RouteSecret{}, RouteSecret{}, time.Time{}, time.Now(), 6*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]string{"BG_MODEL_KEY": "login-secret"}; !reflect.DeepEqual(cred.Secrets(), want) {
		t.Error("Secrets() is not the GitHub login token under BG_MODEL_KEY")
	}
	if _, err := p.RouteCredential(access, NewRouteSecret("api-key-secret"), RouteSecret{}, RouteSecret{}, RouteSecret{}, time.Time{}, time.Now(), time.Hour); err == nil || !strings.Contains(err.Error(), "BG_MODEL_KEY") {
		t.Errorf("no login token: err = %v, want a refusal naming BG_MODEL_KEY", err)
	}

	config, err := p.MeterConfig(dataDir, "run-1", "bg-sandbox-1")
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := structpb.NewStruct(config)
	if err != nil {
		t.Fatal(err)
	}
	policy, err := meter.DecodePolicy(encoded)
	if err != nil {
		t.Fatalf("the meter refuses the config: %v", err)
	}
	if policy.Rewrite != meter.RewriteGitHubCopilot || policy.Route != meter.CredentialModeGitHubCopilot {
		t.Errorf("policy rewrite = %q, route = %q, want both github-copilot", policy.Rewrite, policy.Route)
	}
}

func TestMeterConfigDecodesToTheRoutesMeterPolicy(t *testing.T) {
	dataDir := t.TempDir()
	p := chatGPTPolicy()
	config, err := p.MeterConfig(dataDir, "run-1", "bg-sandbox-1")
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := structpb.NewStruct(config)
	if err != nil {
		t.Fatalf("the config is not a protobuf Struct: %v", err)
	}
	got, err := meter.DecodePolicy(encoded)
	if err != nil {
		t.Fatalf("the meter refuses the config: %v", err)
	}
	runName, err := MeterRunName(dataDir, "run-1")
	if err != nil {
		t.Fatal(err)
	}
	want := meter.Policy{
		Run: runName, Sandbox: "bg-sandbox-1", Route: "chatgpt-codex",
		UsageFormat: "openai-responses", RequestFormat: "openai-responses",
		TokenCeiling: 50000, CostCeilingMicroUSD: 500000,
		Prices: meter.Prices{
			InputMicroUSDPerMTok: 3000000, CachedInputMicroUSDPerMTok: 300000,
			CacheWriteMicroUSDPerMTok: 3750000, OutputMicroUSDPerMTok: 15000000,
		},
		TokenBudget: 10000, TokenWindowSeconds: 60,
		CostBudgetMicroUSD: 100000, CostWindowSeconds: 60,
		RequestsPerMinute: 60, MaxRequestBytes: 1 << 20,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("decoded policy =\n%+v\nwant\n%+v", got, want)
	}
}

func TestMeterRunNameIsUniquePerRunAndDataDirectory(t *testing.T) {
	dataDir := t.TempDir()
	name, err := MeterRunName(dataDir, "run-1")
	if err != nil {
		t.Fatal(err)
	}
	// sha256("run-1") begins 4e65d3fb (shasum).
	if want := dataDirPrefix(t, dataDir) + "-run-1-4e65d3fb"; name != want {
		t.Errorf("MeterRunName = %q, want %q", name, want)
	}
	seen := map[string]string{}
	for _, id := range []string{"run-1", "Run-1", "run_1", "run.1", strings.Repeat("x", 80) + "a", strings.Repeat("x", 80) + "b", "___"} {
		got, err := MeterRunName(dataDir, id)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) > 63 || !sandboxNamePattern.MatchString(got) {
			t.Errorf("MeterRunName(%q) = %q is not a meter run name", id, got)
		}
		if other, dup := seen[got]; dup {
			t.Errorf("run ids %q and %q share the name %q", other, id, got)
		}
		seen[got] = id
	}
	if other, _ := MeterRunName(t.TempDir(), "run-1"); other == name {
		t.Error("two data directories share a run name")
	}
	if _, err := MeterRunName(dataDir, ""); err == nil {
		t.Error("an empty run id was accepted")
	}
}
