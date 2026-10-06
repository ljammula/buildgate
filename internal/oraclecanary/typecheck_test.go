package oraclecanary

import (
	"strings"
	"testing"
	"time"
)

func typeCheck(t *testing.T, files map[string]string, targets map[string]string) TypeCheckResult {
	t.Helper()
	in := map[string][]byte{}
	for name, src := range files {
		in[name] = []byte(src)
	}
	return TypeCheckGoOracles(in, targets)
}

func TestTypeCheckAcceptsUndefinedTargetSymbolsAndStandInImports(t *testing.T) {
	res := typeCheck(t, map[string]string{"oracle_001_test.go": `package mood_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"example.com/app/internal/mood"
	m2 "example.com/app/internal/other/v2"
	"github.com/some/go-thing"
)

func TestOracleParse(t *testing.T) {
	got, err := mood.Parse(strings.TrimSpace(" ALL "))
	if err != nil || got != mood.All {
		t.Fatalf("got %v %v", got, err)
	}
	var s mood.Store = m2.New(thing.Default)
	_ = s
	rec := httptest.NewRecorder()
	mood.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))
	if rec.Code != http.StatusOK {
		t.Fatal(rec.Code)
	}
}
`}, map[string]string{"oracle_001_test.go": "internal/mood/oracle_001_test.go"})
	if res.Skipped != "" || len(res.Problems) != 0 {
		t.Fatalf("want a clean pass, got skipped=%q problems=%v", res.Skipped, res.Problems)
	}
}

func TestTypeCheckFlagsNoValueCallUsedAsArgument(t *testing.T) {
	// The 2026-09-21 live defect: a function with no result used as a value.
	res := typeCheck(t, map[string]string{"oracle_001_test.go": `package p

import "testing"

func run(t *testing.T) {}

func expect(t *testing.T, v string) {}

func TestOracleX(t *testing.T) {
	expect(t, run(t))
}
`}, nil)
	if len(res.Problems) != 1 || !strings.Contains(res.Problems[0], "oracle_001_test.go:") || !strings.Contains(res.Problems[0], "no value") {
		t.Fatalf("want one no-value problem, got %v (skipped %q)", res.Problems, res.Skipped)
	}
}

func TestTypeCheckFlagsUnusedImportsVariablesAndStdlibMisuse(t *testing.T) {
	res := typeCheck(t, map[string]string{"oracle_001_test.go": `package p

import (
	"os"
	"strings"
	"testing"

	"example.com/app/dep"
	str "strings"
)

func TestOracleX(t *testing.T) {
	unused := 1
	_ = strings.NoSuchFunc("a")
	_ = str.AlsoMissing("a")
	_ = dep.Missing
}
`}, nil)
	joined := strings.Join(res.Problems, "\n")
	for _, want := range []string{`"os" imported and not used`, "declared and not used", "undefined: strings.NoSuchFunc", "undefined: str.AlsoMissing"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "dep") {
		t.Errorf("a stand-in import must not be reported:\n%s", joined)
	}
}

func TestTypeCheckGroupsFilesOfOnePackage(t *testing.T) {
	src := func(test string) string {
		return `package p

import "testing"

func helper() int { return 1 }

func ` + test + `(t *testing.T) { _ = helper() }
`
	}
	files := map[string]string{"oracle_001_test.go": src("TestOracleA"), "oracle_002_test.go": src("TestOracleB")}
	res := typeCheck(t, files, map[string]string{"oracle_001_test.go": "a/oracle_001_test.go", "oracle_002_test.go": "a/oracle_002_test.go"})
	if len(res.Problems) == 0 || !strings.Contains(strings.Join(res.Problems, "\n"), "redeclared") {
		t.Fatalf("want a redeclaration in one package, got %v", res.Problems)
	}
	// Different target directories are different packages: no clash.
	res = typeCheck(t, files, map[string]string{"oracle_001_test.go": "a/oracle_001_test.go", "oracle_002_test.go": "b/oracle_002_test.go"})
	if len(res.Problems) != 0 {
		t.Fatalf("different directories must not clash, got %v", res.Problems)
	}
}

func TestTypeCheckLeavesUnparseableFilesToTheParseCheck(t *testing.T) {
	res := typeCheck(t, map[string]string{"oracle_001_test.go": "package p\nfunc {"}, nil)
	if res.Skipped != "" || len(res.Problems) != 0 {
		t.Fatalf("got %+v", res)
	}
}

func TestTypeCheckBoundsTheProblemList(t *testing.T) {
	var b strings.Builder
	b.WriteString("package p\n\nimport \"testing\"\n\nfunc TestOracleX(t *testing.T) {\n")
	for i := 0; i < 30; i++ {
		b.WriteString("\tv" + strings.Repeat("x", i+1) + " := 1\n")
	}
	b.WriteString("}\n")
	res := typeCheck(t, map[string]string{"oracle_001_test.go": b.String()}, nil)
	if len(res.Problems) != typeCheckMaxProblems+1 || !strings.Contains(res.Problems[typeCheckMaxProblems], "more") {
		t.Fatalf("got %d problems: %v", len(res.Problems), res.Problems)
	}
}

func TestTypeCheckCoolsDownAfterATimeoutWithoutSpawningAnotherGoroutine(t *testing.T) {
	old := degradedUntil.Load()
	defer degradedUntil.Store(old)

	degradedUntil.Store(0)
	res := typeCheck(t, map[string]string{"oracle_001_test.go": `package mood_test

func TestOracle(t *testing.T) {}
`}, nil)
	if res.Skipped != "" {
		t.Fatalf("not degraded: Skipped = %q, want a real check to run", res.Skipped)
	}

	degradedUntil.Store(time.Now().Add(time.Minute).UnixNano())
	res = typeCheck(t, map[string]string{"oracle_001_test.go": `package mood_test

func TestOracle(t *testing.T) {}
`}, nil)
	if !strings.Contains(res.Skipped, "paused") {
		t.Fatalf("degraded: Skipped = %q, want a paused notice", res.Skipped)
	}

	degradedUntil.Store(time.Now().Add(-time.Minute).UnixNano())
	res = typeCheck(t, map[string]string{"oracle_001_test.go": `package mood_test

func TestOracle(t *testing.T) {}
`}, nil)
	if res.Skipped != "" {
		t.Fatalf("expired cooldown: Skipped = %q, want a real check to run", res.Skipped)
	}
}
