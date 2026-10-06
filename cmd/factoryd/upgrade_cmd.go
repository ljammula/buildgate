package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"buildgate/internal/consolelink"
	"buildgate/internal/daemonheartbeat"
	"buildgate/internal/hostcontrol"
	"buildgate/internal/sanitize"
	"buildgate/internal/sessionconfig"
	"buildgate/internal/spinner"
)

// upgradeCmd is one external command `upgrade` runs.
type upgradeCmd struct {
	Dir    string
	Env    []string  // appended to the process environment
	Stream io.Writer // when set, output is also written here as it arrives
	Name   string
	Args   []string
}

// runUpgradeCommand is the single seam every external command of `upgrade` (git,
// make, go, launchctl, the installed factoryd) goes through, so tests fake
// them all and never touch a real checkout, build or launchd. It returns the
// combined output.
func (impl realHost) runUpgradeCommand(c upgradeCmd) ([]byte, error) {
	cmd := exec.Command(c.Name, c.Args...)
	cmd.Dir = c.Dir
	cmd.Env = append(os.Environ(), c.Env...)
	var buf bytes.Buffer
	var out io.Writer = &buf
	if c.Stream != nil {
		out = io.MultiWriter(&buf, c.Stream)
	}
	cmd.Stdout, cmd.Stderr = out, out
	err := cmd.Run()
	return buf.Bytes(), err
}

// upgradeWaitInterval is how often -wait re-checks for active requests.
var upgradeWaitInterval = 10 * time.Second

var (
	upgradeReleaseTagPattern = regexp.MustCompile(`^m[0-9]`)
	upgradeModulePattern     = regexp.MustCompile(`(?m)^module buildgate\s*$`)
)

type upgradeFlags struct {
	to     *string
	source *string
	yes    *bool
	wait   *bool
}

func newUpgradeFlags() (*flag.FlagSet, upgradeFlags) {
	flags := flag.NewFlagSet("upgrade", flag.ContinueOnError)
	var f upgradeFlags
	f.to = flags.String("to", "", "release tag or git ref to upgrade to; empty means the newest release tag (m<N>...)")
	f.source = flags.String("source", "", "buildgate checkout to build from; empty means the active profile's image_source_root")
	f.yes = flags.Bool("yes", false, "do not ask for confirmation (required when stdin is not a terminal)")
	f.wait = flags.Bool("wait", false, "when a request is building, wait until every profile's worker is between requests instead of refusing")
	plainFlagUsage(flags)
	return flags, f
}

// upgradeMain implements `factoryd upgrade`: drain, stop, check out the
// release, make install, re-point every profile's images, restart what was
// running, refresh the agent skill.
func upgradeMain(dp *deps, args []string) error {
	return upgradeRun(dp, args, os.Stdin, os.Stdout, quickstartStdinIsInteractive(os.Stdin), spinner.IsTerminal(os.Stdout))
}

// upgradeProcess is one worker or serve that was running for a data dir.
type upgradeProcess struct {
	Kind       string // "worker" or "serve"
	DataDir    string
	Launchd    bool
	Label      string
	ServeAddr  string
	ConfigPath string
	// TemporalAddress is the address a worker serves, restarted with.
	TemporalAddress string
}

func (p upgradeProcess) how() string {
	if p.Launchd {
		return "launchd " + p.Label
	}
	return "detached"
}

// upgradeState is what a failed upgrade needs to say about the machine.
type upgradeState struct {
	source     string
	previous   string
	target     string
	targetSHA  string
	stopped    []upgradeProcess
	checkedOut bool
}

