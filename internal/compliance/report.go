package compliance

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/zaentrum/katalog-manager/internal/graph"
	"github.com/zaentrum/katalog-manager/internal/library"
	"github.com/zaentrum/katalog-manager/internal/processing"
	"github.com/zaentrum/katalog-manager/internal/store"
)

// maxPage is the most titles a page of the report holds.
const maxPage = 500

// filtered is the SQL of the titles the check holds that a report's kind
// ($1, "" both) and search ($2, "" any) match, with what the catalog holds of
// each: its title, year, numbers and series.
const filtered = `WITH f AS (
	SELECT c.item_id, c.kind, c.rules, c.findings, c.originalbytes, c.packagebytes, c.estimatedbytes, c.unchecked,
		c.ruleshash, c.evaluatedat, i.title, i.year, i.seasonnumber, i.episodenumber, sr.id AS series_id,
		sr.title AS series_title
	FROM ` + store.ComplianceTable + ` c
	JOIN com_nalet_katalog_items i ON i.id = c.item_id
	LEFT JOIN com_nalet_katalog_items p ON p.id = i.parent_id
	LEFT JOIN com_nalet_katalog_items g ON g.id = p.parent_id
	CROSS JOIN LATERAL (SELECT
		CASE WHEN lower(p.type) = 'series' THEN p.id WHEN lower(g.type) = 'series' THEN g.id END AS id,
		CASE WHEN lower(p.type) = 'series' THEN p.title WHEN lower(g.type) = 'series' THEN g.title END AS title) sr
	WHERE ($1 = '' OR c.kind = $1)
	  AND ($2 = '' OR strpos(lower(i.title), lower($2)) > 0 OR strpos(lower(COALESCE(sr.title, '')), lower($2)) > 0))
`

// breaking is the SQL condition that a title of f breaks a rule, any of $3
// when it names some.
const breaking = `f.rules <> '{}' AND (cardinality($3::varchar[]) = 0 OR f.rules && $3::varchar[])`

// order is the report's order: the largest package first, then the largest
// original, a title without either last.
const order = `COALESCE(f.packagebytes, -1) DESC, COALESCE(f.originalbytes, -1) DESC, f.item_id`

// cursor is where a page of the report ended: its last title's package and
// original sizes (-1 for none) and id.
type cursor struct {
	pkg, orig int64
	id        string
}

func (c cursor) String() string {
	return base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf("c1:%d:%d:%s", c.pkg, c.orig, c.id)))
}

func parseCursor(s string) (*cursor, error) {
	if s == "" {
		return nil, nil
	}
	bad := fmt.Errorf("after is %q, which is no cursor of the report: pass a page's nextCursor", s)
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return nil, bad
	}
	parts := strings.SplitN(string(b), ":", 4)
	if len(parts) != 4 || parts[0] != "c1" || parts[3] == "" {
		return nil, bad
	}
	pkg, err1 := strconv.ParseInt(parts[1], 10, 64)
	orig, err2 := strconv.ParseInt(parts[2], 10, 64)
	if err1 != nil || err2 != nil {
		return nil, bad
	}
	return &cursor{pkg: pkg, orig: orig, id: parts[3]}, nil
}

// rulesOf are the rules a filter names, each one of Rules.
func rulesOf(f graph.ComplianceFilter) ([]string, error) {
	out := []string{}
	for _, r := range f.Rules {
		if !slices.Contains(Rules, r) {
			return nil, fmt.Errorf("unknown rule %q: one of %s", r, strings.Join(Rules, ", "))
		}
		out = append(out, r)
	}
	return out, nil
}

