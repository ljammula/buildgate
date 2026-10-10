package oraclecanary

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestEvaluate(t *testing.T) {
	cases := []struct {
		real, canary int
		want         Verdict
		trusted      bool
	}{
		{0, 1, VerdictTrustworthy, true},
		{0, 0, VerdictFalsePass, false},
		{1, 1, VerdictOracleFailed, false},
		{1, 0, VerdictOracleFailed, false},
		{2, 3, VerdictOracleFailed, false},
	}
	for _, c := range cases {
		got := Evaluate(c.real, c.canary)
		if got != c.want || got.Trusted() != c.trusted || got.Message() == "" {
			t.Errorf("Evaluate(%d,%d)=%s trusted=%v, want %s trusted=%v", c.real, c.canary, got, got.Trusted(), c.want, c.trusted)
		}
	}
}

func TestClassify(t *testing.T) {
	cases := []struct {
		name   string
		eco    Ecosystem
		isTest bool
		bad    bool
	}{
		{"mood_oracle_test.go", Go, true, false},
		{"test_oracle_mood.py", Python, true, false},
		{"mood_oracle_test.py", Python, true, false},
		{"mood.oracle.test.ts", JS, true, false},
		{"mood.oracle.test.js", JS, true, false},
		{"mood_test.dart", Dart, true, false},
		{"MANIFEST.json", "", false, false},
		{"RUN_COMMAND.txt", "", false, false},
		{"mood.rb", "", false, true},
		{"helper.go", "", false, true},
		{"test_other.py", "", false, true},
	}
	for _, c := range cases {
		eco, isTest, err := Classify(c.name)
		if (err != nil) != c.bad || eco != c.eco || isTest != c.isTest {
			t.Errorf("Classify(%q)=%q,%v,%v", c.name, eco, isTest, err)
		}
	}
}

func TestCanaryContent(t *testing.T) {
	goSrc := []byte("// header\npackage mood\n\nimport \"testing\"\nfunc TestOracleX(t *testing.T) {}\n")
	got, err := canaryContent("x_oracle_test.go", goSrc)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{"package mood\n", "func TestOracleX(t *testing.T)", "t.Fatal("} {
		if !strings.Contains(string(got), w) {
			t.Errorf("go canary missing %q:\n%s", w, got)
		}
	}
	if strings.Contains(string(got), "TestOracleCanary") {
		t.Error("go canary invented a name; it must reuse the original's")
	}
	if _, err := canaryContent("x_oracle_test.go", []byte("no clause")); err == nil {
		t.Error("expected error for Go file without package clause")
	}
	for name, want := range map[string]string{
		"test_oracle_a.py": "def test_oracle_canary():\n    assert False",
		"a.oracle.test.ts": "throw new Error(",
		"a_test.dart":      "fail(",
		"a.oracle.test.js": "oracle canary",
	} {
		got, err := canaryContent(name, []byte("original"))
		if err != nil || !strings.Contains(string(got), want) || strings.Contains(string(got), "original") {
			t.Errorf("CanaryContent(%q)=%q,%v; want containing %q", name, got, err, want)
		}
	}
	if _, err := canaryContent("MANIFEST.json", nil); err == nil {
		t.Error("expected error for non-test file")
	}
}

func writeFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, body := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestBuildSnapshot(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	writeFiles(t, src, map[string]string{
		"MANIFEST.json":       `{"a":1}`,
		"RUN_COMMAND.txt":     "go test .oracle",
		"mood_oracle_test.go": "package mood\n\nfunc TestOracleReal() {}\n",
	})
	eco, err := buildSnapshot(src, dst)
	if err != nil || eco != Go {
		t.Fatalf("BuildSnapshot=%q,%v", eco, err)
	}
	for _, name := range []string{"MANIFEST.json", "RUN_COMMAND.txt"} {
		a, _ := os.ReadFile(filepath.Join(src, name))
		b, err := os.ReadFile(filepath.Join(dst, name))
		if err != nil || !bytes.Equal(a, b) {
			t.Errorf("%s not copied unchanged: %v", name, err)
		}
	}
	b, err := os.ReadFile(filepath.Join(dst, "mood_oracle_test.go"))
	if err != nil || !strings.Contains(string(b), "package mood") || !strings.Contains(string(b), "func TestOracleReal(t *testing.T)") {
		t.Errorf("canary test wrong: %q %v", b, err)
	}
}

