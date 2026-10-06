// Package modelhost enforces mutual exclusion against a single-instance
// model host shared by multiple independent factoryd runs -- different
// repositories, different -data-dir values, even different operator
// processes on the same machine -- all of which contend for the same
// relay upstream. Unlike internal/workspace's DirectLock (repository-
// scoped) or worker's own per-data-dir flock, nothing today serializes
// two runs that both happen to point relay_upstream at the same
// single-instance host: it bit live on 2026-09-22 (two independent
// factoryd runs calling the local ai-stack model concurrently) and again
// via the "parallel agents" incident recorded in this repo's own memory.
//
// The lock is host-global (keyed by the upstream's own host, hashed, and
// stored under the operator's home directory) rather than scoped to any
// one run, repository or data directory -- deliberately, since the whole
// point is to serialize callers that share nothing else in common.
// AcquireNamed offers the same host-global slots under a fixed name, for
// another host-wide resource (internal/sandbox's compose sidecars gate).
package modelhost

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// DefaultConcurrency is the number of concurrent callers a locked model
// host allows when the operator's session config leaves
// model_host_concurrency unset -- one at a time, matching the incidents
// above (both were a single-instance local model).
const DefaultConcurrency = 1

// pollInterval is how often Acquire retries every slot once all of them
// are busy, and how often it re-reports the current holder to onWait.
const pollInterval = 2 * time.Second

// LocksDir returns the directory holding every model-host lock file:
// ~/.config/factoryd/locks. Host-global on purpose -- see the package
// doc comment -- so it lives under the operator's home directory, not
// under any one run's -data-dir.
func LocksDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory for model-host lock: %w", err)
	}
	return filepath.Join(home, ".config", "factoryd", "locks"), nil
}

// hostKey returns the sha256 hex digest of upstream's own host[:port] --
// not the full URL, so two upstreams differing only by path (unlikely in
// practice, since relay_upstream is host-only) or by an irrelevant query
// string still contend for the same lock. Falls back to hashing the raw
// string when upstream doesn't parse as a URL with a host, so a
// malformed value still gets a stable (if overly specific) key rather
// than an error -- ShouldLock/Acquire's callers already validate
// relay_upstream elsewhere (RoutePolicy.Validate); this package's job is
// only to serialize, never to re-validate.
func hostKey(upstream string) string {
	host := upstream
	if u, err := url.Parse(upstream); err == nil && u.Host != "" {
		host = strings.ToLower(u.Host)
	}
	sum := sha256.Sum256([]byte(host))
	return hex.EncodeToString(sum[:])
}

// LockPath returns the lock file for the slot-th concurrent caller of
// upstream (slot 0 through concurrency-1). Slot 0 is named exactly as
// the plan describes (model-<sha256(upstream-host)>.lock); every other
// slot gets a .slotN suffix, so the common concurrency=1 case matches
// the documented path exactly.
func LockPath(locksDir, upstream string, slot int) string {
	key := hostKey(upstream)
	if slot == 0 {
		return filepath.Join(locksDir, fmt.Sprintf("model-%s.lock", key))
	}
	return filepath.Join(locksDir, fmt.Sprintf("model-%s.slot%d.lock", key, slot))
}

// ShouldLock decides whether upstream needs the model-host lock at all,
// per the plan's recommendation: lock by default only for a non-TLS
// upstream, or one whose host is a private/loopback/Tailscale/.lan/.local
// address -- the shapes a single-instance, operator-run model host
// actually takes. A remote SaaS upstream (chatgpt.com,
// api.githubcopilot.com, api.anthropic.com) is a multi-tenant service
// with its own server-side concurrency handling, not a single box one
// factoryd run can starve out from under another, so it defaults to no
// lock -- an operator who still wants one sets model_host_concurrency
// anyway, since Acquire only consults ShouldLock, not the reverse
// (callers may skip this check entirely to force locking).
//
// A URL that fails to parse, or has no host at all, locks (fails
// closed): this function's only job is deciding whether a *shared*
// single-instance host is in play, and "unknown" is closer to "maybe
// shared" than to "definitely a multi-tenant remote service".
func ShouldLock(upstream string) bool {
	u, err := url.Parse(upstream)
	if err != nil || u.Host == "" {
		return true
	}
	if !strings.EqualFold(u.Scheme, "https") {
		return true
	}
	return isPrivateHost(u.Hostname())
}

