package oraclecanary

import (
	"os/exec"
	"strings"
	"testing"
)

// The canary must keep every Test function name of the original, or a
// RUN_COMMAND that selects tests by name (-run TestOracleLevel) selects nothing
// on the canary, exits 0, and reads FALSE_PASS for a good command.
func TestGoCanaryKeepsEveryOriginalTestName(t *testing.T) {
	src := []byte("package p\n\nimport \"testing\"\n\n/*\nfunc TestOracleInComment(t *testing.T) {}\n*/\nfunc TestOracleLevel(t *testing.T) {}\nfunc TestOracleCarry(t *testing.T) {}\nfunc TestMain(m *testing.M) {}\nfunc Testlower(t *testing.T) {}\nfunc (s *S) TestOracleMethod() {}\n")
	got, err := canaryContent("p_oracle_test.go", src)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{"func TestOracleLevel(t *testing.T)", "func TestOracleCarry(t *testing.T)"} {
		if !strings.Contains(string(got), w) {
			t.Errorf("canary lacks %q:\n%s", w, got)
		}
	}
	for _, bad := range []string{"TestOracleCanary", "TestOracleInComment", "TestMain", "Testlower", "TestOracleMethod"} {
		if strings.Contains(string(got), bad) {
			t.Errorf("canary must not contain %q:\n%s", bad, got)
		}
	}
}

func TestOtherEcosystemCanariesKeepOriginalNames(t *testing.T) {
	py := "import pytest\n\ndef test_oracle_level():\n    pass\n\nclass TestOracleCarry:\n    def test_carries(self):\n        pass\n    def helper(self):\n        pass\n\nasync def test_oracle_async():\n    pass\n"
	got, err := canaryContent("test_oracle_a.py", []byte(py))
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{"def test_oracle_level():\n    assert False", "def test_oracle_async():", "class TestOracleCarry:\n    def test_carries(self):\n        assert False"} {
		if !strings.Contains(string(got), w) {
			t.Errorf("python canary lacks %q:\n%s", w, got)
		}
	}
	if strings.Contains(string(got), "test_oracle_canary") || strings.Contains(string(got), "helper") {
		t.Errorf("python canary invented or copied names:\n%s", got)
	}

	js := "import { test, describe, expect } from 'vitest';\ndescribe(\"level rules\", () => {\n  it('carries over', () => {});\n});\ntest.skip(`tpl ${x}`, () => {});\ntest(\"top\", () => {});\n"
	got, err = canaryContent("a.oracle.test.ts", []byte(js))
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{"from 'vitest'", "test('carries over', () =>", "test(\"top\", () =>", "describe(\"level rules\", () =>"} {
		if !strings.Contains(string(got), w) {
			t.Errorf("js canary lacks %q:\n%s", w, got)
		}
	}
	if strings.Contains(string(got), "${") {
		t.Errorf("js canary kept an interpolated name:\n%s", got)
	}
	// No import in the original: rely on globals, do not add an import a jest repo lacks.
	got, _ = canaryContent("a.oracle.test.js", []byte("test('x', () => {});\n"))
	if strings.Contains(string(got), "import") {
		t.Errorf("js canary added an import the original lacked:\n%s", got)
	}

	dart := "import 'package:flutter_test/flutter_test.dart';\nvoid main() {\n  group('level', () {\n    testWidgets('carries', (tester) async {});\n  });\n  test('plain', () {});\n}\n"
	got, err = canaryContent("a_test.dart", []byte(dart))
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{"package:flutter_test/flutter_test.dart", "test('carries', () {", "test('plain', () {", "group('level', () {"} {
		if !strings.Contains(string(got), w) {
			t.Errorf("dart canary lacks %q:\n%s", w, got)
		}
	}
}

// Real-toolchain proof of the design flaw this fixes: operators select tests
// by their real name, so the canary must be selected by the same -run pattern.
func TestGoEndToEndNameSelectingCommand(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not on PATH; skipping real end-to-end canary test")
	}
	oracle := t.TempDir()
	writeFiles(t, oracle, map[string]string{
		"MANIFEST.json":    `{}`,
		"p_oracle_test.go": "package p\n\nimport \"testing\"\n\nfunc TestOracleLevel(t *testing.T) {\n\tif Add(2, 3) != 5 {\n\t\tt.Fatal(\"Add(2,3) != 5\")\n\t}\n}\n",
	})
	canary := t.TempDir()
	if _, err := buildSnapshot(oracle, canary); err != nil {
		t.Fatal(err)
	}
	base, err := GoCommand("svc", "p", "p_oracle_test.go")
	if err != nil {
		t.Fatal(err)
	}
	cmd := strings.Replace(base, "-run TestOracle ", "-run TestOracleLevel ", 1)
	if cmd == base {
		t.Fatalf("test setup: command not rewritten: %s", base)
	}
	realExit := runShell(t, e2eWorkspace(t, "return a + b", oracle), cmd)
	canaryExit := runShell(t, e2eWorkspace(t, "return a + b", canary), cmd)
	if realExit != 0 || canaryExit == 0 {
		t.Fatalf("real=%d canary=%d: a -run TestOracleLevel command must pass the real oracle and FAIL the canary", realExit, canaryExit)
	}
	if v := Evaluate(realExit, canaryExit); v != VerdictTrustworthy {
		t.Errorf("verdict=%s, want TRUSTWORTHY", v)
	}
}
