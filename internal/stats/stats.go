// Package stats answers "is the factory getting better on this repository?"
// as numbers: how many tickets were built right the first time, how many ended
// accepted, how many rounds that took, how often a failed round failed the
// same way again, and what stopped the runs that did not finish clean. It is
// a pure function of run records: nothing is stored, no model is called and
// nothing is gated.
//
// scripts/baseline.py computes the same family of numbers over several data
// directories and older record shapes; this package reads the run records of
// one data directory as they are written now. Where a number is defined
// differently, its field's doc comment says so.
package stats

import (
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"buildgate/internal/run"
	"buildgate/internal/sanitize"
)

const (
	// SmokePrefix starts the ticket id of every run scripts/live-smoke.sh
	// makes. Those tickets are built to prove the pipeline, some to fail.
	SmokePrefix = "live-smoke-"
	// DefaultBucketDays is the width of a bucket when Options names none.
	DefaultBucketDays = 7
	// MaxBuckets caps a report's buckets; the newest are kept.
	MaxBuckets = 26
	// TopN caps the quarantined-by and halted-by lists.
	TopN = 10

	noFailedCheck   = "(no failed check recorded)"
	noHaltReason    = "(no reason recorded)"
	haltReasonLimit = 80
	// noChanges is build_app.py's blocker for a round whose agent turn left
	// the workspace as it found it.
	noChanges = "no changes made to the workspace"
)

// followUpRun matches the suffix of a run that follows a ticket's own run: a
// PR-review, conformity or corrective round, each possibly with -fix<N>. The
// suffix follows the ticket's three-digit index, so a ticket merely named
// "api-review1" keeps its name (the same rule as scripts/baseline.py).
var followUpRun = regexp.MustCompile(`(-\d{3})-(?:review|conformity|corrective)\d+(?:-fix\d+)?$`)

// Options narrows and shapes a report.
type Options struct {
	// Project labels the report; the caller has already narrowed the runs.
	Project string
	// Since and Until bound a ticket by the time of its first run: Since
	// inclusive, Until exclusive. Zero means unbounded.
	Since, Until time.Time
	// BucketDays is the width of a bucket in days; 0 means DefaultBucketDays.
	BucketDays int
	// Now ends the last bucket when Until is zero; when it is zero too, the
	// last bucket holds the newest ticket's first run.
	Now time.Time
	// ExcludeTicketPrefixes leaves out every run whose ticket (or, with no
	// ticket, id) starts with one of them. The count is Report.ExcludedRuns.
	ExcludeTicketPrefixes []string
}

// Count is a name and how many runs it stopped.
type Count struct {
	Name string `json:"name"`
	Runs int    `json:"runs"`
}

// Spread is the median and the 90th percentile of a count over several
// tickets. Series is how many tickets it was taken over; with none, both are 0.
type Spread struct {
	Series int `json:"series"`
	// Median is the middle value, the mean of the two middle ones for an
	// even count.
	Median float64 `json:"median"`
	// P90 is the nearest-rank 90th percentile.
	P90 float64 `json:"p90"`
}

// Corrective counts the runs that came after a ticket's first run: a
// corrective, conformity or PR-review round, or a retry.
type Corrective struct {
	// Ran is how many such runs finished.
	Ran int `json:"ran"`
	// Accepted is how many of them ended accepted.
	Accepted int `json:"accepted"`
}

// Spend is what the finished runs consumed through the relay: the sums
// run.BuildHarnessEval keeps per run. It does not include drafting, and a run
// that recorded no relay spend adds nothing; `factoryd cost` is the full
// account.
type Spend struct {
	Tokens       int64 `json:"tokens"`
	CostMicroUSD int64 `json:"cost_micro_usd"`
	// RunsWithSpend is how many runs recorded any relay spend.
	RunsWithSpend int `json:"runs_with_spend"`
	// PerAcceptedTicketMicroUSD is CostMicroUSD over Accepted, 0 with none.
	PerAcceptedTicketMicroUSD int64 `json:"per_accepted_ticket_micro_usd"`
}

