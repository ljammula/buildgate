package workflow

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"

	"buildgate/internal/codereview"
	"buildgate/internal/evidence"
	"buildgate/internal/pincommit"
	"buildgate/internal/reproduction"
	"buildgate/internal/run"
	"buildgate/internal/runner"
	"buildgate/internal/sandbox"
	wsisolation "buildgate/internal/workspace"
)

// The challenger job (SC-022): one sandbox launch of the challenger role on a
// scratch worktree of the result commit, then the host's hold of what it
// wrote. Nothing the job says is read; the host takes its new test files
// from git, applies the shape rules and keeps its own copy of each.

// Names and caps of the challenger job and its hold.
const (
	// RunChallengerJobActivityName is the Activity that launches the job and
	// holds its files.
	RunChallengerJobActivityName = "RunChallengerJobActivity"
	// CommitChallengeActivityName commits the held files of a green challenge.
	CommitChallengeActivityName = "CommitChallengeActivity"

	challengeAttemptKind = "challenge"
	challengeRunDirName  = "challenge-run"
	challengeScript      = "challenge.py"
	maxChallengeFiles    = 5
	maxChallengeBytes    = 64 << 10
	maxChallengeSpec     = 1 << 20
	maxChallengePaths    = 200
	challengeHeaderLines = 5
	challengeJobLimit    = 45 * time.Minute
)

// challengeHeader is the one derivation header line a challenge file carries:
// a label and a number, read as nothing else.
var challengeHeader = regexp.MustCompile(`buildgate-challenge: (AC|answer)-([1-9][0-9]{0,3})\s*$`)

// ChallengerJobInput is RunChallengerJobActivity's input: the run's input and
// the result commit the job reads.
type ChallengerJobInput struct {
	RunWorkflowInput
	Commit                    string   `json:"commit"`
	ChangedPaths              []string `json:"changed_paths,omitempty"`
	PriorConsumedInputTokens  int64    `json:"prior_consumed_input_tokens,omitempty"`
	PriorConsumedOutputTokens int64    `json:"prior_consumed_output_tokens,omitempty"`
	PriorConsumedCostMicroUSD int64    `json:"prior_consumed_cost_micro_usd,omitempty"`
}

// ChallengerJobResult is what the job and the hold left: the files (held,
// refused or not run), notes for the pull request, the instruction snapshot,
// the launch's attempts, and Cause when no verdict can follow (no file was
// handed over, the job never ran, the spec did not match its hash).
type ChallengerJobResult struct {
	Files          []run.ChallengeFile `json:"files,omitempty"`
	Notes          []string            `json:"notes,omitempty"`
	Cause          string              `json:"cause,omitempty"`
	Attempts       []run.Attempt       `json:"attempts,omitempty"`
	SnapshotSHA256 string              `json:"snapshot_sha256,omitempty"`
	MaskedPaths    []string            `json:"masked_paths,omitempty"`
	RemovedPaths   []string            `json:"removed_paths,omitempty"`
}

// challengeHeaderOf is the criterion label of content's header: exactly one
// matching line in its first challengeHeaderLines lines, whose number is at
// most criteria (AC) or answers (answer). "" when there is none or it is not
// valid.
func challengeHeaderOf(content []byte, criteria, answers int) string {
	lines := strings.Split(string(content), "\n")
	if len(lines) > challengeHeaderLines {
		lines = lines[:challengeHeaderLines]
	}
	found := ""
	for _, line := range lines {
		m := challengeHeader.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		if found != "" {
			return ""
		}
		n, _ := strconv.Atoi(m[2])
		limit := criteria
		if m[1] == "answer" {
			limit = answers
		}
		if n < 1 || n > limit {
			return ""
		}
		found = m[1] + "-" + m[2]
	}
	return found
}

