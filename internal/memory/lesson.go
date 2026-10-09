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

// State is where a lesson stands in the store. A line in force is not a
// state: the fenced section of AGENTS.md at HEAD is the memory, and a lesson
// whose line is there is removed from the store.
type State string

const (
	StateCandidate State = "candidate"
	StateProposed  State = "proposed"
	StateDropped   State = "dropped"
)

// Who wrote a lesson down.
const (
	SourceAgent    = "agent"
	SourceOperator = "operator"
)

// Transition is one recorded move of a lesson.
type Transition struct {
	From   State  `json:"from"`
	To     State  `json:"to"`
	At     string `json:"at"`
	By     string `json:"by"`
	Reason string `json:"reason,omitempty"`
}

// Lesson is one candidate line of repository memory: what a build agent
// noted, or the operator typed, rendered as the one line the section would
// hold.
type Lesson struct {
	ID         string       `json:"id"`
	Reason     string       `json:"reason"`
	Line       string       `json:"line"`
	LineSHA256 string       `json:"line_sha256"`
	Source     string       `json:"source"`
	State      State        `json:"state"`
	History    []Transition `json:"history,omitempty"`
	RequestID  string       `json:"request_id,omitempty"`
	// Seen is how many distinct runs said the line; Runs lists them, newest
	// last, at most MaxLessonRuns.
	Seen        int      `json:"seen,omitempty"`
	Runs        []string `json:"runs,omitempty"`
	FirstSeenAt string   `json:"first_seen_at,omitempty"`
	LastSeenAt  string   `json:"last_seen_at,omitempty"`
}

const (
	maxTextRunes    = 120
	maxCommandBytes = 120
	maxHistory      = 50
	maxEchoBytes    = 40
	maxMoveTextLen  = 200
	reasonPunct     = " .,:;()'\"/=+-"
	commandPunct    = " ._/:=-"
)

// MaxLessonRuns bounds the run ids one lesson keeps.
const MaxLessonRuns = 20

var (
	ipv4Shape = regexp.MustCompile(`[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+`)
	// hexColonShape is three or more short hex groups joined by colons: the
	// shape of an IPv6 or hardware address.
	hexColonShape = regexp.MustCompile(`[0-9A-Fa-f]{1,4}(:[0-9A-Fa-f]{1,4}){2,}`)
	// listStart is how a line would open a nested list, an ordered list or a
	// quote once it follows "- ".
	listStart = regexp.MustCompile(`^([-+>]|[0-9]+[.)])`)
	// absolutePath is a token that starts with "/" and a letter.
	absolutePath = regexp.MustCompile(`(^|[^A-Za-z0-9._/-])/[A-Za-z]`)
	// longToken is 20 or more characters with no space from the set keys,
	// hashes and encoded secrets are written in.
	longToken = regexp.MustCompile(`[A-Za-z0-9+/=_-]{20,}`)
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

// forbiddenParts are refused anywhere, in any letter case: a URL in any form
// ("//" covers "://" and a protocol-relative one, "www." an autolink), a home
// path and a doubled colon.
var forbiddenParts = []string{"//", "www.", "/users/", "/home/", "::"}

func forbiddenPart(s string) string {
	lower := strings.ToLower(s)
	for _, p := range forbiddenParts {
		if strings.Contains(lower, p) {
			return p
		}
	}
	return ""
}

// forbiddenShape names a token shape refused anywhere in a reason or a
// command: a long unbroken token, or an address.
func forbiddenShape(s string) string {
	switch {
	case longToken.MatchString(s):
		return "has an unbroken token of 20 or more characters"
	case ipv4Shape.MatchString(s) || hexColonShape.MatchString(s):
		return "contains an address-shaped token"
	}
	return ""
}

// ValidateReason refuses a reason that is not one plain sentence-like line of
// at most 120 runes. Outside backticks the characters are
// [A-Za-z0-9 .,:;()'"/=+-], with no absolute path; a pair of backticks quotes
// a command, whose inside must pass ValidateCommand. A backtick without its
// pair is refused, and so is a start that Markdown would read as a nested
// list, an ordered list or a quote. Anywhere: no URL in any form, no home
// path, no address, no unbroken token of 20 characters. It never repairs.
func ValidateReason(s string) error {
	switch {
	case s == "":
		return textErr("reason", "is empty", s)
	case utf8.RuneCountInString(s) > maxTextRunes:
		return textErr("reason", "is longer than 120 runes", s)
	}
	if listStart.MatchString(s) {
		return textErr("reason", "starts like a list item or a quote", s)
	}
	if err := validateSpans(s); err != nil {
		return err
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
	if why := forbiddenShape(s); why != "" {
		return textErr("reason", why, s)
	}
	return nil
}

// validateSpans splits s at its backticks: the pieces outside a pair follow
// the reason's character rule, the pieces inside the command's.
func validateSpans(s string) error {
	parts := strings.Split(s, "`")
	if len(parts)%2 == 0 {
		return textErr("reason", "has a backtick without its pair", s)
	}
	for i, part := range parts {
		if i%2 == 1 {
			if err := ValidateCommand(part); err != nil {
				return fmt.Errorf("reason has a quoted command that is refused: %w", err)
			}
			continue
		}
		if firstOutside(part, reasonPunct) >= 0 {
			return textErr("reason", "has a character outside the allowed set", s)
		}
		if absolutePath.MatchString(part) {
			return textErr("reason", "has an absolute path", s)
		}
	}
	return nil
}

// ValidateCommand refuses anything but 1..120 bytes of [A-Za-z0-9 ._/:=-]
// that does not start with "-" and has no leading, trailing or doubled space,
// no URL, home path or address, and no unbroken token of 20 characters.
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
	if why := forbiddenShape(s); why != "" {
		return textErr("command", why, s)
	}
	return nil
}

// RenderLine validates reason and renders the one line the repository will
// hold: "- <reason>.".
func RenderLine(reason string) (string, error) {
	if err := ValidateReason(reason); err != nil {
		return "", err
	}
	if !strings.HasSuffix(reason, ".") {
		reason += "."
	}
	// The full stop can complete a refused shape ("www" becomes "www.").
	if p := forbiddenPart(reason); p != "" {
		return "", textErr("reason", "contains "+p, reason)
	}
	return "- " + reason, nil
}

// lineHash is the hex SHA-256 of a rendered line.
func lineHash(line string) string {
	sum := sha256.Sum256([]byte(line))
	return hex.EncodeToString(sum[:])
}

// NewLesson renders and hashes a lesson in state candidate, written down by
// source (SourceAgent or SourceOperator) at now.
func NewLesson(reason, source, now string) (Lesson, error) {
	if source != SourceAgent && source != SourceOperator {
		return Lesson{}, textErr("source", "is not agent or operator", source)
	}
	line, err := RenderLine(reason)
	if err != nil {
		return Lesson{}, err
	}
	sum := lineHash(line)
	now = moveText(now)
	return Lesson{
		ID: sum[:16], Reason: reason, Line: line, LineSHA256: sum, Source: source,
		State: StateCandidate, FirstSeenAt: now, LastSeenAt: now,
	}, nil
}

// moves is the one table of allowed moves: a candidate is proposed or
// dropped, a proposed lesson goes back to candidate when its request ended
// without the line or is dropped, and the operator may re-add a dropped one.
var moves = map[State][]State{
	StateCandidate: {StateProposed, StateDropped},
	StateProposed:  {StateCandidate, StateDropped},
	StateDropped:   {StateCandidate},
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
