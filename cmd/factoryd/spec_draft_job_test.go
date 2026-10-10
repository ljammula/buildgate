package main

import (
	"buildgate/internal/request"
	"buildgate/internal/requestdriver"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"buildgate/internal/meter"
	"buildgate/internal/modelrole"
	"buildgate/internal/sessionconfig"
)

// TestReadSpecDraftOutputsNeverPopulatesModelFromEvidenceJSON proves
// SpecEvidence.Model is never trusted from anything draft_spec.py itself
// wrote: draftSpecEvidencePayload carries no "model" field at all, so even
// a compromised/malicious evidence.json claiming an operator-trusted model
// id cannot make it into the parsed evidence here -- factoryd's own
// caller (draftSpec) is the only thing that ever sets Model, from the
// relay's own RelayWorkerModelID, after this read.
func TestReadSpecDraftOutputsNeverPopulatesModelFromEvidenceJSON(t *testing.T) {
	scratchDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(scratchDir, "evidence.json"),
		[]byte(`{"schema_version":1,"agent_exit_code":0,"duration_s":1.5,"agents_md_used":true,"model":"attacker-claimed-model"}`),
		0o600); err != nil {
		t.Fatalf("write evidence.json: %v", err)
	}
	if err := os.WriteFile(filepath.Join(scratchDir, "spec.md"), []byte("# Spec\n"), 0o600); err != nil {
		t.Fatalf("write spec.md: %v", err)
	}

	_, evidence, err := readSpecDraftOutputs(scratchDir)
	if err != nil {
		t.Fatalf("readSpecDraftOutputs: %v", err)
	}
	if evidence == nil {
		t.Fatalf("evidence = nil, want a parsed SpecEvidence")
	}
	if evidence.Model != "" {
		t.Fatalf("evidence.Model = %q, want empty -- readSpecDraftOutputs must never trust a script-supplied model id", evidence.Model)
	}
}

// TestDraftSpecArgsShape covers the exact argv draftSpecArgs builds --
// mirroring TestBuildAppArgsIncludesVerifyCommand's own style for
// buildAppArgs.
func TestDraftSpecArgsShape(t *testing.T) {
	args := draftSpecArgs("/harness/draft_spec.py", "/workspace", "/inputs/run/request.md", "/workspace/out/spec.md", "/workspace/out/evidence.json", draftInputFiles{}, 7, "", "pi")
	want := []string{
		"/harness/draft_spec.py",
		"--workspace", "/workspace",
		"--request", "/inputs/run/request.md",
		"--out", "/workspace/out/spec.md",
		"--evidence", "/workspace/out/evidence.json",
		"--timeout-minutes", "7",
		"--harness", "pi",
	}
	if len(args) != len(want) {
		t.Fatalf("draftSpecArgs = %v, want %v", args, want)
	}
	for i := range want {
		if args[i] != want[i] {
			t.Errorf("draftSpecArgs[%d] = %q, want %q", i, args[i], want[i])
		}
	}
}

// TestDraftSpecArgsIncludesFeedbackFileWhenGiven covers the case where a
// non-empty feedbackPath appends --feedback-file, and an empty one omits
// it entirely (TestDraftSpecArgsShape above already covers that case).
func TestDraftSpecArgsIncludesFeedbackFileWhenGiven(t *testing.T) {
	args := draftSpecArgs("/harness/draft_spec.py", "/workspace", "/inputs/run/request.md", "/workspace/out/spec.md", "/workspace/out/evidence.json", draftInputFiles{feedback: "/workspace/.factory-spec-draft/feedback.md"}, 7, "", "pi")
	found := false
	for i, a := range args {
		if a == "--feedback-file" && i+1 < len(args) && args[i+1] == "/workspace/.factory-spec-draft/feedback.md" {
			found = true
		}
	}
	if !found {
		t.Errorf("draftSpecArgs = %v, want --feedback-file <path>", args)
	}
}

