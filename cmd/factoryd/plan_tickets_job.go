package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"buildgate/internal/modelrole"
	"buildgate/internal/request"
	"buildgate/internal/requestdriver"
	"buildgate/internal/runner"
	"buildgate/internal/sandbox"
	"buildgate/internal/sanitize"
)

// planTicketsScriptName is agent/pi/scripts/plan_tickets.py's own embedded
// harness filename -- see specDraftScriptName's own doc comment for why
// this needs no change to internal/harness, only to be named here.
const planTicketsScriptName = "plan_tickets.py"

// planTicketsScratchDirName mirrors specDraftScratchDirName: the only
// writable path a sandboxed worker has (the workspace mount itself), used
// as scratch for plan_tickets.py's own tickets/evidence.json output --
// see specDraftScratchDirName's own doc comment for why "read-only" here
// is a script-level convention, not a mount-level guarantee.
const planTicketsScratchDirName = ".factory-plan-draft"

// planTicketsContainerOutDir/planTicketsContainerEvidencePath are the
// fixed container-side paths planTicketsArgs passes to plan_tickets.py
// for a sandboxed invocation -- see specDraftContainerOutPath's own doc
// comment for why specDraftContainerWorkDir ("/workspace") is safe to
// hardcode here.
const (
	planTicketsContainerOutDir       = specDraftContainerWorkDir + "/" + planTicketsScratchDirName + "/tickets"
	planTicketsContainerEvidencePath = specDraftContainerWorkDir + "/" + planTicketsScratchDirName + "/evidence.json"
)

// planTicketsEvidenceSchemaVersion is the schema_version plan_tickets.py's
// own EVIDENCE_SCHEMA_VERSION constant must equal -- see
// specDraftEvidenceSchemaVersion's own doc comment for why
// readPlanTicketsOutputs rejects a mismatch outright rather than
// tolerating it the way BUILD_EVIDENCE.json's best-effort reader does.
const planTicketsEvidenceSchemaVersion = 1

// planTicketsEvidencePayload mirrors plan_tickets.py's own write_evidence
// JSON shape field-for-field, the same "no translation layer" contract
// draftSpecEvidencePayload documents for request.SpecEvidence.
type planTicketsEvidencePayload struct {
	SchemaVersion int            `json:"schema_version"`
	Generated     string         `json:"generated"`
	Usage         map[string]any `json:"usage"`
	AgentExitCode int            `json:"agent_exit_code"`
	DurationS     float64        `json:"duration_s"`
	AgentsMDUsed  bool           `json:"agents_md_used"`
}

