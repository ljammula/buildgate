package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/sdk/client"

	"buildgate/internal/harness"
	"buildgate/internal/hostcontrol"
	"buildgate/internal/meter"
	"buildgate/internal/modelrole"
	"buildgate/internal/prices"
	"buildgate/internal/release"
	"buildgate/internal/request"
	"buildgate/internal/sandbox"
	"buildgate/internal/sessionconfig"
	"buildgate/internal/spinner"
	"buildgate/internal/workflow"
)

// doctorCheck is one factoryd doctor probe's outcome: a nil Err means
// pass; a non-nil Err carries a human-readable diagnosis, and Fix (when
// set) names the concrete action that closes it -- the same "name the
// real cause and the escape valve" discipline
// verifyWorkDirMountVisibility's own fail-closed diagnostic already
// follows for exactly one of these checks (mount visibility), generalized
// here across every setup mistake this session's own live-validation runs
// actually hit before this command existed.
type doctorCheck struct {
	Name string
	Err  error
	Fix  string
	// Use, when set, is the flag value the operator should now configure
	// -- printed under the check line as "use: ..." after doctor -fix
	// built an image and learned its digest-pinned reference.
	Use string
	// Advisory mirrors ProjectCheckResult.Advisory (project_check.go): a
	// non-nil Err is still surfaced (as "warn", not "FAIL"), but never
	// counted in runDoctorChecks's own failed total or overall exit code
	// -- for a check that is inherently a best-guess (doctorCheckMonorepoModuleRoot)
	// rather than a verifiable pass/fail, where guessing wrong would be
	// worse than staying silent, but silence entirely would lose the hint.
	Advisory bool
	// Detail, when set, is printed after an ok check's name -- a passing
	// check that has something worth naming (which harness each role runs).
	Detail string
}

// doctorMain is a single command that checks the
// things this session's own live runs each discovered the hard way --
// registry login, a route's own upstream path-
// composition bug and the missing-contextWindow bug (both found and
// fixed live, 2026-09-08, and documented at doctorCheckRelayUpstreamPathComposition/
// doctorCheckContextWindowConfigured's own doc comments),
// tmpfs size, mount visibility, and Temporal reachability -- and prints
// the fix instead of leaving an operator to rediscover each one from a
// confusing downstream failure. Also checks (advisory, see
// doctorCheck.Advisory) whether -workspace's own build manifest
// (go.mod/package.json/pyproject.toml/Makefile) lives one directory down
// rather than at -workspace's own root -- the "monorepo module root != repo
// root" gotcha USAGE.md's own gotcha table already documents -- and
// (fail-closed) whether -data-dir resolves inside -workspace, the same
// guard `factoryd <run>` itself fails closed on, caught here before a
// real run instead of after. Every flag here mirrors the identically-
// named flag on `factoryd <run>`/`factoryd serve`, so the values an
// operator is about to use for a real run are exactly what gets checked.
// Each check that has nothing to check against (an unset
// -temporal-address or -workspace) is skipped entirely rather than
// reported as a pass, so the summary count reflects only what was
// actually verified.
// newDoctorFlags builds `factoryd doctor`'s FlagSet in isolation from
// parsing, so USAGE.md's doc-vs-flag drift test
// (TestUSAGEDocFlagsExistOnSubcommand) can enumerate its real flags without
// executing the command.
func newDoctorFlags() (flags *flag.FlagSet, sandboxImage *string, registryProxy *bool, registryProxyImage *string, composeServices, fix *bool, repoRoot, workspace, dataDir, temporalAddress, egressCABundle, lintCommand, securityCommand, unitTestCommand, integrationTestCommand, referenceOracleCommand, referenceOracleDir *string, hitlReminderInterval *time.Duration, configPath *string) {
	flags = flag.NewFlagSet("doctor", flag.ContinueOnError)
	// configPath resolves every session-config-derived value below
	// (settings, -hitl-reminder-interval, -data-dir): `doctor` had no
	// -config flag at all before this fix, so an operator running
	// `factoryd quickstart -config X` had no way to make `factoryd doctor`
	// check that same file; it could only ever check the default-path
	// search's own config, the same class of bug `serve` had.
	configPath = flags.String("config", "", "session config path; empty searches the first of "+strings.Join(sessionconfig.DefaultPaths(), ", ")+" that exists (the same search `factoryd worker`/`quickstart` use)")
	sandboxImage = flags.String("sandbox-image", "", "sandbox image to confirm is present locally; empty uses the session config's sandbox_image (if any) -- there is no built-in default")
	registryProxy = flags.Bool("registry-proxy", true, "check the registry proxy image too; on by default, mirroring -registry-proxy's own default-on resolution for the default, model-backed build_app.py on a real run (found live, P2: doctor still defaulting this off let a bare `factoryd doctor` pass while a bare run would necessarily pull and start an unchecked registry-proxy image) -- pass -registry-proxy=false to skip this check the same way an explicit -registry-proxy=false does on a real run")
	registryProxyImage = flags.String("registry-proxy-image", "", "registry proxy image to confirm is present locally; only checked with -registry-proxy or when named explicitly. Empty uses the session config's registry_proxy_image (if any) -- there is no built-in default; still empty skips this check")
	composeServices = flags.Bool("compose-services", true, "check the docker compose v2 CLI (>= 2.20) is available too; on by default, mirroring -registry-proxy's own default-on resolution -- pass -compose-services=false to skip this check the same way an explicit -compose-services=false does on a real run")
	fix = flags.Bool("fix", false, "build an absent sandbox/registry-proxy image locally via its Makefile target (make sandbox-image/registry-proxy-image) and print the digest-pinned reference to configure; also prints the rm/mv for a stale factoryd shadowing this one on $PATH, applying the rename only with FACTORYD_DOCTOR_APPLY_PATH_FIX=1")
	repoRoot = flags.String("repo-root", "", "this repository's checkout, whose Makefile -fix runs; defaults to $FACTORYD_REPO_ROOT, then the running binary's parent directory when that holds the Makefile")
	workspace = flags.String("workspace", "", "a real directory to probe for the colima/Docker-backend mount-visibility trap (internal/sandbox.verifyWorkDirMountVisibility's own check, run standalone here); empty skips this check")
	dataDir = flags.String("data-dir", "data", "directory for durable run records and logs, checked against -workspace for factoryd <run>'s own -data-dir/-workspace containment guard; see that flag's own doc comment")
	temporalAddress = flags.String("temporal-address", "", "Temporal server address to confirm is reachable; empty skips this check")
	egressCABundle = flags.String("egress-ca-bundle", "", "PEM file on this host to confirm exists and parses, and to mount and trust inside the route-upstream reachability probe below; see factoryd <run>'s own flag of the same name")
	lintCommand = flags.String("lint-command", "", "lint gate command (see factoryd <run>'s own -lint-command) to confirm its leading executable exists in the sandbox image; empty skips this check")
	securityCommand = flags.String("security-command", "", "security-audit gate command (see factoryd <run>'s own -security-command) to confirm its leading executable exists in the sandbox image; empty skips this check")
	unitTestCommand = flags.String("unit-test-command", "", "unit-test gate command (see factoryd <run>'s own -unit-test-command) to confirm its leading executable exists in the sandbox image; empty skips this check")
	integrationTestCommand = flags.String("integration-test-command", "", "integration-test gate command (see factoryd <run>'s own -integration-test-command) to confirm its leading executable exists in the sandbox image; empty skips this check")
	referenceOracleCommand = flags.String("reference-oracle-command", "", "reference-oracle gate command (see factoryd <run>'s own -reference-oracle-command) to confirm its leading executable exists in the sandbox image; empty skips this check")
	referenceOracleDir = flags.String("reference-oracle-dir", "", "see factoryd <run>'s own -reference-oracle-dir; confirms this host directory is actually reachable inside a container by the configured Docker backend, the same mount-visibility probe -workspace itself gets -- found live (2026-09-15): a host path outside colima's default $HOME-only shared mount silently produces an EMPTY mount inside the container rather than an error, so the reference_oracle gate would then find no oracle content to check against at all")
	hitlReminderInterval = flags.Duration("hitl-reminder-interval", 15*time.Minute, "how often `factoryd worker` re-reminds a request waiting in spec_review, oracle_review, plan_review or resume_review; see that command's own flag of the same name. Printed as informational only -- nothing here is actually checked against it")
	plainFlagUsage(flags)
	return
}

func doctorMain(dp *deps, args []string) error {
	// Unconditional, first line of output: so a pasted `doctor` transcript
	// from a confused engineer is self-diagnosing about which binary they
	// actually ran, before anything else below (including the PATH-
	// shadowing check further down) has a chance to fail.
	if selfPath, err := os.Executable(); err == nil {
		fmt.Printf("factoryd doctor: running %s (%s)\n\n", selfPath, factoryVersionOf(selfPath))
	}

	flags, sandboxImage, registryProxy, registryProxyImage, composeServices, fix, repoRoot, workspace, dataDir, temporalAddress, egressCABundle, lintCommand, securityCommand, unitTestCommand, integrationTestCommand, referenceOracleCommand, referenceOracleDir, hitlReminderInterval, configPath := newDoctorFlags()
	// Registered directly on flags rather than threaded through
	// newDoctorFlags' own already-long return tuple -- see
	// doctorRegisterNotifyTestFlag's own doc comment (cmd/factoryd/
	// doctor_notify_test.go).
	notifyTest := doctorRegisterNotifyTestFlag(flags)
	yes := doctorRegisterYesFlag(flags)
	listModels := doctorRegisterListModelsFlag(flags)
	targetRepo := doctorRegisterTargetRepoFlag(flags)
	plainFlagUsage(flags)
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *notifyTest {
		return doctorNotifyTestMain(*dataDir)
	}

	registryProxyImageExplicit := false
	registryProxyExplicit := false
	composeServicesExplicit := false
	hitlReminderIntervalExplicit := false
	sandboxImageExplicit := false
	dataDirExplicit := false
	flags.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "data-dir":
			dataDirExplicit = true
		case "sandbox-image":
			sandboxImageExplicit = true
		case "registry-proxy-image":
			registryProxyImageExplicit = true
		case "registry-proxy":
			registryProxyExplicit = true
		case "compose-services":
			composeServicesExplicit = true
		case "hitl-reminder-interval":
			hitlReminderIntervalExplicit = true
		}
	})

	// sandbox-docker and sandbox-tmpfs-size are config-file only as of
	// flags-consolidate (2026-09-10; see sessionconfig.Settings): resolved
	// here from the same session config a real run would use, so `factoryd
	// doctor` checks the Docker executable and tmpfs size an operator has
	// actually configured, not always the hard default.
	// loadSettingsForConfig(*configPath), not resolveSettings(): the latter
	// always searches the default config paths (via loadDefaultSettings),
	// ignoring -config entirely -- the same class of bug fixed on `serve`.
	settings, err := loadSettingsForConfig(*configPath)
	if err != nil {
		return err
	}
	if !sandboxImageExplicit && settings.SandboxImage != "" {
		*sandboxImage = settings.SandboxImage
	}
	// Mirrors run_ticket.go's own -registry-proxy resolution (see that
	// flag's own doc comment): an explicit CLI value always wins; failing
	// that, an explicit registry_proxy session-config value wins; failing
	// that, this flag's own true default stands -- doctor has no
	// -build-app-script concept of its own to gate on, so it simply
	// assumes the common case (the default, model-backed build_app.py),
	// rather than the stale "off by default" this replaced.
	if !registryProxyExplicit && settings.RegistryProxyConfigured {
		*registryProxy = settings.RegistryProxy
	}
	if !*registryProxy && !registryProxyImageExplicit {
		*registryProxyImage = ""
	}
	// Mirrors -registry-proxy's own three-tier resolution immediately
	// above: an explicit CLI value always wins; failing that, an explicit
	// compose_services session-config value wins; failing that, this
	// flag's own true default stands.
	if !composeServicesExplicit && settings.ComposeServicesConfigured {
		*composeServices = settings.ComposeServices
	}

	// hitl-reminder-interval is not part of sessionconfig.Settings (it is
	// a per-invocation Tier-1 value on worker, mirroring
	// -pr-poll-interval -- see sessionconfig.Config's own doc comment on
	// HITLReminderInterval), so it is not covered by resolveSettings
	// above; resolved here the same way applySessionConfig resolves it
	// for worker itself, so `factoryd doctor`'s own report reflects
	// exactly the value a real `worker` invocation would use.
	if !hitlReminderIntervalExplicit {
		if cfg, _, found, err := loadConfigForPath(*configPath); err == nil && found && cfg.HITLReminderInterval != nil {
			if d, err := time.ParseDuration(*cfg.HITLReminderInterval); err == nil {
				*hitlReminderInterval = d
			}
		}
	}
	fmt.Printf("hitl reminder interval: %s\n\n", hitlReminderInterval.String())
	// Route visibility (2026-09-25): see doctorSubscriptionLoginsLine's own
	// doc comment.
	fmt.Println(doctorSubscriptionLoginsLine())

	*dataDir = resolveConfiguredDataDir(dataDirExplicit, *dataDir, *configPath)

	// resolvedConfigPath is the session config actually in effect --
	// -config verbatim (absolute'd) when given, else whichever
	// sessionconfig.DefaultPaths() entry LoadDefault found, else "" (no
	// config exists yet, so the ordinary `make install` hint applies) --
	// used only by doctorMissingImageFixHint below.
	resolvedConfigPath := ""
	if *configPath != "" {
		if abs, err := filepath.Abs(sessionconfig.ResolveArg(*configPath)); err == nil {
			resolvedConfigPath = abs
		}
	} else if _, foundPath, found, _ := sessionconfig.LoadDefault(); found {
		if abs, err := filepath.Abs(foundPath); err == nil {
			resolvedConfigPath = abs
		}
	}

	in := doctorInputs{
		sandboxDocker:          settings.SandboxDocker,
		sandboxImage:           *sandboxImage,
		meterImage:             settings.MeterImage,
		registryProxyImage:     *registryProxyImage,
		configPath:             resolvedConfigPath,
		imageSourceRoot:        settings.ImageSourceRoot,
		sandboxTmpfsSize:       settings.SandboxTmpfsSize,
		workspace:              *workspace,
		dataDir:                *dataDir,
		temporalAddress:        *temporalAddress,
		egressCABundle:         *egressCABundle,
		lintCommand:            *lintCommand,
		securityCommand:        *securityCommand,
		unitTestCommand:        *unitTestCommand,
		integrationTestCommand: *integrationTestCommand,
		referenceOracleCommand: *referenceOracleCommand,
		referenceOracleDir:     *referenceOracleDir,
		composeServices:        *composeServices,
		targetRepo:             *targetRepo,
		releaseMaxFilesChanged: settings.ReleaseMaxFilesChanged,
		releaseMaxInsertions:   settings.ReleaseMaxInsertions,
		releaseRollbackPlan:    settings.ReleaseRollbackPlan,
		settings:               settings,
		// The repo a request will clone and run version control against is
		// never checked by -workspace unless the operator remembers to pass it.
		checkRequestSourceRepos: true,
	}
	if *listModels {
		listCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		return doctorListModels(listCtx, os.Stdout, in)
	}
	// With -fix the Temporal check can start Docker itself (a Colima VM that
	// `stop -all` stopped), so it runs before the checks that need Docker.
	// Its row keeps its place at the end.
	var fixedTemporal *doctorCheck
	if *fix && in.temporalAddress == "" {
		check := doctorCheckTemporal(dp, context.Background(), true, os.Stdout)
		fixedTemporal = &check
	}
	// The checks probe Docker, images, routes and repos, some for up to 30 s
	// each: on a terminal show that doctor is working. Not with -fix, whose
	// image builds stream their own output.
	var sp *spinner.Spinner
	if !*fix {
		sp = newTTYSpinner(os.Stdout)
	}
	if sp != nil {
		sp.Start("running doctor checks (Docker, images, routes, repo)")
	}
	checks, err := doctorRunChecks(dp, in, *fix, *repoRoot)
	if sp != nil {
		sp.Stop("")
	}
	if err != nil {
		return err
	}
	switch {
	case fixedTemporal != nil:
		checks = append(checks, *fixedTemporal)
	case in.temporalAddress == "":
		// An explicit -temporal-address already has its own reachability check.
		checks = append(checks, doctorCheckTemporal(dp, context.Background(), false, os.Stdout))
	}
	checks = append(checks, doctorOpenShellChecks(dp, context.Background(), in, *fix, os.Stdout)...)
	// -fix also offers to fix the first-run blockers doctorFixAbsentImages
	// (inside doctorRunChecks above) doesn't cover -- see doctorApplyFixes'
	// own doc comment. Runs after checks are collected (so it can see, e.g.,
	// whether -data-dir's own mount-visibility check actually failed) but
	// before the pass/fail summary below, so an applied fix is reported
	// alongside the checks it addresses rather than as an unrelated,
	// separate step.
	if *fix {
		// doctorApplyBuiltImageRefs recovers whatever doctorFixAbsentImages
		// (run once already, inside the doctorRunChecks call above) built
		// and substituted -- that substitution lives only in
		// doctorRunChecks' own local `in` copy (built, in :=
		// doctorFixAbsentImages(in, repoRoot)), never returned to this
		// caller, so without this `in` here still names the unbuilt
		// canonical ref.
		in = doctorApplyBuiltImageRefs(in, checks)
		doctorApplyFixes(checks, *workspace, *dataDir, dataDirExplicit, *yes, os.Stdin, os.Stdout, *configPath)
		// -target-repo's compose images under a registry the operator has
		// not allowed: named, and added to the config only on a yes.
		checks = doctorOfferComposeRegistries(checks, in, *configPath, *yes, os.Stdin, os.Stdout)
		// Re-evaluate rather than trust the checks slice collected above:
		// a fix doctorApplyFixes just applied (e.g. release_* defaults
		// written to the config) can invalidate a check already collected,
		// so the same run previously kept printing the warning it had just
		// fixed. doctorRefreshInputsAfterFix re-resolves the config-derived
		// fields doctorApplyFixes' own fixes can have just changed on disk
		// (release policy, -data-dir).
		//
		// Only the checks those specific fixes can have changed are
		// re-evaluated below, in place (doctorReplaceCheck) -- NOT a full
		// re-run of doctorRunChecks: an adversarial review of Phase A found
		// that a full re-run would also re-run every Docker/
		// image-pull probe a second time (each can block up to
		// doctorImagePullTimeout) and drop a failed build's own error line
		// entirely (doctorFixAbsentImages, folded into doctorRunChecks only
		// when fix is true, must never run twice).
		oldDataDir := in.dataDir
		in, *dataDir, err = doctorRefreshInputsAfterFix(in, dataDirExplicit, *dataDir, *configPath)
		if err != nil {
			return err
		}
		checks = doctorReplaceCheck(checks, doctorCheckReleasePolicy(in.releaseMaxFilesChanged, in.releaseMaxInsertions, in.releaseRollbackPlan))
		if in.workspace != "" {
			// .factory.yml (fix (a), written by doctorApplyFixes above) --
			// a round-2 review of Phase A found this was left
			// out of the original re-evaluation, so the same run still
			// reported "no .factory.yml" right after -fix had just written
			// one. Cheap (a single os.Stat), so always re-evaluated
			// whenever -workspace is set, not only when data_dir changed.
			checks = doctorReplaceCheck(checks, doctorCheckFactoryYML(in.workspace))
			// Mirrors doctorChecksFor's own gating for this check exactly
			// (it lives inside the same `if in.workspace != ""` block) --
			// data_dir is the one field doctorApplyFixes' item (c) fix
			// (repointing it outside $HOME) can have just changed. Also
			// cheap (no Docker): always re-evaluated alongside .factory.yml.
			checks = doctorReplaceCheck(checks, doctorCheckDataDirOutsideWorkspace(in.workspace, in.dataDir))
		}
		// The remaining two check families are Docker-dependent (though
		// neither pulls or builds an image -- doctorCheckDataDirMountVisibility
		// and doctorChecksRequestSourceRepos both only ever launch a
		// container from an image already resolved locally, the same
		// "probes against local paths" class as the release-policy/
		// .factory.yml checks above, just costlier) -- a round-2 review
		// of Phase A found these should be re-probed only when data_dir
		// actually changed (fix (c) repointed it), not on every -fix run.
		if in.dataDir != oldDataDir {
			mountCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			checks = doctorReevaluateDataDirDependentChecks(mountCtx, checks, in)
			cancel()
		}
	}
	if failed := runDoctorChecks(checks, os.Stdout); failed > 0 {
		return fmt.Errorf("%d doctor check(s) failed", failed)
	}
	return nil
}

