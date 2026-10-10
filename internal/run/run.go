// Package run defines the durable record for one factoryd run and how it
// is persisted to disk. This is a deliberately minimal stand-in for the
// full plan's SQLite `runs`/`attempts`/`gate_results` tables: one JSON file
// per run, written atomically, so state survives a crash without an
// external dependency. Replace with SQLite when a second run needs to
// exist concurrently or be queried.
package run

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"buildgate/internal/evidence"
	"buildgate/internal/meter"
	"buildgate/internal/sanitize"
)

type State string

const (
	StateReady        State = "ready"
	StateSliceRunning State = "slice_running"
	StateVerifying    State = "verifying"
	StateAccepted     State = "accepted"
	StateHalted       State = "halted"
	StateQuarantined  State = "quarantined"
)

// BillingSubscription/BillingMetered are the two valid values of
// sandbox.RoutePolicy.Billing/RouteLaunchFacts.Billing/Attempt.
// RelayBilling ("subscription"/"metered" -- Phase 2 routes:/models:
// config, internal/sessionconfig, internal/modelrole). Defined here, not
// in internal/sandbox, because internal/sandbox already imports
// internal/run (for Attempt below) while internal/run imports no package
// that would create a cycle back -- the single shared
// definition both sides use, rather than each side inventing its own
// string literal that could silently drift apart.
const (
	BillingSubscription = "subscription"
	BillingMetered      = "metered"
)

// AttemptRoleExecution/AttemptRoleReview are Attempt.Role's two valid
// values -- the same strings as internal/modelrole.RoleExecution/
// RoleReview, duplicated here (not imported) because internal/run is a
// leaf package modelrole is deliberately never imported into (see
// modelrole's own doc comment: "never imported by cmd/, internal/
// workflow, or internal/api"), while both cmd/factoryd and internal/
// workflow construct Attempt values and need a shared, non-stringly-typed
// constant rather than each inventing its own literal.
const (
	AttemptRoleExecution = "execution"
	AttemptRoleReview    = "review"
)

// RetainBuildArtifacts copies what a build left in workspace that removing
// the worktree would delete into runDir, the run's own directory: the
// agent's report and each failed round's saved output (evidence.RetainFile
// and evidence.RetainRoundLogs, byte copies of agent-written files that
// nothing here reads). A build that left neither is not an error. Call it
// before every rollback of a run's worktree.
func RetainBuildArtifacts(workspace, runDir string) error {
	var errs []error
	err := evidence.RetainFile(filepath.Join(workspace, AgentReportFileName), filepath.Join(runDir, AgentReportFileName))
	if err != nil && !os.IsNotExist(err) {
		errs = append(errs, fmt.Errorf("retain %s: %w", AgentReportFileName, err))
	}
	if _, err := evidence.RetainRoundLogs(workspace, filepath.Join(runDir, evidence.RoundLogsDirName)); err != nil {
		errs = append(errs, fmt.Errorf("retain round logs: %w", err))
	}
	return errors.Join(errs...)
}

// HaltReasonRelayCeilingExceeded is Run.HaltReasonCode's value when a run
// halted because its relay crossed its configured absolute
// TokenCeiling/CostCeilingMicroUSD (see
// internal/sandbox.ErrRelayCeilingExceeded) -- see Run.HaltReasonCode's own
// doc comment for why this exists as a fixed, machine-readable identifier
// distinct from HaltError's free text.
const HaltReasonRelayCeilingExceeded = "relay_ceiling_exceeded"

// HaltReasonComposeServicesRejected is Run.HaltReasonCode's value when a
// run halted before its build because the target repo's compose file was
// rejected (see internal/sandbox.ErrComposeServicesRejected): the fix is
// in the target repo or the operator's config, never in the ticket's code.
const HaltReasonComposeServicesRejected = "compose_services_rejected"

// HaltReasonReviewInstructionsFailed is Run.HaltReasonCode's value when a
// run halted before a model review because the repository's instruction
// files could not be prepared for it (SC-019). The cause is in the review
// attempt's ReviewInstructionsError, for the operator only.
const HaltReasonReviewInstructionsFailed = "review_instructions_failed"

// HaltReasonFactoryDirFailed is Run.HaltReasonCode's value when a launch was
// refused because `.factory/` could not be mounted read-only as the commit
// .factory.yml was read from holds it: the directory at that commit, or the
// worktree's entry of that name, has a shape the mount cannot carry. The
// reason is in the refused attempt's FactoryDirError.
const HaltReasonFactoryDirFailed = "factory_dir_failed"

// RouteSkip is one route modelrole.SelectRoute passed over on its way to
// Attempt.RelayRoute -- a durable, evidence-only copy of modelrole.
// RouteSkip (this package cannot import internal/modelrole: modelrole
// itself imports internal/sessionconfig, which imports this package).
// Reason is always one of modelrole's own fixed, factory-authored
// strings, never a resolver's raw error text (see modelrole.RouteSkip's
// own doc comment for why).
type RouteSkip struct {
	Route  string `json:"route"`
	Reason string `json:"reason"`
}

// Attempt is the record of one build or canonical-verification invocation.
type Attempt struct {
	Kind       string   `json:"kind"`
	Command    []string `json:"command"`
	StartedAt  string   `json:"started_at"`
	FinishedAt string   `json:"finished_at"`
	ExitCode   int      `json:"exit_code"`
	LogPath    string   `json:"log_path"`
	// SetupSHA256 is SetupDigest of the repository setup commands (`.factory.yml`
	// setup:) the step ran before its command, empty for a step that ran none.
	// The step says it ran them, so a reader never infers it from Command,
	// whose shape depends on the sandbox runtime.
	SetupSHA256 string `json:"setup_sha256,omitempty"`
	// FactoryDirSHA256 is the hash of the read-only `.factory/` snapshot this
	// attempt's sandbox had mounted over the worktree's, and FactoryDirCommit
	// the commit it was taken from (Run.ProjectConfigCommitSHA). The hash is
	// the same for every attempt that mounted the commit's directory. Both
	// are empty when nothing was mounted: a review attempt, or neither the
	// commit nor the worktree had the directory at that launch. A commit
	// without it and a worktree with it record the hash of the empty
	// directory that was mounted, so the attempts of one run can differ: ""
	// before a build created the directory, the empty snapshot's hash after.
	FactoryDirSHA256 string `json:"factory_dir_sha256,omitempty"`
	FactoryDirCommit string `json:"factory_dir_commit,omitempty"`
	// FactoryDirError is the cleaned reason a launch was refused because
	// `.factory/` could not be mounted as that commit holds it; the run
	// halts with HaltReasonFactoryDirFailed.
	FactoryDirError string `json:"factory_dir_error,omitempty"`
	// ResumedFromCheckpoint is the sha of the snapshot commit
	// (refs/buildgate/checkpoints/<run>/attempt-<n>) holding the work an
	// interrupted earlier Temporal attempt of this build left in the
	// worktree, which this attempt started from. Empty for a build that
	// was not resumed.
	ResumedFromCheckpoint string `json:"resumed_from_checkpoint,omitempty"`
	// ImageDigest is the sandbox worker image's pinned sha256 digest when
	// this attempt ran inside Docker, and empty for a host-runner attempt —
	// durable evidence of exactly which immutable worker image identity
	// produced this attempt's result.
	ImageDigest string `json:"image_digest,omitempty"`
	// RelayImageDigest, RelayNetwork, RelayContainerName, and RelayUpstream
	// are durable, audit-safe evidence that this attempt's sandboxed worker
	// was launched with its only network path being the factory-owned
	// inference relay, not a general network -- they record what the
	// worker *could* reach, not that it actually issued a request through
	// it (the relay's own request log, not this evidence, is what proves
	// that). All four are empty for an attempt with no relay (unsandboxed,
	// or sandboxed without one configured, e.g. canonical verification and
	// the full-suite gate, neither of which ever calls a model).
	RelayImageDigest   string `json:"relay_image_digest,omitempty"`
	RelayNetwork       string `json:"relay_network,omitempty"`
	RelayContainerName string `json:"relay_container_name,omitempty"`
	RelayUpstream      string `json:"relay_upstream,omitempty"`
	// RelayCredentialMode carries runner.Result.RelayCredentialMode (relay.
	// CredentialModeStatic/CredentialModeGitHubCopilot/
	// CredentialModeChatGPTCodex) -- lets a cost-rendering caller
	// (cmd/factoryd's release_and_evidence.go/status.go/watch.go) tell a
	// subscription-billed attempt's spend apart from a metered API
	// attempt's, instead of always presenting a dollar figure as though it
	// were actually charged. Empty for an attempt with no relay, or one
	// recorded before this field existed.
	RelayCredentialMode string `json:"relay_credential_mode,omitempty"`
	// RelayRoute carries internal/sandbox.RouteLaunchFacts.Route: the
	// session-config route name (e.g. "codex", "copilot", "litellm") this
	// attempt's relay was launched from, from a routes:/models: config
	// (internal/sessionconfig, internal/modelrole, Phase 2). Recorded as
	// its own field, not folded into RelayUpstream, because two routes can
	// legitimately share the same upstream (e.g. two static routes against
	// the same LiteLLM endpoint with different credentials) -- RelayRoute
	// is what tells them apart in evidence. Empty for a legacy config with
	// no routes: key, or one recorded before this field existed.
	RelayRoute string `json:"relay_route,omitempty"`
	// RelayBilling carries internal/sandbox.RouteLaunchFacts.Billing:
	// "subscription" or "metered", labeling how this attempt's route is
	// paid for. Display evidence only -- see run.SubscriptionBilled, which
	// prefers this over RelayCredentialMode inference when set. Empty for
	// a legacy config, or one recorded before this field existed.
	RelayBilling string `json:"relay_billing,omitempty"`
	// RelayRouteSkipped records every route modelrole.SelectRoute passed
	// over before choosing RelayRoute above (Selection.Skipped), for the
	// same job's own attempt -- operator visibility into a fallback that
	// silence would otherwise hide entirely: an operator watching a run
	// succeed on its model's second-declared route has no other way to
	// learn the first one was ever tried, let alone why it was skipped.
	// Empty when the chosen route was the first one tried (no fallback),
	// or for a legacy config with no routes: key.
	RelayRouteSkipped []RouteSkip `json:"relay_route_skipped,omitempty"`
	// RelayWorkerModelID carries internal/sandbox.RouteLaunchFacts.
	// WorkerModelID: the worker model id configured for this attempt's
	// relay, from RouteSpec/RoutePolicy configuration rather than anything
	// the agent reported. The relay does not enforce it (see that field's
	// doc comment), so this is display evidence, never a policy input.
	// Empty for an attempt with no relay, no configured model id, or one
	// recorded before this field existed.
	RelayWorkerModelID string `json:"relay_worker_model_id,omitempty"`
	// RelayReasoningEffort carries internal/sandbox.RouteLaunchFacts.
	// ReasoningEffort: the HIGHEST reasoning effort this attempt's relay
	// observed a forwarded request ask for (relay.Server's own
	// highestReasoningEffort), read from the request itself, not agent
	// self-report. Empty for an attempt with no relay, one that never
	// requested a reasoning effort, or one recorded before this field
	// existed.
	RelayReasoningEffort string `json:"relay_reasoning_effort,omitempty"`
	// RelayReasoningEffortAnomaly carries internal/sandbox.
	// RouteLaunchFacts.ReasoningEffortAnomaly: true when at least one
	// request this attempt's relay forwarded named a reasoning effort
	// this relay could not recognize. Sticky and independent of
	// RelayReasoningEffort itself, which never lets an unrecognized value
	// mask a real, lower one as the highest seen -- this is the separate
	// signal that an anomaly happened at all.
	RelayReasoningEffortAnomaly bool `json:"relay_reasoning_effort_anomaly,omitempty"`
	// RelayConsumedInputTokens/RelayConsumedOutputTokens/
	// RelayConsumedCostMicroUSD/RelayCeilingExceeded are this attempt's
	// actual relay spend -- as opposed to the four fields above, which
	// describe only what the worker COULD reach. Populated at relay
	// cleanup from the relay's own final usage log line (see
	// internal/sandbox.finalRelayUsage): before this, a run's durable
	// evidence recorded only its relay's configured budget, never what it
	// actually cost. Zero/false for an attempt with no relay, or one whose
	// relay was never successfully cleaned up.
	RelayConsumedInputTokens  int64 `json:"relay_consumed_input_tokens,omitempty"`
	RelayConsumedOutputTokens int64 `json:"relay_consumed_output_tokens,omitempty"`
	RelayConsumedCostMicroUSD int64 `json:"relay_consumed_cost_micro_usd,omitempty"`
	RelayCeilingExceeded      bool  `json:"relay_ceiling_exceeded,omitempty"`
	// RelaySpendPartial is true when the three RelayConsumed* fields above
	// are a best-effort recovery from this attempt's relay usage ledger, not
	// a confirmed final total -- the relay exited abnormally (crashed,
	// killed) before its container's own logs could be read at cleanup, and
	// every relay container runs with --rm, so those logs are gone by then.
	// Closes CLAIMS.md's "crash-orphaned relay" residual: before this field
	// existed, that case recorded a bare zero, indistinguishable from "this
	// attempt's build never actually called the model." Any caller
	// rendering RelayConsumed* as evidence (see cmd/factoryd's
	// release_and_evidence.go/status.go) must mark it partial, never present
	// it as the complete spend, when this is true.
	RelaySpendPartial bool `json:"relay_spend_partial,omitempty"`
	// ReferenceOracleSHA256 mirrors GateResult.ReferenceOracleSHA256 (see
	// its own doc comment), recorded on the attempt itself too -- found
	// via review, PR #152 round 2: the gate's own hash computation runs
	// before the sandboxed launch it protects, but a launch failure after
	// that (a halt, not a quarantine) never reaches the point where a
	// GateResult is built at all, so without this the hash a real
	// attempt was launched with -- even one that then failed -- would be
	// silently dropped rather than durably recorded anywhere. Set for a
	// "reference_oracle" attempt with -reference-oracle-dir configured;
	// also set on a "build" attempt when -reference-oracle-in-loop-retry
	// is additionally configured -- found via review: this doc comment
	// previously claimed "reference_oracle" exclusively, which this
	// second case now contradicts. Empty otherwise.
	ReferenceOracleSHA256 string `json:"reference_oracle_sha256,omitempty"`
	// Role is the session-config role (internal/modelrole.RoleExecution/
	// RoleReview, recorded here as a plain string -- internal/run stays a
	// leaf package modelrole itself is never imported into, per
	// modelrole's own doc comment) this attempt's build or review round
	// was driven under: "execution" for a "build" attempt (every round,
	// corrective retries included -- they are still Kind "build"), and
	// "review" for a "spec_conformity" attempt. Empty for any other Kind
	// (verify/full_suite_verify/reference_oracle/...), which never
	// resolve a roles: entry at all, and for an attempt recorded before
	// this field existed.
	Role string `json:"role,omitempty"`
	// Harness is the coding-agent CLI (internal/harness registry name) this
	// attempt's job ran under: the job's role's resolved harness, set for every
	// attempt from the job's own harness (never inferred from a default).
	// Empty for an attempt recorded before this field existed.
	Harness string `json:"harness,omitempty"`
	// Thinking is the reasoning-effort level (internal/sessionconfig's
	// RoleConfig.Thinking, e.g. "low"/"medium"/"high"/"max") this
	// attempt's job was TOLD to use -- resolved from roles: up front,
	// same source as Role above. Distinct from RelayReasoningEffort,
	// which is what the relay actually observed a forwarded request ask
	// for: the two can legitimately differ (a level this attempt's agent
	// could not honor and silently clamped), which is exactly the
	// mismatch cmd/factoryd's watch/status render as a "(requested X,
	// sent Y)" hint. Empty when roles: does not set this role's
	// thinking, or for an attempt recorded before this field existed.
	Thinking string `json:"thinking,omitempty"`
	// ExpectedEffort is ExpectedReasoningEffort(Thinking, <the relay
	// spec's worker_model_extra_json this attempt actually launched
	// with>) -- what Pi was actually going to SEND for Thinking, once a
	// model's own thinkingLevelMap translation is accounted for (e.g.
	// "max" mapped to "xhigh"). This, not Thinking directly, is what a
	// caller must compare against RelayReasoningEffort for a clamp hint:
	// comparing Thinking directly false-positives on every model whose
	// thinkingLevelMap legitimately renames a level (found via review --
	// see ExpectedReasoningEffort's own doc comment). Empty when Thinking
	// is empty/"off", or for an attempt recorded before this field
	// existed.
	ExpectedEffort string `json:"expected_effort,omitempty"`
	// HarnessScriptsSHA256 is sandbox.ScriptsSHA256's digest over the
	// staged harness script this attempt ran plus its staged sibling
	// modules (see sandbox.StageSiblingModules), computed once per
	// launch right after staging -- durable evidence of exactly which
	// harness script bytes produced this attempt's result, the same
	// way ImageDigest already records which container image did.
	// Empty for an attempt that staged no script (e.g. canonical
	// verification, which runs the project's own command, not a
	// harness script), or one recorded before this field existed.
	HarnessScriptsSHA256 string `json:"harness_scripts_sha256,omitempty"`
	// Skills are the operator skills (roles.<role>.skills) this attempt's
	// worker saw at /inputs/skills, in config order, and SkillsSHA256 the
	// digest of that read-only snapshot, taken host-side by the launch
	// itself, so it describes exactly what was mounted. Empty when the
	// role names no skills.
	Skills       []string `json:"skills,omitempty"`
	SkillsSHA256 string   `json:"skills_sha256,omitempty"`
	// RepoSkills are the target repo's own project skills (.github/,
	// .agents/, .claude/, .pi/skills entries) the harness could also load,
	// scanned after the attempt so ones the worker added count too.
	//
	// For a review attempt this is the worktree's view after the build; the
	// instruction paths the review saw as the base commit holds them are in
	// ReviewMaskedPaths.
	RepoSkills []string `json:"repo_skills,omitempty"`
	// ReviewInstructionsSHA256 is the SHA-256 of the snapshot of base-commit
	// instruction files a review attempt read (SC-019). Empty for any other
	// attempt, and for a review whose build changed no instruction path.
	ReviewInstructionsSHA256 string `json:"review_instructions_sha256,omitempty"`
	// ReviewMaskedPaths are the workspace-relative instruction paths a review
	// attempt saw as the base commit holds them, mounted read-only over the
	// worktree. At most 64 are listed, then one "... and N more" entry.
	ReviewMaskedPaths []string `json:"review_masked_paths,omitempty"`
	// ReviewRemovedPaths are the untracked instruction-named paths the host
	// removed from the worktree before a review attempt launched. Capped like
	// ReviewMaskedPaths.
	ReviewRemovedPaths []string `json:"review_removed_paths,omitempty"`
	// ReviewInstructionsError is the cleaned text of the error that stopped a
	// review before it launched because the repository's instruction files
	// could not be prepared (SC-019). It is for the operator: it reaches no
	// handoff and no later build.
	ReviewInstructionsError string `json:"review_instructions_error,omitempty"`
}

