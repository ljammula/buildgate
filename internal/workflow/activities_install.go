package workflow

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"

	"buildgate/internal/languageprofile"
	"buildgate/internal/projectconfig"
	"buildgate/internal/run"
	"buildgate/internal/runner"
	"buildgate/internal/sandbox"
	wsisolation "buildgate/internal/workspace"
)

// A repository's approved sandbox install runs once per install image key, in
// an install sandbox, and every later repository-command sandbox of every run
// with that key starts from its result (sandbox.LaunchSpec.RestoreInstall):
//
//   - the install runs on a scratch worktree of the commit .factory.yml was
//     read from (ProjectConfigCommitSHA), never on the run's worktree, through
//     the same launch path as a gate's rerun on the base commit: the run's
//     image, limits, registry proxy and read-only `.factory/`, and no model
//     route and no Compose services;
//   - when the dependency inputs at the run's base commit differ from that
//     commit's (an earlier build changed a lockfile), the image is built from
//     the base commit with a run-local scope: no other run computes its key,
//     and it is removed when the run ends;
//   - only regular files, directories and links that stay inside a cache path
//     are captured, owned 0:0 with set-id and sticky bits dropped, and the
//     image adds them with no RUN step and no network;
//   - an image is reused only when its key label matches the key and the
//     local registry holds it, and is launched by digest.

// DefaultInstallCacheImages is how many install images of one repository are
// kept when sandbox_install_cache_images is not set.
const DefaultInstallCacheImages = 3

// installBaseDirName is the scratch worktree of the install, in the run's own
// directory; installContextDirName holds the image's build context.
const (
	installBaseDirName    = "install-base"
	installContextDirName = "install-image"
	installLockDirName    = "install-images"
)

// installKeyExtraNames are the files an install reads that the language
// profiles' manifests and version sources do not list: other lockfiles,
// package-manager settings, workspace files, version pins and the build
// tools' wrappers. A name with a slash matches that path, or pattern, at any
// depth. A repository adds a file its install
// reads (a postinstall script) with the block's `inputs:`.
var installKeyExtraNames = []string{
	"yarn.lock", "pnpm-lock.yaml", "requirements*.txt",
	".npmrc", ".yarnrc.yml", "pnpm-workspace.yaml", ".nvmrc", ".node-version",
	"go.work", "go.work.sum",
	"Pipfile", "Pipfile.lock", "setup.py", "setup.cfg", "pyproject.toml", ".python-version",
	"settings.gradle", "settings.gradle.kts", "gradle/wrapper/gradle-wrapper.properties",
	".tool-versions",
	".yarnrc", ".pnpmfile.cjs", "bun.lock", "bun.lockb", ".yarn/releases/*",
	"NuGet.config", "Directory.Packages.props", ".cargo/config.toml", "poetry.toml", ".bundle/config",
	".mvn/wrapper/maven-wrapper.properties", ".mvn/extensions.xml", ".mvn/maven.config",
}

// installKeyExcludedDirs are directories whose manifests are not key inputs:
// vendored and test copies, and an install's own output.
var installKeyExcludedDirs = []string{"vendor", "testdata", "node_modules"}

// InstallImages is the Worker's boundary to the image store install images
// live in (sandbox.DockerInstallImages).
type InstallImages interface {
	Lookup(ctx context.Context, docker, key string) (sandbox.InstallImageRef, error)
	Build(ctx context.Context, docker string, b sandbox.InstallImageBuild) (string, error)
	Evict(ctx context.Context, docker, repoKey string, keep int) (int, error)
	Remove(ctx context.Context, docker, key string) error
	// SweepRunLocal removes the run-local images whose run is over.
	SweepRunLocal(ctx context.Context, docker string, finished func(runID string) bool) (int, error)
}

// InstallImageResult is PrepareInstallImageActivity's result.
type InstallImageResult struct {
	// Image is the install image every later step launches from; empty
	// when the run has no sandbox block.
	Image  string            `json:"image,omitempty"`
	Record *run.InstallImage `json:"record,omitempty"`
	// Attempts holds the install launch, only when the image was built.
	Attempts []run.Attempt `json:"attempts,omitempty"`
}

// EvictRunInstallImageInput names a run-local install image to remove.
type EvictRunInstallImageInput struct {
	Key           string `json:"key"`
	SandboxDocker string `json:"sandbox_docker,omitempty"`
}