func upgradeRun(dp *deps, args []string, in io.Reader, w io.Writer, interactive, tty bool) error {
	flags, f := newUpgradeFlags()
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() > 0 {
		return fmt.Errorf("upgrade takes no arguments, got %q", flags.Arg(0))
	}
	if !interactive && !*f.yes {
		return errors.New("upgrade asks before it stops anything, and stdin is not a terminal; pass -yes to go ahead without asking")
	}

	profiles, err := loadProfiles()
	if err != nil {
		return err
	}
	source, err := upgradeSourceCheckout(*f.source, profiles)
	if err != nil {
		return err
	}
	if err := upgradeCheckSource(dp, source); err != nil {
		return err
	}
	if out, err := upgradeGit(dp, source, "fetch", "--tags", "origin"); err != nil {
		return fmt.Errorf("git fetch --tags origin in %s failed: %s", source, sanitize.Line(hostcontrol.LastLine(string(out))))
	}
	target, err := upgradeTarget(dp, source, *f.to)
	if err != nil {
		return err
	}
	targetSHA, err := upgradeRevParse(dp, source, target)
	if err != nil {
		return fmt.Errorf("target %q does not resolve to a commit in %s: %w", target, source, err)
	}
	binary, err := upgradeInstalledBinary(dp, source)
	if err != nil {
		return err
	}
	current := upgradeProbeVersion(dp, source, binary)
	if current == targetSHA {
		fmt.Fprintf(w, "already on %s (%s)\n", target, targetSHA)
		return nil
	}
	if current == "" {
		current = "unknown"
	}

	dirs, _ := distinctDataDirs(profiles)
	running := upgradeRunning(dp, profiles, dirs, time.Now())
	skills := upgradeSkillTargets()
	upgradePrintPlan(w, source, current, target, targetSHA, upgradeCommitCount(dp, source, current, targetSHA), profiles, running, skills)

	if !*f.yes {
		fmt.Fprint(w, "Proceed? [y/N] ")
		line, _ := bufio.NewReader(in).ReadString('\n')
		if a := strings.ToLower(strings.TrimSpace(line)); a != "y" && a != "yes" {
			return errors.New("upgrade cancelled; nothing was changed")
		}
	}

	if err := upgradeDrain(w, dirs, *f.wait, tty); err != nil {
		return err
	}

	state := &upgradeState{source: source, target: target, targetSHA: targetSHA}
	if state.previous, err = upgradeRevParse(dp, source, "HEAD"); err != nil {
		return fmt.Errorf("read %s's current commit: %w", source, err)
	}
	if err := upgradeStop(dp, w, dirs, running, state); err != nil {
		return upgradeFailure(w, state, err, "")
	}

	if out, err := upgradeGit(dp, source, "checkout", "--detach", targetSHA); err != nil {
		return upgradeFailure(w, state, fmt.Errorf("git checkout %s failed", target), string(out))
	}
	state.checkedOut = true
	fmt.Fprintf(w, "installing %s: make -C %s install\n", target, source)
	if out, err := dp.host.runUpgradeCommand(upgradeCmd{Name: "make", Args: []string{"-C", source, "install"}, Env: []string{"FACTORYD_CONFIG="}, Stream: w}); err != nil {
		return upgradeFailure(w, state, fmt.Errorf("make install failed: %w", err), string(out))
	}
	if got := upgradeProbeVersion(dp, source, binary); got != targetSHA {
		return upgradeFailure(w, state, fmt.Errorf("%s reports version %q after install, want %q", binary, got, targetSHA), "")
	}

	restarted := upgradeRestart(dp, w, binary, running)
	refreshed := upgradeRefreshSkills(dp, w, binary, skills)
	upgradePrintSummary(w, targetSHA, target, profiles, restarted, refreshed)
	return nil
}

// upgradeSourceCheckout is -source, else the active profile's
// image_source_root.
func upgradeSourceCheckout(flagValue string, profiles []profileInfo) (string, error) {
	if flagValue != "" {
		return filepath.Abs(flagValue)
	}
	active := activeProfileName()
	if active == "" {
		active = sessionconfig.DefaultProfile
	}
	for _, p := range profiles {
		if p.Name == active && p.Config != nil && p.Config.ImageSourceRoot != nil && *p.Config.ImageSourceRoot != "" {
			return *p.Config.ImageSourceRoot, nil
		}
	}
	return "", fmt.Errorf("no source checkout: profile %q records no image_source_root; pass -source <buildgate checkout>", active)
}

