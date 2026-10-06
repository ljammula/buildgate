package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"buildgate/internal/daemonheartbeat"
	"buildgate/internal/modelrole"
	"buildgate/internal/progress"
	"buildgate/internal/release"
	"buildgate/internal/request"
	"buildgate/internal/requestdriver"
	"buildgate/internal/run"
	"buildgate/internal/sanitize"
	"buildgate/internal/sessionconfig"
)

// statusShortIDLen is how many characters of a run's ID this command
// prints in its default (non-JSON) table — enough to disambiguate runs by
// eye without the full ID's line-wrapping.
const statusShortIDLen = 8

// statusEntry is one run's line of `factoryd status` output, in both the
// -json and human-readable renderings.
type statusEntry struct {
	ID         string `json:"id"`
	Project    string `json:"project"`
	Repository string `json:"repository"`
	Ticket     string `json:"ticket"`
	State      string `json:"state"`
	Elapsed    string `json:"elapsed,omitempty"`
	Duration   string `json:"duration,omitempty"`
	CostUSD    string `json:"cost_usd,omitempty"`
	// PullRequestURL and Reason are mutually exclusive: a run only ever
	// has a reason to report when it has no PR to point at instead.
	PullRequestURL string `json:"pull_request_url,omitempty"`
	Reason         string `json:"reason,omitempty"`
	// TemporalUIURL links to this run's Temporal Web UI page (see
	// TemporalUIURL's own doc comment) — empty for a direct (non-Temporal)
	// run, or one whose Temporal server isn't on the default port.
	TemporalUIURL string `json:"temporal_ui_url,omitempty"`

	// Stage/LastActivityAt/WaitingReason/Stalled are a non-terminal run's
	// current-activity summary from its own progress feed
	// (internal/progress.Summary), filled in by applyProgressStatus.
	// Empty/false for a terminal run or a queue entry, neither of which
	// has a progress feed left worth reading.
	Stage          string `json:"stage,omitempty"`
	LastActivityAt string `json:"last_activity_at,omitempty"`
	WaitingReason  string `json:"waiting_reason,omitempty"`
	Stalled        bool   `json:"stalled,omitempty"`

	// nonTerminalRun/stalledSince are render-only: set by
	// applyProgressStatus alongside the exported fields above, and read
	// only by statusMain's plain-table renderer (statusProgressColumn)
	// to decide whether this row gets the progress rendering at all, and
	// how many minutes to show in its STALLED prefix. Unexported, so
	// never marshaled to JSON -- mirrors internal/request.Request's own
	// prevState field for the same reason.
	nonTerminalRun bool
	stalledSince   time.Duration
}

// applyProgressStatus fills every entry in entries whose ID matches a
// non-terminal (not yet runTerminalConfirmed) run in runs with that run's
// current-activity summary. A separate pass over buildStatusEntries' own
// output, rather than a dataDir parameter added to buildStatusEntries
// itself, because that function is also called from retry_test.go, which
// this change does not touch.
func applyProgressStatus(entries []statusEntry, runs []*run.Run, dataDir string, now time.Time) {
	byID := make(map[string]*run.Run, len(runs))
	for _, r := range runs {
		byID[r.ID] = r
	}
	for i := range entries {
		r, ok := byID[entries[i].ID]
		if !ok || runTerminalConfirmed(r) {
			continue
		}
		summary, err := progress.Summary(progress.Path(dataDir, r.ID))
		if err != nil {
			continue
		}
		entries[i].Stage = summary.CurrentStage
		entries[i].LastActivityAt = summary.LastAt
		entries[i].WaitingReason = summary.WaitingReason
		entries[i].nonTerminalRun = true
		entries[i].Stalled, entries[i].stalledSince = progress.Stalled(summary, r.CreatedAt, now)
	}
}

// statusProgressColumn renders a non-terminal run's reason/PR column: its
// current stage, its waiting reason when one is set, and a leading
// STALLED marker when its progress feed has gone quiet past
// progress.StallAfter.
// Only called for a row with nonTerminalRun set (see applyProgressStatus)
// -- a terminal run or queue entry keeps its ordinary PR-or-reason
// column untouched.
func statusProgressColumn(e statusEntry) string {
	col := e.Stage
	if e.WaitingReason != "" {
		col += " · waiting: " + e.WaitingReason
	}
	if e.Stalled {
		col = fmt.Sprintf("STALLED %dm %s", int(e.stalledSince/time.Minute), col)
	}
	return col
}

