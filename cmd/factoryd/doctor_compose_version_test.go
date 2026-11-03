package main

import "testing"

// TestComposeVersionAtLeastReadsThePatch: the minimum is compared down to the
// patch (2.23.0 is below 2.23.1), a suffixed patch is read by its leading
// digits, and a version it cannot read as MAJOR.MINOR.PATCH is too old.
func TestComposeVersionAtLeastReadsThePatch(t *testing.T) {
	minimum := [3]int{2, 23, 1}
	for version, want := range map[string]bool{
		"2.23.1":           true,
		"v2.23.1":          true,
		"2.23.1-desktop.1": true,
		"2.24.0":           true,
		"5.3.1":            true,
		"2.23.0":           false,
		"2.20.3":           false,
		"1.29.2":           false,
		"2.23":             false,
		"2.x.1":            false,
		"":                 false,
	} {
		if got := composeVersionAtLeast(version, minimum); got != want {
			t.Errorf("composeVersionAtLeast(%q) = %v, want %v", version, got, want)
		}
	}
}