// RunChallengerJobActivity runs the challenger job once and holds its files.
// Every failure to run it is a Cause on the result, never an error: a
// challenge never halts a run (SC-022). Only a stopped worker or a lost lease
// is an error.
func (a *Activities) RunChallengerJobActivity(ctx context.Context, in ChallengerJobInput) (ChallengerJobResult, error) {
	ctx, endGuard := GuardStep(ctx)
	defer endGuard()
	if err := a.fenceEarlierAttempts(ctx, in.RunWorkflowInput, true); err != nil {
		return ChallengerJobResult{}, err
	}
	progressMark(ctx, a.logDirFor(in.RunWorkflowInput), challengeAttemptKind, "start", "", "")
	res := a.runChallengerJob(ctx, in)
	outcome := "pass"
	if res.Cause != "" {
		outcome = "fail"
	}
	progressMark(ctx, a.logDirFor(in.RunWorkflowInput), challengeAttemptKind, "end", outcome, "")
	if err := ctx.Err(); err != nil {
		return res, err
	}
	return res, nil
}

func (a *Activities) runChallengerJob(ctx context.Context, in ChallengerJobInput) (res ChallengerJobResult) {
	fail := func(format string, args ...any) ChallengerJobResult {
		res.Cause = fmt.Sprintf(format, args...)
		return res
	}
	if in.ChallengerRelayPolicy == nil || in.RoutePolicy == nil {
		return fail("no challenger role is configured")
	}
	spec, err := evidence.ReadHostileFile(in.ChallengeSpecPath, maxChallengeSpec)
	if err != nil {
		return fail("the approved spec could not be read: %v", err)
	}
	if sum := sha256.Sum256(spec); hex.EncodeToString(sum[:]) != in.ChallengeSpecSHA256 {
		return fail("the approved spec no longer matches the hash spec_review approved")
	}
	relay, err := a.challengerRelaySpec(ctx, in)
	if err != nil {
		return fail("the challenger's route could not be bound: %v", err)
	}
	root, err := filepath.Abs(filepath.Join(a.logDirFor(in.RunWorkflowInput), challengeRunDirName, "job"))
	if err != nil {
		return fail("resolve the challenger's scratch directory: %v", err)
	}
	if err := os.RemoveAll(root); err != nil || os.MkdirAll(root, 0o750) != nil {
		return fail("create the challenger's scratch directory")
	}
	scratch := filepath.Join(root, "worktree")
	defer func() {
		removeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), gateBaseRemoveLockWait)
		defer cancel()
		if err := removeGateBaseWorktree(removeCtx, in.WorkspacePath, scratch); err != nil {
			activity.GetLogger(ctx).Warn("the challenger's scratch worktree could not be removed", "path", scratch, "error", err.Error())
		}
		_ = os.RemoveAll(root)
	}()
	if err := wsisolation.AddScratchWorktree(ctx, in.WorkspacePath, scratch, in.Commit); err != nil {
		return fail("check out the result commit in a scratch worktree: %v", err)
	}
	if err := wsisolation.EnableWorkerGroupWrite(scratch, os.Getgid()); err != nil {
		return fail("make the scratch worktree writable for the worker: %v", err)
	}
	paths := in.ChangedPaths
	if len(paths) > maxChallengePaths {
		paths = paths[:maxChallengePaths]
	}
	pathsFile := filepath.Join(root, "changed-paths.txt")
	if err := os.WriteFile(pathsFile, []byte(strings.Join(paths, "\n")+"\n"), 0o444); err != nil {
		return fail("write the list of changed paths: %v", err)
	}
	onScratch := in.RunWorkflowInput
	onScratch.WorkspacePath, onScratch.SpecPath = scratch, in.ChallengeSpecPath
	stepIn := ReviewStepInput{RunWorkflowInput: onScratch, Step: challengeAttemptKind}
	prep, err := a.prepareReviewInstructions(ctx, stepIn, challengeAttemptKind, filepath.Join(root, "instructions"))
	defer func() { _ = prep.finish(ctx) }()
	if err != nil {
		return fail("the instruction files could not be set aside: %v", err)
	}
	res.SnapshotSHA256, res.MaskedPaths, res.RemovedPaths = prep.Snapshot.SHA256, cappedCleanPaths(prep.Snapshot.Paths), cappedCleanPaths(prep.Snapshot.Removed)

	script := filepath.Join(filepath.Dir(a.buildAppScriptFor(in.RunWorkflowInput)), challengeScript)
	args := []string{script, "--workspace", scratch, "--spec", in.ChallengeSpecPath, "--changed-paths", pathsFile, "--harness", harnessArg(in.ChallengerHarness)}
	if in.ChallengerThinking != "" {
		args = append(args, "--thinking", in.ChallengerThinking)
	}
	for _, c := range in.SetupCommands {
		args = append(args, "--setup-command", c)
	}
	args = prep.args(args)
	registrySpec, err := a.registryProxySpecFor(in.RunWorkflowInput)
	if err != nil {
		return fail("the registry proxy could not be prepared: %v", err)
	}
	composeSpec, err := a.composeServicesSpecFor(in.RunWorkflowInput, challengeAttemptKind)
	if err != nil {
		return fail("the compose services could not be prepared: %v", err)
	}
	logPath := activityLogPath(activityExecutionLogPath(ctx, a.logDirFor(in.RunWorkflowInput), "challenge.log"))
	after := func(attempt int, r runner.Result, _ error) error {
		res.Attempts = append(res.Attempts, challengeAttempt(r, logPath(attempt), relay, in))
		prep.record(&res.Attempts[len(res.Attempts)-1])
		return nil
	}
	launchCtx, cancel := context.WithTimeout(ctx, challengeJobLimit)
	defer cancel()
	launchCtx = withWorkspaceMasks(withExtraRunInputs(launchCtx, pathsFile), prep.Snapshot.Masks)
	if prep.Snapshot.DiffPath != "" {
		launchCtx = withExtraRunInputs(launchCtx, prep.Snapshot.DiffPath)
	}
	started := time.Now()
	result, runErr := heartbeatWhileRunning(activityHeartbeatInterval, func() {
		recordStepHeartbeat(ctx, HeartbeatDetails{Stage: challengeAttemptKind, Elapsed: time.Since(started)})
	}, func() (runner.Result, error) {
		return a.runSandboxWithRetries(launchCtx, onScratch, logPath, 1, func(int) error { return nil }, after, &relay, registrySpec, composeSpec, harnessEnv(in.ChallengerHarness), nil, a.buildAppInterpreterFor(in.RunWorkflowInput), args...)
	})
	if finishErr := prep.finish(ctx); finishErr != nil {
		return fail("the instruction stubs could not be removed: %v", finishErr)
	}
	if runErr != nil {
		return fail("the challenger's sandbox did not finish: %v", runErr)
	}
	if result.ExitCode != 0 {
		return fail("the challenger job ended with exit status %d and handed over no test", result.ExitCode)
	}
	files, notes, err := a.holdChallengeFiles(ctx, in, scratch)
	res.Files, res.Notes = files, notes
	if err != nil {
		return fail("the challenge files could not be held: %v", err)
	}
	if len(files) == 0 {
		return fail("the challenger handed over no test file")
	}
	return res
}

