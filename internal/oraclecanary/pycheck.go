package oraclecanary

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// maxPythonOracleCheckBytes bounds the oracle source handed to the checker,
// the same cap CheckGoOracle applies to Go source.
const maxPythonOracleCheckBytes = 1 << 20

// pythonCheckTimeout bounds the subprocess below: parsing is O(file size)
// and the file is already capped, so a hang here can only be an unreachable
// python3 binary or a broken interpreter, not the oracle's own content.
const pythonCheckTimeout = 10 * time.Second

// pythonForbiddenImports names stdlib/top-level modules a drafted oracle must
// not import: anything that reaches the network, spawns a process, or touches
// the filesystem beyond importing the repository's own modules. A drafted
// oracle asserts against values already knowable from the spec (see
// draft_acceptance_oracles.py's DRAFT_INSTRUCTIONS); none of these are ever
// legitimately needed for that.
//
// This is a lint against an obviously wrong model draft, not the
// containment: it is trivially bypassable by a determined adversary
// (builtins.exec, posix.system, getattr(builtins, 'ev'+'al'),
// pathlib.Path.write_text, an aliased `x = eval`, __builtins__['eval'],
// _socket, runpy, webbrowser, and any number of other routes this list does
// not and cannot enumerate completely -- found in review, onboarding P4).
// The real containment is that the drafted oracle NEVER runs on the host at
// all (see CheckPythonOracle's own doc comment): it only ever executes
// inside the Docker sandbox the oracle gate runs in, exactly as for Go
// oracles, which get this same containment with no forbidden-import list at
// all (CheckGoOracle). Do not treat completing this list as a security fix;
// it only catches a model's obvious mistakes before they waste a human
// reviewer's time.
var pythonForbiddenImports = map[string]bool{
	"subprocess": true, "os": true, "sys": true, "socket": true, "shutil": true,
	"urllib": true, "http": true, "ftplib": true, "telnetlib": true, "smtplib": true,
	"requests": true, "socketserver": true, "multiprocessing": true, "ctypes": true,
	"importlib": true, "pty": true, "signal": true, "pickle": true, "marshal": true,
	"pipes": true, "asyncio": true, "builtins": true, "posix": true, "nt": true,
	"_socket": true, "runpy": true, "webbrowser": true, "concurrent": true,
}

// pythonForbiddenCalls names builtins a drafted oracle must not call: dynamic
// code execution and the same process/file/network escapes pythonForbiddenImports
// blocks by import, reachable via a builtin even without importing os/subprocess.
// Same lint-not-containment caveat as pythonForbiddenImports above: a
// determined adversary can reach eval/exec/open through indirection this
// list does not catch (a reassigned name, string-built attribute access,
// __builtins__ indexing, ...); the Docker sandbox is what actually contains
// the drafted oracle's execution.
var pythonForbiddenCalls = map[string]bool{
	"eval": true, "exec": true, "compile": true, "__import__": true,
	"open": true, "input": true, "breakpoint": true,
}

// pythonASTFacts is what the parse-only subprocess below reports about a
// drafted oracle file: the top-level test function names it defines (for the
// TestCase-free, plain-function convention this canary supports -- see
// pythonCanary), every module name imported / builtin called anywhere in
// the file (so CheckPythonOracle can apply pythonForbiddenImports/
// pythonForbiddenCalls without a second, independent parse in Go), and
// ImportedNames: every locally-bound name (`from module import name` binds
// name -> module; `import module` binds module -> module) -- PythonOracleImports
// exposes this for module-resolution checks (PythonUnresolvedModules). FromImports
// is the narrower subset PythonOracleFromImports exposes: only real names bound
// by `from module import name` (a wildcard `import *` binds no real name and is
// excluded), which is what PythonImportConflicts should compare -- a plain
// `import module`/`import module.sub` statement always binds the same top-level
// package to itself, so it can never disagree with another file's copy of the
// same statement.
type pythonASTFacts struct {
	TestFuncs     []string          `json:"test_funcs"`
	Imports       []string          `json:"imports"`
	Calls         []string          `json:"calls"`
	ImportedNames map[string]string `json:"imported_names"`
	FromImports   map[string]string `json:"from_imports"`
}

