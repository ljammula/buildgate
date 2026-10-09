package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"buildgate/internal/modelrole"
	"buildgate/internal/request"
	"buildgate/internal/requestdriver"
	"buildgate/internal/runner"
	"buildgate/internal/sandbox"
	wsisolation "buildgate/internal/workspace"
)

// specDraftScriptName is agent/pi/scripts/draft_spec.py's own embedded
// harness filename -- see internal/harness's doc comment: every
// agent/pi/scripts/*.py file is embedded and extracted together, so this
// needs no change to that package, only to be named here the same way
// resolveHarnessScript's other callers name build_app.py/goal_pilot.py.
const specDraftScriptName = "draft_spec.py"

// specDraftScratchDirName is where the sandboxed spec-drafting job (and
// draft_spec.py's own model-facing prompt, see that script's own
// DRAFT_RELATIVE_PATH) reads and writes spec.md/evidence.json, relative
// to the workspace mount -- the only writable path a sandboxed worker
// has (see sandbox.LaunchSpec's own doc comment on WorkDir). The repo
// checkout itself is mounted at that same path for the sole reason that
// this is the existing launch path's only writable mount; draft_spec.py
// itself never writes anywhere else under it, and runSpecDraftJob removes
// this directory (both before and after each attempt) once its two
// output files are read back, so nothing here is meant to be read-only in
// the strict mount-level sense the plan's own wording ("read-only mount
// of the repo") suggests. Extending sandbox.LaunchSpec with a second,
// dedicated writable mount so the repo checkout itself could be genuinely
// read-only was judged out of scope for this WP -- see
// runSpecDraftJob's own doc comment.
const specDraftScratchDirName = ".factory-spec-draft"

// specDraftContainerOutPath/specDraftContainerEvidencePath are the fixed
// container-side paths draftSpecArgs passes to draft_spec.py for a
// sandboxed invocation -- specDraftContainerWorkDir ("/workspace",
// matching internal/sandbox/docker.go's own workerContainerWorkDir) is
// the one stable, already-relied-upon convention runSandboxWithRetries'
// own workspace-argument translation exists to honor (see that
// function's own translated[i] == workspace check), so these are safe to
// hardcode here rather than threaded through a translation table.
const (
	specDraftContainerWorkDir      = "/workspace"
	specDraftContainerOutPath      = specDraftContainerWorkDir + "/" + specDraftScratchDirName + "/spec.md"
	specDraftContainerEvidencePath = specDraftContainerWorkDir + "/" + specDraftScratchDirName + "/evidence.json"
)

// specDraftEvidenceSchemaVersion is the schema_version draft_spec.py's own
// EVIDENCE_SCHEMA_VERSION constant must equal -- bump both together, in
// the same change, when this payload's shape changes. Unlike
// BUILD_EVIDENCE.json (best-effort, never blocks a run -- see
// run.AgentEvidenceSchemaVersion's own doc comment), readSpecDraftOutputs
// rejects a schema_version it doesn't recognise outright: this evidence
// file is the only source of drafting cost/token accounting, read
// immediately after the job from a scratch dir with no older-version
// evidence ever in play, so there is no case where silently tolerating a
// mismatch is the right call.
const specDraftEvidenceSchemaVersion = 1

// draftSpecEvidencePayload mirrors draft_spec.py's own write_evidence
// JSON shape field-for-field, the same "no translation layer" contract
// build_app.py's write_evidence_json documents for run.AgentEvidence.
type draftSpecEvidencePayload struct {
	SchemaVersion int            `json:"schema_version"`
	Generated     string         `json:"generated"`
	Usage         map[string]any `json:"usage"`
	AgentExitCode int            `json:"agent_exit_code"`
	DurationS     float64        `json:"duration_s"`
	AgentsMDUsed  bool           `json:"agents_md_used"`
}

