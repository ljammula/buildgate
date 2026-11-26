package main

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"time"

	"buildgate/internal/prune"
	"buildgate/internal/sanitize"
)

// With prune_after set the worker prunes its own data dir (internal/prune:
// the worktrees, scratch, build sessions and round refs of requests finished
// longer than prune_after ago; never a record) at most once a day. It never
// touches Docker images: only `factoryd prune` does, when an operator asks.
const (
	autoPruneEvery     = 24 * time.Hour
	autoPruneCheck     = time.Hour
	autoPruneStampName = "prune-last-run"
)

// autoPruneLoop runs autoPruneOnce now and on every tick until ctx is done.
// after == 0 (prune_after absent) does nothing.
func autoPruneLoop(ctx context.Context, dp *deps, dataDir string, after time.Duration, ticks <-chan time.Time) {
	if after <= 0 {
		return
	}
	autoPruneOnce(dp, dataDir, after, time.Now())
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticks:
			autoPruneOnce(dp, dataDir, after, now)
		}
	}
}

// autoPruneOnce prunes dataDir unless it was pruned within autoPruneEvery,
// which the stamp file's time records across restarts, and logs what it
// removed. It reports whether it ran.
func autoPruneOnce(dp *deps, dataDir string, after time.Duration, now time.Time) bool {
	stamp := filepath.Join(dataDir, autoPruneStampName)
	if info, err := os.Stat(stamp); err == nil && now.Sub(info.ModTime()) < autoPruneEvery {
		return false
	}
	if err := os.WriteFile(stamp, []byte(now.UTC().Format(time.RFC3339)+"\n"), 0o600); err != nil {
		log.Printf("factoryd worker: prune: %v", err)
		return false
	}
	_ = os.Chtimes(stamp, now, now)
	plan, err := prune.PlanDataDir(dataDir, after, now)
	if err != nil {
		log.Printf("factoryd worker: prune: %v", err)
		return true
	}
	if len(plan.Items) == 0 && len(plan.Requests) == 0 {
		return true
	}
	freed, err := plan.Apply(pruneRemover{dp: dp})
	for _, it := range plan.Items {
		if _, statErr := os.Lstat(it.Path); os.IsNotExist(statErr) {
			log.Printf("factoryd worker: prune: removed %s %s (%s, %s)", it.Kind, sanitize.Line(it.Path), sanitize.Line(it.Owner), pruneBytes(it.Bytes))
		}
	}
	if err != nil {
		log.Printf("factoryd worker: prune: %v", err)
	}
	log.Printf("factoryd worker: prune: %d finished request(s) older than %s, freed %s", len(plan.Requests), after, pruneBytes(freed))
	return true
}
