package claims

import (
	"bufio"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// The statuses a CLAIMS.md row may have, as the first word of its Status
// cell: Enforced (every test the row names passes in `make verify`), Partial
// (part of the claim is untested; the cell says which), Pending (the contract
// states the rule and nothing enforces it yet; the row names no test).
const (
	statusEnforced = "Enforced"
	statusPartial  = "Partial"
	statusPending  = "Pending"
)

// The two tables of CLAIMS.md whose rows are claims, by section heading.
const (
	claimsSection     = "Claims"
	invariantsSection = "Invariants"
)

var (
	invariantLineRE = regexp.MustCompile(`(?m)^- \*\*(SC-\d{3}) —`)
	invariantIDRE   = regexp.MustCompile(`SC-\d{3}`)
)

// claimRow is one row of a CLAIMS.md claims table: its claim text, the first
// word of its Status cell, and the tests its Tests cell names.
type claimRow struct {
	section string
	line    int
	claim   string
	status  string
	tests   []string
}

func (r claimRow) String() string {
	claim := r.claim
	if len(claim) > 80 {
		claim = claim[:80] + "…"
	}
	return "CLAIMS.md:" + strconv.Itoa(r.line) + " (" + r.section + ": " + claim + ")"
}

// readClaimRows parses the rows of the Claims and Invariants tables: lines
// beginning `|` under either heading, past the header and separator lines.
// A cell ends at a `|` not escaped as `\|`.
func readClaimRows(t *testing.T, root string) []claimRow {
	t.Helper()
	f, err := os.Open(filepath.Join(root, "CLAIMS.md"))
	if err != nil {
		t.Fatalf("read CLAIMS.md: %v", err)
	}
	defer f.Close()
	var rows []claimRow
	section, seenHeader := "", false
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for line := 1; scanner.Scan(); line++ {
		text := scanner.Text()
		if heading, ok := strings.CutPrefix(text, "## "); ok {
			section, seenHeader = heading, false
			continue
		}
		if section != claimsSection && section != invariantsSection || !strings.HasPrefix(text, "|") {
			continue
		}
		if !seenHeader || strings.HasPrefix(text, "|---") {
			seenHeader = true
			continue
		}
		cells := tableCells(text)
		if len(cells) != 3 {
			t.Fatalf("CLAIMS.md:%d: a %s row has %d cells, want 3 (claim, status, tests)", line, section, len(cells))
		}
		rows = append(rows, claimRow{
			section: section,
			line:    line,
			claim:   cells[0],
			status:  firstWord(cells[1]),
			tests:   testNamesIn(cells[2]),
		})
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("read CLAIMS.md: %v", err)
	}
	return rows
}

// tableCells splits one Markdown table row on its unescaped pipes.
func tableCells(row string) []string {
	row = strings.TrimSpace(row)
	row = strings.TrimPrefix(row, "|")
	row = strings.TrimSuffix(row, "|")
	var cells []string
	var cell strings.Builder
	for i := 0; i < len(row); i++ {
		if row[i] == '\\' && i+1 < len(row) && row[i+1] == '|' {
			cell.WriteString(`\|`)
			i++
			continue
		}
		if row[i] == '|' {
			cells = append(cells, strings.TrimSpace(cell.String()))
			cell.Reset()
			continue
		}
		cell.WriteByte(row[i])
	}
	return append(cells, strings.TrimSpace(cell.String()))
}

// firstWord is a Status cell's first word, with Markdown emphasis and
// punctuation around it removed.
func firstWord(cell string) string {
	cell = strings.TrimLeft(cell, "* ")
	end := strings.IndexFunc(cell, func(r rune) bool {
		return !(r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z')
	})
	if end < 0 {
		return cell
	}
	return cell[:end]
}

func testNamesIn(cell string) []string {
	var names []string
	for _, m := range claimsTestNameRE.FindAllStringSubmatch(cell, -1) {
		names = append(names, m[1])
	}
	return names
}

// TestClaimRowsHaveAKnownStatus keeps every row's status one of the three
// words, so a status can be read by a test: an Enforced or Partial row names
// at least one test, and a Pending row names none.
func TestClaimRowsHaveAKnownStatus(t *testing.T) {
	t.Parallel()
	rows := readClaimRows(t, findRepoRoot(t))
	perSection := map[string]int{}
	for _, row := range rows {
		perSection[row.section]++
	}
	for _, section := range []string{claimsSection, invariantsSection} {
		if perSection[section] == 0 {
			t.Fatalf("found no rows under CLAIMS.md's `## %s` heading — did the heading or the table format change?", section)
		}
	}
	for _, row := range rows {
		switch row.status {
		case statusEnforced, statusPartial:
			if len(row.tests) == 0 {
				t.Errorf("%s is %s but its Tests cell names no test", row, row.status)
			}
		case statusPending:
			if len(row.tests) != 0 {
				t.Errorf("%s is Pending but its Tests cell names %v; a Pending row names none (—)", row, row.tests)
			}
		default:
			t.Errorf("%s has status %q; the Status cell must begin Enforced, Partial or Pending", row, row.status)
		}
	}
}

// TestEveryInvariantHasAClaimRow requires one Invariants row per invariant of
// safety-contract.md, and no row for an invariant the contract does not state.
func TestEveryInvariantHasAClaimRow(t *testing.T) {
	t.Parallel()
	root := findRepoRoot(t)
	contract, err := os.ReadFile(filepath.Join(root, "safety-contract.md"))
	if err != nil {
		t.Fatalf("read safety contract: %v", err)
	}
	stated := map[string]bool{}
	for _, m := range invariantLineRE.FindAllStringSubmatch(string(contract), -1) {
		stated[m[1]] = true
	}
	if len(stated) == 0 {
		t.Fatal("found no `- **SC-NNN —` invariant lines in safety-contract.md")
	}
	rowsOf := map[string]int{}
	for _, row := range readClaimRows(t, root) {
		if row.section != invariantsSection {
			continue
		}
		ids := invariantIDRE.FindAllString(row.claim, -1)
		if len(ids) != 1 {
			t.Errorf("%s names %d invariants; an Invariants row names exactly one", row, len(ids))
			continue
		}
		if !stated[ids[0]] {
			t.Errorf("%s names %s, which safety-contract.md does not state", row, ids[0])
		}
		rowsOf[ids[0]]++
	}
	for _, id := range sortedKeys(stated) {
		switch rowsOf[id] {
		case 1:
		case 0:
			t.Errorf("safety-contract.md states %s but CLAIMS.md's Invariants table has no row for it", id)
		default:
			t.Errorf("CLAIMS.md's Invariants table has %d rows for %s; want one", rowsOf[id], id)
		}
	}
}

// TestEnforcedClaimsPassedInThisRun is the end of `make test`
// (scripts/test-sharded.sh, CLAIMS_RAN_CHECK=1): CLAIMS_PASSED_TESTS names a
// file of every top-level test that passed in that whole run (and skipped in
// no package), and every test an Enforced row names must be in it. A test
// that skipped (a live test waiting for an environment variable), or that no
// package ran, makes the row Partial, not Enforced. Skipped subtests of a
// passing test are not seen. With no file named, there is no run: skip.
func TestEnforcedClaimsPassedInThisRun(t *testing.T) {
	path := os.Getenv("CLAIMS_PASSED_TESTS")
	if path == "" {
		t.Skip("CLAIMS_PASSED_TESTS is not set: only `make test` runs this check, over its own run")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the passed tests: %v", err)
	}
	passed := map[string]bool{}
	for _, name := range strings.Fields(string(b)) {
		passed[name] = true
	}
	if len(passed) == 0 {
		t.Fatalf("%s lists no passed test", path)
	}
	for _, row := range readClaimRows(t, findRepoRoot(t)) {
		if row.status != statusEnforced {
			continue
		}
		var missing []string
		for _, name := range row.tests {
			if !passed[name] {
				missing = append(missing, name)
			}
		}
		if len(missing) > 0 {
			t.Errorf("%s is Enforced, but these tests it names did not pass in this run (skipped or not run): %s; make them run in `make verify`, or mark the row Partial and say what is live-only",
				row, strings.Join(missing, ", "))
		}
	}
}