// draftSpecArgs constructs the argv passed to draft_spec.py, mirroring
// buildAppArgs' own "pulled out as a pure function so it's unit-testable
// independent of any subprocess" shape. requestPath and workspace must be
// exactly the same values passed as runSandboxWithRetries' own specPath/
// workspace parameters (or, for an unsandboxed invocation, the same real
// host paths runner.RunWithRetries runs against) -- see this file's own
// doc comment on the two execution modes. feedbackPath may be "" (no
// spec_review rejection to feed back -- see draftInputFiles' own doc
// comment), in which case
// --feedback-file is omitted entirely, matching draft_spec.py's own
// optional argument.
//
// thinking is the planning role's Pi reasoning-effort level
// (requestJobRoleOverride.Thinking) -- "" (roles absent, or planning unset)
// omits --thinking entirely, leaving draft_spec.py's own default untouched.
func draftSpecArgs(script, workspace, requestPath, outPath, evidencePath string, files draftInputFiles, timeoutMinutes int, thinking, harness string) []string {
	args := []string{
		script,
		"--workspace", workspace,
		"--request", requestPath,
		"--out", outPath,
		"--evidence", evidencePath,
		"--timeout-minutes", fmt.Sprint(timeoutMinutes),
	}
	args = append(args, files.args()...)
	if thinking != "" {
		args = append(args, "--thinking", thinking)
	}
	args = append(args, "--harness", harness)
	return args
}

// runSpecDraftJob is the "spec" job kind: it drafts a spec for one
// request by running draft_spec.py inside the sandbox, with the same
// relay, budget ceilings, and timeout shape run_ticket.go's own build
// phase uses, through the exact same low-level launch path
// (runSandboxWithRetries) rather than a second sandbox launcher --
// unlike a ticket build, there is no worktree isolation, no gates, and no
// PR: this is a single one-shot pi invocation, evaluated by the request
// driver's own call to request.ValidateSpecSkeleton, not by this
// function.
//
// r.Workspace is mounted read-write (see specDraftScratchDirName's own
// doc comment for why "read-only" here is a script-level convention, not
// a mount-level guarantee) at the sandboxed worker's fixed /workspace
// path; r's own request.md is mounted read-only under /inputs/run/,
// exactly like a ticket build's own -spec file.
func runSpecDraftJob(ctx context.Context, dataDir string, r *request.Request, cfg requestdriver.WorkerConfig) (specMD string, evidence *request.SpecEvidence, err error) {
	err = withDraftingWorktree(dataDir, r, func(job *request.Request) error {
		var jobErr error
		specMD, evidence, jobErr = runSpecDraftJobIn(ctx, dataDir, job, cfg)
		return jobErr
	})
	return specMD, evidence, err
}

