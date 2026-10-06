package meter

import "math"

// estimateCostMicroUSD is the cost AddUsage would record for an estimate of
// inputTokens uncached input tokens and outputTokens output tokens at prices,
// saturating at math.MaxInt64 when the computation overflows (a reservation
// must never wrap to a small number).
func estimateCostMicroUSD(prices Prices, inputTokens, outputTokens int) int64 {
	cost, overflow := tokenCostMicroUSD(inputTokens, 0, 0, outputTokens, prices.InputMicroUSDPerMTok, prices.CachedInputMicroUSDPerMTok, prices.CacheWriteMicroUSDPerMTok, prices.OutputMicroUSDPerMTok)
	if overflow {
		return math.MaxInt64
	}
	return int64(cost)
}

// CommittedTokens is the weighted input plus output tokens consumed so far,
// saturating at math.MaxInt64.
func (a *Account) CommittedTokens() int64 {
	return SaturatingAdd(a.cumulativeInputTokens, a.cumulativeOutputTokens)
}

// CommittedCostMicroUSD is the cost consumed so far (math.MaxInt64 once a
// cost computation has overflowed).
func (a *Account) CommittedCostMicroUSD() int64 { return a.cumulativeCostMicroUSD }

// Restore sets the cumulative totals and the ceiling counters from a
// durable ledger after a restart. It replaces whatever was counted so far and
// leaves the window limiters empty.
func (a *Account) Restore(inputTokens, outputTokens, costMicroUSD int64) {
	a.cumulativeInputTokens = inputTokens
	a.cumulativeOutputTokens = outputTokens
	a.cumulativeCostMicroUSD = costMicroUSD
	if a.tokenCeiling != nil {
		a.tokenCeiling.seed(SaturatingAdd(inputTokens, outputTokens))
	}
	if a.costCeiling != nil {
		a.costCeiling.seed(costMicroUSD)
	}
}

// SaturatingAdd adds two non-negative int64 values, stopping at
// math.MaxInt64 instead of wrapping.
func SaturatingAdd(a, b int64) int64 {
	if a > math.MaxInt64-b {
		return math.MaxInt64
	}
	return a + b
}
