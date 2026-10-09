package main

import (
	"buildgate/internal/hostcontrol"
	"buildgate/internal/sandbox"
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// uninstallFixture builds a fake HOME holding the buildgate footprint, a fake
// docker that logs every call, and an env wired to both. Nothing real is
// touched: stop and the launchd removal are recorded, not run.
type uninstallFixture struct {
	t         *testing.T
	home      string
	exe       string
	dockerLog string
	out       *bytes.Buffer
	env       *uninstallEnv
	stopArgs  [][]string
	serviceN  int
}

func newUninstallFixture(dp *deps, t *testing.T) *uninstallFixture {
	t.Helper()
	root := t.TempDir()
	f := &uninstallFixture{t: t, home: filepath.Join(root, "home"), out: &bytes.Buffer{}}
	for _, rel := range []string{".agents/skills/buildgate", ".claude/skills/buildgate", ".config/factoryd", "buildgate/data/requests", "go/bin"} {
		if err := os.MkdirAll(filepath.Join(f.home, rel), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, rel := range []string{".agents/skills/buildgate/SKILL.md", ".claude/skills/buildgate/SKILL.md", ".config/factoryd/config.yml", "buildgate/data/requests/r1"} {
		if err := os.WriteFile(filepath.Join(f.home, rel), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	f.exe = filepath.Join(f.home, "go/bin/factoryd")
	if err := os.WriteFile(f.exe, []byte("bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	f.dockerLog = filepath.Join(root, "docker.log")
	docker := writeFakeDockerScript(t, `#!/bin/sh
echo "$@" >> `+f.dockerLog+`
case "$1 $2" in
  "compose -p") [ "$4" = ps ] && echo abc123 ;;
esac
exit 0
`)
	f.env = newUninstallEnv(dp, f.home, f.exe, f.out)
	f.env.docker = docker
	f.env.xdgConfg = filepath.Join(f.home, ".config")
	f.env.stop = func(args []string) error { f.stopArgs = append(f.stopArgs, args); return nil }
	f.env.service = func() error { f.serviceN++; return nil }
	return f
}

func (f *uninstallFixture) exists(rel string) bool {
	_, err := os.Lstat(filepath.Join(f.home, rel))
	return err == nil
}

func (f *uninstallFixture) dockerCalls() string {
	b, _ := os.ReadFile(f.dockerLog)
	return string(b)
}

func TestUninstallDryRunChangesNothing(t *testing.T) {
	dp := newTestDeps(t)
	f := newUninstallFixture(dp, t)
	if err := uninstallRun(f.env, false, true, false, strings.NewReader("")); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{".agents/skills/buildgate/SKILL.md", ".claude/skills/buildgate", ".config/factoryd/config.yml", "buildgate/data", "go/bin/factoryd"} {
		if !f.exists(rel) {
			t.Errorf("dry run removed %s", rel)
		}
	}
	if len(f.stopArgs) != 0 || f.serviceN != 0 {
		t.Errorf("dry run stopped daemons or removed services: %v %d", f.stopArgs, f.serviceN)
	}
	if got := f.dockerCalls(); strings.Contains(got, "rmi") || strings.Contains(got, " down") || strings.Contains(got, "rm -f") {
		t.Errorf("dry run issued destructive docker calls:\n%s", got)
	}
	if !strings.Contains(f.out.String(), "Dry run: nothing changed.") {
		t.Errorf("output missing dry-run notice:\n%s", f.out.String())
	}
}

func TestUninstallRefusesWithoutConfirmationOffTerminal(t *testing.T) {
	dp := newTestDeps(t)
	f := newUninstallFixture(dp, t)
	err := uninstallRun(f.env, false, false, false, strings.NewReader("y\n"))
	if err == nil || !strings.Contains(err.Error(), "-yes") {
		t.Fatalf("err = %v, want a refusal naming -yes", err)
	}
	if !f.exists(".agents/skills/buildgate") || len(f.stopArgs) != 0 {
		t.Error("a refused uninstall must change nothing")
	}
}

func TestUninstallYesRemovesFootprintButKeepsConfigAndData(t *testing.T) {
	dp := newTestDeps(t)
	f := newUninstallFixture(dp, t)
	if err := uninstallRun(f.env, true, false, false, strings.NewReader("")); err != nil {
		t.Fatalf("uninstall: %v\n%s", err, f.out.String())
	}
	for _, rel := range []string{".agents/skills/buildgate", ".claude/skills/buildgate", "go/bin/factoryd"} {
		if f.exists(rel) {
			t.Errorf("%s still exists", rel)
		}
	}
	for _, rel := range []string{".config/factoryd/config.yml", "buildgate/data/requests/r1"} {
		if !f.exists(rel) {
			t.Errorf("%s was deleted without -purge", rel)
		}
	}
	if len(f.stopArgs) != 1 || strings.Join(f.stopArgs[0], " ") != "-all" {
		t.Errorf("stop args = %v, want [-all]", f.stopArgs)
	}
	calls := f.dockerCalls()
	for _, want := range []string{"compose -p buildgate down", "compose -p buildgate-openshell down", "rm -f factoryd-local-registry", "rmi localhost:5050/buildgate-worker:local", "rmi localhost:5050/factoryd-meter:local"} {
		if !strings.Contains(calls, want) {
			t.Errorf("docker calls missing %q:\n%s", want, calls)
		}
	}
	if strings.Contains(calls, "down -v") {
		t.Error("volumes removed without -purge")
	}
}

func TestUninstallPurgeDeletesConfigDataAndVolumes(t *testing.T) {
	dp := newTestDeps(t)
	f := newUninstallFixture(dp, t)
	f.env.purge = true
	if err := uninstallRun(f.env, true, false, false, strings.NewReader("")); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{".config/factoryd", "buildgate"} {
		if f.exists(rel) {
			t.Errorf("%s survived -purge", rel)
		}
	}
	if !strings.Contains(f.dockerCalls(), "compose -p buildgate down -v") {
		t.Errorf("-purge must remove the Temporal volumes:\n%s", f.dockerCalls())
	}
}

// A worker killed mid-launch leaves its read-only `.factory/` snapshot under
// the run's directory (files 0444 in 0555 directories); -purge removes it.
func TestUninstallPurgeDeletesARunDirectoryHoldingAReadOnlyFactoryDirSnapshot(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root removes a read-only tree without help")
	}
	dp := newTestDeps(t)
	f := newUninstallFixture(dp, t)
	f.env.purge = true
	snapshot := filepath.Join(f.home, "buildgate/data/runs/r1/factory-dir-1-1/.factory/sub")
	if err := os.MkdirAll(snapshot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(snapshot, "lint.sh"), []byte("exit 0\n"), 0o444); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{snapshot, filepath.Dir(snapshot)} {
		if err := os.Chmod(dir, 0o555); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { _ = sandbox.RemoveTree(filepath.Join(f.home, "buildgate")) })
	if err := uninstallRun(f.env, true, false, false, strings.NewReader("")); err != nil {
		t.Fatalf("uninstallRun: %v\n%s", err, f.out.String())
	}
	if f.exists("buildgate") {
		t.Errorf("the data directory survived -purge:\n%s", f.out.String())
	}
}

func TestUninstallPurgeNeedsTypedConfirmation(t *testing.T) {
	dp := newTestDeps(t)
	f := newUninstallFixture(dp, t)
	f.env.purge = true
	if err := uninstallRun(f.env, false, false, true, strings.NewReader("y\n")); err == nil {
		t.Fatal("a plain 'y' must not confirm -purge")
	}
	if !f.exists("buildgate/data/requests/r1") {
		t.Fatal("data deleted despite a refused confirmation")
	}
	if err := uninstallRun(f.env, false, false, true, strings.NewReader("purge\n")); err != nil {
		t.Fatalf("typed 'purge' should confirm: %v", err)
	}
}

func TestUninstallSkipsSkillsItDidNotInstall(t *testing.T) {
	dp := newTestDeps(t)
	f := newUninstallFixture(dp, t)
	// A symlinked skill and a directory with no SKILL.md are the operator's.
	target := filepath.Join(t.TempDir(), "mine")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	claude := filepath.Join(f.home, ".claude/skills/buildgate")
	if err := os.RemoveAll(claude); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, claude); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(f.home, ".agents/skills/buildgate/SKILL.md")); err != nil {
		t.Fatal(err)
	}
	if err := uninstallRun(f.env, true, false, false, strings.NewReader("")); err != nil {
		t.Fatal(err)
	}
	if !f.exists(".claude/skills/buildgate") || !f.exists(".agents/skills/buildgate") {
		t.Error("removed a skill directory that is a symlink or lacks SKILL.md")
	}
}

func TestUninstallOnlyRemovesABinaryNamedFactoryd(t *testing.T) {
	dp := newTestDeps(t)
	f := newUninstallFixture(dp, t)
	tmp := filepath.Join(f.home, "go/bin/factoryd.test")
	if err := os.WriteFile(tmp, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	f.env.exePath = tmp
	if err := uninstallRun(f.env, true, false, false, strings.NewReader("")); err != nil {
		t.Fatal(err)
	}
	if !f.exists("go/bin/factoryd.test") {
		t.Error("removed an executable that is not named factoryd (e.g. a go test binary)")
	}
}

// TestUninstallRemovesTheLinksToItsBinary: the symlink `make install` puts
// on PATH goes with the binary; a factoryd that is some other file, or a
// link to some other binary, stays.
func TestUninstallRemovesTheLinksToItsBinary(t *testing.T) {
	dp := newTestDeps(t)
	f := newUninstallFixture(dp, t)
	exe, err := filepath.EvalSymlinks(f.env.exePath)
	if err != nil {
		t.Fatal(err)
	}
	f.env.exePath = exe
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	linked, foreign, other := filepath.Join(root, "brew"), filepath.Join(root, "foreign"), filepath.Join(root, "other")
	for _, dir := range []string{linked, foreign, other} {
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(exe, filepath.Join(linked, "factoryd")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(foreign, "factoryd"), []byte("another build"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(other, "elsewhere"), []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(other, "elsewhere"), filepath.Join(other, "factoryd")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", strings.Join([]string{linked, foreign, other, linked}, string(os.PathListSeparator)))

	if err := uninstallRun(f.env, true, false, false, strings.NewReader("")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(linked, "factoryd")); !os.IsNotExist(err) {
		t.Errorf("the link to the removed binary is still there (err %v)", err)
	}
	if _, err := os.Stat(exe); !os.IsNotExist(err) {
		t.Errorf("the binary is still there (err %v)", err)
	}
	for _, kept := range []string{filepath.Join(foreign, "factoryd"), filepath.Join(other, "factoryd")} {
		if _, err := os.Lstat(kept); err != nil {
			t.Errorf("removed %s, which is not a link to the uninstalled binary", kept)
		}
	}
}

func TestUninstallStopFailureRemovesNothing(t *testing.T) {
	dp := newTestDeps(t)
	f := newUninstallFixture(dp, t)
	f.env.stop = func([]string) error { return errors.New("a request is building") }
	err := uninstallRun(f.env, true, false, false, strings.NewReader(""))
	if err == nil || !strings.Contains(err.Error(), "-force") {
		t.Fatalf("err = %v, want a stop failure pointing at -force", err)
	}
	for _, rel := range []string{".agents/skills/buildgate", "go/bin/factoryd", ".config/factoryd"} {
		if !f.exists(rel) {
			t.Errorf("%s removed after the daemons could not be stopped", rel)
		}
	}
	if got := f.dockerCalls(); strings.Contains(got, "rmi") || strings.Contains(got, " down") {
		t.Errorf("docker teardown ran after a failed stop:\n%s", got)
	}
}

func TestUninstallForcePassesThroughToStop(t *testing.T) {
	dp := newTestDeps(t)
	f := newUninstallFixture(dp, t)
	f.env.force = true
	if err := uninstallRun(f.env, true, false, false, strings.NewReader("")); err != nil {
		t.Fatal(err)
	}
	if len(f.stopArgs) != 1 || strings.Join(f.stopArgs[0], " ") != "-all -force" {
		t.Errorf("stop args = %v, want [-all -force]", f.stopArgs)
	}
}

func TestUninstallRemovesLaunchAgentsWhenPresent(t *testing.T) {
	dp := newTestDeps(t)
	f := newUninstallFixture(dp, t)
	dir := filepath.Join(f.home, "Library", "LaunchAgents")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, hostcontrol.WorkerServiceLabel+".plist"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := uninstallRun(f.env, true, false, false, strings.NewReader("")); err != nil {
		t.Fatal(err)
	}
	if f.serviceN != 1 {
		t.Errorf("uninstall-service ran %d times, want 1", f.serviceN)
	}
}

func TestUninstallWithoutDockerStillRemovesTheRest(t *testing.T) {
	dp := newTestDeps(t)
	f := newUninstallFixture(dp, t)
	f.env.docker = filepath.Join(t.TempDir(), "no-such-docker")
	if err := uninstallRun(f.env, true, false, false, strings.NewReader("")); err != nil {
		t.Fatalf("uninstall without docker: %v\n%s", err, f.out.String())
	}
	if f.exists(".agents/skills/buildgate") || f.exists("go/bin/factoryd") {
		t.Error("non-docker steps were skipped because docker is unavailable")
	}
}

// A purge empties the gateway's state on the Docker VM through a throwaway
// container, before the image it runs from is removed; without -purge the
// state is left alone.
func TestUninstallPurgeDeletesGatewayStateBeforeRemovingItsImage(t *testing.T) {
	dp := newTestDeps(t)
	f := newUninstallFixture(dp, t)
	f.env.purge = true
	if err := uninstallRun(f.env, true, false, false, strings.NewReader("")); err != nil {
		t.Fatalf("uninstallRun: %v\n%s", err, f.out.String())
	}
	calls := f.dockerCalls()
	run := strings.Index(calls, "run --rm --user 0 --entrypoint sh --mount type=bind,source="+uninstallGatewayStateDir)
	rmi := strings.Index(calls, "rmi "+uninstallImages[0])
	if run < 0 || rmi < 0 || run > rmi {
		t.Fatalf("want the state-purge container before removing %s:\n%s", uninstallImages[0], calls)
	}
	if !strings.Contains(f.out.String(), "done    DELETE the gateway's state on the Docker VM") {
		t.Errorf("output does not report the state deletion:\n%s", f.out.String())
	}
}

func TestUninstallWithoutPurgeKeepsGatewayState(t *testing.T) {
	dp := newTestDeps(t)
	f := newUninstallFixture(dp, t)
	if err := uninstallRun(f.env, true, false, false, strings.NewReader("")); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(f.dockerCalls(), uninstallGatewayStateDir) {
		t.Errorf("a non-purge uninstall touched the gateway state:\n%s", f.dockerCalls())
	}
}