// upgradeCheckSource refuses a source that is not a clean buildgate checkout.
func upgradeCheckSource(dp *deps, source string) error {
	mod, err := os.ReadFile(filepath.Join(source, "go.mod"))
	if err != nil || !upgradeModulePattern.Match(mod) {
		return fmt.Errorf("%s is not a buildgate checkout (no go.mod naming `module buildgate`); pass -source <buildgate checkout>", source)
	}
	if _, err := upgradeGit(dp, source, "rev-parse", "--git-dir"); err != nil {
		return fmt.Errorf("%s is not a git checkout; pass -source <buildgate checkout>", source)
	}
	out, err := upgradeGit(dp, source, "status", "--porcelain", "--untracked-files=no")
	if err != nil {
		return fmt.Errorf("git status in %s failed: %s", source, sanitize.Line(hostcontrol.LastLine(string(out))))
	}
	if strings.TrimSpace(string(out)) != "" {
		return fmt.Errorf("%s has uncommitted changes to tracked files; commit or stash them (or pass -source for a clean checkout), then rerun", source)
	}
	return nil
}

func upgradeGit(dp *deps, source string, args ...string) ([]byte, error) {
	return dp.host.runUpgradeCommand(upgradeCmd{Dir: source, Name: "git", Args: append([]string{"-C", source}, args...)})
}

// upgradeTarget is -to, else the newest release tag.
func upgradeTarget(dp *deps, source, flagValue string) (string, error) {
	if flagValue != "" {
		return flagValue, nil
	}
	out, err := upgradeGit(dp, source, "tag", "--sort=-creatordate")
	if err != nil {
		return "", fmt.Errorf("git tag in %s failed: %s", source, sanitize.Line(hostcontrol.LastLine(string(out))))
	}
	for _, tag := range strings.Fields(string(out)) {
		if upgradeReleaseTagPattern.MatchString(tag) {
			return tag, nil
		}
	}
	return "", fmt.Errorf("%s has no release tag (m<N>...); pass -to <tag|ref>", source)
}

// upgradeRevParse resolves ref to the 12-character commit sha `factoryd
// version` prints.
func upgradeRevParse(dp *deps, source, ref string) (string, error) {
	out, err := upgradeGit(dp, source, "rev-parse", "--short=12", ref+"^{commit}")
	if err != nil {
		return "", errors.New(sanitize.Line(hostcontrol.LastLine(string(out))))
	}
	return strings.TrimSpace(string(out)), nil
}

func upgradeCommitCount(dp *deps, source, from, to string) string {
	out, err := upgradeGit(dp, source, "rev-list", "--count", from+".."+to)
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(string(out))
}

// upgradeInstalledBinary is where `make install` puts factoryd: GOBIN, else
// the first GOPATH entry's bin.
func upgradeInstalledBinary(dp *deps, source string) (string, error) {
	goEnv := func(name string) string {
		out, err := dp.host.runUpgradeCommand(upgradeCmd{Dir: source, Name: "go", Args: []string{"env", name}})
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(out))
	}
	if bin := goEnv("GOBIN"); bin != "" {
		return filepath.Join(bin, "factoryd"), nil
	}
	if gopath := goEnv("GOPATH"); gopath != "" {
		return filepath.Join(filepath.SplitList(gopath)[0], "bin", "factoryd"), nil
	}
	return "", errors.New("cannot find where `make install` puts factoryd: `go env GOBIN` and `go env GOPATH` are both empty")
}

// upgradeProbeVersion runs `<binary> version` and returns the sha it prints,
// or "" when it cannot.
func upgradeProbeVersion(dp *deps, source, binary string) string {
	out, err := dp.host.runUpgradeCommand(upgradeCmd{Dir: source, Name: binary, Args: []string{"version"}})
	if err != nil {
		return ""
	}
	fields := strings.Fields(string(out))
	if len(fields) == 0 {
		return ""
	}
	return fields[len(fields)-1]
}