// Report is a page of the titles f matches that break a rule, as the check
// last found them, first of them after the cursor after: the largest package
// first. It says what the workers reported, what is not checked and why,
// how many titles f matches, and of the titles of f's kind and search, those
// that break each rule, whichever rules f names.
func (s *Service) Report(ctx context.Context, f graph.ComplianceFilter, first int32, after string) (graph.ComplianceReport, error) {
	out := graph.ComplianceReport{NotEvaluated: []string{}, Summary: []graph.ComplianceSummary{}, Rows: []graph.ComplianceRow{}}
	if why := s.ready(ctx); why != nil {
		return out, why
	}
	rules, err := rulesOf(f)
	if err != nil {
		return out, err
	}
	cur, err := parseCursor(after)
	if err != nil {
		return out, err
	}
	first = min(max(first, 0), maxPage)
	rs, err := s.state(ctx)
	if err != nil {
		return out, err
	}
	out.Rules = pipelineRulesOf(rs)
	if out.Evaluating, err = s.Evaluating(ctx); err != nil {
		return out, err
	}

	var unchecked int32
	var firstUnchecked *string
	if err := s.pool.QueryRow(ctx, filtered+`SELECT min(f.evaluatedat) FILTER (WHERE f.ruleshash <> ''),
			count(*) FILTER (WHERE `+breaking+`), count(*) FILTER (WHERE f.unchecked IS NOT NULL), min(f.unchecked)
		FROM f`, f.Kind, f.Search, rules).Scan(&out.EvaluatedAt, &out.Total, &unchecked, &firstUnchecked); err != nil {
		return out, err
	}
	out.NotEvaluated = notEvaluated(rs, unchecked, firstUnchecked)
	if out.Pending, err = s.pending(ctx, f.Kind, rs.hash()); err != nil {
		return out, err
	}
	if out.Summary, err = s.summary(ctx, f); err != nil {
		return out, err
	}
	if out.Rows, out.NextCursor, err = s.rows(ctx, f, rules, cur, first); err != nil {
		return out, err
	}
	return out, nil
}

// pending counts the titles of kind ("" both) the next pass checks.
func (s *Service) pending(ctx context.Context, kind, hash string) (int32, error) {
	covered, err := s.hasCoveredBy(ctx)
	if err != nil {
		return 0, err
	}
	var n int32
	err = s.pool.QueryRow(ctx, `SELECT count(*) FROM (`+eligible(covered)+`) e
		LEFT JOIN `+store.ComplianceTable+` c ON c.item_id = e.id
		WHERE `+dirty+` AND ($3 = '' OR e.kind = $3)`, hash, cutoff(s.now()), kind).Scan(&n)
	return n, err
}

// summary is, for every rule, how many of the titles of f's kind and search
// break it, the size of their packages, and what those are to be after
// their rework (nil when that is known of none).
func (s *Service) summary(ctx context.Context, f graph.ComplianceFilter) ([]graph.ComplianceSummary, error) {
	rows, err := s.pool.Query(ctx, filtered+`SELECT r, count(*), COALESCE(sum(f.packagebytes), 0)::float8, sum(f.estimatedbytes)::float8
		FROM f, unnest(f.rules) AS r GROUP BY r`, f.Kind, f.Search)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	by := map[string]graph.ComplianceSummary{}
	for rows.Next() {
		var c graph.ComplianceSummary
		if err := rows.Scan(&c.Rule, &c.Titles, &c.PackageBytes, &c.EstimatedBytes); err != nil {
			return nil, err
		}
		by[c.Rule] = c
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]graph.ComplianceSummary, 0, len(Rules))
	for _, r := range Rules {
		c, ok := by[r]
		if !ok {
			c = graph.ComplianceSummary{Rule: r}
		}
		out = append(out, c)
	}
	return out, nil
}

