package sessionconfig

import (
	"strings"
	"testing"
)

// TestStaticRouteNeedsANamedCredentialEnv proves a static route that needs a
// credential must name the variable it reads: there is no default name, so
// one provider's key (an inherited ANTHROPIC_API_KEY) can never reach
// another route's upstream because that route left credential_env empty.
func TestStaticRouteNeedsANamedCredentialEnv(t *testing.T) {
	s := routesModeSettings()
	s.Routes["litellm"] = Route{CredentialMode: "static", Upstream: "https://litellm.example.invalid"}
	err := ValidateRouting(s)
	if err == nil || !strings.Contains(err.Error(), "routes.litellm: credential_env is required") {
		t.Fatalf("ValidateRouting = %v, want a refusal naming routes.litellm's missing credential_env", err)
	}
}

// TestNoCredentialRouteNeedsNoCredentialEnv proves a route that opts out of a
// credential validates with no credential_env, and that defaulting never
// gives it one.
func TestNoCredentialRouteNeedsNoCredentialEnv(t *testing.T) {
	s := routesModeSettings()
	s.Routes["litellm"] = Route{Upstream: "http://127.0.0.1:8080", AllowPlaintextUpstream: true, AllowNoCredential: true}
	if err := ValidateRouting(s); err != nil {
		t.Fatalf("ValidateRouting: %v, want nil for an allow_no_credential route with no credential_env", err)
	}
	if got := defaultedRoute(s.Routes["litellm"]).CredentialEnv; got != "" {
		t.Fatalf("defaultedRoute gave a no-credential route credential_env %q, want none", got)
	}
}
