package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"buildgate/internal/codereview"
	"buildgate/internal/hostcontrol"
	"buildgate/internal/release"
	"buildgate/internal/requestdriver"
	"buildgate/internal/sandbox"
	"buildgate/internal/sessionconfig"
)

// defaultSpecDraftTimeoutMinutes is runSpecDraftJob's own default when
// workerConfig.specDraftTimeoutMinutes is left at its zero value --
// generous for a single pi invocation (no corrective rounds), well under
// a ticket build's own 45-minute default.
const defaultSpecDraftTimeoutMinutes = 10

// defaultPlanTicketsTimeoutMinutes is runPlanTicketsJob's own default
// when workerConfig.planTicketsTimeoutMinutes is left at its zero
// value -- higher than defaultSpecDraftTimeoutMinutes since a plan pass
// may write several tickets in one pi invocation, not one spec.md.
const defaultPlanTicketsTimeoutMinutes = 15

// defaultOracleDraftTimeoutMinutes is runOracleDraftJob's own default: one
// pi invocation that may write several test files, like a plan pass.
const defaultOracleDraftTimeoutMinutes = 15

// defaultMaxReviewRounds is -max-review-rounds' own default: 3 corrective
// review rounds.
const defaultMaxReviewRounds = 3

// defaultReviewCorrectiveRounds is -review-corrective-rounds' own
// default:
// the operator-approved default is one automatic corrective build, on by
// default -- 0 disables it.
const defaultReviewCorrectiveRounds = 1

// conformityPolicyRequired/conformityPolicyAdvisory are the only legal
// values of -conformity-policy (mirroring build_app.py's own
// --conformity-policy argparse choices) -- named once here and used at
// every site (flag defaults, startup validation, buildTicketRunArgs' own
// zero-value fallback) instead of repeating the literal strings, the
// same convention meter.CredentialModeStatic already follows in this
// same file.
const (
	conformityPolicyRequired = "required"
	conformityPolicyAdvisory = "advisory"
)

// workerFlags bundles every `factoryd worker` flag's *pointer, so
// newWorkerFlags (below) can hand them back to workerMain without an
// unwieldy multi-value return list. Field names match the flag's own local
// variable name at every existing call site.
type workerFlags struct {
	configPath                *string
	skipDoctor                *bool
	dataDir                   *string
	openPullRequest           *bool
	buildAppScript            *string
	draftSpecScript           *string
	specDraftTimeoutMinutes   *int
	planTicketsScript         *string
	planTicketsTimeoutMinutes *int
	oracleDraftScript         *string
	oracleDraftTimeoutMinutes *int
	buildAppMaxAttempts       *int
	verifyMaxAttempts         *int
	sandboxImage              *string
	registryProxy             *bool
	registryProxyImage        *string
	egressCABundle            *string
	composeServices           *bool
	prPollInterval            *time.Duration
	prIgnoreAuthors           *string
	prTrustedAuthors          *string
	maxReviewRounds           *int
	reviewCorrectiveRounds    *int
	hitlReminderInterval      *time.Duration
	advanceOn                 *string
	conformityPolicy          *string
	codeReviewPolicy          *string
	temporalAddress           *string
}