// withDraftingWorktree runs fn against a copy of r whose Workspace is a
// throwaway detached worktree of the repository's HEAD, created under
// the request's own directory and removed afterwards. Drafting jobs mount
// their workspace read-write (the model writes its draft under a scratch
// subdirectory), so running them against the operator's own checkout let
// one job's stray edit to AGENTS.md or README.md -- read verbatim into
// the next job's prompt -- persist into that job and into the checkout
// itself (adversarial review finding). A fresh worktree per job gives
// every draft exactly the committed HEAD, the same discipline a ticket
// build already gets from workspace.Prepare. Outside a git repository
// (unit tests against a bare temp dir) the workspace is used as is.
func withDraftingWorktree(dataDir string, r *request.Request, fn func(job *request.Request) error) error {
	topOut, err := exec.Command("git", "-C", r.Workspace, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return fn(r)
	}
	repoRoot := strings.TrimSpace(string(topOut))
	// git reports the symlink-resolved top level; resolve the workspace
	// the same way or Rel walks out of the root (/var vs /private/var).
	workspace, err := filepath.EvalSymlinks(r.Workspace)
	if err != nil {
		return fmt.Errorf("resolve workspace %s: %w", r.Workspace, err)
	}
	rel, err := filepath.Rel(repoRoot, workspace)
	if err != nil {
		return fmt.Errorf("workspace %s relative to %s: %w", r.Workspace, repoRoot, err)
	}
	// dataDir must be absolute before it's used to build wt: the `git
	// worktree add -C repoRoot ... wt` call below resolves a relative wt
	// against repoRoot (the target repo), but every later consumer of
	// job.Workspace (built from the same wt string) resolves it against
	// this process's own cwd instead -- the same relative-vs-absolute
	// mismatch sandboxDataDirFor's own doc comment describes, just at a
	// different call site. Found live 2026-09-17 immediately after that
	// fix landed: with -data-dir left at its relative default ("data"),
	// this produced "resolve sandbox workspace mount: lstat
	// data/requests/<id>/worktree: no such file or directory" -- the
	// worktree really was created (under the *target* repo's root, not
	// buildgate's), just not where this relative string resolves
	// to from here.
	absDataDir, err := canonicalPath(dataDir)
	if err != nil {
		return fmt.Errorf("resolve -data-dir: %w", err)
	}
	wt := filepath.Join(request.Dir(absDataDir, r.ID), "worktree")
	// Through the workspace mutators, not bare git: drafting jobs run beside
	// isolated builds of the same repository, and worktree add/remove write
	// shared Git metadata that the git metadata lock serializes.
	_ = wsisolation.RemoveWorktreeOnly(repoRoot, wt)
	_ = os.RemoveAll(wt)
	if err := wsisolation.AddDetachedWorktree(repoRoot, wt, "HEAD"); err != nil {
		return fmt.Errorf("create drafting worktree %s: %w", wt, err)
	}
	defer func() {
		if err := wsisolation.RemoveWorktreeOnly(repoRoot, wt); err != nil {
			log.Printf("request %s: remove drafting worktree %s: %v", r.ID, wt, err)
		}
	}()
	job := *r
	job.Workspace = filepath.Join(wt, rel)
	return fn(&job)
}

