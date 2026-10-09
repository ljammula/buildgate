package run

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"buildgate/internal/evidence"
)

func TestDir(t *testing.T) {
	got := Dir("data", "ticket-20260825-120000")
	want := filepath.Join("data", "runs", "ticket-20260825-120000")
	if got != want {
		t.Errorf("Dir() = %q, want %q", got, want)
	}
}

func TestAbsDirResolvesARelativeDataDir(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("os.Getwd: %v", err)
	}

	got := AbsDir("data", "ticket-20260825-120000")
	want := filepath.Join(wd, "data", "runs", "ticket-20260825-120000")
	if got != want {
		t.Errorf("AbsDir(%q, %q) = %q, want %q", "data", "ticket-20260825-120000", got, want)
	}
	if !filepath.IsAbs(got) {
		t.Errorf("AbsDir(%q, %q) = %q, want an absolute path", "data", "ticket-20260825-120000", got)
	}
}

func TestAbsDirLeavesAnAlreadyAbsoluteDataDirUnchanged(t *testing.T) {
	got := AbsDir("/srv/factoryd/data", "ticket-20260825-120000")
	want := filepath.Join("/srv/factoryd/data", "runs", "ticket-20260825-120000")
	if got != want {
		t.Errorf("AbsDir(%q, %q) = %q, want %q", "/srv/factoryd/data", "ticket-20260825-120000", got, want)
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	dataDir := t.TempDir()
	r := &Run{
		ID:                         "t-1",
		Ticket:                     "t",
		ProjectPath:                "/proj",
		WorkspacePath:              "/proj",
		SpecPath:                   "/proj/spec.md",
		SpecSHA256:                 "specsha",
		State:                      StateAccepted,
		BaseSHA:                    "aaa",
		ResultSHA:                  "bbb",
		CommittedByFactoryd:        true,
		ChangedFiles:               []string{"a.go", "b.go"},
		DependencyLockfilesTouched: []string{"go.sum"},
		DiffStat:                   &DiffStat{FilesChanged: 2, Insertions: 10, Deletions: 3},
		AgentEvidence: &AgentEvidence{
			Generated:     "2026-08-25T00:00:20+00:00",
			ReviewPolicy:  "advisory",
			Succeeded:     true,
			StoppedReason: "canonical verification passed",
		},
		Attempts: []Attempt{
			{Kind: "build", Command: []string{"python3", "build_app.py"}, ExitCode: 0},
		},
		GateResults: []GateResult{
			{Check: "canonical_verify", Passed: true, ExitCode: 0, LogSHA256: "logsha"},
		},
		Notifications: []NotificationRecord{
			{RunID: "t-1", Ticket: "t", Reason: "gate failed", State: StateQuarantined, SentAt: "2026-08-25T00:00:30Z"},
		},
		Overrides: []Override{
			{By: "operator", Reason: "reviewed evidence", At: "2026-08-25T00:00:45Z", PriorState: StateQuarantined, NewState: StateAccepted},
		},
		Rescues: []Rescue{
			{By: "operator", Reason: "manual inspection", At: "2026-08-25T00:00:50Z", Action: "re-run verification", PriorState: StateQuarantined},
		},
		CreatedAt: "2026-08-25T00:00:00Z",
		UpdatedAt: "2026-08-25T00:01:00Z",
	}

	if err := r.Save(dataDir); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := Load(dataDir, r.ID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	// Round-trip through JSON for both sides so the comparison catches
	// field drift (e.g. a struct field renamed without updating callers)
	// rather than relying on Go struct equality of two already-typed
	// values, which can't see fields that no longer exist on the struct
	// but still linger in a hand-edited durable record.
	wantJSON, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("marshal want: %v", err)
	}
	gotJSON, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal got: %v", err)
	}
	if string(gotJSON) != string(wantJSON) {
		t.Errorf("round trip mismatch:\n got: %s\nwant: %s", gotJSON, wantJSON)
	}
}

func TestRecordRescueIsAppendOnly(t *testing.T) {
	r := &Run{State: StateQuarantined}
	if err := r.RecordRescue("operator", "manual inspection", "re-run verification", "", func() string { return "2026-08-25T00:00:50Z" }); err != nil {
		t.Fatalf("RecordRescue: %v", err)
	}
	if r.State != StateQuarantined || len(r.Rescues) != 1 {
		t.Fatalf("rescue mutated run: state=%q rescues=%+v", r.State, r.Rescues)
	}
}

func TestRecordRescueRequiresAttribution(t *testing.T) {
	r := &Run{State: StateHalted}
	for _, tc := range []struct{ by, reason, action string }{
		{"", "reason", "action"}, {"operator", "", "action"}, {"operator", "reason", ""},
	} {
		if err := r.RecordRescue(tc.by, tc.reason, tc.action, "", func() string { return "unused" }); err == nil {
			t.Errorf("RecordRescue(%q,%q,%q) unexpectedly succeeded", tc.by, tc.reason, tc.action)
		}
	}
}

// TestAgentEvidenceDecodesAgentsMDFields locks json.Unmarshal's handling of
// the agents_md_used/agents_md_git_blob fields build_app.py's
// write_evidence_json now writes into
// BUILD_EVIDENCE.json: present-and-populated, and omitted entirely (an
// older build_app.py, or a build with no committed AGENTS.md) defaulting
// to the Go zero values rather than erroring or dropping sibling fields.
func TestAgentEvidenceDecodesAgentsMDFields(t *testing.T) {
	var withGuidance AgentEvidence
	raw := []byte(`{
		"schema_version": 2,
		"succeeded": true,
		"stopped_reason": "canonical verification passed",
		"agents_md_used": true,
		"agents_md_git_blob": "e69de29bb2d1d6434b8b29ae775ad8c2e48c5391",
		"rounds": []
	}`)
	if err := json.Unmarshal(raw, &withGuidance); err != nil {
		t.Fatalf("unmarshal with agents_md fields: %v", err)
	}
	if !withGuidance.AgentsMDUsed {
		t.Errorf("AgentsMDUsed = false, want true")
	}
	if withGuidance.AgentsMDGitBlob != "e69de29bb2d1d6434b8b29ae775ad8c2e48c5391" {
		t.Errorf("AgentsMDGitBlob = %q, want the fixture blob id", withGuidance.AgentsMDGitBlob)
	}
	if !withGuidance.Succeeded {
		t.Errorf("Succeeded = false, want true (sibling field must still decode)")
	}

	var withoutGuidance AgentEvidence
	if err := json.Unmarshal([]byte(`{"schema_version": 2, "succeeded": false, "rounds": []}`), &withoutGuidance); err != nil {
		t.Fatalf("unmarshal without agents_md fields: %v", err)
	}
	if withoutGuidance.AgentsMDUsed {
		t.Errorf("AgentsMDUsed = true, want false (zero value) when the field is absent")
	}
	if withoutGuidance.AgentsMDGitBlob != "" {
		t.Errorf("AgentsMDGitBlob = %q, want empty when the field is absent", withoutGuidance.AgentsMDGitBlob)
	}
}