// TestDraftSpecArgsIncludesThinkingWhenGiven covers the planning role's own
// --thinking argument: appended when non-empty, omitted (byte-identical to
// TestDraftSpecArgsShape) when "".
func TestDraftSpecArgsIncludesThinkingWhenGiven(t *testing.T) {
	args := draftSpecArgs("/harness/draft_spec.py", "/workspace", "/inputs/run/request.md", "/workspace/out/spec.md", "/workspace/out/evidence.json", draftInputFiles{}, 7, "xhigh", "pi")
	found := false
	for i, a := range args {
		if a == "--thinking" && i+1 < len(args) && args[i+1] == "xhigh" {
			found = true
		}
	}
	if !found {
		t.Errorf("draftSpecArgs(thinking=xhigh) = %v, want --thinking xhigh", args)
	}
}

// TestResolveSpecDraftRelaySpecAppliesRoleOverride covers the planning
// role's alias replacing the session's own worker model id/API/base path/
// extra JSON in the resolved relay spec.
func TestResolveSpecDraftRelaySpecAppliesRoleOverride(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "test-key")
	settings := sessionconfig.DefaultSettings()
	basePath := "/v1/a"
	settings.Routes = map[string]sessionconfig.Route{
		"anthropic": {
			CredentialMode:    meter.CredentialModeStatic,
			Upstream:          "https://api.anthropic.com",
			AllowedPathPrefix: "/v1/messages",
			CredentialHeader:  meter.CredentialHeaderXAPIKey,
			CredentialEnv:     "ANTHROPIC_API_KEY",
			WorkerBasePath:    &basePath,
		},
	}
	settings.Models = map[string]sessionconfig.Model{
		"model-a": {ID: "model-a", Routes: []string{"anthropic"}, API: meter.RequestFormatOpenAICompletions},
	}
	settings.Roles = &sessionconfig.Roles{
		Execution: &sessionconfig.RoleConfig{Model: "model-a"},
		Planning:  &sessionconfig.RoleConfig{Model: "model-a", Thinking: "max"},
	}
	cfg := requestdriver.WorkerConfig{
		Settings: settings,
	}
	sel, err := modelrole.SelectRoute(settings, modelrole.RolePlanning, "", "", "", func(_ string, r sessionconfig.Route) error {
		_, err := resolveRouteCredentials(r)
		return err
	})
	if err != nil {
		t.Fatalf("SelectRoute: %v", err)
	}
	override := requestJobRoleOverride{Thinking: sel.Thinking, RouteSelection: &sel}
	spec, err := resolveSpecDraftRelaySpec(cfg, "run-1", t.TempDir(), override)
	if err != nil {
		t.Fatalf("resolveSpecDraftRelaySpec: %v", err)
	}
	if spec.WorkerModelID != "model-a" {
		t.Errorf("spec.WorkerModelID = %q, want the role's model-a", spec.WorkerModelID)
	}
	if spec.WorkerModelAPI != meter.RequestFormatOpenAICompletions || spec.WorkerBasePath != "/v1/a" {
		t.Errorf("spec.WorkerModelAPI/WorkerBasePath = %q/%q, want the model's own", spec.WorkerModelAPI, spec.WorkerBasePath)
	}
}

// TestResolveSpecDraftRelaySpecAllowNoCredentialReachesTheSpec covers a role
// alias's own allow_no_credential reaching the resolved relay spec even
// though neither ANTHROPIC_API_KEY nor -relay-allow-no-credential is set:
// found via review -- a hand-rolled override that only touched the relay
// policy after the "requires a credential" check ran left this alias
// refused before it ever got the chance to apply.
func TestResolveSpecDraftRelaySpecAllowNoCredentialReachesTheSpec(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	settings := sessionconfig.DefaultSettings()
	settings.Routes = map[string]sessionconfig.Route{
		"anthropic": {
			CredentialMode:    meter.CredentialModeStatic,
			Upstream:          "https://api.anthropic.com",
			AllowedPathPrefix: "/v1/messages",
			CredentialHeader:  meter.CredentialHeaderXAPIKey,
			AllowNoCredential: true,
		},
	}
	settings.Models = map[string]sessionconfig.Model{
		"model-a": {ID: "model-a", Routes: []string{"anthropic"}},
	}
	settings.Roles = &sessionconfig.Roles{Execution: &sessionconfig.RoleConfig{Model: "model-a"}}
	cfg := requestdriver.WorkerConfig{Settings: settings}
	sel, err := modelrole.SelectRoute(settings, modelrole.RoleExecution, "", "", "", func(_ string, r sessionconfig.Route) error {
		_, err := resolveRouteCredentials(r)
		return err
	})
	if err != nil {
		t.Fatalf("SelectRoute: %v", err)
	}
	override := requestJobRoleOverride{Thinking: sel.Thinking, RouteSelection: &sel}
	spec, err := resolveSpecDraftRelaySpec(cfg, "run-1", t.TempDir(), override)
	if err != nil {
		t.Fatalf("resolveSpecDraftRelaySpec: %v", err)
	}
	if !spec.AllowUnauthenticatedUpstream {
		t.Error("spec.AllowUnauthenticatedUpstream = false, want true from the route's allow_no_credential")
	}
}

