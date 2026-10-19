package sandbox

import (
	"regexp"
	"strings"
)

// Docker label keys this package stamps on every container/network it
// creates are "<labelPrefix><name>" (e.g. "buildgate.run").
const labelPrefix = "buildgate."

// legacyLabelPrefix is the pre-rename (software-factory -> buildgate,
// 2026-09-26) label prefix. It is READ/SWEPT only, never written: it exists
// so a relay/registry-proxy/worker/compose network leaked by a pre-rename
// factoryd -- a relay holds a real upstream credential -- is still found and
// reclaimed by the same data-dir-scoped sweeps. Delete it (and the
// per-prefix loops that use labelPrefixes) once no such resource remains.
const legacyLabelPrefix = "software-factory."

// labelPrefixes is every prefix a sweep must search, current first.
var labelPrefixes = []string{labelPrefix, legacyLabelPrefix}

// labelKey returns the current label key for name.
func labelKey(name string) string { return labelPrefix + name }

// dedupeLines appends the non-empty lines of out to lines, skipping any
// already in seen (whole-line match).
func dedupeLines(lines []string, seen map[string]bool, out string) []string {
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if line == "" || seen[line] {
			continue
		}
		seen[line] = true
		lines = append(lines, line)
	}
	return lines
}

// maxGatewayLabelValueLength is the sandbox gateway's limit on a label value.
const maxGatewayLabelValueLength = 63

var gatewayLabelValueDisallowed = regexp.MustCompile(`[^A-Za-z0-9._-]`)

// GatewayLabelValue fits s to what the sandbox gateway accepts as a label
// value: letters, digits, '-', '_' and '.', at most 63 long, beginning and
// ending with a letter or a digit. The ends are trimmed after the cut: a run
// id cut at the limit can end on a '-', and the gateway then refuses the
// sandbox. The gateway keeps a sandbox's run id and data-dir label in this
// form; the run's own record is what the factory reads.
func GatewayLabelValue(s string) string {
	s = gatewayLabelValueDisallowed.ReplaceAllString(s, "-")
	if len(s) > maxGatewayLabelValueLength {
		s = s[:maxGatewayLabelValueLength]
	}
	return strings.Trim(s, "-_.")
}
