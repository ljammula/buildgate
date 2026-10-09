package main

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"buildgate/internal/progress"
	"buildgate/internal/run"
)

// roundSummaryMaxRounds is how many per-round entries RoundSummary lists
// individually before collapsing the rest into a single "… rN" tail --
// keeps the note within roundSummaryMaxLen even for a run with a high
// -build-app-max-rounds.
const roundSummaryMaxRounds = 8

// roundSummaryMaxLen mirrors progress.jsonl's own 500-char detail limit
// (see progress-contract.md) so RoundSummary's caller never has to
// truncate its result itself.
const roundSummaryMaxLen = 500

// RoundSummary renders ev's rounds as the one-line, factory-authored note
// progress-contract.md's "Additions" section calls for ("Factory `build`
// note"): a per-round pass/fail outcome plus total tokens and, when known,
// cost -- so an operator reading the progress feed doesn't have to
// reconstruct it from raw BUILD_EVIDENCE.json rounds themselves. Returns
// "" for nil evidence or evidence with no rounds -- nothing to summarize.
//
// costUSD is a pre-formatted "12.34"-style string with no leading "$", or
// "" to omit the cost segment entirely. It is a parameter, not computed
// here, because AgentEvidence itself carries no price/cost data of its
// own -- the caller derives it from statusCostMicroUSD, the same relay
// spend evidence `factoryd status` already reports.
func RoundSummary(ev *run.AgentEvidence, costUSD string) string {
	if ev == nil || len(ev.Rounds) == 0 {
		return ""
	}

	parts := make([]string, 0, len(ev.Rounds)+3)
	parts = append(parts, fmt.Sprintf("%d round%s", len(ev.Rounds), progress.Plural(len(ev.Rounds))))

	shown := len(ev.Rounds)
	if shown > roundSummaryMaxRounds {
		shown = roundSummaryMaxRounds
	}
	var inTokens, outTokens int64
	for i, rd := range ev.Rounds {
		in, out := roundTokens(rd)
		inTokens += in
		outTokens += out
		if i < shown {
			parts = append(parts, fmt.Sprintf("r%d %s", rd.Index, roundOutcome(rd)))
		}
	}
	if len(ev.Rounds) > shown {
		parts = append(parts, fmt.Sprintf("… r%d", ev.Rounds[len(ev.Rounds)-1].Index))
	}

	if total := inTokens + outTokens; total > 0 {
		parts = append(parts, formatTokenCount(total)+" tokens")
	}
	if costUSD != "" {
		parts = append(parts, "$"+costUSD)
	}

	summary := strings.Join(parts, " · ")
	if len(summary) > roundSummaryMaxLen {
		cut := roundSummaryMaxLen
		for cut > 0 && !utf8.RuneStart(summary[cut]) {
			cut--
		}
		summary = summary[:cut]
	}
	return summary
}

// roundOutcome classifies one round as "pass" or "fail (<reason>)" using
// only fields AgentEvidenceRound actually records, in the same priority
// order build_app.py's own round_blockers applies (timeout, then the pi
// invocation itself failing, then a fast check substituting for
// verification, then verification itself) -- see that function's doc
// comment in agent/pi/scripts/build_app.py.
//
// build_app.py also blocks a round for reasons no outcome field shows: it
// changed nothing in the workspace, or a required review was not clean. A
// round that recorded blockers while every outcome field reads clean is
// therefore "fail (blocked)". Blockers only ever add a failure: an empty
// list never turns a failing field into a pass, because a round build_app.py
// built without computing them (a restored or fallback round) carries an
// empty list too.
func roundOutcome(rd run.AgentEvidenceRound) string {
	outcome := roundOutcomeFromFields(rd)
	if outcome == "pass" && len(rd.Blockers) > 0 {
		return "fail (blocked)"
	}
	return outcome
}

func roundOutcomeFromFields(rd run.AgentEvidenceRound) string {
	if rd.AgentTimedOut || rd.VerifyTimedOut {
		return "fail (timed out)"
	}
	if rd.AgentReturnCode != 0 {
		return "fail (error)"
	}
	if rd.FastCheckRan && rd.FastCheckPassed != nil && !*rd.FastCheckPassed {
		return "fail (verify)"
	}
	if rd.VerifyPassed != nil {
		if *rd.VerifyPassed {
			return "pass"
		}
		return "fail (verify)"
	}
	// VerifyPassed nil means no canonical command was resolvable (see its
	// own doc comment) -- not a positive pass, so still a failure, but
	// without a more specific reason.
	return "fail (error)"
}

// roundTokens reads rd.Usage's token fields -- tolerating a nil map or
// non-numeric values, since Usage is agent-reported, untrusted JSON.
func roundTokens(rd run.AgentEvidenceRound) (in, out int64) {
	if rd.Usage == nil {
		return 0, 0
	}
	if v, ok := rd.Usage["output"].(float64); ok {
		out = int64(v)
	}
	// pi's "input" excludes prompt-cache reads, which the relay meters
	// as consumed input (found live: 1.8k vs 14k for the same round);
	// prefer totalTokens so this summary agrees with the recap's
	// relay-metered figure, falling back to input+output+cacheRead+
	// cacheWrite when absent (C8, operator demo, 2026-09-26: the
	// fallback previously left cache tokens out, disagreeing with the
	// cached figure once totalTokens itself was missing).
	if v, ok := rd.Usage["totalTokens"].(float64); ok && int64(v) >= out {
		return int64(v) - out, out
	}
	if v, ok := rd.Usage["input"].(float64); ok {
		in = int64(v)
	}
	if v, ok := rd.Usage["cacheRead"].(float64); ok {
		in += int64(v)
	}
	if v, ok := rd.Usage["cacheWrite"].(float64); ok {
		in += int64(v)
	}
	return in, out
}

// formatTokenCount renders n as "823", "12.3k", or "1.2M" -- compact
// enough that a multi-round summary stays well under roundSummaryMaxLen.
// The tenth is rounded half up in integer arithmetic, not by a float
// format: "%.1f" rounds an exact tie (1250 -> 1.25) to even and the
// console's formatter rounded it up, so the same run read "1.2k" here and
// "1.3k" there. console/test/fixtures/vectors/cost.json holds the cases
// both sides are tested against.
func formatTokenCount(n int64) string {
	switch {
	case n >= 1_000_000:
		tenths := (n + 50_000) / 100_000
		return fmt.Sprintf("%d.%dM", tenths/10, tenths%10)
	case n >= 1_000:
		tenths := (n + 50) / 100
		return fmt.Sprintf("%d.%dk", tenths/10, tenths%10)
	default:
		return fmt.Sprintf("%d", n)
	}
}

// buildCostUSD formats r's recorded relay spend for RoundSummary's cost
// segment, reusing statusCostMicroUSD (status.go) -- the same durable
// relay-spend evidence `factoryd status` already reports -- rather than
// recomputing it from a model/provider price list AgentEvidence doesn't
// carry. Returns "" (omit the cost segment) when nothing was recorded,
// including a partial (relay exited abnormally) total: RoundSummary's
// one-line note has no room for that caveat, so it only ever shows a cost
// it can state plainly.
func buildCostUSD(r *run.Run) string {
	total, partial := statusCostMicroUSD(r)
	if total <= 0 || partial {
		return ""
	}
	costUSD := fmt.Sprintf("%.2f", float64(total)/1e6)
	if run.SubscriptionBilled(r.Attempts) {
		costUSD += subscriptionCostSuffix
	}
	return costUSD
}
