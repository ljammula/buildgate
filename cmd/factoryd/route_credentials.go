package main

import (
	"fmt"
	"os"

	"buildgate/internal/meter"
	"buildgate/internal/modelrole"
	"buildgate/internal/sandbox"
	"buildgate/internal/sessionconfig"
	"buildgate/internal/workflow"
)

// routeCredentials is the one credential value shape every relay launch
// site (the direct build, conformity/review, and drafting) needs,
// regardless of which of a route's three credential_mode values resolved
// it -- only the field(s) that mode actually uses are populated; the rest
// stay the zero, unconfigured sandbox.RouteSecret. Mirrors
// sandbox.RouteSpec's own four credential fields (APIKey/GitHubToken/
// ChatGPTToken/ChatGPTAccountID), which is where these ultimately land.
type routeCredentials struct {
	apiKey           sandbox.RouteSecret
	githubToken      sandbox.RouteSecret
	chatGPTToken     sandbox.RouteSecret
	chatGPTAccountID sandbox.RouteSecret
}

// resolveRouteCredentials is the single credential resolver every routes:
// mode relay launch site calls -- replacing the three near-identical
// credential_mode switch blocks that used to be duplicated across
// run_ticket.go, spec_draft_job.go, and internal/workflow/activities.go.
// It resolves
// r's own credential_mode against r's own credential source fields only
// (r.CodexAuthFile, r.GitHubTokenFile/GitHubTokenKey, r.CredentialEnv) --
// never any other route's -- and runs fresh on every call (a ChatGPT
// access token in particular must never be cached across a long-running
// worker; see resolveChatGPTCodexCredential's own doc comment).
//
// Also used, with probe's own resolved value discarded, as
// modelrole.SelectRoute's own credential-availability check (see that
// function's own doc comment) -- so a route this resolver would fail on
// is skipped before any relay ever launches, not only when the real
// launch call below hits the same failure.
func resolveRouteCredentials(r sessionconfig.Route) (routeCredentials, error) {
	mode := r.EffectiveCredentialMode()
	switch mode {
	case meter.CredentialModeChatGPTCodex:
		token, accountID, err := resolveChatGPTCodexCredential(r.CodexAuthFile)
		if err != nil {
			return routeCredentials{}, err
		}
		return routeCredentials{
			chatGPTToken:     sandbox.NewRouteSecret(token),
			chatGPTAccountID: sandbox.NewRouteSecret(accountID),
		}, nil
	case meter.CredentialModeGitHubCopilot:
		token, err := resolveGitHubCopilotToken(r.GitHubTokenFile, r.GitHubTokenKey)
		if err != nil {
			return routeCredentials{}, err
		}
		if token == "" {
			return routeCredentials{}, fmt.Errorf("route: github-copilot credential_mode requires a GitHub OAuth token: set github_token_file, or GITHUB_COPILOT_TOKEN")
		}
		return routeCredentials{githubToken: sandbox.NewRouteSecret(token)}, nil
	default: // static
		env := r.CredentialEnv
		if env == "" {
			env = "ANTHROPIC_API_KEY"
		}
		value := os.Getenv(env)
		if value == "" && !r.AllowNoCredential {
			return routeCredentials{}, fmt.Errorf("route: static credential_mode requires %s in this process's environment, unless allow_no_credential: true is set for a model endpoint with no real credential to protect", env)
		}
		if value != "" && r.AllowPlaintextUpstream {
			if err := sandbox.CredentialSafeForUpstream(r.Upstream, true, true); err != nil {
				return routeCredentials{}, err
			}
		}
		return routeCredentials{apiKey: sandbox.NewRouteSecret(value)}, nil
	}
}

// checkRouteFunc builds a Temporal Worker's own
// workflow.Activities.CheckRoute closure over settings:
// modelrole.CheckRouteBinding is this Worker's own trust check that a
// submitted RoutePolicy/ReviewRelayPolicy is an EXACT match (a target
// repo's project config may only tighten TokenCeiling/
// CostCeilingMicroUSD, never any other field) for role's own configured
// model on this Worker's own routes:/models: config -- see that
// function's own doc comment. role is converted straight to
// modelrole.Role: workflow.Activities.CheckRoute takes a plain string
// (relayRoleExecution/relayRoleReview) so internal/workflow itself never
// needs to import internal/modelrole. Always constructed and wired onto
// every Worker's own Activities (see modelRouteOptions.CheckRoute's own doc
// comment): routes:/models:/roles: is the only session-config schema, so
// every Worker resolves every submitted route through this closure.
func checkRouteFunc(s sessionconfig.Settings) func(role string, p sandbox.RoutePolicy, thinking string) error {
	return func(role string, p sandbox.RoutePolicy, thinking string) error {
		return modelrole.CheckRouteBinding(s, modelrole.Role(role), p, thinking)
	}
}

// resolveRouteCredentialsFunc builds a Temporal Worker's own
// workflow.Activities.ResolveRouteCredentials closure over settings:
// looks up routeName in s's OWN routes:/models: config (s.Routing(), the
// same defaulted map modelrole.CheckRouteBinding/SelectRoute build
// against) and resolves ITS credential (resolveRouteCredentials) -- the
// Worker's own caller (boundRelaySpec/checkAndResolveRoute) only ever
// calls this after CheckRoute has already accepted a policy naming this
// same route, never independently.
func resolveRouteCredentialsFunc(s sessionconfig.Settings) func(routeName string) (workflow.RouteCredentials, error) {
	return func(routeName string) (workflow.RouteCredentials, error) {
		routes, _, err := s.Routing()
		if err != nil {
			return workflow.RouteCredentials{}, err
		}
		route, ok := routes[routeName]
		if !ok {
			return workflow.RouteCredentials{}, fmt.Errorf("route %q is not configured on this Worker", routeName)
		}
		creds, err := resolveRouteCredentials(route)
		if err != nil {
			return workflow.RouteCredentials{}, err
		}
		return workflow.RouteCredentials{
			APIKey:           creds.apiKey,
			GitHubToken:      creds.githubToken,
			ChatGPTToken:     creds.chatGPTToken,
			ChatGPTAccountID: creds.chatGPTAccountID,
		}, nil
	}
}
