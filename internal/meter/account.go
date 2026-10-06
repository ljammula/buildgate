package meter

import (
	"log"
	"math"
	"time"
)

// Prices are the per-token prices in micro-USD per 1,000,000 tokens -- e.g.
// $0.20 per million input tokens is 200000. See tokenCostMicroUSD.
type Prices struct {
	InputMicroUSDPerMTok       int64
	CachedInputMicroUSDPerMTok int64
	CacheWriteMicroUSDPerMTok  int64
	OutputMicroUSDPerMTok      int64
}

// AccountConfig is what NewAccount builds an Account from. The caller has
// already validated every value and resolved the windows (relay.NewServer
// does both): NewAccount never fails and never defaults a window.
type AccountConfig struct {
	// TokenBudget/TokenBudgetWindow: sliding token window, disabled when
	// TokenBudget is zero or negative.
	TokenBudget       int
	TokenBudgetWindow time.Duration
	// CostBudgetMicroUSD/CostBudgetWindow: sliding cost window, disabled when
	// CostBudgetMicroUSD is zero or negative.
	CostBudgetMicroUSD int64
	CostBudgetWindow   time.Duration
	// TokenCeiling/CostCeilingMicroUSD: absolute run-scoped ceilings,
	// disabled when zero or negative.
	TokenCeiling        int
	CostCeilingMicroUSD int64
	// Prices as configured; NewAccount defaults an unset cache rate to the
	// input price.
	Prices Prices
	// UsageMarker is embedded verbatim in every "usage: " line AddUsage logs.
	UsageMarker string
	Logger      *log.Logger
	// Ledger is the open usage ledger, or nil when none was configured.
	Ledger *Ledger
	// Now is the clock AddUsage stamps windows and ledger records with;
	// nil means time.Now.
	Now func() time.Time
}

// Account is the usage accounting of one relay process: its token and cost
// sliding windows, its absolute ceilings, its prices, its cumulative totals,
// its highest requested reasoning effort and the ledger it appends to. It does
// no locking of its own: the caller serializes every method (relay.Server
// holds budgetMu across a whole metered request).
type Account struct {
	tokenLimiter *WindowLimiter
	costLimiter  *WindowLimiter
	tokenCeiling *ceilingCounter
	costCeiling  *ceilingCounter
	usageMarker  string
	prices       Prices
	logger       *log.Logger
	ledger       *Ledger
	now          func() time.Time
	// cumulative{Input,Output}Tokens/cumulativeCostMicroUSD are this
	// process's whole-lifetime consumed totals -- distinct from the window
	// limiters' own totals (which prune) and the ceiling counters' totals
	// (which are gated on TokenCeiling/CostCeilingMicroUSD actually being
	// configured). These are tracked whenever ANY usage accounting runs at
	// all (see metersUsage in relay.ServeHTTP), so Usage() and the "usage:" log
	// line below (see UsageLogPrefix's own doc comment in
	// internal/sandbox/relay.go for why this is the chosen control path) can
	// report real consumption even for a relay configured with windows but
	// no ceiling. cumulativeInputTokens is the WEIGHTED figure (see
	// cachedInputWeightPercent) -- a cache-hit input token already counts
	// for less here, in the token window/ceiling, and in every consumed-
	// token figure derived from this field or the usage ledger; only
	// cumulativeCostMicroUSD is computed from unweighted token counts.
	cumulativeInputTokens  int64
	cumulativeOutputTokens int64
	cumulativeCostMicroUSD int64
	// highestReasoningEffort is the HIGHEST requested reasoning effort
	// parseRequestedReasoningEffort has extracted from any forwarded
	// request so far, ranked by reasoningEffortRank's own doc comment
	// (not "the last one seen" -- a later, lower-effort request must never
	// silently hide an earlier, higher one from the authoritative "usage: "
	// log line) -- only ever updated from within AddUsage. Empty until the
	// first meterable request that asked for one.
	highestReasoningEffort string
	// reasoningEffortAnomaly is a sticky flag: once any single request's
	// own parsed effort was "other" (an unrecognized/malformed value --
	// see parseRequestedReasoningEffort's own doc comment), this stays
	// true for the rest of this process's life, regardless of what
	// highestReasoningEffort itself ends up holding. reasoningEffortRank
	// deliberately ranks "other" below every real level so an anomaly
	// never masks a real, lower effort value as the "highest" -- this
	// flag is the separate signal that an anomaly happened at all, since
	// the ranked value alone can no longer show that once a real level
	// has since taken over highestReasoningEffort.
	reasoningEffortAnomaly bool
}

