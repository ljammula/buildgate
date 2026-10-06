package oraclecanary

import (
	"fmt"
	"regexp"
	"strings"
)

// The non-Go canaries keep the test NAMES the original declares, for the same
// reason as the Go canary: a RUN_COMMAND that selects tests by name (pytest -k,
// vitest/jest -t, flutter test --name) must select the canary's tests exactly
// as it selects the real ones, or it would run nothing and exit 0 (or fail for
// "no tests found") and the canary would prove nothing. Extraction is a
// line/regex scan, not a parse: a name it misses only weakens the canary
// toward FALSE_PASS-blindness for that selector, never toward a false pass.

// msgExpr renders Marker as a runtime concatenation of two literals in quote q,
// so the canary's SOURCE never contains the marker contiguously: only the test
// executing prints it.
func msgExpr(q, nonce string) string {
	// Split inside the nonce: neither literal alone, nor any contiguous run of
	// the source, contains the whole marker.
	half := len(nonce) / 2
	return q + "oracle canary " + nonce[:half] + q + " + " + q + nonce[half:] + ": this test must fail" + q
}

// quotedLiteral matches a single-, double- or backtick-quoted string literal
// (RE2 has no backreferences, hence one alternative per quote).
const quotedLiteral = "(?:'((?:\\\\.|[^'\\\\\\n])*)'|\"((?:\\\\.|[^\"\\\\\\n])*)\"|`((?:\\\\.|[^`\\\\\\n])*)`)"

var (
	pyTopDef   = regexp.MustCompile(`^(?:async[ \t]+)?def[ \t]+(test\w*)[ \t]*\(`)
	pyClass    = regexp.MustCompile(`^class[ \t]+(\w+)`)
	pyMethod   = regexp.MustCompile(`^[ \t]+(?:async[ \t]+)?def[ \t]+(test\w*)[ \t]*\(`)
	jsTestCall = regexp.MustCompile(`\b(?:test|it)(?:\.(?:only|skip|concurrent))?\(\s*` + quotedLiteral)
	jsDescribe = regexp.MustCompile(`\bdescribe(?:\.(?:only|skip|concurrent))?\(\s*` + quotedLiteral)
	jsImport   = regexp.MustCompile(`from\s+['"](vitest|@jest/globals)['"]`)
	dartTest   = regexp.MustCompile(`\b(?:test|testWidgets)\(\s*` + quotedLiteral)
	dartGroup  = regexp.MustCompile(`\bgroup\(\s*` + quotedLiteral)
	dartImport = regexp.MustCompile(`import\s+'package:flutter_test/flutter_test\.dart'`)
)

// pythonCanary emits a failing function per top-level test function and a
// failing method per test method of each Test* class, under the same names.
func pythonCanary(original []byte, nonce string) []byte {
	var funcs []string
	classes := map[string][]string{}
	var classOrder []string
	current := ""
	seen := map[string]bool{}
	for _, line := range strings.Split(string(original), "\n") {
		if m := pyTopDef.FindStringSubmatch(line); m != nil {
			current = ""
			if !seen["f:"+m[1]] {
				seen["f:"+m[1]] = true
				funcs = append(funcs, m[1])
			}
			continue
		}
		if m := pyClass.FindStringSubmatch(line); m != nil {
			current = ""
			if strings.HasPrefix(m[1], "Test") {
				current = m[1]
				if _, ok := classes[current]; !ok {
					classOrder = append(classOrder, current)
					classes[current] = nil
				}
			}
			continue
		}
		if m := pyMethod.FindStringSubmatch(line); m != nil && current != "" && !seen[current+"."+m[1]] {
			seen[current+"."+m[1]] = true
			classes[current] = append(classes[current], m[1])
		}
	}
	if len(funcs) == 0 && len(classOrder) == 0 {
		funcs = []string{"test_oracle_canary"}
	}
	var b strings.Builder
	for _, f := range funcs {
		fmt.Fprintf(&b, "def %s():\n    assert False, %s\n\n\n", f, msgExpr(`"`, nonce))
	}
	for _, c := range classOrder {
		fmt.Fprintf(&b, "class %s:\n", c)
		methods := classes[c]
		if len(methods) == 0 {
			methods = []string{"test_oracle_canary"}
		}
		for _, m := range methods {
			fmt.Fprintf(&b, "    def %s(self):\n        assert False, %s\n\n", m, msgExpr(`"`, nonce))
		}
		b.WriteString("\n")
	}
	return []byte(strings.TrimRight(b.String(), "\n") + "\n")
}

// quotedNames returns the raw (still-escaped) string literals matched by re as
// {quote, contents} pairs, skipping template literals with interpolation.
func quotedNames(re *regexp.Regexp, src string) [][2]string {
	var out [][2]string
	seen := map[string]bool{}
	for _, m := range re.FindAllStringSubmatch(src, -1) {
		q, body := "'", m[1]
		switch {
		case m[2] != "":
			q, body = "\"", m[2]
		case m[3] != "":
			q, body = "`", m[3]
		}
		if q == "`" && strings.Contains(body, "${") {
			continue
		}
		if seen[q+body] {
			continue
		}
		seen[q+body] = true
		out = append(out, [2]string{q, body})
	}
	return out
}

// jsCanary emits a failing test per test/it name and a describe wrapper per
// describe name. The test-framework import is reproduced only when the original
// has one; otherwise the framework globals the original relied on are used.
func jsCanary(original []byte, nonce string) []byte {
	src := string(original)
	var b strings.Builder
	if m := jsImport.FindStringSubmatch(src); m != nil {
		fmt.Fprintf(&b, "import { test, describe, expect } from '%s';\n\n", m[1])
	}
	tests := quotedNames(jsTestCall, src)
	describes := quotedNames(jsDescribe, src)
	if len(tests) == 0 && len(describes) == 0 {
		tests = [][2]string{{"'", "oracle canary"}}
	}
	var body = "() => {\n  throw new Error(" + msgExpr("'", nonce) + ");\n}"
	for _, t := range tests {
		fmt.Fprintf(&b, "test(%s%s%s, %s);\n", t[0], t[1], t[0], body)
	}
	for _, d := range describes {
		fmt.Fprintf(&b, "describe(%s%s%s, () => {\n  test('oracle canary', %s);\n});\n", d[0], d[1], d[0], body)
	}
	return []byte(b.String())
}

// dartCanary emits a failing test per test/testWidgets name and a group wrapper
// per group name.
func dartCanary(original []byte, nonce string) []byte {
	src := string(original)
	pkg := "package:test/test.dart"
	if dartImport.MatchString(src) {
		pkg = "package:flutter_test/flutter_test.dart"
	}
	tests := quotedNames(dartTest, src)
	groups := quotedNames(dartGroup, src)
	if len(tests) == 0 && len(groups) == 0 {
		tests = [][2]string{{"'", "oracle canary"}}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "import '%s';\n\nvoid main() {\n", pkg)
	for _, t := range tests {
		fmt.Fprintf(&b, "  test(%s%s%s, () {\n    fail(%s);\n  });\n", t[0], t[1], t[0], msgExpr("'", nonce))
	}
	for _, g := range groups {
		fmt.Fprintf(&b, "  group(%s%s%s, () {\n    test('oracle canary', () {\n      fail(%s);\n    });\n  });\n", g[0], g[1], g[0], msgExpr("'", nonce))
	}
	b.WriteString("}\n")
	return []byte(b.String())
}
