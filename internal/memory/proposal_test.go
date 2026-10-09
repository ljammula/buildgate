package memory

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestProposalPathRefusesUnsafeComponents(t *testing.T) {
	good, err := ProposalPath("/d", "widget", "req-1")
	if err != nil || good != filepath.Join("/d", "memory", "widget", "proposals", "req-1.json") {
		t.Fatalf("ProposalPath = %q, %v", good, err)
	}
	bad := []string{"", ".", "..", "a/b", `a\b`, "../x", "x/..", "a\x00b", "a\nb", strings.Repeat("x", 201), "\xff"}
	for _, b := range bad {
		if _, err := ProposalPath("/d", b, "r"); err == nil {
			t.Errorf("project %q accepted", b)
		}
		if _, err := ProposalPath("/d", "p", b); err == nil {
			t.Errorf("request id %q accepted", b)
		}
	}
	if _, err := ProposalPath("", "p", "r"); err == nil {
		t.Error("empty data dir accepted")
	}
}

func TestProposalRoundTrip(t *testing.T) {
	path, _ := ProposalPath(t.TempDir(), "widget", "req-1")
	in := Proposal{RequestID: "req-1", LessonIDs: []string{"l1"}, BaseBlobSHA256: "", Expected: "text\n"}
	if err := SaveProposal(path, in); err != nil {
		t.Fatal(err)
	}
	got, ok, err := LoadProposal(path)
	if err != nil || !ok {
		t.Fatalf("LoadProposal = %v, %v", ok, err)
	}
	if got.Expected != "text\n" || got.ExpectedSHA256 != HashHex([]byte("text\n")) || got.RequestID != "req-1" || got.SchemaVersion != 1 {
		t.Fatalf("got %+v", got)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Errorf("file mode %v", info.Mode().Perm())
	}
	if info, _ := os.Stat(filepath.Dir(path)); info.Mode().Perm() != 0o700 {
		t.Errorf("dir mode %v", info.Mode().Perm())
	}
	if entries, _ := os.ReadDir(filepath.Dir(path)); len(entries) != 1 {
		t.Errorf("directory holds %d entries, want only the proposal", len(entries))
	}
}

func TestLoadProposalMissingFile(t *testing.T) {
	_, ok, err := LoadProposal(filepath.Join(t.TempDir(), "none.json"))
	if ok || err != nil {
		t.Fatalf("ok=%v err=%v, want false, nil", ok, err)
	}
}

func TestLoadProposalRefusals(t *testing.T) {
	hash := HashHex([]byte("x"))
	cases := map[string]string{
		"hash mismatch": `{"schema_version":1,"request_id":"r","lesson_ids":[],"base_blob_sha256":"","expected_sha256":"` + strings.Repeat("0", 64) + `","expected":"x"}`,
		"unknown field": `{"schema_version":1,"request_id":"r","lesson_ids":[],"base_blob_sha256":"","expected_sha256":"` + hash + `","expected":"x","extra":1}`,
		"wrong version": `{"schema_version":2,"request_id":"r","lesson_ids":[],"base_blob_sha256":"","expected_sha256":"` + hash + `","expected":"x"}`,
		"trailing data": `{"schema_version":1,"request_id":"r","lesson_ids":[],"base_blob_sha256":"","expected_sha256":"` + hash + `","expected":"x"} {}`,
		"not json":      `nope`,
		"oversize":      `{"schema_version":1,"expected":"` + strings.Repeat("a", MaxProposalBytes) + `"}`,
	}
	for name, body := range cases {
		path := filepath.Join(t.TempDir(), "r.json")
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, ok, err := LoadProposal(path); err == nil || ok {
			t.Errorf("%s: ok=%v err=%v, want an error", name, ok, err)
		}
	}
}

// loadWithin fails the test when a load does not return: a FIFO with no
// writer blocks a plain open for ever.
func loadWithin(t *testing.T, load func() (Proposal, bool, error)) (Proposal, bool, error) {
	t.Helper()
	type result struct {
		p   Proposal
		ok  bool
		err error
	}
	done := make(chan result, 1)
	go func() {
		p, ok, err := load()
		done <- result{p, ok, err}
	}()
	select {
	case r := <-done:
		return r.p, r.ok, r.err
	case <-time.After(10 * time.Second):
		t.Fatal("the load did not return")
		return Proposal{}, false, nil
	}
}

