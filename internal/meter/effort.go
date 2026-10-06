package meter

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// reasoningEffortLevelOrder is the closed set of string effort levels
// this relay will ever record verbatim, in ascending effort order. Both
// sanitizeReasoningEffort's membership check and reasoningEffortRank's
// ordering derive from this one slice (via reasoningEffortLevelRank
// below) so the two can never drift apart. Anything else a worker's
// request names -- a typo, a future level this relay doesn't know about
// yet, an adversarial value -- is recorded as "other" instead (see
// sanitizeReasoningEffort), never forwarded into this relay's own
// logs/ledger unsanitized.
var reasoningEffortLevelOrder = []string{"none", "minimal", "low", "medium", "high", "xhigh", "max"}

// reasoningEffortLevelRank maps each reasoningEffortLevelOrder entry to
// its ascending position, computed once. The sole source both
// sanitizeReasoningEffort (membership) and reasoningEffortRank (ordering)
// read from.
var reasoningEffortLevelRank = func() map[string]int {
	m := make(map[string]int, len(reasoningEffortLevelOrder))
	for i, level := range reasoningEffortLevelOrder {
		m[level] = i
	}
	return m
}()

// sanitizeReasoningEffort returns v unchanged when it is one of
// reasoningEffortLevelOrder's closed set, or "other" otherwise.
func sanitizeReasoningEffort(v string) string {
	if _, ok := reasoningEffortLevelRank[v]; ok {
		return v
	}
	return "other"
}

// isJSONNull reports whether raw is the JSON literal null. A worker's
// request naming a field explicitly as null (e.g. {"reasoning_effort":
// null}) is, semantically, exactly the same as not naming that field at
// all -- every accessor below checks this before decoding, so a null
// value is treated as absent ("") rather than as a JSON string/number/
// object type mismatch ("other").
func isJSONNull(raw json.RawMessage) bool {
	return string(bytes.TrimSpace(raw)) == "null"
}

// ParseRequestedReasoningEffort extracts the reasoning effort a forwarded
// request body asked for, read-only and best-effort: this never fails or
// alters the request being forwarded, it only inspects the bytes already
// in hand. requestFormat (one of the RequestFormat* constants, always
// already resolved by NewServer -- see Config.RequestFormat's own doc
// comment) selects the ONE field shape this relay's own upstream actually
// reads for this route -- a worker sending, say, a Chat-Completions-shaped
// "reasoning_effort" to a Responses-configured relay is not honored by the
// real upstream either, so recording it here would be recording a fiction:
//
//   - RequestFormatOpenAIResponses: top-level "reasoning":{"effort":"..."}
//   - RequestFormatOpenAICompletions: top-level "reasoning_effort":"..."
//   - RequestFormatAnthropic (also the fallback for any other value):
//     top-level "output_config":{"effort":"..."} (Anthropic's newer,
//     direct effort control -- checked first, since it supersedes the
//     older thinking-budget mechanism when both are present) or
//     "thinking":{"budget_tokens":N} (records as "budget:N") or
//     "thinking":{"type":"adaptive"} with no budget_tokens ("adaptive");
//     "thinking":{"type":"disabled"} always records "" regardless of any
//     budget_tokens alongside it -- thinking is off, so no effort was
//     actually requested.
//
// The top level is decoded into a map[string]json.RawMessage, not a
// tagged struct: encoding/json's struct-field matching falls back to
// case-INSENSITIVE key matching when no exact match exists, so a body
// naming both "reasoning" and "REASONING" would let the untrusted worker
// pick which one an earlier struct-based version of this parser saw,
// silently, depending on encoding/json's own internal field-matching
// order -- not a real defense at all. A Go map's key, by contrast, is
// always the exact JSON string, so an exact `top["reasoning"]` lookup
// only ever matches that literal key.
//
// A malformed body returns "" (the upstream will reject the request too,
// so there is nothing to meaningfully record). A well-formed body where
// the field this format reads is simply absent, OR present but explicitly
// JSON null (see isJSONNull), also returns "" (no effort was requested --
// null and absent are the same thing here). A well-formed body where that
// field IS present with a non-null value that has an unexpected shape --
// its parent isn't a JSON object, or the leaf itself isn't the type
// expected (a number where a string effort level belongs, say) --
// returns "other": the anomaly must stay visible, not silently read as
// "no effort requested".
func ParseRequestedReasoningEffort(body []byte, requestFormat string) string {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		return ""
	}
	switch requestFormat {
	case RequestFormatOpenAIResponses:
		return effortFromNestedStringField(top, "reasoning", "effort")
	case RequestFormatOpenAICompletions:
		return effortFromTopLevelStringField(top, "reasoning_effort")
	default:
		return effortFromAnthropicRequest(top)
	}
}

// effortFromTopLevelStringField reads top[key] as a JSON string and
// sanitizes it. "" when key is absent, or present as JSON null. "other"
// when key is present with a non-null value that is not a JSON string.
func effortFromTopLevelStringField(top map[string]json.RawMessage, key string) string {
	raw, present := top[key]
	if !present || isJSONNull(raw) {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "other"
	}
	return sanitizeReasoningEffort(s)
}

