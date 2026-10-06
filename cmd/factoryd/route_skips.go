package main

import (
	"buildgate/internal/modelrole"
	"buildgate/internal/reviewstep"
	"buildgate/internal/run"
)

// routeSkipsOf converts a role selection's skipped route candidates to the
// run record's own type. A nil selection or one with no skips yields nil.
func routeSkipsOf(sel *modelrole.Selection) []run.RouteSkip {
	if sel == nil || len(sel.Skipped) == 0 {
		return nil
	}
	out := make([]run.RouteSkip, 0, len(sel.Skipped))
	for _, s := range sel.Skipped {
		out = append(out, run.RouteSkip{Route: s.Route, Reason: s.Reason})
	}
	return out
}

// stampRouteSkips sets each model-backed attempt's RelayRouteSkipped from
// the skips of the role that ran it: review-step attempts (Kind
// spec_conformity, code_review or review) take the review role's, build
// attempts the execution role's. Every other attempt (verify, gates, oracle
// canary) runs no model and is left alone. An empty list leaves the field untouched.
func stampRouteSkips(attempts []run.Attempt, opts temporalSliceOptions) {
	for i := range attempts {
		switch attempts[i].Kind {
		case reviewstep.SpecConformity, reviewstep.CodeReview, reviewstep.Combined:
			if len(opts.ReviewRouteSkips) > 0 {
				attempts[i].RelayRouteSkipped = opts.ReviewRouteSkips
			}
		case "build":
			if len(opts.ExecutionRouteSkips) > 0 {
				attempts[i].RelayRouteSkipped = opts.ExecutionRouteSkips
			}
		}
	}
}