func runSpecDraftJobIn(ctx context.Context, dataDir string, r *request.Request, cfg requestdriver.WorkerConfig) (string, *request.SpecEvidence, error) {
	script, err := resolveHarnessScript(cfg.DraftSpecScript, specDraftScriptName)
	if err != nil {
		return "", nil, fmt.Errorf("resolve %s: %w", specDraftScriptName, err)
	}
	requestTextPath := request.TextPath(dataDir, r.ID)
	scratchDir := filepath.Join(r.Workspace, specDraftScratchDirName)
	// Best-effort: a leftover scratch directory from a prior, failed
	// attempt must not be mistaken for this attempt's own output below.
	_ = os.RemoveAll(scratchDir)
	defer os.RemoveAll(scratchDir)

	// Stage advanceSpecDrafting's durable spec-feedback.md (written from
	// request.SpecFeedback, if the last rejection was spec_review's own)
	// into the scratch dir, so --feedback-file has something to read
	// whether this attempt is sandboxed or not -- see
	// draftInputFiles' own doc comment for why the scratch
	// dir, not runSandboxWithRetries' own specPath/extraRunInput staging.
	sandboxUser, _ := requestJobSandboxIdentity(cfg)
	var hostFeedbackPath string
	if feedback, readErr := os.ReadFile(request.SpecFeedbackPath(dataDir, r.ID)); readErr == nil && len(feedback) > 0 {
		if err := ensureRequestJobScratchDir(scratchDir, sandboxUser); err != nil {
			return "", nil, fmt.Errorf("create spec-draft scratch dir: %w", err)
		}
		hostFeedbackPath = filepath.Join(scratchDir, "feedback.md")
		if err := writeRequestJobFeedbackFile(hostFeedbackPath, feedback, sandboxUser); err != nil {
			return "", nil, fmt.Errorf("stage spec feedback: %w", err)
		}
	}

	guide, hostGuidePath, err := stageRequestDesignGuide(r.Workspace, cfg.Settings, scratchDir, sandboxUser, designGuideSpecPart)
	if err != nil {
		return "", nil, err
	}

	hostPreviousPath, err := stagePreviousSpec(dataDir, r, scratchDir, sandboxUser)
	if err != nil {
		return "", nil, err
	}
	hostFiles := draftInputFiles{feedback: hostFeedbackPath, designGuide: hostGuidePath, previousDraft: hostPreviousPath}

	timeoutMinutes := cfg.SpecDraftTimeoutMinutes
	if timeoutMinutes <= 0 {
		timeoutMinutes = defaultSpecDraftTimeoutMinutes
	}
	sandboxImage := cfg.SandboxImage
	if sandboxImage == "" && !cfg.AllowUnsandboxedSpecDraft {
		return "", nil, fmt.Errorf("no sandbox image configured: run `make install` from the buildgate checkout (builds images from source and records them via `factoryd configure-images`), or pass -sandbox-image")
	}
	roleOverride, roleErr := resolveRequestJobRole(cfg, modelrole.StageSpecDrafting, "spec drafting", r.Models["planning"], r.Harnesses["planning"])
	if roleErr != nil {
		return "", nil, roleErr
	}
	defer startActiveJob(dataDir, r.ID, modelrole.StageSpecDrafting, roleOverride, time.Now())()

	logDir := filepath.Join(request.Dir(dataDir, r.ID), "logs")
	if err := os.MkdirAll(logDir, 0o750); err != nil {
		return "", nil, fmt.Errorf("create spec-draft log dir: %w", err)
	}
	logPath := func(attempt int) string {
		if attempt == 1 {
			return filepath.Join(logDir, "spec_draft.log")
		}
		return filepath.Join(logDir, fmt.Sprintf("spec_draft.attempt%d.log", attempt))
	}

	var specMD string
	var evidence *request.SpecEvidence
	var res runner.Result
	if sandboxImage != "" {
		sandboxDataDir, sddErr := requestdriver.SandboxDataDirFor(dataDir)
		if sddErr != nil {
			return "", nil, sddErr
		}
		relaySpec, relayErr := resolveSpecDraftRelaySpec(cfg, r.ID, sandboxDataDir, roleOverride)
		if relayErr != nil {
			return "", nil, relayErr
		}
		outPath := specDraftContainerOutPath
		evidencePath := specDraftContainerEvidencePath
		args := draftSpecArgs(script, r.Workspace, requestTextPath, outPath, evidencePath, hostFiles.inContainer(specDraftContainerWorkDir+"/"+specDraftScratchDirName), timeoutMinutes, roleOverride.Thinking, roleOverride.Harness)
		sandboxCtx, cancel := context.WithTimeout(ctx, requestJobContainerDeadline(time.Duration(timeoutMinutes)*time.Minute, false, 0))
		defer cancel()
		_, workerUID := requestJobSandboxIdentity(cfg)
		res, err = runSandboxWithRetriesVia(
			cfg.Sandboxes, cfg.MeterLedgerRoot, sandboxCtx, r.Workspace, requestTextPath, "", script, logPath, 1,
			sandboxImage, cfg.Settings.SandboxDocker, sandboxUser, workerUID, r.ID, sandboxDataDir,
			cfg.Settings.SandboxMemory, cfg.Settings.SandboxCPUs, cfg.Settings.SandboxTmpfsSize,
			nil, relaySpec, nil, sandbox.RegistryProxyHooks{}, nil, sandbox.ComposeServicesHooks{},
			"", "", roleOverride.WorkerEnv, roleOverride.Skills, cfg.DraftSpecInterpreter, args[1:]...,
		)
	} else {
		outPath := filepath.Join(scratchDir, "spec.md")
		evidencePath := filepath.Join(scratchDir, "evidence.json")
		args := draftSpecArgs(script, r.Workspace, requestTextPath, outPath, evidencePath, hostFiles, timeoutMinutes, roleOverride.Thinking, roleOverride.Harness)
		res, err = runner.RunWithRetries(ctx, r.Workspace, logPath, 1, nil, cfg.DraftSpecInterpreter, args[1:]...)
	}
	retainDraftPrompts(dataDir, r.ID, "spec", r.Workspace, []string{specDraftScratchDirName + "/session", specDraftScratchDirName + "/check-session"})
	if err != nil {
		return "", nil, fmt.Errorf("draft_spec.py did not run to completion: %w", err)
	}

	specMD, evidence, readErr := readSpecDraftOutputs(scratchDir)
	if readErr != nil {
		return "", nil, readErr
	}
	// Model is factoryd's own, set from the relay's configured worker model id
	// (res.RelayWorkerModelID), never from anything draft_spec.py itself
	// wrote to evidence.json -- only the relay configuration is
	// factory-authored. Empty when this job had no relay.
	if evidence != nil {
		evidence.Model = res.RelayWorkerModelID
		evidence.Thinking = roleOverride.Thinking
		// Spend is this job's own relay-measured cost -- see JobSpend's
		// own doc comment for why this must come from res
		// (runner.Result), never from anything draft_spec.py itself
		// reports. request_driver.go's advanceSpecDrafting accumulates a
		// re-draft's previous SpecEvidence.Spend into this one before it
		// replaces r.SpecEvidence.
		evidence.Spend = withDesignGuide(jobSpendFromResult(res, roleOverride.Role, time.Now()), guide)
	}
	if res.ExitCode != 0 {
		return "", evidence, fmt.Errorf("%s", requestJobExitReason("draft_spec.py", res.ExitCode, "", res.LogPath))
	}
	if strings.TrimSpace(specMD) == "" {
		return "", evidence, fmt.Errorf("%s", requestJobExitReason("draft_spec.py", 0, "but wrote an empty spec.md", res.LogPath))
	}
	return specMD, evidence, nil
}

