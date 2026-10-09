package evidence

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"syscall"
)

// RoundLogsDirName is the directory, inside a run's own directory, that
// holds the retained copy of each build round's failing-command output:
// <run dir>/round-logs/round-<n>/<verify|fast-check|oracle>.log.
const RoundLogsDirName = "round-logs"

// roundLogsSource is where build_app.py saves that output, relative to the
// workspace (its feedback_dir). It is inside the harness session folder,
// which a resumed build deletes and a rollback removes with the worktree.
const roundLogsSource = ".pi-build-session/feedback"

// roundLogNames are the files build_app.py writes per round
// (FAST_CHECK_LOG, VERIFY_LOG, ORACLE_LOG). Only these names are copied:
// the folder is in a workspace the build agent can write.
var roundLogNames = []string{"fast-check.log", "verify.log", "oracle.log"}

// RoundLogName is where RetainRoundLogs puts a round's log, relative to
// the run's directory, slash-separated.
func RoundLogName(round int, log string) string {
	return path.Join(RoundLogsDirName, fmt.Sprintf("round-%d", round), log)
}

// A round folder as build_app.py names it: no leading zero, so the number
// read from the name names the same folder again.
var roundLogDir = regexp.MustCompile(`^round-([1-9][0-9]{0,2})$`)

const (
	// maxRetainedRounds bounds how many rounds' logs are copied; a build
	// runs a handful (-build-app-max-rounds).
	maxRetainedRounds = 20
	// maxFeedbackEntries bounds how much of the feedback folder's listing
	// is read; build_app.py puts one folder per round there.
	maxFeedbackEntries = 4096
)

// maxRetainedRoundLogBytes bounds the whole copy. One file is already
// bounded by RetainFile's own limit. A variable so a test can lower it.
var maxRetainedRoundLogBytes int64 = 64 << 20

// RetainRoundLogs copies the round logs a build left in workspace into
// dstDir (a run's RoundLogsDirName directory, or a folder under it) and
// returns how many files it copied. A workspace with none is not an error.
//
// The source is hostile, like RetainFile's. Every source path is opened
// through an os.Root on the workspace, so no symlink in it, whenever it was
// planted, can make this read a file outside the workspace; each file is
// then opened and checked as RetainFile opens its own (no symlink, a
// regular file, size-bounded). Only the fixed log names in folders named
// round-<n> are looked at. The latest rounds are copied first, and the copy
// stops at maxRetainedRounds rounds or maxRetainedRoundLogBytes, so what a
// cap drops is the oldest. A file that cannot be copied is skipped and
// reported in the returned error; the others are still copied.
func RetainRoundLogs(workspace, dstDir string) (int, error) {
	root, err := os.OpenRoot(workspace)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	defer root.Close()
	rounds, err := roundLogRounds(root)
	if err != nil || len(rounds) == 0 {
		return 0, err
	}
	var (
		copied, withLogs int
		total            int64
		errs             []error
	)
	for i := len(rounds) - 1; i >= 0 && withLogs < maxRetainedRounds; i-- {
		name := fmt.Sprintf("round-%d", rounds[i])
		before := copied
		for _, log := range roundLogNames {
			src := path.Join(roundLogsSource, name, log)
			size, err := retainFromRoot(root, src, filepath.Join(dstDir, name, log))
			if err != nil {
				if !os.IsNotExist(err) {
					errs = append(errs, fmt.Errorf("%s/%s: %w", name, log, err))
				}
				continue
			}
			copied++
			total += size
			if total > maxRetainedRoundLogBytes {
				errs = append(errs, fmt.Errorf("stopped at %s/%s: more than %d bytes of round logs; earlier rounds were not copied", name, log, maxRetainedRoundLogBytes))
				return copied, errors.Join(errs...)
			}
		}
		if copied > before {
			withLogs++
		}
	}
	return copied, errors.Join(errs...)
}

// roundLogRounds lists the round numbers that have a folder in the
// workspace's feedback folder, ascending. No feedback folder is no rounds.
func roundLogRounds(root *os.Root) ([]int, error) {
	dir, err := root.OpenFile(roundLogsSource, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_DIRECTORY, 0)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("open %s: %w", roundLogsSource, err)
	}
	defer dir.Close()
	entries, err := dir.ReadDir(maxFeedbackEntries)
	if err != nil && err != io.EOF {
		return nil, fmt.Errorf("list %s: %w", roundLogsSource, err)
	}
	var rounds []int
	for _, entry := range entries {
		// entry.IsDir is false for a symlink to a directory.
		if match := roundLogDir.FindStringSubmatch(entry.Name()); match != nil && entry.IsDir() {
			n, _ := strconv.Atoi(match[1])
			rounds = append(rounds, n)
		}
	}
	sort.Ints(rounds)
	return rounds, nil
}

