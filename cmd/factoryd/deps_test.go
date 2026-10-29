package main

import (
	"buildgate/internal/dockerbackend"
	"buildgate/internal/forge"
	"buildgate/internal/hostcontrol"
	"buildgate/internal/requestdriver"
	"buildgate/internal/sandbox"
	"buildgate/internal/toolchain"
	"buildgate/internal/workflow"
	"context"
	"crypto/x509"
	"errors"
	"io"
	"os/exec"
	"testing"
	"time"

	"go.temporal.io/sdk/client"
)

// fakeDocker is a dockerBoundary whose every method is a field, set by newTestDeps
// to the real method and replaced by a test that needs another answer.
type fakeDocker struct {
	backendFn          func(ctx context.Context) dockerbackend.Detection
	colimaBinaryFn     func() string
	dockerBinaryFn     func() string
	imageToolsFn       func(ctx context.Context, dockerBinary string, image string) (toolchain.Installed, string, error)
	initChecksFn       func(ctx context.Context, dockerBinary string, image string, dir string) []doctorCheck
	makeImageFn        func(repoRoot string, target string, vars ...string) (string, error)
	toolImageFn        func(ctx context.Context, dockerBinary string, base string, buildArgs []string) (string, error)
	requiredBinariesFn func(ctx context.Context, dockerBinary string, image string, user string, names []string) ([]string, error)
	workerChecksFn     func(ctx context.Context, in doctorInputs) []doctorCheck
	installImagesFn    func() workflow.InstallImages
}

func (f *fakeDocker) installImages() workflow.InstallImages { return f.installImagesFn() }

// refusingInstallImages is the test default install image store: every call
// fails, and nothing reaches docker.
type refusingInstallImages struct{}

func (refusingInstallImages) Lookup(context.Context, string, string) (sandbox.InstallImageRef, error) {
	return sandbox.InstallImageRef{}, errors.New("test docker: no install image is looked up")
}
func (refusingInstallImages) Build(context.Context, string, sandbox.InstallImageBuild) (string, error) {
	return "", errors.New("test docker: no install image is built")
}
func (refusingInstallImages) Evict(context.Context, string, string, int) (int, error) {
	return 0, errors.New("test docker: no install image is evicted")
}
func (refusingInstallImages) Remove(context.Context, string, string) error {
	return errors.New("test docker: no install image is removed")
}
func (refusingInstallImages) SweepRunLocal(context.Context, string, func(string) bool) (int, error) {
	return 0, errors.New("test docker: no install image is swept")
}

func (f *fakeDocker) colimaBinary() string { return f.colimaBinaryFn() }
func (f *fakeDocker) backend(ctx context.Context) dockerbackend.Detection {
	return f.backendFn(ctx)
}
func (f *fakeDocker) dockerBinary() string { return f.dockerBinaryFn() }
func (f *fakeDocker) imageToolchains(ctx context.Context, dockerBinary string, image string) (toolchain.Installed, string, error) {
	return f.imageToolsFn(ctx, dockerBinary, image)
}
func (f *fakeDocker) initChecks(ctx context.Context, dockerBinary string, image string, dir string) []doctorCheck {
	return f.initChecksFn(ctx, dockerBinary, image, dir)
}
func (f *fakeDocker) makeImage(repoRoot string, target string, vars ...string) (string, error) {
	return f.makeImageFn(repoRoot, target, vars...)
}
func (f *fakeDocker) toolchainImage(ctx context.Context, dockerBinary string, base string, buildArgs []string) (string, error) {
	return f.toolImageFn(ctx, dockerBinary, base, buildArgs)
}
func (f *fakeDocker) requiredBinaries(ctx context.Context, dockerBinary string, image string, user string, names []string) ([]string, error) {
	return f.requiredBinariesFn(ctx, dockerBinary, image, user, names)
}
func (f *fakeDocker) workerChecks(ctx context.Context, in doctorInputs) []doctorCheck {
	return f.workerChecksFn(ctx, in)
}

func fakeDockerOf(dp *deps) *fakeDocker { return dp.docker.(*fakeDocker) }

