package evidence

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"

	"buildgate/internal/sanitize"
)

// PromptsDirName is the directory, inside a run's own directory or a
// request's, that holds the prompts the factory composed and handed to a
// coding-agent harness, as the launch's script saved them:
// <dir>/prompts/<attempt>/<name>.md, the attempt folder being "<kind>-<n>"
// (build-1, code_review-1, spec-2).
//
// A saved prompt is what the launch's session folder held when the host
// copied it. The script and the coding agent run as one user in one sandbox,
// and everything the script could tell the host with (its output file, which
// sits in a directory the sandbox can write, its evidence file, the session
// folder) the agent can write too: a build can alter, add or hide its own
// saved prompts before the copy. The host removes whatever a session's
// prompts folder holds before each launch (DropSavedPrompts), so a file left
// by an earlier step is never copied as a later launch's prompt.
//
// The text is for the operator only (SC-018): it quotes the ticket, the
// earlier attempt's record and failing output, and a corrective round's
// instructions. Nothing but the operator routes and `factoryd logs` reads it.
const PromptsDirName = "prompts"

// promptsSessionSubdir is where a script saves its prompts, relative to the
// harness session folder it names (agent/pi/scripts/saved_prompts.py).
const promptsSessionSubdir = "prompts"

const (
	// MaxSavedPrompts bounds the prompts copied out of one launch, as the
	// script bounds the prompts it saves.
	MaxSavedPrompts = 50
	// maxSavedPromptBytes is saved_prompts.py's 2 MiB cap plus room for the
	// one line that says how much was cut. A larger file was not written by
	// the script.
	maxSavedPromptBytes = 2<<20 + 1<<10
	// maxPromptSessionEntries bounds how much of a prompts folder's listing
	// is read.
	maxPromptSessionEntries = 4096
)

