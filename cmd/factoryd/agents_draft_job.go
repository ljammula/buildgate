package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"buildgate/internal/modelrole"
	"buildgate/internal/requestdriver"
	"buildgate/internal/requestsubmit"
	"buildgate/internal/run"
	"buildgate/internal/sandbox"
	"buildgate/internal/sanitize"
	"buildgate/internal/sessionconfig"
	wsisolation "buildgate/internal/workspace"
)

const (
	agentsDraftScriptName          = "draft_agents.py"
	agentsDraftScratchDirName      = ".factory-agents-draft"
	agentsDraftContainerOutputPath = "/workspace/" + agentsDraftScratchDirName + "/AGENTS.md"
	agentsDraftTimeoutMinutes      = 10
	agentsDraftMaxBytes            = 1 << 20
	agentsDraftInterpreter         = "python3"
)

// agentsDraftJobInput is the doctor-specific subset of WorkerConfig. The
// drafting job remains a model job: the caller supplies the configured
// sandbox runtime, image, route settings and meter ledger root rather than
// giving this path a host-side model or command fallback.
type agentsDraftJobInput struct {
	Workspace       string
	DataDir         string
	SandboxImage    string
	MeterLedgerRoot string
	EgressCABundle  string
	Settings        sessionconfig.Settings
	Sandboxes       sandbox.Runtime
}

type agentsDraftJobResult struct {
	Drafted bool
}

func doctorDraftTargetRepoAgentsFileIfNeeded(dp *deps, in doctorInputs, checks []doctorCheck, w io.Writer) []doctorCheck {
	if in.targetRepo == "" {
		return checks
	}
	return doctorDraftTargetRepoAgentsFile(dp, in, checks, w)
}

// agentsDraftRepoRoot is the git top level of the -target-repo path, the
// directory whose HEAD the AGENTS.md check reads, so a subdirectory or a
// symlinked path drafts the root file the check looks for.
func agentsDraftRepoRoot(dp *deps, targetRepo string) (string, error) {
	top, err := dp.forge.gitToplevel(targetRepo)
	if err != nil {
		return "", fmt.Errorf("%s is not inside a git checkout: %w", targetRepo, err)
	}
	return filepath.EvalSymlinks(top)
}

// doctorDraftTargetRepoAgentsFile runs the one doctor -fix repair that is
// allowed to call a model. The original submit-style check stays in place;
// only its AGENTS.md row is replaced, and a successful draft deliberately
// remains a failure until the operator reviews and commits it.
func doctorDraftTargetRepoAgentsFile(dp *deps, in doctorInputs, checks []doctorCheck, w io.Writer) []doctorCheck {
	name := run.RootInstructionFile + " for " + in.targetRepo
	for _, check := range checks {
		if check.Name == name && check.Err == nil {
			return checks
		}
	}
	repoRoot, err := agentsDraftRepoRoot(dp, in.targetRepo)
	if err != nil {
		return doctorReplaceCheck(checks, doctorCheck{
			Name: name,
			Err:  errors.New(oneLineAgentsDraftError(err)),
			Fix:  "pass the root of a git checkout with a commit at HEAD to -target-repo",
		})
	}
	result, err := runAgentsDraftJob(context.Background(), agentsDraftJobInput{
		Workspace:       repoRoot,
		DataDir:         in.dataDir,
		SandboxImage:    in.sandboxImage,
		MeterLedgerRoot: meterLedgerRoot(),
		EgressCABundle:  in.egressCABundle,
		Settings:        in.settings,
		Sandboxes:       dp.sandbox.runtime(),
	})
	if err != nil {
		return doctorReplaceCheck(checks, doctorCheck{
			Name: name,
			Err:  errors.New(oneLineAgentsDraftError(err)),
			Fix:  "fix the named sandbox/model-job reason, then rerun `factoryd doctor -target-repo <repo> -fix`",
		})
	}
	if !result.Drafted {
		return checks
	}
	fmt.Fprintf(w, "drafted %s at the repository root; review it and commit it before running a request\n", run.RootInstructionFile)
	return doctorReplaceCheck(checks, doctorCheck{
		Name: name,
		Err:  errors.New("drafted, not committed"),
		Fix:  "review the uncommitted AGENTS.md, commit it at the repository root, then run doctor again; runs read committed HEAD",
	})
}

