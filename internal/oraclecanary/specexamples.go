package oraclecanary

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// maxSpecExampleWarnings bounds how many warnings SpecExampleWarnings returns.
const maxSpecExampleWarnings = 6

type polarity int

const (
	unknownPolarity polarity = iota
	acceptedPolarity
	rejectedPolarity
)

func (p polarity) String() string {
	if p == acceptedPolarity {
		return "accepted"
	}
	return "rejected"
}

var (
	// criterionLiteral finds the literal examples a criterion quotes: backtick
	// or double-quoted spans, kept byte for byte (a trailing space matters).
	criterionLiteral = regexp.MustCompile("`([^`\n]+)`|\"([^\"\n]+)\"")
	// criterionClauseBreak splits a criterion into clauses that may state
	// opposite outcomes ("`a` is accepted; `b` is rejected").
	criterionClauseBreak = regexp.MustCompile(`(?i);|\n|\.\s|\bbut\b|\bwhile\b|\bwhereas\b|\bhowever\b|\bunlike\b`)
	rejectWords          = regexp.MustCompile(`(?i)\b(invalid|rejects?|rejected|rejecting|refus\w*|errors?|fails?|failing|failure|illegal|unsupported|unrecogni[sz]ed|unknown|not\s+(?:accepted|valid|allowed|permitted|supported|recogni[sz]ed))\b`)
	acceptWords          = regexp.MustCompile(`(?i)\b(accepts?|accepted|accepting|valid|allowed|permitted|supported|treated\s+as|recogni[sz]ed|succe\w+|equivalent|case-insensitive)\b`)

	// The oracle-side vocabulary. Deliberately narrow: a context that trips
	// both lists, or neither, is undecided and never produces a warning.
	oracleRejectWords = regexp.MustCompile(`(?i)\b(invalid|rejects?|rejected|refus\w*|illegal|unsupported|unrecogni[sz]ed|(?:should|must)\s?fail|valid\s*:\s*false|(?:want|expect)\w*\s+err\w*\s*:\s*true|expect(?:ed|s)?\s+(?:an?\s+)?(?:error|failure|invalid)|err\w*\s*:\s*true)\b`)
	oracleAcceptWords = regexp.MustCompile(`(?i)\b(valid|accepts?|accepted|success\w*|(?:should|must)\s?pass|(?:want|expect)\w*\s+err\w*\s*:\s*false|err\w*\s*:\s*false)\b`)
	camelBoundary     = regexp.MustCompile(`([a-z0-9])([A-Z])`)
)

type specLiteral struct {
	polarity  polarity
	criterion int    // 1-based
	clause    string // the clause it came from, literals kept, for the message
}

