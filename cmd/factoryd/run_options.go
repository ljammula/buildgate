package main

import (
	"path/filepath"
	"time"

	"buildgate/internal/release"
	"buildgate/internal/run"
	"buildgate/internal/sandbox"
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
	ConformityPolicy    string
	CodeReviewPolicy    string
	VerifyCommand       string
	FastCheckCommand    string
	FullSuiteCommand    string
	GateCommands        map[string]string
	SetupCommands       []string
	AutofixCommands     []string
	// ProjectConfigCommitSHA is the commit .factory.yml was read from; see
	// the RunWorkflowInput field of the same name.
	ProjectConfigCommitSHA string
	Skills                 roleSkillSet

	ReferenceOracleDir       string
	ReferenceOracleMountPath string

	MaxRounds           int
	TimeoutMinutes      int
	BuildAppMaxAttempts int
	VerifyMaxAttempts   int
	// OverallTimeout is -timeout; zero means TimeoutMinutes plus five minutes.
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

	AllowedFiles           []string
	RequiredChangedFiles   []string
	RequiredContent        []string
	TestPatterns           []string
	TestsRequiredOptOut    string
	SpecAcceptanceCriteria string

	Slice         temporalSliceOptions
	ReleasePolicy release.MergePolicy

	// Thinking, ReviewThinking and ReviewRelayPolicy are roles.execution's and
	// roles.review's resolved fields; see the RunWorkflowInput fields of the
	// same names.
	Thinking          string
	ReviewThinking    string
	ReviewRelayPolicy *sandbox.RoutePolicy
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

// activities is the Activities value this process's own worker registers for
// the run: the static fallbacks for fields RunWorkflowInput also carries, and
// what only the executing worker knows (sandbox limits, credentials).
func (o runOptions) activities() *workflow.Activities {
	activities := &workflow.Activities{
		BuildAppInterpreter: o.BuildAppInterpreter,
		BuildAppScript:      o.BuildAppScript,
		BuildMaxAttempts:    o.BuildAppMaxAttempts,
		ConformityPolicy:    o.ConformityPolicy,
		MaxRounds:           o.MaxRounds,
		TimeoutMinutes:      o.TimeoutMinutes,
		VerifyCommand:       o.VerifyCommand,
		VerifyMaxAttempts:   o.VerifyMaxAttempts,
		FastCheckCommand:    o.FastCheckCommand,
		// SpecAcceptanceCriteria is this Worker's own static fallback,
		// same treatment as FastCheckCommand above -- almost always unset
		// at this level, since the criteria file is per-ticket, but kept
		// for the identical reason.
		SpecAcceptanceCriteria:   o.SpecAcceptanceCriteria,
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
	}
	applyRelayOptsToActivities(activities, o.Relay)
	return activities
}