// newWorkerFlags builds `factoryd worker`'s FlagSet in isolation from
// parsing, so USAGE.md's doc-vs-flag drift test
// (TestUSAGEDocFlagsExistOnSubcommand) can enumerate its real flags without
// executing the command.
func newWorkerFlags() (flags *flag.FlagSet, f workerFlags) {
	flags = flag.NewFlagSet("worker", flag.ContinueOnError)
	f.configPath = flags.String("config", "", "session config file supplying any flag below not given on the command line (keys mirror the flag names with underscores; see `factoryd init-config`). Default: the first of "+strings.Join(sessionconfig.DefaultPaths(), ", ")+" that exists")
	f.skipDoctor = flags.Bool("skip-doctor", false, "bypass the factoryd doctor preflight (Docker reachable, every configured image present locally, relay upstream well-formed and listing the worker model from inside the sandbox, contextWindow set) that otherwise runs first and refuses to drain on any failure")
	f.dataDir = flags.String("data-dir", "data", "directory holding queue and run records; must match what `factoryd submit` used")
	f.openPullRequest = flags.Bool("open-pull-request", true, "pass -open-pull-request through to each entry's runMainWithReady invocation, so an accepted run pushes its branch and opens a draft PR. Defaults to true here, unlike the bare CLI's own off-by-default -- worker's whole purpose is the automated intake-to-PR flow, so an operator running it has already opted into that outward-facing side effect. Pass -open-pull-request=false to opt back out")

	// Everything below is copied verbatim (name, default, help text) from
	// run_ticket.go's own flag set -- see workerConfig's own doc comment
	// for why. `factoryd worker -help` and `factoryd -help` should read
	// identically for each of these.
	f.buildAppScript = flags.String("build-app-script", "", "path to build_app.py (default: this version's embedded harness copy)")
	f.draftSpecScript = flags.String("draft-spec-script", "", "path to draft_spec.py, used by the request driver's spec-drafting step (default: this version's embedded harness copy)")
	f.specDraftTimeoutMinutes = flags.Int("spec-draft-timeout-minutes", defaultSpecDraftTimeoutMinutes, "wall-clock budget for the request driver's single spec-drafting pi invocation")
	f.planTicketsScript = flags.String("plan-tickets-script", "", "path to plan_tickets.py, used by the request driver's plan-drafting step (default: this version's embedded harness copy)")
	f.planTicketsTimeoutMinutes = flags.Int("plan-tickets-timeout-minutes", defaultPlanTicketsTimeoutMinutes, "wall-clock budget for the request driver's single plan-drafting pi invocation")
	f.oracleDraftScript = flags.String("draft-oracles-script", "", "path to draft_acceptance_oracles.py, used by the request driver's oracle-drafting step for requests submitted with -draft-oracles (default: this version's embedded harness copy)")
	f.oracleDraftTimeoutMinutes = flags.Int("draft-oracles-timeout-minutes", defaultOracleDraftTimeoutMinutes, "wall-clock budget for the request driver's single oracle-drafting pi invocation")
	f.buildAppMaxAttempts = flags.Int("build-app-max-attempts", 2, "total build_app.py attempts allowed for infrastructure failures; see factoryd <run>'s own flag of the same name")
	f.verifyMaxAttempts = flags.Int("verify-max-attempts", 2, "total canonical-verification attempts allowed for infrastructure failures; see factoryd <run>'s own flag of the same name")
	f.sandboxImage = flags.String("sandbox-image", "", "run build and verify inside this Docker image. Must be digest-pinned (name@sha256:...) -- a mutable tag is rejected. No built-in default -- run `make install` from the buildgate checkout (builds images from source and records them via `factoryd configure-images`) or pass this explicitly. The image bakes only this repo's own dependencies and a sandboxed build has no package-registry network; a target project needing another dependency needs its own image instead (`make project-sandbox-image PROJECT_DIR=<path>`, see internal/sandbox/Dockerfile.project). worker always runs sandboxed and does not accept a host-execution opt-out")
	f.registryProxy = flags.Bool("registry-proxy", false, "opt-in: launch a per-run read-only caching package-registry proxy alongside build_app.py's sandboxed worker, fronting an allowlist of npm/PyPI/Go module proxy upstreams (session-config only; see the registry_proxy_*_upstream keys). Requires -sandbox-image.")
	f.registryProxyImage = flags.String("registry-proxy-image", "", "digest-pinned Docker image for the registry proxy, used when -registry-proxy is set; only used with -registry-proxy. No built-in default -- run `make install` or `make registry-proxy-image`, or pass this explicitly; validated the same way as -relay-image (must be pinned by a sha256 digest)")
	f.egressCABundle = flags.String("egress-ca-bundle", "", "PEM file on this host (e.g. a corporate TLS-interception proxy's CA, such as Zscaler) bind-mounted read-only into the registry-proxy container and trusted by it, in addition to their own system CA roots; only used with -relay-image and/or -registry-proxy. The sandboxed worker itself has no network and needs nothing")
	f.composeServices = flags.Bool("compose-services", true, "launch this run's own docker-compose-declared dependency services alongside the sandboxed worker on every phase (build, verify, full suite); see factoryd <run>'s own flag of the same name. Tuning is session-config only (the compose_services_* keys). Default-on: a target repo with no compose file at its base commit is simply a no-op. Pass -compose-services=false to opt out explicitly")

	// prPollInterval/prIgnoreAuthors wire the config only -- the
	// poll loop that actually reads each open PR's review state on this
	// interval is the PR-review driver. See
	// internal/forge.GHPullRequestOpener's own
	// ReadReviewState/DefaultIgnoreAuthors for what these values will feed.
	f.prPollInterval = flags.Duration("pr-poll-interval", 5*time.Minute, "how often the PR-review poll loop will re-read each open PR's review state; must be at least 1m")
	f.prIgnoreAuthors = flags.String("pr-ignore-authors", "", "comma-separated GitHub logins whose review-thread comments the PR-review poll loop always ignores, in addition to internal/forge.DefaultIgnoreAuthors and the factory's own gh account; wins over -pr-trusted-authors when a login appears in both")
	f.prTrustedAuthors = flags.String("pr-trusted-authors", "", "comma-separated GitHub logins whose review-thread comments trigger a corrective build round; the factory's own gh account is always excluded regardless of this list, and empty means no reviewer comment ever triggers a round")
	f.maxReviewRounds = flags.Int("max-review-rounds", defaultMaxReviewRounds, "most corrective build rounds a ticket's PR may go through, in response to reviewer comments, before its request is halted with \"review rounds exhausted\"; must be at least 1")

	// reviewCorrectiveRounds: counted
	// separately from maxReviewRounds above -- see request.Round.Kind's
	// own doc comment for why the two budgets never share one counter.
	f.reviewCorrectiveRounds = flags.Int("review-corrective-rounds", defaultReviewCorrectiveRounds, "most automatic corrective builds a ticket gets when its run quarantines with its failed gates a subset of {spec_conformity, code_review} (every other recorded gate passed) and at least one of those two carries actionable content (a flagged spec-conformity criterion, or a \"high\"-severity code-review finding), before the request quarantines as it does today; each round writes an addendum spec (the ticket spec plus the flagged criteria and/or blocking code-review findings) and rebuilds on the quarantined run's own branch, going through every gate again including a fresh, independent review. A PR-review corrective round the review gate quarantines the same way gets the same number of fix attempts before it ends. 0 disables both; must not be negative")

	// hitlReminderInterval: the request driver's own reminder ticker. A
	// request that enters spec_review or plan_review is reminded
	// immediately (the request driver's own remindRequest call, request_driver.go) and
	// then again every time this much has elapsed since its last
	// reminder (the worker's RemindIfDue call).
	f.hitlReminderInterval = flags.Duration("hitl-reminder-interval", 15*time.Minute, "how often a request waiting in spec_review, oracle_review, plan_review or resume_review is re-reminded; must be at least 1m")

	// advanceOn: the request driver's own ticket-sequencing policy. See
	// startNextTicketOrFinish's own doc comment (request_driver.go) for
	// what each value means.
	f.advanceOn = flags.String("advance-on", requestdriver.AdvanceOnAccepted, `when a request's ticket build advances to the next ticket (or, on the last ticket, to pr_review): "accepted" (default) as soon as the ticket's run is accepted, or "pr_approved" only once its PR is approved`)

	// conformityPolicy mirrors factoryd <run>'s own -conformity-policy
	// flag (previously missing on this command entirely -- every
	// worker-drained entry always ran under build_app.py's hardcoded
	// "required" conformity policy with no escape hatch, even after an
	// operator hit the "unavailable" verdict failure mode -conformity-
	// policy advisory exists specifically for; see issue #164). Defaults
	// to exactly what run_ticket.go's own flag defaults to ("required")
	// -- so an unconfigured worker entry behaves identically to an
	// unconfigured `factoryd <run>` invocation, the same "worker
	// should behave like the direct CLI by default" reasoning
	// -open-pull-request's own doc comment gives for its one deliberate
	// divergence.
	f.conformityPolicy = flags.String("conformity-policy", conformityPolicyRequired, "build_app.py --conformity-policy (required|advisory): governs the per-criterion spec-conformity review -spec-acceptance-criteria enables; see factoryd <run>'s own flag of the same name. advisory is the documented escape valve when a slow/local model's own conformity-review round-trip times out or returns unparseable JSON, which otherwise quarantines objectively-correct work under an all-\"unavailable\" verdict (issue #164)")

	// codeReviewPolicy mirrors factoryd <run>'s own -code-review-policy
	// flag exactly, defaulting to "off" like the bare CLI -- unlike
	// -conformity-policy's deliberately-"required" default (issue #164),
	// there is no equivalent operator complaint yet for a gate that is
	// off by default, so this stays off until an operator opts a
	// drained entry into it.
	f.codeReviewPolicy = flags.String("code-review-policy", codereview.PolicyOff, "standalone AI code-review pass (agent/pi/scripts/code_review.py): off (default), advisory, or required; see factoryd <run>'s own flag of the same name")

	f.temporalAddress = flags.String("temporal-address", "", "Temporal server address. Default (empty): Temporal at localhost:7233, started with Docker if down; with FACTORYD_AUTOSTART=0 the address is required (a running Temporal is not auto-selected)")
	plainFlagUsage(flags)
	return flags, f
}

