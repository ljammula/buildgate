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
		{"second launch not run", fails(KindFail), nil, VerdictAdvice},
		{"second launch passes", fails(KindFail), res(OutcomePassed, map[string]Kind{"TestA": KindPass}), VerdictAdvice},
		{"second launch compile", fails(KindFail), res(OutcomeCompile, map[string]Kind{"TestA": KindCompile}), VerdictAdvice},
		{"different tests fail", res(OutcomeFailed, map[string]Kind{"TestA": KindFail, "TestB": KindPass}),
			res(OutcomeFailed, map[string]Kind{"TestA": KindPass, "TestB": KindFail}), VerdictAdvice},
		{"fails twice", fails(KindFail), fails(KindFail), VerdictBlocks},
		{"error then timeout", fails(KindError), fails(KindTimeout), VerdictBlocks},
	}
	for _, c := range cases {
		d := Decide(Evidence{SpecText: citedLiteral, Content: []byte("x"), Head1: c.head1, Head2: c.head2, Base: base})
		if d.Verdict != c.want || d.Reason == "" {
			t.Errorf("%s: %+v, want %s", c.name, d, c.want)
		}
	}
}

// TestDecideSameBaseFailureIsAdviceUnlessLiteral is the base half: a base
// that does not run, passes, or fails another way blocks; the same failure
// on the base is advice unless the cited line's backticked literal is in the
// reproduction; an unknown base is advice.
func TestDecideSameBaseFailureIsAdviceUnlessLiteral(t *testing.T) {
	without := []byte("func TestA(t *testing.T) { Parse(\"b\") }")
	with := []byte("func TestA(t *testing.T) { Parse(\"a=1\") }")
	cases := []struct {
		name    string
		base    *RunResult
		content []byte
		spec    string
		want    Verdict
	}{
		{"base unknown", nil, with, citedLiteral, VerdictAdvice},
		{"base passes", res(OutcomePassed, map[string]Kind{"TestA": KindPass}), without, citedLiteral, VerdictBlocks},
		{"base does not compile", res(OutcomeCompile, map[string]Kind{"TestA": KindCompile}), without, citedLiteral, VerdictBlocks},
		{"base report missing", res(OutcomeMissing, map[string]Kind{"TestA": KindMissing}), with, citedLiteral, VerdictAdvice},
		{"base fails another kind", fails(KindError), without, citedLiteral, VerdictBlocks},
		{"base skips", res(OutcomeInconclusive, map[string]Kind{"TestA": KindSkipped}), without, citedLiteral, VerdictBlocks},
		{"same failure, no literal in content", fails(KindFail), without, citedLiteral, VerdictAdvice},
		{"same failure, literal in content", fails(KindFail), with, citedLiteral, VerdictBlocks},
		{"same failure, line has no literal", fails(KindFail), with, "- Parse returns a map.", VerdictAdvice},
		{"same failure, literal differs by a byte", fails(KindFail), []byte("Parse(\"a=2\")"), "- `Parse(\"a=1\")` returns.", VerdictAdvice},
	}
	for _, c := range cases {
		d := Decide(Evidence{SpecText: c.spec, Content: c.content, Head1: fails(KindFail), Head2: fails(KindFail), Base: c.base})
		if d.Verdict != c.want {
			t.Errorf("%s: %+v, want %s", c.name, d, c.want)
		}
	}
}

func TestDecideSameBaseNeedsEveryFailingIDTheSame(t *testing.T) {
	two := res(OutcomeFailed, map[string]Kind{"TestA": KindFail, "TestB": KindFail})
	base := res(OutcomeFailed, map[string]Kind{"TestA": KindFail, "TestB": KindPass})
	d := Decide(Evidence{SpecText: "- plain line", Head1: two, Head2: two, Base: base})
	if d.Verdict != VerdictBlocks || len(d.TestIDs) != 2 {
		t.Errorf("one ID that passes on the base: %+v, want blocks on both IDs", d)
	}
	d = Decide(Evidence{SpecText: "- plain line", Head1: two, Head2: two, Base: two})
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
