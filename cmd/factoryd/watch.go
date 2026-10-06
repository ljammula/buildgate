package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"buildgate/internal/progress"
	"buildgate/internal/release"
	"buildgate/internal/request"
	"buildgate/internal/requestdriver"
	"buildgate/internal/run"
	"buildgate/internal/sanitize"
	"buildgate/internal/spinner"
)

// watchLabelWidth is the fixed column width `factoryd watch` pads a
// stage/round/agent label to before printing its status text -- chosen so
// a nested worker line's own indent (2 for a round, 4 for an agent note)
// eats into the same column budget a top-level stage label gets, keeping
// every line's status text starting in the same place regardless of
// nesting depth.
const watchLabelWidth = 20

// watchPollInterval/watchRequestPollInterval are how often `factoryd
// watch` polls a run's progress file and a request's own record while
// following either. Package vars, not constants, so watch_test.go can
// shorten them rather than making a real test wait a full second.
var (
	watchPollInterval        = time.Second
	watchRequestPollInterval = 2 * time.Second
)

// relClock renders elapsed as a "MM:SS" relative-timestamp column --
// minutes are not wrapped at 60 (a run following past 99 minutes just
// gets a wider first field), so this stays monotonic and simple to read
// top to bottom.
func relClock(elapsed time.Duration) string {
	if elapsed < 0 {
		elapsed = 0
	}
	total := int(elapsed.Round(time.Second).Seconds())
	return fmt.Sprintf("%02d:%02d", total/60, total%60)
}

// eventTimestamp parses e.Ts (progress.Append's own RFC3339-with-millis
// format), falling back to fallback (the previous event's own resolved
// time, or the run's creation time for the first event) on a malformed or
// empty timestamp -- a progress line is informational only, so a bad
// timestamp must never crash rendering, only make that one line's clock
// column repeat its predecessor's.
func eventTimestamp(e progress.Event, fallback time.Time) time.Time {
	if e.Ts == "" {
		return fallback
	}
	if t, err := time.Parse("2006-01-02T15:04:05.000Z07:00", e.Ts); err == nil {
		return t
	}
	if t, err := time.Parse(time.RFC3339, e.Ts); err == nil {
		return t
	}
	return fallback
}

// eventIndentAndLabel returns how deep e nests (0 for a top-level factory
// stage, 2 for a worker round, 4 for a worker agent note) and the label
// text before padding -- a gate's label folds in its Detail (the gate
// name) since "gate" alone is meaningless on its own line.
func eventIndentAndLabel(e progress.Event) (indent int, label string) {
	switch {
	case e.Source == "worker" && e.Stage == "round":
		return 2, fmt.Sprintf("round %d/%d", e.Round, e.MaxRounds)
	case e.Source == "worker" && e.Stage == "agent":
		return 4, "agent"
	case e.Stage == "gate" && e.Detail != "":
		return 0, "gate " + e.Detail
	default:
		return 0, e.Stage
	}
}

// durKey identifies which prior "start" event a given "end" event should
// measure its own duration against: a factory stage keys on the stage
// name alone, a worker round keys on stage+round number so two different
// rounds (never actually concurrent, but never assumed not to be) don't
// clobber each other's recorded start time.
func durKey(e progress.Event) string {
	if e.Source == "worker" && e.Stage == "round" {
		return fmt.Sprintf("round:%d", e.Round)
	}
	return "stage:" + e.Stage
}

// eventText renders the status text that follows a rendered line's padded
// label. dur/haveDur is the elapsed time since this event's own matching
// "start" (see durKey), when one was seen.
// displayDetail flattens an event's detail to one printable line: worker
// details are untrusted text that can carry newlines (a heredoc in a bash
// command) or terminal control sequences, neither of which belongs in a
// transcript row.
func displayDetail(detail string) string {
	var b strings.Builder
	b.Grow(len(detail))
	lastSpace := false
	for _, r := range detail {
		if r < 0x20 || r == 0x7f {
			r = ' '
		}
		if r == ' ' {
			if lastSpace {
				continue
			}
			lastSpace = true
		} else {
			lastSpace = false
		}
		b.WriteRune(r)
	}
	return strings.TrimSpace(b.String())
}

