package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"buildgate/internal/daemonheartbeat"
	"buildgate/internal/workflow"
)

// superviseRepositories is a repeatable flag value. Keeping repositories as
// separate argv values is important: supervise never passes them through a
// shell, and repository identities may contain spaces.
type superviseRepositories []string

func (r *superviseRepositories) String() string {
	return strings.Join(*r, ",")
}

func (r *superviseRepositories) Set(value string) error {
	*r = append(*r, value)
	return nil
}

type superviseConfig struct {
	temporalAddress string
	repositories    []string
	dataDir         string
	// configPath, forwarded to every supervised `factoryd daemon` child
	// via daemonArgs (below) as -config, is the fix an adversarial review
	// of Phase A found: before this field existed, a
	// supervised daemon always resolved its own settings (sandbox/relay
	// budgets, release policy -- see daemonMain's own resolveSettings
	// call) from the default session-config search path, regardless of
	// what config `serve -daemon-temporal-address` or `factoryd
	// supervise` itself was told to use -- so a Temporal-path run could
	// silently build against a DIFFERENT config's budgets than every
	// other path on the same `serve` process already honors. Empty means
	// "no -config was set on this supervisor" and
	// daemonArgs omits the flag entirely, matching every prior behavior.
	configPath          string
	buildAppInterpreter string
	buildAppScript      string
	conformityPolicy    string
	maxRounds           int
	timeoutMinutes      int
	verifyCommand       string
	// build_app_max_attempts/verify_max_attempts and the whole sandbox
	// resource ceiling (sandbox_docker/memory/cpus/pids/tmpfs_size/
	// worker_uid) are deliberately absent here: flags-consolidate
	// (2026-09-10) made them session-config-only on `factoryd daemon`
	// itself, so a supervised child resolves them from the same
	// ~/.config/factoryd/config.yml this supervisor's own process would --
	// no argv hop needed, and none possible. Forwarding them anyway (which
	// daemonArgs did until this was caught in review) made every supervised
	// child die at flag parse with "flag provided but not defined", taking
	// the whole `factoryd supervise` and `serve -daemon-temporal-address`
	// path down; re-adding a field here would reintroduce that, or -- if
	// daemonArgs did not forward it -- silently ignore whatever the
	// operator configured.
	startupGrace      time.Duration
	heartbeatPoll     time.Duration
	restartBackoff    time.Duration
	restartBackoffMax time.Duration
	stopTimeout       time.Duration
}

// newSuperviseFlags builds `factoryd supervise`'s FlagSet in isolation from
// parsing/validation, so USAGE.md's doc-vs-flag drift test
// (TestUSAGEDocFlagsExistOnSubcommand) can enumerate its real flags without
// executing the command.
func newSuperviseFlags() (flags *flag.FlagSet, repositories *superviseRepositories, config *superviseConfig) {
	flags = flag.NewFlagSet("supervise", flag.ContinueOnError)
	repositories = &superviseRepositories{}
	config = &superviseConfig{}
	flags.StringVar(&config.temporalAddress, "temporal-address", "", "Temporal server address (required; forwarded to each daemon)")
	flags.Var(repositories, "repository", "repository identity to supervise (repeatable; at least one required)")
	flags.StringVar(&config.dataDir, "data-dir", "data", "directory for durable run records, logs, and daemon heartbeats")
	flags.StringVar(&config.configPath, "config", "", "session config path forwarded to every supervised `factoryd daemon` child as its own -config; empty leaves each child to search the default session-config path itself, same as before this flag existed")
	flags.StringVar(&config.buildAppInterpreter, "build-app-interpreter", "python3", "interpreter used to invoke -build-app-script")
	flags.StringVar(&config.buildAppScript, "build-app-script", "", "path to build_app.py (default: this version's embedded harness copy)")
	flags.StringVar(&config.conformityPolicy, "conformity-policy", "required", "build_app.py --conformity-policy (required|advisory) -- see run_ticket.go's own -conformity-policy flag help")
	flags.IntVar(&config.maxRounds, "max-rounds", 3, "build_app.py --max-rounds")
	flags.IntVar(&config.timeoutMinutes, "timeout-minutes", 45, "build_app.py --timeout-minutes")
	flags.StringVar(&config.verifyCommand, "verify-command", "make verify", "canonical verification command, run in workspace")
	flags.DurationVar(&config.startupGrace, "startup-grace", daemonheartbeat.Interval, "time after a child starts during which a missing heartbeat is tolerated")
	flags.DurationVar(&config.heartbeatPoll, "heartbeat-poll-interval", daemonheartbeat.Interval/3, "interval for checking child heartbeats and exits")
	flags.DurationVar(&config.restartBackoff, "restart-backoff", time.Second, "initial delay before restarting an unexpectedly exited or unhealthy child")
	flags.DurationVar(&config.restartBackoffMax, "restart-backoff-max", time.Minute, "maximum restart delay")
	flags.DurationVar(&config.stopTimeout, "stop-timeout", 10*time.Second, "maximum graceful stop wait before a child is killed")
	plainFlagUsage(flags)
	return flags, repositories, config
}

