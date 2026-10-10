package sandbox

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"time"
	"unicode"

	"buildgate/internal/meter"
	"buildgate/internal/run"
)

// relayEgressNetworkName is the internet-routable Docker network the
// retired inference relay egressed through. Nothing creates it now; a
// pre-upgrade host may still have one, so LaunchSpec.Validate and the
// registry proxy keep refusing to put a worker on it.
const relayEgressNetworkName = "factoryd-relay-egress"

// relayDockerCommandTimeout bounds each individual Docker CLI operation. The
// caller's context still bounds the whole lifecycle; this second bound keeps
// a Docker client which stops responding from holding a factory operation
// forever.
var relayDockerCommandTimeout = 15 * time.Second

// ErrRelayCleanupUnconfirmed means that a sidecar container or its network
// could not be positively confirmed absent. Callers must fail closed rather
// than launch another attempt while either resource may still exist.
//
// It aliases the worker cleanup error so callers that already handle the
// sandbox cleanup stop-line also handle sidecar cleanup uncertainty.
var ErrRelayCleanupUnconfirmed = ErrCleanupUnconfirmed

// ErrRelayCeilingExceeded marks a run that reached its absolute
// TokenCeiling/CostCeilingMicroUSD, or whose spend could not be read -- a
// distinct, machine-readable sentinel so a caller can tell "this run
// legitimately hit its configured budget" apart from "the code is wrong",
// rather than having to pattern-match free text. RunWorkerThroughRuntime
// returns an error wrapping it.
var ErrRelayCeilingExceeded = errors.New("relay token/cost ceiling exceeded")

// RoutePolicy is the credential-free, request-scoped half of a model route's
// configuration: which upstream the worker may reach, and the per-run
// token/cost/rate ceilings the meter enforces. It carries no secret by
// construction, which is what makes it safe to put in a Temporal Workflow
// input — Temporal persists Workflow input in Event History, so anything
// reachable from RunWorkflowInput is durably written to Temporal's own
// storage. The upstream credential is deliberately NOT here: it is
// RouteSecret below, supplied by the Worker's own static configuration.
// See TestRunWorkflowInputCannotCarryACredential, which fails if a
// credential-bearing type or field ever becomes reachable from that input.
//
// Every budget is required: zero does not mean "disabled".
type RoutePolicy struct {
	Upstream string `json:"upstream"`
	// AllowedPathPrefix is the only upstream path prefix the relay may
	// forward. Defaults to "/v1/messages" (the Anthropic Messages API) when
	// empty, matching this package's behavior before this field existed --
	// an operator pointing a route's upstream at a differently-shaped
	// endpoint (e.g. a local/LAN model serving a bare "/v1") sets this to
	// match what that endpoint actually exposes.
	AllowedPathPrefix  string        `json:"allowed_path_prefix"`
	MaxRequestBytes    int64         `json:"max_request_bytes"`
	RequestsPerMinute  int           `json:"requests_per_minute"`
	TokenBudget        int           `json:"token_budget"`
	TokenBudgetWindow  time.Duration `json:"token_budget_window"`
	CostBudgetMicroUSD int64         `json:"cost_budget_micro_usd"`
	CostBudgetWindow   time.Duration `json:"cost_budget_window"`
	// InputMicroUSDPerMTok/CachedInputMicroUSDPerMTok/CacheWriteMicroUSDPerMTok/
	// OutputMicroUSDPerMTok are the model's real provider price, in
	// micro-USD per 1,000,000 tokens, resolved from internal/prices'
	// compiled table by the model's own id (internal/modelrole.SelectRoute)
	// -- never an operator-set session default (2026-09-28: no default
	// dollar figure is any real model's actual price). CachedInputMicroUSDPerMTok/
	// CacheWriteMicroUSDPerMTok fall back to InputMicroUSDPerMTok when the
	// table entry omits them (no cache discount/premium assumed).
	InputMicroUSDPerMTok       int64 `json:"input_micro_usd_per_mtok"`
	CachedInputMicroUSDPerMTok int64 `json:"cached_input_micro_usd_per_mtok"`
	CacheWriteMicroUSDPerMTok  int64 `json:"cache_write_micro_usd_per_mtok"`
	OutputMicroUSDPerMTok      int64 `json:"output_micro_usd_per_mtok"`
	// TokenCeiling/CostCeilingMicroUSD are the absolute, run-scoped ceilings
	// added to close a gap where TokenBudget/CostBudgetMicroUSD above are
	// SLIDING WINDOWS -- see their own doc comments -- which can be
	// re-spent in full every window for the
	// life of a long-running relay, contradicting this very struct's own
	// doc comment calling them "per-run ceilings". These fields are the
	// actual per-run ceiling: relay.Config.TokenCeiling/CostCeilingMicroUSD
	// track a monotonic total that never prunes, so once either is crossed
	// every subsequent request gets 429 for the rest of this relay's life,
	// regardless of the windows. Required positive, like every other budget
	// field on this policy (see this struct's own doc comment on why zero
	// never means "disabled" here) -- cmd/factoryd defaults both
	// generously (5x the configured window budget) so they fire on runaway
	// behavior, not ordinary variance; see -meter-token-ceiling/
	// -meter-cost-ceiling-micro-usd's own flag help for that reasoning.
	TokenCeiling        int   `json:"token_ceiling"`
	CostCeilingMicroUSD int64 `json:"cost_ceiling_micro_usd"`
	// UsageFormat selects which upstream response shape the relay parses
	// usage from for its token/cost budgets -- meter.UsageFormatAnthropic
	// (the default when empty), meter.UsageFormatOpenAI for an
	// OpenAI-compatible Chat Completions endpoint, or
	// meter.UsageFormatOpenAIResponses for the Responses API. See
	// relay.Config.UsageFormat's own doc comment for exactly which fields each
	// parses.
	UsageFormat string `json:"usage_format"`
	// UpstreamAuthHeader is the outbound header the relay injects the real
	// upstream credential into: meter.CredentialHeaderXAPIKey ("x-api-key",
	// the default when empty -- Anthropic's own convention) or
	// meter.CredentialHeaderAuthorization ("Authorization", sent as
	// "Bearer <key>" -- see relay.credentialHeaderValue -- the convention
	// every OpenAI-compatible endpoint this relay targets, e.g. OpenRouter,
	// actually expects). Found via review (Codex, PR #59): before this
	// field existed, a launch always used x-api-key regardless of
	// UsageFormat, so a credentialed OpenAI-compatible upstream got its
	// real key delivered under a header it never checks and rejected every
	// request outright.
	//
	// Named UpstreamAuthHeader, not CredentialHeader, deliberately: this
	// package's own RunWorkflowInput reachability guard
	// (TestRunWorkflowInputCannotCarryACredential) fails closed on any
	// field name merely containing "credential", to catch a real secret
	// ever becoming reachable from Temporal's persisted Workflow input --
	// this field is a header *name* ("x-api-key"/"Authorization"), not a
	// secret, but the guard cannot tell the difference from a name alone,
	// so it is named to simply never collide with that heuristic.
	UpstreamAuthHeader string `json:"upstream_auth_header"`
	// AuthMode selects how the relay authenticates outbound requests to its
	// upstream: meter.CredentialModeStatic (the default when empty --
	// inject RouteSpec.APIKey into UpstreamAuthHeader, this package's
	// original and only behavior before this field existed) or
	// meter.CredentialModeGitHubCopilot (exchange RouteSpec.GitHubToken --
	// a long-lived GitHub OAuth token -- for a short-lived Copilot API
	// token and force it, plus Copilot's own required headers, onto every
	// forwarded request; see meter.CredentialModeGitHubCopilot's own doc
	// comment for the full wire contract). Named AuthMode, not
	// CredentialMode, for the same reason UpstreamAuthHeader is above --
	// this is a policy choice, not a secret, but
	// TestRunWorkflowInputCannotCarryACredential cannot tell that from a
	// field name alone.
	AuthMode string `json:"auth_mode"`
	// WorkerModelID, when set, configures the sandboxed worker to reach this
	// relay as an OpenAI-compatible provider instead of the default
	// Anthropic-shaped wiring: PrepareWorker hands the worker the
	// FACTORY_MODEL_BASE_URL/FACTORY_MODEL_ID/FACTORY_MODEL_API environment
	// variables (base URL pointed at this run's own relay, never the real
	// upstream -- the worker still only ever reaches the relay) and no
	// ANTHROPIC_BASE_URL/ANTHROPIC_API_KEY; the harness adapter inside the
	// worker (agent/pi/scripts/harness_adapters.py) renders its CLI's own
	// config from them. WorkerModelID must equal whatever the real upstream
	// expects in its own request body's "model" field, since it travels
	// through unchanged (see relay.Config.UsageFormat's OpenAI parsing for
	// where that same wire shape is read back). Empty (default) preserves
	// today's Anthropic-only wiring exactly.
	WorkerModelID string `json:"worker_model_id"`
	// WorkerModelAPI selects the pi provider API used by the generated
	// worker model entry: meter.RequestFormatOpenAICompletions (the
	// historical default) or meter.RequestFormatOpenAIResponses. A Responses
	// selection requires WorkerModelID so it cannot be silently ignored.
	WorkerModelAPI string `json:"worker_model_api,omitempty"`
	// WorkerBasePath, only used alongside WorkerModelID, is the path
	// segment appended to the relay's own fixed internal address to build
	// the worker's model baseUrl -- "/v1" (the OpenAI-completions
	// convention) when empty. Deliberately its own field, not
	// AllowedPathPrefix reused (found via review, Codex, PR #60):
	// AllowedPathPrefix names what path the relay itself accepts as valid
	// inbound (a security allowlist, which can legitimately be a complete
	// endpoint -- its own default, "/v1/messages", names Anthropic's one
	// real endpoint exactly, not an API-root prefix). Reusing it here
	// meant an operator whose AllowedPathPrefix happened to already name a
	// complete endpoint got a worker base URL like ".../chat/completions",
	// onto which pi's openai-completions client then appended its own
	// "/chat/completions" -- a 404 against any real upstream. The
	// operator's own AllowedPathPrefix must still separately admit
	// whatever request path this produces.
	WorkerBasePath string `json:"worker_base_path,omitempty"`
	// WorkerModelExtraJSON, when set, is a JSON object merged onto the
	// generated model entry's own id/api/baseUrl fields (those three
	// always win over the extra JSON, never the other way: the adapter's
	// reserved-key rule, tested in agent/pi/tests/harness_adapters_test.py,
	// keeps baseUrl pinned to the relay) -- only used alongside WorkerModelID. Exists
	// because a
	// real local model route commonly needs pi model-config fields this
	// package has no reason to model individually: samplingParams (a
	// vendor-tuned temperature/top_p/top_k preset -- found live,
	// 2026-09-07, that a sandboxed worker with no samplingParams sent
	// temperature 1.0 instead of a host-side pi config's tuned 0.6),
	// reasoning/compat.thinkingFormat/thinkingLevelMap (enabling AND
	// actually controlling a thinking-capable model's extended reasoning
	// -- reasoning:true alone only makes pi parse reasoning_content back
	// out of the response; without compat.thinkingFormat naming the real
	// wire shape, a caller's chosen --thinking level has no real effect.
	// Confirmed live, 2026-09-07: the default "openai" shape sends only a
	// reasoning_effort string, which a real qwen-family local route
	// silently ignores in favor of its own server-side default -- pi
	// believed it configured thinking off, the model kept thinking anyway.
	// "qwen" sends the real top-level enable_thinking boolean that route
	// actually checks), and compat overrides (e.g. supportsDeveloperRole:
	// false, needed when a route's tokenizer rejects the "developer"
	// system-role some generic OpenAI-compatible clients send once
	// reasoning is on). pi's own schema already treats
	// samplingParams/compat as free-form JSON for exactly this reason
	// (see its docs' "Use it to send sampling parameters pi does not
	// model") -- this field is that same free-form escape hatch, not a
	// fixed set of software-factory-modeled options, so it never needs to
	// grow bespoke fields/flags for every upstream's own quirks. Must
	// parse as a JSON object; validated at worker-preparation time, not
	// silently dropped, so a typo fails the run closed rather than
	// quietly building a worker with defaults nobody chose.
	WorkerModelExtraJSON string `json:"worker_model_extra_json,omitempty"`
	// AllowUnauthenticatedUpstream opts out of requiring a real upstream
	// credential: with this set, a RouteSpec built from this policy (see
	// Spec) accepts an empty RouteSecret, for a model endpoint that has
	// no real credential to protect at all. Carried on the policy itself,
	// not just decided by whichever caller assembles the credential (found
	// via review, Codex, PR #59), so the decision travels through Workflow
	// input on the Temporal path exactly like every other relay-shape
	// choice here -- without this, a caller of RunWorkflow had no way to
	// tell RunBuildActivity's own credential check that no credential was
	// ever expected, and that unconditional check rejected the run before
	// a relay-contained local-model build could even reach this package.
	// Named to avoid "credential" for the same reason UpstreamAuthHeader
	// is above -- this is a policy choice, not a secret, but
	// TestRunWorkflowInputCannotCarryACredential cannot tell that from a
	// field name alone.
	AllowUnauthenticatedUpstream bool `json:"allow_unauthenticated_upstream"`
	// AllowPlaintextUpstream opts into letting Upstream be an http:// URL
	// for a private/loopback local model endpoint with no real credential
	// to protect -- see ValidateUpstreamScheme, the only place this field
	// is consulted. Carried on the policy itself, not just decided by
	// whichever caller resolves the CLI flag (matching
	// AllowUnauthenticatedUpstream's own reasoning above), so the Worker's
	// own https-only check can honour -relay-allow-plaintext-upstream.
	AllowPlaintextUpstream bool `json:"allow_plaintext_upstream"`
	// Route is the session-config route name (e.g. "codex", "copilot",
	// "litellm") this policy was resolved from, when the caller resolved it
	// through a routes:/models: config (see internal/sessionconfig and
	// internal/modelrole, Phase 2) -- empty for a legacy config with no
	// routes: key, where no per-job route selection exists yet. It is
	// display evidence, recorded because two routes
	// can legitimately share the same Upstream (see run.Attempt.RelayRoute's
	// own doc comment), so Upstream alone cannot always tell them apart,
	// and it is load-bearing, not just display: it is
	// the ONLY thing a submitted RoutePolicy/ReviewRelayPolicy names --
	// the Worker (internal/modelrole.CheckRouteBinding, via
	// workflow.Activities.CheckRoute) looks this name up in its OWN
	// routes:/models: config and rebuilds the policy that route/role
	// model pair should have, refusing the submitted policy outright
	// unless every other field matches exactly (a target repo's project
	// config may only tighten TokenCeiling/CostCeilingMicroUSD). Named
	// Route, not RouteName, for the same reason UpstreamAuthHeader/
	// AuthMode are above: not a secret, but
	// TestRunWorkflowInputCannotCarryACredential cannot tell that from a
	// field name alone -- "Route" contains neither forbidden substring.
	Route string `json:"route,omitempty"`
	// Billing labels how this policy's route is paid for -- "subscription"
	// (a ChatGPT/Copilot subscription route) or "metered" (a per-token API
	// key), or empty for a legacy config that predates this field, in
	// which case run.SubscriptionBilled falls back to inferring it from
	// AuthMode. Display evidence only, exactly like AuthMode/Route above:
	// it never changes enforcement (a subscription route still counts
	// notional spend against the run's real cost ceiling -- see run.
	// SubscriptionBilled's own doc comment) or CredentialSafeForUpstream's
	// plaintext check.
	Billing string `json:"billing,omitempty"`
}

