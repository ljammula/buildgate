// Package conformity holds the pure logic behind the two-phase
// spec-conformity-review design -- shared between cmd/factoryd's direct
// path (run_ticket.go) and the Temporal path (internal/workflow) so the
// two execution engines can never silently diverge on how
// agent/pi/scripts/conformity_review.py is invoked or how its relay
// budget is derived. See run_ticket.go's own "conformity review, phase
// 2" doc comment for the full incident this design exists to fix: a
// spec-drafted acceptance criterion can ask the reviewer to check
// something about the commit itself, which can only be evaluated
// meaningfully against an already-committed workspace, and .git is
// mounted read-only inside the sandbox unconditionally -- so the review
// has to run as its own, separate sandboxed launch, after the host-side
// safety-net commit has already landed, not folded into build_app.py's
// own single launch.
package conformity

import "buildgate/internal/sandbox"

// ScriptName is agent/pi/scripts/conformity_review.py's own filename --
// both execution paths resolve its full path the same way they resolve
// build_app.py's (a sibling file in the same harness directory), so
// there is nothing else to share here beyond the name itself.
const ScriptName = "conformity_review.py"

// Args constructs the argv passed to conformity_review.py.
// criteriaHostPath is passed as this launch's own specPath by both
// callers (not extraRunInput): the script has no separate --spec of its
// own to share a staged directory with, so the criteria file is staged
// the same way build_app.py's own --spec normally is -- see each
// caller's own launch closure for the staging mechanism.
// thinking is the review role's Pi reasoning-effort level (RoleConfig.
// Thinking, resolved via internal/modelrole) -- empty when roles.review
// is unset, which omits --thinking entirely and preserves today's
// argv byte-for-byte.
// harness is the review role's harness name (the script's --harness flag,
// which selects the coding-agent binary), always passed.
func Args(script, workspace, criteriaHostPath, conformityPolicy, baseSHA, thinking, harness string) []string {
	args := []string{
		script,
		"--workspace", workspace,
		"--spec-acceptance-criteria", criteriaHostPath,
		"--conformity-policy", conformityPolicy,
	}
	if baseSHA != "" {
		args = append(args, "--review-base-sha", baseSHA)
	}
	if thinking != "" {
		args = append(args, "--thinking", thinking)
	}
	args = append(args, "--harness", harness)
	return args
}

// ReviewRoute carries roles.review's fully resolved relay worker model
// fields for the conformity phase's own, separate relay launch -- see
// PhaseRelaySpec's own doc comment. A ReviewRoute built in cmd/factoryd
// (ReviewRelayFromPolicy, in run_ticket.go) always resolves through
// modelrole.SelectRoute and so always sets Upstream/AuthMode/credentials
// too (see PhaseRelaySpec's own Upstream != "" branch); the Temporal
// path's own wire copy (workflow.RunWorkflowInput.ReviewRelayPolicy)
// carries only the credential-free half, resolved fresh by the executing
// Worker (Activities.CheckRoute/ResolveRouteCredentials) -- never a
// credential value, so it is safe to persist in Temporal Event History.
type ReviewRoute struct {
	WorkerModelID        string
	WorkerModelAPI       string
	WorkerBasePath       string
	WorkerModelExtraJSON string
	UsageFormat          string
	AllowedPathPrefix    string
	// Upstream, when non-empty, marks this ReviewRoute as resolved
	// through routes:/models: (modelrole.SelectRoute) with its own
	// credential already attached -- the Temporal path's own wire copy
	// (workflow.RunWorkflowInput.ReviewRelayPolicy) leaves this empty,
	// resolving its route fresh, Worker-side, instead (see this struct's
	// own doc comment). When set, PhaseRelaySpec also
	// replaces base's own Upstream/AuthMode/UpstreamAuthHeader/Route/
	// Billing/credentials with these -- a routes: mode review route can
	// be a completely different upstream and credential than the build's
	// own relay, unlike legacy mode's review, which only ever re-derives
	// the build relay's own worker-model fields.
	Upstream           string
	AuthMode           string
	UpstreamAuthHeader string
	Route              string
	Billing            string
	APIKey             sandbox.RouteSecret
	GitHubToken        sandbox.RouteSecret
	ChatGPTToken       sandbox.RouteSecret
	ChatGPTAccountID   sandbox.RouteSecret
	// AllowPlaintextUpstream/AllowUnauthenticatedUpstream/the four *MicroUSDPerMTok prices
	// are also route-/model-derived (a routes: mode review route can be
	// a plaintext local model with no real credential, at different
	// prices than the build's own route) -- carried only alongside
	// Upstream (see PhaseRelaySpec's own doc comment): a legacy
	// ReviewRoute never sets Upstream, so these stay at their zero value
	// and PhaseRelaySpec never reads them in that case.
	AllowPlaintextUpstream       bool
	AllowUnauthenticatedUpstream bool
	InputMicroUSDPerMTok         int64
	CachedInputMicroUSDPerMTok   int64
	CacheWriteMicroUSDPerMTok    int64
	OutputMicroUSDPerMTok        int64
}

