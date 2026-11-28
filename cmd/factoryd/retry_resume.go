package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"buildgate/internal/request"
	"buildgate/internal/requestdriver"
)

// resumeVerbOf maps `factoryd retry -from` to the resume decision of a
// resume_review request: attempt continues the lost build in its kept
// worktree, scratch rebuilds the ticket. A round number names a new branch of
// a rebuilt ticket and is refused.
func resumeVerbOf(from string) (string, error) {
	switch from {
	case retryFromAttempt:
		return request.ResumeRound, nil
	case retryFromScratch:
		return request.ResumeScratch, nil
	}
	return "", fmt.Errorf("-from for a request in resume_review must be %q or %q, got %q", retryFromAttempt, retryFromScratch, from)
}

// continueLostStep is `factoryd retry` of a request waiting in resume_review
// because the worker running its step stopped (request.ResumeRequest, the
// function POST /requests/{id}/resume shares). For a lost build and -from
// attempt it first checks the preconditions of continuing in the kept
// worktree (no container of the lost run alive, the history intact) and
// refuses, naming `-from scratch` and `factoryd cancel`, when they do not
// hold. reason is recorded on the request's history.
func continueLostStep(dp *deps, dataDir, configPath, id, from, reason string, gate requestdriver.ResumeGate) error {
	verb, err := resumeVerbOf(from)
	if err != nil {
		return err
	}
	settings, err := loadSettingsForConfig(configPath)
	if err != nil {
		return err
	}
	dp.useWebhookFiles(settings, os.Getenv)
	checked := ""
	updated, err := request.ResumeRequest(dataDir, id, verb, currentOSUser(), time.Now(), func(r *request.Request) ([]string, error) {
		reasons, err := gate.RequestResumeRefusals(context.Background(), dataDir, settings.SandboxDocker, r)
		if err == nil && len(reasons) == 0 && r.Resume != nil && r.Resume.LostRunID != "" {
			checked = fmt.Sprintf("resume preconditions hold for run %s: no sandbox container alive, worktree HEAD matches its round state", r.Resume.LostRunID)
		}
		return reasons, err
	}, request.WithGateExpiry(settings.ReviewGateExpiry), request.WithNote(reason))
	if err != nil {
		notifyIfGateExpired(dp, dataDir, id, err)
		return err
	}
	// A decision that continues no kept build (a drafting or planning rerun,
	// or pr_review) ends the wait of any worktree kept for this request.
	if updated.State != request.StateBuilding {
		if err := requestdriver.ClearKeptRunsOfRequest(dataDir, updated.ID); err != nil {
			fmt.Fprintf(os.Stderr, "request %s: could not discard a kept worktree: %v\n", updated.ID, err)
		}
	}
	wakeAfterSave(dp, context.Background(), os.Stderr, dataDir, updated.ID)
	if checked != "" {
		fmt.Println(checked)
	}
	fmt.Printf("request %s: state=%s (retry -from %s)\n", updated.ID, updated.State, from)
	return nil
}