func parseSuperviseArgs(args []string) (superviseConfig, error) {
	flags, repositoriesPtr, config := newSuperviseFlags()
	if err := parseWithDataDir(flags, args, &config.dataDir, config.configPath); err != nil {
		return superviseConfig{}, err
	}
	repositories := *repositoriesPtr
	if flags.NArg() != 0 {
		return superviseConfig{}, fmt.Errorf("unexpected positional argument %q", flags.Arg(0))
	}
	if config.temporalAddress == "" {
		return superviseConfig{}, fmt.Errorf("-temporal-address is required")
	}
	if len(repositories) == 0 {
		return superviseConfig{}, fmt.Errorf("at least one -repository is required")
	}
	seen := make(map[string]struct{}, len(repositories))
	for _, repository := range repositories {
		if strings.TrimSpace(repository) == "" {
			return superviseConfig{}, fmt.Errorf("-repository must not be empty")
		}
		if _, ok := seen[repository]; ok {
			return superviseConfig{}, fmt.Errorf("duplicate -repository %q", repository)
		}
		seen[repository] = struct{}{}
	}
	if config.startupGrace < 0 {
		return superviseConfig{}, fmt.Errorf("-startup-grace must not be negative")
	}
	if config.heartbeatPoll <= 0 {
		return superviseConfig{}, fmt.Errorf("-heartbeat-poll-interval must be positive")
	}
	if config.restartBackoff <= 0 || config.restartBackoffMax <= 0 {
		return superviseConfig{}, fmt.Errorf("restart backoff values must be positive")
	}
	if config.restartBackoff > config.restartBackoffMax {
		return superviseConfig{}, fmt.Errorf("-restart-backoff cannot exceed -restart-backoff-max")
	}
	if config.stopTimeout <= 0 {
		return superviseConfig{}, fmt.Errorf("-stop-timeout must be positive")
	}
	// Leave an omitted script unresolved so the daemon child resolves the
	// embedded default itself.
	if config.buildAppScript != "" {
		resolvedBuildAppScript, err := resolveHarnessScript(config.buildAppScript, "build_app.py")
		if err != nil {
			return superviseConfig{}, err
		}
		config.buildAppScript = resolvedBuildAppScript
	}
	config.repositories = append([]string(nil), repositories...)
	return *config, nil
}

// daemonArgs is the one source of truth for the shared daemon flags that a
// supervisor forwards to every child. Supervisor-only lifecycle flags never
// cross this boundary, and neither does anything `factoryd daemon` no
// longer defines: every argv element here must name a flag daemonMain's own
// flag set registers, or the child dies at flag.Parse before it ever
// reaches its first validation -- see superviseConfig's own comment on the
// absent build/verify-attempt and sandbox-resource fields, and
// TestSuperviseDaemonArgsAreAllDefinedOnTheDaemonCommand, which parses this
// exact argv through daemonMain to keep the two in step.
func (c superviseConfig) daemonArgs(repository string) []string {
	args := []string{
		"daemon",
		"-temporal-address", c.temporalAddress,
		"-repository", repository,
		"-data-dir", c.dataDir,
		"-build-app-interpreter", c.buildAppInterpreter,
		"-build-app-script", c.buildAppScript,
		"-conformity-policy", c.conformityPolicy,
		"-max-rounds", strconv.Itoa(c.maxRounds),
		"-timeout-minutes", strconv.Itoa(c.timeoutMinutes),
		"-verify-command", c.verifyCommand,
	}
	if c.configPath != "" {
		args = append(args, "-config", c.configPath)
	}
	return args
}

func (c superviseConfig) supervisorArgs(repository string) []string {
	return append([]string{"supervise"}, c.daemonArgs(repository)[1:]...)
}

type superviseProcess interface {
	Wait() error
	Signal(os.Signal) error
	Kill() error
	PID() int
}

type superviseProcessFactory func(config superviseConfig, repository string) (superviseProcess, error)