// doctorApplyBuiltImageRefs applies whatever quickstartBuiltImageRefs
// recovers from checks (images doctorFixAbsentImages actually built,
// recorded on each such doctorCheck's own Use field, "-sandbox-image
// <ref>"-shaped) onto in, so a later re-evaluation (doctorMain's own
// -fix step) uses the freshly built image rather than the unbuilt
// canonical ref doctorRunChecks' own local `in` copy substituted but
// never returned. Reused, not duplicated: quickstartEnsureConfig already
// uses this exact mechanism to carry a locally built image into the
// session config it writes.
func doctorApplyBuiltImageRefs(in doctorInputs, checks []doctorCheck) doctorInputs {
	refs := quickstartBuiltImageRefs(checks)
	if ref, ok := refs["-sandbox-image"]; ok {
		in.sandboxImage = ref
	}
	if ref, ok := refs["-registry-proxy-image"]; ok {
		in.registryProxyImage = ref
	}
	return in
}

// doctorReplaceCheck replaces the doctorCheck in checks whose Name matches
// updated.Name with updated (appending it if no such check exists yet) --
// used by doctorMain to re-evaluate exactly the checks a just-applied -fix
// can have changed in place, rather than re-running the full check list
// (which would re-run every Docker/image-pull probe a second time -- see
// doctorMain's own -fix re-evaluation step).
func doctorReplaceCheck(checks []doctorCheck, updated doctorCheck) []doctorCheck {
	for i, c := range checks {
		if c.Name == updated.Name {
			checks[i] = updated
			return checks
		}
	}
	return append(checks, updated)
}

// doctorReplaceChecksWithPrefix removes every doctorCheck in checks whose
// Name has prefix and appends fresh in their place -- doctorReplaceCheck's
// single exact-Name match doesn't apply to a check family with per-item
// dynamic names, such as doctorChecksRequestSourceRepos' one "mount
// visibility (request <id> source repo reachable inside a container)"
// check per in-flight request.
func doctorReplaceChecksWithPrefix(checks []doctorCheck, prefix string, fresh []doctorCheck) []doctorCheck {
	kept := checks[:0:0]
	for _, c := range checks {
		if !strings.HasPrefix(c.Name, prefix) {
			kept = append(kept, c)
		}
	}
	return append(kept, fresh...)
}

// doctorReevaluateDataDirDependentChecks re-evaluates the Docker-dependent
// checks a data_dir change can affect: -data-dir mount visibility and
// every in-flight request's own source-repo mount visibility
// (doctorChecksRequestSourceRepos). Called only once doctorMain already
// knows data_dir actually changed (a repoint fix, item (c), just applied
// it). Neither check family pulls or builds an image -- both only ever
// launch a container from an image already resolved locally -- so this
// never re-runs doctorFixAbsentImages' own work.
// Factored out of doctorMain so it can be tested directly against a
// stubbed/nonexistent Docker binary, without needing a live Docker daemon.
func doctorReevaluateDataDirDependentChecks(ctx context.Context, checks []doctorCheck, in doctorInputs) []doctorCheck {
	image := in.sandboxImage
	if in.workspace != "" && !in.noRelay {
		checks = doctorReplaceCheck(checks, doctorCheckDataDirMountVisibility(ctx, in.sandboxDocker, image, in.dataDir))
	}
	if in.checkRequestSourceRepos && in.dataDir != "" {
		checks = doctorReplaceChecksWithPrefix(checks, "mount visibility (request ", doctorChecksRequestSourceRepos(ctx, in.sandboxDocker, image, in.dataDir, in.workspace))
	}
	return checks
}

// doctorRefreshInputsAfterFix re-resolves in's config-derived fields
// (release policy, -data-dir) from configPath -- called by doctorMain
// right after doctorApplyFixes, whose own fixes (release_* defaults
// backfilled, data_dir repointed) land on disk, not on in, which
// doctorRunChecks already built from settings/dataDir resolved BEFORE
// those fixes ran. Without this refresh, a re-run of the check list still
// used the pre-fix values and so still reported the exact warning -fix
// had just resolved. Factored out of
// doctorMain (rather than inlined) so it can be tested directly against a
// real config file, without doctorMain's own unconditional Docker/network
// checks (doctorCheckDockerReachable, doctorCheckImagePresent) that need
// a live Docker daemon this test suite cannot assume.
func doctorRefreshInputsAfterFix(in doctorInputs, dataDirExplicit bool, dataDir, configPath string) (doctorInputs, string, error) {
	settings, err := loadSettingsForConfig(configPath)
	if err != nil {
		return in, dataDir, err
	}
	in.releaseMaxFilesChanged = settings.ReleaseMaxFilesChanged
	in.releaseMaxInsertions = settings.ReleaseMaxInsertions
	in.releaseRollbackPlan = settings.ReleaseRollbackPlan
	dataDir = resolveConfiguredDataDir(dataDirExplicit, dataDir, configPath)
	in.dataDir = dataDir
	return in, dataDir, nil
}

// doctorRunChecks is the "run all the doctor checks and collect results"
// logic doctorMain wraps with flag parsing and printing, factored out so
// a caller in this same package (a future `factoryd quickstart`) can get
// the exact same []doctorCheck results -- Name/Err/Fix/Use/Advisory --
// doctorMain prints, without string-matching its own collapsed "%d
// doctor check(s) failed"
// error to tell an image-pull failure apart from anything else. Prints
// nothing itself. fix and repoRoot mirror -fix/-repo-root: fix true runs
// doctorFixAbsentImages first, substituting any freshly built image
// references into in before the regular checks run, exactly as doctorMain
// does today. The returned error is non-nil only when checks could not
// even be attempted; every per-check failure -- including -fix's own
// engine-resolution failure, which doctorFixAbsentImages already reports
// as a doctorCheck -- lives in the returned slice's own Err field instead.
func doctorRunChecks(dp *deps, in doctorInputs, fix bool, repoRoot string) ([]doctorCheck, error) {
	var built []doctorCheck
	if fix {
		built, in = doctorFixAbsentImages(dp, in, repoRoot)
	}
	// A fresh clock for the regular checks: a -fix build above can take
	// minutes, and a context started before it would already be expired
	// here, failing every later probe spuriously (Sonnet review).
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	checks := append(built, doctorChecksFor(ctx, in)...)
	if plistPath, plistErr := hostcontrol.WorkerPlistPath(); plistErr == nil {
		checks = append(checks, doctorCheckWorkerService(ctx, "launchctl", plistPath))
	}
	if selfPath, selfErr := os.Executable(); selfErr == nil {
		checks = append(checks, doctorCheckPathShadowing(selfPath, os.Getenv("PATH"), fix))
	}
	return checks, nil
}

// doctorApplyPathFixEnvVar opts a real `factoryd doctor -fix` invocation
// into actually renaming a shadowing binary in a doctorPathShadowSafeDir
// location, rather than only printing the command that would do it --
// unset (the default) leaves every file on disk untouched, since this
// deletes (renames away) a file the operator may not have meant to lose.
const doctorApplyPathFixEnvVar = "FACTORYD_DOCTOR_APPLY_PATH_FIX"

// doctorPathShadowSafeDir reports whether dir is one of the few locations
// this check trusts enough to run a shadowing binary's own "version"
// subcommand against and, with -fix, rename automatically: the operator's
// own $HOME/.local/bin, $GOPATH/bin (default $HOME/go/bin when $GOPATH is
// unset, matching `go env GOPATH`'s own default), or a Homebrew prefix's
// bin/ (/usr/local, /opt/homebrew, or $HOMEBREW_PREFIX when set). Every
// other directory is left exactly as doctorCheckPathShadowing originally
// treated all of them -- reported by os.Stat alone, never executed or
// touched -- because it could be attacker-controlled or otherwise outside
// the operator's own management (see describeCandidateWithoutExecuting's
// own doc comment for the original reasoning, preserved here for anything
// outside these three).
func doctorPathShadowSafeDir(dir string) bool {
	home, _ := os.UserHomeDir()
	if home != "" && dir == filepath.Join(home, ".local", "bin") {
		return true
	}
	gopath := os.Getenv("GOPATH")
	if gopath == "" && home != "" {
		gopath = filepath.Join(home, "go")
	}
	if gopath != "" && dir == filepath.Join(gopath, "bin") {
		return true
	}
	if prefix := os.Getenv("HOMEBREW_PREFIX"); prefix != "" && dir == filepath.Join(prefix, "bin") {
		return true
	}
	return dir == "/usr/local/bin" || dir == "/opt/homebrew/bin"
}

// doctorPathShadowFixLine names the exact command that retires a
// shadowing binary at a doctorPathShadowSafeDir location: the rm/mv pair
// an operator can run by hand, or -- only when apply is true, i.e. -fix
// was given and doctorApplyPathFixEnvVar is set -- the rename actually
// performed here, to "<path>.stale-<today>" rather than deleting outright
// so a wrong guess is still recoverable.
func doctorPathShadowFixLine(path string, apply bool) string {
	stale := fmt.Sprintf("%s.stale-%s", path, time.Now().Format("2006-01-02"))
	if !apply {
		return fmt.Sprintf("rm %s   (or, to keep a copy: mv %s %s)", path, path, stale)
	}
	if err := os.Rename(path, stale); err != nil {
		return fmt.Sprintf("mv %s %s failed: %v", path, stale, err)
	}
	return fmt.Sprintf("renamed %s -> %s", path, stale)
}

// doctorCheckPathShadowing exists because two `factoryd` binaries on $PATH
// (e.g. a release build and a
// from-source build) means whichever comes first wins silently -- the
// other may be a stale binary with fewer doctor checks or a different
// flag set, and nothing before this check ever told an operator it was
// even there. Walks every directory in pathEnv (the same $PATH the shell
// itself would search) for a "factoryd"-named file other than selfPath
// (the currently-running binary, from os.Executable()). A shadow in a
// doctorPathShadowSafeDir location is identified concretely -- its own
// "version" output, run directly, since these directories are the
// operator's own -- and, when fix is true, gets a doctorPathShadowFixLine
// command; every other shadow keeps this check's original treatment,
// reported by os.Stat alone via describeCandidateWithoutExecuting, never
// executed (a shadowing $PATH entry outside those directories can be
// untrusted or attacker-controlled). Advisory (see doctorCheck.Advisory):
// a shadowing binary existing is not itself a broken environment -- this
// doctor invocation already found its way to running -- so it warns
// rather than failing the run.
func doctorCheckPathShadowing(selfPath, pathEnv string, fix bool) doctorCheck {
	const name = "PATH shadowing"
	selfResolved := resolveExecutablePath(selfPath)
	base := filepath.Base(selfPath)
	seen := map[string]bool{selfResolved: true}
	var shadows []string
	for _, dir := range filepath.SplitList(pathEnv) {
		if dir == "" {
			continue
		}
		candidate := filepath.Join(dir, base)
		info, err := os.Stat(candidate)
		if err != nil || info.IsDir() {
			continue
		}
		resolved := resolveExecutablePath(candidate)
		if seen[resolved] {
			continue
		}
		seen[resolved] = true
		shadows = append(shadows, candidate)
	}
	if len(shadows) == 0 {
		return doctorCheck{Name: name}
	}
	applyFix := fix && os.Getenv(doctorApplyPathFixEnvVar) == "1"
	details := make([]string, len(shadows))
	var fixLines []string
	for i, s := range shadows {
		// Never executed, whatever directory it sits in: a Homebrew prefix
		// is group-writable by design, so "trusted location" is not a
		// reason to run a binary someone else may have dropped there. The
		// fix offer below only ever renames a file the operator's own
		// package manager or install script placed.
		details[i] = fmt.Sprintf("%s (%s)", s, describeCandidateWithoutExecuting(s))
		if fix && doctorPathShadowSafeDir(filepath.Dir(s)) {
			fixLines = append(fixLines, doctorPathShadowFixLine(s, applyFix))
		}
	}
	fixMsg := "remove or rename the shadowing binary, or reorder $PATH so the intended one comes first"
	if len(fixLines) > 0 {
		fixMsg = strings.Join(fixLines, "; ")
		if !applyFix {
			fixMsg += fmt.Sprintf(" -- set %s=1 (with -fix) to apply the rename above automatically", doctorApplyPathFixEnvVar)
		}
	}
	return doctorCheck{
		Name: name,
		Err: fmt.Errorf("this binary is %s (%s); also found on $PATH: %s -- whichever comes first on $PATH wins silently, and the other may be a stale build (no shadow is ever executed here, since a $PATH directory can be untrusted or attacker-controlled; run each one with `version` yourself to compare)",
			selfPath, factoryVersionOf(selfPath), strings.Join(details, ", ")),
		Fix:      fixMsg,
		Advisory: true,
	}
}

// describeCandidateWithoutExecuting reports a shadowing binary's identity
// via os.Stat alone (size, mod time) -- deliberately never executes it.
// Found via Codex review of PR #172 (P1): a later $PATH entry can be a
// stale build, a hung process, or a binary in an untrusted/checked-out
// project directory outside the operator's control, so running it (even
// with a harmless-looking "version" subcommand) to report its version
// would execute arbitrary attacker-controlled code on the host. selfPath
// (from os.Executable(), the binary this process actually is) is the only
// candidate factoryVersionOf ever runs.
func describeCandidateWithoutExecuting(path string) string {
	info, err := os.Stat(path)
	if err != nil {
		return "stat failed: " + err.Error()
	}
	return fmt.Sprintf("size %d bytes, modified %s", info.Size(), info.ModTime().Format(time.RFC3339))
}

// resolveExecutablePath follows symlinks so two different $PATH entries
// that point at the same underlying file aren't reported as a shadow of
// each other; falls back to the unresolved path when EvalSymlinks fails
// (e.g. a broken symlink), since that's still enough to tell it apart
// from a different file.
func resolveExecutablePath(path string) string {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}
	return path
}

// factoryVersionOf runs a candidate factoryd binary with "version" to
// report what it actually is, mirroring versionMain's own "factoryd
// version <version>" output; "unknown" when it can't be run at all (e.g.
// not actually executable, or predates the "version" subcommand).
func factoryVersionOf(path string) string {
	out, err := exec.Command(path, "version").CombinedOutput()
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(string(out))
}

// doctorInputs is every value doctorChecksFor checks against. An empty
// field skips the checks that need it. There is no built-in image
// default: an empty sandboxImage is reported as a failing check by
// doctorChecksFor (see resolveDoctorSandboxImage).
type doctorInputs struct {
	sandboxDocker string
	sandboxImage  string
	// meterImage is session config meter_image; empty makes the OpenShell
	// checks report "not configured".
	meterImage         string
	registryProxyImage string
	// harnessOverride, when set, names the one harness every role is checked
	// as (quickstart's image pass before any roles are written).
	harnessOverride string
	// configPath is the absolute path of the session config actually in
	// effect (resolved even when -config was left empty), used only to
	// steer doctorMissingImageFixHint's `make install FACTORYD_CONFIG=`
	// suggestion toward a non-default config -- never consulted by any
	// check itself.
	configPath string
	// imageSourceRoot is settings.ImageSourceRoot (session config
	// image_source_root, written by `factoryd configure-images`) -- when
	// set, doctorChecksFor also warns (never fails) about any of the
	// three images above that is stale against this checkout's current
	// source, per internal/imageinputs.Hash. Empty means "cannot verify",
	// not "fresh" -- no staleness check runs at all.
	imageSourceRoot string
	// relayUpstream/relayWorkerModelID/relayWorkerAPI/relayWorkerBasePath/
	// relayCredentialMode/relayCredentialHeader/relayAllowPlaintextUpstream/
	// relayAllowNoCredential/relayGitHubTokenFile/relayGitHubTokenKey/
	// relayCodexAuthFile are set only by doctorListModels itself, from a
	// real modelrole.SelectRoute(roles.execution) resolution -- never
	// populated at doctorInputs construction time, since routes:/models:/
	// roles: is the only session-config schema and doctorRoutesModeChecks
	// resolves its own per-(route, model) fields directly from Selection.
	relayUpstream               string
	relayWorkerModelID          string
	relayWorkerAPI              string
	relayWorkerBasePath         string
	relayCredentialMode         string
	relayCredentialHeader       string
	relayAllowPlaintextUpstream bool
	relayAllowNoCredential      bool
	relayGitHubTokenFile        string
	relayGitHubTokenKey         string
	relayCodexAuthFile          string
	sandboxTmpfsSize            string
	workspace                   string
	dataDir                     string
	temporalAddress             string
	egressCABundle              string
	lintCommand                 string
	securityCommand             string
	unitTestCommand             string
	integrationTestCommand      string
	referenceOracleCommand      string
	referenceOracleDir          string
	composeServices             bool
	// targetRepo is -target-repo: see doctorTargetRepoChecks.
	targetRepo string
	// releaseMaxFilesChanged/releaseMaxInsertions/releaseRollbackPlan feed
	// doctorCheckReleasePolicy: the same three
	// release_max_files_changed/release_max_insertions/release_rollback_plan
	// session-config keys releasePolicyCanNeverAllow already checks at
	// runtime, surfaced here so an operator learns about a deny-all
	// release policy from `factoryd doctor` before ever running a ticket,
	// not only after a first accepted run silently produces no PR.
	releaseMaxFilesChanged int
	releaseMaxInsertions   int
	releaseRollbackPlan    string
	// checkRequestSourceRepos makes doctorChecksFor also probe the source
	// repo of every in-flight request under dataDir (set by `factoryd doctor`
	// only, not worker's startup preflight).
	checkRequestSourceRepos bool
	// settings feeds doctorCheckRolesResolve: the actual, fully resolved
	// sessionconfig.Settings a real run would validate roles:/routes:/
	// models: against, so ValidateRouting works the same way here as at
	// any real process start. Every caller that resolves a session
	// config sets this directly from that resolved Settings (doctorMain's
	// own top-level command; worker's own lighter internal preflight
	// reuse of this struct, from its own cfg.settings; quickstart's config
	// prefill, from existing.ApplySettings) -- never reconstructed from a
	// handful of cherry-picked fields, which could never have carried
	// enough information for the legacy-key-mix case to be caught at all.
	settings sessionconfig.Settings
	// presenceOnly marks a doctor call that only wants to know whether
	// images are present locally (quickstartPullDoctorInputs, run before
	// this quickstart invocation's own config is finalized -- see its own
	// doc comment) -- never a real preflight for a run that will actually
	// use whatever routes:/models:/roles: happens to already be on disk
	// from a previous run. In routes: mode this skips
	// doctorRoutesModeChecks entirely: probing a route's real network/
	// credential reachability at this stage would resolve and probe
	// yesterday's config, not the one this invocation is about to write,
	// for no benefit to the one thing this pass actually checks (image
	// presence). Never set for `factoryd doctor` itself or worker's
	// own startup preflight -- both want the real thing.
	presenceOnly bool
	// noRelay marks a doctor call whose own caller already knows this
	// invocation calls no model (worker's own startup preflight sets it
	// for an explicit offline -build-app-script) -- skips
	// doctorRoutesModeChecks the same way presenceOnly does, but for a
	// different reason. Left false (the default) for every caller that does
	// not positively know that: `factoryd doctor` itself, worker's real
	// preflight, and every test that doesn't set it.
	noRelay bool
}

