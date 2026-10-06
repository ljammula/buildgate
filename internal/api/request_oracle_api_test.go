package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"buildgate/internal/request"
)

const (
	oracleTestCommand = "go test ./.oracle/...\n"
	oracleTestGoFile  = "package x\n\nimport \"testing\"\n\nfunc TestOracleX(t *testing.T) {}\n"
)

func hexSum(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// seedOracleRequest seeds a request in state with a valid request-level oracle.
func seedOracleRequest(t *testing.T, dataDir, id string, state request.State) string {
	t.Helper()
	seedApprovableRequest(t, dataDir, id, state, false)
	dir := filepath.Join(request.Dir(dataDir, id), "oracle")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{"RUN_COMMAND.txt": oracleTestCommand, "oracle_test.go": oracleTestGoFile, "MANIFEST.json": oracleTestManifest} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeOracleSpec(t, dataDir, id)
	return dir
}

// oracleTestManifest maps oracle_test.go to the one criterion of oracleTestSpec:
// approval now requires a manifest that matches the approved spec.
const oracleTestManifest = `[{"criterion": "It works.", "oracle_file": "oracle_test.go", "criterion_index": 1}]`

const oracleTestSpec = "# Spec\n\n## Problem\n\nx\n\n## Acceptance criteria\n\n1. It works.\n\n## Risks\n\nNone.\n"

func writeOracleSpec(t *testing.T, dataDir, id string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(request.Dir(dataDir, id), "spec.md"), []byte(oracleTestSpec), 0o600); err != nil {
		t.Fatal(err)
	}
}

func oracleServer(dataDir string) *Server {
	return NewServer(dataDir, WithOverrideToken("override"), WithReadToken("read"))
}

func doOracle(t *testing.T, dataDir, method, path, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	oracleServer(dataDir).ServeHTTP(rec, requestActionFor(t, method, path, token, body))
	return rec
}

func putBody(cmd string) string {
	b, _ := json.Marshal(map[string]string{"content": cmd, "by": "alice"})
	return string(b)
}

