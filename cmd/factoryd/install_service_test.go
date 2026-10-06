package main

import (
	"buildgate/internal/hostcontrol"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

func TestBuildServicePlistDeterministicAndEscapesXML(t *testing.T) {
	cfg := workerPlistConfig{
		BinaryPath:      "/usr/local/bin/factoryd",
		ConfigPath:      "/Users/op/.config/factoryd/config.yml",
		DataDir:         "/Users/op/data & more",
		HomeDir:         "/Users/op",
		TemporalAddress: "localhost:7233",
	}

	first := buildWorkerPlist(cfg)
	second := buildWorkerPlist(cfg)
	if string(first) != string(second) {
		t.Fatalf("buildWorkerPlist is not deterministic:\n%s\n---\n%s", first, second)
	}

	s := string(first)
	if !strings.Contains(s, "/usr/local/bin/factoryd") {
		t.Errorf("plist missing binary path:\n%s", s)
	}
	if !strings.Contains(s, "/Users/op/.config/factoryd/config.yml") {
		t.Errorf("plist missing config path:\n%s", s)
	}
	if !strings.Contains(s, "<string>dev.factoryd.worker</string>") {
		t.Errorf("plist label is not dev.factoryd.worker:\n%s", s)
	}
	wantArgs := []string{"/usr/local/bin/factoryd", "worker", "-config", "/Users/op/.config/factoryd/config.yml", "-data-dir", "/Users/op/data & more", "-temporal-address", "localhost:7233"}
	if got := hostcontrol.ProgramArgumentsStrings(first); !reflect.DeepEqual(got, wantArgs) {
		t.Errorf("ProgramArguments = %q, want %q", got, wantArgs)
	}
	if !strings.Contains(s, "data &amp; more") {
		t.Errorf("plist did not XML-escape '&' in the data dir:\n%s", s)
	}
	if strings.Contains(s, "data & more") {
		t.Errorf("plist contains an unescaped '&':\n%s", s)
	}
	if !strings.Contains(s, "data &amp; more/logs/queue-run.out.log") {
		t.Errorf("plist missing stdout log path:\n%s", s)
	}
	if !strings.Contains(s, "data &amp; more/logs/queue-run.err.log") {
		t.Errorf("plist missing stderr log path:\n%s", s)
	}
	if !strings.Contains(s, "<key>ThrottleInterval</key>\n\t<integer>60</integer>") {
		t.Errorf("plist lacks a ThrottleInterval (crash-loop guard while Docker is not yet up):\n%s", s)
	}
	if !strings.Contains(s, "<key>KeepAlive</key>\n\t<true/>") {
		t.Errorf("plist missing KeepAlive true:\n%s", s)
	}
	if !strings.Contains(s, "<key>RunAtLoad</key>\n\t<true/>") {
		t.Errorf("plist missing RunAtLoad true:\n%s", s)
	}
	if !strings.Contains(s, "<key>PATH</key>") || !strings.Contains(s, servicePATH) {
		t.Errorf("plist missing PATH environment variable:\n%s", s)
	}
	if !strings.Contains(s, "<key>HOME</key>\n\t\t<string>/Users/op</string>") {
		t.Errorf("plist missing HOME environment variable:\n%s", s)
	}
}

// TestBuildServicePlistValidatesWithPlutil is the "Done when" requirement
// that generated plists pass `plutil -lint`; skipped when plutil is not
// on PATH (non-macOS).
// stubEnsureTemporal makes install-service's Temporal resolution answer addr
// ("" means none available) for the test.
func stubEnsureTemporal(dp *deps, t *testing.T, addr string) {
	t.Helper()
	old := fakeTemporalOf(dp).ensureFn
	fakeTemporalOf(dp).ensureFn = func(context.Context, io.Writer) string { return addr }
	t.Cleanup(func() { fakeTemporalOf(dp).ensureFn = old })
}

func TestInstallServiceRefusesWithoutTemporal(t *testing.T) {
	dp := newTestDeps(t)
	stubEnsureTemporal(dp, t, "")
	t.Setenv("HOME", t.TempDir())
	dataDir := filepath.Join(t.TempDir(), "data")
	configPath := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(configPath, []byte("sandbox_image: img@sha256:"+strings.Repeat("a", 64)+"\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	launchctlCalled := false
	origRun := fakeHostOf(dp).launchctlFn
	fakeHostOf(dp).launchctlFn = func(args ...string) ([]byte, error) { launchctlCalled = true; return nil, nil }
	t.Cleanup(func() { fakeHostOf(dp).launchctlFn = origRun })

	err := installServiceMain(dp, []string{"-config", configPath, "-data-dir", dataDir})
	if err == nil || !strings.Contains(err.Error(), "worker needs Temporal") {
		t.Fatalf("err = %v, want one line saying the worker needs Temporal", err)
	}
	if strings.Contains(err.Error(), "\n") {
		t.Errorf("refusal is more than one line: %q", err)
	}
	plistPath, _ := hostcontrol.WorkerPlistPath()
	if _, statErr := os.Stat(plistPath); statErr == nil {
		t.Error("a plist was written despite the refusal")
	}
	if launchctlCalled {
		t.Error("launchctl was called despite the refusal")
	}
}

func TestBuildServicePlistValidatesWithPlutil(t *testing.T) {
	plutil, err := exec.LookPath("plutil")
	if err != nil {
		t.Skip("plutil not on PATH")
	}

	plist := buildWorkerPlist(workerPlistConfig{
		BinaryPath: "/usr/local/bin/factoryd",
		ConfigPath: "/Users/op/.config/factoryd/config.yml",
		DataDir:    "/Users/op/data",
		HomeDir:    "/Users/op",
	})

	cmd := exec.Command(plutil, "-lint", "-")
	cmd.Stdin = strings.NewReader(string(plist))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("plutil -lint rejected the generated plist: %v\n%s\n---\n%s", err, out, plist)
	}
}

func TestInstallServicePrintContainsResolvedPaths(t *testing.T) {
	dp := newTestDeps(t)
	stubEnsureTemporal(dp, t, "localhost:7233")
	dataDir := filepath.Join(t.TempDir(), "data & scratch")
	configPath := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(configPath, []byte("sandbox_image: img@sha256:"+strings.Repeat("a", 64)+"\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	wantBinary, err := resolveServiceBinaryPath()
	if err != nil {
		t.Fatalf("resolveServiceBinaryPath: %v", err)
	}
	wantConfigPath, err := filepath.Abs(configPath)
	if err != nil {
		t.Fatalf("filepath.Abs(configPath): %v", err)
	}
	wantDataDir, err := filepath.Abs(dataDir)
	if err != nil {
		t.Fatalf("filepath.Abs(dataDir): %v", err)
	}

	out := captureStdout(t, func() {
		if err := installServiceMain(dp, []string{"-config", configPath, "-data-dir", dataDir, "-print"}); err != nil {
			t.Fatalf("installServiceMain -print: %v", err)
		}
	})

	if !strings.Contains(out, wantBinary) {
		t.Errorf("-print output missing resolved binary path %q:\n%s", wantBinary, out)
	}
	if !strings.Contains(out, wantConfigPath) {
		t.Errorf("-print output missing resolved config path %q:\n%s", wantConfigPath, out)
	}
	if !strings.Contains(out, "data &amp; scratch") {
		t.Errorf("-print output missing XML-escaped data dir (from %q):\n%s", wantDataDir, out)
	}

	// -print must not touch the real filesystem's plist location or
	// launchctl at all.
	if _, err := os.Stat(wantDataDir); err == nil {
		t.Errorf("-print unexpectedly created the data dir %s", wantDataDir)
	}
}

func TestInstallServiceRefusesToOverwriteWithoutForce(t *testing.T) {
	dp := newTestDeps(t)
	stubEnsureTemporal(dp, t, "localhost:7233")
	home := t.TempDir()
	t.Setenv("HOME", home)

	dataDir := filepath.Join(t.TempDir(), "data")
	configPath := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(configPath, []byte("sandbox_image: img@sha256:"+strings.Repeat("a", 64)+"\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	origRun := fakeHostOf(dp).launchctlFn
	fakeHostOf(dp).launchctlFn = func(args ...string) ([]byte, error) { return []byte("ok"), nil }
	t.Cleanup(func() { fakeHostOf(dp).launchctlFn = origRun })

	install := func() error {
		return installServiceMain(dp, []string{"-config", configPath, "-data-dir", dataDir})
	}
	if err := install(); err != nil {
		t.Fatalf("first install: %v", err)
	}

	plistPath, err := hostcontrol.WorkerPlistPath()
	if err != nil {
		t.Fatalf("workerPlistPath: %v", err)
	}
	if _, err := os.Stat(plistPath); err != nil {
		t.Fatalf("plist was not written: %v", err)
	}

	if err := install(); err == nil {
		t.Fatal("second install without -force: want an error, got nil")
	} else if !strings.Contains(err.Error(), "-force") {
		t.Errorf("error does not mention -force: %v", err)
	}

	if err := installServiceMain(dp, []string{"-config", configPath, "-data-dir", dataDir, "-force"}); err != nil {
		t.Fatalf("install with -force: %v", err)
	}
}

func TestDoctorCheckWorkerServiceNamesEachState(t *testing.T) {
	// doctorCheckWorkerService itself short-circuits to a fixed
	// "not applicable (non-macOS)" Name on any non-darwin GOOS -- launchd
	// is macOS-only. This test exercises the darwin-only state machine
	// beyond that early return, so it needs the same guard, not just the
	// function under test: found failing on every push since 2026-09-12
	// once CI started actually running (GitHub Actions' Linux runner),
	// asserting macOS-specific messages the function correctly never
	// produces there.
	if runtime.GOOS != "darwin" {
		t.Skip("launchd doctor check is macOS-only; doctorCheckWorkerService itself short-circuits on this GOOS")
	}
	dir := t.TempDir()
	plistPath := filepath.Join(dir, "dev.factoryd.worker.plist")
	ctx := context.Background()

	t.Run("absent", func(t *testing.T) {
		check := doctorCheckWorkerService(ctx, "launchctl", plistPath)
		if check.Err != nil {
			t.Fatalf("absent state should not fail doctor: %v", check.Err)
		}
		if !strings.Contains(check.Name, "absent") {
			t.Errorf("Name = %q, want it to mention absent", check.Name)
		}
	})

	binary := filepath.Join(dir, "factoryd")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write fake binary: %v", err)
	}
	plist := buildWorkerPlist(workerPlistConfig{
		BinaryPath: binary,
		ConfigPath: filepath.Join(dir, "config.yml"),
		DataDir:    filepath.Join(dir, "data"),
		HomeDir:    dir,
	})
	if err := os.WriteFile(plistPath, plist, 0o644); err != nil {
		t.Fatalf("write plist: %v", err)
	}

	writeFakeLaunchctl := func(t *testing.T, script string) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "launchctl")
		if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script), 0o755); err != nil {
			t.Fatalf("write fake launchctl: %v", err)
		}
		return path
	}

	t.Run("present, not loaded", func(t *testing.T) {
		fake := writeFakeLaunchctl(t, "exit 1\n")
		check := doctorCheckWorkerService(ctx, fake, plistPath)
		if check.Err != nil {
			t.Fatalf("not-loaded state should not fail doctor: %v", check.Err)
		}
		if !strings.Contains(check.Name, "not loaded") {
			t.Errorf("Name = %q, want it to mention not loaded", check.Name)
		}
	})

	t.Run("loaded, running", func(t *testing.T) {
		fake := writeFakeLaunchctl(t, "echo 'state = running'\n")
		check := doctorCheckWorkerService(ctx, fake, plistPath)
		if check.Err != nil {
			t.Fatalf("running state should not fail doctor: %v", check.Err)
		}
		if !strings.Contains(check.Name, "loaded") || !strings.Contains(check.Name, "running") {
			t.Errorf("Name = %q, want it to mention loaded and running", check.Name)
		}
	})

	t.Run("stale binary fails", func(t *testing.T) {
		if err := os.Remove(binary); err != nil {
			t.Fatalf("remove fake binary: %v", err)
		}
		fake := writeFakeLaunchctl(t, "echo 'state = running'\n")
		check := doctorCheckWorkerService(ctx, fake, plistPath)
		if check.Err == nil {
			t.Fatal("stale binary: want a failing check, got nil error")
		}
		if check.Fix == "" {
			t.Error("stale binary: want a Fix naming install-service -force")
		}
	})
}

// TestBuildServePlistDeterministicAndNeverEmbedsToken proves buildServePlist
// mirrors buildWorkerPlist's own determinism/escaping guarantees and,
// critically, never embeds a start token anywhere in the generated plist
// (safety-contract.md's "Console loopback writes" row) -- the stable
// token lives only in the 0600 serve-start-token file next to the
// session config.
func TestBuildServePlistDeterministicAndNeverEmbedsToken(t *testing.T) {
	cfg := servePlistConfig{
		BinaryPath: "/usr/local/bin/factoryd",
		DataDir:    "/Users/op/data & more",
		HomeDir:    "/Users/op",
	}

	first := buildServePlist(cfg)
	second := buildServePlist(cfg)
	if string(first) != string(second) {
		t.Fatalf("buildServePlist is not deterministic:\n%s\n---\n%s", first, second)
	}

	s := string(first)
	if !strings.Contains(s, "dev.factoryd.serve") {
		t.Errorf("plist missing its own Label:\n%s", s)
	}
	if !strings.Contains(s, "/usr/local/bin/factoryd") {
		t.Errorf("plist missing binary path:\n%s", s)
	}
	if !strings.Contains(s, "<string>serve</string>") {
		t.Errorf("plist ProgramArguments missing the \"serve\" subcommand:\n%s", s)
	}
	if !strings.Contains(s, "data &amp; more/logs/serve.out.log") {
		t.Errorf("plist missing stdout log path:\n%s", s)
	}
	if !strings.Contains(s, "data &amp; more/logs/serve.err.log") {
		t.Errorf("plist missing stderr log path:\n%s", s)
	}
	if strings.Contains(s, "FACTORYD_API_START_TOKEN") {
		t.Errorf("plist must never name FACTORYD_API_START_TOKEN at all: token delivery is via the stable file, not env/argv:\n%s", s)
	}
}

// TestBuildServePlistValidatesWithPlutil mirrors
// TestBuildServicePlistValidatesWithPlutil for the serve plist.
func TestBuildServePlistValidatesWithPlutil(t *testing.T) {
	plutil, err := exec.LookPath("plutil")
	if err != nil {
		t.Skip("plutil not on PATH")
	}
	plist := buildServePlist(servePlistConfig{
		BinaryPath: "/usr/local/bin/factoryd",
		DataDir:    "/Users/op/data",
		HomeDir:    "/Users/op",
	})
	cmd := exec.Command(plutil, "-lint", "-")
	cmd.Stdin = strings.NewReader(string(plist))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("plutil -lint rejected the generated serve plist: %v\n%s\n---\n%s", err, out, plist)
	}
}

