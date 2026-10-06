package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestBuildTicketHeaderReports covers a real, well-formed brownfield
// ticket alongside malformed-header, typo'd-header, and no-header fixture
// files -- the same real ticket run_ticket.go's own -spec handling
// expects (data/tickets/math-ops-multiply.spec.md), plus
// the header-absence nuance the plan called out explicitly: an absent or
// misspelled header is not a parse error, so it must be reported as "not
// found", never conflated with a genuinely malformed one.
func TestBuildTicketHeaderReports(t *testing.T) {
	t.Parallel()
	realTicket := repoRootTicketPath(t, "math-ops-multiply.spec.md")

	tests := []struct {
		name           string
		specPath       string
		specContent    string // used instead of specPath when non-empty
		wantMalformed  []string
		wantPresent    []string
		wantNotPresent []string
	}{
		{
			name:        "well-formed real ticket",
			specPath:    realTicket,
			wantPresent: []string{"Verify-Command:", "Allowed-Files:", "Required-Changed-Files:", "Required-Content:"},
			wantNotPresent: []string{
				"Tests-Required:", // this ticket never declares it
			},
		},
		{
			name:          "malformed Verify-Command header",
			specContent:   "Verify-Command:\n",
			wantMalformed: []string{"Verify-Command:"},
		},
		{
			name:           "typo'd header name is absent, not malformed",
			specContent:    "Verify-command: make verify\n", // lowercase c
			wantNotPresent: []string{"Verify-Command:", "Allowed-Files:", "Required-Changed-Files:", "Required-Content:", "Tests-Required:"},
		},
		{
			name:           "no headers at all",
			specContent:    "# Ticket: nothing declared\n\nJust prose.\n",
			wantNotPresent: []string{"Verify-Command:", "Allowed-Files:", "Required-Changed-Files:", "Required-Content:", "Tests-Required:"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			specPath := tt.specPath
			if tt.specContent != "" {
				specPath = filepath.Join(t.TempDir(), "ticket.spec.md")
				if err := os.WriteFile(specPath, []byte(tt.specContent), 0o600); err != nil {
					t.Fatalf("write fixture: %v", err)
				}
			}

			reports, err := buildTicketHeaderReports(specPath)
			if err != nil {
				t.Fatalf("buildTicketHeaderReports: %v", err)
			}

			byPrefix := make(map[string]ticketHeaderReport, len(reports))
			for _, r := range reports {
				byPrefix[r.Prefix] = r
			}

			var gotMalformed []string
			for _, r := range reports {
				if r.Malformed {
					gotMalformed = append(gotMalformed, r.Prefix)
				}
			}
			if !equalStringSets(gotMalformed, tt.wantMalformed) {
				t.Errorf("malformed headers = %v, want %v", gotMalformed, tt.wantMalformed)
			}

			for _, prefix := range tt.wantPresent {
				if r, ok := byPrefix[prefix]; !ok || !r.Present {
					t.Errorf("header %s: Present = false, want true", prefix)
				}
			}
			for _, prefix := range tt.wantNotPresent {
				r, ok := byPrefix[prefix]
				if !ok {
					t.Fatalf("no report for header %s", prefix)
				}
				if r.Present {
					t.Errorf("header %s: Present = true, want false", prefix)
				}
				if r.Malformed {
					t.Errorf("header %s: Malformed = true, want false (absent/misspelled headers must never be reported as malformed)", prefix)
				}
				if r.AbsentNote == "" {
					t.Errorf("header %s: AbsentNote is empty, want a plain-English note", prefix)
				}
			}
		})
	}
}