// NewAccount builds the Account for config: the limiters and ceilings that
// are enabled, and the four prices.
func NewAccount(config AccountConfig) *Account {
	account := &Account{
		usageMarker: config.UsageMarker,
		logger:      config.Logger,
		ledger:      config.Ledger,
		now:         config.Now,
	}
	if account.now == nil {
		account.now = time.Now
	}
	if config.TokenBudget > 0 {
		account.tokenLimiter = NewWindowLimiter(config.TokenBudget, config.TokenBudgetWindow)
	}
	if config.CostBudgetMicroUSD > 0 {
		// int(config.CostBudgetMicroUSD): WindowLimiter's cost/limit fields
		// are int, which is 64-bit on every platform this repo targets
		// (darwin/amd64, linux/amd64), so a micro-USD budget in the tens of
		// millions of dollars still fits comfortably. Not worth widening
		// WindowLimiter to int64 generically for headroom this large.
		account.costLimiter = NewWindowLimiter(int(config.CostBudgetMicroUSD), config.CostBudgetWindow)
	}
	// Copied unconditionally, not only when a cost budget/ceiling is
	// configured: AddUsage tracks cumulative consumed cost for Usage()/the
	// "usage: " log line regardless of whether cost is actually bounded, as
	// long as a caller bothered to configure a price at all.
	account.prices.InputMicroUSDPerMTok = config.Prices.InputMicroUSDPerMTok
	// An unset cache rate defaults to the input price, so a relay started
	// without one never prices cache tokens at zero (budgets and ceilings
	// must not under-count).
	account.prices.CachedInputMicroUSDPerMTok = config.Prices.CachedInputMicroUSDPerMTok
	if account.prices.CachedInputMicroUSDPerMTok == 0 {
		account.prices.CachedInputMicroUSDPerMTok = config.Prices.InputMicroUSDPerMTok
	}
	account.prices.CacheWriteMicroUSDPerMTok = config.Prices.CacheWriteMicroUSDPerMTok
	if account.prices.CacheWriteMicroUSDPerMTok == 0 {
		account.prices.CacheWriteMicroUSDPerMTok = config.Prices.InputMicroUSDPerMTok
	}
	account.prices.OutputMicroUSDPerMTok = config.Prices.OutputMicroUSDPerMTok
	if config.TokenCeiling > 0 {
		account.tokenCeiling = newCeilingCounter(int64(config.TokenCeiling))
	}
	if config.CostCeilingMicroUSD > 0 {
		account.costCeiling = newCeilingCounter(config.CostCeilingMicroUSD)
	}
	return account
}

// Meters is whether this account accounts usage at all: any window or ceiling
// is configured. relay.ServeHTTP computes it once per request and reuses it
// for the budgetMu gate, for whether to parse the requested reasoning effort,
// and for the response side's own gate.
func (a *Account) Meters() bool {
	return a.tokenLimiter != nil || a.costLimiter != nil || a.tokenCeiling != nil || a.costCeiling != nil
}

// TokenCeilingExceeded reports whether the token ceiling is configured and has
// ever been crossed.
func (a *Account) TokenCeilingExceeded() bool {
	return a.tokenCeiling != nil && a.tokenCeiling.exceeded()
}

// CostCeilingExceeded reports whether the cost ceiling is configured and has
// ever been crossed.
func (a *Account) CostCeilingExceeded() bool {
	return a.costCeiling != nil && a.costCeiling.exceeded()
}

// TokenBelowLimit reports whether the token window has room: true when no
// token window is configured.
func (a *Account) TokenBelowLimit(now time.Time) bool {
	return a.tokenLimiter == nil || a.tokenLimiter.BelowLimit(now)
}

// CostBelowLimit reports whether the cost window has room: true when no cost
// window is configured.
func (a *Account) CostBelowLimit(now time.Time) bool {
	return a.costLimiter == nil || a.costLimiter.BelowLimit(now)
}

// TokenLimiter returns the token window limiter, or nil when none is configured.
func (a *Account) TokenLimiter() *WindowLimiter { return a.tokenLimiter }

// CostLimiter returns the cost window limiter, or nil when none is configured.
func (a *Account) CostLimiter() *WindowLimiter { return a.costLimiter }

// Ledger returns the usage ledger, or nil when none was configured.
func (a *Account) Ledger() *Ledger { return a.ledger }

// HighestReasoningEffort is the highest requested reasoning effort recorded so
// far, or "" when none has been.
func (a *Account) HighestReasoningEffort() string { return a.highestReasoningEffort }

