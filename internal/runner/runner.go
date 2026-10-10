// Package runner shells out to the existing pi-harness-hardening Python
// scripts. The agent invoked underneath is an untrusted worker: this
// package only captures what actually happened (exit code, duration, full
// log) — it never parses the agent's own prose as a pass/fail signal.
package runner

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"buildgate/internal/evidence"
	"buildgate/internal/sandbox"
)

// Result is what actually happened when a command ran — the only thing
// callers may use to decide pass/fail. ExitCode is -1 whenever err is
// non-nil (infrastructure failure): the process either never produced a
// real exit code or one can't be trusted, and -1 can't be confused with a
// genuine exit status, which are always >= 0.
type Result struct {
	Command    []string
	ExitCode   int
	StartedAt  time.Time
	FinishedAt time.Time
	LogPath    string
	// ImageDigest is the sandbox worker image's pinned sha256 digest when
	// this attempt ran inside Docker (see internal/sandbox.Result), and
	// empty for a host-runner attempt.
	ImageDigest string
	// ScriptsSHA256 is sandbox.ScriptsSHA256's digest over the harness
	// script this attempt staged plus its staged sibling modules,
	// computed once per launch (not per retry) by the caller right after
	// staging finishes, and empty when the launch staged no script
	// (e.g. a canonical-verification attempt, which runs the project's
	// own command directly). Threaded through to run.Attempt.
	// HarnessScriptsSHA256 by cmd/factoryd/sandbox_exec.go and internal/workflow's
	// activities.
	ScriptsSHA256 string
	// Skills, SkillsSHA256 and RepoSkills are threaded to the matching
	// run.Attempt fields (see their doc comments) by both execution paths.
	Skills       []string
	SkillsSHA256 string
	RepoSkills   []string
	// FactoryDirSHA256, FactoryDirCommit and FactoryDirError are threaded to
	// the run.Attempt fields of the same names: the read-only `.factory/`
	// snapshot the launch mounted, or why the launch was refused.
	FactoryDirSHA256 string
	FactoryDirCommit string
	FactoryDirError  string
	// RelayImageDigest, RelayNetworkName, RelayContainerName, and
	// RelayUpstream carry internal/sandbox.Result.RelayFacts' audit-safe
	// fields when this attempt's sandboxed worker was launched with the
	// factory-owned inference relay as its only network path, instead of
	// with no network. They record what the worker could reach, not that
	// it issued a request -- all four are empty when the attempt had no
	// relay.
	RelayImageDigest   string
	RelayNetworkName   string
	RelayContainerName string
	RelayUpstream      string
	// RelayCredentialMode carries internal/sandbox.RouteLaunchFacts.
	// CredentialMode (meter.CredentialModeStatic/CredentialModeGitHubCopilot/
	// CredentialModeChatGPTCodex) -- evidence of *how* this attempt's
	// spend was billed, so a display layer can tell a subscription
	// route's cost apart from a metered API route's instead of always
	// showing an invented-looking dollar figure. Empty when the attempt
	// had no relay.
	RelayCredentialMode string
	// RelayRoute/RelayBilling carry internal/sandbox.RouteLaunchFacts.
	// Route/Billing: the session-config route name this attempt's relay
	// was launched from, and how that route is billed ("subscription" or
	// "metered"). Both empty for a legacy config with no routes: key, or
	// an attempt with no relay.
	RelayRoute   string
	RelayBilling string
	// RelayWorkerModelID carries internal/sandbox.RouteLaunchFacts.
	// WorkerModelID: the worker model id configured for this attempt's
	// relay (not enforced by the relay -- see that field's doc comment).
	// Empty when the attempt had no relay or no model id was configured.
	RelayWorkerModelID string
	// RelayReasoningEffort carries internal/sandbox.RouteLaunchFacts.
	// ReasoningEffort: the HIGHEST reasoning effort this attempt's relay
	// observed a forwarded request ask for. Empty when the attempt had no
	// relay, none was ever requested, or the relay predates this field.
	RelayReasoningEffort string
	// RelayReasoningEffortAnomaly carries internal/sandbox.
	// RouteLaunchFacts.ReasoningEffortAnomaly: true when at least one
	// request this attempt's relay forwarded named a reasoning effort it
	// could not recognize. See run.Attempt.RelayReasoningEffortAnomaly's
	// own doc comment for why this is tracked separately from
	// RelayReasoningEffort.
	RelayReasoningEffortAnomaly bool
	// RelayConsumedInputTokens/RelayConsumedOutputTokens/
	// RelayConsumedCostMicroUSD/RelayCeilingExceeded carry
	// internal/sandbox.Result.RelayFacts' consumed-usage fields (see
	// RouteLaunchFacts' own doc comment): this attempt's actual relay
	// spend, populated only at relay cleanup, not at launch -- previously
	// only the relay's configured budget, never its real consumption, was
	// ever recorded here. All four are zero/false for an attempt with no
	// relay, or one whose relay was never successfully cleaned up (e.g.
	// reclaimed later by ReconcileRelayOrphans, which cannot read a
	// removed container's logs).
	RelayConsumedInputTokens  int64
	RelayConsumedOutputTokens int64
	RelayConsumedCostMicroUSD int64
	RelayCeilingExceeded      bool
	// RelaySpendPartial carries internal/sandbox.RouteLaunchFacts.
	// SpendPartial: true when the four fields above are a best-effort
	// recovery from this run's usage ledger, not a confirmed final total,
	// because the relay exited abnormally before `docker logs` could be
	// read at cleanup. Callers rendering these fields as evidence must say
	// so rather than presenting them as the complete spend.
	RelaySpendPartial bool
}