// pythonASTInspector is the script executed under python3 -I with the oracle
// source on stdin. It ONLY parses (ast.parse) and walks the resulting syntax
// tree (ast.walk) -- neither call executes a single statement of the input,
// exactly as go/parser.ParseFile + go/ast.Inspect never execute the Go source
// CheckGoOracle parses. -I isolates the interpreter from site customization
// and PYTHON* environment variables; the source is read from stdin only, so
// it is never written to disk as an importable module and nothing here can
// import it, spawn it, or otherwise run any of its code.
const pythonASTInspector = `
import ast, json, sys
tree = ast.parse(sys.stdin.read())
test_funcs = [n.name for n in tree.body if isinstance(n, (ast.FunctionDef, ast.AsyncFunctionDef)) and n.name.startswith("test")]
imports, calls, imported_names, from_imports = set(), set(), {}, {}
for n in ast.walk(tree):
    if isinstance(n, ast.Import):
        imports.update(a.name.split(".")[0] for a in n.names)
        for a in n.names:
            imported_names[(a.asname or a.name).split(".")[0]] = a.name
    elif isinstance(n, ast.ImportFrom) and n.module:
        imports.add(n.module.split(".")[0])
        for a in n.names:
            imported_names[a.asname or a.name] = n.module
            if a.name != "*":
                from_imports[a.asname or a.name] = n.module
    elif isinstance(n, ast.Call) and isinstance(n.func, ast.Name):
        calls.add(n.func.id)
print(json.dumps({"test_funcs": test_funcs, "imports": sorted(imports), "calls": sorted(calls), "imported_names": imported_names, "from_imports": from_imports}))
`

// CheckPythonOracle is the static, host-side check of one drafted Python
// oracle file, the Python counterpart of CheckGoOracle. It only PARSES: the
// source is fed to python3 -I on stdin and inspected with the ast module,
// which builds a syntax tree and never executes a statement of it (see
// pythonASTInspector's own doc comment) -- the drafted oracle's code never
// runs on the host, only in the Docker sandbox, matching this repo's
// unconditional containment rule (AGENTS.md, containment-matrix.md).
//
// It refuses source that does not parse, that declares no top-level
// test-named function (pythonCanary's plain-function convention this
// repository's canary and RUN_COMMAND runner both require -- see
// data/oracles/math-ops-multiply/RUN_COMMAND.txt), or that imports a
// forbidden module or calls a forbidden builtin anywhere in the file
// (pythonForbiddenImports/pythonForbiddenCalls). That forbidden-name check is
// a LINT that catches an obviously wrong model draft before it wastes a
// human reviewer's time -- it is not, and cannot be, an exhaustive list of
// every way Python code can reach the network, spawn a process, or touch the
// filesystem (see pythonForbiddenImports's own doc comment). The actual
// containment is that a drafted oracle's code never runs on the host at all,
// only inside the Docker sandbox the oracle gate runs in -- the same
// unconditional containment CheckGoOracle relies on for Go oracles, which
// has no forbidden-name list at all (AGENTS.md, containment-matrix.md).
func CheckPythonOracle(src []byte) error {
	if len(src) > maxPythonOracleCheckBytes {
		return fmt.Errorf("Python oracle is %d bytes, too large to check (limit %d)", len(src), maxPythonOracleCheckBytes)
	}
	facts, err := inspectPythonAST(src)
	if err != nil {
		return err
	}
	if len(facts.TestFuncs) == 0 {
		return fmt.Errorf("defines no top-level test-named function (def test_...(): ...); the stdlib runner only collects those")
	}
	for _, imp := range facts.Imports {
		if pythonForbiddenImports[imp] {
			return fmt.Errorf("imports %q, which is not allowed in a drafted oracle (network/process/filesystem escape)", imp)
		}
	}
	for _, call := range facts.Calls {
		if pythonForbiddenCalls[call] {
			return fmt.Errorf("calls %q, which is not allowed in a drafted oracle (dynamic execution or I/O escape)", call)
		}
	}
	return nil
}