var (
	// A file the script saves: save_prompt's NAME_RE plus ".md".
	promptFileName = regexp.MustCompile(`^[a-z0-9-]{1,64}\.md$`)
	promptName     = regexp.MustCompile(`^[a-z0-9-]{1,64}$`)
	// An attempt folder: the kind of launch, a dash and its number.
	promptAttempt = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}-[0-9]{1,6}$`)
)

// PromptAttemptDir is the folder, under PromptsDirName, a launch of kind
// (build, spec_conformity, code_review, review, spec, plan, oracle) with the
// given number keeps its prompts in.
func PromptAttemptDir(kind string, n int) string {
	return fmt.Sprintf("%s-%d", kind, n)
}

// RetainPrompts copies the prompts a launch saved in each session folder
// (relative to workspace, such as ".pi-build-session") into dstDir, a
// <dir>/prompts/<attempt> folder, and returns how many it copied. The text
// goes through sanitize.Text on the way, as the build log's text does: the
// prompt quotes failing output and file content, which can hold a credential.
// The sessions' prompts folders stay where they are: DropSavedPrompts
// removes them.
//
// The source is hostile. Every path is opened through an os.Root on the
// workspace, so no link in it can lead outside the workspace; the prompts
// folder must be a real directory; a file is copied only when it is a regular
// file (no link, FIFO or device), named like save_prompt names it, and no
// larger than the script's cap. At most MaxSavedPrompts are copied; a file
// that is refused is reported in the error and the others are still copied.
// A name already in dstDir (a relaunch of the same script) gets a numeric
// suffix, never overwriting.
func RetainPrompts(workspace string, sessions []string, dstDir string) (int, error) {
	root, err := os.OpenRoot(workspace)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	defer root.Close()
	var (
		copied int
		errs   []error
	)
	for _, session := range sessions {
		src := filepath.ToSlash(filepath.Join(session, promptsSessionSubdir))
		n, err := retainSessionPrompts(root, src, dstDir, MaxSavedPrompts-copied)
		copied += n
		errs = append(errs, err)
	}
	return copied, errors.Join(errs...)
}

// DropSavedPrompts removes the prompts folder of each session folder (relative
// to workspace) after RetainPrompts, or before a launch, so a launch never
// finds a prompt it did not save and a prompt planted there is not taken for
// one. The removal goes through an os.Root on the workspace: a link is
// removed, never followed. A folder left read-only is made removable first
// (the session folder and every directory under its prompts folder get their
// owner's write permission). An absent folder is not an error; a prompts
// path that is still there afterwards, or cannot be looked at, is.
func DropSavedPrompts(workspace string, sessions []string) error {
	root, err := os.OpenRoot(workspace)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer root.Close()
	var errs []error
	for _, session := range sessions {
		src := filepath.ToSlash(filepath.Join(session, promptsSessionSubdir))
		if err := dropPromptsFolder(root, filepath.ToSlash(session), src); err != nil {
			errs = append(errs, fmt.Errorf("remove %s: %w", src, err))
		}
	}
	return errors.Join(errs...)
}

// dropPromptsFolder removes src, the prompts folder of session, and reports
// anything short of src being gone.
func dropPromptsFolder(root *os.Root, session, src string) error {
	if err := root.RemoveAll(src); err != nil && !os.IsNotExist(err) {
		makeRemovable(root, session, src)
		if err := root.RemoveAll(src); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	if _, err := root.Lstat(src); err == nil {
		return errors.New("still there after its removal")
	} else if !os.IsNotExist(err) {
		return err
	}
	return nil
}

// makeRemovable gives session, when it is a real directory, and every real
// directory under src its owner's write permission. Nothing is followed out
// of root, and a link is never changed.
func makeRemovable(root *os.Root, session, src string) {
	if info, err := root.Lstat(session); err == nil && info.IsDir() {
		_ = root.Chmod(session, 0o700)
	}
	_ = fs.WalkDir(root.FS(), src, func(p string, d fs.DirEntry, err error) error {
		if d != nil && d.IsDir() {
			_ = root.Chmod(p, 0o700)
		}
		return nil
	})
}

// retainSessionPrompts copies the prompts in the folder src of root, up to
// room of them.
func retainSessionPrompts(root *os.Root, src, dstDir string, room int) (int, error) {
	info, err := root.Lstat(src)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	if !info.IsDir() {
		return 0, fmt.Errorf("%s is not a directory (mode %s)", src, info.Mode())
	}
	dir, err := root.OpenFile(src, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_DIRECTORY, 0)
	if err != nil {
		return 0, fmt.Errorf("open %s: %w", src, err)
	}
	defer dir.Close()
	entries, err := dir.ReadDir(maxPromptSessionEntries)
	if err != nil && err != io.EOF {
		return 0, fmt.Errorf("list %s: %w", src, err)
	}
	var names []string
	for _, entry := range entries {
		if promptFileName.MatchString(entry.Name()) && entry.Type().IsRegular() {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	var (
		copied int
		errs   []error
	)
	if len(names) > room {
		errs = append(errs, fmt.Errorf("%s holds %d prompts; only %d were copied", src, len(names), max(room, 0)))
		names = names[:max(room, 0)]
	}
	for _, name := range names {
		if err := retainPrompt(root, src+"/"+name, dstDir, name); err != nil {
			errs = append(errs, fmt.Errorf("%s/%s: %w", src, name, err))
			continue
		}
		copied++
	}
	return copied, errors.Join(errs...)
}

// retainPrompt reads one saved prompt through root, redacts it, and writes
// it into dstDir under name (or the first free numbered name).
func retainPrompt(root *os.Root, src, dstDir, name string) error {
	opened, err := root.OpenFile(src, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	in, err := checkHostileRegularFile(opened, src, maxSavedPromptBytes)
	if err != nil {
		return err
	}
	defer in.Close()
	data, err := io.ReadAll(io.LimitReader(in, maxSavedPromptBytes+1))
	if err != nil {
		return err
	}
	if int64(len(data)) > maxSavedPromptBytes {
		return fmt.Errorf("%s grew past the %d byte limit while being read", src, int64(maxSavedPromptBytes))
	}
	if err := os.MkdirAll(dstDir, 0o750); err != nil {
		return err
	}
	return writeFreePromptFile(dstDir, name, []byte(sanitize.Text(string(data))))
}

// writeFreePromptFile writes data to dstDir/name, or to name with a "-2",
// "-3" ... before ".md" when that exists, through a temp file renamed into
// place (a hard link, so a name taken meanwhile is never replaced).
func writeFreePromptFile(dstDir, name string, data []byte) error {
	tmp, err := os.CreateTemp(dstDir, ".retain-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	stem := strings.TrimSuffix(name, ".md")
	for n := 1; n < 1000; n++ {
		candidate := name
		if n > 1 {
			suffix := fmt.Sprintf("-%d", n)
			candidate = stem[:min(len(stem), 64-len(suffix))] + suffix + ".md"
		}
		err := os.Link(tmp.Name(), filepath.Join(dstDir, candidate))
		if err == nil {
			return nil
		}
		if !os.IsExist(err) {
			return err
		}
	}
	return fmt.Errorf("no free name for %s in %s", name, dstDir)
}

// SavedPrompt is one prompt kept under a run's or a request's directory.
type SavedPrompt struct {
	Attempt  string    `json:"attempt"`
	Name     string    `json:"name"`
	Bytes    int64     `json:"bytes"`
	Modified time.Time `json:"time"`
}

// ListSavedPrompts returns the prompts RetainPrompts kept under dir (a run
// directory or a request directory), oldest first, ties by attempt and name.
// Nothing readable is an empty list. Every path is opened through an os.Root
// on dir with no link followed at its end, so nothing outside dir is read.
func ListSavedPrompts(dir string) []SavedPrompt {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil
	}
	defer root.Close()
	attempts, err := readRootDir(root, PromptsDirName)
	if err != nil {
		return nil
	}
	var out []SavedPrompt
	for _, a := range attempts {
		if !a.IsDir() || !promptAttempt.MatchString(a.Name()) {
			continue
		}
		files, err := readRootDir(root, PromptsDirName+"/"+a.Name())
		if err != nil {
			continue
		}
		for _, f := range files {
			if !f.Type().IsRegular() || !promptFileName.MatchString(f.Name()) {
				continue
			}
			info, err := f.Info()
			if err != nil || !info.Mode().IsRegular() {
				continue
			}
			out = append(out, SavedPrompt{Attempt: a.Name(), Name: strings.TrimSuffix(f.Name(), ".md"), Bytes: info.Size(), Modified: info.ModTime()})
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].Modified.Equal(out[j].Modified) {
			return out[i].Modified.Before(out[j].Modified)
		}
		if out[i].Attempt != out[j].Attempt {
			return out[i].Attempt < out[j].Attempt
		}
		return out[i].Name < out[j].Name
	})
	return out
}

func readRootDir(root *os.Root, rel string) ([]os.DirEntry, error) {
	dir, err := root.OpenFile(rel, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_DIRECTORY, 0)
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	entries, err := dir.ReadDir(maxPromptSessionEntries)
	if err != nil && err != io.EOF {
		return nil, err
	}
	return entries, nil
}

// ReadSavedPrompt returns the saved prompt attempt/name under dir. An attempt
// or name that is not of the shape RetainPrompts writes (a "..", a slash, a
// dot, a NUL) is refused before any path is built. Nothing readable is
// (nil, false).
func ReadSavedPrompt(dir, attempt, name string) ([]byte, bool) {
	if !promptAttempt.MatchString(attempt) || !promptName.MatchString(name) {
		return nil, false
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, false
	}
	defer root.Close()
	opened, err := root.OpenFile(PromptsDirName+"/"+attempt+"/"+name+".md", os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, false
	}
	defer opened.Close()
	info, err := opened.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxSavedPromptBytes {
		return nil, false
	}
	data, err := io.ReadAll(io.LimitReader(opened, maxSavedPromptBytes))
	if err != nil {
		return nil, false
	}
	return data, true
}

// NextPromptAttempt is the number the next launch of kind takes under dir's
// prompts folder: one more than the highest "<kind>-<n>" there. A drafting
// job, which has no attempt number of its own, names its folder with it.
func NextPromptAttempt(dir, kind string) int {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return 1
	}
	defer root.Close()
	entries, err := readRootDir(root, PromptsDirName)
	if err != nil {
		return 1
	}
	highest := 0
	for _, e := range entries {
		var n int
		if _, err := fmt.Sscanf(strings.TrimPrefix(e.Name(), kind+"-"), "%d", &n); err == nil && strings.HasPrefix(e.Name(), kind+"-") && n > highest {
			highest = n
		}
	}
	return highest + 1
}
