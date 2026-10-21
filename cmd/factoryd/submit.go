package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"buildgate/internal/consolelink"
	"buildgate/internal/consoleweb"
	"buildgate/internal/hostcontrol"
	"buildgate/internal/request"
	"buildgate/internal/requestdriver"
	"buildgate/internal/requestsubmit"
	"buildgate/internal/sessionconfig"
)

// roleChoiceFlag is a flag.Value that accumulates `-<name> role=value`
// occurrences into a role -> value map -- `factoryd submit -model
// execution=sonnet -model planning=opus`, `-harness execution=pifork`. A
// repeated role (the same key passed twice) overwrites rather than erroring:
// the last one wins, the same behavior every other flag.FlagSet flag already
// has. Validated against the session's own roles.<role>.allowed /
// allowed_harnesses by sessionconfig.ValidateRequestModels /
// ValidateRequestHarnesses, not here -- this type only parses the
// "role=value" shape.
type roleChoiceFlag struct {
	name    string // the flag's own name, for messages: "model" or "harness"
	example string // a valid value of the flag, for messages
	values  map[string]string
}

func (f *roleChoiceFlag) String() string {
	if f == nil {
		return ""
	}
	pairs := make([]string, 0, len(f.values))
	for role, value := range f.values {
		pairs = append(pairs, role+"="+value)
	}
	sort.Strings(pairs)
	return strings.Join(pairs, ",")
}

func (f *roleChoiceFlag) Set(s string) error {
	role, value, ok := strings.Cut(s, "=")
	role = strings.TrimSpace(role)
	value = strings.TrimSpace(value)
	if !ok || role == "" || value == "" {
		return fmt.Errorf(`-%s %q: want "role=%s" (e.g. -%s execution=%s)`, f.name, s, f.name, f.name, f.example)
	}
	if f.values == nil {
		f.values = map[string]string{}
	}
	f.values[role] = value
	return nil
}

// submitParams holds the already-resolved inputs submitRequest needs to
// record a new request -- everything submitMain used to compute from
// flags before doing the submission inline. workspaceArg is kept separate
// from the resolved absolute workspace path purely so error messages can
// keep quoting exactly what the caller passed (matching `factoryd
// submit`'s historical wording); everything else is passed already
// resolved to its effective value, not as a flag.FlagSet, so a caller like
// the eventual `factoryd quickstart` never has to fake one up.
type submitParams struct {
	workspaceArg string
	requestFile  string
	// specFile is `factoryd submit -spec-file`: a finished spec handed over
	// in place of the drafted one; see requestsubmit.Params.ImportedSpec.
	specFile string
	// planDir is `factoryd submit -plan-dir`: the tickets of a finished
	// plan for specFile; see requestsubmit.Params.ImportedTickets.
	planDir       string
	issue         string
	trailingText  []string
	verifyCommand string
	// fullSuiteCommand is stored on the request as-is (empty means none).
	fullSuiteCommand string
	// noCommitOracles is `factoryd submit -no-commit-oracles`; see
	// request.Request.NoCommitOracles.
	noCommitOracles bool
	// draftOracles is `factoryd submit -draft-oracles`: stored on the request
	// as-is; see request.Request.DraftOracles.
	draftOracles bool
	// bug is `factoryd submit -bug`; see request.Request.Bug.
	bug bool
	// verifyCommandExplicit/preflightProfileExplicit record whether the
	// caller is treating verifyCommand/preflightProfile as an explicit
	// choice (e.g. a flag the operator actually typed) rather than an
	// unset default -- requestsubmit.ApplyProjectConfigDefaults's
	// "explicit flag wins, otherwise .factory.yml" precedence needs that
	// distinction even though this struct carries no flag.FlagSet of its
	// own.
	verifyCommandExplicit    bool
	preflightProfile         string
	preflightProfileExplicit bool
	dataDir                  string
	// harnesses is `factoryd submit -harness role=name` (repeatable): a
	// per-request coding-agent harness pick for "planning" and/or
	// "execution", checked by requestsubmit.Submit against settings' own
	// roles.<role>.allowed_harnesses via sessionconfig.ValidateRequestHarnesses.
	// Empty (the common case) records nothing on the request.
	harnesses map[string]string
	// sessionTokenCeiling/sessionCostCeilingMicroUSD are the session
	// config's own effective relay ceilings (settings.
	// EffectiveRelayCeilings, resolved once in submitMain), threaded
	// through to requestsubmit.Params.SessionTokenCeiling/
	// SessionCostCeilingMicroUSD so a workspace's .factory.yml is
	// checked against the ceiling the eventual run would actually use,
	// not requestsubmit's own hardcoded legacy defaults. Zero means "no
	// session config resolved" (a test building submitParams directly),
	// which requestsubmit.Submit treats the same as before this field
	// existed.
	sessionTokenCeiling        int64
	sessionCostCeilingMicroUSD int64
	// models is `factoryd submit -model role=model` (repeatable): a
	// per-request model pick for "planning" and/or "execution", checked
	// by requestsubmit.Submit against settings' own roles.<role>.allowed
	// via sessionconfig.ValidateRequestModels. Empty (the common case)
	// records nothing on the request.
	models map[string]string
	// settings is the caller's own already-resolved session config,
	// needed only to validate models above -- see settings' own doc
	// comment on requestsubmit.Params.Settings.
	settings sessionconfig.Settings
	// claimedID and sourceKind are set by `factoryd memory propose`: the
	// request id it claimed before writing its proposal file
	// (requestsubmit.Params.ID), and request.SourceMemory in place of the
	// kind derived from the inputs above.
	claimedID  string
	sourceKind request.SourceKind
	// fetchIssue is threaded through to resolveSubmitRequestText so tests
	// can inject a stub the same way submitMain's own tests already do.
	fetchIssue issueFetchFunc
}

