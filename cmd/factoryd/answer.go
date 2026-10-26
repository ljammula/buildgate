package main

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"buildgate/internal/request"
)

// newAnswerFlags builds `factoryd answer`'s FlagSet in isolation from
// parsing, so the doc-vs-flag tests can enumerate its real flags.
func newAnswerFlags() (flags *flag.FlagSet, dataDir, configPath, question, hash, text *string) {
	flags = flag.NewFlagSet("answer", flag.ContinueOnError)
	dataDir = flags.String("data-dir", "data", "directory containing durable request records")
	configPath = flags.String("config", "", "session config path or profile name, for the data dir; empty: the active profile, else the first of ~/.config/factoryd/config.yml and ~/.factory/config.yml that exists")
	question = flags.String("q", "", "the read-back question to answer, as the spec review lists it (Q1, Q2, ...)")
	hash = flags.String("hash", "", "the short hash factoryd watch -no-follow prints beside the question; the answer is refused if the question has changed since")
	text = flags.String("text", "", "your answer; every ticket's build is given it once the spec is approved. Empty clears the answer.")
	finishFlags(flags)
	return
}

// answerMain implements `factoryd answer -q <question> -hash <hash> -text
// "<answer>" <request-id>`: records the operator's answer to one question of
// a handed-over spec's read-back while the request is in spec_review. Shares
// request.AnswerReadBack with PUT /requests/{id}/read-back/answers/{qid}.
// The answer is for the question text the operator read: -hash names it
// (request.QuestionShortHash, as `factoryd watch` prints it), and an answer
// to a question whose text no longer has that hash is refused.
func answerMain(args []string) error {
	flags, dataDir, configPath, question, hash, text := newAnswerFlags()
	if err := flags.Parse(args); err != nil {
		return err
	}
	positional := flags.Args()
	if len(positional) != 1 || *question == "" || *hash == "" {
		flags.Usage()
		return fmt.Errorf(`usage: factoryd answer -q <question> -hash <hash> -text "<answer>" [-config <path>] [-data-dir <path>] <request-id>`)
	}
	if err := resolveDataDirFromSessionConfig(flags, dataDir, *configPath); err != nil {
		return err
	}
	id := positional[0]
	opts, err := gateExpiryOptions(*configPath)
	if err != nil {
		return fmt.Errorf("answer request %q: %w", id, err)
	}
	questionSHA, err := shownQuestionSHA256(*dataDir, id, *question, *hash)
	if err != nil {
		return fmt.Errorf("answer request %q: %w", id, err)
	}
	r, err := request.AnswerReadBack(*dataDir, id, *question, questionSHA, currentOSUser(), *text, time.Now(), opts...)
	if err != nil {
		notifyIfGateExpired(*dataDir, id, err)
		return fmt.Errorf("answer request %q: %w", id, err)
	}
	verb := "answered"
	if strings.TrimSpace(*text) == "" {
		verb = "answer cleared"
	}
	if _, err := fmt.Fprintf(os.Stdout, "request %s: %s %s\n", r.ID, *question, verb); err != nil {
		return fmt.Errorf("print answer result: %w", err)
	}
	return nil
}

// shownQuestionSHA256 is request.QuestionSHA256 of question qid's text in
// request id's read-back as it is on disk now, provided hash, the short
// hash the operator was shown, is that text's (request.QuestionShortHash,
// at least that many hex digits). It refuses a hash of another text, which
// wraps request.ErrReadBackQuestionChanged. "" with no error when there is
// no such question (AnswerReadBack then names what is missing).
// AnswerReadBack checks the full hash again under the request's lock.
func shownQuestionSHA256(dataDir, id, qid, hash string) (string, error) {
	hash = strings.ToLower(strings.TrimSpace(hash))
	if len(hash) < request.QuestionHashLen {
		return "", fmt.Errorf("-hash %q: give the %d-digit hash `factoryd watch -no-follow %s` prints beside %s", hash, request.QuestionHashLen, id, qid)
	}
	r, err := request.Load(dataDir, id)
	if err != nil || r.ReadBack == nil {
		return "", nil
	}
	for _, q := range r.ReadBack.Questions {
		if q.ID != qid {
			continue
		}
		full := request.QuestionSHA256(q.Text)
		if !strings.HasPrefix(full, hash) {
			return "", fmt.Errorf("%s's text is not the one with hash %s: the read-back was replaced since it was shown %w (run `factoryd watch -no-follow %s` and read the question again)", qid, hash, request.ErrReadBackQuestionChanged, id)
		}
		return full, nil
	}
	return "", nil
}
