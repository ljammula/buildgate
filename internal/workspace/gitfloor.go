package workspace

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// The oldest host git a restack runs on: its git calls read attributes from
// the empty tree (`git --attr-source`, git 2.41) and compute patch ids with
// `git patch-id --verbatim` (git 2.39). GitFloor is the same version as text.
const (
	gitFloorMajor = 2
	gitFloorMinor = 41
	GitFloor      = "2.41"
)

// HostGitVersion is the output of the host's `git version`, trimmed, e.g.
// "git version 2.39.5 (Apple Git-154)".
func HostGitVersion(ctx context.Context) (string, error) {
	out, err := exec.CommandContext(ctx, "git", "version").Output()
	if err != nil {
		return "", fmt.Errorf("git version: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// ParseGitVersion reads the major and minor version from `git version`
// output ("git version 2.39.5 (Apple Git-154)", "git version 2.45.0.windows.1").
func ParseGitVersion(out string) (major, minor int, err error) {
	fields := strings.Fields(out)
	if len(fields) < 3 || fields[0] != "git" || fields[1] != "version" {
		return 0, 0, fmt.Errorf("%q is not the output of `git version`", out)
	}
	parts := strings.Split(fields[2], ".")
	if len(parts) < 2 {
		return 0, 0, fmt.Errorf("git version %q has no minor version", fields[2])
	}
	if major, err = strconv.Atoi(parts[0]); err == nil {
		minor, err = strconv.Atoi(parts[1])
	}
	if err != nil {
		return 0, 0, fmt.Errorf("git version %q: %w", fields[2], err)
	}
	return major, minor, nil
}

// CheckGitFloor says why the git whose `git version` output is out cannot
// run a restack: older than GitFloor, or a version that cannot be read. nil
// when it can.
func CheckGitFloor(out string) error {
	major, minor, err := ParseGitVersion(out)
	if err != nil {
		return err
	}
	if major < gitFloorMajor || (major == gitFloorMajor && minor < gitFloorMinor) {
		return fmt.Errorf("the host's git is %d.%d (%q), older than %s, the oldest a restack's git calls run on", major, minor, out, GitFloor)
	}
	return nil
}
