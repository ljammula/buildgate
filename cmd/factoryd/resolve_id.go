package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"buildgate/internal/request"
	"buildgate/internal/run"
	"buildgate/internal/sanitize"
)

// idKind is which records a command's id argument may name.
type idKind int

const (
	idRequest idKind = iota
	idRun
	idRequestOrRun
)

func (k idKind) noun() string {
	switch k {
	case idRequest:
		return "request"
	case idRun:
		return "run"
	}
	return "request or run"
}

// The most ids a refusal lists: the nearest to a query that matched nothing,
// and the candidates of one that matched several.
const (
	nearestIDsShown   = 3
	ambiguousIDsShown = 10
)

// decisionIDPartMin is the fewest characters of an id a decision accepts.
const decisionIDPartMin = 6

// fullIDShape is what a whole request or run id ends in: the time it was
// made, then for a request the number that told it from another made in the
// same second, for a run the process that launched it.
var fullIDShape = regexp.MustCompile(`^.+-\d{8}-\d{6}(-\d+)?$`)

// resolveIDArg turns the id an operator typed into the full id of one record
// under dataDir, for a command that only reads (watch, inspect, logs, rounds,
// cost, stats). It reads directory names only.
//
// A directory of the typed name is the record named, whatever it holds: the
// command's own load then reports a record that is missing. Otherwise the
// query names the one record whose id contains it (a prefix, or a part such
// as the trailing timestamp). A query that several ids contain is refused
// with those ids listed, never resolved to one of them. A query no id
// contains is refused with the nearest ids.
//
// For idRequestOrRun a request's id is part of each of its runs' ids, so a
// query that one request and only that request's runs contain names the
// request; a query only runs contain must match exactly one.
//
// flags and configPath are those the data dir was resolved from, and only
// word the refusal (which profile was searched).
func resolveIDArg(flags *flag.FlagSet, dataDir, configPath, query string, kind idKind) (string, error) {
	return resolveID(flags, dataDir, configPath, query, kind, false)
}

// resolveDecisionIDArg is resolveIDArg for a command that decides or changes
// a record (approve, reject, answer, override, cancel, retry, resume,
// amend-scope), which must not land on a record the operator did not name.
// It is narrower in three ways: the part must be the start or the end of the
// id, not any part of it; it must be at least decisionIDPartMin characters;
// and a query shaped like a whole id (fullIDShape) names that id or nothing,
// so the id of a record that is gone never resolves to a longer id that
// begins with it (a request made in the same second is "<id>-2").
func resolveDecisionIDArg(flags *flag.FlagSet, dataDir, configPath, query string, kind idKind) (string, error) {
	return resolveID(flags, dataDir, configPath, query, kind, true)
}

func resolveID(flags *flag.FlagSet, dataDir, configPath, query string, kind idKind, decision bool) (string, error) {
	if !run.ValidID(query) {
		return "", fmt.Errorf("%q is not a %s id", query, kind.noun())
	}
	var requests, runs []string
	if kind != idRun {
		if isDir(request.Dir(dataDir, query)) {
			return query, nil
		}
		requests = recordIDs(filepath.Dir(request.Dir(dataDir, "x")), filepath.Base(request.Path(dataDir, "x")))
	}
	if kind != idRequest {
		if isDir(run.Dir(dataDir, query)) {
			return query, nil
		}
		runs = recordIDs(filepath.Dir(run.Dir(dataDir, "x")), "run.json")
	}
	place := func() string { return idSearchPlace(flags, dataDir, configPath) }
	known := append(append([]string{}, requests...), runs...)
	part := strings.Contains
	if decision {
		switch {
		case fullIDShape.MatchString(query):
			return "", unknownIDError(query, kind, place(), known, "It is a whole id, and a decision takes a whole id as written, never a longer one that begins with it.")
		case len(query) < decisionIDPartMin:
			return "", fmt.Errorf("%q is too short to name the %s of a decision: give its full id, or %d or more characters from the start or the end of it", query, kind.noun(), decisionIDPartMin)
		}
		part = startsOrEndsWith
	}
	id, candidates := matchID(query, requests, runs, part)
	switch {
	case id != "":
		return id, nil
	case len(candidates) > 0:
		return "", ambiguousIDError(query, place(), candidates)
	}
	hint := ""
	if decision && len(idsWith(known, query, strings.Contains)) > 0 {
		hint = "A decision takes the start or the end of an id, not a part from the middle."
	}
	return "", unknownIDError(query, kind, place(), known, hint)
}

func isDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

func startsOrEndsWith(id, part string) bool {
	return strings.HasPrefix(id, part) || strings.HasSuffix(id, part)
}