// RouteSecret holds the real upstream model API key. Its value is
// unexported and readable only inside this package (see reveal), so no
// caller outside internal/sandbox can copy it into a log line, a durable run
// record, or a Temporal Workflow input. It also refuses to marshal to JSON
// at all: if a future change did place a credential-bearing value somewhere
// Temporal persists, that submission fails loudly instead of silently
// writing a secret into Event History.
type RouteSecret struct {
	secret string
}

// NewRouteSecret wraps the process-supplied upstream API key. Callers
// read it from their own environment (never from Workflow input or Activity
// arguments) — see cmd/factoryd's relay flag handling.
func NewRouteSecret(secret string) RouteSecret {
	return RouteSecret{secret: secret}
}

func (c RouteSecret) reveal() string { return c.secret }

// Configured reports whether a credential was supplied at all, so a caller
// outside this package can fail closed on a missing one without ever being
// able to read it.
func (c RouteSecret) Configured() bool { return c.secret != "" }

// String and GoString redact, so a credential can never be printed by an
// accidental %v/%s/%#v of a spec that contains one.
func (c RouteSecret) String() string {
	if c.secret == "" {
		return ""
	}
	return "[REDACTED]"
}

func (c RouteSecret) GoString() string { return c.String() }

var errRelayCredentialNotSerializable = errors.New("relay credential must never be serialized (it would be written to durable storage, e.g. Temporal Event History)")

func (c RouteSecret) MarshalJSON() ([]byte, error) { return nil, errRelayCredentialNotSerializable }

func (c *RouteSecret) UnmarshalJSON([]byte) error { return errRelayCredentialNotSerializable }

// RouteSpec is one relay lifecycle's complete, factory-owned configuration:
// the request-scoped RoutePolicy plus the Worker-supplied credential and the
// run identity its container and network are labeled with. It is assembled
// where the relay is actually launched and never travels anywhere durable.
type RouteSpec struct {
	RoutePolicy
	APIKey RouteSecret
	// GitHubToken is the long-lived GitHub OAuth token the relay exchanges
	// for a short-lived Copilot API token -- only consulted, and required,
	// when RoutePolicy.AuthMode is meter.CredentialModeGitHubCopilot. APIKey
	// above is not used in that mode.
	GitHubToken RouteSecret
	// ChatGPTToken/ChatGPTAccountID are the ChatGPT OAuth access token and
	// account id this relay forces onto every forwarded request -- only
	// consulted, and both required, when RoutePolicy.AuthMode is
	// meter.CredentialModeChatGPTCodex. Neither is exchanged/refreshed by
	// this relay (see that mode's own doc comment); the caller resolves a
	// fresh access token from the host's ~/.codex/auth.json before every
	// relay launch (see cmd/factoryd/relay_codex.go), not once per process
	// lifetime.
	ChatGPTToken     RouteSecret
	ChatGPTAccountID RouteSecret
	RunID            string
	// DataDir must name the same durable run-record root LaunchSpec.DataDir
	// does for this same attempt: it becomes this relay's own
	// buildgate.data-dir label (see dataDirLabel), the only thing
	// that makes ReconcileRelayOrphans able to find this container and
	// network again after a crash. Found via review (GitHub Codex App,
	// PR #42): without this label a killed factoryd could never reclaim a
	// detached relay container, which keeps running with the real
	// upstream credential on the ordinary bridge network indefinitely --
	// unlike the worker container, which ReconcileOrphans already covers.
	DataDir string
	// CABundlePath, when set, is a host-side PEM file (e.g. a corporate
	// TLS-interception CA) bind-mounted read-only into the relay container
	// -- see egressCABundleDockerArgs. Host-side, per-Worker fact like
	// APIKey above, not policy: it names a path on THIS machine, so it
	// stays off RoutePolicy and out of Workflow input the same way the
	// credential does (see RoutePolicy's own doc comment) -- a Temporal
	// Worker that needs one sets it from its own environment/config, never
	// from a caller's request.
	CABundlePath string
}

