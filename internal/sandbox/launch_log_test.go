package sandbox

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCreateLaunchLogKeepsTheEarlierLaunchsLog(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "plan_tickets.log")
	write := func(text string) {
		t.Helper()
		f, err := CreateLaunchLog(path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.WriteString(text); err != nil {
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
	}
	write("first: agent exited 1\n")
	write("")        // an empty log is not kept
	write("third\n") // overwrites the empty second
	write("fourth\n")

	for name, want := range map[string]string{
		"plan_tickets.log":   "fourth\n",
		"plan_tickets.1.log": "first: agent exited 1\n",
		"plan_tickets.2.log": "third\n",
	} {
		got, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "plan_tickets.3.log")); !os.IsNotExist(err) {
		t.Errorf("plan_tickets.3.log exists (err %v); the empty log must not be kept", err)
	}
}

func TestCreateLaunchLogRefusesALinkAtThePath(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "elsewhere")
	if err := os.WriteFile(target, []byte("keep me\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "build.log")
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if f, err := CreateLaunchLog(path); err == nil {
		f.Close()
		t.Fatal("CreateLaunchLog followed a symlink; want it refused")
	}
	if got, _ := os.ReadFile(target); string(got) != "keep me\n" {
		t.Errorf("the link's target was changed to %q", got)
	}
}
