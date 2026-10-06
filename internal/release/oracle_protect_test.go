package release

import (
	"reflect"
	"testing"

	"buildgate/internal/run"
)

const protOracle = "internal/mood/zz_oracle_test.go"

func TestProtectedFilesTouchedAlwaysProtectsBuildgateDir(t *testing.T) {
	got := MergePolicy{}.ProtectedFilesTouched([]string{".buildgate/oracles.json", ".buildgate/x", "main.go"})
	if !reflect.DeepEqual(got, []string{".buildgate/oracles.json", ".buildgate/x"}) {
		t.Fatalf("got %v", got)
	}
	r := cleanRun()
	r.ChangedFiles = []string{".buildgate/notes.txt"}
	assertMergePolicyReason(t, r, cleanPolicy(), ".buildgate/notes.txt")
}

func TestMergePolicyNoBaseIndexMeansNoExtraProtection(t *testing.T) {
	// A repo with no oracle index: an ordinary *_oracle_test.go edit is an
	// ordinary change, exactly as before this mechanism existed.
	r := cleanRun()
	r.ChangedFiles = append(r.ChangedFiles, protOracle)
	if passed, reasons := MergePolicyCheck(r, cleanPolicy()); !passed {
		t.Fatalf("no index at base must add no protection: %v", reasons)
	}
}

func TestMergePolicyBaseIndexProtectsCommittedOracleFromAgentEdits(t *testing.T) {
	r := cleanRun()
	r.ChangedFiles = append(r.ChangedFiles, protOracle)
	r.Oracles = &run.OracleEvidence{
		BaseIndexPaths: []string{protOracle},
		ResultSHA256:   map[string]string{protOracle: "agent-edit"},
		BaseSHA256:     map[string]string{protOracle: "original"},
	}
	assertMergePolicyReason(t, r, cleanPolicy(), protOracle)

	// The agent deleting the oracle is tamper too.
	r.Oracles = &run.OracleEvidence{BaseIndexPaths: []string{protOracle}, BaseSHA256: map[string]string{protOracle: "original"}}
	assertMergePolicyReason(t, r, cleanPolicy(), protOracle)
}

func TestMergePolicyFactoryWriteExemptOnlyWhenHashEqual(t *testing.T) {
	authored := []run.OracleFile{{Path: protOracle, SHA256: "pinned"}, {Path: ".buildgate/oracles.json", SHA256: "idx"}}
	r := cleanRun()
	r.ChangedFiles = append(r.ChangedFiles, protOracle, ".buildgate/oracles.json")
	r.Oracles = &run.OracleEvidence{
		Authored:     authored,
		ResultSHA256: map[string]string{protOracle: "pinned", ".buildgate/oracles.json": "idx"},
	}
	if passed, reasons := MergePolicyCheck(r, cleanPolicy()); !passed {
		t.Fatalf("hash-equal factory writes must be exempt: %v", reasons)
	}

	// Same path, different bytes at ResultSHA: exemption is keyed on the hash.
	r.Oracles = &run.OracleEvidence{
		Authored:     authored,
		ResultSHA256: map[string]string{protOracle: "DIFFERENT", ".buildgate/oracles.json": "idx"},
	}
	assertMergePolicyReason(t, r, cleanPolicy(), protOracle)

	// A tampered index is caught too.
	r.Oracles = &run.OracleEvidence{
		Authored:     authored,
		ResultSHA256: map[string]string{protOracle: "pinned", ".buildgate/oracles.json": "forged"},
	}
	assertMergePolicyReason(t, r, cleanPolicy(), ".buildgate/oracles.json")
}

func TestMergePolicySupersessionDeleteExemptOnlyWhenDeclaredAndAbsent(t *testing.T) {
	old := "internal/mood/old_oracle_test.go"
	r := cleanRun()
	r.ChangedFiles = append(r.ChangedFiles, old)
	r.Oracles = &run.OracleEvidence{
		BaseIndexPaths: []string{old, protOracle},
		Deleted:        []string{old},
		ResultSHA256:   map[string]string{},
	}
	if passed, reasons := MergePolicyCheck(r, cleanPolicy()); !passed {
		t.Fatalf("declared supersession deletion must be exempt: %v", reasons)
	}
	// Deleting a DIFFERENT protected oracle is not covered by that declaration.
	r.ChangedFiles = append(r.ChangedFiles, protOracle)
	assertMergePolicyReason(t, r, cleanPolicy(), protOracle)
	// A "deleted" path that is in fact still present at ResultSHA is not exempt.
	r.ChangedFiles = []string{old}
	r.Oracles.ResultSHA256[old] = "modified"
	assertMergePolicyReason(t, r, cleanPolicy(), old)
}