// doctorMissingImageFixHint builds the Fix text for an unconfigured
// sandbox image: `make install` alone when configPath is empty or is
// one of sessionconfig.DefaultPaths() (the plain command already writes
// there), or names FACTORYD_CONFIG=configPath when it is some other
// config -- otherwise the hint is circular, telling an operator who just
// ran `make install` for a non-default -config (e.g. a per-project
// config) to run the exact command that already succeeded, against the
// DEFAULT config it never touches (found 2026-09-26).
func doctorMissingImageFixHint(configPath, flagName string) string {
	installCmd := "make install"
	if configPath != "" {
		isDefault := false
		for _, p := range sessionconfig.DefaultPaths() {
			if abs, err := filepath.Abs(p); err == nil && abs == configPath {
				isDefault = true
				break
			}
		}
		if !isDefault {
			installCmd = fmt.Sprintf("make install FACTORYD_CONFIG=%s", configPath)
		}
	}
	return fmt.Sprintf("run `%s` from the buildgate checkout (builds images from source and records them via `factoryd configure-images`), or pass %s", installCmd, flagName)
}

// doctorChecksFor builds the check list `factoryd doctor` runs, shared
// with worker's startup preflight so both verify the identical set for
// the same inputs.
func doctorChecksFor(ctx context.Context, in doctorInputs) []doctorCheck {
	image := in.sandboxImage
	roleHarnesses, err := in.roleHarnessSets()
	if err != nil {
		return []doctorCheck{{Name: "harness", Err: err}}
	}
	var checks []doctorCheck
	checks = append(checks, doctorCheckDockerReachable(ctx, in.sandboxDocker))
	needsOwnImage := doctorHarnessNeedingOwnImage(roleHarnesses)
	if needsOwnImage != nil && image == "" {
		fix := "set -sandbox-image explicitly for this harness"
		errText := fmt.Sprintf("%s requires an explicit digest-pinned worker image", needsOwnImage.Name)
		if needsOwnImage.Name == harness.Pifork {
			fix = "set -sandbox-image to the digest printed by make pifork-image"
			errText += "; build one with make pifork-image PIFORK_DOCKERFILE=<your Dockerfile>"
		}
		checks = append(checks, doctorCheck{
			Name: needsOwnImage.Name + " worker image",
			Err:  errors.New(errText),
			Fix:  fix,
		})
	} else if image == "" {
		checks = append(checks, doctorCheck{
			Name: "sandbox image",
			Err:  errors.New("no sandbox image configured"),
			Fix:  doctorMissingImageFixHint(in.configPath, "-sandbox-image"),
		})
	} else {
		present := doctorCheckImagePresent(ctx, in.sandboxDocker, "sandbox image", image,
			"images are built from source, never pulled from a registry -- rerun with -fix to build a local copy (make sandbox-image)")
		checks = append(checks, present)
		// No harness gate here: the image's own buildgate.image
		// label (read by doctorCheckImageStale itself) says what kind it
		// actually is -- worker, pifork, or project -- so this
		// runs the same way regardless of which harnesses the roles use.
		if c, ok := doctorCheckImageStale(ctx, in.sandboxDocker, "sandbox image", image, in.imageSourceRoot); ok {
			checks = append(checks, c)
		}
		// Every distinct harness the roles use must have its binary in the
		// one sandbox image; only checked once the image itself is present.
		if present.Err == nil && !in.presenceOnly {
			for _, d := range doctorDistinctHarnesses(roleHarnesses) {
				checks = append(checks, doctorCheckHarnessInImage(ctx, in.sandboxDocker, image, d))
			}
			if runtime.GOOS == "darwin" {
				checks = append(checks, doctorCheckHomeNotShared(ctx, in.sandboxDocker, image))
			}
		}
	}
	if in.registryProxyImage != "" {
		checks = append(checks, doctorCheckImagePresent(ctx, in.sandboxDocker, "registry proxy image", in.registryProxyImage,
			"images are built from source, never pulled from a registry -- rerun with -fix to build it locally (make registry-proxy-image)"))
		if c, ok := doctorCheckImageStale(ctx, in.sandboxDocker, "registry proxy image", in.registryProxyImage, in.imageSourceRoot); ok {
			checks = append(checks, c)
		}
	}
	if in.egressCABundle != "" {
		checks = append(checks, doctorCheckEgressCABundle(in.egressCABundle))
	}
	// presenceOnly (quickstart's own image-presence-only pass, before
	// any config this run will actually use is finalized) and
	// noRelay (a caller who positively knows no model is called) both
	// skip every route/network/credential check below entirely -- neither is "this
	// route is broken", so neither should probe it at all.
	checks = append(checks, doctorPriceChecks(in.settings)...)
	if !in.presenceOnly && !in.noRelay {
		checks = append(checks, doctorRoutesModeChecks(ctx, in, image)...)
	}
	if in.sandboxTmpfsSize != "" {
		checks = append(checks, doctorCheckTmpfsSize(in.sandboxTmpfsSize))
	}
	checks = append(checks, doctorCheckReleasePolicy(in.releaseMaxFilesChanged, in.releaseMaxInsertions, in.releaseRollbackPlan))
	checks = append(checks, doctorCheckRolesResolve(in))
	checks = append(checks, doctorCheckSkills(in))
	checks = append(checks, doctorCheckDesignGuide(in))
	if in.workspace != "" {
		checks = append(checks, doctorCheckMountVisibility(ctx, in.sandboxDocker, image, in.workspace))
		checks = append(checks, doctorCheckMonorepoModuleRoot(in.workspace))
		checks = append(checks, doctorCheckDataDirOutsideWorkspace(in.workspace, in.dataDir))
		checks = append(checks, doctorCheckGitCredentialsNotExposed(in.workspace))
		checks = append(checks, doctorCheckFactoryYML(in.workspace))
		// Skipped (not failed) when -workspace has no remote
		// doctorRepoGitHubHost can resolve a host from -- see
		// doctorCheckGHAuth's own doc comment.
		checks = append(checks, doctorCheckGHAuth(ctx, execGHAuthRunner, in.workspace))
		if !in.noRelay && in.dataDir != "" {
			// -data-dir must be shared with the Docker backend's VM like
			// -workspace (colima's default shares only $HOME; a path
			// outside it mounts as present-but-empty rather than erroring).
			// Gated on -workspace being set too, alongside the other checks
			// in this block: "only probe once a concrete target was
			// actually given"
			// (TestIntegrationDoctorSkipsChecksWithNoInputToCheckAgainst).
			// Enforcement at run-start time does not need -workspace
			// (run_ticket.go/worker_config.go's own
			// doctorCheckDataDirMountVisibility preflight runs
			// regardless) -- this is advance warning for an operator
			// running `doctor` directly.
			checks = append(checks, doctorCheckDataDirMountVisibility(ctx, in.sandboxDocker, image, in.dataDir))
		}
	}
	if in.checkRequestSourceRepos && in.dataDir != "" {
		checks = append(checks, doctorChecksRequestSourceRepos(ctx, in.sandboxDocker, image, in.dataDir, in.workspace)...)
	}
	if in.referenceOracleDir != "" {
		// Reuses -workspace's own mount-visibility probe unchanged (same
		// underlying failure mode: a host path the Docker backend doesn't
		// actually share into its VM produces a silently empty mount, not
		// an error) -- its "-workspace" wording in the reported name/Fix
		// text is accurate about the mechanism, just not about which flag
		// this particular call is checking.
		checks = append(checks, doctorCheckMountVisibility(ctx, in.sandboxDocker, image, in.referenceOracleDir))
	}
	if in.temporalAddress != "" {
		checks = append(checks, doctorCheckTemporalReachable(ctx, in.temporalAddress))
		checks = append(checks, doctorCheckStaleTemporalWorkflows(ctx, in.temporalAddress))
	}
	checks = append(checks, doctorCheckBuildx(ctx, in.sandboxDocker))
	if in.composeServices {
		checks = append(checks, doctorCheckComposeVersion(ctx, in.sandboxDocker))
	}
	if in.targetRepo != "" {
		checks = append(checks, doctorTargetRepoChecks(ctx, in)...)
		checks = append(checks, doctorTargetRepoSubmitChecks(in.targetRepo)...)
	}
	for _, gate := range []struct{ label, command string }{
		{"-lint-command", in.lintCommand},
		{"-security-command", in.securityCommand},
		{"-unit-test-command", in.unitTestCommand},
		{"-integration-test-command", in.integrationTestCommand},
		{"-reference-oracle-command", in.referenceOracleCommand},
	} {
		if gate.command != "" {
			checks = append(checks, doctorCheckExecutableInImage(ctx, in.sandboxDocker, image, gate.label, gate.command))
		}
	}
	return checks
}

// doctorRouteKeys names the session-config key doctor's own Err/Fix text
// should mention for one relay/credential field -- one doctorRouteKeys
// per doctorRoute, built by doctorRoutesModeRouteKeys(routeName,
// modelName) for a routes:/models: (route, model) pair. Several
// doctorCheck* helpers below (doctorCheckCopilotModelListed,
// doctorCheckRelayUpstreamPathComposition, ...) take the doctorRouteKeys
// their caller resolved as a parameter and build their own Err/Fix text
// from it, instead of hard-coding one vocabulary -- replacing an earlier
// version that built the Err/Fix text first and then blindly string-
// replaced key names in it afterward, which could corrupt an operator's
// own upstream/path value that happened to contain one of those names
// as a substring (e.g. an upstream host literally named
// internal-relay-upstream.example.com).
type doctorRouteKeys struct {
	upstream               string
	allowedPathPrefix      string
	workerBasePath         string
	workerModelID          string
	workerAPI              string
	extraJSON              string
	githubTokenFile        string
	githubTokenKey         string
	codexAuthFile          string
	credentialHeader       string
	allowPlaintextUpstream string
}

// doctorRoutesModeRouteKeys is one routes:/models: (route, model) pair's
// own key vocabulary -- the routes.<r>.*/models.<m>.* keys
// doctorRoutesModeChecks' own selection actually configures.
func doctorRoutesModeRouteKeys(routeName, modelName string) doctorRouteKeys {
	return doctorRouteKeys{
		upstream:               fmt.Sprintf("routes.%s.upstream", routeName),
		allowedPathPrefix:      fmt.Sprintf("routes.%s.allowed_path_prefix", routeName),
		workerBasePath:         fmt.Sprintf("routes.%s.worker_base_path", routeName),
		workerModelID:          fmt.Sprintf("models.%s.id", modelName),
		workerAPI:              fmt.Sprintf("models.%s.api", modelName),
		extraJSON:              fmt.Sprintf("models.%s.context_window", modelName),
		githubTokenFile:        fmt.Sprintf("routes.%s.github_token_file", routeName),
		githubTokenKey:         fmt.Sprintf("routes.%s.github_token_key", routeName),
		codexAuthFile:          fmt.Sprintf("routes.%s.codex_auth_file", routeName),
		credentialHeader:       fmt.Sprintf("routes.%s.credential_header", routeName),
		allowPlaintextUpstream: fmt.Sprintf("routes.%s.allow_plaintext_upstream", routeName),
	}
}

// doctorRoute is the one route's worth of relay/credential fields
// doctorRouteChecks needs, factored out of doctorInputs: built from one
// routes:/models: (model, route) pair a real modelrole.SelectRoute call
// actually resolved (that Selection's own Policy/Route, label
// "route <r>, model <m>" -- see doctorRoutesModeChecks). keys names the
// vocabulary every check built from r should use in its own Err/Fix text
// (doctorRoutesModeRouteKeys(routeName, modelName)).
type doctorRoute struct {
	label                  string
	harness                harness.Descriptor
	routeName              string
	modelName              string
	credentialMode         string
	upstream               string
	allowedPathPrefix      string
	workerBasePath         string
	workerAPI              string
	workerModelID          string
	workerModelExtraJSON   string
	githubTokenFile        string
	githubTokenKey         string
	codexAuthFile          string
	allowPlaintextUpstream bool
	keys                   doctorRouteKeys
}

// doctorRouteCheckName prefixes name with r's label (when non-empty), so
// a check reads "route <r>, model <m>: <name>".
func doctorRouteCheckName(label, name string) string {
	if label == "" {
		return name
	}
	return label + ": " + name
}

// doctorRouteChecks is every check that depends on one route's own
// relay/credential fields, factored out of doctorChecksFor so it runs
// once per selected routes:/models: (model, route) pair
// (doctorRoutesModeChecks). Every check name is prefixed with r.label
// (doctorRouteCheckName). runCredentialChecks/runUpstreamProbes gate
// only the checks doctorRoutesModeChecks itself de-duplicates -- once
// per route (Copilot token exchange, ChatGPT codex credential file) or
// once per distinct upstream (the sandbox host-resolves/reachability
// probes, identical for any two pairs that happen to share an upstream)
// -- every other check here is specific to this (model, route) pair (it
// reads r.workerModelID/workerAPI) and always runs. Every check's own
// Err/Fix text is built from r.keys (doctorRouteKeys,
// doctorRoutesModeRouteKeys()).
func doctorRouteChecks(ctx context.Context, in doctorInputs, image string, r doctorRoute, runCredentialChecks, runUpstreamProbes bool) []doctorCheck {
	var checks []doctorCheck
	if r.workerAPI != "" {
		check := doctorCheck{Name: doctorRouteCheckName(r.label, fmt.Sprintf("relay worker API (%s)", r.workerAPI))}
		switch r.workerAPI {
		case meter.RequestFormatOpenAICompletions:
		case meter.RequestFormatOpenAIResponses:
			if r.workerModelID == "" {
				check.Err = fmt.Errorf("openai-responses requires %s", r.keys.workerModelID)
				check.Fix = fmt.Sprintf("set %s to the entitled upstream model id", r.keys.workerModelID)
			}
		default:
			check.Err = fmt.Errorf("unsupported relay worker API %q", r.workerAPI)
			check.Fix = "use openai-completions or openai-responses"
		}
		checks = append(checks, check)
	}
	if r.upstream != "" {
		// chatgpt-codex pins its upstream to meter.ChatGPTCodexAPIBase, whose
		// /backend-api/codex path is exactly the prefix the relay must keep
		// (see RoutePolicy.Validate), so the bare-root heuristic does not apply.
		if r.credentialMode != meter.CredentialModeChatGPTCodex {
			c := doctorCheckRelayUpstreamPathComposition(r.upstream, r.workerModelID, r.keys)
			c.Name = doctorRouteCheckName(r.label, c.Name)
			checks = append(checks, c)
		}
		// github-copilot mode replaces the in-sandbox model-listing
		// reachability check with a direct token exchange below: Copilot's
		// own /models endpoint requires the same identifying headers this
		// relay already sends (see internal/meter's own Copilot header
		// citation), so a bare, credential-less probe container proves
		// nothing about whether this route actually works.
		if runUpstreamProbes && r.credentialMode != meter.CredentialModeGitHubCopilot && r.credentialMode != meter.CredentialModeChatGPTCodex {
			c1 := doctorCheckRelayUpstreamHostResolvesInSandbox(ctx, in.sandboxDocker, image, r.upstream, r.keys)
			c1.Name = doctorRouteCheckName(r.label, c1.Name)
			checks = append(checks, c1)
			c2 := doctorCheckRelayUpstreamReachableFromSandbox(ctx, in.sandboxDocker, image, r.upstream, r.workerBasePath, r.workerModelID, in.egressCABundle, r.keys)
			c2.Name = doctorRouteCheckName(r.label, c2.Name)
			checks = append(checks, c2)
		}
	}
	if r.credentialMode == meter.CredentialModeGitHubCopilot {
		if runCredentialChecks {
			c := doctorCheckCopilotTokenExchange(ctx, r.githubTokenFile, r.githubTokenKey, in.egressCABundle, r.keys)
			c.Name = doctorRouteCheckName(r.label, c.Name)
			checks = append(checks, c)
		}
		// A config-only check, like chatgpt-codex's own "route pinned"
		// check just below -- no network call, so it runs even when the
		// token exchange above fails, and fails on exactly what a real
		// run's own RoutePolicy.Validate refuses (sandbox.
		// ValidateGitHubCopilotWorkerAPI), not "lists the model" (that's
		// doctorCheckCopilotModelListed just below, which no longer
		// duplicates this).
		route := doctorCheck{Name: doctorRouteCheckName(r.label, "github copilot route config (models.<m>.api / routes.<r>.allowed_path_prefix)")}
		if err := sandbox.ValidateGitHubCopilotWorkerAPI(r.workerAPI, r.allowedPathPrefix); err != nil {
			route.Err = err
			route.Fix = fmt.Sprintf("set %s to match %s, or remove %s from session config so it defaults from %s", r.keys.allowedPathPrefix, r.keys.workerAPI, r.keys.allowedPathPrefix, r.keys.workerAPI)
		}
		checks = append(checks, route)
		// Only meaningful once a model id is actually configured --
		// the harness's RequiresWorkerModel check just below reports an
		// unset id.
		if r.workerModelID != "" {
			c := doctorCheckCopilotModelListed(ctx, r.githubTokenFile, r.githubTokenKey, in.egressCABundle, r.workerModelID, r.upstream, r.workerAPI, r.allowPlaintextUpstream, r.keys)
			c.Name = doctorRouteCheckName(r.label, c.Name)
			checks = append(checks, c)
		}
	}
	if r.credentialMode == meter.CredentialModeChatGPTCodex {
		if runCredentialChecks {
			c := doctorCheckChatGPTCodexCredential(r.codexAuthFile, r.keys)
			c.Name = doctorRouteCheckName(r.label, c.Name)
			checks = append(checks, c)
		}
		route := doctorCheck{Name: doctorRouteCheckName(r.label, "chatgpt codex route pinned")}
		if err := meter.ValidateChatGPTCodexRoute(r.upstream, r.allowedPathPrefix, r.workerBasePath); err != nil {
			route.Err = err
			route.Fix = fmt.Sprintf("remove %s/%s/%s from the session config used for this route", r.keys.upstream, r.keys.allowedPathPrefix, r.keys.workerBasePath)
		}
		checks = append(checks, route)
	}
	if r.harness.RequiresWorkerModel && r.workerModelID == "" {
		fix := fmt.Sprintf("set %s in session config", r.keys.workerModelID)
		checks = append(checks, doctorCheck{
			Name: doctorRouteCheckName(r.label, r.harness.Name+" worker model"),
			Err:  fmt.Errorf("%s requires %s for the entitled model", r.harness.Name, r.keys.workerModelID),
			Fix:  fix,
		})
	}
	if r.workerModelID != "" {
		c := doctorCheckContextWindowConfigured(r.workerModelExtraJSON, r.keys)
		c.Name = doctorRouteCheckName(r.label, c.Name)
		checks = append(checks, c)
	}
	return checks
}

