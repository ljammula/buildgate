package main

import (
	"strings"
	"testing"
)

func TestValidateCORSAllowOrigin(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		origin  string
		want    string
		wantErr bool
	}{
		{"empty is fine, CORS stays off", "", "", false},
		{"bare origin", "http://localhost:8091", "http://localhost:8091", false},
		{"https origin", "https://console.example.com", "https://console.example.com", false},
		{"origin with no port", "http://localhost", "http://localhost", false},
		{"wildcard rejected", "*", "", true},
		{"path component rejected", "http://localhost:8091/requests", "", true},
		{"query component rejected", "http://localhost:8091?x=1", "", true},
		{"userinfo rejected", "http://user:pass@localhost:8091", "", true},
		{"no scheme rejected", "localhost:8091", "", true},
		{"not a URL at all", "not a url", "", true},
		{"non-http(s) scheme rejected", "ftp://localhost:8091", "", true},
		// Normalization: a browser's own Origin header is always
		// lowercase scheme/host with the scheme's default port omitted,
		// so anything else must canonicalize to that shape or the later
		// raw string comparison in api.Server.ServeHTTP would silently
		// never match (GitHub Codex App review of this PR, P2).
		{"uppercase scheme/host normalized", "HTTP://LOCALHOST:8091", "http://localhost:8091", false},
		{"explicit default http port stripped", "http://localhost:80", "http://localhost", false},
		{"explicit default https port stripped", "https://localhost:443", "https://localhost", false},
		{"non-default port kept", "http://localhost:8091", "http://localhost:8091", false},
		{"IPv6 brackets preserved with non-default port", "http://[::1]:8091", "http://[::1]:8091", false},
		{"IPv6 brackets preserved with default port stripped", "https://[::1]:443", "https://[::1]", false},
		{"IPv6 brackets preserved with uppercase", "HTTP://[::1]:8091", "http://[::1]:8091", false},
		{"leading-zero port rejected", "http://localhost:080", "", true},
		{"leading-zero default port rejected", "http://localhost:0080", "", true},
		{"port zero rejected", "http://localhost:0", "", true},
		{"port out of range rejected", "http://localhost:70000", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := validateCORSAllowOrigin(tc.origin)
			if tc.wantErr {
				if err == nil {
					t.Errorf("validateCORSAllowOrigin(%q) = (%q, nil), want an error", tc.origin, got)
				}
				return
			}
			if err != nil {
				t.Errorf("validateCORSAllowOrigin(%q) = (_, %v), want nil error", tc.origin, err)
			}
			if got != tc.want {
				t.Errorf("validateCORSAllowOrigin(%q) = %q, want %q", tc.origin, got, tc.want)
			}
		})
	}
}

// TestApiSandboxPolicyImageAllowedEmptyAllowlist proves the empty-
// allowlist fallback: it means exactly this daemon's own configured
// defaultImage, not sandbox.CanonicalImageRef (removed -- every image is
// built from source now, never pulled from a registry) and not "anything"
// -- and when the daemon itself has no image configured either, an empty
// allowlist accepts NOTHING (a naive "empty allowlist accepts everything"
// implementation here would have reopened the exact containment gap the
// image-allowlist fix closed).
func TestApiSandboxPolicyImageAllowedEmptyAllowlist(t *testing.T) {
	t.Parallel()
	configured := "localhost:5050/buildgate-worker@sha256:" + strings.Repeat("a", 64)
	other := "attacker.example/evil@sha256:" + strings.Repeat("b", 64)

	withDefault := apiSandboxPolicy{defaultImage: configured}
	if !withDefault.imageAllowed(configured) {
		t.Error("imageAllowed(configured) = false, want true: an empty allowlist must accept this daemon's own configured image")
	}
	if withDefault.imageAllowed(other) {
		t.Error("imageAllowed(other) = true, want false: an empty allowlist must accept only the configured image, not any digest-pinned one")
	}
	if got, want := withDefault.allowedImagesDescription(), configured; got != want {
		t.Errorf("allowedImagesDescription() = %q, want %q", got, want)
	}

	noDefault := apiSandboxPolicy{}
	if noDefault.imageAllowed(configured) {
		t.Error("imageAllowed(configured) = true, want false: with no allowlist and no daemon-configured image, nothing should be accepted")
	}
	if got, want := noDefault.allowedImagesDescription(), "none configured"; got != want {
		t.Errorf("allowedImagesDescription() = %q, want %q", got, want)
	}
}
