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
