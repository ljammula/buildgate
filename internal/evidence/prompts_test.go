package evidence

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func writePrompt(t *testing.T, ws, session, name, text string) {
	t.Helper()
	dir := filepath.Join(ws, session, "prompts")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

func retained(t *testing.T, dst string) map[string]string {
	t.Helper()
	got := map[string]string{}
	entries, _ := os.ReadDir(dst)
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join(dst, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		got[e.Name()] = string(data)
	}
	return got
}

func TestRetainPromptsCopiesARegularFileRedactedAndPrivate(t *testing.T) {
	ws, dst := t.TempDir(), filepath.Join(t.TempDir(), "prompts", "build-1")
	writePrompt(t, ws, ".pi-build-session", "build-round-1.md", "fix it\napi_key=sk-abcdefghijklmnopqrstuvwxyz\n\x1b[31mred\x1b[0m\n")
	n, err := RetainPrompts(ws, []string{".pi-build-session"}, dst)
	if err != nil || n != 1 {
		t.Fatalf("RetainPrompts = %d, %v", n, err)
	}
	got := retained(t, dst)["build-round-1.md"]
	if !strings.Contains(got, "fix it") || strings.Contains(got, "abcdefghijklmnopqrstuvwxyz") || strings.Contains(got, "\x1b") {
		t.Errorf("copy = %q, want the text redacted and without escapes", got)
	}
	if info, _ := os.Stat(filepath.Join(dst, "build-round-1.md")); info == nil || info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", info)
	}
	// The session copy is the caller's to remove.
	if err := DropSavedPrompts(ws, []string{".pi-build-session"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(ws, ".pi-build-session", "prompts")); !os.IsNotExist(err) {
		t.Errorf("the session's prompts remain (%v)", err)
	}
}

func TestRetainPromptsRefusesWhatTheScriptDoesNotWrite(t *testing.T) {
	ws, dst := t.TempDir(), filepath.Join(t.TempDir(), "p")
	outside := filepath.Join(t.TempDir(), "secret.md")
	if err := os.WriteFile(outside, []byte("SECRET"), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(ws, ".pi-build-session", "prompts")
	writePrompt(t, ws, ".pi-build-session", "ok.md", "fine")
	if err := os.Symlink(outside, filepath.Join(dir, "link.md")); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(dir, "pipe.md"), 0o600); err != nil {
		t.Fatal(err)
	}
	writePrompt(t, ws, ".pi-build-session", "big.md", strings.Repeat("a", maxSavedPromptBytes+1))
	writePrompt(t, ws, ".pi-build-session", "Bad_Name.md", "x")
	writePrompt(t, ws, ".pi-build-session", "notes.txt", "x")
	writePrompt(t, ws, ".pi-build-session", "-.md.md", "x")
	n, err := RetainPrompts(ws, []string{".pi-build-session"}, dst)
	if n != 1 {
		t.Errorf("copied %d, want only ok.md (err %v)", n, err)
	}
	if err == nil || !strings.Contains(err.Error(), "big.md") {
		t.Errorf("err = %v, want the oversize file reported", err)
	}
	got := retained(t, dst)
	if len(got) != 1 || got["ok.md"] != "fine" {
		t.Errorf("copied = %v", got)
	}
}

func TestRetainPromptsStopsAtTheFileCap(t *testing.T) {
	ws, dst := t.TempDir(), filepath.Join(t.TempDir(), "p")
	for i := 0; i < MaxSavedPrompts+1; i++ {
		writePrompt(t, ws, ".pi-build-session", fmt.Sprintf("p-%03d.md", i), "x")
	}
	n, err := RetainPrompts(ws, []string{".pi-build-session"}, dst)
	if n != MaxSavedPrompts || err == nil {
		t.Errorf("RetainPrompts = %d, %v, want %d and an error naming the cap", n, err, MaxSavedPrompts)
	}
	if got := retained(t, dst); len(got) != MaxSavedPrompts || got["p-050.md"] != "" {
		t.Errorf("copied %d files, including the 51st: %v", len(got), got["p-050.md"])
	}
}

func TestRetainPromptsNeverGoesThroughALink(t *testing.T) {
	ws, dst := t.TempDir(), filepath.Join(t.TempDir(), "p")
	elsewhere := t.TempDir()
	if err := os.MkdirAll(filepath.Join(elsewhere, "prompts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(elsewhere, "prompts", "x.md"), []byte("OUTSIDE"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The session folder is a link out of the workspace; so is a prompts folder.
	if err := os.Symlink(elsewhere, filepath.Join(ws, ".pi-build-session")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(ws, ".pi-conformity-session"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(elsewhere, "prompts"), filepath.Join(ws, ".pi-conformity-session", "prompts")); err != nil {
		t.Fatal(err)
	}
	n, _ := RetainPrompts(ws, []string{".pi-build-session", ".pi-conformity-session"}, dst)
	if n != 0 || len(retained(t, dst)) != 0 {
		t.Errorf("copied %d through a link: %v", n, retained(t, dst))
	}
	if err := DropSavedPrompts(ws, []string{".pi-build-session", ".pi-conformity-session"}); err != nil {
		t.Logf("drop: %v", err)
	}
	if _, err := os.Stat(filepath.Join(elsewhere, "prompts", "x.md")); err != nil {
		t.Errorf("a file outside the workspace was removed (%v)", err)
	}
}

func TestRetainPromptsKeepsARelaunchesPromptBesideTheFirst(t *testing.T) {
	ws, dst := t.TempDir(), filepath.Join(t.TempDir(), "p")
	for _, text := range []string{"first", "second"} {
		writePrompt(t, ws, ".pi-build-session", "build-round-1.md", text)
		if _, err := RetainPrompts(ws, []string{".pi-build-session"}, dst); err != nil {
			t.Fatal(err)
		}
	}
	got := retained(t, dst)
	if got["build-round-1.md"] != "first" || got["build-round-1-2.md"] != "second" {
		t.Errorf("copied = %v", got)
	}
}

func TestListAndReadSavedPrompts(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, PromptsDirName, PromptAttemptDir("code_review", 2))
	ws := t.TempDir()
	writePrompt(t, ws, ".pi-code-review-session", "review-code.md", "review this\n")
	if _, err := RetainPrompts(ws, []string{".pi-code-review-session"}, dst); err != nil {
		t.Fatal(err)
	}
	list := ListSavedPrompts(dir)
	if len(list) != 1 || list[0].Attempt != "code_review-2" || list[0].Name != "review-code" || list[0].Bytes != 12 {
		t.Fatalf("list = %+v", list)
	}
	if data, ok := ReadSavedPrompt(dir, "code_review-2", "review-code"); !ok || !bytes.Equal(data, []byte("review this\n")) {
		t.Errorf("read = %q, %v", data, ok)
	}
	for _, bad := range [][2]string{
		{"..", "review-code"}, {"code_review-2", ".."}, {"code_review-2", "../code_review-2/review-code"},
		{"code_review-2/..", "x"}, {"/etc", "passwd"}, {"code_review-2", "review-code\x00"}, {"code_review-2", "REVIEW"},
		{"code_review-2", "review-code.md"}, {"code_review-3", "review-code"}, {"", ""},
	} {
		if data, ok := ReadSavedPrompt(dir, bad[0], bad[1]); ok {
			t.Errorf("ReadSavedPrompt(%q, %q) = %q, want a refusal", bad[0], bad[1], data)
		}
	}
	// A prompt that is a link out of the directory is not read.
	outside := filepath.Join(t.TempDir(), "o.md")
	if err := os.WriteFile(outside, []byte("OUTSIDE"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dst, "linked.md")); err != nil {
		t.Fatal(err)
	}
	if _, ok := ReadSavedPrompt(dir, "code_review-2", "linked"); ok {
		t.Error("read a prompt that is a symlink")
	}
	if got := ListSavedPrompts(dir); len(got) != 1 {
		t.Errorf("list = %+v, want the link left out", got)
	}
}

func TestNextPromptAttempt(t *testing.T) {
	dir := t.TempDir()
	if got := NextPromptAttempt(dir, "spec"); got != 1 {
		t.Errorf("empty = %d", got)
	}
	for _, name := range []string{"spec-1", "spec-3", "plan-7"} {
		if err := os.MkdirAll(filepath.Join(dir, PromptsDirName, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if got := NextPromptAttempt(dir, "spec"); got != 4 {
		t.Errorf("after spec-3 = %d, want 4", got)
	}
}
