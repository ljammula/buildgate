package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"buildgate/internal/hostcontrol"
	"buildgate/internal/sessionconfig"
)

// The session config's docker_context pins the Docker every factoryd
// process of that config reaches. pinDockerContext, called once by dispatch
// before any command runs, exports DOCKER_CONTEXT=<docker_context>, so every
// docker and docker compose call, every make or script factoryd runs and
// every worker or serve it starts uses that context, and the run record and
// the worker heartbeat (daemonheartbeat.LocalDockerEndpoint) record it. A
// config that sets none changes nothing: the docker CLI's current context
// applies, as before.

// dockerPinExempt are the commands that run without the pin: they reach no
// Docker, and are how an operator fixes or switches the config a refusal
// names.
var dockerPinExempt = map[string]bool{"help": true, "version": true, "use": true, "init-config": true}

func (realHost) lookupEnv(key string) (string, bool) { return os.LookupEnv(key) }
func (realHost) setenv(key, value string) error      { return os.Setenv(key, value) }

// configArgOf is the -config value in args ("" when none is given): the
// session config the command will load, resolved the way the command
// resolves it (loadConfigForPath).
func configArgOf(args []string) string {
	for i, a := range args {
		name, value, hasValue := strings.Cut(strings.TrimLeft(a, "-"), "=")
		if !strings.HasPrefix(a, "-") || name != "config" {
			continue
		}
		if hasValue {
			return value
		}
		if i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

// pinDockerContext applies the docker_context of the session config the
// command named (-config in args, else the default resolution) to this
// process. A config that does not load or validate is left to the command,
// which reports it.
func pinDockerContext(dp *deps, command string, args []string) error {
	if dockerPinExempt[command] {
		return nil
	}
	cfg, path, found, err := loadConfigForPath(configArgOf(args))
	if err != nil || !found {
		return nil
	}
	settings, err := cfg.ApplySettings(sessionconfig.DefaultSettings())
	if err != nil || settings.DockerContext == "" {
		return nil
	}
	return applyDockerContextPin(dp, settings.DockerContext, path)
}

// applyDockerContextPin exports DOCKER_CONTEXT=want, refusing when the
// environment already points the docker CLI elsewhere: DOCKER_HOST (which
// overrides every context) or a different DOCKER_CONTEXT.
func applyDockerContextPin(dp *deps, want, configPath string) error {
	if host, _ := dp.host.lookupEnv("DOCKER_HOST"); host != "" {
		return fmt.Errorf("DOCKER_HOST is set (%s), which overrides the docker context %s that %s pins (docker_context): unset DOCKER_HOST, or remove docker_context from that config", host, want, configPath)
	}
	if current, _ := dp.host.lookupEnv("DOCKER_CONTEXT"); current != "" && current != want {
		return fmt.Errorf("DOCKER_CONTEXT is %s, and %s pins docker_context %s: unset DOCKER_CONTEXT (or set it to %s), or change docker_context in that config", current, configPath, want, want)
	}
	return dp.host.setenv("DOCKER_CONTEXT", want)
}

// newConfigDockerContext is the docker_context a config written on this
// machine starts with: colima-buildgate on a Mac with colima on PATH (the
// VM `factoryd doctor -fix` creates), else none.
func newConfigDockerContext(dp *deps, goos string) string {
	if goos != "darwin" {
		return ""
	}
	if _, err := exec.LookPath(dp.docker.colimaBinary()); err != nil {
		return ""
	}
	return hostcontrol.DockerVMContext
}

// dockerVMMounts are the directories buildgate's VM shares: the data root
// (~/buildgate) read-write, and read-only the repositories doctor checks
// (repos, each by the top-level directory of $HOME it is in) and ~/code
// when it exists. Colima does not support a shared directory inside another,
// so $HOME itself is never shared.
func dockerVMMounts(repos ...string) hostcontrol.VMMounts {
	home, _ := os.UserHomeDir()
	mounts := hostcontrol.VMMounts{Writable: sessionconfig.DataRoot()}
	candidates := append([]string{filepath.Join(home, "code")}, repos...)
	for _, dir := range candidates {
		top, ok := topLevelHomeDir(home, dir)
		if !ok {
			continue
		}
		if info, err := os.Stat(top); err == nil && info.IsDir() {
			mounts.ReadOnly = append(mounts.ReadOnly, top)
		}
	}
	return mounts
}

// topLevelHomeDir is the directory directly under home that holds dir, when
// dir is under home.
func topLevelHomeDir(home, dir string) (string, bool) {
	if home == "" || dir == "" {
		return "", false
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", false
	}
	rel, err := filepath.Rel(home, abs)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return "", false
	}
	first, _, _ := strings.Cut(rel, string(filepath.Separator))
	if strings.HasPrefix(first, ".") {
		return "", false
	}
	return filepath.Join(home, first), true
}

// dockerContextMain is the hidden `factoryd docker-context` the Makefile and
// scripts/install-prereqs.sh run: it prints the docker context buildgate
// uses (the config's docker_context; with no config yet, the one a new
// config gets), and with -ensure-vm on a Mac creates or starts that
// context's Colima VM first.
func dockerContextMain(dp *deps, args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("docker-context", flag.ContinueOnError)
	configPath := flags.String("config", "", "internal: the session config whose docker_context is printed")
	ensureVM := flags.Bool("ensure-vm", false, "internal: on a Mac, create or start the Colima VM of the printed context")
	flags.SetOutput(stderr)
	if err := flags.Parse(args); err != nil {
		return err
	}
	want, err := resolvedDockerContext(dp, *configPath, runtime.GOOS)
	if err != nil {
		return err
	}
	profile, colima := colimaProfileOfContext(want)
	if *ensureVM && colima && runtime.GOOS == "darwin" {
		state, err := hostcontrol.EnsureDockerVM(dp, context.Background(), stderr, profile, dockerVMMounts())
		if err != nil {
			return err
		}
		fmt.Fprintln(stderr, state)
	}
	fmt.Fprintln(stdout, want)
	return nil
}

// resolvedDockerContext is the context buildgate uses with configPath: its
// docker_context, or with no config yet the one a new config gets, checked
// against the environment as the pin checks it. With neither it is
// DOCKER_CONTEXT as the environment has it ("" is the docker CLI's current
// context), so exporting the result changes nothing.
func resolvedDockerContext(dp *deps, configPath, goos string) (string, error) {
	cfg, path, found, err := loadConfigForPath(configPath)
	if err != nil {
		return "", fmt.Errorf("session config: %w", err)
	}
	want, source := newConfigDockerContext(dp, goos), "a new session config"
	if found {
		settings, err := cfg.ApplySettings(sessionconfig.DefaultSettings())
		if err != nil {
			return "", fmt.Errorf("session config: %w", err)
		}
		want, source = settings.DockerContext, path
	}
	if want == "" {
		current, _ := dp.host.lookupEnv("DOCKER_CONTEXT")
		return current, nil
	}
	return want, applyDockerContextPin(dp, want, source)
}

// colimaProfileOfContext is the Colima profile of a docker context name
// (colima-<profile>); buildgate never pins the default profile's `colima`.
func colimaProfileOfContext(name string) (string, bool) {
	profile, ok := strings.CutPrefix(name, "colima-")
	return profile, ok && profile != ""
}
