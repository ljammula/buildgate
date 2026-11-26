package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"buildgate/internal/modelrole"
	"buildgate/internal/progress"
	"buildgate/internal/release"
	"buildgate/internal/request"
	"buildgate/internal/requestdriver"
	"buildgate/internal/run"
	"buildgate/internal/sanitize"
	"buildgate/internal/sessionconfig"
)

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
	// TemporalUIURL's own doc comment) — empty for a run with no recorded
	// workflow, or one whose Temporal server isn't on the default port.
	TemporalUIURL string `json:"temporal_ui_url,omitempty"`
	// Baseline is the verify command's result on the base commit, before
	// the build (run.BaselineVerify.Summary): "passed", or "failed" with
	// the failing test named. Empty until the run has got that far.
	Baseline string `json:"baseline,omitempty"`

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
	// Kind is "memory" for a request `factoryd memory propose` created,
	// empty for every other. A label only.
	Kind string `json:"kind,omitempty"`
	// AutoResumed is set while the request is in the step the factory
	// resumed by itself, of either kind (after the provider's limit reset,
	// naming the provider's time, or after a step that never got to the
	// work, naming its cause: request.Request.EnteredByAutoResume). Empty
	// otherwise.
	AutoResumed string `json:"auto_resumed,omitempty"`
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
	// -- the default, shown when PRState is still "" -- "ready", "approved",
	// "merged", or "closed"), followed by ",draft" (the current ticket of a building request) or ",safe-to-merge" (the lowest open ticket of a building request once a later one builds, its advice rounds done; in pr_review, the lowest open ticket whose last merge-readiness check passed (a draft being its only blocker is fine), whose base is not another ticket's branch, with no restack or advice round in flight on the request), then ",ready-to-merge" when the poll's last
	// ready-to-merge check passed (Ticket.MergeReadiness). Empty when this request has no ticket with a PR yet
	// (every state before building/pr_review, or a ticket not yet
	// opened).
	Tickets string `json:"tickets,omitempty"`
	// Wait is why the request is waiting rather than running (request.WaitFor):
	// no worker, every job slot taken, the repository held by another build,
	// a model-host or sidecar slot. Absent while it runs or waits on a human.
	Wait *request.Wait `json:"wait,omitempty"`
	// InState is how long the request has been in State (since EnteredAt):
	// what tells a request that is stuck from one that is merely old.
	InState string `json:"in_state,omitempty"`
}

// requestTicketPRSummary renders requestStatusEntry's own Tickets field:
// one "<index>:<pr_state>" entry per ticket that has a PR open, in ticket
// order, space-separated -- kept to one line so `factoryd status`'s
// existing per-request row (see the -json/table renderers below) only ever
// grows by one field, never a second line.
func requestTicketPRSummary(r *request.Request) string {
	var parts []string
	lowest := lowestUnmergedBelowCurrent(r)
	if r.State == request.StatePRReview {
		lowest = lowestOpenTicket(r, r.TicketCount+1)
	}
	for _, t := range r.Tickets {
		if t.PRURL == "" {
			continue
		}
		state := t.ShownPRState()
		if state == "" {
			state = "open"
		}
		// The poll's last ready-to-merge check, when it passed.
		if t.MergeReadiness != nil && t.MergeReadiness.Ready {
			state += ",ready-to-merge"
		}
		state += ticketMergeMark(r, t, lowest)
		parts = append(parts, fmt.Sprintf("%d:%s", t.Index, state))
	}
	return strings.Join(parts, " ")
}

// lowestUnmergedBelowCurrent is the index of the lowest ticket below the
// current one whose PR is open: its base is the default branch or a merged
// ticket's branch, so it is the only one that is safe to merge now. A higher
// one is stacked on a ticket that has not merged. 0 when there is none.
func lowestUnmergedBelowCurrent(r *request.Request) int {
	return lowestOpenTicket(r, r.TicketIndex)
}

