package workspace

import (
	"context"
	"strings"
	"testing"
)

func TestParseGitVersion(t *testing.T) {
	for _, tc := range []struct {
		out          string
		major, minor int
	}{
		{"git version 2.39.5 (Apple Git-154)", 2, 39},
		{"git version 2.50.1", 2, 50},
		{"git version 2.45.0.windows.1", 2, 45},
		{"git version 3.0.0", 3, 0},
	} {
		major, minor, err := ParseGitVersion(tc.out)
		if err != nil || major != tc.major || minor != tc.minor {
			t.Errorf("ParseGitVersion(%q) = %d, %d, %v; want %d, %d", tc.out, major, minor, err, tc.major, tc.minor)
		}
	}
	for _, out := range []string{"", "git", "hub version 2.41.0", "git version 2", "git version x.y"} {
		if _, _, err := ParseGitVersion(out); err == nil {
			t.Errorf("ParseGitVersion(%q) read a version", out)
		}
	}
}

func TestCheckGitFloor(t *testing.T) {
	for out, ok := range map[string]bool{
		"git version 2.39.5 (Apple Git-154)": false,
		"git version 2.40.1":                 false,
		"git version 2.41.0":                 true,
		"git version 2.50.1 (Apple Git-155)": true,
		"git version 3.0.0":                  true,
		"not git":                            false,
	} {
		err := CheckGitFloor(out)
		if (err == nil) != ok {
			t.Errorf("CheckGitFloor(%q) = %v, want ok %v", out, err, ok)
		}
		if err != nil && !ok && strings.HasPrefix(out, "git") && !strings.Contains(err.Error(), GitFloor) {
			t.Errorf("CheckGitFloor(%q) = %v, want it to name %s", out, err, GitFloor)
		}
	}
}

func TestHostGitVersionReadsThisMachinesGit(t *testing.T) {
	out, err := HostGitVersion(context.Background())
	if err != nil {
		t.Skipf("no git: %v", err)
	}
	if _, _, err := ParseGitVersion(out); err != nil {
		t.Fatal(err)
	}
}
