package forge

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func boolp(b bool) *bool { return &b }

func TestParseChecksReadsCheckRunsAndStatusContexts(t *testing.T) {
	oid, checks, hasNext, _, err := parseChecks(readTestdata(t, "checks_mixed.json"))
	if err != nil || hasNext || !strings.HasPrefix(oid, "4f9d2c") {
		t.Fatalf("parseChecks = %q, %v, %v", oid, hasNext, err)
	}
	want := []Check{
		{Name: "test", Poster: "github-actions", Completed: true, Conclusion: "FAILURE", DetailsURL: "https://github.com/acme/widgets/actions/runs/9/job/77", Required: boolp(true), RunID: 77},
		{Name: "lint", Poster: "github-actions", Completed: true, Conclusion: "FAILURE", DetailsURL: "https://github.com/acme/widgets/actions/runs/9/job/78", Required: boolp(false), RunID: 78},
		{Name: "build", Poster: "github-actions", Completed: false, DetailsURL: "https://github.com/acme/widgets/actions/runs/9/job/79", Required: boolp(true), RunID: 79},
		{Name: "license/cla", Poster: "license/cla", Completed: true, Conclusion: "FAILURE", DetailsURL: "https://cla.example/acme", Required: boolp(true)},
		{Name: "ci/legacy", Poster: "ci/legacy", Completed: true, Conclusion: "SUCCESS", Required: boolp(false)},
		{Name: "ci/pending", Poster: "ci/pending", Completed: false, Required: boolp(true)},
	}
	if !reflect.DeepEqual(checks, want) {
		t.Errorf("checks = %+v\nwant %+v", checks, want)
	}
}

func TestParseChecksNoRollupIsNoChecks(t *testing.T) {
	_, checks, _, _, err := parseChecks([]byte(`{"data":{"repository":{"pullRequest":{"commits":{"nodes":[{"commit":{"oid":"x","statusCheckRollup":null}}]}}}}}`))
	if err != nil || len(checks) != 0 {
		t.Errorf("checks = %v, err = %v, want none", checks, err)
	}
}

