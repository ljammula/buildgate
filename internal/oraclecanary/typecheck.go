package oraclecanary

import (
	"fmt"
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// typeCheckTimeout bounds one TypeCheckGoOracles call (type-checking the
// standard library from source is the slow part). A check that runs out of
// time is reported as skipped, never as a pass. A var only so this package's
// tests can raise it: under a loaded full-suite run the first check (which
// loads the standard library) can exceed 60s and would then skip, and pause,
// every later check in the test process.
var typeCheckTimeout = 60 * time.Second

const (
	// typeCheckMaxProblems bounds how many problems one call reports.
	typeCheckMaxProblems = 12
	// typeCheckCooldown is how long TypeCheckGoOracles refuses to start a new
	// check after one timed out. go/types has no cancellation: a timed-out
	// goroutine keeps running (and, if it is inside stdSource.load, keeps
	// holding stdSource.mu) until it finishes on its own. Found in review
	// (2026-09-21): without a cooldown, every later draft's check would spawn
	// another goroutine that piles up behind the same stuck mutex, each
	// burning its own 60s before reporting skipped -- an untrusted drafted
	// file can degrade the daemon for as long as the model keeps drafting.
	// The cooldown trades a stretch of honestly-skipped checks (never a false
	// pass; CheckGoOracle's parse/package-clause gate is unaffected) for
	// bounding how many goroutines pile up.
	typeCheckCooldown = 5 * time.Minute
)

// degradedUntil is a unix-nanosecond deadline before which TypeCheckGoOracles
// skips immediately instead of starting another goroutine, set after a
// timeout. Zero means not degraded. Process-wide by design, matching
// stdSource: the resource being protected (stdSource.mu and this process's
// CPU) is process-wide too.
var degradedUntil atomic.Int64

func typeCheckDegradedNotice() string {
	until := degradedUntil.Load()
	if until == 0 {
		return ""
	}
	remaining := time.Until(time.Unix(0, until))
	if remaining <= 0 {
		return ""
	}
	return fmt.Sprintf("the type check is paused for %s after a previous attempt did not finish within %s (see typeCheckCooldown's own doc comment)", remaining.Round(time.Second), typeCheckTimeout)
}

// TypeCheckResult is the outcome of TypeCheckGoOracles.
type TypeCheckResult struct {
	// Problems are compile errors the oracle would hit whatever the target
	// package looks like once the change is built, in "<file>:<line>:<col>:
	// message" form, sorted, at most typeCheckMaxProblems (plus a "... and N
	// more" line).
	Problems []string
	// Skipped says why the check could not run to completion ("" when it did).
	// A skipped check found nothing and vouches for nothing.
	Skipped string
}

// TypeCheckGoOracles is the host-side compile self-check of drafted Go oracles.
// It never compiles or runs anything and never starts a process: the files are
// parsed and type-checked with go/types in process, and the standard library
// is read as source from GOROOT (cgo off, function bodies skipped).
//
// The target package does not exist yet in its final form, so this is a
// deliberately narrow check. Every import that is not in the standard library
// is replaced by an empty stand-in, and every error about a name that may
// belong to the target package or to such an import ("undefined: X", "could
// not import", "imported and not used" of a stand-in) is dropped; expressions
// involving such names have an invalid type, which go/types does not complain
// about further. What remains is what does not depend on the target: misuse
// of the standard library, arity and result mismatches against the oracle's
// own helpers, unused imports and variables, redeclarations across the oracle
// files of one package, and type errors such as a no-value call used as an
// argument. It therefore misses every mismatch against the target package's
// real API, and it is not a substitute for the build.
//
// Files are grouped by (target directory, package clause) and each group is
// checked as one package, the way the build splices them together. targets maps
// file name to manifest target_path ("" allowed). An unparseable file is left
// to CheckGoOracle, which reports it.
func TypeCheckGoOracles(files map[string][]byte, targets map[string]string) TypeCheckResult {
	if notice := typeCheckDegradedNotice(); notice != "" {
		return TypeCheckResult{Skipped: notice}
	}
	done := make(chan TypeCheckResult, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				done <- TypeCheckResult{Skipped: fmt.Sprintf("the type checker panicked: %v", r)}
			}
		}()
		done <- typeCheckGoOracles(files, targets)
	}()
	select {
	case res := <-done:
		return res
	case <-time.After(typeCheckTimeout):
		degradedUntil.Store(time.Now().Add(typeCheckCooldown).UnixNano())
		return TypeCheckResult{Skipped: fmt.Sprintf("the type check did not finish within %s; self-check paused for %s to avoid stacking more work behind it", typeCheckTimeout, typeCheckCooldown)}
	}
}

