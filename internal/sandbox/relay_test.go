package sandbox

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"buildgate/internal/meter"
	"buildgate/internal/run"
)

func validRelaySpec() RouteSpec {
	// Matches validSpec()'s own RunID/DataDir.
	return validRelayPolicy().Spec(NewRouteSecret("relay-secret-for-test"), RouteSecret{}, RouteSecret{}, RouteSecret{}, "run1", "/tmp/data")
}

func validRelayPolicy() RoutePolicy {
	return RoutePolicy{
		Upstream:                   "https://models.example/v1",
		Route:                      "test-route",
		MaxRequestBytes:            1 << 20,
		RequestsPerMinute:          60,
		TokenBudget:                10000,
		TokenBudgetWindow:          time.Minute,
		CostBudgetMicroUSD:         100000,
		CostBudgetWindow:           time.Minute,
		InputMicroUSDPerMTok:       3000000,
		CachedInputMicroUSDPerMTok: 300000,
		CacheWriteMicroUSDPerMTok:  3750000,
		OutputMicroUSDPerMTok:      15000000,
		TokenCeiling:               50000,
		CostCeilingMicroUSD:        500000,
	}
}

// TestRelayPolicyValidateRequiresRoute proves routes:/models:/roles: is
// the only session-config schema at the RoutePolicy.Validate layer too:
// a policy with no Route at all is refused, never silently accepted as
// some other, routeless mode.
func TestRelayPolicyValidateRequiresRoute(t *testing.T) {
	policy := validRelayPolicy()
	policy.Route = ""
	if err := policy.Validate(); err == nil || !strings.Contains(err.Error(), "relay route is required") {
		t.Fatalf("Validate() with no route = %v, want a refusal naming the missing route", err)
	}
}

func TestRelaySpecRejectsUnsafeConfiguration(t *testing.T) {
	checks := []struct {
		name string
		edit func(*RouteSpec)
	}{
		{"missing upstream", func(s *RouteSpec) { s.Upstream = "" }},
		{"relative upstream", func(s *RouteSpec) { s.Upstream = "/v1" }},
		{"non-http upstream", func(s *RouteSpec) { s.Upstream = "ftp://models.example/v1" }},
		{"upstream credentials", func(s *RouteSpec) { s.Upstream = "https://user:password@models.example/v1" }},
		{"upstream query", func(s *RouteSpec) { s.Upstream = "https://models.example/v1?key=secret" }},
		{"API key newline", func(s *RouteSpec) { s.APIKey = NewRouteSecret("secret\nvalue") }},
		{"zero request size", func(s *RouteSpec) { s.MaxRequestBytes = 0 }},
		{"zero request rate", func(s *RouteSpec) { s.RequestsPerMinute = 0 }},
		{"zero token budget", func(s *RouteSpec) { s.TokenBudget = 0 }},
		{"zero token window", func(s *RouteSpec) { s.TokenBudgetWindow = 0 }},
		{"zero cost budget", func(s *RouteSpec) { s.CostBudgetMicroUSD = 0 }},
		{"zero cost window", func(s *RouteSpec) { s.CostBudgetWindow = 0 }},
		{"zero input price", func(s *RouteSpec) { s.InputMicroUSDPerMTok = 0 }},
		{"negative output price", func(s *RouteSpec) { s.OutputMicroUSDPerMTok = -1 }},
		{"negative cached-input price", func(s *RouteSpec) { s.CachedInputMicroUSDPerMTok = -1 }},
		{"cached-input price exceeds input price", func(s *RouteSpec) { s.CachedInputMicroUSDPerMTok = s.InputMicroUSDPerMTok + 1 }},
		{"negative cache-write price", func(s *RouteSpec) { s.CacheWriteMicroUSDPerMTok = -1 }},
		{"invalid credential header", func(s *RouteSpec) { s.UpstreamAuthHeader = "X-Custom-Header" }},
		{"slash-containing worker model id", func(s *RouteSpec) { s.WorkerModelID = "openrouter/qwen" }},
		{"control character in worker model id", func(s *RouteSpec) { s.WorkerModelID = "bad\tid" }},
		{"anthropic usage format alongside a worker model id", func(s *RouteSpec) {
			s.WorkerModelID = "qwen"
			s.UsageFormat = meter.UsageFormatAnthropic
		}},
		{"responses worker API without a worker model id", func(s *RouteSpec) {
			s.WorkerModelAPI = meter.RequestFormatOpenAIResponses
		}},
		{"run id newline", func(s *RouteSpec) { s.RunID = "run\n123" }},
		{"missing data dir", func(s *RouteSpec) { s.DataDir = "" }},
		{"relative data dir", func(s *RouteSpec) { s.DataDir = "relative/data" }},
		{"egress CA bundle does not exist", func(s *RouteSpec) { s.CABundlePath = "/nonexistent/corp-ca.pem" }},
	}
	for _, tc := range checks {
		t.Run(tc.name, func(t *testing.T) {
			s := validRelaySpec()
			tc.edit(&s)
			if err := s.Validate(); err == nil {
				t.Fatal("Validate unexpectedly succeeded")
			}
		})
	}
}

// TestRelaySpecAcceptsMissingAPIKey guards the deliberate relaxation added
// for a local/LAN model upstream with no real credential to inject at all
// (see cmd/factoryd's -relay-allow-plaintext-upstream): an empty
// RouteSecret must not be rejected the way it used to be, only a
// present-but-malformed one (a control character) still is.
func TestRelaySpecAcceptsMissingAPIKey(t *testing.T) {
	s := validRelaySpec()
	s.APIKey = RouteSecret{}
	if err := s.Validate(); err != nil {
		t.Fatalf("Validate() with an empty API key = %v, want nil", err)
	}
}

func TestRelaySpecAcceptsHTTPAndHTTPSDigestPinnedConfiguration(t *testing.T) {
	for _, upstream := range []string{"http://127.0.0.1:8080/model", "https://models.example/v1"} {
		s := validRelaySpec()
		s.Upstream = upstream
		if err := s.Validate(); err != nil {
			t.Errorf("Validate(%q) = %v, want nil", upstream, err)
		}
	}
}

