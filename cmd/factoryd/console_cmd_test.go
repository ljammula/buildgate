package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"buildgate/internal/consolelink"
)

// TestConsoleMainNoSessionConfigPrintsPlainLink proves `factoryd console`
// falls back to a plain, tokenless link (and says why) when no session
// config is found at all -- there is then no stable token file to read.
func TestConsoleMainNoSessionConfigPrintsPlainLink(t *testing.T) {
	dp := newTestDeps(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "no-xdg"))

	out := captureStdout(t, func() {
		if err := consoleMain(dp, nil); err != nil {
			t.Fatalf("consoleMain: %v", err)
		}
	})
	if !strings.Contains(out, "no session config found") {
		t.Errorf("output = %q, want a note that no session config was found", out)
	}
	if strings.Contains(out, "#t=") {
		t.Errorf("output = %q, want no token fragment with no session config", out)
	}
}

// TestConsoleMainPrintsStableTokenLink proves `factoryd console` reads
// back the stable start token file and includes it in the printed link's
// #t= fragment.
func TestConsoleMainPrintsStableTokenLink(t *testing.T) {
	dp := newTestDeps(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	cfgDir := filepath.Join(home, "xdg", "factoryd")
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "xdg"))
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "config.yml"), []byte("data_dir: "+filepath.Join(home, "data")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	token, _, err := ensureServeStartTokenFile(dp, filepath.Join(cfgDir, serveStableStartTokenFileName))
	if err != nil {
		t.Fatalf("ensureServeStartTokenFile: %v", err)
	}
	t.Setenv(consolelink.EnvVar, "")
	origListening := consolelink.Listening
	t.Cleanup(func() { consolelink.Listening = origListening })
	consolelink.Listening = func(string) bool { return true }

	// No serve recorded for this data dir: the default address may be
	// another data dir's serve, so the token is withheld.
	out := captureStdout(t, func() {
		if err := consoleMain(dp, nil); err != nil {
			t.Fatalf("consoleMain: %v", err)
		}
	})
	if strings.Contains(out, token) {
		t.Fatalf("output = %q, attached the token with no serve recorded for this data dir", out)
	}

	// This data dir's own serve (this process stands in for it) recorded
	// its address: the link carries the token, on that address.
	remove, err := consolelink.RecordServeAddress(filepath.Join(home, "data"), "127.0.0.1:8092")
	if err != nil {
		t.Fatal(err)
	}
	defer remove()
	out = captureStdout(t, func() {
		if err := consoleMain(dp, nil); err != nil {
			t.Fatalf("consoleMain: %v", err)
		}
	})
	if !strings.Contains(out, "http://127.0.0.1:8092/#t="+token) {
		t.Errorf("output = %q, want the recorded serve's link carrying #t=%s", out, token)
	}

	// A serve on another data dir than the config's (install-service
	// -data-dir) is found through -data-dir.
	otherDataDir := filepath.Join(home, "serve-data")
	removeOther, err := consolelink.RecordServeAddress(otherDataDir, "127.0.0.1:8093")
	if err != nil {
		t.Fatal(err)
	}
	defer removeOther()
	out = captureStdout(t, func() {
		if err := consoleMain(dp, []string{"-data-dir", otherDataDir}); err != nil {
			t.Fatalf("consoleMain -data-dir: %v", err)
		}
	})
	if !strings.Contains(out, "http://127.0.0.1:8093/#t="+token) {
		t.Errorf("output = %q, want the -data-dir serve's link carrying the token", out)
	}
}

// TestConsoleMainOpenLaunchesBrowser proves -open invokes the stubbed
// browser opener with the exact link just printed.
func TestConsoleMainOpenLaunchesBrowser(t *testing.T) {
	dp := newTestDeps(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "no-xdg"))

	var gotURL string
	orig := fakeHostOf(dp).browserCommandFn
	fakeHostOf(dp).browserCommandFn = func(url string) *exec.Cmd {
		gotURL = url
		return exec.Command("true")
	}
	t.Cleanup(func() { fakeHostOf(dp).browserCommandFn = orig })

	if err := consoleMain(dp, []string{"-open"}); err != nil {
		t.Fatalf("consoleMain -open: %v", err)
	}
	if gotURL == "" {
		t.Fatal("openBrowserCommand was never called")
	}
	// host.browserCommand now receives a local redirect FILE PATH, not the
	// URL itself (adversarial review, 2026-09-24) -- the whole point is
	// that the console URL never lands on argv.
	if strings.HasPrefix(gotURL, "http://") {
		t.Errorf("openBrowserCommand was called with a bare URL (%q) -- want a local redirect file path instead", gotURL)
	}
	if _, err := os.Stat(gotURL); err != nil {
		t.Errorf("openBrowserCommand argument %q is not a real file: %v", gotURL, err)
	}
}

// TestConsoleMainRejectsPositionalArgs proves an unexpected positional
// argument is refused rather than silently ignored.
func TestConsoleMainRejectsPositionalArgs(t *testing.T) {
	dp := newTestDeps(t)
	if err := consoleMain(dp, []string{"bogus"}); err == nil {
		t.Fatal("consoleMain with a positional argument: want an error, got nil")
	}
}

// TestConsoleMainOmitsTokenForRemoteConsoleURL is the regression test for
// an adversarial-review finding: when FACTORYD_CONSOLE_URL points at a
// non-loopback (or wrong-port) address, `factoryd console` must never
// attach the stable start token -- a permanent credential for every
// start-class route -- to that link.
//
// Before that fix, consoleMain appended #t=<token> to whatever
// consolelink.BaseURL("") resolved unconditionally; this test fails
// against that old behavior (the printed link would carry the real
// token) and passes only once the loopback-address guard is in place.
func TestConsoleMainOmitsTokenForRemoteConsoleURL(t *testing.T) {
	dp := newTestDeps(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	cfgDir := filepath.Join(home, "xdg", "factoryd")
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "xdg"))
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "config.yml"), []byte("data_dir: "+filepath.Join(home, "data")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	token, _, err := ensureServeStartTokenFile(dp, filepath.Join(cfgDir, serveStableStartTokenFileName))
	if err != nil {
		t.Fatalf("ensureServeStartTokenFile: %v", err)
	}
	t.Setenv("FACTORYD_CONSOLE_URL", "http://example.com:9999")

	out := captureStdout(t, func() {
		if err := consoleMain(dp, nil); err != nil {
			t.Fatalf("consoleMain: %v", err)
		}
	})
	if strings.Contains(out, token) {
		t.Fatalf("output = %q, leaked the start token to a non-loopback FACTORYD_CONSOLE_URL", out)
	}
	if !strings.Contains(out, "example.com:9999") {
		t.Errorf("output = %q, want the plain remote link printed", out)
	}
}