// lowestOpenTicket is the index of the lowest ticket below limit whose PR is
// open (not merged or closed); 0 when there is none. It reads the state status
// shows (Ticket.ShownPRState, an observation made while a later ticket builds
// included), so it is status's own: the driver decides on the recorded state
// (request.Request.LowestOpenTicket) and never on an observation.
func lowestOpenTicket(r *request.Request, limit int) int {
	for _, t := range r.Tickets {
		if t.Index < limit && t.PRURL != "" && t.ShownPRState() != "merged" && t.ShownPRState() != "closed" {
			return t.Index
		}
	}
	return 0
}

// ticketMergeMark is what a ticket PR adds to its state in the summary of a
// building or pr_review request: ",draft" for the current ticket of a building
// request (its advice rounds may still push to it), ",safe-to-merge" for the
// lowest open ticket when merging it drops nothing (see the two functions
// below; a PR whose base is another ticket's branch is not marked), and nothing for a merged or closed PR or in any other state.
func ticketMergeMark(r *request.Request, t request.Ticket, lowest int) string {
	if r.State == request.StatePRReview {
		return prReviewTicketMark(r, t, lowest)
	}
	if r.State != request.StateBuilding || t.ShownPRState() == "merged" || t.ShownPRState() == "closed" {
		return ""
	}
	if t.Index == r.TicketIndex {
		return ",draft"
	}
	if t.Index == lowest && !r.StackedOnAnotherTicket(&t, t.ShownPRBase()) {
		return ",safe-to-merge"
	}
	return ""
}

// prReviewTicketMark is the ",safe-to-merge" mark of a pr_review request's
// ticket, or "". It marks only the lowest ticket whose PR is open (lowest),
// and only when the record shows nothing that could still push to it or hold
// it: the last merge-readiness check passed, or its only blocker is the draft state (the operator marks it ready before merging; USAGE_REFERENCE has the rule) (it is cleared and saved before a
// review or CI round's build starts, and every other blocker, such as a stacked PR, open
// threads or failing checks, fails it), its base is not another ticket's
// branch (a PR approved while stacked has PR state "approved" but is not yet
// retargeted), and no ticket of the request has a restack, a held restack or
// an advice round in flight. Anything since the last poll (a new thread, a push,
// a check result) is not in the record, so it is not reflected.
func prReviewTicketMark(r *request.Request, t request.Ticket, lowest int) string {
	if t.Index != lowest || t.MergeReadiness == nil || !(t.MergeReadiness.Ready || t.MergeReadiness.OnlyDraftBlocks()) || r.StackedOnAnotherTicket(&t, t.PRBase) {
		return ""
	}
	for _, other := range r.Tickets {
		if other.Restack != nil || other.RestackHold != nil || other.Advice != nil {
			return ""
		}
	}
	return ",safe-to-merge"
}

// requestRowSuffix is what follows a request's row in the table: its
// tickets' pull request states, and its kind when it has one.
func requestRowSuffix(r requestStatusEntry) string {
	suffix := ""
	if r.Tickets != "" {
		suffix += "  tickets: " + r.Tickets
	}
	if r.Kind != "" {
		suffix += "  [" + r.Kind + "]"
	}
	if r.AutoResumed != "" {
		suffix += "  [" + r.AutoResumed + "]"
	}
	return suffix
}

