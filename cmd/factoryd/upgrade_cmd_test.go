package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"buildgate/internal/hostcontrol"
	"buildgate/internal/sessionconfig"
)

const (
	upgradeOldSHA    = "aaaaaaaaaaaa"
	upgradeTargetSHA = "bbbbbbbbbbbb"
)

// fakeUpgrade stands in for git, go, make, launchctl and the installed
// factoryd. It records every command as "name args...".
type fakeUpgrade struct {
	t         *testing.T
	source    string
	binary    string
	calls     []string
	dirty     string
	tags      string
	head      string
	installed string // the version the installed binary prints
	afterMake string // the version it prints once make install succeeded
	makeErr   error
	makeOut   string
	onMake    func()
}

func newFakeUpgrade(dp *deps, t *testing.T) *fakeUpgrade {
	t.Helper()
	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "go.mod"), []byte("module buildgate\n\ngo 1.26\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f := &fakeUpgrade{
		t: t, source: source, binary: filepath.Join(t.TempDir(), "factoryd"),
		tags: "m5.1\nm5\nfoo\nm4\n", head: upgradeOldSHA,
		installed: upgradeOldSHA, afterMake: upgradeTargetSHA,
	}
	prev := fakeHostOf(dp).runUpgradeCommandFn
	fakeHostOf(dp).runUpgradeCommandFn = f.exec
	t.Cleanup(func() { fakeHostOf(dp).runUpgradeCommandFn = prev })
	return f
}

func (f *fakeUpgrade) called(prefix string) bool {
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

func (f *fakeUpgrade) exec(c upgradeCmd) ([]byte, error) {
	line := c.Name + " " + strings.Join(c.Args, " ")
	f.calls = append(f.calls, line)
	switch {
	case c.Name == "git":
		rest := strings.TrimPrefix(line, "git -C "+f.source+" ")
		switch {
		case rest == "rev-parse --git-dir":
			return []byte(".git\n"), nil
		case strings.HasPrefix(rest, "status"):
			return []byte(f.dirty), nil
		case strings.HasPrefix(rest, "fetch"):
			return nil, nil
		case strings.HasPrefix(rest, "tag"):
			return []byte(f.tags), nil
		case rest == "rev-parse --short=12 HEAD^{commit}":
			return []byte(f.head + "\n"), nil
		case strings.HasPrefix(rest, "rev-parse --short=12 m5.1^{commit}"), strings.HasPrefix(rest, "rev-parse --short=12 "+upgradeTargetSHA):
			return []byte(upgradeTargetSHA + "\n"), nil
		case strings.HasPrefix(rest, "rev-parse"):
			return []byte("fatal: unknown revision"), errors.New("exit 128")
		case strings.HasPrefix(rest, "rev-list --count"):
			return []byte("42\n"), nil
		case strings.HasPrefix(rest, "checkout --detach"):
			f.head = upgradeTargetSHA
			return nil, nil
		}
	case c.Name == "go" && strings.Join(c.Args, " ") == "env GOBIN":
		return []byte(filepath.Dir(f.binary) + "\n"), nil
	case c.Name == "make":
		if f.onMake != nil {
			f.onMake()
		}
		if f.makeErr != nil {
			return []byte(f.makeOut), f.makeErr
		}
		f.installed = f.afterMake
		return []byte("installing factoryd\n"), nil
	case c.Name == f.binary && len(c.Args) == 1 && c.Args[0] == "version":
		return []byte("factoryd version " + f.installed + "\n"), nil
	case c.Name == f.binary && c.Args[0] == "install-skill", c.Name == "launchctl":
		return nil, nil
	}
	f.t.Fatalf("unexpected command: %s", line)
	return nil, nil
}

type upgradeHarness struct {
	*fakeUpgrade
	profiles *profileFixture
	queue    *stoppedProc
	spawned  []string
}

type stoppedProc struct{ pid int }

// newUpgradeHarness isolates HOME and the config dir, stubs the stop and
// spawn seams, and starts a stand-in worker and serve for the default
// profile.
func newUpgradeHarness(dp *deps, t *testing.T) *upgradeHarness {
	t.Helper()
	stubStopSeams(dp, t)
	profiles := newProfileFixture(t, "default", "work")
	f := newFakeUpgrade(dp, t)
	h := &upgradeHarness{fakeUpgrade: f, profiles: profiles}

	prevS := fakeHostOf(dp).spawnServeFn
	fakeHostOf(dp).spawnServeFn = func(w io.Writer, binaryPath, configPath, dataDir, addr string) error {
		h.spawned = append(h.spawned, "serve "+binaryPath+" "+filepath.Base(configPath)+" "+dataDir+" "+addr)
		return nil
	}
	prevW := fakeHostOf(dp).spawnWorkerFn
	fakeHostOf(dp).spawnWorkerFn = func(w io.Writer, binaryPath, configPath, dataDir string, _ []string, pidPath, temporalAddress string) error {
		h.spawned = append(h.spawned, "worker "+binaryPath+" "+filepath.Base(configPath)+" "+dataDir+" "+temporalAddress)
		return nil
	}
	// A running process that is not a worker (no worker heartbeat) is replaced
	// by a worker on the Temporal address temporal.ensure names.
	prevT := fakeTemporalOf(dp).ensureFn
	fakeTemporalOf(dp).ensureFn = func(context.Context, io.Writer) string { return "localhost:7233" }
	t.Cleanup(func() {
		fakeHostOf(dp).spawnWorkerFn, fakeHostOf(dp).spawnServeFn, fakeTemporalOf(dp).ensureFn = prevW, prevS, prevT
	})

	prevWait := upgradeWaitInterval
	upgradeWaitInterval = 10 * time.Millisecond
	t.Cleanup(func() { upgradeWaitInterval = prevWait })
	return h
}

func (h *upgradeHarness) startDefaultProcesses() (queuePID, servePID int) {
	dir := h.profiles.roots["default"]
	queue, serve := standIn(h.t), standIn(h.t)
	writePIDFile(h.t, dir, "quickstart-queue-run.pid", queue.Process.Pid)
	writeConsoleRecord(h.t, dir, serve.Process.Pid)
	return queue.Process.Pid, serve.Process.Pid
}

func (h *upgradeHarness) makeSkill(parent string) string {
	dir := filepath.Join(os.Getenv("HOME"), parent, "buildgate")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		h.t.Fatal(err)
	}
	return filepath.Dir(dir)
}

