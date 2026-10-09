// Package memory is the trust boundary between text a model produced and
// text that is written into a repository's AGENTS.md. Every function here is
// total: any input gives a value or a typed refusal, never a panic. It runs
// no process, opens no network connection and reads no file.
package memory

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"buildgate/internal/sanitize"
)

// ErrLessonText wraps every refusal of a lesson's text; ErrLessonState wraps
// every refused move between states.
var (
	ErrLessonText  = errors.New("memory: lesson text refused")
	ErrLessonState = errors.New("memory: lesson move refused")
)

// State is where a lesson stands between a model's suggestion and a line in
// force in the repository.
type State string

const (
	StateCandidate       State = "candidate"
	StateChecked         State = "checked"
	StateWaitingOperator State = "waiting_operator"
	StateProposed        State = "proposed"
	StateInForce         State = "in_force"
	StateRetireProposed  State = "retire_proposed"
	StateRetired         State = "retired"
	StateDropped         State = "dropped"
)

// Kind is the shape of a lesson's rendered line.
type Kind string

const (
	KindCommand    Kind = "command"
	KindConvention Kind = "convention"
)

// Transition is one recorded move of a lesson.
type Transition struct {
	From   State  `json:"from"`
	To     State  `json:"to"`
	At     string `json:"at"`
	By     string `json:"by"`
	Reason string `json:"reason,omitempty"`
}

// CheckResult records one run of the lesson check. Outcome is "passed",
// "not_needed" or "did_not_fix".
type CheckResult struct {
	Commit      string `json:"commit"`
	WithoutExit int    `json:"without_exit"`
	WithExit    int    `json:"with_exit"`
	WithoutLog  string `json:"without_log,omitempty"`
	WithLog     string `json:"with_log,omitempty"`
	Outcome     string `json:"outcome"`
	At          string `json:"at"`
}

// Lesson is one candidate or in-force line of repository memory.
type Lesson struct {
	ID              string       `json:"id"`
	Kind            Kind         `json:"kind"`
	Reason          string       `json:"reason"`
	Prerequisite    string       `json:"prerequisite,omitempty"`
	Command         string       `json:"command,omitempty"`
	Line            string       `json:"line"`
	LineSHA256      string       `json:"line_sha256"`
	Observations    []string     `json:"observations,omitempty"`
	Signature       string       `json:"signature,omitempty"`
	Check           string       `json:"check,omitempty"`
	State           State        `json:"state"`
	History         []Transition `json:"history,omitempty"`
	Checked         *CheckResult `json:"checked,omitempty"`
	RequestID       string       `json:"request_id,omitempty"`
	Confirmed       int          `json:"confirmed,omitempty"`
	Recurred        int          `json:"recurred,omitempty"`
	InForceRuns     int          `json:"in_force_runs,omitempty"`
	LastConfirmedAt string       `json:"last_confirmed_at,omitempty"`
}

const (
	maxTextRunes    = 120
	maxCommandBytes = 120
	maxHistory      = 50
	maxObservations = 50
	maxEchoBytes    = 40
	maxMoveTextLen  = 200
	reasonPunct     = " .,:;()'\"/_=+-"
	commandPunct    = " ._/:=-"
)

var (
	ipv4Shape     = regexp.MustCompile(`[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+`)
	observationID = regexp.MustCompile(`^[0-9a-f]{16}$`)
)

// clip makes refused text safe to put in an error: cleaned, at most 40 bytes.
func clip(s string) string {
	s = sanitize.Line(s)
	if len(s) > maxEchoBytes {
		s = strings.ToValidUTF8(s[:maxEchoBytes], "")
	}
	return s
}

func textErr(field, why, s string) error {
	return fmt.Errorf("%w: %s %s (%q)", ErrLessonText, field, why, clip(s))
}

func isASCIIAlnum(r rune) bool {
	return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9'
}

// firstOutside returns the index of the first byte of s that is neither an
// ASCII letter or digit nor one of punct, or -1.
func firstOutside(s, punct string) int {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !isASCIIAlnum(rune(c)) && strings.IndexByte(punct, c) < 0 {
			return i
		}
	}
	return -1
}

// shapeProblem reports a leading, trailing or doubled space.
func shapeProblem(s string) string {
	switch {
	case strings.HasPrefix(s, " ") || strings.HasSuffix(s, " "):
		return "has a leading or trailing space"
	case strings.Contains(s, "  "):
		return "has a doubled space"
	}
	return ""
}

var forbiddenParts = []string{"://", "/Users/", "/home/"}

func forbiddenPart(s string) string {
	for _, p := range forbiddenParts {
		if strings.Contains(s, p) {
			return p
		}
	}
	return ""
}

// ValidateReason refuses a reason that is not one plain sentence-like line of
// at most 120 runes from [A-Za-z0-9 .,:;()'"/_=+-]. It never repairs.
func ValidateReason(s string) error {
	switch {
	case s == "":
		return textErr("reason", "is empty", s)
	case utf8.RuneCountInString(s) > maxTextRunes:
		return textErr("reason", "is longer than 120 runes", s)
	case firstOutside(s, reasonPunct) >= 0:
		return textErr("reason", "has a character outside the allowed set", s)
	}
	if why := shapeProblem(s); why != "" {
		return textErr("reason", why, s)
	}
	if sanitize.Line(s) != s {
		return textErr("reason", "changes when cleaned", s)
	}
	if p := forbiddenPart(s); p != "" {
		return textErr("reason", "contains "+p, s)
	}
	if ipv4Shape.MatchString(s) {
		return textErr("reason", "contains an address-shaped token", s)
	}
	return nil
}