// Metrics are the numbers over a set of ticket attempt series. A series is
// the finished runs of one ticket in the order they were created: its own
// run, then any retry, corrective round, conformity round or PR-review round.
type Metrics struct {
	// Tickets is the number of series.
	Tickets int `json:"tickets"`
	// OneShot is the series that have one run only, accepted with no override
	// or rescue and at most one recorded build round. baseline.py counts a
	// first run accepted without override or rescue, whatever followed or how
	// many rounds it took.
	OneShot     int      `json:"one_shot"`
	OneShotRate *float64 `json:"one_shot_rate"`
	// Accepted is the series whose last run is accepted. baseline.py has no
	// such number.
	Accepted     int      `json:"accepted"`
	AcceptedRate *float64 `json:"accepted_rate"`
	// RoundsToGreen is, over the accepted series that recorded any round, the
	// build rounds summed across all its runs. baseline.py takes the rounds of
	// each accepted run.
	RoundsToGreen Spread `json:"rounds_to_green"`
	// FailedRoundPairs is the consecutive failed rounds within one run.
	FailedRoundPairs int `json:"failed_round_pairs"`
	// ComparablePairs is the failed-round pairs whose two rounds both
	// recorded a failure signature; SameFailurePairs is those whose
	// signatures are equal. baseline.py also derives a signature from the
	// build report for a round that recorded none and keeps recorded and
	// derived pairs apart; this counts recorded signatures only.
	ComparablePairs  int `json:"comparable_pairs"`
	SameFailurePairs int `json:"same_failure_pairs"`
	// NoChangePairs is the failed-round pairs whose second round changed no
	// file.
	NoChangePairs int `json:"no_change_pairs"`
	// QuarantinedBy counts the quarantined runs per failed check, a run once
	// per check it failed; a run that recorded none counts under its reason
	// code in brackets. Largest first, ties by name, at most TopN.
	QuarantinedBy []Count `json:"quarantined_by"`
	// HaltedBy counts the halted runs per reason code, else the first line
	// of their triage sentence. Largest first, ties by name, at most TopN.
	HaltedBy         []Count    `json:"halted_by"`
	CorrectiveBuilds Corrective `json:"corrective_builds"`
	Spend            Spend      `json:"spend"`
}

// Bucket is Metrics over the tickets whose first run began in [Start, End).
type Bucket struct {
	// Start and End are RFC 3339 UTC midnights.
	Start   string  `json:"start"`
	End     string  `json:"end"`
	Metrics Metrics `json:"metrics"`
}

// Report is what a repository's runs say.
type Report struct {
	Project string `json:"project"`
	// Since and Until echo the window, "" when unbounded.
	Since      string `json:"since"`
	Until      string `json:"until"`
	BucketDays int    `json:"bucket_days"`
	// Runs is the finished runs of the counted tickets.
	Runs int `json:"runs"`
	// Unfinished is the runs still in progress, left out; a ticket whose
	// first run is one is not counted.
	Unfinished int `json:"unfinished"`
	// ExcludedRuns is the runs left out for their ticket's prefix.
	ExcludedRuns int     `json:"excluded_runs"`
	Overall      Metrics `json:"overall"`
	// Buckets are oldest first, empty ones kept, at most MaxBuckets.
	Buckets []Bucket `json:"buckets"`
}

type series struct {
	runs  []*run.Run
	start time.Time
}

// Compute builds the report over runs, which the caller has already narrowed
// to Options.Project.
func Compute(runs []*run.Run, opts Options) Report {
	days := opts.BucketDays
	if days <= 0 {
		days = DefaultBucketDays
	}
	report := Report{Project: opts.Project, BucketDays: days, Since: formatTime(opts.Since), Until: formatTime(opts.Until), Buckets: []Bucket{}}
	var kept []series
	for _, s := range groupSeries(report.admit(runs, opts)) {
		// A ticket whose first run is still in progress is not counted yet,
		// whatever its later runs did (as in scripts/baseline.py); a later run
		// still in progress is left out of its series.
		if !finished(s.runs[0].State) || !inWindow(s.start, opts) {
			continue
		}
		s.runs = finishedRuns(s.runs)
		kept = append(kept, s)
		report.Runs += len(s.runs)
	}
	report.Overall = metricsOf(kept)
	report.Buckets = buckets(kept, opts, days)
	return report
}

