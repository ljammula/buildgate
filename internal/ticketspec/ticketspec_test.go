package ticketspec

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseVerifyCommandFound(t *testing.T) {
	path := writeSpec(t, "# Ticket: foo\n\n## Acceptance\n\nVerify-Command: make verify\n\nSome prose.\n")

	got, err := ParseVerifyCommand(path)
	if err != nil {
		t.Fatalf("ParseVerifyCommand: %v", err)
	}
	if got != "make verify" {
		t.Errorf("got %q, want %q", got, "make verify")
	}
}

func TestParseVerifyCommandAbsent(t *testing.T) {
	path := writeSpec(t, "# Ticket: foo\n\nNo machine-readable key here.\n")

	got, err := ParseVerifyCommand(path)
	if err != nil {
		t.Fatalf("ParseVerifyCommand: %v", err)
	}
	if got != "" {
		t.Errorf("got %q, want empty string when ticket declares no verify command", got)
	}
}

func TestParseVerifyCommandEmptyValueErrors(t *testing.T) {
	path := writeSpec(t, "Verify-Command: \n")

	if _, err := ParseVerifyCommand(path); err == nil {
		t.Error("expected an error for a Verify-Command line with no command")
	}
}

func TestParseAllowedFilesFound(t *testing.T) {
	path := writeSpec(t, "# Ticket: foo\n\n## Out of scope\n\nAllowed-Files: a/b.go, a/b_test.go\n")

	got, err := ParseAllowedFiles(path)
	if err != nil {
		t.Fatalf("ParseAllowedFiles: %v", err)
	}
	want := []string{"a/b.go", "a/b_test.go"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestParseAllowedFilesAbsent(t *testing.T) {
	path := writeSpec(t, "# Ticket: foo\n\nNo machine-readable key here.\n")

	got, err := ParseAllowedFiles(path)
	if err != nil {
		t.Fatalf("ParseAllowedFiles: %v", err)
	}
	if got != nil {
		t.Errorf("got %v, want nil when ticket declares no allowed-files list", got)
	}
}

func TestParseAllowedFilesEmptyValueErrors(t *testing.T) {
	path := writeSpec(t, "Allowed-Files: \n")

	if _, err := ParseAllowedFiles(path); err == nil {
		t.Error("expected an error for an Allowed-Files line with no files")
	}
}

// TestParseAllowedFilesRejectsAbsolutePath is the regression test for a
// real run (budget-remaining-amount-20260828-181352-24969, see the plan's
// 2026-08-28 Opus review) that quarantined on every declared-scope gate
// at once because its Allowed-Files declared absolute host paths, which
// internal/policy's diffScopeViolations can never match against
// workspace-relative `git diff --name-only` output. This must now fail
// closed at parse time, before the agent ever runs, instead of silently
// producing an unmatchable scope declaration.
func TestParseAllowedFilesRejectsAbsolutePath(t *testing.T) {
	path := writeSpec(t, "Allowed-Files: /Users/dev/code/repo/a.go\n")

	if _, err := ParseAllowedFiles(path); err == nil {
		t.Error("expected an error for an absolute Allowed-Files path")
	}
}

// TestParseAllowedFilesRejectsTraversalPath pins the same fail-closed
// preflight check for a path that escapes the workspace checkout root.
func TestParseAllowedFilesRejectsTraversalPath(t *testing.T) {
	path := writeSpec(t, "Allowed-Files: ../spec/contract.md\n")

	if _, err := ParseAllowedFiles(path); err == nil {
		t.Error("expected an error for an Allowed-Files path escaping the checkout root")
	}
}

// TestParseAllowedFilesRejectsInternalTraversalSegment is the regression
// test for a real finding from codex review (round 3, 2026-08-28): the
// first version of this check ran on path.Clean(p), so "foo/../bar"
// cleaned to "bar" and slipped past the ".." check even though the raw
// declared string contains one. Since factoryd stores and matches the
// *raw* string (not the cleaned one) against git's canonical paths,
// "foo/../bar" could never match anything real ("bar" as reported by git
// diff) -- the exact class of unmatchable declaration this preflight
// exists to reject, just with an internal ".." instead of a leading one.
func TestParseAllowedFilesRejectsInternalTraversalSegment(t *testing.T) {
	path := writeSpec(t, "Allowed-Files: foo/../bar.go\n")

	if _, err := ParseAllowedFiles(path); err == nil {
		t.Error("expected an error for an Allowed-Files path with an internal .. segment")
	}
}

// TestParseAllowedFilesRejectsNoncanonicalSegments is the regression test
// for a real finding from a GitHub Codex App review comment (2026-08-29):
// "./a.go" or "dir//a.go" are just as unmatchable against git's canonical
// "a.go"/"dir/a.go" as an internal ".." segment is, and for the same
// reason -- factoryd compares the raw declared string, not a cleaned
// one. A single legitimate trailing "/" (the directory-prefix pattern)
// must still be accepted; see TestParseAllowedFilesAcceptsGlobAndDirectoryPatterns.
func TestParseAllowedFilesRejectsNoncanonicalSegments(t *testing.T) {
	for _, bad := range []string{"./a.go", "dir//a.go", "dir//", "."} {
		path := writeSpec(t, "Allowed-Files: "+bad+"\n")
		if _, err := ParseAllowedFiles(path); err == nil {
			t.Errorf("ParseAllowedFiles(%q): expected an error for a noncanonical path, got none", bad)
		}
	}
}

// TestParseAllowedFilesAcceptsGlobAndDirectoryPatterns confirms the
// preflight check doesn't reject the two new pattern forms
// internal/policy.matchesAllowed understands (a directory prefix ending
// in "/", and a shell glob) -- only absolute/escaping paths are rejected.
func TestParseAllowedFilesAcceptsGlobAndDirectoryPatterns(t *testing.T) {
	path := writeSpec(t, "Allowed-Files: app/lib/l10n/, app/lib/l10n/app_localizations*.dart\n")

	got, err := ParseAllowedFiles(path)
	if err != nil {
		t.Fatalf("ParseAllowedFiles: %v", err)
	}
	want := []string{"app/lib/l10n/", "app/lib/l10n/app_localizations*.dart"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestParseRequiredChangedFilesFound(t *testing.T) {
	path := writeSpec(t, "# Ticket: foo\n\n## Out of scope\n\nRequired-Changed-Files: a/b.go, a/b_test.go\n")

	got, err := ParseRequiredChangedFiles(path)
	if err != nil {
		t.Fatalf("ParseRequiredChangedFiles: %v", err)
	}
	want := []string{"a/b.go", "a/b_test.go"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestParseRequiredChangedFilesAbsent(t *testing.T) {
	path := writeSpec(t, "# Ticket: foo\n\nNo machine-readable key here.\n")

	got, err := ParseRequiredChangedFiles(path)
	if err != nil {
		t.Fatalf("ParseRequiredChangedFiles: %v", err)
	}
	if got != nil {
		t.Errorf("got %v, want nil when ticket declares no required-changed-files list", got)
	}
}

func TestParseRequiredChangedFilesEmptyValueErrors(t *testing.T) {
	path := writeSpec(t, "Required-Changed-Files: \n")

	if _, err := ParseRequiredChangedFiles(path); err == nil {
		t.Error("expected an error for a Required-Changed-Files line with no files")
	}
}

// TestParseRequiredChangedFilesRejectsAbsolutePath pins the same
// preflight validation as ParseAllowedFiles (they share
// parseFileListKeyLine) for the key that quarantined budget-remaining-amount's
// required_files_changed and required_content_present gates alongside
// diff_scope.
func TestParseRequiredChangedFilesRejectsAbsolutePath(t *testing.T) {
	path := writeSpec(t, "Required-Changed-Files: /Users/dev/code/repo/a.go\n")

	if _, err := ParseRequiredChangedFiles(path); err == nil {
		t.Error("expected an error for an absolute Required-Changed-Files path")
	}
}

func TestParseTestsRequiredOptOutAbsentMeansGateRunsNormally(t *testing.T) {
	path := writeSpec(t, "# Ticket: foo\n\nNo machine-readable key here.\n")

	got, err := ParseTestsRequiredOptOut(path)
	if err != nil {
		t.Fatalf("ParseTestsRequiredOptOut: %v", err)
	}
	if got != "" {
		t.Errorf("got %q, want \"\" when the ticket declares no Tests-Required: key", got)
	}
}

func TestParseTestsRequiredOptOutYesMeansGateRunsNormally(t *testing.T) {
	path := writeSpec(t, "Tests-Required: yes\n")

	got, err := ParseTestsRequiredOptOut(path)
	if err != nil {
		t.Fatalf("ParseTestsRequiredOptOut: %v", err)
	}
	if got != "" {
		t.Errorf("got %q, want \"\" for an explicit Tests-Required: yes", got)
	}
}

func TestParseTestsRequiredOptOutNoWithReason(t *testing.T) {
	path := writeSpec(t, "Tests-Required: no -- this ticket only updates documentation\n")

	got, err := ParseTestsRequiredOptOut(path)
	if err != nil {
		t.Fatalf("ParseTestsRequiredOptOut: %v", err)
	}
	if got != "this ticket only updates documentation" {
		t.Errorf("got %q, want the declared reason", got)
	}
}

func TestParseTestsRequiredOptOutNoWithoutReasonErrors(t *testing.T) {
	path := writeSpec(t, "Tests-Required: no\n")

	if _, err := ParseTestsRequiredOptOut(path); err == nil {
		t.Error("expected an error for Tests-Required: no with no reason")
	}
}

func TestParseTestsRequiredOptOutNoWithEmptyReasonErrors(t *testing.T) {
	path := writeSpec(t, "Tests-Required: no -- \n")

	if _, err := ParseTestsRequiredOptOut(path); err == nil {
		t.Error("expected an error for Tests-Required: no -- with a blank reason")
	}
}

func TestParseTestsRequiredOptOutInvalidValueErrors(t *testing.T) {
	path := writeSpec(t, "Tests-Required: maybe\n")

	if _, err := ParseTestsRequiredOptOut(path); err == nil {
		t.Error("expected an error for a Tests-Required: value that is neither yes nor no -- <reason>")
	}
}

func TestParseRequiredContentCollectsMultipleLines(t *testing.T) {
	path := writeSpec(t, "# Ticket: foo\n\nRequired-Content: Key('reminder-quick-time-chips')\nRequired-Content: func TestQuickTimeChips(\n")

	got, err := ParseRequiredContent(path)
	if err != nil {
		t.Fatalf("ParseRequiredContent: %v", err)
	}
	want := []string{"Key('reminder-quick-time-chips')", "func TestQuickTimeChips("}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestParseRequiredContentAbsent(t *testing.T) {
	path := writeSpec(t, "# Ticket: foo\n\nNo machine-readable key here.\n")

	got, err := ParseRequiredContent(path)
	if err != nil {
		t.Fatalf("ParseRequiredContent: %v", err)
	}
	if got != nil {
		t.Errorf("got %v, want nil when ticket declares no required content", got)
	}
}

func TestParseRequiredContentEmptyValueErrors(t *testing.T) {
	path := writeSpec(t, "Required-Content: \n")

	if _, err := ParseRequiredContent(path); err == nil {
		t.Error("expected an error for a Required-Content line with no content")
	}
}

func TestParseRequiredContentIgnoresFencedExample(t *testing.T) {
	path := writeSpec(t, "# Ticket: foo\n\nDon't write:\n\n```\nRequired-Content: not-a-real-requirement\n```\n\nRequired-Content: real-requirement\n")

	got, err := ParseRequiredContent(path)
	if err != nil {
		t.Fatalf("ParseRequiredContent: %v", err)
	}
	want := []string{"real-requirement"}
	if len(got) != len(want) || got[0] != want[0] {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestParseVerifyCommandIgnoresFencedExample(t *testing.T) {
	path := writeSpec(t, "# Ticket: foo\n\nDon't write:\n\n```\nVerify-Command: rm -rf /\n```\n\nWrite a real one below.\n")

	got, err := ParseVerifyCommand(path)
	if err != nil {
		t.Fatalf("ParseVerifyCommand: %v", err)
	}
	if got != "" {
		t.Errorf("got %q, want empty string — a fenced example must not be parsed as real metadata", got)
	}
}

func TestParseVerifyCommandIgnoresIndentedExample(t *testing.T) {
	path := writeSpec(t, "# Ticket: foo\n\n- e.g.\n  Verify-Command: rm -rf /\n")

	got, err := ParseVerifyCommand(path)
	if err != nil {
		t.Fatalf("ParseVerifyCommand: %v", err)
	}
	if got != "" {
		t.Errorf("got %q, want empty string — an indented illustration must not be parsed as real metadata", got)
	}
}

func TestParseVerifyCommandFindsRealKeyAfterFencedExample(t *testing.T) {
	path := writeSpec(t, "# Ticket: foo\n\n```\nVerify-Command: not this one\n```\n\nVerify-Command: make verify\n")

	got, err := ParseVerifyCommand(path)
	if err != nil {
		t.Fatalf("ParseVerifyCommand: %v", err)
	}
	if got != "make verify" {
		t.Errorf("got %q, want %q — the real top-level key after the fence should still be found", got, "make verify")
	}
}

func TestParseVerifyCommandIgnoresTildeFencedExample(t *testing.T) {
	path := writeSpec(t, "# Ticket: foo\n\nDon't write:\n\n~~~\nVerify-Command: rm -rf /\n~~~\n")

	got, err := ParseVerifyCommand(path)
	if err != nil {
		t.Fatalf("ParseVerifyCommand: %v", err)
	}
	if got != "" {
		t.Errorf("got %q, want empty string — a tilde-fenced example must not be parsed as real metadata", got)
	}
}

func TestParseVerifyCommandIgnoresLineWithFenceMarkerPrefixButTrailingContent(t *testing.T) {
	// CommonMark requires a closing fence to contain nothing but the
	// fence character and optional whitespace — a line that merely
	// starts with the right marker but has other content after it
	// (e.g. "~~~not-a-close") is not a valid close and must not end the
	// fence early.
	path := writeSpec(t, "# Ticket: foo\n\n~~~\n~~~not-a-close\nVerify-Command: rm -rf /\n~~~\n")

	got, err := ParseVerifyCommand(path)
	if err != nil {
		t.Fatalf("ParseVerifyCommand: %v", err)
	}
	if got != "" {
		t.Errorf("got %q, want empty string — a fence-marker-prefixed line with trailing content must not close the fence", got)
	}
}

func TestParseVerifyCommandIgnoresShorterFenceInsideLongerFence(t *testing.T) {
	// A 4-backtick fence containing a 3-backtick line: the 3-backtick line
	// must not be treated as closing the fence (CommonMark requires a
	// closing fence at least as long as the opening one), so the
	// Verify-Command line that follows it must still be treated as inside
	// the fence, not real metadata.
	path := writeSpec(t, "# Ticket: foo\n\n````\n```\nVerify-Command: rm -rf /\n````\n")

	got, err := ParseVerifyCommand(path)
	if err != nil {
		t.Fatalf("ParseVerifyCommand: %v", err)
	}
	if got != "" {
		t.Errorf("got %q, want empty string — a shorter nested fence must not close the outer one", got)
	}
}

func TestParseVerifyCommandIgnoresMismatchedFenceCharacter(t *testing.T) {
	// A backtick fence containing a tilde line: the tilde line must not
	// close a backtick fence (different fence characters don't match).
	path := writeSpec(t, "# Ticket: foo\n\n```\n~~~\nVerify-Command: rm -rf /\n```\n")

	got, err := ParseVerifyCommand(path)
	if err != nil {
		t.Fatalf("ParseVerifyCommand: %v", err)
	}
	if got != "" {
		t.Errorf("got %q, want empty string — a mismatched fence character must not close the fence", got)
	}
}

func TestParseVerifyCommandMissingFile(t *testing.T) {
	if _, err := ParseVerifyCommand(filepath.Join(t.TempDir(), "does-not-exist.md")); err == nil {
		t.Error("expected an error for a missing spec file")
	}
}

func TestMisprefixedWorkspacePathsFindsAUniqueSubdirMatch(t *testing.T) {
	ws := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ws, "backend", "internal", "service"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(ws, "backend", "internal", "service", "note.go"), []byte("package service\n"), 0o644); err != nil {
		t.Fatalf("write fixture file: %v", err)
	}
	// backend/ must look like a real module root of its own -- matches
	// a Flutter + Go app repo's own real shape (backend/go.mod).
	if err := os.WriteFile(filepath.Join(ws, "backend", "go.mod"), []byte("module example.com/backend\n"), 0o644); err != nil {
		t.Fatalf("write go.mod fixture: %v", err)
	}

	got, err := MisprefixedWorkspacePaths(ws, []string{"internal/service/note.go"})
	if err != nil {
		t.Fatalf("MisprefixedWorkspacePaths: %v", err)
	}
	want := map[string]string{"internal/service/note.go": "backend/internal/service/note.go"}
	if len(got) != len(want) || got["internal/service/note.go"] != want["internal/service/note.go"] {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestMisprefixedWorkspacePathsIgnoresAPathThatExistsAsDeclared(t *testing.T) {
	ws := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ws, "internal", "service"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(ws, "internal", "service", "note.go"), []byte("package service\n"), 0o644); err != nil {
		t.Fatalf("write fixture file: %v", err)
	}
	// Also present under backend/ -- must not matter, since the declared
	// path already exists exactly where declared.
	if err := os.MkdirAll(filepath.Join(ws, "backend", "internal", "service"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(ws, "backend", "internal", "service", "note.go"), []byte("package service\n"), 0o644); err != nil {
		t.Fatalf("write fixture file: %v", err)
	}

	got, err := MisprefixedWorkspacePaths(ws, []string{"internal/service/note.go"})
	if err != nil {
		t.Fatalf("MisprefixedWorkspacePaths: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %v, want no corrections -- the declared path already exists exactly where declared", got)
	}
}

func TestMisprefixedWorkspacePathsIgnoresANewFileThatDoesNotExistAnywhere(t *testing.T) {
	// The ordinary, common case: Required-Changed-Files names a file the
	// ticket is about to create for the first time. Must not be reported
	// as a correction candidate -- it isn't missing a prefix, it's just
	// new.
	ws := t.TempDir()
	if err := os.Mkdir(filepath.Join(ws, "backend"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	got, err := MisprefixedWorkspacePaths(ws, []string{"internal/note/validation.go"})
	if err != nil {
		t.Fatalf("MisprefixedWorkspacePaths: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %v, want no corrections for a file that doesn't exist anywhere", got)
	}
}

func TestMisprefixedWorkspacePathsIgnoresAnAmbiguousMatch(t *testing.T) {
	ws := t.TempDir()
	for _, sub := range []string{"backend", "tools"} {
		dir := filepath.Join(ws, sub, "internal", "service")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, "note.go"), []byte("package service\n"), 0o644); err != nil {
			t.Fatalf("write fixture file: %v", err)
		}
		// Both must be real module roots -- otherwise this exercises the
		// module-marker filter, not the ambiguous-match one.
		if err := os.WriteFile(filepath.Join(ws, sub, "go.mod"), []byte("module example.com/"+sub+"\n"), 0o644); err != nil {
			t.Fatalf("write go.mod fixture: %v", err)
		}
	}

	got, err := MisprefixedWorkspacePaths(ws, []string{"internal/service/note.go"})
	if err != nil {
		t.Fatalf("MisprefixedWorkspacePaths: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %v, want no corrections -- matches under two subdirectories is ambiguous, not worth guessing", got)
	}
}

func TestMisprefixedWorkspacePathsIgnoresACoincidentalMatchWithNoModuleMarker(t *testing.T) {
	// Real false-positive risk found via adversarial review, 2026-09-11:
	// a same-suffix file coincidentally present under an unrelated
	// subdirectory (a vendored copy, a generated stub, or -- this very
	// repo's own shape -- another package following the same
	// internal-package-naming convention) must not be reported as a
	// correction just because it's the only match. Requiring the matched
	// subdirectory to also look like a real module root (carry its own
	// go.mod/package.json/etc.) is what distinguishes the true-positive
	// case (TestMisprefixedWorkspacePathsFindsAUniqueSubdirMatch's own
	// backend/go.mod) from this one.
	ws := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ws, "tools", "internal", "service"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(ws, "tools", "internal", "service", "note.go"), []byte("package service\n"), 0o644); err != nil {
		t.Fatalf("write fixture file: %v", err)
	}
	// Deliberately no go.mod/package.json/etc. under tools/ -- it's not a
	// module root, just a directory that happens to contain a same-suffix
	// file.

	got, err := MisprefixedWorkspacePaths(ws, []string{"internal/service/note.go"})
	if err != nil {
		t.Fatalf("MisprefixedWorkspacePaths: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %v, want no corrections -- the only match has no module-root marker, so it's a coincidence, not a misprefix", got)
	}
}

func TestMisprefixedWorkspacePathsRecognizesAMakefileOnlyModuleRoot(t *testing.T) {
	// Found via Codex review, 2026-09-11: cmd/factoryd's own
	// detectVerifyCommand already treats a subdirectory with a Makefile
	// declaring a verify: or test: target as a real project root, but
	// hasModuleRootMarker didn't recognize Makefile at all -- a ticket
	// declaring a path under such a root (no go.mod/package.json, only a
	// Makefile) would never get corrected, silently reintroducing the
	// wasted-build-round bug this whole function exists to prevent.
	ws := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ws, "backend", "internal", "service"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(ws, "backend", "internal", "service", "note.go"), []byte("package service\n"), 0o644); err != nil {
		t.Fatalf("write fixture file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(ws, "backend", "Makefile"), []byte("verify:\n\tgo test ./...\n"), 0o644); err != nil {
		t.Fatalf("write Makefile fixture: %v", err)
	}

	got, err := MisprefixedWorkspacePaths(ws, []string{"internal/service/note.go"})
	if err != nil {
		t.Fatalf("MisprefixedWorkspacePaths: %v", err)
	}
	want := map[string]string{"internal/service/note.go": "backend/internal/service/note.go"}
	if len(got) != len(want) || got["internal/service/note.go"] != want["internal/service/note.go"] {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestMisprefixedWorkspacePathsIgnoresAMakefileWithNoVerifyOrTestTarget(t *testing.T) {
	// A Makefile that declares neither verify: nor test: is not evidence
	// of a real project root -- detectVerifyCommand wouldn't trust it
	// either, so this coincidental match must not be reported.
	ws := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ws, "tools", "internal", "service"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(ws, "tools", "internal", "service", "note.go"), []byte("package service\n"), 0o644); err != nil {
		t.Fatalf("write fixture file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(ws, "tools", "Makefile"), []byte("build:\n\techo build\n"), 0o644); err != nil {
		t.Fatalf("write Makefile fixture: %v", err)
	}

	got, err := MisprefixedWorkspacePaths(ws, []string{"internal/service/note.go"})
	if err != nil {
		t.Fatalf("MisprefixedWorkspacePaths: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %v, want no corrections -- Makefile has no verify: or test: target", got)
	}
}

func TestMisprefixedWorkspacePathsIgnoresAPackageJSONWithNoTestScript(t *testing.T) {
	// Found via Codex review, 2026-09-11: an existence-only package.json
	// check let a coincidental match under an unrelated subdirectory --
	// e.g. a generated/vendored one with a minimal or malformed
	// package.json -- count as a module root, reintroducing exactly the
	// false-positive class TestMisprefixedWorkspacePathsIgnoresACoincidentalMatchWithNoModuleMarker
	// already covers for other markers.
	ws := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ws, "tools", "internal", "service"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(ws, "tools", "internal", "service", "note.go"), []byte("package service\n"), 0o644); err != nil {
		t.Fatalf("write fixture file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(ws, "tools", "package.json"), []byte(`{"name": "tools"}`), 0o644); err != nil {
		t.Fatalf("write package.json fixture: %v", err)
	}

	got, err := MisprefixedWorkspacePaths(ws, []string{"internal/service/note.go"})
	if err != nil {
		t.Fatalf("MisprefixedWorkspacePaths: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %v, want no corrections -- package.json has no \"test\" script", got)
	}
}

func TestMisprefixedWorkspacePathsRecognizesAPackageJSONWithATestScript(t *testing.T) {
	ws := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ws, "frontend", "src"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(ws, "frontend", "src", "app.js"), []byte("// app\n"), 0o644); err != nil {
		t.Fatalf("write fixture file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(ws, "frontend", "package.json"), []byte(`{"name": "frontend", "scripts": {"test": "jest"}}`), 0o644); err != nil {
		t.Fatalf("write package.json fixture: %v", err)
	}

	got, err := MisprefixedWorkspacePaths(ws, []string{"src/app.js"})
	if err != nil {
		t.Fatalf("MisprefixedWorkspacePaths: %v", err)
	}
	want := map[string]string{"src/app.js": "frontend/src/app.js"}
	if len(got) != len(want) || got["src/app.js"] != want["src/app.js"] {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestMisprefixedWorkspacePathsSkipsHiddenTopLevelDirs(t *testing.T) {
	ws := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ws, ".git", "internal", "service"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(ws, ".git", "internal", "service", "note.go"), []byte("package service\n"), 0o644); err != nil {
		t.Fatalf("write fixture file: %v", err)
	}

	got, err := MisprefixedWorkspacePaths(ws, []string{"internal/service/note.go"})
	if err != nil {
		t.Fatalf("MisprefixedWorkspacePaths: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %v, want no corrections -- a match only under a hidden top-level dir (.git) must not count", got)
	}
}

func TestMisprefixedWorkspacePathsSkipsAlreadyInvalidPaths(t *testing.T) {
	// A path invalidWorkspaceRelativePaths already rejects (e.g. an
	// absolute path) is this function's job to leave alone, not
	// duplicate or contradict.
	ws := t.TempDir()

	got, err := MisprefixedWorkspacePaths(ws, []string{"/etc/passwd"})
	if err != nil {
		t.Fatalf("MisprefixedWorkspacePaths: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %v, want no corrections for an already-invalid path", got)
	}
}

func TestMisprefixedWorkspacePathsMissingWorkspace(t *testing.T) {
	if _, err := MisprefixedWorkspacePaths(filepath.Join(t.TempDir(), "does-not-exist"), []string{"a.go"}); err == nil {
		t.Error("expected an error for a missing workspace directory")
	}
}

// TestHeaderStrictnessProblemsChecksEveryKnownHeaderKey guards against the
// drift Codex review of PR #173 flagged as a risk: headerParsers (the map
// HeaderStrictnessProblems validates against) must have exactly one entry
// per KnownHeaderKeys, no more, no fewer. A future header added to one
// list but not the other fails this test immediately instead of silently
// letting a malformed instance of it pass the mandatory preflight.
func TestHeaderStrictnessProblemsChecksEveryKnownHeaderKey(t *testing.T) {
	if len(headerParsers) != len(KnownHeaderKeys) {
		t.Fatalf("headerParsers has %d entries, KnownHeaderKeys has %d -- they must match exactly", len(headerParsers), len(KnownHeaderKeys))
	}
	for _, key := range KnownHeaderKeys {
		if _, ok := headerParsers[key]; !ok {
			t.Errorf("KnownHeaderKeys entry %q has no headerParsers check registered", key)
		}
	}
}

// TestGoalTitleReadsSanitizesAndCaps is GoalTitle's own baseline test:
// pulled from cmd/factoryd's release_evidence_test.go when this function
// moved here (N2), so it stays covered where it now lives, not only via
// its two call sites (cmd/factoryd's pullRequestTitle and the N2 auto-
// commit subject).
func TestGoalTitleReadsSanitizesAndCaps(t *testing.T) {
	path := writeSpec(t, "## Goal\n\nLine one\nline two continues the same paragraph\n\n## Plan\n")
	got := GoalTitle(path)
	if got != "Line one line two continues the same paragraph" {
		t.Errorf("GoalTitle = %q", got)
	}
}

// TestGoalTitleEmptyForMissingSpecOrGoalSection covers GoalTitle's
// documented "never an error, just empty" contract: callers rely on this
// to fall back to their own generic title/subject.
func TestGoalTitleEmptyForMissingSpecOrGoalSection(t *testing.T) {
	if got := GoalTitle(""); got != "" {
		t.Errorf("GoalTitle(\"\") = %q, want empty", got)
	}
	if got := GoalTitle(filepath.Join(t.TempDir(), "missing.md")); got != "" {
		t.Errorf("GoalTitle(missing file) = %q, want empty", got)
	}
	noGoal := writeSpec(t, "## Plan\n\nDo the thing.\n")
	if got := GoalTitle(noGoal); got != "" {
		t.Errorf("GoalTitle(no Goal section) = %q, want empty", got)
	}
}

// TestGoalTitleTruncatesAtWordBoundaryWithEllipsis is N1's own test at
// GoalTitle's actual home: cutting mid-word must never happen, and the
// ellipsis counts inside the 72-rune cap.
func TestGoalTitleTruncatesAtWordBoundaryWithEllipsis(t *testing.T) {
	goal := "Expose each listed habit's completion fraction for the inclusive 30-calendar-day rolling window on the dashboard"
	path := writeSpec(t, "## Goal\n\n"+goal+"\n\n## Plan\n")
	title := GoalTitle(path)
	if runeCount := len([]rune(title)); runeCount > 72 {
		t.Errorf("title is %d runes, want capped at 72: %q", runeCount, title)
	}
	if !strings.HasSuffix(title, "…") {
		t.Errorf("title = %q, want it to end with an ellipsis", title)
	}
	if strings.Contains(strings.TrimSuffix(title, "…"), "rol") {
		t.Errorf("title = %q, want the straddling word dropped rather than fragmented", title)
	}
}

func writeSpec(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "spec.md")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write spec fixture: %v", err)
	}
	return path
}

func TestClosingFenceIfOpen(t *testing.T) {
	cases := []struct{ name, content, want string }{
		{"no fence", "Allowed-Files: a.go\n\n## Goal\n", ""},
		{"closed backticks", "x\n```\ncode\n```\n", ""},
		{"open backticks", "x\n```go\ncode\n", "```"},
		{"open long tildes", "x\n~~~~\ncode\n~~~\n", "~~~~"},
		{"tilde line inside backtick fence", "x\n```\n~~~\n", "```"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClosingFenceIfOpen(tc.content); got != tc.want {
				t.Errorf("ClosingFenceIfOpen = %q, want %q", got, tc.want)
			}
		})
	}
}