// RouteLaunchFacts contains only safe-to-persist lifecycle metadata. In
// particular, it has no API key, Docker command, or inherited environment.
type RouteLaunchFacts struct {
	Image       string
	ImageDigest string
	// CredentialMode is this relay's own RouteSpec.AuthMode (relay.
	// CredentialModeStatic/CredentialModeGitHubCopilot/
	// CredentialModeChatGPTCodex) -- audit-safe evidence, unlike the
	// credentials RouteSpec itself carries, of *how* this attempt's spend
	// was billed. A finding from the onboarding walkthrough: before this,
	// a run's own evidence had no way to tell a subscription
	// route's cost apart from a metered API route's at display time, so an
	// operator paying via ChatGPT/Copilot subscription saw an invented-
	// looking "$3.20" dollar figure that was never actually billed to them.
	CredentialMode string
	// Route is this relay's own RouteSpec.Route: the session-config route
	// name it was launched from, empty for a legacy config with no
	// routes: key. See RoutePolicy.Route's own doc comment.
	Route string
	// Billing is this relay's own RouteSpec.Billing: "subscription" or
	// "metered", empty for a legacy config that predates this field. See
	// RoutePolicy.Billing's own doc comment.
	Billing string
	// WorkerModelID is this relay's own RouteSpec.WorkerModelID: the model
	// id factoryd configured the worker to use (its FACTORY_MODEL_ID env).
	// Factory-authored, so it records the operator's configuration rather
	// than anything the agent reports -- but nothing enforces it: the
	// request body's "model" field reaches the upstream unchanged,
	// so a worker that asks for another model still has its tokens
	// attributed to this id. Display evidence only. Empty when no worker
	// model id was configured.
	WorkerModelID              string
	Upstream                   string
	NetworkName                string
	NetworkID                  string
	ContainerName              string
	ContainerID                string
	WorkerBaseURL              string
	MaxRequestBytes            int64
	RequestsPerMinute          int
	TokenBudget                int
	TokenBudgetWindow          time.Duration
	CostBudgetMicroUSD         int64
	CostBudgetWindow           time.Duration
	InputMicroUSDPerMTok       int64
	CachedInputMicroUSDPerMTok int64
	CacheWriteMicroUSDPerMTok  int64
	OutputMicroUSDPerMTok      int64
	TokenCeiling               int
	CostCeilingMicroUSD        int64
	// ConsumedInputTokens/ConsumedOutputTokens/ConsumedCostMicroUSD/
	// CeilingExceeded are populated after the step, not at launch (from
	// the sandbox's meter ledger): this run's actual spend, not
	// its configuration, closing a gap where a run's evidence carried no
	// record of actual spend. All four stay zero/false for a relay never
	// successfully cleaned up (e.g. a crash reclaimed by
	// ReconcileRelayOrphans instead), since reading a removed/gone
	// container's logs is no longer possible by the time reconciliation
	// gets to it -- the configured
	// TokenBudget/CostBudgetMicroUSD/TokenCeiling/CostCeilingMicroUSD above
	// are still recorded regardless, so a run's configured limits are never
	// lost even when its actual spend is.
	ConsumedInputTokens  int64
	ConsumedOutputTokens int64
	ConsumedCostMicroUSD int64
	CeilingExceeded      bool
	// ReasoningEffort is the HIGHEST requested reasoning effort this run's
	// relay saw on the wire (see RelayUsage.ReasoningEffort's own doc
	// comment) -- "" when none was ever requested, the relay predates this
	// field, or usage could not be confirmed and no ledger fallback
	// recovered one either.
	ReasoningEffort string
	// ReasoningEffortAnomaly is true when at least one request this run's
	// relay forwarded had a reasoning-effort value this relay could not
	// recognize (see RelayUsage.ReasoningEffortAnomaly's own doc comment)
	// -- sticky and independent of ReasoningEffort itself, which never
	// lets "other" mask a real, lower effort value as the highest seen.
	ReasoningEffortAnomaly bool
	// UsageReadFailed is true when this run's actual spend could not be read
	// from the meter's ledger — distinct from "no meterable request was
	// made" (which legitimately leaves every field above at zero with
	// UsageReadFailed staying false). The launch then ends with
	// ErrRelayCeilingExceeded (fail closed: "cannot confirm this run stayed
	// within budget" gets the same operator-facing halt as "confirmed over
	// budget").
	UsageReadFailed bool
	// SpendPartial is true when the totals above include the worst-case
	// estimate of a request the meter admitted and never saw complete (a
	// client that dropped a stream). Callers must say so in any rendered
	// evidence (see cmd/factoryd's release_and_evidence.go).
	SpendPartial bool
	StartedAt    time.Time
}

// Spec assembles the launchable configuration for one run's relay from this
// request-scoped policy, the Worker's own credential(s), and the run
// identity the relay's container and network are labeled with. This is the
// only way a policy becomes launchable, so the credential is always
// contributed by whoever holds it locally, never carried alongside the
// policy. githubToken is only consulted (and only need be non-empty) when
// AuthMode is meter.CredentialModeGitHubCopilot; credential fills the same
// role for the default static mode. chatGPTToken/chatGPTAccountID are only
// consulted (and only need be non-empty) when AuthMode is
// meter.CredentialModeChatGPTCodex.
func (p RoutePolicy) Spec(credential, githubToken, chatGPTToken, chatGPTAccountID RouteSecret, runID, dataDir string) RouteSpec {
	return RouteSpec{RoutePolicy: p, APIKey: credential, GitHubToken: githubToken, ChatGPTToken: chatGPTToken, ChatGPTAccountID: chatGPTAccountID, RunID: runID, DataDir: dataDir}
}

// Validate rejects mutable images, unsafe upstream URLs, and
// disabled/unbounded relay limits. Split out from RouteSpec.Validate so a
// caller holding only the credential-free half (a Temporal Workflow input's
// RoutePolicy) can reject a bad policy without needing a credential.
func (p RoutePolicy) Validate() error {
	for _, check := range []func() error{
		p.validateFormats,
		p.validateRouteAndBilling,
		p.validateAuthModeRoute,
		p.validateWorkerModel,
		p.validateBudgets,
	} {
		if err := check(); err != nil {
			return err
		}
	}
	return nil
}

// validateFormats is the first group of Validate's rules: the upstream and
// the fixed-value fields. Validate runs the groups in order, and each group
// its rules in order, so the first rule a policy breaks is the one reported.
func (p RoutePolicy) validateFormats() error {
	if !validRelayUpstream(p.Upstream) {
		return errors.New("relay upstream must be an absolute HTTP(S) URL without credentials or query data")
	}
	if p.AllowedPathPrefix != "" && !strings.HasPrefix(p.AllowedPathPrefix, "/") {
		return errors.New("relay allowed path prefix must start with /")
	}
	if p.UsageFormat != "" && p.UsageFormat != meter.UsageFormatAnthropic && p.UsageFormat != meter.UsageFormatOpenAI && p.UsageFormat != meter.UsageFormatOpenAIResponses {
		return fmt.Errorf("relay usage format must be %q, %q, or %q", meter.UsageFormatAnthropic, meter.UsageFormatOpenAI, meter.UsageFormatOpenAIResponses)
	}
	if p.WorkerModelAPI != "" && p.WorkerModelAPI != meter.RequestFormatOpenAICompletions && p.WorkerModelAPI != meter.RequestFormatOpenAIResponses {
		return fmt.Errorf("relay worker model API must be %q or %q", meter.RequestFormatOpenAICompletions, meter.RequestFormatOpenAIResponses)
	}
	if p.WorkerModelAPI == meter.RequestFormatOpenAIResponses && p.WorkerModelID == "" {
		return errors.New("relay worker model id is required when worker API is \"openai-responses\"")
	}
	if p.UpstreamAuthHeader != "" && !strings.EqualFold(p.UpstreamAuthHeader, meter.CredentialHeaderXAPIKey) && !strings.EqualFold(p.UpstreamAuthHeader, meter.CredentialHeaderAuthorization) {
		return fmt.Errorf("relay credential header must be %q or %q", meter.CredentialHeaderXAPIKey, meter.CredentialHeaderAuthorization)
	}
	if p.AuthMode != "" && p.AuthMode != meter.CredentialModeStatic && p.AuthMode != meter.CredentialModeGitHubCopilot && p.AuthMode != meter.CredentialModeChatGPTCodex {
		return fmt.Errorf("relay auth mode must be %q, %q, or %q", meter.CredentialModeStatic, meter.CredentialModeGitHubCopilot, meter.CredentialModeChatGPTCodex)
	}
	return nil
}

