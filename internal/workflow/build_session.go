package workflow

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"go.temporal.io/sdk/activity"

	"buildgate/internal/evidence"
)

// BuildSessionDirName is the folder, in a run's own directory, that holds the
// copy of its build's harness session a later round of the same ticket may
// continue (round_session_continuity).
const BuildSessionDirName = "build-session"

// maxKeptBuildSessionBytes bounds the kept copy of a build session: a
// session larger than this is not kept, and the round that follows starts a
// fresh one.
const maxKeptBuildSessionBytes = 64 << 20

// buildSessionSkipped reports whether rel, a path in a build session folder,
// stays out of the kept copy: the build's saved prompts and failed rounds'
// output (kept for the operator elsewhere), the agent's notes for the next
// attempt (which reach a build only through the factory's record, SC-018),
// and any file a harness keeps a credential or a download in. Neither the
// worker's harnesses nor the relay put a credential in the session (the
// relay's key is a placeholder in the launch's environment); a file of these
// names is still never carried, and a harness that wants one writes it again.
func buildSessionSkipped(rel string) bool {
	if evidence.IsBuildSessionSideFile(rel) {
		return true
	}
	for _, p := range strings.Split(filepath.ToSlash(rel), "/") {
		switch p {
		case "auth.json", ".credentials.json", ".tmp", "tmp":
			return true
		}
	}
	return false
}

// keepBuildSession copies the build session folder src into dst (created
// fresh): only regular files and directories, none skipped by
// buildSessionSkipped, at most maxKeptBuildSessionBytes in all. A link is
// never followed or copied. On any failure, an oversize session included,
// dst is removed and the error returned: a round that follows then starts a
// fresh session.
func keepBuildSession(src, dst string) error {
	if err := os.RemoveAll(dst); err != nil {
		return err
	}
	if err := copyBuildSession(src, dst, maxKeptBuildSessionBytes); err != nil {
		_ = os.RemoveAll(dst)
		return err
	}
	return nil
}

// restoreBuildSession puts the kept session src into the worktree's session
// folder dst, which must not exist or is replaced. The same copy rules as
// keepBuildSession apply. A failure leaves no folder at dst: the build then
// starts a fresh session (build_app.py records why).
func restoreBuildSession(src, dst string) error {
	if err := removeBuildSession(dst); err != nil {
		return err
	}
	if err := copyBuildSession(src, dst, maxKeptBuildSessionBytes); err != nil {
		_ = removeBuildSession(dst)
		return err
	}
	return nil
}

var errSessionTooLarge = errors.New("build session is over the size it may be kept at")

func copyBuildSession(src, dst string, limit int64) error {
	info, err := os.Lstat(src)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", src)
	}
	var total int64
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if rel != "." && buildSessionSkipped(rel) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		target := filepath.Join(dst, rel)
		switch {
		case d.IsDir():
			return os.MkdirAll(target, 0o700)
		case !d.Type().IsRegular():
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		if total += fi.Size(); total > limit {
			return errSessionTooLarge
		}
		return copyRegularFile(path, target)
	})
}

func copyRegularFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// resumeSessionArgs carries the kept build session of the run this round
// follows (RunWorkflowInput.ResumeSessionPath) into the worktree's session
// folder and returns --resume-session for build_app.py, which continues it
// when it can and starts fresh otherwise. Nothing for a run given none, or
// for a build resumed after an interrupted attempt (handedOver), which
// starts a fresh session on its kept work. A copy that fails is logged and
// leaves no folder, so build_app.py records the fresh start and why.
func (a *Activities) resumeSessionArgs(ctx context.Context, input RunWorkflowInput, handedOver bool) []string {
	if input.ResumeSessionPath == "" || handedOver || input.WorkspacePath == "" {
		return nil
	}
	if err := restoreBuildSession(input.ResumeSessionPath, filepath.Join(input.WorkspacePath, buildSessionDir)); err != nil {
		activity.GetLogger(ctx).Warn("the build session of the run this round follows could not be carried; the build starts a fresh one", "error", err)
	}
	return []string{"--resume-session"}
}
