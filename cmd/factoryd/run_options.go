package main

import (
	"path/filepath"
	"time"

	"buildgate/internal/projectconfig"
	"buildgate/internal/release"
	"buildgate/internal/run"
	"buildgate/internal/sandbox"
	"buildgate/internal/sessionconfig"
	"buildgate/internal/workflow"
)

// runOptions is everything one run's dispatch to Temporal needs, resolved from
// the flags and session config. runMainWithReady builds it once; runViaTemporal
// and runViaRepositoryOwner take it whole. A new run input is a field here,
// set in runMainWithReady and read in workflowInput (what the workflow gets)
// or activities (what this process's own worker falls back to).
type runOptions struct {
	// DataDir is made canonical by the function that dispatches the run,
	// before any path is derived from it.
	DataDir          string
	ID               string
	Ticket           string
	WorkspacePath    string
	SpecSnapshotPath string
	BaseSHA          string
	// Repository is -repository: set, the run goes through the repository
	// owner workflow.
	Repository      string
	TemporalAddress string

	BuildAppInterpreter string
	BuildAppScript      string
	Harness             string
	ReviewHarness       string
	CodeReviewPolicy    string
	VerifyCommand       string
	FastCheckCommand    string
	FullSuiteCommand    string
	// BaselineCheck stops the run after its baseline (-baseline-check).
	BaselineCheck   bool
	GateCommands    map[string]string
	SetupCommands   []string
	AutofixCommands []string
	// ProjectConfigCommitSHA is the commit .factory.yml was read from; see
	// the RunWorkflowInput field of the same name.
	ProjectConfigCommitSHA string
	Skills                 roleSkillSet
	// Sandbox is the committed .factory.yml sandbox block the operator
	// approved, nil when the repository has none; SandboxRepoKey names the
	// repository its approval and install image belong to.
	Sandbox        *projectconfig.Sandbox
	SandboxRepoKey string
	// ConfiguredSandboxImage is the operator's sandbox_image before a
	// derived toolchain image replaced SandboxImage.
	ConfiguredSandboxImage string
	// ApprovalDataDir is the operator's data dir, whose sandbox approvals
	// this process's worker checks again; "" means DataDir.
	ApprovalDataDir string
	// InstallCacheImages is sandbox_install_cache_images.
	InstallCacheImages int

	MaxRounds           int
	TimeoutMinutes      int
	BuildAppMaxAttempts int
	VerifyMaxAttempts   int
	// OverallTimeout is -timeout; zero means overallTimeout's default.
	OverallTimeout time.Duration

	SandboxImage         string
	SandboxDocker        string
	SandboxUser          string
	SandboxWorkerUID     int
	SandboxMemory        string
	SandboxCPUs          string
	SandboxTmpfsSize     string
	ModelHostConcurrency int

	Relay               modelRouteOptions
	RegistryProxyPolicy *sandbox.RegistryProxyPolicy
	// GoModuleDir is the host directory of the modules the repository's
	// go.sum lists (repositoryGoModuleDir), which the registry proxy serves
	// to the build; "" when there is none.
	GoModuleDir     string
	ComposeServices composeServicesOptions

	AllowedFiles        []string
	TestPatterns        []string
	TestsRequiredOptOut string
	AllowedPaths        []string
	ScopeUnapproved     bool
	NoTicketScope       bool

	Slice         temporalSliceOptions
	ReleasePolicy release.MergePolicy

	// Thinking, ReviewThinking and ReviewRelayPolicy are roles.execution's and
	// roles.review's resolved fields; see the RunWorkflowInput fields of the
	// same names.
	Thinking          string
	ReviewThinking    string
	ReviewRelayPolicy *sandbox.RoutePolicy

	// Challenge..ChallengeRound and the Challenger fields are -challenge's
	// flags and roles.challenger's resolved values; see the RunWorkflowInput
	// fields of the same names.
	Challenge               bool
	ChallengeSpecPath       string
	ChallengeSpecSHA256     string
	ChallengeCriteria       int
	ChallengeAnswers        []int
	ChallengeCovered        []int
	ChallengeApprovedSHA256 string
	ChallengeApprovedBytes  int
	ChallengerThinking      string
	ChallengerHarness       string
	// ChallengeWithReview is the order resolveChallenger decided from
	// Docker's memory: the challenger job with the review, or after it.
	ChallengeWithReview   bool
	ChallengerRelayPolicy *sandbox.RoutePolicy
	ChallengeHeld         []run.ChallengeFile
	// ChallengeSetAside are a replay's committed held files: set aside by
	// hash, and neither run nor counted in the deadline.
	ChallengeSetAside []run.ChallengeFile
	ChallengeRound    bool
}

