package requestdriver

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"buildgate/internal/request"
	"buildgate/internal/run"
)

// challengeInputFile and challengeHeldFile are the names, in a request's
// challenge directory, of the approved spec the challenger is given and of
// the manifest of held files a later build runs again.
const (
	challengeInputFile = "challenge-spec.md"
	challengeHeldFile  = "held.json"
)

// challengeEnabled reports whether a request's builds challenge: an approved
// spec to give the challenger exists (approved is its path). A bare `factoryd -ticket` run has none
// and never challenges; a request whose Settings hold no roles.challenger is
// still challenged, and its record says no challenger role is configured.
func challengeEnabled(dataDir string, r *request.Request) (approved string, ok bool, err error) {
	approved, err = approvedSpecPath(dataDir, r)
	return approved, approved != "", err
}

// withChallenge sets entry's challenge inputs for a build of ticket (SC-022):
// a ticket not yet challenged gets the challenger job (its first build that
// reaches the review), one challenged gets the held files of its challenge to
// run again on the result. round marks the ticket's challenge round, whose
// build also gets the held files mounted read-only and whose green result
// commits them.
func withChallenge(entry *QueueEntry, dataDir string, r *request.Request, ticket request.Ticket, round bool) error {
	if ticket.Order != nil {
		// A carved ticket's change was challenged (or not) on the run that
		// was carved: no challenger starts for it, now or on a later build.
		return nil
	}
	approved, ok, err := challengeEnabled(dataDir, r)
	if err != nil {
		// An approved spec that cannot be given (it changed since approval) is
		// a challenger with no verdict, never a build that does not start.
		log.Printf("request %s ticket %d: no challenge: %v", r.ID, ticket.Index, err)
		return nil
	}
	if !ok {
		return nil
	}
	dir := filepath.Join(request.Dir(dataDir, r.ID), "challenge")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	if !ticket.Challenged {
		spec, err := os.ReadFile(approved)
		if err != nil {
			return fmt.Errorf("read the approved spec: %w", err)
		}
		// The bytes given to the challenger are the bytes hashed here, so a
		// change between the check and the read cannot reach it.
		if sum := sha256.Sum256(spec); hex.EncodeToString(sum[:]) != r.ApprovedSHA256[requestSpecFile] {
			log.Printf("request %s ticket %d: no challenge: %s changed while it was read", r.ID, ticket.Index, requestSpecFile)
			return nil
		}
		criteria, err := request.SpecAcceptanceCriteriaCount(string(spec))
		if err != nil {
			criteria = 0
			log.Printf("request %s ticket %d: no AC-n header can be valid for the challenger: %v", r.ID, ticket.Index, err)
		}
		answered := r.ReadBack.Answered()
		if r.ReadBack == nil {
			answered = nil
		}
		input := string(spec)
		if block := AnsweredReadBackBlock(answered); block != "" {
			input += "\n\n" + block + "\n"
		}
		path := filepath.Join(dir, challengeInputFile)
		if err := os.WriteFile(path, []byte(input), 0o600); err != nil {
			return fmt.Errorf("write the challenge input: %w", err)
		}
		entry.Challenge, entry.ChallengeSpecPath, entry.ChallengeCriteria, entry.ChallengeAnswers = true, path, criteria, len(answered)
		return nil
	}
	if ticket.ChallengeRunID == "" {
		return nil
	}
	held, err := heldChallengeFiles(dataDir, ticket.ChallengeRunID)
	if err != nil || len(held) == 0 {
		return err
	}
	data, err := json.MarshalIndent(held, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(dir, fmt.Sprintf("t%d-%s", ticket.Index, challengeHeldFile))
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("write the held challenge files: %w", err)
	}
	entry.ChallengeHeld, entry.ChallengeRound = path, round
	return nil
}

// heldChallengeFiles are the files of run id's challenge a later build runs
// again: held copies with a header (a file held and not run is not one), in the order the job handed them over.
func heldChallengeFiles(dataDir, id string) ([]run.ChallengeFile, error) {
	rec, err := run.Load(dataDir, id)
	if err != nil {
		return nil, fmt.Errorf("load the challenge's run %s: %w", id, err)
	}
	if rec.Challenge == nil {
		return nil, nil
	}
	var held []run.ChallengeFile
	for _, f := range rec.Challenge.Files {
		if f.Refused == "" && f.NotRun == "" && f.HostPath != "" && f.SHA256 != "" {
			held = append(held, f)
		}
	}
	return held, nil
}

// noteChallenge records on ticket that runRecord ran the challenger job, so
// no later build of the ticket starts another (SC-022), and which run holds
// the files. A run that only ran held files again records nothing.
func noteChallenge(ticket *request.Ticket, runRecord *run.Run) bool {
	if ticket.Challenged || runRecord == nil || runRecord.Challenge == nil || runRecord.Challenge.Rerun {
		return false
	}
	ticket.Challenged, ticket.ChallengeRunID = true, runRecord.ID
	return true
}
