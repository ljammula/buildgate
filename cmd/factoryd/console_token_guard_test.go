package main

import "testing"

// TestConsoleBaseIsOwnLoopbackServe is the regression test for an
// adversarial-review finding: only an EXACT loopback-host, matching-port
// base counts as "this machine's own serve" -- anything else (a remote
// FACTORYD_CONSOLE_URL, a mismatched port, a malformed URL) must not.
func TestConsoleBaseIsOwnLoopbackServe(t *testing.T) {
	t.Parallel()
	const serveAddr = "127.0.0.1:8090"

	cases := []struct {
		name string
		base string
		want bool
	}{
		{"exact 127.0.0.1 match", "http://127.0.0.1:8090", true},
		{"localhost match", "http://localhost:8090", true},
		{"IPv6 loopback match", "http://[::1]:8090", true},
		{"trailing slash still matches", "http://127.0.0.1:8090/", true},
		{"remote host", "http://example.com:8090", false},
		{"remote host default console port guess", "http://192.168.1.50:8090", false},
		{"loopback but wrong port", "http://127.0.0.1:9999", false},
		{"empty base", "", false},
		{"malformed URL", "http://[::1", false},
		{"scheme-relative garbage", "not-a-url", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := consoleBaseIsOwnLoopbackServe(tc.base, serveAddr)
			if got != tc.want {
				t.Errorf("consoleBaseIsOwnLoopbackServe(%q, %q) = %v, want %v", tc.base, serveAddr, got, tc.want)
			}
		})
	}
}

// TestConsoleBaseIsOwnLoopbackServeEmptyServeAddr proves an empty
// serveAddr (nothing to compare against) never matches, rather than
// panicking or trivially matching everything.
func TestConsoleBaseIsOwnLoopbackServeEmptyServeAddr(t *testing.T) {
	t.Parallel()
	if consoleBaseIsOwnLoopbackServe("http://127.0.0.1:8090", "") {
		t.Error("want false when serveAddr is empty")
	}
}
