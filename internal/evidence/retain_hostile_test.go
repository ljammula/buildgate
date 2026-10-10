package evidence

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// hostileSource is one shape a copy must refuse at its source: make puts it
// at path, a link pointing at the host file outside.
type hostileSource struct {
	name string
	make func(t *testing.T, path, outside string)
}

func hostileSources() []hostileSource {
	return []hostileSource{
		{name: "a symlink", make: func(t *testing.T, path, outside string) {
			if err := os.Symlink(outside, path); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "a directory", make: func(t *testing.T, path, _ string) {
			if err := os.Mkdir(path, 0o755); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "a named pipe", make: func(t *testing.T, path, _ string) {
			if err := syscall.Mkfifo(path, 0o644); err != nil {
				t.Skipf("mkfifo: %v", err)
			}
		}},
	}
}

// sparseFile makes a regular file of size bytes that takes no room on disk.
func sparseFile(t *testing.T, path string, size int64) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := f.Truncate(size); err != nil {
		t.Fatal(err)
	}
}

func hostFile(t *testing.T) string {
	t.Helper()
	outside := filepath.Join(t.TempDir(), "host-file")
	if err := os.WriteFile(outside, []byte("HOST SECRET"), 0o600); err != nil {
		t.Fatal(err)
	}
	return outside
}

// linkInPlaceOf puts, where a copy will be written, a link to a host file,
// and returns a check that the host file was not written through it.
func linkInPlaceOf(t *testing.T, dst string) (hostUntouched func()) {
	t.Helper()
	outside := hostFile(t)
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, dst); err != nil {
		t.Fatal(err)
	}
	return func() {
		t.Helper()
		if got, err := os.ReadFile(outside); err != nil || string(got) != "HOST SECRET" {
			t.Errorf("the file the destination linked to = %q, %v, want it untouched", got, err)
		}
	}
}

func assertRegularCopy(t *testing.T, path, want string) {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		t.Fatalf("copy %s = %v, %v, want a regular 0600 file", path, info, err)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != want {
		t.Errorf("copy %s = %q, %v, want %q", path, got, err, want)
	}
}