// doctorRouteCredentialProbe is the modelrole.SelectRoute credential
// probe every routes: mode caller in this codebase uses (run_ticket.go's
// own inline closure of the same shape): resolveRouteCredentials'
// resolved value is discarded, only its error matters, so a route whose
// credential can't actually be resolved is skipped/refused the same way
// a real relay launch against it would be, before doctor -- or a real
// run -- ever gets that far.
func doctorRouteCredentialProbe(_ string, r sessionconfig.Route) error {
	_, err := resolveRouteCredentials(r)
	return err
}

// doctorRoleSpecs is the fixed (role name, modelrole.Role) list
// doctorRoutesModeChecks resolves a real selection for -- planning,
// execution, review, the only three roles: entries a routes: mode
// session config can carry (sessionconfig.Roles).
var doctorRoleSpecs = []struct {
	name string
	role modelrole.Role
}{
	{"planning", modelrole.RolePlanning},
	{"execution", modelrole.RoleExecution},
	{"review", modelrole.RoleReview},
}

// doctorRoleConfigured mirrors modelrole.RoleConfigured (unexported to
// that package) against s directly, since doctorRoutesModeChecks needs
// to know whether a role is configured before calling SelectRoute, not
// only after.
func doctorRoleConfigured(s sessionconfig.Settings, roleName string) bool {
	if s.Roles == nil {
		return false
	}
	var rc *sessionconfig.RoleConfig
	switch roleName {
	case "planning":
		rc = s.Roles.Planning
	case "execution":
		rc = s.Roles.Execution
	case "review":
		rc = s.Roles.Review
	}
	return rc != nil && rc.Model != ""
}

// doctorRoutesModeChecks is doctorChecksFor's routes:/models: mode
// counterpart to its legacy single doctorRouteChecks call: for each
// configured role (planning/execution/review), it calls
// modelrole.SelectRoute exactly the way a real run does (same probe,
// same "no other role's model" rule), so the check list this produces
// mirrors the real selection a run would make -- an allowed-list model
// or fallback route SelectRoute would never actually pick is never
// probed here. Each sel.Skipped entry becomes its own Advisory row
// ("roles.<role>: route <r> skipped: <reason>") -- a fallback route with
// a failing credential is expected, ordinary operation, not a doctor
// failure, as long as SOME route for that role still resolves. A
// SelectRoute error (ErrNoRouteAvailable, or an unresolved role/model
// name -- never expected once doctorCheckRolesResolve's own
// ValidateRouting call has passed, but never trusted blindly here)
// becomes a FAIL row naming the role.
//
// The per-route checks (doctorRouteChecks) then run only for the
// distinct (selected model, selected route) pairs SelectRoute actually
// returned -- deduplicated across roles, so two roles resolving to the
// same pair (e.g. review sharing execution's model, allow_shared_model:
// true) produce one labelled block, not two. Within that,
// runCredentialChecks deduplicates further to once per distinct route
// name (the Copilot token exchange / ChatGPT codex credential checks
// depend only on the route, not the model), and runUpstreamProbes to
// once per distinct upstream (the sandbox host-resolves/reachability
// probes are identical for any two pairs that share one).
// doctorPriceStaleAfter is `factoryd doctor`'s own staleness threshold for
// a declared model's compiled price (internal/prices.ModelPrice.AsOf):
// past this, a price is warned as possibly out of date with the
// provider's real, current price page, since nothing in this repo
// refreshes prices.yml automatically -- it is only ever updated by an
// operator editing it and rebuilding.
const doctorPriceStaleAfter = 90 * 24 * time.Hour

// doctorPriceChecks reports one check per declared models: entry, naming
// its own compiled price (internal/prices) with source/as_of when found,
// or FAILing with the same "no price for model id ..." reason
// sessionconfig.ValidateRouting itself refuses config load on -- doctor
// is meant to catch this before a human ever drafts against a config that
// can't launch, not just repeat what Load already would have refused by
// the time doctor runs on a config that somehow got this far (e.g. a
// price removed from the table after the config was written). A found
// price with a stale or missing as_of warns rather than fails: the price
// itself may still be correct, but nothing here can prove that from the
// as_of date alone.
func doctorPriceChecks(settings sessionconfig.Settings) []doctorCheck {
	names := make([]string, 0, len(settings.Models))
	for name := range settings.Models {
		names = append(names, name)
	}
	sort.Strings(names)

	checks := make([]doctorCheck, 0, len(names))
	now := time.Now()
	for _, name := range names {
		model := settings.Models[name]
		checkName := fmt.Sprintf("models.%s: price", name)
		price, err := prices.Lookup(model.ID)
		if errors.Is(err, prices.ErrNoPrice) {
			checks = append(checks, doctorCheck{Name: checkName, Err: err, Advisory: true})
			continue
		}
		if err != nil {
			checks = append(checks, doctorCheck{Name: checkName, Err: err})
			continue
		}
		if stale, reason := price.StaleAsOf(now, doctorPriceStaleAfter); stale {
			checks = append(checks, doctorCheck{
				Name:     checkName,
				Err:      fmt.Errorf("%s: %s", model.ID, reason),
				Advisory: true,
			})
			continue
		}
		checks = append(checks, doctorCheck{
			Name: fmt.Sprintf("%s: %s: $%s in / $%s cached / $%s out per 1M (%s, %s)",
				checkName, model.ID,
				formatUSDPerMTok(price.Input), formatUSDPerMTok(price.EffectiveCachedInput()), formatUSDPerMTok(price.Output),
				price.Source, price.AsOf),
		})
	}
	return checks
}

// formatUSDPerMTok renders a prices.USDPerMTok (micro-USD per 1M tokens)
// back as a USD-per-1M-tokens decimal string for doctor's own display
// line, the inverse of prices.ParseUSDPerMTok.
func formatUSDPerMTok(v prices.USDPerMTok) string {
	whole := int64(v) / 1_000_000
	frac := int64(v) % 1_000_000
	if frac == 0 {
		return fmt.Sprintf("%d", whole)
	}
	s := fmt.Sprintf("%06d", frac)
	s = strings.TrimRight(s, "0")
	return fmt.Sprintf("%d.%s", whole, s)
}

func doctorRoutesModeChecks(ctx context.Context, in doctorInputs, image string) []doctorCheck {
	settings := in.settings
	if settings.Roles == nil {
		// This function only ever runs when a model will actually be
		// called (doctorChecksFor's own !in.presenceOnly && !in.noRelay
		// gate) -- unlike sessionconfig.ValidateRouting's own "no
		// routes:/models:/roles: at all is valid" rule (correct for an
		// offline build, which never reaches here), a relay-needing
		// invocation with no roles.execution at all WILL fail its real
		// launch, so this must report that here rather than silently
		// returning nil (found via review: a config missing routes:/
		// models:/roles: entirely still reported a clean "roles resolve"
		// OK from doctorCheckRolesResolve and no checks at all from this
		// function, giving a fully clean preflight for a run that could
		// never actually launch).
		return []doctorCheck{{
			Name: "roles.execution",
			Err:  errors.New("roles.execution is not configured: add routes:/models:/roles: (see USAGE_REFERENCE.md \"Model routes\")"),
			Fix:  "add routes:/models:/roles: to session config (see USAGE_REFERENCE.md \"Model routes\")",
		}}
	}
	var checks []doctorCheck
	type pairKey struct{ route, model, harness string }
	seenPair := map[pairKey]bool{}
	seenRouteCredential := map[string]bool{}
	seenUpstream := map[string]bool{}
	roleSets, err := harnessRoleSets(settings)
	if err != nil {
		return []doctorCheck{{Name: "harness", Err: err}}
	}
	for _, spec := range doctorRoleSpecs {
		if !doctorRoleConfigured(settings, spec.name) {
			continue
		}
		defaultHarness, _ := modelrole.RoleHarness(settings, spec.role, "")
		// Every harness the role can resolve to is checked, not just its
		// default: a per-request pick from allowed_harnesses would otherwise
		// first fail at the build.
		for _, hd := range roleSets[spec.name] {
			sel, err := modelrole.SelectRoute(settings, spec.role, "", hd.Name, "", doctorRouteCredentialProbe)
			if err != nil {
				checks = append(checks, doctorCheck{
					Name: fmt.Sprintf("roles.%s: route selection", spec.name),
					Err:  err,
				})
				continue
			}
			for _, skip := range sel.Skipped {
				if hd.Name != defaultHarness {
					break // the same skips were already reported for the default
				}
				checks = append(checks, doctorCheck{
					Name:     fmt.Sprintf("roles.%s: route %s skipped", spec.name, skip.Route),
					Err:      errors.New(skip.Reason),
					Advisory: true,
				})
			}
			key := pairKey{route: sel.RouteName, model: sel.ModelName, harness: sel.Harness}
			if seenPair[key] {
				continue
			}
			seenPair[key] = true
			harnessDescriptor, err := harness.Lookup(sel.Harness)
			if err != nil {
				checks = append(checks, doctorCheck{Name: fmt.Sprintf("roles.%s: harness", spec.name), Err: err})
				continue
			}

			dr := doctorRoute{
				harness:                harnessDescriptor,
				label:                  fmt.Sprintf("route %s, model %s", sel.RouteName, sel.ModelName),
				routeName:              sel.RouteName,
				modelName:              sel.ModelName,
				credentialMode:         sel.Policy.AuthMode,
				upstream:               sel.Policy.Upstream,
				allowedPathPrefix:      sel.Policy.AllowedPathPrefix,
				workerBasePath:         sel.Policy.WorkerBasePath,
				workerAPI:              sel.Policy.WorkerModelAPI,
				workerModelID:          sel.Policy.WorkerModelID,
				workerModelExtraJSON:   sel.Policy.WorkerModelExtraJSON,
				githubTokenFile:        sel.Route.GitHubTokenFile,
				githubTokenKey:         sel.Route.GitHubTokenKey,
				codexAuthFile:          sel.Route.CodexAuthFile,
				allowPlaintextUpstream: sel.Policy.AllowPlaintextUpstream,
				keys:                   doctorRoutesModeRouteKeys(sel.RouteName, sel.ModelName),
			}
			runCredentialChecks := !seenRouteCredential[sel.RouteName]
			seenRouteCredential[sel.RouteName] = true
			runUpstreamProbes := !seenUpstream[sel.Policy.Upstream]
			seenUpstream[sel.Policy.Upstream] = true
			checks = append(checks, doctorRouteChecks(ctx, in, image, dr, runCredentialChecks, runUpstreamProbes)...)
		}
	}
	return checks
}

// doctorChecksFailed counts the same failures runDoctorChecks' own N/M
// summary line does (an Advisory failure never counts), without printing
// anything -- for a caller that needs to know before doing something
// with a side effect (see runWorkerDoctorPreflight's own -data-dir
// mount probe, deferred until every check run ahead of it has already
// passed, so creating -data-dir to run that probe never happens on a
// refusal for an unrelated reason).
func doctorChecksFailed(checks []doctorCheck) int {
	var failed int
	for _, c := range checks {
		if c.Err != nil && !c.Advisory {
			failed++
		}
	}
	return failed
}

// runDoctorChecks prints one ok/FAIL line per check (plus its fix, when
// one is named) and the N/M summary, returning how many failed. Shared
// by `factoryd doctor` and the init/onboard preflight
// (runInitDoctorPreflight) so both print the identical report.
func runDoctorChecks(checks []doctorCheck, w io.Writer) (failed int) {
	var warned int
	for _, c := range checks {
		switch {
		case c.Err == nil && c.Detail != "":
			fmt.Fprintf(w, "ok    %s (%s)\n", c.Name, c.Detail)
		case c.Err == nil:
			fmt.Fprintf(w, "ok    %s\n", c.Name)
		case c.Advisory:
			// Never counted in failed below -- mirrors
			// project_check.go's own "Advisory failures ... never fail the
			// overall check" rule for ProjectCheckResult.Advisory. Counted
			// in warned, though (found via Codex review of PR #142, P2):
			// the summary line below used to count every non-failed check
			// as "passed", including one that had just printed "warn" --
			// an operator or output consumer reading only the summary
			// would see e.g. "3/3 checks passed" and never learn a warning
			// was even printed above it.
			warned++
			fmt.Fprintf(w, "warn  %s: %s\n", c.Name, c.Err)
			if c.Fix != "" {
				fmt.Fprintf(w, "      fix: %s\n", c.Fix)
			}
		default:
			failed++
			fmt.Fprintf(w, "FAIL  %s: %s\n", c.Name, c.Err)
			if c.Fix != "" {
				fmt.Fprintf(w, "      fix: %s\n", c.Fix)
			}
		}
		if c.Use != "" {
			fmt.Fprintf(w, "      use: %s\n", c.Use)
		}
	}
	passed := len(checks) - failed - warned
	if warned > 0 {
		fmt.Fprintf(w, "\n%d/%d checks passed (%d warning(s))\n", passed, len(checks), warned)
	} else {
		fmt.Fprintf(w, "\n%d/%d checks passed\n", passed, len(checks))
	}
	return failed
}

// initChecks builds the environment preflight initMain and
// onboardMain run before writing anything: Docker reachable, the sandbox image present, and --
// the colima trap a new Mac engineer actually hits -- dir visible from
// inside a container. a boundary method so the in-process init/onboard
// tests can stub it instead of needing a live Docker daemon.
func (impl realDocker) initChecks(ctx context.Context, dockerBinary, image, dir string) []doctorCheck {
	checks := []doctorCheck{doctorCheckDockerReachable(ctx, dockerBinary)}
	if image == "" {
		return append(checks, doctorCheck{
			Name: "sandbox image",
			Err:  errors.New("no sandbox image configured"),
			Fix:  "run `make install` from the buildgate checkout (builds images from source and records them via `factoryd configure-images`), or pass -sandbox-image",
		})
	}
	return append(checks,
		doctorCheckImagePresent(ctx, dockerBinary, "sandbox image", image,
			"images are built from source, never pulled from a registry -- run `make install` (or `make sandbox-image`) to build it"),
		doctorCheckMountVisibility(ctx, dockerBinary, image, dir),
	)
}

// runInitDoctorPreflight runs docker.initChecks against dir and refuses
// (without the caller having written anything) when any fails.
func runInitDoctorPreflight(dp *deps, dockerBinary, image, dir string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	fmt.Println("Running factoryd doctor first -- pass -skip-doctor to bypass:")
	if failed := runDoctorChecks(dp.docker.initChecks(ctx, dockerBinary, image, dir), os.Stdout); failed > 0 {
		return fmt.Errorf("environment not ready: %d doctor check(s) failed (see fixes above, or rerun with -skip-doctor)", failed)
	}
	fmt.Println()
	return nil
}

func doctorCheckDockerReachable(ctx context.Context, dockerBinary string) doctorCheck {
	name := "docker daemon reachable"
	cmd := exec.CommandContext(ctx, dockerBinary, "version", "--format", "{{.Server.Version}}")
	if out, err := cmd.CombinedOutput(); err != nil {
		return doctorCheck{Name: name, Err: fmt.Errorf("%s version: %s", dockerBinary, strings.TrimSpace(string(out))),
			Fix: dockerReachableFix()}
	}
	return doctorCheck{Name: name}
}

// dockerReachableFix names the per-platform fix for an unreachable Docker
// daemon: colima on macOS (the chosen Mac backend), the system Docker
// service elsewhere.
func dockerReachableFix() string {
	switch runtime.GOOS {
	case "darwin":
		return "run `brew install colima docker && colima start` (the Mac backend), and confirm -sandbox-docker names a real executable"
	case "linux":
		return "run `sudo systemctl start docker` (and add yourself to the docker group), and confirm -sandbox-docker names a real executable"
	default:
		return "start Docker and confirm -sandbox-docker names a real executable"
	}
}

// doctorCheckBuildx confirms the `docker buildx` plugin (BuildKit) is
// present. Every image factoryd launches is built from source with
// `RUN --mount` and `--secret`, which the legacy builder rejects ("the
// --mount option requires BuildKit"). A Homebrew CLI-only Docker (docker +
// colima) ships without the plugin, so `make install` otherwise fails three
// image builds deep. Advisory: a host whose images are already built runs
// fine without it.
func doctorCheckBuildx(ctx context.Context, dockerBinary string) doctorCheck {
	const name = "docker buildx available (image builds)"
	out, err := exec.CommandContext(ctx, dockerBinary, "buildx", "version").CombinedOutput()
	if err != nil {
		return doctorCheck{Name: name, Advisory: true,
			Err: fmt.Errorf("%s buildx version: %s", dockerBinary, strings.TrimSpace(string(out))),
			Fix: dockerBuildxFix()}
	}
	return doctorCheck{Name: name}
}

// dockerBuildxFix names the per-platform fix for a missing buildx plugin.
func dockerBuildxFix() string {
	if runtime.GOOS == "darwin" {
		return "run `brew install docker-buildx && mkdir -p ~/.docker/cli-plugins && ln -sf \"$(brew --prefix)/lib/docker/cli-plugins/docker-buildx\" ~/.docker/cli-plugins/docker-buildx`"
	}
	return "install the Docker buildx plugin (docker-buildx-plugin on Debian/Ubuntu) -- see https://docs.docker.com/build/install-buildx/"
}

// doctorCheckComposeVersion confirms `docker compose` (the v2 CLI plugin,
// not the legacy standalone python `docker-compose`) is present and reports
// at least 2.20 -- the minimum internal/sandbox.ComposeServicesLifecycle
// relies on for `up --wait`. A host with only the legacy python tool has no
// `docker compose` subcommand at all, so its own "unknown command" failure
// already surfaces as this check's own Err, with a fix pointing at the v2
// plugin rather than at upgrading a binary that was never the right one.
func doctorCheckComposeVersion(ctx context.Context, dockerBinary string) doctorCheck {
	name := "docker compose v2 available (>= 2.20)"
	cmd := exec.CommandContext(ctx, dockerBinary, "compose", "version", "--short")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return doctorCheck{Name: name,
			Err: fmt.Errorf("%s compose version: %s", dockerBinary, strings.TrimSpace(string(out))),
			Fix: "install the Docker Compose v2 CLI plugin (not the legacy python docker-compose) -- see https://docs.docker.com/compose/install/"}
	}
	version := strings.TrimSpace(string(out))
	if !composeVersionAtLeast(version, 2, 20) {
		return doctorCheck{Name: name,
			Err: fmt.Errorf("docker compose version %q is older than the minimum 2.20", version),
			Fix: "upgrade the Docker Compose v2 CLI plugin to >= 2.20"}
	}
	return doctorCheck{Name: name}
}