// upgradeRunning lists what runs for each data dir, with how it was started.
func upgradeRunning(dp *deps, profiles []profileInfo, dirs []string, now time.Time) []upgradeProcess {
	active := activeProfileName()
	var out []upgradeProcess
	for _, dir := range dirs {
		configPath := ""
		for _, p := range profiles {
			if p.Config == nil || !p.HasDataDir() || p.DataDir() != dir {
				continue
			}
			if configPath == "" || p.Name == active {
				configPath = p.Path
			}
		}
		if launchd := hostcontrol.LaunchdSupervises(dp, hostcontrol.WorkerServiceLabel, hostcontrol.WorkerPlistPath, dir); launchd || len(hostcontrol.WorkerPIDs(dp, dir, now)) > 0 {
			// A running process that is not a worker (an old worker)
			// has no Temporal address; it is replaced by a worker.
			workerAddr := ""
			if hb, err := daemonheartbeat.Read(workerHeartbeatPath(dir)); err == nil && hb.Worker() {
				workerAddr = hb.TemporalAddress
			}
			out = append(out, upgradeProcess{Kind: "worker", DataDir: dir, Launchd: launchd, Label: hostcontrol.WorkerServiceLabel, ConfigPath: configPath, TemporalAddress: workerAddr})
		}
		if launchd := hostcontrol.LaunchdSupervises(dp, hostcontrol.ServeServiceLabel, hostcontrol.ServePlistPath, dir); launchd || len(hostcontrol.ServePIDs(dp, dir)) > 0 {
			addr := consolelink.ServeAddress(dir)
			if addr == "" {
				addr = consolelink.DefaultServeAddr
			}
			out = append(out, upgradeProcess{Kind: "serve", DataDir: dir, Launchd: launchd, Label: hostcontrol.ServeServiceLabel, ServeAddr: addr, ConfigPath: configPath})
		}
	}
	return out
}

// upgradeSkillTargets are the skill parent directories to refresh: those
// holding a real (not symlinked) buildgate directory, since install-skill
// refuses a symlinked target.
func upgradeSkillTargets() (dirs []string) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	for _, parent := range []string{filepath.Join(home, ".agents", "skills"), filepath.Join(home, ".claude", "skills")} {
		if fi, err := os.Lstat(filepath.Join(parent, "buildgate")); err == nil && fi.IsDir() {
			dirs = append(dirs, parent)
		}
	}
	return dirs
}

func upgradePrintPlan(w io.Writer, source, current, target, targetSHA, commits string, profiles []profileInfo, running []upgradeProcess, skills []string) {
	fmt.Fprintf(w, "upgrade: %s -> %s (%s, %s commits)\n", current, targetSHA, target, commits)
	fmt.Fprintf(w, "  source:  %s\n", source)
	fmt.Fprintf(w, "  images:  re-pointed for %s\n", upgradeProfileNames(profiles))
	if len(running) == 0 {
		fmt.Fprintln(w, "  restart: nothing is running")
	}
	for _, p := range running {
		fmt.Fprintf(w, "  restart: %s (%s), %s\n", p.Kind, p.DataDir, p.how())
	}
	if len(skills) == 0 {
		fmt.Fprintln(w, "  skill:   no installed skill to refresh")
	}
	for _, d := range skills {
		fmt.Fprintf(w, "  skill:   refresh %s\n", filepath.Join(d, "buildgate"))
	}
}

func upgradeProfileNames(profiles []profileInfo) string {
	var names []string
	for _, p := range profiles {
		if p.LoadErr == nil {
			names = append(names, p.Name)
		}
	}
	if len(names) == 0 {
		return "no profile"
	}
	return strings.Join(names, ", ")
}

