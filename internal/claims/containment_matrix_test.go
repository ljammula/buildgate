package claims

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

func TestContainmentMatrixCoversRequiredBoundaries(t *testing.T) {
	t.Parallel()
	root := findRepoRoot(t)
	b, err := os.ReadFile(filepath.Join(root, "containment-matrix.md"))
	if err != nil {
		t.Fatalf("read containment matrix: %v", err)
	}
	text := string(b)
	for _, boundary := range []string{"Filesystem", "Network", "Credentials", "Docker control", "Cancellation", "Skills"} {
		if !regexp.MustCompile(`(?m)^\| ` + regexp.QuoteMeta(boundary) + ` \|`).MatchString(text) {
			t.Errorf("matrix missing boundary %q", boundary)
		}
	}
	if !regexp.MustCompile(`(?m)not implemented`).MatchString(text) || !regexp.MustCompile(`(?m)enforced for subprocesses`).MatchString(text) {
		t.Fatal("matrix must distinguish partial controls from enforced cancellation")
	}
}