// rows is a page of the titles f matches that break a rule, at most first of
// them after cur, and the cursor of the next page (nil on the last).
func (s *Service) rows(ctx context.Context, f graph.ComplianceFilter, rules []string, cur *cursor, first int32) (
	[]graph.ComplianceRow, *string, error) {
	out := []graph.ComplianceRow{}
	if first == 0 {
		return out, nil, nil
	}
	var cp, co *int64
	var cid string
	if cur != nil {
		cp, co, cid = &cur.pkg, &cur.orig, cur.id
	}
	rows, err := s.pool.Query(ctx, filtered+`SELECT f.item_id, f.title, f.year, f.kind, f.series_id, f.series_title,
			f.seasonnumber, f.episodenumber, f.findings::text, f.originalbytes, f.packagebytes, f.estimatedbytes,
			f.evaluatedat, q.state, q.reason
		FROM f
		LEFT JOIN LATERAL (SELECT q.state, q.reason FROM `+store.ReencodeQueueTable+` q WHERE q.item_id = f.item_id
			ORDER BY q.seq DESC LIMIT 1) q ON true
		WHERE `+breaking+`
		  AND ($4::bigint IS NULL OR (COALESCE(f.packagebytes, -1), COALESCE(f.originalbytes, -1)) < ($4::bigint, $5::bigint)
		       OR ((COALESCE(f.packagebytes, -1), COALESCE(f.originalbytes, -1)) = ($4::bigint, $5::bigint) AND f.item_id > $6))
		ORDER BY `+order+`
		LIMIT $7`, f.Kind, f.Search, rules, cp, co, cid, first+1)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var r graph.ComplianceRow
		var findings string
		var orig, pkg, est *int64
		if err := rows.Scan(&r.ItemID, &r.Title, &r.Year, &r.Kind, &r.SeriesID, &r.SeriesTitle, &r.Season, &r.Episode,
			&findings, &orig, &pkg, &est, &r.EvaluatedAt, &r.Queue, &r.QueueReason); err != nil {
			return nil, nil, err
		}
		var fs []finding
		if err := json.Unmarshal([]byte(findings), &fs); err != nil {
			return nil, nil, fmt.Errorf("the findings of %s: %w", r.ItemID, err)
		}
		for _, x := range fs {
			r.Findings = append(r.Findings, graph.ComplianceFinding{Rule: x.Rule, Text: x.Text, Measured: x.Measured, Limit: x.Limit})
		}
		r.OriginalBytes, r.PackageBytes, r.EstimatedBytes = floatOf(orig), floatOf(pkg), floatOf(est)
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	var next *string
	if len(out) > int(first) {
		out = out[:first]
		last := out[len(out)-1]
		c := cursor{pkg: -1, orig: -1, id: last.ItemID}
		if last.PackageBytes != nil {
			c.pkg = int64(*last.PackageBytes)
		}
		if last.OriginalBytes != nil {
			c.orig = int64(*last.OriginalBytes)
		}
		s := c.String()
		next = &s
	}
	if err := s.blocked(ctx, out); err != nil {
		return nil, nil, err
	}
	return out, next, nil
}

// blocked says of each row why a rework cannot help it, as the re-encode
// queue skips it: its original deleted after packaging, no file to encode,
// its file a disc image.
func (s *Service) blocked(ctx context.Context, rows []graph.ComplianceRow) error {
	if len(rows) == 0 {
		return nil
	}
	ids := make([]string, len(rows))
	for i, r := range rows {
		ids[i] = r.ItemID
	}
	originals, err := library.OriginalsOf(ctx, s.pool, ids)
	if err != nil {
		return err
	}
	q, err := s.pool.Query(ctx, `SELECT x.id,
			EXISTS (SELECT 1 FROM com_nalet_katalog_playbackassets p WHERE p.item_id = x.id AND p.isprimary = true),
			`+processing.DiscImageOf("x.id")+`
		FROM unnest($1::text[]) AS x(id)`, ids)
	if err != nil {
		return err
	}
	defer q.Close()
	file, disc := map[string]bool{}, map[string]bool{}
	for q.Next() {
		var id string
		var f, d bool
		if err := q.Scan(&id, &f, &d); err != nil {
			return err
		}
		file[id], disc[id] = f, d
	}
	if err := q.Err(); err != nil {
		return err
	}
	for i := range rows {
		id := rows[i].ItemID
		switch o := originals[id]; {
		case o.Gone():
			rows[i].ReworkBlocked = strp(o.Why())
		case !file[id]:
			rows[i].ReworkBlocked = strp("no file to encode")
		case disc[id]:
			rows[i].ReworkBlocked = strp("its file is a " + processing.DiscImageReason)
		}
	}
	return nil
}

