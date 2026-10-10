package oraclecanary

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func requirePython3(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH")
	}
}

func TestCheckPythonOracleAccepts(t *testing.T) {
	requirePython3(t)
	src := []byte("from add import multiply_numbers\n\n\ndef test_oracle_positive_product():\n    assert multiply_numbers(3, 4) == 12\n")
	if err := CheckPythonOracle(src); err != nil {
		t.Fatalf("valid oracle refused: %v", err)
	}
}

func TestCheckPythonOracleAcceptsSeveralTestFuncs(t *testing.T) {
	requirePython3(t)
	src := []byte("def test_oracle_a():\n    assert 1 == 1\n\n\ndef test_oracle_b():\n    assert 2 == 2\n")
	if err := CheckPythonOracle(src); err != nil {
		t.Fatalf("valid oracle refused: %v", err)
	}
}

func TestCheckPythonOracleRejectsSyntaxError(t *testing.T) {
	requirePython3(t)
	err := CheckPythonOracle([]byte("def test_oracle_x(:\n    pass\n"))
	if err == nil || !strings.Contains(err.Error(), "does not parse") {
		t.Fatalf("err = %v, want a parse refusal", err)
	}
}

func TestCheckPythonOracleRejectsNoTestFunction(t *testing.T) {
	requirePython3(t)
	err := CheckPythonOracle([]byte("def helper():\n    return 1\n"))
	if err == nil || !strings.Contains(err.Error(), "no top-level test-named function") {
		t.Fatalf("err = %v, want a no-test-function refusal", err)
	}
}

func TestCheckPythonOracleRejectsForbiddenImport(t *testing.T) {
	requirePython3(t)
	for _, src := range []string{
		"import subprocess\n\n\ndef test_oracle_x():\n    assert True\n",
		"import os\n\n\ndef test_oracle_x():\n    assert True\n",
		"from urllib import request\n\n\ndef test_oracle_x():\n    assert True\n",
	} {
		err := CheckPythonOracle([]byte(src))
		if err == nil || !strings.Contains(err.Error(), "not allowed in a drafted oracle") {
			t.Fatalf("src %q: err = %v, want a forbidden-import refusal", src, err)
		}
	}
}

func TestCheckPythonOracleRejectsForbiddenCall(t *testing.T) {
	requirePython3(t)
	for _, src := range []string{
		"def test_oracle_x():\n    eval('1')\n",
		"def test_oracle_x():\n    exec('1')\n",
		"def test_oracle_x():\n    open('/etc/passwd')\n",
	} {
		err := CheckPythonOracle([]byte(src))
		if err == nil || !strings.Contains(err.Error(), "not allowed in a drafted oracle") {
			t.Fatalf("src %q: err = %v, want a forbidden-call refusal", src, err)
		}
	}
}

// A forbidden call nested inside a helper function called BY a test is still
// caught: the check walks the whole file, not just each test function's own
// body, because the model could route the call through an intermediate
// function to dodge a shallower check.
func TestCheckPythonOracleRejectsForbiddenCallInHelper(t *testing.T) {
	requirePython3(t)
	src := []byte("def _leak():\n    import subprocess\n    subprocess.run(['id'])\n\n\ndef test_oracle_x():\n    _leak()\n    assert True\n")
	err := CheckPythonOracle(src)
	if err == nil || !strings.Contains(err.Error(), "subprocess") {
		t.Fatalf("err = %v, want the nested import caught", err)
	}
}

func TestCheckPythonOracleRejectsOversize(t *testing.T) {
	src := make([]byte, maxPythonOracleCheckBytes+1)
	err := CheckPythonOracle(src)
	if err == nil || !strings.Contains(err.Error(), "too large to check") {
		t.Fatalf("err = %v, want an oversize refusal", err)
	}
}

// The check never executes the drafted file: a "test" that would fail loudly
// if actually run (raising, or writing a marker file) is still accepted --
// only its SHAPE is inspected. This is the same guarantee CheckGoOracle gives
// via go/parser: parsing is not running.
func TestCheckPythonOracleNeverExecutesTheOracle(t *testing.T) {
	requirePython3(t)
	src := []byte("raise RuntimeError('would fail the whole process if executed')\n\n\ndef test_oracle_x():\n    assert True\n")
	if err := CheckPythonOracle(src); err != nil {
		t.Fatalf("a syntactically valid file that would raise if executed was refused (it must only be parsed, not run): %v", err)
	}
}

