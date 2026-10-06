package oraclecanary

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeRepoFile(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func oracleSrc(pkg string) []byte {
	return []byte("package " + pkg + "\n\nimport \"testing\"\n\nfunc TestOracleX(t *testing.T) {}\n")
}

func TestCheckGoOraclePackageClause(t *testing.T) {
	root := t.TempDir()
	writeRepoFile(t, root, "backend/internal/summary/summary.go", "package summary\n")
	writeRepoFile(t, root, "backend/internal/summary/summary_test.go", "package summary_test\n")
	writeRepoFile(t, root, "backend/internal/summary/ignored.go", "//go:build ignore\n\npackage main\n")
	writeRepoFile(t, root, "backend/internal/summary/legacy.go", "// +build ignore\n\npackage main\n")
	writeRepoFile(t, root, "bigheader/a.go", "/*"+strings.Repeat("license ", 20000)+"*/\npackage bigheader\n")
	writeRepoFile(t, root, "winonly/a_windows.go", "package winpkg\n")
	writeRepoFile(t, root, "winonly/b_windows_amd64.go", "package winpkg\n")
	writeRepoFile(t, root, "linuxpkg/a_linux.go", "package linuxpkg\n")
	writeRepoFile(t, root, "linuxpkg/b_darwin.go", "package darwinpkg\n")
	writeRepoFile(t, root, "linuxpkg/c.go", "//go:build !windows && (linux || darwin)\n\npackage linuxpkg\n")
	writeRepoFile(t, root, "linuxpkg/d.go", "//go:build windows\n\npackage winpkg\n")
	writeRepoFile(t, root, "linuxpkg/e.go", "// +build darwin\n\npackage darwinpkg\n")
	writeRepoFile(t, root, "constrained/a.go", "//go:build !linux\n\npackage nonlinux\n")
	writeRepoFile(t, root, "arches/a_arm64.go", "package arches\n")
	writeRepoFile(t, root, "arches/b_amd64.go", "package arches\n")
	writeRepoFile(t, root, "arches/c_s390x.go", "package s390pkg\n")
	writeRepoFile(t, root, "arches/d.go", "//go:build amd64 && !arm64\n\npackage arches\n")
	writeRepoFile(t, root, "arches/e.go", "//go:build cgo\n\npackage arches\n")
	writeRepoFile(t, root, "arches/f.go", "//go:build go1.21 && !windows\n\npackage arches\n")
	writeRepoFile(t, root, "arches/g.go", "//go:build riscv64\n\npackage riscvpkg\n")
	writeRepoFile(t, root, "cgoonly/a.go", "//go:build cgo\n\npackage cgopkg\n")
	writeRepoFile(t, root, "armonly/a_arm64.go", "package armpkg\n")
	writeRepoFile(t, root, "amdonly/a.go", "//go:build amd64 && !arm64\n\npackage amdpkg\n")
	writeRepoFile(t, root, "onlytests/a_test.go", "package onlytests_test\n")
	writeRepoFile(t, root, "hyphen-dir/.keep", "")

	cases := []struct {
		name, target, pkg string
		wantErr           string
	}{
		{"matches existing package", "backend/internal/summary/x_test.go", "summary", ""},
		{"external test package", "backend/internal/summary/x_test.go", "summary_test", ""},
		{"wrong package under a real dir", "backend/internal/summary/x_test.go", "store", `the Go package in backend/internal/summary is "summary"`},
		{"new dir may use any package name", "store/x_test.go", "summary", ""},
		{"new dir with base name", "backend/internal/newpkg/x_test.go", "newpkg", ""},
		{"new dir with base_test", "backend/internal/newpkg/x_test.go", "newpkg_test", ""},
		{"new dir hyphen sanitised", "hyphen-dir/x_test.go", "hyphen_dir", ""},
		{"dir with only tests", "onlytests/x_test.go", "onlytests", ""},
		{"dir with only tests wrong", "onlytests/x_test.go", "other", `is "onlytests"`},
		{"repo root without go files is unchecked", "x_test.go", "anything", ""},
		{"huge header still yields the package", "bigheader/x_test.go", "bigheader", ""},
		{"huge header package mismatch is caught", "bigheader/x_test.go", "other", `is "bigheader"`},
		{"windows-only file does not define the package", "winonly/x_test.go", "other", ""},
		{"linux file defines the package", "linuxpkg/x_test.go", "linuxpkg", ""},
		{"linux file wrong package", "linuxpkg/x_test.go", "other", `is "linuxpkg"`},
		{"tag-constrained file that excludes linux", "constrained/x_test.go", "constrained", ""},
		{"arch-suffixed and constrained files define the package", "arches/x_test.go", "arches", ""},
		{"s390x, riscv64-only files do not", "arches/x_test.go", "s390pkg", `is "arches"`},
		{"riscv64-only constraint excluded", "arches/x_test.go", "riscvpkg", `is "arches"`},
		{"cgo-only file counts", "cgoonly/x_test.go", "other", `is "cgopkg"`},
		{"arm64-suffixed file counts", "armonly/x_test.go", "other", `is "armpkg"`},
		{"amd64 && !arm64 file counts", "amdonly/x_test.go", "other", `is "amdpkg"`},
		{"legacy +build ignore file", "backend/internal/summary/x_test.go", "main", "does not fit"},
		{"go:build ignore file is not the package", "backend/internal/summary/x_test.go", "main", "does not fit"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckGoOracle(root, tc.target, oracleSrc(tc.pkg))
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Fatalf("error = %v, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

func TestCheckGoOracleSyntaxErrorsAreRefusedWithoutATarget(t *testing.T) {
	err := CheckGoOracle(t.TempDir(), "", []byte("package p\n\nfunc TestOracleX(t *testing.T) {\n"))
	if err == nil || !strings.Contains(err.Error(), "does not parse") {
		t.Fatalf("err = %v, want a parse failure", err)
	}
	if err := CheckGoOracle(t.TempDir(), "", []byte("this is not go")); err == nil {
		t.Fatal("garbage accepted")
	}
	if err := CheckGoOracle(t.TempDir(), "", oracleSrc("p")); err != nil {
		t.Fatalf("valid source refused: %v", err)
	}
}

func TestCheckGoOracleParseErrorReportIsBounded(t *testing.T) {
	src := []byte("package p\n" + strings.Repeat("func (\n", 500))
	err := CheckGoOracle(t.TempDir(), "", src)
	if err == nil {
		t.Fatal("accepted")
	}
	if len(err.Error()) > 1000 {
		t.Errorf("error is %d bytes; want a bounded summary", len(err.Error()))
	}
}

func TestCheckGoOracleHostileInputs(t *testing.T) {
	root := t.TempDir()
	writeRepoFile(t, root, "pkg/p.go", "package pkg\n")

	// Hostile package clauses never reach anything but the parser.
	for _, src := range []string{
		"package \x00evil\n",
		"package pkg; import \"os\"; func init() { os.Exit(1) }\n// evil",
		"package pkg /* \n",
		"package 1bad\n",
		"package\n",
		"",
	} {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("panicked on %q: %v", src, r)
				}
			}()
			_ = CheckGoOracle(root, "pkg/x_test.go", []byte(src))
		}()
	}
	if err := CheckGoOracle(root, "pkg/x_test.go", []byte("package \x00evil\n")); err == nil {
		t.Error("NUL package clause accepted")
	}

	// Oversize source is refused rather than parsed.
	huge := append(oracleSrc("pkg"), []byte("// "+strings.Repeat("a", maxGoOracleCheckBytes))...)
	if err := CheckGoOracle(root, "pkg/x_test.go", huge); err == nil || !strings.Contains(err.Error(), "too large") {
		t.Errorf("huge source: err = %v", err)
	}

	// Unclean / escaping targets.
	for _, target := range []string{"../x_test.go", "/etc/x_test.go", "a/../b/x_test.go", "a//x_test.go", "./x_test.go"} {
		if err := CheckGoOracle(root, target, oracleSrc("pkg")); err == nil {
			t.Errorf("target %q accepted", target)
		}
	}
}

func TestCheckGoOracleSymlinksAreNeverFollowed(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	writeRepoFile(t, outside, "p.go", "package leaked\n")

	// A symlinked package directory is refused, not read.
	if err := os.Symlink(outside, filepath.Join(root, "linked")); err != nil {
		t.Fatal(err)
	}
	if err := CheckGoOracle(root, "linked/x_test.go", oracleSrc("leaked")); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Errorf("symlinked dir: err = %v, want a symlink refusal", err)
	}

	// A symlinked .go file inside a real directory does not define the package.
	writeRepoFile(t, root, "real/q.go", "package real\n")
	if err := os.Symlink(filepath.Join(outside, "p.go"), filepath.Join(root, "real", "p.go")); err != nil {
		t.Fatal(err)
	}
	if err := CheckGoOracle(root, "real/x_test.go", oracleSrc("leaked")); err == nil {
		t.Error("package taken from a symlinked file")
	}
	if err := CheckGoOracle(root, "real/x_test.go", oracleSrc("real")); err != nil {
		t.Errorf("real dir base name refused: %v", err)
	}
}

func TestCheckGoOracleSkipsPackageCheckWhenWorkspaceIsMissing(t *testing.T) {
	if err := CheckGoOracle(filepath.Join(t.TempDir(), "gone"), "a/x_test.go", oracleSrc("whatever")); err != nil {
		t.Errorf("err = %v, want the package half skipped", err)
	}
	if err := CheckGoOracle(filepath.Join(t.TempDir(), "gone"), "a/x_test.go", []byte("nope")); err == nil {
		t.Error("syntax half must still run")
	}
}

func TestCheckGoOracleFailsClosedWhenPackageCannotBeEstablished(t *testing.T) {
	root := t.TempDir()
	// A package clause beyond the bounded header read.
	writeRepoFile(t, root, "deep/a.go", "/*"+strings.Repeat("x", goPackageHeaderBytes+10)+"*/\npackage deep\n")
	if err := CheckGoOracle(root, "deep/x_test.go", oracleSrc("other")); err == nil || !strings.Contains(err.Error(), "cannot verify package") {
		t.Errorf("err = %v, want a fail-closed refusal", err)
	}
	// A directory scan that hits the entry cap.
	for i := 0; i <= goPackageDirMaxEntries; i++ {
		writeRepoFile(t, root, fmt.Sprintf("many/f%d.txt", i), "")
	}
	if err := CheckGoOracle(root, "many/x_test.go", oracleSrc("other")); err == nil || !strings.Contains(err.Error(), "directory") {
		t.Errorf("err = %v, want a directory-too-large refusal", err)
	}
}

func TestCheckGoOracleSymlinkedWorkspaceRootIsFine(t *testing.T) {
	real := t.TempDir()
	writeRepoFile(t, real, "pkg/p.go", "package pkg\n")
	link := filepath.Join(t.TempDir(), "ws")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if err := CheckGoOracle(link, "pkg/x_test.go", oracleSrc("pkg")); err != nil {
		t.Errorf("matching package through a symlinked root refused: %v", err)
	}
	if err := CheckGoOracle(link, "pkg/x_test.go", oracleSrc("other")); err == nil {
		t.Error("mismatch through a symlinked root not caught (root resolved wrongly, check skipped)")
	}
}
