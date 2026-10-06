package request

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

const goOracleBody = "package x\n\nimport \"testing\"\n\nfunc TestOracleX(t *testing.T) {}\n"

// matSpec returns an approved-spec body with n numbered acceptance criteria
// ("Criterion i").
func matSpec(n int) string {
	var b strings.Builder
	b.WriteString("# Spec\n\n## Problem\n\nx\n\n## Acceptance criteria\n\n")
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&b, "%d. Criterion %d\n", i, i)
	}
	b.WriteString("\n## Risks\n\nNone.\n")
	return b.String()
}

func matTicket(nums ...int) string {
	var b strings.Builder
	b.WriteString("Verify-Command: true\nAllowed-Files: a.go\nRequired-Changed-Files: a.go\n\n## Goal\n\ng\n\n## Plan\n\n### Files to touch\n\n- a.go\n\n### Steps\n\n1. s\n\n### Tests to add\n\n- t\n\n### Acceptance criteria covered\n\n")
	for _, n := range nums {
		fmt.Fprintf(&b, "- %d\n", n)
	}
	b.WriteString("\n## Out of scope\n\nnone\n")
	return b.String()
}

type matEntry struct {
	Criterion      string   `json:"criterion"`
	OracleFile     *string  `json:"oracle_file"`
	Rationale      string   `json:"rationale,omitempty"`
	CriterionIndex int      `json:"criterion_index"`
	TargetPath     string   `json:"target_path,omitempty"`
	Supersedes     []string `json:"supersedes,omitempty"`
}

func strp(s string) *string { return &s }

// matFixture builds a request already past oracle_review (oracle pinned) and
// in planning, with spec.md of specCriteria criteria, the given request-level
// oracle files (name -> body), MANIFEST.json entries and RUN_COMMAND.txt, and
// one ticket file per element of claims (each a list of claimed criteria).
func matFixture(t *testing.T, specCriteria int, files map[string]string, entries []matEntry, command string, claims [][]int) (string, *Request, []Ticket) {
	t.Helper()
	dataDir, r := matPrepare(t, specCriteria, files, entries, command)
	return matApproveAndPlan(t, dataDir, r, claims)
}

