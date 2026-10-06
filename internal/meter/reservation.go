package meter

import (
	"fmt"
	"time"
)

// timeLayout is the ledger timestamp layout.
const timeLayout = time.RFC3339Nano

// Deny reason codes of the request side.
const (
	CodeInvalidConfig     = "invalid_config"
	CodeSandboxMismatch   = "sandbox_mismatch"
	CodeMethodNotAllowed  = "method_not_allowed"
	CodeRequestTooLarge   = "request_too_large"
	CodeCeilingExceeded   = "ceiling_exceeded"
	CodeBudgetExceeded    = "budget_exceeded"
	CodeRateLimited       = "rate_limited"
	CodePolicyChanged     = "policy_changed"
	CodeInvalidRequestID  = "invalid_request_id"
	CodeDuplicateRequest  = "duplicate_request"
	CodeLedgerUnavailable = "ledger_unavailable"
)

// admitRequest is what admission needs of one request. The body is already
// reduced to its estimate and its parsed fields.
type admitRequest struct {
	requestID string
	policy    Policy
	effort    string
	stream    bool
	estimate  pendingRequest
}

// admission is the outcome: allowed, or a deny reason code.
type admission struct {
	allowed bool
	code    string
}

// admit decides one request under the entry's lock: the policy may only
// tighten, committed plus reserved usage must still be below each ceiling, then
// the windows and the request rate. An admitted request reserves its estimate
// and gets an admit record.
func (e *runEntry) admit(req admitRequest) admission {
	e.mu.Lock()
	defer e.mu.Unlock()
	if req.requestID == "" {
		return admission{code: CodeInvalidRequestID}
	}
	if _, dup := e.pending[req.requestID]; dup {
		return admission{code: CodeDuplicateRequest}
	}
	if err := e.tighten(req.policy); err != nil {
		e.logger.Printf("meter: %v", err)
		return admission{code: CodePolicyChanged}
	}
	est := req.estimate
	if e.ceilingDenies() {
		e.ledger.Append(LedgerRecord{TS: e.stamp(), Kind: LedgerKindCeilingExceeded, RequestID: req.requestID})
		return admission{code: CodeCeilingExceeded}
	}
	now := e.now()
	if !e.account.TokenBelowLimit(now) || !e.account.CostBelowLimit(now) {
		return admission{code: CodeBudgetExceeded}
	}
	if e.requestLimiter != nil && !e.requestLimiter.Allow(now, 1) {
		return admission{code: CodeRateLimited}
	}
	est.stream = req.stream
	est.effort = req.effort
	est.admittedAt = now
	e.pending[req.requestID] = est
	e.reservedTokens = SaturatingAdd(e.reservedTokens, est.estimateTokens())
	e.reservedCost = SaturatingAdd(e.reservedCost, est.estimateCostMicroUSD)
	e.ledger.Append(LedgerRecord{
		TS:                   e.stamp(),
		Kind:                 LedgerKindAdmit,
		RequestID:            req.requestID,
		EstimateTokens:       est.estimateTokens(),
		EstimateOutputTokens: int64(est.estimateOutputTokens),
		EstimateCostMicroUSD: est.estimateCostMicroUSD,
	})
	return admission{allowed: true}
}

func (e *runEntry) stamp() string { return e.now().UTC().Format(timeLayout) }

// ceilingDenies reports whether committed plus reserved usage has reached a
// configured ceiling, or a ceiling counter is already tripped (including by a
// cost overflow). With nothing in flight this is the relay's rule: a request
// is refused only once committed usage has reached the ceiling. A request in
// flight counts at its worst-case estimate, so two requests arriving at the
// ceiling cannot both pass.
func (e *runEntry) ceilingDenies() bool {
	if e.account.TokenCeilingExceeded() || e.account.CostCeilingExceeded() {
		return true
	}
	return !belowCeiling(e.policy.TokenCeiling, e.account.CommittedTokens(), e.reservedTokens) ||
		!belowCeiling(e.policy.CostCeilingMicroUSD, e.account.CommittedCostMicroUSD(), e.reservedCost)
}

