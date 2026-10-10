package api

import (
	"log"
	"net/http"
	"strings"
	"time"

	"buildgate/internal/release"
	"buildgate/internal/run"
	"buildgate/internal/stats"
)

// getProjectTrend serves GET /projects/{project}/trend: whether the factory is
// getting better on one repository, as numbers (stats.Report) overall and per
// bucket of days. Query: since (30d or YYYY-MM-DD), until (same forms, exclusive), bucket (days), all=1 to
// count the live-smoke tickets too. It is computed from the run records on
// each read, stores nothing, calls no model and is gated like GET /runs, whose
// records it is made from. `factoryd stats` prints the same report.
func (s *Server) getProjectTrend(w http.ResponseWriter, r *http.Request) {
	if !s.authorizeRead(r) {
		writeError(w, http.StatusForbidden, "read endpoint is not authorized")
		return
	}
	project := r.PathValue("project")
	if !validRunID(project) {
		writeError(w, http.StatusBadRequest, "project must be a single path component")
		return
	}
	now := time.Now()
	query := r.URL.Query()
	since, err := stats.ParseSince(query.Get("since"), now)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	until, err := stats.ParseSince(query.Get("until"), now)
	if err != nil {
		writeError(w, http.StatusBadRequest, strings.Replace(err.Error(), "since", "until", 1))
		return
	}
	days, err := stats.ParseBucketDays(query.Get("bucket"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	loaded, err := run.LoadAll(s.dataDir)
	if err != nil {
		log.Printf("trend: read runs: %v", err)
		writeError(w, http.StatusInternalServerError, "read runs")
		return
	}
	var runs []*run.Run
	for _, rec := range loaded {
		if release.ProjectOf(rec) == project {
			runs = append(runs, rec)
		}
	}
	opts := stats.Options{Project: project, Since: since, Until: until, BucketDays: days, Now: now}
	if query.Get("all") == "" || query.Get("all") == "0" || query.Get("all") == "false" {
		opts.ExcludeTicketPrefixes = []string{stats.SmokePrefix}
	}
	writeJSON(w, http.StatusOK, stats.Compute(runs, opts))
}

// getStats serves GET /stats: stats.Overview, the report of every repository
// in this data dir and of all of them together, with weekly buckets and the
// live-smoke tickets left out (all=1 counts them). Query since (30d or
// YYYY-MM-DD) narrows it to the tickets begun since then, as it does for the
// trend. Computed from the run
// records on each read, like GET /projects/{project}/trend, and gated like
// it. `factoryd stats` with no project prints the same numbers.
func (s *Server) getStats(w http.ResponseWriter, r *http.Request) {
	if !s.authorizeRead(r) {
		writeError(w, http.StatusForbidden, "read endpoint is not authorized")
		return
	}
	loaded, err := run.LoadAll(s.dataDir)
	if err != nil {
		log.Printf("stats: read runs: %v", err)
		writeError(w, http.StatusInternalServerError, "read runs")
		return
	}
	now := time.Now()
	since, err := stats.ParseSince(r.URL.Query().Get("since"), now)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	opts := stats.Options{Now: now, Since: since}
	if all := r.URL.Query().Get("all"); all == "" || all == "0" || all == "false" {
		opts.ExcludeTicketPrefixes = []string{stats.SmokePrefix}
	}
	writeJSON(w, http.StatusOK, stats.ComputeOverview(loaded, opts, release.ProjectOf))
}