func installFailure(format string, args ...any) error {
	return temporal.NewNonRetryableApplicationError(fmt.Sprintf(format, args...), InstallImageFailureType, nil)
}

// PrepareInstallImageActivity returns the run's install image: the one an
// earlier run built with the same key, or one built now. It returns nothing
// for a run without a sandbox block.
func (a *Activities) PrepareInstallImageActivity(ctx context.Context, input RunWorkflowInput) (InstallImageResult, error) {
	if input.SandboxSHA256 == "" {
		return InstallImageResult{}, nil
	}
	approval, err := a.checkInstallApproval(input)
	if err != nil {
		return InstallImageResult{}, installFailure("%v", err)
	}
	plan, err := a.planInstallImage(ctx, input)
	if err != nil {
		return InstallImageResult{}, installFailure("install image: %v", err)
	}
	record := &run.InstallImage{Key: plan.key, Scope: plan.scope, Commit: plan.commit, SandboxSHA256: input.SandboxSHA256,
		ApprovedBy: approval.ApprovedBy, ApprovedAt: approval.ApprovedAt, Inputs: plan.inputs}
	if a.InstallImages == nil {
		return InstallImageResult{}, installFailure("install image: this Worker has no install image store")
	}
	docker := a.sandboxDockerFor(input)
	if found, err := a.lookupShared(ctx, docker, plan); err != nil || found.Image != "" {
		record.Image, record.CacheHit, record.BuiltByRunID = found.Image, true, found.BuiltBy
		return InstallImageResult{Image: found.Image, Record: record}, err
	}
	var out InstallImageResult
	started := time.Now()
	_, err = heartbeatWhileRunning(activityHeartbeatInterval, func() {
		recordStepHeartbeat(ctx, HeartbeatDetails{Stage: run.InstallAttemptKind, Elapsed: time.Since(started)})
	}, func() (runner.Result, error) {
		out, err = a.buildInstallImageLocked(ctx, input, plan, record, docker)
		return runner.Result{}, err
	})
	if err != nil {
		return InstallImageResult{}, withInstallAttempts(err, out.Attempts)
	}
	record.DurationMs = time.Since(started).Milliseconds()
	return out, nil
}

// withInstallAttempts carries a failed install's attempts on the Activity's
// error as its Details (AttemptsFromError), so the run's record has the
// install attempt and its log even though the Activity failed.
func withInstallAttempts(err error, attempts []run.Attempt) error {
	if len(attempts) == 0 {
		return err
	}
	var appErr *temporal.ApplicationError
	if !errors.As(err, &appErr) {
		return err
	}
	return temporal.NewNonRetryableApplicationError(appErr.Message(), appErr.Type(), nil, attempts)
}

// lookupShared returns the shared image with plan's key, when there is one.
// A run-local image is never looked up.
func (a *Activities) lookupShared(ctx context.Context, docker string, plan installPlan) (sandbox.InstallImageRef, error) {
	if plan.scope != run.InstallScopeShared {
		return sandbox.InstallImageRef{}, nil
	}
	found, err := a.InstallImages.Lookup(ctx, docker, plan.key)
	if err != nil {
		return sandbox.InstallImageRef{}, installFailure("install image: look up %s: %v", short(plan.key), err)
	}
	return found, nil
}

// buildInstallImageLocked builds the image under the key's lock, unless a
// run that held it first built it: then that image is the result.
func (a *Activities) buildInstallImageLocked(ctx context.Context, input RunWorkflowInput, plan installPlan, record *run.InstallImage, docker string) (InstallImageResult, error) {
	unlock, err := lockInstallKey(ctx, a.installLockDir(input), plan.key)
	if err != nil {
		return InstallImageResult{}, installFailure("install image: lock %s: %v", short(plan.key), err)
	}
	defer unlock()
	if found, err := a.lookupShared(ctx, docker, plan); err != nil || found.Image != "" {
		record.Image, record.CacheHit, record.BuiltByRunID = found.Image, true, found.BuiltBy
		return InstallImageResult{Image: found.Image, Record: record}, err
	}
	image, attempts, err := a.buildInstallImage(ctx, input, plan, docker)
	if err != nil {
		return InstallImageResult{Attempts: attempts}, err
	}
	record.Image, record.BuiltByRunID = image, a.runIDFor(input)
	if len(attempts) > 0 {
		record.LogPath = attempts[0].LogPath
	}
	if plan.scope == run.InstallScopeShared {
		keep := a.InstallCacheImages
		if keep <= 0 {
			keep = DefaultInstallCacheImages
		}
		if _, err := a.InstallImages.Evict(ctx, docker, input.InstallRepoKey, keep); err != nil {
			activity.GetLogger(ctx).Warn("older install images were not evicted", "error", err.Error())
		}
	}
	return InstallImageResult{Image: image, Record: record, Attempts: attempts}, nil
}