// submitResult is what a caller of submitRequest -- factoryd submit's own
// CLI wrapper today, POST /requests (internal/api) as of Phase 2 -- needs
// out of a successful submission.
type submitResult struct {
	id string
	// project/repositoryRoot are the same values the entry itself is
	// recorded under; surfaced here so a caller doesn't have to recompute
	// them from workspaceArg itself.
	project        string
	repositoryRoot string
}

// submitRequest resolves submit's own CLI-only inputs (-request-file/
// -issue/trailing text, via resolveSubmitRequestText -- the one part of
// `factoryd submit` with no HTTP equivalent, since an API caller always
// posts the request text directly) and then calls
// internal/requestsubmit.Submit for everything else: workspace
// validation, .factory.yml default resolution, and
// internal/request.Request creation. That package is the single
// implementation POST /requests (internal/api) also calls, so the two
// entry points cannot drift apart on what counts as a legal submission.
func submitRequest(dp *deps, ctx context.Context, p submitParams) (submitResult, error) {
	warnProjectToolchains(dp, ctx, os.Stderr, p.workspaceArg, p.settings)
	importedSpec, trailingText, err := resolveSubmitSpecFile(p)
	if err != nil {
		return submitResult{}, err
	}
	importedTickets, err := readSubmitPlanDir(p.planDir)
	if err != nil {
		return submitResult{}, err
	}
	requestText, issueRef, idText, err := resolveSubmitRequestText(ctx, p.requestFile, p.issue, trailingText, p.fetchIssue)
	if err != nil {
		return submitResult{}, err
	}
	sourceKind := request.SourceText
	switch {
	case p.issue != "":
		sourceKind = request.SourceIssue
	case p.requestFile != "":
		sourceKind = request.SourceFile
	}
	if p.sourceKind != "" {
		sourceKind = p.sourceKind
	}

	result, err := requestsubmit.Submit(requestsubmit.Params{
		WorkspaceArg:               p.workspaceArg,
		DataDir:                    p.dataDir,
		RequestText:                requestText,
		IDText:                     idText,
		ID:                         p.claimedID,
		Source:                     request.Source{Kind: sourceKind, IssueRef: issueRef},
		VerifyCommand:              p.verifyCommand,
		VerifyCommandExplicit:      p.verifyCommandExplicit,
		FullSuiteCommand:           p.fullSuiteCommand,
		NoCommitOracles:            p.noCommitOracles,
		DraftOracles:               p.draftOracles,
		Bug:                        p.bug,
		ImportedSpec:               importedSpec,
		ImportedTickets:            importedTickets,
		PreflightProfile:           p.preflightProfile,
		PreflightProfileExplicit:   p.preflightProfileExplicit,
		Harnesses:                  p.harnesses,
		SessionTokenCeiling:        p.sessionTokenCeiling,
		SessionCostCeilingMicroUSD: p.sessionCostCeilingMicroUSD,
		Models:                     p.models,
		Settings:                   p.settings,
	})
	if err != nil {
		return submitResult{}, err
	}
	wakeAfterSave(dp, ctx, os.Stderr, p.dataDir, result.ID)
	return submitResult{id: result.ID, project: result.Project, repositoryRoot: result.RepositoryRoot}, nil
}