// composeVersionAtLeast parses a "vMAJOR.MINOR.PATCH" (or "MAJOR.MINOR.PATCH")
// string as `docker compose version --short` reports it; any string it
// can't parse as at least MAJOR.MINOR is treated as too old, rather than
// erroring out and hiding a possibly-genuine version mismatch behind a
// parse failure.
func composeVersionAtLeast(version string, minMajor, minMinor int) bool {
	version = strings.TrimPrefix(version, "v")
	parts := strings.SplitN(version, ".", 3)
	if len(parts) < 2 {
		return false
	}
	major, majErr := strconv.Atoi(parts[0])
	minor, minErr := strconv.Atoi(parts[1])
	if majErr != nil || minErr != nil {
		return false
	}
	if major != minMajor {
		return major > minMajor
	}
	return minor >= minMinor
}

// doctorImagePullTimeout bounds a single doctorCheckImagePresent call,
// independent of any timeout the caller's own ctx carries. A package var
// (not const) so tests can shrink it to exercise the timeout path against
// a fake docker that sleeps, without waiting out the real default.
//
// A 2026-09-24 live run: `factoryd doctor` reported the sandbox image "not
// present locally and could not be pulled" and printed an empty `docker compose
// version` on a machine that was in fact still pulling three large,
// newly published images -- a manual `docker pull` of the same ref
// succeeded right after, and a doctor re-run (images now cached) was
// clean. Root cause: doctorRunChecks built ONE context.WithTimeout(...,
// 30*time.Second) shared across every check doctorChecksFor runs,
// including up to three sequential image pulls plus the `docker compose
// version` probe right after them -- so a slow first pull (a large,
// not-yet-cached image) consumed the shared budget, and every check that
// ran after it (including later pulls and doctorCheckComposeVersion) saw
// an already-expired context and failed with an empty/truncated command
// output rather than its own real result. Detaching each image pull onto
// its own independent, generously-sized timeout fixes both symptoms at
// once: the pull itself gets a real budget instead of whatever was left
// over, and every check queued after it stops losing its share of the
// original 30s to a slow pull it has nothing to do with.
var doctorImagePullTimeout = 2 * time.Minute

// doctorCheckImagePresent defers to sandbox.ImagePresent -- the exact
// function a real run's own -sandbox-image resolution
// already depends on -- rather than reimplementing this check with
// `docker manifest inspect`. That first attempt (2026-09-08) diverged on
// purpose to avoid downloading layers, but `docker manifest inspect`
// does not reliably work against a local insecure registry the way
// `docker image inspect` does: found live validating this exact check
// against this repo's own `make sandbox-image` local-
// registry workflow. Reusing ImagePresent means doctor and the real
// launch path can never disagree about whether an image is actually
// usable.
//
// Runs against its own doctorImagePullTimeout budget, deliberately
// detached from ctx's own deadline (see that var's own doc comment for
// why); the ctx parameter is kept only for signature consistency with
// every other doctorCheck* function in this file.
func doctorCheckImagePresent(ctx context.Context, dockerBinary, label, image, fix string) doctorCheck {
	name := fmt.Sprintf("%s present (%s)", label, image)
	probeCtx, cancel := context.WithTimeout(context.Background(), doctorImagePullTimeout)
	defer cancel()
	if sandbox.ImagePresent(probeCtx, dockerBinary, image) {
		return doctorCheck{Name: name}
	}
	if errors.Is(probeCtx.Err(), context.DeadlineExceeded) {
		return doctorCheck{Name: name,
			Err: fmt.Errorf("timed out after %s checking %s", doctorImagePullTimeout, image),
			Fix: fix}
	}
	return doctorCheck{Name: name, Err: errors.New("not present locally"), Fix: fix}
}

// doctorCheckExecutableInImage confirms command's leading executable is on
// PATH inside image -- the named-gate commands (lint_command,
// security_command, unit_test_command, integration_test_command,
// reference_oracle_command) are only useful if their tool is actually
// installed in the sandbox image a real
// run will execute them in; a missing one otherwise surfaces as a
// confusing gate failure deep inside a run instead of naming it here,
// before the run ever starts.
func doctorCheckExecutableInImage(ctx context.Context, dockerBinary, image, label, command string) doctorCheck {
	name := fmt.Sprintf("%s executable present in sandbox image (%s)", label, image)
	fields := strings.Fields(command)
	if len(fields) == 0 {
		return doctorCheck{Name: name, Err: fmt.Errorf("%s is set but empty", label)}
	}
	exe := fields[0]
	cmd := exec.CommandContext(ctx, dockerBinary, "run", "--rm",
		"--read-only", "--cap-drop=ALL", "--security-opt=no-new-privileges",
		image, "sh", "-c", "command -v "+exe)
	if out, err := cmd.CombinedOutput(); err != nil {
		return doctorCheck{
			Name: name,
			Err:  fmt.Errorf("%q not found on PATH in %s: %s", exe, image, strings.TrimSpace(string(out))),
			Fix:  fmt.Sprintf("install %s in the sandbox image, or change %s to a command whose executable is already there", exe, label),
		}
	}
	return doctorCheck{Name: name}
}

// doctorImage is one image `factoryd doctor` checks for pullability and,
// with -fix, can build locally: the check label, the flag it came from
// (named on the "use:" line after a build), the reference to check, and
// the Makefile target that builds and pushes it through the local
// registry, printing a digest-pinned reference as its last line.
type doctorImage struct {
	label, flag, image, makeTarget string
	// set records a freshly built reference into the inputs the regular
	// checks then run against.
	set func(ref string)
}

// makeImage runs `make <target> [vars...]` in repoRoot and returns
// its stdout; both streams also go straight to the terminal (as stderr,
// so the report itself stays parseable on stdout) so a multi-minute
// docker build is not silent. vars are additional "NAME=value" Make
// command-line variable assignments (e.g. "FACTORYD_CONFIG=<path>" for
// `local-images`, so `configure-images` writes to the same session
// config quickstart is using rather than always the default path). A
// boundary method so tests can stub the build instead of shelling out.
func (impl realDocker) makeImage(repoRoot, target string, vars ...string) (string, error) {
	var stdout strings.Builder
	args := append([]string{"-C", repoRoot, target}, vars...)
	cmd := exec.Command("make", args...)
	cmd.Stdout = io.MultiWriter(&stdout, os.Stderr)
	cmd.Stderr = os.Stderr
	err := cmd.Run()
	return stdout.String(), err
}

// doctorImageMakeTargets are the Makefile targets -fix dispatches to;
// resolveDoctorRepoRoot only accepts a directory whose Makefile defines
// all of them.
var doctorImageMakeTargets = []string{"sandbox-image", "registry-proxy-image"}

// resolveDoctorRepoRoot finds the checkout whose Makefile -fix (or
// quickstart's own rebuild path) should run: -repo-root, then
// $FACTORYD_REPO_ROOT, then imageSourceRoot (the session config's own
// image_source_root, when the caller has one -- see
// sessionconfig.Config.ImageSourceRoot's own doc comment), then the
// parent of the running binary's directory (executable, empty when
// unknown) when it holds a Makefile with the image targets. imageSourceRoot
// is checked before the executable heuristic, not after, because a normal
// `go install` copies the binary to $GOBIN (e.g. ~/go/bin), nowhere near
// any checkout -- found via adversarial review: without this, the
// executable heuristic always failed on an installed binary, so neither
// -fix nor quickstart's own missing/stale-image rebuild prompt could ever
// locate a checkout on the common post-`make install` machine. Nothing is
// guessed beyond that; the error tells the operator to pass -repo-root.
func resolveDoctorRepoRoot(flagValue, envValue, imageSourceRoot, executable string) (string, error) {
	if flagValue != "" {
		return flagValue, nil
	}
	if envValue != "" {
		return envValue, nil
	}
	if imageSourceRoot != "" {
		return imageSourceRoot, nil
	}
	if executable != "" {
		candidate := filepath.Join(filepath.Dir(executable), "..")
		if makefileHasTargets(filepath.Join(candidate, "Makefile"), doctorImageMakeTargets) {
			return candidate, nil
		}
	}
	return "", errors.New("no -repo-root, no $FACTORYD_REPO_ROOT, no recorded image_source_root, and the running binary's parent directory has no Makefile with the image targets")
}

func makefileHasTargets(path string, targets []string) bool {
	content, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	for _, target := range targets {
		if !strings.Contains("\n"+string(content), "\n"+target+":") {
			return false
		}
	}
	return true
}

// doctorImageRefPattern matches the digest-pinned reference each image
// Makefile target prints as its last line (e.g.
// localhost:5050/factoryd-relay@sha256:...).
var doctorImageRefPattern = regexp.MustCompile(`^[^\s@]+@sha256:[0-9a-f]{64}$`)

// doctorFixImageByBuilding is what -fix does for an absent image: run its
// Makefile target from repoRoot, read the digest-pinned reference that
// target prints last, re-run the pullability check against that
// reference, and return the check with its "use:" line set so the
// operator knows the exact value to configure. The built reference is
// also returned (empty on failure) so the sandbox-image probes that
// follow can run in the image that now exists.
func doctorFixImageByBuilding(dp *deps, dockerBinary string, im doctorImage, repoRoot string) (doctorCheck, string) {
	name := fmt.Sprintf("%s present (%s) -- built by make %s", im.label, im.image, im.makeTarget)
	out, err := dp.docker.makeImage(repoRoot, im.makeTarget)
	if err != nil {
		return doctorCheck{Name: name, Err: fmt.Errorf("make -C %s %s: %w", repoRoot, im.makeTarget, err),
			Fix: "read the build output above; the Makefile target needs Docker and a local registry it starts itself"}, ""
	}
	lines := strings.Fields(out)
	if len(lines) == 0 || !doctorImageRefPattern.MatchString(lines[len(lines)-1]) {
		return doctorCheck{Name: name, Err: fmt.Errorf("make %s did not print a digest-pinned reference as its last line", im.makeTarget)}, ""
	}
	built := lines[len(lines)-1]
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	check := doctorCheckImagePresent(ctx, dockerBinary, im.label, built, "")
	check.Name += fmt.Sprintf(" -- built by make %s", im.makeTarget)
	check.Use = im.flag + " " + built
	return check, built
}

// doctorCheckRelayUpstreamPathComposition is the regression check for the
// real bug found live 2026-09-08: the relay appends the inbound request
// path onto whatever path the route's own upstream URL already has
// rather than replacing it, so an OpenAI-compatible route (one whose
// model has a worker model id) whose upstream already includes a path
// doubles it and 404s upstream -- reported by build_app.py only as the
// generic, indistinguishable-from-a-real-outage "model route
// unreachable". Only meaningful for the OpenAI-compatible path: the
// default Anthropic-shaped path's own fixed request path
// ("/v1/messages") is designed to compose with an empty-path upstream, so
// this check is skipped entirely when the route's worker model id is unset.
// doctorFixAbsentImages is `factoryd doctor -fix`'s pre-step: for each
// configured image that is absent, build it with its Makefile target and
// substitute the digest-pinned reference the build printed into the
// inputs the regular checks then run against, returning one check line
// per build. Nothing is built when the Docker daemon itself is
// unreachable -- that is what doctorChecksFor will report, and a build
// would only fail the same way after wasting the operator's time (Sonnet
// review). Each pullability probe here has its own short context; the
// build itself has none, a real image build takes minutes.
func doctorFixAbsentImages(dp *deps, in doctorInputs, repoRootFlag string) ([]doctorCheck, doctorInputs) {
	probe := func(f func(context.Context) doctorCheck) doctorCheck {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		return f(ctx)
	}
	if docker := probe(func(ctx context.Context) doctorCheck { return doctorCheckDockerReachable(ctx, in.sandboxDocker) }); docker.Err != nil {
		return nil, in
	}
	roleHarnesses, err := in.roleHarnessSets()
	if err != nil {
		return []doctorCheck{{Name: "harness", Err: err}}, in
	}
	images := []doctorImage{}
	if doctorHarnessNeedingOwnImage(roleHarnesses) == nil {
		images = append(images, doctorImage{label: "sandbox image", flag: "-sandbox-image", image: in.sandboxImage, makeTarget: "sandbox-image", set: func(ref string) { in.sandboxImage = ref }})
	}
	if in.registryProxyImage != "" {
		images = append(images, doctorImage{label: "registry proxy image", flag: "-registry-proxy-image", image: in.registryProxyImage, makeTarget: "registry-proxy-image", set: func(ref string) { in.registryProxyImage = ref }})
	}
	var out []doctorCheck
	resolvedRepoRoot := ""
	for _, im := range images {
		present := probe(func(ctx context.Context) doctorCheck {
			return doctorCheckImagePresent(ctx, in.sandboxDocker, im.label, im.image, "")
		})
		if present.Err == nil {
			continue
		}
		if resolvedRepoRoot == "" {
			exe, _ := os.Executable()
			root, err := resolveDoctorRepoRoot(repoRootFlag, os.Getenv("FACTORYD_REPO_ROOT"), in.imageSourceRoot, exe)
			if err != nil {
				out = append(out, doctorCheck{Name: fmt.Sprintf("%s -- build with -fix", im.label), Err: fmt.Errorf("-fix could not locate the Makefile: %w", err),
					Fix: "pass -repo-root <this repository's checkout> (or set FACTORYD_REPO_ROOT)"})
				continue
			}
			resolvedRepoRoot = root
		}
		check, ref := doctorFixImageByBuilding(dp, in.sandboxDocker, im, resolvedRepoRoot)
		out = append(out, check)
		if ref != "" {
			im.set(ref)
		}
	}
	return out, in
}

func doctorCheckRelayUpstreamPathComposition(relayUpstream, relayWorkerModelID string, keys doctorRouteKeys) doctorCheck {
	name := "relay upstream path composition"
	if relayWorkerModelID == "" {
		return doctorCheck{Name: name}
	}
	parsed, err := url.Parse(relayUpstream)
	if err != nil {
		return doctorCheck{Name: name, Err: fmt.Errorf("parse %s: %w", keys.upstream, err)}
	}
	if parsed.Path != "" && parsed.Path != "/" {
		return doctorCheck{Name: name, Err: fmt.Errorf("%s has a path (%q) set alongside %s", keys.upstream, parsed.Path, keys.workerModelID),
			Fix: fmt.Sprintf("the relay appends the inbound request path onto this URL's own path rather than replacing it -- point %s at the bare API root with no path (e.g. \"http://host:port\", not \".../v1\") and let %s/%s supply the path instead", keys.upstream, keys.workerBasePath, keys.allowedPathPrefix)}
	}
	return doctorCheck{Name: name}
}

// doctorCheckRelayUpstreamHostResolvesInSandbox converts the Tailscale
// hostname-vs-IP gotcha from a USAGE.md prose row into an
// enforced check: a Tailscale MagicDNS short name can resolve fine from
// this host's own DNS (Tailscale's own 100.100.100.100 resolver, wired
// into the host's resolver config) while resolving to nothing from inside
// a sandboxed container, whose DNS goes through whatever the configured
// Docker backend gives it -- a distinct failure mode from
// doctorCheckRelayUpstreamReachableFromSandbox's own HTTP probe just below,
// which reports any DNS failure as the same generic connection error as a
// firewalled port or a down upstream. Resolves the host with `getent
// hosts` run *inside* a throwaway container on the Docker backend's
// default network, never from this host's own resolver -- host-side
// resolution proves nothing about what a sandbox on that backend will
// actually see. Skipped entirely when
// a route's own upstream's host is already a literal IP address, since there is
// nothing to resolve.
func doctorCheckRelayUpstreamHostResolvesInSandbox(ctx context.Context, dockerBinary, image, relayUpstream string, keys doctorRouteKeys) doctorCheck {
	const name = "relay upstream host resolves from inside the sandbox"
	parsed, err := url.Parse(relayUpstream)
	if err != nil {
		return doctorCheck{Name: name, Err: fmt.Errorf("parse %s: %w", keys.upstream, err)}
	}
	host := parsed.Hostname()
	if host == "" || net.ParseIP(host) != nil {
		return doctorCheck{Name: name}
	}
	checkName := fmt.Sprintf("%s (%s)", name, host)
	cmd := exec.CommandContext(ctx, dockerBinary, "run", "--rm",
		"--read-only", "--cap-drop=ALL", "--security-opt=no-new-privileges",
		"--entrypoint", "getent", image, "hosts", host)
	if out, err := cmd.CombinedOutput(); err != nil {
		return doctorCheck{
			Name: checkName,
			Err:  fmt.Errorf("container DNS cannot resolve %q; use the Tailscale IP or FQDN %s.<tailnet>.ts.net", host, host),
			Fix:  fmt.Sprintf("pass %s a literal Tailscale IP, or the FQDN %s.<tailnet>.ts.net, instead of the MagicDNS short name -- a sandboxed container's DNS resolver often doesn't share the host's own Tailscale MagicDNS config: %s", keys.upstream, host, strings.TrimSpace(string(out))),
		}
	}
	return doctorCheck{Name: checkName}
}

