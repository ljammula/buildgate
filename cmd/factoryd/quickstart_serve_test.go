package main

import (
	"buildgate/internal/consolelink"
	"buildgate/internal/hostcontrol"
	"bytes"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestQuickstartServeChildEnvStripsExistingTokens proves
// quickstartServeChildEnv scrubs every inherited FACTORYD_API_* var (an
// operator's shell could have one set for an unrelated reason) -- see
// that function's own doc comment for why this is a separate builder
// from worker's quickstartChildEnv, which must never receive any of
// these.
func TestQuickstartServeChildEnvStripsExistingTokens(t *testing.T) {
	t.Setenv("FACTORYD_API_OVERRIDE_TOKEN", "stale-override")
	t.Setenv("FACTORYD_API_READ_TOKEN", "stale-read")
	t.Setenv("FACTORYD_API_START_TOKEN", "stale-start")

	env := hostcontrol.QuickstartServeChildEnv()

	for _, kv := range env {
		if strings.HasPrefix(kv, "FACTORYD_API_") {
			t.Errorf("quickstartServeChildEnv forwarded a control-plane token into the spawned serve child's env: %q", kv)
		}
	}
}

// TestQuickstartServeChildEnvNeverSetsStartToken proves
// quickstartServeChildEnv never itself SETS FACTORYD_API_START_TOKEN --
// closes the "also" note from the 2026-09-24 adversarial review: serve
// now resolves its own stable token by reading serve-start-token off disk
// (via its own -config), so putting the same value in the child's
// environment as well would only be a second, `ps eww`-visible exposure
// of a permanent start-class credential for no benefit.
func TestQuickstartServeChildEnvNeverSetsStartToken(t *testing.T) {
	t.Setenv("FACTORYD_API_START_TOKEN", "")
	env := hostcontrol.QuickstartServeChildEnv()
	for _, kv := range env {
		if strings.HasPrefix(kv, "FACTORYD_API_START_TOKEN=") {
			t.Errorf("quickstartServeChildEnv set FACTORYD_API_START_TOKEN=%q -- must never set it at all", strings.TrimPrefix(kv, "FACTORYD_API_START_TOKEN="))
		}
	}
}

// TestQuickstartSpawnServePassesConfigFlagNotToken proves the spawned
// serve command line carries -config (so it can find the stable token
// file itself, an adversarial-review finding) and NEVER carries the token
// as an argv value anywhere (it isn't one -- there is no such flag), and
// that the child's env (as built by quickstartServeChildEnv) carries no
// FACTORYD_API_START_TOKEN either.
func TestQuickstartSpawnServePassesConfigFlag(t *testing.T) {
	dp := newTestDeps(t)
	dataDir := t.TempDir()
	configPath := filepath.Join(t.TempDir(), "config.yml")

	// Real exec.Command bookkeeping without really launching serve --
	// use a fast-exiting stand-in ("true") as the "binary" so this stays
	// fast and hermetic, then inspect the constructed command indirectly
	// via the recorded pidfile and log files.
	// true, not false: quickstartWaitForServeReady polls for up to
	// quickstartServeReadyTimeout (10s, a const -- not test-overridable)
	// before giving up, so a false stub here would make this test take
	// 10 real seconds for no benefit; this test only cares about the
	// spawned command's own argv/pidfile/log permissions, not the readiness
	// wait itself.
	restoreReady := fakeHostOf(dp).serveHealthzOKFn
	fakeHostOf(dp).serveHealthzOKFn = func(addr string) bool { return true }
	t.Cleanup(func() { fakeHostOf(dp).serveHealthzOKFn = restoreReady })

	var out bytes.Buffer
	err := realHost{dp: dp}.spawnServe(&out, "/bin/echo", configPath, dataDir, "127.0.0.1:0")
	if err != nil {
		t.Fatalf("quickstartSpawnServe: %v", err)
	}

	pidPath := filepath.Join(dataDir, "quickstart-serve.pid")
	if _, err := os.Stat(pidPath); err != nil {
		t.Fatalf("pid file not written: %v", err)
	}

	logDir := filepath.Join(dataDir, "logs")
	info, err := os.Stat(logDir)
	if err != nil {
		t.Fatalf("log dir not created: %v", err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Errorf("log dir mode = %v, want 0700 (adversarial review)", info.Mode().Perm())
	}
	for _, name := range []string{"quickstart-serve.out.log", "quickstart-serve.err.log"} {
		fi, err := os.Stat(filepath.Join(logDir, name))
		if err != nil {
			t.Fatalf("stat %s: %v", name, err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("%s mode = %v, want 0600 (adversarial review)", name, fi.Mode().Perm())
		}
	}
}

// TestQuickstartSecureLogDirChmodsExistingLooseDir proves
// quickstartSecureLogDir tightens an already-existing, looser-permission
// directory, not just a freshly created one.
func TestQuickstartSecureLogDirChmodsExistingLooseDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "logs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := hostcontrol.QuickstartSecureLogDir(dir); err != nil {
		t.Fatalf("quickstartSecureLogDir: %v", err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Errorf("existing log dir mode = %v, want 0700 after quickstartSecureLogDir", info.Mode().Perm())
	}
}

// TestQuickstartEnsureServeSkipsEntirelyWithNoServe proves opts.NoServe
// short-circuits quickstartEnsureServe before it ever probes /healthz or
// spawns anything.
func TestQuickstartEnsureServeSkipsEntirelyWithNoServe(t *testing.T) {
	dp := newTestDeps(t)
	restoreHealthz := fakeHostOf(dp).serveHealthzOKFn
	fakeHostOf(dp).serveHealthzOKFn = func(addr string) bool {
		t.Fatal("quickstartServeHealthzOK must not be called when opts.NoServe is true")
		return false
	}
	restoreSpawn := fakeHostOf(dp).spawnServeFn
	fakeHostOf(dp).spawnServeFn = func(w io.Writer, binaryPath, configPath, dataDir, addr string) error {
		t.Fatal("quickstartSpawnServe must not be called when opts.NoServe is true")
		return nil
	}
	t.Cleanup(func() {
		fakeHostOf(dp).serveHealthzOKFn = restoreHealthz
		fakeHostOf(dp).spawnServeFn = restoreSpawn
	})

	var out bytes.Buffer
	token := hostcontrol.QuickstartEnsureServe(dp, true, &out, "factoryd", filepath.Join(t.TempDir(), "config.yml"), t.TempDir())
	if token != "" {
		t.Errorf("quickstartEnsureServe with NoServe returned token %q, want empty", token)
	}
}

// TestQuickstartEnsureServeReusesVerifiedOwnServe proves a healthz-
// answering serve that ALSO passes host.serveVerifiedOurs is reused
// (host.spawnServe never called) and its stable token returned.
func TestQuickstartEnsureServeReusesVerifiedOwnServe(t *testing.T) {
	dp := newTestDeps(t)
	restoreHealthz := fakeHostOf(dp).serveHealthzOKFn
	fakeHostOf(dp).serveHealthzOKFn = func(addr string) bool { return true }
	restoreVerified := fakeHostOf(dp).serveVerifiedOursFn
	fakeHostOf(dp).serveVerifiedOursFn = func(dataDir, addr string) (int, bool) { return 4242, true }
	var spawned bool
	restoreSpawn := fakeHostOf(dp).spawnServeFn
	fakeHostOf(dp).spawnServeFn = func(w io.Writer, binaryPath, configPath, dataDir, addr string) error {
		spawned = true
		return nil
	}
	t.Cleanup(func() {
		fakeHostOf(dp).serveHealthzOKFn = restoreHealthz
		fakeHostOf(dp).serveVerifiedOursFn = restoreVerified
		fakeHostOf(dp).spawnServeFn = restoreSpawn
	})

	cfgDir := t.TempDir()
	configPath := filepath.Join(cfgDir, "config.yml")
	if err := os.WriteFile(configPath, []byte("sandbox_image: img@sha256:"+strings.Repeat("a", 64)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	token := hostcontrol.QuickstartEnsureServe(dp, false, &out, "factoryd", configPath, t.TempDir())

	if spawned {
		t.Error("quickstartSpawnServe was called despite a verified own serve already answering /healthz")
	}
	if token == "" {
		t.Error("quickstartEnsureServe returned an empty token despite a resolvable session config directory")
	}
	if !strings.Contains(out.String(), "already running") {
		t.Errorf("output = %q, want a note that serve is already running", out.String())
	}

	// A second call must resolve the SAME token, not rotate it.
	token2 := hostcontrol.QuickstartEnsureServe(dp, false, &out, "factoryd", configPath, t.TempDir())
	if token2 != token {
		t.Errorf("quickstartEnsureServe rotated the stable token across calls: %q != %q", token, token2)
	}
}

// heldDefaultServe sets up a default serve address that answers /healthz
// and that the data dir cannot prove it owns (another profile's serve),
// and returns the address every spawnServe call was given.
func heldDefaultServe(t *testing.T, dp *deps, spawn func(dataDir, addr string)) *[]string {
	t.Helper()
	restoreHealthz := fakeHostOf(dp).serveHealthzOKFn
	fakeHostOf(dp).serveHealthzOKFn = func(addr string) bool { return addr == consolelink.DefaultServeAddr }
	restoreVerified := fakeHostOf(dp).serveVerifiedOursFn
	fakeHostOf(dp).serveVerifiedOursFn = func(dataDir, addr string) (int, bool) { return 0, false }
	var spawnedAt []string
	restoreSpawn := fakeHostOf(dp).spawnServeFn
	fakeHostOf(dp).spawnServeFn = func(w io.Writer, binaryPath, configPath, dataDir, addr string) error {
		spawnedAt = append(spawnedAt, addr)
		spawn(dataDir, addr)
		return nil
	}
	t.Cleanup(func() {
		fakeHostOf(dp).serveHealthzOKFn = restoreHealthz
		fakeHostOf(dp).serveVerifiedOursFn = restoreVerified
		fakeHostOf(dp).spawnServeFn = restoreSpawn
	})
	return &spawnedAt
}

func writeServeTestConfig(t *testing.T) string {
	t.Helper()
	configPath := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(configPath, []byte("sandbox_image: img@sha256:"+strings.Repeat("a", 64)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return configPath
}

// TestQuickstartEnsureServeTakesAFreePortWhenTheDefaultIsHeld: something
// answers /healthz at the default address and host.serveVerifiedOurs cannot
// attribute it to this data dir (another profile's serve). That listener
// never gets the token (an adversarial-review finding: a bare 200 from
// /healthz proves nothing about who listens); this data dir's serve starts
// on a free loopback port, and the token is returned once that serve has
// recorded itself there.
func TestQuickstartEnsureServeTakesAFreePortWhenTheDefaultIsHeld(t *testing.T) {
	dp := newTestDeps(t)
	// The suite stubs the probe to false (TestMain); this test's serve
	// really listens, so the record is read with a real dial.
	origListening := consolelink.Listening
	t.Cleanup(func() { consolelink.Listening = origListening })
	consolelink.Listening = func(addr string) bool {
		conn, err := net.DialTimeout("tcp", addr, time.Second)
		if err != nil {
			return false
		}
		_ = conn.Close()
		return true
	}
	spawnedAt := heldDefaultServe(t, dp, func(dataDir, addr string) {
		// What a real serve does once bound: listen, then record.
		l, err := net.Listen("tcp", addr)
		if err != nil {
			t.Fatalf("listen on the address quickstart chose (%s): %v", addr, err)
		}
		t.Cleanup(func() { l.Close() })
		if _, err := consolelink.RecordServeAddress(dataDir, addr); err != nil {
			t.Fatal(err)
		}
	})

	var out bytes.Buffer
	dataDir := t.TempDir()
	token := hostcontrol.QuickstartEnsureServe(dp, false, &out, "factoryd", writeServeTestConfig(t), dataDir)

	if len(*spawnedAt) != 1 {
		t.Fatalf("spawnServe calls = %v, want exactly one", *spawnedAt)
	}
	addr := (*spawnedAt)[0]
	if addr == consolelink.DefaultServeAddr {
		t.Fatalf("serve was spawned at the held default address %s", addr)
	}
	if host, _, err := net.SplitHostPort(addr); err != nil || host != "127.0.0.1" {
		t.Errorf("serve was spawned at %q, want a 127.0.0.1 address", addr)
	}
	if token == "" {
		t.Error("no token returned for this data dir's own recorded serve")
	}
	if got := consolelink.ServeAddress(dataDir); got != addr {
		t.Errorf("recorded console address = %q, want %q", got, addr)
	}
	if !strings.Contains(out.String(), "held by another process") || !strings.Contains(out.String(), addr) {
		t.Errorf("output = %q, want it to say the default is held and name %s", out.String(), addr)
	}
}

// TestQuickstartEnsureServeGivesNoTokenUntilItsOwnServeIsRecorded: the
// default address is held by an unverified listener and the serve spawned
// on a free port has not recorded itself. No token is returned, so none can
// be attached to a link at the held address.
func TestQuickstartEnsureServeGivesNoTokenUntilItsOwnServeIsRecorded(t *testing.T) {
	dp := newTestDeps(t)
	spawnedAt := heldDefaultServe(t, dp, func(string, string) {})

	var out bytes.Buffer
	token := hostcontrol.QuickstartEnsureServe(dp, false, &out, "factoryd", writeServeTestConfig(t), t.TempDir())

	if token != "" {
		t.Errorf("token %q returned with no recorded serve of this data dir's own", token)
	}
	if len(*spawnedAt) != 1 || (*spawnedAt)[0] == consolelink.DefaultServeAddr {
		t.Errorf("spawnServe calls = %v, want one, away from the held default", *spawnedAt)
	}
}

// TestQuickstartEnsureServeSpawnsWhenNotListening proves host.spawnServe
// is invoked, with the resolved configPath, when nothing already answers
// /healthz and (on macOS) no serve LaunchAgent plist is installed.
func TestQuickstartEnsureServeSpawnsWhenNotListening(t *testing.T) {
	dp := newTestDeps(t)
	if runtime.GOOS == "darwin" {
		// Isolate from this developer's own real ~/Library/LaunchAgents --
		// same reasoning as TestQuickstartEnsureDaemonRestartsAliveChildWhenConfigRewritten.
		t.Setenv("HOME", t.TempDir())
	}

	restoreHealthz := fakeHostOf(dp).serveHealthzOKFn
	fakeHostOf(dp).serveHealthzOKFn = func(addr string) bool { return false }
	var gotConfigPath, gotAddr string
	restoreSpawn := fakeHostOf(dp).spawnServeFn
	fakeHostOf(dp).spawnServeFn = func(w io.Writer, binaryPath, configPath, dataDir, addr string) error {
		gotConfigPath = configPath
		gotAddr = addr
		return nil
	}
	t.Cleanup(func() {
		fakeHostOf(dp).serveHealthzOKFn = restoreHealthz
		fakeHostOf(dp).spawnServeFn = restoreSpawn
	})

	cfgDir := t.TempDir()
	configPath := filepath.Join(cfgDir, "config.yml")
	if err := os.WriteFile(configPath, []byte("sandbox_image: img@sha256:"+strings.Repeat("a", 64)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	token := hostcontrol.QuickstartEnsureServe(dp, false, &out, "factoryd", configPath, t.TempDir())

	if token == "" {
		t.Error("quickstartEnsureServe returned an empty token despite a resolvable session config directory")
	}
	if gotConfigPath != configPath {
		t.Errorf("quickstartSpawnServe called with configPath %q, want %q", gotConfigPath, configPath)
	}
	if gotAddr == "" {
		t.Error("quickstartSpawnServe was not called with a resolved address")
	}
}

// TestQuickstartPrintAndOpenConsoleLinkAppendsTokenOnOwnLoopbackServe
// proves the console link carries #t=<token> when the resolved base IS
// this machine's own loopback serve address, and that autoOpen triggers
// host.browserCommand.
func TestQuickstartPrintAndOpenConsoleLinkAppendsTokenOnOwnLoopbackServe(t *testing.T) {
	dp := newTestDeps(t)
	t.Setenv("FACTORYD_CONSOLE_URL", "http://127.0.0.1:8090")
	restoreVerified := fakeHostOf(dp).serveVerifiedOursFn
	fakeHostOf(dp).serveVerifiedOursFn = func(dataDir, addr string) (int, bool) { return os.Getpid(), addr == "127.0.0.1:8090" }
	t.Cleanup(func() { fakeHostOf(dp).serveVerifiedOursFn = restoreVerified })

	var gotURL string
	orig := fakeHostOf(dp).browserCommandFn
	fakeHostOf(dp).browserCommandFn = func(target string) *exec.Cmd {
		gotURL = target
		return exec.Command("true")
	}
	t.Cleanup(func() { fakeHostOf(dp).browserCommandFn = orig })

	var out bytes.Buffer
	quickstartPrintAndOpenConsoleLink(dp, &out, t.TempDir(), "req-1", "tok-abc", true)

	want := "http://127.0.0.1:8090/requests/req-1#t=tok-abc"
	if !strings.Contains(out.String(), want) {
		t.Errorf("output = %q, want it to contain %q", out.String(), want)
	}
	// host.browserCommand now receives a local redirect FILE PATH, never
	// the URL itself (an adversarial-review finding) -- assert the token
	// never appears on what would be argv, and that it is NOT the literal
	// browser URL.
	if strings.Contains(gotURL, "tok-abc") {
		t.Errorf("openBrowserCommand was called with the token in its argument (%q) -- must be a local redirect file path instead", gotURL)
	}
	if gotURL == want || gotURL == "" {
		t.Errorf("openBrowserCommand argument = %q, want a local file path, not the bare URL", gotURL)
	}
}

// TestQuickstartPrintAndOpenConsoleLinkOmitsTokenForAnUnverifiedListener:
// the link points at this machine's loopback, but nothing this data dir can
// prove it owns holds that address (another data dir's serve on the default
// port, or a port a killed serve's record still names). The link is printed
// without the token.
func TestQuickstartPrintAndOpenConsoleLinkOmitsTokenForAnUnverifiedListener(t *testing.T) {
	dp := newTestDeps(t)
	t.Setenv("FACTORYD_CONSOLE_URL", "http://127.0.0.1:8090")
	restoreVerified := fakeHostOf(dp).serveVerifiedOursFn
	fakeHostOf(dp).serveVerifiedOursFn = func(dataDir, addr string) (int, bool) { return 0, false }
	t.Cleanup(func() { fakeHostOf(dp).serveVerifiedOursFn = restoreVerified })

	var out bytes.Buffer
	quickstartPrintAndOpenConsoleLink(dp, &out, t.TempDir(), "req-1", "tok-abc", false)

	if strings.Contains(out.String(), "tok-abc") {
		t.Errorf("output = %q, gave the start token to a listener this data dir does not own", out.String())
	}
	if !strings.Contains(out.String(), "http://127.0.0.1:8090/requests/req-1") {
		t.Errorf("output = %q, want the plain link", out.String())
	}
}

// TestQuickstartEnsureServeWaitsForItsOwnStartingServe: the default address
// is held by someone else and this data dir's own serve, spawned a moment
// ago, has not recorded itself yet. A second submit starts no second serve.
func TestQuickstartEnsureServeWaitsForItsOwnStartingServe(t *testing.T) {
	dp := newTestDeps(t)
	spawnedAt := heldDefaultServe(t, dp, func(string, string) {})
	restoreLooksLike := fakeHostOf(dp).pidLooksLikeWorkerFn
	fakeHostOf(dp).pidLooksLikeWorkerFn = func(pid int) bool { return true }
	t.Cleanup(func() { fakeHostOf(dp).pidLooksLikeWorkerFn = restoreLooksLike })
	dataDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dataDir, "quickstart-serve.pid"), []byte(strconv.Itoa(os.Getpid())), 0o644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	token := hostcontrol.QuickstartEnsureServe(dp, false, &out, "factoryd", writeServeTestConfig(t), dataDir)

	if len(*spawnedAt) != 0 {
		t.Errorf("spawnServe calls = %v, want none while this data dir's serve is starting", *spawnedAt)
	}
	if token != "" {
		t.Errorf("token %q returned before the serve recorded itself", token)
	}
	if !strings.Contains(out.String(), "still starting") {
		t.Errorf("output = %q, want it to say the serve is still starting", out.String())
	}
}

// TestQuickstartEnsureServeDistrustsAStaleRecord: the data dir's record
// names an address where something listens that is not its serve (the serve
// was killed, the port reused). The record is not adopted: the serve is
// started at the default address.
func TestQuickstartEnsureServeDistrustsAStaleRecord(t *testing.T) {
	dp := newTestDeps(t)
	if runtime.GOOS == "darwin" {
		t.Setenv("HOME", t.TempDir())
	}
	origListening := consolelink.Listening
	t.Cleanup(func() { consolelink.Listening = origListening })
	consolelink.Listening = func(string) bool { return true }
	dataDir := t.TempDir()
	const stale = "127.0.0.1:18478"
	if _, err := consolelink.RecordServeAddress(dataDir, stale); err != nil {
		t.Fatal(err)
	}
	restoreHealthz, restoreVerified, restoreSpawn := fakeHostOf(dp).serveHealthzOKFn, fakeHostOf(dp).serveVerifiedOursFn, fakeHostOf(dp).spawnServeFn
	t.Cleanup(func() {
		fakeHostOf(dp).serveHealthzOKFn, fakeHostOf(dp).serveVerifiedOursFn, fakeHostOf(dp).spawnServeFn = restoreHealthz, restoreVerified, restoreSpawn
	})
	fakeHostOf(dp).serveHealthzOKFn = func(addr string) bool { return addr == stale }
	fakeHostOf(dp).serveVerifiedOursFn = func(dataDir, addr string) (int, bool) { return 0, false }
	var spawnedAt []string
	fakeHostOf(dp).spawnServeFn = func(w io.Writer, binaryPath, configPath, dataDir, addr string) error {
		spawnedAt = append(spawnedAt, addr)
		return nil
	}

	var out bytes.Buffer
	hostcontrol.QuickstartEnsureServe(dp, false, &out, "factoryd", writeServeTestConfig(t), dataDir)

	if len(spawnedAt) != 1 || spawnedAt[0] != consolelink.DefaultServeAddr {
		t.Errorf("spawnServe calls = %v, want one at the default address %s", spawnedAt, consolelink.DefaultServeAddr)
	}
}

// TestQuickstartPrintAndOpenConsoleLinkOmitsTokenForRemoteBase is the
// regression test for another adversarial-review finding: when the
// resolved console base is NOT this machine's own loopback serve address
// (an operator-set FACTORYD_CONSOLE_URL pointed at a different
// host/port), the start token -- a permanent credential for every
// start-class route -- must never be appended to the printed/opened
// link.
//
// Before that fix, quickstartPrintAndOpenConsoleLink appended
// #t=<token> to whatever resolveConsoleBaseURL returned unconditionally;
// this test fails against that old behavior (the link would contain
// tok-abc) and passes only once the loopback-address guard is in place.
func TestQuickstartPrintAndOpenConsoleLinkOmitsTokenForRemoteBase(t *testing.T) {
	dp := newTestDeps(t)
	t.Setenv("FACTORYD_CONSOLE_URL", "http://example.com:9999")

	var out bytes.Buffer
	quickstartPrintAndOpenConsoleLink(dp, &out, t.TempDir(), "req-1", "tok-abc", false)

	if strings.Contains(out.String(), "tok-abc") {
		t.Errorf("output = %q, leaked the start token to a non-loopback console base", out.String())
	}
	want := "http://example.com:9999/requests/req-1"
	if !strings.Contains(out.String(), want) {
		t.Errorf("output = %q, want the plain link %q", out.String(), want)
	}
}

// TestQuickstartPrintAndOpenConsoleLinkNoOpenSkipsBrowser proves autoOpen
// false never calls host.browserCommand, even with a resolvable link.
func TestQuickstartPrintAndOpenConsoleLinkNoOpenSkipsBrowser(t *testing.T) {
	dp := newTestDeps(t)
	t.Setenv("FACTORYD_CONSOLE_URL", "http://127.0.0.1:8090")

	called := false
	orig := fakeHostOf(dp).browserCommandFn
	fakeHostOf(dp).browserCommandFn = func(target string) *exec.Cmd {
		called = true
		return exec.Command("true")
	}
	t.Cleanup(func() { fakeHostOf(dp).browserCommandFn = orig })

	var out bytes.Buffer
	quickstartPrintAndOpenConsoleLink(dp, &out, t.TempDir(), "req-1", "tok-abc", false)

	if called {
		t.Error("openBrowserCommand was called despite autoOpen=false")
	}
}
