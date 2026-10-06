package hostcontrol

import (
	"buildgate/internal/daemonheartbeat"
	"time"
)

// quickstartWorkerReadyTimeout bounds how long quickstart waits for a
// freshly spawned worker child to either hold the drain lock (see
// acquireWorkerLock) or die during startup, before giving up and
// proceeding optimistically.
const quickstartWorkerReadyTimeout = 10 * time.Second

// WorkerLockFileName is the lock file one worker holds for as long
// as it is draining, directly inside <data-dir>/queue. Named with a
// leading dot: a plain file, not a submission.
const WorkerLockFileName = ".queue-run.lock"

// WorkerHeartbeatPID returns the pid in dataDir's worker heartbeat and
// whether that heartbeat is fresh (a stale one names a process that has
// stopped refreshing it, so it is not evidence of a live worker).
func WorkerHeartbeatPID(dataDir string, now time.Time) (pid int, fresh bool) {
	hb, err := daemonheartbeat.Read(daemonheartbeat.WorkerPath(dataDir))
	if err != nil || daemonheartbeat.Stale(hb, now, daemonheartbeat.WorkerStaleAfter) {
		return 0, false
	}
	return hb.PID, true
}