func TestListRequestOracleShape(t *testing.T) {
	dataDir := t.TempDir()
	dir := seedOracleRequest(t, dataDir, "req-1", request.StateOracleReview)
	rec := doOracle(t, dataDir, http.MethodGet, "/requests/req-1/oracle", "read", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var got struct {
		Files []struct {
			Name   string `json:"name"`
			Size   int64  `json:"size"`
			SHA256 string `json:"sha256"`
		} `json:"files"`
		Problems    []string        `json:"problems"`
		OracleDraft json.RawMessage `json:"oracle_draft"`
		State       string          `json:"state"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.State != "oracle_review" || len(got.Files) != 3 || len(got.Problems) != 0 {
		t.Fatalf("got %+v", got)
	}
	if got.Files[0].Name != "MANIFEST.json" || got.Files[1].Name != "RUN_COMMAND.txt" || got.Files[1].SHA256 != hexSum(oracleTestCommand) || got.Files[1].Size != int64(len(oracleTestCommand)) {
		t.Errorf("files 0,1 = %+v %+v", got.Files[0], got.Files[1])
	}
	if got.Files[2].Name != "oracle_test.go" || got.Files[2].SHA256 != hexSum(oracleTestGoFile) {
		t.Errorf("file 2 = %+v", got.Files[2])
	}

	// Everything approval would refuse is reported, not dropped.
	if err := os.WriteFile(filepath.Join(dir, ".hidden"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.swp"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "nested"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc/passwd", filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	rec = doOracle(t, dataDir, http.MethodGet, "/requests/req-1/oracle", "read", "")
	var bad struct {
		Files    []request.OracleFile `json:"files"`
		Problems []string             `json:"problems"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &bad); err != nil {
		t.Fatal(err)
	}
	if len(bad.Files) != 3 || len(bad.Problems) != 4 {
		t.Fatalf("files=%v problems=%v", bad.Files, bad.Problems)
	}
	joined := strings.Join(bad.Problems, "\n")
	for _, want := range []string{".hidden", "a.swp", "nested", "link"} {
		if !strings.Contains(joined, want) {
			t.Errorf("problems do not name %q: %s", want, joined)
		}
	}
}

func TestListRequestOracleAbsentDirAndMissingRequest(t *testing.T) {
	dataDir := t.TempDir()
	seedApprovableRequest(t, dataDir, "req-1", request.StateSpecReview, false)
	rec := doOracle(t, dataDir, http.MethodGet, "/requests/req-1/oracle", "read", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"files":[]`) || !strings.Contains(rec.Body.String(), `"problems":[]`) {
		t.Fatalf("absent dir: %d %s", rec.Code, rec.Body.String())
	}
	if rec := doOracle(t, dataDir, http.MethodGet, "/requests/nope/oracle", "read", ""); rec.Code != http.StatusNotFound {
		t.Errorf("missing request = %d", rec.Code)
	}
}

func TestGetRequestOracleFile(t *testing.T) {
	dataDir := t.TempDir()
	dir := seedOracleRequest(t, dataDir, "req-1", request.StateOracleReview)
	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "leak.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "big.txt"), []byte(strings.Repeat("a", request.MaxOracleFileReadBytes+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".dot"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o750); err != nil {
		t.Fatal(err)
	}

	rec := doOracle(t, dataDir, http.MethodGet, "/requests/req-1/oracle/RUN_COMMAND.txt", "read", "")
	if rec.Code != http.StatusOK || rec.Body.String() != oracleTestCommand {
		t.Fatalf("ok read: %d %q", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("Content-Type = %q", ct)
	}
	if h := rec.Header().Get("X-Content-SHA256"); h != hexSum(oracleTestCommand) {
		t.Errorf("hash header = %q", h)
	}

	for name, tc := range map[string]struct {
		path string
		want int
	}{
		"traversal":       {"/requests/req-1/oracle/..%2Fspec.md", http.StatusBadRequest},
		"encoded slash":   {"/requests/req-1/oracle/a%2Fb", http.StatusBadRequest},
		"backslash":       {"/requests/req-1/oracle/a%5Cb", http.StatusBadRequest},
		"nul":             {"/requests/req-1/oracle/a%00b", http.StatusBadRequest},
		"dotfile":         {"/requests/req-1/oracle/.dot", http.StatusBadRequest},
		"swap":            {"/requests/req-1/oracle/x.swp", http.StatusBadRequest},
		"nested dir":      {"/requests/req-1/oracle/sub", http.StatusNotFound},
		"symlink outside": {"/requests/req-1/oracle/leak.txt", http.StatusNotFound},
		"oversize":        {"/requests/req-1/oracle/big.txt", http.StatusRequestEntityTooLarge},
		"missing":         {"/requests/req-1/oracle/none.go", http.StatusNotFound},
	} {
		t.Run(name, func(t *testing.T) {
			rec := doOracle(t, dataDir, http.MethodGet, tc.path, "read", "")
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tc.want, rec.Body.String())
			}
			if strings.Contains(rec.Body.String(), "secret") {
				t.Fatal("symlink target leaked")
			}
		})
	}
}

func TestOracleEndpointsRequireAuth(t *testing.T) {
	dataDir := t.TempDir()
	seedOracleRequest(t, dataDir, "req-1", request.StateOracleReview)
	for _, tc := range []struct{ method, path, body string }{
		{http.MethodGet, "/requests/req-1/oracle", ""},
		{http.MethodGet, "/requests/req-1/oracle/RUN_COMMAND.txt", ""},
		{http.MethodPut, "/requests/req-1/oracle/RUN_COMMAND.txt", putBody("go test ./.oracle/...")},
	} {
		for _, token := range []string{"", "wrong"} {
			if rec := doOracle(t, dataDir, tc.method, tc.path, token, tc.body); rec.Code != http.StatusForbidden {
				t.Errorf("%s %s token %q = %d, want 403", tc.method, tc.path, token, rec.Code)
			}
		}
	}
	// The read token must not be able to write.
	if rec := doOracle(t, dataDir, http.MethodPut, "/requests/req-1/oracle/RUN_COMMAND.txt", "read", putBody("go test ./.oracle/...")); rec.Code != http.StatusForbidden {
		t.Errorf("read token PUT = %d", rec.Code)
	}
	got, _ := os.ReadFile(filepath.Join(request.Dir(dataDir, "req-1"), "oracle", "RUN_COMMAND.txt"))
	if string(got) != oracleTestCommand {
		t.Errorf("unauthorized PUT changed the file: %q", got)
	}
}

func TestPutOracleRunCommandOnlyAtOracleReview(t *testing.T) {
	for _, state := range []request.State{request.StateSpecReview, request.StateOracleDrafting, request.StatePlanReview, request.StatePlanning} {
		dataDir := t.TempDir()
		dir := seedOracleRequest(t, dataDir, "req-1", state)
		rec := doOracle(t, dataDir, http.MethodPut, "/requests/req-1/oracle/RUN_COMMAND.txt", "override", putBody("go test -count=1 ./.oracle/..."))
		if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "oracle_review") {
			t.Errorf("state %s: %d %s", state, rec.Code, rec.Body.String())
		}
		got, _ := os.ReadFile(filepath.Join(dir, "RUN_COMMAND.txt"))
		if string(got) != oracleTestCommand {
			t.Errorf("state %s: file changed", state)
		}
	}
}

func TestPutOracleOnlyRunCommand(t *testing.T) {
	dataDir := t.TempDir()
	dir := seedOracleRequest(t, dataDir, "req-1", request.StateOracleReview)
	for _, name := range []string{"oracle_test.go", "new_test.go", ".hidden", "..%2Fspec.md", "run_command.txt"} {
		rec := doOracle(t, dataDir, http.MethodPut, "/requests/req-1/oracle/"+name, "override", putBody("x"))
		if rec.Code != http.StatusForbidden {
			t.Errorf("PUT %s = %d, want 403", name, rec.Code)
		}
	}
	got, _ := os.ReadFile(filepath.Join(dir, "oracle_test.go"))
	if string(got) != oracleTestGoFile {
		t.Error("oracle test file was modified")
	}
	if _, err := os.Stat(filepath.Join(dir, "new_test.go")); err == nil {
		t.Error("PUT created a new file")
	}
}

// TestPutOracleRunCommandBaseSHA256Mismatch covers the same
// editor/Approve race as the spec route, for the oracle route: a
// base_sha256 that doesn't match RUN_COMMAND.txt's current content is
// refused with 409 and the current hash, without writing.
func TestPutOracleRunCommandBaseSHA256Mismatch(t *testing.T) {
	dataDir := t.TempDir()
	dir := seedOracleRequest(t, dataDir, "req-1", request.StateOracleReview)

	body, _ := json.Marshal(updateRequestContentBody{Content: "go test -count=1 ./.oracle/...\n", BaseSHA256: strings.Repeat("0", 64)})
	rec := doOracle(t, dataDir, http.MethodPut, "/requests/req-1/oracle/RUN_COMMAND.txt", "override", string(body))
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d: %s", rec.Code, http.StatusConflict, rec.Body.String())
	}
	var got struct {
		CurrentSHA256 string `json:"current_sha256"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got.CurrentSHA256 != hexSum(oracleTestCommand) {
		t.Errorf("current_sha256 = %q, want the seeded RUN_COMMAND.txt's own hash", got.CurrentSHA256)
	}
	onDisk, err := os.ReadFile(filepath.Join(dir, "RUN_COMMAND.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(onDisk) != oracleTestCommand {
		t.Errorf("RUN_COMMAND.txt was overwritten despite the base_sha256 mismatch")
	}
}

// TestPutOracleRunCommandBaseSHA256Match covers the success path: a
// base_sha256 matching RUN_COMMAND.txt's actual current content is applied.
func TestPutOracleRunCommandBaseSHA256Match(t *testing.T) {
	dataDir := t.TempDir()
	dir := seedOracleRequest(t, dataDir, "req-1", request.StateOracleReview)
	newCmd := "go test -count=1 ./.oracle/...\n"

	body, _ := json.Marshal(updateRequestContentBody{Content: newCmd, BaseSHA256: hexSum(oracleTestCommand)})
	rec := doOracle(t, dataDir, http.MethodPut, "/requests/req-1/oracle/RUN_COMMAND.txt", "override", string(body))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	onDisk, err := os.ReadFile(filepath.Join(dir, "RUN_COMMAND.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(onDisk) != newCmd {
		t.Errorf("RUN_COMMAND.txt = %q, want the edited command applied", string(onDisk))
	}
}

func TestPutOracleValidationFailureChangesNothing(t *testing.T) {
	dataDir := t.TempDir()
	dir := seedOracleRequest(t, dataDir, "req-1", request.StateOracleReview)
	path := filepath.Join(dir, "RUN_COMMAND.txt")
	before, _ := os.Stat(path)
	past := time.Now().Add(-time.Hour)
	if err := os.Chtimes(path, past, past); err != nil {
		t.Fatal(err)
	}
	before, _ = os.Stat(path)

	for name, cmd := range map[string]string{
		"empty":        "  \n",
		"wildcard":     "go test ./...\n",
		"comment only": "go test ./... # .oracle\n",
		"nul":          "go test ./.oracle/...\x00",
		"oversize":     "go test ./.oracle/... " + strings.Repeat("a", request.MaxOracleRunCommandBytes),
	} {
		t.Run(name, func(t *testing.T) {
			rec := doOracle(t, dataDir, http.MethodPut, "/requests/req-1/oracle/RUN_COMMAND.txt", "override", putBody(cmd))
			if rec.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
			}
			after, _ := os.Stat(path)
			got, _ := os.ReadFile(path)
			if string(got) != oracleTestCommand || !after.ModTime().Equal(before.ModTime()) {
				t.Errorf("file changed: %q %v", got, after.ModTime())
			}
			entries, _ := os.ReadDir(dir)
			if len(entries) != 3 {
				t.Errorf("stray files left in oracle/: %v", entries)
			}
		})
	}

}

// TestPutOracleSucceedsDespiteBrokenSiblingsAndLeftoverTemp: the command must
// stay repairable however broken the rest of oracle/ is (stray/unsupported
// files, a crash-leftover .tmp); approval still refuses such a directory, and
// the PUT leaves no temp file behind in oracle/ or the request dir.
func TestPutOracleSucceedsDespiteBrokenSiblingsAndLeftoverTemp(t *testing.T) {
	dataDir := t.TempDir()
	dir := seedOracleRequest(t, dataDir, "req-1", request.StateOracleReview)
	for _, name := range []string{"RUN_COMMAND.txt.123.tmp", "helper.txt", ".hidden"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	newCmd := "go test -count=1 ./.oracle/...\n"
	rec := doOracle(t, dataDir, http.MethodPut, "/requests/req-1/oracle/RUN_COMMAND.txt", "override", putBody(newCmd))
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT = %d: %s", rec.Code, rec.Body.String())
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "RUN_COMMAND.txt")); string(got) != newCmd {
		t.Errorf("content = %q", got)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 6 {
		t.Errorf("PUT added or removed files in oracle/: %v", entries)
	}
	reqEntries, _ := os.ReadDir(request.Dir(dataDir, "req-1"))
	for _, e := range reqEntries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("temp file left in request dir: %s", e.Name())
		}
	}
	if rec := doOracle(t, dataDir, http.MethodPost, "/requests/req-1/approve", "override", `{"by":"a"}`); rec.Code == http.StatusOK {
		t.Errorf("approval accepted a directory with stray files: %s", rec.Body.String())
	}
	// A multi-line command is accepted as approval accepts it.
	rec = doOracle(t, dataDir, http.MethodPut, "/requests/req-1/oracle/RUN_COMMAND.txt", "override", putBody("go vet ./...\ngo test ./.oracle/...\n"))
	if rec.Code != http.StatusOK {
		t.Errorf("multi-line PUT = %d: %s", rec.Code, rec.Body.String())
	}
}

func TestPutOracleSuccessThenApprovePinsThatHash(t *testing.T) {
	dataDir := t.TempDir()
	dir := seedOracleRequest(t, dataDir, "req-1", request.StateOracleReview)
	newCmd := "go test -count=1 ./.oracle/...\n"
	rec := doOracle(t, dataDir, http.MethodPut, "/requests/req-1/oracle/RUN_COMMAND.txt", "override", putBody(newCmd))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var got struct{ Name, SHA256 string }
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.SHA256 != hexSum(newCmd) {
		t.Fatalf("sha = %s", got.SHA256)
	}
	info, err := os.Stat(filepath.Join(dir, "RUN_COMMAND.txt"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v err %v", info.Mode(), err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 3 {
		t.Errorf("temp file left behind: %v", entries)
	}

	// Stale (pre-edit) hash is refused with 409 (an adversarial review,
	// 2026-09-24, found: request.ErrApprovalStale now maps to 409, not
	// the generic 400 every other request.Approve error keeps -- see
	// approveRequest's own doc comment); the full, fresh map approves and
	// pins the edited hash.
	stale := `{"by":"a","expected_sha256":{"oracle/RUN_COMMAND.txt":"` + hexSum(oracleTestCommand) + `","oracle/oracle_test.go":"` + hexSum(oracleTestGoFile) + `","oracle/MANIFEST.json":"` + hexSum(oracleTestManifest) + `"}}`
	if rec := doOracle(t, dataDir, http.MethodPost, "/requests/req-1/approve", "override", stale); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), request.ErrApprovalStale.Error()) {
		t.Fatalf("stale approve = %d %s", rec.Code, rec.Body.String())
	}
	omit := `{"by":"a","expected_sha256":{"oracle/RUN_COMMAND.txt":"` + got.SHA256 + `"}}`
	if rec := doOracle(t, dataDir, http.MethodPost, "/requests/req-1/approve", "override", omit); rec.Code != http.StatusConflict {
		t.Fatalf("omitting a file = %d, want 409: %s", rec.Code, rec.Body.String())
	}
	full := `{"by":"a","expected_sha256":{"oracle/RUN_COMMAND.txt":"` + got.SHA256 + `","oracle/oracle_test.go":"` + hexSum(oracleTestGoFile) + `","oracle/MANIFEST.json":"` + hexSum(oracleTestManifest) + `"}}`
	if rec := doOracle(t, dataDir, http.MethodPost, "/requests/req-1/approve", "override", full); rec.Code != http.StatusOK {
		t.Fatalf("full approve = %d %s", rec.Code, rec.Body.String())
	}
	loaded, err := request.Load(dataDir, "req-1")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.ApprovedSHA256["oracle/RUN_COMMAND.txt"] != got.SHA256 {
		t.Errorf("pinned %q, want %q", loaded.ApprovedSHA256["oracle/RUN_COMMAND.txt"], got.SHA256)
	}
}

