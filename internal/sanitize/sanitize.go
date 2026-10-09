// Package sanitize makes untrusted, agent/model-written text safe to
// store in a durable record and show verbatim on an operator surface
// (factoryd status, halt/quarantine notifications, the console) --
// terminal escape sequences, control characters, and obvious secrets
// stripped or redacted. Extracted from cmd/factoryd/oracle_draft_job.go's
// own sanitizeLogText (unchanged logic, moved so internal/triage --
// which cannot import cmd/factoryd, a package main -- can reuse it
// rather than reimplementing the same stripping: a quarantine triage
// sentence started quoting lines straight out of a verify log, which
// agent-written code produced and so is exactly as untrusted as the
// oracle-draft script output this package already existed to sanitize).
package sanitize

import (
	"regexp"
	"strings"
	"unicode"
)

var (
	ansiSequence = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)`)
	// Ordered: whole-line/whole-token credentials first so a later, more
	// general pattern cannot leave part of the value behind.
	secretPatterns = []struct {
		re   *regexp.Regexp
		with string
	}{
		{regexp.MustCompile(`(?i)(authorization\s*[:=])[^\n]*`), "$1 [redacted]"},
		{regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9._~+/=-]+`), "Bearer [redacted]"},
		// JSON-quoted credential fields (`"api_key": "..."` etc): the
		// generic key=value pattern below requires the key itself to be
		// bare word characters immediately followed by `[:=]`, which a
		// JSON key's own closing quote defeats. This matters because pi's
		// own stderr (which can be JSON, e.g. a relay/API error body) can
		// end up in an operator-facing halt reason.
		{regexp.MustCompile(`(?i)"(api_key|access_token|refresh_token|authorization)"(\s*:\s*)"[^"]*"`), `"$1"$2"[redacted]"`},
		// OpenAI-style secret keys (sk-...) and GitHub tokens
		// (ghp_/gho_/ghu_/ghs_/ghr_...): whole-token patterns, checked
		// before the generic key=value pattern so a bare token pasted
		// into a log with no key= prefix at all is still redacted.
		{regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{16,}`), "sk-[redacted]"},
		{regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{20,}`), "gh_[redacted]"},
		// A JWT (three base64url segments joined by '.', always starting
		// "eyJ" -- the base64 of `{"`) can itself be a bearer credential
		// even where no "Bearer "/"api_key=" prefix precedes it.
		{regexp.MustCompile(`\beyJ[\w-]+\.[\w-]+\.[\w-]+`), "[redacted-jwt]"},
		{regexp.MustCompile(`(?i)\b([A-Za-z0-9_-]*(?:api[_-]?key|token|secret|password|passwd)[A-Za-z0-9_-]*)(\s*[:=]\s*)("[^"\n]*"|'[^'\n]*'|[^\s,;]+)`), "$1$2[redacted]"},
	}
)

// Text makes s safe to store in a request record and show in a console:
// ANSI sequences, control characters other than \n \t \r, Unicode format
// characters (category Cf -- bidi overrides, zero-width joiners/spaces,
// the BOM, tag characters, etc.), and obvious credentials are
// dropped/redacted.
//
// Cf, not a hand-picked list of ranges: the earlier version's explicit
// range list missed real Cf characters (e.g. U+061C ARABIC
// LETTER MARK, U+180E MONGOLIAN VOWEL SEPARATOR, U+FFF9-FFFB, and the
// U+E0000-E007F tag block used for invisible Unicode-tag smuggling),
// while unicode.Is(unicode.Cf, r) covers the whole category by
// definition and so can't miss a future one either.
//
// Text alone does NOT fold line-breaking whitespace (\r, \n, \t, U+2028/
// U+2029, ...) to a single space -- categories Zl/Zp aren't Cf or Cc, so
// they pass through unchanged, which is correct for text that is
// legitimately allowed to be multi-line (e.g. a retained oracle-draft
// failure summary). A caller building a SINGLE-line operator-facing
// string from untrusted text (a triage marker, a quoted log reason, a
// pi-stderr hint) must call Line, not Text, or a `\r`/U+2028 embedded in
// the source can still rewrite or split that one line on a terminal (#1,
// #2, round-2 review).
// StripANSI removes terminal escape sequences and nothing else. It is the
// cheap first step for a caller that scans a large log for a few lines and
// cleans only those with Text or Line: a colour code glued to a word hides
// the word from a pattern.
func StripANSI(s string) string {
	return ansiSequence.ReplaceAllString(s, "")
}