// TestInstallServiceAlsoInstallsServeByDefault proves a plain
// `factoryd install-service` (no -no-serve) installs and bootstraps BOTH
// LaunchAgents, generates a stable serve-start-token file (0600) next to
// the session config, and that a second install reuses rather than
// rotates that token -- the whole point being that a service restart
// doesn't invalidate the browser's stored token.
func TestInstallServiceAlsoInstallsServeByDefault(t *testing.T) {
	dp := newTestDeps(t)
	stubEnsureTemporal(dp, t, "localhost:7233")
	home := t.TempDir()
	t.Setenv("HOME", home)

	dataDir := filepath.Join(t.TempDir(), "data")
	configPath := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(configPath, []byte("sandbox_image: img@sha256:"+strings.Repeat("a", 64)+"\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	origRun := fakeHostOf(dp).launchctlFn
	fakeHostOf(dp).launchctlFn = func(args ...string) ([]byte, error) { return []byte("ok"), nil }
	t.Cleanup(func() { fakeHostOf(dp).launchctlFn = origRun })

	if err := installServiceMain(dp, []string{"-config", configPath, "-data-dir", dataDir}); err != nil {
		t.Fatalf("installServiceMain: %v", err)
	}

	workerPlistPath, err := hostcontrol.WorkerPlistPath()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(workerPlistPath); err != nil {
		t.Fatalf("worker plist was not written: %v", err)
	}
	servePlistP, err := hostcontrol.ServePlistPath()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(servePlistP); err != nil {
		t.Fatalf("serve plist was not written: %v", err)
	}
	if data, err := os.ReadFile(servePlistP); err != nil || strings.Contains(string(data), "FACTORYD_API_START_TOKEN") {
		t.Fatalf("serve plist embeds a token or could not be read: err=%v", err)
	}

	tokenPath := filepath.Join(filepath.Dir(configPath), serveStableStartTokenFileName)
	info, err := os.Stat(tokenPath)
	if err != nil {
		t.Fatalf("stable start token file was not written: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("stable start token file mode = %v, want 0600", info.Mode().Perm())
	}
	firstToken, ok := readServeStartTokenFile(tokenPath)
	if !ok {
		t.Fatal("could not read stable start token file")
	}

	// Reinstall with -force: the token must survive unchanged.
	if err := installServiceMain(dp, []string{"-config", configPath, "-data-dir", dataDir, "-force"}); err != nil {
		t.Fatalf("second installServiceMain -force: %v", err)
	}
	secondToken, ok := readServeStartTokenFile(tokenPath)
	if !ok {
		t.Fatal("could not read stable start token file after reinstall")
	}
	if secondToken != firstToken {
		t.Fatalf("stable start token rotated across a -force reinstall: %q != %q", firstToken, secondToken)
	}
}

// TestInstallServiceNoServeSkipsServeAgent proves -no-serve installs only
// the worker LaunchAgent, leaving no serve plist or stable token file
// behind.
func TestInstallServiceNoServeSkipsServeAgent(t *testing.T) {
	dp := newTestDeps(t)
	stubEnsureTemporal(dp, t, "localhost:7233")
	home := t.TempDir()
	t.Setenv("HOME", home)

	dataDir := filepath.Join(t.TempDir(), "data")
	configPath := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(configPath, []byte("sandbox_image: img@sha256:"+strings.Repeat("a", 64)+"\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	origRun := fakeHostOf(dp).launchctlFn
	fakeHostOf(dp).launchctlFn = func(args ...string) ([]byte, error) { return []byte("ok"), nil }
	t.Cleanup(func() { fakeHostOf(dp).launchctlFn = origRun })

	if err := installServiceMain(dp, []string{"-config", configPath, "-data-dir", dataDir, "-no-serve"}); err != nil {
		t.Fatalf("installServiceMain -no-serve: %v", err)
	}

	servePlistP, err := hostcontrol.ServePlistPath()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(servePlistP); err == nil {
		t.Fatalf("-no-serve unexpectedly installed a serve plist at %s", servePlistP)
	}
	tokenPath := filepath.Join(filepath.Dir(configPath), serveStableStartTokenFileName)
	if _, err := os.Stat(tokenPath); err == nil {
		t.Fatalf("-no-serve unexpectedly generated a stable start token at %s", tokenPath)
	}
}

// TestUninstallServiceRemovesServeAgentToo proves `factoryd
// uninstall-service` bootouts and removes BOTH LaunchAgents, not only
// worker's.
func TestUninstallServiceRemovesServeAgentToo(t *testing.T) {
	dp := newTestDeps(t)
	stubEnsureTemporal(dp, t, "localhost:7233")
	home := t.TempDir()
	t.Setenv("HOME", home)

	dataDir := filepath.Join(t.TempDir(), "data")
	configPath := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(configPath, []byte("sandbox_image: img@sha256:"+strings.Repeat("a", 64)+"\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	origRun := fakeHostOf(dp).launchctlFn
	fakeHostOf(dp).launchctlFn = func(args ...string) ([]byte, error) { return []byte("ok"), nil }
	t.Cleanup(func() { fakeHostOf(dp).launchctlFn = origRun })

	if err := installServiceMain(dp, []string{"-config", configPath, "-data-dir", dataDir}); err != nil {
		t.Fatalf("installServiceMain: %v", err)
	}
	if err := uninstallServiceMain(dp, nil); err != nil {
		t.Fatalf("uninstallServiceMain: %v", err)
	}

	workerPlistPath, err := hostcontrol.WorkerPlistPath()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(workerPlistPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("worker plist still present after uninstall: err=%v", err)
	}
	servePlistP, err := hostcontrol.ServePlistPath()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(servePlistP); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("serve plist still present after uninstall: err=%v", err)
	}

	// The stable start token file itself is left in place -- see
	// uninstallServeService's own doc comment.
	tokenPath := filepath.Join(filepath.Dir(configPath), serveStableStartTokenFileName)
	if _, err := os.Stat(tokenPath); err != nil {
		t.Fatalf("uninstall unexpectedly removed the stable start token file: %v", err)
	}
}

// TestBuildServePlistIncludesConfigFlagAndUmask is the regression test
// for two adversarial-review findings: the serve plist's ProgramArguments
// must carry -config <path> (so the supervised serve derives the SAME
// stable-token-file path installServeService itself resolved, rather
// than independently re-deriving a possibly-different default), and the
// plist must set Umask 077 so any file launchd itself creates (the
// StandardOutPath/StandardErrorPath logs) lands non-group/other-readable
// regardless of this host's own inherited umask.
func TestBuildServePlistIncludesConfigFlagAndUmask(t *testing.T) {
	cfg := servePlistConfig{
		BinaryPath: "/usr/local/bin/factoryd",
		ConfigPath: "/Users/op/.config/factoryd/config.yml",
		DataDir:    "/Users/op/data",
		HomeDir:    "/Users/op",
	}
	s := string(buildServePlist(cfg))
	if !strings.Contains(s, "<string>-config</string>") || !strings.Contains(s, "<string>/Users/op/.config/factoryd/config.yml</string>") {
		t.Errorf("plist ProgramArguments missing -config <path>:\n%s", s)
	}
	if !strings.Contains(s, "<key>Umask</key>\n\t<integer>77</integer>") {
		t.Errorf("plist missing Umask 077 (adversarial review):\n%s", s)
	}
}

// TestBuildServicePlistIncludesUmask proves the worker plist gets the
// identical Umask 077 key (the same adversarial review) -- its own logs
// can carry ticket/path detail with no business being world/group
// readable either.
func TestBuildServicePlistIncludesUmask(t *testing.T) {
	cfg := workerPlistConfig{
		BinaryPath: "/usr/local/bin/factoryd",
		ConfigPath: "/Users/op/.config/factoryd/config.yml",
		DataDir:    "/Users/op/data",
		HomeDir:    "/Users/op",
	}
	s := string(buildWorkerPlist(cfg))
	if !strings.Contains(s, "<key>Umask</key>\n\t<integer>77</integer>") {
		t.Errorf("worker plist missing Umask 077 (adversarial review):\n%s", s)
	}
}

// TestInstallServeServicePassesResolvedConfigPathToPlist is the
// end-to-end regression test for the -config-flag adversarial-review
// finding above: a plain `factoryd install-service` must produce a
// serve plist whose own -config argument is the SAME path
// installServeService resolved for the stable
// token file -- so a supervised serve reading -config on its own startup
// derives the identical serveStableStartTokenPathFor result.
func TestInstallServeServicePassesResolvedConfigPathToPlist(t *testing.T) {
	dp := newTestDeps(t)
	stubEnsureTemporal(dp, t, "localhost:7233")
	home := t.TempDir()
	t.Setenv("HOME", home)

	dataDir := filepath.Join(t.TempDir(), "data")
	configPath := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(configPath, []byte("sandbox_image: img@sha256:"+strings.Repeat("a", 64)+"\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	origRun := fakeHostOf(dp).launchctlFn
	fakeHostOf(dp).launchctlFn = func(args ...string) ([]byte, error) { return []byte("ok"), nil }
	t.Cleanup(func() { fakeHostOf(dp).launchctlFn = origRun })

	if err := installServiceMain(dp, []string{"-config", configPath, "-data-dir", dataDir}); err != nil {
		t.Fatalf("installServiceMain: %v", err)
	}

	servePlistP, err := hostcontrol.ServePlistPath()
	if err != nil {
		t.Fatal(err)
	}
	plistBytes, err := os.ReadFile(servePlistP)
	if err != nil {
		t.Fatalf("read serve plist: %v", err)
	}
	args := hostcontrol.ProgramArgumentsStrings(plistBytes)
	gotConfig, ok := hostcontrol.ProgramArgumentsFlagValue(args, "-config")
	if !ok {
		t.Fatalf("serve plist has no -config argument: %v", args)
	}
	wantConfig, err := filepath.Abs(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if gotConfig != wantConfig {
		t.Errorf("serve plist -config = %q, want %q (the same path installServeService resolved for the token file)", gotConfig, wantConfig)
	}
}