// ChargeEstimate is the fail-closed response to a request whose real
// token/cost usage could not be determined (a truncated stream missing its
// terminal event, an unparseable upstream body, ...): charge a
// conservative, request-scoped estimate against every configured budget
// limiter, rather than either of two worse extremes.
//
// Not the original behavior of force-exceeding every limiter for the rest
// of its whole window (found live, 2026-09-07): a single transient
// truncated stream (observed live: an upstream generation call exceeding a
// since-fixed-too-short upstream timeout) permanently wedged every
// subsequent request for up to an hour regardless of the configured budget
// size -- raising it did nothing, since the limiter was already forced
// into its exhausted state by the very first truncation.
//
// Also not a blanket exemption keyed on the relay's apiKey == "" (found via review,
// Codex, PR #60, on this fix's own first draft): a credential-free relay
// still fronts a real, finite local compute resource -- Config.TokenBudget
// exists to bound total usage against that resource, a purpose that has
// nothing to do with protecting a billable credential, and an unconditional
// skip let a client dodge that bound entirely by repeatedly triggering
// unmeterable requests, with only the separate request-rate and timeout
// limits left standing.
//
// Charging requestBody's own declared cap instead bounds the damage of one
// unmeterable event to what that one request could plausibly have cost,
// regardless of credential: its length in bytes as a deliberately
// generous proxy for input tokens (real byte-per-token ratios run several
// times higher, so this overcharges rather than under-), and its own
// max_tokens / max_completion_tokens / max_output_tokens field for output
// tokens, falling back to UnmeterableFallbackOutputTokens when absent.
//
// effort is ServeHTTP's own already-parsed value, passed through rather
// than re-parsed here: parseRequestedReasoningEffort was already run
// exactly once per request, before this relay even knew whether the
// request would end up unmeterable.
func (a *Account) ChargeEstimate(stage string, err error, requestBody []byte, effort string) {
	inputTokens, outputTokens := estimateTokens(requestBody)
	// No cache information exists for an unmeterable estimate -- cachedTokens=0
	// charges it at full weight, the same conservative-overcharge tradeoff
	// this whole function already documents (see cachedInputWeightPercent's
	// own doc comment for why an absent cache field always means full
	// weight).
	a.AddUsage(inputTokens, inputTokens, 0, 0, outputTokens, effort)
	a.logger.Printf("usage unmeterable; charged conservative estimate (input=%d output=%d): %s: %v", inputTokens, outputTokens, stage, err)
}

