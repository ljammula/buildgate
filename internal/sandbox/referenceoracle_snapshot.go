package sandbox

import (
	"errors"
	"fmt"
	"os"

	"buildgate/internal/evidence"
)

// SnapshotReferenceOracle is the one TOCTOU-protected snapshot-then-hash
// sequence every reference-oracle mount in this repo goes through: cmd/factoryd's
// snapshotAndHashReferenceOracle and the build and named-gate
// Activities (internal/workflow). It was previously
// three near-line-for-line copies of the same containment check,
// evidence.SnapshotTree, and evidence.SHA256Tree calls (found via review
// of the in-loop oracle work, which is what would have added a fourth);
// a fix to any one of them -- the hash algorithm, a new error class, the
// containment rule -- now lands once.
//
// Order matters, and is the whole point: sourceDir is checked against
// workDir (see ReferenceOracleSourceContained's own doc comment for why it
// must be the operator's ORIGINALLY CONFIGURED source, not a later
// snapshot destination that is always outside the workspace by
// construction), then copied into snapshotDir, then hashed -- both the
// recorded hash and the eventual read-only mount act on that one fixed
// snapshot, never on two independent reads of a live host directory an
// external process could change between them or across a multi-round
// build's whole lifetime (PR #152 round 2: safety-contract.md SC-012
// requires the recorded hash to actually describe what got mounted).
//
// snapshotDir is removed first if it already exists: a Temporal Activity attempt can be retried by the
// orchestrator after a crash between snapshotting and its own durable
// checkpoint, and evidence.SnapshotTree refuses an already-existing
// destination. snapshotDir must be absolute (LaunchSpec.Validate requires
// an absolute ReferenceOracleDir) and outside workDir; callers own
// removing it once the launch that mounts it has finished -- a defer here
// would delete it before the caller ever mounted it. On any error nothing
// is left behind.
func SnapshotReferenceOracle(workDir, sourceDir, snapshotDir string) (sha256Hash string, err error) {
	// Existence first, so an unreachable source (a Temporal Worker on another
	// host, a container without the data dir mounted) is reported as exactly
	// that: ReferenceOracleSourceContained treats a path it cannot resolve
	// conservatively as "contained", which would tell the operator the
	// directory "must not be inside the workspace" -- true of nothing here.
	if _, err := os.Lstat(sourceDir); err != nil {
		return "", fmt.Errorf("reference-oracle directory %s is not accessible on this host: %w", sourceDir, err)
	}
	if ReferenceOracleSourceContained(workDir, sourceDir) {
		return "", errors.New("reference-oracle directory must not be inside the workspace")
	}
	if err := os.RemoveAll(snapshotDir); err != nil {
		return "", fmt.Errorf("clear stale reference-oracle snapshot: %w", err)
	}
	if err := evidence.SnapshotTree(sourceDir, snapshotDir); err != nil {
		return "", fmt.Errorf("snapshot reference-oracle content: %w", err)
	}
	hash, err := evidence.SHA256Tree(snapshotDir)
	if err != nil {
		_ = os.RemoveAll(snapshotDir)
		return "", fmt.Errorf("hash reference-oracle content: %w", err)
	}
	return hash, nil
}
