package timereport

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"buildgate/internal/progress"
	"buildgate/internal/request"
	"buildgate/internal/run"
)

// The two kinds of wait for the repository.
const (
	// KindRepositoryBusy is a wait before a request's step started a run: the
	// step found the repository held by another build and gave its slot back
	// (the request's wait log).
	KindRepositoryBusy = "repository_busy"
	// KindRepositoryLock is a wait inside a run, for the lock its build needs
	// (the run's progress feed: repository_lock waiting, then acquired).
	KindRepositoryLock = "repository_lock"
	// KindBranchLock is a wait inside a run, for its branch's lock held by
	// another round on the same branch (branch_lock waiting, then acquired).
	KindBranchLock = "branch_lock"
)

// Wait is one wait for the repository. An open wait has no end recorded:
// Seconds is 0 and no total counts it.
type Wait struct {
	Kind  string `json:"kind"`
	Start string `json:"start"`
	End   string `json:"end,omitempty"`
	Open  bool   `json:"open,omitempty"`
	// Ongoing is a wait with no end in a run whose record is not in an end
	// state (accepted, quarantined or halted): it may still be waiting, so it
	// is not called open. Seconds is 0 either way.
	Ongoing bool    `json:"ongoing,omitempty"`
	Seconds float64 `json:"seconds"`

	start, end time.Time
}

// StageTime is the time a run spent in one stage, summed over every start and
// end pair of that stage in its feed (challenge and tests_red_on_base run once
// per finding).
type StageTime struct {
	Stage   string  `json:"stage"`
	Seconds float64 `json:"seconds"`
	// Launches counts the held-file reproduction launches of a
	// reproductionStages stage (a start with a detail; not the challenger job,
	// nor a repository gate, whose start carries a detail too); LaunchSeconds is
	// their summed time, apart from Seconds, which also holds the challenger
	// job; WallSeconds is the launches' first start to last end, less than
	// LaunchSeconds when they overlapped. All are 0 for a stage with no launch.
	Launches      int     `json:"launches,omitempty"`
	LaunchSeconds float64 `json:"launch_seconds,omitempty"`
	WallSeconds   float64 `json:"wall_seconds,omitempty"`
	// Parallel marks a stage that ran at the same time as another (review and
	// challenge do): stage minutes then sum to more than the run's wall time.
	Parallel bool `json:"parallel,omitempty"`
}

// RunTime is one run of a ticket.
type RunTime struct {
	RunID string `json:"run_id"`
	// Kind is build, advice, corrective, conformity, review or restack, read
	// from the run's ticket name; Round is the number in it (0 for a build).
	Kind  string `json:"kind"`
	Round int    `json:"round,omitempty"`
	// Attempt numbers the runs of one ticket, kind and round from 1 when
	// there is more than one (a retried build); 0 otherwise.
	Attempt int    `json:"attempt,omitempty"`
	State   string `json:"state"`
	// Start and End are the feed's first and last line. A run whose feed has
	// one line, or none, has no times.
	Start   string  `json:"start,omitempty"`
	End     string  `json:"end,omitempty"`
	Seconds float64 `json:"seconds"`
	// Stages are the stages that recorded a start and an end, in the order
	// they first appeared. Other is Seconds less the union of the stage
	// intervals and the closed lock waits: time no stage or wait covers
	// (image pull, workspace setup).
	Stages    []StageTime `json:"stages,omitempty"`
	Other     float64     `json:"other_seconds"`
	LockWaits []Wait      `json:"lock_waits,omitempty"`
	// LockSeconds is the closed lock waits' total.
	LockSeconds float64 `json:"lock_seconds"`

	start, end time.Time
}

// TicketTime is a ticket's runs, oldest first.
type TicketTime struct {
	Index int       `json:"index"`
	Runs  []RunTime `json:"runs"`
}