// Run executes name/args with dir as its working directory, tees combined
// stdout+stderr to both this process's stdout and logPath, and returns once
// the command exits or ctx is done. It never returns an error for a
// non-zero exit — that's a normal outcome the caller inspects via
// Result.ExitCode; err is reserved for infrastructure failure (couldn't
// start the process, couldn't read its output, couldn't write the log, or
// ctx ended). The returned Result is
// always populated (Command/StartedAt at minimum) even on error, so a
// caller can record it as partial evidence rather than lose it.
func Run(ctx context.Context, dir, logPath string, name string, args ...string) (Result, error) {
	return run(ctx, dir, logPath, func(logFile *os.File) io.Writer {
		return io.MultiWriter(os.Stdout, logFile)
	}, name, args...)
}

// hostWorkerEnvBaseline is the fixed, always-safe set of environment
// variable names an unsandboxed subprocess this package launches receives
// unconditionally, with no operator configuration required: inert
// interpreter/locale/shell plumbing with no path to a host credential.
// This is a real allowlist, replacing the denylist (subprocessEnvExcluded)
// this package previously shipped, whose exclusion list could never be
// exhaustive against every possible credential-shaped variable name. There
// is no operator-configurable addition to this baseline (the
// -host-worker-env-allow/-host-worker-allow-anthropic-key flags that used
// to widen it existed only for -allow-unsandboxed's now-removed
// host-execution escape hatch) -- an unsandboxed subprocess's environment
// is exactly this fixed set, always.
//
// The request driver's own spec/plan/oracle-drafting jobs run every real
// invocation through the same Docker sandbox a ticket build uses
// (runSandboxWithRetries, cmd/factoryd); each also keeps an unsandboxed
// fallback branch reachable only under workerConfig.allowUnsandboxedSpecDraft,
// which has no operator-facing flag and is exercised only by this repo's
// own tests. This baseline is what that test-only branch's subprocess
// receives.
var hostWorkerEnvBaseline = map[string]bool{
	"PATH":   true,
	"HOME":   true,
	"LANG":   true,
	"TZ":     true,
	"TMPDIR": true,
	"USER":   true,
	"SHELL":  true,
	// The POSIX locale category variables, named exactly rather than
	// matched by an "LC_" prefix (found via the adversarial pass this
	// allowlist's own design demands: a prefix rule here would
	// unconditionally admit ANY environment variable an operator's own
	// process happened to be carrying under a name starting "LC_", with
	// no allowlist gating at all -- e.g. a misconfigured or
	// attacker-influenced "LC_GITHUB_TOKEN" would sail through the
	// baseline untouched, even though IsForbiddenCredentialEnvKey itself
	// has no entry shaped like that to catch it). Listing the real,
	// finite POSIX set by exact name closes that off entirely: nothing
	// outside this list can piggyback on looking vaguely locale-shaped.
	"LC_ALL":            true,
	"LC_COLLATE":        true,
	"LC_CTYPE":          true,
	"LC_MESSAGES":       true,
	"LC_MONETARY":       true,
	"LC_NUMERIC":        true,
	"LC_TIME":           true,
	"LC_PAPER":          true,
	"LC_NAME":           true,
	"LC_ADDRESS":        true,
	"LC_TELEPHONE":      true,
	"LC_MEASUREMENT":    true,
	"LC_IDENTIFICATION": true,
}

// subprocessEnvAllowed reports whether name may reach an unsandboxed
// subprocess this package launches. sandbox.IsForbiddenCredentialEnvKey is
// checked first, unconditionally, ahead of hostWorkerEnvBaseline: no name
// it forbids may ever reach such a subprocess, regardless of the baseline.
func subprocessEnvAllowed(name string) bool {
	if sandbox.IsForbiddenCredentialEnvKey(name) {
		return false
	}
	return hostWorkerEnvBaseline[name]
}

// subprocessEnv returns the subset of this process's own environment
// subprocessEnvAllowed admits, for use as a launched subprocess's cmd.Env.
func subprocessEnv() []string {
	environ := os.Environ()
	filtered := make([]string, 0, len(environ))
	for _, kv := range environ {
		name, _, _ := strings.Cut(kv, "=")
		if subprocessEnvAllowed(name) {
			filtered = append(filtered, kv)
		}
	}
	return filtered
}

