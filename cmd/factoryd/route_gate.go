package main

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"buildgate/internal/meter"
	"buildgate/internal/modelrole"
	"buildgate/internal/requestdriver"
	"buildgate/internal/sandbox"
	"buildgate/internal/sessionconfig"
)

// routeWaitRecheck is how often a request whose step waits for its model
// route (request.Request.RouteWait) checks the route again.
const routeWaitRecheck = 30 * time.Second

// RouteWait is requestdriver.Deps' RouteWait: routeWaitReason.
func (dp *deps) RouteWait(s sessionconfig.Settings, need requestdriver.RouteNeed) string {
	return routeWaitReason(s, need)
}

// routeWaitReason says why a model job on need cannot launch because no route
// of its role has a usable credential, "" when one has. Only a credential
// that cannot be used counts (sandbox.ErrRouteCredential: a missing key, a
// token that expires within its margin): it is fixed outside the factory.
// Any other route fault is left to the launch, which reports it as before.
// The route is chosen as the job chooses it (modelrole.SelectRoute over
// resolveRouteCredentials, a drafting job's planning role falling back to
// execution as resolveRequestJobRole does), so the check and the launch
// agree; no model is called.
func routeWaitReason(s sessionconfig.Settings, need requestdriver.RouteNeed) string {
	if len(s.Routes) == 0 {
		return ""
	}
	role, model, harness := modelrole.Role(need.Role), need.Model, need.Harness
	if role == modelrole.RolePlanning {
		if role = requestJobEffectiveRole(s, modelrole.StagePlanning); role != modelrole.RolePlanning {
			model, harness = "", ""
		}
	}
	failed := map[string]error{}
	probe := func(name string, route sessionconfig.Route) error {
		_, err := resolveRouteCredentials(route)
		if err != nil {
			failed[name] = err
		}
		return err
	}
	if _, err := modelrole.SelectRoute(s, role, model, harness, "", probe); err != nil && errors.Is(err, sandbox.ErrRouteCredential) {
		return routeWaitSentence(s, role, failed)
	}
	return ""
}

// routeWaitSentence is the operator's line for a role none of whose routes
// has a usable credential: each route's cause, and how the fix is picked up.
// A credential read from a file is read again on every check; a key read from
// the worker's environment is fixed only by restarting the worker with it.
func routeWaitSentence(s sessionconfig.Settings, role modelrole.Role, failed map[string]error) string {
	names := make([]string, 0, len(failed))
	for name := range failed {
		names = append(names, name)
	}
	slices.Sort(names)
	causes := make([]string, 0, len(names))
	restart := false
	for _, name := range names {
		causes = append(causes, fmt.Sprintf("route %s: %v", name, failed[name]))
		restart = restart || credentialFromWorkerEnv(s.Routes[name])
	}
	next := fmt.Sprintf("the worker checks again every %s and starts it once the credential is usable", routeWaitRecheck)
	if restart {
		next = "set the key in the worker's environment and run `factoryd restart`; the worker then starts it"
	}
	return fmt.Sprintf("waiting for roles.%s's model route: %s. The step has not started; %s", role, strings.Join(causes, "; "), next)
}

// credentialFromWorkerEnv reports whether route's credential is read from the
// worker process's own environment, which only a restart changes: a static
// key, or a github-copilot route with no token file.
func credentialFromWorkerEnv(route sessionconfig.Route) bool {
	switch route.EffectiveCredentialMode() {
	case meter.CredentialModeStatic:
		return true
	case meter.CredentialModeGitHubCopilot:
		return route.GitHubTokenFile == ""
	}
	return false
}
