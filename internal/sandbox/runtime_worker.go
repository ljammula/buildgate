package sandbox

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"buildgate/internal/meter"
)

// RuntimeWorker is what a worker launched through a Runtime needs from its
// caller beyond the launch spec.
type RuntimeWorker struct {
	// Relay is the step's bound model route with its credential and its
	// ceilings already lowered to what the run has left; nil for a step that
	// calls no model.
	Relay *RouteSpec
	// ModelBinaries are the worker executables allowed to reach the route.
	ModelBinaries []string
	// MeterLedgerRoot is where the meter writes ledgers on this host.
	MeterLedgerRoot string
	// Now is time.Now when nil.
	Now func() time.Time
}

// routeEnvironmentKeys are the worker environment entries a route sets;
// whatever the caller put under them is replaced.
var routeEnvironmentKeys = []string{
	"ANTHROPIC_BASE_URL", "ANTHROPIC_API_KEY", "PI_HARNESS_PROVIDER", "PI_HARNESS_MODEL",
	envModelBaseURL, envModelID, envModelAPI, envModelExtraJSON, envModelKeyEnv, envModelHeadersJSON,
}

// withEnvironment returns existing without any entry naming one of the
// dropKeys, followed by add.
func withEnvironment(existing []string, dropKeys []string, add ...string) []string {
	environment := make([]string, 0, len(existing)+len(add))
	for _, value := range existing {
		key, _, found := strings.Cut(value, "=")
		if found && slices.Contains(dropKeys, key) {
			continue
		}
		environment = append(environment, value)
	}
	return append(environment, add...)
}

// RunWorkerThroughRuntime launches one worker through rt and, for a step
// with a model route, gives it that route and accounts for what it spent:
// the route's endpoint and placeholders, the credential pushed before the
// launch, the meter in front of the endpoint, and afterwards the sandbox's
// usage read from the meter's ledger into Result.RelayFacts.
//
// The returned error wraps ErrRelayCeilingExceeded when the meter refused a
// request at a ceiling or when the ledger cannot be read: spend that cannot
// be counted is never treated as none. A request the client dropped is
// charged its estimate and marks the spend partial, without an error.
// s.Name tells this launch from the run's others.
func RunWorkerThroughRuntime(ctx context.Context, rt Runtime, s LaunchSpec, w RuntimeWorker) (Result, error) {
	launch := RuntimeLaunch{Nonce: s.Name}
	if w.Relay == nil {
		result, _, err := RunThroughRuntime(ctx, rt, s, launch)
		return result, err
	}
	now := time.Now
	if w.Now != nil {
		now = w.Now
	}
	if w.MeterLedgerRoot == "" {
		return Result{}, errors.New("sandbox runtime: a model route needs the meter's ledger root")
	}
	policy := w.Relay.RoutePolicy
	access, err := policy.RouteAccess(s.DataDir, w.ModelBinaries)
	if err != nil {
		return Result{}, err
	}
	name, err := SandboxName(s.DataDir, s.RunID, launch.Nonce)
	if err != nil {
		return Result{}, err
	}
	launch.Route, launch.MeterLedgerRoot = &access, w.MeterLedgerRoot
	if launch.MeterConfig, err = policy.MeterConfig(s.DataDir, s.RunID, name); err != nil {
		return Result{}, err
	}
	if access.Provider != "" {
		credential, err := routeCredential(*w.Relay, access, now(), s.Timeout)
		if err != nil {
			return Result{}, err
		}
		launch.Credential = &credential
	}
	s.Environment = withEnvironment(s.Environment, routeEnvironmentKeys, access.Environment...)

	result, ref, runErr := RunThroughRuntime(ctx, rt, s, launch)
	result.RelayFacts = RouteLaunchFacts{
		CredentialMode:             policy.AuthMode,
		Route:                      policy.Route,
		Billing:                    policy.Billing,
		WorkerModelID:              policy.WorkerModelID,
		Upstream:                   relayAuditUpstream(policy.Upstream),
		MaxRequestBytes:            policy.MaxRequestBytes,
		RequestsPerMinute:          policy.RequestsPerMinute,
		TokenBudget:                policy.TokenBudget,
		TokenBudgetWindow:          policy.TokenBudgetWindow,
		CostBudgetMicroUSD:         policy.CostBudgetMicroUSD,
		CostBudgetWindow:           policy.CostBudgetWindow,
		InputMicroUSDPerMTok:       policy.InputMicroUSDPerMTok,
		CachedInputMicroUSDPerMTok: policy.CachedInputMicroUSDPerMTok,
		CacheWriteMicroUSDPerMTok:  policy.CacheWriteMicroUSDPerMTok,
		OutputMicroUSDPerMTok:      policy.OutputMicroUSDPerMTok,
		TokenCeiling:               policy.TokenCeiling,
		CostCeilingMicroUSD:        policy.CostCeilingMicroUSD,
		StartedAt:                  result.StartedAt.UTC(),
	}
	if ref.ID == "" {
		// No sandbox was created, so no request was made.
		return result, runErr
	}
	usage, usageErr := SandboxMeterUsage(w.MeterLedgerRoot, s.DataDir, s.RunID, ref.ID)
	if usageErr != nil {
		result.RelayFacts.UsageReadFailed = true
		return result, errors.Join(runErr, fmt.Errorf("%w: the sandbox's spend is unknown: %v", ErrRelayCeilingExceeded, usageErr))
	}
	result.RelayFacts.ConsumedInputTokens = usage.InputTokens
	result.RelayFacts.ConsumedOutputTokens = usage.OutputTokens
	result.RelayFacts.ConsumedCostMicroUSD = usage.CostMicroUSD
	result.RelayFacts.CeilingExceeded = usage.CeilingExceeded
	result.RelayFacts.ReasoningEffort = usage.ReasoningEffort
	result.RelayFacts.ReasoningEffortAnomaly = usage.ReasoningEffortAnomaly
	result.RelayFacts.SpendPartial = usage.Unsettled > 0
	return result, errors.Join(runErr, usage.CeilingErr())
}