// ExpectedReasoningEffort answers "what reasoning-effort value was Pi
// actually going to send for this Thinking level", given the model
// alias's own worker_model_extra_json (RouteSpec.WorkerModelExtraJSON,
// JSON-object-shaped text; "" or unparseable is treated as an empty
// object) -- the same source sessionconfig.Settings.ValidateRoles reads
// via alias.WorkerModelExtraJSON["thinkingLevelMap"]. Pi (0.84.4)
// translates a level through thinkingLevelMap whenever the map declares
// a non-empty string for it (a model-specific rename, e.g. "max" ->
// "xhigh") and otherwise sends the level unchanged -- which, for xhigh/
// max specifically, is itself the "clamped to high" outcome
// sessionconfig.RoleConfig's own doc comment describes when the level is
// NOT declared: this function does not special-case that (there is
// nothing to look up), so an undeclared xhigh/max still comes back as
// "xhigh"/"max" here, and a caller comparing this against
// RelayReasoningEffort correctly still sees a mismatch -- exactly the
// real clamp that hint exists to surface. Returns "" for "" or "off"
// (Pi sends no level at all in either case, so there is nothing to
// compare against RelayReasoningEffort).
func ExpectedReasoningEffort(thinking, workerModelExtraJSON string) string {
	if thinking == "" || thinking == "off" {
		return ""
	}
	var extra struct {
		ThinkingLevelMap map[string]any `json:"thinkingLevelMap"`
	}
	// A malformed or absent workerModelExtraJSON simply leaves
	// ThinkingLevelMap nil -- the same "no translation declared" case as
	// a present-but-empty map, so this falls through to returning
	// thinking unchanged below rather than erroring. map[string]any, not
	// map[string]string: a real thinkingLevelMap can carry a non-string
	// value for an unrelated key (e.g. a numeric context-window budget --
	// see sessionconfig_test.go's own fixture), which would otherwise
	// fail the whole Unmarshal; a non-string value for THIS thinking
	// level's own key is exactly "unsupported", the same as absent (see
	// sessionconfig.ValidateRoles' matching comment: "Pi treats a null
	// (or empty) map value as unsupported").
	_ = json.Unmarshal([]byte(workerModelExtraJSON), &extra)
	if mapped, _ := extra.ThinkingLevelMap[thinking].(string); mapped != "" {
		return mapped
	}
	return thinking
}

// SubscriptionBilled reports whether attempts' relay spend was billed to
// an operator's ChatGPT/Copilot subscription rather than a metered API
// key (meter.CredentialModeStatic, or no relay at all). The real rule:
// scanning from the LAST attempt backwards, the first one with a
// non-empty RelayBilling OR RelayCredentialMode decides it, mirroring
// Sandboxed's own "last attempt wins" precedent -- and on that decisive
// attempt, its own RelayBilling wins over its own RelayCredentialMode
// whenever both are set (RelayBilling == "subscription" decides true,
// anything else, i.e. "metered", decides false), falling back to
// inferring from RelayCredentialMode
// (CredentialModeChatGPTCodex/CredentialModeGitHubCopilot) only when that
// same attempt's own RelayBilling is empty -- an attempt recorded before
// RelayBilling existed (Phase 2 routes:/models: config,
// internal/sessionconfig, internal/modelrole), or one from a legacy
// config that never sets it, so older records keep reading exactly as
// they did before this field existed. A dollar figure computed from the
// relay's own configured per-token price is never what a subscription
// route actually billed, so every caller rendering relay cost as
// evidence -- cmd/factoryd's release_and_evidence.go/status.go/watch.go/
// round_summary.go, and internal/api's runCost/apiProjectStatsProvider
// for the console -- needs this to label the figure instead of
// presenting it as a real charge. Lives here, not in cmd/factoryd (where
// it originated), because internal/api -- a separate importer, package
// main cannot be imported by -- needs the identical logic; moved rather
// than duplicated. Purely a display-layer question: budget/ceiling
// enforcement (internal/meter) prices and gates every credential mode
// identically and reads nothing from this.
func SubscriptionBilled(attempts []Attempt) bool {
	for i := len(attempts) - 1; i >= 0; i-- {
		if billing := attempts[i].RelayBilling; billing != "" {
			return billing == BillingSubscription
		}
		if mode := attempts[i].RelayCredentialMode; mode != "" {
			return mode == meter.CredentialModeGitHubCopilot || mode == meter.CredentialModeChatGPTCodex
		}
	}
	return false
}

// Sandboxed reports whether this run's build attempt (the one executing
// untrusted, model-generated code, as opposed to verify/full-suite, which
// run the project's own declared commands) ran inside the Docker sandbox
// rather than directly on the host. Derived from Attempts on every call
// rather than stored as its own field, so it can never drift out of sync
// with the evidence that actually backs it.
//
// Uses the LAST "build" attempt, not the first (found in self-review,
// 2026-09-04, right after this was written): -build-app-max-attempts
// defaults to 2, so a first attempt can fail for an infra reason before
// ever setting ImageDigest -- a launch-time refusal (e.g. the free-space
// guard in internal/sandbox.Run), a transient Docker daemon hiccup, a
// relay-start failure -- while a retry succeeds fully inside the sandbox
// with a real digest. The retry's own result is what the run's final
// state (accepted/quarantined) is actually evaluated against, so it's the
// attempt whose containment status this method must report; scanning
// forward and returning on the first match reported a genuinely
// sandboxed, accepted run as unsandboxed whenever its first attempt had
// merely failed to start.
//
// Exists because, before this, containment status was invisible to the
// fail-closed release-policy path: an accepted run's own state and gate
// results look identical whether or not the code that produced them was
// contained, so nothing could deny an unsandboxed run differently from a
// sandboxed one at the point that actually matters (found via a real
// Opus review pass, 2026-09-04). See
// release.MergePolicy.AllowUnsandboxed for where this gets used.
func (r Run) Sandboxed() bool {
	for i := len(r.Attempts) - 1; i >= 0; i-- {
		if r.Attempts[i].Kind == "build" {
			return r.Attempts[i].ImageDigest != ""
		}
	}
	return false
}

// GateResult is the record of one canonical-verification decision. This is
// the evidence a policy check evaluates — never the agent's own report.
type GateResult struct {
	Check      string   `json:"check"`
	Command    []string `json:"command"`
	Passed     bool     `json:"passed"`
	ExitCode   int      `json:"exit_code"`
	DurationMs int64    `json:"duration_ms"`
	// LogSHA256 is the hash of the exact verify-command output this
	// decision was based on — the "verify-surface hash" from the plan's
	// Phase 4 evidence list. It makes the evidence tamper-evident: the
	// verify.log on disk can be checked against this hash after the fact.
	LogSHA256 string `json:"log_sha256"`
	// ReferenceOracleSHA256 is the "reference_oracle" gate's own
	// content-hash counterpart to LogSHA256, added per PR #151's review
	// (round 2: safety-contract.md SC-012 requires the oracle hash
	// itself be recorded, not only the command's output) — the
	// evidence.SHA256Tree digest of -reference-oracle-dir's content at
	// the moment this gate ran, computed only when that flag is set
	// (empty for every other named gate, and for a reference_oracle run
	// that never configured -reference-oracle-dir at all). Proves WHICH
	// oracle content produced this specific result, independent of
	// whatever that host directory contains later.
	ReferenceOracleSHA256 string `json:"reference_oracle_sha256,omitempty"`
	// BaseCheck is what rerunning a failed command gate on the commit the
	// ticket's work started from showed (see GateBaseCheck). nil for a gate
	// that passed, for a check that is not a named or repository command
	// gate, and for a result recorded before the rerun existed.
	BaseCheck *GateBaseCheck `json:"base_check,omitempty"`
}

