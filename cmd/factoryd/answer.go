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
func newAnswerFlags() (flags *flag.FlagSet, dataDir, configPath, question, text *string) {
	flags = flag.NewFlagSet("answer", flag.ContinueOnError)
	dataDir = flags.String("data-dir", "data", "directory containing durable request records")
	configPath = flags.String("config", "", "session config path or profile name, for the data dir; empty: the active profile, else the first of ~/.config/factoryd/config.yml and ~/.factory/config.yml that exists")
	question = flags.String("q", "", "the read-back question to answer, as the spec review lists it (Q1, Q2, ...)")
	text = flags.String("text", "", "your answer; every ticket's build is given it once the spec is approved. Empty clears the answer.")
	finishFlags(flags)
	return
}

// answerMain implements `factoryd answer -q <question> -text "<answer>"
// <request-id>`: records the operator's answer to one question of a
// handed-over spec's read-back while the request is in spec_review. Shares
// request.AnswerReadBack with PUT /requests/{id}/read-back/answers/{qid}.
func answerMain(args []string) error {
	flags, dataDir, configPath, question, text := newAnswerFlags()
	if err := flags.Parse(args); err != nil {
		return err
	}
	positional := flags.Args()
	if len(positional) != 1 || *question == "" {
		flags.Usage()
		return fmt.Errorf(`usage: factoryd answer -q <question> -text "<answer>" [-config <path>] [-data-dir <path>] <request-id>`)
	}
	if err := resolveDataDirFromSessionConfig(flags, dataDir, *configPath); err != nil {
		return err
	}
	id := positional[0]
	opts, err := gateExpiryOptions(*configPath)
	if err != nil {
		return fmt.Errorf("answer request %q: %w", id, err)
	}
	r, err := request.AnswerReadBack(*dataDir, id, *question, currentOSUser(), *text, time.Now(), opts...)
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