// routeCredential builds the route's credential from the spec's own values.
// A ChatGPT access token carries its expiry; a static key has none.
func routeCredential(spec RouteSpec, access RouteAccess, now time.Time, stepTimeout time.Duration) (RouteCredential, error) {
	var expiresAt time.Time
	if spec.AuthMode == meter.CredentialModeChatGPTCodex {
		var err error
		if expiresAt, err = accessTokenExpiry(spec.ChatGPTToken.reveal()); err != nil {
			return RouteCredential{}, fmt.Errorf("sandbox runtime: route %q access token: %w", spec.Route, err)
		}
	}
	return spec.RoutePolicy.RouteCredential(access, spec.APIKey, spec.GitHubToken, spec.ChatGPTToken, spec.ChatGPTAccountID, expiresAt, now, stepTimeout)
}

// accessTokenExpiry reads the "exp" claim of a compact JWT without verifying
// its signature: the token's issuer is trusted, and the claim only decides
// whether a step may start on it.
func accessTokenExpiry(token string) (time.Time, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return time.Time{}, fmt.Errorf("not a JWT (want 3 dot-separated segments, got %d)", len(parts))
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}, errors.New("JWT payload is not base64url")
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil || claims.Exp <= 0 {
		return time.Time{}, errors.New("JWT payload has no exp claim")
	}
	return time.Unix(claims.Exp, 0), nil
}

// LaunchWorker launches one worker: through rt when the caller has a sandbox
// runtime, with `docker run` otherwise.
func LaunchWorker(ctx context.Context, rt Runtime, dockerBinary string, s LaunchSpec, w RuntimeWorker) (Result, error) {
	if rt == nil {
		return Run(ctx, dockerBinary, s)
	}
	return RunWorkerThroughRuntime(ctx, rt, s, w)
}

// BeginRegistryProxyLifecycleFor is BeginRegistryProxyLifecycle for a launch
// site that may have a sandbox runtime. With one, the worker joins no
// network and is given the proxy's address instead of its alias.
func BeginRegistryProxyLifecycleFor(rt Runtime, spec RegistryProxySpec, dockerBinary, workerRunID, workerDataDir string, hooks RegistryProxyHooks) (*RegistryProxyLifecycle, error) {
	l, err := BeginRegistryProxyLifecycle(spec, dockerBinary, workerRunID, workerDataDir, hooks)
	if err != nil {
		return nil, err
	}
	l.byAddress = rt != nil
	return l, nil
}

// BeginComposeServicesLifecycleFor is BeginComposeServicesLifecycle for a
// launch site that may have a sandbox runtime. With one, the worker joins no
// network and is given each service's address instead of its alias. A
// service on a port the runtime never admits by address, and an operator
// worker-environment value that names a service as a host, are refused
// here, before anything is started.
func BeginComposeServicesLifecycleFor(rt Runtime, spec ComposeServicesSpec, dockerBinary, runID, dataDir string, hooks ComposeServicesHooks) (*ComposeServicesLifecycle, error) {
	compose, err := BeginComposeServicesLifecycle(spec, dockerBinary, runID, dataDir, hooks)
	if err != nil || rt == nil || compose.Disabled() {
		return compose, err
	}
	if service, port := compose.runtimeBlockedService(); service != "" {
		_ = compose.Cleanup(context.Background())
		return nil, fmt.Errorf("%w: compose service %q uses port %d, which the sandbox runtime does not admit", ErrComposeServicesRejected, service, port)
	}
	if key, service := compose.workerEnvNamingAnAlias(); key != "" {
		_ = compose.Cleanup(context.Background())
		return nil, fmt.Errorf("%w: compose_services_worker_env %s names the service %q as a host; a worker launched through the sandbox runtime is given addresses and cannot resolve a service name: use BG_SERVICE_%s in the target repository instead", ErrComposeServicesRejected, key, service, strings.ToUpper(service))
	}
	compose.byAddress = true
	return compose, nil
}

// LaunchLeftNothingToRetryOn reports a launch error after which the same
// worktree must not be launched on again in this call: a sandbox that may
// still exist, or a command the runtime started twice.
func LaunchLeftNothingToRetryOn(err error) bool {
	return errors.Is(err, ErrCleanupUnconfirmed) || errors.Is(err, ErrSandboxRerun)
}

// RetryStops reports a launch error that ends an attempt loop: the two of
// LaunchLeftNothingToRetryOn, and a run that reached its ceiling.
func RetryStops(err error) bool {
	return LaunchLeftNothingToRetryOn(err) || errors.Is(err, ErrRelayCeilingExceeded)
}