// TemporalUIURL maps a Temporal server address to its Web UI's page for one
// workflow execution (USAGE_REFERENCE.md, "Observe a run in Temporal": the UI's default port, 8233,
// is a fixed offset from the gRPC frontend's default port, 7233, in this
// repo's own docker-compose.temporal.yml — not a general Temporal
// convention, so this only applies when address uses that exact default
// port). Returns "" for any other port, rather than guess at a mapping
// that wouldn't actually be the same server's own UI, and for an empty
// workflowID (nothing to link to yet).
func TemporalUIURL(address, workflowID, runID string) string {
	if workflowID == "" {
		return ""
	}
	host, port, err := net.SplitHostPort(address)
	if err != nil || port != "7233" {
		return ""
	}
	return fmt.Sprintf("http://%s:8233/namespaces/default/workflows/%s/%s", host, workflowID, runID)
}

// requestStatusEntry is one request's line of `factoryd status` output --
// shown above every run/queue-entry line, per the plan's own "the console
// shows the state of every request" contract (here, its CLI equivalent).
type requestStatusEntry struct {
	ID      string `json:"id"`
	Project string `json:"project"`
	State   string `json:"state"`
	Age     string `json:"age"`
	Error   string `json:"error,omitempty"`
	// AwaitingPullRequest is set for a halted request whose only gap is a
	// missing pull request (request.HaltAcceptedNoPR); Label is its calm
	// one-line status with the exact next command. Both empty otherwise.
	AwaitingPullRequest bool   `json:"awaiting_pull_request,omitempty"`
	Label               string `json:"label,omitempty"`
	// Ticket is "i/n" (r.TicketIndex/r.TicketCount) while r is building or
	// in pr_review; empty otherwise -- TicketCount is 0 in every
	// other state (see internal/request.Request.TicketCount's own doc
	// comment).
	Ticket string `json:"ticket,omitempty"`
	// PullRequestURL is the latest ticket's own PR URL (r.Tickets
	// [r.TicketIndex-1].PRURL) while r is building or in pr_review; empty
	// until that ticket's run has actually opened one.
	PullRequestURL string `json:"pull_request_url,omitempty"`
	// Tickets is a compact "<index>:<pr_state>" summary, space-separated,
	// one entry per ticket that has an open pull request -- the
	// PR-review driver's own per-ticket PR status (Ticket.PRState: "open"
	// -- the default, shown when PRState is still "" -- "ready", "approved", or
	// "merged"). Empty when this request has no ticket with a PR yet
	// (every state before building/pr_review, or a ticket not yet
	// opened).
	Tickets string `json:"tickets,omitempty"`
	// WaitingOn is the id of the request the driver is advancing while
	// this one waits its turn (request.WaitingOn). Without it, a request
	// queued behind another showed as plain "building", indistinguishable
	// from one whose ticket is actually running (C5).
	WaitingOn string `json:"waiting_on,omitempty"`
}

// requestTicketPRSummary renders requestStatusEntry's own Tickets field:
// one "<index>:<pr_state>" entry per ticket that has a PR open, in ticket
// order, space-separated -- kept to one line so `factoryd status`'s
// existing per-request row (see the -json/table renderers below) only ever
// grows by one field, never a second line.
func requestTicketPRSummary(r *request.Request) string {
	var parts []string
	for _, t := range r.Tickets {
		if t.PRURL == "" {
			continue
		}
		state := t.PRState
		if state == "" {
			state = "open"
		}
		parts = append(parts, fmt.Sprintf("%d:%s", t.Index, state))
	}
	return strings.Join(parts, " ")
}