func eventText(e progress.Event, dur time.Duration, haveDur bool) string {
	e.Detail = displayDetail(e.Detail)
	switch e.Event {
	case "start":
		if e.Detail != "" && !(e.Source == "worker" && e.Stage == "round") && e.Stage != "gate" {
			return "started (" + e.Detail + ")"
		}
		return "started"
	case "note":
		if e.Detail != "" {
			return e.Detail
		}
		return "note"
	case "end":
		text := e.Outcome
		if text == "" {
			text = "done"
		}
		// A failed worker round's own Detail already carries the
		// human-readable reason (e.g. "verify failed: go test ./..."),
		// more useful on this line than the bare "fail" outcome code --
		// and it already reads as a complete sentence, so no duration is
		// appended after it.
		if e.Source == "worker" && e.Stage == "round" && e.Outcome != "" && e.Outcome != "pass" && e.Detail != "" {
			return e.Detail
		}
		if haveDur && e.Stage != "gate" && e.Stage != "finished" {
			text = fmt.Sprintf("%s (%s)", text, dur.Round(time.Second))
		}
		return text
	default:
		if e.Detail != "" {
			return e.Detail
		}
		return e.Event
	}
}

// renderRunEvents renders a run's whole progress feed into one human line
// per event, each starting with a "MM:SS" column relative to t0 (the
// run's own CreatedAt) -- a pure function over data, with no I/O or
// terminal dependency of its own, so it is unit-testable against a
// hand-built event list.
func renderRunEvents(events []progress.Event, t0 time.Time) []string {
	starts := map[string]time.Time{}
	last := t0
	lines := make([]string, 0, len(events))
	for _, e := range events {
		ts := eventTimestamp(e, last)
		last = ts

		key := durKey(e)
		var dur time.Duration
		haveDur := false
		if e.Event == "start" {
			starts[key] = ts
		} else if start, ok := starts[key]; ok {
			dur = ts.Sub(start)
			haveDur = true
		}

		indent, label := eventIndentAndLabel(e)
		text := eventText(e, dur, haveDur)
		padded := label
		if pad := watchLabelWidth - indent - len([]rune(label)); pad > 0 {
			padded += strings.Repeat(" ", pad)
		} else {
			padded += " "
		}
		lines = append(lines, fmt.Sprintf("%s  %s%s%s", relClock(ts.Sub(t0)), strings.Repeat(" ", indent), padded, text))
	}
	return lines
}

// renderRunHeader is `factoryd watch`'s one-line header for a run id.
func renderRunHeader(r *run.Run) string {
	return fmt.Sprintf("run %s · ticket %s · project %s · state %s", r.ID, r.Ticket, release.ProjectOf(r), r.State)
}

// currentStageAndRound scans events for the most recently started factory
// stage and worker round -- what renderFooter shows as "current stage"
// while following a run live.
func currentStageAndRound(events []progress.Event) (stage string, round, maxRound int) {
	for _, e := range events {
		if e.Source == "factory" && e.Event == "start" {
			stage = e.Stage
		}
		if e.Source == "worker" && e.Stage == "round" && e.Event == "start" {
			round, maxRound = e.Round, e.MaxRounds
		}
	}
	return
}

// renderFooter renders `factoryd watch`'s single refreshed status-footer
// line while following a run: elapsed time since the run was created;
// its current stage, or "waiting: <reason>" instead while waiting is set
// and no stage has started yet (stage == ""); its current round (when
// one is in progress); a STALLED marker when stalled; and its run.State.
func renderFooter(elapsed time.Duration, stage, waiting string, round, maxRound int, stalled bool, stalledSince time.Duration, state string) string {
	return fmt.Sprintf("⏱ %s  ·  %s", elapsed.Round(time.Second), renderFooterFacts(stage, waiting, round, maxRound, stalled, stalledSince, state))
}

// renderFooterFacts is renderFooter without its elapsed time: on a terminal
// the spinner line carries the elapsed itself, so the facts must not repeat it.
func renderFooterFacts(stage, waiting string, round, maxRound int, stalled bool, stalledSince time.Duration, state string) string {
	var parts []string
	switch {
	case stage != "":
		parts = append(parts, stage)
	case waiting != "":
		parts = append(parts, "waiting: "+waiting)
	}
	if maxRound > 0 {
		parts = append(parts, fmt.Sprintf("round %d/%d", round, maxRound))
	}
	if stalled {
		parts = append(parts, fmt.Sprintf("STALLED %dm", int(stalledSince/time.Minute)))
	}
	parts = append(parts, state)
	return strings.Join(parts, "  ·  ")
}

// recapMaxRounds returns the max_rounds a run's progress feed recorded
// for its worker rounds (the last round-stage event's own MaxRounds), or
// 0 if the feed has no round events at all (predates this feature, or was
// never written for another reason -- the progress feed is best-effort,
// see internal/progress's own doc comment).
func recapMaxRounds(dataDir, runID string) int {
	events, err := progress.Read(progress.Path(dataDir, runID))
	if err != nil {
		return 0
	}
	max := 0
	for _, e := range events {
		if e.Source == "worker" && e.Stage == "round" && e.MaxRounds > 0 {
			max = e.MaxRounds
		}
	}
	return max
}

