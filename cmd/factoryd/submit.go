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
	requestFile = flags.String("request-file", "", "read the request text from this file instead of the trailing command-line argument. Must precede <workspace> on the command line (flag.Parse stops at the first positional argument)")
	issue = flags.String("issue", "", "fetch the request text from this GitHub issue's own title and body instead of the trailing command-line argument or -request-file (`gh issue view <url>` must succeed: gh installed, authenticated, and the issue reachable). The queued run's eventual draft PR closes this issue on merge (see run.Run.PRCloses). Must precede <workspace> on the command line (flag.Parse stops at the first positional argument)")
	verifyCommand = flags.String("verify-command", "", "canonical verification command for this ticket (default: verify_command from the workspace's .factory.yml). Must precede <workspace> on the command line")
	preflightProfile = flags.String("preflight-profile", "", "project-bootstrap preflight strictness override, same meaning as factoryd's own -preflight-profile (default: preflight_profile from the workspace's .factory.yml). Must precede <workspace> on the command line")
	flags.String("full-suite-command", "", "repo-wide regression command for this request's tickets, run as the full_suite_verify gate the default release policy requires before a pull request can open (default: the workspace's .factory.yml full_suite_command; if neither resolves, this request's own resolved verify command is used instead and recorded as such -- see full_suite_source on the request/run). Pass the literal value \"none\" to opt out of that substitution and keep the prior behavior (no full-suite command configured denies release). Must precede <workspace> on the command line")
	flags.String("spec-file", "", "hand over a finished spec in place of the drafted one: the file must use the spec skeleton (the headings a drafted spec has, in order, with numbered acceptance criteria) or the submission is refused. The request reaches spec_review with no drafting model call; rejecting it there has the model revise your document with your feedback. The request text may be omitted: the spec's own Problem section is used. Must precede <workspace> on the command line.")
	flags.String("plan-dir", "", "with -spec-file, hand over the finished plan too: a directory of tickets named 001.spec.md, 002.spec.md, ... in dependency order, each in the ticket format (header lines, then Goal, Plan and Out of scope), together covering every acceptance criterion of the spec. After you approve the spec the request reaches plan_review with no planning model call; rejecting the plan there has the model revise your tickets with your feedback. Must precede <workspace> on the command line.")
	flags.Bool("no-commit-oracles", false, "opt this request out of the host commit of accepted oracles: the oracle still gates every build, but nothing is written to the target repository and no post-commit verify runs. Recorded on the request and the run. Default false: an accepted oracle with a target_path is committed into the result. On a mixed-version Temporal rollout an older worker ignores the field, so the opt-out is a guarantee only when every binary is current")
	flags.Bool("draft-oracles", false, "opt this request into the staged oracle stage: after spec approval the request goes through oracle_drafting and oracle_review (approve, hand-write, or skip request-level acceptance-test oracles) before planning. Default off: spec approval goes straight to planning. Must precede <workspace> on the command line")
	flags.Var(&roleChoiceFlag{name: "harness", example: "pifork"}, "harness", `choose the coding-agent harness for one role, within that role's own roles.<role>.allowed_harnesses (roles: session config): "role=name", e.g. -harness execution=pifork. Repeatable. Only "planning" and "execution" are selectable; review is never requester-selectable. Must precede <workspace> on the command line`)
	flags.Var(&roleChoiceFlag{name: "model", example: "sonnet"}, "model", `choose the model for one role, within that role's own roles.<role>.allowed (routes:/models:/roles: session config): "role=model", e.g. -model execution=sonnet. Repeatable (-model execution=sonnet -model planning=opus). Only "planning" and "execution" are selectable -- review is never requester-selectable. The factory still owns every other per-role setting (routes, thinking). Must precede <workspace> on the command line`)
	flags.String("config", "", "session config file to validate roles: against (default: the same search path `factoryd worker -config` uses). This command validates the resolved session config's roles: block once at start regardless. Must precede <workspace> on the command line")
	dataDir = flags.String("data-dir", "data", "directory for durable queue and run records; must match what a later `factoryd worker` uses. Must precede <workspace> on the command line")
	consoleBaseURL = flags.String("console-base-url", "", "base URL where the console web app (console/) is served, e.g. http://localhost:PORT -- NOT the factoryd API address. When set (or FACTORYD_CONSOLE_URL is), the printed request id is followed by a direct console link. Must precede <workspace> on the command line")
	watch = flags.Bool("watch", false, "after submitting, attach to the request the same way `factoryd watch <id>` would (in-process, not a subprocess) instead of exiting immediately. Must precede <workspace> on the command line")
	plainFlagUsage(flags)
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