// TestValidateUpstreamScheme guards the AllowPlaintextUpstream exemption
// (migrated from cmd/factoryd's own former validateRelayUpstreamScheme,
// found via review, Codex, PR #59, to be a shared RoutePolicy method both
// the CLI and Temporal paths call identically): a local/LAN model endpoint
// with no real credential can use a plaintext upstream, but the exemption
// stays narrow (private/loopback hosts only, never a public one, regardless
// of the field) so it cannot silently become a general plaintext hole for a
// real credential.
func TestValidateUpstreamScheme(t *testing.T) {
	cases := []struct {
		name       string
		upstream   string
		allowPlain bool
		wantErr    bool
	}{
		{name: "public https always allowed", upstream: "https://api.anthropic.com", allowPlain: false, wantErr: false},
		{name: "public https allowed even with field set", upstream: "https://api.anthropic.com", allowPlain: true, wantErr: false},
		{name: "public http rejected without field", upstream: "http://api.anthropic.com", allowPlain: false, wantErr: true},
		{name: "public http rejected even with field", upstream: "http://api.anthropic.com", allowPlain: true, wantErr: true},
		{name: "loopback http rejected without field", upstream: "http://127.0.0.1:8080", allowPlain: false, wantErr: true},
		{name: "loopback http allowed with field", upstream: "http://127.0.0.1:8080", allowPlain: true, wantErr: false},
		{name: "localhost http allowed with field", upstream: "http://localhost:8080", allowPlain: true, wantErr: false},
		{name: "private LAN http allowed with field", upstream: "http://192.168.1.50:8080", allowPlain: true, wantErr: false},
		{name: "private LAN http rejected without field", upstream: "http://192.168.1.50:8080", allowPlain: false, wantErr: true},
		// Tailscale's tailnet range (100.64.0.0/10, RFC 6598 CGNAT) is not
		// RFC1918 and net.IP.IsPrivate() does not recognize it -- found live,
		// 2026-09-07: a sandboxed worker's own container network could not
		// route to a model endpoint's physical LAN address at all (bare
		// connection refused) but reached the identical endpoint fine over
		// its Tailscale address, which is also already WireGuard-encrypted
		// end-to-end between the two tailnet identities.
		{name: "tailscale CGNAT http allowed with field", upstream: "http://100.101.1.2:8080", allowPlain: true, wantErr: false},
		{name: "tailscale CGNAT http rejected without field", upstream: "http://100.101.1.2:8080", allowPlain: false, wantErr: true},
		{name: "CGNAT lookalike just outside the tailscale /10 rejected", upstream: "http://100.128.0.1:8080", allowPlain: true, wantErr: true},
		{name: "invalid scheme rejected", upstream: "ftp://127.0.0.1:8080", allowPlain: true, wantErr: true},
		{name: "unparseable URL rejected", upstream: "http://[::1", allowPlain: true, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			policy := RoutePolicy{Upstream: tc.upstream, AllowPlaintextUpstream: tc.allowPlain}
			err := policy.ValidateUpstreamScheme()
			if tc.wantErr && err == nil {
				t.Errorf("ValidateUpstreamScheme(%q, %v) = nil, want error", tc.upstream, tc.allowPlain)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("ValidateUpstreamScheme(%q, %v) = %v, want nil", tc.upstream, tc.allowPlain, err)
			}
		})
	}
}

// TestRelaySpecRejectsRealCredentialOverPlaintext guards the credential-leak
// fix (found via review, Codex, PR #59): AllowPlaintextUpstream is
// documented as an exemption for an endpoint with no real credential to
// protect, but nothing enforced that before this check existed -- an
// operator with a real credential still configured (e.g. ANTHROPIC_API_KEY
// left set for an unrelated reason) who opts into plaintext for a private
// local model would otherwise have that real key injected into every
// plaintext request.
func TestRelaySpecRejectsRealCredentialOverPlaintext(t *testing.T) {
	s := validRelaySpec()
	s.AllowPlaintextUpstream = true
	s.Upstream = "http://127.0.0.1:8080/v1"
	if err := s.Validate(); err == nil {
		t.Fatal("Validate() accepted a real credential alongside AllowPlaintextUpstream targeting a plaintext upstream")
	}

	// The same upstream with no credential configured remains fine -- this
	// is exactly the case AllowPlaintextUpstream exists for.
	s.APIKey = RouteSecret{}
	if err := s.Validate(); err != nil {
		t.Fatalf("Validate() with no credential = %v, want nil", err)
	}
}

// TestRelaySpecRejectsGitHubTokenOverPlaintext is
// TestRelaySpecRejectsRealCredentialOverPlaintext's github-copilot-mode
// counterpart (found via adversarial review): CredentialModeGitHubCopilot
// carries its real credential in GitHubToken, never APIKey, so a check
// that only inspected APIKey let a github-copilot relay configured with
// AllowPlaintextUpstream send the GitHub OAuth token and the exchanged
// Copilot bearer token over plaintext HTTP.
func TestRelaySpecRejectsGitHubTokenOverPlaintext(t *testing.T) {
	s := validRelaySpec()
	s.APIKey = RouteSecret{}
	s.GitHubToken = NewRouteSecret("gho_test_token")
	s.AllowPlaintextUpstream = true
	s.Upstream = "http://127.0.0.1:8080"
	if err := s.Validate(); err == nil {
		t.Fatal("Validate() accepted a GitHub OAuth token alongside AllowPlaintextUpstream targeting a plaintext upstream")
	}

	// The same upstream with no credential of either kind remains fine.
	s.GitHubToken = RouteSecret{}
	if err := s.Validate(); err != nil {
		t.Fatalf("Validate() with no credential = %v, want nil", err)
	}
}

