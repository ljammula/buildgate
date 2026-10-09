// factoryd quickstart is the one-command onboarding sequencer: it runs the
// same steps USAGE.md's own "Quick start on a new Mac" walks an operator
// through by hand (doctor, config, onboarding, worker, submit, status) end to end,
// prompting interactively only where a real decision is needed, and
// stopping at the first human review gate (spec_review) with the exact
// `factoryd approve` command printed rather than polling forever for a
// terminal state that needs a manual approval to ever arrive.
//
// This is additive, not a replacement: every wrapped subcommand keeps
// working standalone exactly as today (see that plan's "What this
// explicitly does not change"). quickstart adds no new config schema, no
// new safety bypass, and never approves a spec/plan review on the
// operator's behalf, -non-interactive included.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/term"
	"gopkg.in/yaml.v3"

	"buildgate/internal/codereview"
	"buildgate/internal/consolelink"
	"buildgate/internal/harness"
	"buildgate/internal/hostcontrol"
	"buildgate/internal/meter"
	"buildgate/internal/modelrole"
	"buildgate/internal/projectconfig"
	"buildgate/internal/request"
	"buildgate/internal/sandbox"
	"buildgate/internal/sessionconfig"
)

// quickstartDefaultReleaseMaxFilesChanged/quickstartDefaultReleaseMaxInsertions/
// quickstartDefaultReleaseRollbackPlan alias the single source of truth
// in internal/sessionconfig (sessionconfig.DefaultReleaseMaxFilesChanged
// and friends) so quickstart's fresh-config path and its reuse-path
// backfill (quickstartBackfillReleaseDefaults) and `factoryd init-config`
// (sessionconfig.Example) can never drift apart -- previously each kept
// its own copy of these numbers. With -open-pull-request
// default-on, a first "successful" quickstart run would otherwise end
// with no PR and no explanation at
// all: the shipped zero-value defaults (sessionconfig.DefaultSettings'
// own ReleaseMaxFilesChanged/ReleaseMaxInsertions/ReleaseRollbackPlan)
// deny every PR unconditionally (see internal/release.MergePolicyCheck: 0
// files allowed, empty rollback plan always denies).
const (
	quickstartDefaultReleaseMaxFilesChanged = sessionconfig.DefaultReleaseMaxFilesChanged
	quickstartDefaultReleaseMaxInsertions   = sessionconfig.DefaultReleaseMaxInsertions
	quickstartDefaultReleaseRollbackPlan    = sessionconfig.DefaultReleaseRollbackPlan
)

// quickstartBackfillReleaseDefaults fills in whichever of
// release_max_files_changed/release_max_insertions/release_rollback_plan
// are missing from a REUSED session config with the same usable defaults
// a freshly generated config gets (quickstartDefaultRelease* above), and
// returns the flag names it added so the caller can tell the operator
// what changed. It never overwrites a value the operator (or an earlier
// quickstart run) already set -- only a nil pointer is filled in. Before
// this, only the fresh-config path got usable release defaults, so an
// operator who ran `factoryd init-config` by hand (or reused an older config
// predating this default) still saw every PR denied by
// internal/release.MergePolicyCheck with no indication why.
func quickstartBackfillReleaseDefaults(existing *sessionconfig.Config) []string {
	var added []string
	if existing.ReleaseMaxFilesChanged == nil {
		v := quickstartDefaultReleaseMaxFilesChanged
		existing.ReleaseMaxFilesChanged = &v
		added = append(added, "release_max_files_changed")
	}
	if existing.ReleaseMaxInsertions == nil {
		v := quickstartDefaultReleaseMaxInsertions
		existing.ReleaseMaxInsertions = &v
		added = append(added, "release_max_insertions")
	}
	if existing.ReleaseRollbackPlan == nil {
		v := quickstartDefaultReleaseRollbackPlan
		existing.ReleaseRollbackPlan = &v
		added = append(added, "release_rollback_plan")
	}
	return added
}