// upgradeActive lists "<request> (<data dir>)" for each request a worker or
// worker is running a job for.
func upgradeActive(dirs []string, now time.Time) []string {
	var out []string
	for _, dir := range dirs {
		ids, _ := daemonheartbeat.WorkerActiveRequests(dir, now)
		for _, id := range ids {
			out = append(out, fmt.Sprintf("%s (%s)", sanitize.Line(id), dir))
		}
	}
	sort.Strings(out)
	return out
}

// upgradeDrain refuses while a request is building, or with -wait polls until
// none is. It never interrupts a build.
func upgradeDrain(w io.Writer, dirs []string, wait, tty bool) error {
	active := upgradeActive(dirs, time.Now())
	if len(active) == 0 {
		return nil
	}
	if !wait {
		return fmt.Errorf("a request is building: %s; retry after it reaches a gate (`factoryd watch <request>`), or pass -wait to wait for it", strings.Join(active, ", "))
	}
	sp := spinner.New(w, tty)
	sp.Start("waiting for " + strings.Join(active, ", ") + " to reach a gate")
	defer sp.Stop("")
	for len(active) > 0 {
		time.Sleep(upgradeWaitInterval)
		if active = upgradeActive(dirs, time.Now()); len(active) > 0 {
			sp.Set("waiting for " + strings.Join(active, ", ") + " to reach a gate")
		}
	}
	return nil
}

// upgradeStop stops every detached worker and serve. launchd services keep
// running: the new binary lands at the same path and a kickstart re-execs it.
func upgradeStop(dp *deps, w io.Writer, dirs []string, running []upgradeProcess, state *upgradeState) error {
	for _, p := range running {
		if !p.Launchd {
			state.stopped = append(state.stopped, p)
		}
	}
	for _, dir := range dirs {
		out := hostcontrol.StopDataDir(dp, w, dir, false, time.Now())
		if out.Refused {
			return fmt.Errorf("a request started building in %s while stopping", dir)
		}
		if out.Failed {
			return fmt.Errorf("could not stop the processes of %s", dir)
		}
	}
	return nil
}

// upgradeFailure reports exactly what state the machine is in and how to get
// back, then returns the error.
func upgradeFailure(w io.Writer, state *upgradeState, cause error, output string) error {
	fmt.Fprintf(w, "\nupgrade failed: %v\n", cause)
	if tail := upgradeTail(output, 15); tail != "" {
		fmt.Fprintf(w, "last output:\n%s\n", tail)
	}
	fmt.Fprintln(w, "state:")
	if len(state.stopped) == 0 {
		fmt.Fprintln(w, "  no process was stopped")
	}
	for _, p := range state.stopped {
		fmt.Fprintf(w, "  %s (%s) is stopped\n", p.Kind, p.DataDir)
	}
	if state.checkedOut {
		fmt.Fprintf(w, "  %s is checked out at %s (%s)\n", state.source, state.target, state.targetSHA)
	} else {
		fmt.Fprintf(w, "  %s is still at %s\n", state.source, state.previous)
	}
	fmt.Fprintf(w, "to restore the previous release: git -C %s checkout %s && make -C %s install\n", state.source, state.previous, state.source)
	start := "`factoryd worker`"
	for _, p := range state.stopped {
		if p.Kind == "worker" && p.TemporalAddress != "" {
			start = fmt.Sprintf("`factoryd worker -temporal-address %s`", p.TemporalAddress)
		}
	}
	fmt.Fprintf(w, "then start what was stopped again with %s and `factoryd serve` (or `factoryd quickstart`).\n", start)
	return cause
}