// previousDraftFileName is the staged copy of a handed-over spec's current
// text inside the drafting scratch directory.
const previousDraftFileName = "previous-draft.md"

// stagePreviousSpec stages r's current spec.md for --previous-draft-file
// when the operator handed the spec over (`submit -spec-file`), so a
// redraft after a spec_review rejection revises their document instead of
// drafting afresh from the request. It returns "" for a drafted request.
// The drafting job only runs for a handed-over spec once there is feedback.
// After a spec_review rejection spec.md holds the document that was
// reviewed. After a send-back from a halt of the import itself there is no
// spec.md yet, and the file as handed over is the document to revise.
func stagePreviousSpec(dataDir string, r *request.Request, scratchDir, sandboxUser string) (string, error) {
	if !r.SpecImported {
		return "", nil
	}
	current, err := os.ReadFile(requestdriver.RequestSpecPath(dataDir, r.ID))
	if os.IsNotExist(err) {
		current, err = os.ReadFile(request.ImportedSpecPath(dataDir, r.ID))
	}
	if err != nil {
		return "", fmt.Errorf("read the handed-over spec to revise: %w", err)
	}
	if err := ensureRequestJobScratchDir(scratchDir, sandboxUser); err != nil {
		return "", fmt.Errorf("create spec-draft scratch dir: %w", err)
	}
	path := filepath.Join(scratchDir, previousDraftFileName)
	if err := writeRequestJobFeedbackFile(path, current, sandboxUser); err != nil {
		return "", fmt.Errorf("stage the spec to revise: %w", err)
	}
	return path, nil
}

