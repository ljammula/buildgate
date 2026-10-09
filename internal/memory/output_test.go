package memory

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

const (
	obsA = "0123456789abcdef"
	obsB = "fedcba9876543210"
)

var inputIDs = []string{obsA, obsB}

func lessonObj(kind, reason, pre, cmd string, obs ...string) string {
	q := make([]string, len(obs))
	for i, o := range obs {
		q[i] = `"` + o + `"`
	}
	return fmt.Sprintf(`{"kind":%q,"reason":%q,"prerequisite":%q,"command":%q,"observations":[%s]}`,
		kind, reason, pre, cmd, strings.Join(q, ","))
}

func wrap(lessons ...string) string {
	return `{"schema_version":1,"lessons":[` + strings.Join(lessons, ",") + `]}`
}

func code(t *testing.T, err error) string {
	t.Helper()
	var je *JobOutputError
	if !errors.As(err, &je) {
		t.Fatalf("err = %v, want *JobOutputError", err)
	}
	if len(je.Detail) > 120 {
		t.Fatalf("detail too long: %q", je.Detail)
	}
	return je.Code
}

func TestJobOutputRejectedWithoutParsingFreeText(t *testing.T) {
	good := lessonObj("convention", "Errors are wrapped", "", "", obsA)
	big := `{"schema_version":1,"lessons":[],"x":"` + strings.Repeat("a", MaxJobOutputBytes) + `"}`
	eleven := make([]string, 11)
	for i := range eleven {
		eleven[i] = lessonObj("convention", fmt.Sprintf("Reason number %d", i), "", "", obsA)
	}
	cases := []struct{ name, in, want string }{
		{"zero length", "", "empty"},
		{"not json", "hello", "bad_json"},
		{"whitespace only", "  \n", "bad_json"},
		{"array top level", "[]", "bad_json"},
		{"unknown field", `{"schema_version":1,"lessons":[],"extra":1}`, "bad_json"},
		{"unknown lesson field", wrap(strings.Replace(good, `"kind"`, `"extra":"x","kind"`, 1)), "bad_json"},
		{"trailing data", wrap(good) + ` {}`, "bad_json"},
		{"trailing garbage", wrap(good) + `x`, "bad_json"},
		{"duplicate top key", `{"schema_version":1,"schema_version":1,"lessons":[]}`, "bad_json"},
		{"duplicate lesson key", wrap(strings.Replace(good, `"kind":"convention"`, `"kind":"convention","kind":"convention"`, 1)), "bad_json"},
		{"duplicate key by case", wrap(strings.Replace(good, `"kind":"convention"`, `"kind":"convention","Kind":"command"`, 1)), "bad_json"},
		{"wrong case key", wrap(strings.Replace(good, `"kind"`, `"Kind"`, 1)), "bad_json"},
		{"lessons object", `{"schema_version":1,"lessons":{}}`, "bad_json"},
		{"lessons null", `{"schema_version":1,"lessons":null}`, "bad_json"},
		{"lessons missing", `{"schema_version":1}`, "bad_json"},
		{"version missing", `{"lessons":[]}`, "bad_json"},
		{"version 2", `{"schema_version":2,"lessons":[]}`, "bad_json"},
		{"version string", `{"schema_version":"1","lessons":[]}`, "bad_json"},
		{"version float", `{"schema_version":1.5,"lessons":[]}`, "bad_json"},
		{"number for string", `{"schema_version":1,"lessons":[{"kind":1}]}`, "bad_json"},
		{"deep nesting", `{"schema_version":1,"lessons":[[[[[[[[[[1]]]]]]]]]]}`, "bad_json"},
		{"deep nesting objects", `{"schema_version":1,"lessons":[],"a":{"a":{"a":{"a":{"a":{"a":1}}}}}}`, "bad_json"},
		{"invalid utf8", "{\"schema_version\":1,\"lessons\":[],\"x\":\"\xff\"}", "bad_json"},
		{"bom", "\xef\xbb\xbf" + wrap(good), "bad_json"},
		{"11 lessons", wrap(eleven...), "bad_json"},
		{"too large", big, "too_large"},
		{"unknown observation", wrap(lessonObj("convention", "Errors are wrapped", "", "", "aaaaaaaaaaaaaaaa")), "unknown_observation"},
		{"no observation", wrap(lessonObj("convention", "Errors are wrapped", "", "")), "unknown_observation"},
		{"one of two unknown", wrap(lessonObj("convention", "Errors are wrapped", "", "", obsA, "aaaaaaaaaaaaaaaa")), "unknown_observation"},
		{"bad kind", wrap(lessonObj("shell", "Errors are wrapped", "", "", obsA)), "bad_text"},
		{"bad reason", wrap(lessonObj("convention", "run `x` now", "", "", obsA)), "bad_text"},
		{"empty reason", wrap(lessonObj("convention", "", "", "", obsA)), "bad_text"},
		{"bad command", wrap(lessonObj("command", "ok", "make a", "make; b", obsA)), "bad_text"},
		{"command lesson missing prerequisite", wrap(lessonObj("command", "ok", "", "make b", obsA)), "bad_text"},
		{"convention with command", wrap(lessonObj("convention", "ok", "", "make b", obsA)), "bad_text"},
		{"one bad among good", wrap(good, lessonObj("convention", "Another good one", "", "", obsB), lessonObj("convention", "bad <b>", "", "", obsA)), "bad_text"},
		{"unknown observation among good", wrap(good, lessonObj("convention", "Another good one", "", "", "aaaaaaaaaaaaaaaa")), "unknown_observation"},
		{"null reason", wrap(`{"kind":"convention","reason":null,"observations":["` + obsA + `"]}`), "bad_text"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := ParseJobOutput([]byte(c.in), inputIDs, nil)
			if err == nil {
				t.Fatalf("accepted: %+v", got)
			}
			if got != nil {
				t.Fatalf("partial accept: %+v", got)
			}
			if g := code(t, err); g != c.want {
				t.Fatalf("code = %s (%v), want %s", g, err, c.want)
			}
		})
	}
}

