package requestdriver

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

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

// challengeCriteriaHeading heads the block that names a spec's criteria by ID.
const challengeCriteriaHeading = "# Acceptance criteria by ID"

// criterionLeadRE matches what a folded criterion starts with: its number,
// then any AC-n label the spec gave it. Group 1 is the number, group 2 the
// label's.
var criterionLeadRE = regexp.MustCompile(`^(\d+)[.)]\s+(?:AC-(\d+):\s*)?`)

// challengeCriteriaBlock names each of a spec's criteria by the AC-n ID a
// challenge file's header cites, unless every criterion is labelled with its
// own position (a drafted spec): the challenger can cite only a number the
// text it reads shows, and a handed-over spec may label none, some, or label
// them out of order. A label the spec gave is left out of the text here, so
// the block holds one ID per criterion. "" when there are none.
func challengeCriteriaBlock(criteria []string) string {
	if len(criteria) == 0 || criteriaLabelledInPlace(criteria) {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n\nThe spec above does not label each criterion with its own number; the factory numbers them as below, and a test file's header cites one of these IDs.\n", challengeCriteriaHeading)
	for i, c := range criteria {
		fmt.Fprintf(&b, "\nAC-%d: %s\n", i+1, criterionLeadRE.ReplaceAllString(c, ""))
	}
	return b.String()
}

// challengeCoveredBlockHeading heads the block naming the criteria a ticket's
// challenge may test.
const challengeCoveredBlockHeading = "# Criteria this ticket covers"

// challengeCovered are the criteria numbers ticket and the tickets before it
// cover, sorted: the only ones its challenge may test, since a later ticket's
// criteria are not built yet. nil when ticket's own coverage cannot be read
// (a spec-as-ticket or hand-written ticket): the challenge is then not
// narrowed. Tickets before it whose coverage cannot be read claim nothing.
func challengeCovered(tickets []request.Ticket, ticket request.Ticket) []int {
	seen := map[int]bool{}
	for _, t := range tickets {
		if t.Index > ticket.Index {
			continue
		}
		content, err := os.ReadFile(t.SpecPath)
		if err != nil {
			if t.Index == ticket.Index {
				return nil
			}
			continue
		}
		numbers, err := request.TicketCoveredCriteria(string(content))
		if err != nil {
			if t.Index == ticket.Index {
				return nil
			}
			continue
		}
		for _, n := range numbers {
			seen[n] = true
		}
	}
	covered := make([]int, 0, len(seen))
	for n := range seen {
		covered = append(covered, n)
	}
	slices.Sort(covered)
	return covered
}

// challengeCoveredBlock names the criteria covered, for the challenger to read.
func challengeCoveredBlock(covered []int) string {
	if len(covered) == 0 {
		return ""
	}
	ids := make([]string, len(covered))
	for i, n := range covered {
		ids[i] = fmt.Sprintf("AC-%d", n)
	}
	return fmt.Sprintf("%s\n\nWrite tests only for these criteria: %s. The code for the other criteria is not built yet: a test file citing one is not run.\n", challengeCoveredBlockHeading, strings.Join(ids, ", "))
}

// criteriaLabelledInPlace reports whether criterion i of the folded list
// starts with the label AC-(i+1).
func criteriaLabelledInPlace(criteria []string) bool {
	for i, c := range criteria {
		m := criterionLeadRE.FindStringSubmatch(c)
		if m == nil || m[2] != strconv.Itoa(i+1) {
			return false
		}
	}
	return true
}

// withChallenge sets entry's challenge inputs for a build of ticket (SC-022):
// a ticket not yet challenged gets the challenger job (its first build that
// reaches the review), one challenged gets the held files of its challenge to
// run again on the result. round marks a round while the held files are not
// yet committed on the branch: its build also gets them mounted read-only, and
// its green result commits them.
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
		criteriaText, err := request.SpecAcceptanceCriteria(string(spec))
		if err != nil {
			criteriaText = nil
			log.Printf("request %s ticket %d: no AC-n header can be valid for the challenger: %v", r.ID, ticket.Index, err)
		}
		criteria := len(criteriaText)
		approvedBytes := len(spec)
		answered := r.ReadBack.Answered()
		if r.ReadBack == nil {
			answered = nil
		}
		input := string(spec)
		if block := challengeCriteriaBlock(criteriaText); block != "" {
			input += "\n\n" + block
		}
		if block := AnsweredReadBackBlock(answered); block != "" {
			input += "\n\n" + block + "\n"
		}
		covered := challengeCovered(r.Tickets, ticket)
		if block := challengeCoveredBlock(covered); block != "" {
			input += "\n\n" + block
		}
		path := filepath.Join(dir, challengeInputFile)
		if err := os.WriteFile(path, []byte(input), 0o600); err != nil {
			return fmt.Errorf("write the challenge input: %w", err)
		}
		entry.Challenge, entry.ChallengeSpecPath, entry.ChallengeCriteria, entry.ChallengeAnswers = true, path, criteria, answeredNumbers(answered)
		entry.ChallengeCovered = covered
		entry.ChallengeApprovedSHA256, entry.ChallengeApprovedBytes = r.ApprovedSHA256[requestSpecFile], approvedBytes
		return nil
	}
	path, err := writeHeldManifest(dataDir, r, ticket)
	if err != nil || path == "" {
		return err
	}
	entry.ChallengeHeld, entry.ChallengeRound = path, round
	return nil
}

// withChallengeSetAside sets entry's -challenge-set-aside for a replay run of
// ticket (SC-022): the manifest of the held files its branch already
// carries, so diff_scope sets them aside by their pinned hash as it does for
// every build. A replay runs no challenge, so nothing else is set. A ticket
// that was not challenged, or is carved, gets nothing.
func withChallengeSetAside(entry *QueueEntry, dataDir string, r *request.Request, ticket request.Ticket) error {
	if ticket.Order != nil || !ticket.Challenged {
		return nil
	}
	path, err := writeHeldManifest(dataDir, r, ticket)
	if err != nil {
		return err
	}
	entry.ChallengeSetAside = path
	return nil
}

// writeHeldManifest writes the JSON manifest of the held files of ticket's
// challenge run into the request's challenge directory and returns its path.
// It returns "" when the ticket has no challenge run or that run held no files.
func writeHeldManifest(dataDir string, r *request.Request, ticket request.Ticket) (string, error) {
	if ticket.ChallengeRunID == "" {
		return "", nil
	}
	held, err := heldChallengeFiles(dataDir, ticket.ChallengeRunID)
	if err != nil || len(held) == 0 {
		return "", err
	}
	data, err := json.MarshalIndent(held, "", "  ")
	if err != nil {
		return "", err
	}
	dir := filepath.Join(request.Dir(dataDir, r.ID), "challenge")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", fmt.Errorf("create %s: %w", dir, err)
	}
	path := filepath.Join(dir, fmt.Sprintf("t%d-%s", ticket.Index, challengeHeldFile))
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return "", fmt.Errorf("write the held challenge files: %w", err)
	}
	return path, nil
}

// answeredNumbers are the numbers of the answered read-back questions, the
// n of each ID Q<n>: what a challenge file's answer-<n> header may cite.
func answeredNumbers(answered []request.ReadBackQuestion) []int {
	var ns []int
	for _, q := range answered {
		if n, err := strconv.Atoi(strings.TrimPrefix(q.ID, "Q")); err == nil {
			ns = append(ns, n)
		}
	}
	return ns
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