// runAgentsDraftJob drafts the root AGENTS.md of the repository at
// in.Workspace (its git top level) in the sandbox and installs only a
// successful, non-empty draft there. The model works in a detached worktree
// at HEAD, as the spec draft job does: the committed tree a run reads, with
// no ignored or untracked file (an .env, node_modules) and no write to the
// operator's checkout. It writes to a scratch path in that worktree; the root
// file is created by this process only after the sandbox exits successfully,
// so a failed job leaves no AGENTS.md behind.
func runAgentsDraftJob(ctx context.Context, in agentsDraftJobInput) (agentsDraftJobResult, error) {
	workspace, err := filepath.Abs(in.Workspace)
	if err != nil {
		return agentsDraftJobResult{}, fmt.Errorf("resolve target repository: %w", err)
	}
	root := filepath.Join(workspace, run.RootInstructionFile)
	if _, err := os.Lstat(root); err == nil {
		return agentsDraftJobResult{}, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return agentsDraftJobResult{}, fmt.Errorf("inspect %s: %w", run.RootInstructionFile, err)
	}
	if in.SandboxImage == "" {
		return agentsDraftJobResult{}, errors.New("no sandbox image configured for the AGENTS.md draft job")
	}
	if in.Sandboxes == nil {
		return agentsDraftJobResult{}, errors.New("no sandbox runtime configured for the AGENTS.md draft job")
	}
	if err := os.MkdirAll(in.DataDir, 0o750); err != nil {
		return agentsDraftJobResult{}, fmt.Errorf("create doctor data directory: %w", err)
	}
	script, err := resolveHarnessScript("", agentsDraftScriptName)
	if err != nil {
		return agentsDraftJobResult{}, err
	}
	sandboxDataDir, err := requestdriver.SandboxDataDirFor(in.DataDir)
	if err != nil {
		return agentsDraftJobResult{}, err
	}
	cfg := requestdriver.WorkerConfig{
		Sandboxes:       in.Sandboxes,
		MeterLedgerRoot: in.MeterLedgerRoot,
		SandboxImage:    in.SandboxImage,
		EgressCABundle:  in.EgressCABundle,
		Settings:        in.Settings,
	}
	roleOverride, err := resolveRequestJobRole(cfg, modelrole.StagePlanning, "AGENTS.md drafting", "", "")
	if err != nil {
		return agentsDraftJobResult{}, err
	}
	runID, err := agentsDraftRunID()
	if err != nil {
		return agentsDraftJobResult{}, err
	}
	relaySpec, err := resolveRequestJobRelaySpec(cfg, "AGENTS.md drafting", runID, sandboxDataDir, roleOverride)
	if err != nil {
		return agentsDraftJobResult{}, err
	}
	if err := os.MkdirAll(filepath.Join(in.DataDir, "logs"), 0o750); err != nil {
		return agentsDraftJobResult{}, fmt.Errorf("create AGENTS.md draft log directory: %w", err)
	}
	jobDir := filepath.Join(in.DataDir, "doctor", runID)
	if err := os.MkdirAll(jobDir, 0o750); err != nil {
		return agentsDraftJobResult{}, fmt.Errorf("create AGENTS.md draft staging directory: %w", err)
	}
	defer os.RemoveAll(jobDir)
	defer os.RemoveAll(run.Dir(in.DataDir, runID))
	draftWorkspace := filepath.Join(jobDir, "worktree")
	// Through the workspace mutators, not bare git: worktree add/remove write
	// shared Git metadata that the git metadata lock serializes.
	if err := wsisolation.AddDetachedWorktree(workspace, draftWorkspace, "HEAD"); err != nil {
		return agentsDraftJobResult{}, fmt.Errorf("create AGENTS.md draft worktree at HEAD: %w", err)
	}
	defer func() { _ = wsisolation.RemoveWorktreeOnly(workspace, draftWorkspace) }()
	promptPath := filepath.Join(jobDir, "instructions.txt")
	if err := os.WriteFile(promptPath, []byte(requestsubmit.AgentsFilePrompt+"\n"), 0o600); err != nil {
		return agentsDraftJobResult{}, fmt.Errorf("stage AGENTS.md draft instructions: %w", err)
	}
	logPath := func(attempt int) string {
		name := "agents_draft.log"
		if attempt > 1 {
			name = fmt.Sprintf("agents_draft.attempt%d.log", attempt)
		}
		return filepath.Join(in.DataDir, "logs", runID+"-"+name)
	}
	args := agentsDraftArgs(script, draftWorkspace, promptPath, agentsDraftContainerOutputPath, agentsDraftTimeoutMinutes, roleOverride.Thinking, roleOverride.Harness)
	sandboxCtx, cancel := context.WithTimeout(ctx, requestJobContainerDeadline(time.Duration(agentsDraftTimeoutMinutes)*time.Minute, false, 0))
	defer cancel()
	sandboxUser, workerUID := requestJobSandboxIdentity(cfg)
	res, err := runSandboxWithRetriesVia(
		in.Sandboxes, in.MeterLedgerRoot, sandboxCtx, draftWorkspace, promptPath, "", script, logPath, 1,
		in.SandboxImage, in.Settings.SandboxDocker, sandboxUser, workerUID, runID, sandboxDataDir,
		in.Settings.SandboxMemory, in.Settings.SandboxCPUs, in.Settings.SandboxTmpfsSize,
		nil, relaySpec, nil, sandbox.RegistryProxyHooks{}, nil, sandbox.ComposeServicesHooks{},
		"", "", roleOverride.WorkerEnv, roleOverride.Skills, agentsDraftInterpreter, args[1:]...,
	)
	if err != nil {
		return agentsDraftJobResult{}, fmt.Errorf("AGENTS.md draft job failed: %s", oneLineAgentsDraftError(err))
	}
	if res.ExitCode != 0 {
		return agentsDraftJobResult{}, fmt.Errorf("%s", requestJobExitReason(agentsDraftScriptName, res.ExitCode, "", res.LogPath))
	}
	draft, err := readAgentsDraft(draftWorkspace, filepath.Join(agentsDraftScratchDirName, run.RootInstructionFile))
	if err != nil {
		return agentsDraftJobResult{}, fmt.Errorf("%s", requestJobExitReason(agentsDraftScriptName, 0, "but wrote no readable AGENTS.md draft: "+oneLineAgentsDraftError(err), res.LogPath))
	}
	if strings.TrimSpace(string(draft)) == "" {
		return agentsDraftJobResult{}, fmt.Errorf("%s", requestJobExitReason(agentsDraftScriptName, 0, "but wrote an empty AGENTS.md draft", res.LogPath))
	}
	if err := installAgentsDraft(root, draft); err != nil {
		if errors.Is(err, os.ErrExist) {
			return agentsDraftJobResult{}, nil
		}
		return agentsDraftJobResult{}, fmt.Errorf("install uncommitted AGENTS.md draft: %w", err)
	}
	return agentsDraftJobResult{Drafted: true}, nil
}