// ReviewRelayFromPolicy builds a *ReviewRoute carrying p's full,
// routes:-mode identity (Upstream/AuthMode/UpstreamAuthHeader/Route/
// Billing and the plaintext/no-credential/pricing fields, alongside the
// six worker-model fields every ReviewRoute carries) plus the given,
// already-resolved credentials -- the one shared assembly cmd/factoryd (routes: mode's own reviewRelay
// block) and the Temporal Worker's RunReviewStepActivity (routes: mode's
// own review binding) use, so they can't drift on
// which of p's fields a routes:-mode ReviewRoute carries.
func ReviewRelayFromPolicy(p sandbox.RoutePolicy, apiKey, githubToken, chatGPTToken, chatGPTAccountID sandbox.RouteSecret) *ReviewRoute {
	return &ReviewRoute{
		WorkerModelID:                p.WorkerModelID,
		WorkerModelAPI:               p.WorkerModelAPI,
		WorkerBasePath:               p.WorkerBasePath,
		WorkerModelExtraJSON:         p.WorkerModelExtraJSON,
		UsageFormat:                  p.UsageFormat,
		AllowedPathPrefix:            p.AllowedPathPrefix,
		Upstream:                     p.Upstream,
		AuthMode:                     p.AuthMode,
		UpstreamAuthHeader:           p.UpstreamAuthHeader,
		Route:                        p.Route,
		Billing:                      p.Billing,
		APIKey:                       apiKey,
		GitHubToken:                  githubToken,
		ChatGPTToken:                 chatGPTToken,
		ChatGPTAccountID:             chatGPTAccountID,
		AllowPlaintextUpstream:       p.AllowPlaintextUpstream,
		AllowUnauthenticatedUpstream: p.AllowUnauthenticatedUpstream,
		InputMicroUSDPerMTok:         p.InputMicroUSDPerMTok,
		CachedInputMicroUSDPerMTok:   p.CachedInputMicroUSDPerMTok,
		CacheWriteMicroUSDPerMTok:    p.CacheWriteMicroUSDPerMTok,
		OutputMicroUSDPerMTok:        p.OutputMicroUSDPerMTok,
	}
}

