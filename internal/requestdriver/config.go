package requestdriver

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"buildgate/internal/requestsubmit"
	"buildgate/internal/run"
	"buildgate/internal/sandbox"
	"buildgate/internal/sessionconfig"
)

// TicketRunner matches runMainWithReady's own signature. the worker takes
// one as a parameter (defaulting to runMainWithReady in production) purely
// so tests can inject a stub instead of actually invoking build_app.py --
// the same injectable-dependency shape this codebase already uses
// throughout (e.g. api.RunStarter).
type TicketRunner func(ctx context.Context, args []string, onReady func(*run.Run)) error

// WorkerConfig is worker's own session-wide configuration, applied
// identically to every entry it drains -- unlike QueueEntry's per-
// submission fields (workspace/spec/ticket/verify-command), there is no
// per-submission override for any of this. It mirrors, verbatim, the
// subset of runMainWithReady's own sandbox/PR flags a real run
// needs (names, defaults, and help text copied from run_ticket.go, not
// reinvented): without these, BuildTicketRunArgs built an argv missing
// every sandbox flag, and run_ticket.go's own validation rejected
// every queued entry outright (found via PR #89 review, P0).
//
// The registry-proxy fields mirror the same subset of run_ticket.go's own
// -registry-proxy* flags for the same reason: the proxy existed only on
// the bare `factoryd <run>` path, so the submit/worker flow -- the one
// actual reason to have a registry proxy at all -- had no way to turn it
// on.
//
// settings carries every relay/registry-proxy/sandbox/release knob that
// flags-consolidate (2026-09-10) removed from the CLI entirely (every
// registry-proxy-*-upstream, sandbox resource limits, release-evaluation
// knobs, and the rest of sessionconfig.Settings): resolved once, from the
// same session config applySessionConfig already loaded for the Tier-1
// fields above, and applied identically to every drained entry via
// tier2SettingsOverride (worker and runMainWithReady share this
// process, never a subprocess, so there is no argv-based way to pass
// these through -- see that variable's own doc comment). relay-worker-
// base-path, relay-worker-model-extra-json, relay-allowed-path-prefix,
// and relay-credential-header are NOT among these: they remain real
// Tier-1 flags on both this command and run_ticket.go, each falling back
// to its own settings-derived value only when left unset (applySessionConfig's
// own strFlag map, below, for the first three; relay-worker-model-extra-json
// via relayWorkerModelExtraJSONFlag) -- corrected here after an earlier
// version of this comment claimed they flowed through tier2SettingsOverride,
// which was never true and left them silently unconfigurable from session
// config alone until an adversarial review caught it.
type WorkerConfig struct {
	// resume holds the checks of a resume decision; its zero value runs the
	// real ones.
	Resume ResumeGate
	// Sandboxes is the sandbox runtime this worker launches request jobs'
	// workers through, nil to launch them with `docker run`;
	// MeterLedgerRoot is where the meter writes ledgers on this machine.
	Sandboxes           sandbox.Runtime
	MeterLedgerRoot     string
	OpenPullRequest     bool
	BuildAppScript      string
	BuildAppMaxAttempts int
	VerifyMaxAttempts   int
	SandboxImage        string
	RegistryProxy       bool
	RegistryProxyImage  string
	EgressCABundle      string
	ComposeServices     bool
	// prPollInterval/prIgnoreAuthors/prTrustedAuthors are resolved from the
	// flags/session config above and consumed by the PR-review poll loop
	// (pr_review_driver.go's own pollTicketPR) via forge.AuthorPolicy. See
	// the flags' own help text.
	PrPollInterval   time.Duration
	PrIgnoreAuthors  []string
	PrTrustedAuthors []string
	// maxReviewRounds caps how many corrective rounds a ticket's
	// PR may go through before its request is halted with "review rounds
	// exhausted for ticket i/n". See -max-review-rounds' own help text.
	MaxReviewRounds int
	// reviewCorrectiveRounds caps how many automatic corrective builds a
	// ticket gets when its run quarantines with its failed gates a subset
	// of {spec_conformity, code_review}, before the request quarantines as
	// it does today. See -review-corrective-rounds' own help text and
	// TryReviewCorrectiveRound (request_driver.go). 0 disables the
	// corrective round entirely.
	ReviewCorrectiveRounds int
	// hitlReminderInterval is how often the worker's reminder ticker
	// (RemindIfDue) re-reminds a request waiting in spec_review,
	// oracle_review or plan_review. See -hitl-reminder-interval's own
	// help text.
	HitlReminderInterval time.Duration
	// advanceOn is the request driver's own ticket-sequencing policy: "accepted"
	// (default) or "pr_approved" -- see startNextTicketOrFinish's own doc
	// comment (request_driver.go) for what each means. Threaded to that
	// function via the package-level RequestAdvanceOn variable (see its
	// own doc comment for why), not as a parameter.
	AdvanceOn string
	// conformityPolicy mirrors factoryd <run>'s own -conformity-policy
	// flag -- see workerMain's own flag definition for the default it
	// was deliberately given and why (issue #164).
	ConformityPolicy string
	// conformityPolicyExplicit records whether the operator actually
	// chose conformityPolicy (CLI flag or session config), as opposed to
	// it holding worker's own literal default -- BuildTicketRunArgs
	// must forward the flag to the drained entry's child `factoryd <run>`
	// invocation only when this is true. Forwarding it unconditionally
	// (the bug this field exists to fix, caught via adversarial review,
	// GitHub Codex App, PR #169) makes run_ticket.go's own
	// flags.Visit-based "was -conformity-policy explicit" check see
	// worker's default as if the operator had typed it -- there is no
	// per-target-repo .factory.yml equivalent of this flag today
	// (internal/projectconfig.Config has no conformity_policy key), but
	// silently overriding whatever explicit choice -- CLI flag or
	// factoryd's own session config -- the operator otherwise made is the
	// same class of bug regardless.
	ConformityPolicyExplicit bool
	// codeReviewPolicy mirrors factoryd <run>'s own -code-review-policy
	// flag -- the standalone AI code-review pass (agent/pi/scripts/
	// code_review.py, internal/codereview). off (the default) means
	// every worker-drained entry never runs it, matching the bare CLI's
	// own -code-review-policy default.
	CodeReviewPolicy string
	// codeReviewPolicyExplicit mirrors conformityPolicyExplicit above --
	// BuildTicketRunArgs must forward -code-review-policy to the drained
	// entry's child `factoryd <run>` invocation only when this is true,
	// for the identical reason (a target repo's own project config has
	// no code_review_policy key either, but the same
	// forward-unconditionally class of bug applies regardless).
	CodeReviewPolicyExplicit bool
	// settings carries every other relay/registry-proxy/sandbox/release knob
	// that flags-consolidate (2026-09-10) removed from the CLI entirely
	// (meter-max-request-bytes, every registry-proxy sizing/upstream knob
	// beyond what's still listed above as its own field, sandbox resource
	// limits, release-evaluation knobs): resolved once from the same
	// session config applySessionConfig already loaded for the fields
	// above, and applied identically to every drained entry via
	// tier2SettingsOverride (worker and runMainWithReady share this
	// process, never a subprocess, so there is no argv-based way to pass
	// these through -- see that variable's own doc comment). Unlike those,
	// buildAppMaxAttempts/verifyMaxAttempts above are genuinely
	// per-invocation Tier-1 flags rather than daemon-wide settings, so
	// they are forwarded through argv like every other field above
	// instead of through this struct.
	Settings sessionconfig.Settings

	// draftSpecScript/draftSpecInterpreter/specDraftTimeoutMinutes mirror
	// buildAppScript/buildAppInterpreter/timeoutMinutes above, for the
	// request driver's own spec-drafting job rather than a ticket
	// build -- see cmd/factoryd/spec_draft_job.go's own doc comment.
	// allowUnsandboxedSpecDraft has no worker flag of its own (worker
	// "always runs sandboxed and does not accept a host-execution
	// opt-out", see -sandbox-image's own flag help above): it exists so a
	// test can call runSpecDraftJob directly, on the host, without Docker,
	// the same way the worker's tests inject a stub TicketRunner
	// instead of needing Docker for a ticket build.
	DraftSpecScript           string
	DraftSpecInterpreter      string
	SpecDraftTimeoutMinutes   int
	AllowUnsandboxedSpecDraft bool

	// planTicketsScript/planTicketsInterpreter/planTicketsTimeoutMinutes
	// mirror draftSpecScript/draftSpecInterpreter/specDraftTimeoutMinutes
	// above, for the request driver's own plan-drafting job
	// rather than a spec-drafting or ticket build -- see
	// cmd/factoryd/plan_tickets_job.go's own doc comment.
	// allowUnsandboxedSpecDraft (above) is reused for this job too rather
	// than a second flag: both are the request driver's own one-shot pi
	// invocations, and worker itself never accepts a host-execution
	// opt-out either way (see -sandbox-image's own flag help).
	PlanTicketsScript         string
	PlanTicketsInterpreter    string
	PlanTicketsTimeoutMinutes int

	// oracleDraftScript/oracleDraftInterpreter/oracleDraftTimeoutMinutes are
	// the same trio for the request driver's oracle-drafting job
	// (cmd/factoryd/oracle_draft_job.go); allowUnsandboxedSpecDraft above is
	// reused, exactly as for the plan job.
	OracleDraftScript         string
	OracleDraftInterpreter    string
	OracleDraftTimeoutMinutes int

	// temporalAddress is worker's resolved -temporal-address (see
	// resolveTemporalAddress). Every drained entry's child invocation is
	// given the address explicitly, so the child never re-probes and picks
	// differently.
	TemporalAddress string

	// sessionConfigPath is the session config file applySessionConfig
	// actually loaded (its own foundPath return), or "" for an
	// execution-flags-only invocation with no session config file at all.
	SessionConfigPath string
}

