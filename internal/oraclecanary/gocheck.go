package oraclecanary

import (
	"bytes"
	"fmt"
	"go/build/constraint"
	"go/parser"
	"go/scanner"
	"go/token"
	"io"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"
)

const (
	// maxGoOracleCheckBytes bounds the oracle source handed to the parser.
	maxGoOracleCheckBytes = 1 << 20
	// goPackageHeaderBytes bounds how much of an existing repo file is read to
	// find its package clause.
	goPackageHeaderBytes = 256 << 10
	// goPackageDirMaxEntries bounds how many entries of the target directory
	// are looked at.
	goPackageDirMaxEntries = 4096
)

// CheckGoOracle is the static, host-side check of one drafted Go oracle. It
// only PARSES: nothing is compiled or executed, so model-written source never
// runs on the host (a full compile/typecheck needs the go toolchain and the
// repo's dependency graph, and belongs inside the sandbox).
//
// It refuses (1) source that does not parse, and (2) when targetPath is given
// (workspace-relative, slash-separated), a package clause that cannot share a
// directory with the Go package already living at path.Dir(targetPath): the
// oracle is spliced into that directory, so `package store` beside
// `package summary` never builds. Allowed: the existing package P or P_test,
// where P comes from the directory's non-test .go files that the build would
// select (//go:build and // +build constraints and GOOS/GOARCH file-name
// suffixes evaluated for linux on amd64 or arm64, cgo on or off: selected if any combination selects it), else
// from its test files. A directory with no such files (or one that does not
// exist yet) accepts any package name. It fails closed when the package
// cannot be established: an unreadable directory, more than
// goPackageDirMaxEntries entries, or a candidate file whose package clause lies
// beyond the bounded header read. A workspace root that does not exist skips
// (2); the root itself may be a symlink, anything below it may not.
func CheckGoOracle(workspace, targetPath string, src []byte) error {
	if len(src) > maxGoOracleCheckBytes {
		return fmt.Errorf("Go oracle is %d bytes, too large to check (limit %d)", len(src), maxGoOracleCheckBytes)
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "oracle.go", src, parser.SkipObjectResolution)
	if err != nil {
		return fmt.Errorf("does not parse as Go: %s", summarizeParseError(err))
	}
	if targetPath == "" {
		return nil
	}
	if targetPath != path.Clean(targetPath) || path.IsAbs(targetPath) || targetPath == ".." || strings.HasPrefix(targetPath, "../") || strings.ContainsRune(targetPath, 0) {
		return fmt.Errorf("target_path %q is not a clean relative path", targetPath)
	}
	root, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		return nil
	}
	if info, statErr := os.Stat(root); statErr != nil || !info.IsDir() {
		return nil
	}
	dir := path.Dir(targetPath)
	pkgs, err := existingGoPackages(root, dir)
	if err != nil {
		return err
	}
	if len(pkgs) == 0 {
		return nil
	}
	got := file.Name.Name
	for _, p := range pkgs {
		if got == p || got == p+"_test" {
			return nil
		}
	}
	return fmt.Errorf("package %q does not fit target_path %q: the Go package in %s is %q (use %q or %q, or fix target_path)", got, targetPath, dir, pkgs[0], pkgs[0], pkgs[0]+"_test")
}

func summarizeParseError(err error) string {
	if list, ok := err.(scanner.ErrorList); ok && len(list) > 0 {
		var parts []string
		for i, e := range list {
			if i == 3 {
				parts = append(parts, fmt.Sprintf("... %d more", len(list)-3))
				break
			}
			parts = append(parts, e.Error())
		}
		return strings.Join(parts, "; ")
	}
	return err.Error()
}

// existingGoPackages returns the Go package names present in root/dir (root
// already symlink-resolved): the non-test package(s) if any, else the
// package(s) the test files belong to (with any _test suffix removed). It only
// reads package clauses and never follows a symlink below root; an absent
// directory is empty.
func existingGoPackages(root, dir string) ([]string, error) {
	cur := root
	if dir != "." {
		for _, seg := range strings.Split(dir, "/") {
			cur = filepath.Join(cur, seg)
			info, err := os.Lstat(cur)
			if os.IsNotExist(err) {
				return nil, nil
			}
			if err != nil {
				return nil, fmt.Errorf("cannot verify package: stat %s: %v", dir, err)
			}
			if info.Mode()&os.ModeSymlink != 0 {
				return nil, fmt.Errorf("target directory %s passes through a symlink", dir)
			}
			if !info.IsDir() {
				return nil, fmt.Errorf("target directory %s is not a directory", dir)
			}
		}
	}
	d, err := os.Open(cur)
	if err != nil {
		return nil, fmt.Errorf("cannot verify package: read %s: %v", dir, err)
	}
	defer d.Close()
	entries, err := d.ReadDir(goPackageDirMaxEntries + 1)
	if err != nil && err != io.EOF {
		return nil, fmt.Errorf("cannot verify package: read %s: %v", dir, err)
	}
	if len(entries) > goPackageDirMaxEntries {
		return nil, fmt.Errorf("cannot verify package: directory %s holds more than %d entries", dir, goPackageDirMaxEntries)
	}
	nonTest, test := map[string]bool{}, map[string]bool{}
	for _, e := range entries {
		name := e.Name()
		if !e.Type().IsRegular() || !strings.HasSuffix(name, ".go") || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") || !fileNameMatchesPlatform(name) {
			continue
		}
		pkg, selected, err := readPackageClause(filepath.Join(cur, name))
		if err != nil {
			return nil, fmt.Errorf("cannot verify package: %s/%s: %v", dir, name, err)
		}
		if !selected {
			continue
		}
		if strings.HasSuffix(name, "_test.go") {
			test[strings.TrimSuffix(pkg, "_test")] = true
		} else {
			nonTest[pkg] = true
		}
	}
	set := nonTest
	if len(set) == 0 {
		set = test
	}
	out := make([]string, 0, len(set))
	for p := range set {
		out = append(out, p)
	}
	sort.Strings(out)
	return out, nil
}