// validateRouteAndBilling checks the route name and the billing label.
func (p RoutePolicy) validateRouteAndBilling() error {
	if p.Route == "" {
		return errors.New("relay route is required: routes:/models:/roles: is the only session-config schema")
	}
	if strings.IndexFunc(p.Route, unicode.IsControl) >= 0 {
		return errors.New("relay route must not contain a control character")
	}
	// No separate control-character check for Billing: the very next check
	// already only accepts the two fixed values below, so a Billing value
	// with a control character in it is already refused by that allowed-
	// values check -- a second, redundant check here would only duplicate
	// it under a different error message.
	if p.Billing != "" && p.Billing != run.BillingSubscription && p.Billing != run.BillingMetered {
		return fmt.Errorf("relay billing must be %q or %q", run.BillingSubscription, run.BillingMetered)
	}
	// A metered API-key route (meter.CredentialModeStatic, including the
	// empty/default AuthMode) can never be genuinely labelled
	// "subscription": nothing about a static API key is billed to a
	// ChatGPT/Copilot subscription, so a config asserting otherwise is a
	// misconfiguration (a stale Billing left over from switching a route's
	// credential_mode without also updating its billing), not a real
	// route this relay should launch mislabeled.
	if p.Billing == run.BillingSubscription && p.AuthMode != meter.CredentialModeGitHubCopilot && p.AuthMode != meter.CredentialModeChatGPTCodex {
		return fmt.Errorf("relay billing must not be %q when auth mode is %q: a metered API-key route is never billed to a subscription", run.BillingSubscription, p.AuthMode)
	}
	return nil
}

// validateAuthModeRoute checks what the GitHub Copilot and ChatGPT auth modes require
// of the route.
func (p RoutePolicy) validateAuthModeRoute() error {
	// The worker only ever reaches this relay as an OpenAI-compatible
	// provider (WorkerModelID) or the default Anthropic-shaped wiring --
	// GitHub Copilot's own API is OpenAI-compatible, so github-copilot mode
	// requires the former; without this, the worker would be wired for the
	// Anthropic Messages API while the relay authenticates to and forwards
	// an OpenAI-compatible upstream.
	if p.AuthMode == meter.CredentialModeGitHubCopilot && p.WorkerModelID == "" {
		return errors.New("relay worker model id is required when auth mode is \"github-copilot\": the worker must reach the relay as an OpenAI-compatible provider")
	}
	if p.AuthMode == meter.CredentialModeGitHubCopilot {
		if err := meter.ValidateGitHubCopilotRoute(p.Upstream); err != nil {
			return err
		}
		// So `factoryd run` without a doctor preflight also refuses a stale
		// relay_allowed_path_prefix before ever launching the relay -- doctor
		// calls the same function so its own check fails on exactly this.
		if err := ValidateGitHubCopilotWorkerAPI(p.WorkerModelAPI, p.AllowedPathPrefix); err != nil {
			return err
		}
	}
	if p.AuthMode == meter.CredentialModeChatGPTCodex {
		if err := meter.ValidateChatGPTCodexRoute(p.Upstream, p.AllowedPathPrefix, p.WorkerBasePath); err != nil {
			return err
		}
	}
	if p.AuthMode == meter.CredentialModeChatGPTCodex && p.WorkerModelID == "" {
		return errors.New("relay worker model id is required when auth mode is \"chatgpt-codex\": the worker must reach the relay as an OpenAI-compatible provider")
	}
	return nil
}

// validateWorkerModel checks the worker model id and that the usage format matches
// the API the worker speaks.
func (p RoutePolicy) validateWorkerModel() error {
	if strings.ContainsAny(p.WorkerModelID, "\x00\r\n\t") {
		return errors.New("relay worker model id must not contain a control character")
	}
	// WorkerModelID set means the worker talks to this relay as an
	// OpenAI-compatible provider, so every response the relay meters is
	// OpenAI-shaped by construction -- pairing it with UsageFormatAnthropic
	// is not a policy choice, it is a misconfiguration, and one that fails
	// silently and late (found live, 2026-09-08): the Anthropic SSE parser
	// ignores every OpenAI content chunk without complaint, then dies on the
	// terminal "data: [DONE]", so every single request falls back to
	// exhaustUsageBudgets' conservative estimate and the token budget is
	// gone within a dozen requests -- surfacing to the caller as an
	// indistinguishable-from-a-real-outage 429. Reject it at configuration
	// time instead of paying for it a stream at a time.
	workerModelAPI := p.WorkerModelAPI
	if workerModelAPI == "" {
		workerModelAPI = meter.RequestFormatOpenAICompletions
	}
	if p.WorkerModelID != "" && p.UsageFormat == meter.UsageFormatAnthropic {
		return fmt.Errorf("relay usage format must be OpenAI-compatible when a worker model id is set: use %q for Chat Completions or %q for Responses", meter.UsageFormatOpenAI, meter.UsageFormatOpenAIResponses)
	}
	if p.WorkerModelID != "" && workerModelAPI == meter.RequestFormatOpenAIResponses && p.UsageFormat == meter.UsageFormatOpenAI {
		return fmt.Errorf("relay usage format must be %q when worker model API is %q", meter.UsageFormatOpenAIResponses, meter.RequestFormatOpenAIResponses)
	}
	if p.WorkerModelID != "" && workerModelAPI == meter.RequestFormatOpenAICompletions && p.UsageFormat == meter.UsageFormatOpenAIResponses {
		return fmt.Errorf("relay usage format must be %q when worker model API is %q", meter.UsageFormatOpenAI, meter.RequestFormatOpenAICompletions)
	}
	// Enforced here, not only by cmd/factoryd's own flag validation (found
	// via review, Codex, PR #59): a RoutePolicy submitted directly through
	// a Temporal Workflow input bypasses that CLI-level check entirely, and
	// openAICompatWorkerEnvironment's own doc comment already documents
	// why this exact combination (an explicit --provider, which the Pi
	// adapter always passes alongside --model, plus a slash-containing model
	// id) hangs pi's own CLI outright -- worth failing this policy closed
	// before it ever ties up a worker until timeout, not just at the one
	// entry point that happens to validate it early today.
	if strings.Contains(p.WorkerModelID, "/") {
		return errors.New("relay worker model id must not contain a slash: pi's own CLI hangs resolving a slash-containing --model value together with an explicit --provider (which this package always sets) -- alias this model to a clean id on the upstream side instead")
	}
	return nil
}

// validateBudgets checks the request limits, the budgets, the prices and the
// ceilings.
func (p RoutePolicy) validateBudgets() error {
	if p.MaxRequestBytes <= 0 {
		return errors.New("relay maximum request size must be positive")
	}
	if p.RequestsPerMinute <= 0 {
		return errors.New("relay requests-per-minute limit must be positive")
	}
	if p.TokenBudget <= 0 || p.TokenBudgetWindow <= 0 {
		return errors.New("relay token budget and window must be positive")
	}
	if p.CostBudgetMicroUSD <= 0 || p.CostBudgetWindow <= 0 {
		return errors.New("relay cost budget and window must be positive")
	}
	// Zero is a real price (a local model costs nothing); the dollar budget
	// and ceiling then never trip, and the token budget and ceiling above
	// and below still bound the run.
	if p.InputMicroUSDPerMTok < 0 || p.OutputMicroUSDPerMTok < 0 || p.CacheWriteMicroUSDPerMTok < 0 {
		return errors.New("relay token prices must not be negative")
	}
	if p.CachedInputMicroUSDPerMTok < 0 || p.CachedInputMicroUSDPerMTok > p.InputMicroUSDPerMTok {
		return errors.New("relay cached-input price must not be negative or exceed the input price")
	}
	if p.TokenCeiling <= 0 {
		return errors.New("relay token ceiling must be positive")
	}
	if p.CostCeilingMicroUSD <= 0 {
		return errors.New("relay cost ceiling must be positive")
	}
	// A ceiling below its own window budget would trip on the very first
	// window, making the ceiling indistinguishable from the window budget
	// itself and defeating the reason the two are separate controls (see
	// this struct's own doc comment) -- cmd/factoryd's own flag defaults
	// this generously (5x the window budget) for exactly this reason.
	if p.TokenCeiling < p.TokenBudget {
		return errors.New("relay token ceiling must be at least the token budget: a ceiling this low would trip within the first window, collapsing it into a second, redundant window budget instead of an absolute run-scoped one")
	}
	if p.CostCeilingMicroUSD < p.CostBudgetMicroUSD {
		return errors.New("relay cost ceiling must be at least the cost budget: a ceiling this low would trip within the first window, collapsing it into a second, redundant window budget instead of an absolute run-scoped one")
	}
	return nil
}