// checkpointDir is where the run's Activities checkpoint. Deliberately not
// run.Dir(DataDir, ID): that directory holds the spec snapshot whose absolute
// path build_app.py gets via --spec, so the untrusted subprocess can locate
// it. See workflow.Activities.CheckpointDir's doc comment. Also read directly
// (workflow.RecoverAttemptsFromCheckpointDir) to recover attempt evidence
// when the wait is abandoned client-side.
func (o runOptions) checkpointDir() string {
	return filepath.Join(o.DataDir, "temporal-checkpoints", o.ID)
}

// runDirOfCheckpointDir is the run directory of the run whose
// checkpointDir() this is, for a caller that was handed only that.
func runDirOfCheckpointDir(checkpointDir string) string {
	return run.Dir(filepath.Dir(filepath.Dir(checkpointDir)), filepath.Base(checkpointDir))
}

// overallTimeout is the whole run's deadline: -timeout when set, else
// TimeoutMinutes plus five minutes, plus the challenge phase's own time
// (workflow.ChallengePhaseTime: the phase's launches and one verify of the committed files) for a run that may challenge.
func (o runOptions) overallTimeout() time.Duration {
	if o.OverallTimeout != 0 {
		return o.OverallTimeout
	}
	timeout := time.Duration(o.TimeoutMinutes+5) * time.Minute
	if o.Challenge || len(o.ChallengeHeld) > 0 {
		timeout += workflow.ChallengePhaseTime(o.TimeoutMinutes)
	}
	return timeout
}

// activities is the Activities value this process's own worker registers for
// the run: the static fallbacks for fields RunWorkflowInput also carries, and
// what only the executing worker knows (sandbox limits, credentials).
func (o runOptions) activities() *workflow.Activities {
	activities := &workflow.Activities{
		BuildAppInterpreter:      o.BuildAppInterpreter,
		BuildAppScript:           o.BuildAppScript,
		BuildMaxAttempts:         o.BuildAppMaxAttempts,
		MaxRounds:                o.MaxRounds,
		TimeoutMinutes:           o.TimeoutMinutes,
		VerifyCommand:            o.VerifyCommand,
		VerifyMaxAttempts:        o.VerifyMaxAttempts,
		FastCheckCommand:         o.FastCheckCommand,
		SandboxImage:             o.SandboxImage,
		SandboxDocker:            o.SandboxDocker,
		SandboxUser:              o.SandboxUser,
		SandboxWorkerUID:         o.SandboxWorkerUID,
		SandboxMemory:            o.SandboxMemory,
		SandboxCPUs:              o.SandboxCPUs,
		SandboxTmpfsSize:         o.SandboxTmpfsSize,
		ComposeServicesWorkerEnv: o.ComposeServices.WorkerEnvironment,
		ModelHostConcurrency:     o.ModelHostConcurrency,
		EgressCABundlePath:       o.Relay.CABundlePath,
		DataDir:                  o.DataDir,
		LogDir:                   run.Dir(o.DataDir, o.ID),
		CheckpointDir:            o.checkpointDir(),
		SandboxApprovalsDataDir:  firstNonEmptyString(o.ApprovalDataDir, o.DataDir),
		InstallCacheImages:       o.InstallCacheImages,
		InstallLockDir:           installLockDir(),
	}
	applyRelayOptsToActivities(activities, o.Relay)
	return activities
}

// sandboxSHA256 is the approved sandbox block's hash, "" without one.
func (o runOptions) sandboxSHA256() string {
	if o.Sandbox == nil {
		return ""
	}
	return o.Sandbox.SHA256()
}

// sandboxBlock is the approved sandbox block, the zero block without one.
func (o runOptions) sandboxBlock() projectconfig.Sandbox {
	if o.Sandbox == nil {
		return projectconfig.Sandbox{}
	}
	return *o.Sandbox
}

// sandboxRepoKey is SandboxRepoKey when the run has a sandbox block.
func (o runOptions) sandboxRepoKey() string {
	if o.Sandbox == nil {
		return ""
	}
	return o.SandboxRepoKey
}

// installLockDir is the machine-wide directory of the install image build
// locks, under the root every profile's data dir is in, as the local
// registry the images go to is one per machine.
func installLockDir() string {
	return filepath.Join(sessionconfig.DataRoot(), "install-images")
}