// fakeForge is a forgeBoundary whose every method is a field, set by newTestDeps
// to the real method and replaced by a test that needs another answer.
type fakeForge struct {
	branchTipFn                   func(ctx context.Context, workspaceDir string, branch string) (string, error)
	fetchIssueFn                  func(ctx context.Context, issueURL string) (string, string, int, error)
	gitToplevelFn                 func(dir string) (string, error)
	githubLoginFn                 func(ctx context.Context) string
	insideGitWorkTreeFn           func(dir string) bool
	listReviewCommentsFn          func(ctx context.Context, prURL string) ([]requestdriver.ReviewComment, error)
	markPullRequestReadyFn        func(ctx context.Context, prURL string) error
	pullRequestOpenerFn           func() forge.PullRequestOpener
	pushExistingBranchFn          func(ctx context.Context, workspaceDir string, sha string, branch string) error
	readReviewStateFn             func(ctx context.Context, prURL string, policy forge.AuthorPolicy) (forge.ReviewState, error)
	remoteBranchHeadSHAFn         func(ctx context.Context, workspaceDir string, branch string) (string, error)
	replyToReviewCommentFn        func(ctx context.Context, prURL string, commentID int64, body string) error
	retargetPullRequestBaseFn     func(ctx context.Context, prURL string, base string) error
	roundResultDescendsFromHeadFn func(dir string, ancestor string, descendant string) (bool, error)
	undoMarkPullRequestReadyFn    func(ctx context.Context, prURL string) error
	createRefFn                   func(dir string, ref string, sha string) error
	createBranchAtFn              func(dir string, branch string, sha string) error
	pruneRoundRefsFn              func(dir string, id string, keepPrefixes []string) error
	fetchBranchCommitFn           func(ctx context.Context, dir, branch, sha string) error
	revListRangeFn                func(dir, from, to string) ([]string, []string, error)
	patchIDFn                     func(dir, sha string) (string, error)
	replayCommitsFn               func(ctx context.Context, dir, worktreePath, branch, onto string, commits []string) ([]string, error)
	pushWithLeaseFn               func(ctx context.Context, dir, sha, branch, expected string) error
	moveBranchFn                  func(dir, branch, sha string) error
	restoreBranchFn               func(dir, branch, sha string) (bool, error)
	commentOnPullRequestFn        func(ctx context.Context, prURL, body string) error
	hostGitVersionFn              func(ctx context.Context) (string, error)
	userLoginFn                   func() (string, error)
}