// executable is a variable rather than a direct call inside the
// process factory so tests can verify executable discovery without replacing
// the supervisor's lifecycle loop. Production always uses os.Executable.
func (impl realHost) executable() (string, error) {
	return os.Executable()
}

type execSuperviseProcess struct{ command *exec.Cmd }

func (p *execSuperviseProcess) Wait() error { return p.command.Wait() }

func (p *execSuperviseProcess) Signal(signal os.Signal) error {
	if p.command.Process == nil {
		return errors.New("child process has not started")
	}
	return p.command.Process.Signal(signal)
}

func (p *execSuperviseProcess) Kill() error {
	if p.command.Process == nil {
		return errors.New("child process has not started")
	}
	return p.command.Process.Kill()
}

func (p *execSuperviseProcess) PID() int {
	if p.command.Process == nil {
		return 0
	}
	return p.command.Process.Pid
}

func realSuperviseProcessFactory(dp *deps, config superviseConfig, repository string) (superviseProcess, error) {
	return realFactorydProcess(dp, config.daemonArgs(repository))
}

func realFactorydProcess(dp *deps, args []string) (superviseProcess, error) {
	executable, err := dp.host.executable()
	if err != nil {
		return nil, fmt.Errorf("resolve factoryd executable: %w", err)
	}
	command := exec.Command(executable, args...)
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	command.Env = sanitizedSuperviseEnvironment(os.Environ())
	if err := command.Start(); err != nil {
		return nil, fmt.Errorf("start factoryd child: %w", err)
	}
	return &execSuperviseProcess{command: command}, nil
}

func sanitizedSuperviseEnvironment(environment []string) []string {
	filtered := make([]string, 0, len(environment))
	for _, entry := range environment {
		name, _, ok := strings.Cut(entry, "=")
		if ok && (name == startTokenEnvironmentVariable || name == overrideTokenEnvironmentVariable) {
			continue
		}
		filtered = append(filtered, entry)
	}
	return filtered
}

type superviseChild struct {
	repository string
	process    superviseProcess
	exited     chan error
	startedAt  time.Time
	failures   int
	restartAt  time.Time
	stopping   bool
}

func superviseBackoff(base, maximum time.Duration, failures int) time.Duration {
	if failures <= 0 {
		return base
	}
	delay := base
	for i := 1; i < failures && delay < maximum; i++ {
		if delay > maximum/2 {
			return maximum
		}
		delay *= 2
	}
	if delay > maximum {
		return maximum
	}
	return delay
}

func startSuperviseChild(child *superviseChild, config superviseConfig, factory superviseProcessFactory, now time.Time) error {
	process, err := factory(config, child.repository)
	if err != nil {
		child.failures++
		child.restartAt = now.Add(superviseBackoff(config.restartBackoff, config.restartBackoffMax, child.failures))
		return err
	}
	child.process = process
	exited := make(chan error, 1)
	child.exited = exited
	child.startedAt = now
	child.restartAt = time.Time{}
	child.stopping = false
	go func() {
		exited <- process.Wait()
	}()
	return nil
}

func observeChildExit(child *superviseChild, now time.Time, config superviseConfig) (bool, error) {
	if child.process == nil {
		return false, nil
	}
	select {
	case err := <-child.exited:
		child.process = nil
		child.stopping = false
		child.failures++
		child.restartAt = now.Add(superviseBackoff(config.restartBackoff, config.restartBackoffMax, child.failures))
		return true, err
	default:
		return false, nil
	}
}

func superviseHeartbeatHealthy(heartbeat daemonheartbeat.Heartbeat, repository string, childPID int, now time.Time) bool {
	return superviseHeartbeatIdentityMatches(heartbeat, repository) &&
		heartbeat.PID == childPID &&
		!daemonheartbeat.Stale(heartbeat, now, daemonheartbeat.SandboxStaleAfter)
}

func superviseHeartbeatIdentityMatches(heartbeat daemonheartbeat.Heartbeat, repository string) bool {
	ownerID := workflow.RepositoryOwnerWorkflowID(repository)
	return heartbeat.Repository == repository &&
		heartbeat.TaskQueue == "factoryd-repo-"+ownerID
}

