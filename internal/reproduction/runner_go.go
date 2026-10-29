package reproduction

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// goTest runs a Go test file with go test -json, selecting only its own
// top-level tests. The per-test timeout is go test's -timeout over all of
// them: when it passes, the binary panics with "test timed out" and every
// selected test still running is a timeout. The panic comes from the
// binary's own timer goroutine and the runtime preempts a busy loop, so a
// hang that never yields still reads as a timeout.
type goTest struct{}

func (goTest) ID() string { return "go-test-json" }

func (goTest) Applies(p string, root RootFacts) bool {
	return strings.HasSuffix(p, "_test.go") && root.Has("go.mod")
}

// Tests are the file's top-level func TestXxx(t *testing.T), TestMain
// excepted, as go test names them, each spanning its declaration (go/parser).
func (goTest) Tests(p string, content []byte) ([]TestSpan, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path.Base(p), content, parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	testing := testingName(f)
	var spans []TestSpan
	for _, d := range f.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if ok && fn.Recv == nil && isGoTestName(fn.Name.Name) && takesTestingT(fn, testing) {
			spans = append(spans, TestSpan{ID: fn.Name.Name, Start: fset.Position(fn.Pos()).Line, End: fset.Position(fn.End()).Line})
		}
	}
	return uniqueSpans(spans), nil
}

// testingName is the file's local name for package testing ("." for a dot
// import, "" when the file does not import it).
func testingName(f *ast.File) string {
	for _, imp := range f.Imports {
		if p, _ := strconv.Unquote(imp.Path.Value); p != "testing" {
			continue
		}
		if imp.Name != nil {
			return imp.Name.Name
		}
		return "testing"
	}
	return ""
}

// isGoTestName: Test, or Test followed by a rune that is not lower case.
func isGoTestName(name string) bool {
	if name == "TestMain" || !strings.HasPrefix(name, "Test") {
		return false
	}
	rest := name[len("Test"):]
	if rest == "" {
		return true
	}
	r, _ := utf8.DecodeRuneInString(rest)
	return !unicode.IsLower(r)
}

// takesTestingT: one parameter of type *testing.T, no results.
func takesTestingT(fn *ast.FuncDecl, testing string) bool {
	params := fn.Type.Params.List
	if testing == "" || len(params) != 1 || len(params[0].Names) > 1 || fn.Type.Results != nil {
		return false
	}
	star, ok := params[0].Type.(*ast.StarExpr)
	if !ok {
		return false
	}
	if testing == "." {
		id, ok := star.X.(*ast.Ident)
		return ok && id.Name == "T"
	}
	sel, ok := star.X.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == testing && sel.Sel.Name == "T"
}

func (goTest) Command(inv Invocation) Command {
	pkg := "./" + path.Dir(inv.Path)
	if path.Dir(inv.Path) == "." {
		pkg = "."
	}
	// The binary's own timeout ends before the launch's, so the report
	// names the test that was running.
	timeout := launchTimeout(inv.TestIDs) - 30*time.Second
	run := "^(" + strings.Join(inv.TestIDs, "|") + ")$"
	script := fmt.Sprintf("go test -json -count=1 -run %s -timeout %ds %s > %s",
		shellQuote(run), int(timeout.Seconds()), shellQuote(pkg), shellQuote(inv.ReportPath))
	return Command{Script: script, ReportPath: inv.ReportPath, Timeout: launchTimeout(inv.TestIDs)}
}

// goEvent is one line of go test -json (test2json) output.
type goEvent struct {
	Action      string
	Test        string
	Output      string
	FailedBuild string
}

// goReport is the report folded per top-level test.
type goReport struct {
	compile bool
	// build is the text of the build's own output (build-output events, and
	// output that belongs to no test), read for the reproduction's path.
	build    string
	timedOut bool
	terminal map[string]string // test -> pass|fail|skip
	panicked map[string]bool
	any      bool
}

// goModuleError is the go command's text for a module it could not resolve
// or fetch: a file-anchored line whose message begins "<module>@<version>:",
// the go command's resolution messages, and the network and TLS errors of a
// fetch.
var goModuleError = regexp.MustCompile(`(?m)^\S+\.go:\d+:\d+: \S+@v\S*: ` +
	`|cannot find module providing package|no required module provides package|module lookup disabled|missing go\.sum entry|checksum mismatch|unknown revision|invalid version|unrecognized import path|verifying module` +
	`|dial tcp|reading https?://|Get "http|x509:|tls:|proxyconnect|connection reset|TLS handshake timeout`)

// Classify: a build or setup failure is a compile only when the build's
// output names the reproduction's own file and is no module resolution
// failure (compileFailure; Go 1.24 and later put the compiler's output in
// the report as build-output events): an undefined name or a type error in
// the reproduction. A module that cannot be resolved or fetched, whichever
// file imports it, a dependency that does not build, and an older Go that
// names no file are missing.
func (goTest) Classify(p string, report []byte, _ int, ids []string) (map[string]Kind, Outcome) {
	r := readGoReport(report)
	if r.compile && compileFailure(r.build, p, goModuleError) {
		return allKind(ids, KindCompile), OutcomeCompile
	}
	if r.compile || !r.any {
		return allKind(ids, KindMissing), OutcomeMissing
	}
	kinds := map[string]Kind{}
	for _, id := range ids {
		kinds[id] = r.kind(id)
	}
	return fromKinds(kinds, ids)
}

func (r goReport) kind(id string) Kind {
	switch r.terminal[id] {
	case "pass":
		return KindPass
	case "skip":
		return KindSkipped
	case "fail":
		if r.panicked[id] {
			return KindError
		}
		return KindFail
	}
	if r.timedOut {
		return KindTimeout // still running when the binary's timeout passed
	}
	return KindMissing
}

func readGoReport(report []byte) goReport {
	r := goReport{terminal: map[string]string{}, panicked: map[string]bool{}}
	sc := bufio.NewScanner(bytes.NewReader(report))
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	for sc.Scan() {
		var ev goEvent
		if json.Unmarshal(sc.Bytes(), &ev) != nil || ev.Action == "" {
			continue
		}
		r.any = true
		r.add(ev)
	}
	return r
}

func (r *goReport) add(ev goEvent) {
	top, _, _ := strings.Cut(ev.Test, "/")
	if ev.Test == "" && (ev.Action == "build-output" || ev.Action == "output") {
		r.build += ev.Output
	}
	switch {
	case ev.Action == "build-fail" || ev.FailedBuild != "":
		r.compile = true
	case ev.Action == "output" && ev.Test == "" && (strings.Contains(ev.Output, "[build failed]") || strings.Contains(ev.Output, "[setup failed]")):
		r.compile = true
	case ev.Action == "output" && strings.HasPrefix(ev.Output, "panic: test timed out"):
		r.timedOut = true
	case ev.Action == "output" && top != "" && strings.HasPrefix(ev.Output, "panic: "):
		r.panicked[top] = true
	case ev.Test != "" && ev.Test == top && (ev.Action == "pass" || ev.Action == "fail" || ev.Action == "skip"):
		r.terminal[top] = ev.Action
	}
}