func (h *upgradeHarness) run(dp *deps, args ...string) (string, error) {
	var out bytes.Buffer
	err := upgradeRun(dp, append([]string{"-source", h.source}, args...), strings.NewReader(""), &out, false, false)
	return out.String(), err
}

func TestUpgradeCleanPath(t *testing.T) {
	dp := newTestDeps(t)
	h := newUpgradeHarness(dp, t)
	queuePID, servePID := h.startDefaultProcesses()
	agents := h.makeSkill(".agents/skills")
	claude := h.makeSkill(".claude/skills")

	out, err := h.run(dp, "-yes")
	if err != nil {
		t.Fatalf("upgrade: %v\n%s", err, out)
	}
	t.Logf("plan and summary:\n%s", out)

	if !waitDead(t, queuePID) || !waitDead(t, servePID) {
		t.Error("worker and serve were not stopped")
	}
	// Order: fetch, checkout, make, verify, then skills.
	order := []string{"git -C " + h.source + " fetch --tags origin", "git -C " + h.source + " checkout --detach " + upgradeTargetSHA, "make -C " + h.source + " install INSTALL_FINISH=0", h.binary + " version", h.binary + " install-skill -dir " + agents}
	last := -1
	for _, want := range order {
		idx := -1
		for i, c := range h.calls {
			if i > last && c == want {
				idx = i
				break
			}
		}
		if idx < 0 {
			t.Fatalf("call %q missing or out of order in:\n%s", want, strings.Join(h.calls, "\n"))
		}
		last = idx
	}
	if !h.called(h.binary + " install-skill -dir " + claude) {
		t.Error("the .claude skill was not refreshed")
	}
	dir := h.profiles.roots["default"]
	if len(h.spawned) != 2 ||
		!strings.HasPrefix(h.spawned[0], "worker "+h.binary+" config.yml "+dir) ||
		!strings.HasPrefix(h.spawned[1], "serve "+h.binary+" config.yml "+dir+" 127.0.0.1:8090") {
		t.Errorf("restart used the wrong binary, config or data dir: %v", h.spawned)
	}
	for _, want := range []string{
		"upgrade: " + upgradeOldSHA + " -> " + upgradeTargetSHA + " (m5.1, 42 commits)",
		"images:  re-pointed for default, work",
		"restart: worker (" + dir + "), detached",
		"restart: serve (" + dir + "), detached",
		"upgraded to m5.1 (" + upgradeTargetSHA + ")",
		"skills refreshed:",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q", want)
		}
	}
}