// AddUsage records one accounting event. costInputTokens is the UNCACHED,
// non-cache-write portion of this response's input tokens -- normalized to
// mean the same thing across every usage format by each per-format parser
// above: Anthropic's own usage.input_tokens already excludes both
// cache_creation_input_tokens and cache_read_input_tokens (so it needs no
// adjustment), while an OpenAI-shaped prompt_tokens/input_tokens INCLUDES
// its reported cached_tokens as a subset (so each OpenAI-shaped parser
// subtracts cachedTokens before returning). cachedTokens is the cache-read
// subset (Anthropic's cache_read_input_tokens; an OpenAI-shaped format's
// cached_tokens); cacheWriteTokens is the cache-write subset (Anthropic's
// cache_creation_input_tokens only -- no OpenAI-shaped format this relay
// parses reports a separate cache-write count, so cacheWriteTokens is
// always 0 for those). meterInputTokens is the total input tokens
// (uncached + cached + cache-write) reported for this response, feeding
// WeightedInputTokens(meterInputTokens, cachedTokens) -- the token window
// limiter, the token ceiling, and every consumed-token figure
// (cumulativeInputTokens, the usage ledger, and everything internal/sandbox
// derives from either). See tokenCostMicroUSD for how costInputTokens/
// cachedTokens/cacheWriteTokens/outputTokens combine into a dollar cost.
func (a *Account) AddUsage(costInputTokens, meterInputTokens, cachedTokens, cacheWriteTokens, outputTokens int, effort string) {
	// See highestReasoningEffort's own doc comment: only ever updated
	// here, while budgetMu is held by every caller of AddUsage (ServeHTTP
	// holds it for the metersUsage branches, ChargeEstimate is only
	// ever called from within one of those same branches too). Keeps the
	// HIGHEST effort seen, not the last -- a later, lower-effort request
	// must never silently hide an earlier, higher one from the
	// authoritative "usage: " log line below.
	if effort == "other" {
		// Sticky: see reasoningEffortAnomaly's own doc comment. Set from
		// THIS event's own raw effort, not a.highestReasoningEffort --
		// reasoningEffortRank ranks "other" below every real level
		// specifically so a real, lower level is never masked by it, but
		// that means the anomaly itself would otherwise vanish the moment
		// a real level takes over highestReasoningEffort.
		a.reasoningEffortAnomaly = true
	}
	if reasoningEffortHigherThan(effort, a.highestReasoningEffort) {
		a.highestReasoningEffort = effort
	}
	weightedMeterInput := WeightedInputTokens(meterInputTokens, cachedTokens)
	if a.tokenLimiter != nil {
		a.tokenLimiter.Add(a.now(), weightedMeterInput+outputTokens)
	}
	if a.tokenCeiling != nil {
		a.tokenCeiling.add(int64(weightedMeterInput) + int64(outputTokens))
	}
	// Computed unconditionally, not only when a cost limiter/ceiling is
	// configured: RouteLaunchFacts.ConsumedCostMicroUSD (see
	// internal/sandbox/relay.go) is meant to answer "what did this run
	// cost", a question worth answering even for a relay that only bounds
	// tokens -- as long as a per-token price is actually configured, this
	// still only ever computes 0 when neither price is set. A cache-read
	// token is priced at cachedInputMicroUSDPerMTok and a cache-write token
	// at cacheWriteMicroUSDPerMTok (2026-09-28 operator decision: a real
	// provider price table has separate, usually cheaper, rates for both --
	// see internal/prices -- unlike the earlier flat per-token price this
	// replaced, which had no cached-input rate at all).
	cost, costOverflow := tokenCostMicroUSD(costInputTokens, cachedTokens, cacheWriteTokens, outputTokens, a.prices.InputMicroUSDPerMTok, a.prices.CachedInputMicroUSDPerMTok, a.prices.CacheWriteMicroUSDPerMTok, a.prices.OutputMicroUSDPerMTok)
	// Durable per-response ledger entry, written before any of the
	// budget/ceiling accounting below and using this call's own tokens/cost
	// (not the cumulative totals the "usage: " log line further down
	// carries) -- see Ledger.Append's own doc comment. On costOverflow
	// this records 0 for cost_micro_usd, same known-imprecise-but-honest
	// tradeoff cumulativeCostMicroUSD's own overflow handling below
	// documents; input/output tokens are exact either way. effort (this
	// call's own parameter, not a.highestReasoningEffort) records exactly
	// what THIS request asked for -- the ledger is a per-event record, not
	// a running highest. input_tokens carries weightedMeterInput (the same
	// figure feeding the limiters/ceiling above), and cachedTokens is
	// carried alongside it as cached_input_tokens so an audit can see both
	// the weighted figure and the raw cache-hit count behind it.
	a.ledger.Append(LedgerRecord{
		TS:                a.now().UTC().Format(time.RFC3339Nano),
		InputTokens:       int64(weightedMeterInput),
		OutputTokens:      int64(outputTokens),
		CostMicroUSD:      int64(cost),
		ReasoningEffort:   effort,
		CachedInputTokens: int64(cachedTokens),
	})
	if a.costLimiter != nil {
		if costOverflow {
			a.costLimiter.ForceExceed()
			a.logger.Printf("cost budget forced exceeded: cost computation overflowed")
		} else {
			a.costLimiter.Add(a.now(), cost)
		}
	}
	if a.costCeiling != nil {
		if costOverflow {
			a.costCeiling.forceExceed()
			a.logger.Printf("cost ceiling forced exceeded: cost computation overflowed")
		} else {
			a.costCeiling.add(int64(cost))
		}
	}
	// Tracked whenever ANY usage accounting ran at all (see metersUsage in
	// ServeHTTP, which gates whether AddUsage is ever called), independent
	// of whether a ceiling is configured -- Usage()/the "usage:" log line
	// below must report real cumulative consumption even for a relay with
	// only sliding-window budgets and no absolute ceiling.
	a.cumulativeInputTokens += int64(weightedMeterInput)
	a.cumulativeOutputTokens += int64(outputTokens)
	// On overflow, cumulativeCostMicroUSD is pinned to math.MaxInt64 rather
	// than left unincremented (found via code review): tokenCostMicroUSD
	// always returns cost==0 alongside overflow==true (this request's real
	// cost is simply too large to represent, not actually zero), so
	// silently skipping the add would leave the persisted evidence
	// understating this run's true spend -- specifically omitting the very
	// request whose size caused the overflow/forced-ceiling-trip in the
	// first place. A pinned sentinel is honest about "cost unknown, but at
	// least this large" instead of a wrong exact-looking number, and never
	// decreases once set (a later normal-sized request must not paper over
	// an earlier overflow).
	if costOverflow {
		a.cumulativeCostMicroUSD = math.MaxInt64
	} else if a.cumulativeCostMicroUSD != math.MaxInt64 {
		a.cumulativeCostMicroUSD += int64(cost)
	}
	ceilingExceeded := (a.tokenCeiling != nil && a.tokenCeiling.exceeded()) || (a.costCeiling != nil && a.costCeiling.exceeded())
	// The one stdout/stderr line internal/sandbox.finalRelayUsage parses
	// from `docker logs` at cleanup (see its own doc comment for why this,
	// not a second listener, is this package's chosen control path):
	// logged on every accounting event, carrying the CUMULATIVE totals so far, so the LAST
	// genuine such line at any point in this container's log is
	// authoritative for its whole lifetime. Emitted unconditionally
	// whenever AddUsage runs at all (not gated on a ceiling being
	// configured), so a relay with only window budgets still has its real
	// consumption durably recorded. marker=%s carries a.usageMarker, not
	// just the fixed prefix: see Config.UsageMarker's own doc comment for
	// why a reader must verify this field before trusting a candidate
	// line as genuine, rather than one an untrusted worker forged via its
	// own request path echoed in this server's separate access log.
	a.logger.Printf("%smarker=%s input_tokens=%d output_tokens=%d cost_micro_usd=%d ceiling_exceeded=%t%s%s",
		UsageLogPrefix, a.usageMarker, a.cumulativeInputTokens, a.cumulativeOutputTokens, a.cumulativeCostMicroUSD, ceilingExceeded,
		reasoningEffortLogSuffix(a.highestReasoningEffort), reasoningEffortAnomalyLogSuffix(a.reasoningEffortAnomaly))
}