// printRunRecap prints a short (~8 line) human recap of a run's terminal
// outcome to w: state and duration, ticket/project, diff size, gate
// results, agent evidence (rounds/tokens/cost) when present, a PR URL or
// halt/quarantine reason, and a single closing "next" command. Called
// both at the end of `factoryd watch` and from the direct execution
// path's own terminal exit (run_ticket.go), so an operator sees the same
// recap whether they watched the run live or ran it in the foreground.
// relayModelEffortLine summarizes, per distinct Attempt Kind (in
// first-appearance order), the relay worker model that Kind's attempts
// were configured with and the HIGHEST reasoning effort the LAST such
// attempt's relay actually observed on the wire (relay.Server's own
// highestReasoningEffort -- factory-observed evidence, not agent
// self-report; see run.Attempt.RelayReasoningEffort's own doc comment).
// One "<kind> <model>[ (effort <e>[, anomaly])][ (requested <t>, sent
// <e>)]" fragment per kind, joined with " · ", e.g. "build gpt-5.6-luna
// (effort medium) · spec_conformity gpt-5.6-luna (effort max)" -- exactly
// the "different phases can ask for different effort" case this field
// exists to surface. A " via <harness>" follows the model id when the attempt recorded its
// harness (Attempt.Harness): "build gpt-5.6-luna via pifork (effort medium)". The ", anomaly" suffix appears when that Kind's last
// attempt's RelayReasoningEffortAnomaly is set (some request named an
// unrecognized effort value at some point in that attempt's relay's
// life) -- a separate, sticky signal from the effort value itself, which
// never lets an anomaly mask a real, lower effort as the highest seen.
// The trailing "(requested <t>, sent <e>)" appears whenever that Kind's
// last attempt's ExpectedEffort (what Pi's own thinkingLevelMap
// translation actually turns Thinking into -- see
// run.ExpectedReasoningEffort's own doc comment) is non-empty and differs
// from the level RelayReasoningEffort actually observed -- P0e's mismatch
// hint: an agent that silently clamped a requested level (told "max",
// sent "high") would otherwise be invisible, since RelayReasoningEffort
// alone reads as a perfectly valid level with nothing to compare it
// against. Comparing against ExpectedEffort rather than Thinking directly
// matters: a model whose thinkingLevelMap legitimately renames "max" to
// "xhigh" must not be reported as a clamp just because Thinking
// ("max") differs from RelayReasoningEffort ("xhigh") -- found via
// review, the false positive this field's first version had. "" when no
// attempt named a relay worker model at all.
func relayModelEffortLine(attempts []run.Attempt) string {
	var order []string
	last := map[string]run.Attempt{}
	for _, a := range attempts {
		if a.RelayWorkerModelID == "" {
			continue
		}
		if _, seen := last[a.Kind]; !seen {
			order = append(order, a.Kind)
		}
		last[a.Kind] = a
	}
	if len(order) == 0 {
		return ""
	}
	parts := make([]string, 0, len(order))
	for _, kind := range order {
		a := last[kind]
		frag := fmt.Sprintf("%s %s", kind, a.RelayWorkerModelID)
		if a.Harness != "" {
			frag += " via " + a.Harness
		}
		if a.RelayReasoningEffort != "" {
			frag += fmt.Sprintf(" (effort %s", a.RelayReasoningEffort)
			if a.RelayReasoningEffortAnomaly {
				frag += ", anomaly"
			}
			frag += ")"
		}
		if a.ExpectedEffort != "" && a.RelayReasoningEffort != "" && a.ExpectedEffort != a.RelayReasoningEffort {
			frag += fmt.Sprintf(" (requested %s, sent %s)", a.Thinking, a.RelayReasoningEffort)
		}
		parts = append(parts, frag)
	}
	return strings.Join(parts, " · ")
}

// buildAttemptHarnessScriptsSHA256 returns the most recent "build"-kind
// attempt's HarnessScriptsSHA256, or "" when there is no build attempt or
// it staged no script (e.g. an offline -build-app-script run recorded
// before this field existed).
func buildAttemptHarnessScriptsSHA256(attempts []run.Attempt) string {
	hash := ""
	for _, a := range attempts {
		if a.Kind == "build" && a.HarnessScriptsSHA256 != "" {
			hash = a.HarnessScriptsSHA256
		}
	}
	return hash
}

