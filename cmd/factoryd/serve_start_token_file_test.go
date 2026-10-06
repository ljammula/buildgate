package main

import (
	"bytes"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"buildgate/internal/consoleweb"
)

// TestEnsureServeStartTokenFileGeneratesOnceAndReusesAfter proves
// install-service's own stable-token bootstrap never rotates a token an
// operator's browser may already have stored (generateServeStartTokenFile's
// own doc comment) -- a second call against the same path must return the
// identical token, not mint a fresh one.
func TestEnsureServeStartTokenFileGeneratesOnceAndReusesAfter(t *testing.T) {
	dp := newTestDeps(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "serve-start-token")

	first, generated, err := ensureServeStartTokenFile(dp, path)
	if err != nil {
		t.Fatalf("ensureServeStartTokenFile: %v", err)
	}
	if !generated {
		t.Fatalf("first ensureServeStartTokenFile call reported generated=false")
	}
	if first == "" {
		t.Fatalf("ensureServeStartTokenFile returned an empty token")
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("serve-start-token mode = %v, want 0600", info.Mode().Perm())
	}

	second, generated, err := ensureServeStartTokenFile(dp, path)
	if err != nil {
		t.Fatalf("ensureServeStartTokenFile (second call): %v", err)
	}
	if generated {
		t.Fatalf("second ensureServeStartTokenFile call reported generated=true, want reuse of the existing file")
	}
	if second != first {
		t.Fatalf("ensureServeStartTokenFile rotated the token across calls: %q != %q", first, second)
	}
}

// TestReadServeStartTokenFileMissingOrEmpty proves a missing or
// whitespace-only stable token file reports ("", false), the same
// fallback-to-next-choice contract generateServeStartTokenFile's own doc
// comment describes -- never an error a caller would need to handle
// specially.
func TestReadServeStartTokenFileMissingOrEmpty(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "absent")
	if _, ok := readServeStartTokenFile(missing); ok {
		t.Fatalf("readServeStartTokenFile(%s) reported ok=true for a nonexistent file", missing)
	}

	empty := filepath.Join(dir, "empty")
	if err := os.WriteFile(empty, []byte("\n\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := readServeStartTokenFile(empty); ok {
		t.Fatalf("readServeStartTokenFile(%s) reported ok=true for a whitespace-only file", empty)
	}
}

// TestInspectServeStartTokenFileRefusesSymlink is the regression test for
// an adversarial-review finding: a symlink at the token path must never be
// followed/trusted, even if it points at a perfectly valid 0600 file.
// Before that fix (a plain os.ReadFile with no Lstat check),
// this reported the symlink's target content as a usable token; this test
// fails against that old behavior and passes only once the symlink
// refusal is in place.
func TestInspectServeStartTokenFileRefusesSymlink(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real-token")
	if err := os.WriteFile(real, []byte("real-token-value\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "serve-start-token")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks not supported in this environment: %v", err)
	}

	token, issue, err := inspectServeStartTokenFile(link)
	if err != nil {
		t.Fatalf("inspectServeStartTokenFile: %v", err)
	}
	if token != "" {
		t.Fatalf("inspectServeStartTokenFile followed a symlink and returned its content as a token: %q", token)
	}
	if issue == nil {
		t.Fatal("want a non-nil issue for a symlinked token path")
	}
	if !strings.Contains(issue.Reason, "symlink") {
		t.Errorf("issue.Reason = %q, want it to mention symlink", issue.Reason)
	}

	if _, ok := readServeStartTokenFile(link); ok {
		t.Fatal("readServeStartTokenFile treated a symlinked path as usable")
	}
}