// doctorCheckRelayUpstreamReachableFromSandbox is the regression check for
// a real gap found live, 2026-09-08: a route's own upstream being reachable from
// this host does not mean it is reachable from inside a sandboxed
// container. This host's own Docker backend (colima) only routes a
// container to the physical LAN via $HOME-shared-VM networking that does
// not extend to arbitrary LAN addresses -- a Tailscale address
// (100.64.0.0/10, see isPrivateOrLoopbackHost's own doc comment) worked
// fine from inside a container while the identical host's plain LAN IP
// got a bare connection refused. Nothing before this check ever actually
// dialed a route's own upstream from inside a container: doctorCheckDockerReachable
// and doctorCheckImagePresent only ever exercise the host's own Docker
// CLI, and doctorCheckRelayUpstreamPathComposition is a pure string check.
// Without this, the very first sign of the problem was build_app.py's own
// generic "model route unreachable" after a full ~45-minute round was
// already burned -- indistinguishable from a genuine model outage, a
// misconfigured credential, or the path-composition bug above.
//
// Probes from a throwaway container on the Docker backend's default
// network, using the sandbox/worker image. The probe runs only its own fixed
// script below, never build_app.py/pi or any other attacker-influenced
// code, holds no credential, and is --rm/read-only/cap-dropped/
// no-new-privileges -- but only because --entrypoint python3 forces that to
// actually be true. Found via a real GitHub Codex App review of PR #69, P1:
// -sandbox-image is operator-supplied and can be a custom project image
// (make project-sandbox-image); without --entrypoint, "python3 -c <script>"
// is only the container's CMD, not its actual startup command, and Docker
// runs whatever ENTRYPOINT that image itself declares with CMD as its
// arguments. --entrypoint python3 makes the actual startup command
// unconditionally python3 regardless of what the image itself declares.
//
// Any real HTTP response (even a 404/405 from an upstream that doesn't
// recognize this exact path) proves the network path itself works, so an
// urllib HTTPError does not fail this check -- only a connection-level
// failure (refused, timed out, no route, unresolvable host) does, since
// those are exactly what "model route unreachable" collapses onto today.
func doctorCheckRelayUpstreamReachableFromSandbox(ctx context.Context, dockerBinary, image, relayUpstream, workerBasePath, workerModelID, caBundlePath string, keys doctorRouteKeys) doctorCheck {
	name := fmt.Sprintf("relay upstream reachable from inside the sandbox (%s)", relayUpstream)
	// With a worker model id, the probe asks the question the worker will
	// actually ask -- GET <upstream><base-path>/models -- and requires the
	// id to be listed, instead of accepting any HTTP response at all.
	// Found live 2026-09-10: a stray dev server on the operator's own
	// Tailscale IP answered "404 page not found" to everything, this check
	// reported it reachable, and the first submit-to-PR run then failed
	// its one model turn against it. Without a model id (an Anthropic-
	// shaped upstream) any HTTP response still counts, as before; with
	// one, a 401/403 also counts -- an authenticated OpenAI-compatible
	// endpoint answers that to this credential-less probe while the real
	// relay, which injects the credential, would succeed (Codex review of
	// PR #98) -- so only another error status or a listing without the
	// id fails.
	probeURL := relayUpstream
	expectModel := ""
	if workerModelID != "" {
		probeURL = strings.TrimRight(relayUpstream, "/") + "/" + strings.Trim(workerBasePath, "/") + "/models"
		expectModel = workerModelID
		name = fmt.Sprintf("relay upstream lists the worker model from inside the sandbox (%s, %s)", probeURL, workerModelID)
	}
	containerCABundlePath := ""
	if caBundlePath != "" {
		containerCABundlePath = sandbox.EgressCABundleContainerPath
	}
	probe := fmt.Sprintf(`
import sys, json, ssl, urllib.request, urllib.error
url, expect, cafile = %q, %q, %q
# ssl.create_default_context() already loads the system's own trust store;
# load_verify_locations ADDS cafile to it rather than replacing it (unlike
# SSL_CERT_FILE, which replaces the trust store wholesale in Python too) --
# found via adversarial review: an upstream the corporate proxy does not
# intercept must still verify against public CAs.
ctx = ssl.create_default_context()
if cafile:
    ctx.load_verify_locations(cafile=cafile)
try:
    body = urllib.request.urlopen(url, timeout=5, context=ctx).read()
except urllib.error.HTTPError as e:
    if expect and e.code not in (401, 403):
        print("HTTP %%d from %%s" %% (e.code, url), file=sys.stderr)
        sys.exit(1)
    # 401/403: the route is there but wants the credential only the
    # relay injects; the model listing cannot be checked from here.
    sys.exit(0)
except Exception as e:
    print(str(e), file=sys.stderr)
    sys.exit(1)
if expect:
    try:
        ids = [m.get("id") for m in json.loads(body).get("data", [])]
    except Exception as e:
        print("could not parse a model list from %%s: %%s" %% (url, e), file=sys.stderr)
        sys.exit(1)
    if expect not in ids:
        print("model %%r is not listed at %%s (listed: %%s)" %% (expect, url, ids), file=sys.stderr)
        sys.exit(1)
`, probeURL, expectModel, containerCABundlePath)
	args := []string{"run", "--rm",
		"--read-only", "--cap-drop=ALL", "--security-opt=no-new-privileges"}
	// So this probe trusts the operator's corporate CA, and fails the same
	// way a real run would without -egress-ca-bundle -- but only
	// mounted, no SSL_CERT_FILE env var: the probe script above adds cafile
	// to Python's own default trust store via load_verify_locations rather
	// than replacing it, so an upstream the corporate proxy does not
	// intercept still verifies against public CAs.
	if caBundlePath != "" {
		args = append(args, "--volume", caBundlePath+":"+sandbox.EgressCABundleContainerPath+":ro")
	}
	args = append(args, "--entrypoint", "python3", image, "-c", probe)
	cmd := exec.CommandContext(ctx, dockerBinary, args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		fix := fmt.Sprintf("a host that can reach %s directly does not guarantee a sandboxed container can too -- e.g. colima's own Docker backend does not route a container to the physical LAN at all, only to loopback/private/Tailscale (100.64.0.0/10) addresses; point %s at an address reachable from inside a container (a literal Tailscale IP, not a LAN hostname or IP) or reconfigure the Docker backend's own networking", keys.upstream, keys.upstream)
		if expectModel != "" {
			fix = fmt.Sprintf("confirm %s is the model host itself (not another machine's address that happens to answer on the same port -- check `tailscale ip -4 <host>` against the host running the model, not this machine's own IP) and that %s is one of the ids GET %s returns; %s", keys.upstream, keys.workerModelID, probeURL, fix)
		}
		return doctorCheck{Name: name, Err: fmt.Errorf("the probe container could not confirm it: %s", strings.TrimSpace(string(out))), Fix: fix}
	}
	return doctorCheck{Name: name}
}

// doctorCheckEgressCABundle confirms -egress-ca-bundle names a readable
// file that parses as at least one PEM certificate, before it is trusted
// to mount cleanly into a real relay/registry-proxy container -- the same
// fail-fast-at-startup rule this file already applies to every other
// misconfiguration (a bad path or non-PEM file otherwise only surfaces as
// an opaque TLS handshake failure deep inside a launched container).
func doctorCheckEgressCABundle(path string) doctorCheck {
	name := fmt.Sprintf("egress CA bundle parses (%s)", path)
	if err := sandbox.ValidateEgressCABundle(path); err != nil {
		return doctorCheck{Name: name, Err: err, Fix: "-egress-ca-bundle must name a PEM file containing at least one certificate, e.g. your corporate TLS-interception proxy's own CA"}
	}
	return doctorCheck{Name: name}
}

// doctorCheckCopilotTokenExchange is -relay-credential-mode=github-copilot's
// own check, replacing doctorCheckRelayUpstreamReachableFromSandbox for this
// mode: rather than probe Copilot's /models endpoint from inside a bare
// sandbox container (which would need to send the same Editor-Version/
// Copilot-Integration-Id/etc headers this relay already sends just to get
// past Copilot's own request validation -- not worth reimplementing twice),
// this performs the real token exchange directly, host-side, with the
// exact credential -relay-github-token-file/GITHUB_COPILOT_TOKEN resolves
// to. Success here proves the configured GitHub OAuth token is valid and
// Copilot-entitled; it does not itself prove the sandboxed relay container
// can reach api.github.com/api.individual.githubcopilot.com from inside
// its own egress network, the same caveat every doctor check that runs
// host-side (as opposed to doctorCheckRelayUpstreamReachableFromSandbox,
// which deliberately runs inside a probe container) already carries.
// caBundlePath is threaded through to meter.ExchangeGitHubCopilotToken so
// this check succeeds or fails exactly like a real relay configured with
// the same -egress-ca-bundle would (see that function's own doc comment).
func doctorCheckCopilotTokenExchange(ctx context.Context, tokenFile, tokenKey, caBundlePath string, keys doctorRouteKeys) doctorCheck {
	name := "github copilot token exchange"
	fix := fmt.Sprintf("set %s to a path to pi's auth.json, or set GITHUB_COPILOT_TOKEN, to a valid GitHub OAuth token with Copilot access", keys.githubTokenFile)
	token, err := resolveGitHubCopilotToken(tokenFile, tokenKey)
	if err != nil {
		return doctorCheck{Name: name, Err: err, Fix: fix}
	}
	if token == "" {
		return doctorCheck{Name: name, Err: errors.New("no GitHub OAuth token configured"), Fix: fix}
	}
	if _, err := meter.ExchangeGitHubCopilotToken(ctx, caBundlePath, token); err != nil {
		return doctorCheck{Name: name, Err: fmt.Errorf("token exchange failed: %w", err), Fix: fix}
	}
	return doctorCheck{Name: name}
}

// doctorCheckCopilotModelListed is a regression check for a walkthrough
// finding: `doctor` passed -relay-credential-mode=github-copilot with
// relay_worker_model_id: placeholder-model, because
// doctorCheckCopilotTokenExchange only ever proves the OAuth token itself
// is valid, never that the CONFIGURED model id is one this account is
// actually entitled to. Runs only when modelID is non-empty (mirrors
// doctorCheckContextWindowConfigured's own gating) -- an unset model id is
// preset.RequiresWorkerModel's own check's problem, not this one's. Lists
// up to 5 real ids in Fix so a FAIL is actionable without a second lookup.
//
// relayUpstream is passed straight to meter.ListGitHubCopilotModels so the
// listing comes from the SAME host a real request would (empty resolves
// to the Individual-plan default, meter.CopilotAPIBase, exactly like
// applyGitHubCopilotRelayDefaults does for a real run). When relayUpstream
// is explicitly set to something other than that default -- a
// Business/Enterprise Copilot host -- this relay has never live-verified
// that host's own /models route serves the identical listing shape a
// real request's own upstream does (round-2 review finding), so a
// "not entitled" verdict downgrades to Advisory (a WARN, not a FAIL) in
// that case rather than risking a false block on an operator's real
// account.
//
// A round-3 review: this check runs on every plain `factoryd
// doctor` whenever -relay-credential-mode is github-copilot, so a stale
// relay_upstream left over from a local-model route (e.g.
// "http://local-model:8080") must never actually receive the exchanged
// Copilot API token -- doctorValidateCopilotUpstream applies exactly the
// same sandbox.RoutePolicy.ValidateUpstreamScheme rule a real relay
// launch would, BEFORE resolveGitHubCopilotToken/the token exchange ever
// runs, so an invalid upstream never even causes a GitHub API call, let
// alone a Copilot-token-bearing one. allowPlaintextUpstream mirrors
// relay_allow_plaintext_upstream/-relay-allow-plaintext-upstream.
func doctorCheckCopilotModelListed(ctx context.Context, tokenFile, tokenKey, caBundlePath, modelID, relayUpstream, workerAPI string, allowPlaintextUpstream bool, keys doctorRouteKeys) doctorCheck {
	name := fmt.Sprintf("github copilot lists the configured worker model (%s)", modelID)
	nonDefaultUpstream := relayUpstream != "" && relayUpstream != meter.CopilotAPIBase
	effectiveUpstream := effectiveCopilotUpstream(relayUpstream)
	fallbackFix := fmt.Sprintf("set %s to one of the ids GET %s/models lists for this account (see `factoryd doctor -list-models`)", keys.workerModelID, effectiveUpstream)
	if err := doctorValidateCopilotUpstream(effectiveUpstream, allowPlaintextUpstream); err != nil {
		return doctorCheck{Name: name, Err: err, Fix: fmt.Sprintf("point %s at Copilot's own API host (unset it to use the individual-plan default), or set %s only for a private/loopback host with no real credential", keys.upstream, keys.allowPlaintextUpstream)}
	}
	token, err := resolveGitHubCopilotToken(tokenFile, tokenKey)
	if err != nil {
		return doctorCheck{Name: name, Err: err, Fix: fallbackFix}
	}
	models, err := listGitHubCopilotModelsFn(ctx, caBundlePath, token, relayUpstream)
	if err != nil {
		return doctorCheck{Name: name, Err: fmt.Errorf("list copilot models: %w", err), Fix: fallbackFix, Advisory: nonDefaultUpstream}
	}
	ids := copilotServableModelIDs(models, workerAPI, 5)
	for _, m := range models {
		if m.ID != modelID {
			continue
		}
		if ok, reason, fix := meter.CopilotModelUsable(m, workerAPI); !ok {
			// Listed but unusable (see meter.CopilotModelUsable): found live
			// in the 2026-09-25 closing walk, where two listed models passed
			// this check and then failed spec drafting.
			// Advisory on a non-default upstream, like the not-entitled
			// verdict below: that host's listing shape is unverified live.
			why := reason
			if fix != "" {
				why += "; " + fix
			}
			return doctorCheck{
				Name:     name,
				Err:      fmt.Errorf("%s %q is listed but not usable: %s", keys.workerModelID, modelID, why),
				Fix:      fmt.Sprintf("set %s to a usable model%s (see `factoryd doctor -list-models`)", keys.workerModelID, copilotSuggestionClause(ids)),
				Advisory: nonDefaultUpstream,
			}
		}
		return doctorCheck{Name: name}
	}
	fix := fmt.Sprintf("set %s to one of the entitled ids%s (see `factoryd doctor -list-models` for the full list)", keys.workerModelID, copilotSuggestionClause(ids))
	if nonDefaultUpstream {
		fix += fmt.Sprintf("; %s is a non-default host, so this list may not be authoritative for it -- verify against your account's own Copilot model configuration too", keys.upstream)
	}
	return doctorCheck{
		Name:     name,
		Err:      fmt.Errorf("%s %q is not in this account's entitled Copilot models", keys.workerModelID, modelID),
		Fix:      fix,
		Advisory: nonDefaultUpstream,
	}
}

// copilotServableModelIDs returns up to limit ids from models that this
// relay can actually run under the currently configured workerAPI
// (meter.CopilotModelUsable), for doctor's own fix lines, which must
// never suggest a model unusable under the operator's current
// relay_worker_api without also telling them to change it.
//
// Preview models and models with no context window (embeddings) are never
// suggested, and models the Copilot picker itself offers come first -- the
// closing walk's first suggestions were internal preview ids
// (copilot-search-a, exec-agent-a).
func copilotServableModelIDs(models []meter.CopilotModel, workerAPI string, limit int) []string {
	return copilotFilterServableModelIDs(models, func(m meter.CopilotModel) bool {
		ok, _, _ := meter.CopilotModelUsable(m, workerAPI)
		return ok
	}, limit)
}

// copilotServableModelIDsAnyAPI is copilotServableModelIDs's own filter,
// but usable under WHICHEVER relay_worker_api a model needs
// (meter.CopilotModelUsableAnyAPI) rather than one fixed value --
// quickstart's own suggestion list, unlike doctor's fixed-config one, can
// simply write whichever relay_worker_api the suggested model needs, so a
// responses-only model (e.g. gpt-5.6-luna) belongs in it too.
func copilotServableModelIDsAnyAPI(models []meter.CopilotModel, limit int) []string {
	return copilotFilterServableModelIDs(models, func(m meter.CopilotModel) bool {
		_, ok, _ := meter.CopilotModelUsableAnyAPI(m)
		return ok
	}, limit)
}

// copilotFilterServableModelIDs is copilotServableModelIDs/
// copilotServableModelIDsAnyAPI's own shared preview/embedding filter and
// picker-first ordering, parameterised on what "usable" means for the
// caller.
func copilotFilterServableModelIDs(models []meter.CopilotModel, usable func(meter.CopilotModel) bool, limit int) []string {
	var picker, rest []string
	for _, m := range models {
		if !usable(m) || m.Preview || m.ContextWindow == 0 {
			continue
		}
		if m.ModelPickerEnabled {
			picker = append(picker, m.ID)
		} else {
			rest = append(rest, m.ID)
		}
	}
	ids := append(picker, rest...)
	if len(ids) == 0 {
		// Nothing passes the preview/embedding filter: fall back to any
		// usable id rather than suggesting none.
		for _, m := range models {
			if usable(m) {
				ids = append(ids, m.ID)
			}
		}
	}
	if len(ids) > limit {
		ids = ids[:limit]
	}
	return ids
}

// copilotSuggestionClause is ", e.g.: <ids>", or "" when ids is empty, so
// a fix line never reads "e.g.:  (see ...)".
func copilotSuggestionClause(ids []string) string {
	if len(ids) == 0 {
		return ""
	}
	return ", e.g.: " + strings.Join(ids, ", ")
}

// effectiveCopilotUpstream is relayUpstream, or meter.CopilotAPIBase when
// relayUpstream is empty -- the same default meter.ListGitHubCopilotModels
// and applyGitHubCopilotRelayDefaults both apply, factored out so
// doctorCheckCopilotModelListed's own Fix text names the URL actually
// queried rather than always the individual-plan one.
func effectiveCopilotUpstream(relayUpstream string) string {
	if relayUpstream == "" {
		return meter.CopilotAPIBase
	}
	return relayUpstream
}

// doctorCheckChatGPTCodexCredential is -relay-credential-mode=chatgpt-codex's
// own check: unlike doctorCheckCopilotTokenExchange, there is no token-
// exchange endpoint to probe (see meter.CredentialModeChatGPTCodex's own
// doc comment -- this mode forwards the operator's ChatGPT access token
// verbatim, with no refresh), so this reads and validates the codex auth
// file exactly the way resolveChatGPTCodexCredential does at relay-launch
// time: auth_mode, both token fields, and the access token's own expiry
// margin. No network call, and the token itself is never included in the
// check's own output.
func doctorCheckChatGPTCodexCredential(authFile string, keys doctorRouteKeys) doctorCheck {
	name := "chatgpt codex credential"
	fix := fmt.Sprintf("run any `codex` command on the host to log in / refresh its ChatGPT OAuth login, or set %s", keys.codexAuthFile)
	if _, _, err := resolveChatGPTCodexCredential(authFile); err != nil {
		return doctorCheck{Name: name, Err: err, Fix: fix}
	}
	return doctorCheck{Name: name}
}

// doctorCheckContextWindowConfigured is the regression check for the
// other real bug found live 2026-09-08 (see
// -relay-worker-model-extra-json's own doc comment): without a configured
// contextWindow, pi never knows when to auto-compact a local-model
// route's own conversation, and context grows unbounded across
// --continue rounds until the upstream's own admission limit hard-rejects
// the request -- again surfacing only as "model route unreachable".
func doctorCheckContextWindowConfigured(extraJSON string, keys doctorRouteKeys) doctorCheck {
	name := "contextWindow set for local-model route"
	fix := fmt.Sprintf("add \"contextWindow\": <the upstream's own real per-request admission budget> to %s -- see USAGE_REFERENCE.md for a live-verified example", keys.extraJSON)
	if strings.TrimSpace(extraJSON) == "" {
		return doctorCheck{Name: name, Err: fmt.Errorf("%s is empty", keys.extraJSON), Fix: fix}
	}
	var parsed struct {
		ContextWindow *float64 `json:"contextWindow"`
	}
	if err := json.Unmarshal([]byte(extraJSON), &parsed); err != nil {
		return doctorCheck{Name: name, Err: fmt.Errorf("parse %s: %w", keys.extraJSON, err)}
	}
	// Presence alone is not enough (found via Codex review, PR #64):
	// {"contextWindow":null}, {"contextWindow":0}, or
	// {"contextWindow":"large"} all satisfy a bare key-existence check
	// while supplying no positive numeric admission budget -- the exact
	// value pi actually needs to know when to auto-compact (see this
	// flag's own doc comment on the live incident this check exists to
	// catch). A wrong-typed value fails json.Unmarshal into *float64
	// outright rather than silently decoding as zero, so that case is
	// reported the same as a missing one below.
	if parsed.ContextWindow == nil {
		return doctorCheck{Name: name, Err: errors.New("contextWindow is not set"), Fix: fix}
	}
	if *parsed.ContextWindow <= 0 {
		return doctorCheck{Name: name, Err: fmt.Errorf("contextWindow is %v, want a positive number", *parsed.ContextWindow), Fix: fix}
	}
	return doctorCheck{Name: name}
}

