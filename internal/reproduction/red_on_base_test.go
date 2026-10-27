package reproduction

import (
	"strings"
	"testing"
)

func kinds(k Kind) *RunResult {
	return &RunResult{Outcome: outcomeOf(map[string]Kind{"TestA": k}, []string{"TestA"}), Tests: map[string]Kind{"TestA": k}}
}

// The tests_red_on_base table, one test ID: red on base needs a pass on the
// result and a failure, timeout or compile error on the base.
func TestDecideRedOnBaseOneTest(t *testing.T) {
	for _, tc := range []struct {
		name         string
		result, base *RunResult
		want         string
	}{
		{"fails on the base", kinds(KindPass), kinds(KindFail), RedProved},
		{"errors on the base", kinds(KindPass), kinds(KindError), RedProved},
		{"times out on the base", kinds(KindPass), kinds(KindTimeout), RedProved},
		{"does not compile on the base", kinds(KindPass), kinds(KindCompile), RedProved},
		{"passes on both", kinds(KindPass), kinds(KindPass), RedNotExercised},
		{"fails on the result", kinds(KindFail), kinds(KindFail), RedNoVerdict},
		{"times out on the result", kinds(KindTimeout), nil, RedNoVerdict},
		{"the base left no report", kinds(KindPass), kinds(KindMissing), RedNoVerdict},
		{"skipped on the base", kinds(KindPass), kinds(KindSkipped), RedNoVerdict},
		{"no launch on the base", kinds(KindPass), nil, RedNoVerdict},
		{"the result left no report", kinds(KindMissing), kinds(KindPass), RedNoVerdict},
		{"the result did not compile", kinds(KindCompile), kinds(KindPass), RedNoVerdict},
		{"no launch at all", nil, nil, RedNoVerdict},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := DecideRedOnBase([]FileRuns{{IDs: []string{"TestA"}, Result: tc.result, Base: tc.base}})
			if got.Verdict != tc.want || got.Reason == "" {
				t.Errorf("DecideRedOnBase() = %+v, want verdict %s and a reason", got, tc.want)
			}
		})
	}
}

// Several tests and files: one red test proves the change whatever else
// passes on the base; a test that fails on the result refuses whatever else
// is red; tests that pass on the base are refused only when none is left
// without evidence.
func TestDecideRedOnBaseSeveralTestsAndFiles(t *testing.T) {
	two := func(a, b Kind) *RunResult {
		return &RunResult{Outcome: OutcomePassed, Tests: map[string]Kind{"TestA": a, "TestB": b}}
	}
	ids := []string{"TestA", "TestB"}
	for _, tc := range []struct {
		name  string
		files []FileRuns
		want  string
	}{
		{"one red among passing tests", []FileRuns{{IDs: ids, Result: two(KindPass, KindPass), Base: two(KindFail, KindPass)}}, RedProved},
		{"red in the second file", []FileRuns{
			{IDs: ids, Result: two(KindPass, KindPass), Base: two(KindPass, KindPass)},
			{IDs: ids, Result: two(KindPass, KindPass), Base: two(KindCompile, KindCompile)},
		}, RedProved},
		{"a red test still proves it when another fails alone on the result", []FileRuns{{IDs: ids, Result: two(KindPass, KindFail), Base: two(KindFail, KindFail)}}, RedProved},
		{"tests passing on the base are not refused while another fails alone on the result", []FileRuns{{IDs: ids, Result: two(KindPass, KindFail), Base: two(KindPass, KindFail)}}, RedNoVerdict},
		{"passing everywhere", []FileRuns{{IDs: ids, Result: two(KindPass, KindPass), Base: two(KindPass, KindPass)}}, RedNotExercised},
		{"passing, with one test unknown on the base", []FileRuns{{IDs: ids, Result: two(KindPass, KindPass), Base: two(KindPass, KindMissing)}}, RedNoVerdict},
		{"passing, with a second file that never ran", []FileRuns{
			{IDs: ids, Result: two(KindPass, KindPass), Base: two(KindPass, KindPass)},
			{IDs: []string{"TestC"}},
		}, RedNoVerdict},
		{"no files", nil, RedNoVerdict},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := DecideRedOnBase(tc.files); got.Verdict != tc.want {
				t.Errorf("DecideRedOnBase() = %+v, want %s", got, tc.want)
			}
		})
	}
}

func TestCheckChangedTest(t *testing.T) {
	goRoot := RootFacts{"go.mod": []byte("module x\n")}
	const goTestFile = "package p\n\nimport \"testing\"\n\nfunc TestA(t *testing.T) {}\n"
	if a, err := CheckChangedTest("p/a_test.go", []byte(goTestFile), goRoot); err != nil || a.Runner != "go-test-json" || strings.Join(a.TestIDs, ",") != "TestA" || a.Path != "p/a_test.go" {
		t.Errorf("a Go test file = %+v, %v; want accepted with TestA", a, err)
	}
	for _, tc := range []struct {
		name, path, content string
		root                RootFacts
	}{
		{"no runner for the toolchain", "p/a_test.go", goTestFile, RootFacts{}},
		{"a helper with no test", "p/helper_test.go", "package p\n\nfunc helper() {}\n", goRoot},
		{"empty", "p/a_test.go", "", goRoot},
		{"binary", "p/a_test.go", goTestFile + "\x00", goRoot},
		{"over the size cap", "p/a_test.go", goTestFile + strings.Repeat("//\n", MaxContentBytes), goRoot},
		{"under .factory", ".factory/a_test.go", goTestFile, goRoot},
		{"a path with a parent directory", "../a_test.go", goTestFile, goRoot},
		{"too many tests", "p/a_test.go", "package p\n\nimport \"testing\"\n" + manyGoTests(MaxTestIDs+1), goRoot},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if a, err := CheckChangedTest(tc.path, []byte(tc.content), tc.root); err == nil {
				t.Errorf("CheckChangedTest() = %+v, want a refusal", a)
			}
		})
	}
}

func manyGoTests(n int) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		b.WriteString("func Test")
		b.WriteString(string(rune('A' + i)))
		b.WriteString("(t *testing.T) {}\n")
	}
	return b.String()
}

func TestDecideRedOnBaseNamesAFailureOnTheResultAlone(t *testing.T) {
	got := DecideRedOnBase([]FileRuns{{IDs: []string{"TestA"}, Result: kinds(KindFail), Base: kinds(KindFail)}})
	if got.Verdict != RedNoVerdict || got.Reason != FailedAloneReason {
		t.Errorf("DecideRedOnBase() = %+v, want no verdict with the fixed reason", got)
	}
}
