package main

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"buildgate/internal/projectconfig"
	"buildgate/internal/release"
	"buildgate/internal/run"
	"buildgate/internal/sandbox"
	"buildgate/internal/sessionconfig"
)

// approveIn records the approval of the committed block of repo in dataDir.
func approveIn(t *testing.T, repo, dataDir, hash string) {
	t.Helper()
	if err := projectconfig.OpenApprovals(dataDir).Approve(repoKeyOf(repo), projectconfig.Approval{SandboxSHA256: hash, ApprovedBy: "me"}); err != nil {
		t.Fatal(err)
	}
}

// -baseline-check runs on a data dir of its own; the operator's approvals
// are still the ones it reads.
func TestBaselineCheckReadsTheOperatorsApprovals(t *testing.T) {
	repo, dataDir, hash := sandboxRepo(t, sandboxFactoryYML)
	approveIn(t, repo, dataDir, hash)

	tr := &ticketRun{}
	tr.flags, tr.rf = newRunFlags()
	if err := tr.flags.Parse([]string{"-baseline-check", "-workspace", repo, "-data-dir", dataDir}); err != nil {
		t.Fatal(err)
	}
	tr.ticket, tr.spec, tr.workspace, tr.dataDir = tr.rf.ticket, tr.rf.spec, tr.rf.workspace, tr.rf.dataDir
	tr.repository, tr.priorRun, tr.onBranch, tr.skipProjectCheck, tr.fullSuiteCadence = tr.rf.repository, tr.rf.priorRun, tr.rf.onBranch, tr.rf.skipProjectCheck, tr.rf.fullSuiteCadence
	tr.fullSuiteCommand = tr.rf.fullSuiteCommand
	if err := tr.prepareBaselineCheck(); err != nil {
		t.Fatal(err)
	}
	defer tr.runDeferred()
	if *tr.dataDir == dataDir {
		t.Fatalf("the check kept the operator's data dir %s; the test needs its own", dataDir)
	}
	if err := tr.applyCommittedProjectConfig(false); err != nil {
		t.Fatal(err)
	}
	root := release.RepositoryRoot(repo)
	proxy := true
	tr.repositoryRoot, tr.project, tr.registryProxy, tr.sandboxDataDir = root, filepath.Base(root), &proxy, *tr.dataDir
	if err := tr.requireSandboxApproved(); err != nil {
		t.Fatalf("the baseline check was refused an approved block: %v", err)
	}
	// Revoked in the operator's data dir, it is refused.
	if _, err := projectconfig.OpenApprovals(dataDir).Revoke(repoKeyOf(repo), hash); err != nil {
		t.Fatal(err)
	}
	if err := tr.requireSandboxApproved(); err == nil {
		t.Error("a revoked block passed the baseline check")
	}
}

// An approved block's cache paths are kept out of every build's commit.
func TestApprovedSandboxBlockExcludesItsCachePathsFromCommits(t *testing.T) {
	repo, dataDir, hash := sandboxRepo(t, sandboxFactoryYML)
	approveIn(t, repo, dataDir, hash)
	dp := newTestDeps(t)
	tr := sandboxTicketRun(t, dp, repo, dataDir, true)
	if _, err := runGit(t, repo, "check-ignore", "-q", "node_modules/pkg/index.js"); err == nil {
		t.Fatal("node_modules was ignored before the block was approved")
	}
	if err := tr.requireSandboxApproved(); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(t, repo, "check-ignore", "-q", "node_modules/pkg/index.js"); err != nil {
		t.Errorf("node_modules is not ignored after approval: %v", err)
	}
	if _, err := runGit(t, repo, "check-ignore", "-q", "src/node_modules_x"); err == nil {
		t.Error("the exclusion is not anchored to the cache path")
	}
}

// The registry proxy's routes are the operator's, narrowed to the committed
// block's registries.
func TestRegistryProxyPolicyIsNarrowedByTheCommittedBlock(t *testing.T) {
	repo, dataDir, _ := sandboxRepo(t, sandboxFactoryYML)
	dp := newTestDeps(t)
	tr := sandboxTicketRun(t, dp, repo, dataDir, true)
	settings := sessionconfig.DefaultSettings()
	tr.registryProxyNPMUpstream, tr.registryProxyPyPIUpstream, tr.registryProxyPyPIFilesUpstream = &settings.RegistryProxyNPMUpstream, &settings.RegistryProxyPyPIUpstream, &settings.RegistryProxyPyPIFilesUpstream
	tr.registryProxyGoUpstream, tr.registryProxyGoSumDBUpstream = &settings.RegistryProxyGoUpstream, &settings.RegistryProxyGoSumDBUpstream
	tr.registryProxyMavenUpstream, tr.registryProxyRubyGemsUpstream = &settings.RegistryProxyMavenUpstream, &settings.RegistryProxyRubyGemsUpstream
	tr.registryProxyCacheBytes, tr.registryProxyMaxObjectBytes = &settings.RegistryProxyCacheBytes, &settings.RegistryProxyMaxObjectBytes
	tr.registryProxyMaxConcurrent, tr.registryProxyUpstreamTimeout = &settings.RegistryProxyMaxConcurrentUpstream, &settings.RegistryProxyUpstreamTimeout
	image := "registry-proxy@sha256:" + strings.Repeat("a", 64)
	tr.registryProxyImage = &image

	policy, err := tr.buildRegistryProxyPolicy()
	if err != nil {
		t.Fatal(err)
	}
	var prefixes []string
	for _, r := range policy.Routes {
		prefixes = append(prefixes, r.Prefix)
	}
	if want := []string{"/npm/"}; !reflect.DeepEqual(prefixes, want) {
		t.Errorf("routes = %q, want only the block's npm %q", prefixes, want)
	}
	tr.sandbox = nil
	policy, err = tr.buildRegistryProxyPolicy()
	// The session config's default upstreams are the public registries: the
	// whole default route table, redirect hosts included.
	if err != nil || !reflect.DeepEqual(policy.Routes, sandbox.DefaultRegistryProxyRoutes()) {
		t.Errorf("without a block: routes %+v, %v; want every default route", policy.Routes, err)
	}
}