// doctorCheckTmpfsSize reuses dockerBytes (the same parser
// -sandbox-tmpfs-size's own validateSandboxResourceLimitFlags uses) so a
// malformed value is reported the same way it would be at real launch
// time, not a second, subtly different parse of it.
func doctorCheckTmpfsSize(tmpfsSize string) doctorCheck {
	name := "sandbox tmpfs size"
	bytes, err := dockerBytes(tmpfsSize)
	if err != nil {
		return doctorCheck{Name: name, Err: err}
	}
	const oneGB = 1 << 30
	if bytes < oneGB {
		return doctorCheck{Name: name, Err: fmt.Errorf("%s is below the current 1g default", tmpfsSize),
			Fix: "raise -sandbox-tmpfs-size -- a real dependency graph (protobuf/gRPC/Firebase, live-confirmed against a real brownfield app) can exhaust a smaller tmpfs mid-build with \"no space left on device\""}
	}
	return doctorCheck{Name: name}
}

// doctorCheckMountVisibility is a standalone, simplified rerun of
// internal/sandbox.verifyWorkDirMountVisibility's own live probe (that
// function takes a full LaunchSpec, built only at real launch time --
// this reimplements just the marker-file/bind-mount/`test -f` mechanic
// against a bare directory, so an operator can check *before* ever
// starting a real run): write a marker into workspace on the host, bind-
// mount it read-only into a throwaway container from image, and confirm
// the marker is visible from inside it. A failure here names exactly the
// trap of a path colima does not share, which this session found live.
func doctorCheckMountVisibility(ctx context.Context, dockerBinary, image, workspace string) doctorCheck {
	return doctorCheckMountVisibilityFor(ctx, dockerBinary, image, workspace,
		"-workspace", "pass a real, existing git checkout to -workspace")
}

// doctorCheckDataDirMountVisibility reruns the same colima mount-sharing
// probe against -data-dir instead of -workspace: -data-dir was never
// checked this way anywhere -- an operator whose -data-dir happened to
// land outside colima's default $HOME-only shared mount (common once
// -sandbox-image forces it
// out of -workspace, per USAGE.md's own gotcha table) only found out
// minutes into a real run, deep inside build_app.py/draft_spec.py.
// Unlike the referenceOracleDir call site just below, which deliberately
// keeps doctorCheckMountVisibility's "-workspace" wording for a rarer
// flag, -data-dir is checked on every worker/`factoryd <run>`
// invocation, common enough that the reported name/Fix text should name
// the right flag.
func doctorCheckDataDirMountVisibility(ctx context.Context, dockerBinary, image, dataDir string) doctorCheck {
	return doctorCheckMountVisibilityFor(ctx, dockerBinary, image, dataDir,
		"-data-dir", "pass a real, existing directory to -data-dir")
}

// doctorCheckMountVisibilityFor is the shared probe mechanic behind
// doctorCheckMountVisibility and doctorCheckDataDirMountVisibility --
// flagLabel names the flag in every reported Name/Err string, and
// existHint is the Fix text for a missing/non-directory target (the only
// piece that isn't just a substituted flag name: -workspace's hint talks
// about a git checkout, -data-dir's about a plain directory).
func doctorCheckMountVisibilityFor(ctx context.Context, dockerBinary, image, dir, flagLabel, existHint string) doctorCheck {
	name := fmt.Sprintf("mount visibility (%s reachable inside a container)", flagLabel)
	dirAbs, err := filepath.Abs(dir)
	if err != nil {
		return doctorCheck{Name: name, Err: fmt.Errorf("resolve %s: %w", flagLabel, err)}
	}
	// Requires the path to already exist, rather than creating it (found
	// via Codex review, PR #64): a mistyped or nonexistent -workspace used
	// to get silently created here, and the mount-visibility probe below
	// would then report a real, green result for that empty, freshly
	// created directory -- not the actual git checkout a run will use --
	// while also leaving an unexpected directory behind on disk. A
	// missing path is what this check should fail on, not paper over.
	info, err := os.Stat(dirAbs)
	if err != nil {
		return doctorCheck{Name: name, Err: fmt.Errorf("%s does not exist: %w", flagLabel, err),
			Fix: existHint}
	}
	if !info.IsDir() {
		return doctorCheck{Name: name, Err: fmt.Errorf("%s %q is not a directory", flagLabel, dirAbs),
			Fix: existHint}
	}
	if out, markerErr, probeErr := doctorRunMarkerProbe(ctx, dockerBinary, image, dirAbs); markerErr != nil {
		return doctorCheck{Name: name, Err: markerErr}
	} else if probeErr != nil {
		return doctorCheck{Name: name, Err: fmt.Errorf("the probe container could not see the marker file: %s", out),
			Fix: fmt.Sprintf("the configured Docker backend does not share %s's own path into its containers -- add it to colima's mounts (USAGE.md, \"Data directory and colima\": data dirs under %s read-write, repositories read-only)", flagLabel, sessionconfig.DataRoot()) + doctorMountVisibilityHint(dirAbs)}
	}
	return doctorCheck{Name: name}
}

// doctorCheckHomeNotShared warns (Advisory) when the Docker backend's VM
// can see $HOME itself, which colima does by default: a worker that escapes
// its container then reaches every file under $HOME (SSH keys, gh and
// harness auth, shell profiles, the session configs), not just the data
// dirs and repositories a run mounts. It probes a marker at the top of
// $HOME, which a VM sharing only sessionconfig.DataRoot and the
// repositories cannot see. Only meaningful where Docker runs in a VM
// (macOS); on native Linux the daemon sees the whole host regardless.
func doctorCheckHomeNotShared(ctx context.Context, dockerBinary, image string) doctorCheck {
	const name = "Docker VM does not share all of $HOME"
	home, err := os.UserHomeDir()
	if err != nil {
		return doctorCheck{Name: name, Err: err, Advisory: true}
	}
	out, markerErr, probeErr := doctorRunMarkerProbe(ctx, dockerBinary, image, home)
	var exitErr *exec.ExitError
	switch {
	case markerErr != nil:
		return doctorCheck{Name: name, Err: markerErr, Advisory: true}
	case probeErr == nil:
		return doctorCheck{Name: name, Advisory: true,
			Err: fmt.Errorf("a container bind-mounting %s sees files written there, so a container escape reaches every file under it", home),
			Fix: fmt.Sprintf("share only %s (read-write) and your repositories (read-only) with the VM: USAGE.md's colima mounts", sessionconfig.DataRoot())}
	case errors.As(probeErr, &exitErr) && exitErr.ExitCode() == 1:
		return doctorCheck{Name: name}
	default:
		return doctorCheck{Name: name, Advisory: true, Err: fmt.Errorf("could not probe: %v: %s", probeErr, out)}
	}
}

// doctorRunMarkerProbe writes a fresh marker file into dirAbs, bind-mounts
// dirAbs read-only into a throwaway container from image, and runs `test -f`
// on the marker there. markerErr is a failure to write the marker on the
// host; probeErr is the container's own failure (exit status 1 is `test`'s
// "not there": the backend does not share dirAbs), with out its output.
func doctorRunMarkerProbe(ctx context.Context, dockerBinary, image, dirAbs string) (out string, markerErr, probeErr error) {
	// A unique name created with O_EXCL, never a fixed one written with
	// os.WriteFile: now that init/onboard run this probe against a
	// directory the operator owns, a fixed marker path that already
	// existed there (or was a symlink) would be truncated and then
	// removed -- destroying their file just by running the scaffolder
	// (Codex review of PR #95). O_EXCL guarantees this probe only ever
	// removes a file it created itself.
	markerName := fmt.Sprintf(".factoryd-doctor-mount-probe-%d-%d", os.Getpid(), time.Now().UnixNano())
	markerPath := filepath.Join(dirAbs, markerName)
	marker, err := os.OpenFile(markerPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", fmt.Errorf("write probe marker: %w", err), nil
	}
	if _, err := marker.WriteString("factoryd doctor probe\n"); err != nil {
		_ = marker.Close()
		_ = os.Remove(markerPath)
		return "", fmt.Errorf("write probe marker: %w", err), nil
	}
	if err := marker.Close(); err != nil {
		_ = os.Remove(markerPath)
		return "", fmt.Errorf("write probe marker: %w", err), nil
	}
	defer os.Remove(markerPath)

	const probeMount = "/mnt/factoryd-doctor-probe"
	// --volume, not its -v shorthand (true aliases for a real docker
	// binary, no behavior difference) -- run_ticket.go's own integration
	// tests exercise this probe for the first time through
	// testdata/fake_docker.sh (added alongside doctorCheckDataDirMountVisibility),
	// and that fixture only recognizes --volume's long form.
	//
	// --network none, --user sandbox.DefaultWorkerUID, and an explicit
	// --entrypoint /bin/sh (never bare "test -f <path>" as argv, which
	// only reaches a shell when the configured image has no ENTRYPOINT of
	// its own) mirror internal/sandbox's own verifyWorkDirMountVisibility
	// probe, for the same reason: without them, an operator-configured
	// -sandbox-image with its own ENTRYPOINT runs that image-controlled
	// code instead of "test -f", with dirAbs (now -data-dir -- durable run
	// records/evidence for every run, not just one operator-owned
	// -workspace checkout) bind-mounted read-only and the default Docker
	// network reachable -- caught via adversarial review, GitHub Codex
	// App, PR #163. The marker path travels as sh's own $1, not spliced
	// into the -c script: no shell quoting to get right, and it stays a
	// standalone argv element -- the only shape fake_docker.sh's
	// per-argument mount-prefix translation can rewrite to a host path.
	cmd := exec.CommandContext(ctx, dockerBinary, "run", "--rm",
		"--read-only", "--cap-drop=ALL", "--security-opt=no-new-privileges",
		"--network", "none",
		"--user", strconv.Itoa(sandbox.DefaultWorkerUID),
		"--volume", dirAbs+":"+probeMount+":ro",
		"--entrypoint", "/bin/sh",
		image, "-c", `test -f "$1"`, "factoryd-doctor-probe", probeMount+"/"+markerName)
	raw, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(raw)), nil, err
}

// doctorMonorepoManifests are the build-manifest filenames
// doctorCheckMonorepoModuleRoot looks for, at -workspace's own root and one
// directory down: go.mod/package.json/pyproject.toml/Makefile, plus
// requirements.txt -- found via Codex review of PR #173: an earlier list
// silently dropped requirements.txt, which this check previously matched
// (and init.go's broader detectVerifyCommand still does), disabling the
// advisory for a pure-Python subproject whose only manifest is a bare
// requirements.txt. Not internal/ticketspec's stricter hasModuleRootMarker
// (which also requires a Makefile to declare a verify:/test: target, and a
// package.json to declare a "test" script); a plain-existence check is
// deliberately looser here since a wrong guess only ever produces an
// advisory warning below, never a hard failure.
var doctorMonorepoManifests = []string{"go.mod", "package.json", "pyproject.toml", "Makefile", "requirements.txt"}

// doctorManifestsAt returns which of doctorMonorepoManifests exist directly
// at dir's own root.
func doctorManifestsAt(dir string) []string {
	var found []string
	for _, manifest := range doctorMonorepoManifests {
		if _, err := os.Stat(filepath.Join(dir, manifest)); err == nil {
			found = append(found, manifest)
		}
	}
	return found
}

// doctorHasManifest reports whether any of doctorMonorepoManifests exists
// directly at dir's own root.
func doctorHasManifest(dir string) bool {
	return len(doctorManifestsAt(dir)) > 0
}

// doctorSourceRepoProbeCap bounds how many distinct in-flight source repos
// one doctor run probes (each probe starts a container).
const doctorSourceRepoProbeCap = 10

// doctorRequestSourceRepo is one in-flight request's source repo path.
type doctorRequestSourceRepo struct{ requestID, path string }

// doctorInFlightSourceRepos lists the distinct Workspace paths (the source
// repo the request clones and runs version control against) of every request
// under dataDir that has not finished (not done/cancelled), oldest first,
// skipping skip (already probed as -workspace). An unreadable data dir yields
// none: doctor must work on a fresh install.
func doctorInFlightSourceRepos(dataDir, skip string) []doctorRequestSourceRepo {
	requests, err := request.List(dataDir)
	if err != nil {
		return nil
	}
	skipAbs, _ := filepath.Abs(skip)
	seen := map[string]bool{}
	var repos []doctorRequestSourceRepo
	for _, r := range requests {
		if r.State == request.StateDone || r.State == request.StateCancelled || r.Workspace == "" {
			continue
		}
		abs, err := filepath.Abs(r.Workspace)
		if err != nil || seen[abs] || (skip != "" && abs == skipAbs) {
			continue
		}
		seen[abs] = true
		repos = append(repos, doctorRequestSourceRepo{requestID: r.ID, path: abs})
		if len(repos) == doctorSourceRepoProbeCap {
			break
		}
	}
	return repos
}

// doctorChecksRequestSourceRepos extends the mount-visibility probe to the
// source repo of every in-flight request: -workspace is often not passed, yet
// a repo the Docker VM does not share (on macOS, anything under /private/tmp
// or /var/folders) mounts as an empty directory, so the build's agent writes
// files the host never sees and the harness reports "no changes made to the
// workspace". Needs Docker, never a model.
func doctorChecksRequestSourceRepos(ctx context.Context, dockerBinary, image, dataDir, workspace string) []doctorCheck {
	var checks []doctorCheck
	for _, repo := range doctorInFlightSourceRepos(dataDir, workspace) {
		label := fmt.Sprintf("request %s source repo", repo.requestID)
		checks = append(checks, doctorCheckMountVisibilityFor(ctx, dockerBinary, image, repo.path, label,
			fmt.Sprintf("request %s's source repo %s no longer exists: restore it, or cancel/resubmit the request against a repo that does", repo.requestID, repo.path)))
	}
	return checks
}

// doctorUnsharedTempPrefixes are macOS locations Docker/colima VMs do not
// share by default.
var doctorUnsharedTempPrefixes = []string{"/private/tmp", "/tmp", "/private/var/folders", "/var/folders"}

// doctorMountVisibilityHint is the actionable remedy appended to a failed
// mount-visibility probe, naming the symptom the operator would otherwise see
// only later ("no changes made to the workspace").
func doctorMountVisibilityHint(dir string) string {
	hint := " Symptom if ignored: the build reports \"no changes made to the workspace\" even though the agent wrote files (they landed in an empty mount)."
	for _, prefix := range doctorUnsharedTempPrefixes {
		if dir == prefix || strings.HasPrefix(dir, prefix+"/") {
			return hint + fmt.Sprintf(" %s is a temp path outside the Docker VM's shared mounts: move the repo under a shared directory (for example clone it to ~/code/<name>) and resubmit.", dir)
		}
	}
	return hint + " Share the repo's directory with the Docker VM (read-only is enough; USAGE.md, \"Data directory and colima\") or move the repo under one it shares."
}

// doctorCheckMonorepoModuleRoot is advisory (doctorCheck.Advisory): the
// "monorepo module root != repo root" gotcha USAGE.md's own gotcha table
// already documents -- if go.mod/package.json/pyproject.toml/Makefile
// lives in a subdirectory of -workspace rather than at its own root, and
// PROJECT_DIR/-verify-command are pointed at -workspace itself, `make
// project-sandbox-image` silently bakes nothing useful. This never fails
// closed, by design (TestDoctorCheckMonorepoModuleRoot pins this): none
// anywhere within one level (a legitimate docs-only repo), or one in more
// than one subdirectory (ambiguous -- guessing wrong here would be worse
// than staying silent) both report a clean pass. It only warns when
// exactly one subdirectory one level down has a manifest of a type not
// already found at -workspace's own root, naming that subdirectory in the
// exact hint: "module root appears to be "<sub>"; pass -workspace <sub> or
// set module_root in .factory.yml" -- module_root is not itself a real
// .factory.yml key today (internal/projectconfig has no such field); the
// hint only ever suggests -workspace <sub>, which is real, as the
// actionable fix. Does not recurse past one level down.
//
// A manifest at root does NOT by itself suppress the subdirectory scan
// (found live against a real repo): a root-level manifest for an
// unrelated subsystem -- Flutter + Go app's root
// package.json is Playwright e2e tooling, while its real Go backend is
// rooted at backend/go.mod one level down -- used to make this check
// report a clean pass with no warning at all, exactly the mismatch it
// exists to catch. Only a subdirectory manifest of a type already
// satisfied at root is now treated as "matches", so a same-language
// nested manifest (e.g. a JS workspace's root and package/ package.json)
// still stays silent, but a different-language nested manifest still
// warns even when root has one of its own.
func doctorCheckMonorepoModuleRoot(workspace string) doctorCheck {
	name := "monorepo module root matches -workspace"
	workspaceAbs, err := filepath.Abs(workspace)
	if err != nil {
		return doctorCheck{Name: name, Err: fmt.Errorf("resolve -workspace: %w", err)}
	}
	info, err := os.Stat(workspaceAbs)
	if err != nil || !info.IsDir() {
		// A missing/non-directory -workspace is already reported by
		// doctorCheckMountVisibility; nothing more to say here.
		return doctorCheck{Name: name}
	}
	rootManifests := map[string]bool{}
	for _, manifest := range doctorManifestsAt(workspaceAbs) {
		rootManifests[manifest] = true
	}
	entries, err := os.ReadDir(workspaceAbs)
	if err != nil {
		return doctorCheck{Name: name}
	}
	var hits []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		for _, manifest := range doctorManifestsAt(filepath.Join(workspaceAbs, entry.Name())) {
			if !rootManifests[manifest] {
				hits = append(hits, entry.Name())
				break
			}
		}
	}
	if len(hits) != 1 {
		return doctorCheck{Name: name}
	}
	return doctorCheck{
		Name:     name,
		Err:      fmt.Errorf("module root appears to be %q; pass -workspace %s or set module_root in .factory.yml", hits[0], hits[0]),
		Fix:      fmt.Sprintf("pass -workspace %s or set module_root in .factory.yml -- make project-sandbox-image bakes nothing useful without it", hits[0]),
		Advisory: true,
	}
}

// doctorCheckGitCredentialsNotExposed runs the same git-credential
// preflight (see internal/sandbox/git_preflight.go's own doc comment)
// `sandbox.Run` will run fail-closed at the start of every real run --
// surfaced here in advance so `factoryd doctor -workspace .` reports the
// same refusal a real launch would, instead of the operator only
// discovering it when a run halts. A missing/non-directory -workspace is
// left to doctorCheckMountVisibility to report; nothing more to say here.
func doctorCheckGitCredentialsNotExposed(workspace string) doctorCheck {
	name := "git config has no exposable credential"
	info, err := os.Stat(workspace)
	if err != nil || !info.IsDir() {
		return doctorCheck{Name: name}
	}
	if err := sandbox.PreflightGitCredentials(workspace); err != nil {
		return doctorCheck{
			Name: name,
			Err:  err,
			Fix:  "remove the credential from the repo's git config (a credential helper, http.extraheader, or a userinfo-embedded remote URL); keep it in the operator's own global/system git config, outside the mounted tree",
		}
	}
	return doctorCheck{Name: name}
}

