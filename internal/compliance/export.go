package compliance

import (
	"context"
	"encoding/csv"
	"io"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/zaentrum/katalog-manager/internal/graph"
)

// CSVColumns are the columns of the report's CSV, in order: a title's id,
// kind (MOVIE, EPISODE), title, year, series, season and number; its video's
// size, codec and bit rate (bit/s), whether that was estimated, and the caps
// that hold for it (bit/s, bytes); its duration (s); its original's, its
// package's and its estimated sizes (bytes); its package's and its
// original's audio; the rules it breaks (their codes, by ";"), what
// processing it again does, why that can't help, its state in the re-encode
// queue, and when the check found it (RFC 3339, UTC).
var CSVColumns = []string{"item_id", "kind", "title", "year", "series", "season", "episode", "resolution",
	"video_codec", "video_bitrate_bps", "bitrate_estimated", "bitrate_cap_bps", "size_cap_bytes", "duration_s",
	"original_bytes", "package_bytes", "estimated_bytes", "package_audio", "original_audio", "rules", "outcome",
	"blocked_reason", "queue", "checked_at"}

// exportBatch is how many rows the CSV reads at a time.
const exportBatch = maxPage

// BadQuery refuses a filter or an order the report doesn't know: a rule or a
// sort it has none of.
type BadQuery struct{ Reason string }

func (e *BadQuery) Error() string { return e.Reason }

// Export is the report's CSV of a filter's titles, every title it matches
// that breaks a rule, in an order: its file's name, and Write, which writes
// it.
type Export struct {
	// Name is the file's name, of the kind and of when the check found the
	// titles (the oldest of them, as the report's evaluatedAt; now when it
	// found none), in UTC: "rules-movies-2026-10-26T1343Z.csv".
	Name string

	s      *Service
	f      graph.ComplianceFilter
	rules  []string
	sorted sorting
}

// Export is the CSV of the titles f matches that break a rule, in the order
// o, as the report's rows are: a rule or a sort it doesn't know is a
// BadQuery.
func (s *Service) Export(ctx context.Context, f graph.ComplianceFilter, o graph.ComplianceOrder) (Export, error) {
	if why := s.ready(ctx); why != nil {
		return Export{}, why
	}
	rules, err := rulesOf(f)
	if err != nil {
		return Export{}, &BadQuery{err.Error()}
	}
	sorted, err := sortingOf(o)
	if err != nil {
		return Export{}, &BadQuery{err.Error()}
	}
	var at *time.Time
	if err := s.pool.QueryRow(ctx, filtered+`SELECT min(f.evaluatedat) FILTER (WHERE f.ruleshash <> '') FROM f`,
		f.Kind, f.Search).Scan(&at); err != nil {
		return Export{}, err
	}
	when := s.now()
	if at != nil {
		when = *at
	}
	return Export{Name: csvName(f.Kind, when), s: s, f: f, rules: rules, sorted: sorted}, nil
}

// csvName is the CSV's file name, of kind ("" both) and when.
func csvName(kind string, when time.Time) string {
	what := "all"
	switch kind {
	case graph.ComplianceMovie:
		what = "movies"
	case graph.ComplianceEpisode:
		what = "episodes"
	}
	return "rules-" + what + "-" + when.UTC().Format("2006-01-02T1504Z") + ".csv"
}

// Write writes the CSV to w (RFC 4180: a header row of CSVColumns, then a
// row of each title, each ended by CRLF, quoted where it must be, in UTF-8),
// reading exportBatch titles at a time, and calls flush, when not nil, after
// each of them: it holds no more of the report than that.
func (e Export) Write(ctx context.Context, w io.Writer, flush func()) error {
	cw := csv.NewWriter(w)
	cw.UseCRLF = true
	if err := cw.Write(CSVColumns); err != nil {
		return err
	}
	var cur []any
	for {
		rows, next, err := e.s.rows(ctx, e.f, e.rules, e.sorted, cur, exportBatch)
		if err != nil {
			return err
		}
		for _, r := range rows {
			if err := cw.Write(csvRecord(r)); err != nil {
				return err
			}
		}
		cw.Flush()
		if err := cw.Error(); err != nil {
			return err
		}
		if flush != nil {
			flush()
		}
		if next == nil {
			return nil
		}
		if cur, err = e.sorted.parse(*next); err != nil {
			return err
		}
	}
}

// csvRecord is a row of the report as the CSV writes it: numbers as they
// are, bytes and bit/s whole, nothing for none, and the console's no-break
// hyphens and spaces as plain ones.
func csvRecord(r graph.ComplianceRow) []string {
	rules := make([]string, len(r.Findings))
	for i, f := range r.Findings {
		rules[i] = f.Rule
	}
	return []string{r.ItemID, strings.ToUpper(r.Kind), plainText(r.Title), intText(r.Year), textOf(r.SeriesTitle),
		intText(r.Season), intText(r.Episode), textOf(r.Resolution), textOf(r.VideoCodec), wholeText(r.VideoBitrate),
		strconv.FormatBool(r.VideoBitrateEstimated), wholeText(r.BitrateCap), wholeText(r.SizeCap),
		numberText(r.DurationSeconds), wholeText(r.OriginalBytes), wholeText(r.PackageBytes), wholeText(r.EstimatedBytes),
		textOf(r.PackageAudio), textOf(r.OriginalAudio), strings.Join(rules, ";"), textOf(r.Outcome),
		textOf(r.ReworkBlocked), textOf(r.Queue), r.EvaluatedAt.UTC().Format(time.RFC3339)}
}

// plainText is s with the no-break hyphens and spaces the console's texts
// hold, which only keep a line from breaking there, as plain ones: a
// program that reads the CSV matches "E-AC-3".
var plainText = strings.NewReplacer(nbhy, "-", nbsp, " ").Replace

func textOf(s *string) string {
	if s == nil {
		return ""
	}
	return plainText(*s)
}

func intText(v *int32) string {
	if v == nil {
		return ""
	}
	return strconv.FormatInt(int64(*v), 10)
}

// wholeText is a count of bytes or of bit/s, rounded to a whole one.
func wholeText(v *float64) string {
	if v == nil {
		return ""
	}
	return strconv.FormatFloat(math.Round(*v), 'f', 0, 64)
}

// numberText is a number with as many decimals as it has.
func numberText(v *float64) string {
	if v == nil {
		return ""
	}
	return strconv.FormatFloat(*v, 'f', -1, 64)
}