func typeCheckGoOracles(files map[string][]byte, targets map[string]string) TypeCheckResult {
	fset := token.NewFileSet()
	type group struct {
		pkg   string
		files []*ast.File
	}
	groups := map[string]*group{}
	names := make([]string, 0, len(files))
	for name := range files {
		if strings.HasSuffix(name, ".go") {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	for _, name := range names {
		if len(files[name]) > maxGoOracleCheckBytes {
			continue
		}
		f, err := parser.ParseFile(fset, name, files[name], parser.SkipObjectResolution)
		if err != nil || f.Name == nil {
			continue
		}
		dir := ""
		if t := targets[name]; t != "" {
			dir = path.Dir(t)
		}
		key := dir + "\x00" + f.Name.Name
		g := groups[key]
		if g == nil {
			g = &group{pkg: f.Name.Name}
			groups[key] = g
		}
		g.files = append(g.files, f)
	}
	imp := newOracleImporter()
	var problems []string
	keys := make([]string, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		g := groups[k]
		conf := types.Config{
			Importer: imp,
			Error: func(err error) {
				te, ok := err.(types.Error)
				if !ok || imp.benign(te.Msg, g.files) {
					return
				}
				p := fset.Position(te.Pos)
				problems = append(problems, fmt.Sprintf("%s:%d:%d: %s", p.Filename, p.Line, p.Column, te.Msg))
			},
		}
		_, _ = conf.Check(g.pkg, fset, g.files, nil)
	}
	sort.Strings(problems)
	if len(problems) > typeCheckMaxProblems {
		problems = append(problems[:typeCheckMaxProblems:typeCheckMaxProblems], fmt.Sprintf("... and %d more", len(problems)-typeCheckMaxProblems))
	}
	return TypeCheckResult{Problems: problems}
}

// oracleImporter resolves standard-library imports by type-checking their
// GOROOT source (function bodies skipped, cgo off so no tool is ever run) and
// stands in an empty package for everything else.
type oracleImporter struct {
	fakes map[string]bool // import paths that were replaced by a stand-in
	cache map[string]*types.Package
}

func newOracleImporter() *oracleImporter {
	return &oracleImporter{fakes: map[string]bool{}, cache: map[string]*types.Package{}}
}

// Import is the importer the oracle files are checked with.
func (o *oracleImporter) Import(p string) (*types.Package, error) {
	if pkg, ok := o.cache[p]; ok {
		return pkg, nil
	}
	if isStdlibImport(p) {
		if pkg, err := stdSource.load(p, ""); err == nil {
			o.cache[p] = pkg
			return pkg, nil
		}
	}
	o.fakes[p] = true
	pkg := types.NewPackage(p, guessPackageName(p))
	pkg.MarkComplete()
	o.cache[p] = pkg
	return pkg, nil
}

// stdSource type-checks standard-library packages from GOROOT source once per
// process and shares the result (go/types packages are immutable once checked):
// re-checking net/http for every draft would cost seconds each, and far more
// under the race detector. Calls are serialised by mu.
var stdSource = &stdLoader{fset: token.NewFileSet(), cache: map[string]*types.Package{}}

type stdLoader struct {
	mu    sync.Mutex
	fset  *token.FileSet
	cache map[string]*types.Package // by resolved import path (vendored ones included)
}

// load resolves p as imported from srcDir ("" for a user import) and
// type-checks it, function bodies ignored and cgo off so no tool is ever run.
func (l *stdLoader) load(p, srcDir string) (*types.Package, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.loadLocked(p, srcDir)
}

// fromDir is the importer for a standard-library package living in dir, which
// alone may resolve GOROOT's vendored imports.
type fromDir struct {
	l   *stdLoader
	dir string
}

func (f fromDir) Import(p string) (*types.Package, error) { return f.l.loadLocked(p, f.dir) }

func (l *stdLoader) loadLocked(p, srcDir string) (*types.Package, error) {
	if p == "unsafe" {
		return types.Unsafe, nil
	}
	ctxt := build.Default
	ctxt.CgoEnabled = false
	bp, err := ctxt.Import(p, srcDir, 0)
	if err != nil {
		return nil, err
	}
	if pkg, ok := l.cache[bp.ImportPath]; ok {
		return pkg, nil
	}
	var files []*ast.File
	for _, name := range bp.GoFiles {
		f, err := parser.ParseFile(l.fset, filepath.Join(bp.Dir, name), nil, parser.SkipObjectResolution)
		if err != nil {
			return nil, err
		}
		files = append(files, f)
	}
	conf := types.Config{Importer: fromDir{l, bp.Dir}, IgnoreFuncBodies: true, FakeImportC: true, Error: func(error) {}}
	pkg, _ := conf.Check(bp.ImportPath, l.fset, files, nil)
	if pkg == nil {
		return nil, fmt.Errorf("cannot type-check %s", p)
	}
	l.cache[bp.ImportPath] = pkg
	return pkg, nil
}

// isStdlibImport reports whether p is a standard-library package whose source
// is present under GOROOT. Anything else is never looked up on disk.
func isStdlibImport(p string) bool {
	if p == "unsafe" {
		return true
	}
	first, _, _ := strings.Cut(p, "/")
	if p == "" || p == "C" || strings.Contains(first, ".") || path.Clean(p) != p || strings.HasPrefix(p, "/") {
		return false
	}
	root := build.Default.GOROOT
	if root == "" {
		return false
	}
	info, err := os.Stat(filepath.Join(root, "src", filepath.FromSlash(p)))
	return err == nil && info.IsDir()
}

// guessPackageName is the conventional package name for an import path.
func guessPackageName(p string) string {
	elems := strings.Split(p, "/")
	name := elems[len(elems)-1]
	if len(elems) > 1 && len(name) > 1 && name[0] == 'v' && strings.Trim(name[1:], "0123456789") == "" {
		name = elems[len(elems)-2]
	}
	name = strings.TrimPrefix(name, "go-")
	name = strings.NewReplacer("-", "_", ".", "_").Replace(name)
	if name == "" {
		return "pkg"
	}
	return name
}

// standInName reports whether name is the local name, in any of files, of an
// import that was replaced by a stand-in.
func (o *oracleImporter) standInName(name string, files []*ast.File) bool {
	for _, f := range files {
		for _, spec := range f.Imports {
			p, err := strconv.Unquote(spec.Path.Value)
			if err != nil || !o.fakes[p] {
				continue
			}
			local := guessPackageName(p)
			if spec.Name != nil {
				local = spec.Name.Name
			}
			if local == name {
				return true
			}
		}
	}
	return false
}

// benign reports whether msg is an error that depends on a name the target
// package (or a stand-in import) is expected to define.
func (o *oracleImporter) benign(msg string, files []*ast.File) bool {
	if rest, ok := strings.CutPrefix(msg, "undefined: "); ok {
		// "undefined: pkg.X" is benign only when pkg is a stand-in import (by its
		// local name, alias included); "undefined: strings.Foo" is a real error.
		qualifier, _, qualified := strings.Cut(rest, ".")
		return !qualified || o.standInName(qualifier, files)
	}
	if strings.Contains(msg, "could not import") {
		return true
	}
	if strings.Contains(msg, "and not used") && strings.Contains(msg, "imported") {
		for p := range o.fakes {
			if strings.Contains(msg, fmt.Sprintf("%q", p)) {
				return true
			}
		}
	}
	return false
}