// retainFromRoot is RetainFile for a source named relative to root, and
// returns the bytes it copied.
func retainFromRoot(root *os.Root, src, dst string) (int64, error) {
	opened, err := root.OpenFile(src, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return 0, err
	}
	in, err := checkHostileRegularFile(opened, src, maxRetainFileSize)
	if err != nil {
		return 0, err
	}
	defer in.Close()
	if err := retainOpened(in, src, dst); err != nil {
		return 0, err
	}
	info, err := os.Stat(dst)
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}

// ReadRetainedRoundLog returns the name, relative to runDir, and the first
// n bytes of the output RetainRoundLogs kept for a round: the first of the
// three logs that exists, in the order build_app.py's checks run. No log,
// or one that cannot be read, is two zero values.
//
// The content is a copy of what an untrusted build wrote, so it is data to
// show, never to act on. The path is opened through an os.Root on runDir
// with no symlink followed at its end, so nothing outside the run's own
// directory is read even if a link was planted in it.
func ReadRetainedRoundLog(runDir string, round int, n int64) (name string, data []byte) {
	if round < 1 {
		return "", nil
	}
	root, err := os.OpenRoot(runDir)
	if err != nil {
		return "", nil
	}
	defer root.Close()
	for _, log := range roundLogNames {
		name := RoundLogName(round, log)
		opened, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
		if err != nil {
			continue
		}
		info, err := opened.Stat()
		if err != nil || !info.Mode().IsRegular() {
			opened.Close()
			continue
		}
		data, err := io.ReadAll(io.LimitReader(opened, n))
		opened.Close()
		if err == nil {
			return name, data
		}
	}
	return "", nil
}

// AgentNotesFileName is where RetainAgentNotes puts the notes a build that
// ended without passing wrote for whoever attempts the ticket next, in the
// run's own directory (the one holding RoundLogsDirName). The text is the
// build agent's own: untrusted. Only the handoff reads it (SC-018).
const AgentNotesFileName = "agent-notes.md"

// agentNotesSource is where build_app.py leaves the notes, relative to the
// workspace: inside the harness session folder, which is removed from the
// worktree when the build step returns.
const agentNotesSource = ".pi-build-session/handoff-notes.md"

// maxAgentNotesBytes bounds the notes file; build_app.py cuts the reply to
// 6000 characters, so a larger file was not written by it.
const maxAgentNotesBytes = 16 << 10

// RetainAgentNotes copies the build's notes out of workspace to dstPath
// (0600) and reports whether it did. No notes file is (false, nil). The
// source is hostile, as RetainRoundLogs': it is opened through an os.Root on
// the workspace with no symlink followed, and must be a regular file of at
// most maxAgentNotesBytes; a larger one retains nothing and is an error.
func RetainAgentNotes(workspace, dstPath string) (bool, error) {
	root, err := os.OpenRoot(workspace)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	defer root.Close()
	opened, err := root.OpenFile(agentNotesSource, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	in, err := checkHostileRegularFile(opened, agentNotesSource, maxAgentNotesBytes)
	if err != nil {
		return false, err
	}
	defer in.Close()
	if err := retainOpened(in, agentNotesSource, dstPath); err != nil {
		return false, err
	}
	return true, nil
}

// ReadRetainedAgentNotes returns the notes RetainAgentNotes kept in runDir.
// The text is untrusted. It is opened through an os.Root on runDir with no
// symlink followed; nothing readable is ("", false).
func ReadRetainedAgentNotes(runDir string) (string, bool) {
	root, err := os.OpenRoot(runDir)
	if err != nil {
		return "", false
	}
	defer root.Close()
	opened, err := root.OpenFile(AgentNotesFileName, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return "", false
	}
	defer opened.Close()
	info, err := opened.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxAgentNotesBytes {
		return "", false
	}
	data, err := io.ReadAll(io.LimitReader(opened, maxAgentNotesBytes))
	if err != nil {
		return "", false
	}
	return string(data), true
}
