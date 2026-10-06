package progress

import "buildgate/internal/run"

// Path returns where run id's progress feed lives under dataDir. Kept in
// its own file, separate from the rest of this dependency-free package,
// so the internal/run import is easy to spot and remove if it ever needs
// to go the other way (see the package doc comment).
func Path(dataDir, runID string) string {
	return PathInDir(run.Dir(dataDir, runID))
}
