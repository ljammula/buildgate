package requestsubmit

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"buildgate/internal/memory"
	"buildgate/internal/projectconfig"
	"buildgate/internal/release"
	"buildgate/internal/request"
	"buildgate/internal/testfixture"
)

// An approved block is still refused when the registry proxy is off, with
// the run's own sentence.
func TestSubmitRefusesASandboxBlockWithTheRegistryProxyOff(t *testing.T) {
	workspace := testfixture.NewGitRepo(t)
	block := "verify_command: \"make ci-verify\"\nsandbox:\n  version: 1\n  install: [\"npm ci\"]\n"
	if err := os.WriteFile(filepath.Join(workspace, ".factory.yml"), []byte(block), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", ".factory.yml"}, {"commit", "-q", "-m", "factory yml"}} {
		if out, err := exec.Command("git", append([]string{"-C", workspace}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	testfixture.CommitAgentsFile(t, workspace)
	dataDir := t.TempDir()
	cfg, _, err := projectconfig.Load(workspace)
	if err != nil {
		t.Fatal(err)
	}
	root := release.RepositoryRoot(workspace)
	approval := projectconfig.Approval{SandboxSHA256: cfg.Sandbox.SHA256()}
	if err := projectconfig.OpenApprovals(dataDir).Approve(memory.StoreKey(filepath.Base(root), root), approval); err != nil {
		t.Fatal(err)
	}
	params := Params{WorkspaceArg: workspace, DataDir: dataDir, RequestText: "do a thing", Source: request.Source{Kind: request.SourceText}, PreflightProfile: "brownfield", PreflightProfileExplicit: true}
	params.Settings.RegistryProxyConfigured, params.Settings.RegistryProxy = true, false
	if _, err := Submit(params); err == nil || !strings.HasPrefix(err.Error(), projectconfig.SandboxNeedsRegistryProxy) {
		t.Fatalf("Submit error = %v, want the registry proxy refusal", err)
	}
	params.Settings.RegistryProxy = true
	if _, err := Submit(params); err != nil {
		t.Fatalf("Submit with the proxy on: %v", err)
	}
}

// A workspace whose committed .factory.yml carries a sandbox block is
// refused at submit, with the run's own sentence, until the operator
// approved the block's hash for that repository.
func TestSubmitRefusesAnUnapprovedSandboxBlock(t *testing.T) {
	workspace := testfixture.NewGitRepo(t)
	block := "verify_command: \"make ci-verify\"\nsandbox:\n  version: 1\n  install: [\"npm ci\"]\n"
	if err := os.WriteFile(filepath.Join(workspace, ".factory.yml"), []byte(block), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", ".factory.yml"}, {"commit", "-q", "-m", "factory yml"}} {
		if out, err := exec.Command("git", append([]string{"-C", workspace}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	testfixture.CommitAgentsFile(t, workspace)
	dataDir := t.TempDir()
	params := Params{WorkspaceArg: workspace, DataDir: dataDir, RequestText: "do a thing", Source: request.Source{Kind: request.SourceText}, PreflightProfile: "brownfield", PreflightProfileExplicit: true}
	_, err := Submit(params)
	var refusal *Refusal
	if !errors.As(err, &refusal) || !strings.Contains(err.Error(), "): factoryd sandbox approve -repo ") || !strings.Contains(err.Error(), "is not approved (approvals are read from ") {
		t.Fatalf("Submit error = %v, want the unapproved-block refusal", err)
	}
	if !strings.Contains(refusal.WithoutFlags(), "the operator has to run factoryd sandbox approve") {
		t.Errorf("WithoutFlags = %q", refusal.WithoutFlags())
	}
	cfg, _, err := projectconfig.Load(workspace)
	if err != nil {
		t.Fatal(err)
	}
	root := release.RepositoryRoot(workspace)
	approval := projectconfig.Approval{SandboxSHA256: cfg.Sandbox.SHA256()}
	if err := projectconfig.OpenApprovals(dataDir).Approve(memory.StoreKey(filepath.Base(root), root), approval); err != nil {
		t.Fatal(err)
	}
	if _, err := Submit(params); err != nil {
		t.Fatalf("Submit after approval: %v", err)
	}
}
