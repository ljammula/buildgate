package sessionconfig

import (
	"strings"
	"testing"
)

// TestStaticRouteNeedsANamedCredentialFile proves a static route that needs a
// credential must name the file it reads: there is no default, so one
// provider's key can never reach another route's upstream because that route
// left credential_file empty.
func TestStaticRouteNeedsANamedCredentialFile(t *testing.T) {
	s := routesModeSettings()
	s.Routes["litellm"] = Route{CredentialMode: "static", Upstream: "https://litellm.example.invalid"}
	err := ValidateRouting(s)
	if err == nil || !strings.Contains(err.Error(), "routes.litellm: credential_file is required") {
		t.Fatalf("ValidateRouting = %v, want a refusal naming routes.litellm's missing credential_file", err)
	}
}

// TestNoCredentialRouteNeedsNoCredentialFile proves a route that opts out of a
// credential validates with no credential_file, and that defaulting never
// gives it one.
func TestNoCredentialRouteNeedsNoCredentialFile(t *testing.T) {
	s := routesModeSettings()
	s.Routes["litellm"] = Route{Upstream: "http://127.0.0.1:8080", AllowPlaintextUpstream: true, AllowNoCredential: true}
	if err := ValidateRouting(s); err != nil {
		t.Fatalf("ValidateRouting: %v, want nil for an allow_no_credential route with no credential_file", err)
	}
	if got := defaultedRoute(s.Routes["litellm"]).CredentialFile; got != "" {
		t.Fatalf("defaultedRoute gave a no-credential route credential_file %q, want none", got)
	}
}

// TestLoadRefusesRetiredCredentialEnv proves a config whose route still names
// the retired credential_env is refused when it loads, naming the route and
// credential_file, rather than read or ignored.
func TestLoadRefusesRetiredCredentialEnv(t *testing.T) {
	path := writeConfig(t, "routes:\n  anthropic:\n    upstream: https://api.anthropic.com\n    credential_env: ANTHROPIC_API_KEY\n")
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "routes.anthropic.credential_env") || !strings.Contains(err.Error(), "credential_file") {
		t.Fatalf("Load = %v, want a refusal naming routes.anthropic.credential_env and credential_file", err)
	}
}

// TestCredentialFilePathsMustBeAbsoluteOrHome proves every credential file
// key (a route's credential_file, codex_auth_file and github_token_file, and
// the webhook URL file keys) refuses a relative path, naming the key, and
// accepts an absolute path or one starting ~/.
func TestCredentialFilePathsMustBeAbsoluteOrHome(t *testing.T) {
	routes := map[string]Route{
		"credential_file":   {CredentialMode: "static", Upstream: "https://litellm.example.invalid"},
		"codex_auth_file":   {CredentialMode: "chatgpt-codex"},
		"github_token_file": {CredentialMode: "github-copilot"},
	}
	set := func(r Route, key, path string) Route {
		switch key {
		case "credential_file":
			r.CredentialFile = path
		case "codex_auth_file":
			r.CodexAuthFile = path
		case "github_token_file":
			r.GitHubTokenFile = path
		}
		return r
	}
	for key, base := range routes {
		for _, path := range []string{"keys/route", "./route", "~route"} {
			err := validateRoute("r", set(base, key, path))
			want := `routes.r.` + key + ` "` + path + `" must be an absolute path (or start with ~/)`
			if err == nil || err.Error() != want {
				t.Errorf("validateRoute(%s: %s) = %v, want %q", key, path, err, want)
			}
		}
		for _, path := range []string{"/etc/factoryd/route", "~/.config/factoryd/credentials/r"} {
			if err := validateRoute("r", set(base, key, path)); err != nil {
				t.Errorf("validateRoute(%s: %s) = %v, want nil", key, path, err)
			}
		}
	}
	for _, key := range []string{"slack_webhook_urls_file", "discord_webhook_urls_file"} {
		path := writeConfig(t, key+": hooks/slack\n")
		_, err := Load(path)
		want := "parse " + path + ": " + key + ` "hooks/slack" must be an absolute path (or start with ~/)`
		if err == nil || err.Error() != want {
			t.Errorf("Load(%s: hooks/slack) = %v, want %q", key, err, want)
		}
		if _, err := Load(writeConfig(t, key+": ~/.config/factoryd/credentials/hooks\n")); err != nil {
			t.Errorf("Load(%s: ~/...) = %v, want nil", key, err)
		}
	}
}

// TestRouteNamesMustBeLowerCaseWords proves a route name outside
// ^[a-z0-9][a-z0-9_-]*$ is refused, naming the route, and the usual names
// pass.
func TestRouteNamesMustBeLowerCaseWords(t *testing.T) {
	route := Route{Upstream: "http://127.0.0.1:8080", AllowPlaintextUpstream: true, AllowNoCredential: true}
	for _, name := range []string{"Anthropic", "a.b", "../x", "-lead", "_lead", "a/b", "a b", ""} {
		err := validateRoute(name, route)
		want := "routes." + name + ": a route name must be lower-case letters, digits, '_' or '-', starting with a letter or digit"
		if err == nil || err.Error() != want {
			t.Errorf("validateRoute(%q) = %v, want %q", name, err, want)
		}
	}
	for _, name := range []string{"anthropic", "local", "a-first", "gpt_5", "0route"} {
		if err := validateRoute(name, route); err != nil {
			t.Errorf("validateRoute(%q) = %v, want nil", name, err)
		}
	}
}