// buildRequestStatusEntry converts a loaded request record into its
// display form, with now used to compute Age from SubmittedAt and
// waitingOn the id request.WaitingOn reports for r ("" when none).
func buildRequestStatusEntry(r *request.Request, waitingOn string, now time.Time) requestStatusEntry {
	entry := requestStatusEntry{
		ID:      r.ID,
		Project: r.Project,
		State:   string(r.State),
		Error:   r.Error,
		Tickets: requestTicketPRSummary(r),
	}
	if r.AwaitingPullRequest() {
		entry.AwaitingPullRequest = true
		entry.Label = r.AwaitingPullRequestLabel()
	}
	if submitted, err := time.Parse(time.RFC3339, r.SubmittedAt); err == nil {
		entry.Age = now.Sub(submitted).Round(time.Second).String()
	}
	if r.State == request.StateBuilding || r.State == request.StatePRReview {
		entry.Ticket = fmt.Sprintf("%d/%d", r.TicketIndex, r.TicketCount)
		if r.TicketIndex >= 1 && r.TicketIndex <= len(r.Tickets) {
			entry.PullRequestURL = r.Tickets[r.TicketIndex-1].PRURL
		}
	}
	entry.WaitingOn = waitingOn
	return entry
}

// buildRequestStatusEntries converts every request under dataDir into its
// display form, newest-submitted last reversed to oldest-first -- same
// "most in need of attention first" ordering statusMain's own run/queue
// table uses (buildStatusEntries' own statusCreatedBefore), applied here
// to request.List's oldest-first result.
func buildRequestStatusEntries(requests []*request.Request, active []string, slots int, project string, now time.Time) []requestStatusEntry {
	waiting := request.WaitingOn(requests, active, slots)

	entries := make([]requestStatusEntry, 0, len(requests))
	for i := len(requests) - 1; i >= 0; i-- {
		r := requests[i]
		if project != "" && r.Project != project {
			continue
		}
		entries = append(entries, buildRequestStatusEntry(r, waiting[r.ID], now))
	}
	return entries
}

// statusCostMicroUSD sums the relay spend recorded across every attempt --
// the only durable spend/usage evidence a run carries (see
// Attempt.RelayConsumedCostMicroUSD's own doc comment). Zero when the run
// never had a relay, or its relay was never cleaned up successfully.
// partial is true when any attempt's total is a best-effort recovery from
// its usage ledger, not a confirmed final spend -- see
// Attempt.RelaySpendPartial's own doc comment.
func statusCostMicroUSD(r *run.Run) (total int64, partial bool) {
	for _, a := range r.Attempts {
		total += a.RelayConsumedCostMicroUSD
		if a.RelaySpendPartial {
			partial = true
		}
	}
	return total, partial
}

// statusRepository returns r.Repository when set, else the same project
// id release.ProjectOf reports -- a run that never used a shared
// per-repository task queue (see run.Run.Repository's own doc comment)
// still has something meaningful to show in a repository column.
func statusRepository(r *run.Run) string {
	if r.Repository != "" {
		return r.Repository
	}
	return release.ProjectOf(r)
}

// statusCompletionTime returns the finish time recorded by this run's
// LAST attempt only -- never an earlier one. A run's attempts are
// sequential (e.g. build then verify); if the last attempt has no
// parseable FinishedAt, that attempt itself hasn't recorded finishing, so
// the run's completion time is genuinely unknown even if an earlier
// attempt did finish. r.UpdatedAt is deliberately not used here either:
// it can be overwritten by later successor processing (e.g. spec-drift
// annotation on a downstream chained run), so it is not trustworthy
// evidence of when THIS run itself finished.
func statusCompletionTime(r *run.Run) (time.Time, bool) {
	if len(r.Attempts) == 0 {
		return time.Time{}, false
	}
	last := r.Attempts[len(r.Attempts)-1]
	if last.FinishedAt == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, last.FinishedAt)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// statusCreatedBefore reports whether a's created-at timestamp should
// sort before b's in `factoryd status`'s newest-first ordering: later
// (more recent) times come first, and an unparseable timestamp always
// sorts last regardless of which side it's on.
func statusCreatedBefore(a, b string) bool {
	aTime, aErr := time.Parse(time.RFC3339, a)
	bTime, bErr := time.Parse(time.RFC3339, b)
	if aErr == nil && bErr == nil {
		return aTime.After(bTime)
	}
	if aErr == nil {
		return true
	}
	return false
}

