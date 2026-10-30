package policy

import "strings"

// RepoGatePrefix marks a gate a target repository defines itself in its
// .factory.yml `gates:` list, the only command gates besides canonical
// verification and the full suite: "repo-<id>" is its key in a run's gate
// commands, its run.GateResult.Check and its Temporal activity id suffix. It
// has no slash because the id is part of an activity id and a progress mark.
const RepoGatePrefix = "repo-"

// RepoGateCheck is the check name of the repo-defined gate id.
func RepoGateCheck(id string) string { return RepoGatePrefix + id }

// IsRepoGate reports whether check names a repo-defined gate.
func IsRepoGate(check string) bool { return strings.HasPrefix(check, RepoGatePrefix) }

// RepoGateChecks returns the repo-defined gates among commands (a run's
// gate commands, keyed by check) that have a command, sorted by name: the
// order they run in.
func RepoGateChecks(commands map[string]string) []string {
	var out []string
	for check, command := range commands {
		if !IsRepoGate(check) || command == "" {
			continue
		}
		// Inserted in order: this package keeps to a short stdlib
		// allow-list (internal/claims), and there are at most a few.
		at := len(out)
		for at > 0 && out[at-1] > check {
			at--
		}
		out = append(out, "")
		copy(out[at+1:], out[at:])
		out[at] = check
	}
	return out
}
