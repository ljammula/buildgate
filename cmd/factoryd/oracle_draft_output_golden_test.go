package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"buildgate/internal/request"
)

// draftOutputCase is one drafting output directory: its manifest text (none
// when manifest is "-"), its files, and the directories and symlinks beside
// them.
type draftOutputCase struct {
	name     string
	criteria []string
	manifest string
	files    map[string]string
	dirs     []string
	symlinks map[string]string
}

// draftEntry is one manifest entry in JSON; fields is the text after the
// criterion and its index.
func draftEntry(index int, criterion, fields string) string {
	entry := fmt.Sprintf(`{"criterion": %q, "criterion_index": %d`, criterion, index)
	if fields != "" {
		entry += ", " + fields
	}
	return entry + "}"
}

func draftOutputCases() []draftOutputCase {
	const first, second, third = "1. The first thing.", "2. The second thing.", "3. The third thing."
	two := []string{first, second}
	three := []string{first, second, third}
	pair := func(a, b string) string {
		return "[" + draftEntry(1, first, a) + ", " + draftEntry(2, second, b) + "]"
	}
	triple := func(a, b, c string) string {
		return "[" + draftEntry(1, first, a) + ", " + draftEntry(2, second, b) + ", " + draftEntry(3, third, c) + "]"
	}
	ab := map[string]string{"a_test.go": "package a\n", "b_test.go": "package b\n"}
	big := strings.Repeat("x", maxDraftedOracleFileBytes+1)
	nearCap := strings.Repeat("y", maxDraftedOracleFileBytes)
	fileA := `"oracle_file": "a_test.go"`
	fileB := `"oracle_file": "b_test.go"`
	return []draftOutputCase{
		{name: "two files with targets", criteria: two, files: ab,
			manifest: pair(fileA+`, "target_path": "pkg/a_test.go", "rationale": "covers one"`, fileB+`, "target_path": "pkg/b_test.go"`)},
		{name: "dotfile in the directory", criteria: two, files: map[string]string{"a_test.go": "a", "b_test.go": "b", ".hidden": "h"},
			manifest: pair(fileA, fileB)},
		{name: "directory in the directory", criteria: two, files: ab, dirs: []string{"sub"}, manifest: pair(fileA, fileB)},
		{name: "stray file not in the manifest", criteria: two, files: map[string]string{"a_test.go": "a", "b_test.go": "b", "c_test.go": "c"},
			manifest: pair(fileA, fileB)},
		{name: "no manifest", criteria: two, files: ab, manifest: "-"},
		{name: "malformed manifest", criteria: two, files: ab, manifest: `{"not": "a list"}`},
		{name: "criteria equal after normalisation", criteria: []string{"1. Same thing.", "2. Same thing"}, files: ab,
			manifest: "[" + draftEntry(1, "1. Same thing.", fileA) + ", " + draftEntry(2, "2. Same thing", fileB) + "]"},
		{name: "one entry for two criteria", criteria: two, files: ab, manifest: "[" + draftEntry(1, first, fileA) + "]"},
		{name: "entries in reverse order", criteria: two, files: ab,
			manifest: "[" + draftEntry(2, second, fileB) + ", " + draftEntry(1, first, fileA) + "]"},
		{name: "criterion text differs", criteria: two, files: ab,
			manifest: "[" + draftEntry(1, first, fileA) + ", " + draftEntry(2, "2. Something else.", fileB) + "]"},
		{name: "wrong criterion index", criteria: two, files: ab,
			manifest: "[" + draftEntry(1, first, fileA) + ", " + draftEntry(7, second, fileB) + "]"},
		{name: "wrong index then different criterion", criteria: two, files: ab,
			manifest: "[" + draftEntry(5, first, fileA) + ", " + draftEntry(2, "2. Something else.", fileB) + "]"},
		{name: "non-string rationale", criteria: two, files: ab, manifest: pair(fileA+`, "rationale": 3`, fileB)},
		{name: "non-string target path", criteria: two, files: ab, manifest: pair(fileA+`, "target_path": 3`, fileB)},
		{name: "non-string rationale and target path", criteria: two, files: ab, manifest: pair(fileA+`, "rationale": 3, "target_path": 3`, fileB)},
		{name: "non-string target path and oracle file", criteria: two, files: ab, manifest: pair(`"oracle_file": 3, "target_path": 3`, fileB)},
		{name: "unsafe target path", criteria: two, files: ab, manifest: pair(fileA+`, "target_path": "../a_test.go"`, fileB)},
		{name: "no oracle file but a target", criteria: two, files: ab,
			manifest: pair(`"oracle_file": null, "target_path": "pkg/a_test.go", "rationale": "nothing to test"`, fileB)},
		{name: "run command as oracle file", criteria: two, files: map[string]string{"RUN_COMMAND.txt": "go test ./...\n", "b_test.go": "b"},
			manifest: pair(`"oracle_file": "RUN_COMMAND.txt", "target_path": "pkg/x"`, fileB)},
		{name: "manifest as oracle file", criteria: two, files: ab, manifest: pair(`"oracle_file": "MANIFEST.json"`, fileB)},
		{name: "case alias of the run command", criteria: two, files: ab, manifest: pair(`"oracle_file": "run_command.TXT"`, fileB)},
		{name: "name with a path separator", criteria: two, files: ab, manifest: pair(`"oracle_file": "sub/a_test.go"`, fileB)},
		{name: "non-string oracle file", criteria: two, files: ab, manifest: pair(`"oracle_file": 3`, fileB)},
		{name: "one file for two criteria", criteria: two, files: ab,
			manifest: pair(fileA+`, "target_path": "pkg/a_test.go"`, fileA+`, "target_path": "pkg/a_test.go"`)},
		{name: "one file, second entry without target", criteria: two, files: ab, manifest: pair(fileA+`, "target_path": "pkg/a_test.go"`, fileA)},
		{name: "one file, first entry without target", criteria: two, files: ab, manifest: pair(fileA, fileA+`, "target_path": "pkg/a_test.go"`)},
		{name: "one file with two targets", criteria: two, files: ab,
			manifest: pair(fileA+`, "target_path": "pkg/a_test.go"`, fileA+`, "target_path": "pkg/other_test.go"`)},
		{name: "names differing only in case", criteria: two, files: ab, manifest: pair(fileA, `"oracle_file": "A_test.go"`)},
		{name: "two files with one target", criteria: two, files: ab,
			manifest: pair(fileA+`, "target_path": "pkg/a_test.go"`, fileB+`, "target_path": "pkg/a_test.go"`)},
		{name: "repeated file takes another file's target", criteria: three, files: ab,
			manifest: triple(fileA, fileB+`, "target_path": "pkg/b_test.go"`, fileA+`, "target_path": "pkg/b_test.go"`)},
		{name: "named file missing", criteria: two, files: map[string]string{"b_test.go": "b"}, manifest: pair(fileA+`, "target_path": "pkg/a_test.go"`, fileB)},
		{name: "named file missing twice", criteria: two, files: map[string]string{"b_test.go": "b"}, manifest: pair(fileA, fileA)},
		{name: "named file is a symlink", criteria: two, files: map[string]string{"b_test.go": "b"}, symlinks: map[string]string{"a_test.go": "b_test.go"},
			manifest: pair(fileA, fileB)},
		{name: "file over the size cap", criteria: two, files: map[string]string{"a_test.go": big, "b_test.go": "b"}, manifest: pair(fileA, fileB)},
		{name: "three large files and two strays", criteria: three,
			files:    map[string]string{"a_test.go": nearCap, "b_test.go": nearCap, "c_test.go": nearCap, "d_test.go": nearCap, "e_test.go": nearCap},
			manifest: "[" + draftEntry(1, first, fileA) + ", " + draftEntry(2, second, fileB) + ", " + draftEntry(3, third, `"oracle_file": "c_test.go"`) + "]"},
		{name: "missing file then non-string rationale", criteria: two, files: map[string]string{"b_test.go": "b"}, manifest: pair(fileA, fileB+`, "rationale": 3`)},
		{name: "bad name then missing file", criteria: two, files: map[string]string{}, manifest: pair(`"oracle_file": "sub/a_test.go"`, fileB)},
		{name: "missing file then bad name", criteria: two, files: map[string]string{}, manifest: pair(fileA, `"oracle_file": "sub/b_test.go"`)},
	}
}

