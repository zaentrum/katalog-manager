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
// first. It says what the workers reported, what is not checked and why, how
// many titles f matches and their sizes, of the titles of f's kind and
// search those that break each rule, whichever rules f names, and the
// re-encode queue by state.
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
	m := &out.Matching
	if err := s.pool.QueryRow(ctx, filtered+`SELECT min(f.evaluatedat) FILTER (WHERE f.ruleshash <> ''),
			count(*) FILTER (WHERE `+breaking+`),
			COALESCE(sum(f.originalbytes) FILTER (WHERE `+breaking+`), 0)::float8,
			COALESCE(sum(f.packagebytes) FILTER (WHERE `+breaking+`), 0)::float8,
			(sum(f.estimatedbytes) FILTER (WHERE `+breaking+`))::float8,
			count(*) FILTER (WHERE f.unchecked IS NOT NULL)
		FROM f`, f.Kind, f.Search, rules).Scan(&out.EvaluatedAt, &m.Titles, &m.OriginalBytes, &m.PackageBytes,
		&m.EstimatedBytes, &unchecked); err != nil {
		return out, err
	}
	out.Total = m.Titles
	out.NotEvaluated = notEvaluated(rs, unchecked)
	q := &out.Queue
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE state = 'queued'), count(*) FILTER (WHERE state = 'sent'),
			count(*) FILTER (WHERE state = 'done'), count(*) FILTER (WHERE state = 'failed')
		FROM `+store.ReencodeQueueTable).Scan(&q.Queued, &q.Processing, &q.Processed, &q.Failed); err != nil {
		return out, err
	}
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
// break it, the size of their packages, and what those are to be at most
// once processed again (nil when that is known of none).
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
		var reason *string
		if err := rows.Scan(&r.ItemID, &r.Title, &r.Year, &r.Kind, &r.SeriesID, &r.SeriesTitle, &r.Season, &r.Episode,
			&findings, &orig, &pkg, &est, &r.EvaluatedAt, &r.Queue, &reason); err != nil {
			return nil, nil, err
		}
		if r.Queue != nil && reason != nil {
			r.QueueReason = queueWords(*r.Queue, *reason)
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

// blocked says of each row why processing it again cannot help, as the re-encode
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
		case o.State == library.OriginalRetiring:
			rows[i].ReworkBlocked = strp(deletingText)
		case o.Gone():
			rows[i].ReworkBlocked = strp(deletedText)
		case !file[id]:
			rows[i].ReworkBlocked = strp(noFileText)
		case disc[id]:
			rows[i].ReworkBlocked = strp(discText)
		}
	}
	return nil
}

// Queued says what a request to process titles again did, as the queue
// answered it (res), in words: how many it queued, how many were queued
// already, and why it skipped any, as the catalog holds each now; matching
// says the request named its titles by a filter.
func (s *Service) Queued(ctx context.Context, res graph.ReencodeEnqueued, matching bool) (graph.ComplianceQueued, error) {
	out := graph.ComplianceQueued{Queued: res.Queued, AlreadyQueued: res.AlreadyQueued, Skipped: int32(len(res.Skipped))}
	kinds := map[string]int{}
	if len(res.Skipped) > 0 {
		if why := s.ready(ctx); why != nil {
			return out, why
		}
		ids := make([]string, len(res.Skipped))
		for i, sk := range res.Skipped {
			ids[i] = sk.ItemID
		}
		of, err := s.skipKinds(ctx, ids)
		if err != nil {
			return out, err
		}
		for _, id := range ids {
			kinds[of[id]]++
		}
	}
	out.Message = queuedWords(int(res.Queued), int(res.AlreadyQueued), kinds, matching)
	return out, nil
}

// skipKinds says of each title of ids why the queue skips it, as the queue
// tells (retry's unencodable and EnqueueReencode): not in the catalog, a
// series without episodes, no movie, episode or series, its original deleted
// after packaging or being deleted, no file, a disc image.
func (s *Service) skipKinds(ctx context.Context, ids []string) (map[string]string, error) {
	originals, err := library.OriginalsOf(ctx, s.pool, ids)
	if err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `SELECT x.id, lower(i.type),
			EXISTS (SELECT 1 FROM com_nalet_katalog_playbackassets p WHERE p.item_id = x.id AND p.isprimary = true),
			`+processing.DiscImageOf("x.id")+`
		FROM unnest($1::text[]) AS x(id) LEFT JOIN com_nalet_katalog_items i ON i.id = x.id`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id string
		var typ *string
		var file, disc bool
		if err := rows.Scan(&id, &typ, &file, &disc); err != nil {
			return nil, err
		}
		o := originals[id]
		switch {
		case typ == nil:
			out[id] = skipUnknown
		case *typ == "series":
			out[id] = skipNoEpisodes
		case *typ != "movie" && *typ != "episode":
			out[id] = skipOther
		case o.State == library.OriginalRetiring:
			out[id] = skipDeleting
		case o.Gone():
			out[id] = skipDeleted
		case disc:
			out[id] = skipDisc
		case !file:
			out[id] = skipNoFile
		default:
			out[id] = skipOther
		}
	}
	return out, rows.Err()
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
// each is the limit for.
var capNames = []struct{ key, of string }{
	{"movie1080Mbps", "a 1080 movie"}, {"movie2160Mbps", "a 2160 movie"},
	{"episode1080Mbps", "a 1080 episode"}, {"episode2160Mbps", "a 2160 episode"},
}

// notEvaluated says of each rule the check does not hold the titles against
// why, in a sentence: a worker that never reported, a value its report does
// not give, a limit it does not set, the library's layout, records that
// cannot be read.
func notEvaluated(rs rulesState, unchecked int32) []string {
	out := []string{}
	if rs.transcoder == nil || rs.transcoder.t == nil {
		out = append(out, "The transcoder hasn’t reported its limits, so bit rates, file sizes and the video format can’t be checked.")
	} else {
		t := rs.transcoder.t
		caps := map[string]*float64{"movie1080Mbps": t.Caps.Movie1080Mbps, "movie2160Mbps": t.Caps.Movie2160Mbps,
			"episode1080Mbps": t.Caps.Episode1080Mbps, "episode2160Mbps": t.Caps.Episode2160Mbps}
		if t.Bucket2160.MinHeight == nil || t.Bucket2160.MinWidth == nil {
			out = append(out, "The transcoder doesn’t report where 2160 begins, so bit rates can’t be checked.")
		} else {
			for _, c := range capNames {
				switch v := caps[c.key]; {
				case v == nil:
					out = append(out, fmt.Sprintf("The transcoder doesn’t report the bit rate it allows for %s, so it can’t be checked.", c.of))
				case *v == 0:
					out = append(out, fmt.Sprintf("The transcoder allows any bit rate for %s.", c.of))
				}
			}
		}
		switch v := t.Caps.MovieMaxGiB; {
		case v == nil:
			out = append(out, "The transcoder doesn’t report the file size it allows for a movie, so it can’t be checked.")
		case *v == 0:
			out = append(out, "The transcoder allows movie files of any size.")
		}
		if ok, why := t.hevcRequired(); !ok {
			out = append(out, why)
		}
	}
	if rs.packager == nil || rs.packager.p == nil {
		out = append(out, "The packager hasn’t reported what it keeps, so surround and image subtitles can’t be checked.")
	} else {
		p := rs.packager.p
		switch {
		case p.SurroundAudio == nil:
			out = append(out, "The packager doesn’t report its surround track, so surround can’t be checked.")
		case !surroundOn(p):
			out = append(out, "The packager makes no surround track, so a package without one breaks no rule.")
		}
		switch {
		case p.ImageSubtitles == nil:
			out = append(out, "The packager doesn’t report which image subtitles it keeps, so they can’t be checked.")
		case len(p.ImageSubtitles) == 0:
			out = append(out, "The packager keeps no image subtitles, so a package without them breaks no rule.")
		}
	}
	if !rs.v2 {
		out = append(out, "The library uses the legacy layout, which keeps no records of sources and packages, "+
			"so bit rates, surround and image subtitles can’t be checked.")
	}
	switch {
	case unchecked == 1:
		out = append(out, "The records of 1 title can’t be read, so its bit rate, surround and image subtitles can’t be checked.")
	case unchecked > 1:
		out = append(out, fmt.Sprintf("The records of %s can’t be read, so their bit rates, surround and image subtitles "+
			"can’t be checked.", titlesText(int(unchecked))))
	}
	return out
}