// resolveSubmitSpecFile reads -spec-file and returns its content with the
// trailing request text to use. A handed-over spec may come with no request
// text at all; its own Problem section then stands in, so the request has
// a title and planning has an "original request" to read.
func resolveSubmitSpecFile(p submitParams) (spec string, trailingText []string, err error) {
	if p.specFile == "" {
		return "", p.trailingText, nil
	}
	data, err := os.ReadFile(p.specFile)
	if err != nil {
		return "", nil, fmt.Errorf("read -spec-file: %w", err)
	}
	spec = string(data)
	if p.requestFile != "" || p.issue != "" || len(p.trailingText) > 0 {
		return spec, p.trailingText, nil
	}
	problem := specSection(spec, "## Problem")
	if problem == "" {
		return "", nil, fmt.Errorf("-spec-file %s has no text under \"## Problem\" to use as the request text; pass a request text as well", p.specFile)
	}
	return spec, []string{problem}, nil
}

// readSubmitPlanDir reads -plan-dir's tickets, in name order. Their names
// and content are checked by requestsubmit.Submit.
func readSubmitPlanDir(dir string) ([]requestsubmit.ImportedTicket, error) {
	if dir == "" {
		return nil, nil
	}
	files, err := requestdriver.ReadTicketFiles(dir)
	if err != nil {
		return nil, fmt.Errorf("read -plan-dir: %w", err)
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("-plan-dir %s holds no ticket file (001.spec.md, 002.spec.md, ...)", dir)
	}
	tickets := make([]requestsubmit.ImportedTicket, 0, len(files))
	for _, file := range files {
		tickets = append(tickets, requestsubmit.ImportedTicket{Filename: file.Filename, Content: file.Content})
	}
	return tickets, nil
}

// specSection returns the trimmed text under heading, up to the next
// "## " heading or the end of the spec.
func specSection(spec, heading string) string {
	var body []string
	inside := false
	for _, line := range strings.Split(spec, "\n") {
		switch {
		case strings.TrimSpace(line) == heading:
			inside = true
		case inside && strings.HasPrefix(line, "## "):
			return strings.TrimSpace(strings.Join(body, "\n"))
		case inside:
			body = append(body, line)
		}
	}
	return strings.TrimSpace(strings.Join(body, "\n"))
}