// TestRequestJobContainerDeadlineWorkerTimeoutLeavesSlackOverTheScriptBudget
// reads the REAL computation runSandboxWithRetries applies to a deadline
// built from requestJobContainerDeadline (sandbox_exec.go's own `timeout`
// local: time until the deadline, minus sandboxAttemptMargin) and proves
// that worker timeout always covers the script's own configured
// --timeout-minutes budget plus requestJobScriptTimeoutSlack -- the
// property that lets a drafting script's own Pi timeout fire, and its
// "agent timed out" evidence path run, before Docker kills the container
// (runSpecDraftJobIn's/runPlanTicketsJobIn's own sandboxCtx construction).
func TestRequestJobContainerDeadlineWorkerTimeoutLeavesSlackOverTheScriptBudget(t *testing.T) {
	for _, jobTimeoutMinutes := range []int{1, 5, 10, 30, 90} {
		scriptBudget := time.Duration(jobTimeoutMinutes) * time.Minute
		deadline := requestJobContainerDeadline(scriptBudget, false, 0)
		// Mirrors sandbox_exec.go's own `timeout := time.Until(deadline) -
		// margin`: with no elapsed time between building the deadline (just
		// now) and this computation, time.Until(deadline) == deadline.
		workerTimeout := deadline - sandboxAttemptMargin(false, 0)
		if workerTimeout < scriptBudget+requestJobScriptTimeoutSlack {
			t.Errorf("jobTimeoutMinutes=%d: worker timeout %v < script budget %v + slack %v",
				jobTimeoutMinutes, workerTimeout, scriptBudget, requestJobScriptTimeoutSlack)
		}
	}
}

// TestResolveSpecDraftRelaySpecSucceedsWithACredential covers the
// ordinary static-credential-mode path succeeding once ANTHROPIC_API_KEY
// is set, producing a spec whose own Validate has already been checked.
// The "requires a relay image at all"/"requires a credential or opt-out"
// guards this test used to cover directly now live in
// resolveRequestJobRole (see request_job_role_test.go's own
// TestResolveRequestJobRoleRoutesModeMissingRelayImageClearError) and in
// internal/sandbox's RoutePolicy.Validate respectively -- roleOverride
// always carries a resolved RouteSelection by the time
// resolveRequestJobRelaySpec/resolveSpecDraftRelaySpec sees it.
func TestResolveSpecDraftRelaySpecSucceedsWithACredential(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "test-key")
	settings := sessionconfig.DefaultSettings()
	settings.Routes = map[string]sessionconfig.Route{
		"anthropic": {
			CredentialMode:    meter.CredentialModeStatic,
			Upstream:          "https://api.anthropic.com",
			AllowedPathPrefix: "/v1/messages",
			CredentialHeader:  meter.CredentialHeaderXAPIKey,
			CredentialEnv:     "ANTHROPIC_API_KEY",
		},
	}
	settings.Models = map[string]sessionconfig.Model{
		"model-a": {ID: "model-a", Routes: []string{"anthropic"}},
	}
	settings.Roles = &sessionconfig.Roles{Execution: &sessionconfig.RoleConfig{Model: "model-a"}}
	cfg := requestdriver.WorkerConfig{Settings: settings}
	sel, err := modelrole.SelectRoute(settings, modelrole.RoleExecution, "", "", "", func(_ string, r sessionconfig.Route) error {
		_, err := resolveRouteCredentials(r)
		return err
	})
	if err != nil {
		t.Fatalf("SelectRoute: %v", err)
	}
	spec, err := resolveSpecDraftRelaySpec(cfg, "run-1", t.TempDir(), requestJobRoleOverride{Thinking: sel.Thinking, RouteSelection: &sel})
	if err != nil {
		t.Fatalf("resolveSpecDraftRelaySpec: %v", err)
	}
	if spec == nil {
		t.Fatal("resolveSpecDraftRelaySpec returned a nil spec with a nil error")
	}
}

