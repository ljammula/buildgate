package main

import (
	"buildgate/internal/forge"
	"buildgate/internal/openshell"
	"buildgate/internal/requestdriver"
	"buildgate/internal/sandbox"
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
// Seven package-level function variables remain, none of them one of these
// boundaries. Four are here:
//
//	launchOracleDraft, resolveOracleRelaySpec, oracleDraftClock
//	                           the oracle-draft job's sandbox launch, relay
//	                           spec and clock, stubbed by its own tests
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
}

// newDeps returns the real implementations.
func newDeps() *deps {
	dp := &deps{}
	dp.docker = realDocker{dp: dp}
	dp.forge = realForge{dp: dp}
	dp.host = realHost{dp: dp}
	dp.sandbox = realSandboxRuntime{dp: dp}
	dp.temporal = realTemporal{dp: dp}
	dp.gateway = newGatewayRuntime(dp)
	return dp
}

type dockerBoundary interface {
	colimaBinary() string
	dockerBinary() string
	initChecks(ctx context.Context, dockerBinary string, image string, dir string) []doctorCheck
	makeImage(repoRoot string, target string, vars ...string) (string, error)
	workerChecks(ctx context.Context, in doctorInputs) []doctorCheck
}

// realDocker is the real dockerBoundary; its methods are beside the code that uses them.
type realDocker struct{ dp *deps }

type forgeBoundary interface {
	branchTip(ctx context.Context, workspaceDir string, branch string) (string, error)
	fetchIssue(ctx context.Context, issueURL string) (string, string, int, error)
	gitToplevel(dir string) (string, error)
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
	userLogin() (string, error)
}

// realForge is the real forgeBoundary; its methods are beside the code that uses them.
type realForge struct{ dp *deps }

type hostBoundary interface {
	browserCommand(target string) *exec.Cmd
	executable() (string, error)
	launchctl(args ...string) ([]byte, error)
	launchctlBinary() string
	launchdServicePID(domain string) (int, bool)
	lsof(port string) ([]byte, error)
	pidAlive(pid int) bool
	pidLooksLikeWorker(pid int) bool
	processUID(pid int) (int, bool)
	runUpgradeCommand(c upgradeCmd) ([]byte, error)
	serveHealthzOK(addr string) bool
	serveVerifiedOurs(dataDir string, addr string) (int, bool)
	shippedRootsPEM(ctx context.Context) ([]byte, error)
	sleep(d time.Duration)
	spawnServe(w io.Writer, binaryPath string, configPath string, dataDir string, addr string) error
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