func (f *fakeForge) branchTip(ctx context.Context, workspaceDir string, branch string) (string, error) {
	return f.branchTipFn(ctx, workspaceDir, branch)
}
func (f *fakeForge) fetchIssue(ctx context.Context, issueURL string) (string, string, int, error) {
	return f.fetchIssueFn(ctx, issueURL)
}
func (f *fakeForge) gitToplevel(dir string) (string, error) { return f.gitToplevelFn(dir) }
func (f *fakeForge) insideGitWorkTree(dir string) bool      { return f.insideGitWorkTreeFn(dir) }
func (f *fakeForge) githubLogin(ctx context.Context) string { return f.githubLoginFn(ctx) }
func (f *fakeForge) listReviewComments(ctx context.Context, prURL string) ([]requestdriver.ReviewComment, error) {
	return f.listReviewCommentsFn(ctx, prURL)
}
func (f *fakeForge) markPullRequestReady(ctx context.Context, prURL string) error {
	return f.markPullRequestReadyFn(ctx, prURL)
}
func (f *fakeForge) pullRequestOpener() forge.PullRequestOpener { return f.pullRequestOpenerFn() }
func (f *fakeForge) pushExistingBranch(ctx context.Context, workspaceDir string, sha string, branch string) error {
	return f.pushExistingBranchFn(ctx, workspaceDir, sha, branch)
}
func (f *fakeForge) readReviewState(ctx context.Context, prURL string, policy forge.AuthorPolicy) (forge.ReviewState, error) {
	return f.readReviewStateFn(ctx, prURL, policy)
}
func (f *fakeForge) remoteBranchHeadSHA(ctx context.Context, workspaceDir string, branch string) (string, error) {
	return f.remoteBranchHeadSHAFn(ctx, workspaceDir, branch)
}
func (f *fakeForge) replyToReviewComment(ctx context.Context, prURL string, commentID int64, body string) error {
	return f.replyToReviewCommentFn(ctx, prURL, commentID, body)
}
func (f *fakeForge) retargetPullRequestBase(ctx context.Context, prURL string, base string) error {
	return f.retargetPullRequestBaseFn(ctx, prURL, base)
}
func (f *fakeForge) roundResultDescendsFromHead(dir string, ancestor string, descendant string) (bool, error) {
	return f.roundResultDescendsFromHeadFn(dir, ancestor, descendant)
}
func (f *fakeForge) undoMarkPullRequestReady(ctx context.Context, prURL string) error {
	return f.undoMarkPullRequestReadyFn(ctx, prURL)
}
func (f *fakeForge) createRef(dir string, ref string, sha string) error {
	return f.createRefFn(dir, ref, sha)
}
func (f *fakeForge) pruneRoundRefs(dir string, id string, keepPrefixes []string) error {
	return f.pruneRoundRefsFn(dir, id, keepPrefixes)
}
func (f *fakeForge) createBranchAt(dir string, branch string, sha string) error {
	return f.createBranchAtFn(dir, branch, sha)
}
func (f *fakeForge) fetchBranchCommit(ctx context.Context, dir, branch, sha string) error {
	return f.fetchBranchCommitFn(ctx, dir, branch, sha)
}
func (f *fakeForge) revListRange(dir, from, to string) ([]string, []string, error) {
	return f.revListRangeFn(dir, from, to)
}
func (f *fakeForge) patchID(dir, sha string) (string, error) { return f.patchIDFn(dir, sha) }
func (f *fakeForge) replayCommits(ctx context.Context, dir, worktreePath, branch, onto string, commits []string) ([]string, error) {
	return f.replayCommitsFn(ctx, dir, worktreePath, branch, onto, commits)
}
func (f *fakeForge) pushWithLease(ctx context.Context, dir, sha, branch, expected string) error {
	return f.pushWithLeaseFn(ctx, dir, sha, branch, expected)
}
func (f *fakeForge) moveBranch(dir, branch, sha string) error {
	return f.moveBranchFn(dir, branch, sha)
}
func (f *fakeForge) restoreBranch(dir, branch, sha string) (bool, error) {
	return f.restoreBranchFn(dir, branch, sha)
}
func (f *fakeForge) commentOnPullRequest(ctx context.Context, prURL, body string) error {
	return f.commentOnPullRequestFn(ctx, prURL, body)
}
func (f *fakeForge) hostGitVersion(ctx context.Context) (string, error) {
	return f.hostGitVersionFn(ctx)
}
func (f *fakeForge) userLogin() (string, error) { return f.userLoginFn() }

func fakeForgeOf(dp *deps) *fakeForge { return dp.forge.(*fakeForge) }

// fakeHost is a hostBoundary whose every method is a field, set by newTestDeps
// to the real method and replaced by a test that needs another answer.
type fakeHost struct {
	baselineCheckFn      func(args []string) error
	blobAtCommitFn       func(ctx context.Context, repoDir string, commit string, path string) ([]byte, bool, error)
	browserCommandFn     func(target string) *exec.Cmd
	executableFn         func() (string, error)
	goCommandFn          func(ctx context.Context, dir string, env []string, args ...string) ([]byte, error)
	headCommitFn         func(ctx context.Context, repoDir string) (string, error)
	launchctlFn          func(args ...string) ([]byte, error)
	launchctlBinaryFn    func() string
	launchdServicePIDFn  func(domain string) (int, bool)
	portListenersFn      func(port string) ([]byte, error)
	pidAliveFn           func(pid int) bool
	pidLooksLikeWorkerFn func(pid int) bool
	processUIDFn         func(pid int) (int, bool)
	rootTreeAtCommitFn   func(ctx context.Context, repoDir string, commit string) ([]gitTreeEntry, error)
	runUpgradeCommandFn  func(c upgradeCmd) ([]byte, error)
	serveHealthzOKFn     func(addr string) bool
	tailscaleFn          func(ctx context.Context, args ...string) ([]byte, error)
	terminalNotifierFn   func() bool
	serveVerifiedOursFn  func(dataDir string, addr string) (int, bool)
	serviceManagerFn     func() hostcontrol.ServiceManager
	systemctlFn          func(args ...string) ([]byte, error)
	loginctlFn           func(args ...string) ([]byte, error)
	sleepFn              func(d time.Duration)
	spawnServeFn         func(w io.Writer, binaryPath string, configPath string, dataDir string, addr string) error
	spawnWorkerFn        func(w io.Writer, binaryPath string, configPath string, dataDir string, credentialEnv []string, pidPath string, temporalAddress string) error
	tlsRootFn            func(ctx context.Context, host string) (*x509.Certificate, error)
}