// The outcomes of a GateBaseCheck.
const (
	// GateBaseFailsSame: the gate's command failed on the base commit the
	// same way as on the result (the same exit code and the same whole
	// output once run-to-run noise is removed), so no build of the ticket
	// can make it pass.
	GateBaseFailsSame = "fails_same"
	// GateBaseFailsDifferently: the command failed on the base commit too,
	// with another exit code or other output, or with output that could not
	// be compared (none, unreadable, too long). The gate was already
	// red before the ticket's work (which may be what the ticket is for),
	// and what fails now is not what failed then: a build may still fix it.
	GateBaseFailsDifferently = "fails_differently"
	// GateBasePasses: the command exited zero on the base commit; the build
	// (or a flaky command) is why it failed on the result.
	GateBasePasses = "passes"
	// GateBaseNotChecked: the rerun did not reach an exit code, or was not
	// made. Reason says why. The gate's failure is then treated as it was
	// before the rerun existed.
	GateBaseNotChecked = "not_checked"
)

// GateBaseCheck records one rerun of a failed command gate on the commit the
// ticket's work started from, in a scratch worktree and a sandbox like the
// gate's own. It never changes the gate's result: Passed, ExitCode and
// LogSHA256 of the GateResult are the run on the build's result alone.
type GateBaseCheck struct {
	// Outcome is GateBaseFailsSame, GateBaseFailsDifferently, GateBasePasses
	// or GateBaseNotChecked.
	Outcome string `json:"outcome"`
	// BaseSHA is the commit the command was rerun on: the run's diff base
	// when it names one, else the base commit of a run that made its own
	// branch there. Empty when the run's record does not prove where the
	// ticket's work started (a resumed run, or a run on an existing branch,
	// with no diff base): the rerun is then not made.
	BaseSHA string `json:"base_sha,omitempty"`
	// ExitCode is the command's exit code on the base commit; meaningful
	// only when Outcome is not GateBaseNotChecked.
	ExitCode int `json:"exit_code"`
	// LogPath and LogSHA256 are the rerun's output and its hash.
	LogPath   string `json:"log_path,omitempty"`
	LogSHA256 string `json:"log_sha256,omitempty"`
	// Reason says why Outcome is GateBaseNotChecked, as one cleaned line.
	Reason string `json:"reason,omitempty"`
}

// FailsSameOnBase reports whether g is a failed gate whose command failed the
// same way on the base commit: the one outcome that makes a failed command
// gate the operator's.
func (g GateResult) FailsSameOnBase() bool {
	return !g.Passed && g.BaseCheck != nil && g.BaseCheck.Outcome == GateBaseFailsSame
}

// FailedDifferentlyOnBase reports whether g is a failed gate that was already
// failing on the base commit, in another way.
func (g GateResult) FailedDifferentlyOnBase() bool {
	return !g.Passed && g.BaseCheck != nil && g.BaseCheck.Outcome == GateBaseFailsDifferently
}

// OracleCanaryEvidence records one runtime-canary check of a RUN_COMMAND.txt:
// the verdict (oraclecanary.Verdict as a string) and the two exit codes it was
// judged from. Ecosystem is empty when no snapshot could be built, in which
// case Verdict is "UNSUPPORTED" and CanaryExitCode is -1.
type OracleCanaryEvidence struct {
	Verdict        string `json:"verdict"`
	Ecosystem      string `json:"ecosystem,omitempty"`
	RealExitCode   int    `json:"real_exit_code"`
	CanaryExitCode int    `json:"canary_exit_code"`
	Message        string `json:"message,omitempty"`
}

// OracleFile is one file the factory host authored into a run's result
// commit (an accepted acceptance oracle, or the .buildgate/oracles.json
// index) together with the SHA-256 of the exact bytes it committed.
type OracleFile struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

// OracleEvidence is the host-collected record that lets the gates treat a
// committed acceptance oracle differently from an ordinary changed file.
// nil (and therefore absent from the JSON) for every run in a repository
// with no .buildgate/oracles.json at its base commit and no oracle commit
// of its own, so all pre-existing run records and gate behaviour are
// unchanged.
//
// Every exemption it grants is keyed on a content hash, never on a path
// alone: two writers (the factory host and the sandboxed agent) can both
// touch the same path, and only the factory's bytes are pinned.
type OracleEvidence struct {
	// Authored is what factoryd itself committed this run: oracle files
	// and the index, each with the hash of the committed bytes.
	Authored []OracleFile `json:"authored,omitempty"`
	// Deleted is the manifest-declared supersession deletions factoryd
	// itself committed this run.
	Deleted []string `json:"deleted,omitempty"`
	// BaseIndexPaths is every target_path listed in .buildgate/oracles.json
	// as read from BaseSHA. These are protected from agent edits.
	BaseIndexPaths []string `json:"base_index_paths,omitempty"`
	// BaseIndexSHA256 maps each of those paths to the sha256 the base
	// commit's index pinned for it (see MatchesBaseIndexPin).
	BaseIndexSHA256 map[string]string `json:"base_index_sha256,omitempty"`
	// ResultSHA256 and BaseSHA256 are the SHA-256 of the blob at ResultSHA
	// / BaseSHA for every candidate path (authored, deleted, base-index and
	// changed .buildgate/ paths) that is actually changed by this run. A
	// path absent from the map is absent from that commit.
	ResultSHA256 map[string]string `json:"result_sha256,omitempty"`
	BaseSHA256   map[string]string `json:"base_sha256,omitempty"`
}

// FactoryAuthoredIntact reports whether path p is a factory-authored
// oracle write whose blob at ResultSHA still hashes to the pinned hash, or
// a manifest-declared supersession deletion that is in fact absent at
// ResultSHA. Path membership alone never suffices.
func (o *OracleEvidence) FactoryAuthoredIntact(p string) bool {
	if o == nil {
		return false
	}
	got, present := o.ResultSHA256[p]
	for _, a := range o.Authored {
		if a.Path == p && a.SHA256 != "" && present && got == a.SHA256 {
			return true
		}
	}
	if !present {
		for _, d := range o.Deleted {
			if d == p {
				return true
			}
		}
	}
	return false
}

// MatchesBaseIndexPin reports whether the blob at ResultSHA for p hashes to
// the sha256 the base commit's .buildgate/oracles.json PINNED for it. This is
// what exempts a path that only appears in a cumulative -diff-base inventory
// because an earlier round committed it. It is deliberately keyed on the
// index's pinned hash and NOT on "unchanged from the round's base bytes":
// after a quarantined review round advances the branch, the round base can
// itself contain a tampered oracle, and byte-equality with that base would
// then wave the tampering through (found via adversarial review).
func (o *OracleEvidence) MatchesBaseIndexPin(p string) bool {
	if o == nil {
		return false
	}
	pin, ok := o.BaseIndexSHA256[p]
	got, present := o.ResultSHA256[p]
	return ok && pin != "" && present && got == pin
}

// DiffStat summarizes the size of the accepted diff between BaseSHA and
// ResultSHA — the "diff size" evidence from the plan's Phase 4 list.
type DiffStat struct {
	FilesChanged int `json:"files_changed"`
	Insertions   int `json:"insertions"`
	Deletions    int `json:"deletions"`
}

// AgentEvidenceSchemaVersion is the schema_version this struct's own field
// set/shape currently matches. build_app.py (pi-harness-hardening,
// scripts/build_app.py's own write_evidence_json) writes the identical
// literal value into every BUILD_EVIDENCE.json it emits; loadAgentEvidence
// compares the two and warns (never fails the run -- this evidence is
// best-effort by design, see AgentEvidence's own doc comment) on a
// mismatch, so a future shape change on either side of this two-repo,
// unversioned-file contract surfaces as a visible warning on the very next
// real run instead of silently misparsing or dropping fields the way two
// real bugs already did before this field existed (parse_usage's own
// top-level-"usage" assumption never matching a real agent_end event's
// actual shape; agent_turn_errors' identical class of bug against a
// different event -- both found live, 2026-09-07/09-09, only because
// someone happened to inspect a real run's own evidence closely).
const AgentEvidenceSchemaVersion = 2

// AgentEvidence is best-effort structured evidence emitted by build_app.py.
// It is recorded only after factoryd's policy gates have decided the run's
// terminal state, so none of these agent/reviewer details can become a gate.
type AgentEvidence struct {
	// SchemaVersion is 0 (Go's zero value) for a BUILD_EVIDENCE.json
	// written by a build_app.py old enough to predate this field entirely
	// -- not itself an error, just a signal loadAgentEvidence's own
	// mismatch warning should name distinctly from a genuine future
	// version skew ("no schema_version field at all" vs. "schema_version
	// 2, this factoryd only understands 1").
	SchemaVersion int    `json:"schema_version"`
	Generated     string `json:"generated"`
	ReviewPolicy  string `json:"review_policy"`
	// Provider and Model preserve JSON null when build_app.py inherited its
	// configured defaults instead of explicitly pinning an identity.
	Provider      *string              `json:"provider"`
	Model         *string              `json:"model"`
	Succeeded     bool                 `json:"succeeded"`
	StoppedReason string               `json:"stopped_reason"`
	Rounds        []AgentEvidenceRound `json:"rounds"`
	// AgentsMDUsed and AgentsMDGitBlob record whether a committed
	// AGENTS.md guided this build and, if so, its git blob id (a
	// content-addressed identity build_app.py's committed_agents_md_blob
	// gets from `git rev-parse HEAD:AGENTS.md`, needing no full read to
	// compute unlike a hash). Additive fields: SchemaVersion stays 1 --
	// see its own doc comment above -- since an older build_app.py simply
	// omits them and AgentsMDGitBlob's `omitempty` keeps an absent-guidance
	// evidence file's JSON unchanged.
	AgentsMDUsed    bool   `json:"agents_md_used"`
	AgentsMDGitBlob string `json:"agents_md_git_blob,omitempty"`
	// ReviewVerdicts is the per-criterion independent-reviewer
	// outcome against the approved spec's acceptance criteria (build_app.py's
	// own --spec-acceptance-criteria), one entry per declared criterion.
	// Empty/nil when no criteria file was given to build_app.py.
	//
	// This is agent/reviewer-reported evidence recorded AFTER build_app.py
	// has already exited, same as every other field on this struct (see
	// this struct's own doc comment) -- it is rendered in the PR body for
	// a human to read, and is deliberately NOT consulted by
	// policy.EvaluateRun or any other accept/quarantine decision.
	// build_app.py's own review_policy="required" handling already
	// quarantines a run with a non-clean verdict via its ordinary exit
	// code (canonical_verify), the same way whole-diff review already
	// does — a second, Go-side gate re-deciding the same question from
	// this self-reported field would be exactly the never-trust-self-
	// report-gates invariant this codebase's ground rules forbid
	// weakening.
	ReviewVerdicts []ReviewVerdict `json:"review_verdicts,omitempty"`
}

// ReviewVerdict is one acceptance criterion's independent-reviewer
// verdict, part of AgentEvidence.ReviewVerdicts -- see that field's own
// doc comment for why this is informational only, never a gate input.
type ReviewVerdict struct {
	Criterion string `json:"criterion"`
	Verdict   string `json:"verdict"`
	Detail    string `json:"detail,omitempty"`
}

// CodeReviewFinding is one free-form defect an independent code-review
// pass (agent/pi/scripts/code_review.py) reported against the workspace
// diff -- unlike ReviewVerdict, which answers a declared acceptance
// criterion, a CodeReviewFinding has no criterion to match against: it is
// whatever concrete correctness/security/data-loss/concurrency defect the
// reviewer found, each with its own failure scenario.
type CodeReviewFinding struct {
	Severity        string `json:"severity"`
	File            string `json:"file,omitempty"`
	Line            int    `json:"line,omitempty"`
	Summary         string `json:"summary"`
	FailureScenario string `json:"failure_scenario,omitempty"`
}

// CodeReviewResult is the run-record shape of CODE_REVIEW_EVIDENCE.json's
// outcome -- Policy is the --review-policy this run's code_review.py
// launch was given (off/advisory/required, see internal/codereview),
// Available is false when the reviewer produced no parseable response at
// all (never conflated with a real, empty Findings list), and Findings is
// every finding the reviewer reported, capped by internal/codereview's
// own ParseResult. Like ReviewVerdict/SpecConformityVerdicts, this is
// informational only, never itself an accept/quarantine gate input -- see
// this file's own ReviewVerdicts field doc comment for that invariant.
type CodeReviewResult struct {
	Policy    string              `json:"policy"`
	Available bool                `json:"available"`
	Findings  []CodeReviewFinding `json:"findings,omitempty"`
	// StoppedBy is the spend meter's deny code (meter.CodeBudgetExceeded,
	// CodeCeilingExceeded or CodeRateLimited) when the reviewer gave no verdict
	// because its model calls were refused, "" otherwise. One of a closed
	// set, never text copied from the evidence file.
	StoppedBy string `json:"stopped_by,omitempty"`
}