// CriticalPath splits the request's span. Runs of one request are normally
// sequential, but the figures do not assume it: IdleSeconds is the span less
// the union of the runs and the RepositoryBusy waits, and RunSeconds is the
// plain sum, so it exceeds the union when runs overlapped.
type CriticalPath struct {
	// Start is when the request entered building (else its first run began);
	// End is the last run end or wait end.
	Start string `json:"start,omitempty"`
	End   string `json:"end,omitempty"`
	// SpanSeconds is End less Start. It includes any time the request spent
	// waiting on a person (a pull request review), which no record here
	// separates from other idle time.
	SpanSeconds float64 `json:"span_seconds"`
	RunSeconds  float64 `json:"run_seconds"`
	// LockSeconds is the in-run lock waits' total (inside RunSeconds); BusySeconds
	// the RepositoryBusy waits' total.
	LockSeconds float64 `json:"lock_seconds"`
	BusySeconds float64 `json:"busy_seconds"`
	IdleSeconds float64 `json:"idle_seconds"`
	// OpenWaits counts the waits whose end was never recorded (a finished
	// run's lock wait, a repository-busy wait); OngoingWaits those of a run
	// still in progress. Neither's time is in any total.
	OpenWaits    int `json:"open_waits"`
	OngoingWaits int `json:"ongoing_waits"`
}

// Report is one request's time report.
type Report struct {
	RequestID string       `json:"request_id"`
	State     string       `json:"state"`
	Tickets   []TicketTime `json:"tickets"`
	// Waits are the RepositoryBusy waits, oldest first.
	Waits    []Wait       `json:"repository_busy_waits"`
	Critical CriticalPath `json:"critical_path"`
}

// runName reads a run's kind, ticket index and round number from its ticket
// name (<request>-<NNN>[-<kind><round>[-fix<k>]]).
var runName = regexp.MustCompile(`-(\d{3})(?:-(review|conformity|corrective|advice|restack)(\d+)(?:-fix\d+)?)?$`)

// Compute builds request id's report from the data dir. It reads the request,
// the runs whose request_id is id, their progress feeds and the wait log; it
// changes nothing.
func Compute(dataDir, id string) (*Report, error) {
	req, err := request.Load(dataDir, id)
	if err != nil {
		return nil, err
	}
	rep := &Report{RequestID: id, State: string(req.State), Tickets: []TicketTime{}}
	if rep.Waits, err = readWaits(dataDir, id); err != nil {
		return nil, fmt.Errorf("read repository waits: %w", err)
	}
	if rep.Waits == nil {
		rep.Waits = []Wait{}
	}
	entries, err := os.ReadDir(filepath.Join(dataDir, "runs"))
	if err != nil {
		return nil, fmt.Errorf("list runs: %w", err)
	}
	byTicket := map[int][]RunTime{}
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), id+"-") {
			continue
		}
		r, ok := loadRun(dataDir, e.Name())
		if !ok || (r.RequestID != "" && r.RequestID != id) {
			continue
		}
		rt, idx, err := runTime(dataDir, r)
		if err != nil {
			return nil, err
		}
		byTicket[idx] = append(byTicket[idx], rt)
	}
	for idx, runs := range byTicket {
		sort.SliceStable(runs, func(i, j int) bool { return runs[i].start.Before(runs[j].start) })
		numberAttempts(runs)
		rep.Tickets = append(rep.Tickets, TicketTime{Index: idx, Runs: runs})
	}
	sort.Slice(rep.Tickets, func(i, j int) bool { return rep.Tickets[i].Index < rep.Tickets[j].Index })
	rep.Critical = criticalPath(req, rep)
	return rep, nil
}

// pendingRun is a run directory's name less the <date>-<time>-<pid> suffix the
// launcher adds to the ticket name.
var pendingRun = regexp.MustCompile(`-\d{8}-\d{6}-\d+$`)

// loadRun loads a run record. A run that has a feed and no run.json yet (it
// is still starting, or waiting for the repository lock before its first
// save) is returned with its state "in_progress", its ticket read from its
// directory name; ok is false for a directory that holds neither.
func loadRun(dataDir, id string) (*run.Run, bool) {
	if r, err := run.Load(dataDir, id); err == nil {
		return r, true
	}
	if _, err := os.Stat(progress.PathInDir(run.Dir(dataDir, id))); err != nil {
		return nil, false
	}
	return &run.Run{ID: id, Ticket: pendingRun.ReplaceAllString(id, ""), State: "in_progress"}, true
}

// runTime reads one run's feed. It returns the run's ticket index.
func runTime(dataDir string, r *run.Run) (RunTime, int, error) {
	rt := RunTime{RunID: r.ID, Kind: "build", State: string(r.State)}
	idx := r.TicketIndex
	if m := runName.FindStringSubmatch(r.Ticket); m != nil {
		if idx == 0 {
			idx, _ = strconv.Atoi(m[1])
		}
		if m[2] != "" {
			rt.Kind = m[2]
			rt.Round, _ = strconv.Atoi(m[3])
		}
	}
	ended := r.State == run.StateAccepted || r.State == run.StateQuarantined || r.State == run.StateHalted
	events, err := progress.Read(progress.PathInDir(run.Dir(dataDir, r.ID)))
	if err != nil {
		return rt, idx, fmt.Errorf("read progress of %s: %w", r.ID, err)
	}
	fillFromFeed(&rt, events, ended)
	return rt, idx, nil
}