func Text(s string) string {
	s = strings.ToValidUTF8(ansiSequence.ReplaceAllString(s, ""), "")
	s = strings.Map(func(r rune) rune {
		switch {
		case r == '\n' || r == '\t' || r == '\r':
			return r
		case unicode.IsControl(r):
			return -1
		case unicode.Is(unicode.Cf, r):
			return -1
		}
		return r
	}, s)
	for _, sp := range secretPatterns {
		s = sp.re.ReplaceAllString(s, sp.with)
	}
	return s
}

// lineFoldRune reports whether r is a whitespace or line-breaking rune
// Line must fold to a single ordinary space -- see Line's own doc
// comment for why Text alone doesn't do this. The explicit cases are a
// subset of unicode.IsSpace's own White_Space property table (which
// already includes \v, \f, U+0085, U+2028, U+2029), listed anyway so the
// runes this function exists specifically to defeat are visible at the
// call site, not just implied by a property-table lookup.
func lineFoldRune(r rune) bool {
	switch r {
	case '\r', '\n', '\t', '\v', '\f', '\u0085', '\u2028', '\u2029':
		return true
	}
	return unicode.IsSpace(r)
}

// Line makes s safe to show as ONE line on an operator surface that a
// caller composes further text around (a triage sentence, a halt
// reason, a pi-stderr hint): Text's own stripping/redaction, then every
// whitespace or line-breaking rune folded to a single ordinary space
// (runs collapsed, leading/trailing space dropped). This guards against
// a marker string built with only strings.TrimSpace (trims the ends, not
// the middle) let an embedded `\r` or U+2028 inside untrusted matched
// text rewrite or split that line once printed to a real terminal or
// notification -- e.g. a jest `\u25cf` marker's captured name containing
// "a\rcanonical_verify passed; ready to merge" would overwrite the
// preceding text on any `\r`-honoring terminal (`factoryd status`).
// slugCollapse matches any run of characters Slug must collapse to a
// single "-" -- anything outside [a-z0-9], mirroring internal/request's
// own nonSlugChar (GenerateID), the established convention for a
// factoryd-minted id's own slug portion.
var slugCollapse = regexp.MustCompile(`[^a-z0-9]+`)

// Slug renders s as a strict, git-ref-safe, filesystem-safe slug: Text's
// own control/ANSI/secret stripping first (s is agent-drafted, and this
// package's own untrusted-text status therefore applies -- see Text's own
// doc comment), then lowercased and every run of characters outside
// [a-z0-9] collapsed to a single "-", leading/trailing "-" trimmed, and
// the result bounded to maxLen bytes (trimming any "-" that truncation
// exposes at the new end). "" in, or a result with no [a-z0-9] characters
// at all, returns "".
//
// Used for factoryd's own generated git branch names and worktree
// directory basenames (a bare, unreadable identifier like a raw sha256
// digest told an operator nothing about which PR a branch belonged to)
// -- a plain [a-z0-9-] slug is conservatively valid both as a git ref
// component (no "..", "~", "^", ":", "?", "*", "[", "\", or a leading/
// trailing "."/"/" that `git check-ref-format` would otherwise have to be
// consulted for) and as a filesystem path component on every OS this
// project targets, without needing either check at every call site.
func Slug(s string, maxLen int) string {
	slug := slugCollapse.ReplaceAllString(strings.ToLower(Text(s)), "-")
	slug = strings.Trim(slug, "-")
	if maxLen > 0 && len(slug) > maxLen {
		slug = strings.Trim(slug[:maxLen], "-")
	}
	return slug
}

func Line(s string) string {
	s = Text(s)
	var b strings.Builder
	pendingSpace := false
	for _, r := range s {
		if lineFoldRune(r) {
			pendingSpace = true
			continue
		}
		if pendingSpace {
			if b.Len() > 0 {
				b.WriteByte(' ')
			}
			pendingSpace = false
		}
		b.WriteRune(r)
	}
	return b.String()
}
