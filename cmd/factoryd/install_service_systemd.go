package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"buildgate/internal/hostcontrol"
)

// systemdServicePATH is the PATH a systemd user unit runs with: a user
// manager does not read the operator's shell profile, so the tools a build
// runs (gh, docker, git, python3, go) must be on this fixed list. It covers
// the user's own bin directories, Go's tarball install, the system
// directories and snap's.
func systemdServicePATH(home string) string {
	return strings.Join([]string{
		filepath.Join(home, ".local", "bin"),
		filepath.Join(home, "go", "bin"),
		"/usr/local/go/bin",
		"/usr/local/sbin", "/usr/local/bin", "/usr/sbin", "/usr/bin", "/sbin", "/bin",
		"/snap/bin",
	}, ":")
}

// systemdUnitConfig is every value buildSystemdUnit needs, already resolved.
type systemdUnitConfig struct {
	Description string
	// Args is the command line, binary first.
	Args    []string
	HomeDir string
	// Env is extra NAME=value entries (DOCKER_HOST, DOCKER_CONTEXT).
	Env       []string
	StdoutLog string
	StderrLog string
}

// buildSystemdUnit renders a user unit deterministically, every word quoted
// with hostcontrol.SystemdQuote. Like the LaunchAgents it restarts the
// process whenever it exits, 60 s apart (Docker may not be up yet at boot),
// starts with the user's manager (default.target), and creates files under
// umask 077: the logs can name paths and tickets another account has no
// reason to read. It carries no token: serve reads its start token from
// the 0600 file beside the session config.
func buildSystemdUnit(cfg systemdUnitConfig) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "[Unit]\nDescription=%s\n\n[Service]\nType=simple\n", cfg.Description)
	quoted := make([]string, len(cfg.Args))
	for i, a := range cfg.Args {
		quoted[i] = hostcontrol.SystemdQuote(a)
	}
	fmt.Fprintf(&b, "ExecStart=%s\n", strings.Join(quoted, " "))
	b.WriteString("Restart=always\nRestartSec=60\nUMask=0077\n")
	env := append([]string{"PATH=" + systemdServicePATH(cfg.HomeDir), "HOME=" + cfg.HomeDir}, cfg.Env...)
	for _, kv := range env {
		fmt.Fprintf(&b, "Environment=%s\n", hostcontrol.SystemdQuote(kv))
	}
	fmt.Fprintf(&b, "StandardOutput=append:%s\n", systemdPathValue(cfg.StdoutLog))
	fmt.Fprintf(&b, "StandardError=append:%s\n", systemdPathValue(cfg.StderrLog))
	b.WriteString("\n[Install]\nWantedBy=default.target\n")
	return []byte(b.String())
}

// systemdPathValue escapes a path given as a bare setting value, where only
// the specifier character is special.
func systemdPathValue(p string) string { return strings.ReplaceAll(p, "%", "%%") }

// systemdDockerEnv is the Docker endpoint this shell names, which the unit
// must name too: a user manager starts with none of the shell's variables.
func systemdDockerEnv() []string {
	var env []string
	for _, name := range []string{"DOCKER_HOST", "DOCKER_CONTEXT"} {
		if v := os.Getenv(name); v != "" {
			env = append(env, name+"="+v)
		}
	}
	return env
}

func buildSystemdWorkerUnit(cfg workerPlistConfig) []byte {
	return buildSystemdUnit(systemdUnitConfig{
		Description: "buildgate factoryd worker",
		Args:        []string{cfg.BinaryPath, "worker", "-config", cfg.ConfigPath, "-data-dir", cfg.DataDir, "-temporal-address", cfg.TemporalAddress},
		HomeDir:     cfg.HomeDir,
		Env:         systemdDockerEnv(),
		StdoutLog:   filepath.Join(cfg.DataDir, "logs", "queue-run.out.log"),
		StderrLog:   filepath.Join(cfg.DataDir, "logs", "queue-run.err.log"),
	})
}

