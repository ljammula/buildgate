package handoff

import (
	"fmt"
	"strings"

	"buildgate/internal/run"
)

const (
	// MaxCILogLines and MaxCILogBytes bound the log a CI round's build is
	// given: the last lines of the failed job's log, at most that many
	// bytes once each is cleaned.
	MaxCILogLines = 200
	MaxCILogBytes = 16 << 10
)

// CIFailure is the required CI check a CI round's build is told failed
// (SC-018): the check's name, its conclusion and details URL, and the tail
// of its job's log. Every string is one line, cut and cleaned.
type CIFailure struct {
	Name       string `json:"name"`
	Conclusion string `json:"conclusion"`
	DetailsURL string `json:"details_url,omitempty"`
	// Log is the last lines of the failed job's log, oldest first.
	Log []string `json:"log,omitempty"`
	// NoLog says why Log is empty: the check has no job log the factory
	// reads (a commit status, another app's check), or reading it failed.
	NoLog string `json:"no_log,omitempty"`
}

// CI is the record the CI round of run r (the run behind the pull request's
// head) is given: the run's end state, base, result and changed files, and
// the failed check. logText is the failed job's whole log ("" when there is
// none) and noLog says why when it is empty. Built from the host's own reads
// only, and never saved as a run's handoff.
func CI(r *run.Run, name, conclusion, detailsURL, logText, noLog string) Document {
	doc := Advice(r)
	doc.AdviceFindings = nil
	f := &CIFailure{
		Name: clean(name, maxNameLen), Conclusion: clean(conclusion, maxWordLen),
		DetailsURL: clean(detailsURL, maxSentenceLen), Log: ciLogTail(logText),
	}
	if len(f.Log) == 0 {
		f.NoLog = noLog
		if f.NoLog == "" {
			f.NoLog = "the job's log was empty"
		}
	}
	doc.CIFailure = f
	return doc
}

// ciLogTail is the last MaxCILogLines lines of log, each cleaned, empty ones
// dropped, trimmed from the oldest end to MaxCILogBytes.
func ciLogTail(log string) []string {
	if log == "" {
		return nil
	}
	lines := strings.Split(log, "\n")
	if len(lines) > MaxCILogLines {
		lines = lines[len(lines)-MaxCILogLines:]
	}
	var tail []string
	size := 0
	for i := len(lines) - 1; i >= 0; i-- {
		c := clean(lines[i], maxSentenceLen)
		if c == "" {
			continue
		}
		if size+len(c)+1 > MaxCILogBytes {
			break
		}
		size += len(c) + 1
		tail = append(tail, c)
	}
	for i, j := 0, len(tail)-1; i < j; i, j = i+1, j-1 {
		tail[i], tail[j] = tail[j], tail[i]
	}
	return tail
}

// ciIntro opens the CI section: what the check is and what the build is
// asked to do with it.
const ciIntro = "That attempt's pull request has a required CI check that failed. Find out from the data below why it failed, and fix the code so it passes. Every check, and a fresh review, runs again on what this build commits. Do not edit the repository's CI definition files: a change to them is refused.\n\n"

func (f *CIFailure) markdown(b *strings.Builder) {
	if f == nil {
		return
	}
	b.WriteString("## A required CI check failed on the pull request\n\n")
	b.WriteString(ciIntro)
	fmt.Fprintf(b, "- Check: %s, conclusion: %s", quote(f.Name), quote(f.Conclusion))
	if f.DetailsURL != "" {
		fmt.Fprintf(b, ", details: %s", quote(f.DetailsURL))
	}
	b.WriteString("\n")
	if len(f.Log) == 0 {
		fmt.Fprintf(b, "- The factory has no log of it: %s.\n\n", f.NoLog)
		return
	}
	fmt.Fprintf(b, "- The last %d lines of its job's log said:\n\n", len(f.Log))
	for _, line := range f.Log {
		fmt.Fprintf(b, "      %s\n", line)
	}
	b.WriteString("\n")
}
