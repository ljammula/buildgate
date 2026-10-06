// Package verify holds the reference-oracle check for the
// json-merge-patch-rfc7396 proving-ground ticket (differential/
// reference-oracle testing).
//
// This file is NOT agent-authored and is NOT in the ticket's
// Allowed-Files -- factoryd's diff_scope gate rejects any run that
// touches it, so the candidate implementation cannot make this check
// pass by editing the check itself. The oracle it checks against is
// RFC 7396's own published Appendix A "Example Test Cases" table
// (https://www.rfc-editor.org/rfc/rfc7396#appendix-A), transcribed
// verbatim below -- an independent, pre-existing, externally-authored
// source of truth, not anything derived from the candidate's own code.
// Compare with the "All Smoke, No Alarm" oracle-signal taxonomy
// (arXiv:2606.18168): every case below is a strong oracle signal (an
// independently-derived expected value), not a weak one.
//
// Known limitation (documented, not solved, by design -- see the
// ticket's own "Known limitation" section): this file sits in the
// same repo the agent builds against, so a sufficiently adversarial
// agent could read it during its own build phase and special-case
// these exact 15 inputs rather than implement the general algorithm.
// diff_scope stops it from editing this file; nothing here stops it
// from reading it. This run validates the gate MECHANISM end to end
// (does an independent, non-agent-authored check actually run and
// actually gate the release decision), not adversarial robustness
// against a model trying to game the eval.
package verify

import (
	"encoding/json"
	"reflect"
	"testing"

	mergepatch "json-merge-patch-rfc7396"
)

// rfc7396Cases is RFC 7396 Appendix A, transcribed verbatim from the
// primary source, row for row, in the RFC's own order.
var rfc7396Cases = []struct {
	original, patch, result string
}{
	{`{"a":"b"}`, `{"a":"c"}`, `{"a":"c"}`},
	{`{"a":"b"}`, `{"b":"c"}`, `{"a":"b","b":"c"}`},
	{`{"a":"b"}`, `{"a":null}`, `{}`},
	{`{"a":"b","b":"c"}`, `{"a":null}`, `{"b":"c"}`},
	{`{"a":["b"]}`, `{"a":"c"}`, `{"a":"c"}`},
	{`{"a":"c"}`, `{"a":["b"]}`, `{"a":["b"]}`},
	{`{"a":{"b":"c"}}`, `{"a":{"b":"d","c":null}}`, `{"a":{"b":"d"}}`},
	{`{"a":[{"b":"c"}]}`, `{"a":[1]}`, `{"a":[1]}`},
	{`["a","b"]`, `["c","d"]`, `["c","d"]`},
	{`{"a":"b"}`, `["c"]`, `["c"]`},
	{`{"a":"foo"}`, `null`, `null`},
	{`{"a":"foo"}`, `"bar"`, `"bar"`},
	{`{"e":null}`, `{"a":1}`, `{"e":null,"a":1}`},
	{`[1,2]`, `{"a":"b","c":null}`, `{"a":"b"}`},
	{`{}`, `{"a":{"bb":{"ccc":null}}}`, `{"a":{"bb":{}}}`},
}

// TestRFC7396MergePatchOracle is the reference-oracle gate: it never
// looks at the candidate's own tests, only at MergePatch's actual
// output for every RFC-published case, compared for semantic JSON
// equality (key order and whitespace don't matter; value shape and
// content do).
func TestRFC7396MergePatchOracle(t *testing.T) {
	for i, tc := range rfc7396Cases {
		got, err := mergepatch.MergePatch([]byte(tc.original), []byte(tc.patch))
		if err != nil {
			t.Errorf("case %d: original=%s patch=%s: MergePatch returned error: %v", i, tc.original, tc.patch, err)
			continue
		}
		if !semanticJSONEqual(t, got, []byte(tc.result)) {
			t.Errorf("case %d: original=%s patch=%s: got %s, want %s (RFC 7396 Appendix A)", i, tc.original, tc.patch, got, tc.result)
		}
	}
}

func semanticJSONEqual(t *testing.T, a, b []byte) bool {
	t.Helper()
	var va, vb any
	if err := json.Unmarshal(a, &va); err != nil {
		t.Fatalf("oracle bug: candidate output %s does not parse as JSON: %v", a, err)
	}
	if err := json.Unmarshal(b, &vb); err != nil {
		t.Fatalf("oracle bug: expected value %s does not parse as JSON: %v", b, err)
	}
	return reflect.DeepEqual(va, vb)
}