// effortFromNestedStringField reads top[parentKey][childKey] as a JSON
// string and sanitizes it. "" when parentKey is absent or JSON null, or
// present as an object with no childKey (or childKey present as JSON
// null) -- a well-formed request simply not asking for an effort level.
// "other" when parentKey has a non-null value that isn't a JSON object,
// or childKey has a non-null value that isn't a JSON string.
func effortFromNestedStringField(top map[string]json.RawMessage, parentKey, childKey string) string {
	parentRaw, present := top[parentKey]
	if !present || isJSONNull(parentRaw) {
		return ""
	}
	var parent map[string]json.RawMessage
	if err := json.Unmarshal(parentRaw, &parent); err != nil {
		return "other"
	}
	childRaw, present := parent[childKey]
	if !present || isJSONNull(childRaw) {
		return ""
	}
	var s string
	if err := json.Unmarshal(childRaw, &s); err != nil {
		return "other"
	}
	return sanitizeReasoningEffort(s)
}

// knownThinkingTypes is the closed set of Anthropic "thinking.type"
// values this parser recognizes. Any other non-null value (a typo, a
// future type this relay doesn't know about yet) is an anomaly -- see
// effortFromAnthropicRequest.
var knownThinkingTypes = map[string]bool{"enabled": true, "adaptive": true, "disabled": true}

// effortFromAnthropicRequest implements parseRequestedReasoningEffort's
// Anthropic Messages case -- see that function's own doc comment for the
// full precedence (output_config.effort, then thinking, with
// thinking.type=="disabled" always winning to "").
func effortFromAnthropicRequest(top map[string]json.RawMessage) string {
	if outputConfigRaw, present := top["output_config"]; present && !isJSONNull(outputConfigRaw) {
		var outputConfig map[string]json.RawMessage
		if err := json.Unmarshal(outputConfigRaw, &outputConfig); err != nil {
			return "other"
		}
		if effortRaw, present := outputConfig["effort"]; present && !isJSONNull(effortRaw) {
			var s string
			if err := json.Unmarshal(effortRaw, &s); err != nil {
				return "other"
			}
			return sanitizeReasoningEffort(s)
		}
	}

	thinkingRaw, present := top["thinking"]
	if !present || isJSONNull(thinkingRaw) {
		return ""
	}
	var thinking map[string]json.RawMessage
	if err := json.Unmarshal(thinkingRaw, &thinking); err != nil {
		return "other"
	}

	var thinkingType string
	hasType := false
	if typeRaw, present := thinking["type"]; present && !isJSONNull(typeRaw) {
		if err := json.Unmarshal(typeRaw, &thinkingType); err != nil {
			return "other"
		}
		hasType = true
		if !knownThinkingTypes[thinkingType] {
			return "other"
		}
	}
	// Disabled always wins to "": thinking is off, so any budget_tokens
	// alongside it is a leftover/irrelevant value, not a real request.
	if hasType && thinkingType == "disabled" {
		return ""
	}

	if budgetRaw, present := thinking["budget_tokens"]; present && !isJSONNull(budgetRaw) {
		var n int64
		if err := json.Unmarshal(budgetRaw, &n); err != nil {
			return "other"
		}
		if n < 0 {
			// budget_tokens is a token count; a negative one is not a
			// real value this relay's own construction can produce
			// (ValidReasoningEffort's closed budget form never allows a
			// sign) -- recorded as an anomaly rather than emitting a
			// value that would itself fail re-validation downstream.
			return "other"
		}
		return fmt.Sprintf("budget:%d", n)
	}
	if hasType && thinkingType == "adaptive" {
		return "adaptive"
	}
	return ""
}

// reasoningEffortHighTier is reasoningEffortLevelRank["high"] -- the tier
// "adaptive" and every "budget:N" (N > 0) share with the plain "high"
// level. Computed once from the same ordered slice reasoningEffortRank's
// closed-level lookup uses, so it can never drift from that ordering.
var reasoningEffortHighTier = reasoningEffortLevelRank["high"]

// reasoningEffortNoneTier is reasoningEffortLevelRank["none"] --
// "budget:0" (an explicit zero thinking budget, i.e. no extra effort at
// all) ranks here, not in the high tier alongside every other budget.
var reasoningEffortNoneTier = reasoningEffortLevelRank["none"]

// reasoningEffortOtherTier ranks "other" just above "" (the "never seen
// anything" absence reasoningEffortHigherThan special-cases) and below
// every real level, including "none" -- an anomalous/unrecognized request
// must stay visible (see Account.reasoningEffortAnomaly's own doc comment
// for the separate sticky flag this drives), but must never be able to
// mask a real, lower level that arrives afterward: reasoningEffortRank
// alone decides "highest effort", and "other" is deliberately the lowest
// real rank so any actual level -- even "none" -- always wins over it.
const reasoningEffortOtherTier = -1

