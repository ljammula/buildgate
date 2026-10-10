package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"buildgate/internal/modelrole"
	"buildgate/internal/oraclecanary"
	"buildgate/internal/oraclecommit"
	"buildgate/internal/request"
	"buildgate/internal/requestdriver"
	"buildgate/internal/runner"
	"buildgate/internal/sandbox"
	"buildgate/internal/sanitize"
)

const (
	// oracleDraftScriptName is agent/pi/scripts/draft_acceptance_oracles.py's
	// own embedded harness filename (see specDraftScriptName).
	oracleDraftScriptName = "draft_acceptance_oracles.py"
	// oracleDraftScratchDirName is the job's scratch directory inside the
	// drafting workspace (the only writable mount a sandboxed worker has);
	// distinct from the script's own ".factory-oracle-draft" so the job can
	// clear both.
	oracleDraftScratchDirName = ".factory-oracle-out"
	// oracleDraftScriptScratchDirName is the script's own scratch directory
	// (draft_acceptance_oracles.DRAFT_RELATIVE_DIR's parent).
	oracleDraftScriptScratchDirName = ".factory-oracle-draft"

	oracleDraftContainerOutDir       = specDraftContainerWorkDir + "/" + oracleDraftScratchDirName + "/out"
	oracleDraftContainerEvidencePath = specDraftContainerWorkDir + "/" + oracleDraftScratchDirName + "/evidence.json"

	// Per-file and per-request caps, the same bounds the script applies
	// (draft_acceptance_oracles.MAX_ORACLE_FILE_BYTES/MAX_TOTAL_ORACLE_BYTES);
	// re-enforced here because the script's output is untrusted.
	maxDraftedOracleFileBytes  = 16 * 1024
	maxDraftedOracleTotalBytes = 64 * 1024

	oracleManifestName = "MANIFEST.json"
)

// oracleLaunch is everything the script launch is given. It is the job's
// complete model-facing input surface: the criteria file and the optional
// operator-feedback file (the only two host files staged into the sandbox),
// plus the workspace the script reads AGENTS.md and the file inventory from.
// request.md is deliberately not representable here.
type oracleLaunch struct {
	DataDir      string
	Script       string
	Workspace    string
	CriteriaPath string
	FeedbackPath string
	OutDir       string // host path of the script's output directory
	EvidencePath string // host path of the script's evidence JSON
	// Ecosystem is which language's drafting instructions the script sends
	// the model (--ecosystem): the host classifies the workspace once
	// (classifyDraftEcosystem) and always passes its answer explicitly, so
	// the Go and Python sides never need to independently re-derive the same
	// classification.
	Ecosystem draftEcosystem
	// TimeoutMinutes is the request's overall drafting budget, passed
	// through as the script's own --timeout-minutes (its fallback split
	// when run standalone). CriterionTimeoutMinutes/NCriteria are what the
	// job actually times the container against (see oracleSandboxDeadline):
	// the script runs one pi invocation PER criterion, each
	// bounded by CriterionTimeoutMinutes, so the container's own deadline
	// must cover all of them run sequentially, not just TimeoutMinutes.
	TimeoutMinutes          int
	CriterionTimeoutMinutes int
	NCriteria               int
	LogPath                 func(attempt int) string
	// RoleOverride is resolveRequestJobRole's own result for
	// modelrole.StageOracleDrafting -- the review role. Resolved once by
	// runOracleDraftJobIn (so a bad role/alias fails the job before any
	// launch) and carried here for launchOracleDraftScript, which needs it
	// both to append --thinking and to override the relay policy's own
	// worker model fields.
	RoleOverride requestJobRoleOverride
}

// launchOracleDraft runs the drafting script; a package variable so tests can
// stub the sandboxed launch (the same seam the other jobs get from their
// injectable runners) while exercising the real input assembly and output
// handling above and below it.
var launchOracleDraft = launchOracleDraftScript

// runOracleDraftJob is the production oracle-drafting runner: one sandboxed
// draft_acceptance_oracles.py launch per REQUEST, in its own drafting
// worktree, seeded only with the approved spec's acceptance criteria (plus the
// repo inventory and AGENTS.md the script assembles) and the operator's
// oracle_review feedback. Output lands in in.OracleDir through
// installDraftedOracle. When the drafting script itself is unavailable it
// records not_implemented with the reason instead of failing.
func runOracleDraftJob(ctx context.Context, in requestdriver.OracleDraftInput) (request.OracleDraft, error) {
	// Whatever a previous pass retained is stale for this one, however this one
	// ends (including before any launch).
	_ = os.RemoveAll(filepath.Join(request.Dir(in.DataDir, in.Request.ID), "logs", oracleFailureDirName))
	// A crash inside a previous swap can leave oracle/ missing beside an
	// oracle-old-* holding the operator's files: repair before anything reads
	// oracle/ (see recoverOracleDir).
	if err := recoverOracleDir(in.OracleDir); err != nil {
		return failedOracleDraft(err.Error()), nil
	}
	script, err := resolveHarnessScript(in.Cfg.OracleDraftScript, oracleDraftScriptName)
	if err == nil {
		if _, statErr := os.Stat(script); statErr != nil {
			err = statErr
		}
	}
	if err != nil {
		return request.OracleDraft{
			Status: request.OracleNotImplemented,
			Detail: fmt.Sprintf("the oracle drafting script is unavailable (%v): hand-write oracle/ (including RUN_COMMAND.txt) or approve to skip", err),
		}, nil
	}
	// The drafter's input is the approved spec; refuse to draft from one that
	// changed since approval (advanceOracleDrafting checked just before, this
	// keeps the job safe when called on its own).
	if err := requestdriver.VerifyApprovedHashes(in.DataDir, in.Request); err != nil {
		return request.OracleDraft{}, err
	}
	specContent, err := os.ReadFile(requestdriver.RequestSpecPath(in.DataDir, in.Request.ID))
	if err != nil {
		return request.OracleDraft{}, fmt.Errorf("read approved spec: %w", err)
	}
	criteria, err := request.SpecAcceptanceCriteria(string(specContent))
	if err != nil {
		return request.OracleDraft{}, err
	}
	if len(criteria) == 0 {
		return request.OracleDraft{}, errors.New("approved spec has no acceptance criteria to draft oracles for")
	}

	var draft request.OracleDraft
	err = withDraftingWorktree(in.DataDir, in.Request, func(job *request.Request) error {
		var jobErr error
		draft, jobErr = runOracleDraftJobIn(ctx, in, job, script, criteria, string(specContent))
		return jobErr
	})
	if err != nil {
		if ctx.Err() != nil {
			// A stopped daemon leaves the request as it is; the next pass re-runs.
			return request.OracleDraft{}, err
		}
		draft = failedOracleDraft(err.Error())
	}
	// A failed re-draft that follows a reject must not leave the rejected
	// draft approvable in oracle/ (Approve never reads OracleDraft.Status).
	if draft.Status == request.OracleDraftFailed && in.FeedbackPath != "" && in.Request.OracleDraft != nil {
		if n := quarantineRejectedDraft(in.OracleDir, in.Request.OracleDraft.Files); n > 0 {
			draft.Detail += fmt.Sprintf("; the %d rejected draft file(s) were moved to %s/ so they cannot be approved", n, oracleRejectedDirName)
		}
	}
	return draft, nil
}