// attemptSkillsLine is one "<kind> <name>, <name> (<short sha>)" fragment
// per attempt kind whose last attempt mounted operator skills, joined with
// " · ", in first-seen order; "" when no attempt had skills.
func attemptSkillsLine(attempts []run.Attempt) string {
	var order []string
	last := map[string]run.Attempt{}
	for _, a := range attempts {
		if len(a.Skills) == 0 {
			continue
		}
		if _, seen := last[a.Kind]; !seen {
			order = append(order, a.Kind)
		}
		last[a.Kind] = a
	}
	parts := make([]string, 0, len(order))
	for _, kind := range order {
		a := last[kind]
		parts = append(parts, fmt.Sprintf("%s %s (%s)", kind, strings.Join(a.Skills, ", "), shortSHA256(a.SkillsSHA256)))
	}
	return strings.Join(parts, " · ")
}

// lastRepoSkills is the latest harness attempt's scan of the target repo's
// own project skills, which the harness may load alongside the operator's.
// That attempt's scan wins even when empty (omitted in JSON), so a skill
// the worker deleted stops showing.
func lastRepoSkills(attempts []run.Attempt) []string {
	for i := len(attempts) - 1; i >= 0; i-- {
		if attempts[i].Harness != "" {
			return attempts[i].RepoSkills
		}
	}
	return nil
}

// shortSHA256 truncates a hex sha256 digest to 12 characters for compact
// display, matching buildVersion's own vcs.revision truncation (main.go)
// -- enough to distinguish harness script sets at a glance without
// printing the full 64-character digest.
func shortSHA256(hash string) string {
	if len(hash) > 12 {
		return hash[:12]
	}
	return hash
}

func printRunRecap(w io.Writer, r *run.Run, dataDir string) {
	duration := "?"
	if created, err := time.Parse(time.RFC3339, r.CreatedAt); err == nil {
		// UpdatedAt is stamped by every save, including the terminal one,
		// so it is the run's end for any terminal state; the last
		// attempt's FinishedAt is only a fallback, and an infrastructure
		// failure can leave it zero-valued (found live: a halted run
		// printed a negative multi-century duration).
		end := time.Now()
		if updated, err := time.Parse(time.RFC3339, r.UpdatedAt); err == nil && !updated.Before(created) {
			end = updated
		} else if completed, ok := statusCompletionTime(r); ok && !completed.Before(created) {
			end = completed
		}
		duration = end.Sub(created).Round(time.Second).String()
	}
	fmt.Fprintf(w, "run %s %s in %s\n", r.ID, strings.ToUpper(string(r.State)), duration)
	fmt.Fprintf(w, "  ticket %s · project %s\n", r.Ticket, release.ProjectOf(r))

	// r.FactorydVersion is the version of the factoryd binary that most
	// recently persisted this record (run.Run.Persist) -- "last writer",
	// not necessarily the one that started the run -- and empty for a run
	// recorded before this field existed (M4-K1).
	if r.FactorydVersion != "" {
		fmt.Fprintf(w, "  factoryd %s\n", r.FactorydVersion)
	}
	if hash := buildAttemptHarnessScriptsSHA256(r.Attempts); hash != "" {
		fmt.Fprintf(w, "  harness scripts: %s\n", shortSHA256(hash))
	}
	if line := attemptSkillsLine(r.Attempts); line != "" {
		fmt.Fprintf(w, "  skills: %s\n", line)
	}
	if repo := lastRepoSkills(r.Attempts); len(repo) > 0 {
		fmt.Fprintf(w, "  repo skills: %s\n", strings.Join(repo, ", "))
	}

	if r.DiffStat != nil {
		fmt.Fprintf(w, "  changed: %d files (+%d/-%d)\n", r.DiffStat.FilesChanged, r.DiffStat.Insertions, r.DiffStat.Deletions)
	}

	if len(r.GateResults) > 0 {
		parts := make([]string, 0, len(r.GateResults))
		for _, g := range r.GateResults {
			outcome := "ok"
			if !g.Passed {
				outcome = "failed"
			}
			parts = append(parts, fmt.Sprintf("%s %s", g.Check, outcome))
		}
		fmt.Fprintf(w, "  gates: %s\n", strings.Join(parts, " · "))
	}

	if r.AgentEvidence != nil {
		roundsLabel := fmt.Sprintf("%d", len(r.AgentEvidence.Rounds))
		if max := recapMaxRounds(dataDir, r.ID); max > 0 {
			roundsLabel = fmt.Sprintf("%d/%d", len(r.AgentEvidence.Rounds), max)
		}
		var inTok, outTok, costMicroUSD int64
		for _, a := range r.Attempts {
			inTok += a.RelayConsumedInputTokens
			outTok += a.RelayConsumedOutputTokens
			costMicroUSD += a.RelayConsumedCostMicroUSD
		}
		costSuffix := ""
		if run.SubscriptionBilled(r.Attempts) {
			costSuffix = subscriptionCostSuffix
		}
		fmt.Fprintf(w, "  rounds: %s · tokens %d/%d · $%.4f%s\n", roundsLabel, inTok, outTok, float64(costMicroUSD)/1e6, costSuffix)
	}

	if line := relayModelEffortLine(r.Attempts); line != "" {
		fmt.Fprintf(w, "  model: %s\n", line)
	}

	if ev := r.HarnessEval; ev != nil {
		if ev.ModelID == "" {
			fmt.Fprintf(w, "  harness: %s\n", ev.Harness)
		} else {
			fmt.Fprintf(w, "  harness: %s · model: %s\n", ev.Harness, ev.ModelID)
		}
	}

	if r.PullRequestURL != "" {
		fmt.Fprintf(w, "  PR: %s\n", r.PullRequestURL)
	}

	if r.State == run.StateHalted || r.State == run.StateQuarantined {
		if reason := requestdriver.StatusReason(r); reason != "" {
			fmt.Fprintf(w, "  reason: %s\n", sanitize.Line(reason))
		}
		fmt.Fprintf(w, "  logs: %s\n", run.Dir(dataDir, r.ID))
	}

	owningRequest := findOwningRequest(dataDir, r.ID)
	switch {
	case r.State != run.StateAccepted && owningRequest != "":
		// The owning request's own NextAction() (e.g. Follow-up B's
		// spec_conformity-specific "reject -to spec" wording), not a
		// hardcoded "factoryd retry" -- watching a run this request owns
		// must never advise something the request's own Detail line, right
		// above, contradicts. Falls back to the plain retry hint when the
		// request record can't be loaded or names no next action (a
		// building/reviewing state, where the factory is still working).
		next := fmt.Sprintf("factoryd retry %s", owningRequest)
		if owningReq, err := request.Load(dataDir, owningRequest); err == nil {
			if action := owningReq.NextAction(); action != "" {
				next = action
			}
		}
		fmt.Fprintf(w, "next: %s\n", next)
	case r.PullRequestURL != "":
		fmt.Fprintf(w, "next: review %s\n", r.PullRequestURL)
	}
}