func (f *fakeHost) blobAtCommit(ctx context.Context, repoDir string, commit string, path string) ([]byte, bool, error) {
	return f.blobAtCommitFn(ctx, repoDir, commit, path)
}
func (f *fakeHost) rootTreeAtCommit(ctx context.Context, repoDir string, commit string) ([]gitTreeEntry, error) {
	return f.rootTreeAtCommitFn(ctx, repoDir, commit)
}
func (f *fakeHost) browserCommand(target string) *exec.Cmd { return f.browserCommandFn(target) }
func (f *fakeHost) executable() (string, error)            { return f.executableFn() }
func (f *fakeHost) baselineCheck(args []string) error      { return f.baselineCheckFn(args) }
func (f *fakeHost) goCommand(ctx context.Context, dir string, env []string, args ...string) ([]byte, error) {
	return f.goCommandFn(ctx, dir, env, args...)
}
func (f *fakeHost) headCommit(ctx context.Context, repoDir string) (string, error) {
	return f.headCommitFn(ctx, repoDir)
}
func (f *fakeHost) launchctl(args ...string) ([]byte, error)       { return f.launchctlFn(args...) }
func (f *fakeHost) launchctlBinary() string                        { return f.launchctlBinaryFn() }
func (f *fakeHost) launchdServicePID(domain string) (int, bool)    { return f.launchdServicePIDFn(domain) }
func (f *fakeHost) portListeners(port string) ([]byte, error)      { return f.portListenersFn(port) }
func (f *fakeHost) pidAlive(pid int) bool                          { return f.pidAliveFn(pid) }
func (f *fakeHost) pidLooksLikeWorker(pid int) bool                { return f.pidLooksLikeWorkerFn(pid) }
func (f *fakeHost) processUID(pid int) (int, bool)                 { return f.processUIDFn(pid) }
func (f *fakeHost) runUpgradeCommand(c upgradeCmd) ([]byte, error) { return f.runUpgradeCommandFn(c) }
func (f *fakeHost) serveHealthzOK(addr string) bool                { return f.serveHealthzOKFn(addr) }
func (f *fakeHost) terminalNotifierOnPath() bool                   { return f.terminalNotifierFn() }
func (f *fakeHost) serviceManager() hostcontrol.ServiceManager     { return f.serviceManagerFn() }
func (f *fakeHost) systemctl(args ...string) ([]byte, error)       { return f.systemctlFn(args...) }
func (f *fakeHost) loginctl(args ...string) ([]byte, error)        { return f.loginctlFn(args...) }
func (f *fakeHost) tailscale(ctx context.Context, args ...string) ([]byte, error) {
	return f.tailscaleFn(ctx, args...)
}
func (f *fakeHost) serveVerifiedOurs(dataDir string, addr string) (int, bool) {
	return f.serveVerifiedOursFn(dataDir, addr)
}
func (f *fakeHost) sleep(d time.Duration) { f.sleepFn(d) }
func (f *fakeHost) tlsRoot(ctx context.Context, host string) (*x509.Certificate, error) {
	return f.tlsRootFn(ctx, host)
}
func (f *fakeHost) spawnServe(w io.Writer, binaryPath string, configPath string, dataDir string, addr string) error {
	return f.spawnServeFn(w, binaryPath, configPath, dataDir, addr)
}
func (f *fakeHost) spawnWorker(w io.Writer, binaryPath string, configPath string, dataDir string, credentialEnv []string, pidPath string, temporalAddress string) error {
	return f.spawnWorkerFn(w, binaryPath, configPath, dataDir, credentialEnv, pidPath, temporalAddress)
}