func TestValidateGitHubCopilotWorkerAPI(t *testing.T) {
	for _, tc := range []struct {
		name                         string
		workerAPI, allowedPathPrefix string
		wantErr                      bool
	}{
		{"empty prefix defaults to completions, matches", "", meter.CopilotChatCompletionsPath, false},
		{"empty prefix under responses API", meter.RequestFormatOpenAIResponses, meter.CopilotResponsesPath, false},
		{"exact chat/completions match", meter.RequestFormatOpenAICompletions, meter.CopilotChatCompletionsPath, false},
		{"trailing slash on responses path matches", meter.RequestFormatOpenAIResponses, meter.CopilotResponsesPath + "/", false},
		{"root prefix allows every path", meter.RequestFormatOpenAIResponses, "/", false},
		{"empty prefix allows every path", meter.RequestFormatOpenAIResponses, "", false},
		{"stale chat/completions prefix under responses API", meter.RequestFormatOpenAIResponses, meter.CopilotChatCompletionsPath, true},
		{"stale responses prefix under completions API", meter.RequestFormatOpenAICompletions, meter.CopilotResponsesPath, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateGitHubCopilotWorkerAPI(tc.workerAPI, tc.allowedPathPrefix)
			if (err != nil) != tc.wantErr {
				t.Fatalf("ValidateGitHubCopilotWorkerAPI(%q, %q) = %v, want error: %v", tc.workerAPI, tc.allowedPathPrefix, err, tc.wantErr)
			}
			if err != nil {
				for _, want := range []string{"models.<model>.api", "routes.<route>.allowed_path_prefix"} {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("err = %v, want it to name %q", err, want)
					}
				}
			}
		})
	}
}

// TestRelayPolicyValidateRefusesStaleGitHubCopilotAllowedPathPrefix proves
// RoutePolicy.Validate (the launch-time check `factoryd run` always goes
// through, with or without a doctor preflight) refuses the same stale
// relay_allowed_path_prefix / relay_worker_api combination doctor's own
// route-config check refuses, so a real run can never launch a relay
// silently misrouted onto the wrong Copilot path.
func TestRelayPolicyValidateRefusesStaleGitHubCopilotAllowedPathPrefix(t *testing.T) {
	p := validRelayPolicy()
	p.AuthMode = meter.CredentialModeGitHubCopilot
	p.Upstream = meter.CopilotAPIBase // github-copilot mode pins the upstream host
	p.WorkerModelID = "gpt-5.6-luna"
	p.WorkerModelAPI = meter.RequestFormatOpenAIResponses
	p.AllowedPathPrefix = meter.CopilotChatCompletionsPath
	if err := p.Validate(); err == nil {
		t.Fatal("Validate() with relay_worker_api openai-responses and a stale /chat/completions prefix = nil, want an error")
	}

	// The matching prefix (or leaving it unset) passes.
	p.AllowedPathPrefix = meter.CopilotResponsesPath
	if err := p.Validate(); err != nil {
		t.Fatalf("Validate() with a matching /responses prefix = %v, want nil", err)
	}
	p.AllowedPathPrefix = ""
	if err := p.Validate(); err != nil {
		t.Fatalf("Validate() with no relay_allowed_path_prefix set = %v, want nil", err)
	}
}

func relayCallOperation(call []string) string {
	if len(call) == 0 {
		return ""
	}
	if call[0] == "run" {
		return "run"
	}
	if call[0] == "rm" {
		return "rm"
	}
	if call[0] == "ps" {
		return "ps"
	}
	if call[0] == "inspect" {
		return "inspect"
	}
	if call[0] == "logs" {
		return "logs"
	}
	if call[0] == "network" && len(call) > 1 {
		return "network " + call[1]
	}
	return strings.Join(call, " ")
}

func TestReconcileRelayOrphansRemovesTerminalRunsAndOrphanedNetworks(t *testing.T) {
	dataDir := t.TempDir()
	for _, r := range []run.Run{
		{ID: "run-terminal", State: run.StateHalted, HaltConfirmed: true},
		{ID: "run-active", State: run.StateSliceRunning},
		{ID: "run-network-only", State: run.StateAccepted},
		{ID: "run-in-flight", State: run.StateSliceRunning},
	} {
		r := r
		if err := r.Save(dataDir); err != nil {
			t.Fatalf("save run %s: %v", r.ID, err)
		}
	}

	docker := filepath.Join(t.TempDir(), "docker-fake")
	script := `#!/bin/sh
case "$*" in
  *"ps -a --filter label=buildgate.relay=true --filter label=buildgate.data-dir="*)
    printf 'factoryd-relay-container-terminal\trun-terminal\n'
    printf 'factoryd-relay-container-active\trun-active\n'
    ;;
  *"ps -a --filter name=^/"*)
    ;;
  *"network ls --filter label=buildgate.relay=true --filter label=buildgate.data-dir="*)
    printf 'factoryd-relay-terminal\trun-terminal\n'
    printf 'factoryd-relay-network-only\trun-network-only\n'
    printf 'factoryd-relay-in-flight\trun-in-flight\n'
    ;;
  *"network ls --filter name=^"*)
    ;;
esac
`
	if err := os.WriteFile(docker, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}

	removed, err := ReconcileRelayOrphans(context.Background(), docker, dataDir)
	if err != nil {
		t.Fatalf("ReconcileRelayOrphans: %v", err)
	}
	want := map[string]bool{
		"factoryd-relay-container-terminal": true,
		"factoryd-relay-terminal":           true,
		"factoryd-relay-network-only":       true,
	}
	if len(removed) != len(want) {
		t.Fatalf("removed = %v, want exactly %v", removed, want)
	}
	for _, name := range removed {
		if !want[name] {
			t.Errorf("unexpectedly removed %q", name)
		}
		if name == "factoryd-relay-in-flight" {
			t.Errorf("removed a network for a still-in-flight run with no stale heartbeat -- this is the race a live launch could lose")
		}
	}
	for name := range want {
		if !strings.Contains(strings.Join(removed, ","), name) {
			t.Errorf("expected %q to be removed; removed = %v", name, removed)
		}
	}
}

