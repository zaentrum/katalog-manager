package rest

import (
	"context"
	"errors"
	"fmt"
	"log"
	"mime"
	"net/http"
	"strconv"
	"strings"

	"github.com/zaentrum/katalog-manager/internal/compliance"
	"github.com/zaentrum/katalog-manager/internal/graph"
)

// getComplianceCSV serves GET /api/manage/compliance.csv, for admins, as
// GraphQL's libraryCompliance is: the compliance report's rows as CSV (RFC
// 4180, UTF-8, a header row of compliance.CSVColumns), every title the
// filter matches that breaks a rule, not a page, in the order asked, written
// as it is read. The query is the report's filter and order, as GraphQL
// names their values (case aside): kind (MOVIE or EPISODE; both without),
// rules (rule codes, by comma or the parameter repeated; any rule without),
// search, sort (a ComplianceSort; PACKAGE_SIZE without) and descending (true
// without). The file is named by the kind and when the check found the
// titles (Content-Disposition, "rules-movies-2026-10-26T1343Z.csv"). 400 for
// a kind, a rule, a sort or a descending it doesn't know; 503 without
// migration 051. A CSV that can't be read to its end is cut off, never ended
// as if it were whole.
func (h *Handlers) getComplianceCSV(w http.ResponseWriter, r *http.Request) {
	if h.d.Compliance == nil {
		writeError(w, http.StatusServiceUnavailable, "the library's compliance check is not configured")
		return
	}
	q := r.URL.Query()
	var f graph.ComplianceFilter
	switch k := strings.ToLower(strings.TrimSpace(q.Get("kind"))); k {
	case "":
	case graph.ComplianceMovie, graph.ComplianceEpisode:
		f.Kind = k
	default:
		writeError(w, http.StatusBadRequest, fmt.Sprintf("kind is %q: MOVIE or EPISODE, or none for both", q.Get("kind")))
		return
	}
	for _, v := range q["rules"] {
		for _, rule := range strings.Split(v, ",") {
			if rule = strings.ToUpper(strings.TrimSpace(rule)); rule != "" {
				f.Rules = append(f.Rules, rule)
			}
		}
	}
	f.Search = strings.TrimSpace(q.Get("search"))
	o := graph.ComplianceOrder{Sort: strings.ToUpper(strings.TrimSpace(q.Get("sort"))), Descending: true}
	if v := strings.TrimSpace(q.Get("descending")); v != "" {
		d, err := strconv.ParseBool(v)
		if err != nil {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("descending is %q: true or false", v))
			return
		}
		o.Descending = d
	}
	exp, err := h.d.Compliance.Export(reqCtx(r), f, o)
	var bad *compliance.BadQuery
	switch {
	case errors.As(err, &bad):
		writeError(w, http.StatusBadRequest, bad.Error())
		return
	case errors.Is(err, compliance.ErrNotReady):
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	case err != nil:
		writeError(w, http.StatusInternalServerError, "the report could not be read: "+err.Error())
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8; header=present")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": exp.Name}))
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)
	if err := exp.Write(reqCtx(r), w, func() { _ = rc.Flush() }); err != nil {
		// The rows sent can't be taken back: the response is cut off, so
		// that a CSV that misses rows is never read as a whole one.
		if !errors.Is(err, context.Canceled) {
			log.Printf("compliance: the CSV %s was cut off: %v", exp.Name, err)
		}
		panic(http.ErrAbortHandler)
	}
}
