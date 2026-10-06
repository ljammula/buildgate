package main

import "testing"

func TestResolveConsoleBaseURL(t *testing.T) {
	t.Run("flag wins over env", func(t *testing.T) {
		t.Setenv(consoleLinkEnvVar, "http://env-console:9000")
		if got := resolveConsoleBaseURL("http://flag-console:8000", t.TempDir()); got != "http://flag-console:8000" {
			t.Errorf("got %q, want flag value", got)
		}
	})

	t.Run("falls back to env when flag unset", func(t *testing.T) {
		t.Setenv(consoleLinkEnvVar, "http://env-console:9000")
		if got := resolveConsoleBaseURL("", t.TempDir()); got != "http://env-console:9000" {
			t.Errorf("got %q, want env value", got)
		}
	})

	t.Run("empty when neither set", func(t *testing.T) {
		t.Setenv(consoleLinkEnvVar, "")
		if got := resolveConsoleBaseURL("", t.TempDir()); got != "" {
			t.Errorf("got %q, want empty", got)
		}
	})
}

func TestConsoleRequestURL(t *testing.T) {
	cases := []struct {
		name string
		base string
		id   string
		want string
	}{
		{"empty base skips link", "", "req-123", ""},
		{"joins base and id", "http://localhost:8090", "req-123", "http://localhost:8090/requests/req-123"},
		{"trims trailing slash", "http://localhost:8090/", "req-123", "http://localhost:8090/requests/req-123"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := consoleRequestURL(tc.base, tc.id); got != tc.want {
				t.Errorf("consoleRequestURL(%q, %q) = %q, want %q", tc.base, tc.id, got, tc.want)
			}
		})
	}
}
