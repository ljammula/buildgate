package main

import (
	"buildgate/internal/dockerbackend"
	"buildgate/internal/forge"
	"buildgate/internal/hostcontrol"
	"buildgate/internal/meter"
	"buildgate/internal/notify"
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
// A dependency is passed in like this, never reached through a package
// variable: no function assigns one (TestNoFunctionAssignsAPackageVariable,
// internal/claims, whose baseline lists the timeouts and limits tests still
// shrink).
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
	// releaseWarning logs the never-allowing release policy warning once
	// for the process.
	releaseWarning *releasePolicyWarning
	// ghLogins keeps the factory's own gh login for the process.
	ghLogins *forge.SelfLogins
	// webhooks are the notification webhook files of the session config the
	// process loaded (useWebhookFiles).
	webhooks *webhookSettings
}

// newDeps returns the real implementations.
func newDeps() *deps {
	dp := &deps{backends: &backendMemo{}, releaseWarning: &releasePolicyWarning{}, ghLogins: &forge.SelfLogins{}, webhooks: &webhookSettings{}}
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
	// contextFacts lists the docker CLI's contexts and asks each colima
	// context whether a daemon answers: why doctor finds Docker unreachable.
	contextFacts(ctx context.Context, dockerBinary string) dockerContextFacts
	dockerBinary() string
	// imageToolchains runs image once and reads the versions of its
	// toolchains (imageToolchainLines) and its distribution codename.
	imageToolchains(ctx context.Context, dockerBinary string, image string) (toolchain.Installed, string, error)
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
	// memTotal is Docker's total memory in bytes (the VM's, on a Mac), read
	// within timeout; the rule for running a build's review and challenger
	// together (reviewAndChallengerTogether) and doctor's memory row use it.
	memTotal(ctx context.Context, dockerBinary string, timeout time.Duration) (int64, error)
	// runningContainers names the running containers of the docker context
	// dockerContext ("" is the current one): `docker --context <c> ps`, a
	// read that changes nothing.
	runningContainers(ctx context.Context, dockerBinary string, dockerContext string) ([]string, error)
	// buildgateImages lists the images that carry a buildgate kind label,
	// with the containers that use each and the bytes only it holds;
	// removeImage runs `docker image rm` (no force) on refs; imagesBytes is
	// the total size of every image. `factoryd prune` uses the three.
	buildgateImages(ctx context.Context, dockerBinary string) ([]dockerImage, error)
	removeImage(ctx context.Context, dockerBinary string, refs []string) error
	imagesBytes(ctx context.Context, dockerBinary string) (int64, error)
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
	changedFileStats(ctx context.Context, dir string, base string, head string) ([]requestdriver.FileStat, error)
	undoMarkPullRequestReady(ctx context.Context, prURL string) error
	// createRef points a full ref at a commit in the local .git of dir, and
	// createBranchAt creates a local branch there: the round refs and the
	// branch of `retry -from <N>`. Neither touches a remote.
	createRef(dir string, ref string, sha string) error
	createBranchAt(dir string, branch string, sha string) error
	pruneRoundRefs(dir string, id string, keepPrefixes []string) error
	// removeWorktreeOnly removes a worktree registered in repoDir and keeps
	// its branch: `factoryd prune`'s removal of a finished run's worktree.
	removeWorktreeOnly(repoDir string, worktreePath string) error
	// deleteRoundRefs deletes a request's refs/buildgate/rounds/<id>/ refs
	// and nothing else (never a branch): `factoryd prune`.
	deleteRoundRefs(repoDir string, requestID string) error
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
	// chdir moves this process to dir: a worker or serve leaves the
	// directory it was started in (enterStableWorkDir).
	chdir(dir string) error
	// browserRedirectDir is where a browser can read the redirect file
	// openInBrowser writes ("" is the OS temp directory), or why no browser
	// can be opened on this machine.
	browserRedirectDir() (string, error)
	executable() (string, error)
	// lookupEnv and setenv read and set this process's environment, which
	// every child process inherits: the docker_context pin
	// (pinDockerContext) is the one caller of setenv.
	lookupEnv(key string) (string, bool)
	setenv(key, value string) error
	unsetenv(key string) error
	// hostResources is this machine's physical memory in bytes and its CPU
	// count, and vmStat is macOS's vm_stat output: the Docker VM's size and
	// the Mac's memory headroom (internal/hostcontrol docker_vm.go).
	hostResources() (memory uint64, cpus int, err error)
	vmStat() ([]byte, error)
	// notifyWebhooks reads the notification webhook URLs from files
	// (readWebhookFiles).
	notifyWebhooks(files webhookFiles) notify.Webhooks
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
	// listGitHubCopilotModels exchanges githubToken for a Copilot API token
	// and lists the models it is entitled to, over HTTP.
	listGitHubCopilotModels(ctx context.Context, caBundlePath, githubToken, upstream string) ([]meter.CopilotModel, error)
	launchdServicePID(domain string) (int, bool)
	portListeners(port string) ([]byte, error)
	pidAlive(pid int) bool
	// tcpListening reports whether a TCP listener answers on addr.
	tcpListening(addr string) bool
	pidLooksLikeWorker(pid int) bool
	processUID(pid int) (int, bool)
	runUpgradeCommand(c upgradeCmd) ([]byte, error)
	serveHealthzOK(addr string) bool
	serveVerifiedOurs(dataDir string, addr string) (int, bool)
	// serviceManager is what supervises service install's services on this
	// OS (launchd, systemd or none).
	serviceManager() hostcontrol.ServiceManager
	// systemctl and loginctl run those commands and return their combined
	// output: the systemd user units service install manages, and whether
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
	// reissueClientBundle moves a client bundle from another CA aside and
	// issues the gateway's (hostcontrol.ReissueOpenShellCerts): doctor -fix.
	reissueClientBundle(ctx context.Context, w io.Writer) error
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
