package policy

import (
	"slices"
	"testing"
)

func TestRepoGateChecksAreTheRepoKeysWithACommandSorted(t *testing.T) {
	commands := map[string]string{
		"lint":         "golangci-lint run",
		"repo-zeta":    "true",
		"repo-alpha":   "true",
		"repo-unset":   "",
		"unit_tests":   "go test ./...",
		"repository_x": "true",
	}
	if got, want := RepoGateChecks(commands), []string{"repo-alpha", "repo-zeta"}; !slices.Equal(got, want) {
		t.Errorf("RepoGateChecks = %v, want %v", got, want)
	}
	if RepoGateChecks(nil) != nil {
		t.Error("no commands must give no repo gates")
	}
}

// The prefix is what keeps a repository's gate from being taken for one of
// the registry's, or for a built-in check the release policy can require.
func TestNoBuiltInCheckIsARepoGate(t *testing.T) {
	for _, check := range AllGateChecks {
		if IsRepoGate(check) {
			t.Errorf("built-in check %q has the repo-gate prefix %q", check, RepoGatePrefix)
		}
	}
	if !IsRepoGate(RepoGateCheck("no_todo")) || RepoGateCheck("no_todo") != "repo-no_todo" {
		t.Errorf("RepoGateCheck(no_todo) = %q", RepoGateCheck("no_todo"))
	}
}