// submitMain implements `factoryd submit -<flags> <workspace> "<request
// text>"`: a thin CLI wrapper that parses flags, calls submitRequest, and
// prints exactly what it prints today (just the new request's id).
//
// Every flag must precede the positional <workspace> argument (and any
// trailing request text): Go's flag.FlagSet stops parsing flags at the
// first non-flag argument, so `factoryd submit <workspace> -issue <url>`
// silently treats "-issue" and "<url>" as literal trailing request text
// rather than the -issue flag -- this is documented flag.Parse behavior,
// not specific to any one flag here, so it applies to -request-file/
// -verify-command/-preflight-profile/-data-dir too.
// newSubmitFlags builds `factoryd submit`'s FlagSet in isolation from
// parsing, so USAGE.md's doc-vs-flag drift test
// (TestUSAGEDocFlagsExistOnSubcommand) can enumerate its real flags without
// executing the command.
func newSubmitFlags() (flags *flag.FlagSet, requestFile, issue, verifyCommand, preflightProfile, dataDir, consoleBaseURL *string, watch *bool) {
	flags = flag.NewFlagSet("submit", flag.ContinueOnError)
	requestFile = flags.String("request-file", "", "Read the request text from this file. Cannot be combined with -issue or a request text on the command line.")
	issue = flags.String("issue", "", "Take the request text from this GitHub issue URL's title and body. gh must be installed and authenticated. The request's pull request closes the issue on merge.")
	verifyCommand = flags.String("verify-command", "", "Verification command for this request's tickets. Empty uses verify_command from the workspace's .factory.yml.")
	preflightProfile = flags.String("preflight-profile", "", "Preflight strictness: \"brownfield\" does not require the repository's spec/spec.md, spec/contract.md and ARCHITECTURE.md. Unset uses preflight_profile from the workspace's .factory.yml, else strict.")
	flags.String("full-suite-command", "", "Repo-wide regression command that must pass before a pull request opens. Empty uses .factory.yml's full_suite_command, else the verify command. \"none\" runs no full suite, which denies release.")
	flags.String("spec-file", "", "Hand over a finished spec instead of drafting one. It must use the spec skeleton (a drafted spec's headings in order, with numbered criteria) or submit refuses it. The request text is then optional.")
	flags.String("plan-dir", "", "With -spec-file (required), hand over the plan too: a directory of tickets 001.spec.md, 002.spec.md, ... in dependency order, in the ticket format, covering every acceptance criterion of the spec.")
	flags.Bool("no-commit-oracles", false, "Keep accepted oracles out of the target repository. The oracle still gates every build, but it is not committed into the result and no verify runs after a commit.")
	flags.Bool("bug", false, "The request fixes a bug. Adds the oracle stage and requires a test that fails on the current code: a build whose oracle passes before any change is quarantined without a model call.")
	flags.Bool("draft-oracles", false, "Add the oracle stage: after spec approval, acceptance-test oracles are drafted and wait at oracle_review for you to approve, hand-write or skip before planning.")
	flags.Var(&roleChoiceFlag{name: "harness", example: "pifork"}, "harness", "Coding-agent harness for one role of this request, as role=name (e.g. -harness execution=pifork), within roles.<role>.allowed_harnesses. The role is planning or execution. Repeatable.")
	flags.Var(&roleChoiceFlag{name: "model", example: "sonnet"}, "model", "Model for one role of this request, as role=model (e.g. -model execution=sonnet), within roles.<role>.allowed. The role is planning or execution. Repeatable.")
	flags.String("config", "", "Session config file or profile name. Empty uses the active profile, then the default paths. Its roles block is validated and bounds -model and -harness.")
	dataDir = flags.String("data-dir", "data", "Directory for durable request and run records. Must be the one the worker uses. Unset uses the session config's data_dir.")
	consoleBaseURL = flags.String("console-base-url", "", "Base URL of the console web app (not the API address), used for the link printed after the request id. Empty uses $FACTORYD_CONSOLE_URL, then the address of the running serve.")
	watch = flags.Bool("watch", false, "After submitting, follow the request as `factoryd watch <id>` does instead of exiting.")
	finishFlags(flags)
	return
}

func submitMain(dp *deps, args []string) error {
	flags, requestFile, issue, verifyCommand, preflightProfile, dataDir, consoleBaseURL, watch := newSubmitFlags()
	if err := flags.Parse(args); err != nil {
		return err
	}
	// Once, at submit's own process start: a session config with an
	// invalid roles: block must not be accepted at submit time. POST
	// /requests (internal/api) is a separate entry point into
	// requestsubmit.Submit with no cmd/factoryd process start of its own,
	// so it is covered only by serve's own start check
	// (validateRoles in serveMain), not by anything here.
	settings, err := loadSettingsForConfig(flags.Lookup("config").Value.String())
	if err != nil {
		return err
	}
	if err := validateRoles(settings); err != nil {
		return err
	}
	// The session's own effective relay ceilings -- threaded into
	// submitRequest so requestsubmit.Submit's tighten-only .factory.yml
	// check compares a repo's committed ceiling against the ceiling this
	// same session would actually launch a run under, not
	// requestsubmit's own hardcoded legacy defaults (see
	// requestsubmit.Params.SessionTokenCeiling's doc comment).
	effectiveTokenCeiling, effectiveCostCeilingMicroUSD := settings.EffectiveRelayCeilings()
	sessionTokenCeiling := int64(effectiveTokenCeiling)
	sessionCostCeilingMicroUSD := effectiveCostCeilingMicroUSD
	// resolveDataDirFromSessionConfig, not logDataDirSource: submit used to
	// ignore the session config's own data_dir key entirely and only ever
	// worked because cwd happened to match, unlike worker/serve/status/
	// watch, which all already resolve -data-dir through it. submit's own
	// -config flag is reused here too, so an operator passing -config gets
	// -data-dir from that same file, not a second, independent
	// default-path search.
	if err := resolveDataDirFromSessionConfig(flags, dataDir, flags.Lookup("config").Value.String()); err != nil {
		return err
	}

	positional := flags.Args()
	if len(positional) < 1 {
		flags.Usage()
		return fmt.Errorf("usage: factoryd submit [flags] <workspace> \"<request text>\" (or -request-file <path>, or -issue <url>) -- flags must come before <workspace>")
	}

	explicit := map[string]bool{}
	flags.Visit(func(f *flag.Flag) { explicit[f.Name] = true })

	result, err := submitRequest(dp, context.Background(), submitParams{
		workspaceArg:               positional[0],
		requestFile:                *requestFile,
		issue:                      *issue,
		trailingText:               positional[1:],
		specFile:                   flags.Lookup("spec-file").Value.String(),
		planDir:                    flags.Lookup("plan-dir").Value.String(),
		verifyCommand:              *verifyCommand,
		fullSuiteCommand:           flags.Lookup("full-suite-command").Value.String(),
		noCommitOracles:            flags.Lookup("no-commit-oracles").Value.String() == "true",
		draftOracles:               flags.Lookup("draft-oracles").Value.String() == "true",
		bug:                        flags.Lookup("bug").Value.String() == "true",
		verifyCommandExplicit:      explicit["verify-command"],
		preflightProfile:           *preflightProfile,
		preflightProfileExplicit:   explicit["preflight-profile"],
		dataDir:                    *dataDir,
		harnesses:                  flags.Lookup("harness").Value.(*roleChoiceFlag).values,
		sessionTokenCeiling:        sessionTokenCeiling,
		sessionCostCeilingMicroUSD: sessionCostCeilingMicroUSD,
		models:                     flags.Lookup("model").Value.(*roleChoiceFlag).values,
		settings:                   settings,
		fetchIssue:                 (ghIssueFetcher{}).fetch,
	})
	if err != nil {
		return err
	}

	fmt.Println(result.id)
	startDependenciesAfterSubmit(dp, os.Stdout, flags.Lookup("config").Value.String(), *dataDir)
	if url := consoleRequestURL(resolveConsoleBaseURL(*consoleBaseURL, *dataDir), result.id); url != "" {
		fmt.Println("View:", url)
	} else {
		fmt.Println(noConsoleLinkHint(*dataDir))
	}
	if *watch {
		return watchRequest(os.Stdout, *dataDir, result.id, false)
	}
	return nil
}