func fakeHostOf(dp *deps) *fakeHost { return dp.host.(*fakeHost) }

// fakeSandboxRuntime is a sandboxRuntimeBoundary whose every method is a field,
// set by newTestDeps to the real method and replaced by a test that needs
// another answer.
type fakeSandboxRuntime struct {
	gatewayHealthyFn func(ctx context.Context) error
	meterHealthyFn   func(ctx context.Context) error
	portHoldersFn    func(ctx context.Context, addrs ...string) []string
	runtimeFn        func() sandbox.Runtime
	sandboxNamesFn   func(ctx context.Context) ([]string, error)
	sandboxLabelsFn  func(ctx context.Context, name string) (map[string]string, error)
	startStackFn     func(ctx context.Context, w io.Writer, meterImage string) error
}

func (f *fakeSandboxRuntime) gatewayHealthy(ctx context.Context) error {
	return f.gatewayHealthyFn(ctx)
}
func (f *fakeSandboxRuntime) meterHealthy(ctx context.Context) error { return f.meterHealthyFn(ctx) }
func (f *fakeSandboxRuntime) portHolders(ctx context.Context, addrs ...string) []string {
	return f.portHoldersFn(ctx, addrs...)
}
func (f *fakeSandboxRuntime) runtime() sandbox.Runtime { return f.runtimeFn() }
func (f *fakeSandboxRuntime) sandboxNames(ctx context.Context) ([]string, error) {
	return f.sandboxNamesFn(ctx)
}

func (f *fakeSandboxRuntime) sandboxLabels(ctx context.Context, name string) (map[string]string, error) {
	return f.sandboxLabelsFn(ctx, name)
}

func (f *fakeSandboxRuntime) startStack(ctx context.Context, w io.Writer, meterImage string) error {
	return f.startStackFn(ctx, w, meterImage)
}

func fakeSandboxOf(dp *deps) *fakeSandboxRuntime { return dp.sandbox.(*fakeSandboxRuntime) }

// fakeTemporal is a temporalBoundary whose every method is a field, set by newTestDeps
// to the real method and replaced by a test that needs another answer.
type fakeTemporal struct {
	dialTerminatorFn func(ctx context.Context, address string) (workflowTerminator, func(), error)
	ensureFn         func(ctx context.Context, w io.Writer) string
	healthyFn        func(ctx context.Context, addr string) error
	stillRunningFn   func(temporalClient client.Client, execution client.WorkflowRun) bool
	wakeRequestFn    func(ctx context.Context, dataDir string, requestID string) error
}

func (f *fakeTemporal) dialTerminator(ctx context.Context, address string) (workflowTerminator, func(), error) {
	return f.dialTerminatorFn(ctx, address)
}
func (f *fakeTemporal) ensure(ctx context.Context, w io.Writer) string { return f.ensureFn(ctx, w) }
func (f *fakeTemporal) healthy(ctx context.Context, addr string) error { return f.healthyFn(ctx, addr) }
func (f *fakeTemporal) stillRunning(temporalClient client.Client, execution client.WorkflowRun) bool {
	return f.stillRunningFn(temporalClient, execution)
}
func (f *fakeTemporal) wakeRequest(ctx context.Context, dataDir string, requestID string) error {
	return f.wakeRequestFn(ctx, dataDir, requestID)
}

func fakeTemporalOf(dp *deps) *fakeTemporal { return dp.temporal.(*fakeTemporal) }