// TestReconcileRelayOrphansQuarantinesRunWhenRelayIsTheOnlyOrphan is the
// regression for a codex finding on PR #42: a stale-but-non-terminal run
// whose worker container had already exited and been removed by its own
// --rm left ReconcileOrphans (the worker-side reconciler) nothing to find
// or quarantine, so ReconcileRelayOrphans -- reconciling the still-detached
// relay for the same run -- was the only reconciliation pass that would
// ever see this run again. It must quarantine the run itself, not just
// reclaim the relay's container and network, or the durable record stays
// permanently non-terminal.
func TestReconcileRelayOrphansQuarantinesRunWhenRelayIsTheOnlyOrphan(t *testing.T) {
	previousStale := ownerStaleAfter
	ownerStaleAfter = 0
	t.Cleanup(func() { ownerStaleAfter = previousStale })
	previousInterval := ownerHeartbeatInterval
	ownerHeartbeatInterval = time.Millisecond
	t.Cleanup(func() { ownerHeartbeatInterval = previousInterval })

	dataDir := t.TempDir()
	r := run.Run{ID: "run-relay-only-orphan", State: run.StateSliceRunning}
	if err := r.Save(dataDir); err != nil {
		t.Fatalf("save run: %v", err)
	}
	if err := writeOwnerHeartbeat(dataDir, r.ID); err != nil {
		t.Fatalf("writeOwnerHeartbeat: %v", err)
	}

	docker := filepath.Join(t.TempDir(), "docker-fake")
	// The worker container is already gone (its own --rm already ran): the
	// "ps -a --filter label=buildgate.relay=true" listing is the
	// only place this run's relay container shows up at all, and every
	// removal-confirmation query reports nothing left.
	script := `#!/bin/sh
case "$*" in
  *"ps -a --filter label=buildgate.relay=true --filter label=buildgate.data-dir="*)
    printf 'factoryd-relay-container-only-orphan\trun-relay-only-orphan\n'
    ;;
  *"ps -a --filter name=^/"*) ;;
  *"network ls --filter name=^"*) ;;
  *"network ls --filter label=buildgate.relay=true --filter label=buildgate.data-dir="*) ;;
  *"rm -f"*) exit 0 ;;
  *"network rm"*) exit 0 ;;
esac
`
	if err := os.WriteFile(docker, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}

	removed, err := ReconcileRelayOrphans(context.Background(), docker, dataDir)
	if err != nil {
		t.Fatalf("ReconcileRelayOrphans: %v", err)
	}
	want := map[string]bool{
		"factoryd-relay-container-only-orphan": true,
		"factoryd-relay-only-orphan":           true,
	}
	if len(removed) != len(want) {
		t.Fatalf("removed = %v, want exactly %v", removed, want)
	}
	record, err := run.Load(dataDir, r.ID)
	if err != nil {
		t.Fatalf("run.Load after reconciliation: %v", err)
	}
	if record.State != run.StateQuarantined || !record.HaltConfirmed {
		t.Errorf("run state = %q, HaltConfirmed = %v, want quarantined and confirmed -- the relay was the only orphan reconciliation ever saw for this run", record.State, record.HaltConfirmed)
	}
}

// TestReconcileRelayOrphansRemovesSharedNetworkProxyBeforeItsRelay is the
// regression for a Codex-review finding (PRs #143-#148 follow-up): a
// terminal run whose relay and registry-proxy containers share one network
// (RegistryProxySpec.ExistingInternalNetwork) must have the shared-network
// proxy removed before its relay, or the relay's own `network rm` fails
// while the proxy is still attached, and the surviving `present[name]` entry
// for the relay's container (recorded at listing time regardless of later
// removal) then wrongly suppresses the orphan-network retry pass below,
// leaking the network. The fake docker script here models real Docker's own
// refusal to remove a network with an attached container still on it, via a
// stateful marker file cleared only when the proxy container is actually
// removed first.
func TestReconcileRelayOrphansRemovesSharedNetworkProxyBeforeItsRelay(t *testing.T) {
	dataDir := t.TempDir()
	r := run.Run{ID: "run-shared", State: run.StateAccepted}
	if err := r.Save(dataDir); err != nil {
		t.Fatalf("save run: %v", err)
	}

	mark := t.TempDir()
	for _, f := range []string{"relay_container", "proxy_container", "network"} {
		if err := os.WriteFile(filepath.Join(mark, f), nil, 0o600); err != nil {
			t.Fatalf("seed marker %s: %v", f, err)
		}
	}

	docker := filepath.Join(t.TempDir(), "docker-fake")
	script := `#!/bin/sh
MARK="` + mark + `"
case "$*" in
  *"ps -a --filter label=buildgate.relay=true --filter label=buildgate.data-dir="*)
    printf 'factoryd-relay-container-shared\trun-shared\n'
    ;;
  *"ps -a --filter label=buildgate.registryproxy=true --filter label=buildgate.data-dir="*)
    printf 'factoryd-registryproxy-container-shared\trun-shared\ttrue\n'
    ;;
  *"ps -a --filter name=^/factoryd-relay-container-shared"*)
    if [ -f "$MARK/relay_container" ]; then printf 'factoryd-relay-container-shared\n'; fi
    ;;
  *"ps -a --filter name=^/factoryd-registryproxy-container-shared"*)
    if [ -f "$MARK/proxy_container" ]; then printf 'factoryd-registryproxy-container-shared\n'; fi
    ;;
  *"rm -f factoryd-relay-container-shared"*)
    rm -f "$MARK/relay_container"
    ;;
  *"rm -f factoryd-registryproxy-container-shared"*)
    rm -f "$MARK/proxy_container"
    ;;
  *"network rm factoryd-relay-shared"*)
    if [ -f "$MARK/proxy_container" ]; then
      exit 1
    fi
    rm -f "$MARK/network"
    ;;
  *"network ls --filter name=^factoryd-relay-shared"*)
    if [ -f "$MARK/network" ]; then printf 'factoryd-relay-shared\n'; fi
    ;;
  *"network ls --filter label=buildgate.relay=true --filter label=buildgate.data-dir="*)
    if [ -f "$MARK/network" ]; then printf 'factoryd-relay-shared\trun-shared\n'; fi
    ;;
  *"network ls --filter label=buildgate.registryproxy=true --filter label=buildgate.data-dir="*)
    ;;
esac
`
	if err := os.WriteFile(docker, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}

	removed, err := ReconcileRelayOrphans(context.Background(), docker, dataDir)
	if err != nil {
		t.Fatalf("ReconcileRelayOrphans: %v (network cleanup should have succeeded once the shared-network proxy was removed first)", err)
	}
	want := map[string]bool{
		"factoryd-relay-container-shared":         true,
		"factoryd-registryproxy-container-shared": true,
		"factoryd-relay-shared":                   true,
	}
	if len(removed) != len(want) {
		t.Fatalf("removed = %v, want exactly %v -- a leaked network here means the shared-network ordering regressed", removed, want)
	}
	for _, name := range removed {
		if !want[name] {
			t.Errorf("unexpectedly removed %q", name)
		}
	}
	for name := range want {
		if !strings.Contains(strings.Join(removed, ","), name) {
			t.Errorf("expected %q to be removed; removed = %v", name, removed)
		}
	}
	if _, networkStillPresent := os.Stat(filepath.Join(mark, "network")); networkStillPresent == nil {
		t.Errorf("relay network marker still present on disk after reconciliation -- the network was not actually removed")
	}
}