// buildRequestStatusEntry converts a loaded request record into its
// display form, with now used to compute Age from SubmittedAt and
// wait what request.WaitFor reports for r (nil when none).
func buildRequestStatusEntry(r *request.Request, wait *request.Wait, now time.Time) requestStatusEntry {
	entry := requestStatusEntry{
		ID:      r.ID,
		Project: r.Project,
		State:   string(r.State),
		Error:   r.Error,
		Tickets: requestTicketPRSummary(r),
	}
	if r.Source.Kind == request.SourceMemory {
		entry.Kind = string(request.SourceMemory)
	}
	if auto := r.EnteredByAutoResume(); auto != nil {
		entry.AutoResumed = "resumed automatically " + strings.TrimPrefix(auto.Note(), "automatic, ")
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
	if since, ok := r.InStateSince(); ok {
		entry.InState = now.Sub(since).Round(time.Second).String()
	}
	entry.Wait = wait
	return entry
}

// buildRequestStatusEntries converts every request under dataDir into its
// display form, newest-submitted last reversed to oldest-first -- same
// "most in need of attention first" ordering statusMain's own run/queue
// table uses (buildStatusEntries' own statusCreatedBefore), applied here
// to request.List's oldest-first result.
//
// waits decides each request's wait (request.WaitContext.For).
func buildRequestStatusEntries(requests []*request.Request, waits *request.WaitContext, project string, now time.Time) []requestStatusEntry {
	entries := make([]requestStatusEntry, 0, len(requests))
	for i := len(requests) - 1; i >= 0; i-- {
		r := requests[i]
		if project != "" && r.Project != project {
			continue
		}
		entries = append(entries, buildRequestStatusEntry(r, waits.For(r), now))
	}
	return entries
}

// statusCostMicroUSD sums the relay spend recorded across every attempt --
// the only durable spend/usage evidence a run carries (see
// Attempt.RelayConsumedCostMicroUSD's own doc comment). Zero when the run
// made no metered model request. partial is true when any attempt's total
// holds the meter's estimate for a model request that ended without a usage
// report -- see Attempt.RelaySpendPartial's own doc comment.
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
			entry.CostUSD += " (" + spendEstimateNote + ")"
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
	entry.Baseline = sanitize.Line(r.BaselineVerify.Summary())
	return entry
}

