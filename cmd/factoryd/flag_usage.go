package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
)

// internalFlagPrefix opens the usage string of a flag no operator passes:
// one the worker or serve sets on the runs it starts, or one a test points
// at a stand-in. Such a flag is real and parsed as any other; -h, `factoryd
// help -json` and the usage docs leave it out.
const internalFlagPrefix = "internal: "

func isInternalFlag(f *flag.Flag) bool {
	return strings.HasPrefix(f.Usage, internalFlagPrefix)
}

// finishFlags is called on every FlagSet this package builds, right after
// its flags are defined. It does two things.
//
// It rewrites every backtick in the flags' usage strings to a single quote.
// flag.UnquoteUsage, which the flag package's own help uses, treats the
// first backquoted span of a usage string as the name of the flag's value.
// The usage strings here quote commands, not value names, so with the
// backticks left in a string flag's value would print as "gh issue view
// <url>" and a bool flag would print one at all.
//
// It gives the set the shared -h (printCommandUsage). A command with a -h
// of its own sets it after this call.
func finishFlags(fs *flag.FlagSet) {
	fs.VisitAll(func(f *flag.Flag) {
		if strings.Contains(f.Usage, "`") {
			f.Usage = strings.ReplaceAll(f.Usage, "`", "'")
		}
	})
	fs.Usage = func() { printCommandUsage(fs.Output(), fs) }
}

// printCommandUsage is a command's -h: how it is typed, what it does, then
// its flags without the internal ones.
func printCommandUsage(out io.Writer, fs *flag.FlagSet) {
	if c, ok := lookupCommand(fs.Name()); ok {
		fmt.Fprintf(out, "Usage: %s\n  %s\n", c.commandLine(), c.summary)
	} else {
		fmt.Fprintf(out, "Usage: factoryd %s [flags]\n", fs.Name())
	}
	printOperatorFlags(out, fs)
}

// printOperatorFlags prints fs's flags, without the internal ones, in the
// flag package's own layout. It prints nothing for a set that has none.
func printOperatorFlags(out io.Writer, fs *flag.FlagSet) {
	shown := operatorFlags(fs)
	any := false
	shown.VisitAll(func(*flag.Flag) { any = true })
	if !any {
		return
	}
	fmt.Fprintln(out, "\nFlags:")
	shown.SetOutput(out)
	shown.PrintDefaults()
}

// operatorFlags is a copy of fs without its internal flags. Each flag keeps
// the default it was defined with, not the value a parse has since set, and
// a default under the home directory is shown as ~/.
func operatorFlags(fs *flag.FlagSet) *flag.FlagSet {
	shown := flag.NewFlagSet(fs.Name(), flag.ContinueOnError)
	home, _ := os.UserHomeDir()
	fs.VisitAll(func(f *flag.Flag) {
		if isInternalFlag(f) {
			return
		}
		shown.Var(f.Value, f.Name, f.Usage)
		def := f.DefValue
		if rest, under := strings.CutPrefix(def, home+string(os.PathSeparator)); under && home != "" {
			def = "~/" + rest
		}
		shown.Lookup(f.Name).DefValue = def
	})
	return shown
}