func TestPythonOracleImportsReportsFromAndPlainImports(t *testing.T) {
	requirePython3(t)
	src := []byte("from sub import subtract_numbers\nimport math\n\n\ndef test_oracle_x():\n    assert subtract_numbers(5, 2) == 3\n")
	got, err := PythonOracleImports(src)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"subtract_numbers": "sub", "math": "math"}
	if len(got) != len(want) {
		t.Fatalf("imports = %v, want %v", got, want)
	}
	for name, module := range want {
		if got[name] != module {
			t.Errorf("imports[%q] = %q, want %q", name, got[name], module)
		}
	}
}

func TestPythonOracleImportsRejectsSyntaxError(t *testing.T) {
	requirePython3(t)
	if _, err := PythonOracleImports([]byte("def test_oracle_x(:\n    pass\n")); err == nil {
		t.Fatal("unparseable source accepted")
	}
}

// PythonImportConflicts's own doc comment has the live defect (2026-09-24)
// this closes: a request naming "sub.py with subtract_numbers"/"div.py with
// divide_numbers" produced criteria that guessed different modules for the
// same two functions.
func TestPythonImportConflictsFlagsTheSameNameFromDifferentModules(t *testing.T) {
	problems := PythonImportConflicts(map[string]map[string]string{
		"test_oracle_001.py": {"subtract_numbers": "subtract"},
		"test_oracle_002.py": {"subtract_numbers": "add"},
		"test_oracle_003.py": {"divide_numbers": "divide"},
		"test_oracle_004.py": {"divide_numbers": "add"},
	})
	if len(problems) != 2 {
		t.Fatalf("problems = %v, want exactly 2 (one per conflicting name)", problems)
	}
	joined := strings.Join(problems, "\n")
	for _, want := range []string{"subtract_numbers", "divide_numbers", "subtract", "add", "divide"} {
		if !strings.Contains(joined, want) {
			t.Errorf("problems = %q, want it to mention %q", joined, want)
		}
	}
}

func TestPythonImportConflictsAcceptsAgreeingOrUnrelatedImports(t *testing.T) {
	problems := PythonImportConflicts(map[string]map[string]string{
		"test_oracle_001.py": {"subtract_numbers": "sub"},
		"test_oracle_002.py": {"subtract_numbers": "sub", "divide_numbers": "div"},
	})
	if len(problems) != 0 {
		t.Errorf("problems = %v, want none (both files agree on subtract_numbers, divide_numbers is unrelated)", problems)
	}
}

func TestIsPythonStdlibModuleAcceptsCommonModulesAndRejectsOthers(t *testing.T) {
	for _, name := range []string{"unittest", "math", "decimal", "fractions", "itertools", "functools", "collections", "re", "typing", "dataclasses", "json"} {
		if !IsPythonStdlibModule(name) {
			t.Errorf("IsPythonStdlibModule(%q) = false, want true", name)
		}
	}
	for _, name := range []string{"sub", "divide", "add", "numpy", "requests"} {
		if IsPythonStdlibModule(name) {
			t.Errorf("IsPythonStdlibModule(%q) = true, want false", name)
		}
	}
}

func TestModuleExistsInWorkspaceAcceptsFileAndPackageForms(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "sub.py"), []byte("def subtract_numbers(a, b):\n    return a - b\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "divmod_pkg"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "divmod_pkg", "__init__.py"), []byte(""), 0o600); err != nil {
		t.Fatal(err)
	}
	if !ModuleExistsInWorkspace(root, "sub") {
		t.Error("sub.py not found as module sub")
	}
	if !ModuleExistsInWorkspace(root, "divmod_pkg") {
		t.Error("divmod_pkg/__init__.py not found as module divmod_pkg")
	}
	if ModuleExistsInWorkspace(root, "divide") {
		t.Error("nonexistent module divide reported as existing")
	}
	if ModuleExistsInWorkspace("", "sub") {
		t.Error("empty workspace root reported a module as existing")
	}
}

