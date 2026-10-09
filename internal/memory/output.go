package memory

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"
)

// MaxJobOutputBytes bounds what the consolidation job may write.
const MaxJobOutputBytes = 16 << 10

const (
	maxJobLessons = 10
	maxJSONDepth  = 4
)

// JobOutputError is the whole-output rejection of a consolidation job. Code is
// bad_json, too_large, unknown_observation, bad_text or empty; Detail never
// holds model text beyond 40 cleaned bytes.
type JobOutputError struct{ Code, Detail string }

func (e *JobOutputError) Error() string {
	if e.Detail == "" {
		return "memory job output rejected: " + e.Code
	}
	return "memory job output rejected: " + e.Code + ": " + e.Detail
}

func rejected(code, detail string) error {
	return &JobOutputError{Code: code, Detail: detail}
}

type lessonJSON struct {
	Kind         string   `json:"kind"`
	Reason       string   `json:"reason"`
	Prerequisite string   `json:"prerequisite"`
	Command      string   `json:"command"`
	Observations []string `json:"observations"`
}

type outputJSON struct {
	SchemaVersion *int          `json:"schema_version"`
	Lessons       *[]lessonJSON `json:"lessons"`
}

var (
	topKeys    = map[string]bool{"schema_version": true, "lessons": true}
	lessonKeys = map[string]bool{"kind": true, "reason": true, "prerequisite": true, "command": true, "observations": true}
)

var errShape = errors.New("shape")

// walkValue reads one JSON value, refusing duplicate keys (compared without
// case, as the decoder matches them), keys of the wrong case, depth beyond the
// limit and a "lessons" value that is not an array.
func walkValue(dec *json.Decoder, depth int, tok json.Token) error {
	d, isDelim := tok.(json.Delim)
	if !isDelim {
		return nil
	}
	if depth > maxJSONDepth {
		return errShape
	}
	if d == '[' {
		for dec.More() {
			t, err := dec.Token()
			if err != nil {
				return err
			}
			if err := walkValue(dec, depth+1, t); err != nil {
				return err
			}
		}
		_, err := dec.Token()
		return err
	}
	return walkObject(dec, depth)
}

func allowedKeys(depth int) map[string]bool {
	switch depth {
	case 0:
		return topKeys
	case 2:
		return lessonKeys
	}
	return nil
}

func walkObject(dec *json.Decoder, depth int) error {
	seen := map[string]bool{}
	allowed := allowedKeys(depth)
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return err
		}
		key, _ := kt.(string)
		folded := strings.ToLower(key)
		if seen[folded] || (allowed != nil && !allowed[key]) {
			return errShape
		}
		seen[folded] = true
		vt, err := dec.Token()
		if err != nil {
			return err
		}
		if depth == 0 && key == "lessons" && vt != json.Delim('[') {
			return errShape
		}
		if err := walkValue(dec, depth+1, vt); err != nil {
			return err
		}
	}
	_, err := dec.Token()
	return err
}

// checkShape runs the structural walk over the whole input.
func checkShape(raw []byte) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	tok, err := dec.Token()
	if err != nil || tok != json.Delim('{') {
		return errShape
	}
	if err := walkObject(dec, 0); err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		return errShape
	}
	return nil
}

func decodeOutput(raw []byte) (outputJSON, error) {
	var out outputJSON
	if len(raw) == 0 {
		return out, rejected("empty", "")
	}
	if len(raw) > MaxJobOutputBytes {
		return out, rejected("too_large", fmt.Sprintf("%d bytes", len(raw)))
	}
	if !utf8.Valid(raw) || bytes.HasPrefix(raw, []byte("\xef\xbb\xbf")) || checkShape(raw) != nil {
		return out, rejected("bad_json", "")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&out); err != nil {
		return out, rejected("bad_json", "")
	}
	if _, err := dec.Token(); err != io.EOF {
		return out, rejected("bad_json", "trailing data")
	}
	if out.SchemaVersion == nil || *out.SchemaVersion != 1 || out.Lessons == nil || len(*out.Lessons) > maxJobLessons {
		return out, rejected("bad_json", "schema")
	}
	return out, nil
}

func buildLesson(i int, in lessonJSON, known map[string]bool) (Lesson, error) {
	if len(in.Observations) == 0 {
		return Lesson{}, rejected("unknown_observation", fmt.Sprintf("lesson %d has no observation", i))
	}
	for _, id := range in.Observations {
		if !known[id] {
			return Lesson{}, rejected("unknown_observation", fmt.Sprintf("lesson %d: %s", i, clip(id)))
		}
	}
	l, err := NewLesson(Kind(in.Kind), in.Reason, in.Prerequisite, in.Command, in.Observations)
	if err != nil {
		return Lesson{}, rejected("bad_text", fmt.Sprintf("lesson %d: %s", i, clip(err.Error())))
	}
	return l, nil
}

// ParseJobOutput reads a consolidation job's output. Any violation rejects the
// whole output; a lesson that repeats another of the output or one of
// existingLineSHA256 is dropped silently. No lessons is not an error.
func ParseJobOutput(raw []byte, inputObservationIDs []string, existingLineSHA256 map[string]bool) ([]Lesson, error) {
	out, err := decodeOutput(raw)
	if err != nil {
		return nil, err
	}
	known := map[string]bool{}
	for _, id := range inputObservationIDs {
		known[id] = true
	}
	lessons := []Lesson{}
	seen := map[string]bool{}
	for i, in := range *out.Lessons {
		l, err := buildLesson(i, in, known)
		if err != nil {
			return nil, err
		}
		if seen[l.LineSHA256] || existingLineSHA256[l.LineSHA256] {
			continue
		}
		seen[l.LineSHA256] = true
		lessons = append(lessons, l)
	}
	return lessons, nil
}
