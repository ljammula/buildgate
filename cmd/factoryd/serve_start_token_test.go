package main

import (
	"bytes"
	"log"
	"net"
	"regexp"
	"strings"
	"testing"
)

// TestGenerateStartTokenIsRandomAndSufficientlyLong proves
// generateStartToken produces a fresh, URL/header-safe token each call --
// the building block serveMain uses to fill FACTORYD_API_START_TOKEN in
// when an operator leaves it unset (F: serve-start-token).
func TestGenerateStartTokenIsRandomAndSufficientlyLong(t *testing.T) {
	t.Parallel()
	a, err := generateStartToken()
	if err != nil {
		t.Fatalf("generateStartToken: %v", err)
	}
	b, err := generateStartToken()
	if err != nil {
		t.Fatalf("generateStartToken: %v", err)
	}
	if a == b {
		t.Fatalf("generateStartToken produced the same token twice: %q", a)
	}
	// 32 bytes, base64url (RawURLEncoding, no padding) -- 43 characters.
	// Also must be safe to drop straight into a URL fragment and an
	// Authorization: Bearer header with no escaping (base64url's whole
	// point): reject anything a real base64url alphabet wouldn't produce.
	if len(a) != 43 {
		t.Fatalf("generateStartToken() = %q (len %d), want 43 chars (32 bytes, base64url, unpadded)", a, len(a))
	}
	if !regexp.MustCompile(`^[A-Za-z0-9_-]+$`).MatchString(a) {
		t.Fatalf("generateStartToken() = %q, want only base64url characters", a)
	}
}

// TestServeConsoleLinkCarriesStartTokenInFragment proves
// consoleLinkWithStartToken -- serveMain's own printed console link --
// places the start token in the URL *fragment* (#t=<token>), never a
// query param: a fragment is never sent to any server (so it never
// touches this or any other process's access logging), where a query
// param would be. See that function's own doc comment.
func TestServeConsoleLinkCarriesStartTokenInFragment(t *testing.T) {
	t.Parallel()
	got := consoleLinkWithStartToken("127.0.0.1:8090", "tok-abc123")
	want := "console: http://127.0.0.1:8090/#t=tok-abc123"
	if got != want {
		t.Fatalf("consoleLinkWithStartToken() = %q, want %q", got, want)
	}
	if strings.Contains(got, "?") {
		t.Fatalf("consoleLinkWithStartToken() = %q, must never carry the token as a query param", got)
	}
}

// serveMainLogsBeforeFailingToBind runs serveMain against an already-
// occupied listener address, so serveMain gets all the way through its
// own startup logging (including the start-run endpoint and console-link
// lines this test file cares about) before failing fast on
// ListenAndServe's own "address already in use" -- the same technique
// TestServeMainWithoutDaemonStartsWithUnwritableHarnessCache (harness_
// default_test.go) uses to reach past flag/startup validation without
// actually serving. Returns the captured log output. Not parallel-safe:
// redirects the shared log package's output (log.SetOutput), like every
// other test in this package that does the same (see
// daemon_and_reclaim_checks_test.go's own comment on the pattern).
func serveMainLogsBeforeFailingToBind(dp *deps, t *testing.T, args []string) string {
	t.Helper()
	isolateSessionConfig(t)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve a test listener: %v", err)
	}
	t.Cleanup(func() { listener.Close() })

	var logBuf bytes.Buffer
	previousOutput := log.Writer()
	log.SetOutput(&logBuf)
	t.Cleanup(func() { log.SetOutput(previousOutput) })
	t.Cleanup(func() { tier2SettingsOverride = nil })

	fullArgs := append([]string{"-data-dir", t.TempDir(), "-addr", listener.Addr().String()}, args...)
	if err := serveMain(dp, fullArgs); err == nil {
		t.Fatal("serveMain unexpectedly succeeded (it should have failed to bind the already-occupied address)")
	}
	return logBuf.String()
}

// TestServeGeneratesStartTokenWhenUnset proves that leaving
// FACTORYD_API_START_TOKEN unset no longer leaves the start-run endpoint
// in a permanently unreachable state for the console (which has no way to
// ever learn a manually-configured env var, see console/src/platform/startToken.ts):
// serveMain generates its own per-process token and says so in its
// startup log, instead of the old "start-run endpoint disabled" line.
func TestServeGeneratesStartTokenWhenUnset(t *testing.T) {
	dp := newTestDeps(t)
	t.Setenv(overrideTokenEnvironmentVariable, "")
	t.Setenv(startTokenEnvironmentVariable, "")
	t.Setenv(readTokenEnvironmentVariable, "")

	logs := serveMainLogsBeforeFailingToBind(dp, t, nil)

	if strings.Contains(logs, "start-run endpoint disabled") {
		t.Fatalf("serve log still claims the start-run endpoint is disabled with no token configured -- want it to say a token was generated:\n%s", logs)
	}
	if !strings.Contains(logs, "start-run endpoint enabled (no "+startTokenEnvironmentVariable+" configured -- generated a per-process token") {
		t.Fatalf("serve log = %q, want a line saying a per-process start token was generated", logs)
	}
}

// TestServeKeepsOperatorStartToken proves an operator-configured
// FACTORYD_API_START_TOKEN is used unchanged -- serveMain must not
// generate or otherwise replace it -- while still logging normally (the
// "using configured" wording, not the generated one above).
func TestServeKeepsOperatorStartToken(t *testing.T) {
	dp := newTestDeps(t)
	t.Setenv(overrideTokenEnvironmentVariable, "")
	t.Setenv(startTokenEnvironmentVariable, "operator-chosen-secret")
	t.Setenv(readTokenEnvironmentVariable, "")

	logs := serveMainLogsBeforeFailingToBind(dp, t, nil)

	if strings.Contains(logs, "generated a per-process token") {
		t.Fatalf("serve log claims a token was generated despite an operator-configured %s:\n%s", startTokenEnvironmentVariable, logs)
	}
	if !strings.Contains(logs, "start-run endpoint enabled (using configured "+startTokenEnvironmentVariable+")") {
		t.Fatalf("serve log = %q, want a line saying the configured token is in use", logs)
	}
}
