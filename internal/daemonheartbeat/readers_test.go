package daemonheartbeat

import "time"

// Readers of heartbeat fields that only the tests read back.

// WorkerRepositoryWaits returns the RepositoryWaits of dataDir's worker
// heartbeat, or nil when there is no heartbeat or it is stale.
func WorkerRepositoryWaits(dataDir string, now time.Time) map[string]string {
	hb, err := Read(WorkerPath(dataDir))
	if err != nil || Stale(hb, now, WorkerStaleAfter) {
		return nil
	}
	return hb.RepositoryWaits
}
