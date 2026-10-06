// ValidateAllowedPolicies -- config-time proof that every model a role
// could actually resolve to would produce a launchable relay policy. See
// its own doc comment below for the live case this exists for.
package modelrole

import (
	"fmt"

	"buildgate/internal/sessionconfig"
)

// ValidateAllowedPolicies proves every model roles.<role>.model or
// roles.<role>.allowed could resolve to would actually produce a
// launchable sandbox.RoutePolicy on every one of that model's own
// declared routes -- the same policy construction (relayPolicyForRoute)
// and validation (RoutePolicy.Validate, RoutePolicy.ValidateUpstreamScheme)
// SelectRoute itself runs at launch time, but run here at config-load
// time instead, before a human ever spends review time approving a
// spec/plan built against a model that can only fail once the real build
// starts.
//
// Found live (M3 walk, 2026-09-28): roles.execution had
// allowed: [qwen, qwen-path], and qwen-path's id was a local filesystem
// path ("/Users/.../Qwen3.8-27B-MTPLX-Optimized-Quality") containing a
// slash. sandbox.RoutePolicy.Validate already rejected that (pi's own
// CLI hangs resolving a slash-containing --model value together with an
// explicit --provider), but nothing checked an `allowed` entry against
// Validate before launch -- config load, `factoryd doctor` (16/16
// passed), and `factoryd submit -model execution=qwen-path` all accepted
// it, a human drafted and approved a spec and a plan against it, and the
// ticket build only halted at start, inside SelectRoute, with a skip
// reason that didn't even say why ("route's relay policy failed
// validation") -- see SelectRoute's own RouteSkip doc comment for the
// other half of this fix.
//
// Deliberately omits the credential probe SelectRoute itself also runs
// (its own probe parameter): that step needs a real credential resolver
// and stays a launch-time/doctor-only reachability check -- this only
// proves the policy itself is well-formed, which needs no external
// dependency and so can (and, until this fix, didn't) run at config-load
// time for every candidate model, not just the one a run happens to
// pick.
func ValidateAllowedPolicies(s sessionconfig.Settings) error {
	if s.Routes == nil && s.Models == nil && s.Roles == nil {
		return nil
	}

	routes, models, err := s.Routing()
	if err != nil {
		// sessionconfig.ValidateRouting already refuses a config that
		// would fail here (unknown model, a route_ids key outside the
		// model's own routes, ...) before this ever runs in practice --
		// fail closed rather than let a nil map panic below.
		return fmt.Errorf("modelrole: %w", err)
	}

	for _, r := range []Role{RolePlanning, RoleExecution, RoleReview} {
		rc := roleConfig(s, r)
		if rc == nil || rc.Model == "" {
			continue
		}
		for _, modelName := range candidateModelNames(rc) {
			model, ok := models[modelName]
			if !ok {
				// sessionconfig.ValidateRouting already refuses an
				// allowed/model name that isn't a models: entry.
				continue
			}
			for _, routeName := range model.Routes {
				route, ok := routes[routeName]
				if !ok {
					// ditto: ValidateRouting already refuses a model
					// route not declared in routes:.
					continue
				}
				policy, err := relayPolicyForRoute(s, model, routeName, route)
				if err != nil {
					return fmt.Errorf("roles.%s.allowed model %q on route %q: %w", r, modelName, routeName, err)
				}
				if err := policy.Validate(); err != nil {
					return fmt.Errorf("roles.%s.allowed model %q on route %q: relay policy failed validation: %w", r, modelName, routeName, err)
				}
				if err := policy.ValidateUpstreamScheme(); err != nil {
					return fmt.Errorf("roles.%s.allowed model %q on route %q: relay policy failed validation: %w", r, modelName, routeName, err)
				}
			}
		}
	}
	return nil
}