// AgentEvidenceRound records the structured outcome of one build_app.py
// corrective round without making that outcome part of factoryd's policy.
type AgentEvidenceRound struct {
	Index           int    `json:"index"`
	Agent           string `json:"agent"`
	AgentReturnCode int    `json:"agent_returncode"`
	AgentTimedOut   bool   `json:"agent_timed_out"`
	// Usage preserves null when no token-usage event was available and an
	// empty object when the harness collected a genuinely empty event.
	Usage           map[string]any `json:"usage"`
	ReviewerOutcome string         `json:"reviewer_outcome"`
	ReviewerDetail  string         `json:"reviewer_detail"`
	// VerifyPassed preserves null when no canonical command was resolvable.
	VerifyPassed   *bool   `json:"verify_passed"`
	VerifyTimedOut bool    `json:"verify_timed_out"`
	DurationS      float64 `json:"duration_s"`
	// FastCheckRan and FastCheckPassed record whether a --fast-check-command
	// ran this round and, if so, whether it passed. Additive fields, like
	// AgentsMDUsed/AgentsMDGitBlob above: an older build_app.py simply
	// omits them, decoding to their Go zero values (false, nil) rather than
	// erroring or dropping sibling fields. FastCheckPassed mirrors
	// VerifyPassed's own null-means-"didn't run" convention.
	FastCheckRan    bool  `json:"fast_check_ran"`
	FastCheckPassed *bool `json:"fast_check_passed"`
	// What the next round was told about this one (build_app.py's
	// round_feedback): why the round did not finish clean, the files its
	// agent turn changed, an id two rounds share when they failed the same
	// way, the path inside the build workspace of the failing command's
	// whole output, and what happened to the agent process when the failure
	// was not a failing command. Additive, like FastCheckRan above.
	//
	// Blockers and ChangedFiles are nil (JSON null) for a round written by
	// a build_app.py that did not record them, and empty for a round that
	// passed or changed nothing: the same never-collected/collected-empty
	// split Run.ChangedFiles keeps. The three strings are "" in both cases.
	//
	// All five are agent-reported text from the untrusted workspace,
	// bounded and cleaned by AgentEvidence.CleanRoundFeedback before they
	// are recorded. They are for an operator to read; nothing decides on
	// them, and FailureLog is a name to show, never a path to open.
	Blockers         []string `json:"blockers"`
	ChangedFiles     []string `json:"changed_files"`
	FailureSignature string   `json:"failure_signature,omitempty"`
	FailureLog       string   `json:"failure_log,omitempty"`
	AgentNotes       string   `json:"agent_notes,omitempty"`
}

// Outcome classifies the round as "pass" or "fail (<reason>)" using
// only fields AgentEvidenceRound actually records, in the same priority
// order build_app.py's own round_blockers applies (timeout, then the pi
// invocation itself failing, then a fast check substituting for
// verification, then verification itself) -- see that function's doc
// comment in agent/pi/scripts/build_app.py.
//
// build_app.py also blocks a round for reasons no outcome field shows: it
// changed nothing in the workspace, or a required review was not clean. A
// round that recorded blockers while every outcome field reads clean is
// therefore "fail (blocked)". Blockers only ever add a failure: an empty
// list never turns a failing field into a pass, because a round build_app.py
// built without computing them (a restored or fallback round) carries an
// empty list too.
//
// The console's agentEvidenceRoundOutcome (console/src/domain/run.ts)
// applies the same rule.
func (rd AgentEvidenceRound) Outcome() string {
	outcome := rd.outcomeFromFields()
	if outcome == "pass" && len(rd.Blockers) > 0 {
		return "fail (blocked)"
	}
	return outcome
}

func (rd AgentEvidenceRound) outcomeFromFields() string {
	if rd.AgentTimedOut || rd.VerifyTimedOut {
		return "fail (timed out)"
	}
	if rd.AgentReturnCode != 0 {
		return "fail (error)"
	}
	if rd.FastCheckRan && rd.FastCheckPassed != nil && !*rd.FastCheckPassed {
		return "fail (verify)"
	}
	if rd.VerifyPassed != nil {
		if *rd.VerifyPassed {
			return "pass"
		}
		return "fail (verify)"
	}
	// VerifyPassed nil means no canonical command was resolvable (see its
	// own doc comment) -- not a positive pass, so still a failure, but
	// without a more specific reason.
	return "fail (error)"
}

// Passed reports whether the round finished clean.
func (rd AgentEvidenceRound) Passed() bool {
	return rd.Outcome() == "pass"
}

// Bounds on a round's feedback fields as recorded. build_app.py writes
// well inside each (its own checkpoint reader holds the same list and path
// limits); they exist because BUILD_EVIDENCE.json is a file in the
// workspace the build agent can write, read here up to 10 MiB.
const (
	maxRoundBlockers         = 20
	maxRoundBlockerLen       = 200
	maxRoundChangedFiles     = 200
	maxRoundPathLen          = 400
	maxRoundFailureSignature = 64
	maxRoundAgentNotesLen    = 8000
)

// CleanRoundFeedback bounds every round's feedback fields and strips
// control characters, terminal escapes and recognisable secrets from them
// (sanitize.Text; single-line fields are also folded to one line). Called
// once, where BUILD_EVIDENCE.json is read, so the run record, the API and
// the console never hold the raw text. A nil list stays nil, and a list
// stays a list when cleaning empties it.
func (e *AgentEvidence) CleanRoundFeedback() {
	if e == nil {
		return
	}
	for i := range e.Rounds {
		rd := &e.Rounds[i]
		rd.Blockers = cleanLines(rd.Blockers, maxRoundBlockers, maxRoundBlockerLen)
		rd.ChangedFiles = cleanLines(rd.ChangedFiles, maxRoundChangedFiles, maxRoundPathLen)
		rd.FailureSignature = clipRunes(sanitize.Line(rd.FailureSignature), maxRoundFailureSignature)
		rd.FailureLog = clipRunes(sanitize.Line(rd.FailureLog), maxRoundPathLen)
		rd.AgentNotes = clipRunes(sanitize.Text(rd.AgentNotes), maxRoundAgentNotesLen)
	}
}

func cleanLines(in []string, maxItems, maxLen int) []string {
	if in == nil {
		return nil
	}
	if len(in) > maxItems {
		in = in[:maxItems]
	}
	out := make([]string, 0, len(in))
	for _, s := range in {
		// An entry that was nothing but escapes or control characters is
		// dropped: it names no blocker and no file.
		if cleaned := clipRunes(sanitize.Line(s), maxLen); cleaned != "" {
			out = append(out, cleaned)
		}
	}
	return out
}

// clipRunes cuts s to at most n runes, never inside one.
func clipRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

// HarnessEval is a purely descriptive summary of which harness/model
// combination produced this run and how it performed, for comparing
// combinations across runs later. Nil for a run whose originating
// run recorded no execution attempt with a harness (every run
// before Attempt.Harness existed) — populated only
// when there is something real to report, never defaulted to an
// all-zero-value struct, so its presence alone already answers "did this
// run use a chosen harness/model" without inspecting every field.
//
// Computed once, after State has already reached a terminal value and
// AgentEvidence has already been loaded (see BuildHarnessEval's callers
// in cmd/factoryd) — like AgentEvidence itself, it is recorded only after
// factoryd's policy gates have decided the run's outcome, so it can
// describe that outcome but never influence it.
type HarnessEval struct {
	// Harness is the execution role's resolved harness (internal/harness
	// registry name), taken from the run's execution attempts.
	Harness string `json:"harness"`
	// ModelID is the concrete model identity actually used, taken from
	// AgentEvidence.Model (the agent's own self-report) when present —
	// empty when the build never reported one, e.g. an older build_app.py
	// or a run that halted before reaching the model at all.
	ModelID string `json:"model_id,omitempty"`
	// Outcome is this run's own terminal State at the moment this was
	// computed, duplicated here (rather than requiring a reader to cross-
	// reference Run.State) so a HarnessEval record read on its own already
	// answers "did this combination succeed."
	Outcome string `json:"outcome"`
	// DurationMs is the last attempt's FinishedAt minus StartedAt, in
	// milliseconds — zero when either timestamp is missing or unparseable
	// (mirrors statusCompletionTime's own "last attempt only, best effort"
	// convention in cmd/factoryd/status.go).
	DurationMs int64 `json:"duration_ms,omitempty"`
	// InputTokens/OutputTokens/CostMicroUSD are summed across every
	// attempt's RelayConsumedInputTokens/RelayConsumedOutputTokens/
	// RelayConsumedCostMicroUSD — the same fields Attempt already records
	// per its own doc comment, just totaled here for convenience.
	InputTokens  int64 `json:"input_tokens,omitempty"`
	OutputTokens int64 `json:"output_tokens,omitempty"`
	CostMicroUSD int64 `json:"cost_micro_usd,omitempty"`
	// Rounds is len(AgentEvidence.Rounds) — zero when AgentEvidence is nil.
	Rounds int `json:"rounds,omitempty"`
	// Roles summarizes each session-config role (roles.execution,
	// roles.review — Phase 2 roles: config, internal/modelrole) this run's
	// Attempts actually recorded, one entry per distinct Attempt.Role seen,
	// in first-seen order. Empty when Attempts never set Role at all — a
	// legacy config with no roles: block, or a run recorded before Role
	// existed — so an older run's HarnessEval renders exactly as before.
	Roles []HarnessEvalRole `json:"roles,omitempty"`
}

// HarnessEvalRole is one role's summary within HarnessEval.Roles — built
// entirely from the LAST Attempt recording that Role, mirroring this
// file's existing "last attempt wins" precedent (Sandboxed,
// SubscriptionBilled): a corrective build round's later attempt is a
// closer read of what the role actually ran with than its first.
type HarnessEvalRole struct {
	// Role is Attempt.Role's own value ("execution"/"review").
	Role string `json:"role"`
	// Thinking is the reasoning-effort level this role's job was TOLD to
	// use (Attempt.Thinking) — the requested level, not necessarily what
	// was actually sent (see RelayReasoningEffort below).
	Thinking string `json:"thinking,omitempty"`
	// ModelID is Attempt.RelayWorkerModelID — the configured worker model
	// id for this role's relay.
	ModelID string `json:"model_id,omitempty"`
	// RelayReasoningEffort is Attempt.RelayReasoningEffort — the highest
	// effort this role's relay actually observed a forwarded request ask
	// for. A caller rendering both this and Thinking together and finding
	// them different (e.g. told "max", sent "high") is looking at a
	// silent clamp, not a discrepancy in this evidence.
	RelayReasoningEffort string `json:"relay_reasoning_effort,omitempty"`
}

// BuildHarnessEval computes r's HarnessEval from data r already carries
// (Attempts, AgentEvidence, State) plus the harness its originating
// execution role resolved. Returns nil when harness is empty — no execution
// attempt recorded one, or there was no originating request at all — so
// callers should pass whatever they resolved for the run, or "" when there is
// none, and let this function decide whether a HarnessEval belongs on the run
// at all.
//
// Pure and side-effect-free: it only reads r, never mutates it or
// consults anything that could feed back into a policy decision — callers
// are expected to assign the result to r.HarnessEval themselves, after
// r.State has already reached its terminal value.
func BuildHarnessEval(r *Run, harness string) *HarnessEval {
	if harness == "" {
		return nil
	}
	ev := &HarnessEval{
		Harness: harness,
		Outcome: string(r.State),
	}
	if r.AgentEvidence != nil {
		if r.AgentEvidence.Model != nil {
			ev.ModelID = *r.AgentEvidence.Model
		}
		ev.Rounds = len(r.AgentEvidence.Rounds)
	}
	if n := len(r.Attempts); n > 0 {
		last := r.Attempts[n-1]
		if last.StartedAt != "" && last.FinishedAt != "" {
			start, startErr := time.Parse(time.RFC3339, last.StartedAt)
			end, endErr := time.Parse(time.RFC3339, last.FinishedAt)
			if startErr == nil && endErr == nil {
				ev.DurationMs = end.Sub(start).Milliseconds()
			}
		}
	}
	roleIndex := map[string]int{}
	for _, a := range r.Attempts {
		ev.InputTokens += a.RelayConsumedInputTokens
		ev.OutputTokens += a.RelayConsumedOutputTokens
		ev.CostMicroUSD += a.RelayConsumedCostMicroUSD
		if a.Role == "" {
			continue
		}
		role := HarnessEvalRole{
			Role:                 a.Role,
			Thinking:             a.Thinking,
			ModelID:              a.RelayWorkerModelID,
			RelayReasoningEffort: a.RelayReasoningEffort,
		}
		if i, ok := roleIndex[a.Role]; ok {
			// Last attempt for this role wins -- see HarnessEvalRole's own
			// doc comment.
			ev.Roles[i] = role
		} else {
			roleIndex[a.Role] = len(ev.Roles)
			ev.Roles = append(ev.Roles, role)
		}
	}
	return ev
}