// reasoningEffortRank orders every value parseRequestedReasoningEffort (or
// this relay's own sanitizeReasoningEffort) can ever produce, for the
// "keep the highest effort seen" rule Account.AddUsage applies over the relay's
// whole life (see Account.highestReasoningEffort's own doc comment):
//
//	other < none < minimal < low < medium < {high, adaptive, budget:N (N>0, by N)} < xhigh < max
//
// "adaptive" and "budget:N" (N > 0) share the "high" tier: a caller
// spending an explicit token budget on thinking, or opting into adaptive
// thinking, is treated as at least as serious an effort request as a
// plain "high", but neither is assumed to exceed "xhigh"/"max" (a real
// caller naming those levels explicitly outranks any budget, however
// large). Within that shared tier, higher N ranks higher; plain
// "high"/"adaptive" both sub-rank at 0, below any budget with N > 0.
// "budget:0" -- a caller explicitly asking for zero extra thinking budget
// -- ranks at the "none" tier instead, not the high tier: spending
// nothing is not "high effort" by any reading. "other" ranks lowest of
// every real value (see reasoningEffortOtherTier's own doc comment): a
// real level, even "none", always outranks an anomaly.
//
// Returns (tier, budget): budget is only meaningful when tier is the
// shared high tier, and is otherwise 0. v is assumed already sanitized
// (one of this package's own parser/sanitizer outputs) -- an unrecognized
// v (not reachable from this package's own callers) ranks below "other",
// so it can never win a "keep highest" comparison by accident.
func reasoningEffortRank(v string) (tier int, budget int64) {
	if v == "other" {
		return reasoningEffortOtherTier, 0
	}
	if v == "adaptive" {
		return reasoningEffortHighTier, 0
	}
	if r, ok := reasoningEffortLevelRank[v]; ok {
		return r, 0
	}
	if rest, ok := strings.CutPrefix(v, "budget:"); ok {
		if n, err := strconv.ParseInt(rest, 10, 64); err == nil && n >= 0 {
			if n == 0 {
				return reasoningEffortNoneTier, 0
			}
			return reasoningEffortHighTier, n
		}
	}
	return reasoningEffortOtherTier - 1, 0
}

// ReasoningEffortHigherThan exports reasoningEffortHigherThan for
// internal/sandbox's own ledger-file fallback recovery (readUsageLedger),
// which must reconstruct the same "highest effort seen" value the
// confirmed docker-logs path already carries, by folding this same
// ranking over every per-event ledger line after a crash -- see that
// function's own doc comment.
func ReasoningEffortHigherThan(candidate, current string) bool {
	return reasoningEffortHigherThan(candidate, current)
}

// reasoningEffortHigherThan reports whether candidate ranks strictly
// higher than current per reasoningEffortRank, treating "" (never seen
// anything yet) as lower than any non-empty value -- including "other",
// which reasoningEffortRank itself ranks below every real level but still
// above "". Ties keep current (an equal-ranked candidate is not a
// meaningful update).
func reasoningEffortHigherThan(candidate, current string) bool {
	if current == "" {
		return candidate != ""
	}
	if candidate == "" {
		return false
	}
	candidateTier, candidateBudget := reasoningEffortRank(candidate)
	currentTier, currentBudget := reasoningEffortRank(current)
	if candidateTier != currentTier {
		return candidateTier > currentTier
	}
	return candidateBudget > currentBudget
}

// ValidReasoningEffort reports whether v is a value this relay's own
// reasoning-effort recording could actually have produced: one of
// reasoningEffortLevelOrder's closed set, "other", "adaptive", or
// "budget:<n>" where n is 1-19 decimal digits with no sign, no leading
// zero (unless n is exactly "0"), and within int64 range -- the same
// shape parseRequestedReasoningEffort's own budget construction produces
// (a non-negative int64 formatted with %d never has a sign or a leading
// zero other than the digit "0" itself, and int64's own range tops out at
// 19 digits). Exported so internal/sandbox can re-validate a
// "reasoning_effort" value read back out of this relay's own log/ledger
// output before trusting it -- that value still ultimately traces back to
// an untrusted worker's request body, so a reader must not assume this
// relay's own output already guarantees the shape.
func ValidReasoningEffort(v string) bool {
	if _, ok := reasoningEffortLevelRank[v]; ok {
		return true
	}
	if v == "other" || v == "adaptive" {
		return true
	}
	rest, ok := strings.CutPrefix(v, "budget:")
	if !ok {
		return false
	}
	return validBudgetDigits(rest)
}

// validBudgetDigits reports whether s is 1-19 ASCII digits, with no sign,
// no leading zero unless s is exactly "0", and parses as a valid int64
// (strconv.ParseInt itself rejects overflow -- a 19-digit string can
// still exceed math.MaxInt64, e.g. "9999999999999999999").
func validBudgetDigits(s string) bool {
	if len(s) == 0 || len(s) > 19 {
		return false
	}
	if s[0] == '0' && len(s) > 1 {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	_, err := strconv.ParseInt(s, 10, 64)
	return err == nil
}
