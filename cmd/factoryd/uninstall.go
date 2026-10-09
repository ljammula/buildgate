package main

import (
	"bufio"
	"buildgate/internal/hostcontrol"
	"buildgate/internal/sandbox"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// uninstallComposeProject is the compose project name docker-compose.
// temporal.yml declares (`name: buildgate`), shared by `make install`'s
// temporal-up and factoryd's own autostart, so one `compose -p` call reaches
// the stack whichever started it.
const uninstallComposeProject = "buildgate"

// uninstallLocalRegistry is the container `make install` starts to push
// image digests through.
const uninstallLocalRegistry = "factoryd-local-registry"

// uninstallImages are the tags the Makefile's image targets build and push.
var uninstallImages = []string{
	"localhost:5050/buildgate-worker:local",
	"localhost:5050/factoryd-meter:local",
	"localhost:5050/factoryd-registry-proxy:local",
	"localhost:5050/buildgate-pifork:local",
	"localhost:5050/project-worker:local",
}

// uninstallSkillDirs are the skills directories `make install` writes the
// buildgate skill into, relative to $HOME.
var uninstallSkillDirs = []string{".agents/skills", ".claude/skills"}

const uninstallCommandTimeout = 2 * time.Minute

// uninstallGatewayStateDir is where the OpenShell gateway keeps its database,
// keys and stored credentials, on the Docker VM's own disk (not the host).
const uninstallGatewayStateDir = "/var/lib/openshell"

func newUninstallFlags() (flags *flag.FlagSet, yes, dryRun, purge, force *bool) {
	flags = flag.NewFlagSet("uninstall", flag.ContinueOnError)
	yes = flags.Bool("yes", false, "do not ask for confirmation (required when stdin is not a terminal)")
	dryRun = flags.Bool("dry-run", false, "print what would be removed and change nothing")
	purge = flags.Bool("purge", false, "also delete your session config (~/.config/factoryd), the default data directory (~/buildgate: requests, runs, evidence, logs) the Temporal volumes and the OpenShell gateway's state on the Docker VM. Irreversible")
	force = flags.Bool("force", false, "stop daemons even when a request is building (its build is cancelled)")
	plainFlagUsage(flags)
	return
}

// uninstallStep is one removal. The plan only contains steps whose target
// exists on this machine.
type uninstallStep struct {
	desc string
	run  func() error
}

// uninstallEnv carries everything uninstall touches, so tests point it at a
// temp HOME and a fake docker.
type uninstallEnv struct {
	home     string
	docker   string
	exePath  string
	purge    bool
	force    bool
	stop     func(args []string) error
	service  func() error
	out      io.Writer
	runDock  func(args ...string) error
	dockOut  func(args ...string) (string, error)
	xdgConfg string
}

func uninstallMain(dp *deps, args []string) error {
	flags, yes, dryRun, purge, force := newUninstallFlags()
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected positional argument %q", flags.Arg(0))
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolve home directory: %w", err)
	}
	exe, _ := os.Executable()
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	env := newUninstallEnv(dp, home, exe, os.Stdout)
	env.purge = *purge
	env.force = *force
	interactive := quickstartStdinIsInteractive(os.Stdin)
	return uninstallRun(env, *yes, *dryRun, interactive, os.Stdin)
}

func newUninstallEnv(dp *deps, home, exe string, out io.Writer) *uninstallEnv {
	env := &uninstallEnv{
		home:    home,
		docker:  dp.docker.dockerBinary(),
		exePath: exe,
		stop:    func(a0 []string) error { return stopKeepingColima(dp, a0) },
		service: func() error { return uninstallServiceMain(dp, nil) },
		out:     out,
	}
	env.xdgConfg = os.Getenv("XDG_CONFIG_HOME")
	if env.xdgConfg == "" {
		env.xdgConfg = filepath.Join(home, ".config")
	}
	env.runDock = func(args ...string) error {
		ctx, cancel := context.WithTimeout(context.Background(), uninstallCommandTimeout)
		defer cancel()
		out, err := exec.CommandContext(ctx, env.docker, args...).CombinedOutput()
		if err != nil {
			return fmt.Errorf("docker %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
		}
		return nil
	}
	env.dockOut = func(args ...string) (string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, env.docker, args...).Output()
		return strings.TrimSpace(string(out)), err
	}
	return env
}

func (e *uninstallEnv) dockerUsable() bool {
	_, err := e.dockOut("info", "--format", "{{.ID}}")
	return err == nil
}

func (e *uninstallEnv) composeProjectPresent(project string) bool {
	out, err := e.dockOut("compose", "-p", project, "ps", "-a", "-q")
	return err == nil && out != ""
}

func (e *uninstallEnv) containerPresent(name string) bool {
	err := e.runDock("container", "inspect", name)
	return err == nil
}

