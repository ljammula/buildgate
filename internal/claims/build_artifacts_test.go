package claims

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// executableMagics are the leading bytes of a compiled executable: ELF,
// Mach-O (32/64-bit, either byte order, and universal) and PE.
var executableMagics = [][]byte{
	{0x7f, 'E', 'L', 'F'},
	{0xfe, 0xed, 0xfa, 0xce}, {0xfe, 0xed, 0xfa, 0xcf},
	{0xce, 0xfa, 0xed, 0xfe}, {0xcf, 0xfa, 0xed, 0xfe},
	{0xca, 0xfe, 0xba, 0xbe},
	{'M', 'Z'},
}

// TestNoTrackedBuildArtifacts fails when a compiled executable is tracked.
// Every binary here is rebuilt from source in seconds, and a `go build` run
// inside cmd/factoryd once put a 50 MB factoryd into a commit.
func TestNoTrackedBuildArtifacts(t *testing.T) {
	repoRoot := findRepoRoot(t)
	out, err := exec.Command("git", "-C", repoRoot, "ls-files", "-z").Output()
	if err != nil {
		t.Fatalf("git ls-files: %v", err)
	}
	for _, rel := range bytes.Split(bytes.TrimRight(out, "\x00"), []byte{0}) {
		path := string(rel)
		f, err := os.Open(filepath.Join(repoRoot, path))
		if err != nil {
			continue // deleted in the working tree, not yet staged
		}
		head := make([]byte, 4)
		n, _ := io.ReadFull(f, head)
		f.Close()
		for _, magic := range executableMagics {
			if bytes.HasPrefix(head[:n], magic) {
				t.Errorf("%s is a compiled executable: untrack it (git rm --cached) and ignore it", path)
			}
		}
	}
}
