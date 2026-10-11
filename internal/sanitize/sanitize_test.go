package sanitize

import (
	"strings"
	"testing"
)

// TestTextStripsANSIAndControlCharacters covers the behavior
// internal/triage's extractFailureMarker now depends on directly: an
// ANSI color escape and a raw control character must both be gone from
// the result, while ordinary text survives.
func TestTextStripsANSIAndControlCharacters(t *testing.T) {
	in := "found \x1b[31m'EOF'\x1b[0m\x07 here"
	got := Text(in)
	for _, bad := range []string{"\x1b", "\x07"} {
		if strings.Contains(got, bad) {
			t.Errorf("Text(%q) = %q, want no %q", in, got, bad)
		}
	}
	if !strings.Contains(got, "found") || !strings.Contains(got, "'EOF'") || !strings.Contains(got, "here") {
		t.Errorf("Text(%q) = %q, want the plain text preserved", in, got)
	}
}

// TestTextRedactsObviousSecrets locks in the secret-redaction behavior
// moved here from cmd/factoryd's own sanitizeLogText, so a future change
// to this package can't silently drop it while
// cmd/factoryd/oracle_draft_job_test.go's own (still-passing) coverage of
// the now-thin sanitizeLogText wrapper obscures the regression.
func TestTextRedactsObviousSecrets(t *testing.T) {
	got := Text("Authorization: Bearer sk-abcdefgh12345678")
	if strings.Contains(got, "abcdefgh12345678") {
		t.Errorf("Text() = %q, want the bearer token redacted", got)
	}
}

// TestTextRedactsExtendedSecretShapes covers redaction shapes beyond the
// original hand-picked ones, since pi's own stderr can end up in
// operator-facing halt reasons.
func TestTextRedactsExtendedSecretShapes(t *testing.T) {
	cases := []struct {
		name   string
		in     string
		secret string
	}{
		{"github personal token", "token: ghp_abcdefghijklmnopqrstuvwxyz0123", "ghp_abcdefghijklmnopqrstuvwxyz0123"},
		{"github oauth token", "ghu_abcdefghijklmnopqrstuvwxyz0123456789", "ghu_abcdefghijklmnopqrstuvwxyz0123456789"},
		{"jwt", "Set-Cookie: session=eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.SflKxwRJSMeKKF2QT4fwpMeJf36POk6yJV_adQssw5c", "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.SflKxwRJSMeKKF2QT4fwpMeJf36POk6yJV_adQssw5c"},
		{"json api_key field", `{"api_key": "abcdef0123456789"}`, "abcdef0123456789"},
		{"json access_token field", `{"access_token":"abcdef0123456789"}`, "abcdef0123456789"},
		{"json refresh_token field", `{"refresh_token": "abcdef0123456789"}`, "abcdef0123456789"},
		{"json authorization field", `{"Authorization": "Bearer abcdef0123456789"}`, "abcdef0123456789"},
		{"sk- token at 16+ chars", "sk-abcdefgh12345678", "abcdefgh12345678"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Text(tc.in)
			if strings.Contains(got, tc.secret) {
				t.Errorf("Text(%q) = %q, want %q redacted", tc.in, got, tc.secret)
			}
		})
	}
}

// TestTextStripsFormatCharactersByCategory covers #3 (round-2
// adversarial review): the hand-picked invisible-character range list
// this replaced missed real Cf characters -- U+061C (ARABIC LETTER
// MARK) and U+E0041 (a Unicode TAG character, from the invisible
// prompt-injection tag block U+E0000-E007F) are both category Cf but
// were outside every one of the old ranges.
func TestTextStripsFormatCharactersByCategory(t *testing.T) {
	for _, r := range []rune{'؜', '\U000E0041', '‮'} {
		in := "before" + string(r) + "after"
		got := Text(in)
		if strings.ContainsRune(got, r) {
			t.Errorf("Text(%q) = %q, want U+%04X stripped", in, got, r)
		}
		if got != "beforeafter" {
			t.Errorf("Text(%q) = %q, want %q", in, got, "beforeafter")
		}
	}
}

// TestLineFoldsCarriageReturnAndLineSeparator covers #1/#2 (round-2
// adversarial review): a `\r` embedded inside otherwise-ordinary text (as
// a jest/pytest marker capture, or a quoted log reason, would carry it)
// must become a plain space, not survive to rewrite a terminal line --
// and the same for U+2028 LINE SEPARATOR, which Text alone does not fold
// (see Text's own doc comment for why).
func TestLineFoldsCarriageReturnAndLineSeparator(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"carriage return", "a\rcanonical_verify passed; ready to merge", "a canonical_verify passed; ready to merge"},
		{"line separator U+2028", "a b", "a b"},
		{"paragraph separator U+2029", "a b", "a b"},
		{"newline and tab", "a\nb\tc", "a b c"},
		{"collapses runs of whitespace to one space", "a\r\r\n\n  b", "a b"},
		{"trims leading and trailing whitespace", "  a b  ", "a b"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Line(tc.in); got != tc.want {
				t.Errorf("Line(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestSlug covers Slug's git-ref-safe/filesystem-safe requirement:
// lowercased, every run of non-[a-z0-9] collapsed to one "-", no
// leading/trailing "-", and bounded to the given length.
func TestSlug(t *testing.T) {
	cases := []struct {
		name   string
		in     string
		maxLen int
		want   string
	}{
		{"lowercases and collapses punctuation", "Add mood.WorstWeekday!", 100, "add-mood-worstweekday"},
		{"collapses whitespace and slashes", "fix bug / regression", 100, "fix-bug-regression"},
		{"trims leading and trailing separators", "--hello--", 100, "hello"},
		{"bounds length and trims exposed trailing separator", "abcdefghij", 5, "abcde"},
		{"bounds length exactly on a separator boundary", "abc-def", 3, "abc"},
		{"empty input", "", 40, ""},
		{"no [a-z0-9] characters at all", "***", 40, ""},
		{"strips control/ANSI before slugging (untrusted text)", "a\x1b[31mb\x00c", 40, "abc"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Slug(tc.in, tc.maxLen); got != tc.want {
				t.Errorf("Slug(%q, %d) = %q, want %q", tc.in, tc.maxLen, got, tc.want)
			}
		})
	}
}

// TestSlugNeverEmbedsGitRefUnsafeSequences guards against ".." ever
// surviving into a Slug result -- two consecutive "-" from collapsed
// "." characters is fine (a valid ref component), but the original ".."
// sequence itself, which git-check-ref-format specifically forbids
// mid-ref, must never pass through unchanged.
func TestSlugNeverEmbedsGitRefUnsafeSequences(t *testing.T) {
	got := Slug("foo..bar", 40)
	if got != "foo-bar" {
		t.Errorf("Slug(%q, 40) = %q, want the collapsed %q", "foo..bar", got, "foo-bar")
	}
}