func TestSpecNamesPythonModuleMatchesWholeWordAndDotPyForm(t *testing.T) {
	spec := "## Scope\n\n`sub.py` providing `subtract_numbers(a, b)`.\n"
	if !SpecNamesPythonModule(spec, "sub") {
		t.Error("spec names sub.py but SpecNamesPythonModule said no")
	}
	if SpecNamesPythonModule(spec, "divide") {
		t.Error("spec does not name divide but SpecNamesPythonModule said yes")
	}
	// "sub" must not match inside "subtract" -- only a whole-word or
	// "<module>.py" hit counts.
	if SpecNamesPythonModule("the subtraction operation", "sub") {
		t.Error("\"sub\" matched inside \"subtraction\" -- want a whole-word match only")
	}
}

// PythonUnresolvedModules's own doc comment has the live defect (onboarding
// P4) this closes: an oracle importing a module that is neither a real
// module in the workspace nor named in the approved spec is a name the
// drafter invented rather than one grounded in either.
func TestPythonUnresolvedModulesFlagsInventedModule(t *testing.T) {
	got := PythonUnresolvedModules(map[string]string{"divide_numbers": "divide"}, t.TempDir(), "no module named here")
	if len(got) != 1 || got[0] != "divide" {
		t.Errorf("PythonUnresolvedModules = %v, want [\"divide\"]", got)
	}
}

func TestPythonUnresolvedModulesAcceptsStdlibWorkspaceAndSpecNamed(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "sub.py"), []byte(""), 0o600); err != nil {
		t.Fatal(err)
	}
	imports := map[string]string{
		"subtract_numbers": "sub",      // exists in the workspace
		"divide_numbers":   "div",      // named in the spec text
		"TestCase":         "unittest", // stdlib
	}
	spec := "`div.py` providing `divide_numbers(a, b)`"
	got := PythonUnresolvedModules(imports, root, spec)
	if len(got) != 0 {
		t.Errorf("PythonUnresolvedModules = %v, want none", got)
	}
}

// TestIsPythonStdlibModuleCoversTheWholeStdlib: the pinned list is CPython's
// full sys.stdlib_module_names, not a hand-picked subset -- a drafted oracle
// importing inspect was refused as an "invented module" (console walk,
// 2026-09-24).
func TestIsPythonStdlibModuleCoversTheWholeStdlib(t *testing.T) {
	for _, m := range []string{"inspect", "numbers", "operator", "string", "textwrap", "statistics", "contextlib", "copy"} {
		if !IsPythonStdlibModule(m) {
			t.Errorf("%s is part of the standard library", m)
		}
	}
	if IsPythonStdlibModule("divide") {
		t.Error("divide is not a standard-library module")
	}
}

// TestIsPythonStdlibModuleAcceptsDottedSubmodule closes the dotted-import
// resolution finding from an adversarial review: `from collections.abc
// import Iterable` was flagged as an invented module because the
// stdlib allowlist was keyed only on the full dotted string, never the
// top-level segment.
func TestIsPythonStdlibModuleAcceptsDottedSubmodule(t *testing.T) {
	if !IsPythonStdlibModule("collections.abc") {
		t.Error("collections.abc is part of the standard library (top-level segment collections)")
	}
	if IsPythonStdlibModule("divide.sub") {
		t.Error("divide.sub is not a standard-library module")
	}
}

// TestModuleExistsInWorkspaceAcceptsDottedForm closes the same
// dotted-import resolution finding: `from pkg.core import add` with
// pkg/core.py present in the workspace was flagged unresolved because
// ModuleExistsInWorkspace looked for a literal "pkg.core.py" file
// instead of mapping the dot to a path separator.
func TestModuleExistsInWorkspaceAcceptsDottedForm(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "pkg"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "pkg", "core.py"), []byte(""), 0o600); err != nil {
		t.Fatal(err)
	}
	if !ModuleExistsInWorkspace(root, "pkg.core") {
		t.Error("pkg/core.py not found as dotted module pkg.core")
	}
	// A dotted import also resolves when just its top-level package
	// directory is real, even without the specific submodule file visible.
	if !ModuleExistsInWorkspace(root, "pkg.other") {
		t.Error("pkg.other not resolved via its real top-level package directory pkg")
	}
	if ModuleExistsInWorkspace(root, "nope.sub") {
		t.Error("nope.sub reported as existing when neither nope nor nope/sub exists")
	}
}