// resolveConfiguredDataDir applies the session config's own data_dir the
// same way worker's own applySessionConfig resolves the identical flag
// (found via Codex review of PR #142, round 3): -data-dir is config-file
// only the same way hitlReminderInterval is (see doctorMain's own doc
// comment there for why -- neither is part of
// sessionconfig.Settings/resolveSettings). Without this, `factoryd doctor
// -workspace .` always checked the literal "data" default against
// -workspace even when the operator's session config sets a real data_dir
// worker would actually use -- doctor could report green for a
// directory worker never touches, or FAIL on the literal default while
// the configured directory is actually the one that's unsafe. explicit is
// true only when -data-dir was named on this invocation's own command
// line (flags.Visit), in which case it always wins over the session
// config, matching every other flag's own explicit-wins resolution in
// this function. configPath, when non-empty, names the exact config file
// to resolve data_dir from (loadConfigForPath) instead of the default-path
// search: `factoryd doctor` had no -config flag at all before this fix, so it
// could only ever check the default search path's own data_dir, never
// the one an operator's `-config X` invocation actually uses.
func resolveConfiguredDataDir(explicit bool, dataDir, configPath string) string {
	if explicit {
		return dataDir
	}
	if cfg, _, found, err := loadConfigForPath(configPath); err == nil && found && cfg.DataDir != nil {
		return *cfg.DataDir
	}
	return dataDir
}

// doctorCheckDataDirOutsideWorkspace reproduces run_ticket.go's own
// fail-closed -data-dir/-workspace containment guard (around line 600
// there, using the identical canonicalPath/pathWithin helpers from
// validate.go -- not run_ticket.go's later dataDirInsideWorkspace, a
// different variable computed only for evidence-path redirection, not the
// guard itself) as an advance doctor check, so `factoryd doctor -workspace
// .` no longer reports green right before the very next real run fails
// closed on this. An empty dataDir is treated as nothing to check, not a
// false failure -- worker's own doctorChecksFor caller has no
// -data-dir-equivalent value to thread through today.
func doctorCheckDataDirOutsideWorkspace(workspace, dataDir string) doctorCheck {
	name := "-data-dir outside -workspace"
	if dataDir == "" {
		return doctorCheck{Name: name}
	}
	// Shares run_ticket.go's own real, pre-run guard via
	// dataDirInsideWorkspace (validate.go) rather than a second,
	// hand-synced copy of the same canonicalPath+pathWithin logic -- see
	// that function's own doc comment for why (found via code review,
	// 2026-09-14).
	inside, _, dataAbs, err := dataDirInsideWorkspace(workspace, dataDir)
	if err != nil {
		return doctorCheck{Name: name, Err: err}
	}
	if inside {
		return doctorCheck{
			Name: name,
			Err:  fmt.Errorf("-data-dir (%q, resolving to %q) is inside -workspace; Docker containment is unconditional and requires durable run records to live outside the workspace the worker mounts", dataDir, dataAbs),
			Fix:  "pass -data-dir <path outside -workspace>",
		}
	}
	return doctorCheck{Name: name}
}

// doctorCheckReleasePolicy gives the release policy doctor-visibility:
// releasePolicyCanNeverAllow (release_and_evidence.go) already knows a
// release policy denies every
// PR unconditionally whenever release_rollback_plan is empty or
// release_max_files_changed/release_max_insertions is 0, but until this
// check existed that only ever surfaced as a runtime warning logged once
// -open-pull-request was already set, or as silence and no PR after a
// run had already been accepted. Reports the same condition here, with
// the exact session-config lines to add, so an operator learns about it
// during setup instead of after a first "successful" run produces nothing
// to review.
//
// Advisory (warn, not counted in runDoctorChecks'/queue-run's own failed
// total), deliberately not a hard FAIL: doctorChecksFor is shared with
// worker's own startup preflight (runWorkerDoctorPreflight), which
// refuses to drain on any non-advisory failed check. An operator who
// never sets -open-pull-request has no PR to deny in the first place --
// this check has no -open-pull-request input to gate on the way
// warnIfReleasePolicyCanNeverAllow does, since doctorInputs doesn't carry
// that per-request field -- so making it a hard FAIL here would refuse
// every existing worker deployment that predates the release-defaults
// backfill and never touches -open-pull-request at all, purely on an
// unrelated session-config gap. Warn-and-name-the-fix still satisfies
// that same visibility goal without that regression.
func doctorCheckReleasePolicy(maxFilesChanged, maxInsertions int, rollbackPlan string) doctorCheck {
	name := "release policy allows a PR"
	if !releasePolicyCanNeverAllow(release.MergePolicy{MaxFilesChanged: maxFilesChanged, MaxInsertions: maxInsertions, RollbackPlan: rollbackPlan}) {
		return doctorCheck{Name: name}
	}
	return doctorCheck{
		Name:     name,
		Advisory: true,
		Err:      fmt.Errorf("the effective release policy denies every PR unconditionally (release_max_files_changed=%d, release_max_insertions=%d, release_rollback_plan=%q -- MergePolicyCheck denies whenever any of these is 0/empty); harmless if you never set -open-pull-request", maxFilesChanged, maxInsertions, rollbackPlan),
		Fix: fmt.Sprintf("add these three keys to your session config (factoryd init-config's own scaffold now includes them):\nrelease_max_files_changed: %d\nrelease_max_insertions: %d\nrelease_rollback_plan: %q",
			sessionconfig.DefaultReleaseMaxFilesChanged, sessionconfig.DefaultReleaseMaxInsertions, sessionconfig.DefaultReleaseRollbackPlan),
	}
}

// doctorCheckRolesResolve validates a session config's routes:/models:/
// roles: block the same way a real run would (sessionconfig.
// ValidateRouting): every routes: entry's own internal consistency,
// every role's `allowed` models resolve routes that actually exist, and
// the review/execution independence rule (`allow_shared_model`).
// settings is the actual sessionconfig.Settings a real run would resolve
// against (see doctorInputs.settings' own doc comment for why this check
// reads the whole thing rather than a handful of fields extracted from
// it into a fresh, poorer-fidelity value).
//
// ok when routes:/models:/roles: are all absent (an offline build with
// an explicit -build-app-script needs no relay); a hard FAIL naming the
// offending role/model/route/key otherwise -- ValidateRouting's own
// review/execution independence rule has no unwaived-warning case, only
// a hard FAIL unless allow_shared_model is set.
//
// A schema-clean routes: config is a plain OK here: the direct build/
// conformity/drafting paths resolve an actual route via
// modelrole.SelectRoute, and the Temporal path (`factoryd daemon`,
// `factoryd run -temporal-address`) resolves one too
// (modelrole.CheckRouteBinding, the Worker's own trust check, plus
// Activities.CheckRoute/ResolveRouteCredentials).
//
// This check alone only proves the schema is internally consistent --
// doctorChecksFor's own doctorRoutesModeChecks is what actually probes
// each configured (model, route) pair's real credential/upstream
// reachability.
func doctorCheckRolesResolve(in doctorInputs) doctorCheck {
	name := "roles resolve"
	if err := sessionconfig.ValidateRouting(in.settings); err != nil {
		return doctorCheck{Name: name, Err: err, Fix: "fix the named roles.<role>/models.<model>/routes.<route> entry in your session config"}
	}
	// Found live (M3 walk, 2026-09-28): ValidateRouting's own schema
	// checks passed for a roles.execution.allowed model whose id could
	// never produce a launchable relay policy (a slash-containing id --
	// sandbox.RoutePolicy.Validate rejects it because pi's own CLI hangs
	// on it), so this check alone reported a clean "roles resolve" for a
	// config that halted the very next real build at launch. See
	// modelrole.ValidateAllowedPolicies' own doc comment for the full
	// case.
	if err := modelrole.ValidateAllowedPolicies(in.settings); err != nil {
		return doctorCheck{Name: name, Err: err, Fix: "fix the named roles.<role>.allowed model's relay policy in your session config"}
	}
	return doctorCheck{Name: name, Detail: doctorRoleHarnessSummary(in.settings)}
}

// doctorRoleHarnessSummary names each configured role's harness, e.g.
// "planning: pi, execution: pifork, review: pi"; empty when no role is
// configured.
func doctorRoleHarnessSummary(s sessionconfig.Settings) string {
	var parts []string
	for _, spec := range doctorRoleSpecs {
		if !doctorRoleConfigured(s, spec.name) {
			continue
		}
		name, err := modelrole.RoleHarness(s, spec.role, "")
		if err != nil {
			continue
		}
		parts = append(parts, spec.name+": "+name)
	}
	return strings.Join(parts, ", ")
}

// roleHarnessSets is every harness each role can resolve to (default plus
// allowed_harnesses -- see harnessRoleSets). harnessOverride, when set,
// replaces it with that one harness for every role: quickstart's pre-config
// image pass, before any roles exist to read it from.
func (in doctorInputs) roleHarnessSets() (map[string][]harness.Descriptor, error) {
	if in.harnessOverride == "" {
		return harnessRoleSets(in.settings)
	}
	d, err := harness.Lookup(in.harnessOverride)
	if err != nil {
		return nil, err
	}
	out := map[string][]harness.Descriptor{}
	for _, role := range sessionRoles {
		out[string(role)] = []harness.Descriptor{d}
	}
	return out, nil
}

// doctorHarnessNeedingOwnImage returns the first (in role order) harness among
// roleHarnesses that the canonical worker image does not contain, or nil.
func doctorHarnessNeedingOwnImage(roleHarnesses map[string][]harness.Descriptor) *harness.Descriptor {
	for _, role := range sortedMapKeys(roleHarnesses) {
		for _, d := range roleHarnesses[role] {
			if d.RequiresSandboxImage {
				d := d
				return &d
			}
		}
	}
	return nil
}

// doctorDistinctHarnesses returns each distinct harness roleHarnesses names,
// sorted by name.
func doctorDistinctHarnesses(roleHarnesses map[string][]harness.Descriptor) []harness.Descriptor {
	byName := map[string]harness.Descriptor{}
	for _, ds := range roleHarnesses {
		for _, d := range ds {
			byName[d.Name] = d
		}
	}
	out := make([]harness.Descriptor, 0, len(byName))
	for _, name := range sortedMapKeys(byName) {
		out = append(out, byName[name])
	}
	return out
}

// doctorHarnessProbeArgs is the `docker run` argv for the in-image harness
// probe, hardened like the mount-visibility probe (PR #163) and launched like a
// real worker: no network, the real worker uid, an explicit /bin/sh entrypoint
// (so an image ENTRYPOINT never runs instead), the /home/worker tmpfs a real
// launch mounts, and the harness's own WorkerEnv.
func doctorHarnessProbeArgs(image string, d harness.Descriptor) []string {
	args := []string{"run", "--rm",
		"--read-only", "--cap-drop=ALL", "--security-opt=no-new-privileges",
		"--network", "none",
		"--user", strconv.Itoa(sandbox.DefaultWorkerUID),
		"--tmpfs", "/home/worker:rw,exec,nosuid,mode=1777,size=64m",
		"--entrypoint", "/bin/sh",
	}
	for _, e := range d.WorkerEnv {
		args = append(args, "--env", e)
	}
	return append(args, image, "-c", d.Binary+" --version")
}

// doctorCheckHarnessInImage confirms d's binary runs inside the sandbox image
// (`<binary> --version`), so a role whose harness is missing from the image is
// named before any run starts.
func doctorCheckHarnessInImage(ctx context.Context, dockerBinary, image string, d harness.Descriptor) doctorCheck {
	name := fmt.Sprintf("%s binary in sandbox image (%s)", d.Binary, image)
	cmd := exec.CommandContext(ctx, dockerBinary, doctorHarnessProbeArgs(image, d)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		fix := "rebuild the worker image with make install (Pi) or make pifork-image (pifork), or point the role's harness at one the image contains"
		return doctorCheck{
			Name: name,
			Err:  fmt.Errorf("`%s --version` failed in %s: %s", d.Binary, image, strings.TrimSpace(string(out))),
			Fix:  fix,
		}
	}
	return doctorCheck{Name: name}
}

func doctorCheckTemporalReachable(ctx context.Context, address string) doctorCheck {
	name := fmt.Sprintf("Temporal server reachable (%s)", address)
	fix := "confirm -temporal-address and that a real Temporal server is actually listening there"
	temporalClient, err := client.Dial(client.Options{HostPort: address})
	if err != nil {
		return doctorCheck{Name: name, Err: err, Fix: fix}
	}
	defer temporalClient.Close()
	if _, err := temporalClient.CheckHealth(ctx, &client.CheckHealthRequest{}); err != nil {
		return doctorCheck{Name: name, Err: err, Fix: fix}
	}
	return doctorCheck{Name: name}
}

// staleTemporalWorkflowTypes are the workflow types factoryd itself ever
// starts (cmd/factoryd/daemon_cmd.go, run_repository_owner.go,
// run_temporal.go each RegisterWorkflow(workflow.RunWorkflow) and/or
// RegisterWorkflow(workflow.RepositoryOwnerWorkflow); those two calls are
// the complete set) -- doctorCheckStaleTemporalWorkflows only ever flags
// Running executions of these types, never some other application's
// workflows sharing the same Temporal namespace.
var staleTemporalWorkflowTypes = []string{"RepositoryOwnerWorkflow", "RunWorkflow"}

// staleTemporalWorkflowAge is how long a Running execution of one of
// staleTemporalWorkflowTypes must have been running before
// doctorCheckStaleTemporalWorkflows treats it as a likely orphan: 2x
// workflow.DefaultRepositoryOwnerIdle (RepositoryOwnerWorkflow's own idle
// timeout), so a workflow still inside its own timeout window never
// warns -- only one that has already run well past the timeout that
// should have closed it. Live evidence 2026-09-24 on localhost:7233:
// several RepositoryOwnerWorkflow executions (workflow id
// repo-owner-<digest>) and one child RunWorkflow
// (repo-owner-<digest>-run-...) sat Running for 6 days to over a week --
// their per-run task-queue workers were gone, so no worker was ever left
// to fire the idle timer and the executions could never self-resolve.
const staleTemporalWorkflowAge = 2 * workflow.DefaultRepositoryOwnerIdle

// temporalWorkflowLister is the subset of client.Client
// doctorCheckStaleTemporalWorkflowsUsing needs, so a test can fake
// ListWorkflow's visibility-query response without a real Temporal server
// -- client.Client satisfies this directly, no wrapping required.
type temporalWorkflowLister interface {
	ListWorkflow(ctx context.Context, request *workflowservice.ListWorkflowExecutionsRequest) (*workflowservice.ListWorkflowExecutionsResponse, error)
}

// doctorCheckStaleTemporalWorkflows dials address and lists Running
// executions of factoryd's own workflow types that started more than
// staleTemporalWorkflowAge ago -- an orphaned RepositoryOwnerWorkflow/
// RunWorkflow execution whose worker is gone is otherwise invisible to an
// operator: nothing else in this repo polls or surfaces it, and
// RepositoryOwnerWorkflow's own idle timer only fires from *inside* a
// running worker, so once that workflow's task queue has no worker left,
// the timer that would otherwise close it never fires either.
// Always Advisory (warn, never FAIL) and never terminates anything itself
// -- terminating a Temporal execution is an operator decision this
// command only ever recommends via the printed Fix line, the same "name
// the fix, don't apply it" split every other doctor check (and -fix's own
// image-build-only scope) already follows for state this command doesn't
// own. If the visibility list query itself fails (e.g. an older Temporal
// server without advanced visibility -- see
// https://docs.temporal.io/visibility), that is reported as its own warn
// rather than FAIL: missing visibility support is an environment
// limitation, not a defect this check found.
func doctorCheckStaleTemporalWorkflows(ctx context.Context, address string) doctorCheck {
	name := "no stale Temporal workflows"
	temporalClient, err := client.Dial(client.Options{HostPort: address})
	if err != nil {
		return doctorCheck{Name: name, Err: err, Advisory: true, Fix: "confirm -temporal-address and that a real Temporal server is actually listening there"}
	}
	defer temporalClient.Close()
	return doctorCheckStaleTemporalWorkflowsUsing(ctx, temporalClient, address, time.Now())
}

// doctorCheckStaleTemporalWorkflowsUsing is doctorCheckStaleTemporalWorkflows's
// testable core: lister is a real client.Client in production and a fake
// in tests, and now is threaded through explicitly rather than read from
// time.Now() so a test can place executions on either side of
// staleTemporalWorkflowAge deterministically.
func doctorCheckStaleTemporalWorkflowsUsing(ctx context.Context, lister temporalWorkflowLister, address string, now time.Time) doctorCheck {
	name := "no stale Temporal workflows"
	threshold := now.Add(-staleTemporalWorkflowAge)
	var typeClauses []string
	for _, t := range staleTemporalWorkflowTypes {
		typeClauses = append(typeClauses, fmt.Sprintf("WorkflowType = %q", t))
	}
	query := fmt.Sprintf("ExecutionStatus = 'Running' AND StartTime < %q AND (%s)",
		threshold.UTC().Format(time.RFC3339), strings.Join(typeClauses, " OR "))
	resp, err := lister.ListWorkflow(ctx, &workflowservice.ListWorkflowExecutionsRequest{Query: query})
	if err != nil {
		return doctorCheck{
			Name:     name,
			Err:      fmt.Errorf("could not list Temporal workflow executions to check for orphans: %w", err),
			Advisory: true,
			Fix:      "requires Temporal server-side advanced visibility (e.g. Elasticsearch); an older/minimal Temporal server cannot run this check",
		}
	}
	if len(resp.Executions) == 0 {
		return doctorCheck{Name: name}
	}
	shown := resp.Executions
	if len(shown) > 5 {
		shown = shown[:5]
	}
	var fixLines []string
	for _, e := range shown {
		id := e.GetExecution().GetWorkflowId()
		age := now.Sub(e.GetStartTime().AsTime()).Round(time.Hour)
		fixLines = append(fixLines, fmt.Sprintf("temporal workflow terminate --address %s --workflow-id %s --reason \"orphaned: no worker\"  # %s, running %s",
			address, id, e.GetType().GetName(), age))
	}
	return doctorCheck{
		Name: name,
		Err: fmt.Errorf("%d Running execution(s) of factoryd's own workflow types have been running past %s with no visible worker -- likely orphans left by a worker that is gone; doctor never auto-terminates, review each id below before terminating",
			len(resp.Executions), staleTemporalWorkflowAge),
		Advisory: true,
		Fix:      strings.Join(fixLines, "\n      "),
	}
}

// rejectSymlinkedScaffoldAncestor refuses to scaffold path if root itself,
// or any existing directory between root and path, is a symlink -- found
// via a GitHub Codex App review round, 2026-08-29: the leaf-level Lstat
// conflict check above catches a symlink at the artifact path itself, but
// not a symlinked *ancestor* (e.g. -root/spec pointing somewhere else
// entirely, with no spec.md/contract.md created there yet); os.MkdirAll
// would follow that symlink and the later os.WriteFile would create the
// artifact through it, outside -root. A not-yet-existing ancestor is fine
// -- MkdirAll below only ever creates plain directories, never a symlink
// -- so this only ever rejects a symlink an attacker (or a stale prior
// run) planted before this invocation, not the ordinary fresh-scaffold
// case.