func run(ctx context.Context, dir, logPath string, outputWriter func(*os.File) io.Writer, name string, args ...string) (Result, error) {
	res := Result{Command: append([]string{name}, args...), StartedAt: time.Now(), ExitCode: -1}

	logFile, err := os.OpenFile(logPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return res, fmt.Errorf("create log file: %w", err)
	}
	defer logFile.Close()
	res.LogPath = logPath

	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = subprocessEnv()
	// name is typically a shell/interpreter (sh -c "...", python3
	// build_app.py) that can itself spawn children (e.g. a hung
	// `sleep 300` inside a script). Killing only the direct process on
	// cancellation leaves such a child alive, still holding the stdout
	// pipe open — the scanner loop below would then never see EOF and
	// factoryd would hang well past its own -timeout. Run the command in
	// its own process group and kill the whole group on cancellation, and
	// force Wait() to return (closing the pipes) even if a runaway child
	// still holds them open past WaitDelay.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = 5 * time.Second

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return res, fmt.Errorf("stdout pipe: %w", err)
	}
	cmd.Stderr = cmd.Stdout // combine streams into one ordered pipe

	if err := cmd.Start(); err != nil {
		return res, fmt.Errorf("start %s: %w", name, err)
	}
	// pidFilePath records this process group's leader PID for the
	// duration of the call, so a process that outlives an ungracefully
	// killed factoryd (found via review: SIGKILL gives factoryd no chance
	// to run its own Cancel/cleanup, and this child has no parent-death
	// signal of its own, so it can keep running with no factoryd process
	// left to notice) can still be proven alive or dead by anyone reading
	// this file later -- in particular, isolation-marker reconciliation
	// before it reaps a worktree still in active use. A write failure
	// here is fatal, not a logged-and-continue warning (found via review,
	// round 2: silently continuing left a live, wholly unmarked child --
	// exactly the condition reconciliation depends on never being true --
	// indistinguishable from one that was never started at all). Removed
	// via defer regardless of outcome, so a file that still exists once
	// this call returns is itself the "died without being able to clean
	// up" signal reconciliation depends on.
	//
	// This still leaves the narrow window between Start() returning and
	// this write actually landing -- eliminating that would need the
	// child to write its own marker before executing (e.g. a shell
	// wrapper doing `echo $$ >pidfile; exec "$@"`), which was tried and
	// reverted: it changes a missing command from a clean Start()-time Go
	// error into an opaque wrapped-shell exit code indistinguishable from
	// a normal build failure, itself a regression (three existing tests
	// depend on that exact distinction). Judged not worth trading a
	// correctness regression on every invocation for closing a
	// microseconds-wide window on an already-rare ungraceful-kill path.
	pidFilePath := logPath + ".pid"
	if err := os.WriteFile(pidFilePath, fmt.Appendf(nil, "%d\n", cmd.Process.Pid), 0o600); err != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = cmd.Wait()
		return res, fmt.Errorf("write pid file %s: %w", pidFilePath, err)
	}
	defer os.Remove(pidFilePath)

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	writer := outputWriter(logFile)
	var writeErr error
	for scanner.Scan() {
		if _, err := fmt.Fprintln(writer, scanner.Text()); err != nil {
			// A failed write here (e.g. disk full) would otherwise leave
			// logPath silently truncated with no signal at all — and that
			// file's hash later becomes gate evidence (evidence.SHA256File),
			// so a quiet truncation here is an evidence-integrity bug, not
			// just a cosmetic one. Unlike context cancellation, a write
			// failure has no lifecycle signal that would stop the child. Kill
			// its process group before Wait so a child blocked on the undrained
			// pipe cannot leave Run hung indefinitely.
			writeErr = err
			if cmd.Process != nil {
				_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			}
			break
		}
	}
	scanErr := scanner.Err()

	waitErr := cmd.Wait()
	res.FinishedAt = time.Now()

	if scanErr != nil {
		return res, fmt.Errorf("read output of %s: %w", name, scanErr)
	}
	if writeErr != nil {
		return res, fmt.Errorf("write log output of %s: %w", name, writeErr)
	}
	if waitErr != nil {
		// A cancelled/expired ctx killed the process out from under us;
		// that's an infrastructure failure, not a normal non-zero exit,
		// and callers must be able to tell the two apart (see package
		// doc: retries distinguish infrastructure failure from
		// implementation failure). Only consult ctx.Err() here, inside
		// the waitErr != nil branch — checking it unconditionally would
		// misclassify a command that finished successfully just as its
		// deadline expired (cmd.Wait can return nil in that race).
		if ctx.Err() != nil {
			return res, fmt.Errorf("run %s: %w", name, ctx.Err())
		}
		var exitErr *exec.ExitError
		if errors.As(waitErr, &exitErr) {
			res.ExitCode = exitErr.ExitCode()
			return res, nil
		}
		return res, fmt.Errorf("run %s: %w", name, waitErr)
	}
	res.ExitCode = 0
	return res, nil
}

// RunWithRetries calls Run until it succeeds at the subprocess-execution
// level or maxAttempts is exhausted. A normal non-zero exit is not retried;
// only Run's non-nil infrastructure error is. maxAttempts values below one
// still make one attempt. logPathFor receives a 1-indexed attempt number so
// each attempt can preserve its own output, and onAttempt receives every
// Result and error in order so callers can record complete evidence.
func RunWithRetries(ctx context.Context, dir string, logPathFor func(attempt int) string, maxAttempts int, onAttempt func(attempt int, result Result, err error), name string, args ...string) (Result, error) {
	return RunWithRetriesChecked(ctx, dir, logPathFor, maxAttempts, nil, func(attempt int, result Result, err error) error {
		if onAttempt != nil {
			onAttempt(attempt, result, err)
		}
		return nil
	}, name, args...)
}

// RunWithRetriesChecked is RunWithRetries' error-aware counterpart. before-
// and afterAttempt run immediately before and after each subprocess,
// respectively. A hook error stops the retry loop before another subprocess
// starts, allowing callers to durably record retry evidence without risking a
// second attempt after that evidence becomes unavailable.
func RunWithRetriesChecked(ctx context.Context, dir string, logPathFor func(attempt int) string, maxAttempts int, beforeAttempt func(attempt int) error, afterAttempt func(attempt int, result Result, err error) error, name string, args ...string) (Result, error) {
	if maxAttempts < 1 {
		maxAttempts = 1
	}

	var res Result
	var err error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if beforeAttempt != nil {
			if hookErr := beforeAttempt(attempt); hookErr != nil {
				return res, fmt.Errorf("before attempt %d hook: %w", attempt, hookErr)
			}
		}
		res, err = Run(ctx, dir, logPathFor(attempt), name, args...)
		if afterAttempt != nil {
			if hookErr := afterAttempt(attempt, res, err); hookErr != nil {
				return res, fmt.Errorf("after attempt %d hook: %w", attempt, hookErr)
			}
		}
		if err == nil || ctx.Err() != nil {
			return res, err
		}
	}
	return res, err
}

// GitRevParseHEAD returns the current commit SHA in dir.
func GitRevParseHEAD(dir string) (string, error) {
	out, err := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()
	if err != nil {
		return "", fmt.Errorf("git rev-parse HEAD: %w", err)
	}
	sha := string(out)
	if n := len(sha); n > 0 && sha[n-1] == '\n' {
		sha = sha[:n-1]
	}
	return sha, nil
}