// loadWorkerConfig parses worker's flags, applies the session config,
// validates the result and runs the doctor preflight. `factoryd worker`
// shares it, so both resolve one data dir's configuration the same way.
// It returns the config and the data dir.
func loadWorkerConfig(dp *deps, args []string) (requestdriver.WorkerConfig, string, error) {
	flags, qf := newWorkerFlags()
	configPath, skipDoctor, dataDir, openPullRequest := qf.configPath, qf.skipDoctor, qf.dataDir, qf.openPullRequest
	buildAppScript, draftSpecScript, specDraftTimeoutMinutes, planTicketsScript, planTicketsTimeoutMinutes := qf.buildAppScript, qf.draftSpecScript, qf.specDraftTimeoutMinutes, qf.planTicketsScript, qf.planTicketsTimeoutMinutes
	buildAppMaxAttempts, verifyMaxAttempts, sandboxImage := qf.buildAppMaxAttempts, qf.verifyMaxAttempts, qf.sandboxImage
	registryProxy, registryProxyImage, egressCABundle, composeServices := qf.registryProxy, qf.registryProxyImage, qf.egressCABundle, qf.composeServices
	prPollInterval, prIgnoreAuthors, prTrustedAuthors, maxReviewRounds, hitlReminderInterval := qf.prPollInterval, qf.prIgnoreAuthors, qf.prTrustedAuthors, qf.maxReviewRounds, qf.hitlReminderInterval
	reviewCorrectiveRounds := qf.reviewCorrectiveRounds
	advanceOn, conformityPolicy := qf.advanceOn, qf.conformityPolicy
	codeReviewPolicy := qf.codeReviewPolicy
	temporalAddress := qf.temporalAddress

	if err := flags.Parse(args); err != nil {
		return requestdriver.WorkerConfig{}, "", err
	}
	// worker drains requests through the same build_app.py/canonical-verify
	// subprocess path realMain and daemonMain already guard with this same
	// call (see refuseAPITokensInEnvironment's own doc comment) -- checked
	// here, before any drafting/planning/building work starts, not just
	// once a ticket actually reaches build. Found live: a worker started
	// with FACTORYD_API_START_TOKEN set drafted, reviewed and planned a
	// request for 17 minutes before halting at the first ticket's build.
	if err := refuseAPITokensInEnvironment(); err != nil {
		return requestdriver.WorkerConfig{}, "", err
	}
	settings, sessionConfigPath, err := applySessionConfig(flags, *configPath)
	if err != nil {
		return requestdriver.WorkerConfig{}, "", err
	}
	// conformityPolicyExplicit is checked after applySessionConfig:
	// applySessionConfig's own strFlag helper calls flags.Set("conformity-policy", ...) when the
	// session config supplies a value and the operator didn't already
	// pass the flag -- so flagsWasVisited here correctly reports
	// "explicit" for either source, uniformly, the same as strFlag's own
	// set[name] check already treats them.
	conformityPolicyExplicit := flagsWasVisited(flags, "conformity-policy")
	codeReviewPolicyExplicit := flagsWasVisited(flags, "code-review-policy")
	buildAppScriptExplicit := flagsWasVisited(flags, "build-app-script")
	// -registry-proxy mirrors run_ticket.go's own default-on resolution
	// (see that flag's own doc comment there): explicit CLI value wins;
	// failing that, an explicit registry_proxy session-config value wins;
	// failing that, it defaults on exactly when the default, model-backed
	// build_app.py is in play. registryProxyDeliberate (either source) is
	// kept separately from the resolved *registryProxy value below so the
	// "-registry-proxy requires -sandbox-image" check further down can
	// still fire for an operator's own deliberate choice without also
	// firing on the silent default-on case, where -sandbox-image is
	// legitimately left empty for run_ticket.go's own per-entry
	// resolution to canonical later (found via Codex review of PR #129,
	// P2 -- an operator's explicit -registry-proxy=false was silently
	// turned back on by run_ticket.go's own default-on resolution, since
	// buildTicketRunArgs only forwarded the flag when true, and the
	// preflight below never checked the registry-proxy image the child
	// would launch by that same silent default).
	registryProxyExplicit := flagsWasVisited(flags, "registry-proxy")
	registryProxyDeliberate := registryProxyExplicit || settings.RegistryProxyConfigured
	if !registryProxyExplicit {
		if settings.RegistryProxyConfigured {
			*registryProxy = settings.RegistryProxy
		} else {
			*registryProxy = !buildAppScriptExplicit
		}
	}
	// -compose-services three-tier resolution, mirroring run_ticket.go's
	// own identical resolution: explicit CLI value wins; failing that, an
	// explicit compose_services session-config value wins; failing that,
	// the flag's own default (true) stands, unconditionally -- unlike
	// -registry-proxy this isn't gated on !buildAppScriptExplicit (see
	// run_ticket.go's own comment for why), so there is no separate
	// "Deliberate" tracking needed here either: -compose-services has no
	// -sandbox-image-required check to guard.
	if !flagsWasVisited(flags, "compose-services") && settings.ComposeServicesConfigured {
		*composeServices = settings.ComposeServices
	}
	// Every role whose harness needs its own worker image is refused here, at
	// startup, not once per drained entry.
	if err := requireSessionHarnessSandboxImage(settings, *sandboxImage); err != nil {
		return requestdriver.WorkerConfig{}, "", err
	}
	// Rejected here, at startup, rather than left for run_ticket.go's own
	// identical check to reject every drained entry one at a time -- the
	// same fail-fast-on-a-session-wide-misconfiguration reasoning
	// -preflight-profile and -pr-closes-issue already get (found by a local
	// ai-stack code-review pass on PR #93: without it,
	// `worker -registry-proxy` with no -sandbox-image passed worker's
	// own startup check cleanly and only failed deep inside run_ticket.go,
	// once per drained entry). Gated on registryProxyDeliberate, not the
	// resolved *registryProxy alone: the silent default-on case leaves
	// -sandbox-image legitimately empty here (run_ticket.go now fails that
	// entry closed with its own "no sandbox image configured" error), and
	// that's not a misconfiguration worth failing worker's own startup
	// over -- only an operator's own explicit choice (flag or session
	// config) is.
	if registryProxyDeliberate && *registryProxy && *sandboxImage == "" {
		return requestdriver.WorkerConfig{}, "", fmt.Errorf("-registry-proxy requires -sandbox-image")
	}
	defaultEgressCABundle(egressCABundle)
	if *egressCABundle != "" {
		if err := sandbox.ValidateEgressCABundle(*egressCABundle); err != nil {
			return requestdriver.WorkerConfig{}, "", fmt.Errorf("-egress-ca-bundle: %w", err)
		}
	}
	if *prPollInterval < time.Minute {
		return requestdriver.WorkerConfig{}, "", fmt.Errorf("-pr-poll-interval must be at least 1m, got %s", prPollInterval.String())
	}
	if *maxReviewRounds < 1 {
		return requestdriver.WorkerConfig{}, "", fmt.Errorf("-max-review-rounds must be at least 1, got %d", *maxReviewRounds)
	}
	if *reviewCorrectiveRounds < 0 {
		return requestdriver.WorkerConfig{}, "", fmt.Errorf("-review-corrective-rounds must not be negative, got %d", *reviewCorrectiveRounds)
	}
	if *hitlReminderInterval < time.Minute {
		return requestdriver.WorkerConfig{}, "", fmt.Errorf("-hitl-reminder-interval must be at least 1m, got %s", hitlReminderInterval.String())
	}
	if *advanceOn != requestdriver.AdvanceOnAccepted && *advanceOn != requestdriver.AdvanceOnPRApproved {
		return requestdriver.WorkerConfig{}, "", fmt.Errorf("-advance-on must be %q or %q, got %q", requestdriver.AdvanceOnAccepted, requestdriver.AdvanceOnPRApproved, *advanceOn)
	}
	// Rejected here, at startup, for the same fail-fast reasoning
	// -preflight-profile/-pr-closes-issue/-registry-proxy already get
	// above (see that comment): otherwise this would pass worker's own
	// startup check cleanly and only fail deep inside run_ticket.go, once
	// per drained entry. Mirrors build_app.py's own --conformity-policy
	// argparse `choices`.
	if *conformityPolicy != conformityPolicyRequired && *conformityPolicy != conformityPolicyAdvisory {
		return requestdriver.WorkerConfig{}, "", fmt.Errorf("-conformity-policy must be %q or %q, got %q", conformityPolicyRequired, conformityPolicyAdvisory, *conformityPolicy)
	}
	if !codereview.ValidPolicy(*codeReviewPolicy) {
		return requestdriver.WorkerConfig{}, "", fmt.Errorf("-code-review-policy must be one of off/advisory/required, got %q", *codeReviewPolicy)
	}

	// Resolved once, after every flag and config check above, so a bad flag
	// never waits on a Temporal start. cfg.temporalAddress is never empty.
	resolvedAddress, err := hostcontrol.ResolveTemporalAddress(dp, context.Background(), *temporalAddress, os.Stdout)
	if err != nil {
		return requestdriver.WorkerConfig{}, "", err
	}
	*temporalAddress = resolvedAddress
	ensureSandboxRuntime(dp, context.Background(), os.Stdout, settings.MeterImage)

	cfg := requestdriver.WorkerConfig{
		Sandboxes:                 dp.sandbox.runtime(),
		Resume:                    requestdriver.ResumeGate{Sandboxes: dp.sandbox.runtime()},
		MeterLedgerRoot:           meterLedgerRoot(),
		OpenPullRequest:           *openPullRequest,
		BuildAppScript:            *buildAppScript,
		BuildAppMaxAttempts:       *buildAppMaxAttempts,
		VerifyMaxAttempts:         *verifyMaxAttempts,
		SandboxImage:              *sandboxImage,
		RegistryProxy:             *registryProxy,
		RegistryProxyImage:        *registryProxyImage,
		EgressCABundle:            *egressCABundle,
		ComposeServices:           *composeServices,
		PrPollInterval:            *prPollInterval,
		PrIgnoreAuthors:           splitCommaList(*prIgnoreAuthors),
		PrTrustedAuthors:          splitCommaList(*prTrustedAuthors),
		MaxReviewRounds:           *maxReviewRounds,
		ReviewCorrectiveRounds:    *reviewCorrectiveRounds,
		HitlReminderInterval:      *hitlReminderInterval,
		AdvanceOn:                 *advanceOn,
		ConformityPolicy:          *conformityPolicy,
		ConformityPolicyExplicit:  conformityPolicyExplicit,
		CodeReviewPolicy:          *codeReviewPolicy,
		CodeReviewPolicyExplicit:  codeReviewPolicyExplicit,
		Settings:                  settings,
		DraftSpecScript:           *draftSpecScript,
		DraftSpecInterpreter:      "python3",
		SpecDraftTimeoutMinutes:   *specDraftTimeoutMinutes,
		PlanTicketsScript:         *planTicketsScript,
		PlanTicketsInterpreter:    "python3",
		PlanTicketsTimeoutMinutes: *planTicketsTimeoutMinutes,
		OracleDraftScript:         *qf.oracleDraftScript,
		OracleDraftInterpreter:    "python3",
		OracleDraftTimeoutMinutes: *qf.oracleDraftTimeoutMinutes,
		TemporalAddress:           *temporalAddress,
		SessionConfigPath:         sessionConfigPath,
	}

	if !*skipDoctor {
		if err := runWorkerDoctorPreflight(dp, cfg, *dataDir); err != nil {
			return requestdriver.WorkerConfig{}, "", err
		}
	}
	warnIfImagesStale(cfg.Settings)

	// Startup-time, not per drained entry -- cfg.openPullRequest/
	// cfg.settings.Release* are session-wide for this whole drain loop
	// (workerConfig's own doc comment). Called unconditionally, exactly
	// like the worker's later runMainWithReady calls below: the
	// one-time-logging state lives inside warnIfReleasePolicyCanNeverAllow
	// itself, gated on the warning's own condition, so calling it here AND
	// again from every drained entry's runMainWithReady still logs the
	// warning at most once, never zero and never twice (found via a real
	// /code-review pass, 2026-09-15, and refined further after a second
	// review round on PR #154 found the original external sync.Once
	// wrapper could itself suppress a later, warning-worthy call).
	warnIfReleasePolicyCanNeverAllow(cfg.OpenPullRequest, release.MergePolicy{
		RollbackPlan:    cfg.Settings.ReleaseRollbackPlan,
		MaxFilesChanged: cfg.Settings.ReleaseMaxFilesChanged,
		MaxInsertions:   cfg.Settings.ReleaseMaxInsertions,
	})

	return cfg, *dataDir, nil
}