// ValidateUpstreamScheme rejects an Upstream that would put a real
// credential -- or, when none is configured, still a worker's forwarded
// request -- on the wire in plaintext, unless AllowPlaintextUpstream is set
// AND the upstream targets a private/loopback host. A public http://
// upstream is rejected unconditionally regardless of that field: the
// exemption exists for a local/LAN model endpoint with no real credential
// to protect, never for a public one.
//
// Deliberately separate from Validate above, not folded into it: Validate
// (via validRelayUpstream) has always accepted any absolute http(s) URL
// unconditionally, by design -- RoutePolicy's own doc comment notes it is
// "the credential-free half" of a launchable configuration, reusable by a
// caller that doesn't yet know whether a real credential is even in play.
// The scheme-safety judgment call belongs to whichever entry point is
// actually about to launch a relay with a real (or absent) credential
// attached, which is exactly why every such entry point -- cmd/factoryd's
// CLI flags and RouteSpec.Validate below, itself called from cmd/factoryd and
// the Worker -- must call this explicitly. Exported and exercised from
// both, so the rule cannot drift between them.
func (p RoutePolicy) ValidateUpstreamScheme() error {
	u, err := url.Parse(p.Upstream)
	if err != nil {
		return fmt.Errorf("relay upstream is not a valid URL (got %q): %w", p.Upstream, err)
	}
	if strings.EqualFold(u.Scheme, "https") {
		return nil
	}
	if !strings.EqualFold(u.Scheme, "http") {
		return fmt.Errorf("relay upstream must be an http:// or https:// URL (got %q)", p.Upstream)
	}
	if !p.AllowPlaintextUpstream {
		return fmt.Errorf("relay upstream must be an https:// URL (got %q): a real model API credential must not travel in plaintext; set AllowPlaintextUpstream only for a private/loopback local model endpoint with no real credential to protect", p.Upstream)
	}
	if !isPrivateOrLoopbackHost(u.Host) {
		return fmt.Errorf("AllowPlaintextUpstream only exempts a private or loopback host, not %q: a plaintext upstream reachable off this machine/LAN could still expose a real credential or a worker's request in transit", u.Host)
	}
	return nil
}

// CredentialSafeForUpstream reports whether a real credential is safe to
// send to upstream given allowPlaintextUpstream -- the same rule
// RouteSpec.Validate enforces (found via review, Codex, PR #59, and
// reused here rather than reimplemented, after a later review of the
// onboarding walkthrough found the same gap in a second call site): a
// real credential must never coexist with a plaintext (http://) upstream,
// even one otherwise exempted for a private/loopback host by AllowPlaintextUpstream
// via ValidateUpstreamScheme above -- that exemption exists for a
// credential-FREE local model endpoint, not a credentialed one that
// merely happens to be private. Exported so a caller that hasn't (and
// doesn't need to) assemble a full RouteSpec -- e.g. `factoryd doctor
// -list-models`, which only ever sends a GET, never launches a relay --
// can still apply this exact rule before sending a credential anywhere,
// instead of duplicating it a third time. A malformed upstream URL is
// reported as safe (nil) here, matching RouteSpec.Validate's own
// behavior: URL validity itself is validRelayUpstream/ValidateUpstreamScheme's
// job, not this function's.
func CredentialSafeForUpstream(upstream string, allowPlaintextUpstream, credentialPresent bool) error {
	if !credentialPresent || !allowPlaintextUpstream {
		return nil
	}
	u, err := url.Parse(upstream)
	if err != nil {
		return nil
	}
	if strings.EqualFold(u.Scheme, "http") {
		return errors.New("a real relay credential must not be configured while AllowPlaintextUpstream targets a plaintext (http://) upstream: unset the credential, or use an https:// upstream instead")
	}
	return nil
}

// tailscaleCGNATRange is the IPv4 range Tailscale assigns tailnet addresses
// from (100.64.0.0/10, RFC 6598 "Shared Address Space" for carrier-grade
// NAT) -- net.IP.IsPrivate() does not recognize it (that method only checks
// RFC1918 plus IPv6 ULA), so a real Tailscale-reachable local model
// endpoint (e.g. a Mac Studio on the same tailnet, reached over its
// 100.x.x.x address because a sandboxed worker's own container network
// cannot route to the physical LAN -- found live, 2026-09-07: a colima
// container reached a Tailscale IP fine while the identical LAN IP got a
// bare connection refused) would otherwise fail isPrivateOrLoopbackHost
// even though Tailscale traffic is already WireGuard-encrypted
// end-to-end between the two tailnet identities -- arguably a stronger
// guarantee against a credential or request leaking in transit than a
// bare RFC1918 LAN address already gets by default.
var tailscaleCGNATRange = func() *net.IPNet {
	_, cidr, err := net.ParseCIDR("100.64.0.0/10")
	if err != nil {
		panic(err) // unreachable: the literal above is a valid CIDR
	}
	return cidr
}()

// isPrivateOrLoopbackHost reports whether host (a URL's Host, so possibly
// "host:port") names a loopback address, an RFC1918/ULA private address, a
// link-local address, a Tailscale tailnet address, or the literal hostname
// "localhost" -- the narrow set of upstream targets ValidateUpstreamScheme
// is willing to exempt from the https-only requirement, since only those
// have no real route off the operator's own machine/LAN/tailnet for a
// credential (or, when none is configured, a worker's forwarded request)
// to leak from in transit.
func isPrivateOrLoopbackHost(host string) bool {
	h := host
	if hostOnly, _, err := net.SplitHostPort(host); err == nil {
		h = hostOnly
	}
	if strings.EqualFold(h, "localhost") {
		return true
	}
	ip := net.ParseIP(h)
	if ip == nil {
		return false
	}
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || tailscaleCGNATRange.Contains(ip)
}

// Validate rejects an unusable relay configuration before Docker is invoked:
// the policy's own checks, plus the credential and run identity this
// launchable spec adds.
func (s RouteSpec) Validate() error {
	if err := s.RoutePolicy.Validate(); err != nil {
		return err
	}
	// An empty credential is accepted deliberately: a route's own upstream can
	// target a local/LAN model endpoint with no real credential to inject
	// at all (see routes.<name>.allow_plaintext_upstream). A *present* value
	// is still checked for control characters.
	if strings.ContainsAny(s.APIKey.reveal(), "\x00\r\n") {
		return errors.New("relay API key must not contain a control character")
	}
	if strings.ContainsAny(s.GitHubToken.reveal(), "\x00\r\n") {
		return errors.New("relay GitHub OAuth token must not contain a control character")
	}
	if strings.ContainsAny(s.ChatGPTToken.reveal(), "\x00\r\n") {
		return errors.New("relay ChatGPT access token must not contain a control character")
	}
	if strings.ContainsAny(s.ChatGPTAccountID.reveal(), "\x00\r\n") {
		return errors.New("relay ChatGPT account id must not contain a control character")
	}
	if s.AuthMode == meter.CredentialModeChatGPTCodex && (s.ChatGPTToken.reveal() == "" || s.ChatGPTAccountID.reveal() == "") {
		return errors.New("relay ChatGPT access token and account id are both required when auth mode is \"chatgpt-codex\"")
	}
	if s.AuthMode == meter.CredentialModeGitHubCopilot && s.GitHubToken.reveal() == "" {
		return errors.New("relay GitHub OAuth token is required when auth mode is \"github-copilot\"")
	}
	// A real credential must never coexist with a plaintext upstream, even
	// one otherwise exempted by AllowPlaintextUpstream (found via review,
	// Codex, PR #59): that field is documented as an exemption for "a
	// model endpoint with no real credential to protect", but nothing
	// before this check actually enforced that -- an operator with
	// ANTHROPIC_API_KEY still set in their environment for an unrelated
	// reason, who then opts into plaintext for a private local model, would
	// otherwise have that real key injected into every plaintext request
	// regardless, exactly the exposure the original https-only requirement
	// existed to prevent. This is checked here, not in RoutePolicy.
	// ValidateUpstreamScheme, because the credential lives on RouteSpec,
	// not the credential-free RoutePolicy half.
	// GitHubToken.reveal() != "", not just APIKey.reveal() != "" (found via
	// adversarial review): CredentialModeGitHubCopilot carries its real
	// credential in GitHubToken, never APIKey (see RouteSpec.GitHubToken's
	// own doc comment) -- an APIKey-only check let a github-copilot relay
	// configured with AllowPlaintextUpstream send the GitHub OAuth token
	// (in the token-exchange request) and the exchanged Copilot bearer
	// token (on every forwarded request) over plaintext HTTP, exactly the
	// exposure this check exists to prevent for the static credential.
	if err := CredentialSafeForUpstream(s.Upstream, s.AllowPlaintextUpstream, s.APIKey.reveal() != "" || s.GitHubToken.reveal() != "" || s.ChatGPTToken.reveal() != "" || s.ChatGPTAccountID.reveal() != ""); err != nil {
		return err
	}
	// Found via review (GitHub Codex App local review of the
	// ReconcileRelayOrphans fix, following PR #42's own finding): a merely
	// non-empty, control-character-free RunID isn't enough -- reconciliation
	// below permanently skips any relay container/network whose run label
	// isn't run.ValidID, so a caller launching with an empty or otherwise
	// invalid RunID would recreate the exact credential-leak class this
	// whole mechanism exists to close, just unreclaimable from day one
	// instead of only after a crash.
	if !run.ValidID(s.RunID) {
		return errors.New("relay run id is required and must be a valid run id")
	}
	if strings.ContainsAny(s.RunID, "\x00\r\n\t") {
		return errors.New("relay run id contains an unsafe control character")
	}
	if s.DataDir == "" || !filepath.IsAbs(s.DataDir) {
		return errors.New("relay data directory is required and must be absolute")
	}
	if s.CABundlePath != "" {
		if err := ValidateEgressCABundle(s.CABundlePath); err != nil {
			return err
		}
	}
	return nil
}