// GitRevParseRef returns the commit SHA ref currently resolves to in dir --
// GitRevParseHEAD's own generalization for a caller that needs some other
// ref's tip (e.g. an existing branch's own current commit, for -on-branch's
// own base-SHA capture), not always the checked-out HEAD.
func GitRevParseRef(dir, ref string) (string, error) {
	out, err := exec.Command("git", "-C", dir, "rev-parse", ref).Output()
	if err != nil {
		return "", fmt.Errorf("git rev-parse %s: %w", ref, err)
	}
	sha := string(out)
	if n := len(sha); n > 0 && sha[n-1] == '\n' {
		sha = sha[:n-1]
	}
	return sha, nil
}

// GitResolveCommit verifies that sha exists in dir as a commit and returns
// Git's canonical object ID. This is intentionally separate from
// GitRevParseHEAD: an isolated chained run must validate its predecessor's
// recorded result_sha before asking git worktree add to create anything.
func GitResolveCommit(dir, sha string) (string, error) {
	if strings.TrimSpace(sha) != sha || (len(sha) != 40 && len(sha) != 64) {
		return "", fmt.Errorf("git commit ID %q is not a canonical full object ID", sha)
	}
	for _, c := range sha {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return "", fmt.Errorf("git commit ID %q is not a canonical full object ID", sha)
		}
	}
	out, err := exec.Command("git", "-C", dir, "rev-parse", "--verify", sha+"^{commit}").Output()
	if err != nil {
		return "", fmt.Errorf("git rev-parse --verify %s: %w", sha, err)
	}
	resolved := strings.TrimSpace(string(out))
	if resolved == "" {
		return "", fmt.Errorf("git rev-parse --verify %s returned an empty object ID", sha)
	}
	if resolved != sha {
		return "", fmt.Errorf("git commit ID %q is not a canonical full object ID (resolved to %q)", sha, resolved)
	}
	return resolved, nil
}

// GitIsAncestor reports whether ancestor is an ancestor of (or equal to)
// descendant in dir. Found live: a factoryd run against a real
// build_app.py/pi harness invocation whose workspace's git history no
// longer contained the run's own captured base_sha as an ancestor of
// HEAD by the time build_app.py returned — the harness had reset the
// workspace backward mid-run and committed on top of that older state.
// Nothing before this check verified base_sha was still real; the diff
// against it then looked like a large, spurious deletion of unrelated
// already-completed work, which diff_scope happened to catch only
// because the ticket declared a narrow Allowed-Files — a ticket without
// one would have silently accepted it. `git merge-base --is-ancestor`
// exits 0 for true, 1 for false, and anything else (e.g. an unknown SHA)
// is a real error distinct from either.
func GitIsAncestor(dir, ancestor, descendant string) (bool, error) {
	cmd := exec.Command("git", "-C", dir, "merge-base", "--is-ancestor", ancestor, descendant)
	err := cmd.Run()
	if err == nil {
		return true, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return false, nil
	}
	return false, fmt.Errorf("git merge-base --is-ancestor %s %s: %w", ancestor, descendant, err)
}

// GitShowFile returns path's content as of sha in dir, and whether it
// existed there at all. A file that didn't exist yet at sha (e.g. one the
// current run newly created) is a normal, expected case — not an error —
// so existence is checked first via `git cat-file -e`, which exits
// nonzero only for a missing path, distinctly from any other git failure
// `git show` itself might report.
func GitShowFile(dir, sha, path string) (content string, existed bool, err error) {
	ref := sha + ":" + path
	if err := exec.Command("git", "-C", dir, "cat-file", "-e", ref).Run(); err != nil {
		return "", false, nil
	}
	out, err := exec.Command("git", "-C", dir, "show", ref).Output()
	if err != nil {
		return "", false, fmt.Errorf("git show %s: %w", ref, err)
	}
	return string(out), true, nil
}

// GitTreeFilesNamed lists the paths in commit sha of dir's repository whose
// last element is name, in git's order.
func GitTreeFilesNamed(dir, sha, name string) ([]string, error) {
	out, err := exec.Command("git", "-C", dir, "ls-tree", "-r", "--name-only", "-z", sha).Output()
	if err != nil {
		return nil, fmt.Errorf("git ls-tree %s: %w", sha, err)
	}
	var paths []string
	for _, path := range strings.Split(string(out), "\x00") {
		if path == name || strings.HasSuffix(path, "/"+name) {
			paths = append(paths, path)
		}
	}
	return paths, nil
}

// ReadRegularFile reads path's content, refusing to follow a symlink.
// Found live (review): a required file's content was read via a plain
// os.ReadFile, which follows symlinks — a run that replaced a required
// path with a symlink to any other readable file containing the required
// marker could satisfy required_content_present without the required
// path itself ever containing it, since the filesystem read silently
// dereferenced to the link's target. Returns (empty, false, nil) for a
// path that doesn't exist at all, matching GitShowFile's convention for
// "not present"; returns an error — not a quiet false — for a symlink or
// any other non-regular file, since a legitimate build has no reason to
// turn one of its required files into one.
func ReadRegularFile(path string) (content string, existed bool, err error) {
	info, statErr := os.Lstat(path)
	if statErr != nil {
		if os.IsNotExist(statErr) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("stat %s: %w", path, statErr)
	}
	if !info.Mode().IsRegular() {
		return "", false, fmt.Errorf("%s is not a regular file (mode %s) — refusing to read it as required-content evidence", path, info.Mode())
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", false, fmt.Errorf("read %s: %w", path, err)
	}
	return string(b), true, nil
}

