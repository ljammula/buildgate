package main

import (
	"context"
	"errors"
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
//
// The pin also exports pinnedDockerContextEnv with the same value, so a
// child factoryd (restart, upgrade, quickstart's worker and serve) can tell
// an inherited pin from a DOCKER_CONTEXT the operator exported: an inherited
// one gives way to the child's own config, which may pin another context or
// none.

// pinnedDockerContextEnv marks DOCKER_CONTEXT as set by a factoryd pin.
const pinnedDockerContextEnv = "FACTORYD_PINNED_DOCKER_CONTEXT"

// dockerPinExempt are the commands that run without the pin: they reach no
// Docker, and are how an operator fixes or switches the config a refusal
// names.
var dockerPinExempt = map[string]bool{"help": true, "version": true, "use": true, "init-config": true}

func (realHost) lookupEnv(key string) (string, bool) { return os.LookupEnv(key) }
func (realHost) setenv(key, value string) error      { return os.Setenv(key, value) }
func (realHost) unsetenv(key string) error           { return os.Unsetenv(key) }

// configArgOf is the -config value in args ("" when none is given): the
// session config the command will load, resolved the way the command
// resolves it (loadConfigForPath). args[0] is the command name unless it is
// a flag (a single-ticket run). Like the flag package, it stops at the first
// positional argument (a word that does not follow a flag) and at "--"; a
// flag is spelled with one or two dashes, and a -config value that starts
// with a dash is no value.
func configArgOf(args []string) string {
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		args = args[1:]
	}
	afterFlag := false
	for i, a := range args {
		if a == "--" || (!strings.HasPrefix(a, "-") && !afterFlag) {
			return ""
		}
		if !strings.HasPrefix(a, "-") {
			afterFlag = false
			continue
		}
		name := strings.TrimPrefix(strings.TrimPrefix(a, "-"), "-")
		name, value, hasValue := strings.Cut(name, "=")
		afterFlag = !hasValue
		if name != "config" {
			continue
		}
		if !hasValue && i+1 < len(args) {
			value = args[i+1]
		}
		if strings.HasPrefix(value, "-") {
			return ""
		}
		return value
	}
	return ""
}

// pinDockerContext applies the docker_context of the session config the
// command named (-config in args, else the default resolution) to this
// process: it pins it, or, when the config pins none, drops a pin inherited
// from a parent factoryd. A config that does not load or validate is left to
// the command, which reports it.
func pinDockerContext(dp *deps, command string, args []string) error {
	if dockerPinExempt[command] {
		return nil
	}
	cfg, path, found, err := loadConfigForPath(configArgOf(args))
	if err != nil {
		return nil
	}
	want := ""
	if found {
		settings, err := cfg.ApplySettings(sessionconfig.DefaultSettings())
		if err != nil {
			return nil
		}
		want = settings.DockerContext
	}
	if want == "" {
		if inheritedDockerPin(dp) {
			return errors.Join(dp.host.unsetenv("DOCKER_CONTEXT"), dp.host.unsetenv(pinnedDockerContextEnv))
		}
		return nil
	}
	return applyDockerContextPin(dp, want, path)
}

// inheritedDockerPin reports whether DOCKER_CONTEXT is a parent factoryd's
// pin (pinnedDockerContextEnv names the same context), not the operator's.
func inheritedDockerPin(dp *deps) bool {
	current, _ := dp.host.lookupEnv("DOCKER_CONTEXT")
	marker, _ := dp.host.lookupEnv(pinnedDockerContextEnv)
	return current != "" && current == marker
}

// applyDockerContextPin exports DOCKER_CONTEXT=want and its marker, refusing
// when the operator's environment already points the docker CLI elsewhere:
// DOCKER_HOST (which overrides every context) or a DOCKER_CONTEXT of their
// own naming another context. A parent factoryd's pin gives way.
func applyDockerContextPin(dp *deps, want, configPath string) error {
	if host, _ := dp.host.lookupEnv("DOCKER_HOST"); host != "" {
		return fmt.Errorf("DOCKER_HOST is set (%s), which overrides the docker context %s that %s pins (docker_context): unset DOCKER_HOST, or remove docker_context from that config", host, want, configPath)
	}
	if current, _ := dp.host.lookupEnv("DOCKER_CONTEXT"); current != "" && current != want && !inheritedDockerPin(dp) {
		return fmt.Errorf("DOCKER_CONTEXT is %s, and %s pins docker_context %s: unset DOCKER_CONTEXT (or set it to %s), or change docker_context in that config", current, configPath, want, want)
	}
	return errors.Join(dp.host.setenv("DOCKER_CONTEXT", want), dp.host.setenv(pinnedDockerContextEnv, want))
}

// newConfigDockerContext is the docker_context a config written on this
// machine starts with: colima-buildgate on a Mac with colima on PATH and no
// DOCKER_HOST (the VM `factoryd doctor -fix` creates), else none.
func newConfigDockerContext(dp *deps, goos string) string {
	if goos != "darwin" {
		return ""
	}
	// DOCKER_HOST names another Docker (Docker Desktop, OrbStack): a pin
	// would only be refused.
	if host, _ := dp.host.lookupEnv("DOCKER_HOST"); host != "" {
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
	if *ensureVM && runtime.GOOS == "darwin" {
		if err := ensureDockerContextVM(dp, want, stderr); err != nil {
			return err
		}
	}
	fmt.Fprintln(stdout, want)
	return nil
}

// ensureDockerContextVM is -ensure-vm: it creates or starts the Colima VM of
// a pinned colima-<profile>. With no pin it says the config follows the
// current context and how to pin, and starts colima's default profile only
// when it exists and is stopped (never creating one), after the same
// headroom checks; with DOCKER_HOST set it starts nothing.
func ensureDockerContextVM(dp *deps, want string, w io.Writer) error {
	ctx := context.Background()
	if profile, colima := colimaProfileOfContext(want); colima {
		state, err := hostcontrol.EnsureDockerVM(dp, ctx, w, profile, dockerVMMounts())
		if err == nil {
			fmt.Fprintln(w, state)
		}
		return err
	}
	if want != "" {
		return nil
	}
	fmt.Fprintln(w, "the session config sets no docker_context, so buildgate uses the docker CLI's current context: to give it its own Colima VM, add `docker_context: colima-buildgate` to the config and run `factoryd doctor -fix`")
	if host, _ := dp.host.lookupEnv("DOCKER_HOST"); host != "" {
		return nil
	}
	vm, found, err := hostcontrol.FindColimaVM(dp, ctx, "default")
	if err != nil || !found || vm.Running() {
		return nil
	}
	_, err = hostcontrol.StartDockerVM(dp, ctx, w, "default")
	return err
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
