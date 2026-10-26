package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"buildgate/internal/request"
	"buildgate/internal/requestdriver"
)

// readBackScriptName is agent/pi/scripts/read_back.py's embedded harness
// file name. With -draft-spec-script given it is read from beside that
// script, so a checkout's scripts are used together.
const readBackScriptName = "read_back.py"

// readBackScratchDirName is where read_back.py writes its read-back and
// evidence, relative to the workspace mount (see specDraftScratchDirName for
// why a scratch directory in the workspace).
const readBackScratchDirName = ".factory-read-back"

// readBackRefusedExitCode is read_back.py's REFUSED_EXIT_CODE: the model
// wrote a read-back the script refused for its shape or size.
const readBackRefusedExitCode = 3

// maxReadBackFileBytes bounds the read of read_back.py's output (its own
// MAX_FILE_BYTES); a larger file is refused unread.
const maxReadBackFileBytes = 64 * 1024

// readBackArgs is read_back.py's argv after the script path. specPath and
// workspace are the host paths runSpecStageJob translates for a sandboxed
// launch.
func readBackArgs(workspace, specPath, outPath, evidencePath string, timeoutMinutes int, thinking, harness string) []string {
	args := []string{
		"--workspace", workspace,
		"--spec", specPath,
		"--out", outPath,
		"--evidence", evidencePath,
		"--timeout-minutes", fmt.Sprint(timeoutMinutes),
	}
	if thinking != "" {
		args = append(args, "--thinking", thinking)
	}
	return append(args, "--harness", harness)
}

// readBackInputFileName is where runReadBackJobIn stages the spec text the
// job reads, in the request's directory beside spec.md: the text is the one
// this spec_drafting pass is about to put up for review, not yet spec.md.
const readBackInputFileName = "read-back-input.md"

// runReadBackJob is the read-back job (requestdriver.ReadBackRunner): it
// runs read_back.py over spec in a throwaway worktree of the repository,
// through the spec-drafting job's launch path (runSpecStageJob), and returns
// the read-back it wrote, unvalidated (the request driver validates it). A
// refused read-back's error wraps request.ErrReadBackRefused.
func runReadBackJob(ctx context.Context, dataDir string, r *request.Request, spec string, cfg requestdriver.WorkerConfig) (rb *request.ReadBack, evidence *request.SpecEvidence, err error) {
	err = withDraftingWorktree(dataDir, r, func(job *request.Request) error {
		var jobErr error
		rb, evidence, jobErr = runReadBackJobIn(ctx, dataDir, job, spec, cfg)
		return jobErr
	})
	return rb, evidence, err
}

func runReadBackJobIn(ctx context.Context, dataDir string, r *request.Request, spec string, cfg requestdriver.WorkerConfig) (*request.ReadBack, *request.SpecEvidence, error) {
	explicit := ""
	if cfg.DraftSpecScript != "" {
		explicit = filepath.Join(filepath.Dir(cfg.DraftSpecScript), readBackScriptName)
	}
	script, err := resolveHarnessScript(explicit, readBackScriptName)
	if err != nil {
		return nil, nil, fmt.Errorf("resolve %s: %w", readBackScriptName, err)
	}
	specPath := filepath.Join(request.Dir(dataDir, r.ID), readBackInputFileName)
	if err := os.WriteFile(specPath, []byte(spec), 0o600); err != nil {
		return nil, nil, fmt.Errorf("stage the read-back's spec: %w", err)
	}
	defer os.Remove(specPath)
	scratchDir := filepath.Join(r.Workspace, readBackScratchDirName)
	_ = os.RemoveAll(scratchDir)
	defer os.RemoveAll(scratchDir)

	res, roleOverride, err := runSpecStageJob(ctx, dataDir, r, cfg, specStageJob{
		script:      script,
		scriptName:  readBackScriptName,
		scratchName: readBackScratchDirName,
		inputPath:   specPath,
		logName:     "read_back",
		promptKind:  "read_back",
		sessions:    []string{readBackScratchDirName + "/session"},
		args: func(scratch string, _ bool, timeoutMinutes int, role requestJobRoleOverride) []string {
			return readBackArgs(r.Workspace, specPath, scratch+"/read-back.json", scratch+"/evidence.json", timeoutMinutes, role.Thinking, role.Harness)
		},
	})
	if err != nil {
		return nil, nil, err
	}
	evidence, err := readSpecStageEvidence(scratchDir)
	if err != nil {
		return nil, nil, err
	}
	if evidence != nil {
		evidence.Model = res.RelayWorkerModelID
		evidence.Thinking = roleOverride.Thinking
		evidence.Spend = jobSpendFromResult(res, roleOverride.Role, time.Now())
	}
	switch res.ExitCode {
	case 0:
	case readBackRefusedExitCode:
		return nil, evidence, fmt.Errorf("%w: %s", request.ErrReadBackRefused, requestJobExitReason(readBackScriptName, res.ExitCode, "", res.LogPath))
	default:
		return nil, evidence, errors.New(requestJobExitReason(readBackScriptName, res.ExitCode, "", res.LogPath))
	}
	rb, err := readReadBackOutput(filepath.Join(scratchDir, "read-back.json"))
	return rb, evidence, err
}

// readReadBackOutput reads read_back.py's read-back JSON. A file that is
// missing, over maxReadBackFileBytes, not one JSON object or carrying a field
// the read-back has no place for is refused (request.ErrReadBackRefused).
func readReadBackOutput(path string) (*request.ReadBack, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("%w: read %s: %v", request.ErrReadBackRefused, filepath.Base(path), err)
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxReadBackFileBytes+1))
	if err != nil {
		return nil, fmt.Errorf("%w: read %s: %v", request.ErrReadBackRefused, filepath.Base(path), err)
	}
	if len(data) > maxReadBackFileBytes {
		return nil, fmt.Errorf("%w: %s is over %d bytes", request.ErrReadBackRefused, filepath.Base(path), maxReadBackFileBytes)
	}
	var out struct {
		Summary   string `json:"summary"`
		Questions []struct {
			ID   string `json:"id"`
			Text string `json:"text"`
		} `json:"questions"`
	}
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&out); err != nil {
		return nil, fmt.Errorf("%w: %s is not a read-back: %v", request.ErrReadBackRefused, filepath.Base(path), err)
	}
	rb := &request.ReadBack{Summary: out.Summary}
	for _, q := range out.Questions {
		rb.Questions = append(rb.Questions, request.ReadBackQuestion{ID: q.ID, Text: q.Text})
	}
	return rb, nil
}