func TestLoadProposalRefusesAFileRecordedForAnotherRequest(t *testing.T) {
	dataDir := t.TempDir()
	other, _ := ProposalPath(dataDir, "widget", "req-other")
	if err := SaveProposal(other, Proposal{RequestID: "req-other", Expected: "text\n"}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(other)
	if err != nil {
		t.Fatal(err)
	}
	mine, _ := ProposalPath(dataDir, "widget", "req-1")
	if err := os.WriteFile(mine, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := LoadProposal(mine); err == nil || ok {
		t.Fatalf("LoadProposal: ok=%v err=%v, want an error for a file whose request_id is another request's", ok, err)
	}
	if _, ok, err := LoadProposalFor(dataDir, "widget", "req-1"); err == nil || ok {
		t.Fatalf("LoadProposalFor: ok=%v err=%v, want an error", ok, err)
	}
	if p, ok, err := LoadProposalFor(dataDir, "widget", "req-other"); err != nil || !ok || p.RequestID != "req-other" {
		t.Fatalf("the request's own file: %+v, %v, %v", p, ok, err)
	}
	if _, ok, err := LoadProposalFor(dataDir, "widget", "req-none"); err != nil || ok {
		t.Fatalf("no file: ok=%v err=%v, want false, nil", ok, err)
	}
	if _, ok, err := LoadProposalFor(t.TempDir(), "widget", "req-1"); err != nil || ok {
		t.Fatalf("no memory directory: ok=%v err=%v, want false, nil", ok, err)
	}
}

func TestLoadProposalRefusesASymlink(t *testing.T) {
	dataDir := t.TempDir()
	real, _ := ProposalPath(dataDir, "widget", "req-1")
	if err := SaveProposal(real, Proposal{RequestID: "req-1", Expected: "text\n"}); err != nil {
		t.Fatal(err)
	}
	// The file itself is a link, to a proposal that would otherwise load.
	elsewhere := filepath.Join(t.TempDir(), "req-2.json")
	data, _ := os.ReadFile(real)
	if err := os.WriteFile(elsewhere, []byte(strings.Replace(string(data), "req-1", "req-2", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	link, _ := ProposalPath(dataDir, "widget", "req-2")
	if err := os.Symlink(elsewhere, link); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := LoadProposal(link); err == nil || ok {
		t.Fatalf("a symlinked file: ok=%v err=%v, want an error", ok, err)
	}
	// The proposals directory is a link to a real one.
	linkedData := t.TempDir()
	if err := os.MkdirAll(filepath.Join(linkedData, "memory", "widget"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Dir(real), filepath.Join(linkedData, "memory", "widget", "proposals")); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := LoadProposalFor(linkedData, "widget", "req-1"); err == nil || ok {
		t.Fatalf("a symlinked proposals directory: ok=%v err=%v, want an error", ok, err)
	}
	// The project directory is a link to a real one.
	linkedProject := t.TempDir()
	if err := os.MkdirAll(filepath.Join(linkedProject, "memory"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dataDir, "memory", "widget"), filepath.Join(linkedProject, "memory", "widget")); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := LoadProposalFor(linkedProject, "widget", "req-1"); err == nil || ok {
		t.Fatalf("a symlinked project directory: ok=%v err=%v, want an error", ok, err)
	}
}

func TestLoadProposalDoesNotBlockOnAFIFO(t *testing.T) {
	dataDir := t.TempDir()
	path, _ := ProposalPath(dataDir, "widget", "req-1")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	_, ok, err := loadWithin(t, func() (Proposal, bool, error) { return LoadProposalFor(dataDir, "widget", "req-1") })
	if err == nil || ok {
		t.Fatalf("a FIFO: ok=%v err=%v, want an error", ok, err)
	}
}
