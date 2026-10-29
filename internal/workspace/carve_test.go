package workspace

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// carveFixture is a base commit and a result commit that adds, changes and
// deletes files in two directories.
func carveFixture(t *testing.T) (repo, base, result string) {
	t.Helper()
	repo = newFixtureRepo(t)
	commitFile(t, repo, "keep.txt", "keep\n")
	if err := os.MkdirAll(filepath.Join(repo, "db"), 0o755); err != nil {
		t.Fatal(err)
	}
	commitFile(t, repo, "db/old.sql", "old\n")
	commitFile(t, repo, "app.go", "package app\n")
	base = headOf(t, repo)
	gitT(t, repo, "checkout", "-q", "-b", "ticket")
	commitFile(t, repo, "db/new.sql", "new\n")
	gitT(t, repo, "rm", "-q", "db/old.sql")
	gitT(t, repo, "commit", "-q", "-m", "drop old")
	if err := os.WriteFile(filepath.Join(repo, "app.go"), []byte("package app\n\nfunc F() {}\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(repo, "app.go"), 0o755); err != nil {
		t.Fatal(err)
	}
	gitT(t, repo, "add", "-A")
	gitT(t, repo, "commit", "-q", "-m", "app")
	return repo, base, headOf(t, repo)
}

// TestCarveCommitsChainToTheResultTree: carving the first group onto the
// base and the second onto the first ends at the result's tree, each
// carved tree holds only the groups carved so far, and no ref or worktree
// moves.
func TestCarveCommitsChainToTheResultTree(t *testing.T) {
	repo, base, result := carveFixture(t)
	ctx := context.Background()
	headBefore := headOf(t, repo)
	first, err := CarveCommit(ctx, repo, base, result, []string{"db/new.sql", "db/old.sql"}, "db")
	if err != nil {
		t.Fatal(err)
	}
	if got := gitT(t, repo, "rev-parse", first+"^"); got != base {
		t.Errorf("first parent = %s, want the base %s", got, base)
	}
	if got := gitT(t, repo, "ls-tree", "-r", "--name-only", first); got != "app.go\ncontent.txt\ndb/new.sql\nkeep.txt" {
		t.Errorf("first carved tree holds %q", got)
	}
	if got := gitT(t, repo, "show", first+":app.go"); got != "package app" {
		t.Errorf("the first carved commit changed app.go: %q", got)
	}
	second, err := CarveCommit(ctx, repo, first, result, []string{"app.go"}, "app")
	if err != nil {
		t.Fatal(err)
	}
	want, err := TreeOf(ctx, repo, result)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := TreeOf(ctx, repo, second); got != want {
		t.Errorf("last carved tree = %s, want the result's %s", got, want)
	}
	if mode := gitT(t, repo, "ls-tree", second, "app.go"); mode[:6] != "100755" {
		t.Errorf("a file's mode is not carried: %q", mode)
	}
	if headOf(t, repo) != headBefore || gitT(t, repo, "status", "--porcelain") != "" {
		t.Errorf("carving moved HEAD or touched the worktree")
	}
}

func TestCarveTreeRefusesAPathThatIsNotAFile(t *testing.T) {
	repo, base, result := carveFixture(t)
	if _, err := CarveTree(context.Background(), repo, base, result, []string{"db"}); err == nil {
		t.Error("a directory was carved as a file")
	}
	if _, err := CarveTree(context.Background(), repo, base, result, nil); err == nil {
		t.Error("no paths was carved")
	}
}

func TestCarveChainEndsAtTheResultTreeAndRefusesPartsThatMissAFile(t *testing.T) {
	repo, base, result := carveFixture(t)
	ctx := context.Background()
	commits, err := CarveChain(ctx, repo, base, result, [][]string{{"db/new.sql", "db/old.sql"}, {"app.go"}})
	if err != nil || len(commits) != 2 {
		t.Fatalf("CarveChain = %v, %v", commits, err)
	}
	if got := gitT(t, repo, "rev-parse", commits[1]+"^"); got != commits[0] {
		t.Errorf("second commit's parent = %s, want the first %s", got, commits[0])
	}
	if _, err := CarveChain(ctx, repo, base, result, [][]string{{"db/new.sql"}, {"app.go"}}); err == nil {
		t.Error("parts that leave db/old.sql's deletion out were accepted")
	}
}