func (e *uninstallEnv) imagePresent(ref string) bool {
	return e.runDock("image", "inspect", ref) == nil
}

func (e *uninstallEnv) launchAgents() []string {
	var found []string
	for _, label := range []string{hostcontrol.WorkerServiceLabel, hostcontrol.ServeServiceLabel} {
		p := filepath.Join(e.home, "Library", "LaunchAgents", label+".plist")
		if _, err := os.Lstat(p); err == nil {
			found = append(found, p)
		}
	}
	return found
}

// skillDirs returns the buildgate skill directories that exist and are
// genuinely the installed skill: a real directory (not a symlink the operator
// pointed elsewhere) holding a SKILL.md.
func (e *uninstallEnv) skillDirs() []string {
	var found []string
	for _, rel := range uninstallSkillDirs {
		d := filepath.Join(e.home, rel, "buildgate")
		info, err := os.Lstat(d)
		if err != nil || !info.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(d, "SKILL.md")); err != nil {
			continue
		}
		found = append(found, d)
	}
	return found
}

// gatewayStatePurgeSteps deletes the gateway's state on the Docker VM, so a
// purge leaves no stored credentials behind. A throwaway container of the
// worker image (which has a shell) bind-mounts the VM directory and empties it;
// that image is removed by a later step, so this runs before it. A VM
// directory that does not exist is already clean. Without the worker image the
// step cannot run, so it prints the manual command instead.
func (e *uninstallEnv) gatewayStatePurgeSteps() []uninstallStep {
	helper := uninstallImages[0]
	if !e.imagePresent(helper) {
		return []uninstallStep{{
			desc: "NOTE: the gateway's state on the Docker VM (" + uninstallGatewayStateDir + ": database, keys, stored credentials) is not removed here; remove it yourself, e.g. `colima ssh -- sudo rm -rf " + uninstallGatewayStateDir + "`",
			run:  func() error { return nil },
		}}
	}
	return []uninstallStep{{
		desc: "DELETE the gateway's state on the Docker VM (" + uninstallGatewayStateDir + ": database, keys, stored credentials)",
		run: func() error {
			err := e.runDock("run", "--rm", "--user", "0", "--entrypoint", "sh",
				"--mount", "type=bind,source="+uninstallGatewayStateDir+",target=/state",
				helper, "-c", "rm -rf /state/* /state/.[!.]*")
			if err != nil && strings.Contains(err.Error(), "does not exist") {
				return nil
			}
			return err
		},
	}}
}

func (e *uninstallEnv) purgeTargets() []string {
	var found []string
	for _, p := range []string{filepath.Join(e.xdgConfg, "factoryd"), filepath.Join(e.home, "buildgate")} {
		if _, err := os.Lstat(p); err == nil {
			found = append(found, p)
		}
	}
	return found
}

// binaryLinks are the `factoryd` symlinks on PATH that resolve to exePath:
// the one `make install` puts in ~/.local/bin (scripts/install-finish.sh).
// Left behind, each would dangle once the binary is removed.
func (e *uninstallEnv) binaryLinks() []string {
	var links []string
	seen := map[string]bool{}
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		link := filepath.Join(dir, "factoryd")
		if dir == "" || seen[link] {
			continue
		}
		seen[link] = true
		info, err := os.Lstat(link)
		if err != nil || info.Mode()&os.ModeSymlink == 0 {
			continue
		}
		if resolved, err := filepath.EvalSymlinks(link); err == nil && resolved == e.exePath {
			links = append(links, link)
		}
	}
	return links
}

// binaryPresent reports whether exePath is a factoryd binary uninstall may
// remove: named exactly "factoryd" and not a go-build temp binary.
func (e *uninstallEnv) binaryPresent() bool {
	if e.exePath == "" || filepath.Base(e.exePath) != "factoryd" {
		return false
	}
	_, err := os.Stat(e.exePath)
	return err == nil
}