// challengeAttempt is the run record's attempt for the challenger launch.
func challengeAttempt(r runner.Result, logPath string, relay sandbox.RouteSpec, in ChallengerJobInput) run.Attempt {
	return run.Attempt{
		Kind: challengeAttemptKind, Command: r.Command, Role: run.AttemptRoleChallenger, Thinking: in.ChallengerThinking,
		StartedAt: r.StartedAt.Format(time.RFC3339), FinishedAt: r.FinishedAt.Format(time.RFC3339),
		ExitCode: r.ExitCode, LogPath: logPath, ImageDigest: r.ImageDigest, HarnessScriptsSHA256: r.ScriptsSHA256,
		RelayImageDigest: r.RelayImageDigest, RelayNetwork: r.RelayNetworkName, RelayContainerName: r.RelayContainerName,
		RelayUpstream: r.RelayUpstream, RelayCredentialMode: r.RelayCredentialMode, RelayRoute: r.RelayRoute,
		RelayBilling: r.RelayBilling, RelayWorkerModelID: r.RelayWorkerModelID, RelayReasoningEffort: r.RelayReasoningEffort,
		RelayConsumedInputTokens: r.RelayConsumedInputTokens, RelayConsumedOutputTokens: r.RelayConsumedOutputTokens,
		RelayConsumedCostMicroUSD: r.RelayConsumedCostMicroUSD, RelayCeilingExceeded: r.RelayCeilingExceeded,
		RelaySpendPartial: r.RelaySpendPartial, RelayCalls: run.AttemptCalls(r.RelayCalls),
		FactoryDirSHA256: r.FactoryDirSHA256, FactoryDirCommit: r.FactoryDirCommit, FactoryDirError: r.FactoryDirError,
	}
}