// admit returns the runs that survive the prefix exclusion, and counts the
// excluded ones and the ones still in progress.
func (report *Report) admit(runs []*run.Run, opts Options) []*run.Run {
	var out []*run.Run
	for _, r := range runs {
		switch {
		case r == nil:
		case excluded(r, opts.ExcludeTicketPrefixes):
			report.ExcludedRuns++
		default:
			if !finished(r.State) {
				report.Unfinished++
			}
			out = append(out, r)
		}
	}
	return out
}

func finished(state run.State) bool {
	return state == run.StateAccepted || state == run.StateQuarantined || state == run.StateHalted
}

func finishedRuns(runs []*run.Run) []*run.Run {
	out := make([]*run.Run, 0, len(runs))
	for _, r := range runs {
		if finished(r.State) {
			out = append(out, r)
		}
	}
	return out
}

func ticketName(r *run.Run) string {
	if r.Ticket != "" {
		return r.Ticket
	}
	return r.ID
}

func excluded(r *run.Run, prefixes []string) bool {
	for _, p := range prefixes {
		if p != "" && (strings.HasPrefix(ticketName(r), p) || strings.HasPrefix(r.ID, p)) {
			return true
		}
	}
	return false
}

func parseTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// groupSeries groups runs by request and ticket, a follow-up run under the
// ticket it follows, each series in creation order and the series in order of
// their first run.
func groupSeries(runs []*run.Run) []series {
	groups := map[string][]*run.Run{}
	for _, r := range runs {
		key := r.RequestID + "\x00" + followUpRun.ReplaceAllString(ticketName(r), "$1")
		groups[key] = append(groups[key], r)
	}
	out := make([]series, 0, len(groups))
	for _, g := range groups {
		sort.SliceStable(g, func(i, j int) bool {
			a, b := parseTime(g[i].CreatedAt), parseTime(g[j].CreatedAt)
			if !a.Equal(b) {
				return a.Before(b)
			}
			return g[i].ID < g[j].ID
		})
		out = append(out, series{runs: g, start: parseTime(g[0].CreatedAt)})
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].start.Equal(out[j].start) {
			return out[i].start.Before(out[j].start)
		}
		return out[i].runs[0].ID < out[j].runs[0].ID
	})
	return out
}

func inWindow(start time.Time, opts Options) bool {
	if start.IsZero() {
		return opts.Since.IsZero() && opts.Until.IsZero()
	}
	return (opts.Since.IsZero() || !start.Before(opts.Since)) && (opts.Until.IsZero() || start.Before(opts.Until))
}

func rate(part, whole int) *float64 {
	if whole == 0 {
		return nil
	}
	v := math.Round(float64(part)/float64(whole)*10000) / 10000
	return &v
}

func metricsOf(list []series) Metrics {
	m := Metrics{Tickets: len(list), QuarantinedBy: []Count{}, HaltedBy: []Count{}}
	quarantined, halted := map[string]int{}, map[string]int{}
	var rounds []int
	for _, s := range list {
		m.addSeries(s, &rounds)
		for _, r := range s.runs {
			m.addPairs(r)
			m.addSpend(r)
			switch r.State {
			case run.StateQuarantined:
				for _, check := range failedChecks(r) {
					quarantined[check]++
				}
			case run.StateHalted:
				halted[haltReason(r)]++
			}
		}
	}
	m.OneShotRate, m.AcceptedRate = rate(m.OneShot, m.Tickets), rate(m.Accepted, m.Tickets)
	m.RoundsToGreen = spread(rounds)
	m.QuarantinedBy, m.HaltedBy = top(quarantined), top(halted)
	if m.Accepted > 0 {
		m.Spend.PerAcceptedTicketMicroUSD = m.Spend.CostMicroUSD / int64(m.Accepted)
	}
	return m
}

func roundCount(r *run.Run) int {
	if r.AgentEvidence == nil {
		return 0
	}
	return len(r.AgentEvidence.Rounds)
}