// newWatchFlags builds `factoryd watch`'s FlagSet in isolation from
// parsing, matching newStatusFlags' own reasoning (status.go) so
// USAGE.md's doc-vs-flag drift test can enumerate its real flags without
// executing the command.
func newWatchFlags() (flags *flag.FlagSet, dataDir, configPath *string, noFollow *bool) {
	flags = flag.NewFlagSet("watch", flag.ContinueOnError)
	dataDir = flags.String("data-dir", "data", "directory containing durable run/request records")
	configPath = flags.String("config", "", "session config path (for -data-dir resolution); empty searches the default paths")
	noFollow = flags.Bool("no-follow", false, "print what exists now and exit, instead of following until the run/request finishes")
	plainFlagUsage(flags)
	return
}

func watchMain(args []string) error {
	flags, dataDir, configPath, noFollow := newWatchFlags()
	if err := flags.Parse(args); err != nil {
		return err
	}
	if err := resolveDataDirFromSessionConfig(flags, dataDir, *configPath); err != nil {
		return err
	}

	positional := flags.Args()
	if len(positional) != 1 {
		flags.Usage()
		return fmt.Errorf("usage: factoryd watch [flags] <run-id-or-request-id>")
	}
	id := positional[0]

	if isRequestID(*dataDir, id) {
		return watchRequest(os.Stdout, *dataDir, id, *noFollow)
	}
	return watchRun(os.Stdout, *dataDir, id, *noFollow)
}

// isRequestID reports whether id names a request record in dataDir, so
// `factoryd watch` knows which of the two id kinds it was given. Request
// ids used to all start with "req-" and this was a prefix check, but they
// are now readable slugs of the request text ("goal-add-a-...", see
// submit's own id derivation) -- found live 2026-09-25 driving the
// buildgate operator skill from Codex: `watch <request-id>` looked for a
// run record of that name and failed for every current request.
func isRequestID(dataDir, id string) bool {
	_, err := os.Stat(request.Path(dataDir, id))
	return err == nil
}