// TestAgentEvidenceRoundFeedbackFields covers the five round-feedback
// fields: a round that recorded them round-trips them, a round written by a
// build_app.py without them keeps null lists (never-collected) and writes no
// empty strings, and a round that passed keeps its empty list.
func TestAgentEvidenceRoundFeedbackFields(t *testing.T) {
	raw := []byte(`{"schema_version": 2, "rounds": [
		{"index": 1, "verify_passed": false, "blockers": ["canonical verification failed"],
		 "changed_files": ["sum.go"], "failure_signature": "0123456789abcdef",
		 "failure_log": ".pi-build-session/feedback/verify.log", "agent_notes": "exited 2"},
		{"index": 2, "verify_passed": true, "blockers": [], "changed_files": [],
		 "failure_signature": "", "failure_log": "", "agent_notes": ""},
		{"index": 3, "verify_passed": true}
	]}`)
	var ev AgentEvidence
	if err := json.Unmarshal(raw, &ev); err != nil {
		t.Fatal(err)
	}
	ev.CleanRoundFeedback()
	first := ev.Rounds[0]
	if len(first.Blockers) != 1 || first.Blockers[0] != "canonical verification failed" || len(first.ChangedFiles) != 1 || first.ChangedFiles[0] != "sum.go" ||
		first.FailureSignature != "0123456789abcdef" || first.FailureLog != ".pi-build-session/feedback/verify.log" || first.AgentNotes != "exited 2" {
		t.Errorf("round 1 = %+v, want its five feedback fields as written", first)
	}
	if ev.Rounds[1].Blockers == nil || len(ev.Rounds[1].Blockers) != 0 || ev.Rounds[1].ChangedFiles == nil {
		t.Errorf("round 2 lists = %#v, %#v, want empty and non-nil", ev.Rounds[1].Blockers, ev.Rounds[1].ChangedFiles)
	}
	if ev.Rounds[2].Blockers != nil || ev.Rounds[2].ChangedFiles != nil {
		t.Errorf("round 3 lists = %#v, %#v, want nil for a round that never recorded them", ev.Rounds[2].Blockers, ev.Rounds[2].ChangedFiles)
	}

	out, err := json.Marshal(ev.Rounds)
	if err != nil {
		t.Fatal(err)
	}
	var fields []map[string]json.RawMessage
	if err := json.Unmarshal(out, &fields); err != nil {
		t.Fatal(err)
	}
	if got := string(fields[1]["blockers"]) + string(fields[1]["changed_files"]); got != "[][]" {
		t.Errorf("passed round lists = %s, want [][]", got)
	}
	if got := string(fields[2]["blockers"]) + string(fields[2]["changed_files"]); got != "nullnull" {
		t.Errorf("never-recorded round lists = %s, want nullnull", got)
	}
	for _, key := range []string{"failure_signature", "failure_log", "agent_notes"} {
		if _, ok := fields[2][key]; ok {
			t.Errorf("never-recorded round carries %q, want it omitted", key)
		}
	}
}

// TestCleanRoundFeedbackBoundsAndCleansAgentText: BUILD_EVIDENCE.json is a
// file the build agent can write, so what is recorded from it is cut to
// size and stripped of terminal escapes, control characters and secrets.
func TestCleanRoundFeedbackBoundsAndCleansAgentText(t *testing.T) {
	many := make([]string, 500)
	for i := range many {
		many[i] = "f.go"
	}
	ev := &AgentEvidence{Rounds: []AgentEvidenceRound{{
		Blockers:         append([]string{"verify\x1b[31m failed\r\nsecond line", strings.Repeat("é", 500)}, many...),
		ChangedFiles:     many,
		FailureSignature: strings.Repeat("a", 100),
		FailureLog:       "logs/\u202everify.log\n" + strings.Repeat("p", 600),
		AgentNotes:       "line one\nkey Bearer " + strings.Repeat("A", 40) + "\x07\n" + strings.Repeat("n", 9000),
	}}}
	ev.CleanRoundFeedback()
	rd := ev.Rounds[0]
	if rd.Blockers[0] != "verify failed second line" {
		t.Errorf("Blockers[0] = %q, want one clean line", rd.Blockers[0])
	}
	if len(rd.Blockers) != maxRoundBlockers || utf8.RuneCountInString(rd.Blockers[1]) != maxRoundBlockerLen || !utf8.ValidString(rd.Blockers[1]) {
		t.Errorf("Blockers: %d entries, entry 1 has %d runes, want %d and %d", len(rd.Blockers), utf8.RuneCountInString(rd.Blockers[1]), maxRoundBlockers, maxRoundBlockerLen)
	}
	if len(rd.ChangedFiles) != maxRoundChangedFiles {
		t.Errorf("ChangedFiles has %d entries, want %d", len(rd.ChangedFiles), maxRoundChangedFiles)
	}
	if len(rd.FailureSignature) != maxRoundFailureSignature {
		t.Errorf("FailureSignature has %d bytes, want %d", len(rd.FailureSignature), maxRoundFailureSignature)
	}
	if !strings.HasPrefix(rd.FailureLog, "logs/verify.log ppp") || utf8.RuneCountInString(rd.FailureLog) != maxRoundPathLen {
		t.Errorf("FailureLog = %.40q (%d runes), want one line without the direction override, %d runes", rd.FailureLog, utf8.RuneCountInString(rd.FailureLog), maxRoundPathLen)
	}
	if !strings.HasPrefix(rd.AgentNotes, "line one\nkey Bearer [redacted]\n") || strings.Contains(rd.AgentNotes, "AAAA") || strings.ContainsRune(rd.AgentNotes, '\a') {
		t.Errorf("AgentNotes = %.80q, want its lines kept and the key and bell removed", rd.AgentNotes)
	}
	if utf8.RuneCountInString(rd.AgentNotes) != maxRoundAgentNotesLen {
		t.Errorf("AgentNotes has %d runes, want %d", utf8.RuneCountInString(rd.AgentNotes), maxRoundAgentNotesLen)
	}

	emptied := &AgentEvidence{Rounds: []AgentEvidenceRound{{Blockers: []string{"\x1b[31m", " "}, ChangedFiles: nil}}}
	emptied.CleanRoundFeedback()
	if got := emptied.Rounds[0]; got.Blockers == nil || len(got.Blockers) != 0 || got.ChangedFiles != nil {
		t.Errorf("entries of escapes only: Blockers = %#v, ChangedFiles = %#v, want an empty list and nil", got.Blockers, got.ChangedFiles)
	}

	var none *AgentEvidence
	none.CleanRoundFeedback()
}