// NotificationRecord is the auditable record of an out-of-band notification
// emitted when a run reaches a terminal state (accepted, halted, or
// quarantined).
type NotificationRecord struct {
	RunID  string `json:"run_id"`
	Ticket string `json:"ticket"`
	// RequestID is set instead of RunID/Ticket when this notification is
	// one of internal/request's own HITL reminders rather than a
	// run's terminal-state alert -- a request has no run or ticket of its
	// own until the request driver's ticket-sequencing policy starts one.
	RequestID string `json:"request_id,omitempty"`
	Reason    string `json:"reason"`
	State     State  `json:"state"`
	SentAt    string `json:"sent_at"`
	// RunDir is this run's own directory, absolute (AbsDir(dataDir,
	// RunID), not the bare Dir(dataDir, RunID) also embedded in prose
	// inside Reason: "see <dir> and its build/verify logs for detail").
	// Must stay absolute -- see AbsDir's own doc comment for why a
	// relative one broke the one consumer that needs a bare path to
	// open rather than a sentence to parse it back out of (a
	// click-action notifier's deferred, cwd-detached shell command; see
	// internal/notify/desktop.go's terminal-notifier path). Omitempty: a
	// run.json written before this field existed has no "run_dir" key,
	// and that's fine — it just means the older record predates the
	// click-to-open desktop channel.
	RunDir string `json:"run_dir,omitempty"`
	// Delivered is false when the Notifier itself failed (e.g. the local
	// notification log couldn't be opened or synced) — SentAt alone would
	// otherwise read as delivery having succeeded even when it didn't. A
	// pointer, not a plain bool: a run.json written before this field
	// existed has no "delivered" key at all, and that must unmarshal to
	// "unknown" (nil), not silently read as "failed" (false) for
	// notifications that were, in fact, successfully delivered.
	Delivered *bool `json:"delivered,omitempty"`
	// DeliveryError holds the Notifier's error text when Delivered is
	// false, so the audit record explains why without a human having to
	// go find the corresponding log line.
	DeliveryError string `json:"delivery_error,omitempty"`
	// Next is the one command or URL the operator should act on next
	// (e.g. "factoryd retry req-1", a pull request URL). Omitempty: a
	// run.json written before this field existed has no "next" key.
	Next string `json:"next,omitempty"`
	// Link is the console deep link for this notification's run or
	// request (e.g. "https://console.example/runs/run-1"), empty when no
	// console base URL is configured. Omitempty for the same reason as
	// Next.
	Link string `json:"link,omitempty"`
	// Ask is the headline of a request's notification: what is asked of the
	// operator ("Spec ready for your review"). Empty on a run's own
	// notification.
	Ask string `json:"ask,omitempty"`
	// Subject names the request Ask is about, as the console does: its
	// project and title.
	Subject string `json:"subject,omitempty"`
	// RemoteLink is Link through the address `factoryd remote-console`
	// recorded, for a channel read on another machine (Slack, Discord).
	// Empty while the remote console is off. It carries no token.
	RemoteLink string `json:"remote_link,omitempty"`
}

// Override is the auditable record of a human changing a quarantined run's
// terminal state.
type Override struct {
	By         string `json:"by"`
	Reason     string `json:"reason"`
	At         string `json:"at"`
	PriorState State  `json:"prior_state"`
	NewState   State  `json:"new_state"`
}

// Rescue records an operator-directed recovery action. It is audit history
// only: recording a rescue never changes state or resumes execution.
type Rescue struct {
	By         string `json:"by"`
	Reason     string `json:"reason"`
	At         string `json:"at"`
	Action     string `json:"action"`
	PriorState State  `json:"prior_state"`
	NewState   State  `json:"new_state,omitempty"`
}

// MeterSpend is relay spend in the units of the relay's usage ledger: input
// plus output tokens, and cost in micro-USD.
type MeterSpend struct {
	Tokens       int64 `json:"tokens"`
	CostMicroUSD int64 `json:"cost_micro_usd"`
}