// watchRun implements `factoryd watch <run-id>`: print the header, every
// progress line seen so far, then (unless noFollow) keep polling
// progress.ReadFrom and refreshing a footer until either a "finished"
// progress line arrives or the run's own record reaches a confirmed
// terminal state -- whichever is noticed first, since the progress feed
// is best-effort and can in principle lag or lose its final line. Exits
// (returns nil/an error, mapped to a process exit code by realMain/main)
// 0 for accepted, 1 for halted/quarantined/anything else.
func watchRun(out *os.File, dataDir, runID string, noFollow bool) error {
	r, err := run.Load(dataDir, runID)
	if err != nil {
		return fmt.Errorf("load run %s: %w", runID, err)
	}
	fmt.Fprintln(out, renderRunHeader(r))

	created, err := time.Parse(time.RFC3339, r.CreatedAt)
	if err != nil {
		created = time.Now()
	}

	path := progress.Path(dataDir, runID)
	events, offset, err := progress.ReadFrom(path, 0)
	if err != nil {
		return fmt.Errorf("read progress feed: %w", err)
	}
	parsed := parseProgressLines(events)
	for _, line := range renderRunEvents(parsed, created) {
		fmt.Fprintln(out, line)
	}
	finished := runHasFinishedEvent(parsed)

	if noFollow || finished || runTerminalConfirmed(r) {
		printRunRecap(out, r, dataDir)
		return watchExitError(r)
	}

	// On a terminal the footer is a spinner line that keeps animating
	// between polls; elsewhere it is a plain line printed when it changes.
	terminal := spinner.IsTerminal(out)
	sp := spinner.New(out, terminal)
	defer sp.Stop("")
	lastFooter := ""
	for {
		time.Sleep(watchPollInterval)

		var newLines []string
		newLines, offset, err = progress.ReadFrom(path, offset)
		if err != nil {
			return fmt.Errorf("read progress feed: %w", err)
		}
		if len(newLines) > 0 {
			newEvents := parseProgressLines(newLines)
			// Rendered against the FULL event list so far (not just
			// newEvents) so duration lookups (durKey's matching "start")
			// still resolve across this poll boundary -- renderRunEvents
			// recomputes every line's own relative clock on each call, so
			// only the newly-appended lines are actually printed below.
			parsed = append(parsed, newEvents...)
			allLines := renderRunEvents(parsed, created)
			for _, line := range allLines[len(allLines)-len(newEvents):] {
				sp.Println(line)
			}
			if runHasFinishedEvent(newEvents) {
				finished = true
			}
			lastFooter = ""
		}

		r, err = run.Load(dataDir, runID)
		if err != nil {
			return fmt.Errorf("load run %s: %w", runID, err)
		}
		if finished || runTerminalConfirmed(r) {
			sp.Stop("")
			printRunRecap(out, r, dataDir)
			return watchExitError(r)
		}

		stage, round, maxRound := currentStageAndRound(parsed)
		summary := progress.Summarize(parsed)
		stalled, stalledSince := progress.Stalled(summary, r.CreatedAt, time.Now())
		if terminal {
			sp.Start(renderFooterFacts(stage, summary.WaitingReason, round, maxRound, stalled, stalledSince, string(r.State)))
			sp.SetStart(created)
		} else if footer := renderFooter(time.Since(created), stage, summary.WaitingReason, round, maxRound, stalled, stalledSince, string(r.State)); footer != lastFooter {
			fmt.Fprintln(out, footer)
			lastFooter = footer
		}
	}
}

// parseProgressLines decodes each raw progress.jsonl line (as returned by
// progress.ReadFrom) into a progress.Event, silently skipping any line
// that fails to parse -- mirroring progress.Read's own tolerance for a
// malformed line, since a progress file is written incrementally and a
// reader can in principle catch a torn write.
func parseProgressLines(lines []string) []progress.Event {
	events := make([]progress.Event, 0, len(lines))
	for _, line := range lines {
		var e progress.Event
		if err := json.Unmarshal([]byte(line), &e); err == nil {
			events = append(events, e)
		}
	}
	return events
}

// runHasFinishedEvent reports whether events contains the factory's own
// terminal "finished" progress line (see progress-contract.md) -- the
// signal watchRun stops following on, independent of (and usually a hair
// earlier than) reloading the run record itself.
func runHasFinishedEvent(events []progress.Event) bool {
	for _, e := range events {
		if e.Source == "factory" && e.Stage == "finished" {
			return true
		}
	}
	return false
}

// watchExitError maps a run's terminal state to watchMain/watchRun's own
// exit code via realMain's ordinary error-means-nonzero-exit convention:
// nil for accepted, a non-nil error (only its presence matters) for
// halted/quarantined/anything else.
func watchExitError(r *run.Run) error {
	if r.State == run.StateAccepted {
		return nil
	}
	return fmt.Errorf("run %s ended in state %s", r.ID, r.State)
}

