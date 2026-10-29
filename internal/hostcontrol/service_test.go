package hostcontrol

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestServiceManagerFor(t *testing.T) {
	for goos, want := range map[string]ServiceManager{"darwin": Launchd, "linux": Systemd, "windows": NoServiceManager, "freebsd": NoServiceManager} {
		if got := ServiceManagerFor(goos); got != want {
			t.Errorf("ServiceManagerFor(%q) = %q, want %q", goos, got, want)
		}
	}
}

// TestSystemdQuoteRoundTrips: every word install-service writes into a unit
// reads back as written, whatever it holds.
func TestSystemdQuoteRoundTrips(t *testing.T) {
	words := []string{"/home/op/.local/bin/factoryd", "worker", "-config", `/home/op/my "dir"/c.yml`, `a\b`, "50%", "$HOME", "with space", ""}
	quoted := make([]string, len(words))
	for i, w := range words {
		quoted[i] = SystemdQuote(w)
	}
	unit := "[Unit]\nDescription=x\n\n[Service]\nExecStart=" + strings.Join(quoted, " ") + "\nRestart=always\n"
	if got := SystemdExecStartArgs([]byte(unit)); !reflect.DeepEqual(got, words) {
		t.Errorf("round trip = %q, want %q", got, words)
	}
	if got := SystemdExecStartArgs([]byte("ExecStart=/usr/bin/factoryd serve  -data-dir /d\n")); !reflect.DeepEqual(got, []string{"/usr/bin/factoryd", "serve", "-data-dir", "/d"}) {
		t.Errorf("bare words = %q", got)
	}
	if got := SystemdExecStartArgs([]byte("[Service]\n")); got != nil {
		t.Errorf("no ExecStart = %q, want nil", got)
	}
}

func TestDefinitionPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	got, err := Systemd.DefinitionPath(WorkerService)
	if err != nil || got != filepath.Join(home, ".config", "systemd", "user", "factoryd-worker.service") {
		t.Errorf("systemd worker = %q, %v", got, err)
	}
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "xdg"))
	if got, _ := Systemd.DefinitionPath(ServeService); got != filepath.Join(home, "xdg", "systemd", "user", "factoryd-serve.service") {
		t.Errorf("systemd serve = %q", got)
	}
	if got, _ := Launchd.DefinitionPath(ServeService); got != filepath.Join(home, "Library", "LaunchAgents", ServeServiceLabel+".plist") {
		t.Errorf("launchd serve = %q", got)
	}
	if _, err := NoServiceManager.DefinitionPath(WorkerService); err == nil {
		t.Error("no service manager gave a path")
	}
}

func TestSystemdServicePIDNeedsAnActiveUnitWithAMainPID(t *testing.T) {
	dp := newFakeDeps(t)
	for _, c := range []struct {
		out  string
		err  error
		pid  int
		want bool
	}{
		{out: "ActiveState=active\nMainPID=4242\n", pid: 4242, want: true},
		{out: "MainPID=4242\nActiveState=activating\n"},
		{out: "ActiveState=active\nMainPID=0\n"},
		{out: "ActiveState=inactive\nMainPID=0\n"},
		{err: errors.New("Failed to connect to bus")},
	} {
		dp.systemctlFn = func(...string) ([]byte, error) { return []byte(c.out), c.err }
		pid, ok := SystemdServicePID(dp, "factoryd-worker.service")
		if ok != c.want || pid != c.pid {
			t.Errorf("%q: got (%d, %v), want (%d, %v)", c.out, pid, ok, c.pid, c.want)
		}
	}
}

func TestRestartServiceUsesTheManager(t *testing.T) {
	dp := newFakeDeps(t)
	var got []string
	dp.systemctlFn = func(args ...string) ([]byte, error) { got = append([]string{"systemctl"}, args...); return nil, nil }
	dp.launchctlFn = func(args ...string) ([]byte, error) { got = append([]string{"launchctl"}, args...); return nil, nil }

	dp.serviceManagerFn = func() ServiceManager { return Systemd }
	if err := RestartService(dp, ServeService); err != nil || strings.Join(got, " ") != "systemctl --user restart factoryd-serve.service" {
		t.Errorf("systemd: %q, %v", got, err)
	}
	dp.serviceManagerFn = func() ServiceManager { return Launchd }
	want := fmt.Sprintf("launchctl kickstart -k gui/%d/%s", os.Getuid(), WorkerServiceLabel)
	if err := RestartService(dp, WorkerService); err != nil || strings.Join(got, " ") != want {
		t.Errorf("launchd: %q, %v", got, err)
	}
	dp.serviceManagerFn = func() ServiceManager { return NoServiceManager }
	if err := RestartService(dp, WorkerService); err == nil {
		t.Error("restarted a service with no service manager")
	}
	dp.serviceManagerFn = func() ServiceManager { return Systemd }
	dp.systemctlFn = func(...string) ([]byte, error) { return []byte("Unit not found.\n"), errors.New("exit status 5") }
	if err := RestartService(dp, WorkerService); err == nil || !strings.Contains(err.Error(), "Unit not found.") {
		t.Errorf("a failed restart = %v, want systemctl's output in it", err)
	}
}

// TestStopLeavesASystemdSupervisedWorkerRunning: systemd would restart a
// worker stop killed, so stop names the unit and leaves it.
func TestStopLeavesASystemdSupervisedWorkerRunning(t *testing.T) {
	dp := newFakeDeps(t)
	dp.serviceManagerFn = func() ServiceManager { return Systemd }
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	dir := t.TempDir()
	unitPath, _ := Systemd.DefinitionPath(WorkerService)
	if err := os.MkdirAll(filepath.Dir(unitPath), 0o755); err != nil {
		t.Fatal(err)
	}
	unit := "ExecStart=" + SystemdQuote("/bin/factoryd") + " worker -config /c.yml -data-dir " + SystemdQuote(dir) + "\n"
	if err := os.WriteFile(unitPath, []byte(unit), 0o644); err != nil {
		t.Fatal(err)
	}
	dp.systemctlFn = func(...string) ([]byte, error) { return []byte("ActiveState=active\nMainPID=99999999\n"), nil }

	var b strings.Builder
	out := StopDataDir(dp, &b, dir, false, time.Now())
	if !out.QueueLive || !strings.Contains(b.String(), "worker ("+dir+"): supervised by systemd (factoryd-worker.service), left running") {
		t.Errorf("stop = %+v, said %q", out, b.String())
	}
	if !ServiceSupervises(dp, WorkerService, dir) || ServiceSupervises(dp, WorkerService, t.TempDir()) {
		t.Error("ServiceSupervises does not follow the unit's -data-dir")
	}
}