func runOracleDraftJobIn(ctx context.Context, in requestdriver.OracleDraftInput, job *request.Request, script string, criteria []string, specText string) (request.OracleDraft, error) {
	cfg := in.Cfg
	scratchDir := filepath.Join(job.Workspace, oracleDraftScratchDirName)
	scriptScratch := filepath.Join(job.Workspace, oracleDraftScriptScratchDirName)
	cleanScratch := func() {
		_ = os.RemoveAll(scratchDir)
		_ = os.RemoveAll(scriptScratch)
	}
	cleanScratch()
	defer cleanScratch()

	logDir := filepath.Join(request.Dir(in.DataDir, job.ID), "logs")
	if err := os.MkdirAll(logDir, 0o750); err != nil {
		return request.OracleDraft{}, fmt.Errorf("create oracle-draft log dir: %w", err)
	}
	criteriaPath := filepath.Join(logDir, "oracle_criteria.md")
	if err := os.WriteFile(criteriaPath, []byte(strings.Join(criteria, "\n")+"\n"), 0o600); err != nil {
		return request.OracleDraft{}, fmt.Errorf("write oracle criteria file: %w", err)
	}
	defer os.Remove(criteriaPath)

	timeoutMinutes := cfg.OracleDraftTimeoutMinutes
	if timeoutMinutes <= 0 {
		timeoutMinutes = defaultOracleDraftTimeoutMinutes
	}
	launch := oracleLaunch{
		DataDir:                 in.DataDir,
		Script:                  script,
		Workspace:               job.Workspace,
		CriteriaPath:            criteriaPath,
		FeedbackPath:            in.FeedbackPath,
		OutDir:                  filepath.Join(scratchDir, "out"),
		EvidencePath:            filepath.Join(scratchDir, "evidence.json"),
		TimeoutMinutes:          timeoutMinutes,
		CriterionTimeoutMinutes: oracleCriterionTimeoutMinutes(timeoutMinutes, len(criteria)),
		NCriteria:               len(criteria),
		LogPath: func(attempt int) string {
			if attempt == 1 {
				return filepath.Join(logDir, "oracle_draft.log")
			}
			return filepath.Join(logDir, fmt.Sprintf("oracle_draft.attempt%d.log", attempt))
		},
	}
	if err := refuseRequestText(in.DataDir, job.ID, launch.CriteriaPath, launch.FeedbackPath); err != nil {
		return request.OracleDraft{}, err
	}

	var prev []string
	prevGenerated := ""
	if in.Request.OracleDraft != nil {
		prev = in.Request.OracleDraft.Files
		prevGenerated = in.Request.OracleDraft.GeneratedRunCommandSHA256
	}
	eco, skipEco := classifyDraftEcosystem(job.Workspace)
	if eco == "" && skipEco == "" {
		// Unrecognised workspace: Go instructions are the historic
		// conservative default (a workspace with no marker at all might
		// still be Go).
		eco = draftEcosystemGo
	}
	launch.Ecosystem = eco
	if skipEco != "" {
		detail := fmt.Sprintf("the target repository looks like %s and only Go and Python oracles are drafted automatically (the default sandbox image has no other test runner), so no model pass was spent: hand-write oracle/ test files with a project image that has the runner, or approve to skip", skipEco)
		// This path returns before launchOracleDraft ever runs, so oracle_draft.log
		// would otherwise never be written at all -- live symptom: an
		// operator finding it 0 bytes with no explanation why. Write the
		// same reason there, and give every criterion its own verdict
		// (ineligible, same reason) instead of only the aggregate Detail
		// above -- neither the model nor draft_acceptance_oracles.py ran,
		// so there is no richer per-criterion rationale to report.
		if err := os.WriteFile(launch.LogPath(1), []byte(detail+"\n"), 0o600); err != nil {
			log.Printf("request %s: write oracle-draft log for the ecosystem skip: %v", job.ID, err)
		}
		verdicts := make([]request.OracleCriterionVerdict, len(criteria))
		for i := range criteria {
			verdicts[i] = request.OracleCriterionVerdict{Number: i + 1, Eligible: false, Reason: detail}
		}
		return noneEligibleDraft(in.OracleDir, prev, detail, verdicts)
	}
	// Resolved only once a model pass will actually run: a repo the skip
	// branch above declines never needs the review role, so a bad roles.review
	// must not turn that none_eligible draft into a failure.
	// "" -- oracle drafting is the review role, which is never
	// requester-selectable (sessionconfig.ValidateRequestModels).
	roleOverride, roleErr := resolveRequestJobRole(cfg, modelrole.StageOracleDrafting, "oracle drafting", "", "")
	if roleErr != nil {
		return request.OracleDraft{}, roleErr
	}
	defer startActiveJob(in.DataDir, in.Request.ID, modelrole.StageOracleDrafting, roleOverride, time.Now())()
	launch.RoleOverride = roleOverride

	logPath := launch.LogPath(1)
	// res/err are declared here (not with := below) so fail, defined next,
	// can close over res -- fail's own Model/Thinking assignment then
	// covers every return through it (a launch that never completed, a
	// non-zero exit with no salvage, or installDraftedOracle itself
	// reporting failed) in one place, mirroring SpecEvidence/PlanEvidence's
	// identical "always record what role/relay this attempt actually ran
	// with, success or failure" treatment. res.RelayWorkerModelID is
	// correctly "" when launchOracleDraft itself never returned a result
	// (the ctx.Err()==nil, err!=nil branch below).
	var res runner.Result
	var err error
	fail := func(draft request.OracleDraft) request.OracleDraft {
		draft.Detail = sanitizeLogText(draft.Detail)
		retainOracleDraftFailure(logDir, logPath, draft.Detail, launch, scriptScratch)
		draft.Detail += oracleFailureTail(logPath)
		draft.Model = res.RelayWorkerModelID
		draft.Thinking = launch.RoleOverride.Thinking
		// Spend: see spec_draft_job.go's matching comment -- recorded even
		// on a failed pass, since a launch that ran and spent something
		// through the relay before failing still incurred a real cost.
		draft.Spend = jobSpendFromResult(res, launch.RoleOverride.Role, time.Now())
		return draft
	}
	launchStart := oracleDraftClock()
	res, err = launchOracleDraft(ctx, cfg, job, launch)
	launchElapsed := oracleDraftClock().Sub(launchStart)
	retainDraftPrompts(in.DataDir, job.ID, "oracle", job.Workspace, []string{oracleDraftScriptScratchDirName + "/session"})
	if err != nil {
		if ctx.Err() != nil {
			return request.OracleDraft{}, fmt.Errorf("draft_acceptance_oracles.py did not run to completion: %w", err)
		}
		return fail(failedOracleDraft(fmt.Sprintf("draft_acceptance_oracles.py did not run to completion: %v", err))), nil
	}
	if res.ExitCode != 0 {
		var salvageErr error
		if looksLikeScriptTimeout(res.ExitCode, launchElapsed, oracleScriptBudgetMinutes(launch.CriterionTimeoutMinutes, launch.NCriteria)) {
			draftDir := filepath.Join(scriptScratch, "ORACLES_DRAFT")
			if entries, readErr := os.ReadDir(draftDir); (readErr != nil && os.IsNotExist(readErr)) || (readErr == nil && len(entries) == 0) {
				return fail(failedOracleDraft(fmt.Sprintf("draft_acceptance_oracles.py exited %d; timed out; model wrote no files (log: %s)", res.ExitCode, logPath))), nil
			}
			draft, err := installDrafted(installInput{
				OutDir: launch.OutDir, EvidencePath: launch.EvidencePath, OracleDir: in.OracleDir,
				Workspace: job.Workspace, Criteria: criteria, SpecText: specText, PrevFiles: prev,
				PartialDir: draftDir, PrevGeneratedRunCommandSHA: prevGenerated,
			})
			if err == nil {
				draft.Model = res.RelayWorkerModelID
				draft.Thinking = launch.RoleOverride.Thinking
				draft.Spend = jobSpendFromResult(res, launch.RoleOverride.Role, time.Now())
				return draft, nil
			}
			salvageErr = err
		}
		msg := fmt.Sprintf("draft_acceptance_oracles.py exited %d", res.ExitCode)
		if salvageErr != nil {
			msg += fmt.Sprintf("; timed out, and the partial draft could not be kept: %v", salvageErr)
		}
		msg += fmt.Sprintf(" (log: %s)", logPath)
		// Any other non-zero exit is failed regardless of what the evidence says.
		return fail(failedOracleDraft(msg)), nil
	}
	draft := installDraftedOracle(installInput{
		OutDir: launch.OutDir, EvidencePath: launch.EvidencePath, OracleDir: in.OracleDir,
		Workspace: job.Workspace, Criteria: criteria, SpecText: specText, PrevFiles: prev, PrevGeneratedRunCommandSHA: prevGenerated,
	})
	if draft.Status == request.OracleDraftFailed {
		return fail(draft), nil
	}
	draft.Model = res.RelayWorkerModelID
	draft.Thinking = launch.RoleOverride.Thinking
	draft.Spend = jobSpendFromResult(res, launch.RoleOverride.Role, time.Now())
	if len(draft.CompileProblems) > 0 {
		return autoRedraft(ctx, in, job, launch, criteria, specText, draft, cleanScratch)
	}
	return draft, nil
}

// autoRedrafts is how many times a draft that fails the host's compile
// self-check is redrafted on its own, before the operator ever sees it.
const autoRedrafts = 1

const compileFeedbackHeader = "## Automatic compile check of your previous draft\n\nThe host type-checked the oracle files you wrote. The errors below do not depend on the target package (names the change will add are excluded), so the build would fail on them. Fix them and keep everything else that was right.\n\n"

// autoRedraft runs the drafting script once more with the first draft's compile
// problems appended to the operator's feedback, and returns the better outcome.
// The first draft is already installed, so every way this can go wrong (the
// launch fails, exits non-zero, or installs nothing usable) keeps it, flagged,
// with the reason added to its detail: a redraft can only improve on the first
// pass, never lose it. A second draft that still has problems is kept and
// flagged too; there is no third pass.
func autoRedraft(ctx context.Context, in requestdriver.OracleDraftInput, job *request.Request, launch oracleLaunch, criteria []string, specText string, first request.OracleDraft, cleanScratch func()) (request.OracleDraft, error) {
	keep := func(why string) (request.OracleDraft, error) {
		first.Detail += "; automatic redraft with the compile problems as feedback did not replace it (" + sanitizeLogText(why) + ")"
		return first, nil
	}
	logDir := filepath.Dir(launch.LogPath(1))
	feedbackPath := filepath.Join(logDir, "oracle_autofeedback.md")
	var body strings.Builder
	if launch.FeedbackPath != "" {
		if prior, err := readBounded(launch.FeedbackPath, requestdriver.MaxFeedbackBytes*2); err == nil {
			body.Write(prior)
			body.WriteString("\n\n")
		}
	}
	body.WriteString(compileFeedbackHeader)
	for _, p := range first.CompileProblems {
		body.WriteString("- " + p + "\n")
	}
	if err := os.WriteFile(feedbackPath, []byte(requestdriver.CapFeedback(body.String(), requestdriver.MaxFeedbackBytes, "## Oracle rejected ")), 0o600); err != nil {
		return keep(fmt.Sprintf("could not write the feedback file: %v", err))
	}
	defer os.Remove(feedbackPath)
	if err := refuseRequestText(in.DataDir, job.ID, feedbackPath); err != nil {
		return keep(err.Error())
	}
	cleanScratch()
	relaunch := launch
	relaunch.FeedbackPath = feedbackPath
	relaunch.LogPath = func(int) string { return filepath.Join(logDir, "oracle_draft.redraft.log") }
	res, err := launchOracleDraft(ctx, in.Cfg, job, relaunch)
	if err != nil {
		if ctx.Err() != nil {
			return request.OracleDraft{}, fmt.Errorf("draft_acceptance_oracles.py did not run to completion: %w", err)
		}
		// The redraft launch may have spent something through the relay
		// before failing -- accumulate it into first's own Spend before
		// falling back to keeping first, so a redraft's real cost is
		// never silently dropped just because the redraft itself did not
		// pan out. first.Spend.Add tolerates a nil first.Spend.
		first.Spend = first.Spend.Add(jobSpendFromResult(res, relaunch.RoleOverride.Role, time.Now()))
		return keep(fmt.Sprintf("the launch failed: %v", err))
	}
	first.Spend = first.Spend.Add(jobSpendFromResult(res, relaunch.RoleOverride.Role, time.Now()))
	if res.ExitCode != 0 {
		return keep(fmt.Sprintf("the script exited %d; log: %s", res.ExitCode, relaunch.LogPath(1)))
	}
	second := installDraftedOracle(installInput{
		OutDir: launch.OutDir, EvidencePath: launch.EvidencePath, OracleDir: in.OracleDir,
		Workspace: job.Workspace, Criteria: criteria, SpecText: specText, PrevFiles: first.Files, PrevGeneratedRunCommandSHA: first.GeneratedRunCommandSHA256,
	})
	if second.Status == request.OracleDraftFailed {
		return keep(second.Detail)
	}
	second.Model = res.RelayWorkerModelID
	second.Thinking = relaunch.RoleOverride.Thinking
	// first.Spend by now already sums the first pass's own spend (set at
	// this function's call site, oracle_draft_job.go's draft.Spend
	// assignment) plus this redraft's, so second carries the whole
	// stage's real cost, not just this last attempt's.
	second.Spend = first.Spend
	second.AutoRedrafted = true
	second.Detail += fmt.Sprintf("; redrafted automatically once (%d max) because the first draft failed the compile self-check: %s", autoRedrafts, strings.Join(firstN(first.CompileProblems, 3), "; "))
	return second, nil
}

func firstN(s []string, n int) []string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

const (
	// oracleFailureTailBytes bounds the slice of the script's output log copied
	// into a failed draft's Detail.
	oracleFailureTailBytes = 8 * 1024
	// oracleFailureRetainBytes bounds everything kept under oracleFailureDirName.
	oracleFailureRetainBytes = 256 * 1024
	// oracleFailureMaxEntries / oracleFailureMaxFiles / oracleFailureMaxNameLen
	// bound how much of a model-written directory is even looked at and kept
	// (retention is flat: subdirectories are never descended into).
	oracleFailureMaxEntries = 256
	oracleFailureMaxFiles   = 32
	oracleFailureMaxNameLen = 128
	// oracleFailureDirName is under the request's logs dir. The drafting
	// workspace scratch is deleted after every pass, so without this a failed
	// draft (found live 2026-09-20: exit 2 after ~9.5 minutes, empty log)
	// left nothing to diagnose it from.
	oracleFailureDirName = "oracle_draft_failed"
)

// oracleFailureTail returns the text appended to a failed draft's Detail: the
// last oracleFailureTailBytes of the script's combined output.
func oracleFailureTail(logPath string) string {
	tail := readTail(logPath, oracleFailureTailBytes)
	if tail == "" {
		return "; the script printed no output (log: " + logPath + ")"
	}
	return "\n--- script output (last bytes of " + logPath + ") ---\n" + tail
}