// useWorkerGlobals installs cfg's process-wide values and returns the
// function that clears them. tier2SettingsOverride makes every build use
// cfg.settings rather than reloading the session config per build;
// requestAdvanceOn: see its own doc comment (request_driver.go) for why it
// is a package-level variable rather than a parameter threaded everywhere
// startNextTicketOrFinish is called.
func useWorkerGlobals(cfg *requestdriver.WorkerConfig) func() {
	tier2SettingsOverride = &cfg.Settings
	requestdriver.RequestAdvanceOn = cfg.AdvanceOn
	return func() {
		tier2SettingsOverride = nil
		requestdriver.RequestAdvanceOn = requestdriver.AdvanceOnAccepted
	}
}

// splitCommaList splits a comma-separated -pr-ignore-authors-style flag
// value into its trimmed, non-empty entries; "" returns nil rather than a
// one-element slice holding "".
func splitCommaList(s string) []string {
	if s == "" {
		return nil
	}
	var result []string
	for _, part := range strings.Split(s, ",") {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			result = append(result, trimmed)
		}
	}
	return result
}

// applySessionConfig loads the session config (configPath, or the first
// default path that exists), sets every Tier-1 flag this command still has
// that the command line did not (explicit flag > config file > flag
// default, same as run_ticket.go's own flags), and returns the fully
// resolved Tier-2 settings (sessionconfig.DefaultSettings overlaid with
// this same config file) for workerConfig.settings.
//
// No config file and no configuration on the command line means every
// queued entry would fail identically (no sandbox image, no model route),
// so that case is refused here with the paths looked for
// and how to write one, instead of starting a queue that fails every entry.
// applySessionConfig's third return, foundPath, is "" when no session
// config file was actually loaded (an execution-flags-only invocation --
// see workerExecutionFlags below) and the resolved path otherwise.
func applySessionConfig(flags *flag.FlagSet, configPath string) (sessionconfig.Settings, string, error) {
	set := map[string]bool{}
	flags.Visit(func(f *flag.Flag) { set[f.Name] = true })

	settings := sessionconfig.DefaultSettings()
	var (
		cfg       *sessionconfig.Config
		foundPath string
		found     bool
		err       error
	)
	if configPath != "" {
		configPath = sessionconfig.ResolveArg(configPath)
		cfg, err = sessionconfig.Load(configPath)
		found = err == nil
		foundPath = configPath
	} else {
		cfg, foundPath, found, err = sessionconfig.LoadDefault()
	}
	if err != nil {
		return settings, "", fmt.Errorf("session config: %w", err)
	}
	if !found {
		// Only a flag that actually configures execution counts:
		// -open-pull-request or -build-app-script alone would otherwise
		// slip past this refusal and fail every drained entry on the
		// missing sandbox/relay values (found by the PR #101 review).
		configured := false
		for name := range set {
			if workerExecutionFlags[name] {
				configured = true
			}
		}
		if configured {
			return settings, "", nil
		}
		return settings, "", fmt.Errorf("no session config found at %s and no sandbox flags given; write one with `factoryd init-config`, or pass -config <path>. Example:\n\n%s", strings.Join(sessionconfig.DefaultPaths(), " or "), sessionconfig.Example)
	}

	settings, err = cfg.ApplySettings(settings)
	if err != nil {
		return settings, "", fmt.Errorf("session config: %w", err)
	}
	if err := validateRoles(settings); err != nil {
		return settings, "", err
	}

	// Every Tier-1 flag this command still has: applied only when the
	// operator didn't already pass an explicit flag.
	strFlag := func(name string, v *string) error {
		if set[name] || v == nil {
			return nil
		}
		return flags.Set(name, *v)
	}
	boolFlag := func(name string, v *bool) error {
		if set[name] || v == nil {
			return nil
		}
		return flags.Set(name, strconv.FormatBool(*v))
	}
	intFlag := func(name string, v *int) error {
		if set[name] || v == nil {
			return nil
		}
		return flags.Set(name, strconv.Itoa(*v))
	}
	// prIgnoreAuthorsFlag: PRIgnoreAuthors is a YAML list in the config
	// file (see Config's own doc comment), not a string -- joined into the
	// comma-separated form -pr-ignore-authors takes before being set, the
	// same "config's own shape differs from the flag's" case
	// relayWorkerModelExtraJSONFlag handles just above.
	prIgnoreAuthorsFlag := func() error {
		if set["pr-ignore-authors"] || cfg.PRIgnoreAuthors == nil {
			return nil
		}
		return flags.Set("pr-ignore-authors", strings.Join(cfg.PRIgnoreAuthors, ","))
	}
	// prTrustedAuthorsFlag mirrors prIgnoreAuthorsFlag exactly, for the
	// same "config's own shape differs from the flag's" reason.
	prTrustedAuthorsFlag := func() error {
		if set["pr-trusted-authors"] || cfg.PRTrustedAuthors == nil {
			return nil
		}
		return flags.Set("pr-trusted-authors", strings.Join(cfg.PRTrustedAuthors, ","))
	}
	for name, err := range map[string]error{
		"data-dir":                 applyDataDirFlag(flags, set, cfg),
		"sandbox-image":            strFlag("sandbox-image", cfg.SandboxImage),
		"registry-proxy-image":     strFlag("registry-proxy-image", cfg.RegistryProxyImage),
		"egress-ca-bundle":         strFlag("egress-ca-bundle", cfg.EgressCABundle),
		"open-pull-request":        boolFlag("open-pull-request", cfg.OpenPullRequest),
		"registry-proxy":           boolFlag("registry-proxy", cfg.RegistryProxy),
		"compose-services":         boolFlag("compose-services", cfg.ComposeServices),
		"build-app-max-attempts":   intFlag("build-app-max-attempts", cfg.BuildAppMaxAttempts),
		"verify-max-attempts":      intFlag("verify-max-attempts", cfg.VerifyMaxAttempts),
		"pr-poll-interval":         strFlag("pr-poll-interval", cfg.PRPollInterval),
		"pr-ignore-authors":        prIgnoreAuthorsFlag(),
		"pr-trusted-authors":       prTrustedAuthorsFlag(),
		"max-review-rounds":        intFlag("max-review-rounds", cfg.MaxReviewRounds),
		"review-corrective-rounds": intFlag("review-corrective-rounds", cfg.ReviewCorrectiveRounds),
		"hitl-reminder-interval":   strFlag("hitl-reminder-interval", cfg.HITLReminderInterval),
		"advance-on":               strFlag("advance-on", cfg.AdvanceOn),
		"conformity-policy":        strFlag("conformity-policy", cfg.ConformityPolicy),
		"code-review-policy":       strFlag("code-review-policy", cfg.CodeReviewPolicy),
	} {
		if err != nil {
			return settings, "", fmt.Errorf("session config: %s: %w", name, err)
		}
	}

	// Logged for the same reason serve's own applySessionConfigDataDir
	// logs its resolved directory and source (found via adversarial
	// review of PR #111, should-fix 4): -data-dir is one flag among many
	// this function can silently set from the session config, and "why is
	// worker draining the wrong directory" is otherwise undiagnosable
	// from this command's own startup output alone.
	if dataDirFlag := flags.Lookup("data-dir"); dataDirFlag != nil {
		source := "-data-dir"
		if !set["data-dir"] {
			source = dataDirSource(cfg, foundPath)
		}
		log.Printf("data dir: %q (source: %s)", dataDirFlag.Value.String(), source)
	}
	return settings, foundPath, nil
}