func stopSuperviseChild(child *superviseChild, config superviseConfig, signal os.Signal) error {
	if child.process == nil {
		return nil
	}
	if err := child.process.Signal(signal); err != nil {
		select {
		case <-child.exited:
			child.process = nil
			child.stopping = false
			return nil
		default:
		}
		if killErr := child.process.Kill(); killErr != nil {
			return fmt.Errorf("signal child: %v; kill child: %w", err, killErr)
		}
	}
	timer := time.NewTimer(config.stopTimeout)
	defer timer.Stop()
	select {
	case <-child.exited:
		child.process = nil
		child.stopping = false
		return nil
	case <-timer.C:
		if err := child.process.Kill(); err != nil {
			return fmt.Errorf("kill child after graceful-stop timeout: %w", err)
		}
		killTimer := time.NewTimer(config.stopTimeout)
		defer killTimer.Stop()
		select {
		case <-child.exited:
			child.process = nil
			child.stopping = false
			return nil
		case <-killTimer.C:
			child.stopping = true
			return fmt.Errorf("child did not exit after kill")
		}
	}
}

func stopAllSuperviseChildren(children map[string]*superviseChild, config superviseConfig, signal os.Signal) error {
	repositories := make([]string, 0, len(children))
	for repository := range children {
		repositories = append(repositories, repository)
	}
	sort.Strings(repositories)
	var errs []error
	for _, repository := range repositories {
		child := children[repository]
		if err := stopSuperviseChild(child, config, signal); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", child.repository, err))
		}
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return nil
}

// runSuperviseLoop owns all child state. The ticker is injected so lifecycle
// tests can advance time deterministically; production supplies a real ticker
// from superviseMain. A child is never restarted after shutdown enters this
// function's signal/cancellation branch.
func runSuperviseLoop(ctx context.Context, signals <-chan os.Signal, config superviseConfig, factory superviseProcessFactory, now func() time.Time, ticks <-chan time.Time, tickProcessed func(time.Time)) error {
	children := make(map[string]*superviseChild, len(config.repositories))
	for _, repository := range config.repositories {
		child := &superviseChild{repository: repository}
		children[repository] = child
		if err := startSuperviseChild(child, config, factory, now()); err != nil {
			log.Printf("supervise: %v; restart for repository %q scheduled at %s", err, repository, child.restartAt.Format(time.RFC3339Nano))
		}
	}
	for {
		select {
		case signal := <-signals:
			if signal == nil {
				signal = syscall.SIGTERM
			}
			return stopAllSuperviseChildren(children, config, signal)
		case <-ctx.Done():
			return stopAllSuperviseChildren(children, config, syscall.SIGTERM)
		case tick := <-ticks:
			nowTime := tick
			if nowTime.IsZero() {
				nowTime = now()
			}
			for _, child := range children {
				if child.process != nil {
					if exited, err := observeChildExit(child, nowTime, config); exited {
						log.Printf("supervise: daemon for repository %q exited (%v); restart scheduled at %s", child.repository, err, child.restartAt.Format(time.RFC3339Nano))
					}
				}
				if child.process == nil && !child.restartAt.IsZero() && !nowTime.Before(child.restartAt) {
					if err := startSuperviseChild(child, config, factory, nowTime); err != nil {
						log.Printf("supervise: %v; restart for repository %q scheduled at %s", err, child.repository, child.restartAt.Format(time.RFC3339Nano))
					}
				}
				if child.process == nil || child.stopping || nowTime.Sub(child.startedAt) < config.startupGrace {
					continue
				}
				heartbeat, err := daemonheartbeat.Read(daemonheartbeat.Path(config.dataDir, workflow.RepositoryOwnerWorkflowID(child.repository)))
				if err == nil && superviseHeartbeatHealthy(heartbeat, child.repository, child.process.PID(), nowTime) {
					child.failures = 0
					continue
				}
				if err != nil {
					log.Printf("supervise: heartbeat for repository %q unreadable: %v", child.repository, err)
				} else {
					log.Printf("supervise: heartbeat for repository %q is stale", child.repository)
				}
				if stopErr := stopSuperviseChild(child, config, syscall.SIGTERM); stopErr != nil {
					log.Printf("supervise: stopping unhealthy daemon for repository %q: %v", child.repository, stopErr)
				}
				if child.process == nil {
					child.failures++
					child.restartAt = nowTime.Add(superviseBackoff(config.restartBackoff, config.restartBackoffMax, child.failures))
				}
			}
			if tickProcessed != nil {
				tickProcessed(nowTime)
			}
		}
	}
}

func superviseMain(dp *deps, args []string) error {
	config, err := parseSuperviseArgs(args)
	if err != nil {
		return err
	}
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)
	ticker := time.NewTicker(config.heartbeatPoll)
	defer ticker.Stop()
	return runSuperviseLoop(context.Background(), signals, config, func(a0 superviseConfig, a1 string) (superviseProcess, error) {
		return realSuperviseProcessFactory(dp, a0, a1)
	}, time.Now, ticker.C, nil)
}
