// Package ticketspec reads the machine-readable metadata a ticket spec.md
// declares about itself, so factoryd can act on what the ticket actually
// asked for instead of an independently-defaulted flag value. This is the
// structural fix for the class of bug where a ticket's real requirement
// and what factoryd actually ran drift apart silently.
package ticketspec

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"buildgate/internal/sanitize"
)

// VerifyCommandPrefix is the machine-readable line a ticket spec.md may
// contain, e.g. "Verify-Command: make verify". Free-form prose elsewhere
// in the ticket (like the existing "Acceptance / verification" section)
// is for humans; this is the one line factoryd itself parses.
const VerifyCommandPrefix = "Verify-Command:"

// AllowedFilesPrefix is the machine-readable line a ticket spec.md may
// contain to declare its own diff-scope boundary, e.g.
// "Allowed-Files: path/a.dart, path/a_test.dart" — a comma-separated list.
// A ticket's prose "Out of scope" section states this boundary for
// humans; without this key nothing actually enforces it, so a run can be
// accepted with an unrelated file changed alongside the intended one.
const AllowedFilesPrefix = "Allowed-Files:"

// TestsRequiredPrefix is the machine-readable line a ticket spec.md may
// contain to opt out of the tests_red_on_base gate, e.g.
// "Tests-Required: no -- this ticket only updates documentation". The
// gate defaults to requiring at least one changed file to match the
// repo's test patterns; this key is the ticket's own declared exception,
// and (unlike Allowed-Files:) it must carry a
// reason, since an unexplained opt-out defeats the point of a gate that
// exists to catch an untested change.
const TestsRequiredPrefix = "Tests-Required:"

// EstimatedChangedLinesPrefix is the machine-readable line a ticket spec.md
// may contain to declare the planner's estimate of the ticket's change, e.g.
// "Estimated-Changed-Lines: 240": a positive integer counting insertions plus
// deletions, as git diff --shortstat does. It is advice shown at plan review,
// never a gate on the build, though a declared value that is malformed refuses the plan; absence is a legitimate shape (older and handed-over plans).
const EstimatedChangedLinesPrefix = "Estimated-Changed-Lines:"

// KnownHeaderKeys lists every machine-readable header key this package
// parses, in the order tooling should report them (e.g. `factoryd
// check-ticket`/`ticket-template`). Exported as the single source of
// truth so such tooling can never drift from the exact prefixes the
// Parse* functions above match.
var KnownHeaderKeys = []string{
	VerifyCommandPrefix,
	AllowedFilesPrefix,
	TestsRequiredPrefix,
	EstimatedChangedLinesPrefix,
}

// PresentHeaderKeys returns, for each of KnownHeaderKeys, whether specPath
// contains at least one top-level line starting with that prefix --
// regardless of whether the line's value is well-formed. This is presence
// only, using the same top-level-line rule forEachTopLevelLine documents
// (a header quoted inside an indented example or a fenced code block does
// not count as present). It exists because a Parse* result alone can't
// always distinguish "this key is absent" from "this key is present and
// valid" -- ParseTestsRequiredOptOut in particular returns "" for both
// "Tests-Required:" absent and "Tests-Required: yes" -- so a caller that
// wants to report presence/absence explicitly (e.g. to flag a misspelled
// header a ticket author meant to set) needs this in addition to the
// Parse* functions.
func PresentHeaderKeys(specPath string) (map[string]bool, error) {
	present := make(map[string]bool, len(KnownHeaderKeys))
	err := forEachTopLevelLine(specPath, func(line string) (stop bool) {
		for _, prefix := range KnownHeaderKeys {
			if strings.HasPrefix(line, prefix) {
				present[prefix] = true
			}
		}
		return false
	})
	return present, err
}

// NearMissHeaderKey is an unrecognized top-level "Key:" line in a ticket
// spec whose key, once lowercased and stripped of "-"/"_"/" ", exactly
// matches a KnownHeaderKeys entry -- almost certainly a typo (e.g.
// "Verify-command:" or "Allowed_Files:" for "Verify-Command:"/
// "Allowed-Files:") that PresentHeaderKeys/the Parse* functions would
// otherwise treat as ordinary prose, silently leaving the header the
// author meant to declare unenforced (see check-ticket's own doc comment
// for the incident this class of mistake caused).
type NearMissHeaderKey struct {
	// Found is the raw key exactly as declared, with its trailing colon
	// (e.g. "Verify-command:").
	Found string
	// Known is the KnownHeaderKeys entry Found appears to be a typo of.
	Known string
}

// topLevelKeyLinePattern extracts the "Key" token from a top-level line
// of the shape "Key: value" -- letters/digits/"-"/"_"/" " up to (and not
// including) the first colon. Deliberately loose: it exists only to feed
// normalizeHeaderKey, which does the real, strict comparison, so
// matching too many ordinary prose lines here is harmless as long as
// none of them normalizes to a KnownHeaderKeys entry.
var topLevelKeyLinePattern = regexp.MustCompile(`^([A-Za-z][A-Za-z0-9_ -]*):`)

