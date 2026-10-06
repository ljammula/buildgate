package codereview

import (
	"encoding/json"
	"strings"
	"testing"

	"buildgate/internal/run"
)

func TestArgsIncludesReviewBaseSHAAndThinking(t *testing.T) {
	got := Args("/path/code_review.py", "/ws", "/ws/ticket.md", "required", "deadbeef", "max", "pi")

	want := []string{
		"/path/code_review.py",
		"--workspace", "/ws",
		"--spec", "/ws/ticket.md",
		"--review-policy", "required",
		"--review-base-sha", "deadbeef",
		"--thinking", "max",
		"--harness", "pi",
	}
	if len(got) != len(want) {
		t.Fatalf("Args() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Args()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestArgsOmitsReviewBaseSHAAndThinkingWhenEmpty(t *testing.T) {
	got := Args("/path/code_review.py", "/ws", "/ws/ticket.md", "advisory", "", "", "pi")
	want := []string{
		"/path/code_review.py",
		"--workspace", "/ws",
		"--spec", "/ws/ticket.md",
		"--review-policy", "advisory",
		"--harness", "pi",
	}
	if len(got) != len(want) {
		t.Fatalf("Args() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Args()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
	for i, arg := range got {
		if arg == "--review-base-sha" || arg == "--thinking" {
			t.Fatalf("Args() included %q at index %d when its value was empty: %v", arg, i, got)
		}
	}
}

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

func TestArgsPassesHarnessWhenSet(t *testing.T) {
	got := Args("/path/code_review.py", "/ws", "/ws/ticket.md", "advisory", "", "", "pifork")
	if n := len(got); n < 2 || got[n-2] != "--harness" || got[n-1] != "pifork" {
		t.Fatalf("Args() = %v, want it to end with --harness pifork", got)
	}
}