func agentsDraftArgs(script, workspace, instructionsPath, outputPath string, timeoutMinutes int, thinking, harness string) []string {
	args := []string{
		script,
		"--workspace", workspace,
		"--instructions", instructionsPath,
		"--out", outputPath,
		"--timeout-minutes", fmt.Sprint(timeoutMinutes),
	}
	if thinking != "" {
		args = append(args, "--thinking", thinking)
	}
	return append(args, "--harness", harness)
}

func agentsDraftRunID() (string, error) {
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate AGENTS.md draft run id: %w", err)
	}
	return "doctor-agents-" + hex.EncodeToString(raw[:]), nil
}

// readAgentsDraft reads the model's draft at name inside the sandbox's
// worktree. The sandbox wrote that tree, so the read goes through an os.Root
// (no component may be a symlink out of it) and opens with O_NONBLOCK (a FIFO
// put in its place cannot hang doctor).
func readAgentsDraft(worktree, name string) ([]byte, error) {
	root, err := os.OpenRoot(worktree)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	file, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("draft is not a regular file")
	}
	content, err := io.ReadAll(io.LimitReader(file, agentsDraftMaxBytes+1))
	if err != nil {
		return nil, err
	}
	if len(content) > agentsDraftMaxBytes {
		return nil, fmt.Errorf("draft exceeds %d bytes", agentsDraftMaxBytes)
	}
	return content, nil
}

// installAgentsDraft writes content to a temporary file beside path and links
// it into place, so a failed write leaves no partial AGENTS.md and a file
// that appeared meanwhile is never replaced (os.ErrExist).
func installAgentsDraft(path string, content []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".draft-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(content); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o644); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Link(tmp.Name(), path)
}

func oneLineAgentsDraftError(err error) string {
	if err == nil {
		return "unknown error"
	}
	return sanitize.Line(err.Error())
}