// TestAgentEvidenceRoundDecodesFastCheckFields locks json.Unmarshal's
// handling of the fast_check_ran/fast_check_passed fields build_app.py's
// write_evidence_json writes per round:
// present-and-populated, and omitted entirely (an older build_app.py, or a
// round with no --fast-check-command configured) defaulting to the Go zero
// values (false, nil) rather than erroring or dropping sibling fields.
func TestAgentEvidenceRoundDecodesFastCheckFields(t *testing.T) {
	var withFastCheck AgentEvidence
	raw := []byte(`{
		"schema_version": 2,
		"succeeded": false,
		"rounds": [
			{"index": 1, "agent": "pi", "agent_returncode": 1, "agent_timed_out": false,
			 "usage": null, "reviewer_outcome": "unavailable", "reviewer_detail": "",
			 "verify_passed": false, "verify_timed_out": false, "duration_s": 1.0,
			 "fast_check_ran": true, "fast_check_passed": false}
		]
	}`)
	if err := json.Unmarshal(raw, &withFastCheck); err != nil {
		t.Fatalf("unmarshal with fast_check fields: %v", err)
	}
	rnd := withFastCheck.Rounds[0]
	if !rnd.FastCheckRan {
		t.Errorf("FastCheckRan = false, want true")
	}
	if rnd.FastCheckPassed == nil || *rnd.FastCheckPassed != false {
		t.Errorf("FastCheckPassed = %v, want a pointer to false", rnd.FastCheckPassed)
	}

	var withoutFastCheck AgentEvidence
	raw = []byte(`{
		"schema_version": 2,
		"succeeded": true,
		"rounds": [
			{"index": 1, "agent": "pi", "agent_returncode": 0, "agent_timed_out": false,
			 "usage": null, "reviewer_outcome": "clean", "reviewer_detail": "",
			 "verify_passed": true, "verify_timed_out": false, "duration_s": 1.0}
		]
	}`)
	if err := json.Unmarshal(raw, &withoutFastCheck); err != nil {
		t.Fatalf("unmarshal without fast_check fields: %v", err)
	}
	rnd = withoutFastCheck.Rounds[0]
	if rnd.FastCheckRan {
		t.Errorf("FastCheckRan = true, want false (zero value) when the field is absent")
	}
	if rnd.FastCheckPassed != nil {
		t.Errorf("FastCheckPassed = %v, want nil when the field is absent", rnd.FastCheckPassed)
	}
}

// TestSubscriptionBilledUsesLastNonEmptyMode: the decision must follow
// the same "last attempt wins" precedent as Run.Sandboxed, not
// the first -- an earlier attempt's credential mode (e.g. a retried
// build that started metered before an operator switched session
// config) must not override the mode the run actually finished under.
// Moved here from cmd/factoryd (where this logic originated as
// subscriptionBilledAttempts) so internal/api can reuse it for the
// console's own cost display without duplicating it -- package main
// cannot be imported by internal/api.
func TestSubscriptionBilledUsesLastNonEmptyMode(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		attempts []Attempt
		want     bool
	}{
		{"no attempts", nil, false},
		{"no relay on any attempt", []Attempt{{Kind: "build"}, {Kind: "verify"}}, false},
		{"static credential mode", []Attempt{{Kind: "build", RelayCredentialMode: "static"}}, false},
		{"github-copilot credential mode", []Attempt{{Kind: "build", RelayCredentialMode: "github-copilot"}}, true},
		{"chatgpt-codex credential mode", []Attempt{{Kind: "build", RelayCredentialMode: "chatgpt-codex"}}, true},
		{"last non-empty mode wins over an earlier attempt", []Attempt{
			{Kind: "build", RelayCredentialMode: "chatgpt-codex"},
			{Kind: "build", RelayCredentialMode: "static"},
		}, false},
		{"a trailing no-relay attempt (e.g. verify) doesn't blank out an earlier mode", []Attempt{
			{Kind: "build", RelayCredentialMode: "chatgpt-codex"},
			{Kind: "verify"},
		}, true},
		{"RelayBilling metered wins even with a chatgpt-codex credential mode on the same attempt", []Attempt{
			{Kind: "build", RelayCredentialMode: "chatgpt-codex", RelayBilling: "metered"},
		}, false},
		{"empty RelayBilling on the last attempt falls back to its own RelayCredentialMode, not an earlier attempt's RelayBilling", []Attempt{
			{Kind: "build", RelayCredentialMode: "chatgpt-codex", RelayBilling: "subscription"},
			{Kind: "build", RelayCredentialMode: "static"},
		}, false},
		{"a legacy attempt recorded before RelayBilling existed still infers from RelayCredentialMode", []Attempt{
			{Kind: "build", RelayCredentialMode: "github-copilot"},
		}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := SubscriptionBilled(tc.attempts); got != tc.want {
				t.Errorf("SubscriptionBilled(%+v) = %v, want %v", tc.attempts, got, tc.want)
			}
		})
	}
}

// TestLegacyAttemptMarshalsWithoutRouteFields is the byte-identity
// regression for Phase 2's RelayRoute/RelayBilling additions to Attempt: a
// legacy attempt (a real record from before these fields existed, or one
// from a config with no routes: key, which never sets them) must marshal
// to exactly the same JSON as before this change -- both are omitempty,
// so a zero-valued Attempt must produce no "relay_route"/"relay_billing"
// key at all, and the bytes for every other field must be unaffected by
// the new fields' position in the struct.
func TestLegacyAttemptMarshalsWithoutRouteFields(t *testing.T) {
	legacy := Attempt{
		Kind:                "build",
		Command:             []string{"python3", "build_app.py"},
		StartedAt:           "2026-09-27T00:00:00Z",
		FinishedAt:          "2026-09-27T00:05:00Z",
		ExitCode:            0,
		LogPath:             "build.log",
		ImageDigest:         "sha256:" + strings.Repeat("a", 64),
		RelayImageDigest:    "sha256:" + strings.Repeat("b", 64),
		RelayNetwork:        "factoryd-relay-net-1",
		RelayContainerName:  "factoryd-relay-1",
		RelayUpstream:       "https://api.individual.githubcopilot.com",
		RelayCredentialMode: "github-copilot",
		RelayWorkerModelID:  "gpt-5.6-luna",
	}
	got, err := json.Marshal(legacy)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	want := `{"kind":"build","command":["python3","build_app.py"],"started_at":"2026-09-27T00:00:00Z","finished_at":"2026-09-27T00:05:00Z","exit_code":0,"log_path":"build.log","image_digest":"sha256:` +
		strings.Repeat("a", 64) + `","relay_image_digest":"sha256:` + strings.Repeat("b", 64) +
		`","relay_network":"factoryd-relay-net-1","relay_container_name":"factoryd-relay-1","relay_upstream":"https://api.individual.githubcopilot.com","relay_credential_mode":"github-copilot","relay_worker_model_id":"gpt-5.6-luna"}`
	if string(got) != want {
		t.Fatalf("Marshal(legacy attempt) =\n%s\nwant (byte-identical to before Route/Billing existed):\n%s", got, want)
	}
	for _, key := range []string{"relay_route", "relay_billing"} {
		if strings.Contains(string(got), key) {
			t.Errorf("Marshal(legacy attempt) unexpectedly contains %q: %s", key, got)
		}
	}
}

// TestAttemptRoleAndThinkingRoundTrip locks in Role/Thinking's json tags
// and omitempty behavior: an attempt from a roles: config round-trips
// both fields, and one recorded before they existed (neither set) omits
// both keys entirely -- the same "old records byte-identical" guarantee
// TestLegacyAttemptMarshalsWithoutRouteFields already locks in for
// RelayRoute/RelayBilling, extended to these two fields.
func TestAttemptRoleAndThinkingRoundTrip(t *testing.T) {
	withRoles := Attempt{Kind: "build", Role: AttemptRoleExecution, Thinking: "max"}
	got, err := json.Marshal(withRoles)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	want := `{"kind":"build","command":null,"started_at":"","finished_at":"","exit_code":0,"log_path":"","role":"execution","thinking":"max"}`
	if string(got) != want {
		t.Fatalf("Marshal(attempt with role/thinking) =\n%s\nwant:\n%s", got, want)
	}
	var roundTripped Attempt
	if err := json.Unmarshal(got, &roundTripped); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if roundTripped.Role != AttemptRoleExecution || roundTripped.Thinking != "max" {
		t.Errorf("round trip = Role %q, Thinking %q, want %q, %q", roundTripped.Role, roundTripped.Thinking, AttemptRoleExecution, "max")
	}

	legacy := Attempt{Kind: "build"}
	got, err = json.Marshal(legacy)
	if err != nil {
		t.Fatalf("Marshal(legacy): %v", err)
	}
	for _, key := range []string{`"role"`, `"thinking"`, `"expected_effort"`} {
		if strings.Contains(string(got), key) {
			t.Errorf("Marshal(legacy attempt) unexpectedly contains %s: %s", key, got)
		}
	}
}