type Run struct {
	ID          string `json:"id"`
	Ticket      string `json:"ticket"`
	ProjectPath string `json:"project_path"`
	// Project is the release-decision/kill-switch project identifier,
	// recorded when the run starts as release.ProjectFromWorkspace(
	// ProjectPath): the basename of RepositoryRoot. Readers go through
	// release.ProjectOf rather than re-deriving. Recorded durably so an
	// operator can confirm which id a run's own decisions landed under
	// (found via the 2026-09-05 Opus review, S4); empty on a run recorded
	// before this field existed.
	Project string `json:"project,omitempty"`
	// RepositoryRoot is the git repository containing ProjectPath (or the
	// symlink-resolved ProjectPath itself outside a repository) that
	// Project was derived from -- the same value release.
	// RejectProjectCollision claimed the id for, recorded here so a run's
	// record explains its own id. Empty on a run recorded before this
	// field existed.
	RepositoryRoot string `json:"repository_root,omitempty"`
	WorkspacePath  string `json:"workspace_path"`
	SpecPath       string `json:"spec_path"`
	// Branch is the isolated git branch this run executed on, when
	// the run's isolation put it in its own worktree instead of the shared
	// ProjectPath checkout (see internal/workspace.Prepare) — empty for a
	// run that used the shared checkout directly, in which case
	// WorkspacePath equals ProjectPath. When set, WorkspacePath is that
	// worktree's own path, distinct from ProjectPath, so a later reader
	// can tell "which directory did the agent actually run in" apart from
	// "which project is this run about" for the first time (see the
	// plan's 2026-08-28 Opus review, S1).
	Branch string `json:"branch,omitempty"`
	// PullRequestURL is set once, best-effort, when -open-pull-request
	// caused this accepted run's own Branch to be pushed and a draft PR
	// opened against it (see internal/forge). Empty for every run that
	// didn't request this, and for one that did
	// but the push/PR-open attempt itself failed -- that failure is
	// logged, never treated as a reason to un-accept an already-accepted
	// run, so its absence here does not mean the run failed, only that no
	// PR exists for it.
	PullRequestURL string `json:"pull_request_url,omitempty"`
	// PROpenError is the first line of the error the push/PR-open attempt
	// failed with (empty when it succeeded or was never attempted). Recorded
	// so a request halted for "no pull request was opened" can show the real
	// reason (for example GitHub's "no history in common with main") instead
	// of promising a retry will open one -- found live on todo-service,
	// 2026-09-19, where the reason was only in the daemon log.
	PROpenError string `json:"pr_open_error,omitempty"`
	// OpenPullRequest persists this run's own -open-pull-request request
	// at submission time (see PullRequestURL's own doc comment for the
	// feature this gates), rather than staying a local flag variable.
	// runViaTemporal, runViaRepositoryOwner and reconcileReclaimedRun's
	// delayed reconciliation all complete through applyRunWorkflowResult,
	// which reads the flag from r, the single completion point every run
	// shares, without threading a new parameter through every caller.
	OpenPullRequest bool `json:"open_pull_request,omitempty"`
	// PRCloses is a fully-qualified "<owner>/<repo>#<N>" GitHub issue
	// reference to close via the draft PR's own body (GitHub's "Closes
	// <owner>/<repo>#<N>" auto-close convention), when this run was
	// submitted with `factoryd submit -issue` -- see QueueEntry's own
	// IssueRef field (cmd/factoryd/queue.go), which is where this value
	// originates. Always qualified with the ISSUE's own owner/repo, never
	// just "#<N>": -issue's URL and this run's own repository are
	// independent inputs, and a bare issue number is resolved against
	// whichever repository the PR itself lands in, which could silently
	// close an unrelated same-numbered issue there if the two ever differ
	// (found via Codex review of PR #92). Empty (default) means no issue
	// to close; the PR body then carries no such line. Persisted here for
	// the same reason OpenPullRequest is: the completion point that
	// renders the PR body (renderEvidenceMarkdown) reads r itself, not a
	// flag variable local to whichever execution path submitted this run.
	PRCloses string `json:"pr_closes,omitempty"`
	// PRBase is the git branch this run's own draft PR should open
	// stacked against, instead of the repo default branch, when this run
	// was submitted with `factoryd <run> -pr-base` (worker's own
	// caller: QueueEntry.PRBase, set by ticketQueueEntry for ticket N of a
	// multi-ticket request whose ticket N-1 already has an open PR).
	// Threaded to forge.PullRequestOpener.OpenDraftPullRequest's own base
	// parameter by openEvidencePullRequest -- see that parameter's own doc
	// comment for what happens when the named branch no longer exists on
	// origin (e.g. ticket N-1 merged and its branch was deleted) by the
	// time this run's PR actually opens. Empty (default) means no stacking:
	// the PR opens against the default branch exactly as before this field
	// existed.
	PRBase string `json:"pr_base,omitempty"`
	// Repository is the `-repository`/StartRequest.Repository identity
	// used to route this run through a shared per-repository task queue
	// (see internal/workflow's RepositoryOwnerWorkflow) — empty for a run
	// that never used one. Recorded here, not just passed through at
	// submission time, so a later caller (the console's project-selection
	// screen prefilling a repeat run) can recover it: without this field,
	// GET /projects had no way to carry repository identity forward at
	// all, and every repeat run against a known project still required
	// retyping it (found via review).
	Repository string `json:"repository,omitempty"`
	// TemporalWorkflowID/TemporalRunID/TemporalTaskQueue/TemporalAddress
	// link this run's own record to the Temporal Web UI
	// (http://localhost:8233 locally) so an operator can look up the
	// workflow execution actually driving it instead of finding an opaque
	// list of ids there with nothing to correlate them by. Empty for a
	// direct (non-Temporal) run, and for a Temporal-routed run recorded
	// before these fields existed. TemporalWorkflowID equals this run's
	// own ID on the plain -temporal-address path (see run_temporal.go's
	// ExecuteWorkflow ID); under -repository it is the *child* RunWorkflow
	// execution's id (RepositoryOwnerRunWorkflowID), distinct from the
	// shared RepositoryOwnerWorkflow id.
	TemporalWorkflowID string `json:"temporal_workflow_id,omitempty"`
	TemporalRunID      string `json:"temporal_run_id,omitempty"`
	TemporalTaskQueue  string `json:"temporal_task_queue,omitempty"`
	TemporalAddress    string `json:"temporal_address,omitempty"`
	// SpecSHA256 is the hash of the ticket spec file's content at the
	// moment factoryd read it — the "acceptance-oracle hash" from the
	// plan's Phase 4 evidence list. Ticket specs are mutable prose files;
	// this pins exactly which version of the ticket a run was judged
	// against.
	SpecSHA256 string `json:"spec_sha256"`
	// ProjectConfigSHA256 is the SHA-256 of the committed .factory.yml this
	// run read its commands from, "" when the repository has none.
	ProjectConfigSHA256 string `json:"project_config_sha256,omitempty"`
	// ProjectConfigCommitSHA is the commit of the operator's checkout that
	// .factory.yml was read from at dispatch (its HEAD then), set whether or
	// not the repository has the file. Every sandbox of the run that executes
	// a repository command sees `.factory/` as this commit holds it
	// (Attempt.FactoryDirSHA256).
	ProjectConfigCommitSHA string `json:"project_config_commit_sha,omitempty"`
	// ProductSpecSHA256/ContractSHA256 are the hashes of the *project's*
	// root spec/spec.md and spec/contract.md content at the moment this
	// run's mandatory project-bootstrap preflight read them — distinct
	// from SpecSHA256 above, which pins the per-ticket spec, not the
	// product-level spec/contract a whole multi-slice build is judged
	// against. Empty when the preflight was skipped (-skip-project-check)
	// or a project hasn't adopted the spec/spec.md-spec/contract.md
	// convention. Exists to make gap 5 of the plan's 2026-08-28 Opus
	// review checkable ("spec drift across a long build is unmanaged"):
	// see SpecDriftDetectedByRunID below for how a later chained run
	// compares its own freshly-read values against these.
	ProductSpecSHA256 string `json:"product_spec_sha256,omitempty"`
	ContractSHA256    string `json:"contract_sha256,omitempty"`
	// SkipProjectCheck records whether this run's mandatory
	// project-bootstrap preflight was bypassed via -skip-project-check
	// (converted from opt-in to required, 2026-08-29). Before this field
	// existed, an accepted run's own run.json couldn't tell an auditor
	// whether the preflight ran or was bypassed for a project that hadn't
	// adopted the spec/contract/architecture convention -- the durable
	// record looked identical to a run that never needed the escape hatch
	// at all, indistinguishable from ProductSpecSHA256/ContractSHA256
	// alone (both are also empty for a project that simply has no
	// spec/spec.md yet, not only for a skipped preflight). Found via the
	// 2026-09-03 Opus factory-pipeline review.
	SkipProjectCheck bool `json:"skip_project_check,omitempty"`
	// SpecTicketScopeMismatchFields records which of
	// Allowed-Files:/Required-Changed-Files:/Verify-Command:/
	// Required-Content: -ticket-file declared differently (or not at all)
	// than -spec's own snapshot, when this run proceeded anyway via the
	// explicit -allow-spec-ticket-scope-mismatch opt-out -- empty/absent
	// for a run that never hit the mismatch, and never populated for a
	// mismatch that instead halted the run (an accepted or quarantined run
	// carrying this field means the operator explicitly chose to leave
	// -ticket-file's declared scope unenforced). Same rationale as
	// SkipProjectCheck above: without it, this run's own record would be
	// indistinguishable from one whose -spec and -ticket-file simply
	// agreed, silently defeating the opt-out's own "explicit, logged"
	// design (found via review).
	SpecTicketScopeMismatchFields []string `json:"spec_ticket_scope_mismatch_fields,omitempty"`
	// SpecDriftDetectedByRunID/SpecDriftDetectedAt/SpecDriftReason are gap
	// 5's cross-run attribution: set on THIS run's record when a later
	// chained run (one that declared this run as its -prior-run) found
	// that the project's root spec.md/contract.md no longer hashes the
	// same as what this run's own ProductSpecSHA256/ContractSHA256 above
	// recorded. Unlike InvalidatedByRunID below (a proven full-suite
	// regression), a spec/contract edit is not inherently wrong — it may
	// be a deliberate, planned revision mid-build — so detecting it never
	// halts or quarantines either run and never changes State. It is
	// purely a durable, attributable flag for a human later auditing this
	// run's acceptance evidence: the requirements it was judged against
	// may since have changed. Empty/zero means no drift was observed
	// relative to any later chained run. See runProjectBootstrapCheck and
	// its -prior-run comparison call site (cmd/factoryd/main.go) for where
	// this is set.
	SpecDriftDetectedByRunID string `json:"spec_drift_detected_by_run_id,omitempty"`
	SpecDriftDetectedAt      string `json:"spec_drift_detected_at,omitempty"`
	SpecDriftReason          string `json:"spec_drift_reason,omitempty"`
	State                    State  `json:"state"`
	BaseSHA                  string `json:"base_sha"`
	// DiffBaseSHA is set only when -diff-base overrode the range the
	// diff-shape gates (diff_scope, required_files_changed,
	// required_content, tests_added) and the PR-body evidence were
	// computed from: cmd/factoryd's own -on-branch corrective-PR-review
	// rounds, whose BaseSHA is the round's own small starting point (the
	// branch tip it checked out), not the ticket's original base -- see
	// -diff-base's own flag help. BaseSHA above always stays the
	// checkout point regardless (slice-chain validation and isolated
	// worktree preparation depend on that meaning); empty means the
	// diff-shape gates were evaluated against BaseSHA itself, same as
	// before -diff-base existed.
	DiffBaseSHA string `json:"diff_base_sha,omitempty"`
	// InstructionBaseSHA is the commit whose instruction files this run's
	// reviews read (SC-019), always recorded when the run record is created:
	// -instruction-base's value, else (a resumed run) the one the lost run
	// recorded, else the run's effective diff base (DiffBaseSHA, else
	// BaseSHA). Empty only in a record written before the field existed.
	InstructionBaseSHA string `json:"instruction_base_sha,omitempty"`
	ResultSHA          string `json:"result_sha,omitempty"`
	// HaltConfirmed is true only once a StateHalted run's real outcome is
	// positively known: either its own best-effort in-progress-child
	// termination (or, for a still-queued request, cancellation) actually
	// succeeded, or the repository owner's own durable result later
	// confirmed it via reconciliation. Found via review: treating every
	// StateHalted record as proof the underlying Temporal execution
	// stopped let a daemon's reclaim scan retire that run's recovery
	// Worker (or never start one at all) while the child could still
	// genuinely be running, stranding it with no poller.
	//
	// Deliberately polarized so the JSON zero value is the CONSERVATIVE
	// answer: every run.json written before this field existed decodes
	// with HaltConfirmed absent (false), including halts from the very
	// give-up path this field now guards — found via a later review round
	// that a same-named-but-opposite `HaltUnconfirmed` field got backwards.
	// That polarity silently treated every pre-upgrade halted record as
	// confirmed-safe on the strength of a struct field it never had an
	// opinion on, exactly the records most likely to need reclaiming.
	// With this polarity, an old or unknown record is never mistaken for
	// a confirmed one.
	HaltConfirmed bool `json:"halt_confirmed"`
	// KeptForResume marks a halted run whose isolated worktree, branch and
	// isolation marker were deliberately left in place because the build was
	// lost (its factoryd process or Activity died), not because it failed.
	// The worktree holds the build's round state and uncommitted work, so
	// every worktree reaper (reconcile, stranded-worktree recovery, the
	// workflow and caller-side rollbacks) skips a run with this flag until a
	// human decides: a resume adopts the worktree, a rebuild or cancel reaps
	// it and clears the flag (release.ClearKeptForResume). Containers
	// are still removed. The zero value keeps today's behaviour for every
	// older record.
	KeptForResume bool `json:"kept_for_resume,omitempty"`
	// OnBranch is the existing branch this run was told to build on
	// (-on-branch): a PR-review corrective round. Such a run is never kept
	// for a resume: nothing ever decides about it again, so its worktree
	// would leak.
	OnBranch string `json:"on_branch,omitempty"`
	// ResumeSpendCarried is the relay spend this run inherited from the run
	// whose kept worktree it adopted (that run's own ResumeSpendCarried plus
	// its whole relay ledger), recorded at adoption. Every relay ceiling of a
	// resumed run is lowered by it, so a chain of resumes cannot spend a fresh
	// ceiling each time. nil for a run that adopted nothing.
	ResumeSpendCarried *MeterSpend `json:"resume_spend_carried,omitempty"`
	// HaltError is the actual Go error text that caused this run to halt,
	// when the caller had one in hand -- found live: a run whose
	// build_app.py invocation failed at the infrastructure level (e.g. its
	// own lifecycle context was cancelled out from under it) recorded only
	// a generic notify.PrepareHalt reason pointing at "its build/verify
	// logs for detail", but runner.Run's own infrastructure-failure
	// contract (see that package's doc comment) means exactly this case
	// produces a log file with nothing in it -- the pointer led nowhere,
	// and the one place that ever had the real reason (the error string
	// realMain ultimately log.Fatalf's to this process's own stderr) was
	// never persisted anywhere associated with the run. Empty when the
	// halt had no single causal error (e.g. a policy-gate rejection, which
	// already has its own structured reason) or the call site setting
	// StateHalted predates this field.
	HaltError string `json:"halt_error,omitempty"`
	// HaltReasonCode is a fixed, machine-readable identifier for why a run
	// halted, present only for a halt cause an operator or automation needs
	// to distinguish from HaltError's free text at a glance rather than
	// pattern-match prose for -- a distinct machine-readable quarantine
	// reason so an operator can tell "hit the ceiling" apart from "the
	// code is wrong". Values: HaltReasonRelayCeilingExceeded,
	// HaltReasonComposeServicesRejected, HaltReasonReviewInstructionsFailed,
	// HaltReasonFactoryDirFailed, HaltReasonBaselineVerifyFailed.
	// Empty for every halt cause that predates this field or has no such
	// need -- most halts already carry enough signal in their own
	// structured evidence (GateResults, Attempts, ...).
	HaltReasonCode string `json:"halt_reason_code,omitempty"`
	// CommittedByFactoryd is true when the agent finished with an
	// uncommitted diff and factoryd committed it as a safety net. The POC
	// run against a Flutter + Go app repo found the agent doesn't reliably
	// commit its own verified work when driven outside ticket_runner.py's
	// scaffolding — this is the factory's own backstop for that gap, not a
	// replacement for the agent committing correctly.
	CommittedByFactoryd bool `json:"committed_by_factoryd"`
	// ChangedFiles deliberately has no `omitempty`: a successfully
	// collected zero-change inventory (base == result) must serialize as
	// `[]`, distinguishable from `null`, which means collection was never
	// attempted or failed (see main.go's warning-and-continue path).
	// `omitempty` would drop the key in both cases and make them
	// indistinguishable in the durable record.
	ChangedFiles []string `json:"changed_files"`
	// MemoryEdit is the host's evidence about root AGENTS.md at this run's
	// result commit, recorded before the release decision: whether the
	// fenced memory section changed, whether the run's request has an
	// approved proposal and whether the file matches it. Nil when the run
	// touched no AGENTS.md and has no proposal (nothing was read), and in a
	// record written before the field existed.
	MemoryEdit *MemoryEdit `json:"memory_edit,omitempty"`
	// Oracles is the committed-acceptance-oracle evidence (see
	// OracleEvidence); nil unless the base commit carries an oracle index
	// or this run's factory host committed oracles.
	Oracles *OracleEvidence `json:"oracles,omitempty"`
	// DependencyLockfilesTouched is filename-based evidence only and never
	// affects run state or a gate. Like ChangedFiles, it deliberately has no
	// `omitempty`: a genuinely computed empty result must serialize as `[]`,
	// distinguishable from `null`, which means ChangedFiles was never collected
	// and this evidence was therefore never computed.
	DependencyLockfilesTouched []string                    `json:"dependency_lockfiles_touched"`
	DependencyChanges          []evidence.DependencyChange `json:"dependency_changes"`
	// ComposeFilesChanged lists the changed paths that shape compose
	// services: a root compose file the factory reads
	// (internal/sandbox.ComposeServicesFileNames), or the root .env when
	// the build launched services. Evidence only: the edit was never
	// launched in this run.
	ComposeFilesChanged []string `json:"compose_files_changed,omitempty"`
	// ComposeServices names the compose services the build phase launched
	// from the base commit, empty when it launched none.
	ComposeServices []string  `json:"compose_services,omitempty"`
	DiffStat        *DiffStat `json:"diff_stat,omitempty"`
	// DiffAvailable is true when a full unified diff between BaseSHA and the
	// workspace state at the moment evidence was collected was snapshotted
	// to this run's own durable directory (see DiffPath) — worktree-
	// inclusive, like DiffStat, so a quarantined run's uncommitted dirt is
	// captured too. False for an old run predating this field, or one whose
	// evidence collection warned-and-continued on the diff step, same
	// convention as DiffStat's own nilable pointer.
	//
	// The diff's actual text deliberately does NOT live in this struct, or
	// in run.json at all — found via review: an earlier version stored it
	// as a plain string field here, which meant every ordinary `GET /runs`/
	// `GET /runs/{id}`/SSE state event serialized and transferred a
	// potentially multi-megabyte blob per run just to answer "does this run
	// have one field to click a button", and (for a Temporal-routed run)
	// risked exceeding Temporal's own default Activity-result payload
	// limit. DiffPath, not this struct, is the source of truth for the
	// content; only this small availability/truncation metadata belongs in
	// every run record.
	DiffAvailable bool `json:"diff_available,omitempty"`
	// DiffTruncated is true when the diff at DiffPath was cut down because
	// it exceeded runner.MaxStoredDiffBytes.
	DiffTruncated bool `json:"diff_truncated,omitempty"`
	// AgentEvidence is nil when build_app.py predates BUILD_EVIDENCE.json
	// or factoryd could not read or parse it. As with DiffStat, the pointer
	// distinguishes "not collected" from genuinely empty evidence.
	AgentEvidence *AgentEvidence       `json:"agent_evidence,omitempty"`
	Attempts      []Attempt            `json:"attempts"`
	GateResults   []GateResult         `json:"gate_results"`
	Notifications []NotificationRecord `json:"notifications"`
	Overrides     []Override           `json:"overrides"`
	Rescues       []Rescue             `json:"rescues"`
	// PriorRunID is the run this one is declared to chain from — a
	// multi-slice build's slice N+1 pointing at slice N — or "" for a
	// standalone run. Recorded from the caller's -prior-run flag, never
	// inferred: this closes part of the gap the plan's 2026-08-28 Opus
	// review named (gap 2, "nothing links slice N+1 to slice N") by
	// giving a chain an explicit, auditable record, distinct from the
	// two runs merely sharing a directory. See ValidateSliceChain for the
	// check this enables: that this run's BaseSHA actually equals the
	// prior run's ResultSHA, so a chain can't silently skip or reorder a
	// slice.
	PriorRunID string `json:"prior_run_id,omitempty"`
	// RequestID is the multi-ticket request this run was started for --
	// set once, at the point request_driver.go starts the ticket's run,
	// and never changed afterward. "" for a run started outside a request
	// (a plain `factoryd <run>` invocation). Recorded on the run itself,
	// not just on the request's own Ticket.RunID back-reference, so
	// findOwningRequest can answer "does this run belong to a request"
	// with a single run.Load instead of a full request.List directory
	// scan -- falling back to that scan only for a run that predates this
	// field (see findOwningRequest's own doc comment).
	RequestID string `json:"request_id,omitempty"`
	// EarlierAttemptOf is the id of the quarantined run whose record
	// (its handoff, rendered as text) this run's build was given as
	// -earlier-attempt, "" when it was given none. Set by the request
	// driver at the point it starts the run, like RequestID. It names the
	// source, never the text: a build lost and resumed is given the
	// record again only by loading that run's handoff afresh, under the
	// same checks as the first time (SC-018).
	EarlierAttemptOf string `json:"earlier_attempt_of,omitempty"`
	// TestsRequiredOptOut is the ticket's declared reason (ticketspec's
	// "Tests-Required: no -- <reason>") for skipping the tests_added
	// gate on this run, or "" when the gate ran normally (no opt-out
	// declared). Recorded on the run so a passing tests_added gate that
	// only passed via opt-out is distinguishable from one a real test
	// file actually satisfied, and so release.Decision can carry the
	// reason forward.
	TestsRequiredOptOut string `json:"tests_required_opt_out,omitempty"`
	// InvalidatedByRunID/InvalidatedAt/InvalidatedReason are the
	// cross-run half of gap 3's regression oracle (the plan's 2026-08-28
	// readiness review): set on THIS run's record when a later run that
	// declared this one as its -prior-run failed its own full_suite_verify
	// gate, meaning the repository no longer passes its full test suite as
	// of that later run's result. This never changes State — an already-
	// accepted run stays accepted; a human decides what to do with the
	// annotation, the same way every other override remains a human
	// decision — and it is necessarily an approximation, not a bisected
	// root cause: the regression could equally be the later run's own
	// change interacting badly with this one's, not a latent bug this
	// run's own acceptance evidence should have caught. Pinning blame
	// precisely would need bisecting the full suite across the intervening
	// commits, which this does not attempt. Empty/zero means never
	// invalidated.
	InvalidatedByRunID string `json:"invalidated_by_run_id,omitempty"`
	InvalidatedAt      string `json:"invalidated_at,omitempty"`
	InvalidatedReason  string `json:"invalidated_reason,omitempty"`
	// FullSuiteConfigured/FullSuiteCadence/FullSuiteSlice and
	// FullSuiteScheduled make an optional cadence decision auditable even
	// when the workflow input intentionally carries an empty command for a
	// slice that is not due.
	FullSuiteConfigured bool `json:"full_suite_configured,omitempty"`
	FullSuiteCadence    int  `json:"full_suite_cadence,omitempty"`
	FullSuiteSlice      int  `json:"full_suite_slice,omitempty"`
	FullSuiteScheduled  bool `json:"full_suite_scheduled,omitempty"`
	// FullSuiteSource records whether this run's own effective full-suite
	// command (the command FullSuiteConfigured/FullSuiteScheduled above
	// describe) is a real, operator/`.factory.yml`-configured command
	// ("", the zero value) or the 2026-09-24 operator-approved
	// substitution:
	// "verify_command" when no full_suite_command resolved from any
	// source at this run's own start and its canonical verify command was
	// used instead, or "none" when explicitly opted out
	// (`-full-suite-command none`; FullSuiteConfigured is false in that
	// case, same as before this field existed). Set once at run start
	// (cmd/factoryd's own resolveFullSuiteCommand) and rendered in the PR
	// evidence body so a reviewer can tell "full suite = verify command
	// (no separate full_suite_command configured)" apart from a
	// deliberately configured one.
	FullSuiteSource string `json:"full_suite_source,omitempty"`
	// SpecConformityConfigured records whether this run's own ticket
	// declared -spec-acceptance-criteria at all -- mirrors
	// FullSuiteConfigured's own "declared, independent of whether the gate
	// actually ran" auditability. NOT currently in policy.AllGateChecks
	// (see that var's own doc comment for why); this field exists so a
	// future exemption there has something to key off of without another
	// run-shape change.
	SpecConformityConfigured bool `json:"spec_conformity_configured,omitempty"`
	// SpecConformityVerdicts is the independent reviewer's per-criterion
	// verdicts from CONFORMITY_EVIDENCE.json (the separate, later launch
	// conformity_review.py runs -- see run_ticket.go's "conformity review,
	// phase 2"), loaded into the run record so the release evidence can show
	// them. Before this field existed they were retained only as a file:
	// build_app.py stopped receiving the criteria when phase 2 was split
	// out, so BUILD_EVIDENCE.json's review_verdicts -- the only place the
	// evidence renderer read them from -- was silently always empty, and the
	// PR body's "Spec conformity" section never appeared. Informational
	// only, like AgentEvidence.ReviewVerdicts: the required/advisory
	// enforcement already happened inside conformity_review.py.
	SpecConformityVerdicts []ReviewVerdict `json:"spec_conformity_verdicts,omitempty"`
	// SpecConformityStoppedBy is CodeReviewResult.StoppedBy for the
	// conformity review: the spend meter's deny code when that review's
	// model calls were refused, "" otherwise.
	SpecConformityStoppedBy string `json:"spec_conformity_stopped_by,omitempty"`
	// CodeReview is the independent code reviewer's outcome from
	// CODE_REVIEW_EVIDENCE.json (agent/pi/scripts/code_review.py's own,
	// separate one-turn launch -- see internal/codereview's package doc
	// comment). Nil for a run that never ran this gate. Not yet consumed
	// by any run path or policy gate (that wiring is PR M2-B); recorded
	// here only so the release evidence can render it once that wiring
	// lands, the same additive-field pattern SpecConformityVerdicts above
	// already established for CONFORMITY_EVIDENCE.json.
	CodeReview *CodeReviewResult `json:"code_review,omitempty"`
	// ReferenceOracleDir is the -reference-oracle-dir this run was
	// configured with, recorded so any finalization site -- including a
	// reclaimed run, which sees only this record -- can find the oracle's
	// MANIFEST.json to learn which criteria the oracle covers. Empty when
	// none was configured. A path, not content: the content hash is
	// recorded separately on the attempts that ran against it.
	ReferenceOracleDir string `json:"reference_oracle_dir,omitempty"`
	// OraclesNotCommittedByRequest records the request's explicit
	// -no-commit-oracles opt-out: the oracle gated the build but the host
	// wrote nothing to the target repository.
	OraclesNotCommittedByRequest bool `json:"oracles_not_committed_by_request,omitempty"`
	// OracleCoveredCriteria is the acceptance criteria MANIFEST.json says
	// the oracle checks deterministically (entries with a non-null
	// oracle_file), so the release evidence can say which criteria had a
	// second, non-LLM check. Empty when there is no oracle or no manifest.
	OracleCoveredCriteria []string `json:"oracle_covered_criteria,omitempty"`
	// OracleCanary is the outcome of the reference_oracle gate's runtime
	// canary: after the real oracle command passed, the SAME command ran
	// against a known-failing snapshot of the oracle. nil when the gate never
	// got that far (no oracle, or the real command failed).
	OracleCanary *OracleCanaryEvidence `json:"oracle_canary,omitempty"`
	// BaselineVerify is the verify command's result on the base commit,
	// run before the build's first round (see BaselineVerify's own doc
	// comment). Read from the run's baseline record by cmd/factoryd's save,
	// so a run that halted on it carries it too.
	BaselineVerify *BaselineVerify `json:"baseline_verify,omitempty"`
	// Triage is one factory-authored sentence explaining why this run
	// quarantined or halted -- derived from evidence the factory already
	// holds (GateResults, Attempts and their logs, ChangedFiles, halt
	// codes), never from the agent's own self-reported prose (see
	// AgentEvidence's own doc comment for why that distinction matters).
	// Set by internal/triage.Run, once: the first time cmd/factoryd's own
	// save() persists this run in a terminal quarantined/halted state, or
	// (a second caller) when internal/api's overrideRun moves a run to
	// one of those states directly.
	// Purely informational: empty when nothing could be said with
	// confidence, and never itself consulted by a policy gate or a later
	// run's evaluation.
	Triage string `json:"triage,omitempty"`
	// HandoffSHA256 is the hash of the run's handoff.json (internal/handoff):
	// the factory's record of what this attempt left for a later one, its
	// rounds, each failed check and the bin it is in. Written with the
	// quarantined or halted state, by the same save, and rewritten when a
	// later save finds the run stopped in another way. Empty for a run in
	// any other state, for one recorded before the handoff existed, and
	// for one stopped by a path that does not build it (an orphaned
	// sandbox's quarantine). A
	// reader checks the file against it, and the file's state against the
	// run's, before using it (handoff.Load).
	HandoffSHA256 string `json:"handoff_sha256,omitempty"`
	// HarnessEval is this run's harness/model performance summary — see
	// HarnessEval's own doc comment for why it is nil whenever the
	// originating request didn't select a harness/model.
	HarnessEval *HarnessEval `json:"harness_eval,omitempty"`
	CreatedAt   string       `json:"created_at"`
	UpdatedAt   string       `json:"updated_at"`
	// ReleasePolicy is the effective release.MergePolicy this run was
	// started with (converted to/from internal/release.MergePolicy by
	// cmd/factoryd, since internal/release already imports this package
	// and internal/run cannot import it back without a cycle), recorded
	// once when the run starts so a LATER reconciliation of this same run
	// -- Temporal reconcileReclaimedRun, or a daemon-restart reclaim --
	// evaluates the release decision against the policy that was actually
	// configured, not against a zero-value release.MergePolicy{} that
	// denies every PR unconditionally (found via review: both
	// reconcileReclaimedRun in cmd/factoryd/reclaim.go and
	// applyRunWorkflowResult's own nil-releasePolicy fallback in
	// cmd/factoryd/apply_run_result.go pass release.MergePolicy{}
	// unconditionally, so every reconciled/reclaimed accepted run was
	// denied release regardless of what the operator actually
	// configured). Only the policy is persisted -- no credential ever
	// lives on MergePolicy, so this carries no secret. Nil/absent means
	// this run predates the field (a legacy record): callers must keep
	// today's fail-closed release.MergePolicy{} behavior for those,
	// logging why, rather than guessing a permissive policy for a run
	// that never recorded one.
	ReleasePolicy *ReleasePolicy `json:"release_policy,omitempty"`
	// FactorydVersion is the version of the factoryd binary that most
	// recently persisted this record via Persist -- "last writer", not
	// "created by": a run touched across a daemon upgrade shows whichever
	// build wrote it last, the same build whose behavior produced
	// everything else in the record as of that write. Set from the
	// package-level FactorydVersion variable (cmd/factoryd's main sets it
	// once at startup from buildVersion(version, settings)); empty for a
	// run persisted before this field existed, or in a test that never
	// sets the package variable.
	FactorydVersion string `json:"factoryd_version,omitempty"`
}

