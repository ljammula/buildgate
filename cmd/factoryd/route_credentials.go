package main

import (
	"errors"
	"fmt"
	"os"

	"buildgate/internal/meter"
	"buildgate/internal/modelrole"
	"buildgate/internal/sandbox"
	"buildgate/internal/sessionconfig"
	"buildgate/internal/workflow"
)

// routeCredentials is the one credential value shape every relay launch
// site (the host-side model jobs, and the workflow's activities through
// Activities.ResolveRouteCredentials) needs,
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
// (r.CodexAuthFile, r.GitHubTokenFile/GitHubTokenKey, r.CredentialFile) --
// never any other route's -- and runs fresh on every call (a ChatGPT
// access token in particular must never be cached across a long-running
// worker; see resolveChatGPTCodexCredential's own doc comment).
//
// Also used, with probe's own resolved value discarded, as
// modelrole.SelectRoute's own credential-availability check (see that
// function's own doc comment) -- so a route this resolver would fail on
// is skipped before any relay ever launches, not only when the real
// launch call below hits the same failure.
//
// A credential that cannot be read or is missing, or a ChatGPT token too
// close to expiry, is returned with sandbox.ErrRouteCredential in its chain:
// that is where the factory observes the fact unresolved.RouteCredential
// names. A static key unsafe for its upstream is a configuration fault and
// does not carry it.
func resolveRouteCredentials(r sessionconfig.Route) (routeCredentials, error) {
	mode := r.EffectiveCredentialMode()
	switch mode {
	case meter.CredentialModeChatGPTCodex:
		token, accountID, err := resolveChatGPTCodexCredential(r.CodexAuthFile)
		if err != nil {
			return routeCredentials{}, sandbox.RouteCredentialError(err)
		}
		return routeCredentials{
			chatGPTToken:     sandbox.NewRouteSecret(token),
			chatGPTAccountID: sandbox.NewRouteSecret(accountID),
		}, nil
	case meter.CredentialModeGitHubCopilot:
		token, err := resolveGitHubCopilotToken(r.GitHubTokenFile, r.GitHubTokenKey)
		if err != nil {
			return routeCredentials{}, sandbox.RouteCredentialError(err)
		}
		if token == "" {
			return routeCredentials{}, sandbox.RouteCredentialError(fmt.Errorf("route: github-copilot credential_mode requires a GitHub OAuth token: set github_token_file, or GITHUB_COPILOT_TOKEN"))
		}
		return routeCredentials{githubToken: sandbox.NewRouteSecret(token)}, nil
	default: // static
		value, err := resolveStaticRouteKey(r)
		if err != nil {
			return routeCredentials{}, err
		}
		return routeCredentials{apiKey: sandbox.NewRouteSecret(value)}, nil
	}
}

// resolveStaticRouteKey reads a static route's key from the file its
// credential_file names (readSecretValue), fresh on every call: "" with no
// error for an allow_no_credential route that names no file. It reads no
// environment variable and no other route's file. A file that cannot be
// used carries sandbox.ErrRouteCredential; a route that needs a key and
// names no file, and a key its plaintext upstream must not carry, are
// configuration faults and do not.
func resolveStaticRouteKey(r sessionconfig.Route) (string, error) {
	if r.CredentialFile == "" {
		// Never a default, which would hand another provider's key to this
		// route's upstream.
		if r.AllowNoCredential {
			return "", nil
		}
		// ValidateRouting refuses this first.
		return "", fmt.Errorf("route: a static route needs credential_file naming the file (mode 0600) that holds its key, unless allow_no_credential: true is set for a model endpoint with no real credential to protect")
	}
	if r.AllowNoCredential {
		// The route may be reached with no key: only a file that is there is
		// read (and still checked).
		if path, err := expandAuthPath(r.CredentialFile); err == nil {
			if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
				return "", nil
			}
		}
	}
	value, err := readSecretValue(r.CredentialFile)
	if err != nil {
		return "", sandbox.RouteCredentialError(fmt.Errorf("route credential_file: %w", err))
	}
	if r.AllowPlaintextUpstream {
		if err := sandbox.CredentialSafeForUpstream(r.Upstream, true, true); err != nil {
			return "", err
		}
	}
	return value, nil
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
