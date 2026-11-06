package requestdriver

import (
	"time"

	"buildgate/internal/request"
	"buildgate/internal/unresolved"
)

// RouteNeed is one role a model job is about to launch on, with the model and
// harness the job will ask for ("" for the role's own).
type RouteNeed struct {
	// Role is "planning" (a drafting job: the worker falls back to
	// execution when planning is unset), "execution" or "review".
	Role, Model, Harness string
	// ResumedAt is when the operator resumed this request after the
	// provider refused it with its own limit (RFC 3339): that resume says the
	// limit has reset, so no quota hold from a refusal at or before it holds
	// this launch. "" otherwise.
	ResumedAt string
}

// holdForRoutes records a route wait on r and reports held when a role of
// needs has no route whose credential can be used (Deps.RouteWait), and ends
// an earlier route wait when every role has one. It runs immediately before a
// model job launches, so a step that launches none never waits. Nothing has
// run, so nothing is lost: r keeps its state and the worker checks again on
// its next pass. held is true whenever err is set.
func holdForRoutes(dp Deps, dataDir string, r *request.Request, cfg WorkerConfig, needs []RouteNeed, now time.Time) (held bool, err error) {
	resumedAt := quotaResumedAt(r)
	for _, need := range needs {
		need.ResumedAt = resumedAt
		if reason := dp.RouteWait(dataDir, cfg.Settings, need); reason != "" {
			return true, RecordRouteWait(dp, dataDir, r, reason, cfg.HitlReminderInterval, now)
		}
	}
	if err := EndRouteWait(dataDir, r); err != nil {
		return true, err
	}
	return false, nil
}

// quotaResumedAt is RouteNeed.ResumedAt for r: the time of the operator's
// resume decision its step applies, when the lost step it answers was a
// provider-quota refusal.
func quotaResumedAt(r *request.Request) string {
	if r.PendingResumeVerb() == "" || r.Resume.Cause != unresolved.ProviderQuota {
		return ""
	}
	return r.ResumeDecision.At
}

// draftingNeeds is the role a spec drafting or planning job of r launches on.
func draftingNeeds(r *request.Request) []RouteNeed {
	return []RouteNeed{{Role: "planning", Model: r.Models["planning"], Harness: r.Harnesses["planning"]}}
}

// buildNeeds are the roles a ticket build of r launches on: execution, on the
// escalation's model when the build is escalated (escalate), and review when
// configured. The challenger is not one: a challenger with no usable route is
// a challenge with no verdict and the build goes on (SC-022).
func buildNeeds(r *request.Request, cfg WorkerConfig, escalated bool) []RouteNeed {
	exec := RouteNeed{Role: "execution", Model: r.Models["execution"], Harness: r.Harnesses["execution"]}
	if escalated && executionEscalation(cfg) != nil {
		exec.Model = cfg.Settings.Roles.Execution.EscalationModel()
	}
	needs := []RouteNeed{exec}
	if cfg.Settings.Roles != nil && cfg.Settings.Roles.Review != nil {
		needs = append(needs, RouteNeed{Role: "review"})
	}
	return needs
}
