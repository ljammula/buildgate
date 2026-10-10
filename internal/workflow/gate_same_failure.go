package workflow

import (
	"os"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"buildgate/internal/runner"
	"buildgate/internal/sanitize"
)

// maxGateFailureLogBytes is the most of one run's output two failures of a
// gate are compared over. A longer log is not compared at all: comparing a
// part of it could call two different failures the same.
const maxGateFailureLogBytes = 4 << 20

// sameGateFailure reports whether a gate's command failed on the base commit
// (onBase) the same way as on the build's result (onResult): the same exit
// code and the same output, the whole of it, line for line, once what changes
// from one run of the same failure to the next is removed (gateFailureLine).
//
// Saying "the same" sends the gate to the operator and ends the ticket's
// corrective builds, so every doubt is resolved the other way: output that is
// empty on either side, that cannot be read, or that is longer than
// maxGateFailureLogBytes is never the same failure, and nothing is removed
// from a line unless its shape says it is noise.
func sameGateFailure(onResult, onBase runner.Result) bool {
	if onResult.ExitCode != onBase.ExitCode {
		return false
	}
	result, ok := gateFailureLines(onResult.LogPath)
	if !ok {
		return false
	}
	base, ok := gateFailureLines(onBase.LogPath)
	return ok && result == base
}

// gateFailureLines is the whole log at path as sameGateFailure compares it;
// ok is false when there is nothing that can be compared.
func gateFailureLines(path string) (text string, ok bool) {
	info, err := os.Stat(path)
	if err != nil || info.Size() == 0 || info.Size() > maxGateFailureLogBytes {
		return "", false
	}
	raw, err := os.ReadFile(path)
	if err != nil || int64(len(raw)) > maxGateFailureLogBytes {
		return "", false
	}
	lines := strings.Split(sanitize.StripANSI(string(raw)), "\n")
	for i, line := range lines {
		lines[i] = gateFailureLine(strings.TrimRight(line, " \t\r"))
	}
	text = strings.Join(lines, "\n")
	return text, strings.TrimSpace(text) != ""
}

// The noise gateFailureLine removes. The build loop's round signature
// (round_feedback.failure_signature in agent/pi/scripts) blanks durations,
// addresses, timestamps, UUIDs and temporary paths wherever they appear; this
// is deliberately stricter, because here a difference that is blanked away
// costs a ticket its corrective build. Each pattern names a shape that only
// run-to-run noise has:
//
//   - elapsed: a time with a fraction of a second or a sub-second unit
//     ("0.01s", "312 ms"), never a whole number of seconds, minutes or hours,
//     which is what a setting or a subtest's name looks like ("10s", "5m");
//   - and only where a tool prints how long something took: alone in
//     parentheses ("--- FAIL: TestX (0.01s)"), or at the end of the line
//     after a tab ("FAIL\tpkg\t3.214s"), "in", "after" or "took"
//     ("1 failed in 0.12s ===");
//   - an address: 0x and nine or more hex digits, standing alone (a 32-bit
//     value such as 0xdeadbeef is a value);
//   - a full date and time, and a UUID, standing alone;
//   - in a path under a temporary root, the root's own random parts: the two
//     hashed directories of /var/folders/<x>/<y>/T, the digits that end the
//     first directory under the root (/tmp/go-build123456/, .../T/TestSum17/)
//     and pytest's run number (/tmp/pytest-of-<user>/pytest-<n>). The rest of
//     the path, the file's name included, is compared as written.
//
// "Standing alone" means no letter, digit or underscore on either side, in
// any script: the patterns use no \b, which knows ASCII only.
const elapsed = `(?:\d+\.\d+s|\d+(?:\.\d+)?\s?(?:ms|µs|us|ns))`

var (
	elapsedInParens  = regexp.MustCompile(`\(` + elapsed + `\)`)
	elapsedAtLineEnd = regexp.MustCompile(`((?:^|[^\pL\pN_])(?:in|after|took) |\t)` + elapsed + `([ =]*)$`)
	hexAddress       = regexp.MustCompile(`0x[0-9a-fA-F]{9,}`)
	dateAndTime      = regexp.MustCompile(`\d{4}-\d{2}-\d{2}[T ]\d{2}:\d{2}:\d{2}(?:[.,]\d+)?(?:Z|[+-]\d{2}:?\d{2})?`)
	uuid             = regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)
	tempPath         = regexp.MustCompile(`((?:/private)?(?:/var/folders/[^/\s]+/[^/\s]+/T|/tmp|/var/tmp))/(\S*)`)
	varFolders       = regexp.MustCompile(`/var/folders/[^/]+/[^/]+/T$`)
	endingDigits     = regexp.MustCompile(`\d+$`)
	pytestRun        = regexp.MustCompile(`^pytest-\d+$`)
)

// gateFailureLine is one line of a gate's output with its noise removed.
func gateFailureLine(line string) string {
	line = elapsedInParens.ReplaceAllString(line, "(N)")
	line = elapsedAtLineEnd.ReplaceAllString(line, "${1}N${2}")
	for _, alone := range []*regexp.Regexp{hexAddress, uuid, dateAndTime} {
		line = blankStandingAlone(line, alone)
	}
	return tempPath.ReplaceAllStringFunc(line, blankTempRoot)
}

// blankStandingAlone replaces each match of re in line that has no letter,
// digit or underscore on either side with "N".
func blankStandingAlone(line string, re *regexp.Regexp) string {
	var b strings.Builder
	last := 0
	for _, m := range re.FindAllStringIndex(line, -1) {
		before, _ := utf8.DecodeLastRuneInString(line[:m[0]])
		after, _ := utf8.DecodeRuneInString(line[m[1]:])
		if (m[0] > 0 && wordRune(before)) || (m[1] < len(line) && wordRune(after)) {
			continue
		}
		b.WriteString(line[last:m[0]])
		b.WriteString("N")
		last = m[1]
	}
	b.WriteString(line[last:])
	return b.String()
}

func wordRune(r rune) bool {
	return r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r)
}

// blankTempRoot blanks the random parts of one temporary path's root.
func blankTempRoot(path string) string {
	m := tempPath.FindStringSubmatch(path)
	root, rest := m[1], m[2]
	root = varFolders.ReplaceAllString(root, "/var/folders/N/N/T")
	parts := strings.Split(rest, "/")
	// Only a directory: the last part is the file, or the path's own end.
	if len(parts) > 1 {
		parts[0] = endingDigits.ReplaceAllString(parts[0], "N")
	}
	if len(parts) > 1 && strings.HasPrefix(parts[0], "pytest-of-") && pytestRun.MatchString(parts[1]) {
		parts[1] = "pytest-N"
	}
	return root + "/" + strings.Join(parts, "/")
}
