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
	"syscall"
	"time"

	"go.temporal.io/sdk/activity"

	"buildgate/internal/evidence"
	"buildgate/internal/runner"
)

// BuildSessionDirName is the folder, in a run's own directory, that holds the
// copy of its build's harness session a later round of the same ticket may
// continue (round_session_continuity).
const BuildSessionDirName = "build-session"

// The limits of a kept build session: a session over any of them is not
// kept, and the round that follows starts a fresh one, told why
// (notKeptFile).
const (
	maxKeptBuildSessionBytes   = 64 << 20
	maxKeptBuildSessionEntries = 10000
	maxKeptBuildSessionDepth   = 32
)

// notKeptFile is the one file a kept-session folder holds when the session
// was not kept: the factory's one-line reason, which build_app.py reports
// as the fresh start's reason.
const notKeptFile = "not-kept.txt"

// buildSessionSkipped reports whether rel, a path in a build session folder,
// stays out of the kept copy: what the build script keeps beside the
// harness's state (evidence.IsBuildSessionSideFile: the saved prompts, the
// failed rounds' output and the notes file, each kept for its own reader),
// any file a harness keeps a credential or a download in, a harness's
// rendered configuration, and its skills folder: the adapter writes the
// configuration and the operator's mounted skills again at every launch, so
// nothing the build wrote there is carried. Neither the worker's harnesses
// nor the relay put a credential in the session (the relay's key is a
// placeholder in the launch's environment); a file of these names is still
// never carried. The agent's notes never stay in the session itself: the
// notes turn sets the session's state aside in pre-notes/ and puts it back
// after (build_app.py set_session_aside), so its exchange is dropped; a
// pre-notes/ left behind is not carried either, and a session whose notes
// could not be taken out is marked so and never continued.
func buildSessionSkipped(rel string) bool {
	if evidence.IsBuildSessionSideFile(rel) {
		return true
	}
	for _, p := range strings.Split(filepath.ToSlash(rel), "/") {
		switch p {
		case "auth.json", ".credentials.json", ".tmp", "tmp", "skills", "config.toml", "config.json", "pre-notes":
			return true
		}
	}
	return false
}

// keepBuildSession copies the build session folder src into dst (created
// fresh) under copyBuildSession's rules. A session that cannot be kept
// whole leaves dst holding only notKeptFile with the reason, and the error
// is returned: the round that follows then starts a fresh session and says
// why.
func keepBuildSession(src, dst string) error {
	if err := os.RemoveAll(dst); err != nil {
		return err
	}
	err := copyBuildSession(src, dst)
	if err == nil {
		return nil
	}
	_ = os.RemoveAll(dst)
	if mkErr := os.MkdirAll(dst, 0o700); mkErr == nil {
		reason := strings.Join(strings.Fields(err.Error()), " ")
		if len(reason) > 200 {
			reason = reason[:200]
		}
		_ = os.WriteFile(filepath.Join(dst, notKeptFile), []byte(reason+"\n"), 0o600)
	}
	return err
}

// restoreBuildSession puts the kept session src into the worktree's session
// folder dst, which must not exist or is replaced, under copyBuildSession's
// rules. A failure leaves no folder at dst: the build then starts a fresh
// session (build_app.py records why).
func restoreBuildSession(src, dst string) error {
	if err := removeBuildSession(dst); err != nil {
		return err
	}
	if err := copyBuildSession(src, dst); err != nil {
		_ = removeBuildSession(dst)
		return err
	}
	return nil
}

var errSessionTooLarge = errors.New("build session is over the size, entry count or depth it may be kept at")

// copyBuildSession copies the session folder src into dst, a new folder:
// both are opened as an os.Root, so no path read or written resolves outside
// them; every file is opened with O_NOFOLLOW|O_NONBLOCK and must stat as a
// regular file on the opened handle; links and anything else that is not a
// regular file or a directory are not copied; buildSessionSkipped paths are
// left out; and at most maxKeptBuildSessionBytes (read through a limit, so a
// file that grows fails the copy), maxKeptBuildSessionEntries entries and
// maxKeptBuildSessionDepth levels are copied, or nothing.
func copyBuildSession(src, dst string) error {
	srcRoot, err := os.OpenRoot(src)
	if err != nil {
		return err
	}
	defer srcRoot.Close()
	if err := os.Mkdir(dst, 0o700); err != nil {
		return err
	}
	dstRoot, err := os.OpenRoot(dst)
	if err != nil {
		return err
	}
	defer dstRoot.Close()
	c := sessionCopy{src: srcRoot, dst: dstRoot, left: maxKeptBuildSessionBytes}
	return fs.WalkDir(srcRoot.FS(), ".", c.visit)
}

// sessionCopy is one copyBuildSession: its roots and what is left of its
// limits.
type sessionCopy struct {
	src, dst *os.Root
	left     int64
	entries  int
}

func (c *sessionCopy) visit(rel string, d fs.DirEntry, err error) error {
	if err != nil {
		return err
	}
	if rel == "." {
		return nil
	}
	if buildSessionSkipped(rel) {
		if d.IsDir() {
			return fs.SkipDir
		}
		return nil
	}
	if c.entries++; c.entries > maxKeptBuildSessionEntries || strings.Count(rel, "/")+1 > maxKeptBuildSessionDepth {
		return errSessionTooLarge
	}
	switch {
	case d.IsDir():
		return c.dst.Mkdir(rel, 0o700)
	case !d.Type().IsRegular():
		return nil
	}
	n, err := copySessionFile(c.src, c.dst, rel, c.left)
	c.left -= n
	return err
}

// copySessionFile copies the regular file rel of src into dst, reading at
// most left bytes: a file that is not regular on its opened handle, or that
// holds more than left bytes when read, fails the copy.
func copySessionFile(src, dst *os.Root, rel string, left int64) (int64, error) {
	in, err := src.OpenFile(rel, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return 0, err
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return 0, err
	}
	if !info.Mode().IsRegular() {
		return 0, fmt.Errorf("%s is not a regular file", rel)
	}
	out, err := dst.OpenFile(rel, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return 0, err
	}
	n, err := io.Copy(out, io.LimitReader(in, left+1))
	if closeErr := out.Close(); err == nil {
		err = closeErr
	}
	if err == nil && n > left {
		err = errSessionTooLarge
	}
	return n, err
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

// keepFinishedBuildSession keeps the finished build's session in logDir
// (keepBuildSession), heartbeating the step while it copies, as it did
// while the build ran: the copy is bounded, but never left to outlive the
// step's heartbeat timeout. Best-effort: a session not kept makes the round
// that follows start fresh.
func (a *Activities) keepFinishedBuildSession(ctx context.Context, input RunWorkflowInput, logDir string) {
	started := time.Now()
	_, err := heartbeatWhileRunning(activityHeartbeatInterval, func() {
		recordStepHeartbeat(ctx, HeartbeatDetails{Stage: "build", Elapsed: time.Since(started)})
	}, func() (runner.Result, error) {
		return runner.Result{}, keepBuildSession(filepath.Join(input.WorkspacePath, buildSessionDir), filepath.Join(logDir, BuildSessionDirName))
	})
	if err != nil {
		activity.GetLogger(ctx).Warn("failed to keep the finished build's session for a round that follows", "error", err)
	}
}
