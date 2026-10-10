// factoryd is the minimal Phase 1-2 slice of the agent factory plan: it
// supervises exactly one build_app.py invocation against one ticket in one
// workspace, records durable evidence, and runs the project's own
// canonical verification command as the only pass/fail signal — never the
// agent's own report. Its HTTP API observes those records and exposes
// authenticated start/override commands; no scheduler or UI exists yet.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"sync"
	"syscall"
	"time"

	"buildgate/internal/codereview"
	"buildgate/internal/conformity"
	"buildgate/internal/evidence"
	"buildgate/internal/harness"
	"buildgate/internal/notify"
	"buildgate/internal/request"
	"buildgate/internal/run"
)

// version identifies this build; set via -ldflags "-X main.version=...".
// "dev" otherwise, in which case `factoryd version` reports the git
// revision the Go toolchain embeds in a `go build`/`go install` from a
// checkout instead (see buildVersion). Reported by `factoryd version`
// only -- the embedded harness's own cache directory (harness.Ensure) is
// content-addressed, not version-addressed, so it doesn't depend on this.
var version = "dev"

// overrideTokenEnvironmentVariable names the environment variable holding
// the bearer token `serve`'s POST /runs/{id}/override endpoint requires —
// see api.WithOverrideToken's own doc comment for why that endpoint is
// disabled by default. An env var, not a flag, so it never appears in a
// process listing (e.g. `ps`) -- the same reasoning
// notify.DiscordWebhookURLsEnvironmentVariable's own doc comment gives.
// Its literal value is duplicated (not imported) in internal/runner's own
// subprocessEnvExclude — see that var's doc comment for why a
// build/verify subprocess must never inherit this credential even when
// it shares a process environment with `serve`.
const overrideTokenEnvironmentVariable = "FACTORYD_API_OVERRIDE_TOKEN"

const startTokenEnvironmentVariable = "FACTORYD_API_START_TOKEN"

// readTokenEnvironmentVariable names the environment variable holding the
// bearer credential GET /runs, GET /runs/{id}, GET /runs/{id}/events,
// GET /runs/{id}/diff, and GET /projects require. Unlike overrideToken/
// startToken, an empty value does not disable these routes -- they predate
// this token and are read-only, so leaving it empty preserves prior
// behavior for a deployment that already relies on unauthenticated read
// access from a trusted network. What actually changed the exposure here
// is -addr's own default now being loopback-only (see serveMain); this
// token exists for an operator who deliberately binds wider than that and
// wants the read surface gated too (found via the 2026-09-05 Opus review,
// S3 -- getRunDiff served every run's complete unified source diff,
// absolute host paths, and provider/model identity to any caller that
// could reach the port).
const readTokenEnvironmentVariable = "FACTORYD_API_READ_TOKEN"

// maxAgentEvidenceFileSize bounds loadAgentEvidence's own read of
// BUILD_EVIDENCE.json via evidence.ReadHostileFile. 10 MiB matches
// evidence.RetainFile's own maxRetainFileSize for the agent's build
// report file -- real evidence files are a few KB of structured JSON,
// with the same generous headroom reasoning as that constant's own doc
// comment.
const maxAgentEvidenceFileSize = 10 << 20

// resolveHarnessScript resolves the -build-app-script/-goal-pilot-script
// flag values every flag surface that takes one shares (run_ticket.go,
// daemon_cmd.go, serve_cmd.go, supervisor.go, intake.go): explicit wins
// if the caller passed one, otherwise it's the named script inside the
// embedded harness (agent/pi/scripts/, extracted on demand by
// harness.Ensure to a cache directory -- see that package's doc comment).
//
// Before 2026-09-09 these defaulted to a path under a checkout at
// $HOME/code/software-factory, so a go install'ed factoryd only worked
// for an operator who happened to have that checkout in that exact
// place. Embedding removes that dependency entirely; extraction failing
// (a read-only or unwritable cache directory, say) is reported here with
// the explicit-flag workaround rather than silently falling back to the
// old checkout path.
func resolveHarnessScript(explicit, scriptName string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	dir, err := harness.Ensure()
	if err != nil {
		return "", fmt.Errorf("resolve embedded %s: %w (pass -build-app-script/-goal-pilot-script explicitly to work around)", scriptName, err)
	}
	return filepath.Join(dir, scriptName), nil
}