// isPrivateHost reports whether host is a loopback/RFC1918/Tailscale
// CGNAT address, or ends in .local/.lan -- the address shapes an
// operator's own machine or home/office network model host actually
// uses. A plain public DNS name that isn't one of those suffixes (e.g.
// chatgpt.com) is not resolved here -- this package does no DNS lookups
// of its own (a lookup would make ShouldLock's result depend on network
// reachability, and be one more way a locking decision could hang) -- so
// it reads as not-private, matching the recommendation that only a
// visibly private/local upstream defaults to locked.
func isPrivateHost(host string) bool {
	if host == "" {
		return true
	}
	lower := strings.ToLower(host)
	// .ts.net is Tailscale's own MagicDNS domain: any hostname under it
	// names a device on the operator's private tailnet by construction,
	// the same private-mesh category as .lan/.local -- recognized by
	// suffix, not a DNS lookup (see this function's own doc comment for
	// why this package never resolves names).
	if lower == "localhost" || strings.HasSuffix(lower, ".local") || strings.HasSuffix(lower, ".lan") || strings.HasSuffix(lower, ".ts.net") {
		return true
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	if ip.IsLoopback() || ip.IsPrivate() {
		return true
	}
	// Tailscale's CGNAT range, 100.64.0.0/10 -- not covered by
	// net.IP.IsPrivate (that's RFC1918/RFC4193 only).
	if ip4 := ip.To4(); ip4 != nil && ip4[0] == 100 && ip4[1] >= 64 && ip4[1] <= 127 {
		return true
	}
	return false
}

// Handle is a held slot of the model-host lock. Release is idempotent
// and safe to call once via defer.
type Handle struct {
	file *os.File
}

// Release unlocks and closes the held slot. Safe to call on a nil
// Handle (Acquire returns one whenever locking applies at all).
func (h *Handle) Release() error {
	if h == nil || h.file == nil {
		return nil
	}
	f := h.file
	h.file = nil
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_UN); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// holderRecord is written into a slot's own lock file content by whoever
// holds it -- flock's ownership is process/fd-scoped and invisible to a
// waiter that hasn't acquired it, but the file's own bytes are still
// readable by anyone, so a waiter can report a human-legible "queued
// behind <holder>" without needing to acquire anything itself.
type holderRecord struct {
	RunID      string `json:"run_id"`
	PID        int    `json:"pid"`
	AcquiredAt string `json:"acquired_at"`
}

func writeHolder(f *os.File, runID string) error {
	rec := holderRecord{RunID: runID, PID: os.Getpid(), AcquiredAt: time.Now().UTC().Format(time.RFC3339)}
	b, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	if err := f.Truncate(0); err != nil {
		return err
	}
	if _, err := f.WriteAt(b, 0); err != nil {
		return err
	}
	return nil
}

// readHolder best-effort reads whatever holder identity is currently
// recorded in path's content, or "" if the file is absent, empty, or not
// valid JSON -- never an error: this is purely an operator-visibility
// aid, and a caller with nothing to report simply says less.
func readHolder(path string) string {
	b, err := os.ReadFile(path)
	if err != nil || len(b) == 0 {
		return ""
	}
	var rec holderRecord
	if err := json.Unmarshal(b, &rec); err != nil || rec.RunID == "" {
		return ""
	}
	return rec.RunID + " (pid " + strconv.Itoa(rec.PID) + ")"
}

// Acquire blocks until a free slot (of concurrency total) for upstream
// is obtained, ctx is canceled, or an unexpected filesystem error
// occurs. concurrency <= 0 disables locking outright (the
// model_host_concurrency: 0 escape hatch): Acquire returns a nil Handle
// and nil error immediately, and the caller proceeds unlocked exactly as
// it did before this package existed.
//
// A waiting caller must be visible, not silent (the operator-visibility
// goal: silence is a bug) -- onWait, when non-nil, is called with a
// human-legible holder description ("<run id> (pid <pid>)") every time
// the reported holder changes, including the first observation, so a
// caller can turn that into a progress event ("queued behind <holder>")
// without this package needing to know anything about progress.jsonl.
// Acquire never fails just because every slot is busy; it keeps waiting
// (per pollInterval) until one frees up or ctx ends.
func Acquire(ctx context.Context, upstream, runID string, concurrency int, onWait func(holder string)) (*Handle, error) {
	return acquire(ctx, func(locksDir string, slot int) string { return LockPath(locksDir, upstream, slot) }, runID, concurrency, onWait)
}

// NamedLockPath returns the lock file for the slot-th holder of the named
// host-global lock: <name>.lock, then <name>.slotN.lock.
func NamedLockPath(locksDir, name string, slot int) string {
	if slot == 0 {
		return filepath.Join(locksDir, name+".lock")
	}
	return filepath.Join(locksDir, fmt.Sprintf("%s.slot%d.lock", name, slot))
}

// AcquireNamed is Acquire for a fixed lock name rather than a model
// host's upstream, with the same semantics: concurrency <= 0 disables it,
// onWait reports each new holder, and it waits until a slot frees or ctx
// ends.
func AcquireNamed(ctx context.Context, name, runID string, concurrency int, onWait func(holder string)) (*Handle, error) {
	return acquire(ctx, func(locksDir string, slot int) string { return NamedLockPath(locksDir, name, slot) }, runID, concurrency, onWait)
}

func acquire(ctx context.Context, pathFor func(locksDir string, slot int) string, runID string, concurrency int, onWait func(holder string)) (*Handle, error) {
	if concurrency <= 0 {
		return nil, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	locksDir, err := LocksDir()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(locksDir, 0o700); err != nil {
		return nil, fmt.Errorf("create model-host locks directory %q: %w", locksDir, err)
	}

	lastReported := ""
	for {
		for slot := 0; slot < concurrency; slot++ {
			path := pathFor(locksDir, slot)
			f, openErr := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
			if openErr != nil {
				return nil, fmt.Errorf("open model-host lock %q: %w", path, openErr)
			}
			flockErr := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
			if flockErr == nil {
				if err := writeHolder(f, runID); err != nil {
					_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
					_ = f.Close()
					return nil, fmt.Errorf("record model-host lock holder %q: %w", path, err)
				}
				return &Handle{file: f}, nil
			}
			_ = f.Close()
		}

		holder := ""
		for slot := 0; slot < concurrency; slot++ {
			if h := readHolder(pathFor(locksDir, slot)); h != "" {
				holder = h
				break
			}
		}
		if holder != "" && holder != lastReported && onWait != nil {
			onWait(holder)
			lastReported = holder
		}

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(pollInterval):
		}
	}
}