// TestExpectedReasoningEffort covers run.ExpectedReasoningEffort's three
// cases directly: a declared thinkingLevelMap translation, thinking
// "off"/"" (nothing sent, so nothing to compare), and thinking unchanged
// when the map declares no translation for it (Pi's own undeclared-
// xhigh/max clamp to "high" is a real mismatch this must still surface,
// not mask -- see the function's own doc comment).
func TestExpectedReasoningEffort(t *testing.T) {
	for _, tc := range []struct {
		name            string
		thinking        string
		workerExtraJSON string
		want            string
	}{
		{
			name:            "declared translation is used",
			thinking:        "max",
			workerExtraJSON: `{"reasoning":true,"thinkingLevelMap":{"max":"xhigh"}}`,
			want:            "xhigh",
		},
		{
			name:            "undeclared level returns unchanged (a real clamp downstream is still visible)",
			thinking:        "max",
			workerExtraJSON: `{"reasoning":true}`,
			want:            "max",
		},
		{
			name:            "no worker_model_extra_json at all returns the level unchanged",
			thinking:        "max",
			workerExtraJSON: "",
			want:            "max",
		},
		{
			name:            "off returns empty regardless of any map",
			thinking:        "off",
			workerExtraJSON: `{"thinkingLevelMap":{"off":"low"}}`,
			want:            "",
		},
		{
			name:            "empty thinking returns empty",
			thinking:        "",
			workerExtraJSON: `{"thinkingLevelMap":{"":"low"}}`,
			want:            "",
		},
		{
			name:            "a non-string map value for this level is treated as undeclared",
			thinking:        "xhigh",
			workerExtraJSON: `{"thinkingLevelMap":{"max":"max","xhigh":16000}}`,
			want:            "xhigh",
		},
		{
			name:            "malformed JSON is treated as no translation declared",
			thinking:        "max",
			workerExtraJSON: `{not json`,
			want:            "max",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ExpectedReasoningEffort(tc.thinking, tc.workerExtraJSON); got != tc.want {
				t.Errorf("ExpectedReasoningEffort(%q, %q) = %q, want %q", tc.thinking, tc.workerExtraJSON, got, tc.want)
			}
		})
	}
}

// TestGoldenSchema locks the on-disk JSON field set. A future field
// rename here would silently break any consumer (API, console, policy
// checks) reading `run.json` directly; this test forces that change to
// be deliberate.
func TestGoldenSchema(t *testing.T) {
	// ResultSHA and DiffStat are `omitempty`; leaving either unset here
	// would silently drop its key from both the serialized map and the
	// expected list below, so removing or renaming that field wouldn't be
	// caught by this test — exactly the drift it exists to prevent.
	// ChangedFiles has no `omitempty` (see its own field doc comment) so
	// it's always present regardless, but is populated here too for
	// consistency with the rest of the fixture.
	r := &Run{
		ID:                         "t-1",
		ResultSHA:                  "deadbeef",
		ChangedFiles:               []string{"a.go"},
		DependencyLockfilesTouched: []string{},
		DependencyChanges:          []evidence.DependencyChange{},
		DiffStat:                   &DiffStat{FilesChanged: 1, Insertions: 1, Deletions: 0},
		AgentEvidence: &AgentEvidence{
			Generated: "2026-08-25T00:00:20+00:00",
		},
	}
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(b, &fields); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	want := []string{
		"id", "ticket", "project_path", "workspace_path", "spec_path",
		"spec_sha256", "state", "base_sha", "result_sha", "halt_confirmed", "committed_by_factoryd",
		"changed_files", "dependency_lockfiles_touched", "dependency_changes", "diff_stat", "agent_evidence", "attempts", "gate_results", "notifications", "overrides", "rescues", "created_at", "updated_at",
	}
	for _, k := range want {
		if _, ok := fields[k]; !ok {
			t.Errorf("run.json missing expected field %q", k)
		}
	}
	if len(fields) != len(want) {
		t.Errorf("run.json has %d fields, want %d (unexpected field added?) fields=%v", len(fields), len(want), fields)
	}
}

func TestApplyOverride(t *testing.T) {
	r := &Run{State: StateQuarantined}
	const at = "2026-08-26T12:00:00Z"

	if err := r.ApplyOverride("operator", "reviewed quarantine evidence", StateAccepted, func() string { return at }); err != nil {
		t.Fatalf("ApplyOverride: %v", err)
	}

	if r.State != StateAccepted {
		t.Errorf("State = %q, want %q", r.State, StateAccepted)
	}
	if len(r.Overrides) != 1 {
		t.Fatalf("len(Overrides) = %d, want 1", len(r.Overrides))
	}
	want := Override{
		By:         "operator",
		Reason:     "reviewed quarantine evidence",
		At:         at,
		PriorState: StateQuarantined,
		NewState:   StateAccepted,
	}
	if r.Overrides[0] != want {
		t.Errorf("Overrides[0] = %#v, want %#v", r.Overrides[0], want)
	}
}

// TestApplyOverrideToHaltedSetsHaltConfirmed is the regression test for a
// real P1 finding from codex review: an operator overriding a quarantined
// run to StateHalted is an explicit, attributed human decision — about as
// definitively confirmed as HaltConfirmed's underlying question can get —
// but ApplyOverride never touched that field. Left false, a `factoryd
// daemon` reclaim scan would treat an intentionally-halted run as still
// possibly needing recovery.
func TestApplyOverrideToHaltedSetsHaltConfirmed(t *testing.T) {
	r := &Run{State: StateQuarantined}
	if err := r.ApplyOverride("operator", "policy gate was a false positive", StateHalted, func() string { return "2026-08-26T12:00:00Z" }); err != nil {
		t.Fatalf("ApplyOverride: %v", err)
	}
	if !r.HaltConfirmed {
		t.Fatalf("HaltConfirmed = false after an operator override to StateHalted, want true")
	}
}

func TestApplyOverrideRejectsNotQuarantined(t *testing.T) {
	r := &Run{State: StateHalted}

	if err := r.ApplyOverride("operator", "reviewed evidence", StateAccepted, func() string { return "unused" }); err == nil {
		t.Fatal("ApplyOverride returned nil, want error")
	}
	if len(r.Overrides) != 0 {
		t.Errorf("len(Overrides) = %d, want 0", len(r.Overrides))
	}
	if r.State != StateHalted {
		t.Errorf("State = %q, want %q", r.State, StateHalted)
	}
}