// planTicketsArgs constructs the argv passed to plan_tickets.py, mirroring
// draftSpecArgs' own pure, unit-testable shape -- including feedbackPath's
// own "may be empty, then --feedback-file is omitted" convention. thinking
// mirrors draftSpecArgs' own parameter: the planning role's Pi
// reasoning-effort level, or "" to omit --thinking entirely.
func planTicketsArgs(script, workspace, specPath, requestPath, verifyCommand, outDir, evidencePath string, files draftInputFiles, timeoutMinutes int, thinking, harness string) []string {
	args := []string{
		script,
		"--workspace", workspace,
		"--spec", specPath,
		"--request", requestPath,
		"--verify-command", verifyCommand,
		"--out-dir", outDir,
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

// runPlanTicketsJob is the "plan" job kind: it decomposes r's approved
// spec.md into one or more tickets by running plan_tickets.py inside the
// sandbox, through the exact same low-level launch path
// (runSandboxWithRetries) runSpecDraftJob uses -- see that function's own
// doc comment for why this is a single one-shot pi invocation, evaluated
// by the request driver's own calls to request.ValidateTicketPlan/
// ValidatePlanCoverage and internal/ticketspec/internal/policy, not by
// this function.
func runPlanTicketsJob(ctx context.Context, dataDir string, r *request.Request, cfg requestdriver.WorkerConfig, verifyCommand string) (tickets []requestdriver.DraftedTicket, evidence *request.PlanEvidence, err error) {
	err = withDraftingWorktree(dataDir, r, func(job *request.Request) error {
		var jobErr error
		tickets, evidence, jobErr = runPlanTicketsJobIn(ctx, dataDir, job, cfg, verifyCommand)
		return jobErr
	})
	return tickets, evidence, err
}

func runPlanTicketsJobIn(ctx context.Context, dataDir string, r *request.Request, cfg requestdriver.WorkerConfig, verifyCommand string) ([]requestdriver.DraftedTicket, *request.PlanEvidence, error) {
	script, err := resolveHarnessScript(cfg.PlanTicketsScript, planTicketsScriptName)
	if err != nil {
		return nil, nil, fmt.Errorf("resolve %s: %w", planTicketsScriptName, err)
	}
	specPath := requestdriver.RequestSpecPath(dataDir, r.ID)
	requestTextPath := request.TextPath(dataDir, r.ID)
	scratchDir := filepath.Join(r.Workspace, planTicketsScratchDirName)
	// Best-effort: a leftover scratch directory from a prior, failed
	// attempt must not be mistaken for this attempt's own output below.
	_ = os.RemoveAll(scratchDir)
	defer os.RemoveAll(scratchDir)

	// Stage advancePlanning's durable plan-feedback.md -- see
	// runSpecDraftJobIn's identical block.
	sandboxUser, _ := requestJobSandboxIdentity(cfg)
	var hostFeedbackPath string
	if feedback, readErr := os.ReadFile(request.PlanFeedbackPath(dataDir, r.ID)); readErr == nil && len(feedback) > 0 {
		if err := ensureRequestJobScratchDir(scratchDir, sandboxUser); err != nil {
			return nil, nil, fmt.Errorf("create plan-draft scratch dir: %w", err)
		}
		hostFeedbackPath = filepath.Join(scratchDir, "feedback.md")
		if err := writeRequestJobFeedbackFile(hostFeedbackPath, feedback, sandboxUser); err != nil {
			return nil, nil, fmt.Errorf("stage plan feedback: %w", err)
		}
	}

	guide, hostGuidePath, err := stageRequestDesignGuide(r.Workspace, cfg.Settings, scratchDir, sandboxUser, designGuidePlanPart)
	if err != nil {
		return nil, nil, err
	}

	hostPreviousPath, err := stagePreviousPlan(dataDir, r, scratchDir, sandboxUser)
	if err != nil {
		return nil, nil, err
	}
	hostFiles := draftInputFiles{feedback: hostFeedbackPath, designGuide: hostGuidePath, previousDraft: hostPreviousPath}

	timeoutMinutes := cfg.PlanTicketsTimeoutMinutes
	if timeoutMinutes <= 0 {
		timeoutMinutes = defaultPlanTicketsTimeoutMinutes
	}
	sandboxImage := cfg.SandboxImage
	if sandboxImage == "" && !cfg.AllowUnsandboxedSpecDraft {
		return nil, nil, fmt.Errorf("no sandbox image configured: run `make install` from the buildgate checkout (builds images from source and records them via `factoryd configure-images`), or pass -sandbox-image")
	}
	roleOverride, roleErr := resolveRequestJobRole(cfg, modelrole.StagePlanning, "plan drafting", r.Models["planning"], r.Harnesses["planning"])
	if roleErr != nil {
		return nil, nil, roleErr
	}
	defer startActiveJob(dataDir, r.ID, modelrole.StagePlanning, roleOverride, time.Now())()

	logDir := filepath.Join(request.Dir(dataDir, r.ID), "logs")
	if err := os.MkdirAll(logDir, 0o750); err != nil {
		return nil, nil, fmt.Errorf("create plan-draft log dir: %w", err)
	}
	logPath := func(attempt int) string {
		if attempt == 1 {
			return filepath.Join(logDir, "plan_tickets.log")
		}
		return filepath.Join(logDir, fmt.Sprintf("plan_tickets.attempt%d.log", attempt))
	}

	var res runner.Result
	if sandboxImage != "" {
		sandboxDataDir, sddErr := requestdriver.SandboxDataDirFor(dataDir)
		if sddErr != nil {
			return nil, nil, sddErr
		}
		relaySpec, relayErr := resolveRequestJobRelaySpec(cfg, "plan drafting", r.ID, sandboxDataDir, roleOverride)
		if relayErr != nil {
			return nil, nil, relayErr
		}
		outDir := planTicketsContainerOutDir
		evidencePath := planTicketsContainerEvidencePath
		args := planTicketsArgs(script, r.Workspace, specPath, requestTextPath, verifyCommand, outDir, evidencePath, hostFiles.inContainer(specDraftContainerWorkDir+"/"+planTicketsScratchDirName), timeoutMinutes, roleOverride.Thinking, roleOverride.Harness)
		sandboxCtx, cancel := context.WithTimeout(ctx, requestJobContainerDeadline(time.Duration(timeoutMinutes)*time.Minute, false, 0))
		defer cancel()
		_, workerUID := requestJobSandboxIdentity(cfg)
		// specPath and requestTextPath are two distinct host files
		// plan_tickets.py needs (--spec and --request); both must be
		// staged and translated, not just one, so specPath is passed as
		// its own specPath argument and requestTextPath as the second
		// extraRunInput slot -- both then mounted together at
		// /inputs/run (see runSandboxWithRetries' own doc comment on
		// extraRunInput for the live bug this fixes).
		res, err = runSandboxWithRetriesVia(
			cfg.Sandboxes, cfg.MeterLedgerRoot, sandboxCtx, r.Workspace, specPath, requestTextPath, script, logPath, 1,
			sandboxImage, cfg.Settings.SandboxDocker, sandboxUser, workerUID, r.ID, sandboxDataDir,
			cfg.Settings.SandboxMemory, cfg.Settings.SandboxCPUs, cfg.Settings.SandboxTmpfsSize,
			nil, relaySpec, nil, sandbox.RegistryProxyHooks{}, nil, sandbox.ComposeServicesHooks{},
			"", "", roleOverride.WorkerEnv, roleOverride.Skills, cfg.PlanTicketsInterpreter, args[1:]...,
		)
	} else {
		outDir := filepath.Join(scratchDir, "tickets")
		evidencePath := filepath.Join(scratchDir, "evidence.json")
		args := planTicketsArgs(script, r.Workspace, specPath, requestTextPath, verifyCommand, outDir, evidencePath, hostFiles, timeoutMinutes, roleOverride.Thinking, roleOverride.Harness)
		res, err = runner.RunWithRetries(ctx, r.Workspace, logPath, 1, nil, cfg.PlanTicketsInterpreter, args[1:]...)
	}
	retainDraftPrompts(dataDir, r.ID, "plan", r.Workspace, []string{planTicketsScratchDirName + "/session"})
	if err != nil {
		return nil, nil, fmt.Errorf("plan_tickets.py did not run to completion: %w", err)
	}

	tickets, evidence, readErr := readPlanTicketsOutputs(scratchDir)
	if readErr != nil {
		return nil, nil, readErr
	}
	// Model is factoryd's own, set from the relay's configured worker model id
	// (res.RelayWorkerModelID), never from anything plan_tickets.py itself
	// wrote to evidence.json -- see spec_draft_job.go's matching comment.
	if evidence != nil {
		evidence.Model = res.RelayWorkerModelID
		evidence.Thinking = roleOverride.Thinking
		// Spend: see spec_draft_job.go's matching comment. request_driver.go's
		// advancePlanning accumulates a re-draft's previous PlanEvidence.Spend
		// into this one before it replaces r.PlanEvidence.
		evidence.Spend = withDesignGuide(jobSpendFromResult(res, roleOverride.Role, time.Now()), guide)
	}
	if res.ExitCode != 0 {
		return nil, evidence, fmt.Errorf("%s", requestJobExitReason("plan_tickets.py", res.ExitCode, "", res.LogPath))
	}
	if len(tickets) == 0 {
		return nil, evidence, fmt.Errorf("%s", requestJobExitReason("plan_tickets.py", 0, "but wrote no tickets", res.LogPath))
	}
	return tickets, evidence, nil
}

// requestJobFailureTailBytes bounds how much of a request job's own log
// tail quotedLogReason reads -- generous enough for the one-line reason
// plan_tickets.py/draft_spec.py always print on a failure path without
// risking an unbounded halt-reason string.
const requestJobFailureTailBytes = 2 * 1024

// quotedLogReasonMaxLen bounds the single line quotedLogReason returns --
// an adversarial review of that failure-reason convention found the
// earlier version quoted up to requestJobFailureTailBytes flattened to
// one line, which could itself be arbitrarily long once run through
// strings.Fields/Join.
const quotedLogReasonMaxLen = 200

// quotedLogReasonScriptPrefixes are plan_tickets.py's/draft_spec.py's own
// print prefixes (see their run_plan/run_draft failure branches) --
// quotedLogReason looks for the LAST log line starting with one of these,
// not just the tail's own last line, so it quotes the script's own
// words specifically.
var quotedLogReasonScriptPrefixes = []string{"plan_tickets:", "draft_spec:"}

// quotedLogReason reads logPath's tail (via readTail, oracle_draft_job.go
// -- already sanitises control characters and secrets) and returns the
// bare reason text -- not pre-quoted, no ": " prefix -- or "" if the log
// is empty/missing/has nothing to say. Callers present it explicitly as
// a quote FROM THE LOG (requestJobExitReason's own "%q"), never as a
// fact this package itself vouches for -- see that function's doc
// comment for why. Shared by runPlanTicketsJobIn and runSpecDraftJobIn,
// whose scripts print the same one-line-reason-on-every-failure-path
// convention.
//
// An adversarial review found: the log this reads is captured from a
// sandboxed pi invocation an agent ultimately controls the stdout/stderr
// of, ahead of plan_tickets.py's/draft_spec.py's own reason line -- the
// earlier version's "flatten the whole 2 KB tail into one line" let a
// planted earlier line dominate the halt reason. This instead PREFERS the
// LAST line the script itself printed (identified by its own
// quotedLogReasonScriptPrefixes prefix, not by position), falling back to
// the log's own final non-blank line only when no such prefixed line
// exists at all (e.g. pi itself crashed before the script's own code
// resumed -- see the later fix to plan_tickets.py/draft_spec.py for why
// that case now also prints a reason). "Prefers", not "guarantees is the
// script's own line" -- see requestJobExitReason's own doc comment (a
// round-2 review) for why that stronger claim doesn't actually hold.
//
// A round-2 review also found: sanitize.Line, not just readTail's own
// sanitize.Text, so a `\r`/U+2028 embedded in the selected line can't
// still rewrite or split this reason once it reaches a real terminal.
func quotedLogReason(logPath string) string {
	tail := readTail(logPath, requestJobFailureTailBytes)
	if tail == "" {
		return ""
	}
	lines := strings.Split(tail, "\n")
	reason := lastLineWithPrefix(lines, quotedLogReasonScriptPrefixes)
	if reason == "" {
		reason = lastNonBlankLine(lines)
	}
	reason = sanitize.Line(reason)
	if reason == "" {
		return ""
	}
	return truncateBytes(reason, quotedLogReasonMaxLen)
}

// requestJobExitReason composes the halt-reason text for a
// plan_tickets.py/draft_spec.py failure: the factory's own established
// facts -- scriptName, exitCode, an optional extraFact ("but wrote no
// tickets") -- stated plainly, with the log's own last reason line, when
// there is one, appended as an explicit quote rather than folded into
// the same sentence as fact.
//
// A round-2 adversarial review found the earlier wording
// ("plan_tickets.py exited 2: <reason>") implied the quoted text was
// known to be the script's own final word --
// it is not guaranteed to be. pi runs inside the same sandboxed
// container and under the same uid as plan_tickets.py/draft_spec.py
// (nothing isolates the pi process from the harness script that invokes
// it), so a lingering pi child could
// still write to the log (effectively /proc/<pid>/fd/2) after the
// script's own final print, or the child could kill or hang the script
// so it never gets to print a reason line at all. quotedLogReason's
// prefix-matching still helps in the honest case (nothing else can spoof
// the LAST such line once the script really does print one and exit
// immediately after), but this function's own wording no longer claims
// that as a guarantee -- it presents the line as exactly what it is:
// text quoted from the log.
func requestJobExitReason(scriptName string, exitCode int, extraFact, logPath string) string {
	msg := fmt.Sprintf("%s exited %d", scriptName, exitCode)
	if extraFact != "" {
		msg += " " + extraFact
	}
	if reason := quotedLogReason(logPath); reason != "" {
		msg += fmt.Sprintf("; its log's last reason line: %q", reason)
	}
	return fmt.Sprintf("%s (see %s)", msg, logPath)
}

// lastLineWithPrefix returns the last of lines (trimmed) that starts with
// any of prefixes, or "".
func lastLineWithPrefix(lines []string, prefixes []string) string {
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		for _, prefix := range prefixes {
			if strings.HasPrefix(line, prefix) {
				return line
			}
		}
	}
	return ""
}

// lastNonBlankLine returns the last of lines that is non-empty once
// trimmed, or "".
func lastNonBlankLine(lines []string) string {
	for i := len(lines) - 1; i >= 0; i-- {
		if line := strings.TrimSpace(lines[i]); line != "" {
			return line
		}
	}
	return ""
}

// truncateBytes caps s at n bytes, at a UTF-8-safe boundary.
func truncateBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// stagePreviousPlan stages the tickets to revise for --previous-draft-file
// when the operator handed the plan over (`submit -plan-dir`), so the
// planning job revises their tickets instead of planning afresh. They are
// the request's current tickets when a reviewed plan exists (a plan_review
// rejection), else the tickets as handed over (the factory found the
// imported plan infeasible and removed it). It returns "" for a drafted
// plan. The tickets are staged as one file, each under a line naming it.
func stagePreviousPlan(dataDir string, r *request.Request, scratchDir, sandboxUser string) (string, error) {
	if !r.PlanImported {
		return "", nil
	}
	tickets, err := requestdriver.ReadTicketFiles(filepath.Join(request.Dir(dataDir, r.ID), "tickets"))
	if os.IsNotExist(err) || (err == nil && len(tickets) == 0) {
		tickets, err = requestdriver.ReadTicketFiles(request.ImportedTicketsDir(dataDir, r.ID))
	}
	if err != nil {
		return "", fmt.Errorf("read the handed-over tickets to revise: %w", err)
	}
	var current strings.Builder
	for _, ticket := range tickets {
		fmt.Fprintf(&current, "=== %s ===\n%s\n\n", ticket.Filename, strings.TrimRight(ticket.Content, "\n"))
	}
	if err := ensureRequestJobScratchDir(scratchDir, sandboxUser); err != nil {
		return "", fmt.Errorf("create plan-draft scratch dir: %w", err)
	}
	path := filepath.Join(scratchDir, previousDraftFileName)
	if err := writeRequestJobFeedbackFile(path, []byte(current.String()), sandboxUser); err != nil {
		return "", fmt.Errorf("stage the tickets to revise: %w", err)
	}
	return path, nil
}

// readPlanTicketsOutputs reads tickets/evidence.json back from the host
// side of the same bind-mounted scratch directory plan_tickets.py wrote
// into -- see readSpecDraftOutputs' own doc comment for why
// evidence.json is read (and returned) even when no tickets were
// produced. Tickets are returned sorted by filename, the dependency
// order plan_tickets.py's own prompt instructs the model to number them
// in.
func readPlanTicketsOutputs(scratchDir string) ([]requestdriver.DraftedTicket, *request.PlanEvidence, error) {
	var evidence *request.PlanEvidence
	evidencePath := filepath.Join(scratchDir, "evidence.json")
	if b, err := os.ReadFile(evidencePath); err == nil {
		var payload planTicketsEvidencePayload
		if jsonErr := json.Unmarshal(b, &payload); jsonErr == nil {
			if payload.SchemaVersion != planTicketsEvidenceSchemaVersion {
				return nil, nil, fmt.Errorf("%s schema_version %d, this factoryd understands %d", evidencePath, payload.SchemaVersion, planTicketsEvidenceSchemaVersion)
			}
			evidence = &request.PlanEvidence{
				Usage:         payload.Usage,
				AgentExitCode: payload.AgentExitCode,
				DurationS:     payload.DurationS,
				AgentsMDUsed:  payload.AgentsMDUsed,
			}
		}
	}
	ticketsDir := filepath.Join(scratchDir, "tickets")
	entries, err := os.ReadDir(ticketsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, evidence, nil
		}
		return nil, evidence, fmt.Errorf("read drafted tickets directory: %w", err)
	}
	var tickets []requestdriver.DraftedTicket
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		b, err := os.ReadFile(filepath.Join(ticketsDir, entry.Name()))
		if err != nil {
			return nil, evidence, fmt.Errorf("read drafted ticket %s: %w", entry.Name(), err)
		}
		tickets = append(tickets, requestdriver.DraftedTicket{Filename: entry.Name(), Content: string(b)})
	}
	sort.Slice(tickets, func(i, j int) bool { return tickets[i].Filename < tickets[j].Filename })
	return tickets, evidence, nil
}
