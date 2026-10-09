package main

import (
	"buildgate/internal/forge"
	"buildgate/internal/requestdriver"
	"buildgate/internal/sandbox"
	"buildgate/internal/toolchain"
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
	colimaBinaryFn func() string
	dockerBinaryFn func() string
	imageToolsFn   func(ctx context.Context, dockerBinary string, image string) (toolchain.Installed, string, error)
	initChecksFn   func(ctx context.Context, dockerBinary string, image string, dir string) []doctorCheck
	makeImageFn    func(repoRoot string, target string, vars ...string) (string, error)
	toolImageFn    func(ctx context.Context, dockerBinary string, base string, buildArgs []string) (string, error)
	workerChecksFn func(ctx context.Context, in doctorInputs) []doctorCheck
}

func (f *fakeDocker) colimaBinary() string { return f.colimaBinaryFn() }
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
func (f *fakeForge) userLogin() (string, error) { return f.userLoginFn() }

func fakeForgeOf(dp *deps) *fakeForge { return dp.forge.(*fakeForge) }

// fakeHost is a hostBoundary whose every method is a field, set by newTestDeps
// to the real method and replaced by a test that needs another answer.
type fakeHost struct {
	blobAtCommitFn       func(ctx context.Context, repoDir string, commit string, path string) ([]byte, bool, error)
	browserCommandFn     func(target string) *exec.Cmd
	executableFn         func() (string, error)
	goCommandFn          func(ctx context.Context, dir string, env []string, args ...string) ([]byte, error)
	launchctlFn          func(args ...string) ([]byte, error)
	launchctlBinaryFn    func() string
	launchdServicePIDFn  func(domain string) (int, bool)
	lsofFn               func(port string) ([]byte, error)
	pidAliveFn           func(pid int) bool
	pidLooksLikeWorkerFn func(pid int) bool
	processUIDFn         func(pid int) (int, bool)
	rootTreeAtCommitFn   func(ctx context.Context, repoDir string, commit string) ([]gitTreeEntry, error)
	runUpgradeCommandFn  func(c upgradeCmd) ([]byte, error)
	serveHealthzOKFn     func(addr string) bool
	serveVerifiedOursFn  func(dataDir string, addr string) (int, bool)
	shippedRootsPEMFn    func(ctx context.Context) ([]byte, error)
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
func (f *fakeHost) goCommand(ctx context.Context, dir string, env []string, args ...string) ([]byte, error) {
	return f.goCommandFn(ctx, dir, env, args...)
}
func (f *fakeHost) launchctl(args ...string) ([]byte, error)       { return f.launchctlFn(args...) }
func (f *fakeHost) launchctlBinary() string                        { return f.launchctlBinaryFn() }
func (f *fakeHost) launchdServicePID(domain string) (int, bool)    { return f.launchdServicePIDFn(domain) }
func (f *fakeHost) lsof(port string) ([]byte, error)               { return f.lsofFn(port) }
func (f *fakeHost) pidAlive(pid int) bool                          { return f.pidAliveFn(pid) }
func (f *fakeHost) pidLooksLikeWorker(pid int) bool                { return f.pidLooksLikeWorkerFn(pid) }
func (f *fakeHost) processUID(pid int) (int, bool)                 { return f.processUIDFn(pid) }
func (f *fakeHost) runUpgradeCommand(c upgradeCmd) ([]byte, error) { return f.runUpgradeCommandFn(c) }
func (f *fakeHost) serveHealthzOK(addr string) bool                { return f.serveHealthzOKFn(addr) }
func (f *fakeHost) serveVerifiedOurs(dataDir string, addr string) (int, bool) {
	return f.serveVerifiedOursFn(dataDir, addr)
}
func (f *fakeHost) shippedRootsPEM(ctx context.Context) ([]byte, error) {
	return f.shippedRootsPEMFn(ctx)
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
		workerChecksFn: realDocker.workerChecks,
	}
	realForge := realForge{dp: dp}
	dp.forge = &fakeForge{
		branchTipFn:                   realForge.branchTip,
		fetchIssueFn:                  realForge.fetchIssue,
		gitToplevelFn:                 realForge.gitToplevel,
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
		userLoginFn:                   realForge.userLogin,
	}
	realHost := realHost{dp: dp}
	dp.host = &fakeHost{
		// A local read of git objects in the directory the test names: it
		// reaches nothing outside the test process's own temp repositories.
		blobAtCommitFn:     realHost.blobAtCommit,
		rootTreeAtCommitFn: realHost.rootTreeAtCommit,
		browserCommandFn:   realHost.browserCommand,
		executableFn:       realHost.executable,
		goCommandFn: func(context.Context, string, []string, ...string) ([]byte, error) {
			return nil, errors.New("test host: no go command is run")
		},
		launchctlFn:          realHost.launchctl,
		launchctlBinaryFn:    realHost.launchctlBinary,
		launchdServicePIDFn:  realHost.launchdServicePID,
		lsofFn:               realHost.lsof,
		pidAliveFn:           realHost.pidAlive,
		pidLooksLikeWorkerFn: realHost.pidLooksLikeWorker,
		processUIDFn:         realHost.processUID,
		runUpgradeCommandFn:  realHost.runUpgradeCommand,
		serveHealthzOKFn:     realHost.serveHealthzOK,
		serveVerifiedOursFn:  realHost.serveVerifiedOurs,
		// No test reaches a public host or the machine's keychain: the
		// TLS-interception check is skipped unless a test sets these.
		shippedRootsPEMFn: func(context.Context) ([]byte, error) {
			return nil, errors.New("test host: no shipped roots")
		},
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
func (f *fakeSandboxes) PushCredential(ctx context.Context, cred sandbox.RouteCredential) error {
	if f.pushCredentialFn == nil {
		return errFakeSandboxesUnset
	}
	return f.pushCredentialFn(ctx, cred)
}
