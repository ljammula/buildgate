package main

import (
	"flag"
	"strings"
)

// plainFlagUsage rewrites every backtick in fs's own flags' Usage strings to
// a plain single quote. Called on every FlagSet this package builds, right
// after its flags are defined.
//
// flag.UnquoteUsage (used by the default Usage/PrintDefaults -- see its own
// doc comment) treats the FIRST backquoted span in a flag's Usage as that
// flag's argument placeholder name, stripping the backquotes but leaving the
// enclosed text in the printed description. Most flags here quote a shell
// command in their Usage text (e.g. "`gh issue view <url>` must succeed",
// "`factoryd doctor`") with no intent to declare a placeholder at all, so
// `-help` misrenders them: a string flag's placeholder becomes "gh issue
// view <url>" instead of "string", and a bool flag -- which UnquoteUsage
// never gives a placeholder at all -- prints one anyway, wrongly implying it
// takes an argument. Removing the backticks removes the false placeholder
// and lets flag.UnquoteUsage fall back to each flag's real type name (or
// none, for a bool), while the quoted text still reads fine in the
// description with single quotes instead of backticks.
func plainFlagUsage(fs *flag.FlagSet) {
	fs.VisitAll(func(f *flag.Flag) {
		if strings.Contains(f.Usage, "`") {
			f.Usage = strings.ReplaceAll(f.Usage, "`", "'")
		}
	})
}