// BuildTicketRunArgs builds the runMainWithReady argv equivalent to what an
// operator would type by hand for entry, folding in worker's own
// session-wide cfg -- the CLI-style args runMainWithReady's own
// flag.FlagSet parses. A bool flag (-open-pull-request) is a plain
// flags.Bool on runMainWithReady's own flag set, so its bare name is
// enough to set it true -- no "=true" needed -- and it's simply omitted to
// leave that flag at runMainWithReady's own default (false). An optional
// string flag defaulting to "" (-build-app-script, -sandbox-image) is
// likewise only appended when cfg actually set it, so an operator who
// left it unset gets exactly runMainWithReady's own
// default resolution rather than an explicit empty value.
//
// Every registry-proxy sizing/upstream knob and the rest of cfg.settings
// flags-consolidate (2026-09-10) removed from runMainWithReady's own CLI is
// deliberately NOT built into this argv: runMainWithReady no longer accepts
// those as flags at all, and instead resolves them itself, via
// tier2SettingsOverride, from this exact cfg.settings (see workerMain's
// own assignment of that variable). build-app-max-attempts/
// verify-max-attempts are the exception: despite flags-consolidate also
// moving these off runMainWithReady's CLI, they are genuinely
// per-invocation rather than daemon-wide, so they were restored as
// Tier-1 flags there and are forwarded through this argv like every
// other Tier-1 field above instead of through cfg.settings.
func BuildTicketRunArgs(dataDir string, entry *QueueEntry, cfg WorkerConfig) []string {
	args := []string{
		"-ticket", entry.ID,
		"-workspace", entry.Workspace,
		"-spec", entry.SpecPath,
		"-verify-command", entry.VerifyCommand,
		"-data-dir", dataDir,
		"-build-app-max-attempts", strconv.Itoa(cfg.BuildAppMaxAttempts),
		"-verify-max-attempts", strconv.Itoa(cfg.VerifyMaxAttempts),
	}
	// -conformity-policy is forwarded only when the operator actually
	// chose it (CLI flag or session config) -- forwarding worker's own
	// literal default unconditionally would make run_ticket.go's own
	// flags.Visit-based "was this explicit" check (validate.go's
	// applyProjectConfigDefaults) see it as if the operator had typed it,
	// silently overriding whatever explicit choice the operator otherwise
	// made with worker's own default -- caught via adversarial review,
	// GitHub Codex App, PR #169. Omitting the flag entirely when not
	// explicit lets the child `factoryd <run>` invocation's own flag
	// default resolve exactly as it would for a direct, unconfigured
	// invocation -- including for a WorkerConfig{} built directly in a
	// test that predates this field, which now correctly forwards
	// nothing rather than a stale placeholder default.
	if cfg.ConformityPolicyExplicit {
		args = append(args, "-conformity-policy", cfg.ConformityPolicy)
	}
	// -code-review-policy mirrors -conformity-policy above exactly, for
	// the identical reason.
	if cfg.CodeReviewPolicyExplicit {
		args = append(args, "-code-review-policy", cfg.CodeReviewPolicy)
	}
	// entry.ExecutionHarness is the requester's own per-request execution-role
	// harness pick (`factoryd submit -harness execution=<name>`); every other
	// role's harness comes from session config inside the run itself.
	if entry.ExecutionHarness != "" {
		args = append(args, "-execution-harness", entry.ExecutionHarness)
	}
	if entry.FullSuiteCommand != "" {
		args = append(args, "-full-suite-command", entry.FullSuiteCommand)
	}
	if entry.FullSuiteSource != "" {
		args = append(args, "-full-suite-source", entry.FullSuiteSource)
	}
	if entry.NoCommitOracles {
		args = append(args, "-no-commit-oracles")
	}
	if entry.PreflightProfile != "" {
		args = append(args, "-preflight-profile", entry.PreflightProfile)
	}
	if entry.RequestTicket {
		args = append(args, "-request-ticket")
	}
	if entry.IssueRef != "" {
		args = append(args, "-pr-closes-issue", entry.IssueRef)
	}
	if entry.PRBase != "" {
		args = append(args, "-pr-base", entry.PRBase)
	}
	if entry.ExecutionModel != "" {
		args = append(args, "-execution-model", entry.ExecutionModel)
	}
	if entry.InstructionBase != "" {
		args = append(args, "-instruction-base", entry.InstructionBase)
	}
	if entry.SpecAcceptanceCriteria != "" {
		args = append(args, "-spec-acceptance-criteria", entry.SpecAcceptanceCriteria)
	}
	// ReferenceOracleDir/ReferenceOracleCommand are resolveTicketOracle's
	// own output (Phase 2) -- both set together or neither (see that
	// function's own doc comment), so checking one is sufficient. The
	// mount path and in-loop-retry opt-in are always the same fixed
	// values for an auto-wired oracle -- TicketOracleMountPath, and
	// -reference-oracle-in-loop-retry unconditionally, since resolving
	// an oracle at all already means it was plan_review-approved and
	// hash-verified moments ago; there is no "resolved but don't use it
	// in-loop" case for this path the way the standalone CLI flag has.
	if entry.ReferenceOracleDir != "" {
		args = append(args,
			"-reference-oracle-dir", entry.ReferenceOracleDir,
			"-reference-oracle-mount-path", TicketOracleMountPath,
			"-reference-oracle-command", entry.ReferenceOracleCommand,
			"-reference-oracle-in-loop-retry",
		)
	}
	if cfg.OpenPullRequest {
		args = append(args, "-open-pull-request")
	}
	if cfg.BuildAppScript != "" {
		args = append(args, "-build-app-script", cfg.BuildAppScript)
	}
	if cfg.SandboxImage != "" {
		args = append(args, "-sandbox-image", cfg.SandboxImage)
	}
	// Every route field (upstream, credential, worker model, usage format)
	// is decided entirely by the child's own session config routes:/
	// models:/roles: -- the child (an in-process runMainWithReady call
	// sharing this same process's tier2SettingsOverride -- see
	// resolveSettings' own doc comment) resolves its own route via
	// modelrole.SelectRoute from that same settings override directly,
	// so nothing about the route needs forwarding here.
	// Forwarded explicitly, true or false -- not only when true. cfg.
	// registryProxy already carries worker's own fully-resolved
	// effective value (explicit flag, else explicit session config, else
	// the same default-on-for-model-backed-build_app.py rule run_ticket.go
	// itself applies), so the child must see it as visited rather than
	// re-deriving its own default: omitting a resolved false here let
	// run_ticket.go's own default-on resolution silently turn an
	// operator's explicit -registry-proxy=false back on (found via Codex
	// review of PR #129, P2).
	args = append(args, fmt.Sprintf("-registry-proxy=%t", cfg.RegistryProxy))
	if cfg.RegistryProxy && cfg.RegistryProxyImage != "" {
		args = append(args, "-registry-proxy-image", cfg.RegistryProxyImage)
	}
	if cfg.EgressCABundle != "" {
		args = append(args, "-egress-ca-bundle", cfg.EgressCABundle)
	}
	// Forwarded explicitly, true or false -- not only when true, for the
	// identical reason -registry-proxy is just above (the exact bug class
	// PR #129 fixed for it): cfg.composeServices already carries
	// worker's own fully-resolved effective value, so the child must
	// see it as visited rather than re-deriving its own default-true.
	args = append(args, fmt.Sprintf("-compose-services=%t", cfg.ComposeServices))
	args = append(args, "-temporal-address", cfg.TemporalAddress)
	return args
}

func ResolveFullSuiteCommand(configured, verifyCommand string) (command, source string) {
	return requestsubmit.ResolveFullSuiteCommand(configured, verifyCommand)
}