// TestPutOracleConcurrentWithApprove: PUT and approve contend for the request
// lock; in either order the pinned hash equals the file's final content, and
// the PUT status agrees with which order won (200 before approval, 409 after).
func TestPutOracleConcurrentWithApprove(t *testing.T) {
	newCmd := "go test -count=1 ./.oracle/...\n"
	sawEditFirst, sawApproveFirst := false, false
	for i := 0; i < 40; i++ {
		dataDir := t.TempDir()
		dir := seedOracleRequest(t, dataDir, "req-1", request.StateOracleReview)
		srv := oracleServer(dataDir)
		var wg sync.WaitGroup
		var putCode int
		var approveErr error
		wg.Add(2)
		go func() {
			defer wg.Done()
			rec := httptest.NewRecorder()
			srv.ServeHTTP(rec, requestActionFor(t, http.MethodPut, "/requests/req-1/oracle/RUN_COMMAND.txt", "override", putBody(newCmd)))
			putCode = rec.Code
		}()
		go func() {
			defer wg.Done()
			// The unconditional (CLI) approve: same lock and pin as the API's.
			_, approveErr = request.Approve(dataDir, "req-1", "alice", time.Now(), nil)
		}()
		wg.Wait()
		if approveErr != nil {
			t.Fatalf("iteration %d: approve: %v", i, approveErr)
		}
		loaded, err := request.Load(dataDir, "req-1")
		if err != nil {
			t.Fatal(err)
		}
		content, err := os.ReadFile(filepath.Join(dir, "RUN_COMMAND.txt"))
		if err != nil {
			t.Fatal(err)
		}
		if pinned := loaded.ApprovedSHA256["oracle/RUN_COMMAND.txt"]; pinned != hexSum(string(content)) {
			t.Fatalf("iteration %d: pinned %s but file content hashes to %s", i, pinned, hexSum(string(content)))
		}
		switch putCode {
		case http.StatusOK:
			sawEditFirst = true
			if string(content) != newCmd {
				t.Fatalf("iteration %d: PUT 200 but content %q", i, content)
			}
		case http.StatusConflict:
			sawApproveFirst = true
			if string(content) != oracleTestCommand {
				t.Fatalf("iteration %d: PUT refused but content changed: %q", i, content)
			}
		default:
			t.Fatalf("iteration %d: PUT = %d", i, putCode)
		}
	}
	t.Logf("edit-first seen=%v approve-first seen=%v", sawEditFirst, sawApproveFirst)
}