func readTail(path string, n int64) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return ""
	}
	offset := info.Size() - n
	if offset < 0 {
		offset = 0
	}
	buf := make([]byte, info.Size()-offset)
	read, _ := f.ReadAt(buf, offset)
	clean := sanitizeLogText(string(buf[:read]))
	if len(clean) > int(n) {
		clean = strings.ToValidUTF8(clean[len(clean)-int(n):], "")
	}
	return strings.TrimSpace(clean)
}

// sanitizeLogText makes script output safe to store in a request record and show
// in a console: ANSI sequences, control characters other than \n \t \r, and
// bidi/zero-width characters are dropped, and obvious credentials redacted.
// Moved to internal/sanitize so internal/triage -- which cannot import
// this package main -- can reuse the identical stripping instead of duplicating it; kept as a thin
// wrapper here so this file's many existing sanitizeLogText call sites
// need no change.
func sanitizeLogText(s string) string {
	return sanitize.Text(s)
}

// requireRealDir refuses a scratch directory the sandboxed model could have
// replaced with a symlink (or anything but a directory): reading through it
// would copy host files into the request record or the mounted oracle/.
func requireRealDir(p string) error {
	info, err := os.Lstat(p)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("%s is not a plain directory (symlink or special file): refusing to read through it", filepath.Base(p))
	}
	return nil
}

// retainOracleDraftFailure keeps, best effort and bounded, what a failed pass
// produced before the scratch directories are removed: a SUMMARY.txt (the
// failure detail and output tail), the script's evidence and output files, and
// the model's own draft directory (its MANIFEST.json and oracle files).
func retainOracleDraftFailure(logDir, logPath, detail string, launch oracleLaunch, scriptScratch string) {
	dest := filepath.Join(logDir, oracleFailureDirName)
	_ = os.RemoveAll(dest)
	if os.MkdirAll(dest, 0o750) != nil {
		return
	}
	summary := detail + "\n\n" + readTail(logPath, oracleFailureTailBytes) + "\n"
	if len(summary) > oracleFailureRetainBytes/4 {
		summary = summary[:oracleFailureRetainBytes/4]
	}
	_ = os.WriteFile(filepath.Join(dest, "SUMMARY.txt"), []byte(summary), 0o600)
	budget := int64(oracleFailureRetainBytes - len(summary))
	copyFlat := func(src, sub string) {
		if requireRealDir(filepath.Dir(src)) != nil || requireRealDir(src) != nil {
			return
		}
		dir, err := os.Open(src)
		if err != nil {
			return
		}
		defer dir.Close()
		entries, _ := dir.ReadDir(oracleFailureMaxEntries)
		copied := 0
		for _, e := range entries {
			if !e.Type().IsRegular() || len(e.Name()) > oracleFailureMaxNameLen || copied >= oracleFailureMaxFiles || !draftedNamePattern.MatchString(e.Name()) {
				continue
			}
			info, err := e.Info()
			if err != nil || info.Size() > budget {
				continue
			}
			content, err := readBounded(filepath.Join(src, e.Name()), budget)
			if err != nil {
				continue
			}
			if os.MkdirAll(filepath.Join(dest, sub), 0o750) != nil {
				return
			}
			if os.WriteFile(filepath.Join(dest, sub, e.Name()), content, 0o600) == nil {
				budget -= int64(len(content))
				copied++
			}
		}
	}
	if content, err := readBounded(launch.EvidencePath, maxOracleControlFileBytes); requireRealDir(filepath.Dir(launch.EvidencePath)) == nil && err == nil && int64(len(content)) <= budget {
		content = []byte(sanitizeLogText(string(content)))
		if os.WriteFile(filepath.Join(dest, "evidence.json"), content, 0o600) == nil {
			budget -= int64(len(content))
		}
	}
	copyFlat(launch.OutDir, "out")
	copyFlat(filepath.Join(scriptScratch, "ORACLES_DRAFT"), "draft")
}

// refuseRequestText is the request.md isolation guard: no file handed to the
// drafter may be the request's raw text (or the same file by another path).
// The drafting job builds its inputs from spec.md and oracle-feedback.md
// only; this makes a future edit that adds request.md fail loudly.
func refuseRequestText(dataDir, id string, inputs ...string) error {
	textPath := request.TextPath(dataDir, id)
	textInfo, statErr := os.Stat(textPath)
	for _, p := range inputs {
		if p == "" {
			continue
		}
		if p == textPath {
			return fmt.Errorf("oracle drafting must never read request.md, but %s was passed as an input", p)
		}
		if statErr == nil {
			if info, err := os.Stat(p); err == nil && os.SameFile(info, textInfo) {
				return fmt.Errorf("oracle drafting must never read request.md, but %s is the same file", p)
			}
		}
	}
	return nil
}

// oracleDraftArgs is the pure argv builder for draft_acceptance_oracles.py
// (index 0 is the script, like planTicketsArgs). feedback may be "".
// criterionTimeoutMinutes is always passed explicitly (--criterion-timeout-minutes),
// so the script's own per-criterion split (draft_acceptance_oracles.per_criterion_minutes,
// its fallback for a standalone/manual run) never has to agree independently
// with oracleCriterionTimeoutMinutes below -- this job always tells it exactly
// what to use.
// thinking is the review role's Pi reasoning-effort level (mirroring
// draftSpecArgs/planTicketsArgs' own parameter): "" omits --thinking
// entirely.
func oracleDraftArgs(script, workspace, criteria, feedback, outDir, evidencePath string, timeoutMinutes, criterionTimeoutMinutes int, eco draftEcosystem, thinking, harness string) []string {
	args := []string{
		script,
		"--workspace", workspace,
		"--criteria", criteria,
		"--out-dir", outDir,
		"--evidence", evidencePath,
		"--timeout-minutes", fmt.Sprint(timeoutMinutes),
		"--criterion-timeout-minutes", fmt.Sprint(criterionTimeoutMinutes),
	}
	if eco != "" {
		args = append(args, "--ecosystem", string(eco))
	}
	if feedback != "" {
		args = append(args, "--feedback", feedback)
	}
	if thinking != "" {
		args = append(args, "--thinking", thinking)
	}
	args = append(args, "--harness", harness)
	return args
}

// oracleCriterionTimeoutMinutes splits the request's overall drafting budget
// evenly across criteria, each of which gets its own pi invocation:
// max(floor, total/n), so a request with many criteria never gets an
// unworkably short per-criterion budget. Mirrors
// draft_acceptance_oracles.py's own per_criterion_minutes exactly; this
// value is what --criterion-timeout-minutes passes the script, so the two
// languages never need to keep the split formula in sync independently.
const oracleCriterionFloorMinutes = 3

func oracleCriterionTimeoutMinutes(totalMinutes, nCriteria int) int {
	if nCriteria <= 0 {
		return totalMinutes
	}
	per := totalMinutes / nCriteria
	if per < oracleCriterionFloorMinutes {
		per = oracleCriterionFloorMinutes
	}
	return per
}

// resolveOracleRelaySpec is a seam so a test can run the real sandbox launch
// without starting a relay.
var resolveOracleRelaySpec = resolveRequestJobRelaySpec

// oracleSandboxDeadline sizes the container's own deadline off the SUM of
// every criterion's own budget (they run strictly sequentially inside the
// one script invocation, one pi call per criterion), not just the
// request's overall --timeout-minutes: with the floor in
// oracleCriterionTimeoutMinutes, a request with many small criteria can add
// up to more than the configured overall budget, and the container must
// stay alive for all of it or a mid-run kill would silently lose whatever
// criteria hadn't run yet with no evidence explaining why. The extra margin
// beyond that sum comes from requestJobContainerDeadline (relay mandatory,
// no registry proxy or compose services here), the same shared margin/slack
// every drafting job's container deadline uses, so the container always
// outlives the script's own --criterion-timeout-minutes Pi timer by
// requestJobScriptTimeoutSlack.
func oracleSandboxDeadline(criterionTimeoutMinutes, nCriteria int) time.Duration {
	n := nCriteria
	if n <= 0 {
		n = 1
	}
	return requestJobContainerDeadline(time.Duration(oracleScriptBudgetMinutes(criterionTimeoutMinutes, n))*time.Minute+time.Duration(n)*oraclePerCriterionOverhead, false, 0)
}

// oraclePerCriterionOverhead is the script's own work per criterion outside
// its Pi timer (SIGTERM grace on a timeout, stream-drain joins, per-criterion
// setup), added to the container deadline so many timed-out criteria can't
// eat the shared slack before MANIFEST.json and evidence are written.
const oraclePerCriterionOverhead = 20 * time.Second

// oracleScriptBudgetMinutes is the script's real total Pi budget: the
// per-criterion minutes it is told, times the criteria count. This, not the
// configured overall timeout, is what an all-criteria timeout run takes, so
// both the container deadline and looksLikeScriptTimeout use it.
func oracleScriptBudgetMinutes(criterionTimeoutMinutes, nCriteria int) int {
	if nCriteria <= 0 {
		nCriteria = 1
	}
	return criterionTimeoutMinutes * nCriteria
}

// launchOracleDraftScript is the real launch: sandboxed through
// runSandboxWithRetries with its own relay label, exactly like the plan job.
// The criteria file is the staged "spec" input and the feedback file (when
// present) the second run input; nothing else is mounted at /inputs/run.
func launchOracleDraftScript(ctx context.Context, cfg requestdriver.WorkerConfig, job *request.Request, l oracleLaunch) (runner.Result, error) {
	interpreter := cfg.OracleDraftInterpreter
	if interpreter == "" {
		interpreter = "python3"
	}
	sandboxImage := cfg.SandboxImage
	if sandboxImage == "" && !cfg.AllowUnsandboxedSpecDraft {
		return runner.Result{}, fmt.Errorf("no sandbox image configured: run `make install` from the buildgate checkout (builds images from source and records them via `factoryd configure-images`), or pass -sandbox-image")
	}
	criterionMinutes := l.CriterionTimeoutMinutes
	if criterionMinutes <= 0 {
		criterionMinutes = oracleCriterionTimeoutMinutes(l.TimeoutMinutes, l.NCriteria)
	}
	if sandboxImage == "" {
		// Unsandboxed: only reachable under allowUnsandboxedSpecDraft (tests).
		args := oracleDraftArgs(l.Script, l.Workspace, l.CriteriaPath, l.FeedbackPath, l.OutDir, l.EvidencePath, l.TimeoutMinutes, criterionMinutes, l.Ecosystem, l.RoleOverride.Thinking, l.RoleOverride.Harness)
		return runner.RunWithRetries(ctx, l.Workspace, l.LogPath, 1, nil, interpreter, args[1:]...)
	}
	sandboxDataDir, err := requestdriver.SandboxDataDirFor(l.DataDir)
	if err != nil {
		return runner.Result{}, err
	}
	relaySpec, err := resolveOracleRelaySpec(cfg, "oracle drafting", job.ID, sandboxDataDir, l.RoleOverride)
	if err != nil {
		return runner.Result{}, err
	}
	args := oracleDraftArgs(l.Script, l.Workspace, l.CriteriaPath, l.FeedbackPath, oracleDraftContainerOutDir, oracleDraftContainerEvidencePath, l.TimeoutMinutes, criterionMinutes, l.Ecosystem, l.RoleOverride.Thinking, l.RoleOverride.Harness)
	sandboxCtx, cancel := context.WithTimeout(ctx, oracleSandboxDeadline(criterionMinutes, l.NCriteria))
	defer cancel()
	sandboxUser, workerUID := requestJobSandboxIdentity(cfg)
	return runSandboxWithRetriesVia(
		cfg.Sandboxes, cfg.MeterLedgerRoot, sandboxCtx, l.Workspace, l.CriteriaPath, l.FeedbackPath, l.Script, l.LogPath, 1,
		sandboxImage, cfg.Settings.SandboxDocker, sandboxUser, workerUID, job.ID, sandboxDataDir,
		cfg.Settings.SandboxMemory, cfg.Settings.SandboxCPUs, cfg.Settings.SandboxTmpfsSize,
		nil, relaySpec, nil, sandbox.RegistryProxyHooks{}, nil, sandbox.ComposeServicesHooks{},
		"", "", l.RoleOverride.WorkerEnv, l.RoleOverride.Skills, interpreter, args[1:]...,
	)
}