// checkInstallApproval checks, on this Worker, that the Install* fields of
// input are the block whose hash it carries, and that the operator approved
// that hash for the repository.
func (a *Activities) checkInstallApproval(input RunWorkflowInput) (projectconfig.Approval, error) {
	block := projectconfig.Sandbox{Version: projectconfig.SandboxVersion, Install: input.InstallCommands, CachePaths: input.InstallCachePaths, Inputs: input.InstallInputs, Registries: input.InstallRegistries}
	if block.SHA256() != input.SandboxSHA256 {
		return projectconfig.Approval{}, errors.New("sandbox: the run's install commands do not hash to its sandbox block's hash")
	}
	if a.SandboxApprovalsDataDir == "" || input.InstallRepoKey == "" {
		return projectconfig.Approval{}, errors.New("sandbox: this Worker cannot read the operator's sandbox approvals")
	}
	approval, ok := projectconfig.OpenApprovals(a.SandboxApprovalsDataDir).Approved(input.InstallRepoKey, input.SandboxSHA256)
	if !ok {
		return projectconfig.Approval{}, fmt.Errorf("sandbox: the sandbox block %s is not approved on this Worker (approvals are read from %s)", short(input.SandboxSHA256), projectconfig.OpenApprovals(a.SandboxApprovalsDataDir).Path(input.InstallRepoKey))
	}
	return approval, nil
}

// installPlan is what PrepareInstallImageActivity builds from and keys on.
type installPlan struct {
	key, scope, commit string
	inputs             []run.InstallInput
	routes             []sandbox.RegistryProxyRoute
}

// planInstallImage chooses the commit the install runs on and computes the
// key: the trusted commit with a shared scope when the base commit's inputs
// are the same as its own, else the base commit with a run-local scope.
func (a *Activities) planInstallImage(ctx context.Context, input RunWorkflowInput) (installPlan, error) {
	trusted := input.ProjectConfigCommitSHA
	if trusted == "" || input.BaseSHA == "" {
		return installPlan{}, errors.New("the run names no commit .factory.yml was read from, or no base commit")
	}
	if input.RegistryProxyPolicy == nil {
		return installPlan{}, errors.New(projectconfig.SandboxNeedsRegistryProxy)
	}
	routes, err := sandbox.NarrowRegistryRoutes(input.RegistryProxyPolicy.Routes, input.InstallRegistries)
	if err != nil {
		return installPlan{}, err
	}
	if len(routes) != len(input.RegistryProxyPolicy.Routes) {
		return installPlan{}, errors.New("the registry proxy serves registries the sandbox block does not list")
	}
	plan := installPlan{scope: run.InstallScopeShared, commit: trusted, routes: routes}
	if plan.inputs, err = installKeyInputs(ctx, input.WorkspacePath, trusted, input.InstallInputs); err != nil {
		return installPlan{}, err
	}
	if input.BaseSHA != trusted {
		atBase, err := installKeyInputs(ctx, input.WorkspacePath, input.BaseSHA, input.InstallInputs)
		if err != nil {
			return installPlan{}, err
		}
		if !slices.Equal(atBase, plan.inputs) {
			plan.scope, plan.commit, plan.inputs = run.InstallScopeRunPrefix+a.runIDFor(input), input.BaseSHA, atBase
		}
	}
	user, _ := sandbox.ResolveDefaultWorkerIdentity(a.sandboxUserFor(input), a.sandboxWorkerUID(), os.Getgid())
	effective, err := a.sandboxImageFor(input)
	if err != nil {
		return installPlan{}, err
	}
	plan.key, err = sandbox.InstallImageKey(sandbox.InstallKeyInputs{
		Scope: plan.scope, RepoKey: input.InstallRepoKey, SandboxSHA256: input.SandboxSHA256,
		ConfiguredImage: firstNonEmpty(input.ConfiguredSandboxImage, effective), EffectiveImage: effective,
		WorkerIdentity: user, Routes: routes, ProxyImage: input.RegistryProxyPolicy.Image, Inputs: plan.inputs,
	})
	return plan, err
}

