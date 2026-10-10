// Package claims — this file makes the module's package-boundary rules a
// tested property instead of a convention living only in review comments.
// It parses import declarations directly (go/parser, parser.ImportsOnly)
// rather than shelling out to `go list`/`go vet`: claims_test.go's own doc
// comment already established that re-entering the Go toolchain from
// inside a test process is off the table here, and the same constraint
// applies to any new checker in this package.
package claims

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

const modulePath = "buildgate"

// minInternalPackages is a sanity floor on the import graph's size: if a
// walk bug (or a future repo reorganization this file wasn't updated for)
// silently visited far fewer packages than the module actually has, every
// rule below would trivially "pass" over an empty or near-empty graph.
// Measured 2026-09-27: `go list ./...` reports 44 packages; 30 leaves
// headroom for legitimate package removal without the floor itself
// becoming the thing that needs constant updating.
const minInternalPackages = 30

// importGraph maps a buildgate import path to the set of ALL its direct
// imports (stdlib, third-party, and buildgate/... alike) — the raw
// material every rule below narrows down for its own purpose.
type importGraph map[string]map[string]bool

// buildModuleImportGraph walks every non-test .go file under root and
// returns the module's direct-import graph, keyed by package import path
// (e.g. "buildgate/internal/policy"). It skips exactly what the go
// toolchain itself skips when resolving packages — directories starting
// with "." or "_", "testdata", and "vendor" — so a broken or unrelated .go
// file under, say, a `.claude/worktrees/*` agent checkout inside this repo
// can never fail this test.
func buildModuleImportGraph(t *testing.T, root string) importGraph {
	t.Helper()
	graph := importGraph{}
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if path != root && (strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") || name == "testdata" || name == "vendor") {
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(path) != ".go" || isTestFile(path) {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		dir := filepath.ToSlash(filepath.Dir(rel))
		importPath := modulePath
		if dir != "." {
			importPath = modulePath + "/" + dir
		}
		f, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		if graph[importPath] == nil {
			graph[importPath] = map[string]bool{}
		}
		for _, imp := range f.Imports {
			p, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				return err
			}
			graph[importPath][p] = true
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk repo for import graph: %v", err)
	}
	if len(graph) < minInternalPackages {
		t.Fatalf("import graph has only %d packages, want at least %d — the walk is almost certainly broken (wrong root, or skipping too much), which would make every rule below pass vacuously", len(graph), minInternalPackages)
	}
	return graph
}

// requirePackage fails the test if pkg was never observed in the graph
// (no non-test .go file for it was found), instead of every rule below
// silently ranging over a nil/empty map and passing.
func requirePackage(t *testing.T, g importGraph, pkg string) {
	t.Helper()
	if _, ok := g[pkg]; !ok {
		t.Fatalf("%s was not found in the import graph at all — either it no longer exists, or the walk that builds this graph is broken", pkg)
	}
}

// buildgateImports returns pkg's direct buildgate/... imports only.
func buildgateImports(g importGraph, pkg string) map[string]bool {
	out := map[string]bool{}
	for imp := range g[pkg] {
		if imp == modulePath || strings.HasPrefix(imp, modulePath+"/") {
			out[imp] = true
		}
	}
	return out
}

// findForbiddenImportChain DFS-searches pkg's transitive buildgate/...
// imports for any package in forbidden, returning the chain from pkg to
// the first one found (inclusive) so a failing test can name exactly how
// the forbidden dependency was reached. Returns nil if none is reachable.
func findForbiddenImportChain(g importGraph, pkg string, forbidden map[string]bool) []string {
	visited := map[string]bool{pkg: true}
	var dfs func(p string, path []string) []string
	dfs = func(p string, path []string) []string {
		imports := buildgateImports(g, p)
		names := make([]string, 0, len(imports))
		for imp := range imports {
			names = append(names, imp)
		}
		sort.Strings(names) // deterministic chain in a failure message
		for _, imp := range names {
			next := append(append([]string{}, path...), imp)
			if forbidden[imp] {
				return next
			}
			if !visited[imp] {
				visited[imp] = true
				if chain := dfs(imp, next); chain != nil {
					return chain
				}
			}
		}
		return nil
	}
	return dfs(pkg, []string{pkg})
}

// TestPackageBoundaryRules asserts the module's package-boundary rules
// directly against the repo's real import graph. Each rule is one table
// row with its own reason string, so a failing rule's message names both
// the rule and the offending import (chain).
//
// Rules 1 and 4 below check DIRECT imports only, deliberately: policy
// transitively reaches os/net/http/syscall through internal/run ->
// internal/meter, and modelrole transitively reaches sandbox/meter/
// modelhost through internal/sessionconfig — neither package is
// transitively "pure". What IS true, and enforced here, is that neither
// package's own source directly names anything outside its stated
// allow-list; CLAIMS.md's row for this test is worded to match ("directly
// imports only ..."), not "stays pure".
//
// Measured against this repo (go list -f '{{.ImportPath}}
// {{join .Imports " "}}' ./internal/..., 2026-09-27): every rule below
// holds today. None was dropped.
func TestPackageBoundaryRules(t *testing.T) {
	t.Parallel()
	root := findRepoRoot(t)
	g := buildModuleImportGraph(t, root)

	for _, pkg := range []string{
		"buildgate/internal/policy",
		"buildgate/internal/harness",
		"buildgate/internal/modelrole",
	} {
		requirePackage(t, g, pkg)
	}

	t.Run("policy directly imports only its stdlib allow-list and run", func(t *testing.T) {
		// buildgate/internal/policy may directly import only stdlib
		// packages from this allow-list (measured: exactly fmt, path,
		// regexp, strings today) plus buildgate/internal/run — never a
		// package that touches the OS, the filesystem beyond stdlib
		// string handling, or the network (os, os/exec, net, net/http,
		// syscall, io/fs) directly. This is a direct-import rule only:
		// policy is not transitively pure (see this test's own doc
		// comment).
		allowedStdlib := map[string]bool{"fmt": true, "path": true, "regexp": true, "strings": true}
		const pkg = "buildgate/internal/policy"
		for imp := range g[pkg] {
			if imp == "buildgate/internal/run" {
				continue
			}
			if strings.HasPrefix(imp, modulePath) {
				t.Errorf("policy directly imports only its stdlib allow-list and run: %s imports %s, only buildgate/internal/run is allowed among buildgate/... packages", pkg, imp)
				continue
			}
			if !allowedStdlib[imp] {
				t.Errorf("policy directly imports only its stdlib allow-list and run: %s imports %q, which is outside its stdlib allow-list %v", pkg, imp, sortedKeys(allowedStdlib))
			}
		}
	})

	t.Run("harness never depends on policy/release/request/workflow/api", func(t *testing.T) {
		// harness sits below the release/request/workflow
		// layer in the dependency direction this repo relies on: it
		// extracts the agent scripts and names the coding-agent CLIs, and must never need to know
		// about release policy, the request pipeline, Temporal
		// workflows, or the HTTP api surface, transitively or otherwise.
		// Unlike rule 1/4 above, this one IS a transitive check.
		forbidden := map[string]bool{
			"buildgate/internal/policy":   true,
			"buildgate/internal/release":  true,
			"buildgate/internal/request":  true,
			"buildgate/internal/workflow": true,
			"buildgate/internal/api":      true,
		}
		for _, pkg := range []string{"buildgate/internal/harness"} {
			if chain := findForbiddenImportChain(g, pkg, forbidden); chain != nil {
				t.Errorf("harness never depends on policy/release/request/workflow/api: %s", strings.Join(chain, " -> "))
			}
		}
	})

	t.Run("meter imports only its generated middlewarepb among buildgate packages", func(t *testing.T) {
		// internal/meter is the accounting and usage parsing of a model
		// route (ceilings, windows, pricing, per-format usage parsers, the
		// usage ledger) and the route constants: a standalone leaf, so it can run in a process of its own without
		// pulling the rest of the module's graph in with it. The one exception
		// is middlewarepb, the generated OpenShell supervisor-middleware stubs
		// the factoryd-meter service speaks, which has no buildgate imports.
		const pkg = "buildgate/internal/meter"
		requirePackage(t, g, pkg)
		for imp := range buildgateImports(g, pkg) {
			if imp != "buildgate/internal/meter/middlewarepb" {
				t.Errorf("meter imports only its generated middlewarepb among buildgate packages: %s imports %s", pkg, imp)
			}
		}
		if imports := buildgateImports(g, "buildgate/internal/meter/middlewarepb"); len(imports) > 0 {
			t.Errorf("middlewarepb has no buildgate dependencies: imports %v", sortedKeys(imports))
		}
	})

	t.Run("openshell imports only sandbox among buildgate packages", func(t *testing.T) {
		// internal/openshell is the one package that imports the OpenShell
		// SDK: it turns a sandbox.SandboxRequest into the gateway's spec and
		// policy. Kept a leaf over internal/sandbox so the SDK's types reach
		// no other package, and so what a worker is allowed is decided in
		// internal/sandbox, where it is tested without a gateway.
		const pkg = "buildgate/internal/openshell"
		requirePackage(t, g, pkg)
		for imp := range buildgateImports(g, pkg) {
			if imp != "buildgate/internal/sandbox" {
				t.Errorf("openshell imports only sandbox among buildgate packages: %s imports %s", pkg, imp)
			}
		}
		const testPkg = "buildgate/internal/sandbox/sandboxtest"
		requirePackage(t, g, testPkg)
		for imp := range buildgateImports(g, testPkg) {
			if imp != "buildgate/internal/sandbox" {
				t.Errorf("sandboxtest imports only sandbox among buildgate packages: %s imports %s", testPkg, imp)
			}
		}
	})

	t.Run("modelrole directly imports only sessionconfig/meter/sandbox/prices/harness", func(t *testing.T) {
		// Direct-import rule only, like policy above. Since Phase 2C-1
		// (route selection), modelrole.SelectRoute also names
		// buildgate/internal/meter (credential-mode constants, the
		// Copilot path-prefix helper) and buildgate/internal/sandbox
		// (RoutePolicy, CredentialSafeForUpstream) directly, alongside
		// sessionconfig -- all three are leaves modelrole is allowed to
		// sit above (see modelrole's own package doc comment). Since
		// 2026-09-28, buildgate/internal/prices too (the compiled real-
		// provider-price table, resolving a model's own id into the
		// RoutePolicy price fields) -- also a standalone leaf, see its own
		// "has no buildgate dependencies" check below. Since H3, buildgate/internal/harness too (the compiled harness registry, resolving a role's harness name into its Descriptor). It must still never
		// import cmd/, internal/workflow, or internal/api.
		const pkg = "buildgate/internal/modelrole"
		allowed := map[string]bool{
			"buildgate/internal/sessionconfig": true,
			"buildgate/internal/meter":         true,
			"buildgate/internal/sandbox":       true,
			"buildgate/internal/prices":        true,
			"buildgate/internal/harness":       true,
		}
		for imp := range buildgateImports(g, pkg) {
			if !allowed[imp] {
				t.Errorf("modelrole directly imports only sessionconfig/meter/sandbox/prices/harness: %s imports %s", pkg, imp)
			}
		}
	})

	t.Run("prices has no buildgate dependencies", func(t *testing.T) {
		// internal/prices is the single compiled real-provider-price
		// table (internal/prices/prices.yml, go:embed): a standalone leaf,
		// like internal/meter above, so nothing else in the module can
		// pull the rest of the graph in through it.
		const pkg = "buildgate/internal/prices"
		if imports := buildgateImports(g, pkg); len(imports) > 0 {
			t.Errorf("prices has no buildgate dependencies: %s imports %v", pkg, sortedKeys(imports))
		}
	})

	t.Run("hostcontrol directly imports only its allow-list", func(t *testing.T) {
		// internal/hostcontrol starts, finds and stops the operator's
		// processes and containers. It reaches the machine through the
		// Deps its caller passes, so among buildgate packages it needs
		// only the embedded Temporal compose file (the module root), the
		// heartbeat and console-address records it reads, and output
		// helpers. It must never import the request pipeline, the
		// workflows or the sandbox launcher, and never requestdriver.
		const pkg = "buildgate/internal/hostcontrol"
		requirePackage(t, g, pkg)
		allowed := map[string]bool{
			"buildgate":                          true,
			"buildgate/internal/consolelink":     true,
			"buildgate/internal/daemonheartbeat": true,
			"buildgate/internal/sanitize":        true,
			"buildgate/internal/spinner":         true,
		}
		for imp := range buildgateImports(g, pkg) {
			if !allowed[imp] {
				t.Errorf("hostcontrol directly imports only its allow-list: %s imports %s", pkg, imp)
			}
		}
	})

	t.Run("requestdriver directly imports only its allow-list", func(t *testing.T) {
		// internal/requestdriver advances a request one step at a time.
		// It sits above the request, run, policy, release and workflow
		// packages it reads and writes through, and reaches GitHub, git
		// push and the build entry point through the Deps its caller
		// passes. It must never import hostcontrol: starting and stopping
		// the operator's processes is not the driver's business.
		const pkg = "buildgate/internal/requestdriver"
		requirePackage(t, g, pkg)
		allowed := map[string]bool{
			"buildgate/internal/api":           true,
			"buildgate/internal/codereview":    true,
			"buildgate/internal/consolelink":   true,
			"buildgate/internal/evidence":      true,
			"buildgate/internal/forge":         true,
			"buildgate/internal/handoff":       true,
			"buildgate/internal/notify":        true,
			"buildgate/internal/policy":        true,
			"buildgate/internal/projectconfig": true,
			"buildgate/internal/release":       true,
			"buildgate/internal/request":       true,
			"buildgate/internal/requestsubmit": true,
			"buildgate/internal/run":           true,
			"buildgate/internal/runner":        true,
			"buildgate/internal/sandbox":       true,
			"buildgate/internal/sanitize":      true,
			"buildgate/internal/sessionconfig": true,
			"buildgate/internal/ticketspec":    true,
			"buildgate/internal/workflow":      true,
			"buildgate/internal/workspace":     true,
		}
		for imp := range buildgateImports(g, pkg) {
			if !allowed[imp] {
				t.Errorf("requestdriver directly imports only its allow-list: %s imports %s", pkg, imp)
			}
		}
	})

	t.Run("nothing under internal imports cmd", func(t *testing.T) {
		// Go's own compiler already forbids this as an import cycle
		// wherever cmd/factoryd imports the internal package in
		// question (as it does, pervasively) — this assertion exists to
		// document the direction as a rule, not because Go could
		// silently accept a violation.
		for pkg, imports := range g {
			if !strings.HasPrefix(pkg, "buildgate/internal/") {
				continue
			}
			for imp := range imports {
				if strings.HasPrefix(imp, "buildgate/cmd/") {
					t.Errorf("nothing under internal imports cmd: %s imports %s", pkg, imp)
				}
			}
		}
	})
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestFindForbiddenImportChainCatchesAViolation is the self-test proving
// the checker used above actually catches a violation, over a synthetic
// in-memory import graph — the real repo graph holding today (see
// TestPackageBoundaryRules) proves nothing about whether the checker would
// notice a regression tomorrow.
func TestFindForbiddenImportChainCatchesAViolation(t *testing.T) {
	t.Parallel()
	g := importGraph{
		"buildgate/internal/harness": {"buildgate/internal/helper": true},
		"buildgate/internal/helper":  {"buildgate/internal/policy": true},
		"buildgate/internal/policy":  {"fmt": true},
	}
	forbidden := map[string]bool{"buildgate/internal/policy": true}

	chain := findForbiddenImportChain(g, "buildgate/internal/harness", forbidden)
	if chain == nil {
		t.Fatal("findForbiddenImportChain did not flag a synthetic graph with a real forbidden edge (harness -> helper -> policy); the boundary checker proves nothing")
	}
	want := []string{"buildgate/internal/harness", "buildgate/internal/helper", "buildgate/internal/policy"}
	if len(chain) != len(want) {
		t.Fatalf("chain = %v, want a chain of length %d matching %v", chain, len(want), want)
	}
	for i := range want {
		if chain[i] != want[i] {
			t.Fatalf("chain = %v, want %v", chain, want)
		}
	}

	// And a graph with no forbidden edge at all must report none.
	clean := importGraph{
		"buildgate/internal/harness": {"buildgate/internal/helper": true},
		"buildgate/internal/helper":  {"fmt": true},
	}
	if chain := findForbiddenImportChain(clean, "buildgate/internal/harness", forbidden); chain != nil {
		t.Fatalf("findForbiddenImportChain flagged a clean graph with no forbidden edge: %v", chain)
	}
}

// TestHostcontroltestIsATestsOnlyLeaf: internal/hostcontrol/hostcontroltest
// holds the fake docker and colima the tests of hostcontrol and of
// cmd/factoryd share. It must not import hostcontrol (hostcontrol's own
// in-package tests import it), and no shipped code may import it: the graph
// is built from non-test files only, so any importer in it is shipped code.
func TestHostcontroltestIsATestsOnlyLeaf(t *testing.T) {
	t.Parallel()
	g := buildModuleImportGraph(t, findRepoRoot(t))
	const testPkg = "buildgate/internal/hostcontrol/hostcontroltest"
	requirePackage(t, g, testPkg)
	for imp := range buildgateImports(g, testPkg) {
		if imp != "buildgate/internal/daemonheartbeat" {
			t.Errorf("hostcontroltest imports only daemonheartbeat among buildgate packages: %s imports %s", testPkg, imp)
		}
	}
	for pkg, imports := range g {
		if imports[testPkg] {
			t.Errorf("hostcontroltest is imported by tests only: non-test code of %s imports it", pkg)
		}
	}
}
