package request

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Per-ticket caps on the oracle files (MANIFEST.json and RUN_COMMAND.txt are
// not counted) a ticket may receive from the request-level oracle. Over either
// cap planning is refused rather than silently dropping files: a dropped file
// is a criterion the operator approved an oracle for that nothing then checks.
// MaxTicketOracleFiles equals the drafter's own request-level file cap
// (draft_acceptance_oracles.MAX_ORACLE_FILES): the drafter tends to write one
// file per criterion, so a smaller per-ticket cap made an ordinary
// single-ticket plan of 8 criteria always halt at planning. The byte cap
// (also the drafter's total cap) is the bound that matters for size.
const (
	MaxTicketOracleFiles = 15
	MaxTicketOracleBytes = 64 * 1024
)

// ManifestFileName is MANIFEST.json's name inside an oracle directory (the
// same constant as internal/oraclecommit.ManifestName, which this package does
// not import).
const ManifestFileName = "MANIFEST.json"

// requestOracleRelPrefix is the ApprovedSHA256 key prefix of the request-level
// oracle files an oracle_review approval pins.
const requestOracleRelPrefix = RequestOracleDirName + "/"

// RequestOraclePins returns the request-level oracle files pinned at
// oracle_review approval, keyed by bare file name. Empty means the request has
// no approved request-level oracle (every request submitted without
// -draft-oracles, and a skipped oracle stage): nothing is materialized and
// every ticket oracle directory keeps today's hand-installed semantics.
func RequestOraclePins(r *Request) map[string]string {
	pins := map[string]string{}
	for rel, hash := range r.ApprovedSHA256 {
		if strings.HasPrefix(rel, requestOracleRelPrefix) {
			pins[strings.TrimPrefix(rel, requestOracleRelPrefix)] = hash
		}
	}
	return pins
}

// manifestEntry is one request-level MANIFEST.json entry: the whole raw
// object (so every field survives into the per-ticket manifest verbatim) plus
// the three fields materialization keys on.
type manifestEntry struct {
	raw        map[string]json.RawMessage
	criterion  string
	oracleFile string
	index      int
	hasIndex   bool
}

// parseAllManifestEntries reads every entry, including null-oracle_file
// (judgment-call) ones, which oraclecommit.ParseManifest skips.
func parseAllManifestEntries(data []byte) ([]manifestEntry, error) {
	var raws []map[string]json.RawMessage
	if err := json.Unmarshal(data, &raws); err != nil {
		return nil, fmt.Errorf("%s is not a JSON array of objects: %w", ManifestFileName, err)
	}
	out := make([]manifestEntry, 0, len(raws))
	for i, raw := range raws {
		e := manifestEntry{raw: raw}
		if v, ok := raw["oracle_file"]; ok && string(v) != "null" {
			if err := json.Unmarshal(v, &e.oracleFile); err != nil {
				return nil, fmt.Errorf("%s entry %d: oracle_file: %w", ManifestFileName, i, err)
			}
		}
		if v, ok := raw["criterion"]; ok && string(v) != "null" {
			if err := json.Unmarshal(v, &e.criterion); err != nil {
				return nil, fmt.Errorf("%s entry %d: criterion: %w", ManifestFileName, i, err)
			}
		}
		if v, ok := raw["criterion_index"]; ok && string(v) != "null" {
			if err := json.Unmarshal(v, &e.index); err != nil {
				return nil, fmt.Errorf("%s entry %d: criterion_index: %w", ManifestFileName, i, err)
			}
			e.hasIndex = true
		}
		out = append(out, e)
	}
	return out, nil
}

// readPinnedRequestOracleFile reads oracle/<name> and requires its bytes to
// hash to the value pinned at oracle_review: the bytes returned are exactly
// the approved ones, never a later edit.
func readPinnedRequestOracleFile(dataDir string, r *Request, pins map[string]string, name string) ([]byte, error) {
	want, ok := pins[name]
	if !ok {
		return nil, fmt.Errorf("oracle/%s is not among the files pinned at oracle_review", name)
	}
	full := filepath.Join(Dir(dataDir, r.ID), RequestOracleDirName, name)
	info, err := os.Lstat(full)
	if err != nil {
		return nil, fmt.Errorf("oracle/%s: %w", name, err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("oracle/%s is not a regular file", name)
	}
	b, err := os.ReadFile(full)
	if err != nil {
		return nil, fmt.Errorf("oracle/%s: %w", name, err)
	}
	if got := sha256Hex(b); got != want {
		return nil, fmt.Errorf("oracle/%s has changed since oracle_review approved it (hash %s, pinned %s)", name, got, want)
	}
	return b, nil
}

// oracleFileRefPattern finds `.oracle/<file>` references in a run command.
// The name must start with a letter, digit or underscore, so the directory
// forms `.oracle/...`, `.oracle/` and `.oracle/*` are not file references.
var oracleFileRefPattern = regexp.MustCompile(`(?:^|[\s"'=/$(){},:])` + regexp.QuoteMeta(TicketOracleMountPath) + `/([A-Za-z0-9_][A-Za-z0-9_.\-]*)`)

// checkRunCommandScope enforces the directory-scope rule for one ticket. The
// request-level RUN_COMMAND.txt is copied to every ticket that receives part of
// the oracle, but tickets receive SUBSETS, so a command naming a file breaks
// (the overlay splices a file that is not mounted) for a ticket without it.
// The proven Go overlay commands (oraclecanary.GoCommand) legitimately name
// `.oracle/<file>`, so a named file is not refused outright: it is refused only
// when the ticket's own subset does not contain it. A directory-scoped command
// (`go test ./.oracle/...`, `pytest .oracle`) references no file and is
// accepted for every ticket.
func checkRunCommandScope(command string, subset []string) error {
	have := map[string]bool{}
	for _, f := range subset {
		have[f] = true
	}
	stripped := stripShellComments(command)
	for _, idx := range oracleFileRefPattern.FindAllStringSubmatchIndex(stripped, -1) {
		name := stripped[idx[2]:idx[3]]
		// A name followed by a glob metacharacter is a pattern, not a file:
		// the stdlib Python runner (oraclecanary.PythonStdlibCommand) globs
		// `.oracle/test_oracle_*.py`, which covers whatever subset a ticket
		// receives. Read as a file, it halted every multi-file Python
		// request at materialization (console walk, 2026-09-24).
		if idx[3] < len(stripped) && strings.ContainsRune("*?[", rune(stripped[idx[3]])) {
			continue
		}
		if !have[name] {
			return fmt.Errorf("RUN_COMMAND.txt names %s/%s, which is not among this ticket's oracle files %v -- tickets receive subsets of the request-level oracle, so write the command directory-scoped (for example `go test ./%s/...`) instead of naming a file", TicketOracleMountPath, name, subset, TicketOracleMountPath)
		}
	}
	return nil
}

type ticketOracleAssignment struct {
	files   map[string]bool
	entries []map[string]json.RawMessage
	bytes   int
}

// derivedTicketOracle is one ticket's expected oracle directory content.
type derivedTicketOracle struct {
	files map[string][]byte // every file, including MANIFEST.json and RUN_COMMAND.txt
}

// deriveTicketOracles is the pure planning half of the materializer: from the
// pinned request-level oracle and the tickets it computes, per ticket index,
// the exact directory content each ticket must hold. It touches nothing on
// disk except reading the pinned originals and ticket specs. A ticket absent
// from the result must have no oracle directory. Refusals here are the
// plan-dependent ones (last claimant, per-ticket caps, command scope across the
// split); every plan-independent check lives in ValidateRequestOracleDir and
// runs at oracle_review approval.
func deriveTicketOracles(dataDir string, r *Request, pins map[string]string, tickets []Ticket) (map[int]*derivedTicketOracle, error) {
	manifestBytes, err := readPinnedRequestOracleFile(dataDir, r, pins, ManifestFileName)
	if err != nil {
		return nil, fmt.Errorf("a request-level oracle needs a MANIFEST.json to be assigned to tickets: %w", err)
	}
	command, err := readPinnedRequestOracleFile(dataDir, r, pins, TicketOracleRunCommandFilename)
	if err != nil {
		return nil, err
	}
	if err := ValidateOracleRunCommand(string(command)); err != nil {
		return nil, fmt.Errorf("oracle/%s: %w", TicketOracleRunCommandFilename, err)
	}
	entries, err := parseAllManifestEntries(manifestBytes)
	if err != nil {
		return nil, err
	}
	specContent, err := os.ReadFile(filepath.Join(Dir(dataDir, r.ID), specFileName))
	if err != nil {
		return nil, fmt.Errorf("read approved spec: %w", err)
	}
	criteria, err := SpecAcceptanceCriteria(string(specContent))
	if err != nil {
		return nil, fmt.Errorf("approved spec.md: %w", err)
	}
	pinned := map[string]bool{}
	for n := range pins {
		pinned[n] = true
	}
	if problems := validateOracleManifest(manifestBytes, criteria, pinned); len(problems) > 0 {
		return nil, errors.New(strings.Join(problems, "; "))
	}

	// Last claimant of each criterion.
	lastClaimant := map[int]int{}
	for i, t := range tickets {
		content, err := os.ReadFile(t.SpecPath)
		if err != nil {
			return nil, err
		}
		nums, err := TicketCoveredCriteria(string(content))
		if err != nil {
			return nil, fmt.Errorf("ticket %s: %w", filepath.Base(t.SpecPath), err)
		}
		for _, n := range nums {
			lastClaimant[n] = i
		}
	}

	assigned := map[int]*ticketOracleAssignment{}
	contents := map[string][]byte{}
	type fileClaim struct{ criterion, ticket int }
	firstClaim := map[string]fileClaim{}
	for i, e := range entries {
		if e.oracleFile == "" {
			continue
		}
		label := fmt.Sprintf("%s entry %d (%q)", ManifestFileName, i, e.oracleFile)
		ti, ok := lastClaimant[e.index]
		if !ok {
			return nil, fmt.Errorf("%s: no ticket claims criterion %d", label, e.index)
		}
		if first, seen := firstClaim[e.oracleFile]; !seen {
			firstClaim[e.oracleFile] = fileClaim{criterion: e.index, ticket: ti}
		} else if first.ticket != ti {
			return nil, fmt.Errorf("oracle file %q covers criterion %d (ticket %s) and criterion %d (ticket %s): a shared oracle file is copied whole to a ticket and its tests would run against code the other ticket implements -- split the plan so these criteria land in one ticket, or edit the oracle so each file covers one ticket",
				e.oracleFile, first.criterion, filepath.Base(tickets[first.ticket].SpecPath), e.index, filepath.Base(tickets[ti].SpecPath))
		}
		if _, ok := contents[e.oracleFile]; !ok {
			b, err := readPinnedRequestOracleFile(dataDir, r, pins, e.oracleFile)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", label, err)
			}
			contents[e.oracleFile] = b
		}
		a := assigned[ti]
		if a == nil {
			a = &ticketOracleAssignment{files: map[string]bool{}}
			assigned[ti] = a
		}
		if !a.files[e.oracleFile] {
			a.files[e.oracleFile] = true
			a.bytes += len(contents[e.oracleFile])
		}
		a.entries = append(a.entries, e.raw)
	}

	out := map[int]*derivedTicketOracle{}
	for ti, a := range assigned {
		name := filepath.Base(tickets[ti].SpecPath)
		files := make([]string, 0, len(a.files))
		for f := range a.files {
			files = append(files, f)
		}
		sort.Strings(files)
		if len(files) > MaxTicketOracleFiles {
			return nil, fmt.Errorf("ticket %s would receive %d oracle files %v, over the per-ticket cap of %d -- narrow the request-level oracle or split the plan", name, len(files), files, MaxTicketOracleFiles)
		}
		if a.bytes > MaxTicketOracleBytes {
			return nil, fmt.Errorf("ticket %s would receive %d bytes of oracle files %v, over the per-ticket cap of %d -- narrow the request-level oracle or split the plan", name, a.bytes, files, MaxTicketOracleBytes)
		}
		if err := checkRunCommandScope(string(command), files); err != nil {
			return nil, fmt.Errorf("ticket %s: %w", name, err)
		}
		manifest, err := json.MarshalIndent(a.entries, "", "  ")
		if err != nil {
			return nil, err
		}
		d := &derivedTicketOracle{files: map[string][]byte{ManifestFileName: append(manifest, '\n'), TicketOracleRunCommandFilename: command}}
		for _, f := range files {
			d.files[f] = contents[f]
		}
		out[ti] = d
	}
	return out, nil
}

// MaterializeTicketOracles derives each ticket's own tickets/<NNN>.oracle/ from
// the approved request-level oracle/, so the existing per-ticket pinning,
// resolveTicketOracle, the reference_oracle gate, the canary and the host commit
// all work unchanged. It is a no-op (touches nothing) for a request with no
// approved request-level oracle. Rules, all deterministic (see
// deriveTicketOracles):
//
//   - Manifest entries with an oracle_file are keyed by criterion_index
//     (1-based) into the approved spec's numbered acceptance criteria.
//     Entries with a null oracle_file are judgment calls and are skipped.
//   - A criterion claimed by several tickets goes to the LAST claimant only
//     (tickets are numbered in dependency order; an oracle failure is
//     per-command, so an early copy would only burn rounds).
//   - A ticket assigned nothing gets no directory. Otherwise the directory is
//     flat: its files, a per-ticket MANIFEST.json of only its entries, and a
//     byte-for-byte RUN_COMMAND.txt.
//   - At most MaxTicketOracleFiles files and MaxTicketOracleBytes bytes per
//     ticket, and the command must pass checkRunCommandScope for every ticket.
//   - Every existing tickets/*.oracle is removed first, so a shrunken re-plan
//     leaves no stale directory to be pinned and mounted.
//
// All validation happens before any write. The caller halts planning on a
// returned error (typed, so `factoryd retry` returns the request to
// oracle_review) and removes the tickets directory.
func MaterializeTicketOracles(dataDir string, r *Request, tickets []Ticket) error {
	pins := RequestOraclePins(r)
	if len(pins) == 0 {
		return nil
	}
	derived, err := deriveTicketOracles(dataDir, r, pins, tickets)
	if err != nil {
		return err
	}
	ticketsDir := filepath.Join(Dir(dataDir, r.ID), "tickets")
	if err := clearTicketOracleDirs(ticketsDir); err != nil {
		return fmt.Errorf("clear stale ticket oracle directories: %w", err)
	}
	for ti := range tickets {
		d := derived[ti]
		if d == nil {
			continue
		}
		dir := TicketOracleDir(tickets[ti].SpecPath)
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return err
		}
		for name, b := range d.files {
			if err := os.WriteFile(filepath.Join(dir, name), b, 0o600); err != nil {
				return err
			}
		}
	}
	return nil
}