// oracleDraftEvidenceSchemaVersion is the schema_version
// draft_acceptance_oracles.py's own EVIDENCE_SCHEMA_VERSION constant must
// equal -- see spec_draft_job.go's specDraftEvidenceSchemaVersion doc
// comment for why readDraftEvidence rejects a mismatch outright rather
// than tolerating it the way BUILD_EVIDENCE.json's best-effort reader
// does.
const oracleDraftEvidenceSchemaVersion = 1

// oracleDraftEvidence is the slice of the script's evidence JSON this job
// reads: the status and dropped count it computed.
type oracleDraftEvidence struct {
	SchemaVersion int    `json:"schema_version"`
	Status        string `json:"status"`
	DroppedCount  int    `json:"dropped_count"`
	// Failures is draft_acceptance_oracles.py's own per-criterion "this
	// criterion's own pi invocation did not produce a usable result" list
	// -- always present in the evidence JSON regardless of overall status, even
	// though it is normally empty for none_eligible (every null there is a
	// legitimate judgment call, not a failure; see the script's own doc
	// comment on evidence `status`).
	Failures []oracleDraftFailureEntry `json:"failures"`
}

// oracleDraftFailureEntry mirrors one entry of draft_acceptance_oracles.py's
// own evidence.json "failures" list.
type oracleDraftFailureEntry struct {
	CriterionIndex int    `json:"criterion_index"`
	Reason         string `json:"reason"`
}

// maxOracleControlFileBytes bounds the evidence and manifest reads: both are
// written inside the sandbox on a workspace mount a model-spawned process can
// keep writing to, so neither may be read unbounded.
const maxOracleControlFileBytes = 64 * 1024

// draftedNamePattern is the only shape of installed file name: plain ASCII,
// no leading dot. Excluding non-ASCII outright makes Unicode-normalisation
// aliases (NFD vs NFC) of a reserved or hand-written name impossible; case
// aliases are caught separately with strings.EqualFold.
var draftedNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)

// criterionNumberPrefix is a list marker only: ASCII digits, a mandatory "." or
// ")", then whitespace. "12 rows returned." keeps its "12" and "1.2.3 is
// supported" its "1.2". Mirrored by criterion_key in agent/pi/scripts/build_app.py;
// both are pinned by testdata/criterion_key_vectors.json.
var criterionNumberPrefix = regexp.MustCompile(`^[ \t\r\n\f]*[0-9]+[.)][ \t\r\n\f]+`)

const criterionEdgeSpace = " \t\r\n\f"

// criterionKey is the comparison key for an echoed criterion: the number
// prefix, backticks, ASCII edge whitespace and trailing ".;:," are the only
// things not part of its identity (no substring, prefix, interior or fuzzy
// match). Backticks: on the 2026-09-24 codex-route bar run the model echoed
// `"kroger"` as "kroger" in 15 of 36 criteria, discarding each draft.
func criterionKey(s string) string {
	s = strings.ReplaceAll(s, "`", "")
	s = strings.Trim(criterionNumberPrefix.ReplaceAllString(s, ""), criterionEdgeSpace)
	return strings.TrimRight(s, criterionEdgeSpace+".;:,")
}

// readBounded reads a regular, non-symlink file of at most max bytes.
func readBounded(path string, max int64) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", filepath.Base(path))
	}
	if info.Size() > max {
		return nil, fmt.Errorf("%s is %d bytes, over the %d byte limit", filepath.Base(path), info.Size(), max)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, fmt.Errorf("%s grew past the %d byte limit while being read", filepath.Base(path), max)
	}
	return b, nil
}

// installInput is everything installDraftedOracle needs beyond the script's
// own output.
type installInput struct {
	OutDir, EvidencePath, OracleDir, Workspace string
	Criteria                                   []string // approved spec criteria, in order
	// SpecText is the approved spec.md's full text, used only to check a
	// Python oracle's imported module against names the spec itself states
	// (pythonOracleUnresolvedImportProblems) -- never anything more of the
	// spec than that name-matching.
	SpecText  string
	PrevFiles []string // files the previous pass installed
	// PartialDir, when set, is the model's own draft directory of a run that
	// timed out: installDrafted salvages what is valid in it instead of
	// reading OutDir and the evidence status.
	PartialDir string
	// PrevGeneratedRunCommandSHA is the hash of the RUN_COMMAND.txt the
	// previous pass generated (request.OracleDraft.GeneratedRunCommandSHA256):
	// only a file still holding exactly those bytes is replaced by a re-draft.
	PrevGeneratedRunCommandSHA string
}

// installDraftedOracle turns the script's scratch output into the request's
// oracle/ directory and a status record. It never returns an error and never
// leaves partial files: every failure yields status "failed" with the reason
// and oracle/ untouched. Trust model: the script ran beside a model on a
// writable mount, so its output (manifest and evidence included) is
// untrusted input, re-validated here on the host.
//
//   - RUN_COMMAND.txt is never taken from the model: a model-named one is
//     ignored (its manifest entry nulled), a case- or normalisation-alias of it,
//     or of any kept hand-written file, refuses the draft, and an operator's
//     file is carried across a re-draft byte for byte. The one file the host
//     writes itself is the generated multi-file Go command (planRunCommand),
//     and only where no operator-owned RUN_COMMAND.txt exists.
//   - Only manifest-named regular files are copied, flat, at most 16 KiB each
//     and 64 KiB in total. A dotfile, symlink or subdirectory anywhere in the
//     output refuses the whole draft.
//   - The manifest is rebuilt from validated fields (supersedes always empty).
//   - none_eligible writes no files and leaves no empty oracle/ directory.
//   - The staged directory must contain exactly the expected files and pass
//     oraclecanary.CheckDir before it replaces the old one.
func installDraftedOracle(in installInput) request.OracleDraft {
	draft, err := installDrafted(in)
	if err != nil {
		return failedOracleDraft(err.Error())
	}
	return draft
}

func failedOracleDraft(msg string) request.OracleDraft {
	if len(msg) > 300 {
		msg = msg[:300] + "..."
	}
	return request.OracleDraft{Status: request.OracleDraftFailed, Detail: "oracle drafting failed: " + msg}
}

func installDrafted(in installInput) (request.OracleDraft, error) {
	var ev oracleDraftEvidence
	readDir, partial := in.OutDir, in.PartialDir != ""
	if partial {
		readDir = in.PartialDir
	} else {
		var err error
		if ev, err = readDraftEvidence(in.EvidencePath); err != nil {
			return request.OracleDraft{}, err
		}
		switch ev.Status {
		case string(request.OracleNoneEligible):
			detail := "the model judged no acceptance criterion checkable by a deterministic test (one model pass, not authoritative): hand-write oracle/ test files, or approve to skip"
			if _, err := os.Lstat(filepath.Join(in.OracleDir, request.TicketOracleRunCommandFilename)); err == nil {
				detail = "the model judged no acceptance criterion checkable by a deterministic test (one model pass, not authoritative): add test files, or delete oracle/RUN_COMMAND.txt and approve to skip (a RUN_COMMAND.txt with no test files is refused at approval)"
			}
			// A raw, best-effort read of MANIFEST.json -- never through
			// readDraftOutputMode's full validation, which this status
			// deliberately skips (no oracle file was ever installed) -- so
			// the operator sees WHICH criterion got WHICH rationale, not
			// just the aggregate "none eligible" detail above.
			rawManifest, _ := readBounded(filepath.Join(in.OutDir, oracleManifestName), maxOracleControlFileBytes)
			return noneEligibleDraft(in.OracleDir, in.PrevFiles, detail, oracleCriterionVerdicts(rawManifest))
		case string(request.OracleDrafted), string(request.OracleDraftOverCap):
		case string(request.OracleDraftFailed):
			return request.OracleDraft{}, errors.New("the drafting script reported no usable manifest (model or route failure)")
		default:
			return request.OracleDraft{}, fmt.Errorf("drafting evidence has unknown status %q", ev.Status)
		}
	}

	if err := requireRealDir(filepath.Dir(readDir)); err != nil {
		return request.OracleDraft{}, fmt.Errorf("drafting output: %w", err)
	}
	if err := requireRealDir(readDir); err != nil {
		return request.OracleDraft{}, fmt.Errorf("drafting output: %w", err)
	}
	files, manifest, targets, ignoredRunCommand, err := readDraftOutputMode(readDir, in.Criteria, partial)
	if err != nil {
		return request.OracleDraft{}, err
	}
	if len(files) == 0 {
		return request.OracleDraft{}, fmt.Errorf("status %q but no usable oracle file was produced", ev.Status)
	}
	if err := requireDraftableEcosystem(files); err != nil {
		return request.OracleDraft{}, err
	}

	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	proposed, note := proposeOracleCommand(in.Workspace, names, targets)
	staged, replaced, generatedSHA := planRunCommand(in, files, names, proposed)
	if err := replaceOracleDir(in.OracleDir, replaced, staged, manifest); err != nil {
		return request.OracleDraft{}, err
	}
	detail := fmt.Sprintf("drafted %d oracle file(s): %s", len(names), strings.Join(names, ", "))
	if partial {
		detail = fmt.Sprintf("timed out; partial draft kept (%d of %d criteria); ", draftedCriteria(manifest), len(in.Criteria)) + detail
	}
	status := request.OracleDrafted
	if ev.Status == string(request.OracleDraftOverCap) {
		status = request.OracleDraftOverCap
		detail += fmt.Sprintf("; %d more dropped over the size cap", ev.DroppedCount)
	}
	if ignoredRunCommand {
		detail += "; ignored a model-written RUN_COMMAND.txt (operator-authored only)"
	}
	// The proposal note says the command is "shown for review, not
	// written"; once planRunCommand has written it as the generated
	// RUN_COMMAND.txt, that note contradicts the line below (seen live on a
	// Python draft, 2026-09-24), so only the generated line is reported.
	if note != "" && generatedSHA == "" {
		detail += "; " + note
	}
	if problems := goOracleDraftProblems(in.Workspace, names, files, targets); len(problems) > 0 {
		detail += "; Go check failed, approval will refuse until this is fixed (reject with feedback to re-draft): " + strings.Join(problems, "; ")
	}
	if problems := pythonOracleDraftProblems(names, files); len(problems) > 0 {
		detail += "; Python check failed, approval will refuse until this is fixed (reject with feedback to re-draft): " + strings.Join(problems, "; ")
	}
	if problems := pythonOracleImportConsistencyProblems(names, files); len(problems) > 0 {
		detail += "; Python oracles disagree on where a shared name lives, approval will refuse until this is fixed (reject with feedback to re-draft): " + strings.Join(problems, "; ")
	}
	if problems := pythonOracleUnresolvedImportProblems(in.Workspace, in.SpecText, names, files); len(problems) > 0 {
		detail += "; Python oracle imports an invented module, approval will refuse until this is fixed (reject with feedback to re-draft): " + strings.Join(problems, "; ")
	}
	check := selfCheckDrafted(in.Criteria, files, targets, manifest)
	if check.skipped != "" {
		detail += "; compile self-check skipped: " + check.skipped
	}
	if len(check.compile) > 0 {
		detail += "; compile self-check found problems the build would hit whatever the target package looks like (advisory; approval does not consult it): " + strings.Join(check.compile, "; ")
	}
	if len(check.spec) > 0 {
		detail += "; spec-example check (heuristic, may be wrong; compare the oracle with the spec's own examples): " + strings.Join(check.spec, "; ")
	}
	if generatedSHA != "" && goFileCount(names) > 0 {
		detail += fmt.Sprintf("; wrote a generated RUN_COMMAND.txt for these %d Go files (directory-scoped overlay, -run TestOracle): review or edit it before approving", goFileCount(names))
	} else if generatedSHA != "" {
		detail += fmt.Sprintf("; wrote a generated RUN_COMMAND.txt for these %d Python files (stdlib-only runner): review or edit it before approving", pythonFileCount(names))
	} else {
		detail += "; RUN_COMMAND.txt is yours to write before approving"
	}
	// Per-criterion verdicts from the same rebuilt manifest
	// manifestFileCriteria already reads above, overlaid with any
	// per-criterion failures the evidence recorded (a partial draft --
	// e.g. 2 of 7 criteria's own pi invocation failed -- reports exactly
	// which ones and why, not just the aggregate Detail).
	criteria := mergeOracleFailureVerdicts(oracleCriterionVerdicts(manifest), ev.Failures)
	return request.OracleDraft{
		Status: status, Detail: detail, ProposedCommand: proposed, Files: names,
		CompileProblems: check.compile, SpecWarnings: check.spec, GeneratedRunCommandSHA256: generatedSHA,
		Criteria: criteria,
	}, nil
}