// TestReconcileRelayOrphansLeavesRunNonTerminalWhileWorkerContainerRemains
// is the regression for a second codex finding on the fix above: reclaiming
// a stale run's relay must not quarantine (HaltConfirmed=true, "nothing
// left that could still be running") the run while a worker container for
// that same run is still present -- whether ReconcileOrphans hasn't
// reconciled it yet this pass, or its own removal attempt just failed. Only
// the relay is reclaimed here; the run stays non-terminal for a later
// ReconcileOrphans pass to finish.
func TestReconcileRelayOrphansLeavesRunNonTerminalWhileWorkerContainerRemains(t *testing.T) {
	previousStale := ownerStaleAfter
	ownerStaleAfter = 0
	t.Cleanup(func() { ownerStaleAfter = previousStale })
	previousInterval := ownerHeartbeatInterval
	ownerHeartbeatInterval = time.Millisecond
	t.Cleanup(func() { ownerHeartbeatInterval = previousInterval })

	dataDir := t.TempDir()
	r := run.Run{ID: "run-worker-still-present", State: run.StateSliceRunning}
	if err := r.Save(dataDir); err != nil {
		t.Fatalf("save run: %v", err)
	}
	if err := writeOwnerHeartbeat(dataDir, r.ID); err != nil {
		t.Fatalf("writeOwnerHeartbeat: %v", err)
	}

	docker := filepath.Join(t.TempDir(), "docker-fake")
	script := `#!/bin/sh
case "$*" in
  *"ps -a --filter label=buildgate.relay=true --filter label=buildgate.data-dir="*)
    printf 'factoryd-relay-container-worker-present\trun-worker-still-present\n'
    ;;
  *"ps -a --filter label=buildgate.data-dir="*"--filter label=buildgate.run="*)
    printf 'factoryd-worker-still-running\n'
    ;;
  *"ps -a --filter name=^/"*) ;;
  *"network ls --filter name=^"*) ;;
  *"network ls --filter label=buildgate.relay=true --filter label=buildgate.data-dir="*) ;;
  *"rm -f"*) exit 0 ;;
  *"network rm"*) exit 0 ;;
esac
`
	if err := os.WriteFile(docker, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}

	removed, err := ReconcileRelayOrphans(context.Background(), docker, dataDir)
	if err != nil {
		t.Fatalf("ReconcileRelayOrphans: %v", err)
	}
	want := map[string]bool{
		"factoryd-relay-container-worker-present": true,
		"factoryd-relay-worker-present":           true,
	}
	if len(removed) != len(want) {
		t.Fatalf("removed = %v, want exactly %v", removed, want)
	}
	record, err := run.Load(dataDir, r.ID)
	if err != nil {
		t.Fatalf("run.Load after reconciliation: %v", err)
	}
	if record.State == run.StateQuarantined || record.HaltConfirmed {
		t.Errorf("run state = %q, HaltConfirmed = %v, want still non-terminal and unconfirmed -- its worker container is still present", record.State, record.HaltConfirmed)
	}
}

// TestReconcileRelayOrphansQuarantinesNetworkOnlyOrphan is the regression
// for a third codex finding on the fixes above: a factoryd crash between
// a launch's network create and its paired container starting leaves a
// stale non-terminal run with only that network as evidence -- no relay
// container, and (per the scenario itself) no worker container either, ever
// existed for ReconcileOrphans or this function's own container-listing
// pass to find. Removing the orphaned network without also quarantining the
// run here would leave its durable record permanently non-terminal with
// nothing left for any later scan to rediscover.
func TestReconcileRelayOrphansQuarantinesNetworkOnlyOrphan(t *testing.T) {
	previousStale := ownerStaleAfter
	ownerStaleAfter = 0
	t.Cleanup(func() { ownerStaleAfter = previousStale })
	previousInterval := ownerHeartbeatInterval
	ownerHeartbeatInterval = time.Millisecond
	t.Cleanup(func() { ownerHeartbeatInterval = previousInterval })

	dataDir := t.TempDir()
	r := run.Run{ID: "run-network-only-orphan", State: run.StateSliceRunning}
	if err := r.Save(dataDir); err != nil {
		t.Fatalf("save run: %v", err)
	}
	if err := writeOwnerHeartbeat(dataDir, r.ID); err != nil {
		t.Fatalf("writeOwnerHeartbeat: %v", err)
	}

	docker := filepath.Join(t.TempDir(), "docker-fake")
	script := `#!/bin/sh
case "$*" in
  *"ps -a --filter label=buildgate.relay=true --filter label=buildgate.data-dir="*) ;;
  *"ps -a --filter label=buildgate.data-dir="*"--filter label=buildgate.run="*) ;;
  *"network ls --filter label=buildgate.relay=true --filter label=buildgate.data-dir="*)
    printf 'factoryd-relay-network-only-orphan\trun-network-only-orphan\n'
    ;;
  *"network rm"*) exit 0 ;;
esac
`
	if err := os.WriteFile(docker, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}

	removed, err := ReconcileRelayOrphans(context.Background(), docker, dataDir)
	if err != nil {
		t.Fatalf("ReconcileRelayOrphans: %v", err)
	}
	if len(removed) != 1 || removed[0] != "factoryd-relay-network-only-orphan" {
		t.Fatalf("removed = %v, want [factoryd-relay-network-only-orphan]", removed)
	}
	record, err := run.Load(dataDir, r.ID)
	if err != nil {
		t.Fatalf("run.Load after reconciliation: %v", err)
	}
	if record.State != run.StateQuarantined || !record.HaltConfirmed {
		t.Errorf("run state = %q, HaltConfirmed = %v, want quarantined and confirmed -- the network was the only orphan reconciliation ever saw for this run", record.State, record.HaltConfirmed)
	}
}