// PythonOracleImports returns the `name -> module` bindings src's own import
// statements create (`from module import name` binds name -> module; `import
// module` binds module -> module), via the same parse-only inspector
// CheckPythonOracle uses -- src is never executed. Returns an error only when
// src does not parse; a file with no imports at all is a nil map, nil error.
func PythonOracleImports(src []byte) (map[string]string, error) {
	facts, err := inspectPythonAST(src)
	if err != nil {
		return nil, err
	}
	return facts.ImportedNames, nil
}

// PythonOracleFromImports returns the `name -> module` bindings src's own
// `from module import name` statements create -- a wildcard `from module
// import *` binds no real name and is excluded. This is the subset
// PythonImportConflicts should compare across files: unlike PythonOracleImports
// (which also reports plain `import module` bindings, needed for module-
// resolution checks), it deliberately leaves those out, since a plain import
// always binds the same top-level package to itself and can never disagree
// with another file's copy of the same statement. Returns an error only when
// src does not parse; a file with no `from` imports is a nil map, nil error.
func PythonOracleFromImports(src []byte) (map[string]string, error) {
	facts, err := inspectPythonAST(src)
	if err != nil {
		return nil, err
	}
	return facts.FromImports, nil
}

// PythonImportConflicts checks the `name -> module` bindings
// PythonOracleFromImports reports for several drafted oracle files (keyed by
// their own file name, only for error messages) and reports every name bound
// to two or more DIFFERENT modules across those files. Independently-drafted
// oracles that name the same function must agree on where it lives: a
// ticket's diff scope can only ever contain one real module per name, so two
// oracles disagreeing on it can never both be satisfied by the same build
// (live defect, 2026-09-24: request text named "sub.py with
// subtract_numbers" and "div.py with divide_numbers", but 4
// separately-drafted criteria produced imports from "subtract"/"add" and
// "divide"/"add" for those same two functions, and the build was quarantined
// on diff_scope after creating/editing all of them). Callers must pass only
// `from module import name` bindings (PythonOracleFromImports, never
// PythonOracleImports): a plain `import collections` in one file and `import
// collections.abc` in another both bind the same top-level package
// "collections" to itself and must not be flagged, and a wildcard `from a
// import *` binds no real name at all, so neither belongs in this
// comparison.
func PythonImportConflicts(fileImports map[string]map[string]string) []string {
	// name -> module -> file names that bound it there, so the message can
	// point at exactly which drafts disagree.
	byName := map[string]map[string][]string{}
	var files []string
	for file := range fileImports {
		files = append(files, file)
	}
	sort.Strings(files)
	for _, file := range files {
		for name, module := range fileImports[file] {
			if byName[name] == nil {
				byName[name] = map[string][]string{}
			}
			byName[name][module] = append(byName[name][module], file)
		}
	}
	var names []string
	for name := range byName {
		names = append(names, name)
	}
	sort.Strings(names)
	var problems []string
	for _, name := range names {
		byModule := byName[name]
		if len(byModule) < 2 {
			continue
		}
		var modules []string
		for module := range byModule {
			modules = append(modules, module)
		}
		sort.Strings(modules)
		var parts []string
		for _, module := range modules {
			parts = append(parts, fmt.Sprintf("%s (%s)", module, strings.Join(byModule[module], ", ")))
		}
		problems = append(problems, fmt.Sprintf("%q is imported from different modules across these oracles: %s -- they must agree on one module or they cannot all be satisfied in one ticket", name, strings.Join(parts, ", ")))
	}
	return problems
}