// watchRequest implements `factoryd watch <request-id>`: print the
// request's own header, then either attach to its current ticket's run
// (building/pr_review) exactly the way watchRun does, or print the
// review/retry hint for a request waiting on a human and exit -- never
// polling a run for a request that has none yet.
func watchRequest(out *os.File, dataDir, id string, noFollow bool) error {
	// lastHeader/lastRunID keep the loop quiet between changes: the
	// request driver advances a request on its own poll interval, so for
	// a while after a ticket's run ends the record still names that same
	// (now terminal) run -- re-attaching to it would reprint its header,
	// history, and recap every poll until the driver catches up.
	lastHeader, lastRunID := "", ""
	historyPrinted := 0
	// One working line while the request waits on a drafting job or the
	// request driver; it is stopped before a run is attached (watchRun owns
	// the terminal then) and cleared on return. Lines print through it so
	// they land above the animation.
	sp := spinner.New(out, spinner.IsTerminal(out))
	defer sp.Stop("")
	wait := func(r *request.Request) {
		text, since := requestWaitText(dataDir, r)
		sp.Start(text)
		sp.SetStart(since)
	}
	for {
		r, err := request.Load(dataDir, id)
		if err != nil {
			return fmt.Errorf("load request %s: %w", id, err)
		}
		if header := renderRequestHeader(r); header != lastHeader {
			sp.Println(header)
			if lastHeader == "" {
				if url := consoleRequestURL(resolveConsoleBaseURL("", dataDir), r.ID); url != "" {
					sp.Println("  console: " + url)
				}
			}
			lastHeader = header
			for _, transition := range r.History[historyPrinted:] {
				sp.Println(fmt.Sprintf("  %s", renderTransitionLine(transition, time.Now())))
			}
			historyPrinted = len(r.History)
		}

		switch r.State {
		case request.StateBuilding, request.StatePRReview:
			if r.TicketIndex < 1 || r.TicketIndex > len(r.Tickets) {
				return fmt.Errorf("request %s: ticket index %d out of range (%d ticket(s))", r.ID, r.TicketIndex, len(r.Tickets))
			}
			ticket := r.Tickets[r.TicketIndex-1]
			// pr_review waits on a human reviewer, possibly for days: say
			// so and return, like spec_review/plan_review, rather than
			// polling silently ("silence is a bug"). Attach to the ticket's
			// run first if it hasn't been shown yet, so a corrective round
			// in flight is still followed.
			if r.State == request.StatePRReview && (ticket.RunID == "" || ticket.RunID == lastRunID) {
				fmt.Fprintf(out, "waiting on PR review/merge for ticket %d/%d: %s\n", r.TicketIndex, r.TicketCount, ticket.PRURL)
				return nil
			}
			if ticket.RunID == "" || ticket.RunID == lastRunID {
				if noFollow {
					if ticket.RunID == "" {
						fmt.Fprintf(out, "ticket %d/%d has not started a run yet\n", r.TicketIndex, r.TicketCount)
					}
					return nil
				}
				wait(r)
				time.Sleep(watchRequestPollInterval)
				continue
			}
			lastRunID = ticket.RunID
			sp.Stop("")
			runErr := watchRun(out, dataDir, ticket.RunID, noFollow)
			if noFollow {
				return runErr
			}
			// Loop back around regardless of runErr: the next iteration
			// waits (via lastRunID above) until request_driver.go's own
			// poll has advanced r to its next ticket, pr_review, or a
			// terminal state, then reports whatever r is now.
			continue
		case request.StateSpecReview:
			fmt.Fprintf(out, "Review the drafted spec, then run: factoryd approve %s\n", r.ID)
			return nil
		case request.StateOracleReview:
			fmt.Fprintf(out, "Review or hand-write %s/%s (leave it absent or empty to skip), then run: factoryd approve %s\n", request.Dir(dataDir, r.ID), request.RequestOracleDirName, r.ID)
			fmt.Fprintf(out, "%s\n", request.OracleReviewChecklistHint)
			if notice := request.OracleReviewNotice(dataDir, r); notice != "" {
				fmt.Fprintf(out, "ACTION NEEDED before approve: %s\n", notice)
			}
			return nil
		case request.StatePlanReview:
			fmt.Fprintf(out, "Review the drafted plan, then run: factoryd approve %s\n", r.ID)
			return nil
		case request.StateResumeReview:
			// A lost step waits on a human decision, like a review gate:
			// nothing is failing, so this is not an error exit.
			fmt.Fprintf(out, "reason: %s\nnext: %s\n", sanitize.Line(r.Error), sanitize.Line(r.NextAction()))
			return nil
		case request.StateHalted, request.StateQuarantined:
			// next is always r.NextAction() now, not a hardcoded "factoryd retry"
			// that could contradict the reason right above it -- e.g. a
			// release-policy denial, where a bare retry is denied again
			// against the same evidence.
			//
			// r.Error embeds model/job error text verbatim (e.g. "spec
			// drafting failed: %v"): sanitize.Line flattens it so an
			// embedded newline can't print a forged "next:" line below
			// the real one (follow-up from PR #262's adversarial review --
			// the buildgate operator skill has an agent read this output).
			if r.AwaitingPullRequest() {
				// Calm, not a failure: the ticket is accepted, only its PR is missing.
				fmt.Fprintf(out, "%s\nreason: %s\nnext: %s\n", r.AwaitingPullRequestLabel(), sanitize.Line(r.Error), r.NextAction())
				return nil
			}
			fmt.Fprintf(out, "reason: %s\nnext: %s\n", sanitize.Line(r.Error), sanitize.Line(r.NextAction()))
			return fmt.Errorf("request %s %s", r.ID, r.State)
		case request.StateDone:
			for _, t := range r.Tickets {
				if t.PRURL != "" {
					fmt.Fprintf(out, "PR: %s\n", t.PRURL)
				}
			}
			return nil
		case request.StateCancelled:
			return fmt.Errorf("request %s was cancelled", r.ID)
		default:
			if noFollow {
				return nil
			}
			wait(r)
			time.Sleep(watchRequestPollInterval)
			continue
		}
	}
}

