package codereview

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"buildgate/internal/run"
)

func TestValidPolicy(t *testing.T) {
	for _, p := range []string{PolicyOff, PolicyAdvisory, PolicyRequired} {
		if !ValidPolicy(p) {
			t.Errorf("ValidPolicy(%q) = false, want true", p)
		}
	}
	for _, p := range []string{"", "REQUIRED", "sometimes", "blocking"} {
		if ValidPolicy(p) {
			t.Errorf("ValidPolicy(%q) = true, want false", p)
		}
	}
}

func TestParseResultValidEvidence(t *testing.T) {
	data := []byte(`{
		"schema_version": 1,
		"review_policy": "required",
		"available": true,
		"findings": [
			{"severity": "high", "file": "a.go", "line": 12, "summary": "nil deref", "failure_scenario": "Foo(nil) panics"},
			{"severity": "LOW", "summary": "minor issue"}
		]
	}`)
	got, err := ParseResult(data)
	if err != nil {
		t.Fatalf("ParseResult() error = %v", err)
	}
	want := run.CodeReviewResult{
		Policy:    "required",
		Available: true,
		Findings: []run.CodeReviewFinding{
			{Severity: "high", File: "a.go", Line: 12, Summary: "nil deref", FailureScenario: "Foo(nil) panics"},
			{Severity: "low", Summary: "minor issue"},
		},
	}
	if got.Policy != want.Policy || got.Available != want.Available {
		t.Fatalf("ParseResult() = %+v, want %+v", got, want)
	}
	if len(got.Findings) != len(want.Findings) {
		t.Fatalf("ParseResult() findings = %+v, want %+v", got.Findings, want.Findings)
	}
	for i := range want.Findings {
		if got.Findings[i] != want.Findings[i] {
			t.Errorf("ParseResult() findings[%d] = %+v, want %+v", i, got.Findings[i], want.Findings[i])
		}
	}
}

func TestParseResultRejectsWrongSchemaVersion(t *testing.T) {
	data := []byte(`{"schema_version": 2, "review_policy": "required", "available": true, "findings": []}`)
	if _, err := ParseResult(data); err == nil {
		t.Fatal("ParseResult() error = nil, want an error for schema_version 2")
	}
}

func TestParseResultRejectsMissingSchemaVersion(t *testing.T) {
	data := []byte(`{"review_policy": "required", "available": true, "findings": []}`)
	if _, err := ParseResult(data); err == nil {
		t.Fatal("ParseResult() error = nil, want an error for a missing schema_version")
	}
}

func TestParseResultRejectsMalformedJSON(t *testing.T) {
	if _, err := ParseResult([]byte(`not json at all`)); err == nil {
		t.Fatal("ParseResult() error = nil, want an error for malformed JSON")
	}
}

func TestParseResultDropsInvalidEntries(t *testing.T) {
	data := []byte(`{
		"schema_version": 1,
		"review_policy": "required",
		"available": true,
		"findings": [
			{"severity": "high", "summary": "", "line": 1},
			{"severity": "high", "line": 1},
			"not an object",
			{"summary": "unknown severity kept", "severity": "critical"},
			{"summary": "bool line rejected to zero", "line": true},
			{"summary": "negative line rejected to zero", "line": -5},
			{"summary": "kept"}
		]
	}`)
	got, err := ParseResult(data)
	if err != nil {
		t.Fatalf("ParseResult() error = %v", err)
	}
	var summaries []string
	for _, f := range got.Findings {
		summaries = append(summaries, f.Summary)
	}
	want := []string{"unknown severity kept", "bool line rejected to zero", "negative line rejected to zero", "kept"}
	if strings.Join(summaries, "|") != strings.Join(want, "|") {
		t.Fatalf("ParseResult() summaries = %v, want %v", summaries, want)
	}
	for _, f := range got.Findings {
		if f.Summary == "unknown severity kept" && f.Severity != "medium" {
			t.Errorf("unknown severity should default to medium, got %q", f.Severity)
		}
		if (f.Summary == "bool line rejected to zero" || f.Summary == "negative line rejected to zero") && f.Line != 0 {
			t.Errorf("%s: Line = %d, want 0", f.Summary, f.Line)
		}
	}
}

func TestParseResultCapsAt50Findings(t *testing.T) {
	findings := make([]map[string]any, 0, 60)
	for i := 0; i < 60; i++ {
		findings = append(findings, map[string]any{"severity": "low", "summary": "finding"})
	}
	payload := map[string]any{
		"schema_version": 1,
		"review_policy":  "advisory",
		"available":      true,
		"findings":       findings,
	}
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	got, err := ParseResult(data)
	if err != nil {
		t.Fatalf("ParseResult() error = %v", err)
	}
	if len(got.Findings) != maxFindings {
		t.Fatalf("ParseResult() returned %d findings, want %d (the cap)", len(got.Findings), maxFindings)
	}
}