// PhaseRelaySpec derives the two-phase design's own second, separate
// relay launch's RouteSpec from the run's original one plus what phase 1
// (build) already consumed. base was assembled once, before build's own
// launch, with this run's FULL configured lifetime ceiling -- correct
// for that one launch, but launching a second relay-backed container
// with that same full ceiling again would let this run's real total
// spend reach up to 2x its configured ceiling.
//
// review, when non-nil, replaces base's own copies of its six worker-
// model fields outright -- the caller passes roles.review's fully
// resolved fields (modelrole.SelectRoute's own result for roles.review)
// when roles.review is set. nil (roles.review unset) keeps base's own
// fields entirely unchanged -- this function itself never re-derives
// anything, only copies verbatim and applies the ceiling arithmetic
// below.
//
// review.Upstream != "" (cmd/factoryd's fully-resolved
// ReviewRoute, credential included -- see that struct's own doc comment
// for why the workflow-input wire copy leaves this empty instead)
// additionally replaces EVERY route-/model-derived field the review
// route can legitimately differ on from the build's own: upstream, auth
// mode, header, route, billing, credentials, AllowPlaintextUpstream/
// AllowUnauthenticatedUpstream (a review route can be a plaintext local
// model with no real credential even when the build's own route is not),
// and all four *MicroUSDPerMTok prices (a review route can be priced
// differently than the build's). Only the caller's own ceiling/window-
// budget arithmetic below stays base's -- a run-level ceiling shared
// across build and review by design (see this function's own doc
// comment). The legacy (review.Upstream == "") path deliberately leaves
// per-token prices as base's own: per-role pricing only exists in
// routes: mode.
func PhaseRelaySpec(base sandbox.RouteSpec, review *ReviewRoute, consumedTokens, consumedCostMicroUSD int64) sandbox.RouteSpec {
	spec := base
	if review != nil {
		spec.WorkerModelID = review.WorkerModelID
		spec.WorkerModelAPI = review.WorkerModelAPI
		spec.WorkerBasePath = review.WorkerBasePath
		spec.WorkerModelExtraJSON = review.WorkerModelExtraJSON
		spec.UsageFormat = review.UsageFormat
		spec.AllowedPathPrefix = review.AllowedPathPrefix
		if review.Upstream != "" {
			spec.Upstream = review.Upstream
			spec.AuthMode = review.AuthMode
			spec.UpstreamAuthHeader = review.UpstreamAuthHeader
			spec.Route = review.Route
			spec.Billing = review.Billing
			spec.APIKey = review.APIKey
			spec.GitHubToken = review.GitHubToken
			spec.ChatGPTToken = review.ChatGPTToken
			spec.ChatGPTAccountID = review.ChatGPTAccountID
			spec.AllowPlaintextUpstream = review.AllowPlaintextUpstream
			spec.AllowUnauthenticatedUpstream = review.AllowUnauthenticatedUpstream
			spec.InputMicroUSDPerMTok = review.InputMicroUSDPerMTok
			spec.CachedInputMicroUSDPerMTok = review.CachedInputMicroUSDPerMTok
			spec.CacheWriteMicroUSDPerMTok = review.CacheWriteMicroUSDPerMTok
			spec.OutputMicroUSDPerMTok = review.OutputMicroUSDPerMTok
		}
	}
	tokenCeiling, tokenBudget := ClampRemainingRelayBudget(int64(spec.TokenCeiling), int64(spec.TokenBudget), consumedTokens)
	spec.TokenCeiling, spec.TokenBudget = int(tokenCeiling), int(tokenBudget)
	spec.CostCeilingMicroUSD, spec.CostBudgetMicroUSD = ClampRemainingRelayBudget(spec.CostCeilingMicroUSD, spec.CostBudgetMicroUSD, consumedCostMicroUSD)
	return spec
}

// ClampRemainingRelayBudget is PhaseRelaySpec's own shared arithmetic,
// applied identically to the token pair and the cost pair so a future
// correction to it (the floor value, or the shrink-to-fit step) has
// exactly one place to land -- an earlier version of this logic
// duplicated it in parallel for tokens and cost, which is exactly the
// "fixed one copy, not the other" shape that produced a real bug here
// (found via adversarial review, 2026-09-17: clamping a near-exhausted
// remaining ceiling UP to at least the window budget, to satisfy
// RouteSpec.Validate's own ceiling>=budget invariant, could let a run's
// real total spend exceed its one configured lifetime ceiling -- exactly
// the overrun this whole function exists to prevent).
//
// newCeiling is ceiling-consumed, floored at 1 (RouteSpec.Validate
// rejects a non-positive ceiling outright); newBudget is budget, shrunk
// down to newCeiling if it would otherwise exceed it (Validate also
// requires ceiling >= budget) -- never the reverse: this never raises
// newCeiling above the true remaining amount.
func ClampRemainingRelayBudget(ceiling, budget, consumed int64) (newCeiling, newBudget int64) {
	newCeiling = ceiling - consumed
	if newCeiling < 1 {
		newCeiling = 1
	}
	newBudget = budget
	if newBudget > newCeiling {
		newBudget = newCeiling
	}
	return newCeiling, newBudget
}
