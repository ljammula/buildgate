package evidence

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

func writeRoundLog(t *testing.T, workspace, round, name, content string) {
	t.Helper()
	dir := filepath.Join(workspace, ".pi-build-session", "feedback", round)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestRetainRoundLogsCopiesEachRoundsLogs(t *testing.T) {
	workspace, dst := t.TempDir(), filepath.Join(t.TempDir(), RoundLogsDirName)
	writeRoundLog(t, workspace, "round-1", "verify.log", "round one verify\n")
	writeRoundLog(t, workspace, "round-2", "fast-check.log", "round two fast check\n")
	writeRoundLog(t, workspace, "round-2", "oracle.log", "round two oracle\n")
	// Not a round log: another name, and a folder that is not a round.
	writeRoundLog(t, workspace, "round-2", "notes.txt", "agent scratch\n")
	writeRoundLog(t, workspace, "scratch", "verify.log", "not a round\n")
	writeRoundLog(t, workspace, "round-x", "verify.log", "not a round\n")

	copied, err := RetainRoundLogs(workspace, dst)
	if err != nil || copied != 3 {
		t.Fatalf("RetainRoundLogs = %d, %v, want 3 files and no error", copied, err)
	}
	for path, want := range map[string]string{
		"round-1/verify.log":     "round one verify\n",
		"round-2/fast-check.log": "round two fast check\n",
		"round-2/oracle.log":     "round two oracle\n",
	} {
		got, err := os.ReadFile(filepath.Join(dst, path))
		if err != nil || string(got) != want {
			t.Errorf("%s = %q, %v, want %q", path, got, err, want)
		}
	}
	var kept []string
	_ = filepath.WalkDir(dst, func(path string, d os.DirEntry, _ error) error {
		if !d.IsDir() {
			kept = append(kept, path)
		}
		return nil
	})
	if len(kept) != 3 {
		t.Errorf("retained %v, want only the three round logs", kept)
	}
}

func TestRetainRoundLogsWithNothingToCopyIsNotAnError(t *testing.T) {
	for name, prepare := range map[string]func(workspace string){
		"no session folder":  func(string) {},
		"no feedback folder": func(w string) { _ = os.MkdirAll(filepath.Join(w, ".pi-build-session"), 0o755) },
		"no round folder":    func(w string) { _ = os.MkdirAll(filepath.Join(w, ".pi-build-session", "feedback"), 0o755) },
	} {
		workspace, dst := t.TempDir(), filepath.Join(t.TempDir(), RoundLogsDirName)
		prepare(workspace)
		if copied, err := RetainRoundLogs(workspace, dst); copied != 0 || err != nil {
			t.Errorf("%s: RetainRoundLogs = %d, %v, want 0 and no error", name, copied, err)
		}
		if _, err := os.Stat(dst); !os.IsNotExist(err) {
			t.Errorf("%s: the destination was created for nothing", name)
		}
	}
}

// The folder is the build agent's to write: nothing outside it may be
// copied through a link, and a pipe must not hang the copy.
func TestRetainRoundLogsRefusesLinksAndSpecialFiles(t *testing.T) {
	outside := t.TempDir()
	secret := filepath.Join(outside, "verify.log")
	if err := os.WriteFile(secret, []byte("host secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	assertNothing := func(t *testing.T, dst string) {
		t.Helper()
		_ = filepath.WalkDir(dst, func(path string, d os.DirEntry, err error) error {
			if err == nil && !d.IsDir() {
				t.Errorf("retained %s, want nothing", path)
			}
			return nil
		})
	}

	t.Run("the log is a symlink", func(t *testing.T) {
		workspace, dst := t.TempDir(), filepath.Join(t.TempDir(), RoundLogsDirName)
		writeRoundLog(t, workspace, "round-1", "oracle.log", "real\n")
		if err := os.Symlink(secret, filepath.Join(workspace, ".pi-build-session", "feedback", "round-1", "verify.log")); err != nil {
			t.Fatal(err)
		}
		copied, err := RetainRoundLogs(workspace, dst)
		if copied != 1 || err == nil || !strings.Contains(err.Error(), "round-1/verify.log") {
			t.Fatalf("RetainRoundLogs = %d, %v, want the real log copied and the link named in the error", copied, err)
		}
		if _, statErr := os.Stat(filepath.Join(dst, "round-1", "verify.log")); !os.IsNotExist(statErr) {
			t.Error("the linked file was retained")
		}
	})
	t.Run("the round folder is a symlink", func(t *testing.T) {
		workspace, dst := t.TempDir(), filepath.Join(t.TempDir(), RoundLogsDirName)
		feedback := filepath.Join(workspace, ".pi-build-session", "feedback")
		if err := os.MkdirAll(feedback, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(feedback, "round-1")); err != nil {
			t.Fatal(err)
		}
		if copied, _ := RetainRoundLogs(workspace, dst); copied != 0 {
			t.Errorf("copied %d files through a linked round folder", copied)
		}
		assertNothing(t, dst)
	})
	for _, linked := range []string{".pi-build-session", filepath.Join(".pi-build-session", "feedback")} {
		t.Run(linked+" is a symlink", func(t *testing.T) {
			workspace, dst := t.TempDir(), filepath.Join(t.TempDir(), RoundLogsDirName)
			// A real tree elsewhere that the link points at.
			elsewhere := t.TempDir()
			writeRoundLog(t, elsewhere, "round-1", "verify.log", "elsewhere\n")
			target := filepath.Join(elsewhere, linked)
			if err := os.MkdirAll(filepath.Dir(filepath.Join(workspace, linked)), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, filepath.Join(workspace, linked)); err != nil {
				t.Fatal(err)
			}
			copied, err := RetainRoundLogs(workspace, dst)
			if copied != 0 || err == nil {
				t.Errorf("RetainRoundLogs = %d, %v, want nothing copied and an error", copied, err)
			}
			assertNothing(t, dst)
		})
	}
	t.Run("the log is a named pipe", func(t *testing.T) {
		workspace, dst := t.TempDir(), filepath.Join(t.TempDir(), RoundLogsDirName)
		writeRoundLog(t, workspace, "round-1", "oracle.log", "real\n")
		if err := syscall.Mkfifo(filepath.Join(workspace, ".pi-build-session", "feedback", "round-1", "verify.log"), 0o644); err != nil {
			t.Skipf("mkfifo: %v", err)
		}
		copied, err := RetainRoundLogs(workspace, dst)
		if copied != 1 || err == nil {
			t.Errorf("RetainRoundLogs = %d, %v, want the real log copied and the pipe refused", copied, err)
		}
	})
}

// A folder the agent names like a round but that build_app.py would never
// write is not a round: a zero-padded name, and empty folders that would
// otherwise use up the round cap.
func TestRetainRoundLogsIgnoresPaddedNamesAndEmptyRoundFolders(t *testing.T) {
	workspace, dst := t.TempDir(), filepath.Join(t.TempDir(), RoundLogsDirName)
	writeRoundLog(t, workspace, "round-2", "verify.log", "real\n")
	writeRoundLog(t, workspace, "round-007", "verify.log", "padded\n")
	for n := 900; n < 900+maxRetainedRounds+5; n++ {
		if err := os.MkdirAll(filepath.Join(workspace, ".pi-build-session", "feedback", "round-"+strconv.Itoa(n)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	copied, err := RetainRoundLogs(workspace, dst)
	if copied != 1 || err != nil {
		t.Fatalf("RetainRoundLogs = %d, %v, want the one real log", copied, err)
	}
	if got, err := os.ReadFile(filepath.Join(dst, "round-2", "verify.log")); err != nil || string(got) != "real\n" {
		t.Errorf("round-2/verify.log = %q, %v", got, err)
	}
	for _, unwanted := range []string{"round-7", "round-007"} {
		if _, err := os.Stat(filepath.Join(dst, unwanted)); !os.IsNotExist(err) {
			t.Errorf("%s was retained", unwanted)
		}
	}
}

func TestRetainRoundLogsStopsAtTheTotalSizeKeepingTheLatestRounds(t *testing.T) {
	old := maxRetainedRoundLogBytes
	maxRetainedRoundLogBytes = 25
	t.Cleanup(func() { maxRetainedRoundLogBytes = old })
	workspace, dst := t.TempDir(), filepath.Join(t.TempDir(), RoundLogsDirName)
	for n := 1; n <= 4; n++ {
		writeRoundLog(t, workspace, "round-"+strconv.Itoa(n), "verify.log", "ten bytes\n")
	}
	copied, err := RetainRoundLogs(workspace, dst)
	if copied != 3 || err == nil || !strings.Contains(err.Error(), "earlier rounds were not copied") {
		t.Fatalf("RetainRoundLogs = %d, %v, want 3 files and the cap named", copied, err)
	}
	if _, err := os.Stat(filepath.Join(dst, "round-4", "verify.log")); err != nil {
		t.Errorf("the latest round was not retained: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dst, "round-1")); !os.IsNotExist(err) {
		t.Error("the oldest round was retained past the size cap")
	}
}

func TestRetainRoundLogsKeepsTheLatestRoundsWhenThereAreTooMany(t *testing.T) {
	workspace, dst := t.TempDir(), filepath.Join(t.TempDir(), RoundLogsDirName)
	for n := 1; n <= maxRetainedRounds+5; n++ {
		writeRoundLog(t, workspace, "round-"+strconv.Itoa(n), "verify.log", "x\n")
	}
	copied, err := RetainRoundLogs(workspace, dst)
	if copied != maxRetainedRounds || err != nil {
		t.Fatalf("RetainRoundLogs = %d, %v, want %d", copied, err, maxRetainedRounds)
	}
	if _, err := os.Stat(filepath.Join(dst, "round-5", "verify.log")); !os.IsNotExist(err) {
		t.Error("round 5 was retained, want only the latest rounds")
	}
	if _, err := os.Stat(filepath.Join(dst, "round-"+strconv.Itoa(maxRetainedRounds+5), "verify.log")); err != nil {
		t.Errorf("the last round was not retained: %v", err)
	}
}

func TestReadRetainedRoundLogReadsTheStartOfTheFirstLogThatExists(t *testing.T) {
	runDir := t.TempDir()
	write := func(rel, content string) {
		t.Helper()
		path := filepath.Join(runDir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("round-logs/round-2/oracle.log", "oracle output")
	write("round-logs/round-2/verify.log", "verify output, a long one")
	name, data := ReadRetainedRoundLog(runDir, 2, 13)
	if name != "round-logs/round-2/verify.log" || string(data) != "verify output" {
		t.Errorf("round 2 = %q, %q, want the first 13 bytes of verify.log", name, data)
	}
	for _, round := range []int{0, -1, 3} {
		if name, data := ReadRetainedRoundLog(runDir, round, 100); name != "" || data != nil {
			t.Errorf("round %d = %q, %q, want nothing", round, name, data)
		}
	}
	if name, _ := ReadRetainedRoundLog(filepath.Join(runDir, "missing"), 2, 100); name != "" {
		t.Errorf("a run directory that does not exist gave %q", name)
	}
}

// Nothing outside the run's own directory is read, whichever part of the
// path was replaced by a link.
func TestReadRetainedRoundLogRefusesLinksOutOfTheRunDirectory(t *testing.T) {
	outside := t.TempDir()
	if err := os.MkdirAll(filepath.Join(outside, "round-1"), 0o750); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{filepath.Join(outside, "verify.log"), filepath.Join(outside, "round-1", "verify.log")} {
		if err := os.WriteFile(path, []byte("host secret"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cases := map[string]func(runDir string) error{
		"round-logs is a link": func(runDir string) error { return os.Symlink(outside, filepath.Join(runDir, "round-logs")) },
		"the round folder is a link": func(runDir string) error {
			if err := os.MkdirAll(filepath.Join(runDir, "round-logs"), 0o750); err != nil {
				return err
			}
			return os.Symlink(filepath.Join(outside, "round-1"), filepath.Join(runDir, "round-logs", "round-1"))
		},
		"the log is a link": func(runDir string) error {
			if err := os.MkdirAll(filepath.Join(runDir, "round-logs", "round-1"), 0o750); err != nil {
				return err
			}
			return os.Symlink(filepath.Join(outside, "verify.log"), filepath.Join(runDir, "round-logs", "round-1", "verify.log"))
		},
	}
	for name, plant := range cases {
		runDir := t.TempDir()
		if err := plant(runDir); err != nil {
			t.Fatal(err)
		}
		if got, data := ReadRetainedRoundLog(runDir, 1, 100); got != "" || data != nil {
			t.Errorf("%s: read %q, %q, want nothing", name, got, data)
		}
	}
}

// A round that passed leaves no failing command's log, only what its setup:
// and autofix: commands printed. That output is retained too, so the
// operator can read what they did on the round that was accepted.
func TestRetainRoundLogsKeepsSetupAndAutofixOutputOfARoundThatPassed(t *testing.T) {
	workspace, dst := t.TempDir(), filepath.Join(t.TempDir(), RoundLogsDirName)
	writeRoundLog(t, workspace, "round-1", "setup.log", "round one setup\n")
	writeRoundLog(t, workspace, "round-1", "verify.log", "round one verify\n")
	writeRoundLog(t, workspace, "round-2", "setup.log", "round two setup\n")
	writeRoundLog(t, workspace, "round-2", "autofix.log", "round two autofix\n")

	copied, err := RetainRoundLogs(workspace, dst)
	if err != nil || copied != 4 {
		t.Fatalf("RetainRoundLogs = %d, %v, want 4 files and no error", copied, err)
	}
	for path, want := range map[string]string{
		"round-1/setup.log":   "round one setup\n",
		"round-1/verify.log":  "round one verify\n",
		"round-2/setup.log":   "round two setup\n",
		"round-2/autofix.log": "round two autofix\n",
	} {
		got, err := os.ReadFile(filepath.Join(dst, path))
		if err != nil || string(got) != want {
			t.Errorf("%s = %q, %v, want %q", path, got, err, want)
		}
	}
	// The round's failing output is still the failing command's log, never
	// the setup or autofix output kept beside it.
	if name, _ := ReadRetainedRoundLog(filepath.Dir(dst), 1, 100); name != RoundLogName(1, "verify.log") {
		t.Errorf("round 1's failing log = %q, want its verify.log", name)
	}
	if name, data := ReadRetainedRoundLog(filepath.Dir(dst), 2, 100); name != "" || data != nil {
		t.Errorf("round 2 passed, but its failing log = %q, %q", name, data)
	}
}