// belowCeiling is true when ceiling is 0 (disabled) or committed + reserved is
// still below it.
func belowCeiling(ceiling, committed, reserved int64) bool {
	if ceiling == 0 {
		return true
	}
	return SaturatingAdd(committed, reserved) < ceiling
}

// tighten accepts next for a known sandbox only when nothing but the
// ceilings differ and no ceiling got looser, then adopts the tighter ceilings.
func (e *runEntry) tighten(next Policy) error {
	if sansCeilings(e.policy) != sansCeilings(next) {
		return fmt.Errorf("policy for run %q sandbox %q changed beyond its ceilings", e.policy.Run, e.policy.Sandbox)
	}
	if !tighter(e.policy.TokenCeiling, next.TokenCeiling) || !tighter(e.policy.CostCeilingMicroUSD, next.CostCeilingMicroUSD) {
		return fmt.Errorf("policy for run %q sandbox %q loosens a ceiling", e.policy.Run, e.policy.Sandbox)
	}
	e.policy.TokenCeiling = next.TokenCeiling
	e.policy.CostCeilingMicroUSD = next.CostCeilingMicroUSD
	return nil
}

func sansCeilings(p Policy) Policy {
	p.TokenCeiling = 0
	p.CostCeilingMicroUSD = 0
	return p
}

// tighter is true when next is at least as strict as current: any ceiling
// tightens a disabled one, and a set ceiling may only drop.
func tighter(current, next int64) bool {
	return current == 0 || (next != 0 && next <= current)
}

// settleMode says how an admitted request's reservation ends.
type settleMode int

const (
	// settleRelease frees the reservation without charging anything.
	settleRelease settleMode = iota
	// settleExact charges the usage the response reported.
	settleExact
	// settleEstimate charges the reserved estimate.
	settleEstimate
)

// settlement is a reservation's outcome; usage is read only for settleExact.
type settlement struct {
	mode  settleMode
	usage usageFigures
}

// usageFigures are Account.AddUsage's parameters.
type usageFigures struct {
	costInput, meterInput, cached, cacheWrite, output int
}

// peek returns the pending request, if any.
func (e *runEntry) peek(requestID string) (pendingRequest, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	p, ok := e.pending[requestID]
	return p, ok
}

// settle ends a reservation: it releases the estimate, charges what the
// settlement says and appends the complete record. False when the request
// is not pending (already settled).
func (e *runEntry) settle(requestID string, s settlement) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.settleLocked(requestID, s)
}

func (e *runEntry) settleLocked(requestID string, s settlement) bool {
	p, ok := e.pending[requestID]
	if !ok {
		return false
	}
	delete(e.pending, requestID)
	e.reservedTokens -= p.estimateTokens()
	e.reservedCost -= p.estimateCostMicroUSD
	if e.reservedTokens < 0 || e.reservedCost < 0 {
		e.reservedTokens = max(e.reservedTokens, 0)
		e.reservedCost = max(e.reservedCost, 0)
	}
	switch s.mode {
	case settleExact:
		u := s.usage
		e.account.AddUsage(u.costInput, u.meterInput, u.cached, u.cacheWrite, u.output, p.effort)
	case settleEstimate:
		e.account.AddUsage(p.estimateInputTokens, p.estimateInputTokens, 0, 0, p.estimateOutputTokens, p.effort)
		e.logger.Printf("usage unmeterable; charged conservative estimate (input=%d output=%d): request %s", p.estimateInputTokens, p.estimateOutputTokens, requestID)
	}
	e.ledger.Append(LedgerRecord{TS: e.stamp(), Kind: LedgerKindComplete, RequestID: requestID})
	return true
}

// expire settles every reservation admitted before cutoff at its estimate
// and returns how many.
func (e *runEntry) expire(cutoff time.Time) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.account == nil {
		return 0
	}
	var stale []string
	for id, p := range e.pending {
		if p.admittedAt.Before(cutoff) {
			stale = append(stale, id)
		}
	}
	for _, id := range stale {
		e.settleLocked(id, settlement{mode: settleEstimate})
	}
	return len(stale)
}