func TestBuildSnapshotRefusals(t *testing.T) {
	cases := map[string]map[string]string{
		"unsupported": {"a_oracle_test.go": "package p\n", "notes.rb": "x"},
		"mixed":       {"a_oracle_test.go": "package p\n", "test_oracle_a.py": "x"},
		"no tests":    {"MANIFEST.json": "{}"},
	}
	for name, files := range cases {
		src := t.TempDir()
		writeFiles(t, src, files)
		if _, err := buildSnapshot(src, t.TempDir()); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestCommandStrings(t *testing.T) {
	c, err := GoCommand("svc", "internal/mood", "mood_oracle_test.go")
	if err != nil {
		t.Fatal(err)
	}
	want := `W="$PWD" && cd svc && printf '{"Replace":{"%s":"%s"}}' "$PWD/internal/mood/zz_oracle_test.go" "$W/.oracle/mood_oracle_test.go" > /tmp/oracle-overlay.json && go test -overlay=/tmp/oracle-overlay.json ./internal/mood/ -run TestOracle -count=1`
	if c != want {
		t.Errorf("GoCommand=\n%s\nwant\n%s", c, want)
	}
}

func TestGoCommandRejectsUnsafeInput(t *testing.T) {
	for _, args := range [][3]string{
		{"svc; rm -rf /", "p", "a_test.go"},
		{"svc", "../p", "a_test.go"},
		{"/abs", "p", "a_test.go"},
		{"svc", "p", "sub/a_test.go"},
		{"svc", "p", "a b_test.go"},
		{"", "p", "a_test.go"},
		{"svc", "p", "$(x)_test.go"},
	} {
		if _, err := GoCommand(args[0], args[1], args[2]); err == nil {
			t.Errorf("GoCommand%v: expected error", args)
		}
	}
}

// runShell runs cmd in dir and returns its exit code.
func runShell(t *testing.T, dir, cmd string) int {
	t.Helper()
	c := exec.Command("sh", "-c", cmd)
	c.Dir = dir
	out, err := c.CombinedOutput()
	if err == nil {
		return 0
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode()
	}
	t.Fatalf("run %q: %v\n%s", cmd, err, out)
	return -1
}

// e2eWorkspace builds a workspace with module svc (package p) and the given
// oracle directory content mounted at .oracle.
func e2eWorkspace(t *testing.T, addBody string, oracleDir string) string {
	t.Helper()
	w := t.TempDir()
	writeFiles(t, w, map[string]string{
		"svc/go.mod": "module example.com/e2e\n\ngo 1.21\n",
		"svc/p/p.go": "package p\n\nfunc Add(a, b int) int { " + addBody + " }\n",
	})
	if err := os.CopyFS(filepath.Join(w, ".oracle"), os.DirFS(oracleDir)); err != nil {
		t.Fatal(err)
	}
	return w
}

// TestGoEndToEnd runs the real go toolchain to prove the technique: the
// generated overlay command passes on the real oracle, fails on the canary,
// and a vacuous `go test ./...` is caught as FALSE_PASS.
func TestGoEndToEnd(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not on PATH; skipping real end-to-end canary test")
	}
	oracle := t.TempDir()
	writeFiles(t, oracle, map[string]string{
		"MANIFEST.json":    `{}`,
		"p_oracle_test.go": "package p\n\nimport \"testing\"\n\nfunc TestOracleAdd(t *testing.T) {\n\tif Add(2, 3) != 5 {\n\t\tt.Fatal(\"Add(2,3) != 5\")\n\t}\n}\n",
	})
	canary := t.TempDir()
	if eco, err := buildSnapshot(oracle, canary); err != nil || eco != Go {
		t.Fatalf("BuildSnapshot=%q,%v", eco, err)
	}
	cmd, err := GoCommand("svc", "p", "p_oracle_test.go")
	if err != nil {
		t.Fatal(err)
	}

	realWS := e2eWorkspace(t, "return a + b", oracle)
	canaryWS := e2eWorkspace(t, "return a + b", canary)
	realExit := runShell(t, realWS, cmd)
	canaryExit := runShell(t, canaryWS, cmd)
	if realExit != 0 {
		t.Fatalf("real run exit=%d, want 0", realExit)
	}
	if canaryExit == 0 {
		t.Fatal("canary run exit=0, want non-zero")
	}
	if v := Evaluate(realExit, canaryExit); v != VerdictTrustworthy {
		t.Errorf("verdict=%s, want TRUSTWORTHY", v)
	}

	// A buggy implementation fails the real oracle.
	brokenWS := e2eWorkspace(t, "return a - b", oracle)
	if brokenExit := runShell(t, brokenWS, cmd); Evaluate(brokenExit, 1) != VerdictOracleFailed || brokenExit == 0 {
		t.Errorf("broken impl exit=%d, want non-zero => ORACLE_FAILED", brokenExit)
	}

	// Vacuous command: ./... skips the .oracle dot-directory, so it passes on
	// both the real oracle and the canary.
	vacuous := "echo .oracle && cd svc && go test ./..."
	vr, vc := runShell(t, realWS, vacuous), runShell(t, canaryWS, vacuous)
	if v := Evaluate(vr, vc); v != VerdictFalsePass {
		t.Errorf("vacuous command real=%d canary=%d verdict=%s, want FALSE_PASS", vr, vc, v)
	}
}

// A canary that fails only because the environment broke (timeout, signal,
// command not runnable) must not vouch for the command.
func TestEvaluateTreatsInfrastructureCanaryExitsAsInconclusive(t *testing.T) {
	for _, code := range []int{-1, 124, 125, 126, 127, 137, 139} {
		got := Evaluate(0, code)
		if got != VerdictCanaryInconclusive || got.Trusted() {
			t.Errorf("Evaluate(0, %d) = %q (trusted=%v), want inconclusive and not trusted", code, got, got.Trusted())
		}
	}
	if got := Evaluate(0, 1); got != VerdictTrustworthy {
		t.Errorf("Evaluate(0, 1) = %q, want trustworthy (an ordinary test failure)", got)
	}
	if got := Evaluate(1, 137); got != VerdictOracleFailed {
		t.Errorf("Evaluate(1, 137) = %q, want oracle failed (a failed real run is never trusted)", got)
	}
}

// The Go canary must be excluded exactly when the real file is, or the real run
// excludes the oracle (exit 0, "no tests to run") while the canary fails and the
// command reads as trustworthy though the oracle never ran.
func TestGoCanaryCarriesBuildConstraintsAndRefusesNoTestOracle(t *testing.T) {
	tagged := []byte("//go:build integration\n// +build integration\n\npackage p\n\nimport \"testing\"\n\nfunc TestOracleX(t *testing.T) {}\n")
	got, err := canaryContent("x_oracle_test.go", tagged)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{"//go:build integration", "// +build integration", "package p\n"} {
		if !strings.Contains(string(got), w) {
			t.Errorf("canary lacks %q:\n%s", w, got)
		}
	}
	if _, err := canaryContent("x_oracle_test.go", []byte("package p\n\nimport \"testing\"\n\nfunc TestSomethingElse(t *testing.T) {}\n")); err == nil {
		t.Error("an oracle with no func TestOracle* must be refused: -run TestOracle would run nothing")
	}
}

// A `package` word inside a leading block comment must not be taken as the clause.
func TestGoCanaryReadsThePackageClauseNotAComment(t *testing.T) {
	src := []byte("/*\npackage wrong\n*/\npackage right\n\nimport \"testing\"\n\nfunc TestOracleX(t *testing.T) {}\n")
	got, err := canaryContent("x_oracle_test.go", src)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "package right\n") || strings.Contains(string(got), "package wrong") {
		t.Errorf("canary used the wrong package clause:\n%s", got)
	}
}