// newTestDeps returns the deps of one test: every boundary is a fake whose
// methods call the real ones, except the ten below, where a real call
// would leave the test process. A test that needs another answer sets the
// field (fakeHostOf(dp).spawnWorkerFn = ...); a test of a real method calls
// it on realHost{dp: dp} and its siblings.
//
//	docker.initChecks    needs a live Docker daemon: all-pass
//	docker.docker.colimaBinary  the operator's Colima VM: a binary that does not exist
//	temporal.ensure      starts containers: no address
//	temporal.healthy     dials a server: unhealthy
//	host.spawnWorker     starts a process: refused
//	host.spawnServe      starts a process: refused
//	forge.userLogin      calls `gh api user`: refused
//	sandbox.gatewayHealthy, sandbox.meterHealthy  dial services: unhealthy
//	sandbox.sandboxNames lists a gateway's sandboxes: none
//	sandbox.runtime      the gateway runtime: none, in either build variant
//	sandbox.startStack   starts containers: refused
func newTestDeps(t testing.TB) *deps {
	dp := &deps{}
	realDocker := realDocker{dp: dp}
	dp.docker = &fakeDocker{
		colimaBinaryFn: realDocker.colimaBinary,
		dockerBinaryFn: realDocker.dockerBinary,
		imageToolsFn: func(context.Context, string, string) (toolchain.Installed, string, error) {
			return nil, "", errors.New("test docker: no image is run")
		},
		initChecksFn: realDocker.initChecks,
		makeImageFn:  realDocker.makeImage,
		toolImageFn: func(context.Context, string, string, []string) (string, error) {
			return "", errors.New("test docker: no image is built")
		},
		requiredBinariesFn: func(context.Context, string, string, string, []string) ([]string, error) {
			return nil, errors.New("test docker: no image is probed for required binaries")
		},
		workerChecksFn:  realDocker.workerChecks,
		installImagesFn: func() workflow.InstallImages { return refusingInstallImages{} },
	}
	realForge := realForge{dp: dp}
	dp.forge = &fakeForge{
		branchTipFn:   realForge.branchTip,
		fetchIssueFn:  realForge.fetchIssue,
		gitToplevelFn: realForge.gitToplevel,
		// No test runs gh against the developer's own login.
		githubLoginFn:                 func(context.Context) string { return "" },
		insideGitWorkTreeFn:           realForge.insideGitWorkTree,
		listReviewCommentsFn:          realForge.listReviewComments,
		markPullRequestReadyFn:        realForge.markPullRequestReady,
		pullRequestOpenerFn:           realForge.pullRequestOpener,
		pushExistingBranchFn:          realForge.pushExistingBranch,
		readReviewStateFn:             realForge.readReviewState,
		remoteBranchHeadSHAFn:         realForge.remoteBranchHeadSHA,
		replyToReviewCommentFn:        realForge.replyToReviewComment,
		retargetPullRequestBaseFn:     realForge.retargetPullRequestBase,
		roundResultDescendsFromHeadFn: realForge.roundResultDescendsFromHead,
		undoMarkPullRequestReadyFn:    realForge.undoMarkPullRequestReady,
		createRefFn:                   realForge.createRef,
		createBranchAtFn:              realForge.createBranchAt,
		pruneRoundRefsFn:              realForge.pruneRoundRefs,
		// A restack's git calls are refused: a test that restacks sets the
		// ones it needs (realForge's, on its own temp repositories).
		fetchBranchCommitFn: func(context.Context, string, string, string) error {
			return errors.New("test forge: no branch is fetched")
		},
		revListRangeFn: func(string, string, string) ([]string, []string, error) {
			return nil, nil, errors.New("test forge: no commits are listed")
		},
		patchIDFn: func(string, string) (string, error) { return "", errors.New("test forge: no patch id is computed") },
		replayCommitsFn: func(context.Context, string, string, string, string, []string) ([]string, error) {
			return nil, errors.New("test forge: nothing is replayed")
		},
		pushWithLeaseFn: func(context.Context, string, string, string, string) error {
			return errors.New("test forge: nothing is pushed with a lease")
		},
		moveBranchFn: func(string, string, string) error { return errors.New("test forge: no branch is moved") },
		restoreBranchFn: func(string, string, string) (bool, error) {
			return false, errors.New("test forge: no branch is restored")
		},
		commentOnPullRequestFn: func(context.Context, string, string) error {
			return errors.New("test forge: no pull request is commented on")
		},
		hostGitVersionFn: func(context.Context) (string, error) {
			return "", errors.New("test forge: no git version is read")
		},
		userLoginFn: realForge.userLogin,
	}
	realHost := realHost{dp: dp}
	dp.host = &fakeHost{
		// A local read of git objects in the directory the test names: it
		// reaches nothing outside the test process's own temp repositories.
		blobAtCommitFn:     realHost.blobAtCommit,
		rootTreeAtCommitFn: realHost.rootTreeAtCommit,
		browserCommandFn:   realHost.browserCommand,
		executableFn:       realHost.executable,
		// A passing check, decided in the test process: no run is started.
		baselineCheckFn: func([]string) error { return nil },
		goCommandFn: func(context.Context, string, []string, ...string) ([]byte, error) {
			return nil, errors.New("test host: no go command is run")
		},
		// A local read of the test's own temp repository, like blobAtCommit.
		headCommitFn:         realHost.headCommit,
		launchctlFn:          realHost.launchctl,
		launchctlBinaryFn:    realHost.launchctlBinary,
		launchdServicePIDFn:  realHost.launchdServicePID,
		portListenersFn:      realHost.portListeners,
		pidAliveFn:           realHost.pidAlive,
		pidLooksLikeWorkerFn: realHost.pidLooksLikeWorker,
		processUIDFn:         realHost.processUID,
		runUpgradeCommandFn:  realHost.runUpgradeCommand,
		serveHealthzOKFn:     realHost.serveHealthzOK,
		// The same answer on every machine the tests run on.
		terminalNotifierFn: func() bool { return true },
		// No test touches the developer's own tailnet.
		tailscaleFn: func(context.Context, ...string) ([]byte, error) {
			return nil, errors.New("test host: no tailscale")
		},
		serveVerifiedOursFn: realHost.serveVerifiedOurs,
		// No test reads or changes the developer's own services: no service
		// manager unless a test names one, and systemd is never asked.
		serviceManagerFn: func() hostcontrol.ServiceManager { return hostcontrol.NoServiceManager },
		systemctlFn: func(...string) ([]byte, error) {
			return nil, errors.New("test host: no systemctl")
		},
		loginctlFn: func(...string) ([]byte, error) {
			return nil, errors.New("test host: no loginctl")
		},
		// No test reaches a public host: the TLS-interception check is
		// skipped unless a test sets this.
		tlsRootFn: func(context.Context, string) (*x509.Certificate, error) {
			return nil, errors.New("test host: no public TLS probe")
		},
		sleepFn:       realHost.sleep,
		spawnServeFn:  realHost.spawnServe,
		spawnWorkerFn: realHost.spawnWorker,
	}
	realSandbox := realSandboxRuntime{dp: dp}
	dp.sandbox = &fakeSandboxRuntime{
		gatewayHealthyFn: realSandbox.gatewayHealthy,
		meterHealthyFn:   realSandbox.meterHealthy,
		portHoldersFn:    func(context.Context, ...string) []string { return nil },
		runtimeFn:        realSandbox.runtime,
		sandboxNamesFn:   realSandbox.sandboxNames,
		sandboxLabelsFn:  realSandbox.sandboxLabels,
		startStackFn:     realSandbox.startStack,
	}
	realTemporal := realTemporal{dp: dp}
	dp.temporal = &fakeTemporal{
		dialTerminatorFn: realTemporal.dialTerminator,
		ensureFn:         realTemporal.ensure,
		healthyFn:        realTemporal.healthy,
		stillRunningFn:   realTemporal.stillRunning,
		wakeRequestFn:    realTemporal.wakeRequest,
	}
	fakeDockerOf(dp).initChecksFn = func(context.Context, string, string, string) []doctorCheck { return nil }
	fakeDockerOf(dp).colimaBinaryFn = func() string { return "factoryd-test-no-such-colima" }
	// No test asks the machine's Docker which backend it is: one it could
	// not ask, which refuses nothing.
	fakeDockerOf(dp).backendFn = func(context.Context) dockerbackend.Detection {
		return dockerbackend.Detection{Err: errors.New("test docker: no backend is read")}
	}
	fakeTemporalOf(dp).ensureFn = func(context.Context, io.Writer) string { return "" }
	fakeTemporalOf(dp).healthyFn = func(context.Context, string) error { return errors.New("stubbed in tests") }
	fakeHostOf(dp).spawnWorkerFn = func(io.Writer, string, string, string, []string, string, string) error {
		return errors.New("tests start no real worker")
	}
	fakeHostOf(dp).spawnServeFn = func(io.Writer, string, string, string, string) error {
		return errors.New("tests start no real serve")
	}
	fakeForgeOf(dp).userLoginFn = func() (string, error) { return "", errors.New("stubbed in tests") }
	fakeSandboxOf(dp).gatewayHealthyFn = func(context.Context) error { return errors.New("stubbed in tests") }
	fakeSandboxOf(dp).meterHealthyFn = func(context.Context) error { return errors.New("stubbed in tests") }
	fakeSandboxOf(dp).sandboxNamesFn = func(context.Context) ([]string, error) { return nil, nil }
	fakeSandboxOf(dp).sandboxLabelsFn = func(context.Context, string) (map[string]string, error) {
		return nil, errors.New("stubbed in tests")
	}
	fakeSandboxOf(dp).runtimeFn = func() sandbox.Runtime { return nil }
	fakeSandboxOf(dp).startStackFn = func(context.Context, io.Writer, string) error {
		return errors.New("tests start no real OpenShell stack")
	}
	return dp
}