func upgradeTail(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

// upgradeRestart brings back what was running with the newly installed
// binary and returns "kind (dir): pid N" descriptions.
func upgradeRestart(dp *deps, w io.Writer, binary string, running []upgradeProcess) []string {
	var out []string
	for _, p := range running {
		switch {
		case p.Launchd:
			domain := fmt.Sprintf("gui/%d/%s", os.Getuid(), p.Label)
			if o, err := dp.host.runUpgradeCommand(upgradeCmd{Name: "launchctl", Args: []string{"kickstart", "-k", domain}}); err != nil {
				fmt.Fprintf(w, "%s (%s): `launchctl kickstart -k %s` failed: %s\n", p.Kind, p.DataDir, domain, sanitize.Line(hostcontrol.LastLine(string(o))))
				continue
			}
			out = append(out, fmt.Sprintf("%s (%s): restarted by launchd", p.Kind, p.DataDir))
		case p.Kind == "worker":
			pidPath := filepath.Join(p.DataDir, "quickstart-queue-run.pid")
			addr := p.TemporalAddress
			if addr == "" {
				addr = dp.temporal.ensure(context.Background(), w)
			}
			if addr == "" {
				fmt.Fprintf(w, "worker (%s): could not start: no Temporal -- run `factoryd doctor -fix`, then `factoryd worker`\n", p.DataDir)
				continue
			}
			if err := dp.host.spawnWorker(w, binary, p.ConfigPath, p.DataDir, hostcontrol.ShellCredentialEnv(), pidPath, addr); err != nil {
				fmt.Fprintf(w, "worker (%s): could not start: %v -- start it with `factoryd worker -temporal-address %s`\n", p.DataDir, err, addr)
				continue
			}
			out = append(out, upgradeStarted(dp, p, pidPath))
		case p.Kind == "serve":
			if _, _, err := ensureServeStartTokenFile(dp, serveStableStartTokenPathFor(p.ConfigPath)); err != nil {
				fmt.Fprintf(w, "serve (%s): could not prepare its start token: %v\n", p.DataDir, err)
				continue
			}
			if err := dp.host.spawnServe(w, binary, p.ConfigPath, p.DataDir, p.ServeAddr); err != nil {
				fmt.Fprintf(w, "serve (%s): could not start: %v -- start it with `factoryd serve`\n", p.DataDir, err)
				continue
			}
			out = append(out, upgradeStarted(dp, p, filepath.Join(p.DataDir, "quickstart-serve.pid")))
		}
	}
	return out
}

func upgradeStarted(dp *deps, p upgradeProcess, pidPath string) string {
	if pid, ok := hostcontrol.QuickstartReadAlivePID(dp, pidPath); ok {
		return fmt.Sprintf("%s (%s): pid %d", p.Kind, p.DataDir, pid)
	}
	return fmt.Sprintf("%s (%s): started", p.Kind, p.DataDir)
}

// upgradeRefreshSkills reruns the new binary's install-skill for each skill dir.
func upgradeRefreshSkills(dp *deps, w io.Writer, binary string, skills []string) []string {
	var done []string
	for _, parent := range skills {
		if o, err := dp.host.runUpgradeCommand(upgradeCmd{Name: binary, Args: []string{"install-skill", "-dir", parent}}); err != nil {
			fmt.Fprintf(w, "skill (%s): install-skill failed: %s\n", parent, sanitize.Line(hostcontrol.LastLine(string(o))))
			continue
		}
		done = append(done, filepath.Join(parent, "buildgate"))
	}
	return done
}

func upgradePrintSummary(w io.Writer, sha, target string, profiles []profileInfo, restarted, skills []string) {
	fmt.Fprintf(w, "\nupgraded to %s (%s)\n", target, sha)
	fmt.Fprintf(w, "images re-pointed: %s\n", upgradeProfileNames(profiles))
	if len(restarted) == 0 {
		fmt.Fprintln(w, "restarted: nothing was running")
	} else {
		fmt.Fprintf(w, "restarted: %s\n", strings.Join(restarted, "; "))
	}
	if len(skills) == 0 {
		fmt.Fprintln(w, "skills refreshed: none")
	} else {
		fmt.Fprintf(w, "skills refreshed: %s\n", strings.Join(skills, ", "))
	}
}
