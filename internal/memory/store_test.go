package memory

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

func openTest(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.TempDir(), "proj")
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func mkLesson(t *testing.T, n int, state State, at string) Lesson {
	t.Helper()
	l, err := NewLesson(fmt.Sprintf("Keep rule number %d", n), SourceAgent, "2026-10-09T00:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	l.State = state
	if at != "" {
		l.History = []Transition{{To: state, At: at}}
	}
	return l
}

func TestStoreMissingAndRoundTrip(t *testing.T) {
	s := openTest(t)
	st, err := s.Load()
	if err != nil || st.SchemaVersion != 1 || len(st.Lessons) != 0 {
		t.Fatalf("missing: %+v %v", st, err)
	}
	l := mkLesson(t, 1, StateCandidate, "")
	if err := s.Update(func(st *StoreState) error {
		st.Lessons = append(st.Lessons, l)
		st.CountedRuns = []string{"r1"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	got, err := s.Load()
	if err != nil || len(got.Lessons) != 1 || got.Lessons[0].ID != l.ID || got.CountedRuns[0] != "r1" {
		t.Fatalf("round trip: %+v %v", got, err)
	}
	info, _ := os.Stat(filepath.Join(s.Dir(), "state.json"))
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", info.Mode())
	}
	if m, _ := filepath.Glob(filepath.Join(s.Dir(), "*.tmp")); len(m) != 0 {
		t.Fatalf("temp files left: %v", m)
	}
	di, _ := os.Stat(s.Dir())
	if di.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode %v", di.Mode())
	}
}

func TestStoreRefusesBadFiles(t *testing.T) {
	for name, content := range map[string]string{
		"unknown field": `{"schema_version":1,"lessons":[],"extra":1}`,
		"wrong schema":  `{"schema_version":2,"lessons":[]}`,
		"oversize":      `{"schema_version":1,"pad":"` + strings.Repeat("x", MaxStateBytes) + `"}`,
	} {
		s := openTest(t)
		if err := os.WriteFile(filepath.Join(s.Dir(), "state.json"), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Load(); !errors.Is(err, ErrStore) {
			t.Errorf("%s: Load = %v", name, err)
		}
		if err := s.Update(func(*StoreState) error { return nil }); !errors.Is(err, ErrStore) {
			t.Errorf("%s: Update = %v", name, err)
		}
	}
}

func TestStoreBounds(t *testing.T) {
	s := openTest(t)
	err := s.Update(func(st *StoreState) error {
		for i := 0; i < 150; i++ {
			st.Lessons = append(st.Lessons, mkLesson(t, i, StateProposed, ""))
		}
		for i := 150; i < 200; i++ {
			st.Lessons = append(st.Lessons, mkLesson(t, i, StateDropped, fmt.Sprintf("2026-01-%02dT00:00:00Z", i-149)))
		}
		st.Lessons = append(st.Lessons, mkLesson(t, 200, StateCandidate, ""))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	st, _ := s.Load()
	if len(st.Lessons) != 200 {
		t.Fatalf("lessons %d", len(st.Lessons))
	}
	for _, l := range st.Lessons {
		if l.Reason == "Keep rule number 150" {
			t.Fatal("oldest dropped lesson survived")
		}
	}
	err = s.Update(func(st *StoreState) error {
		st.Lessons = append(st.Lessons, mkLesson(t, 300, StateCandidate, ""))
		return nil
	})
	if err != nil {
		t.Fatalf("one more with dropped present: %v", err)
	}
	// Only active lessons: 201 refused, file unchanged.
	s2 := openTest(t)
	err = s2.Update(func(st *StoreState) error {
		for i := 0; i < 201; i++ {
			st.Lessons = append(st.Lessons, mkLesson(t, i, StateCandidate, ""))
		}
		return nil
	})
	if !errors.Is(err, ErrStore) {
		t.Fatalf("201 active: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(s2.Dir(), "state.json")); !os.IsNotExist(statErr) {
		t.Fatal("refused update wrote a file")
	}
	err = s2.Update(func(st *StoreState) error {
		l := mkLesson(t, 1, StateCandidate, "")
		for i := 0; i < MaxLessonRuns+1; i++ {
			l.Runs = append(l.Runs, fmt.Sprintf("run-%d", i))
		}
		st.Lessons = append(st.Lessons, l)
		return nil
	})
	if !errors.Is(err, ErrStore) {
		t.Fatalf("too many runs on a lesson: %v", err)
	}
}

func TestStoreCountedRunsCapped(t *testing.T) {
	s := openTest(t)
	_ = s.Update(func(st *StoreState) error {
		for i := 0; i < 2100; i++ {
			st.CountedRuns = append(st.CountedRuns, "r"+strconv.Itoa(i))
		}
		return nil
	})
	st, _ := s.Load()
	if len(st.CountedRuns) != MaxCountedRuns || st.CountedRuns[MaxCountedRuns-1] != "r2099" {
		t.Fatalf("cursor %d", len(st.CountedRuns))
	}
}

func TestStoreFailedUpdateKeepsOldFile(t *testing.T) {
	s := openTest(t)
	_ = s.Update(func(st *StoreState) error { st.CountedRuns = []string{"keep"}; return nil })
	boom := errors.New("boom")
	err := s.Update(func(st *StoreState) error { st.CountedRuns = []string{"lost"}; return boom })
	if !errors.Is(err, boom) {
		t.Fatal(err)
	}
	st, _ := s.Load()
	if st.CountedRuns[0] != "keep" {
		t.Fatalf("old file replaced: %v", st.CountedRuns)
	}
	if m, _ := filepath.Glob(filepath.Join(s.Dir(), "*.tmp")); len(m) != 0 {
		t.Fatalf("temp files left: %v", m)
	}
}

func increment(s *Store) error {
	return s.Update(func(st *StoreState) error {
		st.CountedRuns = append(st.CountedRuns, "x")
		return nil
	})
}

func TestStoreConcurrentGoroutines(t *testing.T) {
	s := openTest(t)
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := increment(s); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	st, _ := s.Load()
	if len(st.CountedRuns) != 100 {
		t.Fatalf("lost updates: %d", len(st.CountedRuns))
	}
}

// TestStoreHelperProcess is the child of TestStoreConcurrentProcesses.
func TestStoreHelperProcess(t *testing.T) {
	dir := os.Getenv("MEMORY_STORE_HELPER_DIR")
	if dir == "" {
		t.Skip("helper process only")
	}
	s, err := Open(dir, "proj")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 100; i++ {
		if err := increment(s); err != nil {
			t.Fatal(err)
		}
	}
}

func TestStoreConcurrentProcesses(t *testing.T) {
	dir := t.TempDir()
	var cmds []*exec.Cmd
	for i := 0; i < 2; i++ {
		c := exec.Command(os.Args[0], "-test.run=^TestStoreHelperProcess$", "-test.count=1")
		c.Env = append(os.Environ(), "MEMORY_STORE_HELPER_DIR="+dir)
		if err := c.Start(); err != nil {
			t.Fatal(err)
		}
		cmds = append(cmds, c)
	}
	for _, c := range cmds {
		if err := c.Wait(); err != nil {
			t.Fatal(err)
		}
	}
	s, _ := Open(dir, "proj")
	st, err := s.Load()
	if err != nil || len(st.CountedRuns) != 200 {
		t.Fatalf("lost updates across processes: %d %v", len(st.CountedRuns), err)
	}
}

func TestStoreOffMarker(t *testing.T) {
	s := openTest(t)
	if off, err := s.Off(); off || err != nil {
		t.Fatalf("fresh: %v %v", off, err)
	}
	if err := s.SetOff("kan\nna", "pause\x1b[31m it"); err != nil {
		t.Fatal(err)
	}
	if off, _ := s.Off(); !off {
		t.Fatal("marker missing")
	}
	data, _ := os.ReadFile(filepath.Join(s.Dir(), "off"))
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], ": ") || strings.ContainsAny(lines[0], "\x1b") {
		t.Fatalf("marker body %q", data)
	}
	if err := s.ClearOff(); err != nil {
		t.Fatal(err)
	}
	if err := s.ClearOff(); err != nil {
		t.Fatalf("second clear: %v", err)
	}
	if off, _ := s.Off(); off {
		t.Fatal("marker still there")
	}
}

func TestStoreHostileProjectName(t *testing.T) {
	d := t.TempDir()
	for _, p := range []string{"", ".", "..", ".hidden", "a/b", `a\b`, "../x", "a\x00b", "a\nb", strings.Repeat("a", 129)} {
		if _, err := Open(d, p); !errors.Is(err, ErrStore) {
			t.Errorf("Open(%q) = %v", p, err)
		}
	}
	if _, err := Open(d, strings.Repeat("a", 128)); err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(d); len(entries) != 1 || entries[0].Name() != "memory" {
		t.Fatalf("hostile names created entries: %v", entries)
	}
}

// Each rule of Reconcile: a line the section holds leaves the store whatever
// its state; a proposed lesson goes back to candidate only when its request
// is known and over; everything else stays as it was.
func TestReconcileRules(t *testing.T) {
	mk := func(n int, state State, req string) Lesson {
		l := mkLesson(t, n, state, "")
		l.RequestID = req
		return l
	}
	proposedIn := mk(1, StateProposed, "r-live")
	proposedGone := mk(2, StateProposed, "r-done")
	proposedActive := mk(3, StateProposed, "r-live")
	proposedUnknown := mk(4, StateProposed, "r-missing")
	candidateIn := mk(5, StateCandidate, "")
	droppedIn := mk(6, StateDropped, "")
	candidateOut := mk(7, StateCandidate, "")
	droppedOut := mk(8, StateDropped, "")
	st := &StoreState{Lessons: []Lesson{proposedIn, proposedGone, proposedActive, proposedUnknown, candidateIn, droppedIn, candidateOut, droppedOut}}
	section := []string{proposedIn.Line, candidateIn.Line, droppedIn.Line, "- a human line."}
	reqs := func(id string) (bool, bool) {
		switch id {
		case "r-live":
			return true, true
		case "r-done":
			return false, true
		}
		return false, false
	}
	Reconcile(st, section, reqs, "2026-10-09T00:00:00Z")
	want := map[string]State{
		proposedGone.ID: StateCandidate, proposedActive.ID: StateProposed, proposedUnknown.ID: StateProposed,
		candidateOut.ID: StateCandidate, droppedOut.ID: StateDropped,
	}
	if len(st.Lessons) != len(want) {
		t.Fatalf("%d lessons kept, want %d: %+v", len(st.Lessons), len(want), st.Lessons)
	}
	for _, l := range st.Lessons {
		if want[l.ID] != l.State {
			t.Errorf("lesson %q: %s, want %s", l.Reason, l.State, want[l.ID])
		}
	}
	back := st.Lessons[0]
	if back.ID != proposedGone.ID || back.RequestID != "" || len(back.History) != 1 || back.History[0].By != "reconcile" {
		t.Errorf("the lesson whose request ended: %+v", back)
	}
	if st.Lessons[1].RequestID != "r-live" || len(st.Lessons[1].History) != 0 {
		t.Errorf("a lesson of an active request was touched: %+v", st.Lessons[1])
	}
}

func TestGateOrderAndReasons(t *testing.T) {
	if err := Gate(true, false, false); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		on, marker, kill bool
		want             string
	}{
		{true, true, true, "kill switch"},
		{false, true, true, "kill switch"},
		{false, true, false, "memory.repositories"},
		{false, false, false, "memory.repositories"},
		{true, true, false, "factoryd memory on"},
	}
	for _, c := range cases {
		err := Gate(c.on, c.marker, c.kill)
		if !errors.Is(err, ErrMemoryOff) || !strings.Contains(err.Error(), c.want) {
			t.Errorf("Gate(%v,%v,%v) = %v, want %q", c.on, c.marker, c.kill, err, c.want)
		}
	}
}

// Two repositories that share a base name, or whose names differ only by
// case, have different stores, stop markers and proposal paths.
func TestStoreKeySeparatesRepositoriesWithOneName(t *testing.T) {
	a, b := StoreKey("api", "/clients/acme/api"), StoreKey("api", "/clients/beta/api")
	if a == b || !strings.HasPrefix(a, "api-") || len(a) != len("api-")+12 {
		t.Fatalf("keys %q and %q", a, b)
	}
	if StoreKey("api", "/clients/acme/api/") != a || StoreKey("api", "/clients/acme/./api") != a {
		t.Fatal("the key depends on how the root path is written")
	}
	upper, lower := StoreKey("API", "/src/API"), StoreKey("api", "/src/api")
	if upper == lower || !strings.HasPrefix(upper, "api-") || strings.EqualFold(upper, lower) {
		t.Fatalf("names differing by case share a store on a case-insensitive filesystem: %q %q", upper, lower)
	}
	if err := ValidProject(a); err != nil {
		t.Fatalf("a key is not a valid store name: %v", err)
	}
	data := t.TempDir()
	one, err := Open(data, a)
	if err != nil {
		t.Fatal(err)
	}
	other, err := Open(data, b)
	if err != nil {
		t.Fatal(err)
	}
	if err := one.SetOff("op", "pause"); err != nil {
		t.Fatal(err)
	}
	if off, _ := other.Off(); off {
		t.Fatal("one repository's stop marker stopped the other")
	}
	pa, _ := ProposalPath(data, a, "req-1")
	pb, _ := ProposalPath(data, b, "req-1")
	if pa == pb {
		t.Fatal("the two repositories share a proposal path")
	}
}

func TestChangesRoundTripBesideTheProposal(t *testing.T) {
	data := t.TempDir()
	key := StoreKey("app", "/src/app")
	path, err := ChangesPath(data, key, "req-1")
	if err != nil || filepath.Base(path) != "req-1.changes.json" || filepath.Base(filepath.Dir(path)) != "proposals" {
		t.Fatalf("path %q, %v", path, err)
	}
	if got, err := LoadChanges(path); err != nil || got != nil {
		t.Fatalf("missing file = %v, %v", got, err)
	}
	want := []Change{{Line: "- a.", Source: SourceAgent, Runs: []string{"r1"}}, {Remove: true, Line: "- b."}}
	if err := SaveChanges(path, want); err != nil {
		t.Fatal(err)
	}
	got, err := LoadChanges(path)
	if err != nil || len(got) != 2 || got[0].Runs[0] != "r1" || !got[1].Remove {
		t.Fatalf("round trip = %+v, %v", got, err)
	}
	if err := os.WriteFile(path, []byte(`[{"line":"x","extra":1}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadChanges(path); err == nil {
		t.Fatal("an unknown field was accepted")
	}
	if _, err := ChangesPath(data, "../x", "req-1"); err == nil {
		t.Fatal("a hostile key was accepted")
	}
}