// fillFromFeed sets rt's times, stages and lock waits from a run's feed. ended is whether the run's record is in an end state.
func fillFromFeed(rt *RunTime, events []progress.Event, ended bool) {
	if len(events) < 2 {
		return
	}
	rt.start, rt.end = parseTs(events[0].Ts), parseTs(events[len(events)-1].Ts)
	if rt.start.IsZero() || rt.end.IsZero() {
		rt.start, rt.end = time.Time{}, time.Time{}
		return
	}
	rt.Start, rt.End = events[0].Ts, events[len(events)-1].Ts
	rt.Seconds = max(rt.end.Sub(rt.start), 0).Seconds()
	st := newStageTracker()
	var lock *Wait
	for _, e := range events {
		if e.Source != "factory" {
			continue
		}
		at := parseTs(e.Ts)
		switch {
		case isRunLockStage(e.Stage) && e.Event == "waiting":
			if lock == nil {
				rt.LockWaits = append(rt.LockWaits, Wait{Kind: e.Stage, Start: e.Ts, Open: true, start: at})
				lock = &rt.LockWaits[len(rt.LockWaits)-1]
			}
		case isRunLockStage(e.Stage) && e.Event == "acquired":
			if lock != nil {
				lock.End, lock.Open, lock.end = e.Ts, false, at
				lock.Seconds = max(at.Sub(lock.start), 0).Seconds()
				rt.LockSeconds += lock.Seconds
				lock = nil
			}
		case e.Stage == "finished" || at.IsZero():
		case e.Event == "start":
			st.start(e.Stage, e.Detail, at)
		case e.Event == "end":
			st.end(e.Stage, e.Detail, at)
		}
	}
	if lock != nil && !ended {
		lock.Open, lock.Ongoing = false, true
	}
	var all []interval
	for _, w := range rt.LockWaits {
		if !w.Open && !w.Ongoing {
			all = append(all, interval{w.start, w.end})
		}
	}
	for _, name := range st.order {
		rt.Stages = append(rt.Stages, st.stageTime(name))
		all = append(all, st.spans[name]...)
	}
	rt.Other = max(rt.Seconds-unionSeconds(all), 0)
}

// reproductionStages are the stages whose detailed starts are held-file
// reproduction launches (the workflow's tests_red_on_base, challenge and
// review_reproduction attempt kinds); other stages with a detail, such as a
// repository gate named by its check, are not.
var reproductionStages = map[string]bool{"tests_red_on_base": true, "challenge": true, "review_reproduction": true}

// stageTracker pairs a feed's stage starts with their ends.
type stageTracker struct {
	opened   map[string]time.Time
	latest   map[string]string
	order    []string
	sums     map[string]float64
	spans    map[string][]interval
	launches map[string][]interval
}

func newStageTracker() *stageTracker {
	return &stageTracker{opened: map[string]time.Time{}, latest: map[string]string{}, sums: map[string]float64{}, spans: map[string][]interval{}, launches: map[string][]interval{}}
}

// start opens a stage. Launches of one stage may overlap, so a start is keyed
// by its detail (the case and pass) as well as its stage.
func (t *stageTracker) start(stage, detail string, at time.Time) {
	key := stage + "\x00" + detail
	t.opened[key], t.latest[stage] = at, key
}

// end closes the start with the same detail; an end that names no open start
// closes the stage's latest one.
func (t *stageTracker) end(stage, detail string, at time.Time) {
	key := stage + "\x00" + detail
	if _, ok := t.opened[key]; !ok {
		key = t.latest[stage]
	}
	from, ok := t.opened[key]
	if !ok {
		return
	}
	if !slices.Contains(t.order, stage) {
		t.order = append(t.order, stage)
	}
	t.sums[stage] += max(at.Sub(from).Seconds(), 0)
	t.spans[stage] = append(t.spans[stage], interval{from, at})
	if reproductionStages[stage] && !strings.HasSuffix(key, "\x00") {
		t.launches[stage] = append(t.launches[stage], interval{from, at})
	}
	delete(t.opened, key)
}

