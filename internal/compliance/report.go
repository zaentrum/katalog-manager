package compliance

import (
	"context"
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
		c.blocked, c.resolution, c.videocodec, c.videobitrate, c.bitrateestimated, c.bitratecap, c.sizecap,
		c.durationseconds, c.packageaudio, c.originalaudio, c.ruleshash, c.evaluatedat,
		i.title, i.year, i.seasonnumber, i.episodenumber, sr.id AS series_id,
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

// order is the order of the titles a request by a filter queues: the
// largest package first, then the largest original, a title without either
// last, whatever the report is sorted by.
const order = `COALESCE(f.packagebytes, -1) DESC, COALESCE(f.originalbytes, -1) DESC, f.item_id`

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
// last found them, first of them after the cursor after, in the order o. It
// says what the workers reported, what is not checked and why, how many
// titles of f's kind and search the check has checked (CheckedTitles,
// whether they break a rule or not), how many f matches and their sizes,
// and those of them a request to process them again queues (queueable), of
// the titles of f's kind and search those that break each rule, whichever
// rules f names, and the re-encode queue by state.
func (s *Service) Report(ctx context.Context, f graph.ComplianceFilter, o graph.ComplianceOrder, first int32,
	after string) (graph.ComplianceReport, error) {
	out := graph.ComplianceReport{NotEvaluated: []string{}, Summary: []graph.ComplianceSummary{}, Rows: []graph.ComplianceRow{}}
	if why := s.ready(ctx); why != nil {
		return out, why
	}
	rules, err := rulesOf(f)
	if err != nil {
		return out, err
	}
	sorted, err := sortingOf(o)
	if err != nil {
		return out, err
	}
	cur, err := sorted.parse(after)
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
			count(*) FILTER (WHERE f.ruleshash <> ''),
			count(*) FILTER (WHERE `+breaking+`),
			COALESCE(sum(f.originalbytes) FILTER (WHERE `+breaking+`), 0)::float8,
			COALESCE(sum(f.packagebytes) FILTER (WHERE `+breaking+` AND f.estimatedbytes IS NOT NULL), 0)::float8,
			(sum(f.estimatedbytes) FILTER (WHERE `+breaking+`))::float8,
			count(*) FILTER (WHERE `+breaking+` AND f.estimatedbytes IS NULL),
			count(*) FILTER (WHERE f.unchecked IS NOT NULL)
		FROM f`, f.Kind, f.Search, rules).Scan(&out.EvaluatedAt, &out.CheckedTitles, &m.Titles, &m.OriginalBytes,
		&m.PackageBytes, &m.EstimatedBytes, &m.WithoutEstimate, &unchecked); err != nil {
		return out, err
	}
	out.Total = m.Titles
	out.NotEvaluated = notEvaluated(rs, unchecked)
	matched, err := s.matching(ctx, f, rules)
	if err != nil {
		return out, err
	}
	out.Queueable = totalsOf(matched.queueable)
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
	if out.Rows, out.NextCursor, err = s.rows(ctx, f, rules, sorted, cur, first); err != nil {
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
	since, _, err := s.nightlyCutoff(ctx)
	if err != nil {
		return 0, err
	}
	var n int32
	err = s.pool.QueryRow(ctx, `SELECT count(*) FROM (`+eligible(covered)+`) e
		LEFT JOIN `+store.ComplianceTable+` c ON c.item_id = e.id
		WHERE `+dirty+` AND ($3 = '' OR e.kind = $3)`, hash, since, kind).Scan(&n)
	return n, err
}

// summary is, for every rule, how many of the titles of f's kind and search
// break it, and of the same titles with an estimate, the size of their
// packages and the estimate of them once processed again (nil when none has
// one), and how many have none.
func (s *Service) summary(ctx context.Context, f graph.ComplianceFilter) ([]graph.ComplianceSummary, error) {
	rows, err := s.pool.Query(ctx, filtered+`SELECT r, count(*),
			COALESCE(sum(f.packagebytes) FILTER (WHERE f.estimatedbytes IS NOT NULL), 0)::float8, sum(f.estimatedbytes)::float8,
			count(*) FILTER (WHERE f.estimatedbytes IS NULL)
		FROM f, unnest(f.rules) AS r GROUP BY r`, f.Kind, f.Search)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	by := map[string]graph.ComplianceSummary{}
	for rows.Next() {
		var c graph.ComplianceSummary
		if err := rows.Scan(&c.Rule, &c.Titles, &c.PackageBytes, &c.EstimatedBytes, &c.WithoutEstimate); err != nil {
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

// rows is a page of the titles f matches that break a rule, in the order
// sorted, at most first of them after the cursor's (cur, its arguments; nil
// from the first), and the cursor of the next page (nil on the last).
func (s *Service) rows(ctx context.Context, f graph.ComplianceFilter, rules []string, sorted sorting, cur []any,
	first int32) ([]graph.ComplianceRow, *string, error) {
	out := []graph.ComplianceRow{}
	if first == 0 {
		return out, nil, nil
	}
	args := []any{f.Kind, f.Search, rules}
	where := breaking
	if cur != nil {
		where += " AND " + sorted.after(len(args)+1)
		args = append(args, cur...)
	}
	args = append(args, first+1)
	rows, err := s.pool.Query(ctx, filtered+`SELECT f.item_id, f.title, f.year, f.kind, f.series_id, f.series_title,
			f.seasonnumber, f.episodenumber, f.findings::text, f.originalbytes, f.packagebytes, f.estimatedbytes,
			f.evaluatedat, q.state, q.reason, f.blocked, f.resolution, f.videocodec, f.videobitrate, f.bitrateestimated,
			f.bitratecap, f.sizecap::float8, f.durationseconds, f.packageaudio, f.originalaudio, `+
		strings.Join(sorted.cols, ", ")+`
		FROM f
		LEFT JOIN LATERAL (SELECT q.state, q.reason FROM `+store.ReencodeQueueTable+` q WHERE q.item_id = f.item_id
			ORDER BY q.seq DESC LIMIT 1) q ON true
		WHERE `+where+`
		ORDER BY `+sorted.orderBy+`
		LIMIT $`+strconv.Itoa(len(args)), args...)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var kept []string // each row's blocked kind, as the pass kept it ("" for none)
	var lastCols []any
	for rows.Next() {
		var r graph.ComplianceRow
		var findings string
		var orig, pkg, est *int64
		var reason, blocked *string
		dest := []any{&r.ItemID, &r.Title, &r.Year, &r.Kind, &r.SeriesID, &r.SeriesTitle, &r.Season, &r.Episode,
			&findings, &orig, &pkg, &est, &r.EvaluatedAt, &r.Queue, &reason, &blocked, &r.Resolution, &r.VideoCodec,
			&r.VideoBitrate, &r.VideoBitrateEstimated, &r.BitrateCap, &r.SizeCap, &r.DurationSeconds, &r.PackageAudio,
			&r.OriginalAudio}
		cols := sorted.columns()
		if err := rows.Scan(append(dest, cols...)...); err != nil {
			return nil, nil, err
		}
		if len(out) < int(first) {
			lastCols = cols
		}
		k := ""
		if blocked != nil {
			k = *blocked
		}
		kept = append(kept, k)
		if r.Queue != nil {
			why := ""
			if reason != nil {
				why = *reason
			}
			r.QueueReason, r.QueueDetail = queueWords(*r.Queue, why)
		}
		var fs []finding
		if err := json.Unmarshal([]byte(findings), &fs); err != nil {
			return nil, nil, fmt.Errorf("the findings of %s: %w", r.ItemID, err)
		}
		for _, x := range fs {
			r.Findings = append(r.Findings, graph.ComplianceFinding{Rule: x.Rule, Text: x.Text, Measured: x.Measured, Limit: x.Limit,
				Detail: x.Detail})
		}
		r.OriginalBytes, r.PackageBytes, r.EstimatedBytes = floatOf(orig), floatOf(pkg), floatOf(est)
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	var next *string
	if len(out) > int(first) {
		out, kept = out[:first], kept[:first]
		c := sorted.cursorOf(sorted.values(lastCols))
		next = &c
	}
	if err := s.blocked(ctx, out, kept); err != nil {
		return nil, nil, err
	}
	return out, next, nil
}

// blocked says of each row why processing it again can't help, as the
// clause the console says after a colon: as the re-encode queue skips it
// (its original deleted, no file, a disc image), else as the pass found it
// of its original's record or its failed step (kept).
func (s *Service) blocked(ctx context.Context, rows []graph.ComplianceRow, kept []string) error {
	if len(rows) == 0 {
		return nil
	}
	ids := make([]string, len(rows))
	for i, r := range rows {
		ids[i] = r.ItemID
	}
	kinds, err := s.blockedOf(ctx, ids)
	if err != nil {
		return err
	}
	for i := range rows {
		k, ok := kinds[rows[i].ItemID]
		if !ok && kept[i] != "" {
			k, ok = kept[i], true
		}
		if ok {
			rows[i].ReworkBlocked = strp(skipReason(k))
		}
		rows[i].Outcome = outcomeOf(rows[i])
	}
	return nil
}

// The outcomes of processing a title again (ComplianceOutcome).
const (
	OutcomeReencode     = "REENCODE"
	OutcomeRepackage    = "REPACKAGE"
	OutcomeFirstPackage = "FIRST_PACKAGE"
	OutcomeBlocked      = "BLOCKED"
)

// outcomeOf is what processing a row's title again does to it, of the rules
// it breaks, as the console has said it: nothing, when processing it again
// can't help (BLOCKED); its first package, when it was never packaged
// (FIRST_PACKAGE); an encode of its video, over a cap of the transcoder's or
// not HEVC (REENCODE); its package made again, of its video as it is, when it
// lacks a track (REPACKAGE). A title whose packaging failed is packaged
// again when it has a package, else for the first time. Nil for rules the
// outcomes don't name.
func outcomeOf(r graph.ComplianceRow) *string {
	rules := map[string]bool{}
	for _, f := range r.Findings {
		rules[f.Rule] = true
	}
	packaged := r.PackageBytes != nil && *r.PackageBytes > 0
	switch {
	case r.ReworkBlocked != nil:
		return strp(OutcomeBlocked)
	case rules[NotPackaged]:
		return strp(OutcomeFirstPackage)
	case rules[OverBitrateCap] || rules[OverSizeCap] || rules[VideoNotHEVC]:
		return strp(OutcomeReencode)
	case rules[PackagingFailed] && packaged:
		return strp(OutcomeRepackage)
	case rules[PackagingFailed]:
		return strp(OutcomeFirstPackage)
	case rules[NoSurround] || rules[ImageSubtitlesMissing]:
		return strp(OutcomeRepackage)
	}
	return nil
}

// blockedOf says of each title of ids that processing it again can't help
// why, a kind of skip (graph.Skip*), as the re-encode queue tells
// (retry's unencodable): its original being deleted after packaging, or
// deleted, no file to encode, its file a disc image.
func (s *Service) blockedOf(ctx context.Context, ids []string) (map[string]string, error) {
	out := map[string]string{}
	if len(ids) == 0 {
		return out, nil
	}
	originals, err := library.OriginalsOf(ctx, s.pool, ids)
	if err != nil {
		return nil, err
	}
	q, err := s.pool.Query(ctx, `SELECT x.id,
			EXISTS (SELECT 1 FROM com_nalet_katalog_playbackassets p WHERE p.item_id = x.id AND p.isprimary = true),
			`+processing.DiscImageOf("x.id")+`
		FROM unnest($1::text[]) AS x(id)`, ids)
	if err != nil {
		return nil, err
	}
	defer q.Close()
	for q.Next() {
		var id string
		var file, disc bool
		if err := q.Scan(&id, &file, &disc); err != nil {
			return nil, err
		}
		switch o := originals[id]; {
		case o.State == library.OriginalRetiring:
			out[id] = graph.SkipDeleting
		case o.Gone():
			out[id] = graph.SkipDeleted
		case !file:
			out[id] = graph.SkipNoFile
		case disc:
			out[id] = graph.SkipDiscImage
		}
	}
	return out, q.Err()
}

// Blocked says of each title of ids that processing it again can't help, as
// the check last found it of its original's record or its failed step
// (blockedBy, a row's reworkBlocked), of which kind: an original without
// video (graph.SkipNoVideo), or one of a Dolby Vision profile the transcoder
// leaves as it is (graph.SkipDolbyVision and the profile). The re-encode
// queue skips them (retry.Compliance). None without migration 051.
func (s *Service) Blocked(ctx context.Context, ids []string) (map[string]string, error) {
	out := map[string]string{}
	if len(ids) == 0 || s.ready(ctx) != nil {
		return out, nil
	}
	rows, err := s.pool.Query(ctx, `SELECT item_id, blocked FROM `+store.ComplianceTable+`
		WHERE item_id = ANY($1) AND blocked IS NOT NULL`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, kind string
		if err := rows.Scan(&id, &kind); err != nil {
			return nil, err
		}
		out[id] = kind
	}
	return out, rows.Err()
}

// Queued says what a request to process titles again did, as the queue
// answered it (res): how many it queued and how many were queued already,
// each title it skipped, named, with why, in the reader's words, and all of
// it as the console says it (requestTexts) and in one text; matching says
// the request named its titles by a filter.
func (s *Service) Queued(ctx context.Context, res graph.ReencodeEnqueued, matching bool) (graph.ComplianceQueued, error) {
	out := graph.ComplianceQueued{Queued: res.Queued, AlreadyQueued: res.AlreadyQueued, Skipped: int32(len(res.Skipped)),
		SkippedTitles: []graph.ComplianceSkip{}}
	kinds := map[string]int{}
	if len(res.Skipped) > 0 {
		ids := make([]string, len(res.Skipped))
		for i, sk := range res.Skipped {
			ids[i] = sk.ItemID
		}
		names, err := s.titlesOf(ctx, ids)
		if err != nil {
			return out, err
		}
		for _, sk := range res.Skipped {
			kind := sk.Kind
			if _, ok := skipClauses[kind]; !ok && !strings.HasPrefix(kind, blockedDolbyVision) {
				kind = skipOther
			}
			kinds[kind]++
			name, ok := names[sk.ItemID]
			if !ok {
				name = sk.ItemID
			}
			out.SkippedTitles = append(out.SkippedTitles, graph.ComplianceSkip{ItemID: sk.ItemID, Title: name,
				Reason: skipReason(sk.Kind)})
		}
	}
	out.QueuedText, out.AlreadyText, out.NotQueuedText = requestTexts(int(res.Queued), int(res.AlreadyQueued), kinds, matching)
	out.Message = requestMessage(out.QueuedText, out.AlreadyText, out.NotQueuedText)
	return out, nil
}

// titlesOf names each title of ids the catalog holds as a list of them does
// (titleText); one it doesn't hold has no name.
func (s *Service) titlesOf(ctx context.Context, ids []string) (map[string]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT x.id, lower(i.type), i.title, i.year, i.seasonnumber, i.episodenumber,
			CASE WHEN lower(p.type) = 'series' THEN p.title WHEN lower(g.type) = 'series' THEN g.title END
		FROM unnest($1::text[]) AS x(id)
		JOIN com_nalet_katalog_items i ON i.id = x.id
		LEFT JOIN com_nalet_katalog_items p ON p.id = i.parent_id
		LEFT JOIN com_nalet_katalog_items g ON g.id = p.parent_id`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id string
		var typ, title, series *string
		var year, season, episode *int32
		if err := rows.Scan(&id, &typ, &title, &year, &season, &episode, &series); err != nil {
			return nil, err
		}
		out[id] = titleText(id, typ, title, series, year, season, episode)
	}
	return out, rows.Err()
}

