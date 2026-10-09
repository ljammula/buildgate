package main

import (
	"buildgate/internal/consolelink"
	"buildgate/internal/hostcontrol"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// TestQuickstartPortOwnedByPID proves quickstartPortOwnedByPID parses
// lsof's own whitespace-separated pid list and matches by exact pid.
func TestQuickstartPortOwnedByPID(t *testing.T) {
	dp := newTestDeps(t)
	orig := fakeHostOf(dp).lsofFn
	t.Cleanup(func() { fakeHostOf(dp).lsofFn = orig })

	fakeHostOf(dp).lsofFn = func(port string) ([]byte, error) {
		if port != "8090" {
			t.Fatalf("quickstartLsofRun called with port %q, want 8090", port)
		}
		return []byte("111\n222\n"), nil
	}
	if !hostcontrol.QuickstartPortOwnedByPID(dp, "8090", 222) {
		t.Error("want true: 222 is among lsof's reported pids")
	}
	if hostcontrol.QuickstartPortOwnedByPID(dp, "8090", 333) {
		t.Error("want false: 333 is not among lsof's reported pids")
	}

	fakeHostOf(dp).lsofFn = func(port string) ([]byte, error) {
		return nil, fmt.Errorf("lsof failed")
	}
	if hostcontrol.QuickstartPortOwnedByPID(dp, "8090", 111) {
		t.Error("want false when lsof itself errors")
	}
}

// TestQuickstartServeVerifiedOursRequiresPortOwnershipAndUID is the
// regression test for an adversarial-review finding's low-level
// building block: a pid file alone (alive, matching binary name) is not
// enough -- it must also own the listening port AND run as this same
// uid.
func TestQuickstartServeVerifiedOursRequiresPortOwnershipAndUID(t *testing.T) {
	dp := newTestDeps(t)
	dataDir := t.TempDir()
	pidPath := filepath.Join(dataDir, "quickstart-serve.pid")
	if err := os.WriteFile(pidPath, []byte(fmt.Sprintf("%d", os.Getpid())), 0o644); err != nil {
		t.Fatal(err)
	}

	restoreLooksLike := fakeHostOf(dp).pidLooksLikeWorkerFn
	fakeHostOf(dp).pidLooksLikeWorkerFn = func(pid int) bool { return true }
	restoreLsof := fakeHostOf(dp).lsofFn
	restoreUID := fakeHostOf(dp).processUIDFn
	t.Cleanup(func() {
		fakeHostOf(dp).pidLooksLikeWorkerFn = restoreLooksLike
		fakeHostOf(dp).lsofFn = restoreLsof
		fakeHostOf(dp).processUIDFn = restoreUID
	})

	// Case 1: port not owned by our pid at all.
	fakeHostOf(dp).lsofFn = func(port string) ([]byte, error) { return []byte(""), nil }
	fakeHostOf(dp).processUIDFn = func(pid int) (int, bool) { return os.Getuid(), true }
	if _, ok := dp.host.serveVerifiedOurs(dataDir, "127.0.0.1:8090"); ok {
		t.Error("want false: lsof reports nothing listening")
	}

	// Case 2: port owned by our pid, but uid lookup disagrees.
	fakeHostOf(dp).lsofFn = func(port string) ([]byte, error) { return []byte(fmt.Sprintf("%d\n", os.Getpid())), nil }
	fakeHostOf(dp).processUIDFn = func(pid int) (int, bool) { return os.Getuid() + 1, true }
	if _, ok := dp.host.serveVerifiedOurs(dataDir, "127.0.0.1:8090"); ok {
		t.Error("want false: process uid does not match this process's own uid")
	}

	// Case 3: both port ownership and uid check pass.
	fakeHostOf(dp).processUIDFn = func(pid int) (int, bool) { return os.Getuid(), true }
	gotPID, ok := dp.host.serveVerifiedOurs(dataDir, "127.0.0.1:8090")
	if !ok {
		t.Fatal("want true: pid file alive, port owned, uid matches")
	}
	if gotPID != os.Getpid() {
		t.Errorf("quickstartServeVerifiedOurs returned pid %d, want %d", gotPID, os.Getpid())
	}
}

// TestQuickstartServeVerifiedOursNoPidFileNoLaunchdMeansUnverified proves
// that with neither a live quickstart-serve.pid nor (implicitly, non-
// darwin/no plist) a launchd service, nothing is ever verified as ours.
func TestQuickstartServeVerifiedOursNoPidFileNoLaunchdMeansUnverified(t *testing.T) {
	dp := newTestDeps(t)
	dataDir := t.TempDir()
	if _, ok := dp.host.serveVerifiedOurs(dataDir, "127.0.0.1:8090"); ok {
		t.Error("want false: no pid file and (in this test environment) no matching launchd service")
	}
}

// TestLaunchctlServicePIDParsesOutput proves launchctlServicePID extracts
// a "pid = NNN" value from launchctl print's own output shape.
func TestLaunchctlServicePIDParsesOutput(t *testing.T) {
	dp := newTestDeps(t)
	orig := fakeHostOf(dp).launchctlFn
	t.Cleanup(func() { fakeHostOf(dp).launchctlFn = orig })

	fakeHostOf(dp).launchctlFn = func(args ...string) ([]byte, error) {
		return []byte("gui/501/dev.factoryd.serve = {\n\tactive count = 1\n\tpid = 4321\n\tstate = running\n}\n"), nil
	}
	pid, ok := hostcontrol.LaunchctlServicePID(dp, "gui/501/dev.factoryd.serve")
	if !ok {
		t.Fatal("want ok=true when output contains a pid line")
	}
	if pid != 4321 {
		t.Errorf("launchctlServicePID() = %d, want 4321", pid)
	}

	fakeHostOf(dp).launchctlFn = func(args ...string) ([]byte, error) {
		return nil, fmt.Errorf("not loaded")
	}
	if _, ok := hostcontrol.LaunchctlServicePID(dp, "gui/501/dev.factoryd.serve"); ok {
		t.Error("want ok=false when launchctl itself errors")
	}
}

// TestServeVerifiedOursAcceptsTheDataDirsOwnRecord: a serve started by hand
// has no pid file; the record it wrote in the data dir it serves names it.
// The record's pid is a candidate only for the address it recorded, and is
// held to the same port-ownership check as any other.
func TestServeVerifiedOursAcceptsTheDataDirsOwnRecord(t *testing.T) {
	dp := newTestDeps(t)
	t.Setenv("HOME", t.TempDir())
	dataDir := t.TempDir()
	const addr = "127.0.0.1:18477"
	if _, err := consolelink.RecordServeAddress(dataDir, addr); err != nil {
		t.Fatal(err)
	}
	restoreLsof, restoreUID := fakeHostOf(dp).lsofFn, fakeHostOf(dp).processUIDFn
	t.Cleanup(func() { fakeHostOf(dp).lsofFn, fakeHostOf(dp).processUIDFn = restoreLsof, restoreUID })
	fakeHostOf(dp).processUIDFn = func(pid int) (int, bool) { return os.Getuid(), true }
	fakeHostOf(dp).lsofFn = func(port string) ([]byte, error) { return []byte(fmt.Sprintf("%d\n", os.Getpid())), nil }

	if pid, ok := dp.host.serveVerifiedOurs(dataDir, addr); !ok || pid != os.Getpid() {
		t.Errorf("serveVerifiedOurs(recorded address) = %d, %v; want this process, true", pid, ok)
	}
	if _, ok := dp.host.serveVerifiedOurs(dataDir, "127.0.0.1:8090"); ok {
		t.Error("the record vouched for an address it does not name")
	}
	fakeHostOf(dp).lsofFn = func(port string) ([]byte, error) { return []byte("1\n"), nil }
	if _, ok := dp.host.serveVerifiedOurs(dataDir, addr); ok {
		t.Error("the record vouched for a port another process holds")
	}
}

// TestServeVerifiedOursIgnoresAnotherDataDirsLaunchAgent: the serve
// LaunchAgent is pinned to one data dir. For any other data dir its process
// is a foreign listener, which before this check got that data dir's token
// and left it with no console of its own.
func TestServeVerifiedOursIgnoresAnotherDataDirsLaunchAgent(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("launchd only")
	}
	dp := newTestDeps(t)
	t.Setenv("HOME", t.TempDir())
	serviceDir, otherDir := t.TempDir(), t.TempDir()
	plistPath, err := hostcontrol.ServePlistPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(plistPath), 0o755); err != nil {
		t.Fatal(err)
	}
	plist := "<plist><dict><key>ProgramArguments</key><array><string>factoryd</string><string>serve</string><string>-data-dir</string><string>" + serviceDir + "</string></array></dict></plist>"
	if err := os.WriteFile(plistPath, []byte(plist), 0o644); err != nil {
		t.Fatal(err)
	}
	restoreCtl, restoreLsof, restoreUID := fakeHostOf(dp).launchctlFn, fakeHostOf(dp).lsofFn, fakeHostOf(dp).processUIDFn
	t.Cleanup(func() {
		fakeHostOf(dp).launchctlFn, fakeHostOf(dp).lsofFn, fakeHostOf(dp).processUIDFn = restoreCtl, restoreLsof, restoreUID
	})
	fakeHostOf(dp).launchctlFn = func(args ...string) ([]byte, error) {
		return []byte(fmt.Sprintf("pid = %d\n", os.Getpid())), nil
	}
	fakeHostOf(dp).lsofFn = func(port string) ([]byte, error) { return []byte(fmt.Sprintf("%d\n", os.Getpid())), nil }
	fakeHostOf(dp).processUIDFn = func(pid int) (int, bool) { return os.Getuid(), true }

	if _, ok := dp.host.serveVerifiedOurs(serviceDir, "127.0.0.1:8090"); !ok {
		t.Error("the LaunchAgent's own data dir did not verify its serve")
	}
	if _, ok := dp.host.serveVerifiedOurs(otherDir, "127.0.0.1:8090"); ok {
		t.Error("another data dir took the LaunchAgent's serve for its own")
	}
}
