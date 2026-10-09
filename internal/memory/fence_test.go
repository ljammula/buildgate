package memory

import (
	"bytes"
	"errors"
	"reflect"
	"strings"
	"testing"
)

const canonical = BeginMarker + "\n" + SectionHeading + "\n\n- one.\n- two.\n" + EndMarker + "\n"

func TestParseSectionNoSection(t *testing.T) {
	for _, f := range []string{"", "plain\n", "no newline", "# T\n\ntext\n"} {
		s, err := ParseSection([]byte(f))
		if err != nil || s.Present || s.Before != f || s.After != "" || len(s.Lines) != 0 {
			t.Errorf("%q: %+v %v", f, s, err)
		}
	}
}

func TestParseSectionCanonical(t *testing.T) {
	file := "# Title\n\nintro\n\n" + canonical + "\n## Other\n"
	s, err := ParseSection([]byte(file))
	if err != nil {
		t.Fatal(err)
	}
	want := Section{Present: true, Before: "# Title\n\nintro\n\n", After: "\n## Other\n", Lines: []string{"- one.", "- two."}}
	if !reflect.DeepEqual(s, want) {
		t.Fatalf("got %+v", s)
	}
	if got := s.Render(s.Lines); !bytes.Equal(got, []byte(file)) {
		t.Fatalf("render changed a canonical file:\n%q\n%q", got, file)
	}
}

func TestParseSectionKeepsHumanLinesVerbatim(t *testing.T) {
	file := BeginMarker + "\n\n" + SectionHeading + "\n- human line with  odd   spacing and `code`\n\n- Run `a` before `b`: c.\n" + EndMarker
	s, err := ParseSection([]byte(file))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"- human line with  odd   spacing and `code`", "- Run `a` before `b`: c."}
	if !reflect.DeepEqual(s.Lines, want) || s.After != "" || s.Before != "" {
		t.Fatalf("got %+v", s)
	}
}

func TestParseSectionFenceErrors(t *testing.T) {
	good := SectionHeading + "\n\n- a.\n"
	cases := map[string]string{
		"invalid utf8":                         "x\xff\n",
		"nul":                                  "x\x00\n",
		"begin only":                           BeginMarker + "\n" + good,
		"end only":                             good + EndMarker + "\n",
		"two begins":                           BeginMarker + "\n" + BeginMarker + "\n" + good + EndMarker + "\n",
		"two ends":                             BeginMarker + "\n" + good + EndMarker + "\n" + EndMarker + "\n",
		"two sections":                         canonical + "\n" + canonical,
		"end before begin":                     EndMarker + "\n" + BeginMarker + "\n" + good,
		"trailing space":                       BeginMarker + " \n" + good + EndMarker + "\n",
		"trailing tab":                         BeginMarker + "\n" + good + EndMarker + "\t\n",
		"crlf marker":                          BeginMarker + "\r\n" + good + EndMarker + "\n",
		"indented marker":                      " " + BeginMarker + "\n" + good + EndMarker + "\n",
		"mid line marker":                      "text " + BeginMarker + "\n" + good + EndMarker + "\n",
		"text after marker":                    BeginMarker + " x\n" + good + EndMarker + "\n",
		"marker in code fence":                 "```\n" + BeginMarker + "\n" + good + EndMarker + "\n```\n",
		"marker in tilde fence":                "~~~\n" + BeginMarker + "\n" + good + EndMarker + "\n~~~\n",
		"marker in line":                       BeginMarker + "\n" + SectionHeading + "\n\n- a " + EndMarker + "\n" + EndMarker + "\n",
		"marker elsewhere":                     "see " + EndMarker + " here\n" + canonical,
		"no heading":                           BeginMarker + "\n- a.\n" + EndMarker + "\n",
		"empty block":                          BeginMarker + "\n" + EndMarker + "\n",
		"wrong heading":                        BeginMarker + "\n## Other\n\n- a.\n" + EndMarker + "\n",
		"prose in block":                       BeginMarker + "\n" + SectionHeading + "\n\nsome prose\n" + EndMarker + "\n",
		"star bullet":                          BeginMarker + "\n" + SectionHeading + "\n\n* a\n" + EndMarker + "\n",
		"indented bullet":                      BeginMarker + "\n" + SectionHeading + "\n\n  - a\n" + EndMarker + "\n",
		"crlf in block":                        BeginMarker + "\n" + SectionHeading + "\r\n\n- a.\n" + EndMarker + "\n",
		"crlf line in block":                   BeginMarker + "\n" + SectionHeading + "\n\n- a.\r\n" + EndMarker + "\n",
		"whitespace only line":                 BeginMarker + "\n" + SectionHeading + "\n \n- a.\n" + EndMarker + "\n",
		"empty bullet":                         BeginMarker + "\n" + SectionHeading + "\n\n- \n" + EndMarker + "\n",
		"open fence, no markers":               "# T\n\n```sh\nmake test\n",
		"open tilde fence":                     "# T\n\n~~~\nmake test\n",
		"shorter close":                        "````\ncode\n```\n",
		"other fence character":                "```\ncode\n~~~\n",
		"close with text after":                "```\ncode\n``` not a close\n",
		"marker in long fence":                 "````\n```\n" + BeginMarker + "\n" + good + EndMarker + "\n````\n",
		"marker after tilde in backtick fence": "```\n~~~\n```\n~~~\n" + BeginMarker + "\n" + good + EndMarker + "\n",
	}
	for name, file := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseSection([]byte(file)); !errors.Is(err, ErrFence) {
				t.Fatalf("err = %v, want ErrFence", err)
			}
		})
	}
}

