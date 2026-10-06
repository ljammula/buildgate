package main

import (
	"runtime/debug"
	"testing"
)

// TestBuildVersion proves `factoryd version` reports the embedded git
// revision instead of "dev" for an unstamped `go build`/`go install`, while
// an -ldflags-stamped version still wins.
func TestBuildVersion(t *testing.T) {
	t.Parallel()
	rev := "0123456789abcdef0123456789abcdef01234567"
	for _, tc := range []struct {
		name     string
		stamped  string
		settings []debug.BuildSetting
		want     string
	}{
		{"stamped wins", "v1.2.3", []debug.BuildSetting{{Key: "vcs.revision", Value: rev}}, "v1.2.3"},
		{"clean revision", "dev", []debug.BuildSetting{{Key: "vcs.revision", Value: rev}, {Key: "vcs.modified", Value: "false"}}, "0123456789ab"},
		{"dirty revision", "dev", []debug.BuildSetting{{Key: "vcs.revision", Value: rev}, {Key: "vcs.modified", Value: "true"}}, "0123456789ab-dirty"},
		{"no vcs info", "dev", nil, "dev"},
	} {
		if got := buildVersion(tc.stamped, tc.settings); got != tc.want {
			t.Errorf("%s: buildVersion = %q, want %q", tc.name, got, tc.want)
		}
	}
}