// TestReconcileRelayOrphansSharesOneDebounceAcrossContainerAndNetworkOrphans
// is the regression for a codex finding on PR #42 (seventh round): an
// earlier version gave the container path and the network-only path each
// their own separate ownerHeartbeatInterval debounce wait, so one scan that
// happened to contain both a stale relay container and a stale network-only
// orphan paid the wait twice, serially -- doubling this function's own
// delay (ReconcileOrphans, this same call's own worker-side sibling, runs
// synchronously before factoryd dials Temporal at startup). Proven here by
// measuring wall time across a scan with one of each candidate type: it
// must stay well under two debounce intervals.
func TestReconcileRelayOrphansSharesOneDebounceAcrossContainerAndNetworkOrphans(t *testing.T) {
	// Counts debounce waits through the ownerDebounceAfter seam instead of
	// timing wall clock: the wall-clock version (a 700ms unit vs ~10 forks
	// of a fake docker script) failed under make verify's sharded -race
	// load while passing alone.
	const unit = 700 * time.Millisecond
	debounceWaits := 0
	previousDebounceAfter := ownerDebounceAfter
	ownerDebounceAfter = func(time.Duration) <-chan time.Time {
		debounceWaits++
		fired := make(chan time.Time, 1)
		fired <- time.Now()
		return fired
	}
	t.Cleanup(func() { ownerDebounceAfter = previousDebounceAfter })
	previousStale := ownerStaleAfter
	ownerStaleAfter = 0
	t.Cleanup(func() { ownerStaleAfter = previousStale })
	previousInterval := ownerHeartbeatInterval
	ownerHeartbeatInterval = unit
	t.Cleanup(func() { ownerHeartbeatInterval = previousInterval })

	dataDir := t.TempDir()
	for _, r := range []run.Run{
		{ID: "run-mixed-container", State: run.StateSliceRunning},
		{ID: "run-mixed-network", State: run.StateSliceRunning},
	} {
		r := r
		if err := r.Save(dataDir); err != nil {
			t.Fatalf("save run %s: %v", r.ID, err)
		}
		if err := writeOwnerHeartbeat(dataDir, r.ID); err != nil {
			t.Fatalf("writeOwnerHeartbeat %s: %v", r.ID, err)
		}
	}

	docker := filepath.Join(t.TempDir(), "docker-fake")
	script := `#!/bin/sh
case "$*" in
  *"ps -a --filter label=buildgate.relay=true --filter label=buildgate.data-dir="*)
    printf 'factoryd-relay-container-mixed\trun-mixed-container\n'
    ;;
  *"ps -a --filter label=buildgate.data-dir="*"--filter label=buildgate.run="*) ;;
  *"ps -a --filter name=^/"*) ;;
  *"network ls --filter label=buildgate.relay=true --filter label=buildgate.data-dir="*)
    printf 'factoryd-relay-mixed-network-only\trun-mixed-network\n'
    ;;
  *"network ls --filter name="*) ;;
  *"rm -f"*) exit 0 ;;
  *"network rm"*) exit 0 ;;
esac
`
	if err := os.WriteFile(docker, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}

	removed, err := ReconcileRelayOrphans(context.Background(), docker, dataDir)
	if err != nil {
		t.Fatalf("ReconcileRelayOrphans: %v", err)
	}
	if debounceWaits != 1 {
		t.Fatalf("ReconcileRelayOrphans waited %d debounce intervals with one container and one network-only stale candidate, want exactly 1 (one shared debounce, not one per candidate type)", debounceWaits)
	}
	want := map[string]bool{
		"factoryd-relay-container-mixed":    true,
		"factoryd-relay-mixed":              true,
		"factoryd-relay-mixed-network-only": true,
	}
	// Compared as a set, not just by length (found via review): a
	// length-only check would pass for any three names removed, including
	// the wrong network -- this test's whole point is that both the
	// container candidate's own paired network and the separate
	// network-only candidate were the ones actually reclaimed.
	got := map[string]bool{}
	for _, name := range removed {
		got[name] = true
	}
	if len(got) != len(removed) {
		t.Fatalf("removed = %v contains a duplicate name", removed)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("removed = %v, want exactly %v", removed, want)
	}
}

