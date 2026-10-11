// Package claims — this file keeps `go list -f '{{.ImportPath}}: {{.Doc}}'
// ./...` a usable index of the module (AGENTS.md, "Finding the code"): every
// package says what it is in one package comment.
package claims

import (
	"go/parser"
	"go/token"
	"maps"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// packageDocOpening is how the package comment of a package named name, in a
// directory whose last element is dir, may begin: "Package <name>" for a
// library, and for a command "Command <dir>" or the command's own name.
func packageDocOpening(name, dir, doc string) bool {
	if name != "main" {
		return strings.HasPrefix(doc, "Package "+name+" ")
	}
	return strings.HasPrefix(doc, "Command "+dir+" ") || strings.HasPrefix(doc, dir+" ")
}

func TestEveryPackageHasOnePackageComment(t *testing.T) {
	t.Parallel()
	if !packageDocOpening("run", "run", "Package run defines") || packageDocOpening("run", "run", "Route selection for") ||
		!packageDocOpening("main", "factoryd", "Command factoryd is") || packageDocOpening("main", "factoryd", "factoryd-meter is") {
		t.Fatal("packageDocOpening does not tell a package comment from a file header; the scan below proves nothing")
	}
	root := findRepoRoot(t)
	fset := token.NewFileSet()
	type pkg struct {
		name    string
		docs    []string
		opening bool
	}
	packages := map[string]*pkg{}
	for _, file := range trackedGoFiles(t, root) {
		if isTestFile(file) {
			continue
		}
		src, err := parser.ParseFile(fset, filepath.Join(root, file), nil, parser.PackageClauseOnly|parser.ParseComments)
		if err != nil {
			t.Fatalf("parse %s: %v", file, err)
		}
		dir := path.Dir(file)
		p := packages[dir]
		if p == nil {
			p = &pkg{name: src.Name.Name}
			packages[dir] = p
		}
		if src.Doc == nil {
			continue
		}
		p.docs = append(p.docs, file)
		p.opening = packageDocOpening(src.Name.Name, path.Base(dir), src.Doc.Text())
	}
	for _, dir := range []string{"cmd/factoryd", "internal/claims", "internal/run"} {
		if packages[dir] == nil {
			t.Fatalf("the scan did not reach %s; it proves nothing", dir)
		}
	}
	for _, dir := range slices.Sorted(maps.Keys(packages)) {
		p := packages[dir]
		switch {
		case len(p.docs) == 0:
			t.Errorf("%s has no package comment: say what the package does in a comment directly above one file's package clause", dir)
		case len(p.docs) > 1:
			t.Errorf("%s has a comment directly above the package clause in %s: keep the package comment in one, and put a blank line after a file's own header in the others", dir, strings.Join(p.docs, ", "))
		case !p.opening:
			t.Errorf("%s: the package comment in %s must begin \"Package <name> \", or for a command \"Command <directory> \"", dir, p.docs[0])
		}
	}
}