// pythonStdlibModules is CPython 3.12's sys.stdlib_module_names (public
// top-level names), generated once and pinned here: the host inspector runs
// on python3.9, which lacks that attribute, and a hand-picked subset refused
// real stdlib imports ("inspect", console walk 2026-09-24). This only
// answers "is this module part of the standard library" for module
// resolution; forbidden modules (os, subprocess, socket, ...) are still
// refused by CheckPythonOracle's own separate list.
var pythonStdlibModules = map[string]bool{
	"abc": true, "aifc": true, "antigravity": true, "argparse": true, "array": true,
	"ast": true, "asyncio": true, "atexit": true, "audioop": true, "base64": true,
	"bdb": true, "binascii": true, "bisect": true, "builtins": true, "bz2": true,
	"cProfile": true, "calendar": true, "cgi": true, "cgitb": true, "chunk": true,
	"cmath": true, "cmd": true, "code": true, "codecs": true, "codeop": true,
	"collections": true, "colorsys": true, "compileall": true, "concurrent": true,
	"configparser": true, "contextlib": true, "contextvars": true, "copy": true,
	"copyreg": true, "crypt": true, "csv": true, "ctypes": true, "curses": true,
	"dataclasses": true, "datetime": true, "dbm": true, "decimal": true, "difflib": true,
	"dis": true, "doctest": true, "email": true, "encodings": true, "ensurepip": true,
	"enum": true, "errno": true, "faulthandler": true, "fcntl": true, "filecmp": true,
	"fileinput": true, "fnmatch": true, "fractions": true, "ftplib": true, "functools": true,
	"gc": true, "genericpath": true, "getopt": true, "getpass": true, "gettext": true,
	"glob": true, "graphlib": true, "grp": true, "gzip": true, "hashlib": true,
	"heapq": true, "hmac": true, "html": true, "http": true, "idlelib": true,
	"imaplib": true, "imghdr": true, "importlib": true, "inspect": true, "io": true,
	"ipaddress": true, "itertools": true, "json": true, "keyword": true, "lib2to3": true,
	"linecache": true, "locale": true, "logging": true, "lzma": true, "mailbox": true,
	"mailcap": true, "marshal": true, "math": true, "mimetypes": true, "mmap": true,
	"modulefinder": true, "msilib": true, "msvcrt": true, "multiprocessing": true,
	"netrc": true, "nis": true, "nntplib": true, "nt": true, "ntpath": true,
	"nturl2path": true, "numbers": true, "opcode": true, "operator": true, "optparse": true,
	"os": true, "ossaudiodev": true, "pathlib": true, "pdb": true, "pickle": true,
	"pickletools": true, "pipes": true, "pkgutil": true, "platform": true, "plistlib": true,
	"poplib": true, "posix": true, "posixpath": true, "pprint": true, "profile": true,
	"pstats": true, "pty": true, "pwd": true, "py_compile": true, "pyclbr": true,
	"pydoc": true, "pydoc_data": true, "pyexpat": true, "queue": true, "quopri": true,
	"random": true, "re": true, "readline": true, "reprlib": true, "resource": true,
	"rlcompleter": true, "runpy": true, "sched": true, "secrets": true, "select": true,
	"selectors": true, "shelve": true, "shlex": true, "shutil": true, "signal": true,
	"site": true, "smtplib": true, "sndhdr": true, "socket": true, "socketserver": true,
	"spwd": true, "sqlite3": true, "sre_compile": true, "sre_constants": true,
	"sre_parse": true, "ssl": true, "stat": true, "statistics": true, "string": true,
	"stringprep": true, "struct": true, "subprocess": true, "sunau": true, "symtable": true,
	"sys": true, "sysconfig": true, "syslog": true, "tabnanny": true, "tarfile": true,
	"telnetlib": true, "tempfile": true, "termios": true, "textwrap": true,
	"this": true, "threading": true, "time": true, "timeit": true, "tkinter": true,
	"token": true, "tokenize": true, "tomllib": true, "trace": true, "traceback": true,
	"tracemalloc": true, "tty": true, "turtle": true, "turtledemo": true, "types": true,
	"typing": true, "unicodedata": true, "unittest": true, "urllib": true, "uu": true,
	"uuid": true, "venv": true, "warnings": true, "wave": true, "weakref": true,
	"webbrowser": true, "winreg": true, "winsound": true, "wsgiref": true, "xdrlib": true,
	"xml": true, "xmlrpc": true, "zipapp": true, "zipfile": true, "zipimport": true,
	"zlib": true, "zoneinfo": true,
}