func flagsWasVisited(flags *flag.FlagSet, name string) bool {
	found := false
	flags.Visit(func(f *flag.Flag) {
		if f.Name == name {
			found = true
		}
	})
	return found
}

func explicitValue(flags *flag.FlagSet, name, value string) string {
	if flagsWasVisited(flags, name) {
		return value
	}
	return ""
}

var apiStartMu sync.Mutex

func main() {
	dp := newDeps()
	err := realMain(dp)
	// Gives any DispatchExternal fan-out started during this run (accept,
	// quarantine, or halt notification) a bounded chance to actually reach
	// the network before the process exits, instead of silently dropping
	// it -- found via Codex review round 2 on PR #88: a bare background
	// goroutine can otherwise still be in flight when this process exits,
	// via log.Fatalf below or a normal return. This is factoryd's one
	// true top-level exit point regardless of which subcommand realMain
	// dispatched to, so a single call here covers all of them. Bounded so
	// a genuinely hung webhook still lets the process exit.
	notify.WaitForPendingDispatches(5 * time.Second)
	if err != nil {
		log.Fatalf("factoryd: %v", err)
	}
}

// loadSpecConformityVerdicts fills r.SpecConformityVerdicts from the
// retained CONFORMITY_EVIDENCE.json (run.Dir/CONFORMITY_EVIDENCE.json,
// already copied out of the untrusted workspace by evidence.RetainFile).
// Shared by the run setup and applyRunWorkflowResult so both
// record identical evidence. Best-effort and informational, exactly
// like loadAgentEvidence: a missing file is not an event (the run may never
// have reached the conformity phase), a malformed one is a warning, and
// neither ever changes the run's outcome.
func loadSpecConformityVerdicts(r *run.Run, dataDir, id string) {
	// Cleared first, so a re-finalization whose conformity phase produced no
	// evidence this time never keeps verdicts from an earlier attempt and
	// renders them as if they were current (found via review).
	r.SpecConformityVerdicts = nil
	r.SpecConformityStoppedBy = ""
	if !r.SpecConformityConfigured {
		return
	}
	path := filepath.Join(run.Dir(dataDir, id), "CONFORMITY_EVIDENCE.json")
	data, err := evidence.ReadHostileFile(path, maxAgentEvidenceFileSize)
	if err != nil {
		if !os.IsNotExist(err) {
			fmt.Printf("run %s: warning: could not read retained CONFORMITY_EVIDENCE.json: %v\n", id, err)
		}
		return
	}
	r.SpecConformityStoppedBy = codereview.EvidenceStoppedBy(data)
	verdicts, err := conformity.ParseVerdicts(data)
	if err != nil {
		fmt.Printf("run %s: warning: %v\n", id, err)
		return
	}
	r.SpecConformityVerdicts = verdicts
}

// loadCodeReview fills r.CodeReview from the retained CODE_REVIEW_EVIDENCE.json
// (cmd/factoryd's own code_review phase already retained it into the run
// dir via evidence.RetainFile) -- informational only, the same convention
// loadSpecConformityVerdicts follows: the code_review gate itself already
// judged pass/fail off the phase's own exit code, so a missing or
// malformed evidence file here never changes that outcome, only what the
// run record/PR body can say about it.
func loadCodeReview(r *run.Run, dataDir, id string) {
	// Cleared first, so a re-finalization whose code-review phase
	// produced no evidence this time never keeps a finding set from an
	// earlier attempt and renders it as if it were current (the same
	// guard loadSpecConformityVerdicts has).
	r.CodeReview = nil
	path := filepath.Join(run.Dir(dataDir, id), codereview.EvidenceFile)
	data, err := evidence.ReadHostileFile(path, maxAgentEvidenceFileSize)
	if err != nil {
		if !os.IsNotExist(err) {
			fmt.Printf("run %s: warning: could not read retained %s: %v\n", id, codereview.EvidenceFile, err)
		}
		return
	}
	result, err := codereview.ParseResult(data)
	if err != nil {
		fmt.Printf("run %s: warning: %v\n", id, err)
		return
	}
	r.CodeReview = &result
}