func TestMergePolicyBaseIndexPinExemptsAnUnchangedCommittedOracle(t *testing.T) {
	// A cumulative -diff-base inventory lists an oracle an EARLIER round
	// committed; the blob at ResultSHA still hashes to what the base index
	// pinned for it, so it is not tamper.
	r := cleanRun()
	r.ChangedFiles = append(r.ChangedFiles, protOracle)
	r.Oracles = &run.OracleEvidence{
		BaseIndexPaths:  []string{protOracle},
		BaseIndexSHA256: map[string]string{protOracle: "pinned"},
		ResultSHA256:    map[string]string{protOracle: "pinned"},
	}
	if passed, reasons := MergePolicyCheck(r, cleanPolicy()); !passed {
		t.Fatalf("an oracle still matching its pinned hash must not be tamper: %v", reasons)
	}
}

// Found via adversarial review: after a quarantined review round advances the
// branch, the NEXT round's base can itself contain a tampered oracle. Byte
// equality with that base must not wave it through; only the index's pinned
// hash does.
func TestMergePolicyTamperedBaseBytesDoNotExemptAnOracle(t *testing.T) {
	r := cleanRun()
	r.ChangedFiles = append(r.ChangedFiles, protOracle)
	r.Oracles = &run.OracleEvidence{
		BaseIndexPaths:  []string{protOracle},
		BaseIndexSHA256: map[string]string{protOracle: "pinned"},
		ResultSHA256:    map[string]string{protOracle: "tampered"},
		BaseSHA256:      map[string]string{protOracle: "tampered"}, // same bytes as the tainted round base
	}
	assertMergePolicyReason(t, r, cleanPolicy(), protOracle)
}

func TestMergePolicyNoPinMeansNoExemption(t *testing.T) {
	r := cleanRun()
	r.ChangedFiles = append(r.ChangedFiles, protOracle)
	r.Oracles = &run.OracleEvidence{
		BaseIndexPaths: []string{protOracle},
		ResultSHA256:   map[string]string{protOracle: "x"},
	}
	assertMergePolicyReason(t, r, cleanPolicy(), protOracle)
}

func TestMergePolicyConfiguredProtectedPathNeverExempt(t *testing.T) {
	// An oracle that a policy also protects by config is never exempt, even
	// when the factory wrote it with a matching hash.
	cfg := cleanPolicy()
	cfg.ProtectedPaths = append(cfg.ProtectedPaths, protOracle)
	r := cleanRun()
	r.ChangedFiles = append(r.ChangedFiles, protOracle)
	r.Oracles = &run.OracleEvidence{
		Authored:     []run.OracleFile{{Path: protOracle, SHA256: "pinned"}},
		ResultSHA256: map[string]string{protOracle: "pinned"},
	}
	assertMergePolicyReason(t, r, cfg, protOracle)
}

func TestFactoryYMLNeverExemptByOracleEvidence(t *testing.T) {
	r := cleanRun()
	r.ChangedFiles = append(r.ChangedFiles, ".factory.yml")
	r.Oracles = &run.OracleEvidence{
		Authored:     []run.OracleFile{{Path: ".factory.yml", SHA256: "x"}},
		ResultSHA256: map[string]string{".factory.yml": "x"},
	}
	assertMergePolicyReason(t, r, cleanPolicy(), ".factory.yml")
}

func TestBuildgateDirProtectionFoldsCase(t *testing.T) {
	r := cleanRun()
	r.ChangedFiles = append(r.ChangedFiles, ".BuildGate/oracles.json", ".BUILDGATE/x")
	assertMergePolicyReason(t, r, cleanPolicy(), ".BuildGate/oracles.json")
}