// fakeSandboxes is a sandbox.Runtime whose every method is a field; a method
// whose field is unset fails, so no test reaches a gateway by omission. A test
// installs one with fakeSandboxOf(dp).runtimeFn.
type fakeSandboxes struct {
	createFn         func(ctx context.Context, req sandbox.SandboxRequest) (sandbox.SandboxRef, error)
	waitFn           func(ctx context.Context, name string) (sandbox.SandboxExit, error)
	statusFn         func(ctx context.Context, name string) (sandbox.SandboxState, error)
	deleteFn         func(ctx context.Context, name string) error
	listByRunFn      func(ctx context.Context, dataDir, runID string) ([]string, error)
	pushCredentialFn func(ctx context.Context, cred sandbox.RouteCredential) error
	diskFreeFn       func(ctx context.Context) (sandbox.DiskSpace, error)
}

var errFakeSandboxesUnset = errors.New("fake sandbox runtime: method not set by this test")

func (f *fakeSandboxes) Create(ctx context.Context, req sandbox.SandboxRequest) (sandbox.SandboxRef, error) {
	if f.createFn == nil {
		return sandbox.SandboxRef{}, errFakeSandboxesUnset
	}
	return f.createFn(ctx, req)
}
func (f *fakeSandboxes) Wait(ctx context.Context, name string) (sandbox.SandboxExit, error) {
	if f.waitFn == nil {
		return sandbox.SandboxExit{}, errFakeSandboxesUnset
	}
	return f.waitFn(ctx, name)
}
func (f *fakeSandboxes) Status(ctx context.Context, name string) (sandbox.SandboxState, error) {
	if f.statusFn == nil {
		return sandbox.SandboxState{}, errFakeSandboxesUnset
	}
	return f.statusFn(ctx, name)
}
func (f *fakeSandboxes) Delete(ctx context.Context, name string) error {
	if f.deleteFn == nil {
		return errFakeSandboxesUnset
	}
	return f.deleteFn(ctx, name)
}
func (f *fakeSandboxes) ListByRun(ctx context.Context, dataDir, runID string) ([]string, error) {
	if f.listByRunFn == nil {
		return nil, errFakeSandboxesUnset
	}
	return f.listByRunFn(ctx, dataDir, runID)
}
func (f *fakeSandboxes) DockerDiskFree(ctx context.Context) (sandbox.DiskSpace, error) {
	if f.diskFreeFn == nil {
		return sandbox.DiskSpace{}, errFakeSandboxesUnset
	}
	return f.diskFreeFn(ctx)
}
func (f *fakeSandboxes) PushCredential(ctx context.Context, cred sandbox.RouteCredential) error {
	if f.pushCredentialFn == nil {
		return errFakeSandboxesUnset
	}
	return f.pushCredentialFn(ctx, cred)
}
