package sandbox

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// maxKeptLogs bounds how many earlier logs one log path keeps beside it.
const maxKeptLogs = 99

// CreateLaunchLog opens path for a new launch's output. A non-empty file
// already there, the log of an earlier launch at the same path (a retried
// planning job writes plan_tickets.log again), is first renamed to the
// lowest free "<stem>.<n><ext>" (plan_tickets.1.log, plan_tickets.2.log, ...),
// so the text a halt quoted survives the retry; the halt's path then names
// the new launch's log, and `factoryd logs -list` shows both. An empty file is
// overwritten. After maxKeptLogs kept logs the oldest numbers stay and the
// newest earlier log is overwritten in the last slot. Anything at path that is
// not a regular file (a link, a directory) is refused, never followed.
func CreateLaunchLog(path string) (*os.File, error) {
	info, err := os.Lstat(path)
	switch {
	case err == nil && !info.Mode().IsRegular():
		return nil, fmt.Errorf("log path %s is not a regular file", path)
	case err == nil && info.Size() > 0:
		if err := os.Rename(path, keptLogPath(path)); err != nil {
			return nil, fmt.Errorf("keep the earlier log %s: %w", path, err)
		}
	case err != nil && !os.IsNotExist(err):
		return nil, fmt.Errorf("check log path %s: %w", path, err)
	}
	// O_NOFOLLOW: a link put at path after the check is refused too.
	return os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC|syscall.O_NOFOLLOW, 0o600)
}

// keptLogPath is the first "<stem>.<n><ext>" beside path that does not exist,
// or the maxKeptLogs-th when all are taken.
func keptLogPath(path string) string {
	ext := filepath.Ext(path)
	stem := strings.TrimSuffix(path, ext)
	candidate := ""
	for n := 1; n <= maxKeptLogs; n++ {
		candidate = fmt.Sprintf("%s.%d%s", stem, n, ext)
		if _, err := os.Lstat(candidate); os.IsNotExist(err) {
			return candidate
		}
	}
	return candidate
}