// PackageLockDependencyChanges returns semantic changes for every changed
// package-lock.json path. The base snapshot comes from baseSHA, while the
// result snapshot is read from the workspace so uncommitted verification
// output is included in the same way as the changed-file inventory.
// Missing files are treated as empty lockfiles, allowing additions and
// deletions to be represented as dependency additions/removals. A malformed
// lockfile returns an error rather than silently becoming empty evidence.
func PackageLockDependencyChanges(dir, baseSHA string, changedFiles []string) ([]evidence.DependencyChange, error) {
	paths := make([]string, 0)
	for _, path := range changedFiles {
		if filepath.Base(path) == "package-lock.json" {
			paths = append(paths, path)
		}
	}
	sort.Strings(paths)

	changes := make([]evidence.DependencyChange, 0)
	for _, path := range paths {
		base, found, err := GitShowFile(dir, baseSHA, path)
		if err != nil {
			return nil, fmt.Errorf("read base package-lock %q: %w", path, err)
		}
		if !found {
			base = `{"packages":{}}`
		}

		result, exists, err := ReadRegularFile(filepath.Join(dir, path))
		if err != nil {
			return nil, fmt.Errorf("read result package-lock %q: %w", path, err)
		}
		if !exists {
			result = `{"packages":{}}`
		}

		pathChanges, err := evidence.PackageLockDependencyDiff([]byte(base), []byte(result))
		if err != nil {
			return nil, fmt.Errorf("diff package-lock %q: %w", path, err)
		}
		changes = append(changes, pathChanges...)
	}
	return changes, nil
}

// ComposerLockDependencyChanges returns semantic changes for every changed
// composer.lock path. The base snapshot comes from baseSHA, while the result
// snapshot is read from the workspace so uncommitted verification output is
// included in the same way as the changed-file inventory.
// Missing files are treated as empty lockfiles, allowing additions and
// deletions to be represented as dependency additions/removals. A malformed
// lockfile returns an error rather than silently becoming empty evidence.
func ComposerLockDependencyChanges(dir, baseSHA string, changedFiles []string) ([]evidence.DependencyChange, error) {
	paths := make([]string, 0)
	for _, path := range changedFiles {
		if filepath.Base(path) == "composer.lock" {
			paths = append(paths, path)
		}
	}
	sort.Strings(paths)

	changes := make([]evidence.DependencyChange, 0)
	for _, path := range paths {
		base, found, err := GitShowFile(dir, baseSHA, path)
		if err != nil {
			return nil, fmt.Errorf("read base composer.lock %q: %w", path, err)
		}
		if !found {
			base = `{"packages":[],"packages-dev":[]}`
		}

		result, exists, err := ReadRegularFile(filepath.Join(dir, path))
		if err != nil {
			return nil, fmt.Errorf("read result composer.lock %q: %w", path, err)
		}
		if !exists {
			result = `{"packages":[],"packages-dev":[]}`
		}

		pathChanges, err := evidence.ComposerLockDependencyDiff([]byte(base), []byte(result))
		if err != nil {
			return nil, fmt.Errorf("diff composer.lock %q: %w", path, err)
		}
		changes = append(changes, pathChanges...)
	}
	return changes, nil
}

// PubspecLockDependencyChanges returns semantic changes for every changed
// pubspec.lock path. The base snapshot comes from baseSHA, while the
// result snapshot is read from the workspace so uncommitted verification
// output is included in the same way as the changed-file inventory.
// Missing files are treated as empty lockfiles, allowing additions and
// deletions to be represented as dependency additions/removals. A malformed
// lockfile returns an error rather than silently becoming empty evidence.
func PubspecLockDependencyChanges(dir, baseSHA string, changedFiles []string) ([]evidence.DependencyChange, error) {
	paths := make([]string, 0)
	for _, path := range changedFiles {
		if filepath.Base(path) == "pubspec.lock" {
			paths = append(paths, path)
		}
	}
	sort.Strings(paths)

	changes := make([]evidence.DependencyChange, 0)
	for _, path := range paths {
		base, found, err := GitShowFile(dir, baseSHA, path)
		if err != nil {
			return nil, fmt.Errorf("read base pubspec.lock %q: %w", path, err)
		}
		if !found {
			base = "packages: {}"
		}

		result, exists, err := ReadRegularFile(filepath.Join(dir, path))
		if err != nil {
			return nil, fmt.Errorf("read result pubspec.lock %q: %w", path, err)
		}
		if !exists {
			result = "packages: {}"
		}

		pathChanges, err := evidence.PubspecLockDependencyDiff([]byte(base), []byte(result))
		if err != nil {
			return nil, fmt.Errorf("diff pubspec.lock %q: %w", path, err)
		}
		changes = append(changes, pathChanges...)
	}
	return changes, nil
}

// GoSumDependencyChanges returns semantic changes for every changed go.sum
// path. The base snapshot comes from baseSHA, while the result snapshot is
// read from the workspace so uncommitted verification output is included in
// the same way as the changed-file inventory. Missing files are treated as
// empty lockfiles, allowing additions and deletions to be represented as
// dependency additions/removals. A malformed lockfile returns an error rather
// than silently becoming empty evidence.
func GoSumDependencyChanges(dir, baseSHA string, changedFiles []string) ([]evidence.DependencyChange, error) {
	paths := make([]string, 0)
	for _, path := range changedFiles {
		if filepath.Base(path) == "go.sum" {
			paths = append(paths, path)
		}
	}
	sort.Strings(paths)

	changes := make([]evidence.DependencyChange, 0)
	for _, path := range paths {
		base, found, err := GitShowFile(dir, baseSHA, path)
		if err != nil {
			return nil, fmt.Errorf("read base go.sum %q: %w", path, err)
		}
		if !found {
			base = ""
		}

		result, exists, err := ReadRegularFile(filepath.Join(dir, path))
		if err != nil {
			return nil, fmt.Errorf("read result go.sum %q: %w", path, err)
		}
		if !exists {
			result = ""
		}

		pathChanges, err := evidence.GoSumDependencyDiff([]byte(base), []byte(result))
		if err != nil {
			return nil, fmt.Errorf("diff go.sum %q: %w", path, err)
		}
		changes = append(changes, pathChanges...)
	}
	return changes, nil
}

type dependencyLockDiffFunc func([]byte, []byte) ([]evidence.DependencyChange, error)