// draftSelfCheck is what the host-side self-checks found in a drafted oracle.
type draftSelfCheck struct {
	compile []string
	spec    []string
	skipped string
}

const maxSelfCheckLineChars = 300

// selfCheckDrafted runs the two advisory checks over the drafted Go files:
// the type self-check (oraclecanary.TypeCheckGoOracles) and the spec-example
// contradiction heuristic (oraclecanary.SpecExampleWarnings). Neither runs
// anything from the draft, and neither can block it: a bad or missing result
// only changes what the operator is told.
func selfCheckDrafted(criteria []string, files map[string][]byte, targets map[string]string, manifest []byte) draftSelfCheck {
	clean := func(in []string) []string {
		var out []string
		for _, s := range in {
			s = sanitizeLogText(s)
			if len(s) > maxSelfCheckLineChars {
				s = strings.ToValidUTF8(s[:maxSelfCheckLineChars], "") + "..."
			}
			out = append(out, s)
		}
		return out
	}
	tc := oraclecanary.TypeCheckGoOracles(files, targets)
	return draftSelfCheck{
		compile: clean(tc.Problems),
		spec:    clean(oraclecanary.SpecExampleWarnings(criteria, files, manifestFileCriteria(manifest))),
		skipped: tc.Skipped,
	}
}

// oracleCriterionVerdicts builds request.OracleDraft.Criteria from a
// MANIFEST.json-shaped byte slice -- either the already-normalised bytes
// readDraftOutputMode rebuilds (the drafted/over_cap success path), or a
// raw, unvalidated read straight from the drafting job's own scratch
// output (the none_eligible path, which never calls readDraftOutputMode --
// see installDrafted's own none_eligible branch). Tolerant by design: this
// is receipt data for the operator, never a gate, so a manifest this
// function can't parse yields no verdicts rather than an error.
func oracleCriterionVerdicts(manifest []byte) []request.OracleCriterionVerdict {
	var entries []struct {
		CriterionIndex int     `json:"criterion_index"`
		OracleFile     *string `json:"oracle_file"`
		Rationale      string  `json:"rationale"`
	}
	if json.Unmarshal(manifest, &entries) != nil {
		return nil
	}
	var out []request.OracleCriterionVerdict
	for _, e := range entries {
		if e.CriterionIndex <= 0 {
			continue
		}
		out = append(out, request.OracleCriterionVerdict{
			Number:   e.CriterionIndex,
			Eligible: e.OracleFile != nil,
			Reason:   sanitizeLogText(e.Rationale),
		})
	}
	return out
}

// mergeOracleFailureVerdicts overlays ev's own per-criterion failures onto
// verdicts (from oracleCriterionVerdicts): a criterion whose own pi
// invocation never produced a usable result has no reliable manifest
// rationale for it, so its verdict comes from the failure reason instead,
// replacing (or, if the manifest had no entry for it at all, adding) one
// ineligible entry. Returns verdicts sorted by Number.
func mergeOracleFailureVerdicts(verdicts []request.OracleCriterionVerdict, failures []oracleDraftFailureEntry) []request.OracleCriterionVerdict {
	byNumber := make(map[int]request.OracleCriterionVerdict, len(verdicts))
	for _, v := range verdicts {
		byNumber[v.Number] = v
	}
	for _, f := range failures {
		if f.CriterionIndex <= 0 {
			continue
		}
		byNumber[f.CriterionIndex] = request.OracleCriterionVerdict{
			Number:   f.CriterionIndex,
			Eligible: false,
			Reason:   sanitizeLogText(f.Reason),
		}
	}
	if len(byNumber) == 0 {
		return nil
	}
	out := make([]request.OracleCriterionVerdict, 0, len(byNumber))
	for _, v := range byNumber {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Number < out[j].Number })
	return out
}

// manifestFileCriteria maps each oracle file the (already validated) manifest
// names to the 1-based criteria that name it.
func manifestFileCriteria(manifest []byte) map[string][]int {
	var entries []struct {
		OracleFile *string `json:"oracle_file"`
		Index      int     `json:"criterion_index"`
	}
	out := map[string][]int{}
	if json.Unmarshal(manifest, &entries) != nil {
		return out
	}
	for _, e := range entries {
		if e.OracleFile != nil && e.Index > 0 {
			out[*e.OracleFile] = append(out[*e.OracleFile], e.Index)
		}
	}
	return out
}

func oracleSHA256(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// goFileCount counts the Go test files among names.
func goFileCount(names []string) int {
	n := 0
	for _, name := range names {
		if eco, isTest, err := oraclecanary.Classify(name); err == nil && isTest && eco == oraclecanary.Go {
			n++
		}
	}
	return n
}

// pythonFileCount counts the Python test files among names.
func pythonFileCount(names []string) int {
	n := 0
	for _, name := range names {
		if eco, isTest, err := oraclecanary.Classify(name); err == nil && isTest && eco == oraclecanary.Python {
			n++
		}
	}
	return n
}

// planRunCommand decides whether the drafting job writes RUN_COMMAND.txt, and
// returns the file set to stage, the previous-draft file list to replace, and
// the hash of a generated command ("" when none is written).
//
// A command is generated for a multi-file Go oracle (proposeOracleCommand
// covers the single-file case as a suggestion only, unchanged) or for any
// Python oracle (PythonStdlibCommand's glob-based runner covers one file or
// several identically, so there is no single-file exception for Python),
// whose proposal exists, and only when the host can own the file: there is no
// RUN_COMMAND.txt, or it is byte-for-byte what the previous pass generated. An
// operator's own or edited RUN_COMMAND.txt is never overwritten, and it is
// carried across a re-draft as before. The generated file is not a model
// output: it is built by oraclecanary.GoMultiCommand/PythonStdlibCommand from
// validated file names (and, for Go, manifest target paths), and it goes
// through the same review, hash pin and canary as a hand-written one.
func planRunCommand(in installInput, files map[string][]byte, names []string, proposed string) (staged map[string][]byte, replaced []string, generatedSHA string) {
	staged, replaced = files, in.PrevFiles
	cmdPath := filepath.Join(in.OracleDir, request.TicketOracleRunCommandFilename)
	_, statErr := os.Lstat(cmdPath)
	present := statErr == nil
	existing, readErr := readBounded(cmdPath, 1<<20)
	prevGenerated := present && readErr == nil && in.PrevGeneratedRunCommandSHA != "" && oracleSHA256(existing) == in.PrevGeneratedRunCommandSHA
	if prevGenerated {
		replaced = append(append([]string{}, in.PrevFiles...), request.TicketOracleRunCommandFilename)
	}
	eligible := proposed != "" && (goFileCount(names) >= 2 || pythonFileCount(names) >= 1)
	if !eligible || (present && !prevGenerated) {
		return staged, replaced, ""
	}
	content := []byte(proposed + "\n")
	staged = make(map[string][]byte, len(files)+1)
	for k, v := range files {
		staged[k] = v
	}
	staged[request.TicketOracleRunCommandFilename] = content
	return staged, replaced, oracleSHA256(content)
}

// readDraftEvidence reads the script's evidence JSON from its scratch dir.
func readDraftEvidence(evidencePath string) (oracleDraftEvidence, error) {
	var ev oracleDraftEvidence
	if err := requireRealDir(filepath.Dir(evidencePath)); err != nil {
		return ev, fmt.Errorf("drafting scratch: %w", err)
	}
	raw, err := readBounded(evidencePath, maxOracleControlFileBytes)
	if err != nil {
		return ev, fmt.Errorf("read drafting evidence: %w", err)
	}
	if err := json.Unmarshal(raw, &ev); err != nil {
		return ev, fmt.Errorf("drafting evidence is not valid JSON: %w", err)
	}
	if ev.SchemaVersion != oracleDraftEvidenceSchemaVersion {
		return oracleDraftEvidence{}, fmt.Errorf("%s schema_version %d, this factoryd understands %d", evidencePath, ev.SchemaVersion, oracleDraftEvidenceSchemaVersion)
	}
	return ev, nil
}

// oracleDraftClock times the launch; a variable so tests can simulate a run
// that used its whole budget.
var oracleDraftClock = time.Now

const (
	// oracleScriptTimeoutExit is the script's exit code for every failure it
	// reports itself, a wall-clock kill included.
	oracleScriptTimeoutExit = 2
	// oracleTimeoutMargin is how far short of its budget a run may finish and
	// still count as having hit it (launch bookkeeping and rounding).
	oracleTimeoutMargin = 15 * time.Second
)

// looksLikeScriptTimeout decides on the host, from what the model cannot
// influence, whether a failed run was a wall-clock kill: the script's failure
// exit code AND a launch that took (nearly) the whole configured budget. The
// script's evidence file is model-writable and is deliberately not consulted,
// so a fast failure with a forged "timed_out" is never salvaged. Found live
// 2026-09-21: 7 and 13 valid files, MANIFEST.json included, were thrown away
// as "failed" after a timeout; the model's draft directory is then salvaged
// through the normal host-side validation in partial mode.
func looksLikeScriptTimeout(exitCode int, elapsed time.Duration, timeoutMinutes int) bool {
	return exitCode == oracleScriptTimeoutExit && elapsed >= time.Duration(timeoutMinutes)*time.Minute-oracleTimeoutMargin
}

// draftedCriteria counts the manifest entries that name an oracle file.
func draftedCriteria(manifest []byte) int {
	var entries []struct {
		OracleFile *string `json:"oracle_file"`
	}
	if json.Unmarshal(manifest, &entries) != nil {
		return 0
	}
	n := 0
	for _, e := range entries {
		if e.OracleFile != nil {
			n++
		}
	}
	return n
}

// goOracleDraftProblems statically checks each drafted .go oracle (parse and
// package clause against its target_path's directory) without compiling or
// running it; see oraclecanary.CheckGoOracle. The same check runs again at
// approval, so a flagged draft cannot be approved as is.
func goOracleDraftProblems(workspace string, names []string, files map[string][]byte, targets map[string]string) []string {
	var problems []string
	for _, name := range names {
		if !strings.HasSuffix(name, ".go") {
			continue
		}
		if err := oraclecanary.CheckGoOracle(workspace, targets[name], files[name]); err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", name, err))
		}
	}
	return problems
}

