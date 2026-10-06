package oraclecanary

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestPythonStdlibCommandMatchesProvenFixture pins PythonStdlibCommand's
// output byte-for-byte to data/oracles/math-ops-multiply/RUN_COMMAND.txt, the
// hand-written command scripts/live-smoke.sh's own live-smoke-mathops-oracle
// fixture already proves works against a real sandboxed build (see that
// script's own doc comment on why a stdlib-only runner, not pytest or
// unittest, is used) -- so PythonStdlibCommand can never quietly drift from
// what is actually validated live.
func TestPythonStdlibCommandMatchesProvenFixture(t *testing.T) {
	want, err := os.ReadFile(filepath.Join("..", "..", "data", "oracles", "math-ops-multiply", "RUN_COMMAND.txt"))
	if err != nil {
		t.Fatal(err)
	}
	got := PythonStdlibCommand() + "\n"
	if got != string(want) {
		t.Fatalf("PythonStdlibCommand() =\n%s\nwant (from the fixture):\n%s", got, want)
	}
}

// pythonWorkspace lays out a workspace exactly like an approved ticket's
// mount: the target module(s) at the workspace root and the oracle files
// under .oracle/ (oracleMountPath), the same layout multiWorkspace builds for
// the Go multi-file tests.
func pythonWorkspace(t *testing.T, addBody string, oracle map[string]string) string {
	t.Helper()
	w := t.TempDir()
	if err := os.WriteFile(filepath.Join(w, "add.py"), []byte("def multiply_numbers(a, b):\n    "+addBody+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for name, body := range oracle {
		p := filepath.Join(w, ".oracle", name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return w
}

// TestPythonStdlibCommandEndToEnd runs the real python3 interpreter: the
// generated command runs every test_oracle_*.py under the mount, passes when
// the target module is correct, fails when it is wrong, fails when a matched
// file defines no test_ function (would otherwise exit 0 having run
// nothing), and the canary snapshot's always-failing content is exposed by
// this same command exactly as a hand-authored oracle's would be.
func TestPythonStdlibCommandEndToEnd(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH")
	}
	cmd := PythonStdlibCommand()
	oracle := map[string]string{
		"MANIFEST.json":           `[]`,
		"RUN_COMMAND.txt":         cmd,
		"test_oracle_multiply.py": "from add import multiply_numbers\n\n\ndef test_oracle_positive_product():\n    assert multiply_numbers(3, 4) == 12\n",
	}
	if code := runShell(t, pythonWorkspace(t, "return a * b", oracle), cmd); code != 0 {
		t.Fatalf("correct implementation: exit %d, want 0", code)
	}
	if code := runShell(t, pythonWorkspace(t, "return a + b", oracle), cmd); code == 0 {
		t.Fatal("a wrong implementation must fail the command")
	}
	if code := runShell(t, pythonWorkspace(t, "return a * b", map[string]string{
		"MANIFEST.json":        `[]`,
		"RUN_COMMAND.txt":      cmd,
		"test_oracle_empty.py": "x = 1\n",
	}), cmd); code == 0 {
		t.Fatal("a matched file with no test_ function must fail the command, not exit 0 having run nothing")
	}

	// The canary keeps the same file/function names and always fails: this
	// command must catch it exactly like a real, tampered RUN_COMMAND would.
	src, canary := t.TempDir(), t.TempDir()
	writeFiles(t, src, oracle)
	if eco, err := buildSnapshot(src, canary); err != nil || eco != Python {
		t.Fatalf("buildSnapshot = %q, %v", eco, err)
	}
	cw := pythonWorkspace(t, "return a * b", nil)
	if err := os.CopyFS(filepath.Join(cw, ".oracle"), os.DirFS(canary)); err != nil {
		t.Fatal(err)
	}
	if code := runShell(t, cw, cmd); code == 0 {
		t.Fatal("the canary snapshot must fail this command")
	}
}