// TestRequestJobSandboxIdentityDefaultsToHostUser: request-level jobs run
// against the operator's checkout, not an isolated worktree, so an unset
// sandbox_user resolves to the host's own uid:gid (never "0:gid", which
// LaunchSpec.Validate refuses -- found live on the first spec draft).
func TestRequestJobSandboxIdentityDefaultsToHostUser(t *testing.T) {
	user, _ := requestJobSandboxIdentity(requestdriver.WorkerConfig{})
	if want := fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()); user != want {
		t.Fatalf("user = %q, want %q", user, want)
	}
	cfg := requestdriver.WorkerConfig{}
	cfg.Settings.SandboxUser = "1234:5678"
	if user, _ := requestJobSandboxIdentity(cfg); user != "1234:5678" {
		t.Fatalf("explicit sandbox_user not honored: %q", user)
	}
}

// TestEnsureRequestJobScratchDirIsGroupWritable covers an adversarial-review
// finding (2026-09-24): with `-sandbox-user <dedicated-UID>:
// <factoryd GID>`, the scratch dir spec_draft_job.go/plan_tickets_job.go
// pre-create on the host must be writable by the worker's *group*, since
// the worker's UID differs from the host process (factoryd) that creates
// it -- 0o750 (the mode these files used before this fix) leaves the
// group with only read+execute, not write, so the worker could never
// create spec.md/evidence.json/session/ inside it. Chown to a gid this
// test process already belongs to (os.Getgid()), the same constraint a
// non-root factoryd is under in production (see
// internal/sandbox.EnsureScratchDir's own doc comment).
func TestEnsureRequestJobScratchDirIsGroupWritable(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "scratch")
	user := fmt.Sprintf("999999:%d", os.Getgid())
	if err := ensureRequestJobScratchDir(dir, user); err != nil {
		t.Fatalf("ensureRequestJobScratchDir: %v", err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o770 {
		t.Errorf("scratch dir mode = %o, want %o (group read+write+execute)", got, 0o770)
	}
}

// TestWriteRequestJobFeedbackFileIsGroupReadable is
// TestEnsureRequestJobScratchDirIsGroupWritable's own sibling for the
// feedback file itself: 0o640, not the 0o600 these callers used before,
// since a different-UID worker only ever reads this file back
// (read_feedback_ex in draft_spec.py/plan_tickets.py) through its shared
// group, never writes it.
func TestWriteRequestJobFeedbackFileIsGroupReadable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "feedback.md")
	user := fmt.Sprintf("999999:%d", os.Getgid())
	if err := writeRequestJobFeedbackFile(path, []byte("## Spec rejected\n\ntighten scope\n"), user); err != nil {
		t.Fatalf("writeRequestJobFeedbackFile: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o640 {
		t.Errorf("feedback file mode = %o, want %o (group read)", got, 0o640)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "## Spec rejected\n\ntighten scope\n" {
		t.Errorf("feedback file content = %q, want it written verbatim", content)
	}
}

// TestSandboxUserGIDParsesOnlyUIDColonGID covers sandboxUserGID's own
// parsing rules: a bare uid (no ":"), or anything non-numeric, is ok=false
// so ensureRequestJobScratchDir/writeRequestJobFeedbackFile skip the
// chown rather than erroring on a malformed -sandbox-user value some
// other validation should have already refused.
func TestSandboxUserGIDParsesOnlyUIDColonGID(t *testing.T) {
	cases := []struct {
		user    string
		wantGID int
		wantOK  bool
	}{
		{"1000:2000", 2000, true},
		{"1000", 0, false},
		{"", 0, false},
		{"1000:abc", 0, false},
	}
	for _, tc := range cases {
		gid, ok := sandboxUserGID(tc.user)
		if gid != tc.wantGID || ok != tc.wantOK {
			t.Errorf("sandboxUserGID(%q) = (%d, %v), want (%d, %v)", tc.user, gid, ok, tc.wantGID, tc.wantOK)
		}
	}
}

// TestWithDraftingWorktreeUsesCommittedHEADAndLeavesCheckoutAlone: a job
// sees the committed AGENTS.md, not the checkout's dirty copy, and the
// worktree is gone afterwards.
func TestWithDraftingWorktreeUsesCommittedHEADAndLeavesCheckoutAlone(t *testing.T) {
	repo := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	run("init", "-q")
	if err := os.WriteFile(filepath.Join(repo, "AGENTS.md"), []byte("committed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "AGENTS.md")
	run("commit", "-q", "-m", "init")
	if err := os.WriteFile(filepath.Join(repo, "AGENTS.md"), []byte("dirty\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	dataDir := t.TempDir()
	r := &request.Request{ID: "req-1", Workspace: repo}
	if err := os.MkdirAll(request.Dir(dataDir, r.ID), 0o750); err != nil {
		t.Fatal(err)
	}
	var seen string
	var jobWorkspace string
	err := withDraftingWorktree(dataDir, r, func(job *request.Request) error {
		jobWorkspace = job.Workspace
		b, err := os.ReadFile(filepath.Join(job.Workspace, "AGENTS.md"))
		seen = string(b)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if seen != "committed\n" {
		t.Fatalf("job saw %q, want the committed content", seen)
	}
	if jobWorkspace == repo {
		t.Fatal("job ran against the operator's checkout, not a worktree")
	}
	if _, err := os.Stat(jobWorkspace); !os.IsNotExist(err) {
		t.Fatalf("worktree %s still exists after the job", jobWorkspace)
	}
	if b, _ := os.ReadFile(filepath.Join(repo, "AGENTS.md")); string(b) != "dirty\n" {
		t.Fatalf("checkout was modified: %q", b)
	}
	if r.Workspace != repo {
		t.Fatal("caller's request was mutated")
	}
}

// TestWithDraftingWorktreeWorksWithARelativeDataDir covers the exact
// regression found live 2026-09-17, immediately after
// TestSandboxDataDirForReturnsAnAbsolutePath's own fix landed: a relative
// dataDir (the documented -data-dir default, "data") built a worktree
// path that `git -C repoRoot worktree add` resolved against repoRoot (the
// target repo), while every later consumer of job.Workspace resolved
// that same relative string against this process's own cwd instead --
// "resolve sandbox workspace mount: lstat data/requests/<id>/worktree:
// no such file or directory" on every default-configured spec-draft or
// plan-tickets attempt, since the worktree really was created, just not
// where the relative path resolves to from a different cwd.
func TestWithDraftingWorktreeWorksWithARelativeDataDir(t *testing.T) {
	repo := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	run("init", "-q")
	if err := os.WriteFile(filepath.Join(repo, "AGENTS.md"), []byte("committed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "AGENTS.md")
	run("commit", "-q", "-m", "init")

	cwd := t.TempDir()
	t.Chdir(cwd)
	dataDir := "data" // relative, same as -data-dir's documented default
	r := &request.Request{ID: "req-1", Workspace: repo}
	absDataDir, err := filepath.Abs(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(request.Dir(absDataDir, r.ID), 0o750); err != nil {
		t.Fatal(err)
	}
	var seen string
	err = withDraftingWorktree(dataDir, r, func(job *request.Request) error {
		b, readErr := os.ReadFile(filepath.Join(job.Workspace, "AGENTS.md"))
		seen = string(b)
		return readErr
	})
	if err != nil {
		t.Fatalf("withDraftingWorktree(relative dataDir): %v", err)
	}
	if seen != "committed\n" {
		t.Fatalf("job saw %q, want the committed content", seen)
	}
}

// TestResolveRequestJobRelaySpecRoutesModeCarriesCABundlePath proves the
// routes: mode branch of resolveRequestJobRelaySpec (roleOverride.
// RouteSelection non-nil) carries cfg.egressCABundle through to the
// resolved spec, exactly like the legacy branch just above always has
// (found via review round 1: an earlier version of the routes: mode
// branch built its RouteSpec via policy.Spec(...) alone and never set
// CABundlePath at all, silently dropping -egress-ca-bundle for every
// routes: mode spec/plan/oracle-drafting job).
func TestResolveRequestJobRelaySpecRoutesModeCarriesCABundlePath(t *testing.T) {
	t.Setenv("ROUTES_MODE_CA_BUNDLE_TEST_KEY", "sk-test")
	settings := sessionconfig.DefaultSettings()
	settings.Routes = map[string]sessionconfig.Route{
		"litellm": {CredentialMode: "static", Upstream: "https://litellm.example.invalid", CredentialEnv: "ROUTES_MODE_CA_BUNDLE_TEST_KEY"},
	}
	settings.Models = map[string]sessionconfig.Model{
		"luna": {ID: "gpt-5.6-luna", Routes: []string{"litellm"}},
	}
	settings.Roles = &sessionconfig.Roles{Execution: &sessionconfig.RoleConfig{Model: "luna"}}

	sel, err := modelrole.SelectRoute(settings, modelrole.RoleExecution, "", "", "", func(_ string, r sessionconfig.Route) error {
		_, err := resolveRouteCredentials(r)
		return err
	})
	if err != nil {
		t.Fatalf("SelectRoute: %v", err)
	}

	caBundlePath := generateTestEgressCABundle(t)
	cfg := requestdriver.WorkerConfig{Settings: settings, EgressCABundle: caBundlePath}
	spec, err := resolveRequestJobRelaySpec(cfg, "spec drafting", "run-1", t.TempDir(), requestJobRoleOverride{RouteSelection: &sel})
	if err != nil {
		t.Fatalf("resolveRequestJobRelaySpec: %v", err)
	}
	if spec.CABundlePath != caBundlePath {
		t.Errorf("spec.CABundlePath = %q, want the configured egress CA bundle path %q", spec.CABundlePath, caBundlePath)
	}
}

// generateTestEgressCABundle writes a minimal, valid self-signed CA
// certificate PEM to a temp file and returns its path -- mirrors
// internal/sandbox's own generateTestCABundle test helper, needed here
// because RouteSpec.Validate (called by resolveRequestJobRelaySpec)
// rejects an empty or unparsable CABundlePath.
func generateTestEgressCABundle(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate test key: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "test-egress-ca"},
		NotBefore:    time.Now(),
		NotAfter:     time.Now().Add(time.Hour),
		IsCA:         true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create test certificate: %v", err)
	}
	path := filepath.Join(t.TempDir(), "egress-ca.pem")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatalf("write test CA bundle: %v", err)
	}
	return path
}

// A staged design guide part reaches draft_spec.py as --design-guide-file;
// TestDraftSpecArgsShape covers its absence.
func TestDraftSpecArgsIncludesDesignGuideFileWhenGiven(t *testing.T) {
	args := draftSpecArgs("/harness/draft_spec.py", "/workspace", "/inputs/run/request.md", "/workspace/out/spec.md", "/workspace/out/evidence.json", draftInputFiles{designGuide: "/host/scratch/design-guide.md"}.inContainer("/workspace/.factory-spec-draft"), 7, "", "pi")
	if !slices.Contains(args, "--design-guide-file") || args[slices.Index(args, "--design-guide-file")+1] != "/workspace/.factory-spec-draft/design-guide.md" {
		t.Errorf("draftSpecArgs = %v, want --design-guide-file /workspace/.factory-spec-draft/design-guide.md", args)
	}
}