// workerChecks is the doctor seam worker's startup preflight
// calls; a boundary method so tests can stub it the same way
// docker.initChecks is stubbed, instead of needing a live Docker daemon.
func (impl realDocker) workerChecks(ctx context.Context, in doctorInputs) []doctorCheck {
	return doctorChecksFor(ctx, in)
}

// runWorkerDoctorPreflight runs the same checks `factoryd doctor` would
// for cfg's values and refuses to drain when any fails: a 404ing
// relay upstream was only ever caught by doctor, which nothing ran before
// worker. Only the checks a queued run actually depends on: workspace
// mount visibility and Temporal are per-run or per-entry concerns
// worker has no value for, so those are the ones skipped here.
func runWorkerDoctorPreflight(dp *deps, cfg requestdriver.WorkerConfig, dataDir string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	fmt.Println("Running factoryd doctor first -- pass -skip-doctor to bypass:")
	// Registry-proxy values are checked only alongside the flag that makes
	// them meaningful, the same rule buildTicketRunArgs applies when
	// passing them through.
	//
	// sandboxTmpfsSize is daemon-wide, exactly like sandboxDocker just
	// above (both resolved once from cfg.settings and applied identically
	// to every drained entry via tier2SettingsOverride) -- not a per-entry
	// concern, so it belongs here the same way sandboxDocker already does.
	// Missing before this fix (found via Codex review of this same PR):
	// `factoryd doctor` itself always checks it (doctorMain populates this
	// same field from settings.SandboxTmpfsSize), but this preflight left
	// it empty, and doctorChecksFor skips an empty value's check entirely
	// -- so worker could report a clean preflight and start draining
	// even with a configured tmpfs size too small for a real build,
	// failing individual entries instead of refusing to start at all.
	// releaseMaxFilesChanged/releaseMaxInsertions/releaseRollbackPlan
	// mirror the sandboxTmpfsSize fix just above, for the same reason:
	// doctorChecksFor always appends doctorCheckReleasePolicy, and leaving
	// these three at doctorInputs' own zero value (0, 0, "") makes
	// releasePolicyCanNeverAllow always report "denies every PR
	// unconditionally" here regardless of cfg.settings' real, resolved
	// release policy -- the same false-alarm class as an unresolved
	// session-config value silently defaulting to its zero value.
	in := doctorInputs{
		sandboxDocker:          cfg.Settings.SandboxDocker,
		sandboxTmpfsSize:       cfg.Settings.SandboxTmpfsSize,
		sandboxImage:           cfg.SandboxImage,
		egressCABundle:         cfg.EgressCABundle,
		imageSourceRoot:        cfg.Settings.ImageSourceRoot,
		releaseMaxFilesChanged: cfg.Settings.ReleaseMaxFilesChanged,
		releaseMaxInsertions:   cfg.Settings.ReleaseMaxInsertions,
		releaseRollbackPlan:    cfg.Settings.ReleaseRollbackPlan,
		settings:               cfg.Settings,
	}
	// A queue whose builds call no model (see modelRouteNeeded) skips the
	// routes:/models: checks.
	in.noRelay = !modelRouteNeeded(cfg.Settings, cfg.BuildAppScript != "")
	if cfg.RegistryProxy {
		in.registryProxyImage = cfg.RegistryProxyImage
	}
	checks := dp.docker.workerChecks(ctx, in)
	// -data-dir mount visibility is appended only once every check above
	// has already passed, never folded into `in` above: doctorCheckDataDirMountVisibility
	// needs -data-dir to already exist to probe it (doctorCheckMountVisibilityFor's
	// own doc comment -- it must fail on a missing path, not create one),
	// but creating -data-dir here before knowing whether some unrelated
	// check above -- a bad -sandbox-docker, a 404ing relay upstream, an
	// undersized tmpfs -- is what's actually about to refuse this
	// invocation would leave -data-dir behind on disk even then. That's
	// exactly the side effect -workspace's own doctorCheckMountVisibility
	// refuses to cause ("this check must never create -workspace") and
	// run_ticket.go's sibling -data-dir check is careful to avoid too
	// (found via adversarial review of this same PR -- an earlier version
	// of this function MkdirAll'd -data-dir unconditionally, up front,
	// before any check result was known).
	// A MkdirAll/image-resolution failure here becomes a doctorCheck of
	// its own, appended and reported the same way as everything else
	// below, rather than an early return: an early return here would
	// skip runDoctorChecks entirely, hiding the ok/FAIL lines for every
	// check that already ran above (found via adversarial review of this
	// same PR -- every other failure path in this function prints that
	// full report first).
	if doctorChecksFailed(checks) == 0 {
		const name = "data dir writable (-data-dir)"
		if err := os.MkdirAll(dataDir, 0o750); err != nil {
			checks = append(checks, doctorCheck{Name: name, Err: fmt.Errorf("create -data-dir: %w", err)})
		} else {
			checks = append(checks, doctorCheckDataDirMountVisibility(ctx, in.sandboxDocker, in.sandboxImage, dataDir))
		}
	}
	if failed := runDoctorChecks(checks, os.Stdout); failed > 0 {
		return fmt.Errorf("refusing to drain: %d doctor check(s) failed (see fixes above, or rerun with -skip-doctor)", failed)
	}
	fmt.Println()
	return nil
}

