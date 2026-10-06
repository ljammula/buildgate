package policy

import (
	"reflect"
	"testing"

	"buildgate/internal/run"
)

const (
	oraclePath = "internal/mood/zz_oracle_test.go"
	oracleHash = "aaaa"
)

func oracleEvidence() *run.OracleEvidence {
	return &run.OracleEvidence{
		Authored:     []run.OracleFile{{Path: oraclePath, SHA256: oracleHash}, {Path: ".buildgate/oracles.json", SHA256: "iiii"}},
		Deleted:      []string{"internal/mood/old_oracle_test.go"},
		ResultSHA256: map[string]string{oraclePath: oracleHash, ".buildgate/oracles.json": "iiii"},
	}
}

func TestExcludeFactoryOraclesIsHashKeyed(t *testing.T) {
	changed := []string{"internal/mood/mood.go", oraclePath, ".buildgate/oracles.json", "internal/mood/old_oracle_test.go"}
	got := ExcludeFactoryOracles(changed, oracleEvidence())
	if want := []string{"internal/mood/mood.go"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("hash-equal oracle paths and the declared deletion must be excluded: got %v want %v", got, want)
	}

	// Same path, DIFFERENT bytes at ResultSHA (e.g. the agent's own write
	// survived): not exempt.
	tampered := oracleEvidence()
	tampered.ResultSHA256[oraclePath] = "bbbb"
	got = ExcludeFactoryOracles(changed, tampered)
	if !reflect.DeepEqual(got, []string{"internal/mood/mood.go", oraclePath}) {
		t.Fatalf("same path with different bytes must stay in the inventory: got %v", got)
	}

	// Path never recorded at ResultSHA at all: not exempt.
	missing := oracleEvidence()
	delete(missing.ResultSHA256, oraclePath)
	if got := ExcludeFactoryOracles(changed, missing); !contains(got, oraclePath) {
		t.Fatalf("absent-blob write exempted: %v", got)
	}

	// Deletion exemption needs the path to be actually absent.
	stillThere := oracleEvidence()
	stillThere.ResultSHA256["internal/mood/old_oracle_test.go"] = "cccc"
	if got := ExcludeFactoryOracles(changed, stillThere); !contains(got, "internal/mood/old_oracle_test.go") {
		t.Fatalf("a 'deleted' path still present at ResultSHA was exempted: %v", got)
	}

	// An agent deletion of a path the manifest did NOT supersede is not exempt.
	if got := ExcludeFactoryOracles([]string{"internal/mood/other_oracle_test.go"}, oracleEvidence()); len(got) != 1 {
		t.Fatalf("undeclared deletion exempted: %v", got)
	}
}

func TestExcludeFactoryOraclesInertWithoutEvidence(t *testing.T) {
	changed := []string{"a.go", oraclePath}
	if got := ExcludeFactoryOracles(changed, nil); !reflect.DeepEqual(got, changed) {
		t.Fatalf("nil evidence changed the inventory: %v", got)
	}
	// Base-index-only evidence (no factory writes) never exempts anything.
	if got := ExcludeFactoryOracles(changed, &run.OracleEvidence{BaseIndexPaths: []string{oraclePath}}); !reflect.DeepEqual(got, changed) {
		t.Fatalf("base-index-only evidence exempted a path: %v", got)
	}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func baseInput(changed []string) EvaluateRunInput {
	return EvaluateRunInput{
		VerifyCommand:  []string{"sh", "-c", "make verify"},
		ChangedFiles:   changed,
		AllowedFiles:   []string{"internal/mood/mood.go"},
		TestPatterns:   nil,
		BuildExitCode:  0,
		VerifyExitCode: 0,
	}
}

func gateByName(res EvaluateRunResult, name string) (run.GateResult, bool) {
	for _, g := range res.GateResults {
		if g.Check == name {
			return g, true
		}
	}
	return run.GateResult{}, false
}

func TestEvaluateRunDiffScopeIgnoresOnlyHashVerifiedOracles(t *testing.T) {
	changed := []string{"internal/mood/mood.go", "internal/mood/mood_test.go", oraclePath, ".buildgate/oracles.json"}
	in := baseInput(append([]string{}, changed...))
	in.AllowedFiles = append(in.AllowedFiles, "internal/mood/mood_test.go")

	// Without evidence the oracle paths are out of scope: control.
	if res := EvaluateRun(in); res.Accepted {
		t.Fatal("control: oracle paths outside Allowed-Files must fail diff_scope without evidence")
	}
	in.Oracles = oracleEvidence()
	res := EvaluateRun(in)
	if g, _ := gateByName(res, "diff_scope"); !g.Passed {
		t.Fatalf("diff_scope failed despite hash-verified oracles: violations=%v", res.DiffScopeViolations)
	}

	// Same path, different bytes: NOT exempt (the hash keys the exemption).
	in.Oracles = oracleEvidence()
	in.Oracles.ResultSHA256[oraclePath] = "bbbb"
	res = EvaluateRun(in)
	if g, _ := gateByName(res, "diff_scope"); g.Passed || !contains(res.DiffScopeViolations, oraclePath) {
		t.Fatalf("diff_scope must fail on a same-path/different-bytes oracle: %+v", res.DiffScopeViolations)
	}
}

func TestEvaluateRunTestsAddedNotSatisfiedByFactoryOracle(t *testing.T) {
	// The agent added no test; the factory's oracle file is the only *_test.go.
	in := baseInput([]string{"internal/mood/mood.go", oraclePath, ".buildgate/oracles.json"})
	in.AllowedFiles = nil
	in.Oracles = oracleEvidence()
	res := EvaluateRun(in)
	if g, _ := gateByName(res, "tests_added"); g.Passed {
		t.Fatal("tests_added was satisfied by the factory-authored oracle")
	}
	// Without the evidence (today's behaviour) the same inventory passes.
	in.Oracles = nil
	if g, _ := gateByName(EvaluateRun(in), "tests_added"); !g.Passed {
		t.Fatal("control: without evidence the _test.go file counts, as before")
	}
	// The agent's own test still satisfies it.
	in.Oracles = oracleEvidence()
	in.ChangedFiles = append(in.ChangedFiles, "internal/mood/mood_test.go")
	if g, _ := gateByName(EvaluateRun(in), "tests_added"); !g.Passed {
		t.Fatal("agent-authored test no longer satisfies tests_added")
	}
}

func TestEvaluateRunRequiredFilesChangedUnaffectedByOracles(t *testing.T) {
	in := baseInput([]string{oraclePath})
	in.AllowedFiles = nil
	in.RequiredChangedFiles = []string{oraclePath}
	in.Oracles = oracleEvidence()
	res := EvaluateRun(in)
	if g, ok := gateByName(res, "required_files_changed"); !ok || !g.Passed {
		t.Fatalf("required_files_changed must still see the oracle path: %+v", res.GateResults)
	}
}