// ReleasePolicy mirrors internal/release.MergePolicy's fields for durable
// storage on a Run record -- see Run.ReleasePolicy's own doc comment for
// why this package keeps its own copy instead of importing
// internal/release (import cycle: internal/release already imports
// internal/run).
type ReleasePolicy struct {
	ProtectedPaths                 []string `json:"protected_paths,omitempty"`
	MaxFilesChanged                int      `json:"max_files_changed"`
	MaxInsertions                  int      `json:"max_insertions"`
	RollbackPlan                   string   `json:"rollback_plan"`
	AllowOverrides                 bool     `json:"allow_overrides,omitempty"`
	AllowDependencyLockfileChanges bool     `json:"allow_dependency_lockfile_changes,omitempty"`
	AllowUnsandboxed               bool     `json:"allow_unsandboxed,omitempty"`
	RequiredGates                  []string `json:"required_gates,omitempty"`
	AllowSkippedProjectCheck       bool     `json:"allow_skipped_project_check,omitempty"`
}

// RecordRescue appends attributable recovery history without mutating the
// run. The caller remains responsible for any separately authorized state
// transition and persistence.
func (r *Run) RecordRescue(by, reason, action string, newState State, now func() string) error {
	if by == "" {
		return fmt.Errorf("rescue by is required")
	}
	if reason == "" {
		return fmt.Errorf("rescue reason is required")
	}
	if action == "" {
		return fmt.Errorf("rescue action is required")
	}
	r.Rescues = append(r.Rescues, Rescue{By: by, Reason: reason, At: now(), Action: action, PriorState: r.State, NewState: newState})
	return nil
}

// ApplyOverride changes a quarantined run's terminal state and appends the
// attributable evidence for that change. Persisting the result is the caller's
// responsibility.
func (r *Run) ApplyOverride(by, reason string, newState State, now func() string) error {
	if r.State != StateQuarantined {
		return fmt.Errorf("override requires a quarantined run; current state is %q", r.State)
	}
	if by == "" {
		return fmt.Errorf("override by is required")
	}
	if reason == "" {
		return fmt.Errorf("override reason is required")
	}

	r.Overrides = append(r.Overrides, Override{
		By:         by,
		Reason:     reason,
		At:         now(),
		PriorState: r.State,
		NewState:   newState,
	})
	r.State = newState
	// Found via review: an operator override to StateHalted is an
	// explicit, attributed human decision — about as definitively
	// confirmed as HaltConfirmed's underlying question ("is this run
	// genuinely done, not possibly still running unattended") can get, not
	// the ambiguous give-up case that field otherwise guards against.
	// Leaving it false here would make a `factoryd daemon` reclaim scan
	// treat an intentionally-halted run as still needing recovery.
	if newState == StateHalted {
		r.HaltConfirmed = true
	}
	return nil
}

// TerminalConfirmed reports whether r is durably done in a way that is safe
// to trust without re-verifying against the repository owner: either State
// is StateAccepted or StateQuarantined (never ambiguous -- nothing else
// ever writes those states speculatively), or State is StateHalted and
// HaltConfirmed is true (see that field's own doc comment for the one case
// a Halted record is NOT trustworthy on its own, and why its zero value is
// deliberately the conservative "not confirmed" answer). Shared by
// cmd/factoryd's own reclaim logic (runsNeedingReclaim,
// terminalReclaimedRunIDs, reconcileReclaimedRun -- cmd/factoryd/
// reclaim.go's runTerminalConfirmed delegates here) and internal/api's
// runViewFor (found via review: without this, a quarantined run's
// progress feed -- which will never move again once quarantined -- kept
// getting read on every run-list poll, and progress.Stalled would report
// a stale-progress "stalled" chip five minutes after quarantine even
// though the run is already durably done), so the two packages'
// definition of "genuinely terminal" can't drift apart.
func (r *Run) TerminalConfirmed() bool {
	if r.State == StateAccepted || r.State == StateQuarantined {
		return true
	}
	return r.State == StateHalted && r.HaltConfirmed
}

// ValidateSliceChain checks that baseSHA — this run's freshly captured
// starting point — actually equals prior's ResultSHA, so a declared
// multi-slice chain can't silently skip, reorder, or re-run a slice
// against a workspace state other than the one the previous slice
// actually left behind. prior must have reached run.StateAccepted: a
// chain built on a halted, quarantined, or still-running prior run has no
// trustworthy ResultSHA to chain from at all. projectPath is this run's
// own ProjectPath, checked against prior's — found via codex review
// (round 3, 2026-08-28): a bare SHA match is not enough, since two
// distinct repositories (or a repository re-cloned/re-initialized
// elsewhere) can legitimately share a commit SHA, particularly an early
// or empty-tree one; without this check a -prior-run pointing at the
// wrong checkout entirely could still pass and be durably recorded as
// this run's real predecessor.
//
// This is a syntactic ancestry check only — it does not re-verify prior's
// own gates or content, since EvaluateRun already did that once and
// re-deciding it here would duplicate, not strengthen, that judgment.
func ValidateSliceChain(prior *Run, baseSHA, projectPath string) error {
	if prior.State != StateAccepted {
		return fmt.Errorf("prior run %q has not reached %q (state is %q); cannot chain a new slice onto it", prior.ID, StateAccepted, prior.State)
	}
	if prior.ProjectPath != projectPath {
		return fmt.Errorf("prior run %q was against project %q, not this run's %q — a chain must stay within one project", prior.ID, prior.ProjectPath, projectPath)
	}
	if prior.ResultSHA == "" {
		return fmt.Errorf("prior run %q has no recorded result_sha to chain from", prior.ID)
	}
	if prior.ResultSHA != baseSHA {
		return fmt.Errorf("this run's base_sha %q does not match prior run %q's result_sha %q — the workspace has moved since the prior slice, or the wrong prior run was declared", baseSHA, prior.ID, prior.ResultSHA)
	}
	return nil
}

