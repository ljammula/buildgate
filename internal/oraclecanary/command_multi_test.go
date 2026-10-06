package oraclecanary

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestGoMultiCommandRejectsUnsafeInput(t *testing.T) {
	ok := GoOracleFile{Name: "a_oracle_test.go", PkgDir: "p"}
	for name, args := range map[string]struct {
		module string
		files  []GoOracleFile
	}{
		"empty":            {"svc", nil},
		"module injection": {"svc; rm -rf /", []GoOracleFile{ok}},
		"absolute module":  {"/abs", []GoOracleFile{ok}},
		"dir escape":       {"svc", []GoOracleFile{{Name: "a_test.go", PkgDir: "../p"}}},
		"file path":        {"svc", []GoOracleFile{{Name: "sub/a_test.go", PkgDir: "p"}}},
		"file glob":        {"svc", []GoOracleFile{{Name: "*_test.go", PkgDir: "p"}}},
		"not a test file":  {"svc", []GoOracleFile{{Name: "a.go", PkgDir: "p"}}},
		"shell in name":    {"svc", []GoOracleFile{{Name: "$(x)_test.go", PkgDir: "p"}}},
		"duplicate":        {"svc", []GoOracleFile{ok, ok}},
	} {
		if _, err := GoMultiCommand(args.module, args.files); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func multiWorkspace(t *testing.T, subBody string, oracle map[string]string) string {
	t.Helper()
	w := t.TempDir()
	writeFiles(t, w, map[string]string{
		"svc/go.mod":  "module example.com/multi\n\ngo 1.21\n",
		"svc/p/p.go":  "package p\n\nfunc Add(a, b int) int { return a + b }\n",
		"svc/q/q.go":  "package q\n\nfunc Sub(a, b int) int { " + subBody + " }\n",
		"svc/root.go": "package svc\n\nfunc One() int { return 1 }\n",
	})
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

// TestGoMultiCommandEndToEnd runs the real go toolchain: one command serves
// every subset of the oracle files a ticket may receive, splicing each present
// file into its own package, running only those, and failing (never running
// nothing) when no file is present.
func TestGoMultiCommandEndToEnd(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not on PATH")
	}
	cmd, err := GoMultiCommand("svc", []GoOracleFile{
		{Name: "p_oracle_test.go", PkgDir: "p"},
		{Name: "q_oracle_test.go", PkgDir: "q"},
		{Name: "root_oracle_test.go", PkgDir: "."},
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(cmd, ".oracle/p_oracle") || strings.Contains(cmd, ".oracle/q_oracle") {
		t.Fatalf("the command must be directory-scoped, not name mounted files:\n%s", cmd)
	}
	test := func(pkg, name, want string) string {
		return "package " + pkg + "\n\nimport \"testing\"\n\nfunc " + name + "(t *testing.T) {\n\tif " + want + " {\n\t\tt.Fatal(\"failed\")\n\t}\n}\n"
	}
	oracle := map[string]string{
		"MANIFEST.json":       `[]`,
		"RUN_COMMAND.txt":     cmd,
		"p_oracle_test.go":    test("p", "TestOracleAdd", "Add(2, 3) != 5"),
		"q_oracle_test.go":    test("q", "TestOracleSub", "Sub(5, 3) != 2"),
		"root_oracle_test.go": test("svc", "TestOracleOne", "One() != 1"),
	}
	run := func(w string) int { return runShell(t, w, cmd) }

	if code := run(multiWorkspace(t, "return a - b", oracle)); code != 0 {
		t.Fatalf("all three files present, correct implementation: exit %d, want 0", code)
	}
	if code := run(multiWorkspace(t, "return a + b", oracle)); code == 0 {
		t.Fatal("a wrong implementation in the second package must fail the command")
	}
	// The canary snapshot keeps every test's name and file name, so the same
	// command runs it and fails.
	canary := t.TempDir()
	src := t.TempDir()
	writeFiles(t, src, oracle)
	if eco, err := buildSnapshot(src, canary); err != nil || eco != Go {
		t.Fatalf("buildSnapshot = %q, %v", eco, err)
	}
	cw := multiWorkspace(t, "return a - b", nil)
	if err := os.CopyFS(filepath.Join(cw, ".oracle"), os.DirFS(canary)); err != nil {
		t.Fatal(err)
	}
	if code := run(cw); code == 0 {
		t.Fatal("the always-failing canary must fail the command")
	}

	// A ticket that received only some of the files: the missing ones are not
	// referenced, and their (here broken) packages are not tested.
	subset := map[string]string{"MANIFEST.json": `[]`, "RUN_COMMAND.txt": cmd, "p_oracle_test.go": oracle["p_oracle_test.go"]}
	if code := run(multiWorkspace(t, "return a + b", subset)); code != 0 {
		t.Fatalf("a ticket holding only the p oracle: exit %d, want 0 (q is not its concern)", code)
	}
	// Nothing to place must fail closed rather than run zero tests.
	if code := run(multiWorkspace(t, "return a - b", map[string]string{"MANIFEST.json": `[]`, "stray_test.go": test("p", "TestOracleX", "false")})); code == 0 {
		t.Fatal("a mount with no known oracle file must fail, not pass vacuously")
	}
}