// addSeries counts one series' one-shot and accepted outcome, its rounds to
// green and the runs that followed its first.
func (m *Metrics) addSeries(s series, rounds *[]int) {
	first, last := s.runs[0], s.runs[len(s.runs)-1]
	if len(s.runs) == 1 && first.State == run.StateAccepted && len(first.Overrides) == 0 && len(first.Rescues) == 0 && roundCount(first) <= 1 {
		m.OneShot++
	}
	if last.State == run.StateAccepted {
		m.Accepted++
		total := 0
		for _, r := range s.runs {
			total += roundCount(r)
		}
		if total > 0 {
			*rounds = append(*rounds, total)
		}
	}
	for _, r := range s.runs[1:] {
		m.CorrectiveBuilds.Ran++
		if r.State == run.StateAccepted {
			m.CorrectiveBuilds.Accepted++
		}
	}
}

// addPairs counts the consecutive failed rounds of one run.
func (m *Metrics) addPairs(r *run.Run) {
	if r.AgentEvidence == nil {
		return
	}
	rounds := r.AgentEvidence.Rounds
	for i := 1; i < len(rounds); i++ {
		before, after := rounds[i-1], rounds[i]
		if before.Passed() || after.Passed() {
			continue
		}
		m.FailedRoundPairs++
		if hasBlocker(after.Blockers, noChanges) {
			m.NoChangePairs++
		}
		if before.FailureSignature != "" && after.FailureSignature != "" {
			m.ComparablePairs++
			if before.FailureSignature == after.FailureSignature {
				m.SameFailurePairs++
			}
		}
	}
}

func hasBlocker(blockers []string, want string) bool {
	for _, b := range blockers {
		if b == want {
			return true
		}
	}
	return false
}

func (m *Metrics) addSpend(r *run.Run) {
	var tokens, cost int64
	for _, a := range r.Attempts {
		tokens += a.RelayConsumedInputTokens + a.RelayConsumedOutputTokens
		cost += a.RelayConsumedCostMicroUSD
	}
	if tokens == 0 && cost == 0 {
		return
	}
	m.Spend.RunsWithSpend++
	m.Spend.Tokens += tokens
	m.Spend.CostMicroUSD += cost
}

// failedChecks is the checks a quarantined run failed, each once; its reason
// code in brackets when it recorded none.
func failedChecks(r *run.Run) []string {
	var names []string
	seen := map[string]bool{}
	for _, g := range r.GateResults {
		name := sanitize.Line(g.Check)
		if g.Passed || name == "" || seen[name] {
			continue
		}
		seen[name] = true
		names = append(names, name)
	}
	if len(names) > 0 {
		return names
	}
	if r.HaltReasonCode != "" {
		return []string{"(" + sanitize.Line(r.HaltReasonCode) + ")"}
	}
	return []string{noFailedCheck}
}

// haltReason is why a run halted: its reason code, else the first line of its
// triage sentence.
func haltReason(r *run.Run) string {
	if r.HaltReasonCode != "" {
		return sanitize.Line(r.HaltReasonCode)
	}
	text := strings.TrimPrefix(sanitize.Line(strings.SplitN(strings.TrimSpace(r.Triage), "\n", 2)[0]), "halted: ")
	if len(text) > haltReasonLimit {
		text = text[:haltReasonLimit]
	}
	if text == "" {
		return noHaltReason
	}
	return text
}

func top(counts map[string]int) []Count {
	out := make([]Count, 0, len(counts))
	for name, n := range counts {
		out = append(out, Count{Name: name, Runs: n})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Runs != out[j].Runs {
			return out[i].Runs > out[j].Runs
		}
		return out[i].Name < out[j].Name
	})
	if len(out) > TopN {
		out = out[:TopN]
	}
	return out
}

func spread(values []int) Spread {
	n := len(values)
	if n == 0 {
		return Spread{}
	}
	sorted := append([]int(nil), values...)
	sort.Ints(sorted)
	median := float64(sorted[n/2])
	if n%2 == 0 {
		median = float64(sorted[n/2-1]+sorted[n/2]) / 2
	}
	rank := int(math.Ceil(0.9*float64(n))) - 1
	return Spread{Series: n, Median: median, P90: float64(sorted[rank])}
}

