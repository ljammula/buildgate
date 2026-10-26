package run

import (
	"path/filepath"

	"buildgate/internal/evidence"
)

// HeldCopyOnDisk reports whether e's host path is a file under its run's
// review-repro directory in dataDir with the hash e records. factoryd checks
// every entry again, links included, before a build starts.
func HeldCopyOnDisk(dataDir string, e RecheckEntry) bool {
	if !ValidID(e.RunID) || e.SHA256 == "" {
		return false
	}
	dir, err := filepath.Abs(ReviewReproductionDir(Dir(dataDir, e.RunID)))
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(dir, e.HostPath)
	if err != nil || !filepath.IsLocal(rel) {
		return false
	}
	sum, err := evidence.SHA256File(e.HostPath)
	return err == nil && sum == e.SHA256
}