func TestApplyOverrideRejectsEmptyBy(t *testing.T) {
	r := &Run{State: StateQuarantined}

	if err := r.ApplyOverride("", "reviewed evidence", StateAccepted, func() string { return "unused" }); err == nil {
		t.Fatal("ApplyOverride returned nil, want error")
	}
	if len(r.Overrides) != 0 {
		t.Errorf("len(Overrides) = %d, want 0", len(r.Overrides))
	}
	if r.State != StateQuarantined {
		t.Errorf("State = %q, want %q", r.State, StateQuarantined)
	}
}

func TestApplyOverrideRejectsEmptyReason(t *testing.T) {
	r := &Run{State: StateQuarantined}

	if err := r.ApplyOverride("operator", "", StateAccepted, func() string { return "unused" }); err == nil {
		t.Fatal("ApplyOverride returned nil, want error")
	}
	if len(r.Overrides) != 0 {
		t.Errorf("len(Overrides) = %d, want 0", len(r.Overrides))
	}
	if r.State != StateQuarantined {
		t.Errorf("State = %q, want %q", r.State, StateQuarantined)
	}
}

func TestValidateSliceChainAcceptsMatchingResultSHA(t *testing.T) {
	prior := &Run{ID: "slice-1", State: StateAccepted, ResultSHA: "abc123", ProjectPath: "/repo"}

	if err := ValidateSliceChain(prior, "abc123", "/repo"); err != nil {
		t.Fatalf("ValidateSliceChain: %v", err)
	}
}

func TestValidateSliceChainRejectsMismatchedBaseSHA(t *testing.T) {
	prior := &Run{ID: "slice-1", State: StateAccepted, ResultSHA: "abc123", ProjectPath: "/repo"}

	if err := ValidateSliceChain(prior, "def456", "/repo"); err == nil {
		t.Fatal("ValidateSliceChain returned nil, want error for a base_sha that doesn't match the prior run's result_sha")
	}
}

func TestValidateSliceChainRejectsNonAcceptedPrior(t *testing.T) {
	prior := &Run{ID: "slice-1", State: StateQuarantined, ResultSHA: "abc123", ProjectPath: "/repo"}

	if err := ValidateSliceChain(prior, "abc123", "/repo"); err == nil {
		t.Fatal("ValidateSliceChain returned nil, want error when the prior run never reached accepted")
	}
}

func TestValidateSliceChainRejectsMissingResultSHA(t *testing.T) {
	prior := &Run{ID: "slice-1", State: StateAccepted, ProjectPath: "/repo"}

	if err := ValidateSliceChain(prior, "abc123", "/repo"); err == nil {
		t.Fatal("ValidateSliceChain returned nil, want error when the prior run has no recorded result_sha")
	}
}

// TestValidateSliceChainRejectsMismatchedProjectPath is the regression
// test for a real finding from codex review (round 3, 2026-08-28): a
// result_sha match alone is not proof of the same project, since two
// distinct repositories can legitimately share a commit SHA (most often
// an early or empty-tree one).
func TestValidateSliceChainRejectsMismatchedProjectPath(t *testing.T) {
	prior := &Run{ID: "slice-1", State: StateAccepted, ResultSHA: "abc123", ProjectPath: "/repo-a"}

	if err := ValidateSliceChain(prior, "abc123", "/repo-b"); err == nil {
		t.Fatal("ValidateSliceChain returned nil, want error when the prior run was against a different project")
	}
}

func TestFindChainSuccessorFindsAcceptedSuccessor(t *testing.T) {
	dataDir := t.TempDir()
	prior := &Run{ID: "slice-1", State: StateAccepted, ResultSHA: "abc123", ProjectPath: "/repo"}
	if err := prior.Save(dataDir); err != nil {
		t.Fatalf("save prior: %v", err)
	}
	successor := &Run{ID: "slice-2", State: StateAccepted, ProjectPath: "/repo", PriorRunID: "slice-1"}
	if err := successor.Save(dataDir); err != nil {
		t.Fatalf("save successor: %v", err)
	}

	got, err := FindChainSuccessor(dataDir, "/repo", "slice-1")
	if err != nil {
		t.Fatalf("FindChainSuccessor: %v", err)
	}
	if got != "slice-2" {
		t.Errorf("FindChainSuccessor = %q, want %q", got, "slice-2")
	}
}

func TestFindChainSuccessorReturnsEmptyWhenStillChainTip(t *testing.T) {
	dataDir := t.TempDir()
	prior := &Run{ID: "slice-1", State: StateAccepted, ResultSHA: "abc123", ProjectPath: "/repo"}
	if err := prior.Save(dataDir); err != nil {
		t.Fatalf("save prior: %v", err)
	}

	got, err := FindChainSuccessor(dataDir, "/repo", "slice-1")
	if err != nil {
		t.Fatalf("FindChainSuccessor: %v", err)
	}
	if got != "" {
		t.Errorf("FindChainSuccessor = %q, want empty (no successor exists)", got)
	}
}

// TestFindChainSuccessorIgnoresUnacceptedSuccessor pins that a halted or
// quarantined run declaring priorID as its own PriorRunID must not count
// as having superseded it -- only an accepted successor actually advanced
// the chain.
func TestFindChainSuccessorIgnoresUnacceptedSuccessor(t *testing.T) {
	dataDir := t.TempDir()
	prior := &Run{ID: "slice-1", State: StateAccepted, ResultSHA: "abc123", ProjectPath: "/repo"}
	if err := prior.Save(dataDir); err != nil {
		t.Fatalf("save prior: %v", err)
	}
	halted := &Run{ID: "slice-2", State: StateHalted, ProjectPath: "/repo", PriorRunID: "slice-1"}
	if err := halted.Save(dataDir); err != nil {
		t.Fatalf("save halted successor: %v", err)
	}

	got, err := FindChainSuccessor(dataDir, "/repo", "slice-1")
	if err != nil {
		t.Fatalf("FindChainSuccessor: %v", err)
	}
	if got != "" {
		t.Errorf("FindChainSuccessor = %q, want empty (only-halted successor must not count)", got)
	}
}

// TestFindChainSuccessorIgnoresOtherProjects pins that a same-ID
// PriorRunID declared by an accepted run against a different project must
// not count -- mirroring ValidateSliceChain's own project-path guard.
func TestFindChainSuccessorIgnoresOtherProjects(t *testing.T) {
	dataDir := t.TempDir()
	prior := &Run{ID: "slice-1", State: StateAccepted, ResultSHA: "abc123", ProjectPath: "/repo-a"}
	if err := prior.Save(dataDir); err != nil {
		t.Fatalf("save prior: %v", err)
	}
	unrelated := &Run{ID: "slice-2", State: StateAccepted, ProjectPath: "/repo-b", PriorRunID: "slice-1"}
	if err := unrelated.Save(dataDir); err != nil {
		t.Fatalf("save unrelated successor: %v", err)
	}

	got, err := FindChainSuccessor(dataDir, "/repo-a", "slice-1")
	if err != nil {
		t.Fatalf("FindChainSuccessor: %v", err)
	}
	if got != "" {
		t.Errorf("FindChainSuccessor = %q, want empty (a different project's successor must not count)", got)
	}
}