func short(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

// installKeyInputs lists, sorted by path, the blobs at commit that decide what
// an install writes: every file whose name a language profile lists as a
// dependency manifest or a version source, those in installKeyExtraNames,
// and the block's inputs patterns, outside vendor, testdata and node_modules. It reads
// git's objects, never a worktree.
func installKeyInputs(ctx context.Context, repo, commit string, patterns []string) ([]run.InstallInput, error) {
	cmd := exec.CommandContext(ctx, "git", wsisolation.HardenedGitArgs(repo, "ls-tree", "-r", "-z", "--full-tree", commit)...)
	cmd.Env = wsisolation.HardenedGitEnv()
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("list the files of %s: %w", short(commit), err)
	}
	names := installKeyNames()
	var inputs []run.InstallInput
	for _, entry := range strings.Split(string(out), "\x00") {
		meta, p, ok := strings.Cut(entry, "\t")
		fields := strings.Fields(meta)
		if !ok || len(fields) != 3 || fields[1] != "blob" || !installKeyInput(p, names, patterns) {
			continue
		}
		inputs = append(inputs, run.InstallInput{Path: p, Blob: fields[2]})
	}
	slices.SortFunc(inputs, func(x, y run.InstallInput) int { return strings.Compare(x.Path, y.Path) })
	return inputs, nil
}

func installKeyNames() []string {
	names := slices.Clone(installKeyExtraNames)
	for _, p := range languageprofile.Catalog() {
		names = append(append(names, p.DependencyManifests...), p.VersionSources...)
	}
	slices.Sort(names)
	return slices.Compact(names)
}

func installKeyInput(p string, names, patterns []string) bool {
	for _, dir := range strings.Split(path.Dir(p), "/") {
		if slices.Contains(installKeyExcludedDirs, dir) {
			return false
		}
	}
	base := path.Base(p)
	for _, name := range names {
		if n := strings.Count(name, "/") + 1; n > 1 {
			// A name with a slash matches the same number of trailing
			// components of p, at any depth (a pattern allowed).
			parts := strings.Split(p, "/")
			if len(parts) >= n {
				if ok, _ := path.Match(name, strings.Join(parts[len(parts)-n:], "/")); ok {
					return true
				}
			}
			continue
		}
		if ok, _ := path.Match(name, base); ok {
			return true
		}
	}
	for _, pattern := range patterns {
		if ok, _ := path.Match(pattern, p); ok {
			return true
		}
	}
	return false
}

// installLockDir is where the per-key build locks are: the Worker's
// machine-wide InstallLockDir (shared by every profile's data dir, as the
// local registry is), else the run's data dir.
func (a *Activities) installLockDir(input RunWorkflowInput) string {
	if a.InstallLockDir != "" {
		return a.InstallLockDir
	}
	return filepath.Join(a.dataDirFor(input), installLockDirName)
}

// installLockRetry is how often a held build lock is tried again.
const installLockRetry = time.Second

// lockInstallKey takes an exclusive lock on key under dir, so two runs with
// the same key build it once. It tries without blocking and again every
// installLockRetry until it has the lock or ctx ends.
func lockInstallKey(ctx context.Context, dir, key string) (func(), error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, key+".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			f.Close()
			return nil, err
		}
		select {
		case <-ctx.Done():
			f.Close()
			return nil, ctx.Err()
		case <-time.After(installLockRetry):
		}
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}

// installScript checks that the image has the GNU coreutils the restore
// needs (sandbox.InstallRestorePrelude), then runs each install command (its
// arguments) in order and exits WorkerExitSetupFailed at the first that
// fails. It is a fixed literal: repository text reaches it only as arguments.
const installScript = `cp --version >/dev/null 2>&1 && chmod --version >/dev/null 2>&1 && sha256sum --version >/dev/null 2>&1 || { echo "` + installNoCoreutils + `" >&2; exit 95; }; ` +
	`for s; do sh -c "$s" || { echo "buildgate: install failed: $s" >&2; exit 95; }; done`