func dependencyLockChanges(dir, baseSHA string, changedFiles []string, basename string, diff dependencyLockDiffFunc) ([]evidence.DependencyChange, error) {
	paths := make([]string, 0)
	for _, path := range changedFiles {
		if filepath.Base(path) == basename {
			paths = append(paths, path)
		}
	}
	sort.Strings(paths)
	changes := make([]evidence.DependencyChange, 0)
	for _, path := range paths {
		base, found, err := GitShowFile(dir, baseSHA, path)
		if err != nil {
			return nil, fmt.Errorf("read base %s %q: %w", basename, path, err)
		}
		if !found {
			base = ""
		}
		result, exists, err := ReadRegularFile(filepath.Join(dir, path))
		if err != nil {
			return nil, fmt.Errorf("read result %s %q: %w", basename, path, err)
		}
		if !exists {
			result = ""
		}
		pathChanges, err := diff([]byte(base), []byte(result))
		if err != nil {
			return nil, fmt.Errorf("diff %s %q: %w", basename, path, err)
		}
		changes = append(changes, pathChanges...)
	}
	return changes, nil
}

func YarnLockDependencyChanges(dir, baseSHA string, changedFiles []string) ([]evidence.DependencyChange, error) {
	return dependencyLockChanges(dir, baseSHA, changedFiles, "yarn.lock", evidence.YarnLockDependencyDiff)
}

func PnpmLockDependencyChanges(dir, baseSHA string, changedFiles []string) ([]evidence.DependencyChange, error) {
	return dependencyLockChanges(dir, baseSHA, changedFiles, "pnpm-lock.yaml", evidence.PnpmLockDependencyDiff)
}

func GemfileLockDependencyChanges(dir, baseSHA string, changedFiles []string) ([]evidence.DependencyChange, error) {
	return dependencyLockChanges(dir, baseSHA, changedFiles, "Gemfile.lock", evidence.GemfileLockDependencyDiff)
}

func PoetryLockDependencyChanges(dir, baseSHA string, changedFiles []string) ([]evidence.DependencyChange, error) {
	return dependencyLockChanges(dir, baseSHA, changedFiles, "poetry.lock", evidence.PoetryLockDependencyDiff)
}

func CargoLockDependencyChanges(dir, baseSHA string, changedFiles []string) ([]evidence.DependencyChange, error) {
	return dependencyLockChanges(dir, baseSHA, changedFiles, "Cargo.lock", evidence.CargoLockDependencyDiff)
}

// GitIsClean reports whether dir has no uncommitted changes (tracked or
// untracked).
func GitIsClean(dir string) (bool, error) {
	return GitIsCleanContext(context.Background(), dir)
}

// GitIsCleanContext is GitIsClean ended by ctx: git status reads files of
// the worktree (a .gitignore that is a FIFO blocks it for good), so a caller
// whose worktree a build wrote gives it a deadline.
func GitIsCleanContext(ctx context.Context, dir string) (bool, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", dir, "status", "--porcelain")
	cmd.WaitDelay = time.Second
	out, err := cmd.Output()
	if err != nil {
		if ctx.Err() != nil {
			return false, fmt.Errorf("git status: %w", ctx.Err())
		}
		return false, fmt.Errorf("git status: %w", err)
	}
	return len(out) == 0, nil
}

// GitStatusPaths returns the paths with uncommitted changes (tracked or
// untracked) in dir. Used alongside GitDiffNameOnly so a changed-file
// inventory captures modifications a step (like canonical verification
// running a formatter or codegen) left uncommitted, not only what's
// already in a commit — a diff-scope check run only against committed
// history would otherwise miss exactly that class of change.
//
// Uses `-z` (NUL-terminated, unquoted paths) for the same reason
// GitDiffNameOnly does: a path containing a newline or requiring quoting
// would otherwise come back as a quoted/escaped string that doesn't match
// the real filename — which, fed into a scope check, could wrongly flag
// (or wrongly clear) an actual path.
func GitStatusPaths(dir string) ([]string, error) {
	out, err := exec.Command("git", "-C", dir, "status", "--porcelain", "-z", "--no-renames", "--ignore-submodules=none").Output()
	if err != nil {
		return nil, fmt.Errorf("git status --porcelain -z: %w", err)
	}
	trimmed := strings.TrimRight(string(out), "\x00")
	if trimmed == "" {
		return []string{}, nil
	}
	// Each entry is one NUL-terminated "XY path" token. --no-renames
	// reports a moved file as a deletion and an addition, so both paths are
	// listed, as GitDiffNameOnly lists them. Should git still print a
	// rename/copy entry (X or Y is 'R' or 'C'), the NUL-terminated token
	// after it is the old path, and it is listed too: a file moved out of a
	// protected path changed that path.
	tokens := strings.Split(trimmed, "\x00")
	paths := []string{}
	for i := 0; i < len(tokens); i++ {
		entry := tokens[i]
		if len(entry) < 4 {
			continue
		}
		statusCode, path := entry[:2], entry[3:]
		paths = append(paths, path)
		if strings.ContainsAny(statusCode, "RC") && i+1 < len(tokens) {
			i++
			paths = append(paths, tokens[i])
		}
	}
	return paths, nil
}

// GitDiffNameOnly returns the files that changed between base and result
// in dir — the "changed-file inventory" evidence from the plan's Phase 4
// list. Returns an empty (non-nil) slice, not an error, when base and
// result are identical.
//
// Uses `-z` (NUL-terminated, unquoted output) rather than the default
// newline-terminated form: a path containing a newline or requiring
// quoting would otherwise come back as a quoted/escaped string that
// doesn't match the real filename, making the recorded inventory useless
// for reconciling against the actual repository.
//
// --no-renames: a moved file is listed under both its old and its new path.
// With rename detection (git's default) only the new path is listed, and a
// build that moved `.factory.yml`, a committed oracle or any other protected
// or out-of-scope file away had not, by this list, touched it.
// --ignore-submodules=none: a `.gitmodules` the build wrote (`ignore = all`)
// cannot hide a changed submodule entry. --no-ext-diff: the workspace's own
// `.gitattributes`/`.git/config` cannot name a command to run as its diff
// driver.
func GitDiffNameOnly(dir, base, result string) ([]string, error) {
	out, err := exec.Command("git", "-C", dir, "diff", "--name-only", "-z", "--no-renames", "--ignore-submodules=none", "--no-ext-diff", base, result).Output()
	if err != nil {
		return nil, fmt.Errorf("git diff --name-only %s %s: %w", base, result, err)
	}
	trimmed := strings.TrimRight(string(out), "\x00")
	if trimmed == "" {
		return []string{}, nil
	}
	return strings.Split(trimmed, "\x00"), nil
}

