package meter

// cachedInputWeightPercent is how much a cache-hit input token counts
// toward the relay's token budget window (tokenLimiter), its absolute
// token ceiling (tokenCeiling), and the consumed-token figures that feed
// request/monthly budgets (RouteLaunchFacts.ConsumedInputTokens and the
// usage ledger), relative to an uncached input or output token (100%,
// unchanged). Decided by the operator 2026-09-28, after live evidence: a
// direct Codex CLI run against Luna reported 978,678 input tokens of which
// 899,840 were cached, and metering all input at full weight (today's
// behavior before this constant existed) tripped the relay's
// 1M-tokens/hour window four times in one day on cache hits, not real new
// spend -- each trip lost a code review, a conformity review, or a plan
// draft. "Cached" means exactly what the upstream provider reports on
// that response (see WeightedInputTokens and each per-format usage
// parser above); a response reporting no cache field at all still counts
// every input token at 100%, same as before this change. Cost accounting
// (tokenCostMicroUSD, below) is a separate computation with its own real
// cached-input/cache-write rates (2026-09-28: internal/prices' compiled
// table) -- this constant only ever affects the token meters, never cost.
const cachedInputWeightPercent = 10

// WeightedInputTokens applies cachedInputWeightPercent to inputTokens,
// given how many of those tokens the upstream reported as a cache hit.
// cachedTokens is clamped to [0, inputTokens] first: a cache count is
// trusted but never allowed to make the weighted result exceed
// inputTokens itself, so an over-reported cache hit can only ever
// undercount relative to today, never overcount. The cached portion is
// rounded up (integer ceiling division), so a fractional token is never
// dropped in the relay's own favor. cachedTokens == 0 (no cache field on
// the response, or a genuine zero-cache-hit response) leaves the result
// equal to inputTokens -- full weight, exactly today's behavior.
func WeightedInputTokens(inputTokens, cachedTokens int) int {
	if cachedTokens < 0 {
		cachedTokens = 0
	}
	if cachedTokens > inputTokens {
		cachedTokens = inputTokens
	}
	uncached := inputTokens - cachedTokens
	weightedCached := (cachedTokens*cachedInputWeightPercent + 99) / 100
	return uncached + weightedCached
}

// tokenCostMicroUSD computes a response's cost in micro-USD from its token
// counts and configured prices (each in micro-USD per 1,000,000 tokens --
// see internal/prices): uncachedInputTokens*inputPrice +
// cachedInputTokens*cachedInputPrice + cacheWriteTokens*cacheWritePrice +
// outputTokens*outputPrice, all summed in micro-USD-per-token-times-1e6
// units and then divided by 1,000,000 and rounded UP, so a fractional
// micro-USD is never dropped in the relay's own favor (a budget/ceiling
// must never under-count real spend). uncachedInputTokens is already
// normalized by every caller (addUsage, fed by each per-format usage
// parser) to exclude both cachedInputTokens and cacheWriteTokens -- this
// function itself does no further subtraction. Reports overflow rather
// than silently wrapping — a wrapped negative/garbage cost fed into
// windowLimiter could corrupt its running total for every request sharing
// that window, not just this one.
func tokenCostMicroUSD(uncachedInputTokens, cachedInputTokens, cacheWriteTokens, outputTokens int, inputPriceMicroUSDPerMTok, cachedInputPriceMicroUSDPerMTok, cacheWritePriceMicroUSDPerMTok, outputPriceMicroUSDPerMTok int64) (cost int, overflow bool) {
	const maxInt64 = int64(^uint64(0) >> 1)
	mul := func(tokens int, priceMicroUSDPerMTok int64) (int64, bool) {
		if tokens == 0 || priceMicroUSDPerMTok == 0 {
			return 0, false
		}
		v := int64(tokens) * priceMicroUSDPerMTok
		if v/priceMicroUSDPerMTok != int64(tokens) {
			return 0, true
		}
		return v, false
	}
	add := func(a, b int64) (int64, bool) {
		if a > maxInt64-b {
			return 0, true
		}
		return a + b, false
	}
	uncachedCost, of := mul(uncachedInputTokens, inputPriceMicroUSDPerMTok)
	if of {
		return 0, true
	}
	cachedCost, of := mul(cachedInputTokens, cachedInputPriceMicroUSDPerMTok)
	if of {
		return 0, true
	}
	writeCost, of := mul(cacheWriteTokens, cacheWritePriceMicroUSDPerMTok)
	if of {
		return 0, true
	}
	outputCost, of := mul(outputTokens, outputPriceMicroUSDPerMTok)
	if of {
		return 0, true
	}
	total, of := add(uncachedCost, cachedCost)
	if of {
		return 0, true
	}
	total, of = add(total, writeCost)
	if of {
		return 0, true
	}
	total, of = add(total, outputCost)
	if of {
		return 0, true
	}
	// Round the per-1M-tokens total up to the nearest whole micro-USD.
	if total > maxInt64-999_999 {
		return 0, true
	}
	microUSD := (total + 999_999) / 1_000_000
	return int(microUSD), false
}
