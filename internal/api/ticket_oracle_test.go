package api

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"buildgate/internal/request"
)

// seedTicketOracle seeds a plan_review request whose ticket 1 has a
// materialized 001.oracle/ directory, returning that directory.
func seedTicketOracle(t *testing.T, dataDir string, state request.State) string {
	t.Helper()
	seedApprovableRequest(t, dataDir, "req-1", state, true)
	r, err := request.Load(dataDir, "req-1")
	if err != nil {
		t.Fatal(err)
	}
	r.Tickets = []request.Ticket{{Index: 1, SpecPath: "tickets/001.spec.md"}}
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(request.Dir(dataDir, "req-1"), "tickets", "001.oracle")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{"RUN_COMMAND.txt": oracleTestCommand, "x_oracle_test.go": oracleTestGoFile} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestListTicketOracleServesMaterializedFilesAtPlanReview(t *testing.T) {
	dataDir := t.TempDir()
	seedTicketOracle(t, dataDir, request.StatePlanReview)
	rec := doOracle(t, dataDir, http.MethodGet, "/requests/req-1/tickets/1/oracle", "read", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list status = %d: %s", rec.Code, rec.Body.String())
	}
	var view ticketOracleListingView
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if len(view.Files) != 2 || view.Files[0].Name != "RUN_COMMAND.txt" || view.Files[1].Name != "x_oracle_test.go" || view.State != request.StatePlanReview {
		t.Fatalf("listing = %+v", view)
	}
	rec = doOracle(t, dataDir, http.MethodGet, "/requests/req-1/tickets/1/oracle/x_oracle_test.go", "read", "")
	if rec.Code != http.StatusOK || rec.Body.String() != oracleTestGoFile {
		t.Fatalf("file status = %d body = %q", rec.Code, rec.Body.String())
	}
	if got, want := rec.Header().Get("X-Content-SHA256"), view.Files[1].SHA256; got != want {
		t.Fatalf("X-Content-SHA256 = %q, want listing hash %q", got, want)
	}
}

func TestTicketOracleRoutesRefuseWrongStateAuthTicketAndNames(t *testing.T) {
	dataDir := t.TempDir()
	dir := seedTicketOracle(t, dataDir, request.StatePlanReview)
	if err := os.Symlink("/etc/hosts", filepath.Join(dir, "link_test.go")); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, path, token string
		want              int
	}{
		{"no token", "/requests/req-1/tickets/1/oracle", "", http.StatusForbidden},
		{"unknown ticket", "/requests/req-1/tickets/2/oracle", "read", http.StatusNotFound},
		{"bad ticket", "/requests/req-1/tickets/x/oracle", "read", http.StatusNotFound},
		{"traversal name", "/requests/req-1/tickets/1/oracle/..%2Fsecret", "read", http.StatusBadRequest},
		{"symlink", "/requests/req-1/tickets/1/oracle/link_test.go", "read", http.StatusNotFound},
		{"missing", "/requests/req-1/tickets/1/oracle/nope_test.go", "read", http.StatusNotFound},
	}
	for _, c := range cases {
		if rec := doOracle(t, dataDir, http.MethodGet, c.path, c.token, ""); rec.Code != c.want {
			t.Errorf("%s: status = %d, want %d (%s)", c.name, rec.Code, c.want, rec.Body.String())
		}
	}
	other := t.TempDir()
	seedTicketOracle(t, other, request.StateSpecReview)
	if rec := doOracle(t, other, http.MethodGet, "/requests/req-1/tickets/1/oracle", "read", ""); rec.Code != http.StatusConflict {
		t.Errorf("wrong state: status = %d, want 409", rec.Code)
	}
}

func TestGetTicketOracleFileRefusesOversize(t *testing.T) {
	dataDir := t.TempDir()
	dir := seedTicketOracle(t, dataDir, request.StatePlanReview)
	big := make([]byte, request.MaxOracleFileReadBytes+1)
	if err := os.WriteFile(filepath.Join(dir, "big_oracle_test.go"), big, 0o600); err != nil {
		t.Fatal(err)
	}
	if rec := doOracle(t, dataDir, http.MethodGet, "/requests/req-1/tickets/1/oracle/big_oracle_test.go", "read", ""); rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", rec.Code)
	}
}