// draftOutputTotalCapCase fills the total cap exactly, then goes over it.
func draftOutputTotalCapCase() draftOutputCase {
	chunk := strings.Repeat("z", maxDraftedOracleFileBytes)
	var criteria []string
	var entries []string
	files := map[string]string{}
	for i := 1; i <= maxDraftedOracleTotalBytes/maxDraftedOracleFileBytes+1; i++ {
		criterion := fmt.Sprintf("%d. Thing number %d.", i, i)
		name := fmt.Sprintf("f%d_test.go", i)
		criteria = append(criteria, criterion)
		entries = append(entries, draftEntry(i, criterion, fmt.Sprintf(`"oracle_file": %q`, name)))
		files[name] = chunk
	}
	return draftOutputCase{name: "one file past the total cap", criteria: criteria, files: files, manifest: "[" + strings.Join(entries, ", ") + "]"}
}

// draftOutputFileCapCase names one file more than the file cap.
func draftOutputFileCapCase() draftOutputCase {
	var criteria []string
	var entries []string
	files := map[string]string{}
	for i := 1; i <= request.MaxTicketOracleFiles+1; i++ {
		criterion := fmt.Sprintf("%d. Thing number %d.", i, i)
		name := fmt.Sprintf("f%d_test.go", i)
		criteria = append(criteria, criterion)
		entries = append(entries, draftEntry(i, criterion, fmt.Sprintf(`"oracle_file": %q`, name)))
		files[name] = "package f\n"
	}
	return draftOutputCase{name: "one file past the file cap", criteria: criteria, files: files, manifest: "[" + strings.Join(entries, ", ") + "]"}
}