// loadOracleCoverage fills r.OracleCoveredCriteria from the operator's
// MANIFEST.json in r.ReferenceOracleDir, so the release evidence can say
// which criteria had a deterministic second check. Unlike the verdicts it
// depends only on the run record, so a reclaimed run -- which must not read
// workspace evidence at all -- can still call it. ReadHostileFile because
// the directory is operator-populated: a symlink, FIFO or oversized
// MANIFEST.json must not hang or exhaust this process.
func loadOracleCoverage(r *run.Run, id string) {
	// Cleared first for the same stale-state reason as loadSpecConformityVerdicts.
	r.OracleCoveredCriteria = nil
	if r.ReferenceOracleDir == "" {
		return
	}
	data, err := evidence.ReadHostileFile(filepath.Join(r.ReferenceOracleDir, "MANIFEST.json"), maxAgentEvidenceFileSize)
	if err != nil {
		if !os.IsNotExist(err) {
			fmt.Printf("run %s: warning: could not read oracle MANIFEST.json: %v\n", id, err)
		}
		return
	}
	covered, err := conformity.ParseOracleManifest(data)
	if err != nil {
		fmt.Printf("run %s: warning: %v\n", id, err)
		return
	}
	r.OracleCoveredCriteria = covered
}

// loadAgentEvidence reads the host's copy of the build's BUILD_EVIDENCE.json
// from the run's directory, if present, and attaches it to r.AgentEvidence. Shared by the run setup and
// runViaTemporal so the durable audit record — the agent's own reported
// provider/model/rounds/usage, distinct from the gate outcome that
// decides accept/quarantine — is recorded identically. A missing or unparseable file is logged as
// a warning, not an error: BUILD_EVIDENCE.json is best-effort agent
// self-reporting, never the acceptance oracle.
//
// Also retains run.AgentReportFileName — the agent's own free-text
// account of what it did, written into workspace by build_app.py
// alongside BUILD_EVIDENCE.json — into this run's own durable directory
// (run.Dir(dataDir, id)), the same directory diff.patch already lives in.
// Before this existed, that report was only ever printed as an
// informational path into the workspace (see runMain's own evidence-path
// log line, further down this file) and never retained anywhere durable:
// a shared, non-isolated checkout overwrites it with the next slice's own
// report, and an isolated-worktree rollback (internal/release.Rollback)
// deletes it outright, so a human later asking "on what basis did the
// reviewer call this clean?" had nothing durable to read even moments
// after the run finished (found via the 2026-09-05 Opus review, S6).
// Copied via evidence.RetainFile, not os.ReadFile/os.WriteFile here: this
// content is agent-authored prose, and TestNeverReadsAgentAuthoredEvidence
// statically guards that main.go itself never opens or reads it — only
// ever prints its path. Delegating the byte copy to a package that
// assigns no meaning to what it copies preserves that guarantee while
// still retaining the file. Best-effort and silently skipped, like
// BUILD_EVIDENCE.json above, when absent: a build_app.py version that
// predates this report, or a run that never reached the point of writing
// one, is not itself an error.
func loadAgentEvidence(r *run.Run, workspace, dataDir, id string) {
	// evidence.ReadHostileFile, not a bare os.ReadFile (found via this
	// session's own adversarial review, 2026-09-09): BUILD_EVIDENCE.json
	// sits in the exact same untrusted workspace the agent's build report
	// does, written by the same untrusted build, but its own read path
	// had none of RetainFile's symlink/FIFO/size protections until now --
	// see ReadHostileFile's own doc comment for the concrete exploit shape
	// this closes.
	//
	// Read from the run's own directory, not the workspace: the build step
	// moves the file there when the build returns (the workflow package's
	// takeRoundNotesOut), so no review works beside the per-round agent_notes
	// it holds. The copy is the same untrusted bytes.
	evidenceBytes, err := evidence.ReadHostileFile(filepath.Join(run.Dir(dataDir, id), evidence.BuildEvidenceFileName), maxAgentEvidenceFileSize)
	if err != nil {
		fmt.Printf("run %s: warning: could not read BUILD_EVIDENCE.json: %v\n", id, err)
	} else {
		// schema_version is decoded into this minimal envelope before the
		// real, strict decode below -- found via a real GitHub Codex App
		// review of this PR: decoding straight into run.AgentEvidence first
		// means a genuinely incompatible future schema (a field whose JSON
		// type no longer matches the Go struct's, not just an added/removed
		// field, which json.Unmarshal already tolerates) fails that decode
		// outright, before the version-skew switch below ever runs -- so
		// the operator sees only a generic "could not parse" with no
		// mention of schema_version at all, exactly the diagnosis
		// schema_version exists to provide. This envelope decode uses the
		// same bytes and, needing only one int field, succeeds independently
		// of whatever shape conflict breaks the full decode.
		var envelope struct {
			SchemaVersion int `json:"schema_version"`
		}
		_ = json.Unmarshal(evidenceBytes, &envelope)

		var agentEvidence run.AgentEvidence
		if err := json.Unmarshal(evidenceBytes, &agentEvidence); err != nil {
			if envelope.SchemaVersion != run.AgentEvidenceSchemaVersion && envelope.SchemaVersion != 0 {
				fmt.Printf("run %s: warning: BUILD_EVIDENCE.json schema_version %d, this factoryd understands %d, could not parse it into the expected shape: %v -- fields have likely moved or changed shape since\n", id, envelope.SchemaVersion, run.AgentEvidenceSchemaVersion, err)
			} else {
				fmt.Printf("run %s: warning: could not parse BUILD_EVIDENCE.json: %v\n", id, err)
			}
		} else {
			// Warned, not failed: AgentEvidence is best-effort by design (see
			// its own doc comment), so a schema a future build_app.py has
			// moved past should never block the run -- but silently
			// continuing to json.Unmarshal into a struct shape that no
			// longer matches is exactly how the parse_usage/agent_turn_errors
			// class of bug went unnoticed on every real run until someone
			// happened to inspect one closely. See
			// run.AgentEvidenceSchemaVersion's own doc comment.
			switch agentEvidence.SchemaVersion {
			case run.AgentEvidenceSchemaVersion:
			case 0:
				fmt.Printf("run %s: warning: BUILD_EVIDENCE.json has no schema_version field -- written by a build_app.py old enough to predate it; fields this factoryd expects may be silently missing\n", id)
			default:
				fmt.Printf("run %s: warning: BUILD_EVIDENCE.json schema_version %d, this factoryd understands %d -- fields may have moved or changed shape since\n", id, agentEvidence.SchemaVersion, run.AgentEvidenceSchemaVersion)
			}
			agentEvidence.CleanRoundFeedback()
			r.AgentEvidence = &agentEvidence
		}
	}

	src := filepath.Join(workspace, run.AgentReportFileName)
	dst := filepath.Join(run.Dir(dataDir, id), run.AgentReportFileName)
	if err := evidence.RetainFile(src, dst); err != nil {
		if !os.IsNotExist(err) {
			fmt.Printf("run %s: warning: could not retain %s: %v\n", id, run.AgentReportFileName, err)
		}
	}
	// Each failed round's whole output, which the round's failure_log
	// names: copied like the report, never read here.
	if _, err := evidence.RetainRoundLogs(workspace, filepath.Join(run.Dir(dataDir, id), evidence.RoundLogsDirName)); err != nil {
		fmt.Printf("run %s: warning: could not retain every round log: %v\n", id, err)
	}
}