// dockerListEitherLabel runs `dockerBinary <listArgs...>` once per kind (and
// per label prefix, current then legacy) in kinds, each call ANDed with the
// data-dir hash filter, and returns their combined
// output, deduplicated by whole line. Docker's CLI ANDs --filter label=
// flags together whenever they name different keys -- confirmed live
// 2026-09-14 (`docker ps --filter label=a=true --filter label=b=true` never
// matched a container carrying only label a) -- so a single `docker ps`/
// `docker network ls` call listing "label=buildgate.relay=true"
// alongside "label=buildgate.registryproxy=true" as ReconcileRelay
// Orphans used to (both call sites) never matched anything: no container or
// network in this codebase ever carries both labels at once, only ever one
// or the other. Matching "relay OR registry-proxy" needs one list call per
// label instead, unioned here.
//
// perCallTimeout bounds each label's own docker invocation independently
// (found via review): ctx is the caller's own outer bound, not
// pre-shrunk to a single call's budget by the caller — giving every label
// the same full perCallTimeout regardless of how long an earlier label's
// call took. A caller that pre-wrapped ctx down to one call's worth of
// deadline before handing it here would silently starve the second (or
// later) label's call under a slow/loaded daemon, reintroducing the same
// "reconciliation finds nothing" failure mode this function exists to fix
// — just from a timeout instead of a filter bug.
func dockerListEitherLabel(ctx context.Context, dockerBinary string, listArgs []string, dataDirHash string, format func(prefix string) string, kinds []string, perCallTimeout time.Duration) (string, error) {
	seen := map[string]bool{}
	var lines []string
	// Per label prefix as well as per kind: the legacy prefix exists only to
	// reclaim resources from a pre-rename factoryd (see legacyLabelPrefix).
	for _, prefix := range labelPrefixes {
		for _, kind := range kinds {
			args := append(append([]string{}, listArgs...),
				"--filter", "label="+prefix+kind+"=true",
				"--filter", "label="+prefix+"data-dir="+dataDirHash,
				"--format", format(prefix))
			callCtx, cancel := context.WithTimeout(ctx, perCallTimeout)
			cmd := exec.CommandContext(callCtx, dockerBinary, args...)
			cmd.Env = dockerClientEnv()
			out, err := cmd.Output()
			cancel()
			if err != nil {
				return "", fmt.Errorf("%v: %w", args, err)
			}
			lines = dedupeLines(lines, seen, string(out))
		}
	}
	return strings.Join(lines, "\n"), nil
}