// TestFindChainSuccessorSkipsADirectoryWithNoRunJSONYet pins that a run
// directory with no run.json -- e.g. cmd/factoryd created Dir(dataDir, id)
// and wrote spec.snapshot.md into it, then a preflight check (ticketspec
// parsing, MisprefixedWorkspacePaths, ...) rejected the invocation and
// returned before Run was ever constructed/saved -- is skipped as "not a
// candidate," not treated as a fatal load error. Such a directory was
// never a real run and can never be a chain successor (found via Codex
// review, 2026-09-11, on the misprefixed-path preflight).
func TestFindChainSuccessorSkipsADirectoryWithNoRunJSONYet(t *testing.T) {
	dataDir := t.TempDir()
	prior := &Run{ID: "slice-1", State: StateAccepted, ResultSHA: "abc123", ProjectPath: "/repo"}
	if err := prior.Save(dataDir); err != nil {
		t.Fatalf("save prior: %v", err)
	}
	halfCreatedDir := Dir(dataDir, "rejected-before-save")
	if err := os.MkdirAll(halfCreatedDir, 0o750); err != nil {
		t.Fatalf("create half-created run dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(halfCreatedDir, "spec.snapshot.md"), []byte("# ticket\n"), 0o600); err != nil {
		t.Fatalf("write spec snapshot: %v", err)
	}
	successor := &Run{ID: "slice-2", State: StateAccepted, ProjectPath: "/repo", PriorRunID: "slice-1"}
	if err := successor.Save(dataDir); err != nil {
		t.Fatalf("save successor: %v", err)
	}

	got, err := FindChainSuccessor(dataDir, "/repo", "slice-1")
	if err != nil {
		t.Fatalf("FindChainSuccessor: %v (want the run.json-less directory skipped, not fatal)", err)
	}
	if got != "slice-2" {
		t.Errorf("FindChainSuccessor = %q, want %q", got, "slice-2")
	}
}

func TestFindChainSuccessorFailsClosedOnUnreadableCandidate(t *testing.T) {
	dataDir := t.TempDir()
	prior := &Run{ID: "slice-1", State: StateAccepted, ResultSHA: "abc123", ProjectPath: "/repo"}
	if err := prior.Save(dataDir); err != nil {
		t.Fatalf("save prior: %v", err)
	}
	badDir := Dir(dataDir, "unreadable-candidate")
	if err := os.MkdirAll(badDir, 0o750); err != nil {
		t.Fatalf("create candidate directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(badDir, "run.json"), []byte("{not-json"), 0o600); err != nil {
		t.Fatalf("write malformed candidate: %v", err)
	}

	if successor, err := FindChainSuccessor(dataDir, "/repo", "slice-1"); err == nil {
		t.Fatalf("FindChainSuccessor = %q, nil; want a fail-closed load error", successor)
	}
}

// TestChangedFilesNullVsEmptyDistinguishable pins the fix for a finding
// from review: ChangedFiles must not be `omitempty`, because a
// successfully collected zero-change inventory (base == result) needs to
// serialize as `[]`, distinguishable from `null` (collection was never
// attempted, e.g. main.go's warning-and-continue path on a git error).
// `omitempty` would drop the key in both cases and make them
// indistinguishable in the durable record.
func TestChangedFilesNullVsEmptyDistinguishable(t *testing.T) {
	neverCollected := &Run{ID: "t-1"} // ChangedFiles left nil
	collectedEmpty := &Run{ID: "t-1", ChangedFiles: []string{}}

	nullJSON, err := json.Marshal(neverCollected)
	if err != nil {
		t.Fatalf("marshal neverCollected: %v", err)
	}
	emptyJSON, err := json.Marshal(collectedEmpty)
	if err != nil {
		t.Fatalf("marshal collectedEmpty: %v", err)
	}

	var neverFields, collectedFields map[string]json.RawMessage
	if err := json.Unmarshal(nullJSON, &neverFields); err != nil {
		t.Fatalf("unmarshal neverCollected: %v", err)
	}
	if err := json.Unmarshal(emptyJSON, &collectedFields); err != nil {
		t.Fatalf("unmarshal collectedEmpty: %v", err)
	}

	if got := string(neverFields["changed_files"]); got != "null" {
		t.Errorf("never-collected changed_files = %s, want null", got)
	}
	if got := string(collectedFields["changed_files"]); got != "[]" {
		t.Errorf("collected-empty changed_files = %s, want []", got)
	}
}

func TestDependencyLockfilesTouchedNullVsEmptyDistinguishable(t *testing.T) {
	neverComputed := &Run{ID: "t-1"}
	computedEmpty := &Run{ID: "t-1", DependencyLockfilesTouched: []string{}}

	nullJSON, err := json.Marshal(neverComputed)
	if err != nil {
		t.Fatalf("marshal neverComputed: %v", err)
	}
	emptyJSON, err := json.Marshal(computedEmpty)
	if err != nil {
		t.Fatalf("marshal computedEmpty: %v", err)
	}

	var neverFields, computedFields map[string]json.RawMessage
	if err := json.Unmarshal(nullJSON, &neverFields); err != nil {
		t.Fatalf("unmarshal neverComputed: %v", err)
	}
	if err := json.Unmarshal(emptyJSON, &computedFields); err != nil {
		t.Fatalf("unmarshal computedEmpty: %v", err)
	}

	if got := string(neverFields["dependency_lockfiles_touched"]); got != "null" {
		t.Errorf("never-computed dependency_lockfiles_touched = %s, want null", got)
	}
	if got := string(computedFields["dependency_lockfiles_touched"]); got != "[]" {
		t.Errorf("computed-empty dependency_lockfiles_touched = %s, want []", got)
	}
}

// TestWithLockSerializesConcurrentCallers proves WithLock actually
// excludes two concurrent callers targeting the same run: fn tracks how
// many concurrent invocations are in flight and records the high-water
// mark, which must never exceed 1 if WithLock genuinely serializes them.
// Tracked with atomics, not a plain shared counter — the mutual exclusion
// this test verifies is enforced by the kernel (syscall.Flock), which the
// race detector has no way to recognize as a happens-before edge between
// goroutines; a plain unsynchronized read-modify-write would report a
// false "data race" under -race even though WithLock correctly prevented
// any actual overlap.
func TestWithLockSerializesConcurrentCallers(t *testing.T) {
	dataDir := t.TempDir()
	seeded := &Run{ID: "run-1"}
	if err := seeded.Save(dataDir); err != nil {
		t.Fatalf("seed run: %v", err)
	}

	const n = 20
	var inFlight, maxInFlight, completed atomic.Int32
	done := make(chan struct{}, n)
	for i := 0; i < n; i++ {
		go func() {
			defer func() { done <- struct{}{} }()
			_ = WithLock(dataDir, "run-1", func() error {
				cur := inFlight.Add(1)
				for {
					prevMax := maxInFlight.Load()
					if cur <= prevMax || maxInFlight.CompareAndSwap(prevMax, cur) {
						break
					}
				}
				time.Sleep(time.Millisecond)
				inFlight.Add(-1)
				completed.Add(1)
				return nil
			})
		}()
	}
	for i := 0; i < n; i++ {
		<-done
	}
	if got := completed.Load(); got != n {
		t.Fatalf("completed = %d, want %d", got, n)
	}
	if got := maxInFlight.Load(); got != 1 {
		t.Fatalf("max concurrent fn executions = %d, want 1 — WithLock did not serialize concurrent callers", got)
	}
}

// TestWithLockDoesNotCreateDirectoryForNonexistentRun is the regression
// test for a real P2 finding from codex review: WithLock used to
// MkdirAll the run's directory unconditionally before fn ever ran, so an
// authenticated but arbitrary or typo'd run id left an empty runs/<id>/
// directory with a .lock file behind forever — unbounded disk-filling
// side effect of an operation that was always going to report "not
// found" anyway, since WithLock never itself checks whether the run
// exists.
func TestWithLockDoesNotCreateDirectoryForNonexistentRun(t *testing.T) {
	dataDir := t.TempDir()
	fnCalled := false
	if err := WithLock(dataDir, "does-not-exist", func() error {
		fnCalled = true
		return nil
	}); err != nil {
		t.Fatalf("WithLock: %v", err)
	}
	if !fnCalled {
		t.Fatal("fn was never called — WithLock must still run fn (so its own not-found handling fires) even when there's nothing to lock")
	}
	if _, err := os.Stat(Dir(dataDir, "does-not-exist")); !os.IsNotExist(err) {
		t.Fatalf("stat run dir = %v, want it to not exist — WithLock must not create it for a run that was never there", err)
	}
}

// TestWithLockRejectsTraversalID is the regression test for a real P2
// finding from codex review: internal/api's own HTTP-layer id validation
// (validRunID) has no effect on cmd/factoryd's `override` CLI subcommand,
// whose -run flag reaches WithLock directly with no validation of its
// own. A traversal-style id (e.g. "../evil") resolves outside the runs
// directory once joined — WithLock would open or create a .lock file
// there, a filesystem side effect entirely outside dataDir/runs, driven
// by unvalidated CLI input. fn must never run for a rejected id: unlike
// the merely-nonexistent-run case above, this is a malformed id, so there
// is no not-found handling that still deserves a chance to run.
func TestWithLockRejectsTraversalID(t *testing.T) {
	dataDir := t.TempDir()
	const traversalID = "../evil"

	fnCalled := false
	err := WithLock(dataDir, traversalID, func() error {
		fnCalled = true
		return nil
	})
	if err == nil {
		t.Fatal("WithLock: want an error for a traversal id, got nil")
	}
	if fnCalled {
		t.Fatal("fn was called for a rejected traversal id — it must never run")
	}
	// "runs"/"../evil" resolves to a sibling of "runs" directly under
	// dataDir — confirms nothing was created there, not just that WithLock
	// returned an error.
	if _, statErr := os.Stat(filepath.Join(dataDir, "evil")); !os.IsNotExist(statErr) {
		t.Fatalf("stat %q = %v, want it to not exist", filepath.Join(dataDir, "evil"), statErr)
	}
}

// TestValidID is a direct unit test of the guard WithLock relies on.
func TestValidID(t *testing.T) {
	valid := []string{"run-1", "a", "fixture-ticket-20260826-1"}
	for _, id := range valid {
		if !ValidID(id) {
			t.Errorf("ValidID(%q) = false, want true", id)
		}
	}
	invalid := []string{"", ".", "..", "a/b", `a\b`, "../evil", "/etc/passwd"}
	for _, id := range invalid {
		if ValidID(id) {
			t.Errorf("ValidID(%q) = true, want false", id)
		}
	}
}

func TestSandboxedFalseWithNoAttempts(t *testing.T) {
	if (Run{}).Sandboxed() {
		t.Error("Sandboxed() = true, want false for a run with no attempts at all")
	}
}

func TestSandboxedFalseForHostRunBuildAttempt(t *testing.T) {
	r := Run{Attempts: []Attempt{{Kind: "build"}}}
	if r.Sandboxed() {
		t.Error("Sandboxed() = true, want false for a build attempt with no ImageDigest")
	}
}

func TestSandboxedTrueForSandboxedBuildAttempt(t *testing.T) {
	r := Run{Attempts: []Attempt{{Kind: "build", ImageDigest: "sha256:deadbeef"}}}
	if !r.Sandboxed() {
		t.Error("Sandboxed() = false, want true for a build attempt with an ImageDigest")
	}
}

// TestSandboxedUsesLastBuildAttemptNotFirst is the regression test for a
// real self-review finding, 2026-09-04: -build-app-max-attempts defaults
// to 2, so a first build attempt can fail for an infra reason (a
// launch-time refusal, a transient Docker daemon hiccup) before ever
// setting ImageDigest, while a retry succeeds fully inside the sandbox
// with a real digest. Sandboxed() must report the LAST build attempt's
// containment status -- the one the run's final accepted/quarantined
// state was actually evaluated against -- not the first, which would
// wrongly report a genuinely sandboxed, accepted run as unsandboxed.
func TestSandboxedUsesLastBuildAttemptNotFirst(t *testing.T) {
	r := Run{Attempts: []Attempt{
		{Kind: "build"}, // first attempt: failed to start, no digest
		{Kind: "build", ImageDigest: "sha256:deadbeef"}, // retry: succeeded, sandboxed
	}}
	if !r.Sandboxed() {
		t.Error("Sandboxed() = false, want true -- the retry (last build attempt) was sandboxed")
	}
}

// TestSandboxedUsesLastBuildAttemptEvenWhenLaterVerifyAttemptsFollow proves
// the scan isn't fooled by non-"build" attempts appended after the build
// attempt it should be reading (verify, full_suite_verify) -- those never
// carry a meaningful ImageDigest signal for this question, since
// Sandboxed() is specifically about the untrusted, model-generated build
// step, not the project's own declared verify commands.
func TestSandboxedUsesLastBuildAttemptEvenWhenLaterVerifyAttemptsFollow(t *testing.T) {
	r := Run{Attempts: []Attempt{
		{Kind: "build", ImageDigest: "sha256:deadbeef"},
		{Kind: "verify"},
		{Kind: "full_suite_verify"},
	}}
	if !r.Sandboxed() {
		t.Error("Sandboxed() = false, want true -- the build attempt was sandboxed regardless of later verify attempts")
	}
}

func TestBuildHarnessEvalReturnsNilWithoutHarness(t *testing.T) {
	r := &Run{State: StateAccepted}
	if got := BuildHarnessEval(r, ""); got != nil {
		t.Errorf("BuildHarnessEval() = %+v, want nil for a run whose request never selected a harness", got)
	}
}

func TestRunRecordsHarnessEvalFromAttemptsAndEvidence(t *testing.T) {
	model := "qwen3.8-27b"
	r := &Run{
		State: StateAccepted,
		Attempts: []Attempt{
			{
				Kind:                      "build",
				StartedAt:                 "2026-09-19T10:00:00Z",
				FinishedAt:                "2026-09-19T10:00:00Z",
				RelayConsumedInputTokens:  100,
				RelayConsumedOutputTokens: 40,
				RelayConsumedCostMicroUSD: 500,
			},
			{
				Kind:                      "verify",
				StartedAt:                 "2026-09-19T10:00:10Z",
				FinishedAt:                "2026-09-19T10:00:22Z",
				RelayConsumedInputTokens:  10,
				RelayConsumedOutputTokens: 5,
				RelayConsumedCostMicroUSD: 50,
			},
		},
		AgentEvidence: &AgentEvidence{
			Model:  &model,
			Rounds: []AgentEvidenceRound{{Index: 1}, {Index: 2}},
		},
	}

	got := BuildHarnessEval(r, "pi")
	if got == nil {
		t.Fatal("BuildHarnessEval() = nil, want a populated HarnessEval")
	}
	want := &HarnessEval{
		Harness:      "pi",
		ModelID:      "qwen3.8-27b",
		Outcome:      string(StateAccepted),
		DurationMs:   12000,
		InputTokens:  110,
		OutputTokens: 45,
		CostMicroUSD: 550,
		Rounds:       2,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("BuildHarnessEval() = %+v, want %+v", got, want)
	}
}

// TestBuildHarnessEvalRoles locks in HarnessEval.Roles: one entry per
// distinct Attempt.Role, in first-seen order, and the LAST attempt for a
// repeated role (a corrective build round) wins over an earlier one --
// see HarnessEvalRole's own doc comment for why.
func TestBuildHarnessEvalRoles(t *testing.T) {
	r := &Run{
		State: StateAccepted,
		Attempts: []Attempt{
			{Kind: "build", Role: AttemptRoleExecution, Thinking: "max", RelayWorkerModelID: "gpt-5.6-luna", RelayReasoningEffort: "high"},
			{Kind: "spec_conformity", Role: AttemptRoleReview, Thinking: "high", RelayWorkerModelID: "gpt-5.6-luna", RelayReasoningEffort: "high"},
			// A corrective retry of the build round: same Role, later
			// attempt, different observed effort -- must win over the
			// first build entry above.
			{Kind: "build", Role: AttemptRoleExecution, Thinking: "max", RelayWorkerModelID: "gpt-5.6-luna", RelayReasoningEffort: "max"},
		},
	}
	got := BuildHarnessEval(r, "pi")
	if got == nil {
		t.Fatal("BuildHarnessEval() = nil, want a populated HarnessEval")
	}
	want := []HarnessEvalRole{
		{Role: AttemptRoleExecution, Thinking: "max", ModelID: "gpt-5.6-luna", RelayReasoningEffort: "max"},
		{Role: AttemptRoleReview, Thinking: "high", ModelID: "gpt-5.6-luna", RelayReasoningEffort: "high"},
	}
	if !reflect.DeepEqual(got.Roles, want) {
		t.Errorf("BuildHarnessEval().Roles = %+v, want %+v", got.Roles, want)
	}
}

func TestRunHarnessEvalNilWithoutAgentEvidenceStillSumsAttempts(t *testing.T) {
	r := &Run{
		State: StateQuarantined,
		Attempts: []Attempt{
			{Kind: "build", RelayConsumedInputTokens: 7, RelayConsumedOutputTokens: 3},
		},
	}
	got := BuildHarnessEval(r, "pifork")
	if got == nil {
		t.Fatal("BuildHarnessEval() = nil, want a populated HarnessEval (harness alone is enough)")
	}
	if got.ModelID != "" || got.Rounds != 0 {
		t.Errorf("BuildHarnessEval() = %+v, want empty ModelID and zero Rounds with no AgentEvidence", got)
	}
	if got.InputTokens != 7 || got.OutputTokens != 3 {
		t.Errorf("BuildHarnessEval() token totals = %d/%d, want 7/3", got.InputTokens, got.OutputTokens)
	}
}

// TestListByRequestIDGroupsByRequestAndSkipsUntagged covers
// ListByRequestID's own contract: every run carrying a RequestID lands
// under that id's own slice, a run with RequestID == "" is omitted
// entirely (not grouped under ""), and an empty/missing runs directory
// returns (nil, nil) rather than an error -- the same "absent means
// none yet" convention request.List already uses.
func TestListByRequestIDGroupsByRequestAndSkipsUntagged(t *testing.T) {
	dataDir := t.TempDir()

	if got, err := ListByRequestID(dataDir); err != nil || got != nil {
		t.Fatalf("ListByRequestID(no runs dir) = %v, %v, want nil, nil", got, err)
	}

	mustSave := func(r Run) {
		t.Helper()
		if err := r.Save(dataDir); err != nil {
			t.Fatalf("save run %q: %v", r.ID, err)
		}
	}
	mustSave(Run{ID: "run-a1", RequestID: "req-a", State: StateAccepted})
	mustSave(Run{ID: "run-a2", RequestID: "req-a", State: StateQuarantined})
	mustSave(Run{ID: "run-b1", RequestID: "req-b", State: StateAccepted})
	mustSave(Run{ID: "run-untagged", State: StateAccepted})

	got, err := ListByRequestID(dataDir)
	if err != nil {
		t.Fatalf("ListByRequestID: %v", err)
	}
	if len(got["req-a"]) != 2 {
		t.Errorf("req-a runs = %d, want 2", len(got["req-a"]))
	}
	if len(got["req-b"]) != 1 {
		t.Errorf("req-b runs = %d, want 1", len(got["req-b"]))
	}
	if _, ok := got[""]; ok {
		t.Errorf(`got[""] present, want RequestID=="" runs omitted entirely`)
	}
	total := 0
	for _, runs := range got {
		total += len(runs)
	}
	if total != 3 {
		t.Errorf("total grouped runs = %d, want 3 (run-untagged excluded)", total)
	}
}

// TestAttemptHarnessRoundTrip locks in Harness's json tag and omitempty
// behavior: a recorded harness round-trips, and an attempt recorded before the
// field existed marshals without the key.
func TestAttemptHarnessRoundTrip(t *testing.T) {
	got, err := json.Marshal(Attempt{Kind: "build", Role: AttemptRoleExecution, Harness: "pifork"})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !strings.Contains(string(got), `"harness":"pifork"`) {
		t.Fatalf("Marshal = %s, want the harness key", got)
	}
	var back Attempt
	if err := json.Unmarshal(got, &back); err != nil || back.Harness != "pifork" {
		t.Fatalf("round trip = %+v, %v, want Harness pifork", back, err)
	}
	legacy, err := json.Marshal(Attempt{Kind: "build"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(legacy), "harness") {
		t.Errorf("Marshal(legacy) = %s, want no harness key", legacy)
	}
}

func TestRetainBuildArtifactsCopiesTheReportAndTheRoundLogs(t *testing.T) {
	workspace, runDir := t.TempDir(), filepath.Join(t.TempDir(), "runs", "run-1")
	if err := RetainBuildArtifacts(workspace, runDir); err != nil {
		t.Fatalf("a workspace with neither: %v, want no error", err)
	}
	roundLog := filepath.Join(workspace, ".pi-build-session", "feedback", "round-3", "oracle.log")
	if err := os.MkdirAll(filepath.Dir(roundLog), 0o755); err != nil {
		t.Fatal(err)
	}
	for path, content := range map[string]string{roundLog: "oracle said no\n", filepath.Join(workspace, AgentReportFileName): "# report\n"} {
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := RetainBuildArtifacts(workspace, runDir); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{
		filepath.Join(runDir, AgentReportFileName):                                "# report\n",
		filepath.Join(runDir, evidence.RoundLogsDirName, "round-3", "oracle.log"): "oracle said no\n",
	} {
		if got, err := os.ReadFile(path); err != nil || string(got) != want {
			t.Errorf("%s = %q, %v, want %q", path, got, err, want)
		}
	}
}