// writeAgentNotes puts text where build_app.py leaves the notes in ws and
// returns the path; an empty text makes only the session folder.
func writeAgentNotes(t *testing.T, ws, text string) string {
	t.Helper()
	src := filepath.Join(ws, ".pi-build-session", "handoff-notes.md")
	if err := os.MkdirAll(filepath.Dir(src), 0o755); err != nil {
		t.Fatal(err)
	}
	if text != "" {
		if err := os.WriteFile(src, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return src
}

// earlierCopy is a destination that already holds an earlier attempt's copy.
func earlierCopy(t *testing.T, name string) string {
	t.Helper()
	dst := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(dst, []byte("an earlier attempt"), 0o600); err != nil {
		t.Fatal(err)
	}
	return dst
}

func assertGone(t *testing.T, dst string) {
	t.Helper()
	if _, err := os.Lstat(dst); !os.IsNotExist(err) {
		t.Errorf("the earlier copy at %s is still there (%v)", dst, err)
	}
}

func TestRetainAgentNotesCopiesOnlyAPlainFileOfTheNotesSize(t *testing.T) {
	t.Run("a plain file", func(t *testing.T) {
		ws, dst := t.TempDir(), filepath.Join(t.TempDir(), "run", AgentNotesFileName)
		writeAgentNotes(t, ws, "What I did\n- a `token=abc` thing\n")
		if ok, err := RetainAgentNotes(ws, dst); !ok || err != nil {
			t.Fatalf("RetainAgentNotes = %v, %v, want true, nil", ok, err)
		}
		// A byte copy: the notes are not redacted here.
		assertRegularCopy(t, dst, "What I did\n- a `token=abc` thing\n")
		if m, _ := filepath.Glob(filepath.Join(filepath.Dir(dst), ".retain-*")); len(m) != 0 {
			t.Errorf("temp files left: %v", m)
		}
	})
	t.Run("a file of exactly the cap", func(t *testing.T) {
		ws, dst := t.TempDir(), earlierCopy(t, AgentNotesFileName)
		sparseFile(t, writeAgentNotes(t, ws, ""), maxAgentNotesBytes)
		if ok, err := RetainAgentNotes(ws, dst); !ok || err != nil {
			t.Fatalf("RetainAgentNotes = %v, %v, want true, nil", ok, err)
		}
		if info, err := os.Stat(dst); err != nil || info.Size() != maxAgentNotesBytes {
			t.Errorf("copy = %v, %v", info, err)
		}
	})
	t.Run("a file over the cap", func(t *testing.T) {
		ws, dst := t.TempDir(), earlierCopy(t, AgentNotesFileName)
		sparseFile(t, writeAgentNotes(t, ws, ""), maxAgentNotesBytes+1)
		if ok, err := RetainAgentNotes(ws, dst); ok || err == nil || !strings.Contains(err.Error(), "exceeding") {
			t.Fatalf("RetainAgentNotes = %v, %v, want false and the size refused", ok, err)
		}
		assertGone(t, dst)
	})
}

func TestRetainAgentNotesRefusesHostileShapesAndNeverWritesThroughALink(t *testing.T) {
	for _, shape := range hostileSources() {
		t.Run(shape.name, func(t *testing.T) {
			ws, dst := t.TempDir(), earlierCopy(t, AgentNotesFileName)
			shape.make(t, writeAgentNotes(t, ws, ""), hostFile(t))
			if ok, err := RetainAgentNotes(ws, dst); ok || err == nil {
				t.Fatalf("RetainAgentNotes = %v, %v, want false and an error", ok, err)
			}
			assertGone(t, dst)
		})
	}
	t.Run("the session folder is a symlink", func(t *testing.T) {
		ws, dst, elsewhere := t.TempDir(), earlierCopy(t, AgentNotesFileName), t.TempDir()
		writeAgentNotes(t, elsewhere, "elsewhere\n")
		if err := os.Symlink(filepath.Join(elsewhere, ".pi-build-session"), filepath.Join(ws, ".pi-build-session")); err != nil {
			t.Fatal(err)
		}
		if ok, err := RetainAgentNotes(ws, dst); ok || err == nil {
			t.Fatalf("RetainAgentNotes = %v, %v, want false and an error", ok, err)
		}
		assertGone(t, dst)
	})
	t.Run("no notes, no session, no workspace", func(t *testing.T) {
		ws := t.TempDir()
		for _, workspace := range []string{ws, filepath.Join(ws, "absent")} {
			dst := earlierCopy(t, AgentNotesFileName)
			if ok, err := RetainAgentNotes(workspace, dst); ok || err != nil {
				t.Fatalf("RetainAgentNotes(%s) = %v, %v, want false, nil", workspace, ok, err)
			}
			assertGone(t, dst)
		}
		writeAgentNotes(t, ws, "")
		dst := earlierCopy(t, AgentNotesFileName)
		if ok, err := RetainAgentNotes(ws, dst); ok || err != nil {
			t.Fatalf("an empty session = %v, %v, want false, nil", ok, err)
		}
		assertGone(t, dst)
	})
	t.Run("a link where the copy goes", func(t *testing.T) {
		ws, dst := t.TempDir(), filepath.Join(t.TempDir(), "run", AgentNotesFileName)
		writeAgentNotes(t, ws, "notes\n")
		hostUntouched := linkInPlaceOf(t, dst)
		if ok, err := RetainAgentNotes(ws, dst); !ok || err != nil {
			t.Fatalf("RetainAgentNotes = %v, %v, want true, nil", ok, err)
		}
		assertRegularCopy(t, dst, "notes\n")
		hostUntouched()
	})
	t.Run("a link where the copy goes and nothing to copy", func(t *testing.T) {
		ws, dst := t.TempDir(), filepath.Join(t.TempDir(), "run", AgentNotesFileName)
		hostUntouched := linkInPlaceOf(t, dst)
		if ok, err := RetainAgentNotes(ws, dst); ok || err != nil {
			t.Fatalf("RetainAgentNotes = %v, %v, want false, nil", ok, err)
		}
		assertGone(t, dst)
		hostUntouched()
	})
}

func TestRetainRoundLogsRefusesADirectoryAnOversizedLogAndALinkedDestination(t *testing.T) {
	t.Run("the log is a directory", func(t *testing.T) {
		workspace, dst := t.TempDir(), filepath.Join(t.TempDir(), RoundLogsDirName)
		writeRoundLog(t, workspace, "round-1", "oracle.log", "real\n")
		if err := os.Mkdir(filepath.Join(workspace, ".pi-build-session", "feedback", "round-1", "verify.log"), 0o755); err != nil {
			t.Fatal(err)
		}
		copied, err := RetainRoundLogs(workspace, dst)
		if copied != 1 || err == nil || !strings.Contains(err.Error(), "round-1/verify.log") || !strings.Contains(err.Error(), "not a regular file") {
			t.Fatalf("RetainRoundLogs = %d, %v, want the real log copied and the directory named", copied, err)
		}
		if _, statErr := os.Lstat(filepath.Join(dst, "round-1", "verify.log")); !os.IsNotExist(statErr) {
			t.Error("something was retained for the directory")
		}
		assertRegularCopy(t, filepath.Join(dst, "round-1", "oracle.log"), "real\n")
	})
	t.Run("the log is over one file's limit", func(t *testing.T) {
		workspace, dst := t.TempDir(), filepath.Join(t.TempDir(), RoundLogsDirName)
		writeRoundLog(t, workspace, "round-1", "oracle.log", "real\n")
		sparseFile(t, filepath.Join(workspace, ".pi-build-session", "feedback", "round-1", "verify.log"), maxRetainFileSize+1)
		copied, err := RetainRoundLogs(workspace, dst)
		if copied != 1 || err == nil || !strings.Contains(err.Error(), "round-1/verify.log") || !strings.Contains(err.Error(), "exceeding") {
			t.Fatalf("RetainRoundLogs = %d, %v, want the real log copied and the size refused", copied, err)
		}
		if _, statErr := os.Lstat(filepath.Join(dst, "round-1", "verify.log")); !os.IsNotExist(statErr) {
			t.Error("the oversized log was retained")
		}
	})
	t.Run("a link where the copy goes", func(t *testing.T) {
		workspace, dst := t.TempDir(), filepath.Join(t.TempDir(), RoundLogsDirName)
		writeRoundLog(t, workspace, "round-1", "verify.log", "real\n")
		hostUntouched := linkInPlaceOf(t, filepath.Join(dst, "round-1", "verify.log"))
		if copied, err := RetainRoundLogs(workspace, dst); copied != 1 || err != nil {
			t.Fatalf("RetainRoundLogs = %d, %v, want 1, nil", copied, err)
		}
		assertRegularCopy(t, filepath.Join(dst, "round-1", "verify.log"), "real\n")
		hostUntouched()
	})
}

func TestRetainBuildEvidenceRefusesSpecialAndOversizedFilesAndReplacesALinkedDestination(t *testing.T) {
	for _, shape := range hostileSources() {
		t.Run(shape.name, func(t *testing.T) {
			workspace, dst := t.TempDir(), earlierCopy(t, BuildEvidenceFileName)
			shape.make(t, filepath.Join(workspace, BuildEvidenceFileName), hostFile(t))
			if kept, err := RetainBuildEvidence(workspace, dst); kept || err == nil {
				t.Fatalf("RetainBuildEvidence = %v, %v, want a refusal", kept, err)
			}
			assertGone(t, dst)
		})
	}
	t.Run("over the limit", func(t *testing.T) {
		workspace, dst := t.TempDir(), earlierCopy(t, BuildEvidenceFileName)
		sparseFile(t, filepath.Join(workspace, BuildEvidenceFileName), maxRetainFileSize+1)
		if kept, err := RetainBuildEvidence(workspace, dst); kept || err == nil || !strings.Contains(err.Error(), "exceeding") {
			t.Fatalf("RetainBuildEvidence = %v, %v, want the size refused", kept, err)
		}
		assertGone(t, dst)
	})
	t.Run("a link where the copy goes", func(t *testing.T) {
		workspace, dst := t.TempDir(), filepath.Join(t.TempDir(), "run", BuildEvidenceFileName)
		if err := os.WriteFile(filepath.Join(workspace, BuildEvidenceFileName), []byte(`{}`), 0o644); err != nil {
			t.Fatal(err)
		}
		hostUntouched := linkInPlaceOf(t, dst)
		if kept, err := RetainBuildEvidence(workspace, dst); !kept || err != nil {
			t.Fatalf("RetainBuildEvidence = %v, %v, want true, nil", kept, err)
		}
		assertRegularCopy(t, dst, `{}`)
		hostUntouched()
	})
}

func TestRetainPromptsSkipsADirectoryAndNeverWritesThroughALinkedDestination(t *testing.T) {
	t.Run("a directory named like a prompt", func(t *testing.T) {
		ws, dst := t.TempDir(), filepath.Join(t.TempDir(), "p")
		writePrompt(t, ws, ".pi-build-session", "ok.md", "fine")
		if err := os.Mkdir(filepath.Join(ws, ".pi-build-session", "prompts", "dir.md"), 0o755); err != nil {
			t.Fatal(err)
		}
		// The listing already says it is no regular file: not copied, and
		// not an error.
		if n, err := RetainPrompts(ws, []string{".pi-build-session"}, dst); n != 1 || err != nil {
			t.Fatalf("RetainPrompts = %d, %v, want 1, nil", n, err)
		}
		if got := retained(t, dst); len(got) != 1 || got["ok.md"] != "fine" {
			t.Errorf("copied = %v", got)
		}
	})
	t.Run("a file of exactly the cap and one over it", func(t *testing.T) {
		ws, dst := t.TempDir(), filepath.Join(t.TempDir(), "p")
		writePrompt(t, ws, ".pi-build-session", "at.md", "x")
		writePrompt(t, ws, ".pi-build-session", "over.md", "x")
		dir := filepath.Join(ws, ".pi-build-session", "prompts")
		sparseFile(t, filepath.Join(dir, "at.md"), maxSavedPromptBytes)
		sparseFile(t, filepath.Join(dir, "over.md"), maxSavedPromptBytes+1)
		n, err := RetainPrompts(ws, []string{".pi-build-session"}, dst)
		if n != 1 || err == nil || !strings.Contains(err.Error(), "prompts/over.md") || !strings.Contains(err.Error(), "exceeding") {
			t.Fatalf("RetainPrompts = %d, %v, want 1 and the larger file refused", n, err)
		}
		if _, statErr := os.Lstat(filepath.Join(dst, "over.md")); !os.IsNotExist(statErr) {
			t.Error("the oversized prompt was retained")
		}
	})
	t.Run("a link where the copy goes", func(t *testing.T) {
		ws, dst := t.TempDir(), filepath.Join(t.TempDir(), "p")
		writePrompt(t, ws, ".pi-build-session", "build.md", "the prompt")
		hostUntouched := linkInPlaceOf(t, filepath.Join(dst, "build.md"))
		if n, err := RetainPrompts(ws, []string{".pi-build-session"}, dst); n != 1 || err != nil {
			t.Fatalf("RetainPrompts = %d, %v, want 1, nil", n, err)
		}
		// The name is taken, by a link: the copy goes beside it.
		if info, err := os.Lstat(filepath.Join(dst, "build.md")); err != nil || info.Mode()&os.ModeSymlink == 0 {
			t.Errorf("the link at the destination = %v, %v, want it left as it was", info, err)
		}
		assertRegularCopy(t, filepath.Join(dst, "build-2.md"), "the prompt")
		hostUntouched()
		if m, _ := filepath.Glob(filepath.Join(dst, ".retain-*")); len(m) != 0 {
			t.Errorf("temp files left: %v", m)
		}
	})
}

// The saved copies are read back the way they were written: no link
// followed, a regular file only, of at most the size the copy allowed.
func TestRetainedNotesAndPromptsAreReadOnlyFromAPlainFileOfTheirSize(t *testing.T) {
	for _, shape := range hostileSources() {
		t.Run("notes: "+shape.name, func(t *testing.T) {
			runDir := t.TempDir()
			shape.make(t, filepath.Join(runDir, AgentNotesFileName), hostFile(t))
			if text, ok := ReadRetainedAgentNotes(runDir); ok || text != "" {
				t.Errorf("ReadRetainedAgentNotes = %q, %v, want nothing", text, ok)
			}
		})
		t.Run("prompt: "+shape.name, func(t *testing.T) {
			dir := t.TempDir()
			attempt := filepath.Join(dir, PromptsDirName, "build-1")
			if err := os.MkdirAll(attempt, 0o750); err != nil {
				t.Fatal(err)
			}
			shape.make(t, filepath.Join(attempt, "build.md"), hostFile(t))
			if data, ok := ReadSavedPrompt(dir, "build-1", "build"); ok || data != nil {
				t.Errorf("ReadSavedPrompt = %q, %v, want nothing", data, ok)
			}
		})
	}
	t.Run("notes at and over the cap", func(t *testing.T) {
		runDir := t.TempDir()
		sparseFile(t, filepath.Join(runDir, AgentNotesFileName), maxAgentNotesBytes)
		if text, ok := ReadRetainedAgentNotes(runDir); !ok || len(text) != maxAgentNotesBytes {
			t.Errorf("at the cap = %d bytes, %v, want all of it", len(text), ok)
		}
		sparseFile(t, filepath.Join(runDir, AgentNotesFileName), maxAgentNotesBytes+1)
		if text, ok := ReadRetainedAgentNotes(runDir); ok || text != "" {
			t.Errorf("over the cap = %d bytes, %v, want nothing", len(text), ok)
		}
	})
	t.Run("a prompt at and over the cap", func(t *testing.T) {
		dir := t.TempDir()
		attempt := filepath.Join(dir, PromptsDirName, "build-1")
		if err := os.MkdirAll(attempt, 0o750); err != nil {
			t.Fatal(err)
		}
		sparseFile(t, filepath.Join(attempt, "build.md"), maxSavedPromptBytes)
		if data, ok := ReadSavedPrompt(dir, "build-1", "build"); !ok || len(data) != maxSavedPromptBytes {
			t.Errorf("at the cap = %d bytes, %v, want all of it", len(data), ok)
		}
		sparseFile(t, filepath.Join(attempt, "build.md"), maxSavedPromptBytes+1)
		if data, ok := ReadSavedPrompt(dir, "build-1", "build"); ok || data != nil {
			t.Errorf("over the cap = %d bytes, %v, want nothing", len(data), ok)
		}
	})
}