// FindChainSuccessor scans dataDir for an already-accepted run in
// projectPath that itself declares priorID as its own PriorRunID — i.e., a
// run that has already advanced the chain past priorID. Returns that
// successor's ID, or "" if priorID is still the chain tip.
//
// This exists specifically because ValidateSliceChain's own baseSHA
// comparison stops being a real staleness signal for an isolated chain:
// on the non-isolated path, a superseded prior run's result_sha can no
// longer match the shared checkout's live HEAD (which the real successor
// already advanced), so that comparison catches a stale -prior-run
// declaration automatically. An isolated successor instead starts its own
// fresh worktree from the declared predecessor's recorded ResultSHA
// directly (see this file's own isolated-chaining callers) — the shared
// checkout's HEAD never moves at all, isolated or not, so it carries no
// signal that a later slice already superseded an earlier one. Call this
// wherever that baseSHA comparison is skipped for exactly that reason, to
// keep "a slice can't chain onto the wrong prior state" true regardless of
// isolation, as CLAIMS.md documents it.
//
// A load error for any candidate record fails the scan closed. Skipping an
// unreadable record could hide the accepted successor this check exists to
// find and let a stale isolated chain silently discard later accepted work.
// The record's project and predecessor are themselves inside the unreadable
// payload, so there is no safe way to classify it as unrelated here.
func FindChainSuccessor(dataDir, projectPath, priorID string) (string, error) {
	entries, err := os.ReadDir(filepath.Join(dataDir, "runs"))
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	for _, entry := range entries {
		if !entry.IsDir() || entry.Name() == priorID {
			continue
		}
		candidate, loadErr := Load(dataDir, entry.Name())
		if loadErr != nil {
			// A run directory can legitimately exist with no run.json yet:
			// cmd/factoryd creates Dir(dataDir, id) and writes
			// spec.snapshot.md into it before several preflight checks run
			// (ticketspec parsing, MisprefixedWorkspacePaths, ...), any of
			// which can reject the invocation and return before Run is ever
			// constructed/saved (found live via Codex review, 2026-09-11,
			// on the misprefixed-path preflight -- but the same gap already
			// existed for every earlier preflight check in that function).
			// Such a directory was never a real run and can never be a
			// chain successor, so skip it rather than treating an
			// unrelated invocation's early rejection as fatal to this one.
			if errors.Is(loadErr, fs.ErrNotExist) {
				continue
			}
			return "", fmt.Errorf("load candidate run %q while finding successor of %q: %w", entry.Name(), priorID, loadErr)
		}
		if candidate.State == StateAccepted && candidate.ProjectPath == projectPath && candidate.PriorRunID == priorID {
			return candidate.ID, nil
		}
	}
	return "", nil
}

// Dir returns the durable-record directory for a run under dataDir.
func Dir(dataDir, id string) string {
	return filepath.Join(dataDir, "runs", id)
}

// AbsDir is Dir, resolved to an absolute path against the caller's
// current working directory. dataDir is the raw -data-dir flag value,
// which defaults to the relative string "data" everywhere in this
// codebase -- fine for every other Dir(dataDir, id) call site, since
// they all run inside the same factoryd invocation and resolve against
// its own cwd consistently. It stops being fine the moment a path
// leaves that invocation to be resolved later by something else: found
// via code review of NotificationRecord.RunDir (this codebase's one
// example so far), which terminal-notifier's -execute runs through
// `sh -c` only on a later click -- possibly after factoryd itself has
// exited, in a shell with an unrelated cwd, where a relative RunDir
// would resolve against the wrong directory (or, worse, silently open
// an unrelated one that happens to exist at that relative path).
//
// filepath.Abs's only failure mode is os.Getwd failing; that's rare
// enough, and this is best-effort context on an already best-effort
// notification, that falling back to the plain (relative) Dir value
// on error is preferable to propagating the failure into every caller.
func AbsDir(dataDir, id string) string {
	dir := Dir(dataDir, id)
	if abs, err := filepath.Abs(dir); err == nil {
		return abs
	}
	return dir
}

// DiffFileName is the file, within Dir(dataDir, id), a run's snapshotted
// diff text is written to (see Run.DiffAvailable's doc comment for why it
// isn't stored in run.json itself). Exported so a writer that only has the
// resolved directory in hand — internal/workflow's CollectEvidenceActivity,
// via its LogDir input, which cmd/factoryd always sets to this same
// Dir(dataDir, id) — can name the exact same file DiffPath below resolves,
// without either side duplicating the literal filename.
const DiffFileName = "diff.patch"

// AgentReportFileName is the free-text report build_app.py writes into a
// run's workspace, retained (via internal/evidence.RetainFile) into this
// same Dir(dataDir, id) alongside diff.patch so it survives the workspace
// being overwritten by a later slice or discarded by an isolated-worktree
// rollback (found via the 2026-09-05 Opus review, S6). Named as a
// constant, not a literal, so cmd/factoryd's own static guard against
// reading agent-authored content to decide anything (see
// TestNeverReadsAgentAuthoredEvidence) never has to special-case a second
// occurrence of that filename appearing in its source.
const AgentReportFileName = "BUILD_REPORT.md"

// DiffPath returns where a run's snapshotted diff text lives on disk.
// Colocated with run.json under Dir, not somewhere content-addressed or
// separately configured, so a run's own directory remains the single place
// to find everything durable about it.
func DiffPath(dataDir, id string) string {
	return filepath.Join(Dir(dataDir, id), DiffFileName)
}

// FactorydVersion is the version of the currently-running factoryd binary.
// cmd/factoryd's main sets it exactly once, at startup, from
// buildVersion(version, settings), before any run can be persisted; a test
// that never sets it gets the zero value "", matching a pre-this-field run
// record. Persist stamps this package-level value onto every run record it
// writes -- see Run.FactorydVersion's own doc comment for what the stamped
// field means.
var FactorydVersion string

// Persist is the one funnel every write of a run record goes through: it
// stamps provenance (r.UpdatedAt, r.FactorydVersion) then durably writes
// the record twice -- Save's authoritative run.json, and RecordEvent's
// append-only audit-trail entry in events.db -- so no caller can produce
// one without the other. Introduced because ~11 call sites called
// r.Save directly and so never appended an event (M4-K1); every one of
// those, plus save()'s own former inline Save+RecordEvent pair in
// cmd/factoryd/sandbox_exec.go, now calls this instead.
//
// A Save failure is returned, since a caller must know synchronously
// whether the authoritative record actually landed. A RecordEvent failure
// is only logged, never returned -- preserving save()'s pre-existing
// behavior of treating the event log as supplementary evidence that must
// never fail an otherwise-successful persist (see RecordEvent's own doc
// comment).
func (r *Run) Persist(dataDir string) error {
	r.UpdatedAt = time.Now().Format(time.RFC3339)
	r.FactorydVersion = FactorydVersion
	r.AttachBaselineVerify(dataDir)
	if err := r.Save(dataDir); err != nil {
		return err
	}
	if err := r.RecordEvent(dataDir); err != nil {
		log.Printf("run %s: warning: could not append durable event log entry: %v", r.ID, err)
	}
	return nil
}

// Save writes r atomically (write to a temp file, then rename) so a crash
// mid-write never leaves a corrupt record behind.
func (r *Run) Save(dataDir string) error {
	dir := Dir(dataDir, r.ID)
	// 0o750/0o600: a run's durable record can include workspace paths and
	// command arguments and is evidence, not something other local users on
	// the same machine need to read.
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("create run dir: %w", err)
	}
	path := filepath.Join(dir, "run.json")
	tmp := path + ".tmp"
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal run: %w", err)
	}
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return fmt.Errorf("write run: %w", err)
	}
	return os.Rename(tmp, path)
}

// lockPath returns the path of the advisory lock file that WithLock uses
// to serialize every load-modify-save sequence against one run's durable
// record, across every process that can mutate it.
func lockPath(dataDir, id string) string {
	return filepath.Join(Dir(dataDir, id), ".lock")
}

// ValidID reports whether id is safe to join beneath dataDir/"runs" — a
// single path segment, not "." or "..", containing no separator of its
// own. Mirrors internal/api's own (independently maintained) check on the
// HTTP path value; this one guards every caller of WithLock, including
// ones — like cmd/factoryd's `override` CLI subcommand's -run flag — that
// never go through the HTTP layer at all. Exported so any other caller
// that must validate an externally-influenced id before passing it to
// Load/WithLock (e.g. internal/sandbox.ReconcileOrphans, whose run id
// comes from a Docker container label) can reuse this exact check instead
// of maintaining a second copy that could drift.
func ValidID(id string) bool {
	return id != "" && id != "." && id != ".." && !strings.ContainsAny(id, `/\`)
}

// WithLock runs fn while holding an exclusive, real OS-level advisory
// lock (syscall.Flock, not merely an in-process mutex) on this run's
// durable record, and blocks until it acquires one. Found via review: two
// independent callers that can each load-modify-save the same run —
// `cmd/factoryd`'s `override` CLI subcommand and `internal/api`'s
// POST /runs/{id}/override endpoint, potentially from entirely separate
// processes sharing the same -data-dir — previously had no way to
// exclude each other. Both could load the same quarantined record before
// either saved, and the later save would silently overwrite the earlier
// one's State and Overrides entry. An in-process mutex (internal/api's
// own overrideMu, since removed) cannot see a different process at all;
// this can, because flock is enforced by the kernel against the actual
// file, not against a lock object private to one process's memory.
// Unix-only (syscall.Flock), matching this codebase's existing precedent
// of Unix-only assumptions elsewhere (e.g. internal/runner's process-group
// signaling).
//
// Does NOT create the run's directory if it doesn't already exist —
// found via review, an earlier version's MkdirAll meant a nonexistent (a
// typo, or an authenticated but arbitrary) run id left an empty
// runs/<id>/ directory with a .lock file behind forever, an unbounded
// disk-filling side effect of an operation that was always going to fail
// with "not found" anyway. Every real run's directory already exists by
// the time anything could call WithLock on it (Save creates it when the
// run is first persisted, long before it could ever reach quarantined and
// become override-eligible), so a missing directory here means the id
// simply doesn't identify a real run; fn still runs in that case, without
// ever holding a lock, so its own not-found handling (loadRun inside the
// closure both callers pass) fires exactly as if a real lock had been
// held and found nothing.
// Validated here, not just by internal/api's own HTTP-layer check —
// found via review: `cmd/factoryd override`'s `-run` flag reaches this
// function directly, with no equivalent validation of its own, so a
// traversal-style value (`-run ../../tmp/existing`) could still open or
// create a `.lock` file outside the runs directory whenever that target
// happened to already exist. WithLock is the one function every caller
// that can mutate a run's durable record goes through, so it's the right
// place to enforce this once, for all of them, rather than duplicating
// the check in each caller and risking one forgetting it.
func WithLock(dataDir, id string, fn func() error) error {
	if !ValidID(id) {
		return fmt.Errorf("invalid run id %q", id)
	}
	path := lockPath(dataDir, id)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if os.IsNotExist(err) {
		return fn()
	}
	if err != nil {
		return fmt.Errorf("open lock file: %w", err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("acquire run lock: %w", err)
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return fn()
}

// Load reconstructs a run from its durable record.
func Load(dataDir, id string) (*Run, error) {
	path := filepath.Join(Dir(dataDir, id), "run.json")
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read run: %w", err)
	}
	var r Run
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, fmt.Errorf("unmarshal run: %w", err)
	}
	// A run still building has its baseline record on disk and not yet in
	// run.json: every reader (status, watch, the API) sees it from here.
	r.AttachBaselineVerify(dataDir)
	return &r, nil
}

// ListByRequestID scans dataDir/runs once and groups every run that
// carries a RequestID (run.Run.RequestID -- set once, at the point
// request_driver.go/pr_review_driver.go starts a ticket's build, a
// spec_conformity corrective round, or a PR-review corrective round; see
// that field's own doc comment) by that id. A run with RequestID == ""
// (predates the field, or started outside a request) is omitted.
//
// This exists so a cost rollup (api.Server.ComputeCostSummary) can find
// EVERY run a request ever spent on -- including a superseded first
// build, a retried build, or a corrective round -- rather than only the
// runs still reachable by walking Ticket.RunID (the latest build only)
// and Ticket.Rounds[].RunID (corrective rounds only, not full retries).
// Live evidence (a Flutter + Go app repo M-E1, 2026-09-28,
// feature-habit-insights-endpoint-and-mcp-20260928-082441): a ticket
// retried after a quarantined first build left that first build's run
// unreachable from either field, silently dropping $6.42 of $19.81 in
// real run spend from the request's reported total.
//
// A single directory scan grouping every request at once, not one scan
// per request, so a caller building cost summaries for many requests
// (listRequests) can scan once and look up per request, rather than
// rescanning dataDir/runs once per request in the list.
//
// A run whose record fails to load (a race between a run directory being
// created and run.json being written, or a corrupt file) is silently
// skipped, the same best-effort tolerance runCost/runModelUsage already
// have for a single missing run -- this is a cost rollup, never a gate.
func ListByRequestID(dataDir string) (map[string][]*Run, error) {
	entries, err := os.ReadDir(filepath.Join(dataDir, "runs"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	out := map[string][]*Run{}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		r, err := Load(dataDir, entry.Name())
		if err != nil {
			continue
		}
		if r.RequestID == "" {
			continue
		}
		out[r.RequestID] = append(out[r.RequestID], r)
	}
	return out, nil
}