// pythonOracleDraftProblems statically checks each drafted .py oracle (parse,
// at least one top-level test function, no forbidden import/call) without
// running it; see oraclecanary.CheckPythonOracle. The same check runs again
// at approval, so a flagged draft cannot be approved as is.
func pythonOracleDraftProblems(names []string, files map[string][]byte) []string {
	var problems []string
	for _, name := range names {
		if !strings.HasSuffix(name, ".py") {
			continue
		}
		if err := oraclecanary.CheckPythonOracle(files[name]); err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", name, err))
		}
	}
	return problems
}

// pythonOracleImportConsistencyProblems flags a drafted Python oracle set
// where the same imported name comes from two different modules across
// files: each criterion is drafted in its own isolated pi invocation (this
// job's own prior_imports addition helps the model avoid this, but doesn't
// guarantee it), so this is the host's own static backstop over the
// ASSEMBLED set. Same check runs again at approval (internal/request/
// oracle_validate.go's own pythonImportConsistencyProblems), so a flagged
// draft cannot be approved as is -- see oraclecanary.PythonImportConflicts's
// own doc comment for the live defect this closes.
func pythonOracleImportConsistencyProblems(names []string, files map[string][]byte) []string {
	imports := map[string]map[string]string{}
	for _, name := range names {
		if !strings.HasSuffix(name, ".py") {
			continue
		}
		bindings, err := oraclecanary.PythonOracleFromImports(files[name])
		if err != nil {
			// A parse failure is already reported by pythonOracleDraftProblems.
			continue
		}
		imports[name] = bindings
	}
	return oraclecanary.PythonImportConflicts(imports)
}

// pythonOracleUnresolvedImportProblems flags a drafted Python oracle
// importing a module that is neither a real module in the workspace nor
// named anywhere in the approved spec text: the approved spec abstracting a
// file/module name away no longer means the drafter is free to invent one
// (draft_spec.py's own prompt now asks the spec drafter to carry such names
// over verbatim -- see its Scope/Acceptance-criteria instructions), so an
// import resolving to neither is a guess, not a name grounded in either the
// real repository or the human-approved spec. Same check runs again at
// approval (internal/request/oracle_validate.go's own
// pythonModuleResolutionProblems).
func pythonOracleUnresolvedImportProblems(workspace, specText string, names []string, files map[string][]byte) []string {
	var problems []string
	for _, name := range names {
		if !strings.HasSuffix(name, ".py") {
			continue
		}
		bindings, err := oraclecanary.PythonOracleImports(files[name])
		if err != nil {
			// A parse failure is already reported by pythonOracleDraftProblems.
			continue
		}
		for _, module := range oraclecanary.PythonUnresolvedModules(bindings, workspace, specText) {
			problems = append(problems, fmt.Sprintf("oracle %s imports module %q, which neither exists in the repository nor is named in the approved spec", name, module))
		}
	}
	return problems
}

// noneEligibleDraft records a none_eligible outcome: it clears the previous
// pass's files (no empty oracle/ is left) and returns the status with detail.
func noneEligibleDraft(oracleDir string, prevFiles []string, detail string, criteria []request.OracleCriterionVerdict) (request.OracleDraft, error) {
	if err := replaceOracleDir(oracleDir, prevFiles, nil, nil); err != nil {
		return request.OracleDraft{}, err
	}
	return request.OracleDraft{Status: request.OracleNoneEligible, Detail: detail, Criteria: criteria}, nil
}

// draftEcosystem is the ecosystem name the drafter is told to write for, sent
// to draft_acceptance_oracles.py as --ecosystem so the two languages never
// need to independently re-derive the same workspace classification.
type draftEcosystem string

const (
	draftEcosystemGo     draftEcosystem = "go"
	draftEcosystemPython draftEcosystem = "python"
)

// classifyDraftEcosystem decides which ecosystem's drafting instructions to
// send the model: "go" or "python" when the workspace clearly belongs to one
// of the two the default sandbox image can run oracles for (a Go toolchain,
// or plain python3 with no extra test runner -- see PythonStdlibCommand); ""
// with a human-readable ecosystem name when the workspace clearly belongs to
// one this drafter does not support yet (skipDetail names it, and the caller
// declines before spending a model pass); or "", "" when nothing is
// recognised, in which case Go instructions are sent as the historic
// conservative default (a workspace with no marker at all might still be
// Go). A polyglot monorepo with Go only in a nested module counts as Go.
//
// Order matters: an explicit Python project marker (pyproject.toml,
// requirements.txt, setup.py) means Python outright, but the weaker "bare
// top-level *.py file" signal is checked only AFTER the JS/Dart markers --
// found in review, onboarding P4: a JS/Dart repository that also happens to
// carry a root build.py (a build script, not a Python project) was
// misclassified as Python because the bare-*.py fallback used to run before
// the package.json/pubspec.yaml check.
func classifyDraftEcosystem(workspace string) (eco draftEcosystem, skipDetail string) {
	if workspaceHasGo(workspace) {
		return draftEcosystemGo, ""
	}
	if workspaceHasExplicitPythonMarker(workspace) {
		return draftEcosystemPython, ""
	}
	exists := func(name string) bool { _, err := os.Lstat(filepath.Join(workspace, name)); return err == nil }
	for _, m := range []struct{ marker, eco string }{
		{"package.json", "a JavaScript/TypeScript project"},
		{"pubspec.yaml", "a Dart/Flutter project"},
	} {
		if exists(m.marker) {
			return "", m.eco
		}
	}
	if workspaceHasBareTopLevelPyFile(workspace) {
		return draftEcosystemPython, ""
	}
	return "", ""
}

// workspaceHasExplicitPythonMarker reports whether the workspace root
// carries a Python project marker (pyproject.toml, requirements.txt,
// setup.py) -- a strong signal that always means Python, checked before any
// other ecosystem's markers.
func workspaceHasExplicitPythonMarker(workspace string) bool {
	exists := func(name string) bool { _, err := os.Lstat(filepath.Join(workspace, name)); return err == nil }
	for _, m := range []string{"pyproject.toml", "requirements.txt", "setup.py"} {
		if exists(m) {
			return true
		}
	}
	return false
}

// workspaceHasBareTopLevelPyFile reports whether the workspace root has at
// least one plain top-level *.py file with no Python project marker present
// (found live 2026-09-24: a bare repository like math_ops with only add.py
// and no pyproject.toml/requirements.txt/setup.py previously fell through to
// the Go-only model pass, which then marked every criterion ineligible
// because its prompt said Go only). This is the weak fallback signal:
// classifyDraftEcosystem only consults it after ruling out every other
// ecosystem's own markers, since a *.py file alone does not outweigh a real
// JS/Dart/Go project marker. Root-level only, matching the shallow marker
// checks above: a nested Python project several directories down is not
// enough to call the whole workspace Python.
func workspaceHasBareTopLevelPyFile(workspace string) bool {
	entries, err := os.ReadDir(workspace)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if e.Type().IsRegular() && strings.HasSuffix(e.Name(), ".py") {
			return true
		}
	}
	return false
}

const (
	goSearchMaxDepth   = 4
	goSearchMaxEntries = 20000
)

// workspaceHasGo reports whether a go.mod, go.work or .go file exists within
// goSearchMaxDepth levels, skipping vendored/tool directories, never following
// symlinks and visiting at most goSearchMaxEntries entries.
func workspaceHasGo(workspace string) bool {
	found := false
	visited := 0
	_ = filepath.WalkDir(workspace, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if visited++; visited > goSearchMaxEntries {
			return filepath.SkipAll
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "vendor", ".buildgate", ".oracle":
				return filepath.SkipDir
			}
			if rel, relErr := filepath.Rel(workspace, path); relErr == nil && rel != "." && strings.Count(rel, string(filepath.Separator)) >= goSearchMaxDepth {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		if n := d.Name(); n == "go.mod" || n == "go.work" || strings.HasSuffix(n, ".go") {
			found = true
			return filepath.SkipAll
		}
		return nil
	})
	return found
}

func isReservedOracleName(name string) bool {
	return strings.EqualFold(name, request.TicketOracleRunCommandFilename) || strings.EqualFold(name, oracleManifestName)
}

// readDraftOutput validates the script's output directory against the
// approved criteria and returns the files to install (name -> bytes), a
// manifest rebuilt on the host from validated fields, and each installed
// file's target_path.
func readDraftOutput(outDir string, criteria []string) (files map[string][]byte, manifest []byte, targets map[string]string, ignoredRunCommand bool, err error) {
	return readDraftOutputMode(outDir, criteria, false)
}

// readDraftOutputMode is readDraftOutput with an optional partial mode, used
// only to salvage a timed-out draft directory: entries are matched to criteria
// by their (normalised) criterion text rather than by position, a criterion
// with no entry, or whose named file is missing, unreadable, oversize or over
// the total cap, is left undrafted (oracle_file null) instead of refusing the
// whole draft, and unnamed stray entries in the directory are ignored (only
// manifest-named regular files are ever copied). Every other check still
// refuses the draft.
func readDraftOutputMode(outDir string, criteria []string, partial bool) (files map[string][]byte, manifest []byte, targets map[string]string, ignoredRunCommand bool, err error) {
	in, err := readDraftManifestEntries(outDir, criteria, partial)
	if err != nil {
		return nil, nil, nil, false, err
	}
	d := &draftOutput{
		outDir:      outDir,
		partial:     partial,
		files:       map[string][]byte{},
		targets:     map[string]string{},
		targetOwner: map[string]string{},
		seen:        map[string]string{},
	}
	out := make([]map[string]any, 0, len(in))
	for i, entry := range in {
		row, err := d.manifestRow(i, entry, criteria[i])
		if err != nil {
			return nil, nil, nil, false, err
		}
		out = append(out, row)
	}
	manifest, err = json.MarshalIndent(out, "", "  ")
	if err != nil {
		return nil, nil, nil, false, fmt.Errorf("%v", err)
	}
	return d.files, append(manifest, '\n'), d.targets, d.ignoredRunCommand, nil
}

