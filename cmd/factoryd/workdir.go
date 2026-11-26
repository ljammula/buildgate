package main

import (
	"fmt"
	"os"
	"path/filepath"

	"buildgate/internal/hostcontrol"
	"buildgate/internal/sessionconfig"
)

// absolutePaths makes each non-empty path absolute against the directory
// the command started in, so it keeps naming the same file once the
// process works from its stable directory (enterStableWorkDir). An empty
// value stays empty: it means the default.
func absolutePaths(paths ...*string) error {
	for _, p := range paths {
		if *p == "" || filepath.IsAbs(*p) {
			continue
		}
		abs, err := filepath.Abs(*p)
		if err != nil {
			return fmt.Errorf("resolve %s: %w", *p, err)
		}
		*p = abs
	}
	return nil
}

// absoluteConfigArg is a -config value as an absolute path: a profile name
// becomes its file (sessionconfig.ResolveArg), a relative path is resolved
// against the directory the command started in, and "" stays "" (the
// active profile, then the default paths).
func absoluteConfigArg(v string) (string, error) {
	p := sessionconfig.ResolveArg(v)
	err := absolutePaths(&p)
	return p, err
}

// enterStableWorkDir moves a long-lived worker or serve out of the
// directory it was started in, which can be a worktree removed while it
// runs (a process whose directory is gone fails every child it starts),
// into the data dir, or $HOME when the data dir does not exist. Every path
// it still uses was made absolute before (absolutePaths).
func enterStableWorkDir(dp *deps, dataDir string) error {
	dir := hostcontrol.StableWorkDir(dataDir)
	if err := dp.host.chdir(dir); err != nil {
		return fmt.Errorf("change to %s: %w", dir, err)
	}
	return nil
}

func (realHost) chdir(dir string) error { return os.Chdir(dir) }
