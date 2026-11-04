package reproduction

import (
	"bytes"
	"encoding/xml"
	"errors"
	"io"
)

// junitCase is one <testcase> of a JUnit XML report, as pytest's
// --junitxml and Maven Surefire write it.
type junitCase struct {
	ClassName string        `xml:"classname,attr"`
	Name      string        `xml:"name,attr"`
	Failure   *junitProblem `xml:"failure"`
	Error     *junitProblem `xml:"error"`
	Skipped   *junitProblem `xml:"skipped"`
}

// junitProblem is a <failure>, <error> or <skipped> element.
type junitProblem struct {
	Message string `xml:"message,attr"`
	Type    string `xml:"type,attr"`
	Body    string `xml:",chardata"`
}

// readJUnit returns every <testcase> of the report at any depth (a
// <testsuites> root or a bare <testsuite>). ok is false when the report is
// empty or not well-formed XML.
func readJUnit(report []byte) (cases []junitCase, ok bool) {
	if len(bytes.TrimSpace(report)) == 0 {
		return nil, false
	}
	dec := xml.NewDecoder(bytes.NewReader(report))
	sawRoot := false
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			return cases, sawRoot
		}
		if err != nil {
			return nil, false
		}
		start, isStart := tok.(xml.StartElement)
		if !isStart {
			continue
		}
		sawRoot = true
		if start.Name.Local != "testcase" {
			continue
		}
		var c junitCase
		if dec.DecodeElement(&c, &start) != nil {
			return nil, false
		}
		cases = append(cases, c)
	}
}

// junitCaseKind maps one JUnit test case onto a Kind: JUnit's four outcomes
// (a failure, an error, skipped, passed), a failure or an error read by the
// runner's own problemKind, given the Kind the outcome means when the
// problem says nothing more (KindFail, KindError). A runner whose tool writes
// JUnit needs only problemKind (its timeout and assertion signals) and its
// own test-ID convention.
func junitCaseKind(c junitCase, problemKind func(p *junitProblem, outcome Kind) Kind) Kind {
	switch {
	case c.Failure != nil:
		return problemKind(c.Failure, KindFail)
	case c.Error != nil:
		return problemKind(c.Error, KindError)
	case c.Skipped != nil:
		return KindSkipped
	}
	return KindPass
}
