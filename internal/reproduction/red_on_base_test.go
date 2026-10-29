package reproduction

import (
	"errors"
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

func TestJudgeChangedTestRefusals(t *testing.T) {
	goRoot := RootFacts{"go.mod": []byte("module x\n")}
	const goTestFile = "package p\n\nimport \"testing\"\n\nfunc TestA(t *testing.T) {}\n"
	if a, err := JudgeChangedTest(ChangedTest{Path: "p/a_test.go", Result: []byte(goTestFile), Root: goRoot}); err != nil || a.Runner != "go-test-json" || strings.Join(a.TestIDs, ",") != "TestA" || a.Path != "p/a_test.go" {
		t.Errorf("a new Go test file = %+v, %v; want accepted with TestA", a, err)
	}
	big := goTestFile + strings.Repeat("//\n", MaxContentBytes)
	for _, tc := range []struct {
		name string
		c    ChangedTest
		want error
	}{
		{"no runner for the toolchain", ChangedTest{Path: "p/a_test.go", Result: []byte(goTestFile)}, errNoRunner},
		{"a helper with no test", ChangedTest{Path: "p/helper_test.go", Result: []byte("package p\n\nfunc helper() {}\n"), Root: goRoot}, errNoTests},
		{"empty", ChangedTest{Path: "p/a_test.go", Root: goRoot}, errUnreadable},
		{"binary", ChangedTest{Path: "p/a_test.go", Result: []byte(goTestFile + "\x00"), Root: goRoot}, errUnreadable},
		{"a new file over the size cap", ChangedTest{Path: "p/a_test.go", Result: []byte(big), Root: goRoot}, errAddedTooBig},
		{"a new file with too many tests", ChangedTest{Path: "p/a_test.go", Result: []byte("package p\n\nimport \"testing\"\n" + manyGoTests(MaxTestIDs+1)), Root: goRoot}, errAddedTooMany},
		{"a parse failure", ChangedTest{Path: "p/a_test.go", Result: []byte("package p\nfunc TestA("), Root: goRoot}, errParse},
		{"under .factory", ChangedTest{Path: ".factory/a_test.go", Result: []byte(goTestFile), Root: goRoot}, errPathShape},
		{"a path with a parent directory", ChangedTest{Path: "../a_test.go", Result: []byte(goTestFile), Root: goRoot}, errPathShape},
		{"a change outside any test", ChangedTest{Path: "p/a_test.go", Result: []byte(goTestFile), Base: []byte(goTestFile + "// x\n"), Changes: LineChanges{Added: []LineRange{{Start: 1, End: 1}}}, Root: goRoot}, errNoJudged},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if a, err := JudgeChangedTest(tc.c); !errors.Is(err, tc.want) {
				t.Errorf("JudgeChangedTest() = %+v, %v; want %v", a, err, tc.want)
			}
		})
	}
}

// A file the base commit holds has no size or test-count cap, and only the
// change's own tests are judged: the added ones and the ones with a changed
// line in their span.
func TestJudgeChangedTestJudgesOnlyTheChangesOwnTests(t *testing.T) {
	goRoot := RootFacts{"go.mod": []byte("module x\n")}
	header := "package p\n\nimport \"testing\"\n\n"
	var base strings.Builder
	base.WriteString(header)
	for i := 0; i < 3*MaxTestIDs; i++ {
		base.WriteString("func Test" + string(rune('A'+i%26)) + string(rune('a'+i/26)) + "(t *testing.T) {\n\t_ = 1\n}\n\n")
	}
	baseSrc := base.String() + "// padding\n" + strings.Repeat("// padding\n", MaxContentBytes/10)
	if len(baseSrc) <= MaxContentBytes {
		t.Fatalf("the fixture is %d bytes, want over the new-file cap", len(baseSrc))
	}
	lineOf := func(src, needle string) int { return 1 + strings.Count(src[:strings.Index(src, needle)], "\n") }
	// Rewrite the body of TestBa, and add TestNew at the end.
	result := strings.Replace(baseSrc, "func TestBa(t *testing.T) {\n\t_ = 1\n}", "func TestBa(t *testing.T) {\n\t_ = 2\n}", 1) + "func TestNew(t *testing.T) {}\n"
	n := lineOf(result, "_ = 2")
	total := 1 + strings.Count(result, "\n")
	changes := LineChanges{Added: []LineRange{{Start: n, End: n}, {Start: total - 1, End: total - 1}}}
	got, err := JudgeChangedTest(ChangedTest{Path: "p/big_test.go", Result: []byte(result), Base: []byte(baseSrc), Changes: changes, Root: goRoot})
	if err != nil || strings.Join(got.TestIDs, ",") != "TestBa,TestNew" {
		t.Fatalf("JudgeChangedTest() = %+v, %v; want TestBa and TestNew only", got, err)
	}
	// Removing a line from inside TestAa judges it; removing one between tests does not.
	m := lineOf(baseSrc, "_ = 1")
	got, err = JudgeChangedTest(ChangedTest{Path: "p/big_test.go", Result: []byte(baseSrc), Base: []byte(baseSrc), Changes: LineChanges{DeletedAfter: []int{m}}, Root: goRoot})
	if err != nil || strings.Join(got.TestIDs, ",") != "TestAa" {
		t.Errorf("a deletion inside a test = %+v, %v; want TestAa", got, err)
	}
	between := lineOf(baseSrc, "func TestBa")
	if _, err = JudgeChangedTest(ChangedTest{Path: "p/big_test.go", Result: []byte(baseSrc), Base: []byte(baseSrc), Changes: LineChanges{DeletedAfter: []int{between - 1}}, Root: goRoot}); !errors.Is(err, errNoJudged) {
		t.Errorf("a deletion between tests: err = %v, want errNoJudged", err)
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