// IsPythonStdlibModule reports whether name (a top-level module name, as
// PythonOracleImports/pythonASTInspector report it) is in the fixed stdlib
// allowlist above. name may be dotted (e.g. "collections.abc", as
// `from collections.abc import Iterable` reports it) -- only the top-level
// segment before the first "." is looked up, since that is the actual
// package the stdlib allowlist names (found in review, onboarding P4: a
// dotted stdlib import was flagged as an invented module).
func IsPythonStdlibModule(name string) bool {
	if i := strings.IndexByte(name, '.'); i >= 0 {
		name = name[:i]
	}
	return pythonStdlibModules[name]
}

// ModuleExistsInWorkspace reports whether name resolves to a real module in
// the target repository, relative to workspaceRoot. name may be dotted (e.g.
// "pkg.core", as `from pkg.core import add` reports it): dots map to path
// separators, so "pkg.core" resolves against pkg/core.py or
// pkg/core/__init__.py (found in review, onboarding P4: a dotted import of a
// real workspace module was flagged as invented). A dotted name also
// resolves when just its top-level package directory exists (e.g. "pkg" is a
// real directory for "import pkg.sub") -- `import pkg.sub` only requires pkg
// itself to be a real package, and a submodule several directories deep is
// not otherwise enumerable here. Checked by file/directory existence only
// (os.Stat) -- this never imports, parses, or otherwise runs anything, the
// same never-execute guarantee every other check in this file gives a
// drafted oracle's own content.
func ModuleExistsInWorkspace(workspaceRoot, name string) bool {
	if workspaceRoot == "" || name == "" {
		return false
	}
	relPath := filepath.Join(strings.Split(name, ".")...)
	if info, err := os.Stat(filepath.Join(workspaceRoot, relPath+".py")); err == nil && !info.IsDir() {
		return true
	}
	if info, err := os.Stat(filepath.Join(workspaceRoot, relPath, "__init__.py")); err == nil && !info.IsDir() {
		return true
	}
	if top := strings.SplitN(name, ".", 2)[0]; top != name {
		if info, err := os.Stat(filepath.Join(workspaceRoot, top)); err == nil && info.IsDir() {
			return true
		}
	}
	return false
}

// SpecNamesPythonModule reports whether specText explicitly names module as a
// file or module the approved spec expects to exist: "<module>.py" or
// "<path>.py" (dots mapped to path separators for a dotted module, e.g.
// "pkg.core" also matches "pkg/core.py") appearing as its own word
// (case-sensitive, so "sub" does not match inside "subtract"), or module (or
// its path form) appearing as a backtick-quoted “ `<module>` “ or
// “ `<module>.py` “ token. A plain prose word no longer counts on its own
// (found in review, onboarding P4: "add the result" wrongly named a module
// called "add") -- draft_spec.py's own prompt instructs the spec drafter to
// carry over file/module names the request states verbatim into Scope and
// the acceptance criteria as backtick-quoted code spans (e.g. "`sub.py`
// providing `subtract_numbers(a, b)`"), so a module actually intended by the
// request is still explicitly marked, not just mentioned in passing prose.
func SpecNamesPythonModule(specText, module string) bool {
	if module == "" || specText == "" {
		return false
	}
	for _, candidate := range []string{module, strings.ReplaceAll(module, ".", "/")} {
		if regexp.MustCompile(`\b` + regexp.QuoteMeta(candidate) + `\.py\b`).MatchString(specText) {
			return true
		}
		if strings.Contains(specText, "`"+candidate+"`") || strings.Contains(specText, "`"+candidate+".py`") {
			return true
		}
	}
	return false
}

// PythonUnresolvedModules returns, sorted, every distinct module imports
// binds a name to that is neither a stdlib module, nor a real module in the
// workspace, nor named anywhere in the approved spec text -- a module the
// drafter invented rather than one grounded in the real repository or the
// human-approved spec. imports is the name -> module map PythonOracleImports
// returns for one drafted file.
func PythonUnresolvedModules(imports map[string]string, workspaceRoot, specText string) []string {
	seen := map[string]bool{}
	var out []string
	for _, module := range imports {
		if seen[module] {
			continue
		}
		seen[module] = true
		if IsPythonStdlibModule(module) || ModuleExistsInWorkspace(workspaceRoot, module) || SpecNamesPythonModule(specText, module) {
			continue
		}
		out = append(out, module)
	}
	sort.Strings(out)
	return out
}