// readPackageClause returns the package name of an existing repo file and
// whether the build would select it under any linux sandbox arch/cgo combination. An error means the package could not be established (a package clause
// beyond the bounded header read is unverifiable, never skipped); a file that
// is unreadable or does not parse within the whole file is skipped as broken.
func readPackageClause(p string) (name string, selected bool, err error) {
	f, err := os.Open(p)
	if err != nil {
		return "", false, nil
	}
	defer f.Close()
	head, err := io.ReadAll(io.LimitReader(f, goPackageHeaderBytes))
	if err != nil {
		return "", false, nil
	}
	fset := token.NewFileSet()
	file, perr := parser.ParseFile(fset, filepath.Base(p), head, parser.PackageClauseOnly|parser.ParseComments)
	if perr != nil || file.Name == nil {
		if len(head) >= goPackageHeaderBytes {
			return "", false, fmt.Errorf("package clause not found within the first %d bytes", goPackageHeaderBytes)
		}
		return "", false, nil
	}
	off := fset.Position(file.Package).Offset
	if off < 0 || off > len(head) {
		off = 0
	}
	return file.Name.Name, constraintsSatisfied(head[:off]), nil
}

var (
	knownOS   = map[string]bool{"aix": true, "android": true, "darwin": true, "dragonfly": true, "freebsd": true, "hurd": true, "illumos": true, "ios": true, "js": true, "linux": true, "nacl": true, "netbsd": true, "openbsd": true, "plan9": true, "solaris": true, "wasip1": true, "windows": true, "zos": true}
	knownArch = map[string]bool{"386": true, "amd64": true, "amd64p32": true, "arm": true, "armbe": true, "arm64": true, "arm64be": true, "loong64": true, "mips": true, "mipsle": true, "mips64": true, "mips64le": true, "mips64p32": true, "mips64p32le": true, "ppc": true, "ppc64": true, "ppc64le": true, "riscv": true, "riscv64": true, "s390": true, "s390x": true, "sparc": true, "sparc64": true, "wasm": true}

	// sandboxArches are the architectures the sandbox worker image may run on;
	// the host cannot know which, so a file counts as selected when ANY of them
	// (and either cgo setting) selects it. Package names of one directory do
	// not vary by platform, so over-inclusion only widens what is accepted.
	sandboxArches = []string{"amd64", "arm64"}
)

// buildTagSatisfiedFor is the tag set of a sandbox worker: linux, unix, gc,
// the given arch, cgo when enabled, and every go1.N release tag.
func buildTagSatisfiedFor(arch string, cgo bool) func(string) bool {
	return func(tag string) bool {
		switch tag {
		case "linux", "unix", "gc", arch:
			return true
		case "cgo":
			return cgo
		}
		return strings.HasPrefix(tag, "go1.") && len(tag) > 4 && strings.Trim(tag[4:], "0123456789") == ""
	}
}

// fileNameMatchesPlatform applies the GOOS/GOARCH file-name suffix rule
// (name_GOOS.go, name_GOARCH.go, name_GOOS_GOARCH.go, each optionally before
// _test) for linux on any sandbox arch.
func fileNameMatchesPlatform(name string) bool {
	base := strings.TrimSuffix(strings.TrimSuffix(name, ".go"), "_test")
	parts := strings.Split(base, "_")
	if len(parts) < 2 {
		return true
	}
	last := parts[len(parts)-1]
	if knownArch[last] {
		if !slices.Contains(sandboxArches, last) {
			return false
		}
		if len(parts) >= 3 && knownOS[parts[len(parts)-2]] {
			return parts[len(parts)-2] == "linux"
		}
		return true
	}
	if knownOS[last] {
		return last == "linux"
	}
	return true
}

// constraintsSatisfied evaluates the //go:build line, or else the // +build
// lines, of a file header under every sandbox arch and cgo setting, true when
// any selects the file. A malformed constraint counts as unsatisfied (the go
// tool would refuse such a file).
func constraintsSatisfied(header []byte) bool {
	var goBuild constraint.Expr
	var plus []constraint.Expr
	for _, raw := range bytes.Split(header, []byte("\n")) {
		line := string(bytes.TrimSpace(raw))
		if constraint.IsGoBuild(line) {
			x, err := constraint.Parse(line)
			if err != nil {
				return false
			}
			goBuild = x
			break
		}
		if constraint.IsPlusBuild(line) {
			x, err := constraint.Parse(line)
			if err != nil {
				return false
			}
			plus = append(plus, x)
		}
	}
	for _, arch := range sandboxArches {
		for _, cgo := range []bool{true, false} {
			ok := buildTagSatisfiedFor(arch, cgo)
			if goBuild != nil {
				if goBuild.Eval(ok) {
					return true
				}
				continue
			}
			all := true
			for _, x := range plus {
				all = all && x.Eval(ok)
			}
			if all {
				return true
			}
		}
	}
	return false
}
