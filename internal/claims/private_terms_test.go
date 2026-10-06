package claims

import (
	"bufio"
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// privateTermsFile lists, one case-insensitive regular expression per line,
// names this public repo must not carry: repos that are not public, hosts,
// accounts, home-directory paths. It is gitignored, so the list itself is
// never published; on a clone without it the guard has nothing to check.
const privateTermsFile = ".private-terms"

// privateTermsExempt are tracked files allowed to match: LICENSE names the
// copyright holder.
var privateTermsExempt = map[string]bool{"LICENSE": true}

func TestNoPrivateTerms(t *testing.T) {
	repoRoot := findRepoRoot(t)
	terms, err := loadPrivateTerms(filepath.Join(repoRoot, privateTermsFile))
	if os.IsNotExist(err) {
		t.Skipf("no %s in this checkout", privateTermsFile)
	}
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("git", "-C", repoRoot, "ls-files", "-z").Output()
	if err != nil {
		t.Fatalf("git ls-files: %v", err)
	}
	for _, rel := range bytes.Split(bytes.TrimRight(out, "\x00"), []byte{0}) {
		path := string(rel)
		if privateTermsExempt[path] {
			continue
		}
		content, err := os.ReadFile(filepath.Join(repoRoot, path))
		if err != nil {
			t.Errorf("read %s: %v", path, err)
			continue
		}
		if bytes.IndexByte(content, 0) >= 0 {
			continue // binary
		}
		sc := bufio.NewScanner(bytes.NewReader(content))
		sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
		for n := 1; sc.Scan(); n++ {
			for _, re := range terms {
				if m := re.FindString(sc.Text()); m != "" {
					t.Errorf("%s:%d names a private term (%q)", path, n, m)
				}
			}
		}
	}
}

func loadPrivateTerms(path string) ([]*regexp.Regexp, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var terms []*regexp.Regexp
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		re, err := regexp.Compile("(?i)" + line)
		if err != nil {
			return nil, err
		}
		terms = append(terms, re)
	}
	return terms, nil
}