// TestPythonUnresolvedModulesAcceptsDottedWorkspaceAndStdlibImports closes
// the same dotted-import resolution finding end to end through
// PythonUnresolvedModules: a dotted import of a real workspace module, and a
// dotted stdlib import, must both resolve.
func TestPythonUnresolvedModulesAcceptsDottedWorkspaceAndStdlibImports(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "pkg"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "pkg", "core.py"), []byte(""), 0o600); err != nil {
		t.Fatal(err)
	}
	imports := map[string]string{
		"add":      "pkg.core",
		"Iterable": "collections.abc",
	}
	got := PythonUnresolvedModules(imports, root, "")
	if len(got) != 0 {
		t.Errorf("PythonUnresolvedModules = %v, want none (both are dotted but real)", got)
	}
}

// TestSpecNamesPythonModuleRequiresExplicitMention closes an adversarial
// review finding: a whole-word match anywhere in prose let a common
// verb like "add" pass as naming module "add". Only an explicit mention
// (<module>.py / <path>.py, or a backtick-quoted token) counts now.
func TestSpecNamesPythonModuleRequiresExplicitMention(t *testing.T) {
	if SpecNamesPythonModule("add the result to the total", "add") {
		t.Error("\"add the result\" wrongly named module \"add\" via plain prose")
	}
	if !SpecNamesPythonModule("`sub.py` provides subtract_numbers", "sub") {
		t.Error("backtick-quoted `sub.py` should name module sub")
	}
	if !SpecNamesPythonModule("defined in `sub.py`", "sub") {
		t.Error("\"in `sub.py`\" should name module sub")
	}
}

// TestSpecNamesPythonModuleAcceptsDottedAndPathForm closes the same
// dotted-import resolution finding: a dotted module named in the spec
// only in its path form (pkg/core.py), or vice versa, must still be
// recognised.
func TestSpecNamesPythonModuleAcceptsDottedAndPathForm(t *testing.T) {
	if !SpecNamesPythonModule("implemented in `pkg/core.py`", "pkg.core") {
		t.Error("path form `pkg/core.py` should name dotted module pkg.core")
	}
	if !SpecNamesPythonModule("see `pkg.core`", "pkg.core") {
		t.Error("backtick-quoted dotted form `pkg.core` should name module pkg.core")
	}
}