// readSpecDraftOutputs reads spec.md/evidence.json back from the host
// side of the same bind-mounted scratch directory draft_spec.py wrote
// into -- no container/host translation needed for the read, exactly
// like loadAgentEvidence reads BUILD_EVIDENCE.json straight from a
// build's own execDir. evidence.json is read (and returned) even when
// spec.md is missing or draft_spec.py exited non-zero, since the
// evidence file is always written on any invocation that actually ran
// (see draft_spec.py's own doc comment) and is useful even for a
// halt reason.
func readSpecDraftOutputs(scratchDir string) (string, *request.SpecEvidence, error) {
	var evidence *request.SpecEvidence
	evidencePath := filepath.Join(scratchDir, "evidence.json")
	if b, err := os.ReadFile(evidencePath); err == nil {
		var payload draftSpecEvidencePayload
		if jsonErr := json.Unmarshal(b, &payload); jsonErr == nil {
			if payload.SchemaVersion != specDraftEvidenceSchemaVersion {
				return "", nil, fmt.Errorf("%s schema_version %d, this factoryd understands %d", evidencePath, payload.SchemaVersion, specDraftEvidenceSchemaVersion)
			}
			evidence = &request.SpecEvidence{
				Usage:         payload.Usage,
				AgentExitCode: payload.AgentExitCode,
				DurationS:     payload.DurationS,
				AgentsMDUsed:  payload.AgentsMDUsed,
			}
		}
	}
	specMD, err := os.ReadFile(filepath.Join(scratchDir, "spec.md"))
	if err != nil {
		if os.IsNotExist(err) {
			return "", evidence, nil
		}
		return "", evidence, fmt.Errorf("read drafted spec.md: %w", err)
	}
	return string(specMD), evidence, nil
}

// resolveSpecDraftRelaySpec builds the same shape of sandbox.RouteSpec
// run_ticket.go's own relay-policy block does, from cfg's already-parsed
// fields, rather than a second, independent flag surface -- see
// workerConfig's own doc comment for why cfg already carries this
// verbatim from run_ticket.go's flags. A thin wrapper over
// resolveRequestJobRelaySpec (called directly by runPlanTicketsJob in
// plan_tickets_job.go for the plan-drafting job kind) naming this job
// kind in its one job-specific error message. roleOverride is
// resolveRequestJobRole's own result for modelrole.StageSpecDrafting.
func resolveSpecDraftRelaySpec(cfg requestdriver.WorkerConfig, runID, sandboxDataDir string, roleOverride requestJobRoleOverride) (*sandbox.RouteSpec, error) {
	return resolveRequestJobRelaySpec(cfg, "spec drafting", runID, sandboxDataDir, roleOverride)
}

// resolveRequestJobRelaySpec is the relay-configuration logic
// resolveSpecDraftRelaySpec and runPlanTicketsJob (plan_tickets_job.go)
// both need -- one request-driver job kind (spec drafting or plan
// drafting), talking to a model through the sandboxed worker's only
// network path. jobName only varies the "requires a relay" error message
// so it names the actual job that failed to configure one.
//
// roleOverride (resolveRequestJobRole's own result) always carries a
// resolved RouteSelection: routes:/models:/roles: is the only schema, so
// this job's whole relay policy is built and validated the same way
// run_ticket.go's own relay-launch does.
func resolveRequestJobRelaySpec(cfg requestdriver.WorkerConfig, jobName, runID, sandboxDataDir string, roleOverride requestJobRoleOverride) (*sandbox.RouteSpec, error) {
	sel := roleOverride.RouteSelection
	creds, err := resolveRouteCredentials(sel.Route)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", jobName, err)
	}
	policy := sel.Policy
	if err := policy.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", jobName, err)
	}
	if err := policy.ValidateUpstreamScheme(); err != nil {
		return nil, err
	}
	spec := policy.Spec(creds.apiKey, creds.githubToken, creds.chatGPTToken, creds.chatGPTAccountID, runID, sandboxDataDir)
	spec.CABundlePath = cfg.EgressCABundle
	if err := spec.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", jobName, err)
	}
	return &spec, nil
}

