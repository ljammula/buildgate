package openshell

import (
	"regexp"
	"strings"
	"testing"
)

// gatewayLabelValue is the gateway's rule for a label value: empty, or
// letters, digits, '-', '_' and '.', beginning and ending with a letter or a
// digit, at most 63 characters.
var gatewayLabelValue = regexp.MustCompile(`^([A-Za-z0-9]([A-Za-z0-9._-]{0,61}[A-Za-z0-9])?)?$`)

// Every label value the factory sends must be one the gateway accepts,
// whatever the run id: a refused label is a sandbox that never starts. The
// first case is a run id whose cut at the limit falls right after a '-'.
func TestLabelValueIsAlwaysOneTheGatewayAccepts(t *testing.T) {
	cases := []string{
		"retry-strategies-in-tenacity-retry-py-co-20261011-113459-2-001-20261011-113512-77894",
		"", "-", "---", "a", "a-", "-a", ".a_", "run 1/with spaces", "ünïcödé-run", "/Users/kanna/buildgate/walk3/data",
		strings.Repeat("a", 62) + "-" + "b", strings.Repeat("a", 63) + "-", strings.Repeat("-", 200), strings.Repeat("a-", 100), strings.Repeat("a.", 31) + "-x",
	}
	// Every cut position against every separator: the class, not the one id.
	for n := 55; n <= 70; n++ {
		for _, sep := range []string{"-", "_", ".", "--", "-.", " ", "/"} {
			cases = append(cases, strings.Repeat("r", n)+sep+"tail", strings.Repeat("r", n-len(sep))+sep, sep+strings.Repeat("r", n))
		}
	}
	for _, in := range cases {
		got := labelValue(in)
		if !gatewayLabelValue.MatchString(got) {
			t.Errorf("labelValue(%q) = %q, which the gateway refuses", in, got)
		}
	}
	if got := labelValue("plain-run-1"); got != "plain-run-1" {
		t.Errorf("a value already valid changed: %q", got)
	}
	// Only the ends go: the cut value keeps everything else.
	if got, want := labelValue(cases[0]), "retry-strategies-in-tenacity-retry-py-co-20261011-113459-2-001"; got != want {
		t.Errorf("labelValue of a run id cut after a '-' = %q, want %q", got, want)
	}
}
