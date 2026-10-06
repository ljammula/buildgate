package claims

import (
	"bufio"
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"testing"
)

// notesRefRE matches references to the private planning-notes repo and to
// the dated plan/review/run write-ups that live there. This repo is synced
// to places that cannot read that repo, so its docs and comments must stand
// on their own: explain the reason inline instead of citing a dated plan.
var notesRefRE = regexp.MustCompile(`software-factory-notes|(?i:notes repo)|CLAIMS_HISTORY|lights-dim-agent-factory-plan|` +
	// Dated write-ups by name, including one wrapped mid-name onto the next line.
	`\b(plan|proving-ground|follow-up|code-review|fable-review|onboarding-walk|console-walk|bar-runbook)-20\d\d-\d\d|` +
	`[a-z]-plan-20\d\d-\d\d|[a-z]-20\d\d-\d\d-\d\d[a-z0-9-]*\.md|` +
	// The same write-ups cited without their date.
	`console-ux-plan|onboarding-walk-findings|operator-onboarding|console-operator-walkthrough|` +
	`containment-hardening-plan|engineer-workstation-plan|production-rollout-plan|quickstart-onboarding|` +
	`staged-oracle-authoring|execution-quality-usability|confidence-audit-and|acceptance-test-oracles|` +
	`operator-recovery-and-throughput|quality-hardening-spec|lights-off-delivery|harness-model-choice`)

// planLabelRE matches the finding and work-package ids those write-ups
// define (F12, O1, G7, B8, WP-D1). Without the write-up they name nothing,
// so a comment must say what the finding was instead. T-NN threat ids and
// SC-NNN invariants are defined in safety-contract.md and stay allowed.
var planLabelRE = regexp.MustCompile(`\b([FOGB]\d{1,2}|WP-[A-Z]\d)\b|` +
	// Numbered findings of a past review session ("adversarial review
	// finding 5", "round-2 review, item 3(b)"): the numbered list lives in
	// that session's output, not here.
	`(?i:\bfindings? #?\d|\bitems? #?\d+(\([a-z]\)|[a-z])?\b)`)

// ownLabelFiles define their own ids with this shape (a demo's findings
// table, a pilot plan's blank ticket log), so planLabelRE doesn't apply.
var ownLabelFiles = map[string]bool{}

func TestNoPrivateNotesReferences(t *testing.T) {
	t.Parallel()
	repoRoot := findRepoRoot(t)
	out, err := exec.Command("git", "-C", repoRoot, "ls-files", "-z").Output()
	if err != nil {
		t.Fatalf("git ls-files: %v", err)
	}
	self := filepath.Join("internal", "claims", "notes_refs_test.go")

	var paths []string
	for _, rel := range bytes.Split(bytes.TrimRight(out, "\x00"), []byte{0}) {
		path := string(rel)
		if path == self {
			continue
		}
		paths = append(paths, path)
	}

	// Scanning ~700 tracked files line-by-line against two alternation-heavy
	// regexes is the dominant cost of this package (dozens of seconds under
	// -race, single-threaded); files are independent of one another, so a
	// worker pool spreads the same checks across cores instead of skipping
	// any of them.
	jobs := make(chan string)
	var wg sync.WaitGroup
	workers := runtime.GOMAXPROCS(0)
	if workers > len(paths) {
		workers = len(paths)
	}
	if workers < 1 {
		workers = 1
	}
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for path := range jobs {
				scanFileForNotesRefs(t, repoRoot, path)
			}
		}()
	}
	for _, path := range paths {
		jobs <- path
	}
	close(jobs)
	wg.Wait()
}

// scanFileForNotesRefs checks one tracked file for private-notes references
// and numbered-finding citations. It runs on a worker goroutine, so it never
// calls t.Fatal/t.FailNow (only the test's own goroutine may): a read error
// is reported with t.Errorf like any other finding.
func scanFileForNotesRefs(t *testing.T, repoRoot, path string) {
	content, err := os.ReadFile(filepath.Join(repoRoot, path))
	if err != nil {
		t.Errorf("read %s: %v", path, err)
		return
	}
	if bytes.IndexByte(content, 0) >= 0 {
		return // binary (images, video): ids in its bytes are noise
	}
	sc := bufio.NewScanner(bytes.NewReader(content))
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for n := 1; sc.Scan(); n++ {
		if m := notesRefRE.FindString(sc.Text()); m != "" {
			t.Errorf("%s:%d references the private notes repo (%q)", path, n, m)
		}
		// Ticket specs and fixture repos describe other repos; their
		// "item 3.5" points into that repo's own docs (and go.sum hashes
		// contain label-shaped runs).
		if ownLabelFiles[path] || strings.HasPrefix(path, "data/tickets/") || strings.HasPrefix(path, "testdata/fixtures/") {
			continue
		}
		if m := planLabelRE.FindString(sc.Text()); m != "" {
			t.Errorf("%s:%d cites a finding by number (%q); say what the finding was instead", path, n, m)
		}
	}
	if err := sc.Err(); err != nil {
		t.Errorf("scan %s: %v", path, err)
	}
}
