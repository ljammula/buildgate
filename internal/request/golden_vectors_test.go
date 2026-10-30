package request

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"testing"
)

// contentHashVectorsPath is the golden file the console's own tests read
// too (console/src/domain/contentHash.test.ts): one set of inputs and expected
// digests, so the client that builds an approval's expected_sha256 and the
// server that checks it cannot drift apart unnoticed.
const contentHashVectorsPath = "../../console/test/fixtures/vectors/content-hash.json"

type contentHashVectors struct {
	Digests []struct {
		Name   string  `json:"name"`
		Text   *string `json:"text"`
		Base64 *string `json:"base64"`
		SHA256 string  `json:"sha256"`
	} `json:"digests"`
	Approvals []struct {
		Name    string `json:"name"`
		State   State  `json:"state"`
		Spec    string `json:"spec"`
		Tickets []struct {
			Index    int    `json:"index"`
			SpecPath string `json:"spec_path"`
			Content  string `json:"content"`
		} `json:"tickets"`
		Want map[string]string `json:"want"`
	} `json:"approvals"`
	ReadBackAnswers []struct {
		Name      string             `json:"name"`
		Questions []ReadBackQuestion `json:"questions"`
		SHA256    string             `json:"sha256"`
	} `json:"read_back_answers"`
}

func loadContentHashVectors(t *testing.T) contentHashVectors {
	t.Helper()
	raw, err := os.ReadFile(contentHashVectorsPath)
	if err != nil {
		t.Fatalf("read golden vectors: %v", err)
	}
	var v contentHashVectors
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("decode golden vectors: %v", err)
	}
	if len(v.Digests) == 0 || len(v.Approvals) == 0 || len(v.ReadBackAnswers) == 0 {
		t.Fatalf("golden vectors are missing a section: %d digests, %d approvals, %d read-back answers", len(v.Digests), len(v.Approvals), len(v.ReadBackAnswers))
	}
	return v
}

// TestContentHashGoldenVectorDigests hashes every vector both ways the
// server hashes approved content: sha256Hex over bytes in memory
// and HashFile over a file on disk (spec.md, ticket specs).
func TestContentHashGoldenVectorDigests(t *testing.T) {
	for _, d := range loadContentHashVectors(t).Digests {
		t.Run(d.Name, func(t *testing.T) {
			var content []byte
			switch {
			case d.Text != nil && d.Base64 == nil:
				content = []byte(*d.Text)
			case d.Base64 != nil && d.Text == nil:
				decoded, err := base64.StdEncoding.DecodeString(*d.Base64)
				if err != nil {
					t.Fatalf("decode base64: %v", err)
				}
				content = decoded
			default:
				t.Fatal("a digest vector carries exactly one of text and base64")
			}
			if got := sha256Hex(content); got != d.SHA256 {
				t.Errorf("sha256Hex = %s, want %s", got, d.SHA256)
			}
			dataDir := t.TempDir()
			if err := os.MkdirAll(Dir(dataDir, "req-1"), 0o750); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(Dir(dataDir, "req-1"), specFileName), content, 0o600); err != nil {
				t.Fatal(err)
			}
			got, err := HashFile(dataDir, "req-1", specFileName)
			if err != nil {
				t.Fatalf("HashFile: %v", err)
			}
			if got != d.SHA256 {
				t.Errorf("HashFile = %s, want %s", got, d.SHA256)
			}
		})
	}
}

// TestContentHashGoldenVectorApprovals writes each vector's request to disk
// and approves it with the vector's expected map through ApproveShown, the
// entry point the HTTP API uses: the keys and digests a console computes
// from what it displayed are the ones the server binds the approval to.
func TestContentHashGoldenVectorApprovals(t *testing.T) {
	for _, a := range loadContentHashVectors(t).Approvals {
		t.Run(a.Name, func(t *testing.T) {
			dataDir := t.TempDir()
			const id = "req-1"
			if err := SaveText(dataDir, id, "the original request text"); err != nil {
				t.Fatalf("SaveText: %v", err)
			}
			r := New(id, "/repos/app", "app", Source{Kind: SourceText}, fixedNow)
			r.State = a.State
			if err := os.WriteFile(filepath.Join(Dir(dataDir, id), specFileName), []byte(a.Spec), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Join(Dir(dataDir, id), "tickets"), 0o750); err != nil {
				t.Fatal(err)
			}
			for _, ticket := range a.Tickets {
				name := fmt.Sprintf("%03d.spec.md", ticket.Index)
				if err := os.WriteFile(filepath.Join(Dir(dataDir, id), "tickets", name), []byte(ticket.Content), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if err := r.Save(dataDir); err != nil {
				t.Fatalf("Save: %v", err)
			}

			if _, gated := NextApprovalState(r); !gated {
				if len(a.Want) != 0 {
					t.Fatalf("state %q has no review gate, so the vector must bind nothing; it wants %v", a.State, a.Want)
				}
				return
			}
			approved, err := ApproveShown(dataDir, id, "alice", fixedNow, a.Want)
			if err != nil {
				t.Fatalf("ApproveShown with the vector's expected map: %v", err)
			}
			files := maps.Clone(a.Want)
			delete(files, ReadBackAnswersKey)
			if len(approved.ApprovedSHA256) != len(files) {
				t.Errorf("ApprovedSHA256 = %v, want exactly the vector's files %v", approved.ApprovedSHA256, files)
			}
			for relPath, want := range files {
				if got := approved.ApprovedSHA256[relPath]; got != want {
					t.Errorf("ApprovedSHA256[%q] = %q, want %q", relPath, got, want)
				}
			}
		})
	}
}

// TestContentHashGoldenVectorReadBackAnswers pins the digest of the read-back
// answers a console showed (ReadBackShownSHA256), which a spec approval sends
// under ReadBackAnswersKey.
func TestContentHashGoldenVectorReadBackAnswers(t *testing.T) {
	for _, v := range loadContentHashVectors(t).ReadBackAnswers {
		t.Run(v.Name, func(t *testing.T) {
			if got := ReadBackShownSHA256(&ReadBack{Questions: v.Questions}); got != v.SHA256 {
				t.Errorf("ReadBackShownSHA256 = %s, want %s", got, v.SHA256)
			}
		})
	}
	if got := ReadBackShownSHA256(nil); got != sha256Hex(nil) {
		t.Errorf("ReadBackShownSHA256(nil) = %s, want the digest of no bytes", got)
	}
}