// pythonInterpreterCandidates are the python3 executables resolvePythonInterpreter
// searches PATH for, newest-first: the host inspector must accept syntax the
// Docker sandbox's own python3 runs (match statements, except*, PEP 695
// generics, PEP 701 f-strings), which this host's own /usr/bin/python3 can be
// too old for (found live, onboarding P4: a 3.9 host refused syntax the
// sandbox executes fine). Falling back to plain "python3" keeps a host with
// no versioned binary on PATH working exactly as before.
var pythonInterpreterCandidates = []string{"python3.13", "python3.12", "python3.11", "python3.10", "python3"}

// lookPath is exec.LookPath, indirected so tests can inject a fake PATH
// search without touching the real filesystem or PATH.
var lookPath = exec.LookPath

var (
	pythonInterpreterMu   sync.Mutex
	pythonInterpreterPath string
	pythonInterpreterErr  error
	pythonInterpreterDone bool
)

// resolvePythonInterpreter returns the absolute path of the newest available
// python3.NN on PATH (falling back to plain python3), caching the result for
// the process lifetime -- PATH is not expected to change between checks.
func resolvePythonInterpreter() (string, error) {
	pythonInterpreterMu.Lock()
	defer pythonInterpreterMu.Unlock()
	if pythonInterpreterDone {
		return pythonInterpreterPath, pythonInterpreterErr
	}
	for _, name := range pythonInterpreterCandidates {
		if path, err := lookPath(name); err == nil {
			pythonInterpreterPath, pythonInterpreterDone = path, true
			return pythonInterpreterPath, nil
		}
	}
	pythonInterpreterErr = fmt.Errorf("none of %s found on PATH", strings.Join(pythonInterpreterCandidates, ", "))
	pythonInterpreterDone = true
	return "", pythonInterpreterErr
}

// inspectPythonAST runs pythonASTInspector against src and returns the facts
// it reports. A python3 that cannot be found or that exits non-zero (a parse
// error, most commonly) is reported as the oracle failing to parse -- the
// same fail-closed shape CheckGoOracle uses for a Go source that does not
// parse.
func inspectPythonAST(src []byte) (pythonASTFacts, error) {
	ctx, cancel := context.WithTimeout(context.Background(), pythonCheckTimeout)
	defer cancel()
	interp, err := resolvePythonInterpreter()
	if err != nil {
		return pythonASTFacts{}, fmt.Errorf("could not check the Python oracle (no python3 interpreter found on PATH): %w", err)
	}
	cmd := exec.CommandContext(ctx, interp, "-I", "-c", pythonASTInspector)
	cmd.Stdin = bytes.NewReader(src)
	cmd.Env = []string{} // the inspector needs no environment; -I already ignores PYTHON* anyway. An
	// absolute interpreter path is used above, so an empty PATH here cannot
	// make the interpreter itself unresolvable.
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return pythonASTFacts{}, fmt.Errorf("could not check the Python oracle: python3 did not finish within %s", pythonCheckTimeout)
		}
		if _, isExit := err.(*exec.ExitError); isExit {
			return pythonASTFacts{}, fmt.Errorf("does not parse as Python (checked with %s): %s", filepath.Base(interp), summarizePythonParseError(stderr.String()))
		}
		return pythonASTFacts{}, fmt.Errorf("could not check the Python oracle (%s unavailable on the host): %w", filepath.Base(interp), err)
	}
	var facts pythonASTFacts
	if err := json.Unmarshal(stdout.Bytes(), &facts); err != nil {
		return pythonASTFacts{}, fmt.Errorf("could not check the Python oracle: unexpected checker output: %v", err)
	}
	return facts, nil
}

// summarizePythonParseError trims a python3 traceback down to its last
// non-blank line (the SyntaxError message itself), mirroring
// summarizeParseError's own "first few, not the whole traceback" bound.
func summarizePythonParseError(stderr string) string {
	lines := strings.Split(strings.TrimRight(stderr, "\n"), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if t := strings.TrimSpace(lines[i]); t != "" {
			return t
		}
	}
	return "python3 reported no error detail"
}
