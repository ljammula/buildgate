package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
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
	result, err := runAgentsDraftJob(context.Background(), agentsDraftJobInput{
		Workspace:       in.targetRepo,
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

// runAgentsDraftJob drafts the target repository's root AGENTS.md in the
// sandbox and installs only the successful, non-empty draft in the target
// worktree. The model writes to a scratch path; the root file is created by
// this process only after the sandbox exits successfully, so a failed job
// cannot leave a partial AGENTS.md behind.
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
	jobDir, err := os.MkdirTemp(in.DataDir, "doctor-agents-")
	if err != nil {
		return agentsDraftJobResult{}, fmt.Errorf("create AGENTS.md draft staging directory: %w", err)
	}
	defer os.RemoveAll(jobDir)
	defer os.RemoveAll(run.Dir(in.DataDir, runID))
	draftWorkspace, err := copyAgentsDraftWorkspace(workspace, jobDir)
	if err != nil {
		return agentsDraftJobResult{}, err
	}
	defer os.RemoveAll(draftWorkspace)
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
	draftPath := filepath.Join(draftWorkspace, agentsDraftScratchDirName, run.RootInstructionFile)
	draft, err := readAgentsDraft(draftPath)
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

// copyAgentsDraftWorkspace makes the model's writable sandbox workspace a
// private copy of the repository. The operator's checkout is therefore only
// read by the factory; the root AGENTS.md is the one host-side write made
// after a successful sandbox exit. Git metadata is deliberately omitted: the
// drafting prompt needs repository files and commands, never host-side Git
// credentials or a repository it could commit to.
func copyAgentsDraftWorkspace(source, parent string) (string, error) {
	destination, err := os.MkdirTemp(parent, "workspace-")
	if err != nil {
		return "", fmt.Errorf("create private AGENTS.md draft workspace: %w", err)
	}
	removeOnError := true
	defer func() {
		if removeOnError {
			_ = os.RemoveAll(destination)
		}
	}()
	source, err = filepath.Abs(source)
	if err != nil {
		return "", fmt.Errorf("resolve AGENTS.md draft source: %w", err)
	}
	destination, err = filepath.Abs(destination)
	if err != nil {
		return "", fmt.Errorf("resolve AGENTS.md draft workspace: %w", err)
	}
	err = filepath.WalkDir(source, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		if rel == ".git" || rel == agentsDraftScratchDirName || agentsPathWithin(rel, ".git") || agentsPathWithin(rel, agentsDraftScratchDirName) {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		// When the data directory is inside the target checkout, the private
		// destination is inside the source tree too. Skip the data-directory
		// ancestor and the destination itself to avoid copying the copy
		// recursively or exposing factory runtime state to the model.
		if path != source && (agentsPathWithin(path, destination) || agentsPathWithin(destination, path)) {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		target := filepath.Join(destination, rel)
		if entry.IsDir() {
			return os.MkdirAll(target, 0o750)
		}
		if entry.Type()&fs.ModeSymlink != 0 {
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			return os.Symlink(link, target)
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		return copyAgentsDraftFile(path, target, entry)
	})
	if err != nil {
		return "", fmt.Errorf("copy repository for sandboxed AGENTS.md draft: %w", err)
	}
	removeOnError = false
	return destination, nil
}

func agentsPathWithin(path, parent string) bool {
	rel, err := filepath.Rel(parent, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func copyAgentsDraftFile(source, target string, entry fs.DirEntry) error {
	info, err := entry.Info()
	if err != nil {
		return err
	}
	in, err := os.OpenFile(source, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY|syscall.O_NOFOLLOW, info.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
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

func readAgentsDraft(path string) ([]byte, error) {
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
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

func installAgentsDraft(path string, content []byte) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY|syscall.O_NOFOLLOW, 0o644)
	if err != nil {
		return err
	}
	if _, writeErr := file.Write(content); writeErr != nil {
		_ = file.Close()
		return writeErr
	}
	return file.Close()
}

func oneLineAgentsDraftError(err error) string {
	if err == nil {
		return "unknown error"
	}
	return sanitize.Line(err.Error())
}