// gitShortStatRE parses a line like:
//
//	3 files changed, 12 insertions(+), 4 deletions(-)
//
// git omits whichever of "insertions"/"deletions" is zero, so both groups
// are optional and default to 0 when absent.
var gitShortStatRE = regexp.MustCompile(`(\d+) files? changed(?:, (\d+) insertions?\(\+\))?(?:, (\d+) deletions?\(-\))?`)

// GitDiffShortStatIncludingWorktree returns file/insertion/deletion counts
// for everything that has changed since base — both committed changes and
// whatever is still uncommitted in the working tree (tracked or
// untracked) — so it stays consistent with a ChangedFiles inventory built
// from GitDiffNameOnly unioned with GitStatusPaths. A two-commit
// `git diff --shortstat` alone would silently exclude uncommitted changes (e.g.
// left behind by a verify command that runs a formatter without
// committing), producing diff-size evidence that disagrees with the
// changed-file list recorded alongside it.
//
// git's own `diff <commit>` (one-commit, worktree-comparing form) already
// covers committed-since-base *and* uncommitted-but-tracked changes in
// one command. It never includes brand-new untracked files, though — git
// diff doesn't diff paths it doesn't know about — so this marks any
// untracked path as intent-to-add first (`git add -N`, which records the
// path without staging its content) and undoes that with `git reset`
// afterward, leaving the index exactly as found either way.
func GitDiffShortStatIncludingWorktree(dir, base string) (filesChanged, insertions, deletions int, err error) {
	gitFn, cleanup, err := worktreeInclusiveGit(dir)
	if err != nil {
		return 0, 0, 0, err
	}
	defer cleanup()

	out, err := gitFn("diff", "--shortstat", "--no-ext-diff", "--no-textconv", base).Output()
	if err != nil {
		return 0, 0, 0, fmt.Errorf("git diff --shortstat %s: %w", base, err)
	}
	line := strings.TrimSpace(string(out))
	if line == "" {
		return 0, 0, 0, nil
	}
	m := gitShortStatRE.FindStringSubmatch(line)
	if m == nil {
		return 0, 0, 0, fmt.Errorf("unrecognized git diff --shortstat output: %q", line)
	}
	return atoiOrZero(m[1]), atoiOrZero(m[2]), atoiOrZero(m[3]), nil
}

// MaxStoredDiffBytes bounds how much diff content GitDiffIncludingWorktreeToFile
// will keep on disk — and, by construction, how much a caller ever holds in
// memory at once. Found via review: an unbounded diff for a run with a very
// large text change would otherwise be buffered in full by exec.Cmd.Output,
// stored in full, and re-served in full on every read, with no limit on
// memory or on-disk size. Comfortably under Temporal's default 2 MiB
// Activity-result payload limit is not a constraint here — this content
// never travels through an Activity result at all (see
// GitDiffIncludingWorktreeToFile's doc comment).
const MaxStoredDiffBytes = 4 * 1024 * 1024

// GitDiffIncludingWorktreeToFile writes the full unified diff between base
// and the current worktree — committed changes since base *and* whatever is
// still uncommitted (tracked or untracked), the content counterpart to
// GitDiffShortStatIncludingWorktree's own counts, through the same
// worktreeInclusiveGit machinery — directly to destPath, and never buffers
// more than maxBytes of it in memory or on disk. Worktree-inclusive because
// a quarantined run's evidence collection deliberately leaves a failed
// build/verify's dirt uncommitted (so ResultSHA can equal BaseSHA), and a
// diff of BaseSHA..ResultSHA would omit exactly the content an operator
// most needs to inspect. Streamed because an exec.Cmd.Output call buffers
// the entire diff before any truncation could be applied, so a workspace
// containing a very large text change could make factoryd hold (and OOM
// on) hundreds of megabytes even though the stored result was capped —
// and, separately, returning that
// content through a Temporal Activity's return value risks exceeding
// Temporal's own default 2 MiB Activity-result payload limit regardless of
// this package's own cap. Writing straight to a file used as the Activity's
// only side effect (the durable per-run directory factoryd's CollectEvidenceActivity
// already writes logs to) avoids both: the Activity result carries only the
// small truncated flag this function returns, never the diff text itself.
func GitDiffIncludingWorktreeToFile(dir, base, destPath string, maxBytes int) (truncated bool, err error) {
	gitFn, cleanup, err := worktreeInclusiveGit(dir)
	if err != nil {
		return false, err
	}
	defer cleanup()

	out, err := os.OpenFile(destPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return false, fmt.Errorf("create diff file: %w", err)
	}

	cw := &cappedWriter{w: out, max: maxBytes}
	cmd := gitFn("diff", "--no-color", "--no-ext-diff", "--no-textconv", base)
	cmd.Stdout = cw
	var stderr strings.Builder
	cmd.Stderr = &stderr
	runErr := cmd.Run()
	closeErr := out.Close()
	if runErr != nil {
		return false, fmt.Errorf("git diff %s: %w: %s", base, runErr, stderr.String())
	}
	if closeErr != nil {
		return false, fmt.Errorf("close diff file: %w", closeErr)
	}
	if cw.truncated {
		if err := truncateFileAtLastNewline(destPath); err != nil {
			return false, fmt.Errorf("truncate diff file at line boundary: %w", err)
		}
	}
	return cw.truncated, nil
}