// requestJobSandboxIdentity is the container identity a request-level job
// (spec drafting, planning) runs as. These jobs run in withDraftingWorktree's
// own throwaway, detached git worktree under the request's own data-dir
// (removed once the job returns), created host-side via a plain `git
// worktree add`, not through the per-run isolation
// mechanism -- but the reasoning is identical either way: that worktree
// is host-owned (created by this process's own uid:gid), so the
// separated worker UID would have no write access there. Default to the
// host's own uid:gid, and honor an explicit sandbox_user. Found live:
// passing "" and 0 resolved to "0:<gid>", which LaunchSpec.Validate
// rightly refuses, so every spec draft halted before the model ran.
//
// Future hardening: chown the drafting worktree to the configured worker
// UID right after withDraftingWorktree creates it, so these jobs could
// run as the separated worker identity like every sandboxed ticket build
// already does, instead of carving out this host-identity exception.
// Not done here -- withDraftingWorktree's own worktree is torn down and
// recreated per request, so the chown would need to run on every single
// invocation, and the request-level jobs this identity serves (spec
// drafting, planning) have no untrusted model-driven code execution of
// their own the way a ticket build's agent does, unlike the isolation
// per-run worktrees were built to contain.
func requestJobSandboxIdentity(cfg requestdriver.WorkerConfig) (user string, workerUID int) {
	user = cfg.Settings.SandboxUser
	if user == "" {
		user = fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid())
	}
	return user, cfg.Settings.SandboxWorkerUID
}

// sandboxUserGID parses user (requestJobSandboxIdentity's own "<uid>:<gid>"
// or "<uid>" shape) and returns its gid, if any -- ok is false for a bare
// uid (no ":") or anything that doesn't parse as two integers. Mirrors
// internal/sandbox.parseWorkerIdentity, unexported from that package, for
// the one field this file's own callers need.
func sandboxUserGID(user string) (gid int, ok bool) {
	_, gidText, hasGID := strings.Cut(user, ":")
	if !hasGID {
		return 0, false
	}
	gid, err := strconv.Atoi(gidText)
	if err != nil {
		return 0, false
	}
	return gid, true
}

// ensureRequestJobScratchDir creates dir (and any missing parents) writable
// by a sandboxed worker running as user, a distinct UID sharing this
// process's own primary group -- e.g. `factoryd serve -sandbox-user
// <dedicated-UID>:<factoryd GID>` (an adversarial review,
// 2026-09-24). Mirrors internal/sandbox.EnsureScratchDir's own MkdirAll +
// explicit Chmod (the process umask would otherwise strip the group
// bit) + best-effort Chown-to-group shape: 0o770, not the 0o750 this
// file's own callers used before, since a *different*-UID worker gets
// only its group bits here, and 0o750 leaves the group with no write
// permission at all -- before this branch's -sandbox-user support, the
// worker (the image's default user, sharing factoryd's own uid:gid) was
// always the directory's *owner*, so owner-only permissions never mattered.
func ensureRequestJobScratchDir(dir, user string) error {
	if err := os.MkdirAll(dir, 0o770); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o770); err != nil {
		return err
	}
	if gid, ok := sandboxUserGID(user); ok {
		if err := os.Chown(dir, -1, gid); err != nil {
			return err
		}
	}
	return nil
}

// writeRequestJobFeedbackFile writes content to path, readable by a
// sandboxed worker running as user -- ensureRequestJobScratchDir's own
// sibling for a file instead of a directory (the same review). 0o640, not the
// 0o600 this file's own callers used before: group-read only, since the
// worker only ever reads this file back (read_feedback_ex in draft_spec.py/
// plan_tickets.py), never writes it.
func writeRequestJobFeedbackFile(path string, content []byte, user string) error {
	if err := os.WriteFile(path, content, 0o640); err != nil {
		return err
	}
	if gid, ok := sandboxUserGID(user); ok {
		if err := os.Chown(path, -1, gid); err != nil {
			return err
		}
	}
	return nil
}