// readDraftManifestEntries refuses a drafting output directory that holds
// anything but plain files (skipped in partial mode), reads its manifest and
// returns the manifest's entries, one per criterion: aligned to the criteria
// by text in partial mode, refused on a count mismatch otherwise.
func readDraftManifestEntries(outDir string, criteria []string, partial bool) ([]map[string]any, error) {
	entries, err := os.ReadDir(outDir)
	if err != nil {
		return nil, fmt.Errorf("read drafting output: %v", err)
	}
	for _, e := range entries {
		if partial {
			break
		}
		if strings.HasPrefix(e.Name(), ".") || !e.Type().IsRegular() {
			return nil, fmt.Errorf("drafting output contains %q, which is a dotfile, symlink or directory: refusing the whole draft", e.Name())
		}
	}
	rawManifest, err := readBounded(filepath.Join(outDir, oracleManifestName), maxOracleControlFileBytes)
	if err != nil {
		return nil, fmt.Errorf("read drafted manifest: %v", err)
	}
	var in []map[string]any
	if err := json.Unmarshal(rawManifest, &in); err != nil {
		return nil, fmt.Errorf("drafted manifest is malformed: %v", err)
	}
	for i := range criteria {
		for j := 0; j < i; j++ {
			if criterionKey(criteria[i]) == criterionKey(criteria[j]) {
				return nil, fmt.Errorf("criteria %d and %d are the same after normalisation, so an echoed criterion cannot be matched to one of them", j+1, i+1)
			}
		}
	}
	if partial {
		in = alignPartialManifest(in, criteria)
	}
	if len(in) != len(criteria) {
		return nil, fmt.Errorf("drafted manifest has %d entries, want one per criterion (%d)", len(in), len(criteria))
	}
	return in, nil
}

// draftOutput is what readDraftOutputMode has accepted so far while it walks
// the manifest's entries in order.
type draftOutput struct {
	outDir  string
	partial bool

	files             map[string][]byte
	targets           map[string]string
	targetOwner       map[string]string // target_path -> the file that claimed it
	seen              map[string]string // lower-cased name -> the exact name already claimed
	total             int
	ignoredRunCommand bool
}

// manifestRow validates entry i against its approved criterion and returns
// the row the host-built manifest records for it.
func (d *draftOutput) manifestRow(i int, entry map[string]any, criterion string) (map[string]any, error) {
	crit, ok := entry["criterion"].(string)
	if !ok || criterionKey(crit) != criterionKey(criterion) {
		return nil, fmt.Errorf("drafted manifest entry %d does not match approved criterion %d", i+1, i+1)
	}
	if idx, ok := entry["criterion_index"].(float64); !ok || idx != float64(i+1) {
		return nil, fmt.Errorf("drafted manifest entry %d has criterion_index %v, want %d", i+1, entry["criterion_index"], i+1)
	}
	rationale := ""
	switch v := entry["rationale"].(type) {
	case nil:
	case string:
		rationale = v
	default:
		return nil, fmt.Errorf("drafted manifest entry %d has a non-string rationale", i+1)
	}
	var target any
	switch v := entry["target_path"].(type) {
	case nil:
	case string:
		// Never trusted: an unsafe target_path is dropped, not repaired.
		if oraclecommit.ValidateTargetPath(v, nil) == nil {
			target = v
		}
	default:
		return nil, fmt.Errorf("drafted manifest entry %d has a non-string target_path", i+1)
	}
	var oracleFile any
	switch name := entry["oracle_file"].(type) {
	case nil:
		target = nil
	case string:
		var err error
		if oracleFile, target, rationale, err = d.oracleFile(name, target, rationale); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("drafted manifest entry %d has a non-string oracle_file", i+1)
	}
	return map[string]any{
		"criterion":       criterion,
		"criterion_index": i + 1,
		"oracle_file":     oracleFile,
		"target_path":     target,
		// supersedes is operator-authored only; never taken from output.
		"supersedes": []string{},
		"rationale":  rationale,
	}, nil
}

// oracleFile handles an entry that names a file. It returns the entry's
// oracle_file, target_path and rationale as the manifest records them: the
// file is nil when the entry stays undrafted.
func (d *draftOutput) oracleFile(name string, target any, rationale string) (any, any, string, error) {
	switch {
	case name == request.TicketOracleRunCommandFilename || name == oracleManifestName:
		// Never installed from model output; the criterion stays undrafted.
		d.ignoredRunCommand = d.ignoredRunCommand || name == request.TicketOracleRunCommandFilename
		return nil, nil, "dropped: " + name + " is not an oracle test file", nil
	case isReservedOracleName(name):
		return nil, nil, "", fmt.Errorf("drafted manifest names %q, a case alias of a reserved file name", name)
	case !draftedNamePattern.MatchString(name):
		return nil, nil, "", fmt.Errorf("drafted manifest names %q, which is not a plain ASCII file name", name)
	}
	if prior, dup := d.seen[strings.ToLower(name)]; dup {
		target, err := d.repeatedFile(name, prior, target)
		if err != nil {
			return nil, nil, "", err
		}
		return name, target, rationale, nil
	}
	return d.newFile(name, target, rationale)
}

// repeatedFile handles a name an earlier entry already claimed and returns
// this entry's target_path. One file may cover several criteria (one entry
// each); a case-variant of an earlier name is still an alias.
func (d *draftOutput) repeatedFile(name, prior string, target any) (any, error) {
	if prior != name {
		return nil, fmt.Errorf("drafted manifest names %q and %q, which differ only in case", prior, name)
	}
	t, isStr := target.(string)
	if !isStr {
		if have, had := d.targets[name]; had {
			return have, nil
		}
		return nil, nil
	}
	if have, had := d.targets[name]; had && have != t {
		return nil, fmt.Errorf("drafted manifest gives %q two different target_path values", name)
	}
	if other, clash := d.targetOwner[t]; clash && other != name {
		return nil, fmt.Errorf("drafted files %q and %q both target %q: one repository path cannot hold two oracle files", other, name, t)
	}
	d.targetOwner[t] = name
	d.targets[name] = t
	return target, nil
}

// newFile reads a file named for the first time and records it. A file that
// cannot be taken refuses the draft, or in partial mode leaves the entry
// undrafted with the problem as its rationale.
func (d *draftOutput) newFile(name string, target any, rationale string) (any, any, string, error) {
	d.seen[strings.ToLower(name)] = name
	content, problem := d.readNamedFile(name)
	if problem != "" {
		if !d.partial {
			return nil, nil, "", fmt.Errorf("%s", problem)
		}
		delete(d.seen, strings.ToLower(name))
		return nil, nil, "dropped: " + problem, nil
	}
	d.total += len(content)
	d.files[name] = content
	if s, ok := target.(string); ok {
		if other, clash := d.targetOwner[s]; clash && other != name {
			return nil, nil, "", fmt.Errorf("drafted files %q and %q both target %q: one repository path cannot hold two oracle files", other, name, s)
		}
		d.targetOwner[s] = name
		d.targets[name] = s
	}
	return name, target, rationale, nil
}

// readNamedFile returns the content of the output file a manifest entry
// names, or the problem that keeps it from being installed.
func (d *draftOutput) readNamedFile(name string) (content []byte, problem string) {
	p := filepath.Join(d.outDir, name)
	info, statErr := os.Lstat(p)
	switch {
	case statErr != nil || !info.Mode().IsRegular():
		return nil, fmt.Sprintf("drafted manifest names %q, which is not a regular file in the output", name)
	case info.Size() > maxDraftedOracleFileBytes:
		return nil, fmt.Sprintf("drafted file %q is %d bytes, over the %d byte cap", name, info.Size(), maxDraftedOracleFileBytes)
	case len(d.files) >= request.MaxTicketOracleFiles:
		return nil, fmt.Sprintf("drafted files exceed the %d file cap", request.MaxTicketOracleFiles)
	case d.total+int(info.Size()) > maxDraftedOracleTotalBytes:
		return nil, fmt.Sprintf("drafted files exceed the %d byte total cap", maxDraftedOracleTotalBytes)
	}
	content, readErr := readBounded(p, maxDraftedOracleFileBytes)
	if readErr != nil {
		return nil, fmt.Sprintf("read drafted file %q: %v", name, readErr)
	}
	return content, ""
}

// alignPartialManifest returns one entry per criterion, in criterion order: the
// model's entry whose criterion text matches (by criterionKey), else an
// undrafted placeholder. Entries matching no criterion are dropped.
func alignPartialManifest(in []map[string]any, criteria []string) []map[string]any {
	out := make([]map[string]any, len(criteria))
	for i, c := range criteria {
		out[i] = map[string]any{"criterion": c, "criterion_index": float64(i + 1), "oracle_file": nil, "rationale": "not drafted: the run timed out"}
		for _, e := range in {
			if crit, ok := e["criterion"].(string); ok && criterionKey(crit) == criterionKey(c) {
				entry := make(map[string]any, len(e))
				for k, v := range e {
					entry[k] = v
				}
				entry["criterion"], entry["criterion_index"] = c, float64(i+1)
				out[i] = entry
				break
			}
		}
	}
	return out
}

