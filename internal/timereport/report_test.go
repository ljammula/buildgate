package timereport

import (
	"math"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"buildgate/internal/progress"
)

// The testdata data dir holds two requests of a real build (the board app's
// backend and browser client), trimmed to the request record, each run's
// factory lines and the run's identity. The backend request's wait log is
// written by hand (no worker had recorded one when the data was taken): one
// closed wait and one open.
const (
	backendID = "the-board-app-needs-a-backend-that-imple-20261031-120845"
	browserID = "the-board-app-needs-a-browser-client-for-20261031-130949"
)

func near(t *testing.T, what string, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 2 {
		t.Errorf("%s = %.1f s, want %.1f s", what, got, want)
	}
}

func runByID(t *testing.T, rep *Report, suffix string) RunTime {
	t.Helper()
	for _, tk := range rep.Tickets {
		for _, r := range tk.Runs {
			if r.RunID == suffix {
				return r
			}
		}
	}
	t.Fatalf("run %s not in the report", suffix)
	return RunTime{}
}

func stage(r RunTime, name string) float64 {
	for _, s := range r.Stages {
		if s.Stage == name {
			return s.Seconds
		}
	}
	return -1
}

func TestComputeGroupsRunsByTicketAndKind(t *testing.T) {
	rep, err := Compute("testdata/board-app", backendID)
	if err != nil {
		t.Fatal(err)
	}
	if rep.State != "pr_review" || len(rep.Tickets) != 3 {
		t.Fatalf("state %q, %d tickets; want pr_review and tickets 1, 4, 7", rep.State, len(rep.Tickets))
	}
	got := map[int][]string{}
	for _, tk := range rep.Tickets {
		for _, r := range tk.Runs {
			got[tk.Index] = append(got[tk.Index], r.Kind)
			if r.Kind == "advice" && r.Round != 1 || r.Kind == "review" && r.Round != 3 {
				t.Errorf("%s: round %d", r.RunID, r.Round)
			}
		}
	}
	want := map[int][]string{1: {"build", "advice"}, 4: {"advice", "review"}, 7: {"restack"}}
	for idx, kinds := range want {
		if !slices.Equal(got[idx], kinds) {
			t.Errorf("ticket %d kinds = %v, want %v", idx, got[idx], kinds)
		}
	}
}

func TestComputeMinutesPerStageOfAnAdviceRound(t *testing.T) {
	rep, err := Compute("testdata/board-app", backendID)
	if err != nil {
		t.Fatal(err)
	}
	r := runByID(t, rep, backendID+"-001-advice1-20261031-123158-52457")
	// Feed: first line 17:31:58.916, last 17:44:09.
	near(t, "wall", r.Seconds, 730)
	near(t, "build", stage(r, "build"), 174)          // 17:32:19 to 17:35:13
	near(t, "verify", stage(r, "verify"), 71)         // 17:35:13 to 17:36:24
	near(t, "full_suite", stage(r, "full_suite"), 65) // 17:36:24 to 17:37:29
	near(t, "review", stage(r, "review"), 50)         // 17:41:07 to 17:41:57
	// Three findings, each a result and a base run, summed.
	near(t, "tests_red_on_base", stage(r, "tests_red_on_base"), 219) // 17:37:29 to 17:41:07 less nothing between
	near(t, "challenge", stage(r, "challenge"), 132)                 // 3 x ~44 s
	if len(r.LockWaits) != 0 || r.LockSeconds != 0 {
		t.Errorf("lock waits = %v, want none", r.LockWaits)
	}
	var sum float64
	for _, s := range r.Stages {
		sum += s.Seconds
	}
	near(t, "stages + other", sum+r.Other+r.LockSeconds, r.Seconds)
}