func (e *uninstallEnv) plan() []uninstallStep {
	var steps []uninstallStep
	steps = append(steps, uninstallStep{
		desc: "stop worker and serve for every profile (factoryd stop -all)",
		run: func() error {
			args := []string{"-all"}
			if e.force {
				args = append(args, "-force")
			}
			return e.stop(args)
		},
	})
	if agents := e.launchAgents(); len(agents) > 0 {
		steps = append(steps, uninstallStep{
			desc: "remove launchd service(s): " + strings.Join(agents, ", "),
			run:  e.service,
		})
	}
	dockerUp := e.dockerUsable()
	if dockerUp && e.composeProjectPresent(uninstallComposeProject) {
		desc := "remove the Temporal containers (docker compose -p " + uninstallComposeProject + " down)"
		args := []string{"compose", "-p", uninstallComposeProject, "down"}
		if e.purge {
			desc += " and its volumes (workflow history)"
			args = append(args, "-v")
		}
		steps = append(steps, uninstallStep{desc: desc, run: func() error { return e.runDock(args...) }})
	}
	if dockerUp && e.composeProjectPresent(hostcontrol.OpenShellProject) {
		steps = append(steps, uninstallStep{
			desc: "remove the OpenShell gateway and meter containers (docker compose -p " + hostcontrol.OpenShellProject + " down)",
			run:  func() error { return e.runDock("compose", "-p", hostcontrol.OpenShellProject, "down") },
		})
	}
	if dockerUp && e.containerPresent(uninstallLocalRegistry) {
		steps = append(steps, uninstallStep{
			desc: "remove the local image registry container (" + uninstallLocalRegistry + ")",
			run:  func() error { return e.runDock("rm", "-f", uninstallLocalRegistry) },
		})
	}
	if e.purge && dockerUp {
		steps = append(steps, e.gatewayStatePurgeSteps()...)
	}
	if dockerUp {
		// The images builds derived for a repository's toolchains carry a
		// tag per set of versions, so they are listed, not named.
		derived, _ := e.dockOut("images", "--format", "{{.Repository}}:{{.Tag}}", toolchainImageRepo)
		refs := append([]string(nil), uninstallImages...)
		for _, ref := range strings.Fields(derived) {
			if strings.HasPrefix(ref, toolchainImageRepo+":") {
				refs = append(refs, ref)
			}
		}
		for _, ref := range refs {
			if !e.imagePresent(ref) {
				continue
			}
			ref := ref
			steps = append(steps, uninstallStep{
				desc: "remove image " + ref,
				run:  func() error { return e.runDock("rmi", ref) },
			})
		}
	}
	for _, d := range e.skillDirs() {
		d := d
		steps = append(steps, uninstallStep{desc: "remove the buildgate skill " + d, run: func() error { return os.RemoveAll(d) }})
	}
	if e.purge {
		for _, p := range e.purgeTargets() {
			p := p
			steps = append(steps, uninstallStep{desc: "DELETE " + p + " (config / requests, runs, evidence, logs)", run: func() error { return sandbox.RemoveTree(p) }})
		}
	}
	if e.binaryPresent() {
		for _, link := range e.binaryLinks() {
			steps = append(steps, uninstallStep{desc: "remove the link " + link, run: func() error { return os.Remove(link) }})
		}
		exe := e.exePath
		steps = append(steps, uninstallStep{desc: "remove the binary " + exe, run: func() error { return os.Remove(exe) }})
	}
	return steps
}

func uninstallRun(env *uninstallEnv, yes, dryRun, interactive bool, stdin io.Reader) error {
	steps := env.plan()
	fmt.Fprintln(env.out, "factoryd uninstall will:")
	for _, s := range steps {
		fmt.Fprintln(env.out, "  -", s.desc)
	}
	if !env.purge {
		kept := env.purgeTargets()
		if len(kept) > 0 {
			fmt.Fprintf(env.out, "Keeping %s (pass -purge to delete).\n", strings.Join(kept, ", "))
		}
	}
	fmt.Fprintln(env.out, "Not touched: Docker/colima, Go, Node, gh, Homebrew packages, git config, the repo checkout, and anything under ~/.docker.")
	if dryRun {
		fmt.Fprintln(env.out, "Dry run: nothing changed.")
		return nil
	}
	if !yes {
		if !interactive {
			return errors.New("refusing to uninstall without confirmation: stdin is not a terminal; pass -yes")
		}
		prompt := "Proceed? [y/N] "
		if env.purge {
			prompt = "This also DELETES your config and all request/run data. Type 'purge' to proceed: "
		}
		fmt.Fprint(env.out, prompt)
		answer, _ := bufio.NewReader(stdin).ReadString('\n')
		answer = strings.ToLower(strings.TrimSpace(answer))
		if env.purge {
			if answer != "purge" {
				return errors.New("aborted")
			}
		} else if answer != "y" && answer != "yes" {
			return errors.New("aborted")
		}
	}
	var failed []string
	for _, s := range steps {
		if err := s.run(); err != nil {
			fmt.Fprintf(env.out, "FAILED  %s: %v\n", s.desc, err)
			failed = append(failed, s.desc)
			// Stopping the daemons failing (a request is building) must
			// not be followed by deleting what they are using.
			if strings.HasPrefix(s.desc, "stop ") {
				return fmt.Errorf("%s failed: %w (nothing else was removed; rerun with -force to cancel running builds)", s.desc, err)
			}
			continue
		}
		fmt.Fprintf(env.out, "done    %s\n", s.desc)
	}
	if len(failed) > 0 {
		return fmt.Errorf("%d step(s) failed; rerun `factoryd uninstall` to retry them", len(failed))
	}
	fmt.Fprintln(env.out, "Uninstalled. Reinstall with `make install` from a buildgate checkout.")
	return nil
}