// attachHarnessEval sets r.HarnessEval from the harness its originating
// request selected (see request.Request.Harness), falling back to
// findOwningRequest for a run recorded before r.RequestID itself existed.
// Best-effort like loadAgentEvidence above: a run with no owning request,
// or that recorded no execution attempt (so no resolved harness), simply
// leaves r.HarnessEval nil (see run.BuildHarnessEval's own doc comment).
// Must be called after r.State and r.AgentEvidence are already final —
// same ordering requirement as loadAgentEvidence, since HarnessEval
// summarizes both.
func attachHarnessEval(r *run.Run, dataDir, id string) {
	requestID := r.RequestID
	if requestID == "" {
		requestID = findOwningRequest(dataDir, id)
	}
	if requestID == "" {
		return
	}
	if _, err := request.Load(dataDir, requestID); err != nil {
		fmt.Printf("run %s: warning: could not load owning request %s for harness eval: %v\n", id, requestID, err)
		return
	}
	r.HarnessEval = run.BuildHarnessEval(r, executionHarnessOf(r))
}

// executionHarnessOf is the harness the run's execution role resolved to: the
// last execution attempt's recorded Attempt.Harness, "" when no execution
// attempt recorded one.
func executionHarnessOf(r *run.Run) string {
	for i := len(r.Attempts) - 1; i >= 0; i-- {
		if a := r.Attempts[i]; a.Role == run.AttemptRoleExecution && a.Harness != "" {
			return a.Harness
		}
	}
	return ""
}

