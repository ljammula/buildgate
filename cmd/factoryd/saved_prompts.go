package main

import (
	"log"
	"path/filepath"

	"buildgate/internal/evidence"
	"buildgate/internal/request"
)

// retainDraftPrompts copies the prompts a drafting job's script saved in the
// given session folders (relative to workspace) into the request's own
// directory, prompts/<kind>-<n>/, for the operator (SC-018), and removes them
// from the drafting worktree. Best-effort: a failure is logged and the job
// goes on, since the prompts are an aid.
func retainDraftPrompts(dataDir, requestID, kind, workspace string, sessions []string) {
	dir := request.Dir(dataDir, requestID)
	dst := filepath.Join(dir, evidence.PromptsDirName, evidence.PromptAttemptDir(kind, evidence.NextPromptAttempt(dir, kind)))
	if _, err := evidence.RetainPrompts(workspace, sessions, dst); err != nil {
		log.Printf("request %s: retain the %s job's prompts: %v", requestID, kind, err)
	}
	if err := evidence.DropSavedPrompts(workspace, sessions); err != nil {
		log.Printf("request %s: remove the %s job's saved prompts: %v", requestID, kind, err)
	}
}