// fakeGHChecks is a fake gh whose `api graphql` call for the checks query
// prints checksOut, or fails when failOnRequired is set and the query asks
// for isRequired.
func fakeGHChecks(t *testing.T, dir, prViewOut, threadsOut, checksOut string, failOnRequired bool) string {
	t.Helper()
	fail := ""
	if failOnRequired {
		fail = `case "$*" in *isRequired*) echo "Field 'isRequired' doesn't exist" >&2; exit 1;; esac`
	}
	script := `#!/bin/sh
case "$*" in
  "pr view"*) cat <<'EOF'
` + prViewOut + `
EOF
  ;;
  "api graphql"*statusCheckRollup*) ` + fail + `
  cat <<'EOF'
` + checksOut + `
EOF
  ;;
  "api graphql"*) cat <<'EOF'
` + threadsOut + `
EOF
  ;;
  "api user"*) printf 'factory-bot' ;;
  *) echo "fake-gh: unexpected: $*" >&2; exit 1 ;;
esac
`
	path := filepath.Join(dir, "fake-gh")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func readChecksState(t *testing.T, checksFixture string, failOnRequired bool) ReviewState {
	t.Helper()
	resetSelfLoginCache(t)
	gh := fakeGHChecks(t, t.TempDir(),
		string(readTestdata(t, "prview_approved.json")),
		string(readTestdata(t, "reviewthreads_none.json")),
		string(readTestdata(t, checksFixture)), failOnRequired)
	state, err := GHPullRequestOpener{GHBinary: gh}.ReadReviewState(context.Background(), "https://github.com/acme/widgets/pull/42", AuthorPolicy{})
	if err != nil {
		t.Fatalf("ReadReviewState: %v", err)
	}
	return state
}

func TestReadReviewStateCarriesTheChecksWithTheirRequiredFlag(t *testing.T) {
	state := readChecksState(t, "checks_mixed.json", false)
	if len(state.Checks) != 6 || state.RequiredUnknown {
		t.Fatalf("Checks = %d, RequiredUnknown = %v", len(state.Checks), state.RequiredUnknown)
	}
	if state.Checks[0].Required == nil || !*state.Checks[0].Required {
		t.Errorf("first check = %+v, want required", state.Checks[0])
	}
}

func TestReadReviewStateRequiredUnknownWhenIsRequiredCannotBeRead(t *testing.T) {
	state := readChecksState(t, "checks_no_required.json", true)
	if !state.RequiredUnknown || len(state.Checks) != 6 {
		t.Fatalf("RequiredUnknown = %v, Checks = %d", state.RequiredUnknown, len(state.Checks))
	}
	for _, c := range state.Checks {
		if c.Required != nil {
			t.Errorf("check %q Required = %v, want nil", c.Name, *c.Required)
		}
	}
}

func TestReadReviewStateDropsChecksOfAnotherHead(t *testing.T) {
	state := readChecksState(t, "checks_other_head.json", false)
	if len(state.Checks) != 0 {
		t.Errorf("Checks = %+v, want none (the head moved)", state.Checks)
	}
}

func TestReadJobLogAsksForTheJobsLogEndpoint(t *testing.T) {
	dir := t.TempDir()
	gh := filepath.Join(dir, "fake-gh")
	script := "#!/bin/sh\n[ \"$1 $2\" = \"api repos/acme/widgets/actions/jobs/77/logs\" ] || { echo \"bad: $*\" >&2; exit 1; }\nprintf 'line one\\nline two\\n'\n"
	if err := os.WriteFile(gh, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := GHPullRequestOpener{GHBinary: gh}.ReadJobLog(context.Background(), "https://github.com/acme/widgets/pull/42", 77)
	if err != nil || got != "line one\nline two\n" {
		t.Errorf("ReadJobLog = %q, %v", got, err)
	}
}

func TestClassifyChecksTable(t *testing.T) {
	chk := func(name, poster string, completed bool, conclusion string, required *bool) Check {
		return Check{Name: name, Poster: poster, Completed: completed, Conclusion: conclusion, Required: required}
	}
	yes, no := boolp(true), boolp(false)
	cases := []struct {
		name           string
		checks         []Check
		unknown        bool
		pending, notes []string
		rounds, triage []string
	}{
		{name: "required failure starts a round", checks: []Check{chk("t", "github-actions", true, "FAILURE", yes)}, rounds: []string{"t"}},
		{name: "optional failure is a note", checks: []Check{chk("t", "github-actions", true, "FAILURE", no)}, notes: []string{"t"}},
		{name: "pending required waits", checks: []Check{chk("t", "github-actions", false, "", yes)}, pending: []string{"t"}},
		{name: "pending optional does not wait", checks: []Check{chk("t", "github-actions", false, "", no)}},
		{name: "success is nothing", checks: []Check{chk("t", "github-actions", true, "SUCCESS", yes), chk("u", "x", true, "SKIPPED", yes), chk("v", "x", true, "NEUTRAL", no)}},
		{name: "cancelled required is triage", checks: []Check{chk("t", "github-actions", true, "CANCELLED", yes)}, triage: []string{"t"}},
		{name: "timed out required is triage", checks: []Check{chk("t", "github-actions", true, "TIMED_OUT", yes)}, triage: []string{"t"}},
		{name: "startup failure, action required, stale are triage", checks: []Check{chk("a", "x", true, "STARTUP_FAILURE", yes), chk("b", "x", true, "ACTION_REQUIRED", yes), chk("c", "x", true, "STALE", yes)}, triage: []string{"a", "b", "c"}},
		{name: "cancelled optional is a note", checks: []Check{chk("t", "x", true, "CANCELLED", no)}, notes: []string{"t"}},
		{name: "required CLA failure is triage", checks: []Check{chk("cla", "CLA-Assistant", true, "FAILURE", yes)}, triage: []string{"cla"}},
		{name: "required DCO status failure is triage", checks: []Check{chk("DCO", "DCO", true, "FAILURE", yes)}, triage: []string{"DCO"}},
		{name: "optional CLA failure is a note", checks: []Check{chk("cla", "cla-assistant", true, "FAILURE", no)}, notes: []string{"cla"}},
		{name: "unknown: every failure counts as required", unknown: true, checks: []Check{chk("t", "x", true, "FAILURE", nil)}, rounds: []string{"t"}},
		{name: "unknown: other conclusions are notes", unknown: true, checks: []Check{chk("t", "x", true, "CANCELLED", nil)}, notes: []string{"t"}},
		{name: "unknown: CLA failure is still triage", unknown: true, checks: []Check{chk("c", "license/cla", true, "FAILURE", nil)}, triage: []string{"c"}},
		{name: "unknown: pending waits", unknown: true, checks: []Check{chk("t", "x", false, "", nil)}, pending: []string{"t"}},
		{name: "commit status ERROR required is triage", checks: []Check{chk("s", "s", true, "ERROR", yes)}, triage: []string{"s"}},
	}
	names := func(cs []Check) []string {
		var out []string
		for _, c := range cs {
			out = append(out, c.Name)
		}
		return out
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v := ClassifyChecks(c.checks, c.unknown)
			for label, pair := range map[string][2][]string{
				"pending": {names(v.Pending), c.pending}, "notes": {names(v.Notes), c.notes},
				"rounds": {names(v.Rounds), c.rounds}, "triage": {names(v.Triage), c.triage},
			} {
				if !reflect.DeepEqual(pair[0], pair[1]) {
					t.Errorf("%s = %v, want %v", label, pair[0], pair[1])
				}
			}
		})
	}
}