// ReconcileRelayOrphans finds the registry-proxy containers and networks
// this package launched for dataDir -- and any relay container or network a
// factoryd from before the relay was retired left behind -- and removes the
// ones whose owning run has reached a terminal state, or whose owner
// heartbeat has gone stale: the exact same contract ReconcileOrphans applies
// to worker containers, extended to a sidecar's second resource (its
// internal network). Callers run this at the same startup/periodic points
// ReconcileOrphans already is. A per-run service container is reconciled by
// exactly one sweep, so both kinds share this one.
func ReconcileRelayOrphans(ctx context.Context, dockerBinary, dataDir string) ([]string, error) {
	dataDir, err := filepath.Abs(dataDir)
	if err != nil {
		return nil, fmt.Errorf("resolve sandbox data directory: %w", err)
	}
	if dockerBinary == "" {
		dockerBinary = "docker"
	}
	label := dataDirLabel(dataDir)

	// One list call per label, unioned by dockerListEitherLabel -- extended,
	// not duplicated, so the registry proxy added by
	// internal/sandbox/registryproxy.go is reconciled by this exact same
	// pass, never a second copy of this logic. See that function's own doc
	// comment for why this can't be one `docker ps` call with both filters.
	// ctx itself (not a pre-shrunk child) is passed through: each label's
	// own call gets its own fresh 15s via perCallTimeout, not a shared slice
	// of one combined deadline -- see dockerListEitherLabel's own comment.
	out, err := dockerListEitherLabel(ctx, dockerBinary, []string{"ps", "-a"},
		label,
		func(p string) string {
			return `{{.Names}}\t{{.Label "` + p + `run"}}\t{{.Label "` + p + `registryproxy-shared-network"}}`
		},
		[]string{"relay", "registryproxy"}, 15*time.Second)
	if err != nil {
		return nil, fmt.Errorf("list relay containers: %w", err)
	}

	// sharedNetwork is true only for a registry-proxy container LaunchRegistryProxy
	// attached to its run's own relay network instead of creating one of its
	// own (RegistryProxySpec.ExistingInternalNetwork) -- reconciliation must
	// never attempt to remove a network such a container never created (see
	// removeRelayContainerAndNetwork's own doc comment); every relay
	// container, and every standalone registry-proxy container, carries no
	// such label at all, so this is false for them exactly as before this
	// field existed.
	type container struct {
		name, runID   string
		sharedNetwork bool
	}
	var removed []string
	var errs []error
	var staleCandidates []container
	present := map[string]bool{}

	// Parse every listed container first, then process shared-network
	// containers (a registry-proxy attached to its run's relay network via
	// RegistryProxySpec.ExistingInternalNetwork) before any relay that owns
	// that same network (found via review): dockerListEitherLabel's own
	// per-label listing has no ordering guarantee between the two labels,
	// so a crashed run with both a relay and a registry-proxy sharing one
	// network could see the relay processed first. Removing the relay
	// container's own network then fails while the proxy is still attached
	// to it, and because present[name] is set for every container this scan
	// ever saw (including ones later removed), the orphan-network retry
	// pass below (which skips any network whose derived container name is
	// still in present) wrongly treats that failed removal as already
	// accounted for -- leaking the network until a later pass happens to
	// see the container gone from a fresh `docker ps`. Removing every
	// shared-network container first means its relay's own network has no
	// other attachment left by the time the relay itself is torn down, so
	// the network removal that matters succeeds in this same pass instead
	// of depending on a subsequent one.
	var parsed []container
	for _, line := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
		if line == "" {
			continue
		}
		fields := strings.SplitN(line, "\t", 3)
		name := fields[0]
		present[name] = true
		var runID string
		if len(fields) >= 2 {
			runID = fields[1]
		}
		sharedNetwork := len(fields) >= 3 && fields[2] == "true"
		parsed = append(parsed, container{name: name, runID: runID, sharedNetwork: sharedNetwork})
	}
	sort.SliceStable(parsed, func(i, j int) bool {
		return parsed[i].sharedNetwork && !parsed[j].sharedNetwork
	})

	for _, c := range parsed {
		name, runID, sharedNetwork := c.name, c.runID, c.sharedNetwork
		// Same rationale as ReconcileOrphans: run.ValidID, not just a
		// non-empty check, since this label is Docker-attacker-influenced
		// in principle.
		if runID == "" || !run.ValidID(runID) {
			continue
		}
		record, loadErr := run.Load(dataDir, runID)
		if loadErr != nil {
			// Recorded, not silently skipped (found via review): this
			// container is the one actually holding the relay's
			// credentials, so an operator scanning errs for exactly that
			// signal deserves to see why nothing here reclaimed it.
			errs = append(errs, fmt.Errorf("load run %q for orphaned relay %q: %w", runID, name, loadErr))
			continue
		}
		terminal := record.State == run.StateAccepted ||
			record.State == run.StateQuarantined ||
			(record.State == run.StateHalted && record.HaltConfirmed)
		if !terminal {
			if !ownerHeartbeatStale(dataDir, runID) {
				continue
			}
			staleCandidates = append(staleCandidates, container{name: name, runID: runID, sharedNetwork: sharedNetwork})
			continue
		}
		containerRemoved, networkRemoved, removeErr := removeRelayContainerAndNetwork(ctx, dockerBinary, name, sharedNetwork)
		if removeErr != nil {
			errs = append(errs, fmt.Errorf("reconcile orphaned relay %q (run %q): %w", name, runID, removeErr))
			if containerRemoved {
				// present[name] stays true (set above) even though the
				// container is gone: its paired network's own removal is
				// what failed, and the orphan-network pass below must still
				// treat this network as already accounted for here rather
				// than parsing its own stale confirmation.
				removed = append(removed, name)
			}
			continue
		}
		// present[name] stays true (set above) regardless of whether
		// networkRemoved is non-empty: even a registry-proxy container that
		// owned no network of its own (shared a relay's) still had its
		// container name registered in present above, and the orphan-
		// network pass below keys strictly off network names, never this
		// map, so leaving it true here is a no-op for that case either way.
		removed = append(removed, name)
		if networkRemoved != "" {
			removed = append(removed, networkRemoved)
		}
	}

	// A relay network with no container of the matching name in the
	// listing above needs the exact same terminal/stale run-record check a
	// container gets, not unconditional removal (found via review): between
	// a launch creating the network and creating its paired container --
	// a real, if narrow, window -- a concurrent reconciliation pass would
	// otherwise see only the network and delete it out from under a launch
	// that is still actively in progress. Keying this decision off the
	// network's own buildgate.run label (its run record's owner
	// heartbeat is freshly written for the length of that window) closes
	// the race the same way an actively-owned container is already
	// protected: nothing here is deleted while its run is both non-terminal
	// and has a fresh heartbeat.
	//
	// ctx directly, each label call getting its own fresh 15-second budget
	// via dockerListEitherLabel's own perCallTimeout: this listing and the
	// container listing above share nothing but ctx, and a slow
	// `docker network ls` must not be charged against however much of a
	// shared deadline the container listing/removal work already spent —
	// same reasoning as the container listing's own call above.
	//
	// Same relay-OR-registryproxy union as the container listing above, and
	// the same reason a single `docker network ls` filtering on both labels
	// can't express it -- see dockerListEitherLabel's own doc comment.
	netOut, netErr := dockerListEitherLabel(ctx, dockerBinary, []string{"network", "ls"},
		label,
		func(p string) string { return `{{.Name}}\t{{.Label "` + p + `run"}}` },
		[]string{"relay", "registryproxy"}, 15*time.Second)
	type networkOrphan struct{ name, runID string }
	var staleNetworkCandidates []networkOrphan
	if netErr != nil {
		// Recorded, not returned immediately (found via review, GitHub
		// Codex App, PR #42, seventh round -- a consequence of moving this
		// listing ahead of the shared debounce below): staleCandidates
		// (container) may already hold real work from the listing above,
		// and a failure to list *networks* must not cost that already-
		// gathered container reconciliation, which the shared debounce
		// block below still processes regardless of staleNetworkCandidates
		// staying empty here.
		errs = append(errs, fmt.Errorf("list relay networks: %w", netErr))
	}
	// netErr == nil only: cmd.Output() can return a partially-read,
	// truncated stdout alongside a non-nil error (found via review) --
	// parsing it anyway risks a truncated line driving a real removal (or
	// quarantineAbandonedRun) off a name/run-id pair Docker never actually
	// reported in full.
	if netErr == nil {
		for _, line := range strings.Split(strings.TrimRight(string(netOut), "\n"), "\n") {
			if line == "" {
				continue
			}
			fields := strings.SplitN(line, "\t", 2)
			name := fields[0]
			// factoryd-relay-egress/factoryd-registryproxy-egress are never
			// matched here regardless: they never carry the
			// buildgate.relay/registryproxy=true label this listing
			// filters on in the first place (see ensureRelayEgressNetwork/
			// ensureRegistryProxyEgressNetwork), so this prefix check exists
			// only to tell a per-run relay network from a per-run
			// registry-proxy network, not to exclude the egress networks.
			var containerName string
			switch {
			case strings.HasPrefix(name, "factoryd-registryproxy-"):
				containerName = strings.Replace(name, "factoryd-registryproxy-", "factoryd-registryproxy-container-", 1)
			case strings.HasPrefix(name, "factoryd-relay-"):
				containerName = strings.Replace(name, "factoryd-relay-", "factoryd-relay-container-", 1)
			default:
				continue
			}
			if present[containerName] {
				continue
			}
			var runID string
			if len(fields) == 2 {
				runID = fields[1]
			}
			if runID == "" || !run.ValidID(runID) {
				continue
			}
			record, loadErr := run.Load(dataDir, runID)
			if loadErr != nil {
				// Recorded, not silently skipped (found via review): this
				// network's paired container -- the one actually holding the
				// relay's credentials -- is left running with nothing here to
				// reclaim it, and an operator scanning errs for exactly that
				// signal deserves to see why, rather than this scan quietly
				// doing nothing about a relay it could not even classify.
				errs = append(errs, fmt.Errorf("load run %q for orphaned relay network %q: %w", runID, name, loadErr))
				continue
			}
			terminal := record.State == run.StateAccepted ||
				record.State == run.StateQuarantined ||
				(record.State == run.StateHalted && record.HaltConfirmed)
			if terminal {
				// cleanupNetwork's own remove-then-confirm protocol (found via
				// review, GitHub Codex App, PR #42, sixth round), not a bare
				// `network rm` trusted at face value for its own exit status:
				// the daemon can remove the network while the CLI itself loses
				// the response, the same ambiguity every other removal in this
				// package already treats as remove-and-confirm.
				if removeErr := removeRelayNetwork(ctx, dockerBinary, name); removeErr != nil {
					errs = append(errs, fmt.Errorf("reconcile orphaned relay network %q (run %q): %w", name, runID, removeErr))
					continue
				}
				removed = append(removed, name)
				continue
			}
			if !ownerHeartbeatStale(dataDir, runID) {
				continue
			}
			staleNetworkCandidates = append(staleNetworkCandidates, networkOrphan{name: name, runID: runID})
		}
	}

	if len(staleCandidates) > 0 || len(staleNetworkCandidates) > 0 {
		// One shared wait for both candidate lists, not one per list
		// (found via review, GitHub Codex App, PR #42, seventh round): an
		// earlier version of this fix gave the network-only path its own
		// separate debounce, so a scan containing both a stale container
		// and a stale network-only orphan paid ownerHeartbeatInterval
		// twice serially (30s with production defaults) -- doubling
		// factoryd's own startup delay (this runs synchronously before
		// dialing Temporal) and risking periodic reconciliation overrunning
		// its own cadence. Same one-wait-for-the-whole-batch rationale
		// ReconcileOrphans' own debounce already documents: N abandoned
		// candidates should not each (or, here, each *type* of candidate)
		// add their own ownerHeartbeatInterval of delay.
		select {
		case <-ownerDebounceAfter(ownerHeartbeatInterval):
		case <-ctx.Done():
			for _, c := range staleCandidates {
				errs = append(errs, fmt.Errorf("reconcile abandoned relay %q (run %q): staleness debounce wait: %w", c.name, c.runID, ctx.Err()))
			}
			for _, c := range staleNetworkCandidates {
				errs = append(errs, fmt.Errorf("reconcile abandoned relay network %q (run %q): staleness debounce wait: %w", c.name, c.runID, ctx.Err()))
			}
			return removed, errors.Join(errs...)
		}
		for _, c := range staleCandidates {
			if !ownerHeartbeatStale(dataDir, c.runID) {
				continue
			}
			// Found via review (GitHub Codex App, PR #42, third round):
			// checked before removal, not after -- an earlier version
			// removed the relay first and checked worker presence
			// afterward, so a transient failure of this query alone left
			// the run stranded with its last discoverable anchor (the
			// relay) already gone and nothing quarantined. Checking first
			// means that failure instead just skips this run for this
			// pass, relay included, so the next reconciliation pass has
			// exactly the same resources to work with and can simply retry.
			present, presentErr := WorkerContainerPresentForRun(ctx, dockerBinary, dataDir, c.runID)
			if presentErr != nil {
				errs = append(errs, fmt.Errorf("check worker container for run %q before reclaiming relay %q: %w", c.runID, c.name, presentErr))
				continue
			}
			containerRemoved, networkRemoved, removeErr := removeRelayContainerAndNetwork(ctx, dockerBinary, c.name, c.sharedNetwork)
			if removeErr != nil {
				errs = append(errs, fmt.Errorf("reconcile abandoned relay %q (run %q): %w", c.name, c.runID, removeErr))
				if containerRemoved {
					removed = append(removed, c.name)
				}
				continue
			}
			// The comment this replaces assumed ReconcileOrphans always has
			// a worker container of its own to find and quarantine for this
			// same runID -- true only if that container still exists. A
			// worker container already exited and removed by its own --rm
			// before factoryd crashed leaves ReconcileOrphans nothing to
			// discover, so this relay is the only orphan reconciliation
			// ever sees for that run; without quarantining here too, the
			// durable record stays permanently non-terminal with no
			// resource left for any later scan to act on.
			// quarantineAbandonedRun's own lock and terminal-state recheck
			// make it safe to call even when a concurrent worker-side
			// reconciliation is also quarantining the same run right now.
			//
			// Only when !present: a worker container that ReconcileOrphans
			// (which always runs immediately before this function at both
			// of its call sites) failed to remove this same pass would
			// still be present here, and quarantining marks HaltConfirmed
			// true -- "nothing left that could still be running" -- which
			// would be false while that worker container remains. Leave
			// the run non-terminal in that case; only the relay is
			// reclaimed this pass, and the worker container stays
			// reachable for a later ReconcileOrphans pass to finish.
			if !present {
				if quarantineErr := quarantineAbandonedRun(dataDir, c.runID); quarantineErr != nil {
					errs = append(errs, fmt.Errorf("quarantine abandoned run %q after removing relay %q: %w", c.runID, c.name, quarantineErr))
				}
			}
			removed = append(removed, c.name)
			if networkRemoved != "" {
				removed = append(removed, networkRemoved)
			}
		}
		for _, c := range staleNetworkCandidates {
			if !ownerHeartbeatStale(dataDir, c.runID) {
				continue
			}
			// Checked before removal, not after -- same reordering as the
			// container path above, and for the same reason: a transient
			// failure of this query must not remove the network out from
			// under a run this query itself couldn't finish evaluating,
			// since that network is this run's own last discoverable
			// anchor.
			present, presentErr := WorkerContainerPresentForRun(ctx, dockerBinary, dataDir, c.runID)
			if presentErr != nil {
				errs = append(errs, fmt.Errorf("check worker container for run %q before reclaiming relay network %q: %w", c.runID, c.name, presentErr))
				continue
			}
			if removeErr := removeRelayNetwork(ctx, dockerBinary, c.name); removeErr != nil {
				errs = append(errs, fmt.Errorf("reconcile abandoned relay network %q (run %q): %w", c.name, c.runID, removeErr))
				continue
			}
			// A crash between a launch's network create and its paired
			// container starting leaves exactly this shape -- a stale
			// non-terminal run with only this now-removed network as
			// evidence, no container of its own ever existed for this
			// reconciler or ReconcileOrphans to find. Without quarantining
			// here too (only when !present, same as the container path
			// above), the durable record stays permanently non-terminal
			// with nothing left for any later scan to rediscover.
			if !present {
				if quarantineErr := quarantineAbandonedRun(dataDir, c.runID); quarantineErr != nil {
					errs = append(errs, fmt.Errorf("quarantine abandoned run %q after removing relay network %q: %w", c.runID, c.name, quarantineErr))
				}
			}
			removed = append(removed, c.name)
		}
	}
	return removed, errors.Join(errs...)
}