// Matching are the titles f matches that break a rule, in the report's
// order.
func (s *Service) Matching(ctx context.Context, f graph.ComplianceFilter) ([]string, error) {
	if why := s.ready(ctx); why != nil {
		return nil, why
	}
	rules, err := rulesOf(f)
	if err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, filtered+`SELECT f.item_id FROM f WHERE `+breaking+` ORDER BY `+order,
		f.Kind, f.Search, rules)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func floatOf(v *int64) *float64 {
	if v == nil {
		return nil
	}
	f := float64(*v)
	return &f
}

// capNames are the transcoder's caps by their key in its report, and what
// each is the cap of.
var capNames = []struct{ key, of string }{
	{"movie1080Mbps", "a 1080 movie"}, {"movie2160Mbps", "a 2160 movie"},
	{"episode1080Mbps", "a 1080 episode"}, {"episode2160Mbps", "a 2160 episode"},
}

// notEvaluated says of each rule the check does not hold the titles against
// why: a worker that never reported, a value its report does not give, a
// rule it switched off, the library's layout, records that cannot be read.
func notEvaluated(rs rulesState, unchecked int32, firstUnchecked *string) []string {
	out := []string{}
	if rs.transcoder == nil || rs.transcoder.t == nil {
		out = append(out, "the transcoder has not reported its rules, so the caps and the video codec are not checked")
	} else {
		t := rs.transcoder.t
		caps := map[string]*float64{"movie1080Mbps": t.Caps.Movie1080Mbps, "movie2160Mbps": t.Caps.Movie2160Mbps,
			"episode1080Mbps": t.Caps.Episode1080Mbps, "episode2160Mbps": t.Caps.Episode2160Mbps}
		if t.Bucket2160.MinHeight == nil || t.Bucket2160.MinWidth == nil {
			out = append(out, "the transcoder's rules give no 2160 bucket (bucket2160), so the bitrate caps are not checked")
		} else {
			for _, c := range capNames {
				switch v := caps[c.key]; {
				case v == nil:
					out = append(out, fmt.Sprintf("the transcoder's rules give no caps.%s, so the bitrate of %s is not checked", c.key, c.of))
				case *v == 0:
					out = append(out, fmt.Sprintf("the transcoder's bitrate cap of %s is off, so it is not checked", c.of))
				}
			}
		}
		switch v := t.Caps.MovieMaxGiB; {
		case v == nil:
			out = append(out, "the transcoder's rules give no caps.movieMaxGiB, so a movie's file size is not checked")
		case *v == 0:
			out = append(out, "the transcoder's file size cap of a movie is off, so it is not checked")
		}
		if ok, why := t.hevcRequired(); !ok {
			out = append(out, "the transcoder's rules do not make every package's video hevc ("+why+"), so the video codec is not checked")
		}
	}
	if rs.packager == nil || rs.packager.p == nil {
		out = append(out, "the packager has not reported its rules, so surround and image subtitles are not checked")
	} else {
		p := rs.packager.p
		switch {
		case p.SurroundAudio == nil:
			out = append(out, "the packager's rules give no surroundAudio, so surround is not checked")
		case !surroundOn(p):
			out = append(out, "the packager makes no 5.1 companion (surroundAudio "+strings.TrimSpace(*p.SurroundAudio)+
				"), so surround is not checked")
		}
		switch {
		case p.ImageSubtitles == nil:
			out = append(out, "the packager's rules give no imageSubtitles, so image subtitles are not checked")
		case len(p.ImageSubtitles) == 0:
			out = append(out, "the packager carries no image subtitles, so they are not checked")
		}
	}
	if !rs.v2 {
		out = append(out, "the library's layout is legacy, which keeps no record of a source or a package, "+
			"so the bitrate caps, surround and image subtitles are not checked")
	}
	if unchecked > 0 && firstUnchecked != nil {
		titles := fmt.Sprintf("%d titles", unchecked)
		if unchecked == 1 {
			titles = "1 title"
		}
		out = append(out, fmt.Sprintf("the records of %s could not be read (the first: %s), so their bitrate caps, "+
			"surround and image subtitles are not checked", titles, *firstUnchecked))
	}
	return out
}
