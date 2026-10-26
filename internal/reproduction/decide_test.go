package reproduction

import "testing"

func res(o Outcome, kinds map[string]Kind) *RunResult { return &RunResult{Outcome: o, Tests: kinds} }

func fails(k Kind) *RunResult { return res(OutcomeFailed, map[string]Kind{"TestA": k}) }

const citedLiteral = "- `Parse(\"a=1\")` returns a map with key `a=1`."

// TestDecideBlocksOnlyOnTwoHeadFailures is the head half of SC-022's table:
// any head launch that is not a failure, or a failure of different tests on
// the two launches, is advice.
func TestDecideBlocksOnlyOnTwoHeadFailures(t *testing.T) {
	base := res(OutcomePassed, map[string]Kind{"TestA": KindPass})
	cases := []struct {
		name         string
		head1, head2 *RunResult
		want         Verdict
	}{
		{"compile on head", res(OutcomeCompile, map[string]Kind{"TestA": KindCompile}), nil, VerdictAdvice},
		{"report missing on head", res(OutcomeMissing, map[string]Kind{"TestA": KindMissing}), nil, VerdictAdvice},
		{"passes on head", res(OutcomePassed, map[string]Kind{"TestA": KindPass}), nil, VerdictAdvice},
		{"skipped on head", res(OutcomeInconclusive, map[string]Kind{"TestA": KindSkipped}), nil, VerdictAdvice},
		{"not staged on head", res(OutcomeUnstaged, nil), nil, VerdictAdvice},
		{"second launch not run", fails(KindFail), nil, VerdictAdvice},
		{"second launch passes", fails(KindFail), res(OutcomePassed, map[string]Kind{"TestA": KindPass}), VerdictAdvice},
		{"second launch compile", fails(KindFail), res(OutcomeCompile, map[string]Kind{"TestA": KindCompile}), VerdictAdvice},
		{"different tests fail", res(OutcomeFailed, map[string]Kind{"TestA": KindFail, "TestB": KindPass}),
			res(OutcomeFailed, map[string]Kind{"TestA": KindPass, "TestB": KindFail}), VerdictAdvice},
		{"fails twice", fails(KindFail), fails(KindFail), VerdictBlocks},
		{"error then timeout", fails(KindError), fails(KindTimeout), VerdictBlocks},
	}
	for _, c := range cases {
		d := Decide(Evidence{HoldsLiteral: NewLiteral(citedLiteral, []byte("x"), notInBase), Head1: c.head1, Head2: c.head2, Base: base})
		if d.Verdict != c.want || d.Reason == "" {
			t.Errorf("%s: %+v, want %s", c.name, d, c.want)
		}
	}
}

// notInBase is a base commit that holds no literal.
func notInBase(string) bool { return false }