// TestReconcileRelayOrphansDebouncesNetworkOnlyStaleness is the regression
// for a codex finding on PR #42 (sixth round): the container path's own
// debounce -- wait one ownerHeartbeatInterval, then recheck staleness
// before actually reclaiming anything -- protects a live owner that resumes
// from a suspend longer than ownerStaleAfter but shorter than one
// reconciliation pass. The network-only path lacked the same protection:
// without it, a resumed owner mid-launch (network created, container
// not yet up) could have its network reclaimed and its run quarantined out
// from under it before its heartbeat goroutine got a chance to refresh.
// Mirrors TestReconcileOrphansDebouncesStalenessBeforeReaping's own timing
// approach: refresh the marker partway through the debounce wait and prove
// the network -- and the run record -- survive untouched.
func TestReconcileRelayOrphansDebouncesNetworkOnlyStaleness(t *testing.T) {
	const unit = 250 * time.Millisecond
	previousStale := ownerStaleAfter
	ownerStaleAfter = 3 * unit
	t.Cleanup(func() { ownerStaleAfter = previousStale })
	previousInterval := ownerHeartbeatInterval
	ownerHeartbeatInterval = 2 * unit
	t.Cleanup(func() { ownerHeartbeatInterval = previousInterval })

	dataDir := t.TempDir()
	r := run.Run{ID: "run-network-only-resumed-owner", State: run.StateSliceRunning}
	if err := r.Save(dataDir); err != nil {
		t.Fatalf("save run: %v", err)
	}
	if err := writeOwnerHeartbeat(dataDir, r.ID); err != nil {
		t.Fatalf("writeOwnerHeartbeat: %v", err)
	}

	go func() {
		time.Sleep(5 * unit)
		_ = writeOwnerHeartbeat(dataDir, r.ID)
	}()
	time.Sleep(4 * unit) // let the initial marker actually go stale first

	docker := filepath.Join(t.TempDir(), "docker-fake")
	script := `#!/bin/sh
case "$*" in
  *"ps -a --filter label=buildgate.relay=true --filter label=buildgate.data-dir="*) ;;
  *"ps -a --filter label=buildgate.data-dir="*"--filter label=buildgate.run="*) ;;
  *"network ls --filter label=buildgate.relay=true --filter label=buildgate.data-dir="*)
    printf 'factoryd-relay-network-only-resumed\trun-network-only-resumed-owner\n'
    ;;
  *"network rm"*) exit 0 ;;
esac
`
	if err := os.WriteFile(docker, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}

	removed, err := ReconcileRelayOrphans(context.Background(), docker, dataDir)
	if err != nil {
		t.Fatalf("ReconcileRelayOrphans: %v", err)
	}
	if len(removed) != 0 {
		t.Fatalf("removed = %v, want none: the owner refreshed its heartbeat during the debounce wait, so this network must survive", removed)
	}
	record, err := run.Load(dataDir, r.ID)
	if err != nil {
		t.Fatalf("run.Load after reconciliation: %v", err)
	}
	if record.State != run.StateSliceRunning {
		t.Errorf("run state = %q, want unchanged %q -- a resumed owner's run must not be quarantined out from under it", record.State, run.StateSliceRunning)
	}
}

// TestReconcileRelayOrphansQuarantinesNetworkOnlyOrphanDespiteAmbiguousRemoval
// is the regression for a codex finding on PR #42 (sixth round): the
// network-only path used to trust `docker network rm`'s own exit status at
// face value -- a spurious CLI-reported failure (the daemon actually
// removed the network, but the CLI itself timed out or lost the response)
// meant this run's quarantine was skipped even though its network, its
// last discoverable resource, really was gone. cleanupNetwork's own
// remove-then-confirm protocol (now used here) must positively confirm the
// network's absence via a separate query, not just trust the removal
// command's own reported result -- proven here with a `network rm` that
// exits nonzero while `network ls` confirms the network is actually gone.
func TestReconcileRelayOrphansQuarantinesNetworkOnlyOrphanDespiteAmbiguousRemoval(t *testing.T) {
	previousStale := ownerStaleAfter
	ownerStaleAfter = 0
	t.Cleanup(func() { ownerStaleAfter = previousStale })
	previousInterval := ownerHeartbeatInterval
	ownerHeartbeatInterval = time.Millisecond
	t.Cleanup(func() { ownerHeartbeatInterval = previousInterval })

	dataDir := t.TempDir()
	r := run.Run{ID: "run-network-only-ambiguous-removal", State: run.StateSliceRunning}
	if err := r.Save(dataDir); err != nil {
		t.Fatalf("save run: %v", err)
	}
	if err := writeOwnerHeartbeat(dataDir, r.ID); err != nil {
		t.Fatalf("writeOwnerHeartbeat: %v", err)
	}

	docker := filepath.Join(t.TempDir(), "docker-fake")
	script := `#!/bin/sh
case "$*" in
  *"ps -a --filter label=buildgate.relay=true --filter label=buildgate.data-dir="*) ;;
  *"ps -a --filter label=buildgate.data-dir="*"--filter label=buildgate.run="*) ;;
  *"network ls --filter label=buildgate.relay=true --filter label=buildgate.data-dir="*)
    printf 'factoryd-relay-network-ambiguous-removal\trun-network-only-ambiguous-removal\n'
    ;;
  *"network rm"*) exit 1 ;;
  *"network ls --filter name="*) ;;
esac
`
	if err := os.WriteFile(docker, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}

	removed, err := ReconcileRelayOrphans(context.Background(), docker, dataDir)
	if err != nil {
		t.Fatalf("ReconcileRelayOrphans: %v", err)
	}
	if len(removed) != 1 || removed[0] != "factoryd-relay-network-ambiguous-removal" {
		t.Fatalf("removed = %v, want [factoryd-relay-network-ambiguous-removal] -- confirmed absent despite `network rm`'s own reported failure", removed)
	}
	record, err := run.Load(dataDir, r.ID)
	if err != nil {
		t.Fatalf("run.Load after reconciliation: %v", err)
	}
	if record.State != run.StateQuarantined || !record.HaltConfirmed {
		t.Errorf("run state = %q, HaltConfirmed = %v, want quarantined and confirmed -- the network's confirmed absence, not `network rm`'s own exit status, must decide this", record.State, record.HaltConfirmed)
	}
}

func readRelayCalls(t *testing.T, logPath string) [][]string {
	t.Helper()
	contents, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read Docker call log: %v", err)
	}
	var calls [][]string
	for _, line := range strings.Split(strings.TrimSpace(string(contents)), "\n") {
		if line != "" {
			calls = append(calls, strings.Fields(line))
		}
	}
	return calls
}

// TestRelayPolicyPinsChatGPTCodexUpstream is a thin wiring proof, not the
// authoritative pin table (that lives in internal/meter's own
// TestValidateChatGPTCodexRoute, the single source of truth every caller
// of ValidateChatGPTCodexRoute shares): just enough to prove
// RoutePolicy.Validate genuinely calls relay.ValidateChatGPTCodexRoute
// with its own Upstream/AllowedPathPrefix/WorkerBasePath, rather than
// skipping it or silently swallowing its result.
func TestRelayPolicyPinsChatGPTCodexUpstream(t *testing.T) {
	policy := RoutePolicy{
		Upstream:          "http://model-host:8080",
		Route:             "test-route",
		AllowedPathPrefix: meter.ChatGPTCodexResponsesPath,
		UsageFormat:       meter.UsageFormatOpenAIResponses,
		AuthMode:          meter.CredentialModeChatGPTCodex,
		WorkerModelID:     "gpt-5.6-luna",
	}
	if err := policy.Validate(); err == nil || !strings.Contains(err.Error(), meter.ChatGPTCodexAPIBase) {
		t.Fatalf("Validate() = %v, want a pinned-upstream refusal", err)
	}
	policy.Upstream = meter.ChatGPTCodexAPIBase + "/"
	if err := policy.Validate(); err != nil && strings.Contains(err.Error(), "never sent to any other host") {
		t.Fatalf("Validate() rejected the ChatGPT Codex upstream itself: %v", err)
	}
}