// acquireWorkerLock takes an exclusive, non-blocking kernel lock
// (syscall.Flock, the same mechanism internal/workspace.DirectLock and
// run.WithLock already use, so a crashed worker cannot leave a stale
// lock behind) on dataDir's queue, and returns the release function
// the worker defers.
//
// Without this, a second worker against the same -data-dir -- an operator
// forgetting one is already backgrounded, the common way this happens --
// would drive the same requests concurrently and exceed max_parallel_jobs.
//
// Fails rather than waits, matching workspace.ErrBusy's own reasoning: an
// operator who started a second drainer should see that immediately, not
// have it sit silently behind the first one forever.
func acquireWorkerLock(dp *deps, dataDir string) (func(), error) {
	dir := filepath.Join(dataDir, "queue")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("create queue directory %q: %w", dir, err)
	}
	path := filepath.Join(dir, hostcontrol.WorkerLockFileName)
	contendedMsg := fmt.Sprintf("a factoryd worker is already driving %q%s: stop it before starting a second one", dataDir, drainLockHolderNote(dp, dataDir))
	return acquireExclusiveLock(path, contendedMsg)
}

// drainLockHolderNote names the pid in dataDir's heartbeat (" (pid N)"), or
// "" when it is unreadable: the daemon holding the drain lock wrote it.
func drainLockHolderNote(dp *deps, dataDir string) string {
	pid, fresh := hostcontrol.WorkerHeartbeatPID(dataDir, time.Now())
	if !fresh || !hostcontrol.AliveFactoryd(dp, pid) {
		return ""
	}
	return fmt.Sprintf(" (pid %d)", pid)
}