// cappedWriter writes at most max bytes to the underlying writer, silently
// discarding anything beyond that cap while still reporting every Write
// call as fully successful. io.Copy (and exec.Cmd's own stdout-copying
// goroutine) treats a Write returning fewer bytes than it was given as a
// fatal error and aborts — this cap needs to keep draining and discarding
// the process's remaining stdout instead, so the subprocess itself never
// sees a short write, a closed pipe, or a SIGPIPE.
type cappedWriter struct {
	w         io.Writer
	max       int
	written   int
	truncated bool
}

func (c *cappedWriter) Write(p []byte) (int, error) {
	if c.written >= c.max {
		c.truncated = true
		return len(p), nil
	}
	toWrite := p
	if remaining := c.max - c.written; len(p) > remaining {
		toWrite = p[:remaining]
		c.truncated = true
	}
	n, err := c.w.Write(toWrite)
	c.written += n
	if err != nil {
		return n, err
	}
	return len(p), nil
}

// truncateFileAtLastNewline cuts path back to its last newline, so a
// capped write ends on a whole line instead of splitting mid-line (or
// mid-UTF-8-rune) at an arbitrary byte offset. path is expected to be at
// most MaxStoredDiffBytes long by construction, so reading it back in full
// here stays bounded.
func truncateFileAtLastNewline(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if i := strings.LastIndexByte(string(data), '\n'); i >= 0 {
		return os.Truncate(path, int64(i+1))
	}
	return nil
}

// worktreeInclusiveGit returns a git command runner scoped to dir that
// operates against a private, temporary copy of the real index, with
// every untracked path already marked intent-to-add (`git add -A -N`) on
// that copy — so a single-ref `git diff <base>` run through it reflects
// the true current worktree (committed-since-base, uncommitted-but-
// tracked, and brand-new-untracked changes alike), not just what's been
// committed. The intent-to-add marks must land on this private copy, not
// the real index: a bare `git reset` afterward (the alternative to
// undo them) performs a full mixed reset to HEAD, which would also
// unstage anything genuinely staged before this ran (e.g. a verify
// command that ran `git add` without committing) — the real index is
// never opened for writing at all. Callers must call the returned
// cleanup func (removes the temp index file) even on error.
func worktreeInclusiveGit(dir string) (gitFn func(args ...string) *exec.Cmd, cleanup func(), err error) {
	gitDir, err := exec.Command("git", "-C", dir, "rev-parse", "--git-dir").Output()
	if err != nil {
		return nil, nil, fmt.Errorf("git rev-parse --git-dir: %w", err)
	}
	realIndexPath := strings.TrimSpace(string(gitDir))
	if !filepath.IsAbs(realIndexPath) {
		realIndexPath = filepath.Join(dir, realIndexPath)
	}
	realIndexPath = filepath.Join(realIndexPath, "index")

	tmpIndex, err := os.CreateTemp("", "factoryd-git-index-*")
	if err != nil {
		return nil, nil, fmt.Errorf("create temp index: %w", err)
	}
	tmpIndexPath := tmpIndex.Name()
	tmpIndex.Close()
	cleanup = func() { os.Remove(tmpIndexPath) }

	// A repo can legitimately have no index file yet (nothing ever
	// staged); git treats a missing GIT_INDEX_FILE the same way, as
	// empty, so only copy when the real one actually exists.
	if b, err := os.ReadFile(realIndexPath); err == nil {
		if err := os.WriteFile(tmpIndexPath, b, 0o644); err != nil {
			cleanup()
			return nil, nil, fmt.Errorf("seed temp index: %w", err)
		}
	} else if !os.IsNotExist(err) {
		cleanup()
		return nil, nil, fmt.Errorf("read real index: %w", err)
	}

	gitFn = func(args ...string) *exec.Cmd {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_INDEX_FILE="+tmpIndexPath)
		return cmd
	}
	if out, err := gitFn("add", "-A", "-N").CombinedOutput(); err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("git add -A -N: %w: %s", err, out)
	}
	return gitFn, cleanup, nil
}

func atoiOrZero(s string) int {
	if s == "" {
		return 0
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0
	}
	return n
}

// GitCommitAll stages every change in dir (tracked and untracked) and
// commits it with message. Used only as factoryd's own safety-net commit
// when an agent finishes with a verified but uncommitted diff — the
// agent's own commit is always preferred when it makes one.
//
// core.hooksPath=/dev/null (found via review, round 2): a sandboxed
// worker's writable /workspace mount can still contain a script or
// Makefile a repo's own hook delegates to, even though .git itself is
// mounted read-only — so this host-side commit, run with factoryd's own
// credentials and Docker access, would otherwise execute attacker-modified
// code through that hook. --no-verify alone is not enough (git-commit(1)
// documents it as skipping only pre-commit and commit-msg) — a
// prepare-commit-msg or post-commit hook would still fire. Pointing
// core.hooksPath at a location no hook file can exist under (verified:
// /dev/null is not a directory, so every hook lookup under it fails the
// same way a missing hook does — "no hook configured", not an error) skips
// every hook git-commit(1) documents, not just two of them. This is a
// control-plane safety-net commit of the worker's own already
// build/verify-evaluated output, not a place a repository's own commit
// hooks have anything legitimate left to check.
func GitCommitAll(dir, message string) error {
	if out, err := exec.Command("git", "-C", dir, "add", "-A").CombinedOutput(); err != nil {
		return fmt.Errorf("git add -A: %w: %s", err, out)
	}
	if out, err := exec.Command("git", "-C", dir, "-c", "core.hooksPath=/dev/null", "commit", "-m", message).CombinedOutput(); err != nil {
		return fmt.Errorf("git commit: %w: %s", err, out)
	}
	return nil
}