// matPrepare writes spec.md and the request-level oracle for a request at
// oracle_review, without approving it.
func matPrepare(t *testing.T, specCriteria int, files map[string]string, entries []matEntry, command string) (string, *Request) {
	t.Helper()
	dataDir := t.TempDir()
	r := newDraftOraclesRequest(t, dataDir, "req-1", StateOracleReview)
	dir := Dir(dataDir, r.ID)
	if err := os.WriteFile(filepath.Join(dir, specFileName), []byte(matSpec(specCriteria)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, RequestOracleDirName), 0o750); err != nil {
		t.Fatal(err)
	}
	manifest, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	all := map[string]string{ManifestFileName: string(manifest), TicketOracleRunCommandFilename: command}
	for n, b := range files {
		all[n] = b
	}
	for n, b := range all {
		if err := os.WriteFile(filepath.Join(dir, RequestOracleDirName, n), []byte(b), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dataDir, r
}

// matApproveAndPlan approves oracle_review and writes one ticket per claims
// element.
func matApproveAndPlan(t *testing.T, dataDir string, r *Request, claims [][]int) (string, *Request, []Ticket) {
	t.Helper()
	dir := Dir(dataDir, r.ID)
	if _, err := Approve(dataDir, r.ID, "alice", fixedNow, nil); err != nil {
		t.Fatalf("approve oracle_review: %v", err)
	}
	r = mustLoad(t, dataDir, r.ID)
	if r.State != StatePlanning {
		t.Fatalf("state %s, want planning", r.State)
	}
	if err := os.MkdirAll(filepath.Join(dir, "tickets"), 0o750); err != nil {
		t.Fatal(err)
	}
	var tickets []Ticket
	for i, nums := range claims {
		p := filepath.Join(dir, "tickets", fmt.Sprintf("%03d.spec.md", i+1))
		if err := os.WriteFile(p, []byte(matTicket(nums...)), 0o600); err != nil {
			t.Fatal(err)
		}
		tickets = append(tickets, Ticket{Index: i + 1, SpecPath: p})
	}
	return dataDir, r, tickets
}

// threeCriteria is the common shape: oracles a/b/c for criteria 1/2/3, ticket
// 001 claims {1,2}, ticket 002 claims {2,3}.
func threeCriteria(t *testing.T, command string) (string, *Request, []Ticket) {
	t.Helper()
	return matFixture(t, 3,
		map[string]string{"a_oracle_test.go": goOracleBody, "b_oracle_test.go": goOracleBody, "c_oracle_test.go": goOracleBody},
		[]matEntry{
			{Criterion: "1. Criterion 1", OracleFile: strp("a_oracle_test.go"), Rationale: "ra", CriterionIndex: 1},
			{Criterion: "Criterion 2", OracleFile: strp("b_oracle_test.go"), Rationale: "rb", CriterionIndex: 2, TargetPath: "pkg/b_oracle_test.go", Supersedes: []string{"pkg/old_oracle_test.go"}},
			{Criterion: "Criterion 3", OracleFile: strp("c_oracle_test.go"), CriterionIndex: 3},
		},
		command, [][]int{{1, 2}, {2, 3}})
}

func dirNames(t *testing.T, dir string) []string {
	t.Helper()
	es, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range es {
		out = append(out, e.Name())
	}
	sort.Strings(out)
	return out
}

func TestMaterializeIsANoOpWithoutARequestLevelOracle(t *testing.T) {
	dataDir := t.TempDir()
	r := newApprovableRequest(t, dataDir, "req-1", StatePlanning, true)
	// A hand-installed ticket oracle on a flag-less request must be left alone.
	hand := filepath.Join(Dir(dataDir, r.ID), "tickets", "001.oracle")
	if err := os.MkdirAll(hand, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hand, "keep.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	before := dirNames(t, filepath.Join(Dir(dataDir, r.ID), "tickets"))
	tickets := []Ticket{{Index: 1, SpecPath: filepath.Join(Dir(dataDir, r.ID), "tickets", "001.spec.md")}}
	if err := MaterializeTicketOracles(dataDir, r, tickets); err != nil {
		t.Fatal(err)
	}
	if got := dirNames(t, filepath.Join(Dir(dataDir, r.ID), "tickets")); strings.Join(got, ",") != strings.Join(before, ",") {
		t.Errorf("tickets dir changed: %v -> %v", before, got)
	}
	if _, err := os.Stat(filepath.Join(hand, "keep.txt")); err != nil {
		t.Errorf("hand-installed oracle touched: %v", err)
	}
}

func TestMaterializeAssignsToLastClaimantWithPerTicketManifest(t *testing.T) {
	cmd := "go test ./.oracle/...\n"
	dataDir, r, tickets := threeCriteria(t, cmd)
	if err := MaterializeTicketOracles(dataDir, r, tickets); err != nil {
		t.Fatal(err)
	}
	tdir := filepath.Join(Dir(dataDir, r.ID), "tickets")
	// Criterion 2 is claimed by both tickets: b goes to 002 only.
	if got := strings.Join(dirNames(t, filepath.Join(tdir, "001.oracle")), ","); got != "MANIFEST.json,RUN_COMMAND.txt,a_oracle_test.go" {
		t.Errorf("001.oracle = %s", got)
	}
	if got := strings.Join(dirNames(t, filepath.Join(tdir, "002.oracle")), ","); got != "MANIFEST.json,RUN_COMMAND.txt,b_oracle_test.go,c_oracle_test.go" {
		t.Errorf("002.oracle = %s", got)
	}
	for _, name := range []string{"001", "002"} {
		b, err := os.ReadFile(filepath.Join(tdir, name+".oracle", TicketOracleRunCommandFilename))
		if err != nil || string(b) != cmd {
			t.Errorf("%s RUN_COMMAND = %q, %v", name, b, err)
		}
	}
	var m2 []matEntry
	b, _ := os.ReadFile(filepath.Join(tdir, "002.oracle", ManifestFileName))
	if err := json.Unmarshal(b, &m2); err != nil {
		t.Fatal(err)
	}
	if len(m2) != 2 || m2[0].CriterionIndex != 2 || m2[0].TargetPath != "pkg/b_oracle_test.go" || len(m2[0].Supersedes) != 1 || m2[0].Rationale != "rb" || m2[1].CriterionIndex != 3 {
		t.Errorf("002 manifest = %+v", m2)
	}
	var m1 []matEntry
	b, _ = os.ReadFile(filepath.Join(tdir, "001.oracle", ManifestFileName))
	if err := json.Unmarshal(b, &m1); err != nil {
		t.Fatal(err)
	}
	if len(m1) != 1 || m1[0].CriterionIndex != 1 || len(m1[0].Supersedes) != 0 {
		t.Errorf("001 manifest = %+v (supersedes must live only on the owning ticket)", m1)
	}
	// Copies are byte-identical to the pinned originals.
	orig, _ := os.ReadFile(filepath.Join(Dir(dataDir, r.ID), "oracle", "c_oracle_test.go"))
	cp, _ := os.ReadFile(filepath.Join(tdir, "002.oracle", "c_oracle_test.go"))
	if string(orig) != string(cp) {
		t.Error("copy differs from original")
	}
}

func TestMaterializeTicketWithNoFilesGetsNoDirAndNullEntriesAreSkipped(t *testing.T) {
	dataDir, r, tickets := matFixture(t, 2,
		map[string]string{"a_oracle_test.go": goOracleBody},
		[]matEntry{
			{Criterion: "Criterion 1", OracleFile: strp("a_oracle_test.go"), CriterionIndex: 1},
			{Criterion: "Criterion 2", OracleFile: nil, CriterionIndex: 2, Rationale: "judgment call"},
		},
		"go test ./.oracle/...\n", [][]int{{1}, {2}})
	if err := MaterializeTicketOracles(dataDir, r, tickets); err != nil {
		t.Fatal(err)
	}
	tdir := filepath.Join(Dir(dataDir, r.ID), "tickets")
	if _, err := os.Stat(filepath.Join(tdir, "002.oracle")); !os.IsNotExist(err) {
		t.Errorf("ticket 002 has an oracle dir (err=%v)", err)
	}
	if _, err := os.Stat(filepath.Join(tdir, "001.oracle")); err != nil {
		t.Error(err)
	}
}

// TestApproveOracleReviewRefusesWhatPlanningWouldHaltOn: every plan-independent
// oracle problem is refused at oracle_review (state unchanged, files editable),
// not at planning, and ListOracle shows the same problems.
func TestApproveOracleReviewRefusesWhatPlanningWouldHaltOn(t *testing.T) {
	one := map[string]string{"a_oracle_test.go": goOracleBody}
	entry := func(idx int, text, file string) []matEntry {
		return []matEntry{{Criterion: text, OracleFile: strp(file), CriterionIndex: idx}}
	}
	for name, tc := range map[string]struct {
		manifest string // raw MANIFEST.json body; "" = derive from entries; "-" = no manifest at all
		entries  []matEntry
		want     string
	}{
		"no manifest":         {manifest: "-", want: "add MANIFEST.json"},
		"unparseable":         {manifest: "{not json", want: "not a JSON array"},
		"no criterion_index":  {manifest: `[{"criterion":"Criterion 1","oracle_file":"a_oracle_test.go"}]`, want: "no criterion_index"},
		"text mismatch":       {entries: entry(1, "Something else entirely", "a_oracle_test.go"), want: "does not match spec criterion 1"},
		"index too big":       {entries: entry(9, "Criterion 1", "a_oracle_test.go"), want: "outside the spec's 2"},
		"index zero":          {entries: entry(0, "Criterion 1", "a_oracle_test.go"), want: "outside the spec's 2"},
		"points at other":     {entries: entry(2, "Criterion 1", "a_oracle_test.go"), want: "does not match spec criterion 2"},
		"oracle_file missing": {entries: entry(1, "Criterion 1", "ghost_oracle_test.go"), want: "not a file in the oracle directory"},
		"path in oracle_file": {entries: entry(1, "Criterion 1", "sub/a_oracle_test.go"), want: "bare, non-hidden file name"},
		"unreferenced file":   {entries: entry(1, "Criterion 1", "a_oracle_test.go"), want: "named by no MANIFEST.json entry"},
	} {
		t.Run(name, func(t *testing.T) {
			files := one
			if name == "unreferenced file" {
				files = map[string]string{"a_oracle_test.go": goOracleBody, "extra_oracle_test.go": goOracleBody}
			}
			dataDir, r := matPrepare(t, 2, files, tc.entries, "go test ./.oracle/...\n")
			mp := filepath.Join(Dir(dataDir, r.ID), RequestOracleDirName, ManifestFileName)
			switch tc.manifest {
			case "":
			case "-":
				if err := os.Remove(mp); err != nil {
					t.Fatal(err)
				}
			default:
				if err := os.WriteFile(mp, []byte(tc.manifest), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			_, err := Approve(dataDir, r.ID, "alice", fixedNow, nil)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("approve err = %v, want %q", err, tc.want)
			}
			got := mustLoad(t, dataDir, r.ID)
			if got.State != StateOracleReview || len(got.ApprovedSHA256) != 0 {
				t.Errorf("refusal changed the request: %s %v", got.State, got.ApprovedSHA256)
			}
			listing, lerr := ListOracle(dataDir, r.ID)
			if lerr != nil {
				t.Fatal(lerr)
			}
			if !strings.Contains(strings.Join(listing.Problems, "\n"), tc.want) {
				t.Errorf("ListOracle.Problems = %v, want %q", listing.Problems, tc.want)
			}
		})
	}
}

// TestMaterializeRefusesDefensivelyWhenPinnedManifestIsBad: approval refuses bad
// manifests, but the materializer re-checks the pinned bytes (a request approved
// before this check existed) instead of trusting them.
func TestMaterializeRefusesDefensivelyWhenPinnedManifestIsBad(t *testing.T) {
	dataDir, r, tickets := matFixture(t, 2, map[string]string{"a_oracle_test.go": goOracleBody},
		[]matEntry{{Criterion: "Criterion 1", OracleFile: strp("a_oracle_test.go"), CriterionIndex: 1}}, "go test ./.oracle/...\n", [][]int{{1, 2}})
	bad := `[{"criterion":"Nope","oracle_file":"a_oracle_test.go","criterion_index":1}]`
	if err := os.WriteFile(filepath.Join(Dir(dataDir, r.ID), "oracle", ManifestFileName), []byte(bad), 0o600); err != nil {
		t.Fatal(err)
	}
	r.ApprovedSHA256["oracle/"+ManifestFileName] = mustHash(t, dataDir, r.ID, "oracle/"+ManifestFileName)
	err := MaterializeTicketOracles(dataDir, r, tickets)
	if err == nil || !strings.Contains(err.Error(), "does not match spec criterion 1") {
		t.Fatalf("err = %v", err)
	}
}

func mustHash(t *testing.T, dataDir, id, rel string) string {
	t.Helper()
	h, err := HashFile(dataDir, id, rel)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestMaterializeRefusesEditedOriginal(t *testing.T) {
	dataDir, r, tickets := threeCriteria(t, "go test ./.oracle/...\n")
	if err := os.WriteFile(filepath.Join(Dir(dataDir, r.ID), "oracle", "a_oracle_test.go"), []byte(goOracleBody+"// edited\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := MaterializeTicketOracles(dataDir, r, tickets)
	if err == nil || !strings.Contains(err.Error(), "has changed since oracle_review approved it") {
		t.Fatalf("err = %v", err)
	}
}

func TestMaterializeEnforcesPerTicketCaps(t *testing.T) {
	// The request-level file cap (== the per-ticket one) is now refused at
	// oracle_review, so planning can never see a ticket subset over it; see
	// TestApproveOracleReviewRefusesOverFileCap.
	t.Run("a file per criterion for an 8-criteria single ticket is fine", func(t *testing.T) {
		files := map[string]string{}
		var entries []matEntry
		for i := 1; i <= 8; i++ {
			n := fmt.Sprintf("f%d_oracle_test.go", i)
			files[n] = goOracleBody
			entries = append(entries, matEntry{Criterion: fmt.Sprintf("Criterion %d", i), OracleFile: strp(n), CriterionIndex: i})
		}
		dataDir, r, tickets := matFixture(t, 8, files, entries, "go test ./.oracle/...\n", [][]int{{1, 2, 3, 4, 5, 6, 7, 8}})
		if err := MaterializeTicketOracles(dataDir, r, tickets); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("over 64 KiB", func(t *testing.T) {
		big := goOracleBody + "// " + strings.Repeat("x", 40*1024) + "\n"
		dataDir, r, tickets := matFixture(t, 2,
			map[string]string{"a_oracle_test.go": big, "b_oracle_test.go": big},
			[]matEntry{
				{Criterion: "Criterion 1", OracleFile: strp("a_oracle_test.go"), CriterionIndex: 1},
				{Criterion: "Criterion 2", OracleFile: strp("b_oracle_test.go"), CriterionIndex: 2},
			}, "go test ./.oracle/...\n", [][]int{{1, 2}})
		err := MaterializeTicketOracles(dataDir, r, tickets)
		if err == nil || !strings.Contains(err.Error(), "bytes") || !strings.Contains(err.Error(), "over the per-ticket cap") {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestMaterializeClearsStaleTicketOracleDirs(t *testing.T) {
	dataDir, r, tickets := threeCriteria(t, "go test ./.oracle/...\n")
	stale := filepath.Join(Dir(dataDir, r.ID), "tickets", "009.oracle")
	if err := os.MkdirAll(stale, 0o750); err != nil {
		t.Fatal(err)
	}
	// A symlink named *.oracle must be unlinked, never followed.
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "precious"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(Dir(dataDir, r.ID), "tickets", "008.oracle")); err != nil {
		t.Fatal(err)
	}
	if err := MaterializeTicketOracles(dataDir, r, tickets); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"009.oracle", "008.oracle"} {
		if _, err := os.Lstat(filepath.Join(Dir(dataDir, r.ID), "tickets", n)); !os.IsNotExist(err) {
			t.Errorf("%s survived (err=%v)", n, err)
		}
	}
	if _, err := os.Stat(filepath.Join(outside, "precious")); err != nil {
		t.Errorf("symlink target was followed: %v", err)
	}
}

// TestMaterializeRunCommandScope: a directory-scoped command is accepted for
// every ticket; a command naming .oracle/<file> is accepted only for tickets
// whose subset holds that file.
func TestMaterializeRunCommandScope(t *testing.T) {
	overlay := func(file string) string {
		return `W="$PWD" && printf '{"Replace":{"%s":"%s"}}' "$PWD/pkg/zz_oracle_test.go" "$W/.oracle/` + file + `" > /tmp/o.json && go test -overlay=/tmp/o.json ./pkg/ -run TestOracle -count=1` + "\n"
	}
	t.Run("directory scoped", func(t *testing.T) {
		for _, cmd := range []string{"go test ./.oracle/...\n", "pytest .oracle\n", "go test ./.oracle/ # .oracle/zzz_test.go is ignored in a comment\n"} {
			dataDir, r, tickets := threeCriteria(t, cmd)
			if err := MaterializeTicketOracles(dataDir, r, tickets); err != nil {
				t.Errorf("%q: %v", cmd, err)
			}
		}
	})
	t.Run("names a file every claimant has", func(t *testing.T) {
		// One file covers both criteria, so both tickets... only the last
		// claimant receives it; the command names it and that ticket has it.
		dataDir, r, tickets := matFixture(t, 2,
			map[string]string{"a_oracle_test.go": goOracleBody},
			[]matEntry{
				{Criterion: "Criterion 1", OracleFile: strp("a_oracle_test.go"), CriterionIndex: 1},
				{Criterion: "Criterion 2", OracleFile: strp("a_oracle_test.go"), CriterionIndex: 2},
			}, overlay("a_oracle_test.go"), [][]int{{1, 2}})
		if err := MaterializeTicketOracles(dataDir, r, tickets); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("names a file a ticket lacks", func(t *testing.T) {
		dataDir, r, tickets := threeCriteria(t, overlay("a_oracle_test.go"))
		err := MaterializeTicketOracles(dataDir, r, tickets)
		if err == nil || !strings.Contains(err.Error(), "002.spec.md") || !strings.Contains(err.Error(), ".oracle/a_oracle_test.go") || !strings.Contains(err.Error(), "directory-scoped") {
			t.Fatalf("err = %v", err)
		}
	})
}

// approvePlanAfterMaterialize puts a materialized request into plan_review.
func approvePlanAfterMaterialize(t *testing.T) (string, *Request) {
	t.Helper()
	dataDir, r, tickets := threeCriteria(t, "go test ./.oracle/...\n")
	if err := MaterializeTicketOracles(dataDir, r, tickets); err != nil {
		t.Fatal(err)
	}
	r.State = StatePlanReview
	r.prevState = StatePlanReview
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	return dataDir, r
}

func TestApprovePlanAcceptsUneditedMaterializedOracle(t *testing.T) {
	dataDir, r := approvePlanAfterMaterialize(t)
	got, err := Approve(dataDir, r.ID, "alice", fixedNow, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != StateBuilding || got.ApprovedSHA256["tickets/002.oracle/b_oracle_test.go"] == "" {
		t.Errorf("state %s pins %v", got.State, got.ApprovedSHA256)
	}
}

func TestApprovePlanRefusesEditedMaterializedCopies(t *testing.T) {
	edit := func(t *testing.T, dataDir, rel, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(Dir(dataDir, "req-1"), rel), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for name, mutate := range map[string]func(t *testing.T, dataDir string){
		"oracle file": func(t *testing.T, d string) {
			edit(t, d, "tickets/002.oracle/b_oracle_test.go", goOracleBody+"// hand edit\n")
		},
		"run command": func(t *testing.T, d string) {
			edit(t, d, "tickets/001.oracle/RUN_COMMAND.txt", "go test ./.oracle/... -run X\n")
		},
		"manifest target_path": func(t *testing.T, d string) {
			b, err := os.ReadFile(filepath.Join(Dir(d, "req-1"), "tickets/002.oracle/MANIFEST.json"))
			if err != nil {
				t.Fatal(err)
			}
			edit(t, d, "tickets/002.oracle/MANIFEST.json", strings.Replace(string(b), "pkg/b_oracle_test.go", "pkg/elsewhere_test.go", 1))
		},
		"manifest supersedes": func(t *testing.T, d string) {
			b, err := os.ReadFile(filepath.Join(Dir(d, "req-1"), "tickets/002.oracle/MANIFEST.json"))
			if err != nil {
				t.Fatal(err)
			}
			edit(t, d, "tickets/002.oracle/MANIFEST.json", strings.Replace(string(b), "pkg/old_oracle_test.go", "pkg/important_test.go", 1))
		},
		"planted file": func(t *testing.T, d string) {
			edit(t, d, "tickets/001.oracle/extra_oracle_test.go", goOracleBody)
		},
	} {
		t.Run(name, func(t *testing.T) {
			dataDir, r := approvePlanAfterMaterialize(t)
			mutate(t, dataDir)
			before := mustLoad(t, dataDir, r.ID)
			_, err := Approve(dataDir, r.ID, "alice", fixedNow, nil)
			if !errors.Is(err, ErrMaterializedOracleEdited) {
				t.Fatalf("err = %v, want ErrMaterializedOracleEdited", err)
			}
			after := mustLoad(t, dataDir, r.ID)
			if after.State != StatePlanReview || len(after.ApprovedSHA256) != len(before.ApprovedSHA256) || after.ApprovedBy != before.ApprovedBy {
				t.Errorf("state not unchanged: %s %v", after.State, after.ApprovedSHA256)
			}
		})
	}
}

// TestApprovePlanKeepsHandInstalledOracleSemanticsWithoutRequestOracle: a
// request whose oracle stage was skipped (nothing pinned) approves a
// hand-installed ticket oracle exactly as before, even one that looks nothing
// like a request-level oracle.
func TestApprovePlanKeepsHandInstalledOracleSemanticsWithoutRequestOracle(t *testing.T) {
	dataDir := t.TempDir()
	r := newApprovableRequest(t, dataDir, "req-1", StatePlanReview, true)
	dir := filepath.Join(Dir(dataDir, r.ID), "tickets", "001.oracle")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "hand_oracle_test.go"), []byte(goOracleBody), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, TicketOracleRunCommandFilename), []byte("go test ./.oracle/...\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := Approve(dataDir, r.ID, "alice", fixedNow, nil)
	if err != nil || got.State != StateBuilding {
		t.Fatalf("state %v err %v", got, err)
	}
}

// TestApprovePlanRefusesWrongAssignment: the check at plan approval re-derives
// the assignment, so provenance alone (every file is a pinned original) is not
// enough: a deleted directory, a pinned file copied into another ticket, and a
// file plus its entry moved to an earlier ticket are all refused, state
// unchanged.
func TestApprovePlanRefusesWrongAssignment(t *testing.T) {
	readManifest := func(t *testing.T, dataDir, rel string) []matEntry {
		t.Helper()
		b, err := os.ReadFile(filepath.Join(Dir(dataDir, "req-1"), rel))
		if err != nil {
			t.Fatal(err)
		}
		var es []matEntry
		if err := json.Unmarshal(b, &es); err != nil {
			t.Fatal(err)
		}
		return es
	}
	writeManifest := func(t *testing.T, dataDir, rel string, es []matEntry) {
		t.Helper()
		b, _ := json.MarshalIndent(es, "", "  ")
		if err := os.WriteFile(filepath.Join(Dir(dataDir, "req-1"), rel), append(b, '\n'), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for name, mutate := range map[string]func(t *testing.T, dataDir string){
		"deleted directory": func(t *testing.T, d string) {
			if err := os.RemoveAll(filepath.Join(Dir(d, "req-1"), "tickets", "002.oracle")); err != nil {
				t.Fatal(err)
			}
		},
		"pinned file copied into another ticket": func(t *testing.T, d string) {
			b, _ := os.ReadFile(filepath.Join(Dir(d, "req-1"), "oracle", "a_oracle_test.go"))
			if err := os.WriteFile(filepath.Join(Dir(d, "req-1"), "tickets", "002.oracle", "a_oracle_test.go"), b, 0o600); err != nil {
				t.Fatal(err)
			}
		},
		"file and entry moved to an earlier ticket": func(t *testing.T, d string) {
			root := Dir(d, "req-1")
			b, _ := os.ReadFile(filepath.Join(root, "oracle", "c_oracle_test.go"))
			if err := os.WriteFile(filepath.Join(root, "tickets", "001.oracle", "c_oracle_test.go"), b, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(filepath.Join(root, "tickets", "002.oracle", "c_oracle_test.go")); err != nil {
				t.Fatal(err)
			}
			m1 := readManifest(t, d, "tickets/001.oracle/MANIFEST.json")
			m2 := readManifest(t, d, "tickets/002.oracle/MANIFEST.json")
			writeManifest(t, d, "tickets/001.oracle/MANIFEST.json", append(m1, m2[1]))
			writeManifest(t, d, "tickets/002.oracle/MANIFEST.json", m2[:1])
		},
	} {
		t.Run(name, func(t *testing.T) {
			dataDir, r := approvePlanAfterMaterialize(t)
			mutate(t, dataDir)
			_, err := Approve(dataDir, r.ID, "alice", fixedNow, nil)
			if !errors.Is(err, ErrMaterializedOracleEdited) {
				t.Fatalf("err = %v, want ErrMaterializedOracleEdited", err)
			}
			if got := mustLoad(t, dataDir, r.ID); got.State != StatePlanReview {
				t.Errorf("state %s, want plan_review unchanged", got.State)
			}
		})
	}
}

// TestRunCommandScopeSeesListSeparatedReferences: a comma or colon separates
// file references just as whitespace does.
func TestRunCommandScopeSeesListSeparatedReferences(t *testing.T) {
	for _, cmd := range []string{
		"runner --files=.oracle/a_test.go,.oracle/b_test.go",
		"runner --files=.oracle/a_test.go:.oracle/b_test.go",
		"runner .oracle/a_test.go,.oracle/b_test.go",
	} {
		err := checkRunCommandScope(cmd, []string{"a_test.go"})
		if err == nil || !strings.Contains(err.Error(), ".oracle/b_test.go") {
			t.Errorf("%q: err = %v, want a refusal naming b_test.go", cmd, err)
		}
		if err := checkRunCommandScope(cmd, []string{"a_test.go", "b_test.go"}); err != nil {
			t.Errorf("%q with both files: %v", cmd, err)
		}
	}
}

// TestRunCommandScopeAcceptsGlobPatterns: a globbed reference such as the
// stdlib Python runner's `.oracle/test_oracle_*.py` is a pattern covering any
// subset, not a file named `test_oracle_` (it halted a live multi-file Python
// request at materialization, 2026-09-24). A plain named file is still checked.
func TestRunCommandScopeAcceptsGlobPatterns(t *testing.T) {
	subset := []string{"test_oracle_007.py"}
	for _, cmd := range []string{
		`python3 - <<'PY'` + "\nfor path in sorted(glob.glob(\".oracle/test_oracle_*.py\")):\n    pass\nPY",
		"runner .oracle/test_oracle_?.py",
		"runner .oracle/test_oracle_[0-9]*.py",
	} {
		if err := checkRunCommandScope(cmd, subset); err != nil {
			t.Errorf("%q: %v, want a glob accepted for any subset", cmd, err)
		}
	}
	if err := checkRunCommandScope("python3 .oracle/test_oracle_001.py", subset); err == nil {
		t.Error("a named file outside the subset must still be refused")
	}
}

func TestReturnToOracleReviewOnlyAfterMaterializeHalt(t *testing.T) {
	r := New("req-1", "/w", "w", Source{Kind: SourceText}, fixedNow)
	r.State, r.prevState = StatePlanning, StatePlanning
	r.ApprovedSHA256 = map[string]string{"spec.md": "s", "oracle/a": "x", "tickets/001.spec.md": "t"}
	if err := r.Halt("some other halt", fixedNow); err != nil {
		t.Fatal(err)
	}
	if err := r.ReturnToOracleReview("op", "", fixedNow); err == nil {
		t.Fatal("an ordinary halt returned to oracle_review")
	}
	r.State, r.prevState = StatePlanning, StatePlanning
	if err := r.HaltOracleMaterialization("cap", fixedNow); err != nil {
		t.Fatal(err)
	}
	if r.HaltKind != HaltOracleMaterialize {
		t.Fatalf("kind %q", r.HaltKind)
	}
	if err := r.ReturnToOracleReview("op", "", fixedNow); err != nil {
		t.Fatal(err)
	}
	if r.State != StateOracleReview || r.HaltKind != "" || r.Error != "" || r.ApprovedSHA256["oracle/a"] != "" || r.ApprovedSHA256["spec.md"] != "s" {
		t.Errorf("after return: %s kind=%q err=%q pins=%v", r.State, r.HaltKind, r.Error, r.ApprovedSHA256)
	}
}

// TestApprovePlanRefusesDirectoryForTicketWithNoAssignedFiles: a ticket the
// derivation gives no oracle (an earlier claimant of every criterion) must have
// no oracle directory, even one made only of pinned originals.
func TestApprovePlanRefusesDirectoryForTicketWithNoAssignedFiles(t *testing.T) {
	dataDir, r, tickets := matFixture(t, 2,
		map[string]string{"a_oracle_test.go": goOracleBody, "b_oracle_test.go": goOracleBody},
		[]matEntry{
			{Criterion: "Criterion 1", OracleFile: strp("a_oracle_test.go"), CriterionIndex: 1},
			{Criterion: "Criterion 2", OracleFile: strp("b_oracle_test.go"), CriterionIndex: 2},
		},
		"go test ./.oracle/...\n", [][]int{{1, 2}, {1, 2}})
	if err := MaterializeTicketOracles(dataDir, r, tickets); err != nil {
		t.Fatal(err)
	}
	early := filepath.Join(Dir(dataDir, r.ID), "tickets", "001.oracle")
	if err := os.MkdirAll(early, 0o750); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"a_oracle_test.go", "RUN_COMMAND.txt"} {
		b, _ := os.ReadFile(filepath.Join(Dir(dataDir, r.ID), "oracle", n))
		if err := os.WriteFile(filepath.Join(early, n), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	r.State, r.prevState = StatePlanReview, StatePlanReview
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	if _, err := Approve(dataDir, r.ID, "alice", fixedNow, nil); !errors.Is(err, ErrMaterializedOracleEdited) {
		t.Fatalf("err = %v, want ErrMaterializedOracleEdited", err)
	}
}

func sharedFileFixture(t *testing.T, claims [][]int) (string, *Request, []Ticket) {
	t.Helper()
	return matFixture(t, 2,
		map[string]string{"shared_oracle_test.go": goOracleBody},
		[]matEntry{
			{Criterion: "Criterion 1", OracleFile: strp("shared_oracle_test.go"), CriterionIndex: 1, TargetPath: "pkg/shared_oracle_test.go"},
			{Criterion: "Criterion 2", OracleFile: strp("shared_oracle_test.go"), CriterionIndex: 2, TargetPath: "pkg/shared_oracle_test.go"},
		},
		"go test ./.oracle/...\n", claims)
}

func TestMaterializeRefusesSharedFileAcrossTickets(t *testing.T) {
	dataDir, r, tickets := sharedFileFixture(t, [][]int{{1}, {2}})
	err := MaterializeTicketOracles(dataDir, r, tickets)
	if err == nil {
		t.Fatal("a shared oracle file whose criteria land in two tickets was materialized")
	}
	for _, want := range []string{"shared_oracle_test.go", "criterion 1", "001.spec.md", "criterion 2", "002.spec.md", "split the plan so these criteria land in one ticket"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}
	if es, _ := os.ReadDir(filepath.Join(Dir(dataDir, r.ID), "tickets")); len(es) != 2 {
		t.Errorf("tickets dir changed: %d entries (no .oracle dirs may be written on refusal)", len(es))
	}
}

func TestMaterializeAllowsSharedFileWithinOneTicket(t *testing.T) {
	dataDir, r, tickets := sharedFileFixture(t, [][]int{{1, 2}})
	if err := MaterializeTicketOracles(dataDir, r, tickets); err != nil {
		t.Fatal(err)
	}
	// Overlapping claims: both criteria still resolve to the last ticket.
	dataDir, r, tickets = sharedFileFixture(t, [][]int{{1, 2}, {1, 2}})
	if err := MaterializeTicketOracles(dataDir, r, tickets); err != nil {
		t.Fatal(err)
	}
}

// TestApproveOracleReviewRefusesGoOracleThatCannotBuild: a Go oracle whose
// package clause does not fit its target_path's directory, or that does not
// parse, is refused at oracle_review and listed as a Problem; a fitting one is
// approved.
func TestApproveOracleReviewRefusesGoOracleThatCannotBuild(t *testing.T) {
	for name, tc := range map[string]struct {
		body   string
		target string
		want   string // "" = approved
	}{
		"fits directory":      {body: "package summary\n\nimport \"testing\"\n\nfunc TestOracleX(t *testing.T) {}\n", target: "backend/internal/summary/x_test.go"},
		"wrong package":       {body: "package store\n\nimport \"testing\"\n\nfunc TestOracleX(t *testing.T) {}\n", target: "backend/internal/summary/x_test.go", want: `the Go package in backend/internal/summary is "summary"`},
		"does not parse":      {body: "package summary\n\nfunc TestOracleX(t *testing.T) {\n", target: "backend/internal/summary/x_test.go", want: "does not parse as Go"},
		"no target: syntax":   {body: "package whatever\n\nimport \"testing\"\n\nfunc TestOracleX(t *testing.T) {}\n", target: ""},
		"no target: bad code": {body: "not go at all TestOracleX", target: "", want: "does not parse as Go"},
	} {
		t.Run(name, func(t *testing.T) {
			dataDir, r := matPrepare(t, 1, map[string]string{"a_oracle_test.go": tc.body},
				[]matEntry{{Criterion: "Criterion 1", OracleFile: strp("a_oracle_test.go"), CriterionIndex: 1}}, "go test ./.oracle/...\n")
			repo := t.TempDir()
			if err := os.MkdirAll(filepath.Join(repo, "backend/internal/summary"), 0o750); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(repo, "backend/internal/summary/summary.go"), []byte("package summary\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			r.Workspace = repo
			if err := r.Save(dataDir); err != nil {
				t.Fatal(err)
			}
			if tc.target != "" {
				mp := filepath.Join(Dir(dataDir, r.ID), RequestOracleDirName, ManifestFileName)
				manifest := fmt.Sprintf(`[{"criterion":"Criterion 1","oracle_file":"a_oracle_test.go","criterion_index":1,"target_path":%q}]`, tc.target)
				if err := os.WriteFile(mp, []byte(manifest), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			listing, lerr := ListOracle(dataDir, r.ID)
			if lerr != nil {
				t.Fatal(lerr)
			}
			_, err := Approve(dataDir, r.ID, "alice", fixedNow, nil)
			if tc.want == "" {
				if err != nil || len(listing.Problems) != 0 {
					t.Fatalf("err = %v, problems = %v; want approval", err, listing.Problems)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("approve err = %v, want %q", err, tc.want)
			}
			if !strings.Contains(strings.Join(listing.Problems, "\n"), tc.want) {
				t.Errorf("ListOracle.Problems = %v, want %q", listing.Problems, tc.want)
			}
			if got := mustLoad(t, dataDir, r.ID); got.State != StateOracleReview {
				t.Errorf("refusal changed state to %s", got.State)
			}
		})
	}
}

// TestApproveOracleReviewRefusesPythonOracleThatCannotRun: a Python oracle
// that does not parse, defines no top-level test function, or imports/calls
// something forbidden is refused at oracle_review and listed as a Problem; a
// well-formed one is approved.
func TestApproveOracleReviewRefusesPythonOracleThatCannotRun(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH")
	}
	for name, tc := range map[string]struct {
		body string
		want string // "" = approved
	}{
		"well formed":       {body: "def test_oracle_x():\n    assert 1 == 1\n"},
		"does not parse":    {body: "def test_oracle_x(:\n    pass\n", want: "does not parse as Python"},
		"no test function":  {body: "def helper():\n    return 1\n", want: "no top-level test-named function"},
		"forbidden import":  {body: "import subprocess\n\n\ndef test_oracle_x():\n    assert True\n", want: "not allowed in a drafted oracle"},
		"forbidden builtin": {body: "def test_oracle_x():\n    eval('1')\n", want: "not allowed in a drafted oracle"},
	} {
		t.Run(name, func(t *testing.T) {
			dataDir, r := matPrepare(t, 1, map[string]string{"test_oracle_x.py": tc.body},
				[]matEntry{{Criterion: "Criterion 1", OracleFile: strp("test_oracle_x.py"), CriterionIndex: 1}}, "python3 -m unittest .oracle/test_oracle_x.py\n")
			listing, lerr := ListOracle(dataDir, r.ID)
			if lerr != nil {
				t.Fatal(lerr)
			}
			_, err := Approve(dataDir, r.ID, "alice", fixedNow, nil)
			if tc.want == "" {
				if err != nil || len(listing.Problems) != 0 {
					t.Fatalf("err = %v, problems = %v; want approval", err, listing.Problems)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("approve err = %v, want %q", err, tc.want)
			}
			if !strings.Contains(strings.Join(listing.Problems, "\n"), tc.want) {
				t.Errorf("ListOracle.Problems = %v, want %q", listing.Problems, tc.want)
			}
			if got := mustLoad(t, dataDir, r.ID); got.State != StateOracleReview {
				t.Errorf("refusal changed state to %s", got.State)
			}
		})
	}
}

// TestApproveOracleReviewRefusesPythonOraclesWithInconsistentImports: live
// defect, 2026-09-24 -- a request naming "sub.py with subtract_numbers" and
// "div.py with divide_numbers" produced independently-drafted criteria that
// imported subtract_numbers/divide_numbers from inconsistent modules
// (subtract/add, divide/add); the build created/edited all of them and was
// quarantined on diff_scope. Two well-formed Python oracles that individually
// pass CheckPythonOracle but disagree on which module a shared name lives in
// can never both be satisfied within one ticket's allowed files, so approval
// refuses them too, mirroring pythonOracleProblems' own per-file refusal.
func TestApproveOracleReviewRefusesPythonOraclesWithInconsistentImports(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH")
	}
	dataDir, r := matPrepare(t, 2, map[string]string{
		"test_oracle_001.py": "from subtract import subtract_numbers\n\n\ndef test_oracle_a():\n    assert subtract_numbers(5, 2) == 3\n",
		"test_oracle_002.py": "from add import subtract_numbers\n\n\ndef test_oracle_b():\n    assert subtract_numbers(9, 4) == 5\n",
	}, []matEntry{
		{Criterion: "Criterion 1", OracleFile: strp("test_oracle_001.py"), CriterionIndex: 1},
		{Criterion: "Criterion 2", OracleFile: strp("test_oracle_002.py"), CriterionIndex: 2},
	}, "python3 -m unittest .oracle/test_oracle_001.py .oracle/test_oracle_002.py\n")
	listing, lerr := ListOracle(dataDir, r.ID)
	if lerr != nil {
		t.Fatal(lerr)
	}
	_, err := Approve(dataDir, r.ID, "alice", fixedNow, nil)
	if err == nil || !strings.Contains(err.Error(), "subtract_numbers") {
		t.Fatalf("approve err = %v, want it to name the conflicting import", err)
	}
	if !strings.Contains(strings.Join(listing.Problems, "\n"), "subtract_numbers") {
		t.Errorf("ListOracle.Problems = %v, want it to name the conflicting import", listing.Problems)
	}
	if got := mustLoad(t, dataDir, r.ID); got.State != StateOracleReview {
		t.Errorf("refusal changed state to %s", got.State)
	}
}

// TestApproveOracleReviewRefusesPythonOracleWithInventedModule: onboarding
// P4 -- a Python oracle importing a module that is neither a real module in
// the target workspace nor named anywhere in the approved spec text is an
// invented module the drafter guessed, refused at oracle_review just like an
// inconsistent import (oraclecanary.PythonUnresolvedModules). A module that
// DOES exist in the workspace, or that the spec itself names (draft_spec.py
// now instructs the spec drafter to carry such names over verbatim into
// Scope/Acceptance criteria rather than abstracting them away), and any
// stdlib module, are all accepted.
func TestApproveOracleReviewRefusesPythonOracleWithInventedModule(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH")
	}
	oracleBody := func(importLine string) string {
		return importLine + "\n\n\ndef test_oracle_x():\n    assert True\n"
	}
	for name, tc := range map[string]struct {
		importLine    string
		specExtra     string
		workspaceFile string
		want          string // "" = approved
	}{
		"stdlib import is accepted": {
			importLine: "import math",
		},
		"module existing in the workspace is accepted": {
			importLine:    "from sub import subtract_numbers",
			workspaceFile: "sub.py",
		},
		"module named in the approved spec is accepted": {
			importLine: "from sub import subtract_numbers",
			specExtra:  "`sub.py` providing `subtract_numbers(a, b)`",
		},
		"invented module is refused": {
			importLine: "from sub import subtract_numbers",
			want:       `oracle test_oracle_x.py imports module "sub", which neither exists in the repository nor is named in the approved spec`,
		},
	} {
		t.Run(name, func(t *testing.T) {
			dataDir, r := matPrepare(t, 1, map[string]string{"test_oracle_x.py": oracleBody(tc.importLine)},
				[]matEntry{{Criterion: "Criterion 1", OracleFile: strp("test_oracle_x.py"), CriterionIndex: 1}}, "python3 -m unittest .oracle/test_oracle_x.py\n")
			if tc.workspaceFile != "" {
				workspace := t.TempDir()
				if err := os.WriteFile(filepath.Join(workspace, tc.workspaceFile), []byte("def subtract_numbers(a, b):\n    return a - b\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				r.Workspace = workspace
				if err := r.Save(dataDir); err != nil {
					t.Fatal(err)
				}
			}
			if tc.specExtra != "" {
				spec := "# Spec\n\n## Problem\n\nx\n\n## Scope\n\n" + tc.specExtra + "\n\n## Acceptance criteria\n\n1. Criterion 1\n\n## Risks\n\nNone.\n"
				if err := os.WriteFile(filepath.Join(Dir(dataDir, r.ID), specFileName), []byte(spec), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			listing, lerr := ListOracle(dataDir, r.ID)
			if lerr != nil {
				t.Fatal(lerr)
			}
			_, err := Approve(dataDir, r.ID, "alice", fixedNow, nil)
			if tc.want == "" {
				if err != nil || len(listing.Problems) != 0 {
					t.Fatalf("err = %v, problems = %v; want approval", err, listing.Problems)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("approve err = %v, want %q", err, tc.want)
			}
			if !strings.Contains(strings.Join(listing.Problems, "\n"), tc.want) {
				t.Errorf("ListOracle.Problems = %v, want %q", listing.Problems, tc.want)
			}
			if got := mustLoad(t, dataDir, r.ID); got.State != StateOracleReview {
				t.Errorf("refusal changed state to %s", got.State)
			}
		})
	}
}

func TestApproveOracleReviewRefusesOverFileCap(t *testing.T) {
	files := map[string]string{}
	var entries []matEntry
	for i := 1; i <= MaxTicketOracleFiles+1; i++ {
		name := fmt.Sprintf("o%02d_oracle_test.go", i)
		files[name] = goOracleBody
		entries = append(entries, matEntry{Criterion: fmt.Sprintf("Criterion %d", i), OracleFile: strp(name), CriterionIndex: i})
	}
	dataDir, r := matPrepare(t, len(entries), files, entries, "go test ./.oracle/...\n")
	_, err := Approve(dataDir, r.ID, "alice", fixedNow, nil)
	if err == nil || !strings.Contains(err.Error(), "file cap") {
		t.Fatalf("approve err = %v, want the file cap refusal", err)
	}
	listing, _ := ListOracle(dataDir, r.ID)
	if !strings.Contains(strings.Join(listing.Problems, "\n"), "file cap") {
		t.Errorf("ListOracle.Problems = %v", listing.Problems)
	}
}
