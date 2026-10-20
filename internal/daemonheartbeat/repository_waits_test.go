package daemonheartbeat

import (
	"testing"
	"time"
)

func TestWorkerRepositoryWaitsRoundTripsAndIgnoresAStaleHeartbeat(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	write := func(updated time.Time) {
		t.Helper()
		hb := Heartbeat{UpdatedAt: updated.Format(time.RFC3339Nano), RepositoryWaits: map[string]string{"req-1": "2026-10-20T10:03:00Z"}, TemporalAddress: "localhost:7233"}
		if err := Write(WorkerPath(dir), hb); err != nil {
			t.Fatal(err)
		}
	}
	if got := WorkerRepositoryWaits(dir, now); got != nil {
		t.Errorf("no heartbeat: got %v, want nil", got)
	}
	write(now)
	if got := WorkerRepositoryWaits(dir, now); len(got) != 1 || got["req-1"] != "2026-10-20T10:03:00Z" {
		t.Errorf("fresh heartbeat: got %v, want req-1 at 10:03", got)
	}
	write(now.Add(-2 * WorkerStaleAfter))
	if got := WorkerRepositoryWaits(dir, now); got != nil {
		t.Errorf("stale heartbeat: got %v, want nil", got)
	}
}