// SpecExampleWarnings is the heuristic check for an oracle that contradicts
// the spec's own worked examples. A literal quoted (backticks or double quotes)
// in a clause of a criterion that clearly says it is accepted, valid or
// supported (or clearly rejected) is looked for, byte for byte, among the
// string literals of the Go oracle files that cover that criterion; where the
// nearest enclosing context of the literal in the test clearly asserts the
// opposite outcome (an "invalid" table or variable, a wantErr: true element,
// a subtest or message that says invalid/rejected), a warning is produced.
//
// It only warns and is only a heuristic: it never blocks a draft, it can be
// wrong in both directions, it cannot see an example the criterion states in
// prose without quoting it, an outcome an oracle asserts through a helper it
// does not name, or a normalisation rule applied to a spelling the criterion
// never quotes. A literal that some clause accepts and another rejects, or a
// clause that mixes both kinds of words, is skipped. fileCriteria maps an
// oracle file to the 1-based numbers of the criteria it covers (a missing entry
// means every criterion).
func SpecExampleWarnings(criteria []string, files map[string][]byte, fileCriteria map[string][]int) []string {
	names := make([]string, 0, len(files))
	for n := range files {
		if strings.HasSuffix(n, ".go") {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	var warnings []string
	for _, name := range names {
		lits := specLiterals(criteria, fileCriteria[name])
		if len(lits) == 0 {
			continue
		}
		warnings = append(warnings, oracleContradictions(name, files[name], lits)...)
		if len(warnings) >= maxSpecExampleWarnings {
			return warnings[:maxSpecExampleWarnings]
		}
	}
	return warnings
}

// specLiterals extracts the unambiguous quoted examples of the given criteria
// (all of them when which is empty).
func specLiterals(criteria []string, which []int) map[string]specLiteral {
	use := map[int]bool{}
	for _, n := range which {
		use[n] = true
	}
	out := map[string]specLiteral{}
	conflicted := map[string]bool{}
	for i, c := range criteria {
		if len(which) > 0 && !use[i+1] {
			continue
		}
		for _, clause := range criterionClauseBreak.Split(c, -1) {
			matches := criterionLiteral.FindAllStringSubmatch(clause, -1)
			if len(matches) == 0 {
				continue
			}
			words := criterionLiteral.ReplaceAllString(clause, " ")
			rej := rejectWords.MatchString(words)
			acc := acceptWords.MatchString(rejectWords.ReplaceAllString(words, " "))
			if rej == acc {
				continue
			}
			pol := acceptedPolarity
			if rej {
				pol = rejectedPolarity
			}
			for _, m := range matches {
				lit := m[1]
				if lit == "" {
					lit = m[2]
				}
				if len(lit) < 2 {
					continue
				}
				if prev, ok := out[lit]; ok && prev.polarity != pol {
					conflicted[lit] = true
				}
				out[lit] = specLiteral{polarity: pol, criterion: i + 1, clause: strings.TrimSpace(clause)}
			}
		}
	}
	for lit := range conflicted {
		delete(out, lit)
	}
	return out
}

func oracleContradictions(name string, src []byte, lits map[string]specLiteral) []string {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, name, src, parser.SkipObjectResolution)
	if err != nil {
		return nil
	}
	var warnings []string
	seen := map[string]bool{}
	var stack []ast.Node
	ast.Inspect(file, func(n ast.Node) bool {
		if n == nil {
			stack = stack[:len(stack)-1]
			return true
		}
		stack = append(stack, n)
		bl, ok := n.(*ast.BasicLit)
		if !ok || bl.Kind != token.STRING {
			return true
		}
		value, err := strconv.Unquote(bl.Value)
		if err != nil {
			return true
		}
		want, ok := lits[value]
		if !ok {
			return true
		}
		got, excerpt := contextPolarity(fset, src, stack, bl)
		if got == unknownPolarity || got == want.polarity {
			return true
		}
		key := fmt.Sprintf("%q:%d", value, fset.Position(bl.Pos()).Line)
		if seen[key] {
			return true
		}
		seen[key] = true
		warnings = append(warnings, fmt.Sprintf("%s:%d: the literal %q looks asserted as %s (%s), but criterion %d quotes it as %s (%q): compare the oracle with the spec's own example",
			name, fset.Position(bl.Pos()).Line, value, got, excerpt, want.criterion, want.polarity, want.clause))
		return true
	})
	return warnings
}

// contextPolarity walks the literal's ancestors from the inside out and returns
// the polarity of the first one whose own text is decisive, with a short
// excerpt of that text for the message. The literal itself is blanked out of
// the text it is judged by.
func contextPolarity(fset *token.FileSet, src []byte, stack []ast.Node, lit *ast.BasicLit) (polarity, string) {
	for i := len(stack) - 2; i >= 0; i-- {
		var text string
		switch n := stack[i].(type) {
		case *ast.FuncDecl:
			text = n.Name.Name
			if p := decide(text); p != unknownPolarity {
				return p, "function " + n.Name.Name
			}
			return unknownPolarity, ""
		case *ast.CompositeLit:
			// Judge the element of this composite that holds the literal.
			for _, elt := range n.Elts {
				if elt.Pos() <= lit.Pos() && lit.End() <= elt.End() {
					text = nodeText(fset, src, elt, lit)
				}
			}
		case *ast.KeyValueExpr:
			text = nodeText(fset, src, n.Key, lit)
		case *ast.AssignStmt:
			for _, l := range n.Lhs {
				text += " " + nodeText(fset, src, l, lit)
			}
		case *ast.ValueSpec:
			for _, id := range n.Names {
				text += " " + id.Name
			}
		case *ast.RangeStmt, *ast.IfStmt, *ast.CallExpr:
			text = nodeText(fset, src, n, lit)
		default:
			continue
		}
		if p := decide(text); p != unknownPolarity {
			return p, excerpt(text)
		}
	}
	return unknownPolarity, ""
}

// decide is the polarity of text when exactly one vocabulary matches it.
func decide(text string) polarity {
	// Identifiers are read as words: invalidStates, wantErr, is_valid.
	text = strings.ReplaceAll(camelBoundary.ReplaceAllString(text, "$1 $2"), "_", " ")
	rej, acc := oracleRejectWords.MatchString(text), oracleAcceptWords.MatchString(oracleRejectWords.ReplaceAllString(text, " "))
	switch {
	case rej && !acc:
		return rejectedPolarity
	case acc && !rej:
		return acceptedPolarity
	}
	return unknownPolarity
}

func nodeText(fset *token.FileSet, src []byte, n ast.Node, blank *ast.BasicLit) string {
	start, end := fset.Position(n.Pos()).Offset, fset.Position(n.End()).Offset
	if start < 0 || end > len(src) || start >= end {
		return ""
	}
	text := string(src[start:end])
	if bs, be := fset.Position(blank.Pos()).Offset-start, fset.Position(blank.End()).Offset-start; bs >= 0 && be <= len(text) {
		text = text[:bs] + "_" + text[be:]
	}
	return text
}

func excerpt(text string) string {
	text = strings.Join(strings.Fields(text), " ")
	if len(text) > 80 {
		text = strings.ToValidUTF8(text[:80], "") + "..."
	}
	return text
}
