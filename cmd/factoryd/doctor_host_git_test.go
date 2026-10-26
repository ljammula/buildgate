package main

import (
	"errors"
	"strings"
	"testing"

	"buildgate/internal/workspace"
)

// TestDoctorWarnsOnAHostGitBelowTheRestackFloor: a host git older than the
// floor a restack needs, or one whose version cannot be read, is one
// advisory warning naming the floor, never a failure; a git at or above it
// passes.
func TestDoctorWarnsOnAHostGitBelowTheRestackFloor(t *testing.T) {
	old := doctorHostGitCheck("git version 2.39.5 (Apple Git-154)", nil)
	if old.Err == nil || !old.Advisory || !strings.Contains(old.Err.Error(), workspace.GitFloor) || !strings.Contains(old.Fix, "upgrade git") {
		t.Errorf("git 2.39.5: %+v, want an advisory warning naming %s and the upgrade", old, workspace.GitFloor)
	}
	if unread := doctorHostGitCheck("", errors.New("git: not found")); unread.Err == nil || !unread.Advisory {
		t.Errorf("unreadable git: %+v, want an advisory warning", unread)
	}
	if ok := doctorHostGitCheck("git version 2.50.1 (Apple Git-155)", nil); ok.Err != nil {
		t.Errorf("git 2.50.1: %+v, want a pass", ok)
	}
}