// sized is a title a report matches, with its sizes: its original's, its
// package's, and its package's once processed again.
type sized struct {
	id             string
	orig, pkg, est *int64
}

// matched are the titles a filter matches that break a rule, as a request
// by the filter takes them (graph.ComplianceMatching).
type matched struct {
	queueable []sized
	queued    int32
}

// matching are the titles f matches that break a rule (any of rules), in the
// report's order, as a request to process them again by the filter takes
// them: those processing again can't help are left out, as the queue would
// skip them (their original deleted after packaging or being deleted, no
// file, a disc image) or as the pass found the transcoder would refuse them
// again (blockedBy); those waiting or sent in the re-encode queue are there
// already, as the queue keeps them; the others are queueable, what a
// request queues.
func (s *Service) matching(ctx context.Context, f graph.ComplianceFilter, rules []string) (matched, error) {
	var out matched
	rows, err := s.pool.Query(ctx, filtered+`SELECT f.item_id, f.originalbytes, f.packagebytes, f.estimatedbytes, f.blocked,
			EXISTS (SELECT 1 FROM `+store.ReencodeQueueTable+` q WHERE q.item_id = f.item_id AND q.state IN ('queued', 'sent'))
		FROM f WHERE `+breaking+` ORDER BY `+order, f.Kind, f.Search, rules)
	if err != nil {
		return out, err
	}
	type candidate struct {
		sized
		kept   *string
		queued bool
	}
	var titles []candidate
	for rows.Next() {
		var t candidate
		if err := rows.Scan(&t.id, &t.orig, &t.pkg, &t.est, &t.kept, &t.queued); err != nil {
			rows.Close()
			return out, err
		}
		titles = append(titles, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return out, err
	}
	ids := make([]string, len(titles))
	for i, t := range titles {
		ids[i] = t.id
	}
	kinds, err := s.blockedOf(ctx, ids)
	if err != nil {
		return out, err
	}
	for _, t := range titles {
		_, blocked := kinds[t.id]
		switch {
		case blocked || t.kept != nil:
			// left out: the console never offers it
		case t.queued:
			out.queued++
		default:
			out.queueable = append(out.queueable, t.sized)
		}
	}
	return out, nil
}

// totalsOf sums titles up: how many, and the sizes of their originals; and
// of the same titles with an estimate, the sizes of their packages and the
// estimate of them once processed again (nil when none has one), and how
// many have none.
func totalsOf(titles []sized) graph.ComplianceTotals {
	out := graph.ComplianceTotals{Titles: int32(len(titles))}
	var est int64
	known := false
	for _, t := range titles {
		if t.orig != nil {
			out.OriginalBytes += float64(*t.orig)
		}
		if t.est == nil {
			out.WithoutEstimate++
			continue
		}
		if t.pkg != nil {
			out.PackageBytes += float64(*t.pkg)
		}
		est += *t.est
		known = true
	}
	if known {
		e := float64(est)
		out.EstimatedBytes = &e
	}
	return out
}

// Matching are the titles f matches that break a rule, as a request by the
// filter takes them (matching): those it queues, in the report's order, and
// how many of the others are in the re-encode queue already.
func (s *Service) Matching(ctx context.Context, f graph.ComplianceFilter) (graph.ComplianceMatching, error) {
	out := graph.ComplianceMatching{Queueable: []string{}}
	if why := s.ready(ctx); why != nil {
		return out, why
	}
	rules, err := rulesOf(f)
	if err != nil {
		return out, err
	}
	m, err := s.matching(ctx, f, rules)
	if err != nil {
		return out, err
	}
	for _, t := range m.queueable {
		out.Queueable = append(out.Queueable, t.id)
	}
	out.AlreadyQueued = m.queued
	return out, nil
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
	{"movie1080Mbps", "a 1080p movie"}, {"movie2160Mbps", "a 2160p movie"},
	{"episode1080Mbps", "a 1080p episode"}, {"episode2160Mbps", "a 2160p episode"},
}

// notEvaluated says of each rule the check does not hold the titles against
// why, in a sentence: a worker that never reported, a value its report does
// not give, a limit it does not set, the library's layout, records that
// cannot be read.
func notEvaluated(rs rulesState, unchecked int32) []string {
	out := []string{}
	if rs.transcoder == nil || rs.transcoder.t == nil {
		out = append(out, "The transcoder hasn’t reported its rules, so bit rates, file sizes and the video format can’t be checked.")
	} else {
		t := rs.transcoder.t
		caps := map[string]*float64{"movie1080Mbps": t.Caps.Movie1080Mbps, "movie2160Mbps": t.Caps.Movie2160Mbps,
			"episode1080Mbps": t.Caps.Episode1080Mbps, "episode2160Mbps": t.Caps.Episode2160Mbps}
		if t.Bucket2160.MinHeight == nil || t.Bucket2160.MinWidth == nil {
			out = append(out, "The transcoder doesn’t report where 2160p begins, so bit rates can’t be checked.")
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
		out = append(out, "The library uses the older layout, which keeps no records of originals and packages, "+
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