// removeRelayContainerAndNetwork removes a relay or registry-proxy container
// and the network paired with it (the network name is deterministic from the
// container's -- see relayNetworkNameForContainer). It reports
// containerRemoved separately from the returned error (found via review):
// the container can be removed successfully and confirming the paired
// network's own removal can then fail, and a caller that only checked the
// returned error for whether to record this container in its own "removed"
// accounting would then under-report a resource that really is gone.
//
// sharedNetwork, when true, skips network cleanup entirely and returns an
// empty networkRemoved: this container never created or owned a network of
// its derived name to begin with -- the shape a registry-proxy container
// attached to an existing network takes (RegistryProxySpec.
// ExistingInternalNetwork). The caller (ReconcileRelayOrphans) determines
// this from the container's own buildgate.registryproxy-shared-network
// label, set at launch time -- never inferred here from a network-presence
// probe, which a minimal Docker CLI response (real or faked in a test)
// cannot reliably distinguish from "not yet checked."
func removeRelayContainerAndNetwork(ctx context.Context, dockerBinary, containerName string, sharedNetwork bool) (containerRemoved bool, networkRemoved string, err error) {
	if err := removeRelayContainer(ctx, dockerBinary, containerName); err != nil {
		return false, "", err
	}
	if sharedNetwork {
		return true, "", nil
	}
	networkName := relayNetworkNameForContainer(containerName)
	if err := removeRelayNetwork(ctx, dockerBinary, networkName); err != nil {
		return true, "", err
	}
	return true, networkName, nil
}

// removeRelayContainer removes a container and confirms it is gone. Every
// error it returns wraps ErrCleanupUnconfirmed.
func removeRelayContainer(ctx context.Context, dockerBinary, containerName string) error {
	_, removeErr := runRelayDocker(ctx, dockerBinary, "remove relay container", "rm", "-f", containerName)
	present, checkErr := relayContainerPresent(ctx, dockerBinary, containerName)
	if checkErr != nil {
		return fmt.Errorf("%w: confirm relay container %q removal: %v", ErrRelayCleanupUnconfirmed, containerName, checkErr)
	}
	if present {
		if removeErr != nil {
			return fmt.Errorf("%w: remove relay container %q: command failed and container remains", ErrRelayCleanupUnconfirmed, containerName)
		}
		return fmt.Errorf("%w: relay container %q remains after removal", ErrRelayCleanupUnconfirmed, containerName)
	}
	// An already-gone container is safe to treat as removed: the positive
	// empty listing above still makes the result fail closed.
	return nil
}

// removeRelayNetwork removes a network and confirms it is gone. Every error
// it returns wraps ErrCleanupUnconfirmed.
func removeRelayNetwork(ctx context.Context, dockerBinary, networkName string) error {
	_, removeErr := runRelayDocker(ctx, dockerBinary, "remove relay network", "network", "rm", networkName)
	present, checkErr := relayNetworkPresent(ctx, dockerBinary, networkName)
	if checkErr != nil {
		return fmt.Errorf("%w: confirm relay network %q removal: %v", ErrRelayCleanupUnconfirmed, networkName, checkErr)
	}
	if present {
		if removeErr != nil {
			return fmt.Errorf("%w: remove relay network %q: command failed and network remains", ErrRelayCleanupUnconfirmed, networkName)
		}
		return fmt.Errorf("%w: relay network %q remains after removal", ErrRelayCleanupUnconfirmed, networkName)
	}
	return nil
}

// relayNetworkNameForContainer derives a relay container's paired network
// name — the two are always created together with the same suffix —
// without an extra Docker round trip to look it up.
func relayNetworkNameForContainer(containerName string) string {
	// A registry-proxy container's name is checked first: it happens to
	// also match the relay branch's own "factoryd-relay-container-"
	// replacement as a no-op substring test would not, but keeping the more
	// specific prefix first avoids relying on that -- see
	// internal/sandbox/registryproxy.go for where these container/network
	// names are actually constructed.
	if strings.HasPrefix(containerName, "factoryd-registryproxy-container-") {
		return strings.Replace(containerName, "factoryd-registryproxy-container-", "factoryd-registryproxy-", 1)
	}
	return strings.Replace(containerName, "factoryd-relay-container-", "factoryd-relay-", 1)
}

func relayContainerPresent(ctx context.Context, dockerBinary, name string) (bool, error) {
	out, err := runRelayDocker(ctx, dockerBinary, "confirm relay container removal", "ps", "-a", "--filter", "name=^/"+name+"$", "--format", "{{.Names}}")
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(string(out)) != "", nil
}

func relayNetworkPresent(ctx context.Context, dockerBinary, name string) (bool, error) {
	out, err := runRelayDocker(ctx, dockerBinary, "confirm relay network removal", "network", "ls", "--filter", "name=^"+name+"$", "--format", "{{.Name}}")
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(string(out)) != "", nil
}

func runRelayDocker(ctx context.Context, dockerBinary, operation string, args ...string) ([]byte, error) {
	boundedCtx, cancel := context.WithTimeout(ctx, relayDockerCommandTimeout)
	defer cancel()
	cmd := exec.CommandContext(boundedCtx, dockerBinary, args...)
	cmd.Env = dockerClientEnv()
	out, err := cmd.Output()
	if err == nil {
		return out, nil
	}
	if boundedCtx.Err() != nil {
		return nil, fmt.Errorf("%s timed out: %w", operation, boundedCtx.Err())
	}
	// Do not include stderr or argv here: Docker error output may echo
	// command material.
	return nil, fmt.Errorf("%s failed: %w", operation, err)
}

// confirmRelayRunning reports an error unless the named container is running.
func confirmRelayRunning(ctx context.Context, dockerBinary, containerName string) error {
	out, err := runRelayDocker(ctx, dockerBinary, "confirm relay container running state", "inspect", "--type", "container", "--format", "{{.State.Running}}", containerName)
	if err != nil {
		return fmt.Errorf("relay container running state could not be confirmed: %w", err)
	}
	if strings.TrimSpace(string(out)) != "true" {
		return errors.New("relay container did not remain running")
	}
	return nil
}

// relayReadinessPollInterval and relayReadinessTimeout bound the registry
// proxy's readiness polling of its container's own logs.
var (
	relayReadinessPollInterval = 100 * time.Millisecond
	relayReadinessTimeout      = 5 * time.Second
)

func relayImageDigest(image string) (string, bool) {
	name, digest, found := strings.Cut(image, "@sha256:")
	if !found || name == "" || digest == "" || len(digest) != 64 {
		return "", false
	}
	for _, c := range digest {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return "", false
		}
	}
	if strings.HasPrefix(image, "-") || strings.ContainsAny(image, " \t\r\n") {
		return "", false
	}
	return "sha256:" + digest, true
}

func validRelayUpstream(raw string) bool {
	if raw == "" || strings.ContainsAny(raw, "\x00\r\n\t ") {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil || !u.IsAbs() || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return false
	}
	return strings.EqualFold(u.Scheme, "http") || strings.EqualFold(u.Scheme, "https")
}

// relayAuditUpstream intentionally drops the configured path. The path is
// needed by the relay process itself, but arbitrary path material can carry a
// provider credential or other secret and therefore is not audit-safe.
func relayAuditUpstream(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Scheme) + "://" + u.Host
}

var relaySequence atomic.Uint64

func relayUniqueSuffix() string {
	seq := relaySequence.Add(1)
	var random [6]byte
	if _, err := rand.Read(random[:]); err != nil {
		return fmt.Sprintf("%x-%x", time.Now().UnixNano(), seq)
	}
	return fmt.Sprintf("%x-%x-%s", time.Now().UnixNano(), seq, hex.EncodeToString(random[:]))
}

// ValidateGitHubCopilotWorkerAPI enforces meter.CredentialModeGitHubCopilot's
// own worker-API/path-prefix pairing: relay_worker_api picks which of
// Copilot's two OpenAI-compatible paths (meter.CopilotChatCompletionsPath
// or meter.CopilotResponsesPath) every forwarded request goes to (see
// meter.CopilotAllowedPathPrefixFor), so an explicit
// relay_allowed_path_prefix that doesn't actually cover that path -- most
// often a leftover from before relay_worker_api was switched -- would 404
// every request rather than fail at launch. Checked with the relay's own
// request-admission semantics (meter.HasPathPrefix: a trailing "/"
// trimmed, "" or "/" allowing every path), not an exact-string compare,
// so an operator-set prefix merely broader than what workerAPI needs
// (e.g. "/", or the exact path with a trailing slash) still passes.
// Shared by RoutePolicy.Validate and factoryd doctor so doctor fails on
// exactly what a real run refuses.
func ValidateGitHubCopilotWorkerAPI(workerAPI, allowedPathPrefix string) error {
	wantPath := meter.CopilotAllowedPathPrefixFor(workerAPI)
	if meter.HasPathPrefix(wantPath, allowedPathPrefix) {
		return nil
	}
	return fmt.Errorf("the route's allowed_path_prefix %q does not cover %q, which the model's api %q forwards every request to: set routes.<route>.allowed_path_prefix to %q (or remove it so it defaults from models.<model>.api)", allowedPathPrefix, wantPath, workerAPI, wantPath)
}

// RunRelaySpend returns the tokens (input plus output) and cost in micro-USD
// the run has spent so far: what the meter recorded for every sandbox the
// run launched (RecordedMeterSpend).
func RunRelaySpend(dataDir, runID string) (tokens, costMicroUSD int64, err error) {
	return RecordedMeterSpend(dataDir, runID)
}
