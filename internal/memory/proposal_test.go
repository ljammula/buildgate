package memory

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
		path := filepath.Join(t.TempDir(), "p.json")
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, ok, err := LoadProposal(path); err == nil || ok {
			t.Errorf("%s: ok=%v err=%v, want an error", name, ok, err)
		}
	}
}