func buildSystemdServeUnit(cfg servePlistConfig) []byte {
	return buildSystemdUnit(systemdUnitConfig{
		Description: "buildgate factoryd console (serve)",
		Args:        serveServiceArgs(cfg),
		HomeDir:     cfg.HomeDir,
		Env:         systemdDockerEnv(),
		StdoutLog:   filepath.Join(cfg.DataDir, "logs", "serve.out.log"),
		StderrLog:   filepath.Join(cfg.DataDir, "logs", "serve.err.log"),
	})
}

// systemdUnitFile is one unit install-service writes.
type systemdUnitFile struct {
	svc  hostcontrol.Service
	path string
	body []byte
}

// systemdServiceReady refuses, with the one step that fixes it, a machine
// whose user units would not run as installed: no user manager answers
// (a shell under su or sudo -u has none), or the user does not linger, so
// the units would start only at a login and stop at its end. Turning linger
// on needs an administrator's polkit grant, so it is named, never run.
func systemdServiceReady(dp *deps) error {
	if out, err := dp.host.systemctl("--user", "show-environment"); err != nil {
		return fmt.Errorf("install-service: no systemd user manager answers (%v: %s); log in as this user directly (ssh or a desktop session), not through su or sudo -u, and rerun", err, strings.TrimSpace(string(out)))
	}
	user := strconv.Itoa(os.Getuid())
	out, err := dp.host.loginctl("show-user", user, "--property=Linger")
	if err != nil {
		return fmt.Errorf("install-service: could not ask logind whether this user's services start at boot (%v: %s)", err, strings.TrimSpace(string(out)))
	}
	if hostcontrol.SystemdProperties(out)["Linger"] != "yes" {
		return errors.New("install-service: this user's services would start only at a login and stop at its end; turn on lingering with `sudo loginctl enable-linger $USER`, then rerun")
	}
	return nil
}