// buildStatusEntry converts a loaded run record into its display form,
// with now used for an in-progress run's elapsed time.
func buildStatusEntry(r *run.Run, now time.Time) statusEntry {
	entry := statusEntry{
		ID:         r.ID,
		Project:    release.ProjectOf(r),
		Repository: statusRepository(r),
		Ticket:     r.Ticket,
		State:      string(r.State),
	}

	created, createdErr := time.Parse(time.RFC3339, r.CreatedAt)
	// runTerminalConfirmed, not a bare state check: a Halted run whose
	// HaltConfirmed is still false may genuinely still be running (see
	// that function's own doc comment), so it must stay in elapsed mode
	// rather than being shown with a finished duration.
	if runTerminalConfirmed(r) {
		if completed, ok := statusCompletionTime(r); ok && createdErr == nil {
			entry.Duration = completed.Sub(created).Round(time.Second).String()
		} else {
			entry.Duration = "?"
		}
	} else if createdErr == nil {
		entry.Elapsed = now.Sub(created).Round(time.Second).String()
	}

	if costMicroUSD, partial := statusCostMicroUSD(r); costMicroUSD > 0 || partial {
		entry.CostUSD = fmt.Sprintf("%.4f", float64(costMicroUSD)/1e6)
		if partial {
			entry.CostUSD += " (partial, relay exited abnormally)"
		}
		if run.SubscriptionBilled(r.Attempts) {
			entry.CostUSD += subscriptionCostSuffix
		}
	}

	if r.PullRequestURL != "" {
		entry.PullRequestURL = r.PullRequestURL
	} else {
		entry.Reason = requestdriver.StatusReason(r)
	}
	entry.TemporalUIURL = TemporalUIURL(r.TemporalAddress, r.TemporalWorkflowID, r.TemporalRunID)
	return entry
}