func writeDraftOutputCase(t *testing.T, c draftOutputCase) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range c.files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range c.dirs {
		if err := os.Mkdir(filepath.Join(dir, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for name, target := range c.symlinks {
		if err := os.Symlink(target, filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	if c.manifest != "-" {
		if err := os.WriteFile(filepath.Join(dir, oracleManifestName), []byte(c.manifest), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// TestReadDraftOutputMatchesGolden pins everything readDraftOutputMode
// returns, in both modes, for output directories that break its rules one
// and two at a time: the error, or the installed files, the rebuilt manifest,
// the targets and whether a run command was ignored. Regenerate with
// FACTORYD_UPDATE_GOLDEN=1 only for a deliberate change to those rules.
func TestReadDraftOutputMatchesGolden(t *testing.T) {
	var got strings.Builder
	for _, c := range append(draftOutputCases(), draftOutputTotalCapCase(), draftOutputFileCapCase()) {
		for _, partial := range []bool{false, true} {
			dir := writeDraftOutputCase(t, c)
			files, manifest, targets, ignoredRunCommand, err := readDraftOutputMode(dir, c.criteria, partial)
			fmt.Fprintf(&got, "== %s (partial=%v)\n", c.name, partial)
			if err != nil {
				fmt.Fprintf(&got, "error: %s\n", strings.ReplaceAll(err.Error(), dir, "<dir>"))
			}
			names := make([]string, 0, len(files))
			for name := range files {
				names = append(names, name)
			}
			sort.Strings(names)
			for _, name := range names {
				fmt.Fprintf(&got, "file: %s (%d bytes)\n", name, len(files[name]))
			}
			names = names[:0]
			for name := range targets {
				names = append(names, name)
			}
			sort.Strings(names)
			for _, name := range names {
				fmt.Fprintf(&got, "target: %s -> %s\n", name, targets[name])
			}
			fmt.Fprintf(&got, "ignored run command: %v\n", ignoredRunCommand)
			if manifest != nil {
				fmt.Fprintf(&got, "manifest:\n%s", manifest)
			}
			got.WriteString("\n")
		}
	}
	goldenPath := filepath.Join("testdata", "read_draft_output.golden.txt")
	if os.Getenv(updateGoldenEnv) == "1" {
		if err := os.WriteFile(goldenPath, []byte(got.String()), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatal(err)
	}
	if got.String() != string(want) {
		t.Fatalf("readDraftOutputMode differs from %s\n--- got ---\n%s", goldenPath, got.String())
	}
}