// TestCheckTicketMainExitsNonZeroOnlyForMalformedHeaders is check-ticket's
// exit-code contract: a ticket with only absent headers exits 0 (missing
// is a valid state), a ticket with a malformed header exits non-zero.
func TestCheckTicketMainExitsNonZeroOnlyForMalformedHeaders(t *testing.T) {
	t.Parallel()
	noHeaders := filepath.Join(t.TempDir(), "ticket.spec.md")
	if err := os.WriteFile(noHeaders, []byte("# Ticket\n\nprose only\n"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	if err := checkTicketMain([]string{noHeaders}); err != nil {
		t.Errorf("checkTicketMain(no headers) = %v, want nil (absent headers alone must not fail)", err)
	}

	malformed := filepath.Join(t.TempDir(), "ticket.spec.md")
	if err := os.WriteFile(malformed, []byte("Verify-Command:\n"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	if err := checkTicketMain([]string{malformed}); err == nil {
		t.Error("checkTicketMain(malformed header) = nil, want an error")
	}
}

// TestTicketTemplateRoundTripsThroughCheckTicket is the plan's own
// explicit design goal: ticket-template's output must parse cleanly
// through check-ticket, with every header it declares present and
// well-formed, so the two tools can never silently drift apart.
func TestTicketTemplateRoundTripsThroughCheckTicket(t *testing.T) {
	t.Parallel()
	reports, err := buildTicketHeaderReportsFromContent(t, ticketTemplateContent)
	if err != nil {
		t.Fatalf("buildTicketHeaderReports(ticketTemplateContent): %v", err)
	}

	wantDeclared := []string{"Verify-Command:", "Allowed-Files:", "Required-Changed-Files:", "Required-Content:", "Tests-Required:"}
	if len(reports) != len(wantDeclared) {
		t.Fatalf("got %d header reports, want %d", len(reports), len(wantDeclared))
	}
	for _, r := range reports {
		if r.Malformed {
			t.Errorf("template header %s is malformed: %v", r.Prefix, r.Err)
		}
		if !r.Present {
			t.Errorf("template header %s: Present = false, want true (the template declares every known header as a live example)", r.Prefix)
		}
	}
}

// TestTicketTemplateMainRefusesToOverwriteExistingFile matches
// writeScaffoldFiles' own defensive convention in init.go.
func TestTicketTemplateMainRefusesToOverwriteExistingFile(t *testing.T) {
	t.Parallel()
	out := filepath.Join(t.TempDir(), "ticket.spec.md")
	if err := os.WriteFile(out, []byte("pre-existing content\n"), 0o600); err != nil {
		t.Fatalf("seed existing file: %v", err)
	}

	err := ticketTemplateMain([]string{"-o", out})
	if err == nil {
		t.Fatal("ticketTemplateMain(-o <existing file>) = nil, want a refusal error")
	}
	if !strings.Contains(err.Error(), "refusing to overwrite") {
		t.Errorf("ticketTemplateMain error = %v, want it to mention refusing to overwrite", err)
	}

	got, readErr := os.ReadFile(out)
	if readErr != nil {
		t.Fatalf("read %s: %v", out, readErr)
	}
	if string(got) != "pre-existing content\n" {
		t.Errorf("existing file content changed: got %q", got)
	}
}

// TestTicketTemplateMainWritesToOutputPath is ticket-template's -o happy
// path.
func TestTicketTemplateMainWritesToOutputPath(t *testing.T) {
	t.Parallel()
	out := filepath.Join(t.TempDir(), "ticket.spec.md")
	if err := ticketTemplateMain([]string{"-o", out}); err != nil {
		t.Fatalf("ticketTemplateMain: %v", err)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read %s: %v", out, err)
	}
	if string(got) != ticketTemplateContent {
		t.Errorf("written file content does not match ticketTemplateContent")
	}
}

// TestPrintTicketHeaderReport checks the report's ok/FAIL/-- line shape
// and summary count.
func TestPrintTicketHeaderReport(t *testing.T) {
	t.Parallel()
	reports := []ticketHeaderReport{
		{Prefix: "Verify-Command:", Present: true, Declared: "make verify"},
		{Prefix: "Allowed-Files:", Malformed: true, Err: errFixture("bad")},
	}
	var buf bytes.Buffer
	malformed := printTicketHeaderReport(&buf, "ticket.spec.md", reports)
	if malformed != 1 {
		t.Errorf("malformed = %d, want 1", malformed)
	}
	out := buf.String()
	if !strings.Contains(out, "ok    Verify-Command:          make verify") {
		t.Errorf("report missing expected ok line, got:\n%s", out)
	}
	if !strings.Contains(out, "FAIL  Allowed-Files:           bad") {
		t.Errorf("report missing expected FAIL line, got:\n%s", out)
	}
	if !strings.Contains(out, "1/2 header(s) malformed") {
		t.Errorf("report missing expected summary line, got:\n%s", out)
	}
}

// repoRootTicketPath resolves a fixture ticket under data/tickets/,
// walking up from the test binary's working directory the same way other
// tests in this package locate repo-root fixtures.
func repoRootTicketPath(t *testing.T, name string) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		candidate := filepath.Join(dir, "data", "tickets", name)
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("could not find data/tickets/%s walking up from %s", name, dir)
		}
		dir = parent
	}
}

// buildTicketHeaderReportsFromContent writes content to a temp file and
// runs buildTicketHeaderReports against it -- a small helper so tests that
// only care about in-memory content (like the template round-trip) don't
// need to know buildTicketHeaderReports takes a path, not a []byte.
func buildTicketHeaderReportsFromContent(t *testing.T, content string) ([]ticketHeaderReport, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ticket.spec.md")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return buildTicketHeaderReports(path)
}

func equalStringSets(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[string]int, len(a))
	for _, s := range a {
		seen[s]++
	}
	for _, s := range b {
		seen[s]--
	}
	for _, n := range seen {
		if n != 0 {
			return false
		}
	}
	return true
}

type errFixture string

func (e errFixture) Error() string { return string(e) }