// installNoCoreutils is the install's refusal of an image without GNU
// coreutils.
const installNoCoreutils = "buildgate: the sandbox image lacks GNU coreutils (cp --preserve, chmod -R, sha256sum), which restoring an install image needs; use an image built on the one make install produces"

// installCommand is the argv of the install launch.
func installCommand(commands []string) []string {
	return append([]string{"sh", "-c", installScript, "buildgate-install"}, commands...)
}

// buildInstallImage runs the install on a scratch worktree of plan.commit,
// captures its cache paths and builds the image from them.
func (a *Activities) buildInstallImage(ctx context.Context, input RunWorkflowInput, plan installPlan, docker string) (string, []run.Attempt, error) {
	logDir := a.logDirFor(input)
	scratch, err := filepath.Abs(filepath.Join(logDir, installBaseDirName))
	if err != nil {
		return "", nil, installFailure("install image: %v", err)
	}
	if err := os.RemoveAll(scratch); err != nil {
		return "", nil, installFailure("install image: remove an earlier scratch worktree: %v", err)
	}
	defer func() {
		removeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), gateBaseRemoveLockWait)
		defer cancel()
		if err := removeGateBaseWorktree(removeCtx, input.WorkspacePath, scratch); err != nil {
			activity.GetLogger(ctx).Warn("the install's scratch worktree could not be removed", "path", scratch, "error", err.Error())
		}
	}()
	if err := wsisolation.AddScratchWorktree(ctx, input.WorkspacePath, scratch, plan.commit); err != nil {
		return "", nil, installFailure("install image: check out %s in a scratch worktree: %v", short(plan.commit), err)
	}
	if err := wsisolation.EnableWorkerGroupWrite(scratch, os.Getgid()); err != nil {
		return "", nil, installFailure("install image: make the scratch worktree writable for the worker: %v", err)
	}
	sums, err := installInputSums(scratch, plan.inputs)
	if err != nil {
		return "", nil, installFailure("install image: %v", err)
	}
	// Checked against the checked-out commit's ignore rules before the
	// install can write anything, a .gitignore included.
	if err := requireIgnored(ctx, scratch, input.InstallCachePaths); err != nil {
		return "", nil, installFailure("install image: %v", err)
	}
	attempts, err := a.runInstall(ctx, input, scratch, logDir)
	if err != nil {
		return "", attempts, err
	}
	image, err := a.captureAndBuild(ctx, input, plan, scratch, logDir, sums, docker)
	return image, attempts, err
}

// runInstall launches the install on scratch and returns its attempt.
func (a *Activities) runInstall(ctx context.Context, input RunWorkflowInput, scratch, logDir string) ([]run.Attempt, error) {
	registrySpec, err := a.registryProxySpecFor(input)
	if err != nil {
		return nil, installFailure("install image: %v", err)
	}
	onInstall := input
	onInstall.WorkspacePath, onInstall.InstallImage = scratch, ""
	command := installCommand(input.InstallCommands)
	logPath := activityLogPath(activityExecutionLogPath(ctx, logDir, run.InstallAttemptKind+".log"))
	var attempts []run.Attempt
	after := func(attempt int, res runner.Result, _ error) error {
		attempts = append(attempts, run.Attempt{Kind: run.InstallAttemptKind, Command: res.Command,
			FactoryDirSHA256: res.FactoryDirSHA256, FactoryDirCommit: res.FactoryDirCommit, FactoryDirError: res.FactoryDirError,
			StartedAt: res.StartedAt.Format(time.RFC3339), FinishedAt: res.FinishedAt.Format(time.RFC3339),
			ExitCode: res.ExitCode, LogPath: logPath(attempt), ImageDigest: res.ImageDigest, Resources: attemptResources(res)})
		return nil
	}
	before := a.leaseChecked(ctx, a.checkpointDirFor(input), nil)
	var res runner.Result
	if a.hasFakeRunner() {
		res, err = a.runWithRetriesFn()(ctx, scratch, logPath, 1, before, after, command[0], command[1:]...)
	} else {
		// nil relay and nil compose: the install calls no model and
		// starts no services; it reaches registries only through the proxy.
		res, err = a.runSandboxWithRetries(ctx, onInstall, logPath, 1, before, after, nil, registrySpec, nil, nil, nil, command[0], command[1:]...)
	}
	if err != nil {
		return attempts, installFailure("install image: the install could not be run: %v", err)
	}
	if res.ExitCode != 0 {
		return attempts, installFailure("install image: the sandbox install exited %d (log %s)", res.ExitCode, res.LogPath)
	}
	return attempts, nil
}