// A section line that holds a control or format character is not read: it
// would reach the operator's terminal and a spec.
func TestParseSectionRefusesControlCharacters(t *testing.T) {
	for name, ch := range map[string]string{
		"escape": "\x1b[31m", "bell": "\x07", "tab": "\t", "delete": "\x7f", "c1 csi": "\u009b", "c1 nel": "\u0085",
		"rtl override": "\u202e", "zero width": "\u200b", "bom": "\ufeff", "line separator": "\u2028",
	} {
		t.Run(name, func(t *testing.T) {
			file := BeginMarker + "\n" + SectionHeading + "\n\n- one " + ch + "two.\n" + EndMarker + "\n"
			if _, err := ParseSection([]byte(file)); !errors.Is(err, ErrFence) {
				t.Fatalf("err = %v, want ErrFence", err)
			}
			out := Section{}.Render([]string{"- one " + ch + "two.", "- good."})
			if strings.Contains(string(out), ch) {
				t.Fatalf("render kept the character: %q", out)
			}
		})
	}
}

// A file with no section that leaves a code fence open is refused: a section
// appended to it would sit inside the fence and never be read back. Closed
// fences, of either character and any length, are not in the way.
func TestParseSectionTracksCodeFences(t *testing.T) {
	for _, file := range []string{
		"# T\n\n```sh\nmake test\n```\n",
		"````\n```\ninner\n```\n````\n",
		"~~~\n```\n~~~\n",
		"```\n~~~\n```\n",
		"use `` ``` `` in prose\n",
		"```\ncode\n`````\n",
		"```\ncode\n```   \n",
	} {
		s, err := ParseSection([]byte(file))
		if err != nil {
			t.Errorf("%q: %v", file, err)
			continue
		}
		if _, err := ParseSection(s.Render([]string{"- a."})); err != nil {
			t.Errorf("%q: rendered section does not parse: %v", file, err)
		}
		if _, err := ParseSection([]byte(file + canonical)); err != nil {
			t.Errorf("%q: section after closed fences refused: %v", file, err)
		}
	}
}

func TestRenderChangesOnlyTheFencedSection(t *testing.T) {
	files := map[string]string{
		"empty":            "",
		"no newline":       "# Title\nbody",
		"before and after": "# T\n\nintro\n\n" + canonical + "\n## After\ntext\n",
		"crlf outside":     "# T\r\n\r\nline\r\n\r\n" + canonical + "tail\r\n",
		"non ascii":        "# Titre éàü 日本語\n\n" + canonical + "fin ✓\n",
		"section only":     canonical,
		"no trailing nl":   "x\n" + BeginMarker + "\n" + SectionHeading + "\n- a.\n" + EndMarker,
	}
	for name, f := range files {
		t.Run(name, func(t *testing.T) {
			s, err := ParseSection([]byte(f))
			if err != nil {
				t.Fatal(err)
			}
			for _, lines := range [][]string{nil, {"- x."}, {"- y.", "- z."}} {
				out := s.Render(lines)
				if !bytes.HasPrefix(out, []byte(s.Before)) && s.Present {
					t.Fatalf("prefix changed: %q", out)
				}
				if s.Present && !bytes.HasSuffix(out, []byte(s.After)) {
					t.Fatalf("suffix changed: %q", out)
				}
				back, err := ParseSection(out)
				if err != nil {
					t.Fatalf("render output does not parse: %v\n%q", err, out)
				}
				if !back.Present || len(back.Lines) != len(lines) {
					t.Fatalf("lines lost: %+v", back)
				}
				if s.Present && (back.Before != s.Before || back.After != s.After) {
					t.Fatalf("outside bytes changed: %+v vs %+v", back, s)
				}
				if !s.Present && (!strings.HasPrefix(back.Before, s.Before) || strings.TrimLeft(back.Before[len(s.Before):], "\n") != "") {
					t.Fatalf("appended section altered the text: %q", back.Before)
				}
			}
		})
	}
}