// replaceOracleDir installs files (+ manifest) into oracleDir, replacing the
// previous draft: prevFiles (what the last pass recorded it installed) plus
// MANIFEST.json. Every other file (RUN_COMMAND.txt above all, and any
// hand-written file) is carried over byte for byte, and a new file colliding
// with one of those under any case-fold is refused rather than written over
// it. The new directory is built in a sibling staging dir with exclusive
// creates, verified to hold exactly the expected files, checked with
// CheckDir, and swapped in with renames; with no files (none_eligible) the
// previous draft is removed and an empty oracle/ is not left behind.
func replaceOracleDir(oracleDir string, prevFiles []string, files map[string][]byte, manifest []byte) error {
	parent := filepath.Dir(oracleDir)
	oldDraft := map[string]bool{oracleManifestName: true}
	for _, n := range prevFiles {
		oldDraft[n] = true
	}
	keep := map[string][]byte{}
	existing := false
	if info, statErr := os.Lstat(oracleDir); statErr == nil {
		if !info.IsDir() {
			return fmt.Errorf("%s is not a directory", oracleDir)
		}
		existing = true
		entries, readErr := os.ReadDir(oracleDir)
		if readErr != nil {
			return fmt.Errorf("read %s: %w", oracleDir, readErr)
		}
		for _, e := range entries {
			if oldDraft[e.Name()] {
				continue
			}
			if !e.Type().IsRegular() {
				return fmt.Errorf("%s/%s is not a regular file: refusing to redraft over it", filepath.Base(oracleDir), e.Name())
			}
			content, readErr := readBounded(filepath.Join(oracleDir, e.Name()), 1<<20)
			if readErr != nil {
				return fmt.Errorf("read %s/%s: %w", filepath.Base(oracleDir), e.Name(), readErr)
			}
			keep[e.Name()] = content
		}
	} else if !os.IsNotExist(statErr) {
		return statErr
	}
	for name := range files {
		for kept := range keep {
			if strings.EqualFold(name, kept) {
				return fmt.Errorf("drafted file %q would overwrite %q in %s (names equal under case-folding), which is not part of the previous draft", name, kept, filepath.Base(oracleDir))
			}
		}
	}
	if !existing && len(files) == 0 {
		return nil
	}

	stage, err := os.MkdirTemp(parent, oracleStagePrefix)
	if err != nil {
		return fmt.Errorf("stage oracle draft: %w", err)
	}
	defer os.RemoveAll(stage)
	if err := os.Chmod(stage, 0o750); err != nil {
		return err
	}
	expected := map[string]bool{}
	write := func(name string, content []byte) error {
		// O_EXCL: any alias the checks above missed fails loudly instead of
		// truncating a file already staged (case-insensitive filesystems).
		f, err := os.OpenFile(filepath.Join(stage, name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return err
		}
		if _, err := f.Write(content); err != nil {
			f.Close()
			return err
		}
		expected[name] = true
		return f.Close()
	}
	for name, content := range keep {
		if err := write(name, content); err != nil {
			return fmt.Errorf("stage %s: %w", name, err)
		}
	}
	for name, content := range files {
		if err := write(name, content); err != nil {
			return fmt.Errorf("stage %s: %w", name, err)
		}
	}
	if len(files) > 0 {
		if err := write(oracleManifestName, manifest); err != nil {
			return fmt.Errorf("stage %s: %w", oracleManifestName, err)
		}
	}
	staged, err := os.ReadDir(stage)
	if err != nil {
		return err
	}
	if len(staged) != len(expected) {
		return fmt.Errorf("staged directory holds %d entries, want exactly %d", len(staged), len(expected))
	}
	for _, e := range staged {
		if !expected[e.Name()] || !e.Type().IsRegular() {
			return fmt.Errorf("staged directory holds an unexpected entry %q", e.Name())
		}
	}
	if len(files) > 0 {
		if err := oraclecanary.CheckDir(stage); err != nil {
			return fmt.Errorf("the drafted oracle directory cannot be canaried: %w", err)
		}
	}

	var backup string
	if existing {
		backup = filepath.Join(parent, fmt.Sprintf("%s%d", oracleOldPrefix, time.Now().UnixNano()))
		if err := os.Rename(oracleDir, backup); err != nil {
			return fmt.Errorf("move the previous draft aside: %w", err)
		}
	}
	if len(keep)+len(files) > 0 {
		if err := os.Rename(stage, oracleDir); err != nil {
			if backup != "" {
				_ = os.Rename(backup, oracleDir)
			}
			return fmt.Errorf("install the drafted oracle directory: %w", err)
		}
	}
	if backup != "" {
		_ = os.RemoveAll(backup)
	}
	return nil
}

const (
	oracleStagePrefix     = "oracle-stage-"
	oracleOldPrefix       = "oracle-old-"
	oracleRejectedDirName = "oracle-rejected"
)

// recoverOracleDir repairs what a crash inside replaceOracleDir's swap leaves
// behind. With oracle/ absent, a single oracle-old-* is the operator's
// directory and is restored (otherwise their RUN_COMMAND.txt and hand-written
// files would silently not carry over); several are ambiguous and refuse the
// pass. With oracle/ present, any oracle-old-* is a stale backup. Stale
// oracle-stage-* directories are always removed.
func recoverOracleDir(oracleDir string) error {
	parent := filepath.Dir(oracleDir)
	stages, _ := filepath.Glob(filepath.Join(parent, oracleStagePrefix+"*"))
	for _, s := range stages {
		_ = os.RemoveAll(s)
	}
	olds, _ := filepath.Glob(filepath.Join(parent, oracleOldPrefix+"*"))
	if len(olds) == 0 {
		return nil
	}
	if _, err := os.Lstat(oracleDir); err == nil {
		for _, o := range olds {
			_ = os.RemoveAll(o)
		}
		return nil
	}
	if len(olds) > 1 {
		return fmt.Errorf("oracle/ is missing and %d oracle-old-* backups exist in %s: cannot tell which holds the operator's files; resolve by hand", len(olds), parent)
	}
	if err := os.Rename(olds[0], oracleDir); err != nil {
		return fmt.Errorf("restore %s after an interrupted re-draft: %w", olds[0], err)
	}
	return nil
}

// quarantineRejectedDraft moves the previously drafted files (prevFiles plus
// MANIFEST.json) out of oracle/ into a sibling oracle-rejected/ directory
// that is never pinned or mounted, leaving only operator-written files. It
// runs after a failed re-draft that followed a reject, so approval cannot pin
// the very draft the operator just rejected. Best effort per file; reports
// how many were moved.
func quarantineRejectedDraft(oracleDir string, prevFiles []string) int {
	if len(prevFiles) == 0 {
		return 0
	}
	rejected := filepath.Join(filepath.Dir(oracleDir), oracleRejectedDirName)
	_ = os.RemoveAll(rejected)
	moved := 0
	for _, name := range append(append([]string{}, prevFiles...), oracleManifestName) {
		src := filepath.Join(oracleDir, name)
		info, err := os.Lstat(src)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		if err := os.MkdirAll(rejected, 0o750); err != nil {
			return moved
		}
		if os.Rename(src, filepath.Join(rejected, name)) == nil {
			moved++
		}
	}
	if entries, err := os.ReadDir(oracleDir); err == nil && len(entries) == 0 {
		_ = os.Remove(oracleDir)
	}
	return moved
}

// requireDraftableEcosystem refuses a draft that is neither Go nor Python.
// Found live 2026-09-20: a drafted Python oracle was pytest-style, the
// default worker image has no pytest (nor vitest, jest or flutter), so the
// oracle could not run there and the build halted after the in-loop retry
// could not fix it -- a false gate. Python drafting (2026-09-24) writes only
// the plain-function convention PythonStdlibCommand's stdlib-only runner
// supports, so that ecosystem is now allowed too; JS/Dart still are not. An
// operator with a project image that has the runner writes such an oracle by
// hand; only the automatic drafter is limited.
func requireDraftableEcosystem(files map[string][]byte) error {
	for name := range files {
		eco, isTest, err := oraclecanary.Classify(name)
		if err != nil || !isTest {
			return fmt.Errorf("drafted file %q is not a recognised oracle test: %v", name, err)
		}
		if eco != oraclecanary.Go && eco != oraclecanary.Python {
			return fmt.Errorf("drafted %s oracle %q: only Go and Python oracles are drafted automatically, because the default sandbox image has no %s test runner; write this oracle by hand (with a project image that has the runner) or skip the oracle stage", eco, name, eco)
		}
	}
	return nil
}

// proposeOracleCommand computes a RUN_COMMAND suggestion from the drafted
// files. Go and Python only: one Go file gets the single-file overlay
// command, several Go files of one module get the directory-scoped
// multi-file one (see proposeMultiFileGoCommand), and any number of Python
// files get PythonStdlibCommand (one command covers the whole glob
// regardless of file count); every other case, or a draft mixing Go and
// Python, returns "" and a note saying why. It runs nothing: the real canary
// runs at the gate. Whether the command is also written as RUN_COMMAND.txt is
// planRunCommand's decision.
func proposeOracleCommand(workspace string, names []string, targets map[string]string) (command, note string) {
	var goFiles, pyFiles []string
	for _, name := range names {
		eco, isTest, err := oraclecanary.Classify(name)
		if err != nil || !isTest {
			continue
		}
		switch eco {
		case oraclecanary.Go:
			goFiles = append(goFiles, name)
		case oraclecanary.Python:
			pyFiles = append(pyFiles, name)
		default:
			return "", fmt.Sprintf("no command proposal for %s oracles yet", eco)
		}
	}
	if len(goFiles) > 0 && len(pyFiles) > 0 {
		return "", "no command proposal (drafted files mix Go and Python oracles)"
	}
	if len(pyFiles) > 0 {
		return oraclecanary.PythonStdlibCommand(), "a proposed command running every drafted Python oracle with the stdlib is shown for review, not written to RUN_COMMAND.txt"
	}
	if len(goFiles) == 0 {
		return "", "no command proposal (no Go or Python oracle file)"
	}
	if len(goFiles) > 1 {
		return proposeMultiFileGoCommand(workspace, goFiles, targets)
	}
	file := goFiles[0]
	target := targets[file]
	if target == "" || !strings.HasSuffix(target, ".go") {
		return "", "no command proposal (the manifest gave no Go target_path)"
	}
	pkgDir := path.Dir(target)
	moduleDir := goModuleDirFor(workspace, pkgDir)
	if moduleDir == "" {
		return "", "no command proposal (no go.mod at or above the target package)"
	}
	command, err := oraclecanary.GoCommand(moduleDir, relToModule(moduleDir, pkgDir), file)
	if err != nil {
		return "", fmt.Sprintf("no command proposal (%v)", err)
	}
	return command, "a proposed command is shown for review, not written to RUN_COMMAND.txt"
}

// goModuleDirFor is the workspace-relative directory of the nearest go.mod at
// or above pkgDir ("" when there is none).
func goModuleDirFor(workspace, pkgDir string) string {
	for dir := pkgDir; ; dir = path.Dir(dir) {
		if _, err := os.Stat(filepath.Join(workspace, filepath.FromSlash(dir), "go.mod")); err == nil {
			return dir
		}
		if dir == "." || dir == "/" {
			return ""
		}
	}
}

// relToModule is pkgDir relative to moduleDir, "." for the module root.
func relToModule(moduleDir, pkgDir string) string {
	if moduleDir == pkgDir {
		return "."
	}
	if moduleDir == "." {
		return pkgDir
	}
	return strings.TrimPrefix(pkgDir, moduleDir+"/")
}

// proposeMultiFileGoCommand is the several-Go-files case: one directory-scoped
// overlay command (oraclecanary.GoMultiCommand) when every file has a Go
// target_path and they all live in one module. Files of different modules need
// one `go test` per module, which no single command here expresses, so the
// operator writes that one by hand.
func proposeMultiFileGoCommand(workspace string, goFiles []string, targets map[string]string) (string, string) {
	var moduleDir string
	files := make([]oraclecanary.GoOracleFile, 0, len(goFiles))
	for _, file := range goFiles {
		target := targets[file]
		if target == "" || !strings.HasSuffix(target, ".go") {
			return "", fmt.Sprintf("no command proposal (%d Go oracle files and the manifest gave %s no Go target_path)", len(goFiles), file)
		}
		pkgDir := path.Dir(target)
		mod := goModuleDirFor(workspace, pkgDir)
		if mod == "" {
			return "", fmt.Sprintf("no command proposal (no go.mod at or above the target package of %s)", file)
		}
		if moduleDir != "" && mod != moduleDir {
			return "", fmt.Sprintf("no command proposal (%d Go oracle files span more than one module: %s and %s)", len(goFiles), moduleDir, mod)
		}
		moduleDir = mod
		files = append(files, oraclecanary.GoOracleFile{Name: file, PkgDir: relToModule(mod, pkgDir)})
	}
	command, err := oraclecanary.GoMultiCommand(moduleDir, files)
	if err != nil {
		return "", fmt.Sprintf("no command proposal (%v)", err)
	}
	return command, "a proposed command covering all the Go files is shown for review"
}