// challengerRelaySpec binds the challenger role's route on this Worker as the
// review's is (checkAndResolveRoute, then the phase relay spec over the
// build's own ceilings).
func (a *Activities) challengerRelaySpec(ctx context.Context, in ChallengerJobInput) (sandbox.RouteSpec, error) {
	if a.CheckRoute == nil {
		return sandbox.RouteSpec{}, errors.New("this Worker has no routes: config")
	}
	pol := in.ChallengerRelayPolicy
	if pol.Route == "" {
		return sandbox.RouteSpec{}, errors.New("the challenger policy names no route")
	}
	if err := a.CheckRoute(relayRoleExecution, *in.RoutePolicy, in.Thinking); err != nil {
		return sandbox.RouteSpec{}, err
	}
	base := in.RoutePolicy.Spec(sandbox.RouteSecret{}, sandbox.RouteSecret{}, sandbox.RouteSecret{}, sandbox.RouteSecret{}, a.runIDFor(in.RunWorkflowInput), a.dataDirFor(in.RunWorkflowInput))
	base.CABundlePath = a.EgressCABundlePath
	creds, err := a.checkAndResolveRoute(relayRoleChallenger, *pol, in.ChallengerThinking)
	if err != nil {
		return sandbox.RouteSpec{}, err
	}
	if err := pol.ValidateUpstreamScheme(); err != nil {
		return sandbox.RouteSpec{}, err
	}
	route := codereview.ReviewRelayFromPolicy(*pol, creds.APIKey, creds.GitHubToken, creds.ChatGPTToken, creds.ChatGPTAccountID)
	spec := codereview.PhaseRelaySpec(base, route, in.PriorConsumedInputTokens+in.PriorConsumedOutputTokens, in.PriorConsumedCostMicroUSD)
	if err := spec.Validate(); err != nil {
		return sandbox.RouteSpec{}, err
	}
	if err := a.capRelayForAttempt(ctx, in.RunWorkflowInput, &spec); err != nil {
		return sandbox.RouteSpec{}, err
	}
	return spec, nil
}

// untrackedPaths lists the files the scratch worktree holds that git does not
// track, NUL-separated (-z), by a host git that runs nothing of the
// repository's.
func untrackedPaths(ctx context.Context, scratch string) ([]string, error) {
	out, err := exec.CommandContext(ctx, "git", append(hostGitAfterSandbox(scratch), "ls-files", "--others", "-z")...).Output()
	if err != nil {
		return nil, fmt.Errorf("list the files the challenger added: %w", err)
	}
	var paths []string
	for _, p := range strings.Split(string(out), "\x00") {
		if p != "" {
			paths = append(paths, p)
		}
	}
	return paths, nil
}

// harnessStatePath reports a path the challenger's own harness or the job's
// session folder wrote: not a test file, and not worth a note.
func harnessStatePath(p string) bool {
	first, _, _ := strings.Cut(p, "/")
	return strings.HasPrefix(first, ".pi") || first == ".codex" || first == ".copilot" || first == ".buildgate-reproduction-report"
}