// recordIDs are the names of the directories in dir that hold a record file,
// each one record's id. A directory without one is not a record: runs/ also
// holds a directory per request, for the sandboxes of its own jobs. None
// when dir cannot be read: the caller then reports that nothing matches.
func recordIDs(dir, recordFile string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	ids := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(dir, e.Name(), recordFile)); err == nil {
			ids = append(ids, e.Name())
		}
	}
	return ids
}

// matchID is the resolver's decision over the ids on disk: the one id query
// names, or, when it names none, every id it is a part of by the rule part
// (empty when there is none). It never returns an id together with other
// candidates.
func matchID(query string, requests, runs []string, part func(id, query string) bool) (id string, candidates []string) {
	for _, known := range append(append([]string{}, requests...), runs...) {
		if known == query {
			return query, nil
		}
	}
	requestHits, runHits := idsWith(requests, query, part), idsWith(runs, query, part)
	if len(requestHits) == 1 && runsAreOf(requestHits[0], runHits) {
		return requestHits[0], nil
	}
	if len(requestHits) == 0 && len(runHits) == 1 {
		return runHits[0], nil
	}
	return "", append(requestHits, runHits...)
}

func idsWith(ids []string, query string, part func(id, query string) bool) []string {
	var hits []string
	for _, id := range ids {
		if part(id, query) {
			hits = append(hits, id)
		}
	}
	sort.Strings(hits)
	return hits
}

// runsAreOf reports whether every run id is one of requestID's runs: a run of
// a request is named after it (<request-id>-<ticket>-...).
func runsAreOf(requestID string, runIDs []string) bool {
	for _, id := range runIDs {
		if !strings.HasPrefix(id, requestID+"-") {
			return false
		}
	}
	return true
}

// idSearchPlace names where an id was looked for: the data dir, and the
// profile it came from unless -data-dir named it.
func idSearchPlace(flags *flag.FlagSet, dataDir, configPath string) string {
	explicit := false
	if flags != nil {
		flags.Visit(func(f *flag.Flag) { explicit = explicit || f.Name == "data-dir" })
	}
	if name, _ := statusProfile(configPath); name != "" && !explicit {
		return fmt.Sprintf("%s (profile %s)", dataDir, name)
	}
	return dataDir
}

func ambiguousIDError(query string, place string, candidates []string) error {
	var b strings.Builder
	fmt.Fprintf(&b, "%q matches %d ids in %s, so nothing was done. Give more of the id of the one you mean:", query, len(candidates), place)
	for i, id := range candidates {
		if i == ambiguousIDsShown {
			fmt.Fprintf(&b, "\n  ... and %d more", len(candidates)-i)
			break
		}
		b.WriteString("\n  " + sanitize.Line(id))
	}
	return fmt.Errorf("%s", b.String())
}

// unknownIDError refuses a query that names nothing; hint, when not empty, is
// a sentence on why a near id was not taken.
func unknownIDError(query string, kind idKind, place string, known []string, hint string) error {
	msg := fmt.Sprintf("no %s matches %q in %s", kind.noun(), query, place)
	if hint != "" {
		msg += ". " + hint
		msg = strings.TrimSuffix(msg, ".")
	}
	nearest := nearestIDs(query, known, nearestIDsShown)
	if len(nearest) == 0 {
		return fmt.Errorf("%s. `factoryd status -all` lists the ids there, `factoryd use` the other profiles", msg)
	}
	for i := range nearest {
		nearest[i] = sanitize.Line(nearest[i])
	}
	return fmt.Errorf("%s. Nearest:\n  %s", msg, strings.Join(nearest, "\n  "))
}

// nearestIDs are up to n of known ranked by the longest run of characters
// each shares with query, longest first, then by id. An id sharing fewer
// than three characters in a row is not near.
func nearestIDs(query string, known []string, n int) []string {
	type scored struct {
		id    string
		score int
	}
	var ranked []scored
	for _, id := range known {
		if s := longestCommonRun(query, id); s >= 3 {
			ranked = append(ranked, scored{id, s})
		}
	}
	sort.Slice(ranked, func(i, j int) bool {
		if ranked[i].score != ranked[j].score {
			return ranked[i].score > ranked[j].score
		}
		return ranked[i].id < ranked[j].id
	})
	if len(ranked) > n {
		ranked = ranked[:n]
	}
	out := make([]string, len(ranked))
	for i, r := range ranked {
		out[i] = r.id
	}
	return out
}

// longestCommonRun is the length of the longest substring a and b share.
func longestCommonRun(a, b string) int {
	best := 0
	prev := make([]int, len(b)+1)
	for i := 1; i <= len(a); i++ {
		cur := make([]int, len(b)+1)
		for j := 1; j <= len(b); j++ {
			if a[i-1] == b[j-1] {
				cur[j] = prev[j-1] + 1
				best = max(best, cur[j])
			}
		}
		prev = cur
	}
	return best
}