// quickstartAppendReleaseDefaultsText appends the release_* keys named in
// added (quickstartBackfillReleaseDefaults' own return value, already
// applied to cfg in memory) to path's file text as new lines, leaving
// every existing byte of the file untouched. Before this, the only way to
// persist a backfilled default was quickstartWriteConfig's yaml.Marshal of
// the whole reused Config -- which re-serializes every field, silently
// dropping the operator's own comments and key order/formatting on every
// -reconfigure-free quickstart run that merely noticed a missing
// release_* key (found in review of the onboarding-p1 branch). Appending
// only the missing lines means a backfill-only run changes nothing an
// operator wrote by hand.
//
// cfg's ReleaseMaxFilesChanged/ReleaseMaxInsertions/ReleaseRollbackPlan
// pointers must already be non-nil for every key named in added (true
// immediately after quickstartBackfillReleaseDefaults, which is this
// function's only caller).
//
// writer names the command actually appending these lines ("factoryd
// quickstart" or "factoryd doctor -fix", its two callers): before this
// parameter existed, the appended comment always said "factoryd
// quickstart" even when `factoryd doctor -fix` was the one that wrote it.
func quickstartAppendReleaseDefaultsText(path string, cfg *sessionconfig.Config, added []string, writer string) error {
	if len(added) == 0 {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	var b strings.Builder
	b.Write(data)
	if len(data) > 0 && data[len(data)-1] != '\n' {
		b.WriteByte('\n')
	}
	fmt.Fprintf(&b, "# added by %s %s: usable release policy defaults\n", writer, time.Now().Format("2006-01-02"))
	for _, key := range added {
		switch key {
		case "release_max_files_changed":
			fmt.Fprintf(&b, "release_max_files_changed: %d\n", *cfg.ReleaseMaxFilesChanged)
		case "release_max_insertions":
			fmt.Fprintf(&b, "release_max_insertions: %d\n", *cfg.ReleaseMaxInsertions)
		case "release_rollback_plan":
			fmt.Fprintf(&b, "release_rollback_plan: %s\n", quickstartYAMLScalar(*cfg.ReleaseRollbackPlan))
		}
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// quickstartYAMLScalar renders s as a YAML scalar safe to append inline
// after a `key: ` prefix -- yaml.Marshal itself decides whether s needs
// quoting (a colon, a leading special character, etc.), rather than this
// function guessing, since a hand-written quoting rule here would drift
// from whatever gopkg.in/yaml.v3 actually requires to parse it back
// unquoted-when-safe/quoted-when-not.
func quickstartYAMLScalar(s string) string {
	data, err := yaml.Marshal(s)
	if err != nil {
		// yaml.Marshal of a plain string cannot fail; this is unreachable
		// in practice but a literal fallback beats a panic.
		return s
	}
	return strings.TrimSuffix(string(data), "\n")
}

// quickstartDataDirHasRecords reports whether dir already looks like a
// factoryd data directory in active use -- any of its requests/, runs/ or
// queue/ subdirectories exists (internal/request.Dir, internal/run.Dir
// and the queue package's own layout, respectively). Used only by
// quickstartNoticeCwdDataDirRecords, to decide whether to print a notice
// -- never to decide where to persist data_dir. An earlier version of
// this function's caller auto-adopted such a directory as data_dir
// (an adversarial review of Phase A); a follow-up round of that same
// review found that adoption both broken (it probed
// filepath.Abs(opts.DataDir), which is the shell's OWN cwd when
// opts.DataDir is its own empty default, not <cwd>/data) and unsafe (a
// project root with its own top-level runs/ or queue/ directory --
// common in ML repos -- would have been silently adopted as data_dir,
// inside the workspace). quickstartEnsureConfig now always persists
// sessionconfig.DefaultDataDirFor and only ever notices <cwd>/data specifically.
func quickstartDataDirHasRecords(dir string) bool {
	for _, sub := range []string{"requests", "runs", "queue"} {
		if info, err := os.Stat(filepath.Join(dir, sub)); err == nil && info.IsDir() {
			return true
		}
	}
	return false
}

// quickstartNoticeCwdDataDirRecords prints (never persists) a notice when
// the shell's current working directory has its own "data" subdirectory
// -- the exact path quickstart's own earlier behavior used, and the
// literal default every other factoryd command's own -data-dir flag
// still has -- already holding real factoryd records. A follow-up round
// of that Phase A review found quickstart should always persist
// resolvedDataDir (sessionconfig.DefaultDataDirFor) now; this only tells the operator
// where their earlier records are and how to keep using them, rather
// than silently starting a second, disconnected history. A no-op when
// resolvedDataDir already IS <cwd>/data (nothing to notice about) or
// <cwd>/data has no records at all.
func quickstartNoticeCwdDataDirRecords(w io.Writer, resolvedDataDir, configPath string) {
	cwd, err := os.Getwd()
	if err != nil {
		return
	}
	cwdDataDir, err := filepath.Abs(filepath.Join(cwd, "data"))
	if err != nil || cwdDataDir == resolvedDataDir {
		return
	}
	if !quickstartDataDirHasRecords(cwdDataDir) {
		return
	}
	fmt.Fprintf(w, "Note: earlier records found at %s were not moved; to keep using them, pass -data-dir %s (or set data_dir in %s).\n", cwdDataDir, cwdDataDir, configPath)
}

// quickstartOptions bundles quickstartMain's own flags, plus derived
// values a later step fills in (builtImageRefs), as explicit fields
// instead of a FlagSet -- the same shape submitParams/onboardOptions
// already establish for a caller that wants to drive this logic directly
// (tests, here) rather than through flag parsing.
type quickstartOptions struct {
	NonInteractive           bool
	Goal                     string
	Issue                    string
	RequestFile              string
	VerifyCommand            string
	VerifyCommandExplicit    bool
	PreflightProfile         string
	PreflightProfileExplicit bool
	Scaffold                 bool
	Reconfigure              bool
	BuildImages              bool
	DataDir                  string
	DataDirExplicit          bool
	ConfigPath               string
	SandboxDocker            string
	RepoRootFlag             string
	Route                    string
	ModelHost                string
	ModelID                  string
	ContextWindow            int
	ContextWindowExplicit    bool
	Credential               string
	CredentialProvided       bool
	EgressCABundle           string
	PollInterval             time.Duration
	PollTimeout              time.Duration
	// NoServe skips
	// quickstartEnsureServe entirely (no reuse probe, no spawned/kickstarted
	// serve), leaving quickstart exactly as before -- a worker
	// alone with no console/API. NoOpen still ensures serve (and still
	// prints the tokenized console link once the request is submitted) but
	// never calls openInBrowser on it.
	NoServe bool
	NoOpen  bool
	// Harness is -harness: "" or harness.Pi (the default), harness.Codex or
	// harness.Pifork,
	// written as roles.<role>.harness into every role quickstart scaffolds.
	// SandboxImage is -sandbox-image, the digest-pinned pifork worker
	// image (`make pifork-image`); required with, and only valid with,
	// -harness pifork.
	Harness      string
	SandboxImage string
	// HarnessExplicit is whether -harness was given: when it was not, an
	// interactive run asks on a route that can run more than one harness.
	HarnessExplicit bool

	// builtImageRefs is populated by quickstartEnsureImages when -fix built
	// one or more absent images, keyed by the flag name doctorCheck.Use
	// names (e.g. "-sandbox-image") -- quickstartBuildConfig writes these
	// into the freshly generated config so a later worker actually uses
	// the image quickstart itself just built, instead of re-attempting the
	// pull that just failed.
	builtImageRefs map[string]string
}

// quickstartMain implements `factoryd quickstart [flags] [<repo-path>]
// ["<goal text>"]`. Flags must precede the positional arguments, the same
// convention `factoryd submit` already uses (flag.Parse stops at the
// first positional argument).
// quickstartFlags bundles every `factoryd quickstart` flag's pointer, so
// newQuickstartFlags (below) can hand them back to quickstartMain without an
// unwieldy multi-value return list. Field names match the flag's own local
// variable name at every existing call site.
type quickstartFlags struct {
	nonInteractive   *bool
	goal             *string
	issue            *string
	requestFile      *string
	verifyCommand    *string
	preflightProfile *string
	scaffold         *bool
	reconfigure      *bool
	buildImages      *bool
	dataDir          *string
	configPath       *string
	sandboxDocker    *string
	repoRootFlag     *string
	route            *string
	modelHost        *string
	modelID          *string
	contextWindow    *int
	credential       *string
	egressCABundle   *string
	pollInterval     *time.Duration
	pollTimeout      *time.Duration
	noServe          *bool
	noOpen           *bool
	harness          *string
	sandboxImage     *string
}

// newQuickstartFlags builds `factoryd quickstart`'s FlagSet in isolation
// from parsing, so USAGE.md's doc-vs-flag drift test
// (TestUSAGEDocFlagsExistOnSubcommand) can enumerate its real flags without
// executing the command.
func newQuickstartFlags() (flags *flag.FlagSet, f quickstartFlags) {
	flags = flag.NewFlagSet("quickstart", flag.ContinueOnError)
	f.nonInteractive = flags.Bool("non-interactive", false, "skip every prompt; every value a prompt would have asked for must be given as a flag instead, or this fails naming the missing one. Also inferred automatically when stdin is not a terminal, so a piped invocation never hangs on a prompt")
	f.goal = flags.String("goal", "", "what the factory should build, instead of trailing positional request text, -issue, -request-file, or an interactive prompt. Must precede <repo-path> on the command line")
	f.issue = flags.String("issue", "", "fetch the request text from this GitHub issue's own title and body instead of -goal, trailing positional text, or -request-file (`gh issue view <url>` must succeed: gh installed, authenticated, and the issue reachable). The queued run's eventual draft PR closes this issue on merge (see run.Run.PRCloses). Must precede <repo-path> on the command line (flag.Parse stops at the first positional argument)")
	f.requestFile = flags.String("request-file", "", "read the request text from this file instead of -goal, trailing positional text, or -issue. Must precede <repo-path> on the command line (flag.Parse stops at the first positional argument)")
	f.verifyCommand = flags.String("verify-command", "", "canonical verification command for the submitted request; default: detected from the repo's own Makefile/go.mod/package.json (see detectVerifyCommand), falling back to the repo's committed .factory.yml if that names one and this flag is left unset")
	f.preflightProfile = flags.String("preflight-profile", "brownfield", "preflight strictness passed to the submitted request; the repo's own committed .factory.yml wins over this default when it sets preflight_profile and this flag is left at its default")
	f.scaffold = flags.Bool("scaffold", false, "also scaffold onboarding docs (spec/spec.md, spec/contract.md, ARCHITECTURE.md) into the repo via `factoryd onboard`, pre-authorizing the step an interactive run would otherwise ask about; skipped automatically if those files already exist. Never run under -non-interactive unless passed explicitly")
	f.reconfigure = flags.Bool("reconfigure", false, "rewrite the session config even if a valid one already exists at the resolved path, instead of reusing it")
	f.buildImages = flags.Bool("build-images", false, "pre-authorize building any image that fails to pull, locally via this checkout's Makefile (see -repo-root/$FACTORYD_REPO_ROOT), instead of prompting for confirmation. Never triggered silently otherwise -- under -non-interactive a pull failure fails outright unless this is set")
	f.dataDir = flags.String("data-dir", "", "directory for durable queue and run records worker/submit both use; default: the resolved session config's own data_dir, or, for a newly written config, ~/buildgate/data for a profile and <config-dir>/data for a config elsewhere")
	f.configPath = flags.String("config", "", "session config path; default: the first of "+strings.Join(sessionconfig.DefaultPaths(), ", ")+" that exists, or the first of those for a newly written config")
	f.sandboxDocker = flags.String("sandbox-docker", "docker", "Docker executable the doctor preflight checks with")
	f.repoRootFlag = flags.String("repo-root", "", "this repository's own checkout, whose Makefile -build-images (or an interactively confirmed image build) runs; default: $FACTORYD_REPO_ROOT, then the running binary's parent directory when that holds the Makefile")
	f.route = flags.String("route", "", "model route kind: \"openai\" (an OpenAI-compatible /v1 endpoint), \"anthropic\" (the Anthropic API), \"copilot\" (GitHub Copilot), or \"chatgpt-codex\" (ChatGPT via a `codex` CLI login). Left empty, quickstart detects a Codex/Copilot/Anthropic login already on this host and offers it first -- under -non-interactive, a single unambiguous detected login is used automatically (see USAGE.md); otherwise required")
	f.modelHost = flags.String("model-host", "", "bare API root (no /v1) of an OpenAI-compatible model endpoint; only used with -route openai")
	f.modelID = flags.String("model-id", "", "model id to configure; required (without a prompt) under -non-interactive for -route openai or -route copilot. Must not contain a slash -- the relay rejects a slash-containing id later anyway")
	f.contextWindow = flags.Int("context-window", 0, "the configured model's context window in tokens; used with -route openai and -route copilot (worker's own startup preflight requires it whenever a worker model id is configured, regardless of route)")
	f.credential = flags.String("credential", "", "API key or OAuth token for a credentialed route; kept only in the daemon child process's own environment, never written to config. Falls back to $ANTHROPIC_API_KEY (routes openai/anthropic) or $GITHUB_COPILOT_TOKEN (route copilot) when left empty")
	f.egressCABundle = flags.String("egress-ca-bundle", "", "PEM file on this host trusted (in addition to the system trust store) for outbound TLS calls quickstart itself makes -- currently only -route copilot's own entitled-model listing -- and written into the generated session config's egress_ca_bundle so a real run trusts it too; see factoryd <run>/worker/doctor's own flag of the same name")
	f.pollInterval = flags.Duration("poll-interval", 3*time.Second, "how often to re-check the submitted request's state while it is still submitted/spec_drafting")
	f.pollTimeout = flags.Duration("poll-timeout", 5*time.Minute, "how long to keep polling before giving up and printing `factoryd status` as the way to keep watching, without itself failing")
	f.noServe = flags.Bool("no-serve", false, "skip ensuring `factoryd serve` (console + API) is running -- worker alone, exactly like quickstart before this flag existed")
	f.harness = flags.String("harness", harness.Pi, "coding-agent harness the written config uses for every role: \"pi\", \"codex\" (the Codex CLI; needs a Responses route such as -route chatgpt-codex) or \"pifork\" (a Pi fork; needs -sandbox-image)")
	f.sandboxImage = flags.String("sandbox-image", "", "digest-pinned pifork worker image (name@sha256:..., from `make pifork-image PIFORK_DOCKERFILE=<your Dockerfile>`); required with -harness pifork, refused otherwise")
	f.noOpen = flags.Bool("no-open", false, "do not open the submitted request's tokenized console link in the default browser; it is still printed")
	plainFlagUsage(flags)
	return flags, f
}

func quickstartMain(dp *deps, args []string) error {
	flags, qf := newQuickstartFlags()
	nonInteractive, goal, issue, requestFile := qf.nonInteractive, qf.goal, qf.issue, qf.requestFile
	verifyCommand, preflightProfile, scaffold := qf.verifyCommand, qf.preflightProfile, qf.scaffold
	reconfigure, buildImages, dataDir, configPath, sandboxDocker := qf.reconfigure, qf.buildImages, qf.dataDir, qf.configPath, qf.sandboxDocker
	repoRootFlag, route, modelHost, modelID, contextWindow := qf.repoRootFlag, qf.route, qf.modelHost, qf.modelID, qf.contextWindow
	credential, egressCABundle, pollInterval, pollTimeout := qf.credential, qf.egressCABundle, qf.pollInterval, qf.pollTimeout
	noServe, noOpen := qf.noServe, qf.noOpen
	harnessName, sandboxImage := qf.harness, qf.sandboxImage
	if err := flags.Parse(args); err != nil {
		return err
	}

	explicit := map[string]bool{}
	flags.Visit(func(f *flag.Flag) { explicit[f.Name] = true })

	positional := flags.Args()
	repoPathArg := "."
	if len(positional) > 0 {
		repoPathArg = positional[0]
	}
	goalArg := ""
	if len(positional) > 1 {
		goalArg = strings.Join(positional[1:], " ")
	}
	resolvedGoal := *goal
	if resolvedGoal == "" {
		resolvedGoal = goalArg
	}

	opts := &quickstartOptions{
		NonInteractive:           *nonInteractive || !quickstartStdinIsInteractive(os.Stdin),
		Goal:                     resolvedGoal,
		Issue:                    *issue,
		RequestFile:              *requestFile,
		VerifyCommand:            *verifyCommand,
		VerifyCommandExplicit:    explicit["verify-command"],
		PreflightProfile:         *preflightProfile,
		PreflightProfileExplicit: explicit["preflight-profile"],
		Scaffold:                 *scaffold,
		Reconfigure:              *reconfigure,
		BuildImages:              *buildImages,
		DataDir:                  *dataDir,
		DataDirExplicit:          explicit["data-dir"],
		ConfigPath:               *configPath,
		SandboxDocker:            *sandboxDocker,
		RepoRootFlag:             *repoRootFlag,
		Route:                    *route,
		ModelHost:                *modelHost,
		ModelID:                  *modelID,
		ContextWindow:            *contextWindow,
		ContextWindowExplicit:    explicit["context-window"],
		Credential:               *credential,
		CredentialProvided:       explicit["credential"],
		EgressCABundle:           *egressCABundle,
		PollInterval:             *pollInterval,
		PollTimeout:              *pollTimeout,
		NoServe:                  *noServe,
		NoOpen:                   *noOpen,
		Harness:                  *harnessName,
		HarnessExplicit:          explicit["harness"],
		SandboxImage:             *sandboxImage,
	}
	if err := quickstartValidateHarness(opts); err != nil {
		return err
	}

	return runQuickstart(dp, opts, repoPathArg, os.Stdin, os.Stdout)
}

// quickstartStdinIsInteractive reports whether f is a real terminal, the
// same check gating -non-interactive's own automatic inference: a piped or
// redirected stdin must never hang this command on a prompt it can never
// receive an answer to. This must be a true TTY check (term.IsTerminal),
// not just os.ModeCharDevice -- /dev/null is a character device but not a
// terminal, so under cron/launchd/CI or `</dev/null` a ModeCharDevice check
// misidentified it as interactive and quickstart hung on a prompt (found
// live 2026-09-26).
// quickstartValidateHarness checks -harness/-sandbox-image before anything
// else runs: pifork needs its own digest-pinned worker image (it never falls
// back to the Pi image), and -sandbox-image is refused with pi, whose image
// quickstart manages itself.
func quickstartValidateHarness(opts *quickstartOptions) error {
	switch opts.Harness {
	case "", harness.Pi, harness.Codex:
		if opts.SandboxImage != "" {
			return fmt.Errorf("-sandbox-image is only for -harness pifork; the pi and codex worker image is built and recorded by quickstart itself")
		}
	case harness.Pifork:
		if opts.SandboxImage == "" {
			return fmt.Errorf("-harness pifork requires -sandbox-image: build it with `make pifork-image PIFORK_DOCKERFILE=<your Dockerfile>` and pass the digest it prints")
		}
		if !strings.Contains(opts.SandboxImage, "@sha256:") {
			return fmt.Errorf("-sandbox-image must be pinned by digest (name@sha256:...), got %q", opts.SandboxImage)
		}
	default:
		return fmt.Errorf("-harness must be %q, %q or %q, got %q", harness.Pi, harness.Codex, harness.Pifork, opts.Harness)
	}
	return nil
}

func quickstartStdinIsInteractive(f *os.File) bool {
	return term.IsTerminal(int(f.Fd()))
}

// quickstartValidateRequestSource enforces, before any doctor/config/daemon
// side effect runs, the same "at most one request-text source" rule
// resolveSubmitRequestText enforces at actual submission time -- opts.Goal
// (itself already -goal or trailing positional text, merged by
// quickstartMain), -issue, and -request-file are mutually exclusive -- plus
// the -non-interactive "a source is required" check (there is no prompt to
// fall back on) and early, side-effect-free sanity checks on -issue (a
// well-formed GitHub issue URL, via parseGitHubIssueURL) and -request-file
// (readable), so an invocation that was always going to fail at the final
// submit step doesn't first run doctor/config/daemon setup (a Codex review
// finding on the PR that introduced the original -non-interactive-requires-
// goal version of this check, round 3).
func quickstartValidateRequestSource(opts *quickstartOptions) error {
	sources := 0
	for _, set := range []bool{opts.Goal != "", opts.Issue != "", opts.RequestFile != ""} {
		if set {
			sources++
		}
	}
	if sources > 1 {
		return fmt.Errorf("-issue, -request-file, and -goal/trailing request text are mutually exclusive")
	}
	if opts.NonInteractive && sources == 0 {
		return fmt.Errorf("a goal is required under -non-interactive: pass -goal, trailing positional request text, -issue, or -request-file")
	}
	if opts.Issue != "" {
		if _, _, err := parseGitHubIssueURL(opts.Issue); err != nil {
			return err
		}
	}
	if opts.RequestFile != "" {
		// ReadFile, not Stat: a directory or a mode-0000 file passes Stat
		// but still fails submit's own read after all the setup has run.
		if _, err := os.ReadFile(opts.RequestFile); err != nil {
			return fmt.Errorf("-request-file: %w", err)
		}
	}
	return nil
}

// runQuickstart is quickstartMain's own logic, factored out so tests can
// drive it with an explicit stdin/stdout instead of the process' real
// ones.
func runQuickstart(dp *deps, opts *quickstartOptions, repoPathArg string, stdin io.Reader, w io.Writer) error {
	// Checked before any of the (potentially slow, side-effecting) steps
	// below rather than only once quickstartSubmitAndWatch is finally
	// reached: under -non-interactive there is no prompt to fall back on,
	// so a missing goal can never be satisfied later, and doctor/config/
	// daemon work should not run at all on a request that was always
	// going to fail at the very last step (a Codex review on this PR,
	// round 3). See quickstartValidateRequestSource's own doc comment for
	// the full rule (also covers -issue/-request-file and their mutual
	// exclusivity with -goal/trailing text).
	if err := quickstartValidateRequestSource(opts); err != nil {
		return err
	}

	repoRoot, err := resolveQuickstartRepoRoot(repoPathArg, dp.forge.gitToplevel)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "Repository: %s\n", repoRoot)

	// Checked before the (slower) image-pull/config steps below, not only
	// after a full build -- opening a PR is quickstart's whole point, and this
	// costs one `gh auth status` call. Skipped silently for a repo with no
	// GitHub remote (doctorCheckGHAuth's own doc comment).
	if err := quickstartCheckGHAuth(w, repoRoot); err != nil {
		return err
	}

	p := newQuickstartPrompter(stdin)

	configPath, existingConfig, err := quickstartResolveExistingConfig(opts)
	if err != nil {
		return err
	}

	if err := quickstartEnsureImages(dp, opts, p, w, configPath, existingConfig); err != nil {
		return err
	}

	dataDir, credentialEnv, restartNeeded, err := quickstartEnsureConfig(dp, opts, p, w, configPath, existingConfig)
	if err != nil {
		return err
	}
	opts.DataDir = dataDir

	if err := quickstartCheckRepoMountVisible(opts, configPath, repoRoot); err != nil {
		return err
	}

	verifyCommand, verifyExplicit, preflightProfile, preflightExplicit, err := quickstartEnsureRepoReady(dp, opts, p, w, repoRoot)
	if err != nil {
		return err
	}
	// The repo's compose images under a registry the config does not allow
	// would halt its first build. Say so now, and add the prefixes only on
	// a yes (offerComposeRegistries); non-interactively it only says so.
	if settings, settingsErr := loadSettingsForConfig(configPath); settingsErr == nil && settings.ComposeServices {
		offerComposeRegistries(p, w, false, !opts.NonInteractive, settings, repoRoot, configPath, "factoryd quickstart")
	}

	temporalAddress := dp.temporal.ensure(context.Background(), w)

	binaryPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve this binary's own path: %w", err)
	}
	if err := hostcontrol.QuickstartEnsureDaemon(dp, w, binaryPath, configPath, dataDir, credentialEnv, restartNeeded, temporalAddress); err != nil {
		return err
	}

	// quickstartEnsureServe is best-effort by design: quickstart's own
	// primary job (get the ticket queued and draining) is already done by
	// the time this
	// runs, so a serve/console problem (a port fight with something else
	// already using 8090, a launchd hiccup) is reported but never fails
	// the whole invocation -- see that function's own doc comment.
	consoleToken := hostcontrol.QuickstartEnsureServe(dp, opts.NoServe, w, binaryPath, configPath, dataDir)

	// -non-interactive is already ruled out above (this function's own
	// upfront check) whenever opts.Goal/Issue/RequestFile are all empty, so
	// reaching here with an empty goal AND no -issue/-request-file always
	// means an interactive prompt is the right next step -- -issue/
	// -request-file each already carry their own request text, so neither
	// needs (or gets) this prompt.
	goal := opts.Goal
	if goal == "" && opts.Issue == "" && opts.RequestFile == "" {
		goal, err = p.ask(w, "What should the factory build? ")
		if err != nil {
			return err
		}
		if goal == "" {
			return fmt.Errorf("a goal is required")
		}
	}

	return quickstartSubmitAndWatch(dp, w, repoRoot, dataDir, goal, opts.Issue, opts.RequestFile, verifyCommand, verifyExplicit, preflightProfile, preflightExplicit, opts.PollInterval, opts.PollTimeout, consoleToken, !opts.NoOpen)
}

// gitToplevel resolves the git repository containing dir, via
// `git -C <dir> rev-parse --show-toplevel` -- the plan's chosen mechanism
// (mirroring internal/release.RepositoryRoot's own git-toplevel check) for
// "is this really inside a git repo, and if so, what's its root". A
// boundary method so tests can stub it without a real git binary or repo.
func (impl realForge) gitToplevel(dir string) (string, error) {
	cmd := exec.Command("git", "-C", dir, "rev-parse", "--show-toplevel")
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// resolveQuickstartRepoRoot resolves quickstart's own <repo-path>
// positional argument (or "." when omitted) to an absolute git repository
// root, refusing anything not inside one -- quickstart's own target is
// exactly an existing checkout ("a non-repo path is refused with a
// pointer to factoryd init/intake"), never a from-scratch app (that path is
// intake + factoryd <run>, not submit, which has no from-scratch shape at
// all).
func resolveQuickstartRepoRoot(pathArg string, toplevel func(dir string) (string, error)) (string, error) {
	if pathArg == "" {
		pathArg = "."
	}
	abs, err := filepath.Abs(pathArg)
	if err != nil {
		return "", fmt.Errorf("resolve %q: %w", pathArg, err)
	}
	if info, err := os.Stat(abs); err != nil {
		return "", fmt.Errorf("%q: %w", pathArg, err)
	} else if !info.IsDir() {
		return "", fmt.Errorf("%q is not a directory", pathArg)
	}
	root, err := toplevel(abs)
	if err != nil {
		return "", fmt.Errorf("%q is not inside a git repository -- factoryd quickstart works against an existing checkout with a real history; for a brand-new app use `factoryd intake` followed by `factoryd <run>` instead, or `factoryd init` to scaffold a fresh repo first", pathArg)
	}
	return root, nil
}

// quickstartCheckGHAuth runs the same
// doctorCheckGHAuth `factoryd doctor -workspace` now runs, called here
// with repoRoot -- quickstart already resolved the repo path as its own
// first step, so there is no reason to wait for `doctor` to be run
// separately (or for a full build to fail) before this surfaces.
func quickstartCheckGHAuth(w io.Writer, repoRoot string) error {
	check := doctorCheckGHAuth(context.Background(), execGHAuthRunner, repoRoot)
	if runDoctorChecks([]doctorCheck{check}, w) > 0 {
		return fmt.Errorf("%s: see fix above", check.Name)
	}
	return nil
}

// ---- step 1: doctor / images ----------------------------------------

// quickstartPullDoctorInputs builds the doctorInputs quickstart's own
// presence-focused preflight checks against: the sandbox image, relay
// image, and registry proxy image, plus docker-reachable and
// compose-version, mirroring doctorMain's own default-on set for the
// checks that don't need a resolved model route to run. There is no
// built-in default image for any of the three -- every image is built
// from source (see quickstartEnsureImages' own -fix/-build-images path)
// and recorded via `factoryd configure-images`; existing, when non-nil
// (an already valid session config quickstart is about to reuse),
// supplies each image from that config's own value.
//
// releaseMaxFilesChanged/releaseMaxInsertions/releaseRollbackPlan default
// to the same quickstartDefaultRelease* values a fresh config gets and are
// overridden by existing's own values below when it has them:
// doctorChecksFor always appends doctorCheckReleasePolicy, and an earlier
// version of this
// function left those three fields at doctorInputs' own zero value
// (0, 0, ""), which releasePolicyCanNeverAllow always reports as "denies
// every PR unconditionally" -- a false alarm on this preflight regardless
// of what the config quickstart is about to use actually says, since the
// real release decision reads the config, not this hardcoded zero.
func quickstartPullDoctorInputs(sandboxDocker string, existing *sessionconfig.Config) doctorInputs {
	in := doctorInputs{
		sandboxDocker:          sandboxDocker,
		composeServices:        true,
		releaseMaxFilesChanged: quickstartDefaultReleaseMaxFilesChanged,
		releaseMaxInsertions:   quickstartDefaultReleaseMaxInsertions,
		releaseRollbackPlan:    quickstartDefaultReleaseRollbackPlan,
		// This is an image-presence-only pass, run before this
		// quickstart invocation's own config is finalized -- see
		// doctorInputs.presenceOnly's own doc comment for why routes:
		// mode's route/network/credential checks must never run here.
		presenceOnly: true,
	}
	if existing != nil {
		if existing.ReleaseMaxFilesChanged != nil {
			in.releaseMaxFilesChanged = *existing.ReleaseMaxFilesChanged
		}
		if existing.ReleaseMaxInsertions != nil {
			in.releaseMaxInsertions = *existing.ReleaseMaxInsertions
		}
		if existing.ReleaseRollbackPlan != nil {
			in.releaseRollbackPlan = *existing.ReleaseRollbackPlan
		}
		if existing.SandboxImage != nil && *existing.SandboxImage != "" {
			in.sandboxImage = *existing.SandboxImage
		}
		if existing.RegistryProxyImage != nil && *existing.RegistryProxyImage != "" {
			in.registryProxyImage = *existing.RegistryProxyImage
		}
		if existing.ImageSourceRoot != nil {
			in.imageSourceRoot = *existing.ImageSourceRoot
		}
		if settings, err := existing.ApplySettings(sessionconfig.DefaultSettings()); err == nil {
			in.settings = settings
		}
		// An explicitly disabled registry proxy means the worker this
		// config drives never launches one at all -- checking its image
		// is present would reject an offline operator over a component
		// their own configuration already turns off (a Codex review on
		// this PR, round 2).
		if existing.RegistryProxy != nil && !*existing.RegistryProxy {
			in.registryProxyImage = ""
		}
	}
	return in
}

// quickstartSeedImagesFromDefaultConfig fills in's own image fields from
// the default session config (sessionconfig.DefaultPaths()) when the
// config quickstart is about to use has no image pins of its own --
// found live 2026-09-26: a fresh -config quickstart run (a
// separate session config, e.g. a per-project one) ignored the images
// `make install` had already built and recorded into the DEFAULT config,
// and doctor's FAIL hint circularly told the operator to re-run the very
// `make install` they had just run. Skipped when configPath IS a default
// path (nothing to copy from) or in already has its own sandbox_image.
// Only pins whose images are still present locally are copied (via
// sandbox.ImagePresent, the same presence probe doctorChecksFor itself
// uses) -- a reference recorded on another machine, or since removed
// locally, must not be copied in as if it still worked.
//
// The returned map, keyed exactly like opts.builtImageRefs
// ("-sandbox-image" etc.), is quickstartEnsureImages' own signal to carry
// these borrowed refs into the written config too -- found via
// adversarial review (2026-09-26): an earlier version fed the borrowed
// refs into the doctor-check inputs only, so a fresh config quickstart
// wrote (having borrowed and never rebuilt anything, since doctor passed)
// ended up with no sandbox_image at all, and a real worker
// against it failed the same "no sandbox image configured" check this fix
// exists to get past.
func quickstartSeedImagesFromDefaultConfig(in doctorInputs, configPath string, w io.Writer) (doctorInputs, map[string]string) {
	in.configPath = configPath
	if in.sandboxImage != "" {
		return in, nil
	}
	for _, p := range sessionconfig.DefaultPaths() {
		absDefault, err := filepath.Abs(p)
		if err != nil || absDefault == configPath {
			continue
		}
		cfg, err := sessionconfig.Load(p)
		if err != nil {
			continue
		}
		if cfg.SandboxImage == nil || *cfg.SandboxImage == "" {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		present := sandbox.ImagePresent(ctx, in.sandboxDocker, *cfg.SandboxImage)
		cancel()
		if !present {
			continue
		}
		borrowed := map[string]string{}
		in.sandboxImage = *cfg.SandboxImage
		borrowed["-sandbox-image"] = *cfg.SandboxImage
		if cfg.RegistryProxyImage != nil && *cfg.RegistryProxyImage != "" {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			proxyPresent := sandbox.ImagePresent(ctx, in.sandboxDocker, *cfg.RegistryProxyImage)
			cancel()
			if proxyPresent {
				in.registryProxyImage = *cfg.RegistryProxyImage
				borrowed["-registry-proxy-image"] = *cfg.RegistryProxyImage
			}
		}
		if cfg.ImageSourceRoot != nil {
			in.imageSourceRoot = *cfg.ImageSourceRoot
		}
		fmt.Fprintf(w, "Reusing sandbox image(s) already built and recorded in %s (this config has none of its own).\n", p)
		return in, borrowed
	}
	return in, nil
}

// quickstartMergeBorrowedImageRefs layers built (from an actual -fix
// build this invocation just ran) over borrowed (from
// quickstartSeedImagesFromDefaultConfig) so a freshly built ref always
// wins over a merely-borrowed one for the same flag -- the partial-borrow
// case (sandbox borrowed and present, relay borrowed-absent-then-built)
// must keep both, not let the borrowed map get clobbered wholesale by
// opts.builtImageRefs = built.
func quickstartMergeBorrowedImageRefs(borrowed, built map[string]string) map[string]string {
	if len(borrowed) == 0 {
		return built
	}
	merged := map[string]string{}
	for k, v := range borrowed {
		merged[k] = v
	}
	for k, v := range built {
		merged[k] = v
	}
	return merged
}

// quickstartBuiltImageRefs extracts the "-flag value" pairs doctorFixImageByBuilding
// left on doctorCheck.Use for each image -fix actually built, so
// quickstartBuildConfig can carry the freshly built, digest-pinned
// reference straight into the session config it writes.
func quickstartBuiltImageRefs(checks []doctorCheck) map[string]string {
	refs := map[string]string{}
	for _, c := range checks {
		fields := strings.Fields(c.Use)
		if len(fields) == 2 {
			refs[fields[0]] = fields[1]
		}
	}
	return refs
}

// quickstartImagePullFailures filters checks down to the ones about an
// image -- doctorCheckImagePresent's own naming convention
// ("<label> present (<image>)"), or the plain "sandbox image"/"<engine>
// worker image" check an unconfigured image resolves to -- that actually
// failed. The "on a failed check, offer -fix" branch needs to tell that
// class of failure apart from any other doctor check (Docker unreachable,
// compose version too old, ...) without string-matching runDoctorChecks'
// own collapsed summary.
func quickstartImagePullFailures(checks []doctorCheck) []doctorCheck {
	var out []doctorCheck
	for _, c := range checks {
		if c.Err != nil && !c.Advisory && strings.Contains(c.Name, "image") {
			out = append(out, c)
		}
	}
	return out
}

// quickstartEnsureImages runs the presence-focused doctor checks and, on
// a missing-image failure, offers (or, with -build-images, pre-
// authorizes) the -fix build path -- offered and confirmed, never a
// silent default. Never offers -fix
// under -non-interactive; never offers it at all when resolveDoctorRepoRoot
// cannot locate a checkout, printing that error and the
// -repo-root/$FACTORYD_REPO_ROOT hint instead of attempting anything.
func quickstartEnsureImages(dp *deps, opts *quickstartOptions, p *quickstartPrompter, w io.Writer, configPath string, existing *sessionconfig.Config) error {
	pulled := quickstartPullDoctorInputs(opts.SandboxDocker, existing)
	if opts.Harness == harness.Pifork {
		// The pifork worker image comes only from -sandbox-image: quickstart
		// can build the Pi images, never pifork's (it needs the operator's
		// own Dockerfile), so a missing one is a stop, not an offer to build.
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		present := sandbox.ImagePresent(ctx, opts.SandboxDocker, opts.SandboxImage)
		cancel()
		if !present {
			return fmt.Errorf("-sandbox-image %s is not in the local Docker image store: build it with `make pifork-image PIFORK_DOCKERFILE=<your Dockerfile>` and pass the digest it prints", opts.SandboxImage)
		}
		quickstartPiforkDoctorInputs(&pulled)
		pulled.sandboxImage = opts.SandboxImage
	}
	in, borrowed := quickstartSeedImagesFromDefaultConfig(pulled, configPath, w)
	checks, err := doctorRunChecks(dp, in, false, "")
	if err != nil {
		return err
	}
	failed := runDoctorChecks(checks, w)
	if failed == 0 {
		if len(borrowed) > 0 {
			opts.builtImageRefs = quickstartMergeBorrowedImageRefs(borrowed, opts.builtImageRefs)
		}
		return quickstartOfferStaleRebuild(dp, opts, p, w, configPath, existing, checks, in.sandboxImage, in.imageSourceRoot)
	}
	pullFailures := quickstartImagePullFailures(checks)
	if len(pullFailures) == 0 {
		return fmt.Errorf("%d doctor check(s) failed -- see output above", failed)
	}

	exe, _ := os.Executable()
	repoRoot, rootErr := resolveDoctorRepoRoot(opts.RepoRootFlag, os.Getenv("FACTORYD_REPO_ROOT"), in.imageSourceRoot, exe)
	if rootErr != nil {
		fmt.Fprintf(w, "%d image(s) missing or unconfigured, and a local checkout to build them with -fix could not be located: %v\n", len(pullFailures), rootErr)
		fmt.Fprintln(w, "Pass -repo-root <this repository's checkout> (or set FACTORYD_REPO_ROOT) to enable building locally.")
		return fmt.Errorf("%d doctor check(s) failed", failed)
	}

	doFix := opts.BuildImages
	if !doFix {
		if opts.NonInteractive {
			return fmt.Errorf("%d image(s) missing or unconfigured; rerun with -build-images to build them locally from %s (several minutes)", len(pullFailures), repoRoot)
		}
		doFix, err = p.confirm(w, fmt.Sprintf("%d image(s) missing or unconfigured. Build them locally from %s instead (several minutes)? [y/N] ", len(pullFailures), repoRoot), false)
		if err != nil {
			return err
		}
	}
	if !doFix {
		return fmt.Errorf("%d doctor check(s) failed", failed)
	}

	// Reuses the same `in`/`borrowed` from the presence-check pass above
	// (not a second quickstartSeedImagesFromDefaultConfig call) so a
	// borrowed sandbox_image is not re-probed and re-announced a second
	// time here -- the doctor inputs quickstart already resolved at the
	// top of this function are still valid; nothing about the default
	// config or local Docker state changed in between (only the operator's
	// confirmation above).
	checks, err = doctorRunChecks(dp, in, true, repoRoot)
	if err != nil {
		return err
	}
	failed = runDoctorChecks(checks, w)
	// A freshly built ref always wins over a merely-borrowed one for the
	// same flag (e.g. sandbox borrowed-and-present, relay
	// borrowed-absent-then-built-here) -- see
	// quickstartMergeBorrowedImageRefs' own doc comment.
	opts.builtImageRefs = quickstartMergeBorrowedImageRefs(borrowed, quickstartBuiltImageRefs(checks))
	if failed > 0 {
		return fmt.Errorf("%d doctor check(s) failed even after building locally", failed)
	}
	return nil
}

// quickstartImageStaleChecks filters checks down to doctorCheckImageStale's
// own reported-stale results -- Advisory checks whose Err is set (never
// counted in runDoctorChecks' own failed total, so quickstartEnsureImages'
// own "failed == 0" path above would otherwise say nothing about them).
func quickstartImageStaleChecks(checks []doctorCheck) []doctorCheck {
	var out []doctorCheck
	for _, c := range checks {
		if c.Err != nil && c.Advisory && strings.Contains(c.Name, "up to date with") {
			out = append(out, c)
		}
	}
	return out
}

// quickstartNonWorkerStaleSandboxImageTarget reports the real make target
// (pifork-image) that rebuilds sandboxImage (the effective sandbox_image
// in play -- existing's own, or one quickstartSeedImagesFromDefaultConfig
// borrowed from the default config), when stale names that image
// ("sandbox image up to date with ..." -- see quickstartImageStaleChecks)
// and its own stamped buildgate.image label is "pifork" -- never
// "local-images", which doesn't build it. Returns "" whenever the
// sandbox image isn't the stale one, sandboxImage is empty, or the label
// can't be read (docker unreachable, image absent, unlabeled, or labeled
// "worker"/"project") -- callers must fall back to the ordinary `make
// local-images` path in every one of those cases, exactly as before this
// check existed.
func quickstartNonWorkerStaleSandboxImageTarget(stale []doctorCheck, opts *quickstartOptions, sandboxImage string) string {
	if sandboxImage == "" {
		return ""
	}
	sandboxStale := false
	for _, c := range stale {
		if strings.HasPrefix(c.Name, "sandbox image up to date with") {
			sandboxStale = true
			break
		}
	}
	if !sandboxStale {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	kind, err := imageKindFor(ctx, opts.SandboxDocker, sandboxImage)
	if err != nil {
		return ""
	}
	switch kind {
	case "pifork":
		return "pifork-image"
	default:
		return ""
	}
}

// quickstartOfferStaleRebuild is quickstartEnsureImages' own follow-up
// once every image check already passed (present and digest-pinned):
// warn about, and offer to fix, an image that is merely stale against
// its recorded image_source_root (internal/imageinputs.Hash mismatch) --
// distinct from quickstartEnsureImages' own missing/unconfigured-image
// flow above, and never itself a failure (a running image is still
// digest-pinned and contained regardless of staleness). Rebuilds via
// `make local-images` (builds all three and records them via `factoryd
// configure-images`) rather than the single-image -fix path above, since
// staleness has no per-image "absent" signal for doctorFixAbsentImages to
// act on. Mirrors quickstartEnsureImages' own -build-images/
// -non-interactive/confirm conventions.
//
// Rebuilds in the effective image_source_root (existing's own, or one
// quickstartSeedImagesFromDefaultConfig borrowed from the default
// config), not wherever resolveDoctorRepoRoot's own env/executable
// fallbacks would otherwise find -- unless -repo-root was given explicitly
// -- because staleness was measured against that exact checkout;
// rebuilding somewhere else could legitimately produce a different (and
// still "stale" by comparison) hash. Forwards FACTORYD_CONFIG=configPath
// to `make local-images` so its own `configure-images` call writes the
// freshly built refs into the same session config this quickstart
// invocation is using, not always the default path (see that Makefile
// variable's own doc comment) -- then re-reads configPath afterward and
// stages any changed image ref into opts.builtImageRefs, the exact signal
// quickstartEnsureConfig's own imagesUpdated/restartNeeded logic already
// watches for the missing-image rebuild path, so a running worker
// daemon gets restarted onto the fresh digests here too.
//
// sandboxImage/imageSourceRoot are the effective values quickstartEnsureImages
// already resolved (existing's own, or borrowed from the default config by
// quickstartSeedImagesFromDefaultConfig) -- not re-derived from existing
// here, or a borrowed sandbox image's own staleness/rebuild-checkout
// handling would silently fall back to the generic `make local-images`
// path and the executable-heuristic checkout guess, ignoring exactly the
// config quickstart just borrowed it from.
func quickstartOfferStaleRebuild(dp *deps, opts *quickstartOptions, p *quickstartPrompter, w io.Writer, configPath string, existing *sessionconfig.Config, checks []doctorCheck, sandboxImage, imageSourceRoot string) error {
	stale := quickstartImageStaleChecks(checks)
	if len(stale) == 0 {
		return nil
	}
	// A stale pifork sandbox_image needs its own dedicated make target --
	// never `make local-images`, which only rebuilds the plain
	// worker/relay/registry-proxy trio (Makefile's own local-images
	// target) and would silently replace a pifork sandbox_image with a
	// plain worker image, discarding its engine packaging. Detected
	// the same way factoryd configure-images' own
	// nonWorkerSandboxImageAdvice protects the config file itself (found via
	// the same adversarial review pass, 2026-09-25): read back the
	// sandbox_image's own buildgate.image label rather than
	// guessing it from -engine, since a project image FROMs the worker and
	// would otherwise misreport as one. Only the sandbox_image's own
	// staleness triggers this -- a stale relay/registry-proxy image with a
	// pifork sandbox_image alongside it still rebuilds normally through
	// `make local-images` below.
	if target := quickstartNonWorkerStaleSandboxImageTarget(stale, opts, sandboxImage); target != "" {
		fmt.Fprintf(w, "sandbox image %s is out of date; rebuild it with `make %s` (not `make local-images`, which does not build it).\n", sandboxImage, target)
		return nil
	}
	repoRoot := opts.RepoRootFlag
	if repoRoot == "" {
		repoRoot = imageSourceRoot
	}
	if repoRoot == "" {
		exe, _ := os.Executable()
		var rootErr error
		repoRoot, rootErr = resolveDoctorRepoRoot(opts.RepoRootFlag, os.Getenv("FACTORYD_REPO_ROOT"), "", exe)
		if rootErr != nil {
			fmt.Fprintf(w, "%d image(s) are out of date with their recorded source, and a local checkout to rebuild them could not be located: %v\n", len(stale), rootErr)
			return nil
		}
	}
	doRebuild := opts.BuildImages
	if !doRebuild {
		if opts.NonInteractive {
			fmt.Fprintf(w, "%d image(s) are out of date with %s; rerun with -build-images to rebuild them, or run `make install` there yourself.\n", len(stale), repoRoot)
			return nil
		}
		var err error
		doRebuild, err = p.confirm(w, fmt.Sprintf("Images are out of date with %s; rebuild now? [Y/n] ", repoRoot), true)
		if err != nil {
			return err
		}
	}
	if !doRebuild {
		return nil
	}
	if _, err := dp.docker.makeImage(repoRoot, "local-images", "FACTORYD_CONFIG="+configPath); err != nil {
		return fmt.Errorf("make -C %s local-images: %w", repoRoot, err)
	}
	fmt.Fprintf(w, "Rebuilt images from %s.\n", repoRoot)
	if refreshed, err := sessionconfig.Load(configPath); err == nil {
		for _, im := range []struct {
			flag string
			prev *string
			next *string
		}{
			{"-sandbox-image", quickstartImageOrNil(existing, func(c *sessionconfig.Config) *string { return c.SandboxImage }), refreshed.SandboxImage},
			{"-registry-proxy-image", quickstartImageOrNil(existing, func(c *sessionconfig.Config) *string { return c.RegistryProxyImage }), refreshed.RegistryProxyImage},
		} {
			if im.next == nil || *im.next == "" {
				continue
			}
			if im.prev != nil && *im.prev == *im.next {
				continue
			}
			if opts.builtImageRefs == nil {
				opts.builtImageRefs = map[string]string{}
			}
			opts.builtImageRefs[im.flag] = *im.next
		}
	}
	return nil
}

// quickstartImageOrNil applies get to existing, nil-safely -- existing
// itself may be nil (a fresh quickstart run with no reused config).
func quickstartImageOrNil(existing *sessionconfig.Config, get func(*sessionconfig.Config) *string) *string {
	if existing == nil {
		return nil
	}
	return get(existing)
}

// ---- step 2: session config ------------------------------------------

// quickstartResolveExistingConfig resolves the session config path
// quickstart will use, and loads it too when a valid one already exists
// and -reconfigure was not passed -- called before quickstartEnsureImages
// so an already-valid config's own image refs can steer that step (see
// quickstartPullDoctorInputs), and again before quickstartEnsureConfig so
// the two never disagree about which config is "the existing one".
//
// When -config is left unset, mirrors sessionconfig.LoadDefault's own
// "first of DefaultPaths() that exists" precedence, not just
// DefaultPaths()[0] unconditionally: a Codex review on this PR found that
// picking DefaultPaths()[0] regardless meant an operator with only the
// legacy ~/.factory/config.yml (no XDG path) never had it recognized as
// existing, so quickstart prompted for a fresh route and wrote a second,
// divergent config next to the one that already worked.
func quickstartResolveExistingConfig(opts *quickstartOptions) (configPath string, existing *sessionconfig.Config, err error) {
	if opts.ConfigPath != "" {
		configPath, err = filepath.Abs(sessionconfig.ResolveArg(opts.ConfigPath))
		if err != nil {
			return "", nil, fmt.Errorf("resolve -config: %w", err)
		}
		if opts.Reconfigure {
			return configPath, quickstartRecordedImagesOnly(configPath), nil
		}
		cfg, loadErr := sessionconfig.Load(configPath)
		switch {
		case loadErr == nil:
			return configPath, cfg, nil
		case os.IsNotExist(loadErr):
			return configPath, nil, nil
		default:
			return "", nil, fmt.Errorf("existing config at %s is invalid (pass -reconfigure to rewrite it): %w", configPath, loadErr)
		}
	}

	if !opts.Reconfigure {
		cfg, foundPath, found, loadErr := sessionconfig.LoadDefault()
		if loadErr != nil {
			return "", nil, fmt.Errorf("existing config at %s is invalid (pass -reconfigure to rewrite it): %w", foundPath, loadErr)
		}
		if found {
			configPath, err = filepath.Abs(foundPath)
			if err != nil {
				return "", nil, fmt.Errorf("resolve %s: %w", foundPath, err)
			}
			return configPath, cfg, nil
		}
	} else if foundPath, found := quickstartFirstExistingDefaultPath(); found {
		// -reconfigure means "rewrite the config in place, wherever it is"
		// -- it must still land on the SAME first-existing-default-path
		// precedence the reuse branch above uses (a Codex review on this
		// PR), or an operator with only the legacy ~/.factory/config.yml
		// would have -reconfigure write a brand new XDG config next to it
		// instead of rewriting the one that's actually in use. Content is
		// deliberately not reused here -- -reconfigure means write a fresh
		// one, not merge into the old -- except the image refs `make
		// install` recorded (quickstartRecordedImagesOnly), which are not
		// operator choices and which the rewrite would otherwise lose.
		configPath, err = filepath.Abs(foundPath)
		if err != nil {
			return "", nil, fmt.Errorf("resolve %s: %w", foundPath, err)
		}
		return configPath, quickstartRecordedImagesOnly(configPath), nil
	}

	configPath, err = filepath.Abs(sessionconfig.DefaultPaths()[0])
	if err != nil {
		return "", nil, fmt.Errorf("resolve default config path: %w", err)
	}
	return configPath, nil, nil
}

// quickstartRecordedImagesOnly is what -reconfigure keeps of the config it
// rewrites: only the image refs and image_source_root that `make install`'s
// configure-images step recorded, as the same images-only config a fresh
// install leaves behind (quickstartEnsureConfig completes it). Without it a
// -reconfigure on a freshly installed machine fails its image check with "no
// sandbox image configured", and the rewritten config loses the images. nil
// when the file is missing, invalid, or records no image.
func quickstartRecordedImagesOnly(configPath string) *sessionconfig.Config {
	old, err := sessionconfig.Load(configPath)
	if err != nil {
		return nil
	}
	kept := &sessionconfig.Config{
		SandboxImage:       old.SandboxImage,
		RegistryProxyImage: old.RegistryProxyImage,
		MeterImage:         old.MeterImage,
		ImageSourceRoot:    old.ImageSourceRoot,
	}
	if kept.SandboxImage == nil && kept.RegistryProxyImage == nil && kept.MeterImage == nil && kept.ImageSourceRoot == nil {
		return nil
	}
	return kept
}

// quickstartFirstExistingDefaultPath returns the first of
// sessionconfig.DefaultPaths() that exists on disk, ignoring whether its
// content is actually valid YAML -- callers here only need to know WHERE
// to write under -reconfigure, not what the file currently holds.
func quickstartFirstExistingDefaultPath() (string, bool) {
	p, found, err := sessionconfig.ResolvePath()
	return p, err == nil && found
}

// quickstartCredentialEnvForReusedConfig derives the credential
// environment variable a reused config's own relay_credential_mode
// implies (see quickstartBuildConfig's own copilot/openai/anthropic
// branches for why those are the only two names this ever produces),
// when the operator passed -credential alongside a config being reused
// rather than freshly built. Without this, an explicit -credential was
// silently discarded on the reuse path (a Codex review on this PR),
// so a subsequently spawned worker had no credential in its
// environment at all and failed its own relay preflight.
func quickstartCredentialEnvForReusedConfig(opts *quickstartOptions, existing *sessionconfig.Config) []string {
	if !opts.CredentialProvided {
		return nil
	}
	// existing.Routes is a map -- iterating it directly and returning on
	// the first static route found put the credential in a random env
	// var whenever more than one static route was configured. Resolve
	// the SAME route roles.execution would actually select instead
	// (modelrole.SelectRoute, probe discarded -- this is about which env
	// var name to use, not whether the route's credential already
	// resolves) and read that one route's own credential_env. A
	// resolution failure (unusable settings, no routes: at all, ...)
	// falls through to the static ANTHROPIC_API_KEY default below.
	if settings, err := existing.ApplySettings(sessionconfig.DefaultSettings()); err == nil {
		probe := func(string, sessionconfig.Route) error { return nil }
		if sel, err := modelrole.SelectRoute(settings, modelrole.RoleExecution, "", "", "", probe); err == nil {
			switch sel.Route.EffectiveCredentialMode() {
			case meter.CredentialModeGitHubCopilot:
				return []string{"GITHUB_COPILOT_TOKEN=" + opts.Credential}
			case meter.CredentialModeChatGPTCodex:
				// No daemon-env credential for this mode at all -- the
				// ChatGPT access token is read fresh from its own auth
				// file at relay-launch time (see quickstartBuildConfig's
				// own "chatgpt-codex" case).
				return nil
			default:
				env := sel.Route.CredentialEnv
				if env == "" {
					env = "ANTHROPIC_API_KEY"
				}
				return []string{env + "=" + opts.Credential}
			}
		}
	}
	return []string{"ANTHROPIC_API_KEY=" + opts.Credential}
}

// quickstartEnsureConfig reuses existing (already resolved by
// quickstartResolveExistingConfig) when non-nil and -reconfigure was not
// passed (plan's "Config" row: "if ~/.config/factoryd/config.yml already
// exists and looks valid, reuse it"), otherwise builds and writes a fresh
// one interactively or from flags. Returns the data directory a later
// worker/submit should use, any credential environment variables the
// daemon's own process needs (never written to config -- see the plan's
// "Does not write credentials to disk"), and whether quickstartEnsureDaemon
// must restart an already-running daemon to pick up a change
// (restartNeeded) -- true not only when a fresh config was written, but
// also when the reused config's own image refs were just updated
// (opts.builtImageRefs) or the operator supplied a new -credential
// alongside the reused config (a Codex review on this PR, round 2: both
// cases leave a live daemon running against stale settings otherwise,
// same as a config rewrite would).
func quickstartEnsureConfig(dp *deps, opts *quickstartOptions, p *quickstartPrompter, w io.Writer, configPath string, existing *sessionconfig.Config) (dataDir string, credentialEnv []string, restartNeeded bool, err error) {
	if existing != nil && !quickstartConfigHasExecutionRole(existing) {
		// An images-only config, e.g. the one `make install`'s
		// configure-images step creates on a fresh machine: it names no
		// model, so reusing it would leave every run with no route. Write
		// a full config instead, keeping the images it already recorded.
		fmt.Fprintf(w, "Session config at %s has no roles.execution (images only); writing routes/models/roles into it.\n", configPath)
		return quickstartWriteNewConfig(dp, opts, p, w, configPath, existing)
	}
	if existing != nil {
		if err := quickstartCheckReusedHarness(opts, existing, configPath); err != nil {
			return "", nil, false, err
		}
		fmt.Fprintf(w, "Reusing existing session config at %s.\n", configPath)
		// resolvedDataDir precedence: explicit -data-dir, then the reused
		// config's own data_dir (dataDirFromConfig -- non-nil AND
		// non-empty; a bare `data_dir:` (null) or `data_dir: ""` counts as
		// "not configured", same as absent), then a fresh
		// sessionconfig.DefaultDataDirFor (the same default the
		// brand-new-config path below already uses). Before this
		// fix, a reused config with no data_dir key fell through to
		// opts.DataDir's own flag default (""), resolved against the
		// shell's cwd -- filepath.Abs("") is the cwd itself, so this
		// created a "requests"/"runs"/"queue" directory right there.
		//
		// Deliberately NOT auto-adopting <cwd>/data even when it already
		// holds real records (an earlier version of this fix tried that,
		// per a follow-up round of that Phase A review): with
		// opts.DataDir's own empty default, the "old default" it probed
		// was the cwd itself, not <cwd>/data, so it never found real
		// records there at all -- and worse, a project root with its own
		// top-level runs/ or queue/ directory (common in ML repos) would
		// have been silently adopted as data_dir, INSIDE the workspace.
		// sessionconfig.DefaultDataDirFor always wins now; quickstartNoticeCwdDataDirRecords
		// below only ever prints a notice, never persists a cwd-derived
		// path without an explicit -data-dir.
		resolvedDataDir := opts.DataDir
		dataDirFromConfig := false
		switch {
		case opts.DataDirExplicit:
		case !existing.DataDirIsDefault():
			resolvedDataDir = existing.EffectiveDataDir()
			dataDirFromConfig = true
		default:
			resolvedDataDir = sessionconfig.DefaultDataDirFor(configPath)
		}
		resolvedDataDir, err = filepath.Abs(resolvedDataDir)
		if err != nil {
			return "", nil, false, fmt.Errorf("resolve data dir: %w", err)
		}
		if !opts.DataDirExplicit && !dataDirFromConfig {
			quickstartNoticeCwdDataDirRecords(w, resolvedDataDir, configPath)
		}
		// Persisted into the config -- not merely resolved in memory -- so
		// every later command (approve/watch/status/worker/serve, all of
		// which resolve -data-dir from session config too) agrees on the
		// same directory instead of only this one quickstart invocation
		// knowing where it put things. doctorRepointDataDir,
		// not a second append-only helper: it already replaces an existing
		// top-level `data_dir:` line in place instead of appending a
		// second, YAML-illegal duplicate key -- an adversarial review of
		// Phase A found this is exactly what a bare `data_dir:`
		// (null) or `data_dir: ""` line (dataDirFromConfig false, but a
		// key already present) would otherwise hit.
		dataDirAdded := false
		if !opts.DataDirExplicit && !dataDirFromConfig {
			if err := doctorRepointDataDir(configPath, existing, resolvedDataDir); err != nil {
				return "", nil, false, err
			}
			dataDirAdded = true
			fmt.Fprintf(w, "Set data_dir: %s in %s (every later command reads it from there).\n", resolvedDataDir, configPath)
		}
		// quickstartEnsureImages (step 1, already run) may have just built
		// one or more images locally after they were found missing. A
		// reused config predates that build and still names the old (or
		// no) ref, so the worker this quickstart invocation is about to
		// spawn would fail against the very image -fix just spent minutes
		// building -- rewrite the reused config with the freshly built
		// refs rather than silently discarding them.
		imagesUpdated := len(opts.builtImageRefs) > 0
		if imagesUpdated {
			if ref, ok := opts.builtImageRefs["-sandbox-image"]; ok {
				existing.SandboxImage = &ref
			}
			if ref, ok := opts.builtImageRefs["-registry-proxy-image"]; ok {
				existing.RegistryProxyImage = &ref
			}
		}
		releaseDefaultsAdded := quickstartBackfillReleaseDefaults(existing)
		if len(releaseDefaultsAdded) > 0 {
			// Appended as its own text edit, independent of the
			// imagesUpdated full-rewrite branch below, so a backfill-only
			// run (the common case: quickstart re-run against an already
			// up-to-date image set) never re-marshals the whole file and
			// loses the operator's own comments/formatting.
			if err := quickstartAppendReleaseDefaultsText(configPath, existing, releaseDefaultsAdded, "factoryd quickstart"); err != nil {
				return "", nil, false, err
			}
			fmt.Fprintf(w, "Added usable release policy default(s) missing from %s: %s (see safety-contract.md's release-gate text; edit config.yml to change).\n", configPath, strings.Join(releaseDefaultsAdded, ", "))
		}
		if imagesUpdated {
			if err := quickstartWriteConfig(configPath, existing); err != nil {
				return "", nil, false, err
			}
			fmt.Fprintf(w, "Updated %s with the image(s) just built locally.\n", configPath)
		}
		credentialEnv := quickstartCredentialEnvForReusedConfig(opts, existing)
		return resolvedDataDir, credentialEnv, imagesUpdated || len(releaseDefaultsAdded) > 0 || dataDirAdded || opts.CredentialProvided, nil
	}

	return quickstartWriteNewConfig(dp, opts, p, w, configPath, nil)
}

// quickstartWriteNewConfig builds and writes a full config. imagesFrom,
// when non-nil, is an images-only config being completed: its recorded
// images, image_source_root and data_dir carry over unless this run
// built or was given newer ones.
func quickstartWriteNewConfig(dp *deps, opts *quickstartOptions, p *quickstartPrompter, w io.Writer, configPath string, imagesFrom *sessionconfig.Config) (dataDir string, credentialEnv []string, restartNeeded bool, err error) {
	dataDir = opts.DataDir
	if !opts.DataDirExplicit {
		// Outside the target repo; for a profile, under the one directory
		// the Docker VM shares read-write rather than next to the config,
		// which the VM must not be able to write (sessionconfig.DataRoot).
		dataDir = sessionconfig.DefaultDataDirFor(configPath)
		if imagesFrom != nil && imagesFrom.DataDir != nil && *imagesFrom.DataDir != "" {
			dataDir = *imagesFrom.DataDir
		}
	}
	dataDir, err = filepath.Abs(dataDir)
	if err != nil {
		return "", nil, false, fmt.Errorf("resolve data dir: %w", err)
	}
	fmt.Fprintf(w, "Data directory: %s\n", dataDir)

	cfg, credentialEnv, err := quickstartBuildConfig(dp, opts, p, w, dataDir)
	if err != nil {
		return "", nil, false, err
	}
	if imagesFrom != nil {
		keep := func(dst **string, src *string) {
			if *dst == nil && src != nil && *src != "" {
				*dst = src
			}
		}
		keep(&cfg.SandboxImage, imagesFrom.SandboxImage)
		keep(&cfg.RegistryProxyImage, imagesFrom.RegistryProxyImage)
		keep(&cfg.MeterImage, imagesFrom.MeterImage)
		keep(&cfg.ImageSourceRoot, imagesFrom.ImageSourceRoot)
	}
	if err := quickstartWriteConfig(configPath, cfg); err != nil {
		return "", nil, false, err
	}
	fmt.Fprintf(w, "Wrote session config to %s.\n", configPath)
	return dataDir, credentialEnv, true, nil
}

// quickstartCheckRepoMountVisible refuses, before any request is submitted, a
// repository a sandbox could not bind: first one outside the home directory
// (the OpenShell gateway sees the home directory and nothing else, and refuses
// a bind whose source it cannot see, on any Docker), then one the Docker VM
// does not share (doctor's own probe). Otherwise the first sandbox launch
// fails minutes in with a bare "bind source path does not exist". The probe is
// skipped when the config records no sandbox image or the image is not in the
// local store (nothing to probe with); the home check needs no Docker.
func quickstartCheckRepoMountVisible(opts *quickstartOptions, configPath, repoRoot string) error {
	if err := quickstartRepoUnderHome(repoRoot); err != nil {
		return err
	}
	cfg, err := sessionconfig.Load(configPath)
	if err != nil || cfg.SandboxImage == nil || *cfg.SandboxImage == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if !sandbox.ImagePresent(ctx, opts.SandboxDocker, *cfg.SandboxImage) {
		return nil
	}
	check := doctorCheckMountVisibilityFor(ctx, opts.SandboxDocker, *cfg.SandboxImage, repoRoot,
		"repository", "pass a real, existing git checkout as <repo-path>")
	if check.Err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w\n%s", check.Name, check.Err, check.Fix)
}

// quickstartRepoUnderHome fails when repoRoot (symlinks resolved) is not inside
// the user's home directory.
func quickstartRepoUnderHome(repoRoot string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	resolve := func(p string) string {
		if abs, err := filepath.Abs(p); err == nil {
			p = abs
		}
		if r, err := filepath.EvalSymlinks(p); err == nil {
			p = r
		}
		return p
	}
	repo, homeDir := resolve(repoRoot), resolve(home)
	if rel, err := filepath.Rel(homeDir, repo); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil
	}
	return fmt.Errorf("the repository %s is outside your home directory (%s): the sandbox gateway only sees your home directory, so a build there fails minutes in with \"bind source path does not exist\"; clone or move the repository under %s and rerun", repo, homeDir, homeDir)
}

// quickstartConfigHasExecutionRole reports whether cfg names a model for
// roles.execution, i.e. whether it's a config quickstart can reuse as is.
func quickstartConfigHasExecutionRole(cfg *sessionconfig.Config) bool {
	return cfg.Roles != nil && cfg.Roles.Execution != nil && cfg.Roles.Execution.Model != ""
}

// quickstartPiforkDoctorInputs points the image-presence doctor pass at the
// pifork image. A fresh scaffold has no roles yet, so doctor checks every role
// as pifork (a pifork sandbox_image is the pifork worker image, never one
// `make sandbox-image` may rebuild over). A config that already has roles keeps
// them untouched: the normal per-role harness sets decide what gets probed,
// including any allowed pi or an unset role's default.
func quickstartPiforkDoctorInputs(in *doctorInputs) {
	if in.settings.Roles == nil {
		in.harnessOverride = harness.Pifork
	}
}

// quickstartCheckReusedHarness refuses -harness pifork that disagrees with
// a config quickstart is about to reuse (some role not on pifork, or another
// sandbox image), rather than silently running on the config's own harnesses
// and image.
func quickstartCheckReusedHarness(opts *quickstartOptions, existing *sessionconfig.Config, configPath string) error {
	if opts.Harness != harness.Pifork {
		return nil
	}
	existingImage := ""
	if existing.SandboxImage != nil {
		existingImage = *existing.SandboxImage
	}
	allPifork := existing.Roles != nil
	if allPifork {
		for _, rc := range []*sessionconfig.RoleConfig{existing.Roles.Planning, existing.Roles.Execution, existing.Roles.Review} {
			if rc != nil && rc.HarnessName() != harness.Pifork {
				allPifork = false
			}
		}
	}
	if !allPifork || existingImage != opts.SandboxImage {
		return fmt.Errorf("-harness pifork -sandbox-image %s differs from the existing config at %s (some role is not on the pifork harness, or sandbox_image is %q): pass -reconfigure to rewrite it", opts.SandboxImage, configPath, existingImage)
	}
	return nil
}

// quickstartWriteConfig marshals cfg (the same sessionconfig.Config shape
// `factoryd init-config`'s own YAML scaffold parses into) and writes it,
// overwriting whatever was there before -- callers only reach this once
// they've already decided a fresh config is wanted (missing, or
// -reconfigure).
func quickstartWriteConfig(path string, cfg *sessionconfig.Config) error {
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshal session config: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(path), err)
	}
	header := "# factoryd session config, written by `factoryd quickstart`.\n# Each key mirrors a `factoryd worker` flag; edit freely -- see `factoryd init-config`'s own scaffold and USAGE.md for the full key reference.\n"
	if err := os.WriteFile(path, append([]byte(header), data...), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// quickstartRouteOption is one choice quickstart's own "how should the
// daemon reach a model?" prompt offers. Detected marks a login already
// found on this host -- quickstartDetectRouteOptions sorts detected
// options first: an operator with a working
// ChatGPT/Copilot/Anthropic login already on the machine should never have
// to hand-type a route quickstart could see for itself). DetectedDetail is
// a human-readable, NEVER-secret hint (a file path, "ANTHROPIC_API_KEY is
// set") shown next to the label -- detection checks existence and
// structure only and never reads, logs, or returns a token/credential
// value (see quickstartDetectCodexLogin/quickstartDetectCopilotLogin).
type quickstartRouteOption struct {
	Route          string
	Label          string
	Detected       bool
	DetectedDetail string
	// AutoSelectable marks a Detected option as safe for
	// -non-interactive's own single-unambiguous-detection auto-pick
	// (a round-2 review): true only for a file-based login
	// (chatgpt-codex, copilot) that this process had to go looking for.
	// ANTHROPIC_API_KEY is deliberately NOT auto-selectable even when
	// Detected -- it is routine for anyone who also uses Claude Code, has
	// nothing to do with which model route THIS invocation should use,
	// and auto-picking it under -non-interactive could silently write an
	// anthropic config and bill an inherited key the operator never
	// intended for this route. It can still be picked interactively (the
	// prompt below shows it, "detected" label included) or explicitly via
	// -route anthropic.
	AutoSelectable bool
	// copilotTokenFile is set only for the "copilot" option, and only when
	// Detected -- the discovered auth.json path quickstartBuildConfig's
	// own copilot branch persists into relay_github_token_file, never a
	// pasted token.
	copilotTokenFile string
}

// quickstartDetectCodexLogin reports whether a structurally valid ChatGPT
// Codex OAuth login exists on this host: the same auth.json
// resolveChatGPTCodexCredential resolves at relay-launch time
// (-relay-codex-auth-file, then $CODEX_HOME/auth.json, then
// ~/.codex/auth.json), parsed and checked for auth_mode "chatgpt" plus
// both token fields present. Existence and shape only, deliberately NOT
// the expiry check resolveChatGPTCodexCredential itself enforces
// fail-closed at launch time -- a login merely close to expiring should
// still be offered here (doctor's own "chatgpt codex credential" check is
// the later, authoritative gate). Never reads the token value into a
// variable this function returns or logs.
func quickstartDetectCodexLogin() bool {
	path, err := resolveCodexAuthFilePath("")
	if err != nil {
		return false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var auth codexAuthFileCredential
	if err := json.Unmarshal(data, &auth); err != nil {
		return false
	}
	return auth.AuthMode == "chatgpt" && auth.Tokens.AccessToken != "" && auth.Tokens.AccountID != ""
}

// quickstartDetectCopilotLogin reports whether a structurally valid pi
// GitHub Copilot OAuth login exists on this host, mirroring
// quickstartDetectCodexLogin: existence and shape only (a real "oauth"
// entry with a non-empty refresh token under -relay-github-token-key's own
// default provider id, at githubCopilotAuthPaths' location), reusing parseGitHubCopilotAuthFile rather than duplicating
// its parsing. The returned path is exactly the value
// -relay-github-token-file/relay_github_token_file take -- never a token
// value, which parseGitHubCopilotAuthFile's own error messages already
// take care never to include either.
func quickstartDetectCopilotLogin() (detected bool, path string) {
	home, err := os.UserHomeDir()
	if err != nil {
		return false, ""
	}
	for _, p := range githubCopilotAuthPaths(home) {
		if _, err := parseGitHubCopilotAuthFile(p, defaultGitHubCopilotTokenKey, "discovered auth file"); err == nil {
			return true, p
		}
	}
	return false, ""
}

// quickstartDetectRouteOptions builds every route quickstart offers,
// detected logins first (a stable sort: ties keep this list's own order --
// chatgpt-codex, copilot, anthropic, openai). openai is never "detected":
// unlike the other three, there is no login to discover -- an operator
// always names a model host explicitly for it.
func quickstartDetectRouteOptions() []quickstartRouteOption {
	codexDetected := quickstartDetectCodexLogin()
	copilotDetected, copilotPath := quickstartDetectCopilotLogin()
	anthropicDetected := os.Getenv("ANTHROPIC_API_KEY") != ""

	options := []quickstartRouteOption{
		{Route: "chatgpt-codex", Label: "ChatGPT (via your Codex login)", Detected: codexDetected, DetectedDetail: "found a ChatGPT login in codex-cli's own auth.json", AutoSelectable: true},
		{Route: "copilot", Label: "GitHub Copilot", Detected: copilotDetected, DetectedDetail: "found a Copilot login at " + copilotPath, copilotTokenFile: copilotPath, AutoSelectable: true},
		{Route: "anthropic", Label: "Anthropic API key", Detected: anthropicDetected, DetectedDetail: "ANTHROPIC_API_KEY is set"},
		{Route: "openai", Label: "OpenAI-compatible endpoint (local model server, OpenRouter, etc.)"},
	}
	sort.SliceStable(options, func(i, j int) bool { return options[i].Detected && !options[j].Detected })
	return options
}

// doctorSubscriptionLoginsLine is `factoryd doctor`'s route-visibility info
// line (route-visibility plan, 2026-09-25): an operator asking "which other
// subscription logins exist on this machine" -- e.g. before switching
// worker's route -- otherwise has no answer short of re-running
// `factoryd quickstart` and reading its interactive picker. Reuses
// quickstartDetectCodexLogin/quickstartDetectCopilotLogin rather than a
// separate check, so this line and quickstart's own route picker can never
// disagree about what counts as "detected" -- existence/shape only, same
// as those two functions' own doc comments, never a validity or expiry
// check, hence "detected" in the label rather than "available"/"valid".
func doctorSubscriptionLoginsLine() string {
	var found []string
	if quickstartDetectCodexLogin() {
		found = append(found, "chatgpt-codex")
	}
	if detected, _ := quickstartDetectCopilotLogin(); detected {
		found = append(found, "copilot")
	}
	if len(found) == 0 {
		return "subscription logins detected: none"
	}
	return "subscription logins detected: " + strings.Join(found, ", ")
}

// quickstartWeakModelAdvisories is a small, evidence-dated list of model
// ids observed too weak for the build loop -- deliberately not a
// "good models" list that would silently rot: each entry names the date
// and the run that found it, and quickstartWarnIfWeakModel only ever
// warns, never blocks, since a weak-today model can be fixed upstream and
// a strong-today one can regress.
var quickstartWeakModelAdvisories = map[string]string{
	"gpt-4.1": "2026-09-25 onboarding walk (github-copilot route, guessed as \"the base model every Copilot plan includes\"): wrote a vague first spec, flaked in planning (plan_tickets.py exited 2), and then could not finish a small ticket -- it wrote a test file cut off mid-way and looped (\"the file is already correct\") until the relay's own token budget returned 429. gpt-5.6-luna via chatgpt-codex did the same size of task on the first try.",
}

// quickstartWarnIfWeakModel prints quickstartWeakModelAdvisories' own
// entry for modelID, if any, as a warning -- called after a model id is
// resolved for every route that can pick a weak one (openai, copilot,
// chatgpt-codex), never as a reason to refuse it.
func quickstartWarnIfWeakModel(w io.Writer, modelID string) {
	if evidence, weak := quickstartWeakModelAdvisories[modelID]; weak {
		fmt.Fprintf(w, "Warning: %q has been observed too weak for the build loop -- %s\n", modelID, evidence)
	}
}

// quickstartBuildConfig prompts for (or reads from flags) whichever model
// route the operator chooses -- OpenAI-compatible endpoint, Anthropic API
// key, GitHub Copilot, or ChatGPT via a Codex login -- and returns the
// sessionconfig.Config to write plus any credential environment variables
// the daemon process needs, including chatgpt-codex and the
// detected-login-first route prompt.
// anthropicOpenAICompatDefaultModelID/anthropicOpenAICompatDefaultContextWindow
// are `quickstart -route anthropic`'s own defaults when the operator
// gives no -model-id/-context-window. This route targets Anthropic's
// OpenAI-compatible endpoint (https://api.anthropic.com/v1, Chat
// Completions-shaped), not its native Messages API -- an operator
// decision (2026-09-27) to keep this route representable in routes:/
// models: mode at all (a routes: mode Model.ID is required non-empty;
// the native Messages API only works with an EMPTY worker model id,
// which routes: mode has no way to express -- see the git history on
// this route's own case in quickstartBuildConfig for the fuller
// analysis). Placeholder, not live-tested: nothing in this repo's own
// live-validation runs has exercised Anthropic's OpenAI-compatible
// endpoint yet.
const (
	anthropicOpenAICompatDefaultModelID       = "claude-sonnet-5"
	anthropicOpenAICompatDefaultContextWindow = 200000
)

// applyHarness settles the harness (askHarness) and writes it, and the
// worker image that goes with it, into the config being built.
func (b *quickstartConfigBuild) applyHarness(route string) error {
	if err := b.askHarness(route); err != nil {
		return err
	}
	roles := []*sessionconfig.RoleConfig{b.cfg.Roles.Planning, b.cfg.Roles.Execution, b.cfg.Roles.Review}
	if b.opts.Harness == harness.Pifork || b.opts.Harness == harness.Codex {
		for _, rc := range roles {
			if rc != nil {
				rc.Harness = b.opts.Harness
			}
		}
	}
	if b.opts.Harness == harness.Pifork {
		image := b.opts.SandboxImage
		b.cfg.SandboxImage = &image
	} else if ref, ok := b.opts.builtImageRefs["-sandbox-image"]; ok {
		b.cfg.SandboxImage = &ref
	}
	return nil
}

// askHarness asks which coding agent runs the work, only where the answer
// is open: an interactive run, no -harness given, and a route that can run
// more than one (chatgpt-codex: Pi or the Codex CLI, which needs its
// Responses API). Every other route runs Pi and is not asked.
func (b *quickstartConfigBuild) askHarness(route string) error {
	if b.opts.NonInteractive || b.opts.HarnessExplicit || route != "chatgpt-codex" {
		return nil
	}
	if b.opts.Harness != "" && b.opts.Harness != harness.Pi {
		return nil
	}
	choice, err := b.p.choose(b.w, "Which coding agent should do the work?", []string{
		"pi (the default harness)",
		"codex (the Codex CLI)",
	})
	if err != nil {
		return err
	}
	if choice == 1 {
		b.opts.Harness = harness.Codex
	}
	return nil
}

// quickstartSingleModelRoles builds the roles: block every quickstart
// route writes once it has resolved a single models: entry named model:
// planning/execution/review all name that one model (quickstart never
// configures more than one), and review sets allow_shared_model: true --
// ValidateRouting's review/execution independence rule exists to catch an
// operator who accidentally reuses a builder's own model as its
// reviewer, which cannot happen here since there is only ever one model
// to begin with.
func quickstartSingleModelRoles(model string) *sessionconfig.Roles {
	return &sessionconfig.Roles{
		Planning:  &sessionconfig.RoleConfig{Model: model},
		Execution: &sessionconfig.RoleConfig{Model: model},
		Review:    &sessionconfig.RoleConfig{Model: model, AllowSharedModel: true},
	}
}

// quickstartConfigBuild is what the route cases of one quickstartBuildConfig
// call share: its parameters, the config being filled and the credential
// environment for the worker.
type quickstartConfigBuild struct {
	// dp is the external boundaries this call reaches.
	dp            *deps
	opts          *quickstartOptions
	w             io.Writer
	p             *quickstartPrompter
	cfg           *sessionconfig.Config
	credentialEnv []string
}

func quickstartBuildConfig(dp *deps, opts *quickstartOptions, p *quickstartPrompter, w io.Writer, dataDir string) (*sessionconfig.Config, []string, error) {
	b := &quickstartConfigBuild{dp: dp, opts: opts, p: p, w: w}
	route := b.opts.Route
	if route == "" {
		// Detection (real filesystem/env probes) only runs when actually
		// needed -- an explicit -route never touches ~/.codex/auth.json or
		// a pi auth.json at all, which also keeps every existing
		// -route openai/anthropic test hermetic without having to sandbox
		// HOME.
		routeOptions := quickstartDetectRouteOptions()
		if b.opts.NonInteractive {
			// Only file-based logins auto-select (a round-2 review):
			// see AutoSelectable's own doc comment for why ANTHROPIC_API_KEY
			// alone never silently picks -route anthropic here.
			var detected []quickstartRouteOption
			for _, o := range routeOptions {
				if o.Detected && o.AutoSelectable {
					detected = append(detected, o)
				}
			}
			switch len(detected) {
			case 0:
				return nil, nil, fmt.Errorf("-route is required under -non-interactive (chatgpt-codex, copilot, anthropic, or openai) when no valid session config already exists and no login was detected")
			case 1:
				route = detected[0].Route
				fmt.Fprintf(b.w, "Detected %s (%s); using -route %s (pass -route to override).\n", detected[0].Label, detected[0].DetectedDetail, detected[0].Route)
			default:
				names := make([]string, len(detected))
				for i, o := range detected {
					names[i] = o.Route
				}
				return nil, nil, fmt.Errorf("-route is required under -non-interactive: multiple logins detected (%s) -- pass -route to pick one", strings.Join(names, ", "))
			}
		} else {
			labels := make([]string, len(routeOptions))
			for i, o := range routeOptions {
				labels[i] = o.Label
				if o.Detected {
					labels[i] = fmt.Sprintf("%s (detected: %s)", o.Label, o.DetectedDetail)
				}
			}
			choice, err := b.p.choose(b.w, "How should the daemon reach a model?", labels)
			if err != nil {
				return nil, nil, err
			}
			route = routeOptions[choice].Route
		}
	}

	b.cfg = &sessionconfig.Config{}

	switch route {
	case "openai":
		if r0, r1, err := b.openAIRoute(); err != nil {
			return r0, r1, err
		}
	case "anthropic":
		if r0, r1, err := b.anthropicRoute(); err != nil {
			return r0, r1, err
		}
	case "copilot":
		if r0, r1, err := b.copilotRoute(); err != nil {
			return r0, r1, err
		}
	case "chatgpt-codex":
		if r0, r1, err := b.chatGPTCodexRoute(); err != nil {
			return r0, r1, err
		}
	default:
		return nil, nil, fmt.Errorf("-route must be \"openai\", \"anthropic\", \"copilot\", or \"chatgpt-codex\", got %q", route)
	}

	absDataDir := dataDir
	b.cfg.DataDir = &absDataDir

	if b.opts.EgressCABundle != "" {
		// A round-3 review found this must be absolute'd before persisting
		// (a relative -egress-ca-bundle would resolve against whatever
		// directory a LATER command happens to run from, not this one) and
		// confirmed to actually load -- via meter.OutboundTransport, the exact
		// parser every real relay/registry-proxy launch and doctor's own
		// listing calls use -- before it's ever written into the config,
		// rather than failing silently at the first real run that reads
		// it back.
		absCABundle, err := filepath.Abs(b.opts.EgressCABundle)
		if err != nil {
			return nil, nil, fmt.Errorf("resolve -egress-ca-bundle %q: %w", b.opts.EgressCABundle, err)
		}
		if _, err := meter.OutboundTransport(absCABundle); err != nil {
			return nil, nil, fmt.Errorf("-egress-ca-bundle %s: %w", absCABundle, err)
		}
		b.cfg.EgressCABundle = &absCABundle
	}

	maxFiles := quickstartDefaultReleaseMaxFilesChanged
	maxInsertions := quickstartDefaultReleaseMaxInsertions
	rollbackPlan := quickstartDefaultReleaseRollbackPlan
	b.cfg.ReleaseMaxFilesChanged = &maxFiles
	b.cfg.ReleaseMaxInsertions = &maxInsertions
	b.cfg.ReleaseRollbackPlan = &rollbackPlan

	// A freshly written config turns the standalone AI code-review pass on
	// by default (worker's own bare -code-review-policy flag default is
	// "off", kept that way for an operator already running without a
	// config -- see that flag's own doc comment) -- quickstart is the
	// setup path for a NEW factory, so its own defaults are the ones an
	// operator should actually run with. Only set here, in
	// quickstartWriteNewConfig's own path: a config quickstart is REUSING
	// (quickstartEnsureConfig's early return above) is never rewritten
	// with this, so an operator who already chose "off" keeps it.
	codeReviewPolicy := codereview.PolicyRequired
	b.cfg.CodeReviewPolicy = &codeReviewPolicy

	if err := b.applyHarness(route); err != nil {
		return nil, nil, err
	}
	if ref, ok := b.opts.builtImageRefs["-registry-proxy-image"]; ok {
		b.cfg.RegistryProxyImage = &ref
	}
	// Deliberately NOT setting cfg.RegistryProxy = true here (a Codex
	// review on this PR, P1): worker_config.go's own startup check requires
	// -sandbox-image whenever -registry-proxy is "deliberate" (an
	// explicit flag or an explicit registry_proxy config key). Writing
	// registry_proxy: true here would make every quickstart-generated
	// config "deliberate" and trip that check on the very next spawn if
	// cfg.SandboxImage somehow ended up unset, refusing to start on what
	// should be the common, successful path. Leaving the key entirely
	// unset lets worker's own non-deliberate default-on behavior apply
	// instead, which the same check explicitly exempts.

	// G17b: an operator who never sets -pr-trusted-authors/
	// pr_trusted_authors has every one of their own PR review comments
	// on the very draft PR quickstart is about to open silently ignored
	// by pr_review_driver's forge.AuthorPolicy (an untrusted-author
	// comment is never even read as a review request) -- defaulting to
	// their own gh login closes that trap for the common case without
	// requiring a flag they'd have no reason to know exists yet.
	if login := quickstartDefaultPRTrustedAuthor(dp); login != "" {
		b.cfg.PRTrustedAuthors = []string{login}
		fmt.Fprintf(b.w, "Defaulting -pr-trusted-authors to %q (the current `gh` user).\n", login)
	}

	return b.cfg, b.credentialEnv, nil
}

// openAIRoute fills the config for an OpenAI-compatible endpoint: upstream, model and credential.
func (b *quickstartConfigBuild) openAIRoute() (*sessionconfig.Config, []string, error) {
	var err error
	host := b.opts.ModelHost
	if host == "" {
		if b.opts.NonInteractive {
			return nil, nil, fmt.Errorf("-model-host is required under -non-interactive for -route openai")
		}
		host, err = b.p.ask(b.w, "Model host (bare API root, e.g. http://100.x.y.z:8080 -- no /v1): ")
		if err != nil {
			return nil, nil, err
		}
	}
	host = strings.TrimRight(host, "/")
	if host == "" {
		return nil, nil, fmt.Errorf("a model host is required for -route openai")
	}

	modelID := b.opts.ModelID
	if modelID == "" {
		if b.opts.NonInteractive {
			return nil, nil, fmt.Errorf("-model-id is required under -non-interactive for -route openai")
		}
		modelID, err = quickstartPickModelID(b.p, b.w, host)
		if err != nil {
			return nil, nil, err
		}
	}
	if strings.Contains(modelID, "/") {
		return nil, nil, fmt.Errorf("-model-id must not contain a slash (the relay rejects it later anyway), got %q", modelID)
	}
	quickstartWarnIfWeakModel(b.w, modelID)

	contextWindow := b.opts.ContextWindow
	if !b.opts.ContextWindowExplicit {
		if b.opts.NonInteractive {
			return nil, nil, fmt.Errorf("-context-window is required under -non-interactive for -route openai")
		}
		var cwText string
		cwText, err = b.p.ask(b.w, "Model context window, in tokens (e.g. 131072): ")
		if err != nil {
			return nil, nil, err
		}
		contextWindow, err = strconv.Atoi(strings.TrimSpace(cwText))
		if err != nil {
			return nil, nil, fmt.Errorf("context window must be an integer: %w", err)
		}
	}
	if contextWindow <= 0 {
		return nil, nil, fmt.Errorf("-context-window must be a positive integer, got %d", contextWindow)
	}

	// Deliberately does NOT let ANTHROPIC_API_KEY's mere presence in
	// the environment decide needsCredential on its own when
	// interactive: that env var is routine for anyone who also uses
	// Claude Code, has nothing to do with whether THIS
	// OpenAI-compatible endpoint needs a credential, and silently
	// skipping the confirmation below on its account previously meant
	// a real Anthropic key could be configured (and later sent) to an
	// entirely unrelated host without the operator ever being asked
	// (a Codex review on this PR, round 3). Under -non-interactive,
	// where there is no prompt to ask, presence still resolves the
	// question -- consistent with -credential's own documented
	// ANTHROPIC_API_KEY fallback for this route.
	needsCredential := b.opts.CredentialProvided
	if !needsCredential {
		if b.opts.NonInteractive {
			needsCredential = os.Getenv("ANTHROPIC_API_KEY") != ""
		} else {
			prompt := "Does this endpoint require a credential? [y/N] "
			if os.Getenv("ANTHROPIC_API_KEY") != "" {
				prompt = "Does this endpoint require a credential? (ANTHROPIC_API_KEY is set in your environment, but that alone doesn't mean this endpoint needs it -- confirm explicitly.) [y/N] "
			}
			needsCredential, err = b.p.confirm(b.w, prompt, false)
			if err != nil {
				return nil, nil, err
			}
		}
	}
	allowNoCredential := true
	// Default is x-api-key; a credentialed OpenAI-compatible endpoint
	// usually wants Authorization instead (sessionconfig.Example's own
	// documented guidance for this same key).
	header := "Authorization"
	if needsCredential {
		allowNoCredential = false
		key := b.opts.Credential
		if key == "" {
			key = os.Getenv("ANTHROPIC_API_KEY")
		}
		if key == "" {
			if b.opts.NonInteractive {
				return nil, nil, fmt.Errorf("a credential is required for this route: pass -credential, or set ANTHROPIC_API_KEY")
			}
			key, err = b.p.askSecret(b.w, "Credential (kept only in the daemon process's own environment, never written to config): ")
			if err != nil {
				return nil, nil, err
			}
			if key == "" {
				return nil, nil, fmt.Errorf("a credential is required for this route")
			}
		}
		b.credentialEnv = append(b.credentialEnv, "ANTHROPIC_API_KEY="+key)
	} else {
		header = meter.CredentialHeaderXAPIKey
	}
	allowPlaintext := strings.HasPrefix(host, "http://")
	basePath := "/v1"
	allowedPrefix := "/v1"
	routeName := "local"
	modelKey := modelID
	localRoute := sessionconfig.Route{
		Upstream:               host,
		AllowedPathPrefix:      allowedPrefix,
		WorkerBasePath:         &basePath,
		CredentialHeader:       header,
		AllowPlaintextUpstream: allowPlaintext,
		AllowNoCredential:      allowNoCredential,
	}
	if needsCredential {
		localRoute.CredentialEnv = "ANTHROPIC_API_KEY"
	}
	b.cfg.Routes = map[string]sessionconfig.Route{routeName: localRoute}
	b.cfg.Models = map[string]sessionconfig.Model{
		modelKey: {
			ID:            modelID,
			Routes:        []string{routeName},
			ContextWindow: contextWindow,
		},
	}
	b.cfg.Roles = quickstartSingleModelRoles(modelKey)
	return nil, nil, nil
}

// anthropicRoute fills the config for the Anthropic API route.
func (b *quickstartConfigBuild) anthropicRoute() (*sessionconfig.Config, []string, error) {
	var err error
	key := b.opts.Credential
	if key == "" {
		key = os.Getenv("ANTHROPIC_API_KEY")
	}
	if key == "" {
		if b.opts.NonInteractive {
			return nil, nil, fmt.Errorf("-credential is required under -non-interactive for -route anthropic (or set ANTHROPIC_API_KEY)")
		}
		key, err = b.p.askSecret(b.w, "Anthropic API key (kept only in the daemon process's own environment, never written to config): ")
		if err != nil {
			return nil, nil, err
		}
		if key == "" {
			return nil, nil, fmt.Errorf("an API key is required for -route anthropic")
		}
	}
	b.credentialEnv = append(b.credentialEnv, "ANTHROPIC_API_KEY="+key)

	modelID := b.opts.ModelID
	if modelID == "" {
		modelID = anthropicOpenAICompatDefaultModelID
	}
	if strings.Contains(modelID, "/") {
		return nil, nil, fmt.Errorf("-model-id must not contain a slash, got %q", modelID)
	}
	quickstartWarnIfWeakModel(b.w, modelID)

	contextWindow := b.opts.ContextWindow
	if !b.opts.ContextWindowExplicit {
		contextWindow = anthropicOpenAICompatDefaultContextWindow
	}
	if contextWindow <= 0 {
		return nil, nil, fmt.Errorf("-context-window must be a positive integer, got %d", contextWindow)
	}

	routeName := "anthropic"
	modelKey := modelID
	basePath := "/v1"
	// This targets Anthropic's OpenAI-compatible endpoint
	// (https://api.anthropic.com/v1, Chat Completions-shaped), not
	// its native Messages API -- see anthropicOpenAICompatDefaultModelID's
	// own doc comment for why. Untested live (placeholder). No
	// credential_header is set: empty currently defaults to
	// X-Api-Key (internal/sandbox/relay_lifecycle.go, "Anthropic's
	// own convention"), which is likely wrong for this
	// OpenAI-compatible endpoint's own Authorization: Bearer
	// convention -- left as the operator's own explicit choice
	// rather than silently overridden; revisit once this route is
	// actually live-validated.
	b.cfg.Routes = map[string]sessionconfig.Route{
		routeName: {
			CredentialMode:    meter.CredentialModeStatic,
			Upstream:          "https://api.anthropic.com",
			AllowedPathPrefix: "/v1",
			WorkerBasePath:    &basePath,
			CredentialEnv:     "ANTHROPIC_API_KEY",
		},
	}
	b.cfg.Models = map[string]sessionconfig.Model{
		modelKey: {
			ID:            modelID,
			API:           meter.RequestFormatOpenAICompletions,
			Routes:        []string{routeName},
			ContextWindow: contextWindow,
		},
	}
	b.cfg.Roles = quickstartSingleModelRoles(modelKey)
	return nil, nil, nil
}

// copilotRoute fills the config for the GitHub Copilot route: token source, model and its limits.
func (b *quickstartConfigBuild) copilotRoute() (*sessionconfig.Config, []string, error) {
	var err error
	// A round-2 review found: under -non-interactive
	// a missing -model-id fails regardless of what the listing below
	// would find, so check it FIRST -- before ever resolving a token
	// or making a network call that could take up to 15s only to be
	// thrown away.
	if b.opts.ModelID == "" && b.opts.NonInteractive {
		return nil, nil, fmt.Errorf("-model-id is required under -non-interactive for -route copilot")
	}

	// Source precedence: an explicit -credential, then
	// GITHUB_COPILOT_TOKEN, then the auto-discovered pi login
	// file -- a round-2 review. An operator who explicitly names
	// a credential must have it win, every time, over whatever account
	// pi happens to be logged in as on this machine; auto-
	// discovery is the convenience
	// fallback for "nothing else was given", never a silent override --
	// before this fix, discoveredTokenFile won unconditionally, so an
	// explicit -credential/GITHUB_COPILOT_TOKEN was silently ignored
	// and the persisted relay_github_token_file kept steering every
	// later run at pi's account instead. usedTokenFile, when set, is
	// what gets persisted into relay_github_token_file below -- a
	// path, never a token; only the WINNING source's identity is ever
	// printed, never a credential value.
	var preresolvedToken, usedTokenFile string
	switch {
	case b.opts.Credential != "":
		preresolvedToken = b.opts.Credential
		fmt.Fprintln(b.w, "Using the GitHub Copilot token from -credential.")
	case os.Getenv("GITHUB_COPILOT_TOKEN") != "":
		preresolvedToken = os.Getenv("GITHUB_COPILOT_TOKEN")
		fmt.Fprintln(b.w, "Using the GitHub Copilot token from GITHUB_COPILOT_TOKEN.")
	default:
		if ok, path := quickstartDetectCopilotLogin(); ok {
			usedTokenFile = path
			preresolvedToken, err = resolveGitHubCopilotToken(path, "")
			if err != nil {
				return nil, nil, fmt.Errorf("discovered GitHub Copilot login at %s could not be read: %w", path, err)
			}
			fmt.Fprintf(b.w, "Using the GitHub Copilot login discovered at %s (never pasted, never written to config as a token).\n", path)
		}
	}

	// Interactive only: without a token from any of the sources above,
	// prompt for one now -- before the listing below -- so the picker
	// and the worker-API verification below actually have a real
	// token to list against, rather than silently skipping both and
	// asking for a token only once everything else is already
	// resolved (a round-2 review finding). -non-interactive keeps its
	// own -credential-is-required failure further down, after
	// -context-window (TestQuickstartBuildConfigNonInteractiveCopilotRequiresModelIDAndCredential's
	// own ordering) -- it can't prompt, so there is nothing to do here.
	if preresolvedToken == "" && !b.opts.NonInteractive {
		preresolvedToken, err = b.p.askSecret(b.w, "GitHub OAuth token (kept only in the daemon process's own environment, never written to config): ")
		if err != nil {
			return nil, nil, err
		}
	}

	// List the models this token is actually entitled to (host-side,
	// reusing the same token-exchange doctor's own copilot check
	// performs) rather than asking the operator to guess an id, the
	// way -route copilot did before this fix (the walk's gpt-4.1
	// guess). Run whenever a token is already available, even with
	// both -model-id and -context-window given explicitly: the
	// listing is also how the worker-API check below (a /responses-
	// only model like gpt-5.6-luna needs relay_worker_api set) gets
	// verified at all, not only how a missing id/context-window gets
	// filled in -- a round-1 review found writing a completions
	// config for an unverified model silently instead of checking it
	// against the listing whenever possible. A listing failure is
	// never fatal here -- it falls back to the free-text prompt this
	// route always had, exactly like quickstartPickModelID's own
	// openai fallback, and the worker-API check below degrades to an
	// unverified warning. opts.EgressCABundle threads the operator's
	// own -egress-ca-bundle through, same as a real run would trust
	// (per that same round-2 review).
	var models []meter.CopilotModel
	listingAttempted := false
	var listingErr error
	if preresolvedToken != "" {
		listingAttempted = true
		listCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		models, listingErr = listGitHubCopilotModelsFn(listCtx, b.opts.EgressCABundle, preresolvedToken, "")
		cancel()
		if listingErr != nil {
			// Worded by whether a model id is already in hand: with one,
			// there is nothing left to "pick manually" -- the operator
			// needs to know their choice couldn't be checked instead.
			if b.opts.ModelID != "" {
				fmt.Fprintf(b.w, "Could not list entitled Copilot models (%v) -- could not verify -model-id %s's worker API.\n", listingErr, b.opts.ModelID)
			} else {
				fmt.Fprintf(b.w, "Could not list entitled Copilot models (%v) -- pick a model id manually.\n", listingErr)
			}
			models = nil
		}
	}

	// doctorChecksFor runs doctorCheckContextWindowConfigured
	// unconditionally whenever relayWorkerModelID is non-empty -- not
	// gated on credential mode -- so a copilot config needs
	// contextWindow exactly as much as an openai one does, despite
	// -context-window's own flag help once claiming this route never
	// used it. Without this, worker's own startup preflight refused
	// every copilot quickstart run outright (a Codex review on this
	// PR, round 3).
	//
	// workerAPI starts at the relay's default (openai-completions) and
	// switches to openai-responses only when the picked model needs
	// it (e.g. gpt-5.6-luna, which Copilot serves on /responses only)
	// -- see meter.CopilotModelUsableAnyAPI. Three distinct outcomes
	// when modelID isn't found among models, each worded differently
	// (a round-2 review): no listing ever ran (no token was ever
	// available) -- warn that the worker API couldn't be verified;
	// the listing ran but failed -- the specific message above already
	// covers it; the listing ran, succeeded, and modelID genuinely
	// isn't among the entitled models it returned -- refuse
	// (-non-interactive) or re-prompt (interactive) naming up to 5
	// usable ids, since this is stronger evidence than "unverifiable"
	// (the account was actually asked and modelID wasn't in it).
	modelID := b.opts.ModelID
	workerAPI := meter.RequestFormatOpenAICompletions
	for {
		if modelID == "" {
			// -non-interactive already refused above when ModelID is
			// empty, so reaching here interactive-only.
			modelID, err = quickstartPickCopilotModelID(b.p, b.w, models)
			if err != nil {
				return nil, nil, err
			}
			if modelID == "" {
				return nil, nil, fmt.Errorf("a model id is required for -route copilot")
			}
		}
		if strings.Contains(modelID, "/") {
			return nil, nil, fmt.Errorf("-model-id must not contain a slash, got %q", modelID)
		}

		found := false
		for _, m := range models {
			if m.ID != modelID {
				continue
			}
			found = true
			api, ok, why := meter.CopilotModelUsableAnyAPI(m)
			if !ok {
				return nil, nil, fmt.Errorf("model %q is not usable on this Copilot account: %s; pick a usable one%s (see `factoryd doctor -list-models`)", modelID, why, copilotSuggestionClause(copilotServableModelIDsAnyAPI(models, 5)))
			}
			workerAPI = api
		}
		if found {
			break
		}
		if listingAttempted && listingErr == nil && len(models) > 0 {
			msg := fmt.Errorf("model %q is not among this account's entitled Copilot models%s (see `factoryd doctor -list-models`)", modelID, copilotSuggestionClause(copilotServableModelIDsAnyAPI(models, 5)))
			if b.opts.NonInteractive {
				return nil, nil, msg
			}
			fmt.Fprintf(b.w, "%v\n", msg)
			modelID = ""
			continue
		}
		if !listingAttempted {
			fmt.Fprintf(b.w, "warning: could not verify %s's worker API (no Copilot model listing) -- if it serves only /responses (many GPT-5.x/6 models do), set models.<name>.api: openai-responses in the session config\n", modelID)
		}
		// listingAttempted && (listingErr != nil || len(models) == 0):
		// the listing's own failure message was already printed above,
		// or the account genuinely has zero entitled models -- nothing
		// more to check modelID against either way.
		break
	}
	quickstartWarnIfWeakModel(b.w, modelID)

	contextWindow := b.opts.ContextWindow
	if !b.opts.ContextWindowExplicit {
		// The listing carries the entitled model's own real
		// context window (Copilot's capabilities.limits.
		// max_context_window_tokens) -- use it instead of asking the
		// operator to guess a number, when the listing has one.
		if cw := quickstartCopilotContextWindowFor(models, modelID); cw > 0 {
			contextWindow = cw
			fmt.Fprintf(b.w, "Using context window %d from the Copilot model listing for %s.\n", cw, modelID)
		} else if b.opts.NonInteractive {
			return nil, nil, fmt.Errorf("-context-window is required under -non-interactive for -route copilot")
		} else {
			var cwText string
			cwText, err = b.p.ask(b.w, "Model context window, in tokens (e.g. 131072): ")
			if err != nil {
				return nil, nil, err
			}
			contextWindow, err = strconv.Atoi(strings.TrimSpace(cwText))
			if err != nil {
				return nil, nil, fmt.Errorf("context window must be an integer: %w", err)
			}
		}
	}
	if contextWindow <= 0 {
		return nil, nil, fmt.Errorf("-context-window must be a positive integer, got %d", contextWindow)
	}

	token := preresolvedToken
	if usedTokenFile == "" && token == "" {
		if b.opts.NonInteractive {
			return nil, nil, fmt.Errorf("-credential is required under -non-interactive for -route copilot (or set GITHUB_COPILOT_TOKEN, or authenticate pi so quickstart can discover the login itself)")
		}
		token, err = b.p.askSecret(b.w, "GitHub OAuth token (kept only in the daemon process's own environment, never written to config): ")
		if err != nil {
			return nil, nil, err
		}
		if token == "" {
			return nil, nil, fmt.Errorf("a GitHub OAuth token is required for -route copilot")
		}
	}
	if usedTokenFile == "" {
		b.credentialEnv = append(b.credentialEnv, "GITHUB_COPILOT_TOKEN="+token)
	}

	routeName := "copilot"
	modelKey := modelID
	copilotRoute := sessionconfig.Route{CredentialMode: meter.CredentialModeGitHubCopilot}
	if usedTokenFile != "" {
		copilotRoute.GitHubTokenFile = usedTokenFile
	}
	b.cfg.Routes = map[string]sessionconfig.Route{routeName: copilotRoute}
	copilotModel := sessionconfig.Model{
		ID:            modelID,
		Routes:        []string{routeName},
		ContextWindow: contextWindow,
	}
	if workerAPI == meter.RequestFormatOpenAIResponses {
		copilotModel.API = workerAPI
	}
	b.cfg.Models = map[string]sessionconfig.Model{modelKey: copilotModel}
	b.cfg.Roles = quickstartSingleModelRoles(modelKey)
	return nil, nil, nil
}

// chatGPTCodexRoute fills the config for the ChatGPT Codex subscription route.
func (b *quickstartConfigBuild) chatGPTCodexRoute() (*sessionconfig.Config, []string, error) {
	var err error
	// codex exec's own relay route speaks only POST <base>/responses
	// (meter.ChatGPTCodexResponsesPath's own doc comment) -- there is
	// no GET /models listing to discover a model id or context window
	// from, unlike -route openai/copilot, so this route falls back to
	// the one model/context-window pair this repository has actually
	// live-validated (chatGPTCodexDefaultModelID/
	// chatGPTCodexDefaultContextWindow's own doc comment).
	modelID := b.opts.ModelID
	if modelID == "" {
		if b.opts.NonInteractive {
			modelID = chatGPTCodexDefaultModelID
			fmt.Fprintf(b.w, "No -model-id given; using the default %s (chatgpt-codex has no model-listing endpoint -- codex exec speaks only POST /responses).\n", modelID)
		} else {
			fmt.Fprintln(b.w, "chatgpt-codex has no model-listing endpoint to discover from (codex exec speaks only POST /responses).")
			var ans string
			ans, err = b.p.ask(b.w, fmt.Sprintf("Model id [%s]: ", chatGPTCodexDefaultModelID))
			if err != nil {
				return nil, nil, err
			}
			modelID = chatGPTCodexDefaultModelID
			if ans != "" {
				modelID = ans
			}
		}
	}
	if strings.Contains(modelID, "/") {
		return nil, nil, fmt.Errorf("-model-id must not contain a slash, got %q", modelID)
	}
	quickstartWarnIfWeakModel(b.w, modelID)

	contextWindow := b.opts.ContextWindow
	if !b.opts.ContextWindowExplicit {
		if modelID == chatGPTCodexDefaultModelID {
			contextWindow = chatGPTCodexDefaultContextWindow
		} else if b.opts.NonInteractive {
			return nil, nil, fmt.Errorf("-context-window is required under -non-interactive for -route chatgpt-codex with a non-default -model-id %q", modelID)
		} else {
			var cwText string
			cwText, err = b.p.ask(b.w, fmt.Sprintf("Model context window, in tokens (e.g. %d): ", chatGPTCodexDefaultContextWindow))
			if err != nil {
				return nil, nil, err
			}
			contextWindow, err = strconv.Atoi(strings.TrimSpace(cwText))
			if err != nil {
				return nil, nil, fmt.Errorf("context window must be an integer: %w", err)
			}
		}
	}
	if contextWindow <= 0 {
		return nil, nil, fmt.Errorf("-context-window must be a positive integer, got %d", contextWindow)
	}
	routeName := "codex"
	modelKey := modelID
	codexRoute := sessionconfig.Route{CredentialMode: meter.CredentialModeChatGPTCodex}
	// No config or credentialEnv field carries a credential here: the
	// ChatGPT access token is read fresh from ~/.codex/auth.json (or
	// -relay-codex-auth-file/$CODEX_HOME) at relay-launch time by
	// resolveChatGPTCodexCredential -- see that function's own doc
	// comment for why this mode never caches or forwards a credential
	// through the daemon's own environment the way anthropic/copilot
	// do.
	//
	// relay_codex_auth_file IS persisted, though (a round-2
	// review found): quickstartDetectCodexLogin/resolveCodexAuthFilePath
	// resolve this same file honoring $CODEX_HOME, but a daemon
	// started later (e.g. `factoryd serve` from a launchd/systemd
	// unit, or any process with a different environment) may not have
	// CODEX_HOME set at all -- without pinning the exact path here, it
	// would silently fall back to ~/.codex/auth.json and could read a
	// different login than the one this quickstart run just detected
	// and confirmed. Only the path is written, mirroring the copilot
	// branch's own relay_github_token_file -- never a token value.
	// A round-3 review found this must be absolute'd before persisting -- a
	// relative $CODEX_HOME (filepath.Join keeps it relative) would
	// otherwise save a relative relay_codex_auth_file that resolves
	// against whatever directory a LATER command happens to run from.
	if codexAuthFile, statErr := resolveCodexAuthFilePath(""); statErr == nil {
		if absCodexAuthFile, absErr := filepath.Abs(codexAuthFile); absErr == nil {
			if _, err := os.Stat(absCodexAuthFile); err == nil {
				codexRoute.CodexAuthFile = absCodexAuthFile
			}
		}
	}
	b.cfg.Routes = map[string]sessionconfig.Route{routeName: codexRoute}
	model := sessionconfig.Model{
		ID:            modelID,
		Routes:        []string{routeName},
		ContextWindow: contextWindow,
	}
	b.cfg.Roles = quickstartSingleModelRoles(modelKey)
	if modelID == chatGPTCodexDefaultModelID {
		// The Luna profile: the default model declares its reasoning
		// levels (Pi clamps an undeclared xhigh/max silently), builds
		// at medium effort and plans and reviews at max. Only for the
		// default id, whose levels are known; any other -model-id
		// keeps Pi's defaults.
		model.Reasoning = true
		model.ThinkingLevelMap = map[string]string{"xhigh": "xhigh", "max": "max"}
		b.cfg.Roles.Planning.Thinking = "max"
		b.cfg.Roles.Execution.Thinking = "medium"
		b.cfg.Roles.Review.Thinking = "max"
	}
	b.cfg.Models = map[string]sessionconfig.Model{modelKey: model}
	return nil, nil, nil
}

// userLogin runs `gh api user --jq .login`, resolving the
// currently authenticated GitHub user -- a boundary method so tests can
// stub it without needing a real `gh` on PATH or a real authenticated
// session.
func (impl realForge) userLogin() (string, error) {
	out, err := exec.Command("gh", "api", "user", "--jq", ".login").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// quickstartDefaultPRTrustedAuthor resolves forge.userLogin,
// returning "" (skip silently, never an error) whenever gh isn't
// installed, isn't authenticated, or the call otherwise fails -- this is
// a convenience default (G17b), not a requirement, and quickstart must
// never abort onboarding over it.
func quickstartDefaultPRTrustedAuthor(dp *deps) string {
	login, err := dp.forge.userLogin()
	if err != nil {
		return ""
	}
	return login
}

// quickstartFetchModelIDs lists model ids from GET <host>/v1/models --
// host-side enumeration for picking an id only, never proof the route
// works (the real reachability test is the in-sandbox doctor check that
// runs later against the written config; see this plan's Open Question
// 2). A short, fixed timeout: this is a convenience lookup during an
// interactive prompt, not a step quickstart should ever hang on.
func quickstartFetchModelIDs(host string) ([]string, error) {
	return fetchOpenAIModelIDs(strings.TrimRight(host, "/") + "/v1/models")
}

// fetchOpenAIModelIDs is quickstartFetchModelIDs' own logic against an
// already-built URL rather than a bare host -- shared with `factoryd
// doctor -list-models`, whose openai/static route needs the same GET
// <upstream><base-path>/models listing but at a base path that isn't
// always "/v1" (see -relay-worker-base-path). A short, fixed timeout in
// both callers: this is a convenience lookup, never a step either command
// should hang on.
func fetchOpenAIModelIDs(url string) ([]string, error) {
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	// Capped at 1 MiB, matching listGitHubCopilotModels' own bound
	// (a round-2 review): a misbehaving/enormous endpoint
	// must not be read into memory unbounded.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read model list: %w", err)
	}
	return parseOpenAIModelIDs(body)
}

// parseOpenAIModelIDs is fetchOpenAIModelIDs' own response-shape parsing,
// factored out so doctor's own credentialed/CA-aware model-listing fetch
// (doctorFetchModelIDs, doctor_list_models.go) shares it instead of
// duplicating the JSON shape.
func parseOpenAIModelIDs(body []byte) ([]string, error) {
	var parsed struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("parse model list: %w", err)
	}
	ids := make([]string, 0, len(parsed.Data))
	for _, m := range parsed.Data {
		if m.ID != "" {
			ids = append(ids, m.ID)
		}
	}
	sort.Strings(ids)
	return ids, nil
}

// quickstartPickModelID implements Open Question 2's resolved model-id
// prompt: always offer free text; auto-list only when the returned list
// is short (<= 20 ids, a numbered pick list plus "type an id myself"); a
// longer list asks for a substring filter first. Any fetch error goes
// straight to free text with the error shown, rather than blocking the
// prompt on a host-side lookup that isn't the real reachability test
// anyway.
func quickstartPickModelID(p *quickstartPrompter, w io.Writer, host string) (string, error) {
	ids, err := quickstartFetchModelIDs(host)
	if err != nil {
		fmt.Fprintf(w, "Could not list models from %s/v1/models (%v) -- type the model id directly.\n", host, err)
		return p.ask(w, "Model id: ")
	}
	if len(ids) == 0 {
		fmt.Fprintf(w, "%s/v1/models listed no models -- type the model id directly.\n", host)
		return p.ask(w, "Model id: ")
	}

	candidates := ids
	if len(candidates) > 20 {
		filter, err := p.ask(w, fmt.Sprintf("%d models found -- type a substring to filter (blank to skip filtering): ", len(candidates)))
		if err != nil {
			return "", err
		}
		if filter != "" {
			var filtered []string
			for _, id := range candidates {
				if strings.Contains(id, filter) {
					filtered = append(filtered, id)
				}
			}
			candidates = filtered
		}
	}
	if len(candidates) == 0 {
		return p.ask(w, "No matches -- type the model id directly: ")
	}
	if len(candidates) > 20 {
		fmt.Fprintf(w, "%d matches -- showing the first 20; refine your filter or type the id directly.\n", len(candidates))
		candidates = candidates[:20]
	}

	options := append(append([]string{}, candidates...), "(type an id myself)")
	choice, err := p.choose(w, "Pick a model id", options)
	if err != nil {
		return "", err
	}
	if choice == len(options)-1 {
		return p.ask(w, "Model id: ")
	}
	return candidates[choice], nil
}

// quickstartPickCopilotModelID offers the entitled models list (Copilot's
// own preview/model-picker-default/premium-multiplier metadata shown as
// tags next to each id -- recommend from real listing metadata rather
// than a hardcoded "good models" guess) plus a free-text fallback, mirroring
// quickstartPickModelID's own shape for -route openai. An empty models
// list (listing failed, or genuinely empty) falls straight to free text.
func quickstartPickCopilotModelID(p *quickstartPrompter, w io.Writer, listed []meter.CopilotModel) (string, error) {
	// Offer only models this relay can run under whichever worker API
	// they need (meter.CopilotModelUsableAnyAPI) -- picking a
	// listed-but-unusable one used to exit quickstart right after the
	// choice. A /responses-only model (e.g. gpt-5.6-luna) is offered here
	// too: the caller picks the matching relay_worker_api once a model id
	// is chosen. The typed-id escape hatch below stays for anything the
	// listing judges wrongly.
	var models []meter.CopilotModel
	for _, m := range listed {
		if _, ok, _ := meter.CopilotModelUsableAnyAPI(m); ok {
			models = append(models, m)
		}
	}
	if hidden := len(listed) - len(models); hidden > 0 {
		fmt.Fprintf(w, "%d listed model(s) hidden: not usable through this relay (see `factoryd doctor -list-models` for why).\n", hidden)
	}
	if len(models) == 0 {
		return p.ask(w, "Entitled GitHub Copilot model id: ")
	}
	ids := make([]string, len(models))
	labels := make([]string, len(models), len(models)+1)
	for i, m := range models {
		ids[i] = m.ID
		var tags []string
		if m.ModelPickerEnabled {
			tags = append(tags, "picker default")
		}
		if m.Preview {
			tags = append(tags, "preview")
		}
		if m.IsPremium {
			tags = append(tags, fmt.Sprintf("premium x%g", m.Multiplier))
		}
		label := m.ID
		if len(tags) > 0 {
			label = fmt.Sprintf("%s (%s)", m.ID, strings.Join(tags, ", "))
		}
		labels[i] = label
	}
	options := append(labels, "(type an id myself)")
	choice, err := p.choose(w, "Pick an entitled Copilot model", options)
	if err != nil {
		return "", err
	}
	if choice == len(options)-1 {
		return p.ask(w, "Model id: ")
	}
	return ids[choice], nil
}

// quickstartCopilotContextWindowFor returns the listed context window for
// modelID (0 if models is empty, modelID isn't in it, or the listing
// didn't report one for that entry -- see CopilotModel's own doc comment
// on why a zero value means "not reported", not "no limit").
func quickstartCopilotContextWindowFor(models []meter.CopilotModel, modelID string) int {
	for _, m := range models {
		if m.ID == modelID {
			return m.ContextWindow
		}
	}
	return 0
}

// ---- step 3: repo readiness (verify command, onboarding scaffold) ----

// quickstartEnsureRepoReady resolves the verify command and preflight
// profile the submission will use, and, only when explicitly authorized
// (-scaffold, or an interactive confirmation), scaffolds onboarding docs
// via `factoryd onboard` -- never silently, and only after checking
// onboardGuardFileStatus so an already-scaffolded repo is skipped rather
// than hitting onboard's own refuse-to-overwrite error (plan's "New app
// vs. existing repo" row).
func quickstartEnsureRepoReady(dp *deps, opts *quickstartOptions, p *quickstartPrompter, w io.Writer, repoRoot string) (verifyCommand string, verifyExplicit bool, preflightProfile string, preflightExplicit bool, err error) {
	verifyCommand = opts.VerifyCommand
	verifyExplicit = opts.VerifyCommandExplicit
	source := "flag"
	// Precedence, matching applyProjectConfigDefaults' own "explicit flag
	// wins, otherwise .factory.yml": only fall through to the committed
	// config, then to Makefile/go.mod/package.json detection, when the
	// operator did not actually pass -verify-command. verifyExplicit
	// itself must stay false here (it tracks whether the *flag* was
	// explicit, not whether a value was found) so that whichever value we
	// display is exactly what submitRequest's own applyProjectConfigDefaults
	// will independently resolve to -- if it disagreed, the printed
	// "Verify command" line would lie about what actually got submitted.
	if !verifyExplicit {
		if cfg, found, cfgErr := projectconfig.Load(repoRoot); cfgErr == nil && found && cfg.VerifyCommand != "" {
			verifyCommand, source = cfg.VerifyCommand, projectconfig.FileName
		} else {
			verifyCommand, source = detectVerifyCommand(repoRoot)
		}
	}
	switch {
	case verifyCommand != "":
		fmt.Fprintf(w, "Verify command (%s): %s\n", source, verifyCommand)
	case opts.NonInteractive:
		return "", false, "", false, fmt.Errorf("no verify command could be detected in %s; pass -verify-command under -non-interactive", repoRoot)
	default:
		verifyCommand, err = p.ask(w, "No verify command detected -- what command verifies this repo (e.g. `make test`)? ")
		if err != nil {
			return "", false, "", false, err
		}
		if verifyCommand == "" {
			return "", false, "", false, fmt.Errorf("a verify command is required")
		}
		// Typed interactively because nothing else resolved one at all --
		// there is no committed/detected value it could ever lose to, so
		// treat it the same as an explicit flag from here on.
		verifyExplicit = true
	}

	preflightProfile = opts.PreflightProfile
	preflightExplicit = opts.PreflightProfileExplicit

	if opts.Scaffold {
		if err := quickstartScaffoldOnboarding(dp, w, repoRoot); err != nil {
			return "", false, "", false, err
		}
	} else {
		existingArtifacts, err := onboardGuardFileStatus(repoRoot, false)
		if err != nil {
			return "", false, "", false, fmt.Errorf("check existing onboarding scaffold: %w", err)
		}
		if len(existingArtifacts) == 0 && quickstartRepoHasCommits(repoRoot) {
			// Brownfield auto-detection (plan 2.2): a real, pre-existing
			// repo with none of the three onboarding docs is exactly the
			// case -preflight-profile already defaults to "brownfield"
			// for, so there is no decision left to ask about here -- the
			// old y/N "scaffold onboarding docs now?" prompt below was
			// the last question standing between an operator and a repo
			// this confident a detection already covers. Print what was
			// decided (and the override) instead of asking, and persist
			// the one piece of it worth keeping across runs: a
			// .factory.yml an operator can commit so a later
			// quickstart/worker invocation against this repo doesn't
			// need -verify-command or -preflight-profile at all. Pass
			// -scaffold to also get spec/spec.md, spec/contract.md, and
			// ARCHITECTURE.md themselves.
			fmt.Fprintf(w, "Detected an existing repo (has commits) with no onboarding docs -- treating it as brownfield (preflight profile: %s) without asking; pass -scaffold to generate spec/spec.md, spec/contract.md, and ARCHITECTURE.md instead.\n", preflightProfile)
			factoryYMLPath := filepath.Join(repoRoot, projectconfig.FileName)
			if _, statErr := os.Lstat(factoryYMLPath); os.IsNotExist(statErr) {
				if err := writeScaffoldFiles(repoRoot, map[string]string{factoryYMLPath: factoryYMLContent(verifyCommand, preflightProfile)}); err != nil {
					return "", false, "", false, err
				}
				fmt.Fprintln(w, factoryYMLQuickstartNote())
				// The reminder just printed already told the operator this
				// file isn't committed yet -- suppress projectconfig's own
				// identical warning the very next Load (submitRequest,
				// moments later) would otherwise print twice.
				projectconfig.NoteReminderShown(factoryYMLPath)
			} else if statErr != nil {
				return "", false, "", false, fmt.Errorf("check %s: %w", factoryYMLPath, statErr)
			}
		} else if !opts.NonInteractive {
			// Found via Codex review of PR #173: this used to check only
			// os.Stat(ARCHITECTURE.md), so a repo with a hand-written
			// ARCHITECTURE.md but no spec/ yet (existingArtifacts non-empty,
			// so the brownfield auto-detect branch above is skipped) was
			// never offered scaffolding for the two files it's actually
			// missing -- exactly the "has ARCHITECTURE.md, missing spec/"
			// case this PR's own doOnboard change (scaffold only what's
			// missing) was built to handle. Use the same existingArtifacts
			// count computed above instead of re-deriving it from a single
			// file.
			if len(existingArtifacts) < 3 {
				ok, err := p.confirm(w, "Onboarding docs missing -- scaffold onboarding docs (spec/spec.md, spec/contract.md, ARCHITECTURE.md; only what's missing is written) now? [y/N] ", false)
				if err != nil {
					return "", false, "", false, err
				}
				if ok {
					if err := quickstartScaffoldOnboarding(dp, w, repoRoot); err != nil {
						return "", false, "", false, err
					}
				}
			}
		}
	}

	return verifyCommand, verifyExplicit, preflightProfile, preflightExplicit, nil
}

// quickstartRepoHasCommits reports whether repoRoot's git repository has at
// least one commit -- the other half of quickstart's own brownfield
// auto-detection alongside onboardGuardFileStatus (see
// quickstartEnsureRepoReady): a freshly `git init`'d repo with no history
// yet is genuinely greenfield even if it happens to have no onboarding
// docs either, and still gets the interactive scaffold prompt rather than
// a silent brownfield default. Mirrors projectconfig's own
// readCommitted check (internal/projectconfig/projectconfig.go). A
// git-command failure (not a git repo, git not installed) is treated as
// "no commits" -- resolveQuickstartRepoRoot has already confirmed
// repoRoot is inside a git repository by the time this runs, so in
// practice this only ever returns false here for the no-HEAD-yet case.
func quickstartRepoHasCommits(repoRoot string) bool {
	return exec.Command("git", "-C", repoRoot, "rev-parse", "--verify", "-q", "HEAD").Run() == nil
}

// quickstartScaffoldOnboarding runs onboard's own doOnboard directly
// against repoRoot. It used to pre-check onboardGuardFileStatus and skip
// entirely whenever any of the three guard files already existed, since
// onboard itself used to hard-refuse to scaffold anything at all in that
// case -- which meant a typical existing repo (hand-written
// ARCHITECTURE.md, no spec/ yet) got nothing scaffolded via quickstart
// either. Now that doOnboard itself only scaffolds whichever of the three
// are actually missing and leaves the rest untouched, that pre-check is
// redundant: it's simplest, and strictly more useful, to just let
// doOnboard decide. SkipDoctor is set: quickstart's own pull-focused
// doctor step has already run by the time this is reached, and
// re-running onboard's mount-visibility preflight here would just repeat
// work with no new information.
func quickstartScaffoldOnboarding(dp *deps, w io.Writer, repoRoot string) error {
	result, err := doOnboard(dp, onboardOptions{
		Project:    filepath.Base(repoRoot),
		Root:       repoRoot,
		SkipDoctor: true,
	})
	if err != nil {
		return fmt.Errorf("scaffold onboarding docs: %w", err)
	}
	if len(result.Written) == 0 {
		fmt.Fprintln(w, "Onboarding scaffold already present -- nothing to add.")
	}
	return nil
}

// ---- step 4: daemon ----------------------------------------------------

// ---- step 5: submit + watch -------------------------------------------

// quickstartInFlightStates are every request.State quickstartFindInFlight
// Request treats as "already being worked" -- every state except the
// four terminal ones (request.go's own unexported nonTerminalStates is
// the same set, just not exported for this package to reuse directly).
var quickstartInFlightStates = map[request.State]bool{
	request.StateSubmitted:    true,
	request.StateSpecDrafting: true,
	request.StateSpecReview:   true,
	// The oracle stage is opt-in (submit -draft-oracles) and quickstart never
	// opts in, but a request submitted that way against the same repo is
	// still in flight and must not be duplicated.
	request.StateOracleDrafting: true,
	request.StateOracleReview:   true,
	request.StatePlanning:       true,
	request.StatePlanReview:     true,
	request.StateBuilding:       true,
	request.StatePRReview:       true,
	request.StateResumeReview:   true,
}

// quickstartFindInFlightRequest returns the most recently submitted
// non-terminal request already recorded against repoRoot in dataDir, or
// nil if none -- release.RejectProjectCollision only catches two
// DIFFERENT repository roots claiming the same project name, not a
// second quickstart run resubmitting the SAME repo while an earlier
// request is still in flight, so quickstart's own plan ("Idempotent by
// design": "a request already in flight for this repo ... show it
// instead of submitting a duplicate") needs this separate check (a Codex
// review on this PR, round 3).
func quickstartFindInFlightRequest(dataDir, repoRoot string) (*request.Request, error) {
	requests, err := request.List(dataDir)
	if err != nil {
		return nil, fmt.Errorf("list existing requests: %w", err)
	}
	var found *request.Request
	for _, r := range requests {
		if r.Workspace == repoRoot && quickstartInFlightStates[r.State] {
			found = r // request.List sorts oldest-first; keep the newest match
		}
	}
	return found, nil
}

// fetchIssue is quickstartSubmitAndWatch's own default -issue
// fetcher (a real `gh issue view`) -- a boundary method, mirroring
// forge.gitToplevel, so a test can stub it without invoking a real gh
// binary.
func (impl realForge) fetchIssue(ctx context.Context, issueURL string) (title, body string, number int, err error) {
	return (ghIssueFetcher{}).fetch(ctx, issueURL)
}

// quickstartSubmitAndWatch submits a request against repoRoot via the same
// submitRequest the plain `factoryd submit` CLI uses, then watches the
// resulting request until it reaches a state quickstart knows what to say
// about. Exactly one of goal, issue, or requestFile carries the request
// text (runQuickstart/quickstartValidateRequestSource already enforce
// that); trailingText is built here rather than always passed as
// []string{goal} because resolveSubmitRequestText rejects trailing text
// alongside -issue/-request-file. A project-id collision
// (release.RejectProjectCollision) or any other submission error is
// surfaced verbatim and never retried under a different id (plan's
// "Idempotency" row). Checks quickstartFindInFlightRequest first so a
// rerun against a repo that already has a request in flight reports that
// request instead of submitting a second one.
func quickstartSubmitAndWatch(dp *deps, w io.Writer, repoRoot, dataDir, goal, issue, requestFile, verifyCommand string, verifyCommandExplicit bool, preflightProfile string, preflightProfileExplicit bool, pollInterval, pollTimeout time.Duration, consoleToken string, autoOpen bool) error {
	if inFlight, err := quickstartFindInFlightRequest(dataDir, repoRoot); err != nil {
		return err
	} else if inFlight != nil {
		fmt.Fprintf(w, "Request %s against this repo is already in flight (%s) -- not submitting a duplicate.\n", inFlight.ID, inFlight.State)
		quickstartPrintAndOpenConsoleLink(dp, w, dataDir, inFlight.ID, consoleToken, autoOpen)
		return quickstartWatchRequest(dp, dataDir, inFlight.ID, w, pollInterval, pollTimeout)
	}

	var trailingText []string
	if issue == "" && requestFile == "" {
		trailingText = []string{goal}
	}
	result, err := submitRequest(dp, context.Background(), submitParams{
		workspaceArg:             repoRoot,
		requestFile:              requestFile,
		issue:                    issue,
		trailingText:             trailingText,
		verifyCommand:            verifyCommand,
		verifyCommandExplicit:    verifyCommandExplicit,
		preflightProfile:         preflightProfile,
		preflightProfileExplicit: preflightProfileExplicit,
		dataDir:                  dataDir,
		fetchIssue:               dp.forge.fetchIssue,
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "Submitted request %s (project %s).\n", result.id, result.project)
	quickstartPrintAndOpenConsoleLink(dp, w, dataDir, result.id, consoleToken, autoOpen)
	fmt.Fprintf(w, "Follow along: factoryd watch%s %s\n", quickstartDataDirArg(dataDir), result.id)
	return quickstartWatchRequest(dp, dataDir, result.id, w, pollInterval, pollTimeout)
}

// quickstartPrintAndOpenConsoleLink prints (and, when autoOpen, opens in
// the default browser) the console deep link to request id -- carrying
// consoleToken's #t=<token> fragment when quickstartEnsureServe resolved
// one, so a fresh install's very first console visit doesn't 403 on New run/
// release/stats screens (F: serve-start-token). Printing nothing at all
// when resolveConsoleBaseURL itself has no console to link to (-no-serve,
// or no console embedded in this binary) matches quickstartSubmitAndWatch's
// prior behavior exactly. A browser-open failure (no `open`/
// `xdg-open` on PATH, a headless box) is reported but never fails the
// run -- the link itself was already printed either way.
func quickstartPrintAndOpenConsoleLink(dp *deps, w io.Writer, dataDir, requestID, consoleToken string, autoOpen bool) {
	base := resolveConsoleBaseURL("", dataDir)
	url := consoleRequestURL(base, requestID)
	if url == "" {
		return
	}
	// consoleBaseIsOwnLoopbackServe (an adversarial review,
	// 2026-09-24): consoleToken is a permanent credential for this
	// machine's own serve -- only ever attach it when base actually IS
	// that same loopback serve, never a remote FACTORYD_CONSOLE_URL
	// override.
	// ...and only when a serve this data dir can prove it owns holds that
	// address: its record alone can outlive the serve that wrote it, and
	// the default address can be another data dir's serve.
	ownServe := consolelink.ServeAddress(dataDir)
	if ownServe == "" {
		ownServe = consolelink.DefaultServeAddr
	}
	_, verified := dp.ServeVerifiedOurs(dataDir, ownServe)
	if consoleToken != "" && verified && consoleBaseIsOwnLoopbackServe(base, ownServe) {
		url += "#t=" + consoleToken
	}
	fmt.Fprintln(w, "View:", url)
	if autoOpen {
		if err := openInBrowser(dp, url); err != nil {
			fmt.Fprintf(w, "could not open the console link in a browser (%v) -- open it manually.\n", err)
		}
	}
}

// quickstartDataDirArg is the " -data-dir <dataDir>" fragment every
// command quickstart itself prints (approve/reject/retry/watch/status)
// splices in before the request/run id: quickstart always resolves
// an absolute data dir (quickstartEnsureConfig), which need not be any of
// those commands' own "data" default, so the bare command it used to
// print (e.g. "factoryd approve <id>") could fail with a "no such file"
// against the wrong directory. Always including the flag -- rather than
// only when dataDir differs from the literal default -- means the printed
// command is correct regardless of what a later `factoryd approve`'s own
// default happens to resolve to, without this function needing to
// duplicate that resolution to decide when to omit it.
func quickstartDataDirArg(dataDir string) string {
	return " -data-dir " + quickstartShellQuote(dataDir)
}

// quickstartShellQuote formats s for splicing into a literal shell command
// line quickstart prints for the operator to copy and paste -- single-
// quoted (POSIX-safe against every shell metacharacter, including a
// literal single quote via the standard '\” escape) when it contains
// anything a shell would treat specially, and left bare otherwise so the
// common case (an ordinary path with no spaces) stays readable. An
// adversarial review of Phase A found an unquoted data dir containing a
// space (e.g. macOS's own "~/Library/Application Support/factoryd/data")
// or a shell metacharacter previously pasted straight into the printed
// command, which a real shell would then split or reinterpret.
func quickstartShellQuote(s string) string {
	if quickstartShellSafe(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// quickstartShellSafe reports whether s can appear unquoted on a shell
// command line with no risk of being split, glob-expanded, or otherwise
// reinterpreted -- conservative by construction (an allowlist of the
// characters an ordinary absolute path is built from), not an attempt to
// enumerate every character POSIX shells actually tolerate unquoted.
func quickstartShellSafe(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '/' || r == '.' || r == '_' || r == '-':
		default:
			return false
		}
	}
	return true
}

// quickstartWatchRequest polls the request record until it leaves
// StateSubmitted/StateSpecDrafting (plan's "Submit + watch" row), then
// hands off to quickstartReportRequestState to decide what to print and
// whether that's a success or failure exit. Never fails merely from
// hitting pollTimeout while still in-flight -- that just means the work
// is still running, not that anything went wrong; `factoryd status`
// remains the way to keep watching.
func quickstartWatchRequest(dp *deps, dataDir, id string, w io.Writer, pollInterval, pollTimeout time.Duration) error {
	deadline := time.Now().Add(pollTimeout)
	// On a terminal, one working line names what the request waits on;
	// elsewhere the wait stays silent, as before.
	sp := newTTYSpinner(w)
	if sp != nil {
		defer sp.Stop("")
	}
	for {
		r, err := request.Load(dataDir, id)
		if err != nil {
			return fmt.Errorf("load request %s: %w", id, err)
		}
		if r.State != request.StateSubmitted && r.State != request.StateSpecDrafting {
			if sp != nil {
				sp.Stop("")
			}
			return quickstartReportRequestState(dataDir, r, w)
		}
		if time.Now().After(deadline) {
			if sp != nil {
				sp.Stop("")
			}
			fmt.Fprintf(w, "Request %s is still %s after %s -- it's still working; keep watching with: factoryd watch%s %s (or factoryd status%s for a one-shot snapshot).\n", id, r.State, pollTimeout, quickstartDataDirArg(dataDir), id, quickstartDataDirArg(dataDir))
			return nil
		}
		if sp != nil {
			text, since := requestWaitText(dataDir, r)
			sp.Start(text)
			sp.SetStart(since)
		}
		dp.host.sleep(pollInterval)
	}
}

// quickstartReportRequestState prints what an operator needs to know once
// a request leaves the submitted/spec_drafting states, and reports
// whether that's an error exit. Deliberately never calls
// internal/request.Approve itself, -non-interactive included (plan's
// "Does not bypass the request pipeline's review gates"): spec_review and
// plan_review both print the exact `factoryd approve` command instead and
// exit cleanly, since approving is a human decision quickstart never
// makes on the operator's behalf.
//
// Takes only a *request.Request and an io.Writer -- no queue, no Docker,
// no daemon -- so this is fully testable against a hand-built Request
// value.
func quickstartReportRequestState(dataDir string, r *request.Request, w io.Writer) error {
	arg := quickstartDataDirArg(dataDir)
	switch r.State {
	case request.StateSpecReview:
		fmt.Fprintf(w, "Spec drafted for request %s: %s\nReview it, then run: factoryd approve%s %s\n", r.ID, request.SpecPath(dataDir, r.ID), arg, r.ID)
		return nil
	case request.StateOracleReview:
		fmt.Fprintf(w, "Oracle stage for request %s is waiting on you: %s/%s\nReview or hand-write it (absent or empty skips), then run: factoryd approve%s %s\n", r.ID, request.Dir(dataDir, r.ID), request.RequestOracleDirName, arg, r.ID)
		if notice := request.OracleReviewNotice(dataDir, r); notice != "" {
			fmt.Fprintf(w, "ACTION NEEDED before approve: %s\n", notice)
		}
		return nil
	case request.StatePlanReview:
		fmt.Fprintf(w, "Plan drafted for request %s: %s\nReview it, then run: factoryd approve%s %s\n", r.ID, filepath.Join(request.Dir(dataDir, r.ID), "tickets"), arg, r.ID)
		return nil
	case request.StateResumeReview:
		fmt.Fprintf(w, "Request %s is waiting on you: %s\n", r.ID, strings.Join(strings.Fields(r.NextAction()), " "))
		return nil
	case request.StateHalted:
		if r.AwaitingPullRequest() {
			fmt.Fprintf(w, "Request %s: %s\n", r.ID, r.AwaitingPullRequestLabel())
			return nil
		}
		fmt.Fprintf(w, "Request %s halted: %s\nOnce fixed, run: factoryd retry%s %s\n", r.ID, r.Error, arg, r.ID)
		return fmt.Errorf("request %s halted: %s", r.ID, r.Error)
	case request.StateQuarantined:
		fmt.Fprintf(w, "Request %s quarantined: %s\nOnce fixed, run: factoryd retry%s %s\n", r.ID, r.Error, arg, r.ID)
		return fmt.Errorf("request %s quarantined: %s", r.ID, r.Error)
	case request.StateCancelled:
		fmt.Fprintf(w, "Request %s was cancelled.\n", r.ID)
		return fmt.Errorf("request %s was cancelled", r.ID)
	case request.StateDone:
		fmt.Fprintf(w, "Request %s is done -- check its pull request with: factoryd status%s\n", r.ID, arg)
		return nil
	default:
		fmt.Fprintf(w, "Request %s is now %s -- keep watching with: factoryd watch%s %s (or factoryd status%s for a one-shot snapshot)\n", r.ID, r.State, arg, r.ID, arg)
		return nil
	}
}

// ---- hand-rolled interactive prompting ---------------------------------

// quickstartPrompter is the plan's own resolved choice for prompting
// (Open Question 3: "hand-roll, on a bufio.Reader" -- no TUI/prompt
// dependency this codebase doesn't already have). Every prompt in this
// flow is pick-one/free-text/yes-no; nothing here needs more than that.
type quickstartPrompter struct {
	in *bufio.Reader
	// rawIn is the same reader in, unbuffered -- kept only so askSecret can
	// type-assert it to *os.File and check term.IsTerminal before reading a
	// credential through it directly, bypassing in's own buffer.
	rawIn io.Reader
}

func newQuickstartPrompter(r io.Reader) *quickstartPrompter {
	return &quickstartPrompter{in: bufio.NewReader(r), rawIn: r}
}

// ask prints prompt and returns one trimmed line of input. Only a caller
// that has already checked NonInteractive should ever call this --
// quickstartPrompter itself carries no such flag, since every call site
// above already branches on opts.NonInteractive before reaching a prompt.
func (p *quickstartPrompter) ask(w io.Writer, prompt string) (string, error) {
	fmt.Fprint(w, prompt)
	line, err := p.in.ReadString('\n')
	if err != nil && line == "" {
		return "", fmt.Errorf("read input: %w", err)
	}
	return strings.TrimSpace(line), nil
}

// askSecret behaves like ask, but for a credential: a value that must
// never be echoed to the screen or land in terminal scrollback as it is
// typed (AGENTS.md L43-48; a Codex review on this PR flagged the earlier
// plain p.ask call for exactly that). When rawIn is a real terminal, it
// reads directly off that terminal's fd with echo disabled
// (term.ReadPassword), bypassing p.in's own buffer entirely -- safe here
// because every askSecret call site is the last prompt on its own
// interactive path (nothing reads from p.in again afterwards in the same
// run), so there is no risk of losing bytes p.in had already buffered
// ahead of a later ReadString. Falls back to the plain, echoed ask when
// rawIn isn't a terminal at all (piped/redirected stdin, or a test
// driving a strings.Reader): there is no terminal to suppress echo on,
// and a -non-interactive caller never reaches this path anyway, since it
// requires -credential as a flag instead of a prompt.
func (p *quickstartPrompter) askSecret(w io.Writer, prompt string) (string, error) {
	f, ok := p.rawIn.(*os.File)
	if !ok || !term.IsTerminal(int(f.Fd())) {
		return p.ask(w, prompt)
	}
	fmt.Fprint(w, prompt)
	b, err := term.ReadPassword(int(f.Fd()))
	fmt.Fprintln(w)
	if err != nil {
		return "", fmt.Errorf("read secret: %w", err)
	}
	return strings.TrimSpace(string(b)), nil
}

// confirm asks a yes/no question; an empty answer takes defaultYes,
// "y"/"yes"/"n"/"no" (any case) answer accordingly, and anything else is
// an error rather than a silent guess.
func (p *quickstartPrompter) confirm(w io.Writer, prompt string, defaultYes bool) (bool, error) {
	answer, err := p.ask(w, prompt)
	if err != nil {
		return false, err
	}
	switch strings.ToLower(answer) {
	case "":
		return defaultYes, nil
	case "y", "yes":
		return true, nil
	case "n", "no":
		return false, nil
	default:
		return false, fmt.Errorf("please answer y or n, got %q", answer)
	}
}

// choose prints title followed by a numbered list of options and returns
// the chosen index (0-based). Any non-numeric or out-of-range answer is
// an error rather than a silent default -- the same "refuse rather than
// guess" posture this whole command follows at every other checkpoint.
func (p *quickstartPrompter) choose(w io.Writer, title string, options []string) (int, error) {
	fmt.Fprintln(w, title)
	for i, o := range options {
		fmt.Fprintf(w, "  %d) %s\n", i+1, o)
	}
	answer, err := p.ask(w, "> ")
	if err != nil {
		return 0, err
	}
	n, convErr := strconv.Atoi(answer)
	if convErr != nil || n < 1 || n > len(options) {
		return 0, fmt.Errorf("enter a number between 1 and %d, got %q", len(options), answer)
	}
	return n - 1, nil
}