// An approved run's workflow input carries the block for the install image,
// and its setup list is the repository's setup alone.
func TestApprovedRunsWorkflowInputCarriesTheSandboxBlock(t *testing.T) {
	repo, dataDir, hash := sandboxRepo(t, strings.Replace(sandboxFactoryYML, "sandbox:", "setup: [\"make generate\"]\nsandbox:", 1))
	approveIn(t, repo, dataDir, hash)
	tr := sandboxTicketRun(t, newTestDeps(t), repo, dataDir, true)
	if err := tr.requireSandboxApproved(); err != nil {
		t.Fatal(err)
	}
	in := runOptions{SetupCommands: tr.setup, Sandbox: tr.sandbox, SandboxRepoKey: repoKeyOf(repo)}.workflowInput()
	if want := []string{"make generate"}; !reflect.DeepEqual(in.SetupCommands, want) {
		t.Errorf("workflow SetupCommands = %q, want setup alone %q", in.SetupCommands, want)
	}
	if in.SandboxSHA256 != hash || !reflect.DeepEqual(in.InstallCommands, []string{"npm ci"}) || in.InstallRepoKey != repoKeyOf(repo) {
		t.Errorf("workflow input sandbox = %q %q %q", in.SandboxSHA256, in.InstallCommands, in.InstallRepoKey)
	}
	if bare := (runOptions{}).workflowInput(); bare.SandboxSHA256 != "" || bare.InstallCommands != nil || bare.InstallRepoKey != "" {
		t.Error("a run without a block carries one")
	}
}

// A reclaimed result runs the commit's block only while it is approved.
func TestReclaimRefusesARevokedSandboxBlock(t *testing.T) {
	repo, dataDir, hash := sandboxRepo(t, sandboxFactoryYML)
	cfg, _, err := projectconfig.Load(repo)
	if err != nil {
		t.Fatal(err)
	}
	root := release.RepositoryRoot(repo)
	r := &run.Run{ID: "r1", RepositoryRoot: root, ProjectConfigCommitSHA: projectconfig.HeadCommit(repo), ProjectConfigSHA256: cfg.SHA256}
	dp := newTestDeps(t)
	_, _, _, reason, err := dispatchedRepoChecks(context.Background(), dp, dataDir, root, r)
	if reason != reclaimSandboxNotApproved || err == nil || !strings.Contains(err.Error(), "factoryd sandbox approve -repo") {
		t.Fatalf("unapproved: reason %q, %v", reason, err)
	}
	approveIn(t, repo, dataDir, hash)
	_, setup, sandboxSHA, reason, err := dispatchedRepoChecks(context.Background(), dp, dataDir, root, r)
	if reason != "" || err != nil || len(setup) != 0 || sandboxSHA != hash {
		t.Fatalf("approved: setup %q, sandbox %q, reason %q, %v", setup, sandboxSHA, reason, err)
	}
	if _, err := projectconfig.OpenApprovals(dataDir).Revoke(repoKeyOf(repo), hash); err != nil {
		t.Fatal(err)
	}
	if _, _, _, reason, _ := dispatchedRepoChecks(context.Background(), dp, dataDir, root, r); reason != reclaimSandboxNotApproved {
		t.Errorf("revoked: reason %q, want a refusal", reason)
	}
}

// approve and show print the approvals file, and a corrupt one is named.
func TestSandboxApproveNamesTheApprovalsFile(t *testing.T) {
	repo, dataDir, hash := sandboxRepo(t, sandboxFactoryYML)
	store := projectconfig.OpenApprovals(dataDir)
	file := store.Path(repoKeyOf(repo))
	out, err := runSandbox(t, "show", "-repo", repo, "-data-dir", dataDir)
	if err != nil || !strings.Contains(out, file) {
		t.Errorf("show = %q, %v; want the approvals file %s", out, err, file)
	}
	if out, err = runSandbox(t, "approve", "-repo", repo, "-data-dir", dataDir, "-sha256", hash); err != nil || !strings.Contains(out, "recorded in "+file) {
		t.Errorf("approve = %q, %v; want the approvals file %s", out, err, file)
	}
	if err := os.WriteFile(file, []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := runSandbox(t, "approve", "-repo", repo, "-data-dir", dataDir, "-sha256", hash); err == nil || !strings.Contains(err.Error(), file) {
		t.Errorf("approve over a corrupt file = %v, want it to name %s", err, file)
	}
}

// A draft is made only where the install would not write over tracked files.
func TestSandboxBlockDraftSkipsTrackedCachePaths(t *testing.T) {
	tracked := commitFiles(t, map[string]string{"package.json": "{}\n", "package-lock.json": "{}\n", "node_modules/x/index.js": "x\n"})
	if got := sandboxBlockDraft(tracked); got != "" {
		t.Errorf("draft with a tracked node_modules = %q, want none", got)
	}
}