// TestInspectServeStartTokenFileRefusesLoosePermissions is the regression
// test for that same adversarial-review finding: a token file readable
// beyond its owner (mode&0o077 != 0) must be refused with the exact
// chmod fix named, not silently trusted. Fails against the pre-fix
// plain os.ReadFile (which never checked mode at all) and passes only
// with the Lstat-based permission check in place.
func TestInspectServeStartTokenFileRefusesLoosePermissions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "serve-start-token")
	if err := os.WriteFile(path, []byte("some-token\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	token, issue, err := inspectServeStartTokenFile(path)
	if err != nil {
		t.Fatalf("inspectServeStartTokenFile: %v", err)
	}
	if token != "" {
		t.Fatalf("inspectServeStartTokenFile trusted a group/other-readable file: token=%q", token)
	}
	if issue == nil {
		t.Fatal("want a non-nil issue for a 0644 token file")
	}
	wantFix := "chmod 600 " + path
	if issue.Fix != wantFix {
		t.Errorf("issue.Fix = %q, want %q", issue.Fix, wantFix)
	}
}

// TestInspectServeStartTokenFileAbsentIsNotAnIssue proves a path that
// simply doesn't exist reports (\"\", nil, nil) -- the ordinary "generate
// a fresh one" case, not something to warn about.
func TestInspectServeStartTokenFileAbsentIsNotAnIssue(t *testing.T) {
	path := filepath.Join(t.TempDir(), "absent")
	token, issue, err := inspectServeStartTokenFile(path)
	if token != "" || issue != nil || err != nil {
		t.Fatalf("inspectServeStartTokenFile(absent) = (%q, %v, %v), want (\"\", nil, nil)", token, issue, err)
	}
}

// TestGenerateServeStartTokenFileUsesExclusiveCreate proves
// generateServeStartTokenFile refuses to overwrite a file that already
// exists at path (O_EXCL) -- a second call against an already-populated
// path must fail rather than silently clobber it.
func TestGenerateServeStartTokenFileUsesExclusiveCreate(t *testing.T) {
	dp := newTestDeps(t)
	path := filepath.Join(t.TempDir(), "serve-start-token")
	if _, err := generateServeStartTokenFile(dp, path); err != nil {
		t.Fatalf("first generateServeStartTokenFile: %v", err)
	}
	if _, err := generateServeStartTokenFile(dp, path); err == nil {
		t.Fatal("second generateServeStartTokenFile against an existing path succeeded, want an error (O_EXCL)")
	}
}

// TestGenerateServeStartTokenFileRefusesGitWorkTree is the regression
// test for an adversarial-review finding's git-worktree refusal: writing
// the token inside a git working tree risks it being committed/pushed.
// Stubs forge.insideGitWorkTree rather than depending on a real git
// binary/repository.
func TestGenerateServeStartTokenFileRefusesGitWorkTree(t *testing.T) {
	dp := newTestDeps(t)
	orig := fakeForgeOf(dp).insideGitWorkTreeFn
	fakeForgeOf(dp).insideGitWorkTreeFn = func(dir string) bool { return true }
	t.Cleanup(func() { fakeForgeOf(dp).insideGitWorkTreeFn = orig })

	path := filepath.Join(t.TempDir(), "repo", "serve-start-token")
	if _, err := generateServeStartTokenFile(dp, path); err == nil {
		t.Fatal("generateServeStartTokenFile inside a (stubbed) git working tree succeeded, want a refusal")
	} else if !strings.Contains(err.Error(), "git working tree") {
		t.Errorf("error = %v, want it to mention a git working tree", err)
	}
	if _, err := os.Stat(path); err == nil {
		t.Fatal("token file was written despite the git-worktree refusal")
	}
}

// TestResolveEffectiveConfigPathPrefersExplicit proves
// resolveEffectiveConfigPath (from that same adversarial-review finding)
// honors an explicit -config value over the default-path search, made
// absolute.
func TestResolveEffectiveConfigPathPrefersExplicit(t *testing.T) {
	dir := t.TempDir()
	explicit := filepath.Join(dir, "custom-config.yml")
	if err := os.WriteFile(explicit, []byte("sandbox_image: img\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	path, ok := resolveEffectiveConfigPath(explicit)
	if !ok {
		t.Fatal("resolveEffectiveConfigPath reported ok=false for an explicit path")
	}
	if path != explicit {
		t.Fatalf("resolveEffectiveConfigPath(%q) = %q, want %q", explicit, path, explicit)
	}
}

// TestResolveEffectiveConfigPathFallsBackToDefaultSearch proves an empty
// explicit value falls back to sessionconfig.DefaultPaths()' own search,
// reporting ok=false when nothing exists there.
func TestResolveEffectiveConfigPathFallsBackToDefaultSearch(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "no-config-here"))

	if _, ok := resolveEffectiveConfigPath(""); ok {
		t.Fatalf("resolveEffectiveConfigPath(\"\") reported ok=true with no session config present")
	}

	cfgDir := filepath.Join(dir, "cfg")
	if err := os.MkdirAll(filepath.Join(cfgDir, "factoryd"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(cfgDir, "factoryd", "config.yml")
	if err := os.WriteFile(cfgPath, []byte("data_dir: "+filepath.Join(dir, "data")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CONFIG_HOME", cfgDir)

	path, ok := resolveEffectiveConfigPath("")
	if !ok {
		t.Fatalf("resolveEffectiveConfigPath(\"\") reported ok=false with a session config present at %s", cfgPath)
	}
	if path != cfgPath {
		t.Fatalf("resolveEffectiveConfigPath(\"\") = %q, want %q", path, cfgPath)
	}
	want := filepath.Join(cfgDir, "factoryd", serveStableStartTokenFileName)
	if got := serveStableStartTokenPathFor(path); got != want {
		t.Fatalf("serveStableStartTokenPathFor(%q) = %q, want %q", path, got, want)
	}
}

// TestServeConsoleFlagAndInstallServiceAgreeOnTokenPath is the regression
// test for that same adversarial-review finding's core claim: `serve
// -config X` and `factoryd console -config X` (and install-service's own
// resolved configPath) must derive the exact same stable-token-file path
// for the same X, whether or not X is one of sessionconfig.DefaultPaths().
func TestServeConsoleFlagAndInstallServiceAgreeOnTokenPath(t *testing.T) {
	// A NON-default path (outside any XDG_CONFIG_HOME/~/.factory
	// location) -- the exact case that finding called out: install-service's
	// own -config could name a path serve's fixed default-path search
	// would never independently rediscover.
	explicit := filepath.Join(t.TempDir(), "somewhere-else", "config.yml")

	fromServeFlag, ok1 := resolveEffectiveConfigPath(explicit)
	fromConsoleFlag, ok2 := resolveEffectiveConfigPath(explicit)
	if !ok1 || !ok2 {
		t.Fatalf("resolveEffectiveConfigPath ok = (%v, %v), want (true, true)", ok1, ok2)
	}
	if fromServeFlag != fromConsoleFlag {
		t.Fatalf("serve's and console's own resolved config paths disagree: %q != %q", fromServeFlag, fromConsoleFlag)
	}
	if serveStableStartTokenPathFor(fromServeFlag) != serveStableStartTokenPathFor(fromConsoleFlag) {
		t.Fatal("serve's and console's own derived token-file paths disagree")
	}
}

// TestServeUsesStableStartTokenFileWhenPresent proves serveMain prefers an
// on-disk stable start token over generating a fresh per-process one:
// a launchd-restarted serve LaunchAgent must keep using the same token
// an operator's browser already stored. Also the regression test for the
// finding that the token's own VALUE must never appear in serveMain's
// own log output when it came from the stable file -- only the
// ephemeral, per-process case may print the fragment.
func TestServeUsesStableStartTokenFileWhenPresent(t *testing.T) {
	dp := newTestDeps(t)
	// Deliberately does NOT call the shared isolateSessionConfig helper
	// (worker_config_test.go) / serveMainLogsBeforeFailingToBind (serve_start_
	// token_test.go): both mint a FRESH isolated HOME/XDG_CONFIG_HOME via
	// their own t.TempDir(), so calling one after the other here would
	// silently discard whichever config file this test wrote first. This
	// test sets up its own isolated HOME/XDG_CONFIG_HOME once and drives
	// serveMain directly instead.
	home := t.TempDir()
	t.Setenv("HOME", home)
	cfgDir := filepath.Join(home, "xdg", "factoryd")
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "xdg"))
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	dataDir := filepath.Join(home, "data")
	if err := os.WriteFile(filepath.Join(cfgDir, "config.yml"), []byte("data_dir: "+dataDir+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	stableToken, _, err := ensureServeStartTokenFile(dp, filepath.Join(cfgDir, serveStableStartTokenFileName))
	if err != nil {
		t.Fatalf("ensureServeStartTokenFile: %v", err)
	}

	t.Setenv(overrideTokenEnvironmentVariable, "")
	t.Setenv(startTokenEnvironmentVariable, "")
	t.Setenv(readTokenEnvironmentVariable, "")

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

	if err := serveMain(dp, []string{"-data-dir", dataDir, "-addr", listener.Addr().String()}); err == nil {
		t.Fatal("serveMain unexpectedly succeeded (it should have failed to bind the already-occupied address)")
	}
	logs := logBuf.String()

	if strings.Contains(logs, "generated a per-process token") {
		t.Fatalf("serve log claims a token was generated despite a stable start token file being present:\n%s", logs)
	}
	if !strings.Contains(logs, "stable "+serveStableStartTokenFileName+" file") {
		t.Fatalf("serve log = %q, want a line naming the stable token file", logs)
	}
	if strings.Contains(logs, stableToken) {
		t.Fatalf("serve log leaked the stable token's own value (an adversarial-review finding):\n%s", logs)
	}
	// serveMain prints the `factoryd console` pointer only when the console
	// bundle is embedded (it logs "console: not embedded in this binary"
	// otherwise). Asserting it unconditionally made this test pass only in a
	// checkout where `make console-build` had been run locally -- it failed
	// 3/3 in every fresh worktree (found 2026-09-25, onboarding-walk Phase E).
	if consoleweb.Embedded() && !strings.Contains(logs, "factoryd console") {
		t.Fatalf("serve log = %q, want it to point at `factoryd console` for the signed-in link", logs)
	}
}
