package release

import (
	"buildgate/internal/policy"
	"buildgate/internal/projectconfig"
	"buildgate/internal/run"
	"strings"
)

// buildgateDir is the repository directory the factory's committed-oracle
// index lives in (internal/oraclecommit.IndexDir). It is protected
// unconditionally, beside projectconfig.FileName: only the factory host
// writes there, and only via a hash-pinned oracle commit.
const buildgateDir = ".buildgate"

// ProtectedFilesTouched returns the subset of changed that is protected --
// cfg's own ProtectedPaths, plus projectconfig.FileName (.factory.yml) and
// the .buildgate/ directory unconditionally -- in changed's original order.
// .factory.yml is always
// included, on top of whatever the operator configured: the sandboxed
// agent must not be able to weaken its own acceptance oracle (a lower
// verify_command, higher ceilings, a brownfield profile) and have that
// diff merged as an ordinary change. Extracted out of MergePolicyCheck's
// own per-file loop so a caller that only wants to know which files are
// protected (cmd/factoryd's risk header) doesn't have to re-derive
// MergePolicyCheck's full pass/fail evaluation just to answer that, and so
// every caller -- including the API and Temporal paths, which never go
// through cmd/factoryd's own flag parsing at all -- gets the .factory.yml
// rule from this one place.
//
// This form has no run evidence, so it cannot exempt anything: use
// ProtectedFilesTouchedByRun wherever a run record is available.
func (cfg MergePolicy) ProtectedFilesTouched(changed []string) []string {
	protectedPaths := append([]string{projectconfig.FileName, buildgateDir, buildgateDir + "/"}, cfg.ProtectedPaths...)
	return matchProtected(changed, protectedPaths)
}

// ProtectedFilesTouchedByRun is ProtectedFilesTouched with the run's
// committed-oracle evidence applied:
//
//   - every path listed in .buildgate/oracles.json as read from the run's
//     BASE commit (r.Oracles.BaseIndexPaths) is protected, so an agent
//     edit or delete of a previously committed oracle quarantines through
//     the ordinary release check. No index at base means no extra
//     protection, not an error;
//   - .buildgate/ is protected always;
//   - the run's own factory-authored oracle writes are exempt, but ONLY
//     while the blob at ResultSHA still hashes to the hash the host pinned
//     (or a declared supersession deletion is really absent), and paths
//     merely unchanged from the run's own base are exempt (a cumulative
//     -diff-base inventory lists an earlier round's committed oracle) are
//     exempt only while the blob at ResultSHA hashes to the sha256 the BASE
//     index pinned for them. The exemption is keyed on the hash, never on
//     the path, and never on "same bytes as the round's own base";
//   - projectconfig.FileName and cfg.ProtectedPaths are never exempt.
func (cfg MergePolicy) ProtectedFilesTouchedByRun(r run.Run) []string {
	explicit := append([]string{projectconfig.FileName}, cfg.ProtectedPaths...)
	oracleProtected := []string{buildgateDir, buildgateDir + "/"}
	if r.Oracles != nil {
		oracleProtected = append(oracleProtected, r.Oracles.BaseIndexPaths...)
		// A path this run itself authored or deleted is protected too, so
		// that an exemption is what lets it through, not silence.
		for _, a := range r.Oracles.Authored {
			oracleProtected = append(oracleProtected, a.Path)
		}
		oracleProtected = append(oracleProtected, r.Oracles.Deleted...)
	}
	var touched []string
	for _, c := range r.ChangedFiles {
		// Case-insensitive worktrees (macOS) let an agent commit
		// ".BuildGate/..." past an exact-case match, so fold case for the
		// factory's own directory. Only a DIFFERENTLY-cased path is refused
		// here; the exact-case path goes through the hash-keyed logic below
		// so the factory's own index write stays exempt.
		if first, _, _ := strings.Cut(c, "/"); first != buildgateDir && strings.EqualFold(first, buildgateDir) {
			touched = append(touched, c)
			continue
		}
		if len(matchProtected([]string{c}, explicit)) > 0 {
			touched = append(touched, c)
			continue
		}
		if len(matchProtected([]string{c}, oracleProtected)) == 0 {
			continue
		}
		if r.Oracles.FactoryAuthoredIntact(c) || r.Oracles.MatchesBaseIndexPin(c) {
			continue
		}
		touched = append(touched, c)
	}
	return touched
}

func matchProtected(changed, protectedPaths []string) []string {
	var touched []string
	for _, c := range changed {
		// DiffScope is the repository's canonical exact path-list matcher.
		// A one-file scope passes precisely when that file is protected.
		gate, _ := policy.DiffScope([]string{c}, protectedPaths)
		if gate.Passed {
			touched = append(touched, c)
		}
	}
	return touched
}
