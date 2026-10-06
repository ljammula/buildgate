package requestdriver

import (
	"fmt"
	"strings"

	"buildgate/internal/run"
)

// StatusReason returns why a run with no PullRequestURL has none to show:
// its recorded halt error, its machine-readable halt reason code, or its
// first failed gate check, in that order of preference. Empty if none of
// those are set (e.g. a run still in progress).
func StatusReason(r *run.Run) string {
	if r.Triage != "" {
		// A halt's triage is a category ("halted: sandbox/relay timeout")
		// classified from HaltError by substring, so the exact error text
		// still follows it -- the category alone can hide the real cause
		// when the match was loose (mirrors notify.PrepareHalt).
		if r.HaltError != "" && !strings.Contains(r.Triage, r.HaltError) {
			return fmt.Sprintf("%s (%s)", r.Triage, r.HaltError)
		}
		return r.Triage
	}
	if r.HaltError != "" {
		return r.HaltError
	}
	if r.HaltReasonCode != "" {
		return r.HaltReasonCode
	}
	for _, g := range r.GateResults {
		if !g.Passed {
			return fmt.Sprintf("gate failed: %s", g.Check)
		}
	}
	return ""
}