// holdChallengeFiles takes the new files of the scratch worktree through the
// shape rules and holds a copy of each accepted one (SC-022): a clean new
// relative path matching test_patterns, not an instruction, .factory/ or
// protected path, within the size and count caps. A file the rules refuse is a
// note naming the rule; one without a valid header is held and not run.
func (a *Activities) holdChallengeFiles(ctx context.Context, in ChallengerJobInput, scratch string) ([]run.ChallengeFile, []string, error) {
	untracked, err := untrackedPaths(ctx, scratch)
	if err != nil {
		return nil, nil, err
	}
	tracked, err := treePaths(ctx, in.WorkspacePath, in.Commit)
	if err != nil {
		return nil, nil, err
	}
	root, err := rootFactsAt(ctx, in.WorkspacePath, in.Commit, tracked)
	if err != nil {
		return nil, nil, err
	}
	judged := reproduction.Judged{TestPatterns: in.TestPatterns, Tracked: tracked, Protected: in.ReleaseProtectedPaths, Root: root}
	dir := filepath.Join(a.logDirFor(in.RunWorkflowInput), run.ChallengeDirName)
	var files []run.ChallengeFile
	var notes []string
	for _, p := range untracked {
		if harnessStatePath(p) {
			continue
		}
		file := run.ChallengeFile{Path: p}
		if len(files) >= maxChallengeFiles {
			file.Refused = fmt.Sprintf("more than %d files", maxChallengeFiles)
		} else if err := reproduction.CheckPath(p, judged); err != nil {
			file.Refused = err.Error()
		}
		var content []byte
		if file.Refused == "" {
			if content, err = readScratchFile(scratch, p, maxChallengeBytes); err != nil {
				file.Refused = err.Error()
			}
		}
		if file.Refused != "" {
			files = append(files, file)
			notes = append(notes, fmt.Sprintf("a file the challenger wrote was not held (%s): %s", file.Refused, cleanNoteName(p)))
			continue
		}
		acc := reproduction.Accepted{Path: p}
		if r, ok := reproduction.For(p, root); ok {
			if spans, err := r.Tests(p, content); err == nil {
				for _, s := range spans {
					acc.TestIDs = append(acc.TestIDs, s.ID)
				}
				if len(acc.TestIDs) > reproduction.MaxTestIDs {
					acc.TestIDs = nil
				}
				if len(acc.TestIDs) > 0 {
					acc.Runner = r.ID()
				}
			}
		}
		h, err := holdOne(dir, len(files), acc, content, false)
		if err != nil {
			return files, notes, err
		}
		file.SHA256, file.HostPath, file.Runner, file.TestIDs = h.SHA256, h.HostPath, h.Runner, h.TestIDs
		if file.Criterion = challengeHeaderOf(content, in.ChallengeCriteria, in.ChallengeAnswers); file.Criterion == "" {
			file.NotRun = "no valid derivation header"
			notes = append(notes, "a challenge file with no valid derivation header was held and not run: "+cleanNoteName(p))
		}
		files = append(files, file)
	}
	return files, notes, nil
}

// readScratchFile reads a regular file of the scratch worktree through an
// os.Root, so a link the job planted cannot reach outside it.
func readScratchFile(scratch, rel string, limit int64) ([]byte, error) {
	root, err := os.OpenRoot(scratch)
	if err != nil {
		return nil, errors.New("the worktree could not be opened")
	}
	defer root.Close()
	info, err := root.Lstat(filepath.FromSlash(rel))
	if err != nil || !info.Mode().IsRegular() {
		return nil, errors.New("the path is not a regular file")
	}
	if info.Size() > limit {
		return nil, fmt.Errorf("the file is over %d bytes", limit)
	}
	f, err := root.Open(filepath.FromSlash(rel))
	if err != nil {
		return nil, errors.New("the file could not be read")
	}
	defer f.Close()
	buf := make([]byte, info.Size())
	if _, err := f.Read(buf); err != nil && info.Size() > 0 {
		return nil, errors.New("the file could not be read")
	}
	return buf, nil
}

// cleanNoteName is a path a model chose, made one short line for a note.
func cleanNoteName(p string) string {
	p = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || r == '`' {
			return -1
		}
		return r
	}, p)
	if runes := []rune(p); len(runes) > 120 {
		p = string(runes[:120]) + "..."
	}
	return p
}

// CommitChallengeInput is CommitChallengeActivity's input: the run's input,
// the result commit the worktree is at and the files to commit.
type CommitChallengeInput struct {
	RunWorkflowInput
	ResultSHA string              `json:"result_sha"`
	Files     []run.ChallengeFile `json:"files"`
}

// CommitChallengeResult is the commit that holds the files.
type CommitChallengeResult struct {
	SHA string `json:"sha,omitempty"`
}