// newTTYSpinner returns a spinner for w when w is an interactive terminal,
// or nil otherwise -- for waits that print nothing today and must stay silent
// in logs, pipes and tests.
func newTTYSpinner(w io.Writer) *spinner.Spinner {
	if f, ok := w.(*os.File); ok && spinner.IsTerminal(f) {
		return spinner.New(f, true)
	}
	return nil
}

// requestWaitText is the working-line text for a request that is waiting
// (submitted, drafting, or building with no run in view) and when that state
// began, or the zero time when unknown. It names only facts on disk: the
// state, the drafting job's role, model and harness, or the ticket. It carries
// no elapsed time, so the plain (non-terminal) line prints once per change.
func requestWaitText(dataDir string, r *request.Request) (text string, since time.Time) {
	text = strings.ReplaceAll(string(r.State), "_", " ")
	if job := request.LoadActiveJob(dataDir, r.ID); job != nil && job.Stage == string(r.State) {
		text += " · " + job.Role + " role"
		if job.Model != "" {
			text += " " + job.Model
		}
		if job.Harness != "" {
			text += " (" + job.Harness + ")"
		}
		if at, err := time.Parse(time.RFC3339Nano, job.StartedAt); err == nil {
			return text, at
		}
	}
	switch r.State {
	case request.StateSubmitted:
		text += " · waiting for the request driver to pick it up"
	case request.StateBuilding:
		text += fmt.Sprintf(" · ticket %d/%d waiting on the request driver", r.TicketIndex, r.TicketCount)
	}
	for i := len(r.History) - 1; i >= 0; i-- {
		if r.History[i].To == r.State {
			if at, err := time.Parse(time.RFC3339Nano, r.History[i].At); err == nil {
				since = at
			}
			break
		}
	}
	return text, since
}

// renderTransitionLine formats one Request.History entry for `factoryd
// watch`'s pipeline-history listing, printed once per request-header
// change: a local "HH:mm" clock (or "Jan 2 15:04" when t.At isn't today,
// so an old entry doesn't read as if it happened today), the state move,
// who made it, and its reason when it has one -- e.g. "10:14 spec_review
// -> planning   by operator  approved". An unparseable t.At is printed
// as-is rather than dropped, so a malformed record's history is still
// visible.
func renderTransitionLine(t request.Transition, now time.Time) string {
	when := t.At
	if at, err := time.Parse(time.RFC3339Nano, t.At); err == nil {
		local := at.Local()
		if sameDay(local, now) {
			when = local.Format("15:04")
		} else {
			when = local.Format("Jan 2 15:04")
		}
	}
	line := fmt.Sprintf("%s  %s -> %s   by %s", when, t.From, t.To, sanitize.Line(t.By))
	if t.Reason != "" {
		// A halt/quarantine transition's Reason is the same untrusted
		// error text as Request.Error -- see watchRequest's reason: line.
		line += "  " + sanitize.Line(t.Reason)
	}
	return line
}

// sameDay reports whether a and b fall on the same calendar day.
func sameDay(a, b time.Time) bool {
	ay, am, ad := a.Date()
	by, bm, bd := b.Date()
	return ay == by && am == bm && ad == bd
}

// renderRequestHeader is `factoryd watch`'s one-line header for a
// request id.
func renderRequestHeader(r *request.Request) string {
	ticket := "-"
	if r.TicketCount > 0 {
		ticket = fmt.Sprintf("%d/%d", r.TicketIndex, r.TicketCount)
	}
	return fmt.Sprintf("request %s · %s · %s · ticket %s", r.ID, r.Project, r.State, ticket)
}