// loadStatusRuns loads every run record under dataDir, warning to stderr
// (rather than aborting) about any record that fails to load -- a status
// command exists specifically to be usable while other parts of a
// project's data directory are in an unexpected state, so one corrupt
// run.json must not hide every other run's status.
func loadStatusRuns(dataDir string, warn func(format string, args ...any)) ([]*run.Run, error) {
	entries, err := os.ReadDir(filepath.Join(dataDir, "runs"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	runs := make([]*run.Run, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		r, loadErr := run.Load(dataDir, entry.Name())
		if loadErr != nil {
			if errors.Is(loadErr, fs.ErrNotExist) {
				// runs/<id>/ was created (e.g. as a workspace) but run.json
				// hasn't been written yet -- not corruption, nothing to warn
				// about.
				continue
			}
			warn("status: warning: could not load run %q: %v", entry.Name(), loadErr)
			continue
		}
		runs = append(runs, r)
	}
	sort.SliceStable(runs, func(i, j int) bool {
		return statusCreatedBefore(runs[i].CreatedAt, runs[j].CreatedAt)
	})
	return runs, nil
}

// buildStatusEntries turns run records into status's final, ordered, limited
// output list: sorted newest-first by CreatedAt, filtered by project and
// state, capped at n.
func buildStatusEntries(runs []*run.Run, project, state string, n int, now time.Time) []statusEntry {
	type candidate struct {
		entry     statusEntry
		createdAt string
	}
	var candidates []candidate
	for _, r := range runs {
		if project != "" && release.ProjectOf(r) != project {
			continue
		}
		if state != "" && string(r.State) != state {
			continue
		}
		candidates = append(candidates, candidate{entry: buildStatusEntry(r, now), createdAt: r.CreatedAt})
	}

	sort.SliceStable(candidates, func(i, j int) bool {
		return statusCreatedBefore(candidates[i].createdAt, candidates[j].createdAt)
	})
	if len(candidates) > n {
		candidates = candidates[:n]
	}

	entries := make([]statusEntry, len(candidates))
	for i, c := range candidates {
		entries[i] = c.entry
	}
	return entries
}

// statusMain implements `factoryd status`: a read-only summary of every
// internal/request.Request under -data-dir (oldest first), shown above the
// runs recorded there (newest first); a request has no run record until it
// reaches building. The operator
// surface for answering "what is the factory doing right now" without
// querying `factoryd serve`'s HTTP API.
// rolesStatusLines renders one line per session-config role
// (planning/execution/review, in that fixed order) this configPath's
// roles: block resolves, e.g. "planning: fast (gpt-5-mini, thinking
// high, harness pi)". nil whenever roles: is absent, unset for every role, or the
// config fails to load at all -- a status read must never itself fail or
// print a warning over this (loadSettingsForConfig's own "never
// validate roles:" contract; validateRoles is what enforces that, at the
// commands that actually launch a run), so an
// invalid or missing config here simply renders no roles: section,
// exactly as if roles: were never set.
func rolesStatusLines(configPath string) []string {
	settings, err := loadSettingsForConfig(configPath)
	if err != nil || settings.Roles == nil {
		return nil
	}
	var lines []string
	for _, role := range []modelrole.Role{modelrole.RolePlanning, modelrole.RoleExecution, modelrole.RoleReview} {
		modelName, modelID, thinking, ok := modelrole.ConfiguredModel(settings, role)
		if !ok {
			continue
		}
		details := []string{}
		if modelID != "" {
			details = append(details, modelID)
		}
		if thinking != "" {
			details = append(details, "thinking "+thinking)
		}
		harnessName, err := modelrole.RoleHarness(settings, role, "")
		if err != nil {
			continue
		}
		details = append(details, "harness "+harnessName)
		line := fmt.Sprintf("%s: %s (%s)", role, modelName, strings.Join(details, ", "))
		lines = append(lines, line)
	}
	return lines
}

// newStatusFlags builds `factoryd status`'s FlagSet in isolation from
// parsing, so USAGE.md's doc-vs-flag drift test
// (TestUSAGEDocFlagsExistOnSubcommand) can enumerate its real flags without
// executing the command.
// statusProfile names the session config in effect: the -config value when
// given, else the one sessionconfig.ResolvePath finds. Both are "" when no
// config resolves (or the active profile's file is missing, which the
// data-dir resolution in statusMain already reported).
func statusProfile(configFlag string) (name, path string) {
	if configFlag != "" {
		path = sessionconfig.ResolveArg(configFlag)
	} else if p, found, err := sessionconfig.ResolvePath(); err == nil && found {
		path = p
	}
	if path == "" {
		return "", ""
	}
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	name = sessionconfig.ProfileNameOfPath(path)
	if name == "" {
		name = strings.TrimSuffix(filepath.Base(path), ".yml")
	}
	return name, path
}

// statusProfileLine is `factoryd status`'s first line, so an operator or
// agent sees which profile and data dir every following row comes from.
func statusProfileLine(name, path, dataDir string) string {
	if name == "" {
		return fmt.Sprintf("profile: none (no session config) · data dir %s", dataDir)
	}
	return fmt.Sprintf("profile: %s (%s) · data dir %s", name, path, dataDir)
}

func newStatusFlags() (flags *flag.FlagSet, dataDir, project, state, configPath *string, n *int, jsonOutput *bool) {
	flags = flag.NewFlagSet("status", flag.ContinueOnError)
	dataDir = flags.String("data-dir", "data", "directory containing durable run records")
	project = flags.String("project", "", "only show runs whose project id matches (see GET /projects or internal/release.ProjectFromWorkspace)")
	state = flags.String("state", "", "only show runs in this state (e.g. ready, slice_running, verifying, accepted, halted, quarantined)")
	n = flags.Int("n", 20, "maximum number of runs to show")
	jsonOutput = flags.Bool("json", false, "print machine-readable JSON instead of a table")
	configPath = flags.String("config", "", "session config path (for the roles: summary below); empty searches the default paths")
	plainFlagUsage(flags)
	return
}

func statusMain(args []string) error {
	flags, dataDir, project, state, configPath, n, jsonOutput := newStatusFlags()
	if err := flags.Parse(args); err != nil {
		return err
	}
	// *configPath, not "": status now has its own -config flag (for the
	// roles: summary below), and an operator who names one almost
	// certainly also wants -data-dir resolved from it, the same
	// "-config wins" precedence every other -config-bearing command
	// already gives that flag (applySessionConfig, doctorMain, ...).
	if err := resolveDataDirFromSessionConfig(flags, dataDir, *configPath); err != nil {
		return err
	}
	if *n <= 0 {
		flags.Usage()
		return fmt.Errorf("-n must be positive")
	}

	requests, err := request.List(*dataDir)
	if err != nil {
		return fmt.Errorf("load requests from %q: %w", *dataDir, err)
	}
	runs, err := loadStatusRuns(*dataDir, func(format string, args ...any) {
		fmt.Fprintf(os.Stderr, format+"\n", args...)
	})
	if err != nil {
		return fmt.Errorf("load runs from %q: %w", *dataDir, err)
	}

	now := time.Now()
	// -state only ever filters run.State values, never a
	// request.State -- a request has no equivalent flag yet (that's
	// a console filter instead, and a CLI -request-state would be
	// speculative ahead of any caller needing it), so requests are only
	// ever filtered by -project here.
	activeRequests, jobSlots := daemonheartbeat.WorkerActiveRequests(*dataDir, now)
	requestEntries := buildRequestStatusEntries(requests, activeRequests, jobSlots, *project, now)
	entries := buildStatusEntries(runs, *project, *state, *n, now)
	applyProgressStatus(entries, runs, *dataDir, now)

	// worker liveness. heartbeatLine is "" whenever worker has
	// never run against this data dir, or its heartbeat is still fresh --
	// see workerHeartbeatStatusLine's own doc comment. The stale-request
	// desktop notification (notifyWorkerStale) runs unconditionally,
	// not only in the human-readable branch below, so `factoryd status
	// -json` (a script or launchd-driven poll) still gets the same
	// one-time alert a human running the plain command would.
	heartbeatLine := workerHeartbeatStatusLine(*dataDir, now)
	notifyWorkerStale(*dataDir, requests, now, heartbeatLine)
	// Route visibility (2026-09-25): which subscription the live worker
	// bills to. Both "" whenever heartbeatLine is non-empty (worker
	// isn't running) or the heartbeat predates these fields -- see
	// workerRoute's own doc comment.
	routeMode, routeModel := workerRoute(*dataDir, now)
	routeLine := workerRouteStatusLine(*dataDir, now)
	// roles: visibility (P0e): one line per configured role
	// (planning/execution/review), each naming the alias it resolved to,
	// its worker model id, and its thinking level -- the per-role
	// counterpart to the single worker route line above, which only
	// ever shows one model regardless of how many roles: entries a
	// session config sets. Empty (nil) whenever roles: is absent or
	// unresolvable, so a config that never uses roles: renders identically
	// to before this field existed -- see TestStatusJSONWithoutRolesIsUnchanged.
	roleLines := rolesStatusLines(*configPath)
	profileName, profilePath := statusProfile(*configPath)

	if *jsonOutput {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(struct {
			Requests         []requestStatusEntry `json:"requests"`
			Runs             []statusEntry        `json:"runs"`
			WorkerHeartbeat  string               `json:"queue_run_heartbeat,omitempty"`
			WorkerRouteMode  string               `json:"queue_run_route_credential_mode,omitempty"`
			WorkerRouteModel string               `json:"queue_run_route_worker_model,omitempty"`
			Roles            []string             `json:"roles,omitempty"`
			Profile          string               `json:"profile,omitempty"`
			ConfigPath       string               `json:"config_path,omitempty"`
		}{
			Requests:         requestEntries,
			Runs:             entries,
			WorkerHeartbeat:  heartbeatLine,
			WorkerRouteMode:  routeMode,
			WorkerRouteModel: routeModel,
			Roles:            roleLines,
			Profile:          profileName,
			ConfigPath:       profilePath,
		})
	}

	fmt.Fprintln(os.Stdout, statusProfileLine(profileName, profilePath, *dataDir))

	// Requests are printed above runs (the plan's own "console shows the
	// state of every request", here its CLI equivalent) -- a request has
	// no run record of its own yet in this WP, so it would otherwise be
	// invisible to `factoryd status` entirely.
	// Request rows print the full id, padded to the longest one shown:
	// approve/reject/retry take only a full id, and ids generated from
	// similar titles share long prefixes, so a short id was ambiguous and
	// unusable (two "add-get-" rows in the 2026-09-26 Flutter + Go app run).
	idWidth := 0
	for _, r := range requestEntries {
		idWidth = max(idWidth, len(r.ID))
	}
	for _, r := range requestEntries {
		id := r.ID
		ticketOrDash := r.Ticket
		if ticketOrDash == "" {
			ticketOrDash = "-"
		}
		// A ticket's PR URL, once it has one, is more actionable to show
		// than the request's own Error (which is empty in building/
		// pr_review anyway -- those are non-terminal states) -- the same
		// "PR URL takes priority over a reason" rule buildStatusEntry's
		// own run rendering already follows.
		prOrError := r.PullRequestURL
		if prOrError == "" {
			// Untrusted error text: one line, so it can't split this row.
			prOrError = sanitize.Line(r.Error)
		}
		if prOrError == "" {
			prOrError = "-"
		}
		stateCol := r.State
		if r.AwaitingPullRequest {
			// Calm, distinct label instead of an alarming "halted" plus a
			// retry hint: the work is accepted, only the PR is missing.
			stateCol = "accepted*"
			prOrError = r.Label
		} else if r.WaitingOn != "" {
			// C5: a request next in line looked identical to the one the
			// driver is actually advancing -- its own state starred plus
			// the id it's queued behind, the same "asterisk marks a state
			// this factory annotates" convention accepted* above uses.
			stateCol = r.State + "*"
			prOrError = "queued behind " + r.WaitingOn
		}
		line := fmt.Sprintf("%-*s  %-24s  %-13s  %-10s  %-7s  %s", idWidth, id, r.Project, stateCol, r.Age, ticketOrDash, prOrError)
		if r.Tickets != "" {
			line += "  tickets: " + r.Tickets
		}
		if _, err := fmt.Fprintln(os.Stdout, line); err != nil {
			return fmt.Errorf("print request status line: %w", err)
		}
	}
	if len(requestEntries) > 0 && len(entries) > 0 {
		fmt.Fprintln(os.Stdout)
	}

	for _, e := range entries {
		id := e.ID
		if len(id) > statusShortIDLen {
			id = id[:statusShortIDLen]
		}
		elapsedOrDuration := e.Elapsed
		if elapsedOrDuration == "" {
			elapsedOrDuration = e.Duration
		}
		cost := e.CostUSD
		if cost != "" {
			cost = "$" + cost
		} else {
			cost = "-"
		}
		prOrReason := e.PullRequestURL
		if prOrReason == "" {
			prOrReason = sanitize.Line(e.Reason)
		}
		if e.nonTerminalRun {
			prOrReason = statusProgressColumn(e)
		}
		if _, err := fmt.Fprintf(os.Stdout, "%s  %-24s  %-24s  %-16s  %-13s  %-10s  %-10s  %s\n",
			id, e.Project, e.Repository, e.Ticket, e.State, elapsedOrDuration, cost, prOrReason); err != nil {
			return fmt.Errorf("print status line: %w", err)
		}
		if e.TemporalUIURL != "" {
			if _, err := fmt.Fprintf(os.Stdout, "  %s\n", e.TemporalUIURL); err != nil {
				return fmt.Errorf("print status Temporal UI line: %w", err)
			}
		}
	}
	if heartbeatLine != "" {
		if len(requestEntries) > 0 || len(entries) > 0 {
			fmt.Fprintln(os.Stdout)
		}
		if _, err := fmt.Fprintln(os.Stdout, heartbeatLine); err != nil {
			return fmt.Errorf("print worker heartbeat line: %w", err)
		}
	}
	if routeLine != "" {
		if len(requestEntries) > 0 || len(entries) > 0 {
			fmt.Fprintln(os.Stdout)
		}
		if _, err := fmt.Fprintln(os.Stdout, routeLine); err != nil {
			return fmt.Errorf("print worker route line: %w", err)
		}
	}
	if len(roleLines) > 0 {
		if len(requestEntries) > 0 || len(entries) > 0 || routeLine != "" {
			fmt.Fprintln(os.Stdout)
		}
		for _, line := range roleLines {
			if _, err := fmt.Fprintln(os.Stdout, line); err != nil {
				return fmt.Errorf("print roles: line: %w", err)
			}
		}
	}
	return nil
}