// stageTime is the stage's summed time, its launches and whether it ran at the
// same time as another stage.
func (t *stageTracker) stageTime(name string) StageTime {
	st := StageTime{Stage: name, Seconds: t.sums[name], Parallel: overlapsOther(name, t.spans)}
	if l := t.launches[name]; len(l) > 0 {
		st.Launches, st.WallSeconds = len(l), wallSeconds(l)
		for _, s := range l {
			st.LaunchSeconds += max(s.to.Sub(s.from).Seconds(), 0)
		}
	}
	return st
}

// isRunLockStage reports whether stage brackets an in-run wait for the
// repository or the run's branch (the stage names are the wait kinds).
func isRunLockStage(stage string) bool {
	return stage == KindRepositoryLock || stage == KindBranchLock
}

func parseTs(s string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

type interval struct{ from, to time.Time }

// criticalPath totals rep's runs and waits against the request's span.
func criticalPath(req *request.Request, rep *Report) CriticalPath {
	var cp CriticalPath
	var spans []interval
	var first, last time.Time
	extend := func(from, to time.Time) {
		if first.IsZero() || from.Before(first) {
			first = from
		}
		if to.After(last) {
			last = to
		}
	}
	for _, t := range rep.Tickets {
		for _, r := range t.Runs {
			cp.RunSeconds += r.Seconds
			cp.LockSeconds += r.LockSeconds
			for _, w := range r.LockWaits {
				if w.Open {
					cp.OpenWaits++
				}
				if w.Ongoing {
					cp.OngoingWaits++
				}
			}
			if !r.start.IsZero() {
				spans = append(spans, interval{r.start, r.end})
				extend(r.start, r.end)
			}
		}
	}
	for _, w := range rep.Waits {
		if w.Open {
			cp.OpenWaits++
			continue
		}
		cp.BusySeconds += w.Seconds
		spans = append(spans, interval{w.start, w.end})
		extend(w.start, w.end)
	}
	start := first
	for _, h := range req.History {
		if h.To == request.StateBuilding {
			if at := parseTs(h.At); !at.IsZero() {
				start = at
			}
			break
		}
	}
	if start.IsZero() || last.IsZero() {
		return cp
	}
	if first.Before(start) {
		start = first
	}
	cp.Start, cp.End = start.UTC().Format(time.RFC3339), last.UTC().Format(time.RFC3339)
	cp.SpanSeconds = last.Sub(start).Seconds()
	cp.IdleSeconds = max(cp.SpanSeconds-unionSeconds(spans), 0)
	return cp
}

// wallSeconds is the time from the earliest start to the latest end.
func wallSeconds(spans []interval) float64 {
	first, last := spans[0].from, spans[0].to
	for _, s := range spans[1:] {
		if s.from.Before(first) {
			first = s.from
		}
		if s.to.After(last) {
			last = s.to
		}
	}
	return max(last.Sub(first).Seconds(), 0)
}

// unionSeconds is the total length of the intervals' union.
func unionSeconds(spans []interval) float64 {
	sort.Slice(spans, func(i, j int) bool { return spans[i].from.Before(spans[j].from) })
	var total float64
	var cur interval
	for i, s := range spans {
		switch {
		case i == 0:
			cur = s
		case !s.from.After(cur.to):
			if s.to.After(cur.to) {
				cur.to = s.to
			}
		default:
			total += cur.to.Sub(cur.from).Seconds()
			cur = s
		}
	}
	if len(spans) > 0 {
		total += cur.to.Sub(cur.from).Seconds()
	}
	return total
}

// overlapsOther reports whether any interval of stage overlaps one of another
// stage.
func overlapsOther(stage string, spans map[string][]interval) bool {
	for other, theirs := range spans {
		if other == stage {
			continue
		}
		for _, a := range spans[stage] {
			for _, b := range theirs {
				if a.from.Before(b.to) && b.from.Before(a.to) {
					return true
				}
			}
		}
	}
	return false
}

// numberAttempts sets Attempt on runs (oldest first) that share a kind and
// round with another run of the ticket.
func numberAttempts(runs []RunTime) {
	type key struct {
		kind  string
		round int
	}
	total := map[key]int{}
	for _, r := range runs {
		total[key{r.Kind, r.Round}]++
	}
	seen := map[key]int{}
	for i, r := range runs {
		k := key{r.Kind, r.Round}
		seen[k]++
		if total[k] > 1 {
			runs[i].Attempt = seen[k]
		}
	}
}
