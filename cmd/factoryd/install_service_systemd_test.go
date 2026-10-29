package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"buildgate/internal/hostcontrol"
)

// systemdHost records every systemctl call and answers each as a working
// user manager with the given linger.
type systemdHost struct {
	calls  []string
	linger string
}

// systemdTestDeps is newTestDeps on a machine whose services systemd runs,
// with a temporary home and unit directory, and a reachable Temporal.
func systemdTestDeps(t *testing.T, linger string) (*deps, *systemdHost) {
	t.Helper()
	dp := newTestDeps(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("DOCKER_HOST", "")
	t.Setenv("DOCKER_CONTEXT", "")
	h := &systemdHost{linger: linger}
	fakeHostOf(dp).serviceManagerFn = func() hostcontrol.ServiceManager { return hostcontrol.Systemd }
	fakeHostOf(dp).systemctlFn = func(args ...string) ([]byte, error) {
		h.calls = append(h.calls, strings.Join(args, " "))
		return []byte("ok\n"), nil
	}
	fakeHostOf(dp).loginctlFn = func(args ...string) ([]byte, error) {
		return []byte("Linger=" + h.linger + "\n"), nil
	}
	fakeHostOf(dp).launchctlFn = func(...string) ([]byte, error) {
		t.Error("launchctl was run on a systemd machine")
		return nil, errors.New("no launchctl")
	}
	stubEnsureTemporal(dp, t, "localhost:7233")
	return dp, h
}

func writeServiceTestConfig(t *testing.T) string {
	t.Helper()
	configPath := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(configPath, []byte("sandbox_image: img@sha256:"+strings.Repeat("a", 64)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return configPath
}

func TestBuildSystemdWorkerUnit(t *testing.T) {
	t.Setenv("DOCKER_HOST", "unix:///run/user/1000/docker.sock")
	cfg := workerPlistConfig{
		BinaryPath:      "/home/op/.local/share/buildgate/factoryd",
		ConfigPath:      "/home/op/.config/factoryd/config.yml",
		DataDir:         "/home/op/build gate/100% data",
		HomeDir:         "/home/op",
		TemporalAddress: "localhost:7233",
	}
	unit := buildSystemdWorkerUnit(cfg, nil)
	if !bytes.Equal(unit, buildSystemdWorkerUnit(cfg, nil)) {
		t.Fatal("the unit is not deterministic")
	}
	want := []string{cfg.BinaryPath, "worker", "-config", cfg.ConfigPath, "-data-dir", cfg.DataDir, "-temporal-address", cfg.TemporalAddress}
	if got := hostcontrol.SystemdExecStartArgs(unit); !reflect.DeepEqual(got, want) {
		t.Errorf("ExecStart reads back as %q, want %q", got, want)
	}
	s := string(unit)
	for _, line := range []string{
		"Restart=always\n",
		"RestartSec=60\n",
		"UMask=0077\n",
		`Environment="HOME=/home/op"` + "\n",
		`Environment="DOCKER_HOST=unix:///run/user/1000/docker.sock"` + "\n",
		"StandardOutput=append:/home/op/build gate/100%% data/logs/queue-run.out.log\n",
		"StandardError=append:/home/op/build gate/100%% data/logs/queue-run.err.log\n",
		"[Install]\nWantedBy=default.target\n",
	} {
		if !strings.Contains(s, line) {
			t.Errorf("unit lacks %q:\n%s", line, s)
		}
	}
	for _, dir := range []string{"/home/op/.local/bin", "/home/op/go/bin", "/home/linuxbrew/.linuxbrew/bin", "/usr/bin", "/snap/bin"} {
		if !strings.Contains(s, dir) {
			t.Errorf("unit PATH lacks %s:\n%s", dir, s)
		}
	}
	if strings.Contains(s, "DOCKER_CONTEXT") {
		t.Errorf("an unset DOCKER_CONTEXT was written:\n%s", s)
	}
}

func TestBuildSystemdServeUnitCarriesNoToken(t *testing.T) {
	unit := buildSystemdServeUnit(servePlistConfig{
		BinaryPath:   "/usr/local/bin/factoryd",
		ConfigPath:   "/home/op/.config/factoryd/config.yml",
		DataDir:      "/home/op/buildgate/data",
		HomeDir:      "/home/op",
		AllowedHosts: []string{"host.docker.internal:8090"},
	}, nil)
	want := []string{"/usr/local/bin/factoryd", "serve", "-data-dir", "/home/op/buildgate/data", "-config", "/home/op/.config/factoryd/config.yml", "-allowed-host", "host.docker.internal:8090"}
	if got := hostcontrol.SystemdExecStartArgs(unit); !reflect.DeepEqual(got, want) {
		t.Errorf("ExecStart = %q, want %q", got, want)
	}
	if strings.Contains(strings.ToLower(string(unit)), "token") {
		t.Errorf("the serve unit names a token:\n%s", unit)
	}
}

func TestInstallServiceSystemdWritesEnablesAndStartsBothUnits(t *testing.T) {
	dp, h := systemdTestDeps(t, "yes")
	configPath := writeServiceTestConfig(t)
	dataDir := filepath.Join(t.TempDir(), "data")

	out := captureStdout(t, func() {
		if err := installServiceMain(dp, []string{"-config", configPath, "-data-dir", dataDir}); err != nil {
			t.Fatalf("install-service: %v", err)
		}
	})
	for _, svc := range []hostcontrol.Service{hostcontrol.WorkerService, hostcontrol.ServeService} {
		path, _ := hostcontrol.Systemd.DefinitionPath(svc)
		unit, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("%s not written: %v", svc.Unit, err)
		}
		if args := hostcontrol.SystemdExecStartArgs(unit); len(args) < 2 || args[1] != svc.Kind {
			t.Errorf("%s runs %q", svc.Unit, args)
		}
		if !strings.Contains(out, "installed "+path) {
			t.Errorf("output does not name %s:\n%s", path, out)
		}
	}
	want := []string{
		"--user show-environment",
		"--user daemon-reload",
		"--user enable factoryd-worker.service factoryd-serve.service",
		"--user restart factoryd-worker.service factoryd-serve.service",
	}
	if !reflect.DeepEqual(h.calls, want) {
		t.Errorf("systemctl calls = %q, want %q", h.calls, want)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(configPath), serveStableStartTokenFileName)); err != nil {
		t.Errorf("no stable start token was prepared: %v", err)
	}

	// The worker service is now what doctor and quickstart see.
	if c := doctorCheckWorkerService(context.Background(), dp, mustDefinitionPath(t, hostcontrol.WorkerService)); c.Err != nil || !strings.Contains(c.Name, "worker systemd service: present") {
		t.Errorf("doctor = %+v", c)
	}

	if err := installServiceMain(dp, []string{"-config", configPath, "-data-dir", dataDir}); err == nil || !strings.Contains(err.Error(), "-force") {
		t.Errorf("a second install without -force = %v, want a refusal naming -force", err)
	}
	h.calls = nil
	captureStdout(t, func() {
		if err := installServiceMain(dp, []string{"-config", configPath, "-data-dir", dataDir, "-force", "-no-serve"}); err != nil {
			t.Fatalf("install-service -force: %v", err)
		}
	})
	if got := strings.Join(h.calls, "\n"); !strings.Contains(got, "--user restart factoryd-worker.service") || strings.Contains(got, "factoryd-serve") {
		t.Errorf("-force -no-serve systemctl calls = %q", h.calls)
	}

	h.calls = nil
	captureStdout(t, func() {
		if err := uninstallServiceMain(dp, nil); err != nil {
			t.Fatalf("uninstall-service: %v", err)
		}
	})
	for _, svc := range []hostcontrol.Service{hostcontrol.WorkerService, hostcontrol.ServeService} {
		if _, err := os.Stat(mustDefinitionPath(t, svc)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s left after uninstall: %v", svc.Unit, err)
		}
	}
	if want := []string{"--user disable --now factoryd-worker.service", "--user disable --now factoryd-serve.service", "--user daemon-reload"}; !reflect.DeepEqual(h.calls, want) {
		t.Errorf("uninstall systemctl calls = %q, want %q", h.calls, want)
	}
}

func mustDefinitionPath(t *testing.T, svc hostcontrol.Service) string {
	t.Helper()
	path, err := hostcontrol.Systemd.DefinitionPath(svc)
	if err != nil {
		t.Fatal(err)
	}
	return path
}

// TestInstallServiceSystemdWarnsWithoutLinger: without linger the units run
// only while the user is logged in, as a LaunchAgent does, so they are
// installed, with a warning naming the one command that makes them start at
// boot. logind's error for a user that is neither logged in nor lingering
// reads as linger off.
func TestInstallServiceSystemdWarnsWithoutLinger(t *testing.T) {
	for name, loginctl := range map[string]func(...string) ([]byte, error){
		"linger off": func(...string) ([]byte, error) { return []byte("Linger=no\n"), nil },
		"logind does not know the user": func(...string) ([]byte, error) {
			return []byte("Failed to get user: User ID 1000 is not logged in or lingering"), errors.New("exit status 1")
		},
	} {
		t.Run(name, func(t *testing.T) {
			dp, h := systemdTestDeps(t, "yes")
			fakeHostOf(dp).loginctlFn = loginctl
			out := captureStdout(t, func() {
				if err := installServiceMain(dp, []string{"-config", writeServiceTestConfig(t), "-data-dir", t.TempDir()}); err != nil {
					t.Fatalf("install-service: %v", err)
				}
			})
			if !strings.Contains(out, "sudo loginctl enable-linger $USER") {
				t.Errorf("no linger warning:\n%s", out)
			}
			if _, err := os.Stat(mustDefinitionPath(t, hostcontrol.WorkerService)); err != nil {
				t.Errorf("the worker unit was not installed: %v", err)
			}
			if !strings.Contains(strings.Join(h.calls, ";"), "--user restart factoryd-worker.service") {
				t.Errorf("the units were not started: %q", h.calls)
			}
		})
	}
	dp, _ := systemdTestDeps(t, "yes")
	out := captureStdout(t, func() {
		if err := installServiceMain(dp, []string{"-config", writeServiceTestConfig(t), "-data-dir", t.TempDir()}); err != nil {
			t.Fatal(err)
		}
	})
	if strings.Contains(out, "linger") {
		t.Errorf("a lingering user was warned:\n%s", out)
	}
}

// TestInstallServiceSystemdRefusesWithoutAUserManager: a shell under su or
// sudo -u has no user manager to install into.
func TestInstallServiceSystemdRefusesWithoutAUserManager(t *testing.T) {
	dp, _ := systemdTestDeps(t, "yes")
	fakeHostOf(dp).systemctlFn = func(...string) ([]byte, error) {
		return []byte("Failed to connect to bus: No medium found"), errors.New("exit status 1")
	}
	err := installServiceMain(dp, []string{"-config", writeServiceTestConfig(t), "-data-dir", t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "not through su or sudo -u") {
		t.Fatalf("err = %v, want the user-manager refusal", err)
	}
}

func TestInstallServiceSystemdPrintWritesNothing(t *testing.T) {
	dp, h := systemdTestDeps(t, "no")
	out := captureStdout(t, func() {
		if err := installServiceMain(dp, []string{"-config", writeServiceTestConfig(t), "-data-dir", t.TempDir(), "-print"}); err != nil {
			t.Fatalf("-print: %v", err)
		}
	})
	for _, svc := range []hostcontrol.Service{hostcontrol.WorkerService, hostcontrol.ServeService} {
		if !strings.Contains(out, "# "+mustDefinitionPath(t, svc)+"\n[Unit]") {
			t.Errorf("-print lacks %s:\n%s", svc.Unit, out)
		}
	}
	if len(h.calls) != 0 {
		t.Errorf("-print ran systemctl: %q", h.calls)
	}
}

func TestInstallServiceRefusesWithNoServiceManager(t *testing.T) {
	dp := newTestDeps(t)
	err := installServiceMain(dp, []string{"-config", writeServiceTestConfig(t)})
	if err == nil || !strings.Contains(err.Error(), "no service manager") {
		t.Fatalf("err = %v, want a refusal naming the missing service manager", err)
	}
}

func TestDoctorCheckSystemdWorkerServiceNamesEachState(t *testing.T) {
	dp, _ := systemdTestDeps(t, "yes")
	unitPath := mustDefinitionPath(t, hostcontrol.WorkerService)
	if c := doctorCheckSystemdWorkerService(dp, unitPath); c.Err != nil || !strings.Contains(c.Name, "absent") {
		t.Errorf("absent = %+v", c)
	}
	binary := filepath.Join(t.TempDir(), "factoryd")
	if err := writeExecutable(binary, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(unitPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(unitPath, buildSystemdWorkerUnit(workerPlistConfig{BinaryPath: binary, ConfigPath: "/c.yml", DataDir: "/d", HomeDir: "/h"}, nil), 0o644); err != nil {
		t.Fatal(err)
	}
	for show, want := range map[string]string{
		"LoadState=loaded\nActiveState=active\n":    "present, loaded, running",
		"LoadState=loaded\nActiveState=failed\n":    "present, loaded, not running",
		"LoadState=not-found\nActiveState=inactive": "present, not loaded",
	} {
		fakeHostOf(dp).systemctlFn = func(...string) ([]byte, error) { return []byte(show), nil }
		if c := doctorCheckSystemdWorkerService(dp, unitPath); c.Err != nil || !strings.HasSuffix(c.Name, want) && !strings.Contains(c.Name, want+" (") {
			t.Errorf("%q: %+v, want %q", show, c, want)
		}
	}
	if err := os.Remove(binary); err != nil {
		t.Fatal(err)
	}
	if c := doctorCheckSystemdWorkerService(dp, unitPath); c.Err == nil || !strings.Contains(c.Fix, "install-service -force") {
		t.Errorf("a unit naming a removed binary = %+v, want a failure with the reinstall fix", c)
	}
}

// TestQuickstartEnsureDaemonRestartsMatchingSystemdServiceWhenRestartNeeded
// is the systemd twin of the launchd case: a unit draining this
// invocation's queue is restarted through systemctl once the config was
// rewritten, and left alone otherwise.
func TestQuickstartEnsureDaemonRestartsMatchingSystemdServiceWhenRestartNeeded(t *testing.T) {
	dp, h := systemdTestDeps(t, "yes")
	home := os.Getenv("HOME")
	dataDir, configPath := filepath.Join(home, "data"), filepath.Join(home, "config.yml")
	binary := filepath.Join(home, "factoryd")
	if err := writeExecutable(binary, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	unitPath := mustDefinitionPath(t, hostcontrol.WorkerService)
	if err := os.MkdirAll(filepath.Dir(unitPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(unitPath, buildSystemdWorkerUnit(workerPlistConfig{BinaryPath: binary, ConfigPath: configPath, DataDir: dataDir, HomeDir: home}, nil), 0o644); err != nil {
		t.Fatal(err)
	}
	fakeHostOf(dp).systemctlFn = func(args ...string) ([]byte, error) {
		h.calls = append(h.calls, strings.Join(args, " "))
		return []byte("LoadState=loaded\nActiveState=active\n"), nil
	}

	var out bytes.Buffer
	if err := hostcontrol.QuickstartEnsureDaemon(dp, &out, binary, configPath, dataDir, nil, false, ""); err != nil {
		t.Fatalf("no restart needed: %v", err)
	}
	if !strings.Contains(out.String(), "already managed by systemd") || strings.Contains(strings.Join(h.calls, ";"), "restart") {
		t.Errorf("no restart needed: said %q, ran %q", out.String(), h.calls)
	}

	out.Reset()
	if err := hostcontrol.QuickstartEnsureDaemon(dp, &out, binary, configPath, dataDir, nil, true, ""); err != nil {
		t.Fatalf("restart needed: %v", err)
	}
	if h.calls[len(h.calls)-1] != "--user restart factoryd-worker.service" || !strings.Contains(out.String(), "restarted it") {
		t.Errorf("restart needed: said %q, ran %q", out.String(), h.calls)
	}
}

func TestUpgradeRestartsSystemdServicesInsteadOfSpawning(t *testing.T) {
	dp, _ := systemdTestDeps(t, "yes")
	h := newUpgradeHarness(dp, t)
	dir := h.profiles.roots["default"]
	unit := "[Service]\nExecStart=" + hostcontrol.SystemdQuote("/x/factoryd") + " serve -data-dir " + hostcontrol.SystemdQuote(dir) + "\n"
	writeSessionConfig(t, mustDefinitionPath(t, hostcontrol.ServeService), unit)
	fakeHostOf(dp).systemctlFn = func(args ...string) ([]byte, error) {
		if args[len(args)-1] == hostcontrol.ServeService.Unit {
			return []byte("ActiveState=active\nMainPID=99\n"), nil
		}
		return []byte("ActiveState=inactive\nMainPID=0\n"), nil
	}

	out, err := h.run(dp, "-yes")
	if err != nil {
		t.Fatalf("upgrade: %v\n%s", err, out)
	}
	if !h.called("systemctl --user restart factoryd-serve.service") {
		t.Errorf("serve was not restarted through systemd: %v", h.calls)
	}
	if len(h.spawned) != 0 {
		t.Errorf("a systemd service must not be spawned: %v", h.spawned)
	}
	if !strings.Contains(out, "restart: serve ("+dir+"), systemd factoryd-serve.service") {
		t.Errorf("plan lacks the systemd serve:\n%s", out)
	}
}

// TestSystemdUnitEnvironmentKeepsDollarSigns: Environment= expands
// specifiers but not variables, so "%" is doubled there and "$" is not,
// unlike in ExecStart=.
func TestSystemdUnitEnvironmentKeepsDollarSigns(t *testing.T) {
	unit := string(buildSystemdUnit(systemdUnitConfig{
		Args:    []string{"/bin/factoryd", "a$b"},
		HomeDir: "/home/op",
		Env:     []string{"DOCKER_CONTEXT=ci$1%x"},
	}))
	if !strings.Contains(unit, `Environment="DOCKER_CONTEXT=ci$1%%x"`+"\n") {
		t.Errorf("environment line:\n%s", unit)
	}
	if !strings.Contains(unit, `ExecStart="/bin/factoryd" "a$$b"`+"\n") {
		t.Errorf("ExecStart line:\n%s", unit)
	}
}

func TestSystemdServicePATHPutsTheFoundToolsFirstOnce(t *testing.T) {
	got := systemdServicePATH("/home/op", []string{"/opt/docker/bin", "/usr/bin", "/opt/docker/bin"})
	if !strings.HasPrefix(got, "/opt/docker/bin:/usr/bin:/home/op/.local/bin:") || strings.Count(got, "/opt/docker/bin") != 1 || strings.Count(got, ":/usr/bin:") != 1 {
		t.Errorf("PATH = %q", got)
	}
}
