package main

import (
	"buildgate/internal/dockerbackend"
	"buildgate/internal/forge"
	"buildgate/internal/hostcontrol"
	"buildgate/internal/openshell"
	"buildgate/internal/requestdriver"
	"buildgate/internal/sandbox"
	"buildgate/internal/toolchain"
	"buildgate/internal/workflow"
	"context"
	"crypto/x509"
	"io"
	"os/exec"
	"time"

	"go.temporal.io/sdk/client"
)

// deps are the external boundaries a command reaches: each field is the
// interface its code calls. main builds the real ones (newDeps) and passes
// them down; a function that needs one takes dp as its first parameter, and
// a long-lived struct (ticketRun, daemonRun, serveRun, requestActivities)
// holds it. A test builds its own with newTestDeps (deps_test.go).
//
// Four package-level function variables remain, none of them one of these
// boundaries. One is here:
//
//	listGitHubCopilotModelsFn  doctor's model listing over HTTP
//
// and three in internal/requestdriver:
//
//	PrReviewCorrectiveRunner, ReviewCorrectiveRunner
//	                           test overrides of the build entry point
//	                           (Deps.RunTicket); nil in production
//	ContinueAfterPRApproval    the request driver's own next step, replaced
//	                           by the tests of the step before it
type deps struct {
	docker   dockerBoundary
	forge    forgeBoundary
	host     hostBoundary
	sandbox  sandboxRuntimeBoundary
	temporal temporalBoundary
	// gateway is the runtime over this machine's OpenShell gateway, which
	// sandbox reaches; it connects on first use.
	gateway *openshell.Lazy
	// backends keeps docker.backend's detections for the process.
	backends *backendMemo
}

// newDeps returns the real implementations.
func newDeps() *deps {
	dp := &deps{backends: &backendMemo{}}
	dp.docker = realDocker{dp: dp}
	dp.forge = realForge{dp: dp}
	dp.host = realHost{dp: dp}
	dp.sandbox = realSandboxRuntime{dp: dp}
	dp.temporal = realTemporal{dp: dp}
	dp.gateway = newGatewayRuntime(dp)
	return dp
}

type dockerBoundary interface {
	// backend names the Docker that the docker CLI dockerBinary reaches
	// ("" is the default CLI): colima, Docker Engine on this Linux host, or
	// one buildgate refuses. Read once per process and binary.
	backend(ctx context.Context, dockerBinary string) dockerbackend.Detection
	colimaBinary() string
	dockerBinary() string
	// imageToolchains runs image once and reads the versions of its
	// toolchains (imageToolchainLines) and its distribution codename.
	imageToolchains(ctx context.Context, dockerBinary string, image string) (toolchain.Installed, string, error)
	initChecks(ctx context.Context, dockerBinary string, image string, dir string) []doctorCheck
	makeImage(repoRoot string, target string, vars ...string) (string, error)
	// toolchainImage builds, or finds already built, the image that is base
	// with the toolchains buildArgs name, and returns it by digest.
	toolchainImage(ctx context.Context, dockerBinary string, base string, buildArgs []string) (string, error)
	// installImages is the store of repositories' install images: what a
	// run's worker finds, builds and removes them through, and what
	// `factoryd sandbox evict` removes.
	installImages() workflow.InstallImages
	// requiredBinaries checks that the image has the requested executables on
	// PATH as the worker identity.
	requiredBinaries(ctx context.Context, dockerBinary string, image string, user string, names []string) ([]string, error)
	workerChecks(ctx context.Context, in doctorInputs) []doctorCheck
}

// realDocker is the real dockerBoundary; its methods are beside the code that uses them.
type realDocker struct{ dp *deps }

type forgeBoundary interface {
	branchTip(ctx context.Context, workspaceDir string, branch string) (string, error)
	fetchIssue(ctx context.Context, issueURL string) (string, string, int, error)
	gitToplevel(dir string) (string, error)
	githubLogin(ctx context.Context) string
	insideGitWorkTree(dir string) bool
	listReviewComments(ctx context.Context, prURL string) ([]requestdriver.ReviewComment, error)
	markPullRequestReady(ctx context.Context, prURL string) error
	pullRequestOpener() forge.PullRequestOpener
	pushExistingBranch(ctx context.Context, workspaceDir string, sha string, branch string) error
	readReviewState(ctx context.Context, prURL string, policy forge.AuthorPolicy) (forge.ReviewState, error)
	remoteBranchHeadSHA(ctx context.Context, workspaceDir string, branch string) (string, error)
	replyToReviewComment(ctx context.Context, prURL string, commentID int64, body string) error
	retargetPullRequestBase(ctx context.Context, prURL string, base string) error
	roundResultDescendsFromHead(dir string, ancestor string, descendant string) (bool, error)
	undoMarkPullRequestReady(ctx context.Context, prURL string) error
	// createRef points a full ref at a commit in the local .git of dir, and
	// createBranchAt creates a local branch there: the round refs and the
	// branch of `retry -from <N>`. Neither touches a remote.
	createRef(dir string, ref string, sha string) error
	createBranchAt(dir string, branch string, sha string) error
	pruneRoundRefs(dir string, id string, keepPrefixes []string) error
	// The git calls of a restack: fetchBranchCommit fetches origin's tip of
	// a branch when it is not in dir, pushWithLease is the restack's leased
	// forced push, and revListRange, patchID, replayCommits and moveBranch
	// read and change only dir's local repository.
	fetchBranchCommit(ctx context.Context, dir string, branch string, sha string) error
	revListRange(dir string, from string, to string) ([]string, []string, error)
	patchID(dir string, sha string) (string, error)
	replayCommits(ctx context.Context, dir string, worktreePath string, branch string, onto string, commits []string) ([]string, error)
	pushWithLease(ctx context.Context, dir string, sha string, branch string, expected string) error
	moveBranch(dir string, branch string, sha string) error
	// The git calls of a pr_order carve: object writes on dir's local
	// repository, with no ref or worktree change.
	changedPaths(dir string, base string, result string) ([]string, error)
	carveChain(ctx context.Context, dir string, base string, result string, parts [][]string) error
	carveCommit(ctx context.Context, dir string, parent string, result string, paths []string, message string) (string, error)
	// restoreBranch points a local branch ahead of sha back at it, and
	// commentOnPullRequest posts a comment on a pull request: an advice
	// round's, when its result is not pushed and when it is.
	restoreBranch(dir string, branch string, sha string) (bool, error)
	commentOnPullRequest(ctx context.Context, prURL string, body string) error
	// hostGitVersion is the host's `git version` output, which a restack
	// checks against workspace.GitFloor.
	hostGitVersion(ctx context.Context) (string, error)
	// readJobLog is the log of a GitHub Actions job of the pull request's
	// repository, which a CI round gives its build the tail of.
	readJobLog(ctx context.Context, prURL string, jobID int64) (string, error)
	userLogin() (string, error)
}