// noConsoleLinkHint says why submit printed no console link and what fixes
// it: a binary built without the console (serve runs but has nothing to
// show), or no serve for this data dir.
func noConsoleLinkHint(dataDir string) string {
	if !consoleweb.Embedded() {
		return "No console link: this factoryd was built without the console -- `make install` (with Node and npm on PATH) embeds it."
	}
	if consolelink.ServeAddress(dataDir) == "" {
		return "No console link: no `factoryd serve` is running for this data dir -- start one with `factoryd serve`."
	}
	return "No console link could be built for this request."
}

// startDependenciesAfterSubmit makes sure the request just queued gets
// drained and has a console: it starts a worker (worker without Temporal) and serve when missing
// (autostart on), or only warns when nothing drains the queue (autostart off).
func startDependenciesAfterSubmit(dp *deps, w io.Writer, configFlag, dataDir string) {
	if !hostcontrol.AutostartEnabled() {
		hostcontrol.WarnIfNoWorker(w, dataDir)
		return
	}
	configPath, _ := resolveEffectiveConfigPath(configFlag)
	if abs, err := filepath.Abs(dataDir); err == nil {
		dataDir = abs
	}
	hostcontrol.EnsureWorkerAndServe(dp, w, configPath, dataDir)
}

// issueFetchFunc fetches one GitHub issue's title, body, and number;
// matches ghIssueFetcher.fetch's own signature. Taken as a parameter
// (rather than read off a package var) so a test can inject a stub
// without ever touching shared mutable state.
type issueFetchFunc func(ctx context.Context, issueURL string) (title, body string, number int, err error)