// reasoningEffortLogSuffix is the trailing " reasoning_effort=<v>" field
// AddUsage's own authoritative "usage: " log line appends when a
// reasoning effort has been seen, or "" (adding nothing) when none has --
// this line's format is parsed by internal/sandbox.parseRelayUsageLine,
// where the field is optional, unlike every field ahead of it.
func reasoningEffortLogSuffix(effort string) string {
	if effort == "" {
		return ""
	}
	return " reasoning_effort=" + effort
}

// reasoningEffortAnomalyLogSuffix is the trailing
// " reasoning_effort_anomaly=true" field AddUsage's own "usage: " log
// line appends once Account.reasoningEffortAnomaly has ever been set, or
// "" (adding nothing) when it never has -- never emitted as "...=false",
// so an older reader that doesn't know this field yet sees the same line
// shape it always has for a relay that never saw an anomaly. Also parsed
// by internal/sandbox.parseRelayUsageLine, optional like
// reasoning_effort itself.
func reasoningEffortAnomalyLogSuffix(anomaly bool) string {
	if !anomaly {
		return ""
	}
	return " reasoning_effort_anomaly=true"
}

// UsageLogPrefix is the fixed prefix of the usage line AddUsage logs on
// every accounting event -- internal/sandbox.finalRelayUsage parses the
// last occurrence of this prefix out of `docker logs` at relay cleanup.
// Exported (found via code review: a package-private duplicate of this
// exact literal in internal/sandbox previously had nothing enforcing the
// two stayed byte-identical, so an edit to one without the other would
// silently break finalRelayUsage's parsing -- fail OPEN, since "no usage
// line found" reads as valid absence-of-data, not a parse error) so that
// package can import this constant directly instead of keeping its own
// copy.
const UsageLogPrefix = "usage: "

// Usage returns this account's cumulative consumed tokens/cost and whether
// either configured ceiling has been crossed -- the in-process accessor
// equivalent of the "usage: " log line above, for a caller (tests, or a
// future second control-plane listener) that already holds a *Account
// directly rather than needing to parse container logs. inputTokens is the
// WEIGHTED figure (see cachedInputWeightPercent); costMicroUSD is not
// affected by that weighting.
func (a *Account) Usage() (inputTokens, outputTokens, costMicroUSD int64, ceilingExceeded bool) {
	ceilingExceeded = (a.tokenCeiling != nil && a.tokenCeiling.exceeded()) || (a.costCeiling != nil && a.costCeiling.exceeded())
	return a.cumulativeInputTokens, a.cumulativeOutputTokens, a.cumulativeCostMicroUSD, ceilingExceeded
}