// TestPythonImportConflictsIgnoresPlainImportOfSameTopLevelPackage closes
// an import-conflict false-positive finding from an adversarial review:
// `import collections` in one file and
// `import collections.abc` in another both bind the same top-level package
// "collections" to itself and must not be flagged as disagreeing modules.
// PythonOracleFromImports (never PythonOracleImports) is what callers must
// pass to PythonImportConflicts, so this exercises the real parser too.
func TestPythonImportConflictsIgnoresPlainImportOfSameTopLevelPackage(t *testing.T) {
	requirePython3(t)
	a, err := PythonOracleFromImports([]byte("import collections\n\n\ndef test_oracle_a():\n    assert True\n"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := PythonOracleFromImports([]byte("import collections.abc\n\n\ndef test_oracle_b():\n    assert True\n"))
	if err != nil {
		t.Fatal(err)
	}
	problems := PythonImportConflicts(map[string]map[string]string{"a.py": a, "b.py": b})
	if len(problems) != 0 {
		t.Errorf("problems = %v, want none (plain import of the same top-level package never conflicts)", problems)
	}
}

// TestPythonImportConflictsIgnoresWildcardImports closes the same
// import-conflict false-positive finding: `from a import *` and
// `from b import *` both bind the name "*", which is not a real name
// and must not be flagged.
func TestPythonImportConflictsIgnoresWildcardImports(t *testing.T) {
	requirePython3(t)
	a, err := PythonOracleFromImports([]byte("from a import *\n\n\ndef test_oracle_a():\n    assert True\n"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := PythonOracleFromImports([]byte("from b import *\n\n\ndef test_oracle_b():\n    assert True\n"))
	if err != nil {
		t.Fatal(err)
	}
	problems := PythonImportConflicts(map[string]map[string]string{"a.py": a, "b.py": b})
	if len(problems) != 0 {
		t.Errorf("problems = %v, want none (wildcard imports bind no real name)", problems)
	}
}

// TestPythonImportConflictsStillFlagsRealFromImportDisagreement is the
// not-regressed half of the same import-conflict finding: two files
// each doing `from <module> import f` with different modules must
// still be flagged.
func TestPythonImportConflictsStillFlagsRealFromImportDisagreement(t *testing.T) {
	requirePython3(t)
	a, err := PythonOracleFromImports([]byte("from add import f\n\n\ndef test_oracle_a():\n    assert True\n"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := PythonOracleFromImports([]byte("from div import f\n\n\ndef test_oracle_b():\n    assert True\n"))
	if err != nil {
		t.Fatal(err)
	}
	problems := PythonImportConflicts(map[string]map[string]string{"a.py": a, "b.py": b})
	if len(problems) != 1 {
		t.Fatalf("problems = %v, want exactly 1 (f bound to two different real modules)", problems)
	}
}

// TestResolvePythonInterpreterPrefersNewestAvailable closes the
// python-interpreter-selection finding from an adversarial review: the
// host inspector used to hardcode "python3",
// which can be older than the sandbox's own interpreter and refuse syntax
// the sandbox runs fine. resolvePythonInterpreter must prefer the newest
// python3.NN on PATH via an injectable lookup.
func TestResolvePythonInterpreterPrefersNewestAvailable(t *testing.T) {
	origLookPath := lookPath
	defer func() { lookPath = origLookPath; resetPythonInterpreterCacheForTest() }()

	available := map[string]string{
		"python3.10": "/fake/bin/python3.10",
		"python3":    "/fake/bin/python3",
	}
	lookPath = func(name string) (string, error) {
		if p, ok := available[name]; ok {
			return p, nil
		}
		return "", fmt.Errorf("%s: not found", name)
	}
	resetPythonInterpreterCacheForTest()

	got, err := resolvePythonInterpreter()
	if err != nil {
		t.Fatal(err)
	}
	if got != "/fake/bin/python3.10" {
		t.Errorf("resolvePythonInterpreter() = %q, want the newest available (python3.10), not the plain python3 fallback", got)
	}
}

// TestResolvePythonInterpreterFallsBackToPlainPython3 is the
// not-regressed half of the same python-interpreter-selection finding:
// a host with only plain python3 on PATH still resolves.
func TestResolvePythonInterpreterFallsBackToPlainPython3(t *testing.T) {
	origLookPath := lookPath
	defer func() { lookPath = origLookPath; resetPythonInterpreterCacheForTest() }()

	lookPath = func(name string) (string, error) {
		if name == "python3" {
			return "/fake/bin/python3", nil
		}
		return "", fmt.Errorf("%s: not found", name)
	}
	resetPythonInterpreterCacheForTest()

	got, err := resolvePythonInterpreter()
	if err != nil {
		t.Fatal(err)
	}
	if got != "/fake/bin/python3" {
		t.Errorf("resolvePythonInterpreter() = %q, want the plain python3 fallback", got)
	}
}

// TestCheckPythonOracleParseFailureNamesTheInterpreter closes the same
// python-interpreter-selection finding: the parse-failure message must say
// which interpreter/version parsed the file.
func TestCheckPythonOracleParseFailureNamesTheInterpreter(t *testing.T) {
	requirePython3(t)
	resetPythonInterpreterCacheForTest()
	defer resetPythonInterpreterCacheForTest()
	err := CheckPythonOracle([]byte("def test_oracle_x(:\n    pass\n"))
	if err == nil || !strings.Contains(err.Error(), "checked with python3") {
		t.Fatalf("err = %v, want the parse-failure message to name the interpreter it checked with", err)
	}
}

// resetPythonInterpreterCacheForTest clears the cached interpreter choice so
// a test can inject a fake lookPath and observe a fresh resolution.
func resetPythonInterpreterCacheForTest() {
	pythonInterpreterMu.Lock()
	defer pythonInterpreterMu.Unlock()
	pythonInterpreterDone = false
	pythonInterpreterPath, pythonInterpreterErr = "", nil
}