// resolveSubmitRequestText returns the request text from exactly one of
// -request-file, -issue, or the trailing positional command-line
// arguments, the fully-qualified "<owner>/<repo>#<N>" reference to close
// (empty unless issueURL was used), and idText -- the text request IDs
// should be slugged from, which for -issue is the issue's own title
// alone rather than title+body (title+body used to leak issue body words
// into the id, e.g. an issue body containing a stack trace producing an
// unrecognizable slug) -- empty for every other source, meaning "slug
// the request text itself" (see requestsubmit.Params.IDText). All three
// sources are mutually exclusive. fetchIssue is only called when
// issueURL != "".
func resolveSubmitRequestText(ctx context.Context, requestFile, issueURL string, trailing []string, fetchIssue issueFetchFunc) (text, issueRef, idText string, err error) {
	sources := 0
	for _, set := range []bool{requestFile != "", issueURL != "", len(trailing) > 0} {
		if set {
			sources++
		}
	}
	if sources > 1 {
		return "", "", "", fmt.Errorf("-request-file, -issue, and an inline request text are mutually exclusive")
	}

	switch {
	case issueURL != "":
		owner, repo, err := parseGitHubIssueURL(issueURL)
		if err != nil {
			return "", "", "", err
		}
		title, body, number, err := fetchIssue(ctx, issueURL)
		if err != nil {
			return "", "", "", fmt.Errorf("fetch -issue %s: %w", issueURL, err)
		}
		text = title
		idText = title
		if strings.TrimSpace(body) != "" {
			text = title + "\n\n" + body
		}
		// Fully qualified, even though the PR this eventually closes will
		// almost always live in this same owner/repo: -issue's URL and the
		// <workspace> being submitted against are two independent inputs
		// with nothing enforcing they name the same repository, and
		// GitHub's bare "Closes #<N>" form is resolved against whatever
		// repository the PR itself lands in -- silently closing an
		// unrelated same-numbered issue there if the two ever differ.
		// The qualified form is accepted by GitHub even when it IS the
		// same repo, so always qualifying sidesteps the ambiguity rather
		// than needing to detect a mismatch (found via Codex review of PR
		// #92).
		issueRef = fmt.Sprintf("%s/%s#%d", owner, repo, number)
	case requestFile != "":
		b, err := os.ReadFile(requestFile)
		if err != nil {
			return "", "", "", fmt.Errorf("read -request-file: %w", err)
		}
		text = string(b)
	default:
		if len(trailing) == 0 {
			return "", "", "", fmt.Errorf("usage: factoryd submit [flags] <workspace> \"<request text>\" (or -request-file <path>, or -issue <url>) -- flags must come before <workspace>")
		}
		text = strings.Join(trailing, " ")
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return "", "", "", fmt.Errorf("request text must not be empty")
	}
	return text, issueRef, idText, nil
}

// githubIssueURLPattern extracts owner and repo from a GitHub issue URL,
// e.g. "https://github.com/acme/widgets/issues/42" -- the same URL shape
// `gh issue view` itself accepts.
var githubIssueURLPattern = regexp.MustCompile(`^https://github\.com/([^/]+)/([^/]+)/issues/\d+/?$`)

// parseGitHubIssueURL returns the owner/repo an issue URL names, so
// resolveSubmitRequestText can always render a fully-qualified Closes
// reference (see its own doc comment) regardless of which repository
// <workspace> itself is.
func parseGitHubIssueURL(issueURL string) (owner, repo string, err error) {
	m := githubIssueURLPattern.FindStringSubmatch(issueURL)
	if m == nil {
		return "", "", fmt.Errorf("-issue must be a GitHub issue URL of the form https://github.com/<owner>/<repo>/issues/<number>, got %q", issueURL)
	}
	return m[1], m[2], nil
}

// ghIssueFetcher fetches one GitHub issue's title, body, and number via
// `gh issue view <url> --json title,body,number` -- the same shelling-out-
// to-gh convention internal/forge.GHPullRequestOpener already establishes
// for this codebase's GitHub operations. GHBinary defaults to "gh" when
// empty; a struct field, not a hardcoded call, so tests can point it at a
// fake executable instead of ever invoking a real gh.
type ghIssueFetcher struct {
	GHBinary string
}

// ghIssueViewTimeout bounds one `gh issue view` invocation -- generous
// enough for a real network round trip, short enough that an
// unauthenticated or hung gh doesn't block `submit` indefinitely.
const ghIssueViewTimeout = 30 * time.Second

func (f ghIssueFetcher) fetch(ctx context.Context, issueURL string) (title, body string, number int, err error) {
	ghBinary := f.GHBinary
	if ghBinary == "" {
		ghBinary = "gh"
	}
	ctx, cancel := context.WithTimeout(ctx, ghIssueViewTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, ghBinary, "issue", "view", issueURL, "--json", "title,body,number")
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", "", 0, fmt.Errorf("gh issue view failed (check gh is installed and authenticated, the issue exists, and the network is reachable): %s", msg)
	}

	var parsed struct {
		Title  string `json:"title"`
		Body   string `json:"body"`
		Number int    `json:"number"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil {
		return "", "", 0, fmt.Errorf("parse gh issue view output: %w", err)
	}
	return parsed.Title, parsed.Body, parsed.Number, nil
}