// TestDecideSameBaseFailureIsAdviceUnlessLiteral is the base half: a base
// that does not run, passes, or fails another way blocks; the same failure
// on the base is advice unless the reproduction holds a backticked literal of
// the cited line that no tracked file of the base holds (an API name the base
// already has never counts, and an unknown base counts none); an unknown
// base, or one the reproduction could not be staged in, is advice ("base not
// checked").
func TestDecideSameBaseFailureIsAdviceUnlessLiteral(t *testing.T) {
	without := []byte("func TestA(t *testing.T) { Parse(\"b\") }")
	with := []byte("func TestA(t *testing.T) { Parse(\"a=1\") }")
	baseHas := func(lits ...string) func(string) bool {
		return func(l string) bool {
			for _, x := range lits {
				if x == l {
					return true
				}
			}
			return false
		}
	}
	cases := []struct {
		name    string
		base    *RunResult
		content []byte
		spec    string
		inBase  func(string) bool
		want    Verdict
	}{
		{"base unknown", nil, with, citedLiteral, notInBase, VerdictAdvice},
		{"base not staged", res(OutcomeUnstaged, nil), with, citedLiteral, notInBase, VerdictAdvice},
		{"base passes", res(OutcomePassed, map[string]Kind{"TestA": KindPass}), without, citedLiteral, notInBase, VerdictBlocks},
		{"base does not compile", res(OutcomeCompile, map[string]Kind{"TestA": KindCompile}), without, citedLiteral, notInBase, VerdictBlocks},
		{"base report missing", res(OutcomeMissing, map[string]Kind{"TestA": KindMissing}), with, citedLiteral, notInBase, VerdictAdvice},
		{"base fails another kind", fails(KindError), without, citedLiteral, notInBase, VerdictBlocks},
		{"base skips", res(OutcomeInconclusive, map[string]Kind{"TestA": KindSkipped}), without, citedLiteral, notInBase, VerdictBlocks},
		{"same failure, no literal in content", fails(KindFail), without, citedLiteral, notInBase, VerdictAdvice},
		{"same failure, literal in content, not in base", fails(KindFail), with, citedLiteral, notInBase, VerdictBlocks},
		{"same failure, only literal in content is in base", fails(KindFail), with, citedLiteral, baseHas("Parse(\"a=1\")", "a=1"), VerdictAdvice},
		{"same failure, one literal of two is in base", fails(KindFail), with, citedLiteral, baseHas("a=1"), VerdictBlocks},
		{"same failure, API name in base", fails(KindFail), []byte("toml.Marshal(v)"), "- `toml.Marshal` returns an error with `cycle`.", baseHas("toml.Marshal", "cycle"), VerdictAdvice},
		{"same failure, base unknown to the literal check", fails(KindFail), with, citedLiteral, nil, VerdictAdvice},
		{"same failure, line has no literal", fails(KindFail), with, "- Parse returns a map.", notInBase, VerdictAdvice},
		{"same failure, literal differs by a byte", fails(KindFail), []byte("Parse(\"a=2\")"), "- `Parse(\"a=1\")` returns.", notInBase, VerdictAdvice},
		{"same failure, literal differs in case", fails(KindFail), []byte("parse(\"a=1\")"), "- `Parse(\"a=1\")` returns.", notInBase, VerdictAdvice},
	}
	for _, c := range cases {
		d := Decide(Evidence{HoldsLiteral: NewLiteral(c.spec, c.content, c.inBase), Head1: fails(KindFail), Head2: fails(KindFail), Base: c.base})
		if d.Verdict != c.want {
			t.Errorf("%s: %+v, want %s", c.name, d, c.want)
		}
	}
}

func TestDecideSameBaseNeedsEveryFailingIDTheSame(t *testing.T) {
	two := res(OutcomeFailed, map[string]Kind{"TestA": KindFail, "TestB": KindFail})
	base := res(OutcomeFailed, map[string]Kind{"TestA": KindFail, "TestB": KindPass})
	d := Decide(Evidence{Head1: two, Head2: two, Base: base})
	if d.Verdict != VerdictBlocks || len(d.TestIDs) != 2 {
		t.Errorf("one ID that passes on the base: %+v, want blocks on both IDs", d)
	}
	d = Decide(Evidence{Head1: two, Head2: two, Base: two})
	if d.Verdict != VerdictAdvice {
		t.Errorf("both IDs fail the same on the base: %+v, want advice", d)
	}
}

// TestRecheckAnythingButAPassIsStillFailing: after a corrective build every
// ID must pass, and a result commit holding the path fails without a launch.
func TestRecheckAnythingButAPassIsStillFailing(t *testing.T) {
	ids := []string{"TestA", "TestB"}
	pass := res(OutcomePassed, map[string]Kind{"TestA": KindPass, "TestB": KindPass})
	cases := []struct {
		name   string
		r      *RunResult
		inTree bool
		want   Verdict
	}{
		{"every ID passes", pass, false, VerdictPasses},
		{"path in the result commit", pass, true, VerdictStillFailing},
		{"not run", nil, false, VerdictStillFailing},
		{"one fails", res(OutcomeFailed, map[string]Kind{"TestA": KindPass, "TestB": KindFail}), false, VerdictStillFailing},
		{"one skipped", res(OutcomeInconclusive, map[string]Kind{"TestA": KindPass, "TestB": KindSkipped}), false, VerdictStillFailing},
		{"one missing from the report", res(OutcomeInconclusive, map[string]Kind{"TestA": KindPass}), false, VerdictStillFailing},
		{"compile", res(OutcomeCompile, map[string]Kind{"TestA": KindCompile, "TestB": KindCompile}), false, VerdictStillFailing},
		{"report missing", res(OutcomeMissing, map[string]Kind{"TestA": KindMissing, "TestB": KindMissing}), false, VerdictStillFailing},
	}
	for _, c := range cases {
		if d := Recheck(c.r, ids, c.inTree); d.Verdict != c.want {
			t.Errorf("%s: %+v, want %s", c.name, d, c.want)
		}
	}
	if d := Recheck(pass, nil, false); d.Verdict != VerdictStillFailing {
		t.Errorf("no IDs: %+v", d)
	}
}