// refuseAPITokensInEnvironment fails loudly if a control-plane bearer token
// is set in this process's own environment. Called by every code path that
// goes on to execute
// untrusted build_app.py/canonical-verify subprocesses — the plain
// run-invocation path in realMain and `factoryd daemon`'s daemonMain
// alike (found via review: daemon mode is exactly as exposed as a plain
// run invocation once it starts servicing real Activities, and the first
// version of this fix only guarded the latter) — never `serve` or
// `override`, where the token is expected.
//
// Found via review: internal/runner's own subprocessEnv() strips this
// var from a launched subprocess's inherited environment, but that alone
// doesn't isolate the credential from an untrusted subprocess determined
// to read it — same-UID process-inspection (e.g. /proc/<this-pid>/environ
// on Linux) can recover it directly from *this* process's own
// environment regardless of what env the child itself was launched with.
// There is no code-level fix for that once the credential is already
// here: it must simply never be set in the environment of a process that
// executes untrusted subprocesses, only in `serve`'s own. Failing loudly
// and refusing to run at all, rather than silently proceeding with it
// present, is what turns that operational misconfiguration into
// something an operator actually notices instead of a latent
// vulnerability.
func refuseAPITokensInEnvironment() error {
	for _, name := range []string{overrideTokenEnvironmentVariable, startTokenEnvironmentVariable, readTokenEnvironmentVariable} {
		if os.Getenv(name) != "" {
			return fmt.Errorf("%s must not be set here — it belongs only to `factoryd serve`'s own environment; a build/verify subprocess launched from a process that also holds it can read this process's own environment directly (e.g. via /proc/<pid>/environ), regardless of what env that subprocess is itself launched with", name)
		}
	}
	return nil
}

// versionMain implements `factoryd version`: print the version this
// binary was built with (see the version var's own doc comment).
func versionMain() error {
	var settings []debug.BuildSetting
	if info, ok := debug.ReadBuildInfo(); ok {
		settings = info.Settings
	}
	fmt.Println("factoryd version", buildVersion(version, settings))
	return nil
}