// realForge is the real forgeBoundary; its methods are beside the code that uses them.
type realForge struct{ dp *deps }

type hostBoundary interface {
	// baselineCheck runs `factoryd -baseline-check` with args in this
	// process (runMain: its own context, owning SIGINT/SIGTERM, so a caller's
	// probe deadline cannot cut the run short): a real run through the
	// sandbox that stops after its baseline.
	baselineCheck(args []string) error
	// blobAtCommit reads path as of commit in repoDir from git objects,
	// never from a worktree: (nil, false, nil) when absent, errNotRegularBlob
	// for a symlink or submodule, errBlobTooLarge over 1 MiB.
	blobAtCommit(ctx context.Context, repoDir string, commit string, path string) ([]byte, bool, error)
	browserCommand(target string) *exec.Cmd
	// browserRedirectDir is where a browser can read the redirect file
	// openInBrowser writes ("" is the OS temp directory), or why no browser
	// can be opened on this machine.
	browserRedirectDir() (string, error)
	executable() (string, error)
	// rootTreeAtCommit lists the entries of commit's root tree in repoDir
	// (name, mode, type, object) from git objects, never from a worktree.
	rootTreeAtCommit(ctx context.Context, repoDir string, commit string) ([]gitTreeEntry, error)
	// headCommit is the full id of the commit HEAD names in repoDir.
	headCommit(ctx context.Context, repoDir string) (string, error)
	// goCommand runs the operator's go command in dir with env added to the
	// process environment, and returns its standard output.
	goCommand(ctx context.Context, dir string, env []string, args ...string) ([]byte, error)
	launchctl(args ...string) ([]byte, error)
	launchctlBinary() string
	launchdServicePID(domain string) (int, bool)
	portListeners(port string) ([]byte, error)
	pidAlive(pid int) bool
	pidLooksLikeWorker(pid int) bool
	processUID(pid int) (int, bool)
	runUpgradeCommand(c upgradeCmd) ([]byte, error)
	serveHealthzOK(addr string) bool
	serveVerifiedOurs(dataDir string, addr string) (int, bool)
	// serviceManager is what supervises install-service's services on this
	// OS (launchd, systemd or none).
	serviceManager() hostcontrol.ServiceManager
	// systemctl and loginctl run those commands and return their combined
	// output: the systemd user units install-service manages, and whether
	// the user's units start at boot (linger).
	systemctl(args ...string) ([]byte, error)
	loginctl(args ...string) ([]byte, error)
	sleep(d time.Duration)
	spawnServe(w io.Writer, binaryPath string, configPath string, dataDir string, addr string) error
	// terminalNotifierOnPath reports whether terminal-notifier, which gives
	// a desktop notification its click, is on PATH.
	terminalNotifierOnPath() bool
	// tailscale runs the operator's tailscale CLI and returns its output.
	tailscale(ctx context.Context, args ...string) ([]byte, error)
	spawnWorker(w io.Writer, binaryPath string, configPath string, dataDir string, credentialEnv []string, pidPath string, temporalAddress string) error
	tlsRoot(ctx context.Context, host string) (*x509.Certificate, error)
}

// realHost is the real hostBoundary; its methods are beside the code that uses them.
type realHost struct{ dp *deps }

// sandboxRuntimeBoundary is the OpenShell gateway and the meter, the services
// sandboxes run through.
type sandboxRuntimeBoundary interface {
	gatewayHealthy(ctx context.Context) error
	meterHealthy(ctx context.Context) error
	// portHolders names each running container outside the stack that
	// publishes one of addrs' ports; nil when Docker cannot be asked.
	portHolders(ctx context.Context, addrs ...string) []string
	// runtime is what workers are launched through: the gateway runtime,
	// nil only in the integration tests' own build.
	runtime() sandbox.Runtime
	// startStack starts the meter, then the gateway, and waits for both.
	startStack(ctx context.Context, w io.Writer, meterImage string) error
	sandboxNames(ctx context.Context) ([]string, error)
	// sandboxLabels are the labels the gateway stores with a sandbox.
	sandboxLabels(ctx context.Context, name string) (map[string]string, error)
}

type temporalBoundary interface {
	dialTerminator(ctx context.Context, address string) (workflowTerminator, func(), error)
	ensure(ctx context.Context, w io.Writer) string
	healthy(ctx context.Context, addr string) error
	stillRunning(temporalClient client.Client, execution client.WorkflowRun) bool
	wakeRequest(ctx context.Context, dataDir string, requestID string) error
}

// realTemporal is the real temporalBoundary; its methods are beside the code that uses them.
type realTemporal struct{ dp *deps }