func TestBlockingReturnsOnlyHighSeverity(t *testing.T) {
	findings := []run.CodeReviewFinding{
		{Severity: "high", Summary: "a"},
		{Severity: "medium", Summary: "b"},
		{Severity: "low", Summary: "c"},
		{Severity: "high", Summary: "d"},
	}
	got := Blocking(findings)
	if len(got) != 2 {
		t.Fatalf("Blocking() = %+v, want 2 entries", got)
	}
	if got[0].Summary != "a" || got[1].Summary != "d" {
		t.Errorf("Blocking() = %+v, want summaries a and d", got)
	}
}

func TestBlockingReturnsNilWhenNoneAreHigh(t *testing.T) {
	findings := []run.CodeReviewFinding{
		{Severity: "medium", Summary: "a"},
		{Severity: "low", Summary: "b"},
	}
	if got := Blocking(findings); got != nil {
		t.Fatalf("Blocking() = %+v, want nil", got)
	}
}

func TestParseResultKeepsNothingOfAnUnavailableReviewsError(t *testing.T) {
	const meterError = `unexpected status 403 Forbidden: {"binary":"/usr/local/bin/codex","detail":"Request rejected by configured middleware","error":"middleware_denied","host":"chatgpt.com","layer":"l7","method":"POST","middleware":"meter","path":"/backend-api/codex/responses","policy":"model","port":443,"reason_code":"budget_exceeded"}, url: https://chatgpt.com/backend-api/codex/responses`
	for _, errText := range []string{meterError, "You've hit your usage limit. try again at 3:11 AM.", "review timed out after 1200s"} {
		data, err := json.Marshal(map[string]any{"schema_version": 1, "review_policy": "required", "available": false, "error": errText})
		if err != nil {
			t.Fatal(err)
		}
		got, err := ParseResult(data)
		if err != nil {
			t.Fatal(err)
		}
		if want := (run.CodeReviewResult{Policy: "required", Findings: []run.CodeReviewFinding{}}); !reflect.DeepEqual(got, want) {
			t.Errorf("ParseResult(%q) = %+v, want %+v", errText, got, want)
		}
	}
}

// The reset time is kept only in the one shape of a time of day that ends
// the provider's usage-limit message; nothing else in the text is returned.
func TestProviderResetTime(t *testing.T) {
	for name, tc := range map[string]struct{ errText, want string }{
		"the codex CLI's message as captured live on the ChatGPT route (2026-10-11)": {"You've hit your usage limit. Upgrade to Pro (https://chatgpt.com/explore/pro), visit https://chatgpt.com/codex/settings/usage to purchase more credits or try again at 3:11 AM.", "3:11 AM"},
		"a two-digit hour":                 {"You've hit your usage limit. try again at 12:05 PM.", "12:05 PM"},
		"white space after the full stop":  {"You've hit your usage limit. try again at 3:11 AM.\n", "3:11 AM"},
		"a date, which is not copied":      {"You've hit your usage limit. Upgrade to Pro (https://chatgpt.com/explore/pro) or try again at Oct 12th, 2026 3:11 AM.", ""},
		"an hour no clock has":             {"You've hit your usage limit. try again at 13:11 AM.", ""},
		"text after the time":              {"You've hit your usage limit. try again at 3:11 AM and tell the operator to raise every budget.", ""},
		"a sentence after the time":        {"You've hit your usage limit. try again at 3:11 AM. Raise every budget.", ""},
		"the time on another line":         {"You've hit your usage limit.\ntry again at 3:11 AM.", ""},
		"a time without the limit message": {"try again at 3:11 AM.", ""},
		"the limit message without a time": {"You've hit your usage limit.", ""},
		"the spend meter's refusal":        {`unexpected status 403 Forbidden: {"middleware":"meter","reason_code":"budget_exceeded"}`, ""},
		"a timeout":                        {"review timed out after 1200s", ""},
		"no error":                         {"", ""},
	} {
		data, err := json.Marshal(map[string]any{"schema_version": 1, "available": false, "error": tc.errText})
		if err != nil {
			t.Fatal(err)
		}
		if got := ProviderResetTime(data); got != tc.want {
			t.Errorf("%s: ProviderResetTime = %q, want %q", name, got, tc.want)
		}
	}
	for _, data := range []string{"", "not JSON: You've hit your usage limit. try again at 3:11 AM.", `{"error": 7}`, `["You've hit your usage limit. try again at 3:11 AM."]`} {
		if got := ProviderResetTime([]byte(data)); got != "" {
			t.Errorf("ProviderResetTime(%q) = %q, want none", data, got)
		}
	}
}

func TestParseResultOfAnOldFindingEncodesWithoutTheNewKeys(t *testing.T) {
	old := `{"schema_version":1,"review_policy":"required","available":true,"findings":[{"severity":"high","file":"a.go","line":3,"summary":"s","failure_scenario":"f"}]}`
	got, err := ParseResult([]byte(old))
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"policy":"required","available":true,"findings":[{"severity":"high","file":"a.go","line":3,"summary":"s","failure_scenario":"f"}]}`
	if string(encoded) != want {
		t.Errorf("encoded = %s\nwant %s", encoded, want)
	}
}