// symlinkTarget makes a directory outside the request holding a readable
// file, the thing a symlinked oracle directory would expose.
func symlinkTarget(t *testing.T) string {
	t.Helper()
	target := t.TempDir()
	for _, name := range []string{"secret_test.go", "RUN_COMMAND.txt"} {
		if err := os.WriteFile(filepath.Join(target, name), []byte("HOST SECRET\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return target
}

func TestOracleRoutesRefuseSymlinkedDirectoryComponents(t *testing.T) {
	type layout struct {
		name  string
		build func(t *testing.T, dataDir, target string)
		list  string
		file  string
	}
	ticketList := "/requests/req-1/tickets/1/oracle"
	ticketFile := ticketList + "/secret_test.go"
	cases := []layout{
		{"symlinked ticket oracle dir", func(t *testing.T, dataDir, target string) {
			dir := seedTicketOracle(t, dataDir, request.StatePlanReview)
			if err := os.RemoveAll(dir); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, dir); err != nil {
				t.Fatal(err)
			}
		}, ticketList, ticketFile},
		{"symlinked tickets dir", func(t *testing.T, dataDir, target string) {
			seedTicketOracle(t, dataDir, request.StatePlanReview)
			tickets := filepath.Join(request.Dir(dataDir, "req-1"), "tickets")
			if err := os.Rename(tickets, tickets+".real"); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(filepath.Join(tickets+".real", "001.oracle"), filepath.Join(target, "001.oracle")); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, tickets); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(target, "001.oracle", "secret_test.go"), []byte("HOST SECRET\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}, ticketList, ticketFile},
		{"symlinked request-level oracle dir", func(t *testing.T, dataDir, target string) {
			dir := seedOracleRequest(t, dataDir, "req-1", request.StateOracleReview)
			if err := os.RemoveAll(dir); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, dir); err != nil {
				t.Fatal(err)
			}
		}, "/requests/req-1/oracle", "/requests/req-1/oracle/secret_test.go"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dataDir := t.TempDir()
			c.build(t, dataDir, symlinkTarget(t))
			rec := doOracle(t, dataDir, http.MethodGet, c.list, "read", "")
			if rec.Code != http.StatusOK {
				t.Fatalf("list status = %d: %s", rec.Code, rec.Body.String())
			}
			var view ticketOracleListingView
			if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
				t.Fatal(err)
			}
			if len(view.Files) != 0 || len(view.Problems) == 0 {
				t.Fatalf("listing must show no files and a problem, got %+v", view)
			}
			rec = doOracle(t, dataDir, http.MethodGet, c.file, "read", "")
			if rec.Code != http.StatusNotFound || strings.Contains(rec.Body.String(), "HOST SECRET") {
				t.Fatalf("read status = %d body = %q, want 404 without the target's content", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestOracleManifestMayBeLargerThanOtherOracleFiles(t *testing.T) {
	dataDir := t.TempDir()
	ticketDir := seedTicketOracle(t, dataDir, request.StatePlanReview)
	manifest := make([]byte, 300<<10)
	for i := range manifest {
		manifest[i] = ' '
	}
	if err := os.WriteFile(filepath.Join(ticketDir, "MANIFEST.json"), manifest, 0o600); err != nil {
		t.Fatal(err)
	}
	if rec := doOracle(t, dataDir, http.MethodGet, "/requests/req-1/tickets/1/oracle/MANIFEST.json", "read", ""); rec.Code != http.StatusOK || rec.Body.Len() != len(manifest) {
		t.Fatalf("ticket manifest: status = %d, %d bytes", rec.Code, rec.Body.Len())
	}
	// Any other file keeps the 256 KiB cap, and the manifest its own 1 MiB one.
	if err := os.WriteFile(filepath.Join(ticketDir, "big_test.go"), manifest, 0o600); err != nil {
		t.Fatal(err)
	}
	if rec := doOracle(t, dataDir, http.MethodGet, "/requests/req-1/tickets/1/oracle/big_test.go", "read", ""); rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("300 KiB non-manifest: status = %d, want 413", rec.Code)
	}
	if err := os.WriteFile(filepath.Join(ticketDir, "MANIFEST.json"), make([]byte, 1<<20+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if rec := doOracle(t, dataDir, http.MethodGet, "/requests/req-1/tickets/1/oracle/MANIFEST.json", "read", ""); rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("over-1MiB manifest: status = %d, want 413", rec.Code)
	}

	reqDir := seedOracleRequest(t, dataDir+"-2", "req-1", request.StateOracleReview)
	if err := os.WriteFile(filepath.Join(reqDir, "MANIFEST.json"), manifest, 0o600); err != nil {
		t.Fatal(err)
	}
	if rec := doOracle(t, dataDir+"-2", http.MethodGet, "/requests/req-1/oracle/MANIFEST.json", "read", ""); rec.Code != http.StatusOK || rec.Body.Len() != len(manifest) {
		t.Fatalf("request-level manifest: status = %d, %d bytes", rec.Code, rec.Body.Len())
	}
}

func TestListTicketOracleReportsAnExistingEmptyDirectory(t *testing.T) {
	dataDir := t.TempDir()
	dir := seedTicketOracle(t, dataDir, request.StatePlanReview)
	for _, name := range []string{"RUN_COMMAND.txt", "x_oracle_test.go"} {
		if err := os.Remove(filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	list := func() ticketOracleListingView {
		rec := doOracle(t, dataDir, http.MethodGet, "/requests/req-1/tickets/1/oracle", "read", "")
		var view ticketOracleListingView
		if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil || rec.Code != http.StatusOK {
			t.Fatalf("status = %d err = %v", rec.Code, err)
		}
		return view
	}
	if view := list(); len(view.Files) != 0 || len(view.Problems) != 1 || !strings.Contains(view.Problems[0], "RUN_COMMAND.txt") {
		t.Fatalf("empty dir: %+v, want the missing-RUN_COMMAND.txt problem approval raises", view)
	}
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	if view := list(); len(view.Files) != 0 || len(view.Problems) != 0 {
		t.Fatalf("absent dir: %+v, want an empty problem-free listing", view)
	}
}

// The listing must show every problem plan approval would refuse, including
// the runtime canary check, so the console blocks Approve with the reason.
func TestListTicketOracleReportsTheCanaryProblemsApprovalRaises(t *testing.T) {
	cases := map[string]struct {
		files   map[string]string
		mention string
	}{
		"stray helper file":       {map[string]string{"helper.go": "package x\n"}, "helper.go"},
		"test without TestOracle": {map[string]string{"helper_test.go": "package x\n\nimport \"testing\"\n\nfunc TestHelper(t *testing.T) {}\n"}, "helper_test.go"},
		"manifest only":           {map[string]string{"MANIFEST.json": "[]"}, "no test files"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			dataDir := t.TempDir()
			dir := seedTicketOracle(t, dataDir, request.StatePlanReview)
			if name == "manifest only" {
				_ = os.Remove(filepath.Join(dir, "x_oracle_test.go"))
			}
			for n, body := range c.files {
				if err := os.WriteFile(filepath.Join(dir, n), []byte(body), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			rec := doOracle(t, dataDir, http.MethodGet, "/requests/req-1/tickets/1/oracle", "read", "")
			var view ticketOracleListingView
			if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil || rec.Code != http.StatusOK {
				t.Fatalf("status = %d err = %v", rec.Code, err)
			}
			if len(view.Problems) != 1 || !strings.Contains(view.Problems[0], c.mention) {
				t.Fatalf("problems = %v, want the canary refusal naming %q", view.Problems, c.mention)
			}
			// Same refusal approval gives.
			if _, err := request.Approve(dataDir, "req-1", "alice", time.Now(), nil); err == nil || !strings.Contains(err.Error(), c.mention) {
				t.Fatalf("approval error = %v, want it to name %q too", err, c.mention)
			}
		})
	}
}

func TestListOracleRefusesToHashAFileOverTheReadCap(t *testing.T) {
	dataDir := t.TempDir()
	dir := seedTicketOracle(t, dataDir, request.StatePlanReview)
	if err := os.WriteFile(filepath.Join(dir, "huge_oracle_test.go"), make([]byte, 3<<20), 0o600); err != nil {
		t.Fatal(err)
	}
	rec := doOracle(t, dataDir, http.MethodGet, "/requests/req-1/tickets/1/oracle", "read", "")
	var view ticketOracleListingView
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	for _, f := range view.Files {
		if f.Name == "huge_oracle_test.go" {
			t.Fatalf("over-cap file was hashed and listed: %+v", f)
		}
	}
	if len(view.Problems) != 1 || !strings.Contains(view.Problems[0], "huge_oracle_test.go is too large to serve (3145728 bytes)") {
		t.Fatalf("problems = %v", view.Problems)
	}
}