func TestJobOutputExactlyAtTheLimitIsNotTooLarge(t *testing.T) {
	pad := strings.Repeat(" ", MaxJobOutputBytes-len(wrap()))
	if _, err := ParseJobOutput([]byte(wrap()+pad), inputIDs, nil); err != nil {
		t.Fatalf("at the limit: %v", err)
	}
	if _, err := ParseJobOutput([]byte(wrap()+pad+" "), inputIDs, nil); code(t, err) != "too_large" {
		t.Fatalf("one over: %v", err)
	}
}

func TestJobOutputAccepted(t *testing.T) {
	in := wrap(
		lessonObj("command", "the schema must exist", "make db-migrate", "make test", obsA, obsB),
		lessonObj("convention", "Errors are wrapped.", "", "", obsB),
	)
	got, err := ParseJobOutput([]byte(in), inputIDs, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d lessons", len(got))
	}
	if got[0].Line != "- Run `make db-migrate` before `make test`: the schema must exist." || got[1].Line != "- Errors are wrapped." {
		t.Fatalf("lines: %q %q", got[0].Line, got[1].Line)
	}
	if got[0].State != StateCandidate || len(got[0].Observations) != 2 {
		t.Fatalf("lesson: %+v", got[0])
	}
	for _, ok := range []string{wrap(), " " + wrap() + "\n", `{"lessons":[],"schema_version":1}`} {
		l, err := ParseJobOutput([]byte(ok), nil, nil)
		if err != nil || l == nil || len(l) != 0 {
			t.Errorf("%q: %v %v", ok, l, err)
		}
	}
}

func TestJobOutputDropsDuplicatesSilently(t *testing.T) {
	a := lessonObj("convention", "Errors are wrapped", "", "", obsA)
	b := lessonObj("convention", "Errors are wrapped.", "", "", obsB) // same rendered line
	c := lessonObj("convention", "Logs are structured", "", "", obsA)
	first, err := ParseJobOutput([]byte(wrap(a, b, c)), inputIDs, nil)
	if err != nil || len(first) != 2 {
		t.Fatalf("within output: %v %v", first, err)
	}
	existing := map[string]bool{first[0].LineSHA256: true}
	got, err := ParseJobOutput([]byte(wrap(a, c)), inputIDs, existing)
	if err != nil || len(got) != 1 || got[0].Line != "- Logs are structured." {
		t.Fatalf("against existing: %v %v", got, err)
	}
}

func TestJobOutputErrorDoesNotEchoModelText(t *testing.T) {
	secret := strings.Repeat("SECRETTEXT", 20)
	in := wrap(lessonObj("convention", secret+"!", "", "", obsA))
	_, err := ParseJobOutput([]byte(in), inputIDs, nil)
	if err == nil || strings.Contains(err.Error(), secret) || len(err.Error()) > 300 {
		t.Fatalf("err = %v", err)
	}
	_, err = ParseJobOutput([]byte(wrap(lessonObj("convention", "ok", "", "", secret))), inputIDs, nil)
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("err = %v", err)
	}
}

func FuzzParseJobOutput(f *testing.F) {
	good := lessonObj("command", "the schema must exist", "make db-migrate", "make test", obsA)
	for _, s := range []string{
		"", wrap(), wrap(good), wrap(good, good), `{"schema_version":1,"lessons":null}`,
		`{"schema_version":1,"lessons":[],"schema_version":1}`, `[[[[[[[[`, `{"a":`, "\xef\xbb\xbf{}",
		wrap(strings.Replace(good, "make", "ma\\u0000ke", 1)), wrap(strings.Replace(good, "reason", "Reason", 1)),
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, raw []byte) {
		lessons, err := ParseJobOutput(raw, inputIDs, nil)
		if err != nil {
			if lessons != nil {
				t.Fatal("partial accept")
			}
			var je *JobOutputError
			if !errors.As(err, &je) {
				t.Fatalf("untyped error %v", err)
			}
			return
		}
		for _, l := range lessons {
			line, rerr := RenderLine(l.Kind, l.Reason, l.Prerequisite, l.Command)
			if rerr != nil || line != l.Line || strings.ContainsAny(l.Line, "\n\r") {
				t.Fatalf("accepted a lesson that does not re-render: %+v", l)
			}
		}
	})
}
