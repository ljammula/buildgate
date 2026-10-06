package request

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// endlessReader models a file that keeps growing after its fstat: it yields
// far more bytes than any cap.
type endlessReader struct{}

func (endlessReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'a'
	}
	return len(p), nil
}

func TestReadAtMostBoundsBytesActuallyRead(t *testing.T) {
	if _, err := readAtMost(endlessReader{}, 1024); !errors.Is(err, ErrOracleFileTooLarge) {
		t.Fatalf("growing file: err = %v, want ErrOracleFileTooLarge", err)
	}
	if _, err := readAtMost(strings.NewReader(strings.Repeat("a", 1025)), 1024); !errors.Is(err, ErrOracleFileTooLarge) {
		t.Errorf("limit+1 bytes: err = %v", err)
	}
	b, err := readAtMost(strings.NewReader(strings.Repeat("a", 1024)), 1024)
	if err != nil || len(b) != 1024 {
		t.Errorf("exactly limit: %d bytes, err %v", len(b), err)
	}
}

// TestSetOracleRunCommandTempNeverInsideOracleDir: in the window between the
// temp write and the rename (where a crash would strand the file), oracle/
// must hold no temp file, and the temp lives in the request dir.
func TestSetOracleRunCommandTempNeverInsideOracleDir(t *testing.T) {
	dataDir := t.TempDir()
	id := "req-1"
	if err := SaveText(dataDir, id, "text"); err != nil {
		t.Fatal(err)
	}
	r := New(id, "/repos/app", "app", Source{Kind: SourceText}, time.Now())
	r.State = StateOracleReview
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	inRequestDir := false
	oracleBeforeRename = func() {
		entries, _ := os.ReadDir(Dir(dataDir, id))
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), ".tmp") {
				inRequestDir = true
			}
		}
		oracleEntries, _ := os.ReadDir(filepath.Join(Dir(dataDir, id), RequestOracleDirName))
		for _, e := range oracleEntries {
			t.Errorf("oracle/ holds %s before the rename", e.Name())
		}
	}
	defer func() { oracleBeforeRename = nil }()
	if _, err := SetOracleRunCommand(dataDir, id, TicketOracleRunCommandFilename, "go test ./.oracle/...\n", ""); err != nil {
		t.Fatal(err)
	}
	if !inRequestDir {
		t.Error("temp file was not in the request dir before the rename")
	}
}