func TestComputeInRunLockWait(t *testing.T) {
	rep, err := Compute("testdata/board-app", backendID)
	if err != nil {
		t.Fatal(err)
	}
	r := runByID(t, rep, backendID+"-004-review3-20261031-204640-94389")
	if len(r.LockWaits) != 1 || r.LockWaits[0].Open || r.LockWaits[0].Kind != KindRepositoryLock {
		t.Fatalf("lock waits = %+v, want one closed repository_lock wait", r.LockWaits)
	}
	if r.LockSeconds < 600 || r.LockSeconds > 800 || r.LockSeconds != r.LockWaits[0].Seconds {
		t.Errorf("lock seconds = %.1f, want about 11.5 minutes", r.LockSeconds)
	}
	// The wait is its own entry, not part of any stage or of "other".
	if stage(r, "repository_lock") != -1 {
		t.Error("repository_lock listed as a stage")
	}
	if r.Other > 10 {
		t.Errorf("other = %.1f s: the lock wait was counted twice or not at all", r.Other)
	}
}

func TestComputeReportsAWaitWithNoEndAsOpen(t *testing.T) {
	rep, err := Compute("testdata/board-app", backendID)
	if err != nil {
		t.Fatal(err)
	}
	// A run with a feed and no run.json yet, whose lock wait never ended.
	r := runByID(t, rep, backendID+"-007-restack1-20261031-230205-60770")
	if r.State != "in_progress" || r.Kind != "restack" || len(r.LockWaits) != 1 || !r.LockWaits[0].Ongoing || r.LockWaits[0].Open || r.LockSeconds != 0 {
		t.Fatalf("run = %+v, want an in-progress restack with one open lock wait", r)
	}
	if len(rep.Waits) != 2 || rep.Waits[0].Open || !rep.Waits[1].Open || rep.Waits[1].Seconds != 0 {
		t.Fatalf("repository-busy waits = %+v, want one closed then one open", rep.Waits)
	}
	near(t, "closed busy wait", rep.Waits[0].Seconds, 750)
	if rep.Critical.OpenWaits != 1 || rep.Critical.OngoingWaits != 1 {
		t.Errorf("open %d, ongoing %d; want 1 (the request's) and 1 (the running restack's)", rep.Critical.OpenWaits, rep.Critical.OngoingWaits)
	}
	near(t, "busy total", rep.Critical.BusySeconds, 750)
}

func TestComputeCriticalPath(t *testing.T) {
	rep, err := Compute("testdata/board-app", backendID)
	if err != nil {
		t.Fatal(err)
	}
	c := rep.Critical
	start, _ := time.Parse(time.RFC3339, c.Start)
	end, _ := time.Parse(time.RFC3339, c.End)
	// Building was entered 17:09:56 on 2026-10-31; the last record is the
	// in-progress restack's, 04:02:05 on 2026-11-01.
	if c.Start != "2026-10-31T17:09:56Z" || c.End != "2026-11-01T04:02:05Z" {
		t.Errorf("span = %s to %s", c.Start, c.End)
	}
	near(t, "span", c.SpanSeconds, end.Sub(start).Seconds())
	var runs, locks float64
	for _, tk := range rep.Tickets {
		for _, r := range tk.Runs {
			runs += r.Seconds
			locks += r.LockSeconds
		}
	}
	near(t, "run total", c.RunSeconds, runs)
	near(t, "lock total", c.LockSeconds, locks)
	// The runs here are sequential and do not overlap the busy wait, so the
	// idle time is exactly what the three kinds leave of the span.
	near(t, "idle", c.IdleSeconds, c.SpanSeconds-c.RunSeconds-c.BusySeconds)
}

func TestComputeSecondRequestReviewRoundWaitsForTheLock(t *testing.T) {
	rep, err := Compute("testdata/board-app", browserID)
	if err != nil {
		t.Fatal(err)
	}
	r := runByID(t, rep, browserID+"-001-review3-20261031-203042-94389")
	// waiting 01:30:42, acquired 01:46:36 (UTC, next day): 954 s.
	near(t, "lock wait", r.LockSeconds, 954)
	if len(rep.Waits) != 0 {
		t.Errorf("busy waits = %v, want none: no wait log", rep.Waits)
	}
}

func TestComputeUnknownRequest(t *testing.T) {
	if _, err := Compute("testdata/board-app", "nope"); err == nil {
		t.Error("Compute of an unknown request returned no error")
	}
}

func TestUnionSecondsCountsOverlapOnce(t *testing.T) {
	at := func(s int) time.Time { return time.Date(2026, 1, 1, 0, 0, s, 0, time.UTC) }
	got := unionSeconds([]interval{{at(50), at(60)}, {at(0), at(20)}, {at(10), at(30)}, {at(30), at(40)}})
	if got != 50 {
		t.Errorf("union = %v, want 50", got)
	}
}

