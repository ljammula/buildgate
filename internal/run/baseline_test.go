package run

import (
	"strings"
	"testing"
)

func TestBaselineVerifySummaryNamesTheFailingTest(t *testing.T) {
	for _, tc := range []struct {
		name string
		b    *BaselineVerify
		want string
	}{
		{"none", nil, ""},
		{"passed", &BaselineVerify{Passed: true}, "passed"},
		{"no test named", &BaselineVerify{ExitCode: 127}, "failed: exit 127"},
		{"no test named, first error", &BaselineVerify{ExitCode: 127, FirstError: "sh: 1: tox: not found"}, `failed: exit 127; first error in log: "sh: 1: tox: not found"`},
		{"needs what the ticket creates", &BaselineVerify{ExitCode: 1, NeedsCreated: "tests", NamedAs: []string{"tests"}, FirstError: "ImportError: x", Expected: true}, "failed as the ticket expects: the command needs tests, which the ticket creates"},
		{"expected, one", &BaselineVerify{ExitCode: 1, FailingTests: []string{"TestA"}, FailingCount: 1, Expected: true}, "failed as the ticket expects: TestA"},
		{"expected, several", &BaselineVerify{ExitCode: 1, FailingTests: []string{"TestA", "TestB"}, FailingCount: 2, Expected: true}, "failed as the ticket expects: TestA and 1 more"},
		{"one, unnamed", &BaselineVerify{ExitCode: 1, FailingTests: []string{"TestA"}, FailingCount: 1, Unnamed: []string{"TestA"}, UnnamedCount: 1}, "failed: TestA; the ticket does not name it"},
		{"all unnamed", &BaselineVerify{ExitCode: 1, FailingTests: []string{"TestA", "TestB"}, FailingCount: 15, Unnamed: []string{"TestA", "TestB"}, UnnamedCount: 15}, "failed: TestA and 14 more; the ticket names none of them"},
		{"some unnamed", &BaselineVerify{ExitCode: 1, FailingTests: []string{"TestA", "TestB", "TestC"}, FailingCount: 3, Unnamed: []string{"TestB", "TestC"}, UnnamedCount: 2}, "failed: TestA and 2 more; the ticket does not name TestB and 1 more"},
	} {
		if got := tc.b.Summary(); got != tc.want {
			t.Errorf("%s: Summary() = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestBaselineVerifyHaltsOnlyOnAFailureTheTicketDoesNotName(t *testing.T) {
	var none *BaselineVerify
	if none.Halts() || (&BaselineVerify{Passed: true}).Halts() || (&BaselineVerify{Expected: true}).Halts() {
		t.Error("a missing, passed or expected baseline halts the run")
	}
	b := &BaselineVerify{ExitCode: 1, BaseSHA: "0123456789abcdef0123"}
	if !b.Halts() {
		t.Error("a failed baseline the ticket does not expect lets the run go on")
	}
	if msg := b.HaltMessage(); !strings.HasPrefix(msg, "baseline verify failed: exit 1. No model call was made") || !strings.Contains(msg, "(0123456789ab)") {
		t.Errorf("halt message = %q", msg)
	}
}

// TestARunCarriesItsBaselineRecordOnLoadAndPersist: the Activity writes the
// record beside run.json; the run record gains it on the next read or
// write, whichever path the run ended by.
func TestARunCarriesItsBaselineRecordOnLoadAndPersist(t *testing.T) {
	dataDir := t.TempDir()
	r := &Run{ID: "run-1", State: StateSliceRunning}
	if err := r.Persist(dataDir); err != nil {
		t.Fatal(err)
	}
	if loaded, err := Load(dataDir, "run-1"); err != nil || loaded.BaselineVerify != nil {
		t.Fatalf("before the baseline ran: %+v, %v", loaded, err)
	}
	want := &BaselineVerify{Command: "make verify", ExitCode: 1, FailingTests: []string{"TestA"}, FailingCount: 1, Unnamed: []string{"TestA"}, UnnamedCount: 1}
	if err := SaveBaselineVerify(Dir(dataDir, "run-1"), want); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(dataDir, "run-1")
	if err != nil || loaded.BaselineVerify == nil || loaded.BaselineVerify.Summary() != want.Summary() {
		t.Fatalf("a run still building does not show its baseline: %+v, %v", loaded, err)
	}
	// The in-memory record of the process that started the run predates the
	// baseline; its terminal save must not drop it.
	r.State = StateHalted
	if err := r.Persist(dataDir); err != nil {
		t.Fatal(err)
	}
	if r.BaselineVerify == nil || r.BaselineVerify.FailingCount != 1 {
		t.Errorf("Persist left the run without its baseline: %+v", r.BaselineVerify)
	}
}