// installSystemdServices writes the worker unit, and the serve unit unless
// serve is nil, then enables and (re)starts them. Nothing is written when a
// check fails or a unit exists without force.
func installSystemdServices(dp *deps, worker workerPlistConfig, serve *servePlistConfig, requireTokens, force bool) error {
	if err := systemdServiceReady(dp); err != nil {
		return err
	}
	units := []systemdUnitFile{{svc: hostcontrol.WorkerService, body: buildSystemdWorkerUnit(worker)}}
	if serve != nil {
		units = append(units, systemdUnitFile{svc: hostcontrol.ServeService, body: buildSystemdServeUnit(*serve)})
	}
	for i := range units {
		path, err := hostcontrol.Systemd.DefinitionPath(units[i].svc)
		if err != nil {
			return err
		}
		if err := refuseExistingDefinition(path, force); err != nil {
			return err
		}
		units[i].path = path
	}
	if serve != nil {
		if err := prepareServeTokens(dp, serve.ConfigPath, requireTokens); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(filepath.Join(worker.DataDir, "logs"), 0o755); err != nil {
		return fmt.Errorf("create log directory: %w", err)
	}
	names := make([]string, len(units))
	for i, u := range units {
		if err := os.MkdirAll(filepath.Dir(u.path), 0o755); err != nil {
			return fmt.Errorf("create %s: %w", filepath.Dir(u.path), err)
		}
		if err := os.WriteFile(u.path, u.body, 0o644); err != nil {
			return fmt.Errorf("write %s: %w", u.path, err)
		}
		names[i] = u.svc.Unit
	}
	for _, args := range [][]string{{"--user", "daemon-reload"}, append([]string{"--user", "enable"}, names...), append([]string{"--user", "restart"}, names...)} {
		if out, err := dp.host.systemctl(args...); err != nil {
			return fmt.Errorf("systemctl %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
		}
	}
	for _, u := range units {
		fmt.Printf("installed %s, enabled and started it (systemctl --user)\n", u.path)
	}
	if serve != nil {
		printServeTokenNote(serve, requireTokens)
	}
	return nil
}

// printSystemdUnits is install-service -print under systemd: each unit
// under a comment naming where it would be written.
func printSystemdUnits(worker workerPlistConfig, serve *servePlistConfig) error {
	units := []systemdUnitFile{{svc: hostcontrol.WorkerService, body: buildSystemdWorkerUnit(worker)}}
	if serve != nil {
		units = append(units, systemdUnitFile{svc: hostcontrol.ServeService, body: buildSystemdServeUnit(*serve)})
	}
	for i, u := range units {
		path, err := hostcontrol.Systemd.DefinitionPath(u.svc)
		if err != nil {
			return err
		}
		if i > 0 {
			fmt.Println()
		}
		fmt.Printf("# %s\n%s", path, u.body)
	}
	return nil
}

// uninstallSystemdServices stops and disables both units (best-effort: they
// may never have been installed) and removes their files.
func uninstallSystemdServices(dp *deps) error {
	var firstErr error
	for _, svc := range []hostcontrol.Service{hostcontrol.WorkerService, hostcontrol.ServeService} {
		if out, err := dp.host.systemctl("--user", "disable", "--now", svc.Unit); err != nil {
			fmt.Printf("systemctl --user disable --now %s: %v: %s\n", svc.Unit, err, strings.TrimSpace(string(out)))
		}
		path, err := hostcontrol.Systemd.DefinitionPath(svc)
		if err != nil {
			return err
		}
		if err := removeServiceDefinition(path); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if out, err := dp.host.systemctl("--user", "daemon-reload"); err != nil {
		fmt.Printf("systemctl --user daemon-reload: %v: %s\n", err, strings.TrimSpace(string(out)))
	}
	return firstErr
}

// doctorCheckSystemdWorkerService is doctorCheckWorkerService under
// systemd: the unit absent, present but not active, or active; the one
// failure is a unit naming a binary that is gone.
func doctorCheckSystemdWorkerService(dp *deps, unitPath string) doctorCheck {
	unit, err := os.ReadFile(unitPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return doctorCheck{Name: "worker systemd service: absent (run `factoryd install-service` to enable)"}
		}
		return doctorCheck{Name: "worker systemd service", Err: fmt.Errorf("read %s: %w", unitPath, err)}
	}
	if args := hostcontrol.SystemdExecStartArgs(unit); len(args) > 0 {
		if _, statErr := os.Stat(args[0]); statErr != nil {
			return doctorCheck{Name: "worker systemd service", Err: fmt.Errorf("unit at %s points at %s, which no longer exists", unitPath, args[0]),
				Fix: "reinstall with `factoryd install-service -force`"}
		}
	}
	out, err := dp.host.systemctl("--user", "show", "--property=LoadState,ActiveState", hostcontrol.WorkerService.Unit)
	props := hostcontrol.SystemdProperties(out)
	if err != nil || props["LoadState"] != "loaded" {
		return doctorCheck{Name: "worker systemd service: present, not loaded (run `factoryd install-service -force` to load it)"}
	}
	if props["ActiveState"] == "active" {
		return doctorCheck{Name: "worker systemd service: present, loaded, running"}
	}
	return doctorCheck{Name: "worker systemd service: present, loaded, not running"}
}

// The real systemd and logind commands.
func (impl realHost) systemctl(args ...string) ([]byte, error) {
	return exec.Command("systemctl", args...).CombinedOutput()
}

func (impl realHost) loginctl(args ...string) ([]byte, error) {
	return exec.Command("loginctl", args...).CombinedOutput()
}

// serviceManager is this OS's: launchd on macOS, systemd on Linux.
func (impl realHost) serviceManager() hostcontrol.ServiceManager {
	return hostcontrol.ServiceManagerFor(runtime.GOOS)
}