// TestPutOracleRunCommandBaseSHA256CheckedUnderLock locks in a fix from
// an adversarial review (2026-09-24): two concurrent PUTs both carry the
// SAME base_sha256 (RUN_COMMAND.txt's original hash) and race each
// other. Before this, the base_sha256 check read the file
// (request.GetOracleFile) BEFORE SetOracleRunCommand ever took its own
// request lock, so both goroutines could read the same original content,
// both pass the check, and both go on to write -- the second write
// silently clobbering the first's, with neither call ever reporting 409.
// With the check moved inside SetOracleRunCommand's own lock (checked
// against the file's real current content at that moment, not a value
// read before the lock was acquired), exactly one PUT can ever succeed
// per race: the other sees the first's already-applied new content and
// is refused with 409, never silently overwriting it.
func TestPutOracleRunCommandBaseSHA256CheckedUnderLock(t *testing.T) {
	newCmdA := "go test -count=1 ./.oracle/...\n"
	newCmdB := "go test -count=2 ./.oracle/...\n"
	for i := 0; i < 40; i++ {
		dataDir := t.TempDir()
		seedOracleRequest(t, dataDir, "req-1", request.StateOracleReview)
		srv := oracleServer(dataDir)
		base := hexSum(oracleTestCommand)
		bodyA, _ := json.Marshal(updateRequestContentBody{Content: newCmdA, BaseSHA256: base})
		bodyB, _ := json.Marshal(updateRequestContentBody{Content: newCmdB, BaseSHA256: base})

		var wg sync.WaitGroup
		var codeA, codeB int
		wg.Add(2)
		go func() {
			defer wg.Done()
			rec := httptest.NewRecorder()
			srv.ServeHTTP(rec, requestActionFor(t, http.MethodPut, "/requests/req-1/oracle/RUN_COMMAND.txt", "override", string(bodyA)))
			codeA = rec.Code
		}()
		go func() {
			defer wg.Done()
			rec := httptest.NewRecorder()
			srv.ServeHTTP(rec, requestActionFor(t, http.MethodPut, "/requests/req-1/oracle/RUN_COMMAND.txt", "override", string(bodyB)))
			codeB = rec.Code
		}()
		wg.Wait()

		succeeded := 0
		if codeA == http.StatusOK {
			succeeded++
		}
		if codeB == http.StatusOK {
			succeeded++
		}
		if succeeded != 1 {
			t.Fatalf("iteration %d: codes A=%d B=%d, want exactly one 200 and one 409 -- base_sha256 must be checked against the file's real current content under the same lock as the write, not a value read before the lock was taken", i, codeA, codeB)
		}
	}
}
