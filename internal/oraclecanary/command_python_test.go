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

// TestPythonStdlibCommandImportsASrcLayoutPackage runs the real python3
// interpreter on a repository that keeps its package under src/ (the src
// layout): the oracle imports it by its package name, as it would after an
// install, and the command passes on a correct implementation and fails on a
// wrong one.
func TestPythonStdlibCommandImportsASrcLayoutPackage(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH")
	}
	cmd := PythonStdlibCommand()
	workspace := func(body string) string {
		w := t.TempDir()
		writeFiles(t, w, map[string]string{
			"src/mathpkg/__init__.py":         "def multiply_numbers(a, b):\n    " + body + "\n",
			".oracle/MANIFEST.json":           `[]`,
			".oracle/RUN_COMMAND.txt":         cmd,
			".oracle/test_oracle_multiply.py": "from mathpkg import multiply_numbers\n\n\ndef test_oracle_positive_product():\n    assert multiply_numbers(3, 4) == 12\n",
		})
		return w
	}
	if code := runShell(t, workspace("return a * b"), cmd); code != 0 {
		t.Fatalf("correct src-layout implementation: exit %d, want 0", code)
	}
	if code := runShell(t, workspace("return a + b"), cmd); code == 0 {
		t.Fatal("a wrong src-layout implementation must fail the command")
	}
}

// TestPythonImportRootsAgreeWithTheRunner ties the approval-time import check
// (ModuleExistsInWorkspace, over PythonImportRoots) to the runner
// (PythonStdlibCommand) on the layouts that differ: for each, the check finds
// mathpkg exactly when the runner can import it, and a src/ that is itself a
// package never puts its modules ahead of the standard library's.
func TestPythonImportRootsAgreeWithTheRunner(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH")
	}
	cmd := PythonStdlibCommand()
	oracle := "import json\nfrom mathpkg import multiply_numbers\n\n\ndef test_oracle_positive_product():\n    assert multiply_numbers(3, 4) == 12\n    assert json.dumps([1]) == \"[1]\"\n"
	impl := "def multiply_numbers(a, b):\n    return a * b\n"
	for _, tc := range []struct {
		name   string
		files  map[string]string
		finds  bool
		passes bool
	}{
		{"package at the root", map[string]string{"mathpkg/__init__.py": impl}, true, true},
		{"src layout", map[string]string{"src/mathpkg/__init__.py": impl}, true, true},
		{"src is a package: its modules do not shadow the standard library", map[string]string{"src/__init__.py": "", "src/json.py": "raise ImportError('shadowed')\n", "mathpkg/__init__.py": impl}, true, true},
		{"src is a package: a package inside it is not a top-level import", map[string]string{"src/__init__.py": "", "src/mathpkg/__init__.py": impl}, false, false},
		{"no such package", map[string]string{"other/__init__.py": ""}, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := t.TempDir()
			tc.files[".oracle/test_oracle_multiply.py"] = oracle
			writeFiles(t, w, tc.files)
			if got := ModuleExistsInWorkspace(w, "mathpkg"); got != tc.finds {
				t.Errorf("ModuleExistsInWorkspace(mathpkg) = %v, want %v", got, tc.finds)
			}
			if code := runShell(t, w, cmd); (code == 0) != tc.passes {
				t.Errorf("runner exit %d, want pass=%v", code, tc.passes)
			}
		})
	}
}