func TestRecordWaitPairsStartAndEnd(t *testing.T) {
	dataDir := t.TempDir()
	id := "req-1"
	if err := os.MkdirAll(filepath.Dir(WaitsPath(dataDir, id)), 0o755); err != nil {
		t.Fatal(err)
	}
	if got, err := readWaits(dataDir, id); err != nil || got != nil {
		t.Fatalf("no log: %v, %v", got, err)
	}
	t0 := time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)
	for _, rec := range []struct {
		event string
		at    time.Time
	}{
		{WaitEnd, t0.Add(-time.Hour)}, // an end with no start: ignored
		{WaitStart, t0},               // closed below
		{WaitEnd, t0.Add(90 * time.Second)},
		{WaitStart, t0.Add(time.Hour)},     // worker restarted: never ended
		{WaitStart, t0.Add(2 * time.Hour)}, // still open
	} {
		if err := RecordWait(dataDir, id, rec.event, rec.at); err != nil {
			t.Fatal(err)
		}
	}
	if err := RecordWait(dataDir, id, "retry", t0); err == nil {
		t.Error("an unknown event was written")
	}
	got, err := readWaits(dataDir, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].Open || got[0].Seconds != 90 || !got[1].Open || !got[2].Open {
		t.Fatalf("waits = %+v, want one closed 90 s wait then two open", got)
	}
}

func TestLockWaitOfAnEndedRunWithoutAcquiredIsOpen(t *testing.T) {
	var rt RunTime
	fillFromFeed(&rt, []progress.Event{
		{Ts: "2026-10-01T10:00:00.000Z", Source: "factory", Stage: "repository_lock", Event: "waiting"},
		{Ts: "2026-10-01T10:05:00.000Z", Source: "factory", Stage: "build", Event: "start"},
	}, true)
	if len(rt.LockWaits) != 1 || !rt.LockWaits[0].Open || rt.LockWaits[0].Ongoing || rt.LockSeconds != 0 {
		t.Errorf("lock waits = %+v, want one open, not ongoing", rt.LockWaits)
	}
}

func TestLockWaitOfARunInProgressIsOngoing(t *testing.T) {
	var rt RunTime
	fillFromFeed(&rt, []progress.Event{
		{Ts: "2026-10-01T10:00:00.000Z", Source: "factory", Stage: "repository_lock", Event: "waiting"},
		{Ts: "2026-10-01T10:05:00.000Z", Source: "factory", Stage: "build", Event: "start"},
	}, false)
	if len(rt.LockWaits) != 1 || rt.LockWaits[0].Open || !rt.LockWaits[0].Ongoing {
		t.Errorf("lock waits = %+v, want one ongoing", rt.LockWaits)
	}
}

func TestParallelStagesAreMarkedAndOtherUsesTheirUnion(t *testing.T) {
	rep, err := Compute("testdata/board-app", backendID)
	if err != nil {
		t.Fatal(err)
	}
	// Feed shape: review (17:21:45 to 17:22:40) runs inside the first challenge
	// stage (17:21:45 to 17:28:35).
	r := runByID(t, rep, backendID+"-001-20261031-120957-52457")
	parallel := map[string]bool{}
	var sum float64
	for _, s := range r.Stages {
		parallel[s.Stage] = s.Parallel
		sum += s.Seconds
	}
	if !parallel["review"] || !parallel["challenge"] || parallel["build"] || parallel["verify"] {
		t.Errorf("parallel = %v, want only review and challenge", parallel)
	}
	if sum+r.Other <= r.Seconds {
		t.Errorf("stages %.0f + other %.0f should exceed wall %.0f: review overlaps challenge", sum, r.Other, r.Seconds)
	}
	// Everything but the image/workspace gaps is covered: other is small, not
	// clamped to zero by the overlap.
	if r.Other > 10 {
		t.Errorf("other = %.1f s, want a few seconds", r.Other)
	}
}