// CommitChallengeActivity commits the held files onto the run's branch as a
// child of the result commit with the host's one step that commits pinned
// files, which re-reads each committed file against its pinned hash (SC-022).
// The new commit is the run's ResultSHA.
func (a *Activities) CommitChallengeActivity(ctx context.Context, in CommitChallengeInput) (CommitChallengeResult, error) {
	pinned := make([]pincommit.PinnedFile, 0, len(in.Files))
	for _, f := range in.Files {
		pinned = append(pinned, pincommit.PinnedFile{Path: f.Path, HostPath: f.HostPath, SHA256: f.SHA256})
	}
	if _, err := pincommit.Commit(in.WorkspacePath, pinned, in.ResultSHA, "Add the challenge tests the approved criteria imply", in.ReleaseProtectedPaths); err != nil {
		return CommitChallengeResult{}, err
	}
	head, err := runner.GitRevParseHEAD(in.WorkspacePath)
	if err != nil {
		return CommitChallengeResult{}, fmt.Errorf("read the new commit: %w", err)
	}
	return CommitChallengeResult{SHA: head}, nil
}

// withChallengeMounts is ctx with the held challenge files of a challenge
// round's build mounted read-only at their paths (SC-022): each copy is read
// from its host path, checked against its pinned hash and staged outside the
// worktree, and an empty mountpoint is made for it, which the returned
// function removes again. The build can read a held file and never write it.
// ctx is unchanged for any other build.
func (a *Activities) withChallengeMounts(ctx context.Context, input RunWorkflowInput) (context.Context, func(), error) {
	if !input.ChallengeRound || len(input.ChallengeHeld) == 0 {
		return ctx, func() {}, nil
	}
	root, err := filepath.Abs(filepath.Join(a.logDirFor(input), challengeRunDirName, "mounts"))
	if err != nil {
		return ctx, func() {}, err
	}
	if err := os.RemoveAll(root); err != nil {
		return ctx, func() {}, err
	}
	if err := os.MkdirAll(root, 0o750); err != nil {
		return ctx, func() {}, err
	}
	var masks []sandbox.WorkspaceMask
	for i, f := range input.ChallengeHeld {
		content, err := evidence.ReadHostileFile(f.HostPath, reproduction.MaxHeldBytes)
		if err != nil {
			return ctx, func() {}, unheld("read the host copy: %v", err)
		}
		if err := checkHeldHash(content, f.SHA256); err != nil {
			return ctx, func() {}, err
		}
		staged := filepath.Join(root, strconv.Itoa(i))
		if err := os.WriteFile(staged, content, 0o444); err != nil {
			return ctx, func() {}, unheld("stage the held file: %v", err)
		}
		masks = append(masks, sandbox.WorkspaceMask{Source: staged, Target: f.Path, AbsentInWorktree: true})
	}
	manifest := reviewStubManifestPath(a.dataDirFor(input), input.WorkspacePath)
	if err := createReviewStubs(input.WorkspacePath, manifest, masks); err != nil {
		return ctx, func() {}, err
	}
	cleanup := func() {
		_, _ = removeReviewStubs(input.WorkspacePath, manifest)
		_ = os.RemoveAll(root)
	}
	return withWorkspaceMasks(ctx, masks), cleanup, nil
}

// buildLaunchInputs is what a build's sandbox launch is given besides its
// command: the execution role's skills, and, for a challenge round, the held
// challenge files mounted read-only (withChallengeMounts); the returned
// function removes the mountpoints and is always safe to call.
func (a *Activities) buildLaunchInputs(ctx context.Context, input RunWorkflowInput) ([]sandbox.SkillSource, context.Context, func(), error) {
	skills, err := a.boundSkills(relayRoleExecution, input.Skills)
	if err != nil {
		return nil, ctx, func() {}, err
	}
	mounted, unmount, err := a.withChallengeMounts(ctx, input)
	if err != nil {
		return nil, ctx, unmount, temporal.NewApplicationErrorWithCause("mount the held challenge files", InfrastructureFailureType, err)
	}
	return skills, mounted, unmount, nil
}