// TestRelayPolicyPinsChatGPTCodexPathAndBasePath is the same thin wiring
// proof for RoutePolicy's own AllowedPathPrefix/WorkerBasePath fields: a
// leftover relay_allowed_path_prefix/relay_worker_base_path from another
// route must reach relay.ValidateChatGPTCodexRoute and fail closed there.
func TestRelayPolicyPinsChatGPTCodexPathAndBasePath(t *testing.T) {
	valid := validRelayPolicy()
	valid.Upstream = meter.ChatGPTCodexAPIBase
	valid.AllowedPathPrefix = meter.ChatGPTCodexResponsesPath
	valid.UsageFormat = meter.UsageFormatOpenAIResponses
	valid.WorkerModelAPI = meter.RequestFormatOpenAIResponses
	valid.AuthMode = meter.CredentialModeChatGPTCodex
	valid.WorkerModelID = "gpt-5.6-luna"
	if err := valid.Validate(); err != nil {
		t.Fatalf("Validate() on the pinned route = %v, want nil", err)
	}
	widened := valid
	widened.WorkerBasePath = "/v1"
	if err := widened.Validate(); err == nil || !strings.Contains(err.Error(), meter.ChatGPTCodexResponsesPath) {
		t.Errorf("non-empty WorkerBasePath: Validate() = %v, want relay.ValidateChatGPTCodexRoute's own pinned-path refusal", err)
	}
}

// TestRelayPolicyPinsGitHubCopilotUpstream is a thin wiring proof, not the
// authoritative pin table (that lives in internal/meter's own
// TestValidateGitHubCopilotRoute, the single source of truth every caller
// of ValidateGitHubCopilotRoute shares): just enough cases to prove
// RoutePolicy.Validate genuinely calls relay.ValidateGitHubCopilotRoute
// with its own Upstream, rather than skipping it or silently swallowing
// its result.
func TestRelayPolicyPinsGitHubCopilotUpstream(t *testing.T) {
	valid := validRelayPolicy()
	valid.AuthMode = meter.CredentialModeGitHubCopilot
	valid.WorkerModelID = "gpt-5.6-luna"
	valid.UsageFormat = meter.UsageFormatOpenAI

	valid.Upstream = "https://api.individual.githubcopilot.com"
	if err := valid.Validate(); err != nil {
		t.Errorf("Upstream=%q: Validate() = %v, want nil", valid.Upstream, err)
	}

	refused := valid
	refused.Upstream = "https://api.anthropic.com"
	if err := refused.Validate(); err == nil || !strings.Contains(err.Error(), "must be, or end with") {
		t.Errorf("Upstream=%q: Validate() = %v, want relay.ValidateGitHubCopilotRoute's own pinned-upstream refusal", refused.Upstream, err)
	}
}

// TestRelayPolicyValidatesBilling covers RoutePolicy.Billing: only ""/
// run.BillingSubscription/run.BillingMetered are accepted values, a
// static (metered-API-key) route can never genuinely be labelled
// "subscription", and a control character in Route is refused the same
// way WorkerModelID's already is (via unicode.IsControl, not a fixed
// substring set, so it also catches e.g. a form-feed or vertical tab that
// "\x00\r\n\t" alone would miss). Billing gets no separate
// control-character check: a control character in Billing already fails
// the very next "must be subscription or metered" check below, since
// neither value contains one -- a second check here would just duplicate
// that refusal under a different message.
func TestRelayPolicyValidatesBilling(t *testing.T) {
	valid := validRelayPolicy()

	for _, billing := range []string{"", run.BillingMetered} {
		policy := valid
		policy.Billing = billing
		if err := policy.Validate(); err != nil {
			t.Errorf("Billing=%q with AuthMode static: Validate() = %v, want nil", billing, err)
		}
	}

	invalidValue := valid
	invalidValue.Billing = "invoiced"
	if err := invalidValue.Validate(); err == nil {
		t.Error("Billing=\"invoiced\": Validate() = nil, want a refusal (not subscription or metered)")
	}

	subscriptionOverStatic := valid
	subscriptionOverStatic.Billing = run.BillingSubscription
	if err := subscriptionOverStatic.Validate(); err == nil {
		t.Error("Billing=subscription with AuthMode static: Validate() = nil, want a refusal (a metered API-key route can't be labelled subscription)")
	}

	// A genuinely subscription-billed route (github-copilot/chatgpt-codex
	// auth mode) may legitimately set Billing=subscription.
	copilotPolicy := valid
	copilotPolicy.AuthMode = meter.CredentialModeGitHubCopilot
	copilotPolicy.Upstream = meter.CopilotAPIBase
	copilotPolicy.WorkerModelID = "gpt-5.6-luna"
	copilotPolicy.UsageFormat = meter.UsageFormatOpenAI
	copilotPolicy.Billing = run.BillingSubscription
	if err := copilotPolicy.Validate(); err != nil {
		t.Errorf("Billing=subscription with AuthMode github-copilot: Validate() = %v, want nil", err)
	}

	for name, mutate := range map[string]func(*RoutePolicy){
		"NUL byte in Route":  func(p *RoutePolicy) { p.Route = "codex\x00" },
		"form feed in Route": func(p *RoutePolicy) { p.Route = "codex\x0c" },
		"Billing outside subscription/metered still refused (no dedicated control-char check needed)": func(p *RoutePolicy) { p.Billing = "metered\n" },
	} {
		policy := valid
		mutate(&policy)
		if err := policy.Validate(); err == nil {
			t.Errorf("%s: Validate() = nil, want a control-character refusal", name)
		}
	}
}