// clearTicketOracleDirs removes every tickets/*.oracle entry. It works only
// inside ticketsDir, which must be a real directory (Lstat, not a symlink), and
// removes a symlink entry itself rather than following it.
func clearTicketOracleDirs(ticketsDir string) error {
	info, err := os.Lstat(ticketsDir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", ticketsDir)
	}
	entries, err := os.ReadDir(ticketsDir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".oracle") {
			if err := os.RemoveAll(filepath.Join(ticketsDir, e.Name())); err != nil {
				return err
			}
		}
	}
	return nil
}

// ErrMaterializedOracleEdited is wrapped by Approve's error when a ticket
// oracle directory of a request that HAS an approved request-level oracle
// differs from what the materializer derives from it.
var ErrMaterializedOracleEdited = errors.New("ticket oracle does not match the request-level oracle it was derived from")

// verifyMaterializedOracles is the check at plan approval. For a request with
// no approved request-level oracle it does nothing, so a hand-installed ticket
// oracle on a flag-less request approves exactly as before. Otherwise it
// RE-DERIVES the assignment (deriveTicketOracles: last claimant, caps, command
// scope, all from the pinned originals and the current ticket specs) and
// requires each ticket's on-disk oracle files to equal the derived set exactly:
// the same file names, each hashing to the derived bytes (copies of the pinned
// originals, the derived per-ticket MANIFEST.json, the pinned RUN_COMMAND.txt),
// and no directory at all for a ticket the derivation gives none. hashes maps
// every relPath this approval covers to its current hash, so what is compared
// is exactly what is about to be pinned. Hand edits, a deleted directory, a file
// copied into another ticket, or a file and entry moved between tickets are all
// refused, not approved as a new version.
func verifyMaterializedOracles(dataDir string, r *Request, specRelPaths []string, hashes map[string]string) error {
	pins := RequestOraclePins(r)
	if len(pins) == 0 {
		return nil
	}
	tickets := make([]Ticket, len(specRelPaths))
	for i, rel := range specRelPaths {
		tickets[i] = Ticket{Index: i + 1, SpecPath: filepath.Join(Dir(dataDir, r.ID), rel)}
	}
	derived, err := deriveTicketOracles(dataDir, r, pins, tickets)
	if err != nil {
		return fmt.Errorf("%w: re-deriving the assignment failed: %v (reject the plan to re-plan)", ErrMaterializedOracleEdited, err)
	}
	actual := map[string]map[string]string{} // oracle dir relPath -> file name -> hash
	for rel, h := range hashes {
		if !isOracleRelPath(rel) {
			continue
		}
		dir := filepath.Dir(rel)
		if actual[dir] == nil {
			actual[dir] = map[string]string{}
		}
		actual[dir][filepath.Base(rel)] = h
	}
	known := map[string]bool{}
	for i, spec := range specRelPaths {
		dir := TicketOracleDir(spec)
		known[dir] = true
		got := actual[dir]
		want := derived[i]
		if want == nil {
			if len(got) > 0 {
				return fmt.Errorf("%w: %s exists but the request-level oracle assigns this ticket no files -- delete it, or reject the plan to re-plan", ErrMaterializedOracleEdited, dir)
			}
			continue
		}
		for name, b := range want.files {
			h, ok := got[name]
			switch {
			case !ok:
				return fmt.Errorf("%w: %s/%s is missing -- restore it, or reject the plan to re-plan", ErrMaterializedOracleEdited, dir, name)
			case h != sha256Hex(b):
				return fmt.Errorf("%w: %s/%s has been edited (copies are derived from oracle/ and are read-only) -- restore it, or reject the plan to re-plan", ErrMaterializedOracleEdited, dir, name)
			}
		}
		for name := range got {
			if _, ok := want.files[name]; !ok {
				return fmt.Errorf("%w: %s/%s is not part of this ticket's derived oracle -- remove it, or reject the plan to re-plan", ErrMaterializedOracleEdited, dir, name)
			}
		}
	}
	for dir := range actual {
		if !known[dir] {
			return fmt.Errorf("%w: %s does not belong to any ticket", ErrMaterializedOracleEdited, dir)
		}
	}
	return nil
}