// ValidateCommand refuses anything but 1..120 bytes of [A-Za-z0-9 ._/:=-]
// that does not start with "-" and has no leading, trailing or doubled space.
// What remains cannot hold a shell operator, quote, variable, glob,
// redirection or newline.
func ValidateCommand(s string) error {
	switch {
	case s == "":
		return textErr("command", "is empty", s)
	case len(s) > maxCommandBytes:
		return textErr("command", "is longer than 120 bytes", s)
	case firstOutside(s, commandPunct) >= 0:
		return textErr("command", "has a character outside the allowed set", s)
	case s[0] == '-':
		return textErr("command", "starts with a dash", s)
	}
	if why := shapeProblem(s); why != "" {
		return textErr("command", why, s)
	}
	if p := forbiddenPart(s); p != "" {
		return textErr("command", "contains "+p, s)
	}
	return nil
}

// RenderLine validates every field for the kind and renders the one line the
// repository will hold. A command lesson needs both commands; a convention
// lesson must have neither.
func RenderLine(kind Kind, reason, prerequisite, command string) (string, error) {
	if err := ValidateReason(reason); err != nil {
		return "", err
	}
	sentence := reason
	if !strings.HasSuffix(sentence, ".") {
		sentence += "."
	}
	switch kind {
	case KindCommand:
		if err := ValidateCommand(prerequisite); err != nil {
			return "", fmt.Errorf("prerequisite: %w", err)
		}
		if err := ValidateCommand(command); err != nil {
			return "", err
		}
		return "- Run `" + prerequisite + "` before `" + command + "`: " + sentence, nil
	case KindConvention:
		if prerequisite != "" || command != "" {
			return "", textErr("convention", "must have no commands", prerequisite+command)
		}
		return "- " + sentence, nil
	}
	return "", textErr("kind", "is not command or convention", string(kind))
}

// lineHash is the hex SHA-256 of a rendered line.
func lineHash(line string) string {
	sum := sha256.Sum256([]byte(line))
	return hex.EncodeToString(sum[:])
}

// cleanObservations checks and deduplicates observation ids in order.
func cleanObservations(ids []string) ([]string, error) {
	seen := map[string]bool{}
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if !observationID.MatchString(id) {
			return nil, textErr("observation", "is not a 16-hex id", id)
		}
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	if len(out) == 0 || len(out) > maxObservations {
		return nil, fmt.Errorf("%w: observations must number 1 to 50", ErrLessonText)
	}
	return out, nil
}

// NewLesson renders and hashes a lesson in state candidate.
func NewLesson(kind Kind, reason, prerequisite, command string, observations []string) (Lesson, error) {
	line, err := RenderLine(kind, reason, prerequisite, command)
	if err != nil {
		return Lesson{}, err
	}
	obs, err := cleanObservations(observations)
	if err != nil {
		return Lesson{}, err
	}
	sum := lineHash(line)
	return Lesson{
		ID: sum[:16], Kind: kind, Reason: reason, Prerequisite: prerequisite,
		Command: command, Line: line, LineSHA256: sum, Observations: obs,
		State: StateCandidate,
	}, nil
}

// moves is the one table of allowed moves. Any state but dropped may be
// dropped (a memory drop, or a failed check).
var moves = map[State][]State{
	StateCandidate:       {StateChecked, StateWaitingOperator, StateDropped},
	StateWaitingOperator: {StateChecked, StateDropped},
	StateChecked:         {StateProposed, StateDropped},
	StateProposed:        {StateInForce, StateChecked, StateDropped},
	StateInForce:         {StateRetireProposed, StateRetired, StateDropped},
	StateRetireProposed:  {StateRetired, StateDropped},
	StateRetired:         {StateDropped},
}

// CanMove reports whether the table allows from -> to.
func CanMove(from, to State) bool {
	for _, s := range moves[from] {
		if s == to {
			return true
		}
	}
	return false
}

func moveText(s string) string {
	s = sanitize.Line(s)
	if len(s) > maxMoveTextLen {
		s = strings.ToValidUTF8(s[:maxMoveTextLen], "")
	}
	return s
}

// Move appends a Transition, or refuses a move the table does not allow. The
// history keeps the newest 50 entries.
func (l *Lesson) Move(to State, at, by, reason string) error {
	if l == nil {
		return fmt.Errorf("%w: no lesson", ErrLessonState)
	}
	if !CanMove(l.State, to) {
		return fmt.Errorf("%w: %q to %q", ErrLessonState, clip(string(l.State)), clip(string(to)))
	}
	l.History = append(l.History, Transition{
		From: l.State, To: to, At: moveText(at), By: moveText(by), Reason: moveText(reason),
	})
	if n := len(l.History); n > maxHistory {
		l.History = append([]Transition(nil), l.History[n-maxHistory:]...)
	}
	l.State = to
	return nil
}
