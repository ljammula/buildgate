package workflow

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
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
	// UndoChallengeCommitActivityName moves the branch back to the result
	// commit when the verify command failed on the commit of the held files.
	UndoChallengeCommitActivityName = "UndoChallengeCommitActivity"

	challengeAttemptKind = "challenge"
	challengeRunDirName  = "challenge-run"
	challengeScript      = "challenge.py"
	maxChallengeFiles    = 5
	// maxChallengeRecords and maxChallengeNotes bound how many refused files a
	// run record and the pull request name; the rest are counted in one note.
	maxChallengeRecords  = 20
	maxChallengeNotes    = 20
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
// most criteria (AC) or one of the answered question numbers (answer). "" when there is none or it is not
// valid.
func challengeHeaderOf(content []byte, criteria int, answers []int) string {
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
		if n < 1 || (m[1] == "answer" && !slices.Contains(answers, n)) || (m[1] != "answer" && n > criteria) {
			return ""
		}
		found = m[1] + "-" + m[2]
	}
	return found
}

// challengeInScope reports whether header (as challengeHeaderOf returns it)
// cites a criterion the ticket or an earlier one covers. An answer-<n>, and
// any header when covered is empty, is in scope.
func challengeInScope(header string, covered []int) bool {
	n, ok := strings.CutPrefix(header, "AC-")
	if !ok || len(covered) == 0 {
		return true
	}
	v, err := strconv.Atoi(n)
	return err == nil && slices.Contains(covered, v)
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

// readApprovedChallengeSpec reads the spec snapshot the challenger is given
// and checks it twice: the whole snapshot against the hash taken with it, and
// its leading ChallengeApprovedBytes (the approved spec.md; the rest is the
// criteria-by-ID block and the read-back answers) against the hash
// spec_review approved. cause is "" when both match.
func readApprovedChallengeSpec(in RunWorkflowInput) (spec []byte, cause string) {
	spec, err := evidence.ReadHostileFile(in.ChallengeSpecPath, maxChallengeSpec)
	if err != nil {
		return nil, fmt.Sprintf("the approved spec could not be read: %v", err)
	}
	if sum := sha256.Sum256(spec); hex.EncodeToString(sum[:]) != in.ChallengeSpecSHA256 {
		return nil, "the approved spec no longer matches the hash spec_review approved"
	}
	n := in.ChallengeApprovedBytes
	if in.ChallengeApprovedSHA256 == "" || n < 1 || n > len(spec) {
		return nil, "no approved hash was given for the spec"
	}
	if sum := sha256.Sum256(spec[:n]); hex.EncodeToString(sum[:]) != in.ChallengeApprovedSHA256 {
		return nil, "the approved spec no longer matches the hash spec_review approved"
	}
	return spec, ""
}

func (a *Activities) runChallengerJob(ctx context.Context, in ChallengerJobInput) (res ChallengerJobResult) {
	fail := func(format string, args ...any) ChallengerJobResult {
		res.Cause = fmt.Sprintf(format, args...)
		return res
	}
	if in.ChallengerRelayPolicy == nil || in.RoutePolicy == nil {
		return fail("no challenger role is configured")
	}
	_, cause := readApprovedChallengeSpec(in.RunWorkflowInput)
	if cause != "" {
		return fail("%s", cause)
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
	// The files are held whatever the job's exit status: a turn the provider
	// cut off or that ran out of time may have left tests, and nothing the
	// job says, its exit status included, decides what the host takes.
	files, notes, err := a.holdChallengeFiles(ctx, in, scratch)
	res.Files, res.Notes = files, notes
	if err != nil {
		return fail("the challenge files could not be held: %v", err)
	}
	cause, note := challengeHoldOutcome(result.ExitCode, files)
	if cause != "" {
		return fail("%s", cause)
	}
	if note != "" {
		res.Notes = append(res.Notes, note)
	}
	return res
}

// challengeHoldOutcome is what the job's exit status and the files it left
// mean for the challenge: a cause (no verdict) when none was held (a file the
// shape rules refused is a note, not a held file), and a note when files were
// held from a job that did not exit 0.
func challengeHoldOutcome(exitCode int, files []run.ChallengeFile) (cause, note string) {
	held := 0
	for _, f := range files {
		if f.Refused == "" {
			held++
		}
	}
	switch {
	case held == 0 && exitCode != 0:
		return fmt.Sprintf("the challenger job ended with exit status %d and handed over no test", exitCode), ""
	case held == 0:
		return "the challenger handed over no test file", ""
	case exitCode != 0:
		return "", fmt.Sprintf("the challenger job ended with exit status %d; the test files it left were held", exitCode)
	}
	return "", ""
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
	out, err := exec.CommandContext(ctx, "git", append(hostGitAfterSandbox(scratch), "ls-files", "--others", "--exclude-standard", "-z")...).Output()
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
	facts, err := markerFactsAt(ctx, in.WorkspacePath, in.Commit, tracked, untracked)
	if err != nil {
		return nil, nil, err
	}
	judged := reproduction.Judged{TestPatterns: in.TestPatterns, Tracked: tracked, Protected: in.ReleaseProtectedPaths}
	dir := filepath.Join(a.logDirFor(in.RunWorkflowInput), run.ChallengeDirName)
	var files []run.ChallengeFile
	var notes []string
	held, unnoted, noCriteriaNoted := 0, 0, false
	for _, p := range untracked {
		if harnessStatePath(p) {
			continue
		}
		file := run.ChallengeFile{Path: p}
		// The cap counts files held, not files refused: refused files
		// (setup output, caches) must not use up the room a test needs.
		if held >= maxChallengeFiles {
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
			if len(files) < maxChallengeRecords {
				files = append(files, file)
			}
			if len(notes) < maxChallengeNotes {
				notes = append(notes, fmt.Sprintf("a file the challenger wrote was not held (%s): %s", file.Refused, cleanNoteName(p)))
			} else {
				unnoted++
			}
			continue
		}
		acc := reproduction.Accepted{Path: p}
		if r, runDir, ok := reproduction.For(p, facts); ok {
			if spans, err := r.Tests(p, content); err == nil {
				for _, s := range spans {
					acc.TestIDs = append(acc.TestIDs, s.ID)
				}
				if len(acc.TestIDs) > reproduction.MaxTestIDs {
					acc.TestIDs = nil
				}
				if len(acc.TestIDs) > 0 {
					acc.Runner, acc.Dir = r.ID(), runDir
				}
			}
		}
		h, err := holdOne(dir, len(files), acc, content, false)
		if err != nil {
			return files, notes, err
		}
		file.SHA256, file.HostPath, file.Runner, file.Dir, file.TestIDs = h.SHA256, h.HostPath, h.Runner, h.Dir, h.TestIDs
		if file.Criterion = challengeHeaderOf(content, in.ChallengeCriteria, in.ChallengeAnswers); file.Criterion == "" {
			file.NotRun = "no valid derivation header"
			notes = append(notes, "a challenge file with no valid derivation header was held and not run: "+cleanNoteName(p))
			if in.ChallengeCriteria == 0 && !noCriteriaNoted {
				noCriteriaNoted = true
				notes = append(notes, "the approved spec lists no numbered criteria under an exact `## Acceptance criteria` heading, so no AC-n header is valid")
			}
		}
		if file.NotRun == "" && !challengeInScope(file.Criterion, in.ChallengeCovered) {
			file.NotRun = "cites a criterion outside the ticket's and earlier tickets' coverage"
			notes = append(notes, fmt.Sprintf("a challenge file citing %s, a criterion outside the ticket's and earlier tickets' coverage, was held and not run: %s", file.Criterion, cleanNoteName(p)))
		}
		files = append(files, file)
		held++
	}
	if unnoted > 0 {
		notes = append(notes, fmt.Sprintf("%d more files the challenger wrote were not held", unnoted))
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

// UndoChallengeCommitInput is UndoChallengeCommitActivity's input: the run's
// input, the commit the worktree goes back to (the run's result before the
// challenge commit) and the challenge commit it leaves.
type UndoChallengeCommitInput struct {
	RunWorkflowInput
	ResultSHA string `json:"result_sha"`
	CommitSHA string `json:"commit_sha"`
}

// UndoChallengeCommitActivity hard-resets the worktree and its branch to the
// result commit, dropping the challenge commit CommitChallengeActivity made
// on top of it (SC-022). It refuses unless HEAD is that challenge commit and
// its parent is the result commit, so it never drops other work, and it checks
// HEAD is the result commit afterwards: a branch left on the commit is an
// error the workflow halts on.
func (a *Activities) UndoChallengeCommitActivity(ctx context.Context, in UndoChallengeCommitInput) error {
	if in.ResultSHA == "" || in.CommitSHA == "" {
		return errors.New("no result commit or challenge commit to undo")
	}
	head, err := runner.GitRevParseHEAD(in.WorkspacePath)
	if err != nil {
		return fmt.Errorf("read HEAD before the undo: %w", err)
	}
	if head != in.CommitSHA {
		return fmt.Errorf("HEAD is %s, not the challenge commit %s: not undoing", head, in.CommitSHA)
	}
	parent, err := runner.GitRevParseRef(in.WorkspacePath, "HEAD^")
	if err != nil {
		return fmt.Errorf("read the challenge commit's parent: %w", err)
	}
	if parent != in.ResultSHA {
		return fmt.Errorf("the challenge commit's parent is %s, not the result commit %s: not undoing", parent, in.ResultSHA)
	}
	if err := runner.GitResetHard(in.WorkspacePath, in.ResultSHA); err != nil {
		return err
	}
	head, err = runner.GitRevParseHEAD(in.WorkspacePath)
	if err != nil {
		return fmt.Errorf("read HEAD after the reset: %w", err)
	}
	if head != in.ResultSHA {
		return fmt.Errorf("HEAD is %s after the reset, want %s", head, in.ResultSHA)
	}
	return nil
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
	made, err := makeMountParents(input.WorkspacePath, masks)
	if err != nil {
		removeMountParents(made)
		return ctx, func() {}, err
	}
	if err := createReviewStubs(input.WorkspacePath, manifest, masks); err != nil {
		removeMountParents(made)
		return ctx, func() {}, err
	}
	unexclude, err := excludeMountsFromGit(input.WorkspacePath, input.RunID, masks)
	if err != nil {
		_, _ = removeReviewStubs(input.WorkspacePath, manifest)
		removeMountParents(made)
		return ctx, func() {}, err
	}
	cleanup := func() {
		unexclude()
		_, _ = removeReviewStubs(input.WorkspacePath, manifest)
		removeMountParents(made)
		_ = os.RemoveAll(root)
	}
	return withWorkspaceMasks(ctx, masks), cleanup, nil
}

// excludeMountsFromGit lists each mountpoint in the repository's
// info/exclude, so `git add -A` in the round's worktree never stages the
// empty stub a held file's mount leaves on the host (the build would commit
// an empty file where the held content is only visible through the mount).
// The file is the one every git call reads, the sandbox's included, so the
// entries are written to it: as a block of this round alone, under the git
// metadata lock (SC-011). The block's token is the run and its worktree, so a
// retry of the same activity replaces its block rather than adding another.
// The returned function removes that block and nothing else, so it never
// writes back a stale copy over lines another run added meanwhile; a removal
// that fails is logged, and the next attempt of the run replaces the block.
func excludeMountsFromGit(workDir, runID string, masks []sandbox.WorkspaceMask) (func(), error) {
	patterns := make([]string, 0, len(masks))
	for _, m := range masks {
		patterns = append(patterns, "/"+escapeExcludePattern(m.Target))
	}
	sum := sha256.Sum256([]byte(workDir))
	token := runID + "@" + hex.EncodeToString(sum[:6])
	release, err := wsisolation.ExcludeForRun(workDir, token, patterns)
	if err != nil {
		return nil, fmt.Errorf("write the git exclude file: %w", err)
	}
	return func() {
		if err := release(); err != nil {
			log.Printf("challenge round %s: could not remove its info/exclude block %q: %v", runID, token, err)
		}
	}, nil
}

// escapeExcludePattern escapes the characters a gitignore pattern reads as
// syntax, so a held path matches only itself.
func escapeExcludePattern(p string) string {
	return strings.NewReplacer(`\`, `\\`, "*", `\*`, "?", `\?`, "[", `\[`, "#", `\#`, "!", `\!`, " ", `\ `).Replace(p)
}

// makeMountParents creates the directories a held file's path needs that the
// worktree lacks (a test in a new directory), one component at a time with
// Mkdir so a link is never followed, and returns the ones it made, deepest
// first. A component that exists and is not a directory is left for
// createReviewStubs to refuse.
func makeMountParents(workDir string, masks []sandbox.WorkspaceMask) ([]string, error) {
	var made []string
	for _, m := range masks {
		if !filepath.IsLocal(filepath.FromSlash(m.Target)) {
			return made, fmt.Errorf("mask target %q is not inside the workspace", m.Target)
		}
		dir := workDir
		parts := strings.Split(m.Target, "/")
		for _, part := range parts[:len(parts)-1] {
			dir = filepath.Join(dir, part)
			if _, err := os.Lstat(dir); err == nil {
				continue
			}
			if err := os.Mkdir(dir, 0o755); err != nil {
				return made, fmt.Errorf("create the directory for held file %q: %w", m.Target, err)
			}
			made = append([]string{dir}, made...)
		}
	}
	return made, nil
}

// removeMountParents removes the directories makeMountParents made, deepest
// first, when still empty.
func removeMountParents(dirs []string) {
	for _, d := range dirs {
		_ = os.Remove(d)
	}
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