// installInputSums hashes each key input as the scratch worktree holds it,
// before the install runs. A link or a missing file is skipped.
func installInputSums(scratch string, inputs []run.InstallInput) ([]sandbox.InstallInputSum, error) {
	root, err := os.OpenRoot(scratch)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	var sums []sandbox.InstallInputSum
	for _, in := range inputs {
		info, err := root.Lstat(in.Path)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		f, err := root.Open(in.Path)
		if err != nil {
			return nil, err
		}
		h := sha256.New()
		_, err = io.Copy(h, f)
		f.Close()
		if err != nil {
			return nil, err
		}
		sums = append(sums, sandbox.InstallInputSum{Path: in.Path, SHA256: hex.EncodeToString(h.Sum(nil))})
	}
	return sums, nil
}

// requireIgnored refuses a cache path the commit's ignore rules (and the
// repository's info/exclude) do not ignore: what the install writes there
// must never reach a build's commit. It runs before the install.
func requireIgnored(ctx context.Context, scratch string, paths []string) error {
	for _, p := range paths {
		cmd := exec.CommandContext(ctx, "git", wsisolation.HardenedGitArgs(scratch, "check-ignore", "-q", "--", p)...)
		// check-ignore takes paths, not pathspecs, and refuses the literal
		// pathspec switch; a cache path is never a pattern (validated).
		cmd.Env = slices.DeleteFunc(wsisolation.HardenedGitEnv(), func(kv string) bool { return kv == "GIT_LITERAL_PATHSPECS=1" })
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			var exit *exec.ExitError
			if errors.As(err, &exit) && exit.ExitCode() == 1 {
				return fmt.Errorf("sandbox.cache_paths entry %q is not ignored by git at the commit the install ran on: add it to .gitignore", p)
			}
			return fmt.Errorf("git check-ignore %q: %v: %s", p, err, strings.TrimSpace(stderr.String()))
		}
	}
	return nil
}

// captureAndBuild captures the cache paths of scratch into the image's build
// context and builds and pushes the image.
func (a *Activities) captureAndBuild(ctx context.Context, input RunWorkflowInput, plan installPlan, scratch, logDir string, sums []sandbox.InstallInputSum, docker string) (string, error) {
	contextDir := filepath.Join(logDir, installContextDirName)
	if err := os.RemoveAll(contextDir); err != nil {
		return "", installFailure("install image: %v", err)
	}
	if err := os.MkdirAll(contextDir, 0o750); err != nil {
		return "", installFailure("install image: %v", err)
	}
	defer os.RemoveAll(contextDir)
	f, err := os.OpenFile(filepath.Join(contextDir, "tree.tar"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return "", installFailure("install image: %v", err)
	}
	_, err = sandbox.CaptureInstallTree(ctx, scratch, input.InstallCachePaths, f, sandbox.DefaultInstallCaptureLimits)
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return "", installFailure("install image: capture the install's cache paths: %v", err)
	}
	base, err := a.sandboxImageFor(input)
	if err != nil {
		return "", installFailure("install image: %v", err)
	}
	image, err := a.InstallImages.Build(ctx, docker, sandbox.InstallImageBuild{
		Key: plan.key, Base: base, RepoKey: input.InstallRepoKey, BuiltBy: a.runIDFor(input), Scope: plan.scope,
		ContextDir: contextDir, Paths: input.InstallCachePaths, Commands: input.InstallCommands, Inputs: sums,
	})
	if err != nil {
		return "", installFailure("install image: %v", err)
	}
	return image, nil
}

// EvictRunInstallImageActivity removes a run-local install image at the end
// of its run. A failure is logged: the image is never looked up again.
func (a *Activities) EvictRunInstallImageActivity(ctx context.Context, input EvictRunInstallImageInput) error {
	if a.InstallImages == nil || input.Key == "" {
		return nil
	}
	docker := input.SandboxDocker
	if docker == "" {
		docker = a.SandboxDocker
	}
	if err := a.InstallImages.Remove(ctx, docker, input.Key); err != nil {
		activity.GetLogger(ctx).Warn("the run-local install image was not removed", "key", input.Key, "error", err.Error())
	}
	return nil
}