// normalizeHeaderKey lowercases key and removes "-", "_", and " ", so
// "Verify-Command", "verify_command", and "Verify Command" all compare
// equal.
func normalizeHeaderKey(key string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(key) {
		if r == '-' || r == '_' || r == ' ' {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// NearMissHeaderKeys scans specPath the same way PresentHeaderKeys does
// and reports every top-level "Key:" line whose key is not itself one of
// KnownHeaderKeys but normalizes (case-/punctuation-insensitively) to the
// same string as one of them. Equality-based, not a general fuzzy match,
// on purpose: it catches exactly the case-and-punctuation typo class the
// examples above show without ever flagging an unrelated prose line that
// happens to start with "Word:".
func NearMissHeaderKeys(specPath string) ([]NearMissHeaderKey, error) {
	normalizedKnown := make(map[string]string, len(KnownHeaderKeys))
	for _, k := range KnownHeaderKeys {
		normalizedKnown[normalizeHeaderKey(strings.TrimSuffix(k, ":"))] = k
	}

	var misses []NearMissHeaderKey
	err := forEachTopLevelLine(specPath, func(line string) (stop bool) {
		m := topLevelKeyLinePattern.FindStringSubmatch(line)
		if m == nil {
			return false
		}
		found := m[1] + ":"
		for _, k := range KnownHeaderKeys {
			if found == k {
				return false // already a recognized key, not a near miss
			}
		}
		if known, ok := normalizedKnown[normalizeHeaderKey(m[1])]; ok {
			misses = append(misses, NearMissHeaderKey{Found: found, Known: known})
		}
		return false
	})
	return misses, err
}

// headerParsers maps each KnownHeaderKeys entry to the Parse* call that
// validates it. Keyed by prefix (not a parallel slice) and iterated via
// KnownHeaderKeys itself in HeaderStrictnessProblems below -- found via
// Codex review of PR #173: a parallel hardcoded slice here previously let
// this list silently drift out of sync with KnownHeaderKeys if a header
// were ever added to one but not the other, which would let a malformed
// instance of the forgotten header pass the mandatory preflight silently
// while check-ticket's own report (which iterates KnownHeaderKeys
// directly) still flagged it -- the exact "check-ticket says clean, but a
// real run rejects it" class of drift this preflight exists to prevent,
// just for a header not yet added. TestHeaderStrictnessProblemsChecksEveryKnownHeaderKey
// asserts this map has no missing/extra entries relative to KnownHeaderKeys.
var headerParsers = map[string]func(specPath string) error{
	VerifyCommandPrefix:         func(specPath string) error { _, err := ParseVerifyCommand(specPath); return err },
	AllowedFilesPrefix:          func(specPath string) error { _, err := ParseAllowedFiles(specPath); return err },
	TestsRequiredPrefix:         func(specPath string) error { _, err := ParseTestsRequiredOptOut(specPath); return err },
	EstimatedChangedLinesPrefix: func(specPath string) error { _, _, err := ParseEstimatedChangedLines(specPath); return err },
}

// HeaderStrictnessProblems runs the two checks a mandatory preflight (see
// cmd/factoryd's run_ticket.go and internal/workflow's PreflightActivity)
// refuses to start a run over: a known header present but malformed
// (fails its own Parse* call), and a NearMissHeaderKeys typo. An absent
// optional header is never a problem here -- see each header's own doc
// comment for why that's a legitimate ticket shape, not a mistake this
// should catch. Returns one formatted "FAIL ..." line per problem found
// (nil when the ticket is clean), in KnownHeaderKeys order for malformed
// headers followed by near-miss keys in file order.
func HeaderStrictnessProblems(specPath string) ([]string, error) {
	return headerStrictnessProblems(specPath, false)
}

// SpecTicketHeaderStrictnessProblems leaves Allowed-Files-shaped prose in
// the verbatim spec alone; the factory's spec ticket has no ticket scope.
func SpecTicketHeaderStrictnessProblems(specPath string) ([]string, error) {
	return headerStrictnessProblems(specPath, true)
}

func headerStrictnessProblems(specPath string, noTicketScope bool) ([]string, error) {
	present, err := PresentHeaderKeys(specPath)
	if err != nil {
		return nil, err
	}

	var problems []string
	for _, prefix := range KnownHeaderKeys {
		if noTicketScope && prefix == AllowedFilesPrefix {
			continue
		}
		if !present[prefix] {
			continue
		}
		parse, ok := headerParsers[prefix]
		if !ok {
			// Cannot happen once TestHeaderStrictnessProblemsChecksEveryKnownHeaderKey
			// passes, but fail loudly rather than silently skip validation
			// for a header this package doesn't yet know how to check.
			return nil, fmt.Errorf("ticket spec %s: no strictness check registered for %s", specPath, prefix)
		}
		if err := parse(specPath); err != nil {
			problems = append(problems, fmt.Sprintf("FAIL  %-24s %s", prefix, err))
		}
	}

	nearMisses, err := NearMissHeaderKeys(specPath)
	if err != nil {
		return nil, err
	}
	for _, nm := range nearMisses {
		if noTicketScope && nm.Known == AllowedFilesPrefix {
			continue
		}
		problems = append(problems, fmt.Sprintf("FAIL  %-24s looks like a misspelling of %s -- rename it, or the header it was meant to declare is silently unenforced", nm.Found, nm.Known))
	}
	return problems, nil
}

// ParseTestsRequiredOptOut returns the ticket's declared opt-out reason
// for the tests_red_on_base gate, or "" if the ticket doesn't opt out --
// either it doesn't declare Tests-Required: at all, or declares
// "Tests-Required: yes" (the same thing said explicitly). Callers treat
// "" as "the gate must run normally". A declared "no" requires a reason
// in the form "Tests-Required: no -- <reason>"; a "no" with no reason,
// or a value that is neither "yes" nor "no -- <reason>", is a parse
// error.
func ParseTestsRequiredOptOut(specPath string) (string, error) {
	val, found, err := parseTopLevelKeyLine(specPath, TestsRequiredPrefix)
	if err != nil {
		return "", err
	}
	if !found || val == "yes" {
		return "", nil
	}
	rest, isNo := strings.CutPrefix(val, "no")
	if !isNo {
		return "", fmt.Errorf("ticket spec %s: %s must be \"yes\" or \"no -- <reason>\", got %q", specPath, TestsRequiredPrefix, val)
	}
	reason, hasReason := strings.CutPrefix(strings.TrimSpace(rest), "--")
	reason = strings.TrimSpace(reason)
	if !hasReason || reason == "" {
		return "", fmt.Errorf("ticket spec %s: %s no requires a reason (\"no -- <reason>\")", specPath, TestsRequiredPrefix)
	}
	return reason, nil
}

// ParseEstimatedChangedLines returns the ticket's declared estimate of its
// changed lines and whether it declares one. A declared value that is not a
// plain positive integer is a parse error.
func ParseEstimatedChangedLines(specPath string) (lines int, declared bool, err error) {
	val, found, err := parseTopLevelKeyLine(specPath, EstimatedChangedLinesPrefix)
	if err != nil || !found {
		return 0, false, err
	}
	lines, err = ParseEstimatedChangedLinesValue(val)
	if err != nil {
		return 0, true, fmt.Errorf("ticket spec %s: %s %w", specPath, EstimatedChangedLinesPrefix, err)
	}
	return lines, true, nil
}

// estimatedChangedLinesValue is the one accepted shape of the estimate: one
// to six digits, no leading zero, so the value is a whole number above zero.
var estimatedChangedLinesValue = regexp.MustCompile(`^[1-9][0-9]{0,5}$`)

// ParseEstimatedChangedLinesValue parses the text after the header key: a
// whole number above zero written as plain digits with no leading zero, at
// most 999999.
func ParseEstimatedChangedLinesValue(val string) (int, error) {
	if !estimatedChangedLinesValue.MatchString(val) {
		return 0, fmt.Errorf("must be a whole number above zero written as digits only, without a leading zero, got %q", val)
	}
	return strconv.Atoi(val)
}

// EstimatedChangedLinesOf reads the declared estimate from ticket content
// (the text of a spec.md) with the same top-level-line rule as
// ParseEstimatedChangedLines. It reports declared=false when the line is
// absent or malformed.
func EstimatedChangedLinesOf(content string) (lines int, declared bool) {
	var val string
	var found bool
	_ = forEachTopLevelReaderLine(strings.NewReader(content), keyLineFinder(EstimatedChangedLinesPrefix, &val, &found))
	if !found {
		return 0, false
	}
	n, err := ParseEstimatedChangedLinesValue(val)
	return n, err == nil
}

// ParseVerifyCommand returns the ticket's declared canonical verification
// command, or "" if the ticket doesn't declare one (callers fall back to
// their own default in that case).
func ParseVerifyCommand(specPath string) (string, error) {
	val, found, err := parseTopLevelKeyLine(specPath, VerifyCommandPrefix)
	if err != nil {
		return "", err
	}
	if !found {
		return "", nil
	}
	if val == "" {
		return "", fmt.Errorf("ticket spec %s: %s line has no command", specPath, VerifyCommandPrefix)
	}
	return val, nil
}

// ParseAllowedFiles returns the ticket's declared list of files a run may
// change, or nil if the ticket doesn't declare one — callers should skip
// the scope check entirely in that case (matching Verify-Command's
// optional-key pattern), so tickets written before this key existed keep
// their prior, unenforced behavior rather than failing closed unexpectedly.
func ParseAllowedFiles(specPath string) ([]string, error) {
	return parseFileListKeyLine(specPath, AllowedFilesPrefix)
}

// parseFileListKeyLine is the shared comma-separated-file-list parsing
// behind ParseAllowedFiles.
func parseFileListKeyLine(specPath, prefix string) ([]string, error) {
	val, found, err := parseTopLevelKeyLine(specPath, prefix)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, nil
	}
	var files []string
	for _, p := range strings.Split(val, ",") {
		if p = strings.TrimSpace(p); p != "" {
			files = append(files, p)
		}
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("ticket spec %s: %s line has no files", specPath, prefix)
	}
	// Preflight validation, closing a real gap (found live 2026-08-28,
	// budget-remaining-amount-20260828-181352-24969): internal/policy's
	// diffScopeViolations compares these paths by exact string equality
	// against `git diff --name-only` output from inside the run's
	// -workspace checkout, which is always relative to that checkout root.
	// An absolute host path or one that escapes the checkout can never
	// match a real changed file, so every declared-scope gate fails
	// against a diff that may be entirely correct — a run then burns a
	// full build round only to quarantine on an authoring mistake it
	// could have rejected before the agent ever started. Failing closed
	// here, at parse time, is the single choke point every execution path
	// (direct, Temporal RunWorkflow, RepositoryOwnerWorkflow) already
	// shares, since all three call ParseAllowedFiles
	// once, before branching on how to execute.
	if invalid := invalidWorkspaceRelativePaths(files); len(invalid) > 0 {
		return nil, fmt.Errorf("ticket spec %s: %s declares path(s) that are not valid workspace-relative paths (must not be absolute and must not contain a \"..\" segment): %v", specPath, prefix, invalid)
	}
	return files, nil
}

// RewriteAllowedFilesLine appends each of add to specPath's single
// top-level Allowed-Files: line, leaving every other byte of the file
// untouched, and returns the file's full new content -- internal/request's
// AmendScope (factoryd amend-scope) is the one caller, widening a
// quarantined ticket's approved diff-scope boundary by hand. Refuses,
// leaving specPath unread any further, unless it declares EXACTLY one
// top-level Allowed-Files: line: zero means there is no scope to widen,
// and more than one is already ambiguous about which line diff_scope's
// own gate enforces (ParseAllowedFiles silently uses only the first top-
// level occurrence it finds).
//
// Uses the same top-level-line rule forEachTopLevelLine documents (outside
// any fenced code block, no leading whitespace) so this can never rewrite
// a line ParseAllowedFiles itself would not have parsed as the real
// Allowed-Files declaration.
func RewriteAllowedFilesLine(specPath string, add []string) (string, error) {
	raw, err := os.ReadFile(specPath)
	if err != nil {
		return "", fmt.Errorf("read ticket spec: %w", err)
	}
	trailingNewline := len(raw) > 0 && raw[len(raw)-1] == '\n'
	lines := strings.Split(string(raw), "\n")
	if trailingNewline {
		lines = lines[:len(lines)-1] // strings.Split leaves a trailing "" element after the final "\n"
	}

	matchIdx := -1
	inFence := false
	var fenceChar byte
	var fenceLen int
	for i, line := range lines {
		trimmedForFence := strings.TrimSpace(line)
		if inFence {
			if ClosesFence(trimmedForFence, fenceChar, fenceLen) {
				inFence = false
			}
			continue
		}
		if ch, n := FenceMarker(trimmedForFence); ch != 0 {
			inFence, fenceChar, fenceLen = true, ch, n
			continue
		}
		if line != strings.TrimLeft(line, " \t") {
			continue // indented (example/illustration), not top-level metadata
		}
		if !strings.HasPrefix(line, AllowedFilesPrefix) {
			continue
		}
		if matchIdx != -1 {
			return "", fmt.Errorf("ticket spec %s: more than one top-level %s line", specPath, AllowedFilesPrefix)
		}
		matchIdx = i
	}
	if matchIdx == -1 {
		return "", fmt.Errorf("ticket spec %s: no top-level %s line to widen", specPath, AllowedFilesPrefix)
	}
	lines[matchIdx] = lines[matchIdx] + ", " + strings.Join(add, ", ")
	out := strings.Join(lines, "\n")
	if trailingNewline {
		out += "\n"
	}
	return out, nil
}

// invalidWorkspaceRelativePaths returns the subset of paths that cannot
// possibly match a workspace-relative git diff path: absolute paths, and
// paths containing a ".." segment that would escape the checkout root.
// This is a syntactic check only — it does not require the path to exist
// in the tree, since Allowed-Files routinely names a file the
// ticket expects to be newly created and so cannot exist at base.
func invalidWorkspaceRelativePaths(paths []string) []string {
	var invalid []string
	for _, p := range paths {
		if !isValidWorkspaceRelativePath(p) {
			invalid = append(invalid, p)
		}
	}
	return invalid
}

// ValidWorkspaceRelativePath is isValidWorkspaceRelativePath, exported for
// internal/request's AmendScope (factoryd amend-scope): it validates an
// operator-widened Allowed-Files entry with the exact same rule
// parseFileListKeyLine already enforces at ticket-authoring time, so a path
// amend-scope accepts can never be one diff_scope itself could never match.
func ValidWorkspaceRelativePath(p string) bool {
	return isValidWorkspaceRelativePath(p)
}

func isValidWorkspaceRelativePath(p string) bool {
	// Git and ticket authors both use "/"-separated paths regardless of
	// OS, so "path" (not "path/filepath") is the right package here.
	if p == "" || path.IsAbs(p) {
		return false
	}
	// Segment check runs on the RAW string, not path.Clean(p) (found via
	// codex review round 3, 2026-08-28): path.Clean("foo/../bar") is
	// "bar", which contains no ".." segment and so would wrongly pass —
	// but factoryd stores and matches the *raw* declared string (see
	// parseFileListKeyLine and internal/policy.matchesAllowed), which
	// compares "foo/../bar" against git's own canonical "bar" and can
	// never match. Cleaning here would validate a pattern this preflight
	// exists specifically to reject: one that can never be satisfied,
	// exactly the class of false-quarantine bug this whole change targets.
	//
	// The same reasoning extends to "." segments and doubled "/"s (found
	// via a real GitHub Codex App review comment, 2026-08-29):
	// "Allowed-Files: ./a.go" or "dir//a.go" are just as unmatchable
	// against git's canonical "a.go"/"dir/a.go" as a ".." segment is, and
	// for the identical reason -- the raw string is what gets compared. A
	// single trailing "/" is deliberately still allowed: that's the
	// directory-prefix pattern internal/policy.matchesAllowed itself
	// understands (e.g. "app/l10n/"), so an empty segment is only
	// rejected when it is NOT the last one -- a legitimate single
	// trailing slash produces exactly one empty final segment, while
	// "dir//a.go" or "dir//" (a doubled slash, trailing or not) produce
	// an empty segment somewhere before the last one too.
	segs := strings.Split(p, "/")
	for i, seg := range segs {
		if seg == ".." || seg == "." {
			return false
		}
		if seg == "" && i != len(segs)-1 {
			return false
		}
	}
	return true
}

// MisprefixedWorkspacePaths returns, for each of paths that does not exist
// at workspace/<path> but DOES exist as a real file at exactly one
// workspace/<subdir>/<path>, a map from the declared path to that corrected
// one -- e.g. "internal/service/note.go" -> "backend/internal/service/note.go"
// for a repo whose real Go module root is a backend/ subdirectory rather
// than the git checkout root.
//
// Found via a real live brownfield run against a Flutter + Go app repo,
// 2026-09-11: a ticket (hand-written, but the same shape a real
// goal_pilot.py /spec-plan drafting produced for an earlier ticket in the
// same session) declared Allowed-Files the way a
// human or an LLM naturally thinks of a package path, with no awareness
// that this repo's own application code lives under backend/, not the git
// root. diff_scope correctly quarantined the run
// after a real, ~25-minute sandboxed round produced entirely correct
// code -- the earliest this repo's existing checks catch a misprefixed
// path is after the full build cost has already been paid. This function
// exists to catch it before that cost, as a preflight, the same way
// invalidWorkspaceRelativePaths already catches a syntactically-impossible
// path before the build rather than after.
//
// Deliberately NOT "path doesn't exist anywhere" (see
// invalidWorkspaceRelativePaths's own doc comment): Allowed-Files
// routinely, legitimately names a file the ticket is about to create for
// the first time, which cannot exist anywhere yet -- that is the ordinary
// case, not an error, and is not reported here. Only a path that fails to
// exist at its declared location while a real file with that exact suffix
// already exists under precisely one other top-level subdirectory is
// reported: a strong, narrow signal that the declared path is missing a
// prefix, not evidence the file is new. A match under zero or more than
// one subdirectory is not reported either -- no match means "plausibly a
// new file," and more than one match is ambiguous, not worth guessing.
//
// A matching subdirectory must also look like a real module/app root of
// its own -- carry one of moduleRootMarkers, the same manifest set
// cmd/factoryd's own detectVerifyCommand already uses to recognize a
// project's real root -- not just any subdirectory that happens to
// contain a same-suffix file (found via adversarial review, 2026-09-11):
// without this, a coincidental match -- a vendored copy, a generated
// stub, or (this very own repo's own shape: internal/registryproxy
// and internal/sandbox each have their own
// canonical_image.go) another unrelated part of a monorepo sharing an
// internal-package-naming convention -- would report a "correction" for
// a ticket that was never actually misprefixed, hard-rejecting an
// otherwise-valid run. Requiring a module marker keeps the true-positive
// case (a Flutter + Go app repo's own backend/go.mod) working while
// sharply narrowing the false-positive one to "also happens to have its
// own go.mod/package.json/etc.", a much rarer coincidence.
func MisprefixedWorkspacePaths(workspace string, paths []string) (map[string]string, error) {
	entries, err := os.ReadDir(workspace)
	if err != nil {
		return nil, fmt.Errorf("read workspace %s: %w", workspace, err)
	}
	var subdirs []string
	for _, e := range entries {
		// Hidden entries (.git, .github, ...) are never where a repo's
		// real application source lives relative to its own checkout
		// root, and walking into .git in particular would be both wrong
		// and wasteful.
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		if !hasModuleRootMarker(path.Join(workspace, e.Name())) {
			continue
		}
		subdirs = append(subdirs, e.Name())
	}

	corrections := map[string]string{}
	for _, p := range paths {
		if !isValidWorkspaceRelativePath(p) {
			continue // invalidWorkspaceRelativePaths' own job, not this function's
		}
		if _, err := os.Stat(path.Join(workspace, p)); err == nil {
			continue // exists exactly where declared -- nothing to suggest
		}
		var matches []string
		for _, d := range subdirs {
			if _, err := os.Stat(path.Join(workspace, d, p)); err == nil {
				matches = append(matches, d)
			}
		}
		if len(matches) == 1 {
			corrections[p] = path.Join(matches[0], p)
		}
	}
	if len(corrections) == 0 {
		return nil, nil
	}
	return corrections, nil
}

// makefileRootTargetRE matches the same top-level "verify:" or "test:"
// Makefile target line cmd/factoryd's own detectVerifyCommand looks for
// (see cmd/factoryd/onboard.go) before trusting a Makefile as evidence of
// a real project root.
var makefileRootTargetRE = regexp.MustCompile(`(?m)^(verify|test):`)

// hasModuleRootMarker mirrors detectVerifyCommand's own root-detection
// rules exactly, not just "one of the same filenames exists": a Makefile
// only counts when it declares a verify: or test: target, and a
// package.json only when it parses and declares a "test" script -- the
// same two signals detectVerifyCommand itself requires before trusting
// either as a real project root. Duplicated here rather than shared
// because cmd/factoryd already imports this package, so importing back
// would cycle; keep the two in sync by hand if either changes (found via
// Codex review, 2026-09-11: plain existence checks let a Makefile-only
// root go undetected, and let an unparseable/no-test-script package.json
// wrongly count as one -- a coincidental match under a directory like
// that then produces a false-positive "correction").
func hasModuleRootMarker(dir string) bool {
	if _, err := os.Stat(path.Join(dir, "go.mod")); err == nil {
		return true
	}
	if b, err := os.ReadFile(path.Join(dir, "Makefile")); err == nil {
		if makefileRootTargetRE.MatchString(string(b)) {
			return true
		}
	}
	if b, err := os.ReadFile(path.Join(dir, "package.json")); err == nil {
		var pkg struct {
			Scripts map[string]string `json:"scripts"`
		}
		if json.Unmarshal(b, &pkg) == nil {
			if _, ok := pkg.Scripts["test"]; ok {
				return true
			}
		}
	}
	// pyproject.toml/requirements.txt: detectVerifyCommand doesn't
	// recognize either ecosystem at all, so there's no shared rule to
	// mirror -- kept as plain existence checks, same as before.
	for _, marker := range []string{"pyproject.toml", "requirements.txt"} {
		if _, err := os.Stat(path.Join(dir, marker)); err == nil {
			return true
		}
	}
	return false
}

// parseTopLevelKeyLine scans specPath for the first top-level line
// starting with prefix and returns the trimmed text after it. "Top-level"
// means outside any fenced code block and with no leading whitespace —
// shared by every machine-readable ticket key so this fence/indentation
// handling (proven correct through several review rounds; see
// FenceMarker/ClosesFence) isn't duplicated and can't drift between keys.
//
// A ticket spec.md is prose that can quote or illustrate a key it's
// describing (e.g. an indented example, or a fenced ```block``` showing
// what NOT to write) without meaning to declare it — and since these
// values are later executed or used to gate acceptance, treating such an
// example as real metadata would let ticket content control factoryd's
// behavior. This tracks both backtick and tilde fences (CommonMark allows
// either), the exact opening fence character and length, and only closes
// on a matching fence of at least that length, so a shorter or
// differently-charactered fence nested inside a longer one can't
// prematurely end it.
func parseTopLevelKeyLine(specPath, prefix string) (value string, found bool, err error) {
	err = forEachTopLevelLine(specPath, keyLineFinder(prefix, &value, &found))
	return value, found, err
}

// keyLineFinder returns the line callback every single-valued key parse
// shares: it stores the trimmed text after prefix of the first top-level line
// that starts with it, and stops there.
func keyLineFinder(prefix string, value *string, found *bool) func(line string) (stop bool) {
	return func(raw string) (stop bool) {
		if rest, ok := strings.CutPrefix(raw, prefix); ok {
			*value, *found = strings.TrimSpace(rest), true
			return true
		}
		return false
	}
}

// forEachTopLevelLine scans specPath and calls fn with each top-level
// line — outside any fenced code block and with no leading whitespace,
// the shared fence/indentation handling (proven correct through several
// review rounds; see FenceMarker/ClosesFence) every machine-readable
// ticket key relies on so it can't drift between keys. fn returning true
// stops the scan early.
//
// A ticket spec.md is prose that can quote or illustrate a key it's
// describing (e.g. an indented example, or a fenced ```block``` showing
// what NOT to write) without meaning to declare it — and since these
// values are later executed or used to gate acceptance, treating such an
// example as real metadata would let ticket content control factoryd's
// behavior. This tracks both backtick and tilde fences (CommonMark allows
// either), the exact opening fence character and length, and only closes
// on a matching fence of at least that length, so a shorter or
// differently-charactered fence nested inside a longer one can't
// prematurely end it.
func forEachTopLevelLine(specPath string, fn func(line string) (stop bool)) error {
	f, err := os.Open(specPath)
	if err != nil {
		return fmt.Errorf("open ticket spec: %w", err)
	}
	defer f.Close()
	if err := forEachTopLevelReaderLine(f, fn); err != nil {
		return fmt.Errorf("read ticket spec: %w", err)
	}
	return nil
}

// forEachTopLevelReaderLine is forEachTopLevelLine over any reader.
func forEachTopLevelReaderLine(r io.Reader, fn func(line string) (stop bool)) error {
	inFence := false
	var fenceChar byte
	var fenceLen int
	return forEachLine(r, func(raw string) (stop bool) {
		trimmedForFence := strings.TrimSpace(raw)
		if inFence {
			if ClosesFence(trimmedForFence, fenceChar, fenceLen) {
				inFence = false
			}
			return false
		}
		if ch, n := FenceMarker(trimmedForFence); ch != 0 {
			inFence, fenceChar, fenceLen = true, ch, n
			return false
		}
		if raw != strings.TrimLeft(raw, " \t") {
			return false // indented (example/illustration), not top-level metadata
		}
		return fn(raw)
	})
}

// forEachLine calls fn with one line without its line ending, including a
// final line that has no newline. Unlike bufio.Scanner, it has no per-line
// token limit; ticket specs are bounded by their callers, not by 64 KiB.
func forEachLine(r io.Reader, fn func(string) bool) error {
	reader := bufio.NewReader(r)
	for {
		raw, err := reader.ReadString('\n')
		if len(raw) > 0 {
			raw = strings.TrimSuffix(raw, "\n")
			raw = strings.TrimSuffix(raw, "\r")
			if fn(raw) {
				return nil
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// FenceVerbatim returns content inside a tilde fence that nothing in content
// can close: one tilde longer than the longest tilde run that opens any of
// its lines, and at least four. Every reader of a ticket's machine-readable
// lines (forEachTopLevelLine, GoalTitle) skips a fenced block, so a document
// quoted this way into a build input can never declare a header key, a
// required string or the ticket's goal.
func FenceVerbatim(content string) string {
	longest := 0
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		n := 0
		for n < len(trimmed) && trimmed[n] == '~' {
			n++
		}
		longest = max(longest, n)
	}
	fence := strings.Repeat("~", max(4, longest+1))
	body := content
	if body != "" && !strings.HasSuffix(body, "\n") {
		body += "\n"
	}
	return fence + "\n" + body + fence + "\n"
}

// ClosingFenceIfOpen returns the delimiter line that closes a Markdown fence
// content leaves open at end of input, or "" when every fence is closed --
// judged by the same FenceMarker/ClosesFence rules forEachTopLevelLine
// parses headers with. A caller appending text after content (a corrective
// round's addendum) writes this first, so the appended text is never read
// as the inside of content's fence, or, worse, as the close of it followed
// by top-level header lines.
func ClosingFenceIfOpen(content string) string {
	inFence := false
	var fenceChar byte
	var fenceLen int
	for _, raw := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(raw)
		if inFence {
			if ClosesFence(trimmed, fenceChar, fenceLen) {
				inFence = false
			}
			continue
		}
		if ch, n := FenceMarker(trimmed); ch != 0 {
			inFence, fenceChar, fenceLen = true, ch, n
		}
	}
	if !inFence {
		return ""
	}
	return strings.Repeat(string(fenceChar), fenceLen)
}

// FenceMarker reports the fence character (` or ~) and length of a
// Markdown fence delimiter at the start of a trimmed line, or (0, 0) if
// the line isn't a fence delimiter (fewer than 3 of the same character).
// An opening fence may be followed by an info string (e.g. "```python"),
// which this permits — only ClosesFence requires nothing but whitespace
// after the marker, matching CommonMark's actual asymmetric rule.
func FenceMarker(trimmed string) (ch byte, length int) {
	if trimmed == "" {
		return 0, 0
	}
	ch = trimmed[0]
	if ch != '`' && ch != '~' {
		return 0, 0
	}
	for length < len(trimmed) && trimmed[length] == ch {
		length++
	}
	if length < 3 {
		return 0, 0
	}
	return ch, length
}

// ClosesFence reports whether trimmed is a valid closing delimiter for a
// fence opened with character ch and length minLen. Unlike an opening
// fence, CommonMark requires a closing fence to contain nothing but the
// fence character (at least minLen repetitions) and optional trailing
// whitespace — a line like "~~~not-a-close" has the right marker but
// trailing content, so it does not close the fence and must still be
// treated as content inside it.
func ClosesFence(trimmed string, ch byte, minLen int) bool {
	n := 0
	for n < len(trimmed) && trimmed[n] == ch {
		n++
	}
	if n < minLen {
		return false
	}
	return strings.TrimSpace(trimmed[n:]) == ""
}

// GoalTitle reads specPath's own "## Goal" section (plan_tickets.py's own
// ticket template: a heading followed by "One paragraph: what this ticket
// accomplishes.") and returns a single-line, sanitized, length-capped
// rendering fit for a commit subject or PR title. Shared by cmd/factoryd's
// pullRequestTitle (the PR title) and internal/workflow's PostBuildActivity
// plus cmd/factoryd's own direct-run host auto-commit (both build the
// safety-net commit's subject line from it, N2) -- one goal-title
// derivation, not three that could drift. The ticket text is agent-drafted
// and therefore untrusted, the same status as every other agent-written
// field a PR body already sanitizes before interpolating -- sanitize.Line
// strips control/ANSI/secret-shaped content and folds it to one line
// first.
//
// Returns "" (never an error) for anything that stops this from producing
// a real title -- unreadable specPath, no "## Goal" heading, or an empty
// section -- so a caller's own generic fallback always applies; a missing
// or malformed Goal section must never block a PR or a commit.
func GoalTitle(specPath string) string {
	if specPath == "" {
		return ""
	}
	raw, err := parseGoalSection(specPath)
	if err != nil || raw == "" {
		return ""
	}
	title := sanitize.Line(raw)
	// goalTitleMaxLen (that same review's "cap it around 72 chars"):
	// git/GitHub impose no hard limit, but 72 is the conventional commit
	// subject-line bound this repo's own commit messages already follow
	// (see AGENTS.md's own commit-message guidance), so a title this size
	// still reads cleanly in a PR list, `git log --oneline`, or a squash
	// merge's own subject. Counted and sliced in runes, not bytes (a
	// review found): title[:72] on a byte string can split a multi-byte
	// rune (a ticket goal with non-ASCII text -- an em dash, an accented
	// name, an emoji) mid-encoding, producing invalid UTF-8 in a public
	// GitHub PR title.
	const goalTitleMaxLen = 72
	runes := []rune(title)
	if len(runes) > goalTitleMaxLen {
		title = truncateAtWordBoundary(runes, goalTitleMaxLen)
	}
	return title
}

// goalTitleEllipsis marks a GoalTitle cut short of the goal's real end.
const goalTitleEllipsis = "…"

// truncateAtWordBoundary cuts runes to at most maxLen runes -- INCLUDING
// goalTitleEllipsis, appended -- breaking at the last word boundary
// within that budget rather than mid-word. Found live: a plain
// runes[:maxLen] cut produced PR titles like "Expose each listed habit's
// completion fraction for the inclusive 30-cale" (N1), silently dropping
// the rest of "30-calendar-day" with no indication the title was cut at
// all. Falls back to a hard cut at the budget when the truncated span
// contains no space at all (e.g. one very long unbroken word) rather than
// producing just the ellipsis.
func truncateAtWordBoundary(runes []rune, maxLen int) string {
	limit := maxLen - len([]rune(goalTitleEllipsis))
	if limit < 0 {
		limit = 0
	}
	candidate := runes[:limit]
	cut := limit
	for cut > 0 && !unicode.IsSpace(candidate[cut-1]) {
		cut--
	}
	if cut == 0 {
		// No word boundary anywhere in the window -- keep the hard cut
		// rather than trim the whole candidate away.
		cut = limit
	}
	return strings.TrimRight(string(candidate[:cut]), " \t") + goalTitleEllipsis
}

// parseGoalSection reads specPath and returns the paragraph under its own
// top-level "## Goal" heading: every contiguous non-blank line
// immediately following the heading (its own lead paragraph only,
// matching the ticket template's "(One paragraph: ...)" convention),
// joined with a single space. Returns "" (no error) when the file has no
// such heading or the section is empty -- a caller's own fallback
// decides what that means, this never treats it as a read failure.
func parseGoalSection(specPath string) (string, error) {
	f, err := os.Open(specPath)
	if err != nil {
		return "", err
	}
	defer f.Close()

	var lines []string
	inGoal := false
	inFence := false
	var fenceChar byte
	var fenceLen int
	var result string
	err = forEachLine(f, func(raw string) (stop bool) {
		trimmed := strings.TrimSpace(raw)
		// A "## Goal" inside a fenced block (an approved spec quoted into a
		// build input by FenceVerbatim) is content, never the ticket's goal.
		if inFence {
			if ClosesFence(trimmed, fenceChar, fenceLen) {
				inFence = false
			}
			return false
		}
		if ch, n := FenceMarker(trimmed); ch != 0 {
			if inGoal && len(lines) > 0 {
				result = strings.Join(lines, " ")
				return true
			}
			inFence, fenceChar, fenceLen = true, ch, n
			return false
		}
		switch {
		case !inGoal:
			if trimmed == "## Goal" {
				inGoal = true
			}
		case trimmed == "":
			if len(lines) > 0 {
				result = strings.Join(lines, " ")
				return true
			}
			// Blank line(s) right after the heading itself: keep waiting
			// for the paragraph to start.
		case strings.HasPrefix(trimmed, "#"):
			// A later heading with no paragraph text in between.
			result = strings.Join(lines, " ")
			return true
		default:
			lines = append(lines, trimmed)
		}
		return false
	})
	if err != nil {
		return "", err
	}
	if result != "" {
		return result, nil
	}
	return strings.Join(lines, " "), nil
}