func dayFloor(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

// buckets cuts the series into windows of days, ending the day after the end
// of the window, oldest first, empty ones kept, newest MaxBuckets at most.
func buckets(list []series, opts Options, days int) []Bucket {
	var first, last time.Time
	for _, s := range list {
		if s.start.IsZero() {
			continue
		}
		if first.IsZero() || s.start.Before(first) {
			first = s.start
		}
		if s.start.After(last) {
			last = s.start
		}
	}
	if first.IsZero() {
		return []Bucket{}
	}
	if !opts.Since.IsZero() {
		first = opts.Since
	}
	end := last
	switch {
	case !opts.Until.IsZero():
		end = opts.Until.Add(-time.Nanosecond)
	case !opts.Now.IsZero():
		end = opts.Now
	}
	width := time.Duration(days) * 24 * time.Hour
	endDay := dayFloor(end).Add(24 * time.Hour)
	n := int(math.Ceil(float64(endDay.Sub(dayFloor(first))) / float64(width)))
	if n < 1 {
		n = 1
	}
	if n > MaxBuckets {
		n = MaxBuckets
	}
	begin := endDay.Add(-time.Duration(n) * width)
	out := make([]Bucket, n)
	members := make([][]series, n)
	for _, s := range list {
		if s.start.IsZero() || s.start.Before(begin) || !s.start.Before(endDay) {
			continue
		}
		i := int(s.start.Sub(begin) / width)
		members[i] = append(members[i], s)
	}
	for i := range out {
		start := begin.Add(time.Duration(i) * width)
		out[i] = Bucket{Start: start.Format(time.RFC3339), End: start.Add(width).Format(time.RFC3339), Metrics: metricsOf(members[i])}
	}
	return out
}

// ParseSince reads a window start: "30d" is that many days before now, and
// "2026-09-01" is that UTC midnight. "" is no bound.
func ParseSince(text string, now time.Time) (time.Time, error) {
	if text == "" {
		return time.Time{}, nil
	}
	if n, ok := strings.CutSuffix(text, "d"); ok {
		days, err := strconv.Atoi(n)
		if err != nil || days <= 0 || days > 36500 {
			return time.Time{}, fmt.Errorf("since %q: want a number of days such as 30d, or a date such as 2026-09-01", text)
		}
		return now.AddDate(0, 0, -days), nil
	}
	t, err := time.Parse("2006-01-02", text)
	if err != nil {
		return time.Time{}, fmt.Errorf("since %q: want a number of days such as 30d, or a date such as 2026-09-01", text)
	}
	return t, nil
}

// ParseBucketDays reads a bucket width in days; "" is the default.
func ParseBucketDays(text string) (int, error) {
	if text == "" {
		return DefaultBucketDays, nil
	}
	n, err := strconv.Atoi(text)
	if err != nil || n < 1 || n > 365 {
		return 0, fmt.Errorf("bucket %q: want a number of days from 1 to 365", text)
	}
	return n, nil
}

// Overview is one Report per project, by name, and one over every project's
// runs: what `factoryd stats` prints with no project and GET /stats returns.
type Overview struct {
	Overall  Report   `json:"overall"`
	Projects []Report `json:"projects"`
}

// ComputeOverview builds the Overview of runs. project names the repository
// a run belongs to.
func ComputeOverview(runs []*run.Run, opts Options, project func(*run.Run) string) Overview {
	byProject := map[string][]*run.Run{}
	for _, r := range runs {
		name := project(r)
		byProject[name] = append(byProject[name], r)
	}
	names := make([]string, 0, len(byProject))
	for name := range byProject {
		names = append(names, name)
	}
	sort.Strings(names)
	overview := Overview{Projects: []Report{}}
	for _, name := range names {
		o := opts
		o.Project = name
		overview.Projects = append(overview.Projects, Compute(byProject[name], o))
	}
	overview.Overall = Compute(runs, opts)
	return overview
}