// acquireExclusiveLock takes an exclusive, non-blocking kernel lock
// (syscall.Flock) on path, creating the lock file if it does not already
// exist, and returns the release function the caller should defer. It
// fails fast rather than waiting -- if the lock is already held, it
// returns an error whose text is exactly contendedMsg (a caller-supplied,
// already-formatted description naming what's contended) rather than
// blocking until it becomes available, the same reasoning
// acquireWorkerLock's own doc comment above states: an operator who
// started a second concurrent invocation against the same resource should
// see that immediately, not have it sit silently behind the first one.
//
// Extracted from acquireWorkerLock (this file) so withIntakeLock
// (intake.go) can take the same fail-fast lock on a pilot dir without
// duplicating the flock boilerplate.
func acquireExclusiveLock(path string, contendedMsg string) (func(), error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock %q: %w", path, err)
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = file.Close()
		if errors.Is(err, syscall.EAGAIN) || errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, errors.New(contendedMsg)
		}
		return nil, fmt.Errorf("acquire lock %q: %w", path, err)
	}
	return func() {
		_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		_ = file.Close()
	}, nil
}

// workerExecutionFlags are the worker flags whose presence means the
// operator configured how entries execute -- the sandbox and
// registry-proxy surface -- as opposed to where the queue lives or what
// happens after acceptance.
var workerExecutionFlags = map[string]bool{
	"sandbox-image":        true,
	"registry-proxy":       true,
	"registry-proxy-image": true,
	"compose-services":     true,
}

// applyDataDirFlag sets -data-dir to the session config's data dir
// (Config.EffectiveDataDir, which every config has) unless the command line
// named one; a flag set without -data-dir takes nothing.
func applyDataDirFlag(flags *flag.FlagSet, set map[string]bool, cfg *sessionconfig.Config) error {
	if set["data-dir"] || flags.Lookup("data-dir") == nil {
		return nil
	}
	return flags.Set("data-dir", cfg.EffectiveDataDir())
}

// dataDirSource says where a data dir taken from the session config at path
// came from: its data_dir, or the default for a config that sets none.
func dataDirSource(cfg *sessionconfig.Config, path string) string {
	if cfg.DataDirIsDefault() {
		return fmt.Sprintf("the default for session config %s, which sets no data_dir", path)
	}
	return fmt.Sprintf("session config %s", path)
}