func TestRetriedBuildsAreNumbered(t *testing.T) {
	runs := []RunTime{{Kind: "build"}, {Kind: "build"}, {Kind: "advice", Round: 1}}
	numberAttempts(runs)
	if runs[0].Attempt != 1 || runs[1].Attempt != 2 || runs[2].Attempt != 0 {
		t.Errorf("attempts = %d %d %d, want 1 2 0", runs[0].Attempt, runs[1].Attempt, runs[2].Attempt)
	}
}

func TestBranchLockWaitCountsAsAnInRunLockWaitOfItsOwnKind(t *testing.T) {
	var rt RunTime
	fillFromFeed(&rt, []progress.Event{
		{Ts: "2026-10-01T10:00:00.000Z", Source: "factory", Stage: "branch_lock", Event: "waiting"},
		{Ts: "2026-10-01T10:01:00.000Z", Source: "factory", Stage: "branch_lock", Event: "acquired"},
		{Ts: "2026-10-01T10:01:00.000Z", Source: "factory", Stage: "repository_lock", Event: "waiting"},
		{Ts: "2026-10-01T10:01:30.000Z", Source: "factory", Stage: "repository_lock", Event: "acquired"},
	}, true)
	if len(rt.LockWaits) != 2 || rt.LockWaits[0].Kind != KindBranchLock || rt.LockWaits[0].Seconds != 60 || rt.LockWaits[1].Kind != KindRepositoryLock || rt.LockSeconds != 90 {
		t.Errorf("lock waits = %+v (total %v), want a 60 s branch_lock then a 30 s repository_lock", rt.LockWaits, rt.LockSeconds)
	}
}

// Launches that overlap are paired by their detail: the stage's summed time
// counts each, its wall time runs from the first start to the last end.
func TestOverlappingLaunchesAreSummedAndTheirWallTimeIsShorter(t *testing.T) {
	at := func(sec int) string { return time.Date(2026, 11, 1, 10, 0, sec, 0, time.UTC).Format(time.RFC3339Nano) }
	ev := func(sec int, stage, event, detail string) progress.Event {
		return progress.Event{Ts: at(sec), Source: "factory", Stage: stage, Event: event, Detail: detail}
	}
	events := []progress.Event{
		ev(0, "build", "start", ""), ev(10, "build", "end", ""),
		ev(10, "tests_red_on_base", "start", "case 0 result"),
		ev(10, "tests_red_on_base", "start", "case 1 result"),
		ev(10, "tests_red_on_base", "start", "case 2 result"),
		ev(40, "tests_red_on_base", "end", "case 1 result"),
		ev(50, "tests_red_on_base", "end", "case 0 result"),
		ev(60, "tests_red_on_base", "end", "case 2 result"),
		ev(60, "challenge", "start", ""), ev(70, "challenge", "end", ""),
		ev(70, "challenge", "start", "case 0 challenge1"), ev(75, "challenge", "end", "case 0 challenge1"),
		ev(75, "gate", "start", "canonical_verify"), ev(78, "gate", "end", "canonical_verify"),
		ev(80, "finished", "end", ""),
	}
	var rt RunTime
	fillFromFeed(&rt, events, true)
	got := map[string]StageTime{}
	for _, s := range rt.Stages {
		got[s.Stage] = s
	}
	if s := got["tests_red_on_base"]; s.Seconds != 30+40+50 || s.LaunchSeconds != 120 || s.Launches != 3 || s.WallSeconds != 50 {
		t.Errorf("tests_red_on_base = %+v, want 120 s summed, 3 launches, 50 s wall", s)
	}
	if s := got["challenge"]; s.Seconds != 10+5 || s.LaunchSeconds != 5 || s.Launches != 1 || s.WallSeconds != 5 {
		t.Errorf("challenge = %+v, want 15 s stage time, 1 launch of 5 s (the job is not one), 5 s wall", s)
	}
	if s := got["gate"]; s.Launches != 0 || s.LaunchSeconds != 0 || s.Seconds != 3 {
		t.Errorf("gate = %+v, want 3 s and no launches: a gate's detail is its check", s)
	}
	if s := got["build"]; s.Launches != 0 || s.Seconds != 10 {
		t.Errorf("build = %+v, want 10 s and no launches", s)
	}
}
