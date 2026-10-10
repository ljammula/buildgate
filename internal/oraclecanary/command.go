package oraclecanary

import (
	"fmt"
	"path"
	"regexp"
	"strings"
)

// oracleMountPath is where an approved ticket oracle is mounted in the
// workspace. It duplicates request.TicketOracleMountPath (this package must not
// import internal/request, which imports it for CheckDir); an external test
// asserts the two stay equal.
const oracleMountPath = ".oracle"

// safePathPart limits template inputs to characters that cannot break out of
// the generated shell command.
var safePathPart = regexp.MustCompile(`^[A-Za-z0-9_.][A-Za-z0-9_./-]*$`)

func checkPart(what, v string) error {
	if !safePathPart.MatchString(v) || path.Clean(v) != v || v == ".." || strings.HasPrefix(v, "../") {
		return fmt.Errorf("%s %q must be a clean relative path of [A-Za-z0-9_./-]", what, v)
	}
	return nil
}

// GoCommand proposes a RUN_COMMAND for a Go oracle: splice the mounted
// .oracle/<file> into <moduleDir>/<pkgDir> as zz_oracle_test.go through
// `go test -overlay` and run only TestOracle* there. moduleDir and pkgDir are
// workspace-relative ("." allowed); pkgDir is relative to moduleDir. The
// workspace root is captured before cd so the mount is found wherever the
// module lives. Every oracle test must be named TestOracle* (the canary is).
func GoCommand(moduleDir, pkgDir, file string) (string, error) {
	for _, p := range []struct{ what, v string }{{"module dir", moduleDir}, {"package dir", pkgDir}, {"oracle file", file}} {
		if err := checkPart(p.what, p.v); err != nil {
			return "", err
		}
	}
	if path.Base(file) != file {
		return "", fmt.Errorf("oracle file %q must be a bare file name", file)
	}
	target := path.Join(pkgDir, "zz_oracle_test.go")
	pkgArg := "./" + pkgDir + "/"
	if pkgDir == "." {
		pkgArg = "./"
	}
	return fmt.Sprintf(`W="$PWD" && cd %s && printf '{"Replace":{"%%s":"%%s"}}' "$PWD/%s" "$W/%s/%s" > /tmp/oracle-overlay.json && go test -overlay=/tmp/oracle-overlay.json %s -run TestOracle -count=1`,
		moduleDir, target, oracleMountPath, file, pkgArg), nil
}

// GoOracleFile is one drafted Go oracle file and the package directory (relative
// to the module directory, "." allowed) it is spliced into.
type GoOracleFile struct {
	Name   string // bare file name under the .oracle mount
	PkgDir string
}

// GoMultiCommand proposes a RUN_COMMAND for SEVERAL Go oracle files of one
// module: the same overlay as GoCommand, one Replace entry per file, but
// directory-scoped. A ticket receives a subset of the request-level oracle, so
// the command must not name a mounted file; it loops over the *_test.go files
// actually present under the mount and looks each one's package directory up in
// a case table keyed by file name. A present file missing from the table is
// skipped (it cannot be placed), and when nothing is placed the command fails
// (`[ -n "$ov" ]`) instead of running no tests. Only the packages that hold a
// present file are tested, with -run TestOracle. moduleDir and every PkgDir are
// workspace-relative clean paths, and the file names are safe bare names, so
// nothing here can break out of the shell command.
func GoMultiCommand(moduleDir string, files []GoOracleFile) (string, error) {
	if err := checkPart("module dir", moduleDir); err != nil {
		return "", err
	}
	if len(files) == 0 {
		return "", fmt.Errorf("no Go oracle files")
	}
	seen := map[string]bool{}
	var table strings.Builder
	for _, f := range files {
		if err := checkPart("oracle file", f.Name); err != nil {
			return "", err
		}
		if err := checkPart("package dir", f.PkgDir); err != nil {
			return "", err
		}
		if path.Base(f.Name) != f.Name || !strings.HasSuffix(f.Name, "_test.go") || strings.ContainsAny(f.Name, "*?[") {
			return "", fmt.Errorf("oracle file %q must be a bare *_test.go name", f.Name)
		}
		if seen[f.Name] {
			return "", fmt.Errorf("oracle file %q listed twice", f.Name)
		}
		seen[f.Name] = true
		prefix := ""
		if f.PkgDir != "." {
			prefix = f.PkgDir + "/"
		}
		fmt.Fprintf(&table, "%s) pre=%s;; ", f.Name, prefix)
	}
	return fmt.Sprintf(`W="$PWD" && cd %s && ov= pk= && for f in "$W"/%s/*_test.go; do [ -f "$f" ] || continue; b=${f##*/}; case "$b" in %s*) continue;; esac; ov="$ov${ov:+,}\"$PWD/${pre}zz_$b\":\"$f\""; case " $pk " in *" ./$pre "*) ;; *) pk="$pk ./$pre";; esac; done && [ -n "$ov" ] && printf '{"Replace":{%%s}}' "$ov" > /tmp/oracle-overlay.json && go test -overlay=/tmp/oracle-overlay.json $pk -run TestOracle -count=1`,
		moduleDir, oracleMountPath, table.String()), nil
}

// PythonStdlibCommand runs every drafted plain-function oracle
// (test_oracle_*.py under the mount, one or more files) with a small
// stdlib-only loader: no pytest, no unittest. Matches
// data/oracles/math-ops-multiply/RUN_COMMAND.txt, the hand-written fixture
// this shape was proven against (see scripts/live-smoke.sh's own doc
// comment): unittest is not used here because it only collects
// unittest.TestCase methods, and this canary's Python convention is plain
// top-level test_ functions (pythonCanary), which unittest's own test loader
// cannot discover. Every module-level name in each glob-matched file starting
// with "test" is executed and any exception it raises counts as a failure;
// the process exits non-zero if any test failed or none ran at all (a file
// containing no test_ functions could otherwise exit 0 having run nothing).
func PythonStdlibCommand() string {
	return "python3 - <<'PY'\n" +
		"import glob, importlib.util, sys, traceback\n" +
		"sys.path.insert(0, \".\")\n" +
		"ran = failed = 0\n" +
		"for path in sorted(glob.glob(\"" + oracleMountPath + "/test_oracle_*.py\")):\n" +
		"    spec = importlib.util.spec_from_file_location(\"oracle_mod\", path)\n" +
		"    mod = importlib.util.module_from_spec(spec)\n" +
		"    spec.loader.exec_module(mod)\n" +
		"    for name in sorted(vars(mod)):\n" +
		"        fn = getattr(mod, name)\n" +
		"        if name.startswith(\"test\") and callable(fn):\n" +
		"            ran += 1\n" +
		"            try:\n" +
		"                fn()\n" +
		"            except Exception:\n" +
		"                failed += 1\n" +
		"                traceback.print_exc()\n" +
		"print(\"oracle tests: %d ran, %d failed\" % (ran, failed))\n" +
		"sys.exit(1 if failed or not ran else 0)\n" +
		"PY"
}