// buildVersion returns stamped unless it is "dev", in which case it falls
// back to the embedded vcs.revision (12 hex digits, "-dirty" when the
// checkout had uncommitted changes). `go run` embeds no revision, so that
// still reports "dev".
func buildVersion(stamped string, settings []debug.BuildSetting) string {
	if stamped != "dev" {
		return stamped
	}
	var revision string
	var dirty bool
	for _, s := range settings {
		switch s.Key {
		case "vcs.revision":
			revision = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	if revision == "" {
		return stamped
	}
	if len(revision) > 12 {
		revision = revision[:12]
	}
	if dirty {
		revision += "-dirty"
	}
	return revision
}

// topLevelHelpTokens are the ways an operator asks for top-level help
// rather than naming a real subcommand or starting a bare run --
// isTopLevelHelpRequest below is the only place these are checked.
var topLevelHelpTokens = map[string]bool{"-h": true, "-help": true, "--help": true, "help": true}

// isTopLevelHelpRequest reports whether args (os.Args[1:]) asks for
// top-level help: no subcommand at all (bare `factoryd`), or the first
// token is one of topLevelHelpTokens. Deliberately only the FIRST token --
// `factoryd -ticket t -workspace w -spec s -h` still falls through to the
// bare-run path below and gets `factoryd <run>`'s own full flag dump from
// -h being parsed by that FlagSet as always (found while writing this:
// checking args[1:] for the token anywhere would swallow that too), so
// the full reference stays one flag away for an operator who already
// started typing a real invocation, while a bare `factoryd`/`factoryd -h`
// gets the short, actionable command list instead of a several-hundred-
// line flag dump (previously the only thing either printed, exiting 1
// with "flag: help requested" logged as an error -- not the 0 a plain
// help request should exit with).
func isTopLevelHelpRequest(args []string) bool {
	return len(args) == 0 || topLevelHelpTokens[args[0]]
}

// printTopLevelHelp is what a bare `factoryd` or `factoryd -h`/`-help`/
// `--help`/`help` prints: the handful of commands an operator actually
// reaches for, not every subcommand this binary has (`factoryd <cmd> -h`
// -- or USAGE.md/USAGE_REFERENCE.md -- covers the rest).
func printTopLevelHelp(out io.Writer) {
	fmt.Fprintln(out, "factoryd -- an unattended build/verify agent factory (merge and deploy stay human-gated)")
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Common commands:")
	fmt.Fprintln(out, `  factoryd quickstart <repo-path> "<task>"   one-command onboarding: configure, build, watch (start here)`)
	fmt.Fprintln(out, "  factoryd [flags]                            build one ticket directly (-h for its full flag reference)")
	fmt.Fprintln(out, "  factoryd submit / worker                    queue tickets across repos and build them unattended")
	fmt.Fprintln(out, "  factoryd serve                              console + API, for watching and approving runs")
	fmt.Fprintln(out, "  factoryd status <run-id>                    inspect one run's state")
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Also: doctor (check setup), approve / reject / retry / cancel (act on a request), stop (stop what quickstart started), logs, inbox, stats (is it getting better).")
	fmt.Fprintln(out)
	fmt.Fprintln(out, "See USAGE.md for the full walkthrough, or `factoryd <command> -h` for that command's own flags.")
}

func realMain(dp *deps) error {
	// Set once, here, before any subcommand can persist a run record --
	// run.Persist (internal/run/run.go) stamps this package-level value
	// onto every run it saves, so a run always shows which build last
	// wrote it (M4-K1).
	var settings []debug.BuildSetting
	if info, ok := debug.ReadBuildInfo(); ok {
		settings = info.Settings
	}
	run.FactorydVersion = buildVersion(version, settings)
	if isTopLevelHelpRequest(os.Args[1:]) {
		printTopLevelHelp(os.Stdout)
		return nil
	}
	if handler, ok := subcommands()[os.Args[1]]; ok {
		return handler(dp, os.Args[2:])
	}

	return runMain(dp, os.Args[1:])
}

// runMain is the existing bounded supervisor entry point, factored so the
// authenticated API can start the exact same path in-process.
func runMain(dp *deps, args []string) error {
	// runMain is the top-level CLI entry point: this process's only job is
	// running this one invocation to completion, so it owns SIGINT/SIGTERM
	// for its own lifetime. runMainWithReady is also called embedded inside
	// the long-lived API server (apiStartStarter), where installing a
	// competing signal handler here would be wrong — see runMainWithReady's
	// doc comment.
	signalCtx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()
	return runMainWithReady(dp, signalCtx, args, nil)
}

// subcommand is the one signature every subcommand is dispatched through:
// dp is the process's dependencies, args is what follows the subcommand's
// name on the command line.
type subcommand func(dp *deps, args []string) error

// withoutDeps adapts a subcommand that takes only its arguments.
func withoutDeps(run func(args []string) error) subcommand {
	return func(_ *deps, args []string) error { return run(args) }
}

// subcommands is the dispatch table realMain looks its first argument up in:
// one line per subcommand. A first argument that is not a key here is a
// single-ticket run (runMain). `build-ca-bundle`, `image-inputs-hash`,
// `stage-agent-tests`, `project-image-args` and `image-reuse` are the hidden
// ones only the Makefile uses (through `go run ./cmd/factoryd`).
func subcommands() map[string]subcommand {
	return map[string]subcommand{
		"override":          overrideMain,
		"serve":             serveMain,
		"daemon":            daemonMain,
		"supervise":         superviseMain,
		"reset-stop-line":   withoutDeps(resetStopLineMain),
		"override-rate":     withoutDeps(overrideRateMain),
		"check-project":     withoutDeps(checkProjectMain),
		"check-ticket":      withoutDeps(checkTicketMain),
		"ticket-template":   withoutDeps(ticketTemplateMain),
		"init":              initMain,
		"onboard":           onboardMain,
		"doctor":            doctorMain,
		"intake":            withoutDeps(intakeMain),
		"kill-switch":       withoutDeps(killSwitchMain),
		"reconcile":         withoutDeps(reconcileMain),
		"status":            withoutDeps(statusMain),
		"inbox":             withoutDeps(inboxMain),
		"stop":              stopMain,
		"uninstall":         uninstallMain,
		"upgrade":           upgradeMain,
		"use":               useMain,
		"watch":             withoutDeps(watchMain),
		"logs":              withoutDeps(logsMain),
		"cost":              withoutDeps(costMain),
		"submit":            submitMain,
		"approve":           approveMain,
		"reject":            rejectMain,
		"cancel":            cancelMain,
		"worker":            workerMain,
		"retry":             retryMain,
		"resume":            resumeMain,
		"amend-scope":       amendScopeMain,
		"init-config":       withoutDeps(initConfigMain),
		"install-service":   installServiceMain,
		"uninstall-service": uninstallServiceMain,
		"quickstart":        quickstartMain,
		"console":           consoleMain,
		"install-skill":     withoutDeps(installSkillMain),
		"configure-images":  withoutDeps(configureImagesMain),
		"build-ca-bundle": func(dp *deps, args []string) error {
			return buildCABundleMain(dp, args, os.Stdout, os.Stderr)
		},
		"image-inputs-hash": withoutDeps(imageInputsHashMain),
		"restart":           restartMain,
		"stage-agent-tests": withoutDeps(stageAgentTestsMain),
		"project-image-args": func(dp *deps, args []string) error {
			return projectImageArgsMain(dp, args, os.Stdout, os.Stderr)
		},
		"image-reuse": withoutDeps(imageReuseMain),
		"setup":       setupMain,
		"mcp": func(dp *deps, args []string) error {
			return mcpMain(dp, os.Stdout, args)
		},
		"memory": memoryMain,
		"stats":  withoutDeps(statsMain),
		"version": func(*deps, []string) error {
			return versionMain()
		},
	}
}
