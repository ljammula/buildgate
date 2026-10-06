package request

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeOracleDir creates tickets/001.oracle with the named files
// (RUN_COMMAND.txt gets a valid command, others a stub).
func writeOracleDir(t *testing.T, dataDir, id string, names ...string) {
	t.Helper()
	dir := filepath.Join(Dir(dataDir, id), "tickets", "001.oracle")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		content := "package x\n\nimport \"testing\"\n\nfunc TestOracleX(t *testing.T) {}\n"
		if name == TicketOracleRunCommandFilename {
			content = "go test ./.oracle/...\n"
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// TestApprovePlanRefusesOracleDirWithoutRunCommand: an empty oracle
// directory, or a populated one lacking RUN_COMMAND.txt, is refused at
// approval (resolveTicketOracle would halt the build on it later), names the
// directory, and leaves the request untouched.
func TestApprovePlanRefusesOracleDirWithoutRunCommand(t *testing.T) {
	for name, files := range map[string][]string{"empty": nil, "no run command": {"oracle_001_test.go"}} {
		t.Run(name, func(t *testing.T) {
			dataDir := t.TempDir()
			newApprovableRequest(t, dataDir, "req-1", StatePlanReview, true)
			writeOracleDir(t, dataDir, "req-1", files...)

			_, err := Approve(dataDir, "req-1", "alice", fixedNow, nil)
			if err == nil {
				t.Fatal("Approve: want an error, got nil")
			}
			if !strings.Contains(err.Error(), "001.oracle") || !strings.Contains(err.Error(), TicketOracleRunCommandFilename) {
				t.Errorf("error = %v, want it to name the directory and RUN_COMMAND.txt", err)
			}
			loaded, err := Load(dataDir, "req-1")
			if err != nil {
				t.Fatal(err)
			}
			if loaded.State != StatePlanReview || len(loaded.ApprovedSHA256) != 0 {
				t.Errorf("request changed by a refused approval: state=%q approved=%v", loaded.State, loaded.ApprovedSHA256)
			}
		})
	}
}

// TestApproveShownRefusesOracleFilesTheClientDidNotShow: the API-facing
// variant refuses when oracle files are not covered by expectedSHA256 (nil or
// console-shaped), accepts when they are, and leaves state untouched on
// refusal.
func TestApproveShownRefusesOracleFilesTheClientDidNotShow(t *testing.T) {
	setup := func(t *testing.T) (string, map[string]string) {
		dataDir := t.TempDir()
		newApprovableRequest(t, dataDir, "req-1", StatePlanReview, true)
		writeOracleDir(t, dataDir, "req-1", TicketOracleRunCommandFilename, "oracle_001_test.go")
		full := map[string]string{}
		for _, rel := range []string{"tickets/001.spec.md", "tickets/002.spec.md", "tickets/001.oracle/RUN_COMMAND.txt", "tickets/001.oracle/oracle_001_test.go"} {
			h, err := HashFile(dataDir, "req-1", rel)
			if err != nil {
				t.Fatal(err)
			}
			full[rel] = h
		}
		return dataDir, full
	}

	t.Run("nil map", func(t *testing.T) {
		dataDir, _ := setup(t)
		_, err := ApproveShown(dataDir, "req-1", "alice", fixedNow, nil)
		if !errors.Is(err, ErrOracleNotShown) {
			t.Fatalf("err = %v, want ErrOracleNotShown", err)
		}
		loaded, _ := Load(dataDir, "req-1")
		if loaded.State != StatePlanReview {
			t.Errorf("state = %q, want unchanged", loaded.State)
		}
	})
	t.Run("console-shaped map", func(t *testing.T) {
		dataDir, full := setup(t)
		console := map[string]string{"tickets/001.spec.md": full["tickets/001.spec.md"], "tickets/002.spec.md": full["tickets/002.spec.md"]}
		if _, err := ApproveShown(dataDir, "req-1", "alice", fixedNow, console); !errors.Is(err, ErrOracleNotShown) {
			t.Fatalf("err = %v, want ErrOracleNotShown", err)
		}
	})
	t.Run("full map", func(t *testing.T) {
		dataDir, full := setup(t)
		if _, err := ApproveShown(dataDir, "req-1", "alice", fixedNow, full); err != nil {
			t.Fatalf("ApproveShown with every oracle file covered: %v", err)
		}
	})
	t.Run("no oracle dirs unchanged", func(t *testing.T) {
		dataDir := t.TempDir()
		newApprovableRequest(t, dataDir, "req-2", StatePlanReview, true)
		if _, err := ApproveShown(dataDir, "req-2", "alice", fixedNow, nil); err != nil {
			t.Fatalf("ApproveShown without oracle dirs: %v", err)
		}
	})
}

// Approval must itself refuse a symlinked ticket oracle directory and a
// non-regular entry, before (and independent of) the canary's file
// classification: the error must come from that guard, not CheckDir.
func TestApprovePlanRefusesSymlinkedTicketOracleDirAndEntries(t *testing.T) {
	setup := func(t *testing.T) (dataDir, oracleDir string) {
		dataDir = t.TempDir()
		newApprovableRequest(t, dataDir, "req-1", StatePlanReview, true)
		return dataDir, filepath.Join(Dir(dataDir, "req-1"), "tickets", "001.oracle")
	}
	t.Run("symlinked directory", func(t *testing.T) {
		dataDir, oracleDir := setup(t)
		target := t.TempDir()
		for n, body := range map[string]string{TicketOracleRunCommandFilename: "go test ./.oracle/...\n", "x_oracle_test.go": "package x\n\nimport \"testing\"\n\nfunc TestOracleX(t *testing.T) {}\n"} {
			if err := os.WriteFile(filepath.Join(target, n), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.Symlink(target, oracleDir); err != nil {
			t.Fatal(err)
		}
		_, err := Approve(dataDir, "req-1", "alice", fixedNow, nil)
		if err == nil || !strings.Contains(err.Error(), "is a symlink or not a directory") {
			t.Fatalf("error = %v, want the symlinked-directory guard's refusal", err)
		}
	})
	t.Run("symlinked entry", func(t *testing.T) {
		dataDir, oracleDir := setup(t)
		if err := os.MkdirAll(oracleDir, 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(oracleDir, TicketOracleRunCommandFilename), []byte("go test ./.oracle/...\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("/etc/hosts", filepath.Join(oracleDir, "x_oracle_test.go")); err != nil {
			t.Fatal(err)
		}
		_, err := Approve(dataDir, "req-1", "alice", fixedNow, nil)
		if err == nil || !strings.Contains(err.Error(), "(symlink or special file)") || strings.Contains(err.Error(), "runtime canary") {
			t.Fatalf("error = %v, want the non-regular-entry guard's refusal", err)
		}
	})
}