func TestUpgradeAlreadyUpToDate(t *testing.T) {
	dp := newTestDeps(t)
	h := newUpgradeHarness(dp, t)
	h.installed = upgradeTargetSHA
	out, err := h.run(dp, "-yes")
	if err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	if !strings.Contains(out, "already on m5.1 ("+upgradeTargetSHA+")") {
		t.Errorf("output: %s", out)
	}
	if h.called("make") || h.called("git -C "+h.source+" checkout") {
		t.Error("an up-to-date machine must not be touched")
	}
}

func TestUpgradeRefusesDirtySource(t *testing.T) {
	dp := newTestDeps(t)
	h := newUpgradeHarness(dp, t)
	h.dirty = " M Makefile\n"
	out, err := h.run(dp, "-yes")
	if err == nil || !strings.Contains(err.Error(), "uncommitted changes") {
		t.Fatalf("want a dirty-tree refusal, got %v\n%s", err, out)
	}
	if h.called("make") || h.called("git -C "+h.source+" fetch") {
		t.Error("nothing may run against a dirty source")
	}
}

func TestUpgradeRefusesNonBuildgateCheckout(t *testing.T) {
	dp := newTestDeps(t)
	h := newUpgradeHarness(dp, t)
	if err := os.WriteFile(filepath.Join(h.source, "go.mod"), []byte("module example.com/other\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := h.run(dp, "-yes")
	if err == nil || !strings.Contains(err.Error(), "not a buildgate checkout") || !strings.Contains(err.Error(), "-source") {
		t.Fatalf("want a not-buildgate refusal naming -source, got %v", err)
	}
}

func TestUpgradeFallsBackToActiveProfileImageSourceRoot(t *testing.T) {
	dp := newTestDeps(t)
	h := newUpgradeHarness(dp, t)
	h.installed = upgradeTargetSHA
	path := sessionconfig.ProfilePath("default")
	b, _ := os.ReadFile(path)
	writeSessionConfig(t, path, string(b)+"image_source_root: "+h.source+"\n")
	var out bytes.Buffer
	if err := upgradeRun(dp, []string{"-yes"}, strings.NewReader(""), &out, false, false); err != nil {
		t.Fatalf("upgrade: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "already on m5.1") {
		t.Errorf("output: %s", out.String())
	}
}

func TestUpgradeRestartsAWorkerAsAWorker(t *testing.T) {
	dp := newTestDeps(t)
	h := newUpgradeHarness(dp, t)
	queuePID, _ := h.startDefaultProcesses()
	dir := h.profiles.roots["default"]
	writeFreshWorkerHeartbeat(t, dir, queuePID, "localhost:7233", 3)
	out, err := h.run(dp, "-yes")
	if err != nil {
		t.Fatalf("upgrade: %v\n%s", err, out)
	}
	if !strings.Contains(out, "restart: worker ("+dir+"), detached") {
		t.Errorf("plan lacks the worker restart:\n%s", out)
	}
	if len(h.spawned) != 2 || h.spawned[0] != "worker "+h.binary+" config.yml "+dir+" localhost:7233" {
		t.Errorf("spawned %v, want the worker with its Temporal address then serve", h.spawned)
	}
}

func TestUpgradeRefusesWhileAWorkerRunsSeveralRequests(t *testing.T) {
	dp := newTestDeps(t)
	h := newUpgradeHarness(dp, t)
	queuePID, _ := h.startDefaultProcesses()
	writeFreshWorkerHeartbeat(t, h.profiles.roots["default"], queuePID, "localhost:7233", 3, "req-41", "req-42")
	out, err := h.run(dp, "-yes")
	if err == nil || !strings.Contains(err.Error(), "req-41") || !strings.Contains(err.Error(), "req-42") {
		t.Fatalf("want a refusal naming both requests, got %v\n%s", err, out)
	}
	if !alive(queuePID) {
		t.Error("a build must never be interrupted")
	}
}

func TestUpgradeRefusesWhileARequestIsBuilding(t *testing.T) {
	dp := newTestDeps(t)
	h := newUpgradeHarness(dp, t)
	queuePID, _ := h.startDefaultProcesses()
	writeFreshHeartbeatWithoutAddress(t, h.profiles.roots["default"], queuePID, "req-42")
	out, err := h.run(dp, "-yes")
	if err == nil || !strings.Contains(err.Error(), "req-42") || !strings.Contains(err.Error(), "-wait") {
		t.Fatalf("want a refusal naming req-42 and -wait, got %v\n%s", err, out)
	}
	if !alive(queuePID) || h.called("make") || h.called("git -C "+h.source+" checkout") {
		t.Error("a build must never be interrupted")
	}
}

func TestUpgradeWaitWaitsThenProceeds(t *testing.T) {
	dp := newTestDeps(t)
	h := newUpgradeHarness(dp, t)
	queuePID, _ := h.startDefaultProcesses()
	dir := h.profiles.roots["default"]
	writeFreshHeartbeatWithoutAddress(t, dir, queuePID, "req-42")
	go func() {
		time.Sleep(60 * time.Millisecond)
		writeFreshHeartbeatWithoutAddress(t, dir, queuePID, "")
	}()
	start := time.Now()
	out, err := h.run(dp, "-yes", "-wait")
	if err != nil {
		t.Fatalf("upgrade -wait: %v\n%s", err, out)
	}
	if time.Since(start) < 50*time.Millisecond {
		t.Error("-wait did not wait for the request")
	}
	if !strings.Contains(out, "waiting for req-42") {
		t.Errorf("no waiting line:\n%s", out)
	}
	if !h.called("make -C " + h.source + " install INSTALL_FINISH=0") {
		t.Error("the install did not run after the wait")
	}
}

func TestUpgradeMakeInstallFailureSaysStateAndRestore(t *testing.T) {
	dp := newTestDeps(t)
	h := newUpgradeHarness(dp, t)
	queuePID, _ := h.startDefaultProcesses()
	h.makeErr, h.makeOut = errors.New("exit status 2"), "step 1\nboom: image build failed\n"
	out, err := h.run(dp, "-yes")
	if err == nil {
		t.Fatal("want an error")
	}
	if !waitDead(t, queuePID) {
		t.Fatal("worker should be stopped")
	}
	for _, want := range []string{
		"boom: image build failed",
		"worker (" + h.profiles.roots["default"] + ") is stopped",
		h.source + " is checked out at m5.1 (" + upgradeTargetSHA + ")",
		"git -C " + h.source + " checkout " + upgradeOldSHA + " && make -C " + h.source + " install",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if len(h.spawned) != 0 {
		t.Error("nothing is restarted after a failed install")
	}
}

func TestUpgradeVersionMismatchAfterInstall(t *testing.T) {
	dp := newTestDeps(t)
	h := newUpgradeHarness(dp, t)
	h.afterMake = "cccccccccccc"
	out, err := h.run(dp, "-yes")
	if err == nil || !strings.Contains(err.Error(), "cccccccccccc") {
		t.Fatalf("want a version-mismatch error, got %v\n%s", err, out)
	}
	if !strings.Contains(out, "checkout "+upgradeOldSHA+" && make") || len(h.spawned) != 0 {
		t.Errorf("mismatch must report the restore command and restart nothing:\n%s", out)
	}
}

func TestUpgradeNonTTYWithoutYesIsRefused(t *testing.T) {
	dp := newTestDeps(t)
	h := newUpgradeHarness(dp, t)
	var out bytes.Buffer
	err := upgradeRun(dp, []string{"-source", h.source}, strings.NewReader("y\n"), &out, false, false)
	if err == nil || !strings.Contains(err.Error(), "-yes") {
		t.Fatalf("want a -yes refusal, got %v", err)
	}
	if len(h.calls) != 0 {
		t.Errorf("nothing may run: %v", h.calls)
	}
}

func TestUpgradeInteractiveAnswerNoChangesNothing(t *testing.T) {
	dp := newTestDeps(t)
	h := newUpgradeHarness(dp, t)
	var out bytes.Buffer
	err := upgradeRun(dp, []string{"-source", h.source}, strings.NewReader("n\n"), &out, true, false)
	if err == nil || !strings.Contains(err.Error(), "cancelled") {
		t.Fatalf("want cancelled, got %v", err)
	}
	if !strings.Contains(out.String(), "Proceed? [y/N]") || h.called("make") {
		t.Errorf("output %q, calls %v", out.String(), h.calls)
	}
}

func TestUpgradeLeavesSymlinkedSkillAlone(t *testing.T) {
	dp := newTestDeps(t)
	h := newUpgradeHarness(dp, t)
	real := h.makeSkill(".agents/skills")
	home := os.Getenv("HOME")
	elsewhere := filepath.Join(t.TempDir(), "skill-source")
	if err := os.MkdirAll(elsewhere, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, ".claude", "skills"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, filepath.Join(home, ".claude", "skills", "buildgate")); err != nil {
		t.Fatal(err)
	}
	out, err := h.run(dp, "-yes")
	if err != nil {
		t.Fatalf("upgrade: %v\n%s", err, out)
	}
	if !h.called(h.binary + " install-skill -dir " + real) {
		t.Error("the real skill dir should be refreshed")
	}
	if h.called(h.binary + " install-skill -dir " + filepath.Join(home, ".claude", "skills")) {
		t.Error("a symlinked skill dir must be left alone")
	}
}

func TestUpgradeKickstartsLaunchdServicesInsteadOfSpawning(t *testing.T) {
	dp := newTestDeps(t)
	if runtime.GOOS != "darwin" {
		t.Skip("launchd supervision is macOS-only")
	}
	h := newUpgradeHarness(dp, t)
	dir := h.profiles.roots["default"]
	plist, err := hostcontrol.ServePlistPath()
	if err != nil {
		t.Fatal(err)
	}
	writeSessionConfig(t, plist, "<plist><dict><key>ProgramArguments</key><array><string>/x/factoryd</string><string>serve</string><string>-data-dir</string><string>"+dir+"</string></array></dict></plist>")
	fakeHostOf(dp).launchdServicePIDFn = func(domain string) (int, bool) {
		return 99, strings.HasSuffix(domain, "/"+hostcontrol.ServeServiceLabel)
	}

	out, err := h.run(dp, "-yes")
	if err != nil {
		t.Fatalf("upgrade: %v\n%s", err, out)
	}
	if !h.called("launchctl kickstart -k gui/") || !strings.Contains(strings.Join(h.calls, "\n"), hostcontrol.ServeServiceLabel) {
		t.Errorf("serve was not kickstarted: %v", h.calls)
	}
	if len(h.spawned) != 0 {
		t.Errorf("a launchd service must not be spawned: %v", h.spawned)
	}
	if !strings.Contains(out, "restart: serve ("+dir+"), launchd "+hostcontrol.ServeServiceLabel) {
		t.Errorf("plan lacks the launchd serve:\n%s", out)
	}
}