func TestRenderAppendsToAFileWithoutASection(t *testing.T) {
	cases := map[string]string{
		"":      BeginMarker + "\n" + SectionHeading + "\n\n- a.\n" + EndMarker + "\n",
		"x":     "x\n\n" + BeginMarker + "\n" + SectionHeading + "\n\n- a.\n" + EndMarker + "\n",
		"x\n":   "x\n\n" + BeginMarker + "\n" + SectionHeading + "\n\n- a.\n" + EndMarker + "\n",
		"x\n\n": "x\n\n" + BeginMarker + "\n" + SectionHeading + "\n\n- a.\n" + EndMarker + "\n",
		"\n":    "\n" + BeginMarker + "\n" + SectionHeading + "\n\n- a.\n" + EndMarker + "\n",
	}
	for before, want := range cases {
		got := Section{Before: before}.Render([]string{"- a."})
		if string(got) != want {
			t.Errorf("before %q: got %q want %q", before, got, want)
		}
	}
}

func TestRenderRoundTrips(t *testing.T) {
	s := Section{Present: true, Before: "a\n\n", After: "\nz\n"}
	lines := []string{"- one.", "- Run `a` before `b`: c."}
	back, err := ParseSection(s.Render(lines))
	if err != nil {
		t.Fatal(err)
	}
	want := Section{Present: true, Before: s.Before, After: s.After, Lines: lines}
	if !reflect.DeepEqual(back, want) {
		t.Fatalf("got %+v", back)
	}
	// Empty lines still render the markers and heading.
	empty := Section{}.Render(nil)
	if string(empty) != BeginMarker+"\n"+SectionHeading+"\n\n"+EndMarker+"\n" {
		t.Fatalf("empty = %q", empty)
	}
	back, err = ParseSection(empty)
	if err != nil || !back.Present || len(back.Lines) != 0 {
		t.Fatalf("empty reparse: %+v %v", back, err)
	}
}

func TestRenderDropsLinesItCouldNotReadBack(t *testing.T) {
	bad := []string{
		"- has " + BeginMarker, "- has " + EndMarker, "- two\nlines", "- cr\r", "no bullet",
		"", "-", "- ", "- \t", "- nul\x00", "- bad\xffutf8",
	}
	lines := append(append([]string{"- good."}, bad...), "- also good.")
	out := Section{}.Render(lines)
	back, err := ParseSection(out)
	if err != nil {
		t.Fatalf("output does not parse: %v\n%q", err, out)
	}
	if !reflect.DeepEqual(back.Lines, []string{"- good.", "- also good."}) {
		t.Fatalf("lines = %q", back.Lines)
	}
	if strings.Count(string(out), BeginMarker) != 1 || strings.Count(string(out), EndMarker) != 1 {
		t.Fatalf("marker count wrong: %q", out)
	}
}

func FuzzParseSection(f *testing.F) {
	for _, s := range []string{
		"", "plain\n", canonical, "x\n\n" + canonical + "y\n", BeginMarker, EndMarker + "\n" + BeginMarker + "\n",
		BeginMarker + "\n" + SectionHeading + "\n\n- a\n" + EndMarker,
		"```\n" + canonical + "```\n", BeginMarker + " \n" + SectionHeading + "\n" + EndMarker + "\n",
		BeginMarker + "\r\n" + SectionHeading + "\r\n" + EndMarker + "\r\n", "\xff", "a\x00b",
		BeginMarker + "\n" + SectionHeading + "\n\n- a " + BeginMarker + "\n" + EndMarker + "\n",
		"# T\n\n```sh\nmake test\n", "````\n```\n````\n", "~~~\n```\n", "```\n~~~\n```\n", "   ```\nx\n",
		BeginMarker + "\n" + SectionHeading + "\n\n- a \x1b[0m\n" + EndMarker + "\n",
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, file []byte) {
		s, err := ParseSection(file)
		if err != nil {
			if !errors.Is(err, ErrFence) {
				t.Fatalf("untyped error %v", err)
			}
			return
		}
		if !s.Present {
			if s.Before != string(file) {
				t.Fatal("absent section lost bytes")
			}
			// Whatever is accepted can be given a section and read back.
			lines := []string{"- a.", "- Run `x` before `y`."}
			back, err := ParseSection(s.Render(lines))
			if err != nil || !back.Present || !reflect.DeepEqual(back.Lines, lines) || !strings.HasPrefix(back.Before, s.Before) {
				t.Fatalf("a section appended to an accepted file does not parse: %v\n%q", err, file)
			}
			return
		}
		out := s.Render(s.Lines)
		back, err := ParseSection(out)
		if err != nil || !reflect.DeepEqual(back, s) {
			t.Fatalf("render does not re-parse to the same value: %v\n%+v\n%+v", err, s, back)
		}
	})
}