// statusRunDetailLines is what a run's row is followed by in the plain
// table: its baseline verify result and its Temporal Web UI link.
func statusRunDetailLines(e statusEntry) []string {
	var lines []string
	if e.Baseline != "" {
		lines = append(lines, "baseline verify: "+e.Baseline)
	}
	if e.TemporalUIURL != "" {
		lines = append(lines, e.TemporalUIURL)
	}
	return lines
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
	for _, role := range []modelrole.Role{modelrole.RolePlanning, modelrole.RoleExecution, modelrole.RoleReview, modelrole.RoleChallenger} {
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

// profileConfigArg is the " -config <profile>" a printed command carries so it
// reads the data dir of the session config in effect (configFlag, else the one
// resolved): the profile's name, or the quoted path of a config that is not a
// profile. Empty when no config resolves.
func profileConfigArg(configFlag string) string {
	_, path := statusProfile(configFlag)
	return quickstartConfigArg(path)
}

// statusProfileLine is `factoryd status`'s first line, so an operator or
// agent sees which profile and data dir every following row comes from.
func statusProfileLine(name, path, dataDir string) string {
	if name == "" {
		return fmt.Sprintf("profile: none (no session config) · data dir %s", dataDir)
	}
	return fmt.Sprintf("profile: %s (%s) · data dir %s", name, path, dataDir)
}

// statusFlags are `factoryd status`'s flags.
type statusFlags struct {
	set         *flag.FlagSet
	dataDir     *string
	project     *string
	state       *string
	configPath  *string
	n           *int
	jsonOutput  *bool
	all         *bool
	verbose     *bool
	allProfiles *bool
}

func newStatusFlags() statusFlags {
	flags := flag.NewFlagSet("status", flag.ContinueOnError)
	f := statusFlags{set: flags}
	f.dataDir = flags.String("data-dir", "data", "internal: the data dir to use in place of the session config's data_dir")
	f.project = flags.String("project", "", "only show runs and requests of this project id, which is the name of the repository's root directory")
	f.state = flags.String("state", "", "only show runs in this state (e.g. ready, slice_running, verifying, accepted, halted, quarantined), finished ones included")
	f.n = flags.Int("n", 20, "maximum number of runs to show")
	f.jsonOutput = flags.Bool("json", false, "print machine-readable JSON instead of a table: every request, finished ones included, with whole reasons")
	f.configPath = flags.String("config", "", "session config path or profile name, for the data dir and the roles summary; empty: the active profile, else the first of ~/.config/factoryd/config.yml and ~/.factory/config.yml that exists")
	f.all = flags.Bool("all", false, "also list finished requests (done, closed, cancelled) and the finished runs of no open request, which the table leaves out")
	f.verbose = flags.Bool("v", false, "print whole reasons instead of cutting them short, each run's baseline result and Temporal link, and where the data dir came from")
	f.allProfiles = flags.Bool("all-profiles", false, "list the open requests of every profile's data dir in one table, with no runs; add -all for finished ones")
	finishFlags(flags)
	return f
}

func statusMain(dp *deps, args []string) error {
	f := newStatusFlags()
	flags, dataDir, project, state, configPath, n, jsonOutput := f.set, f.dataDir, f.project, f.state, f.configPath, f.n, f.jsonOutput
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *f.allProfiles {
		return statusAllProfiles(os.Stdout, os.Stderr, f, time.Now())
	}
	// *configPath, not "": status now has its own -config flag (for the
	// roles: summary below), and an operator who names one almost
	// certainly also wants -data-dir resolved from it, the same
	// "-config wins" precedence every other -config-bearing command
	// already gives that flag (applySessionConfig, doctorMain, ...).
	// The line naming the data dir's source is -v's: the profile line
	// below already names the profile and the data dir.
	if err := resolveDataDir(flags, dataDir, *configPath, *f.verbose); err != nil {
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
	requestEntries := buildRequestStatusEntries(requests, request.NewWaitContext(*dataDir, requests, now), *project, now)

	// worker liveness. heartbeatLine is "" whenever worker has
	// never run against this data dir, or its heartbeat is still fresh --
	// see workerHeartbeatStatusLine's own doc comment. The stale-request
	// desktop notification (notifyWorkerStale) runs unconditionally,
	// not only in the human-readable branch below, so `factoryd status
	// -json` (a script or launchd-driven poll) still gets the same
	// one-time alert a human running the plain command would.
	heartbeatLine := workerHeartbeatStatusLine(*dataDir, now)
	notifyWorkerStale(dp, *dataDir, requests, now, heartbeatLine)
	// Route visibility (2026-09-25): which subscription the live worker
	// bills to. Both "" whenever heartbeatLine is non-empty (worker
	// isn't running) or the heartbeat predates these fields -- see
	// workerRoute's own doc comment.
	routeMode, routeModel := workerRoute(*dataDir, now)
	routeLine := workerRouteStatusLine(*dataDir, now)
	if drain := workerDrainStatusLine(*dataDir, now); drain != "" {
		routeLine = strings.TrimSuffix(drain+"\n"+routeLine, "\n")
	}
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
		entries := buildStatusEntries(runs, *project, *state, *n, now)
		applyProgressStatus(entries, runs, *dataDir, now)
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

	// The table is what is open: a finished request, and a finished run of
	// no open request, are left out unless -all (or, for runs, -state)
	// asks for them, and counted in a closing line.
	shownRequests, shownRuns := requestEntries, runs
	if !*f.all {
		shownRequests = openRequestEntries(requestEntries)
		if *state == "" {
			shownRuns = runsWorthShowing(runs, requests)
		}
	}
	entries := buildStatusEntries(shownRuns, *project, *state, *n, now)
	applyProgressStatus(entries, runs, *dataDir, now)

	fmt.Fprintln(os.Stdout, statusProfileLine(profileName, profilePath, *dataDir))
	cut, err := printRequestStatusRows(os.Stdout, shownRequests, now, *f.verbose)
	if err != nil {
		return err
	}
	if len(shownRequests) > 0 && len(entries) > 0 {
		fmt.Fprintln(os.Stdout)
	}
	runsCut, err := printRunStatusRows(os.Stdout, entries, *f.verbose)
	if err != nil {
		return err
	}
	anyRows := len(shownRequests) > 0 || len(entries) > 0
	hiddenRequests := len(requestEntries) - len(shownRequests)
	// Counted over the same -project and -state the rows passed: finished
	// runs left out, and rows past -n.
	listable := countStatusRuns(shownRuns, *project, *state)
	hiddenRuns := countStatusRuns(runs, *project, *state) - listable
	printStatusNotes(os.Stdout, anyRows, hiddenRequests, hiddenRuns, listable-len(entries), *n, cut || runsCut)
	return printStatusFooter(true, workerHeartbeatLine(*dataDir, now, humanAge), routeLine, roleLines)
}

// openRequestEntries are the entries of requests that can still move: not
// done, closed or cancelled (request.State.Finished).
func openRequestEntries(entries []requestStatusEntry) []requestStatusEntry {
	open := make([]requestStatusEntry, 0, len(entries))
	for _, e := range entries {
		if !request.State(e.State).Finished() {
			open = append(open, e)
		}
	}
	return open
}

// runsWorthShowing are the runs the table lists without -all: one still
// running, and every run of a request that is not finished (a halted or
// quarantined request's runs say why).
func runsWorthShowing(runs []*run.Run, requests []*request.Request) []*run.Run {
	open := map[string]bool{}
	for _, r := range requests {
		if !r.State.Finished() {
			open[r.ID] = true
		}
	}
	var shown []*run.Run
	for _, r := range runs {
		if !runTerminalConfirmed(r) || (r.RequestID != "" && open[r.RequestID]) {
			shown = append(shown, r)
		}
	}
	return shown
}

// countStatusRuns counts the runs buildStatusEntries lists before its cap:
// those of project and in state, each filter off when "".
func countStatusRuns(runs []*run.Run, project, state string) int {
	n := 0
	for _, r := range runs {
		if (project == "" || release.ProjectOf(r) == project) && (state == "" || string(r.State) == state) {
			n++
		}
	}
	return n
}

// printStatusNotes prints what the table left out or cut short, and how to
// see it. With no rows it says so, since an empty table reads as a failure.
// pastLimit is how many runs the table would list but for -n, which is limit.
func printStatusNotes(w io.Writer, anyRows bool, hiddenRequests, hiddenRuns, pastLimit, limit int, cut bool) {
	if !anyRows {
		fmt.Fprintln(w, "No open requests or runs.")
	}
	if hiddenRequests > 0 || hiddenRuns > 0 {
		fmt.Fprintf(w, "%d finished requests and %d finished runs not shown: factoryd status -all\n", hiddenRequests, hiddenRuns)
	}
	if pastLimit > 0 {
		fmt.Fprintf(w, "%d more runs past the newest %d: factoryd status -n %d\n", pastLimit, limit, limit+pastLimit)
	}
	if cut {
		fmt.Fprintln(w, "A reason ending in … is cut short: factoryd watch -no-follow <id> or factoryd status -v prints it whole.")
	}
}

// statusDetailWidth is how many characters of a reason the table prints
// without -v. A pull request URL and a line naming a command are never cut.
const statusDetailWidth = 80

// cutStatusDetail returns text cut to statusDetailWidth characters with a
// closing "…", and whether it was cut. Unchanged when whole is set.
func cutStatusDetail(text string, whole bool) (string, bool) {
	runes := []rune(text)
	if whole || len(runes) <= statusDetailWidth {
		return text, false
	}
	return strings.TrimRight(string(runes[:statusDetailWidth-1]), " ") + "…", true
}

// compactAge renders a duration string of a status entry (Go's "591h35m52s")
// as the table prints it: its largest unit alone ("24d", "3h", "5m", "40s").
// A string that is not a duration is returned as it is; "" is "-".
func compactAge(d string) string {
	if d == "" {
		return "-"
	}
	parsed, err := time.ParseDuration(d)
	if err != nil {
		return d
	}
	return humanAge(parsed)
}

// The header rows of the request table, the run table and -all-profiles'.
const (
	requestStatusHeader = "REQUEST\tPROJECT\tSTATE\tAGE\tIN STATE\tTICKET\tDETAIL"
	runStatusHeader     = "RUN\tPROJECT\tSTATE\tTIME\tCOST\tDETAIL"
)

func newStatusTable(w io.Writer) *tabwriter.Writer {
	return tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
}

// requestStatusCells are one request's cells after its id: project, state,
// age since submitted, time in this state, ticket, and the PR, reason or
// wait with the row's suffix. cut reports a reason cut short (whole unset).
func requestStatusCells(r requestStatusEntry, now time.Time, whole bool) (cells string, cut bool) {
	ticketOrDash := r.Ticket
	if ticketOrDash == "" {
		ticketOrDash = "-"
	}
	// A ticket's PR URL, once it has one, is more actionable to show
	// than the request's own Error (which is empty in building/
	// pr_review anyway -- those are non-terminal states) -- the same
	// "PR URL takes priority over a reason" rule buildStatusEntry's
	// own run rendering already follows.
	detail := r.PullRequestURL
	if detail == "" {
		// Untrusted error text: one line, so it can't split this row.
		detail, cut = cutStatusDetail(sanitize.Line(r.Error), whole)
	}
	if detail == "" {
		detail = "-"
	}
	stateCol := r.State
	if r.AwaitingPullRequest {
		// Calm, distinct label instead of an alarming "halted" plus a
		// retry hint: the work is accepted, only the PR is missing.
		stateCol = "accepted*"
		detail, cut = r.Label, false
	} else if r.Wait != nil {
		// A request that waits looked identical to one that runs: its
		// state starred plus the sentence naming the wait, the same
		// "asterisk marks a state this factory annotates" convention
		// accepted* above uses.
		stateCol = r.State + "*"
		detail, cut = cutStatusDetail(r.Wait.Sentence(now), whole)
	}
	cells = fmt.Sprintf("%s\t%s\t%s\t%s\t%s\t%s%s", r.Project, stateCol, compactAge(r.Age), compactAge(r.InState), ticketOrDash, detail, requestRowSuffix(r))
	return cells, cut
}

// printRequestStatusRows prints the request table under its header: one row
// per request with its full id (any unique part of which the other commands
// take). Nothing for no requests. cut reports a reason cut short, which
// whole (-v) prevents.
func printRequestStatusRows(w io.Writer, requestEntries []requestStatusEntry, now time.Time, whole bool) (cut bool, err error) {
	if len(requestEntries) == 0 {
		return false, nil
	}
	tw := newStatusTable(w)
	fmt.Fprintln(tw, requestStatusHeader)
	for _, r := range requestEntries {
		cells, rowCut := requestStatusCells(r, now, whole)
		cut = cut || rowCut
		fmt.Fprintf(tw, "%s\t%s\n", r.ID, cells)
	}
	if err := tw.Flush(); err != nil {
		return cut, fmt.Errorf("print request status rows: %w", err)
	}
	return cut, nil
}

// runIDColumn is a run's id as the table prints it: its ticket, then the
// launch suffix (time and process) that tells one build of the ticket from
// another, so two runs of a request never print alike.
func runIDColumn(e statusEntry) string {
	if suffix, ok := strings.CutPrefix(e.ID, e.Ticket+"-"); ok && e.Ticket != "" {
		return e.Ticket + " " + suffix
	}
	return e.ID
}

// printRunStatusRows prints the run table under its header: one row per run
// (its ticket and launch, its project, state, time, cost and PR or reason).
// With whole (-v) no reason is cut and each row is followed by its detail
// lines. Nothing for no runs. cut reports a reason cut short.
func printRunStatusRows(w io.Writer, entries []statusEntry, whole bool) (cut bool, err error) {
	if len(entries) == 0 {
		return false, nil
	}
	// The table is laid out in memory, then printed a row at a time with
	// the row's detail lines after it: a detail line written into the
	// table would end the block of rows the columns are aligned over.
	var table bytes.Buffer
	tw := newStatusTable(&table)
	fmt.Fprintln(tw, runStatusHeader)
	for _, e := range entries {
		project := e.Project
		if e.Repository != "" && e.Repository != e.Project {
			project += " (repository " + e.Repository + ")"
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
		detail := e.PullRequestURL
		if detail == "" {
			reason := sanitize.Line(e.Reason)
			if e.nonTerminalRun {
				reason = statusProgressColumn(e)
			}
			var rowCut bool
			detail, rowCut = cutStatusDetail(reason, whole)
			cut = cut || rowCut
		}
		if detail == "" {
			detail = "-"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", runIDColumn(e), project, e.State, compactAge(elapsedOrDuration), cost, detail)
	}
	if err := tw.Flush(); err != nil {
		return cut, fmt.Errorf("print run status rows: %w", err)
	}
	for i, row := range strings.Split(strings.TrimSuffix(table.String(), "\n"), "\n") {
		if _, err := fmt.Fprintln(w, row); err != nil {
			return cut, fmt.Errorf("print run status rows: %w", err)
		}
		if !whole || i == 0 {
			continue
		}
		for _, line := range statusRunDetailLines(entries[i-1]) {
			fmt.Fprintf(w, "  %s\n", line)
		}
	}
	return cut, nil
}

// statusAllProfiles implements `factoryd status -all-profiles`: the open
// requests (every request with -all) of each distinct data dir among the
// profiles, the dirs `factoryd inbox` reads, in one table whose first column
// names the profile. It lists no runs and starts nothing.
func statusAllProfiles(w, warn io.Writer, f statusFlags, now time.Time) error {
	for _, name := range []string{"data-dir", "config", "json", "state", "n"} {
		if flagWasSet(f.set, name) {
			return fmt.Errorf("-all-profiles lists every profile's requests and cannot be combined with -%s", name)
		}
	}
	dirs, label, err := inboxDataDirs(warn, "")
	if err != nil {
		return err
	}
	tw := newStatusTable(w)
	fmt.Fprintln(tw, "PROFILE\t"+requestStatusHeader)
	rows, hidden, cut := 0, 0, false
	for _, dir := range dirs {
		requests, err := request.List(dir)
		if err != nil {
			return fmt.Errorf("load requests from %q: %w", dir, err)
		}
		entries := buildRequestStatusEntries(requests, request.NewWaitContext(dir, requests, now), *f.project, now)
		shown := entries
		if !*f.all {
			shown = openRequestEntries(entries)
		}
		hidden += len(entries) - len(shown)
		for _, r := range shown {
			cells, rowCut := requestStatusCells(r, now, *f.verbose)
			cut = cut || rowCut
			fmt.Fprintf(tw, "%s\t%s\t%s\n", label[dir], r.ID, cells)
			rows++
		}
	}
	if rows > 0 {
		if err := tw.Flush(); err != nil {
			return fmt.Errorf("print request status rows: %w", err)
		}
	} else {
		fmt.Fprintf(w, "No open requests in %d data dirs.\n", len(dirs))
	}
	if hidden > 0 {
		fmt.Fprintf(w, "%d finished requests not shown: factoryd status -all-profiles -all\n", hidden)
	}
	if cut {
		fmt.Fprintln(w, "A reason ending in … is cut short: factoryd status -all-profiles -v prints it whole.")
	}
	return nil
}

// printStatusFooter prints the worker heartbeat line, the worker route line
// and the roles lines, each set off by a blank line when anything was
// printed above it. anyRows is whether a request or a run row was printed.
func printStatusFooter(anyRows bool, heartbeatLine, routeLine string, roleLines []string) error {
	if heartbeatLine != "" {
		if anyRows {
			fmt.Fprintln(os.Stdout)
		}
		if _, err := fmt.Fprintln(os.Stdout, heartbeatLine); err != nil {
			return fmt.Errorf("print worker heartbeat line: %w", err)
		}
	}
	if routeLine != "" {
		if anyRows {
			fmt.Fprintln(os.Stdout)
		}
		if _, err := fmt.Fprintln(os.Stdout, routeLine); err != nil {
			return fmt.Errorf("print worker route line: %w", err)
		}
	}
	if len(roleLines) > 0 {
		if anyRows || routeLine != "" {
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
