package compliance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/zaentrum/katalog-manager/internal/library"
	"github.com/zaentrum/katalog-manager/internal/store"
)

// eligible is the SQL of the titles the check holds against the rules: the
// movies and the episodes with a file or a package, but an episode another's
// file covers (its holder's file is checked for it). covered says the
// catalog has migration 045's column.
func eligible(covered bool) string {
	sql := `SELECT i.id, lower(i.type) AS kind FROM com_nalet_katalog_items i
		WHERE lower(i.type) IN ('movie', 'episode')
		  AND (EXISTS (SELECT 1 FROM com_nalet_katalog_playbackassets a
		               WHERE a.item_id = i.id AND (a.isprimary = true OR a.kind = 'packaged'))
		       OR EXISTS (SELECT 1 FROM ` + store.VersionsTable + ` v WHERE v.item_id = i.id AND v.state = 'complete'))`
	if covered {
		sql += ` AND i.coveredby IS NULL`
	}
	return sql
}

// dirty is the SQL condition that the title of the eligible row e, with the
// check's row c, is to be checked: never checked, marked to be checked again,
// checked against other rules than $1, or before the nightly pass's last
// moment, $2.
const dirty = `(c.item_id IS NULL OR c.staleat IS NOT NULL OR c.ruleshash <> $1 OR c.evaluatedat < $2)`

// hasCoveredBy reports whether the catalog's items have migration 045's
// coveredby.
func (s *Service) hasCoveredBy(ctx context.Context) (bool, error) {
	var ok bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_attribute WHERE attrelid = to_regclass('com_nalet_katalog_items')
		AND attname = 'coveredby' AND attnum > 0 AND NOT attisdropped)`).Scan(&ok)
	return ok, err
}

// state is what the titles are held against now: the workers' rules and the
// library's layout.
func (s *Service) state(ctx context.Context) (rulesState, error) {
	rs, err := s.loadRules(ctx)
	if err != nil {
		return rs, err
	}
	set, err := library.ReadSettings(ctx, s.pool)
	if err != nil {
		return rs, fmt.Errorf("the library's settings: %w", err)
	}
	rs.v2 = set.V2()
	rs.retries = s.retries
	return rs, nil
}

// Pass checks every title that needs it now, a batch at a time, in one
// instance at a time, and answers how many it checked. A title is checked
// once a pass: one marked again while the pass runs waits for the next.
func (s *Service) Pass(ctx context.Context) (int, error) {
	if why := s.ready(ctx); why != nil {
		return 0, why
	}
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return 0, err
	}
	defer conn.Release()
	var locked bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock(`+passLock+`)`).Scan(&locked); err != nil {
		return 0, err
	}
	if !locked {
		return 0, nil // another instance checks now
	}
	defer func() {
		_, _ = conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock(`+passLock+`)`)
	}()
	s.running.Store(true)
	defer s.running.Store(false)

	rs, err := s.state(ctx)
	if err != nil {
		return 0, err
	}
	covered, err := s.hasCoveredBy(ctx)
	if err != nil {
		return 0, err
	}
	hash, since := rs.hash(), cutoff(s.now())
	seen := map[string]bool{}
	checked := 0
	var bad unread
	defer bad.say()
	for ctx.Err() == nil {
		ids, err := s.due(ctx, covered, hash, since)
		if err != nil {
			return checked, err
		}
		var fresh []string
		for _, id := range ids {
			if !seen[id] {
				seen[id] = true
				fresh = append(fresh, id)
			}
		}
		if len(fresh) == 0 {
			break
		}
		if err := s.check(ctx, fresh, rs, hash, &bad); err != nil {
			return checked, err
		}
		checked += len(fresh)
	}
	if checked > 0 {
		if err := s.prune(ctx, covered); err != nil {
			return checked, err
		}
	}
	return checked, ctx.Err()
}

// due are the titles to check next, at most a batch: those never checked
// first.
func (s *Service) due(ctx context.Context, covered bool, hash string, since time.Time) ([]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT e.id FROM (`+eligible(covered)+`) e
		LEFT JOIN `+store.ComplianceTable+` c ON c.item_id = e.id
		WHERE `+dirty+`
		ORDER BY c.item_id IS NOT NULL, e.id
		LIMIT $3`, hash, since, batch)
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

// prune deletes the rows of the titles the check holds against the rules no
// more: an item that is no movie or episode now, has neither file nor
// package, or is covered by another's file.
func (s *Service) prune(ctx context.Context, covered bool) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM `+store.ComplianceTable+` c
		WHERE NOT EXISTS (SELECT 1 FROM (`+eligible(covered)+`) e WHERE e.id = c.item_id)`)
	return err
}

// unread are the titles of a pass whose records can't be read: how many,
// and the first of them, and why. The report says how many; the log says
// which, for whoever looks after the library.
type unread struct {
	n          int
	first, why string
}

func (u *unread) add(results []result) {
	for _, r := range results {
		if r.Unchecked == "" {
			continue
		}
		if u.n == 0 {
			u.first, u.why = r.ItemID, r.Unchecked
		}
		u.n++
	}
}

func (u *unread) say() {
	if u.n > 0 {
		log.Printf("compliance: %s whose records can't be read, their bit rates, surround and image subtitles "+
			"not checked; the first, %s: %s", titlesText(u.n), u.first, u.why)
	}
}

// check checks the titles ids against rs and keeps what it found, under
// hash; bad counts those whose records can't be read.
func (s *Service) check(ctx context.Context, ids []string, rs rulesState, hash string, bad *unread) error {
	var start time.Time
	if err := s.pool.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&start); err != nil {
		return err
	}
	titles, err := s.titles(ctx, ids)
	if err != nil {
		return err
	}
	results := make([]result, len(titles))
	sem := make(chan struct{}, readers)
	var wg sync.WaitGroup
	for i := range titles {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			var r records
			if rs.v2 && titles[i].Version != nil {
				sem <- struct{}{}
				r = s.readRecords(titles[i])
				<-sem
			}
			results[i] = evaluate(titles[i], r, rs)
		}(i)
	}
	wg.Wait()
	bad.add(results)
	return s.write(ctx, results, hash, start)
}

// titleCols is what check reads of a title: its type and series, its file
// with its source, its complete version with the size of its first source,
// whether a version was taken in, its packaged row and its transcode and
// package steps.
const titleCols = `SELECT i.id, lower(i.type),
		COALESCE(CASE WHEN lower(p.type) = 'series' THEN p.id END, CASE WHEN lower(g.type) = 'series' THEN g.id END, ''),
		i.durationms,
		pa.path, pa.sizebytes, ps.sizebytes, pa.resolution, pa.durationms,
		v.id, COALESCE(v.sourceids, '{}'), vs.sizebytes,
		EXISTS (SELECT 1 FROM ` + store.VersionsTable + ` tv WHERE tv.item_id = i.id AND tv.state = 'taken'),
		pk.codec, pk.resolution, pk.sizebytes, pk.durationms,
		t.status, COALESCE(t.error, t.lasterror), t.details, t.nextretryat,
		k.status, COALESCE(k.error, k.lasterror), k.details, k.nextretryat
	FROM unnest($1::text[]) AS x(id)
	JOIN com_nalet_katalog_items i ON i.id = x.id
	LEFT JOIN com_nalet_katalog_items p ON p.id = i.parent_id
	LEFT JOIN com_nalet_katalog_items g ON g.id = p.parent_id
	LEFT JOIN LATERAL (SELECT a.path, a.sizebytes, a.resolution, a.durationms, a.sourceid FROM com_nalet_katalog_playbackassets a
		WHERE a.item_id = i.id AND a.isprimary = true ORDER BY a.id LIMIT 1) pa ON true
	LEFT JOIN ` + store.SourcesTable + ` ps ON ps.id = pa.sourceid
	LEFT JOIN ` + store.VersionsTable + ` v ON v.item_id = i.id AND v.state = 'complete'
	LEFT JOIN ` + store.SourcesTable + ` vs ON vs.id = v.sourceids[1]
	LEFT JOIN LATERAL (SELECT a.codec, a.resolution, a.sizebytes, a.durationms FROM com_nalet_katalog_playbackassets a
		WHERE a.item_id = i.id AND a.kind = 'packaged' ORDER BY a.id LIMIT 1) pk ON true
	LEFT JOIN com_nalet_katalog_itemprocessingsteps t ON t.item_id = i.id AND t.step = 'transcode'
	LEFT JOIN com_nalet_katalog_itemprocessingsteps k ON k.item_id = i.id AND k.step = 'package'`

// titles reads what the catalog holds of the titles ids for the check, and
// what each has of its original.
func (s *Service) titles(ctx context.Context, ids []string) ([]title, error) {
	rows, err := s.pool.Query(ctx, titleCols, ids)
	if err != nil {
		return nil, err
	}
	var out []title
	for rows.Next() {
		var t title
		var pkgCodec, pkgRes *string
		if err := rows.Scan(&t.ID, &t.Kind, &t.SeriesID, &t.ItemDurMs,
			&t.Path, &t.FileBytes, &t.SourceBytes, &t.FileResolution, &t.FileDurationMs,
			&t.Version, &t.VersionSources, &t.VersionBytes, &t.Taken,
			&pkgCodec, &pkgRes, &t.PkgBytes, &t.PkgDurationMs,
			&t.Transcode.Status, &t.Transcode.Error, &t.Transcode.Details, &t.Transcode.NextRetryAt,
			&t.Package.Status, &t.Package.Error, &t.Package.Details, &t.Package.NextRetryAt); err != nil {
			rows.Close()
			return nil, err
		}
		t.PkgCodec, t.PkgResolution = pkgCodec, pkgRes
		out = append(out, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	originals, err := library.OriginalsOf(ctx, s.pool, ids)
	if err != nil {
		return nil, err
	}
	languages, err := s.audioLanguages(ctx, ids)
	if err != nil {
		return nil, err
	}
	for i := range out {
		out[i].Original = originals[out[i].ID]
		out[i].AudioLanguages = languages[out[i].ID]
	}
	return out, nil
}

// audioLanguages are the languages an admin set for the audio tracks of the
// titles ids, by title and by the track's place among the original's audio
// streams (migration 037); none without it.
func (s *Service) audioLanguages(ctx context.Context, ids []string) (map[string]map[int]string, error) {
	out := map[string]map[int]string{}
	if !s.tracks.Load() {
		ok, err := s.st.TrackLanguagesReady(ctx)
		if err != nil || !ok {
			return out, err
		}
		s.tracks.Store(true)
	}
	rows, err := s.pool.Query(ctx, `SELECT item_id, ordinal, language FROM com_nalet_katalog_itemtracklanguages
		WHERE item_id = ANY($1) AND kind = 'audio'`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, language string
		var ordinal int
		if err := rows.Scan(&id, &ordinal, &language); err != nil {
			return nil, err
		}
		if out[id] == nil {
			out[id] = map[int]string{}
		}
		out[id][ordinal] = language
	}
	return out, rows.Err()
}

// readRecords reads the records of a title's complete version: its
// package.json and the source.json of the source it was made of, each a
// small JSON file of the library; never a media file. What it cannot read
// says why in unchecked, the rules that read it not checked.
func (s *Service) readRecords(t title) records {
	var r records
	if !library.ValidID(t.ID) || !library.ValidID(*t.Version) || (t.SeriesID != "" && !library.ValidID(t.SeriesID)) {
		r.unchecked = "its records are not read: the library names a folder by a lower-case UUID"
		return r
	}
	if t.Kind == "episode" && t.SeriesID == "" {
		r.unchecked = "its records are not read: an episode's lie in its series' folder, and it has no series"
		return r
	}
	dir := s.paths.ItemDir(library.Place{ID: t.ID, Type: t.Kind, SeriesID: t.SeriesID})
	rel := "versions/" + *t.Version + "/" + library.PackageFile
	b, err := os.ReadFile(filepath.Join(library.VersionDir(dir, *t.Version), library.PackageFile))
	if err != nil {
		r.unchecked = unreadable(rel, err)
		return r
	}
	var pkg packageRecord
	if err := json.Unmarshal(b, &pkg); err != nil {
		r.unchecked = rel + " is no package record: " + err.Error()
		return r
	}
	r.pkg = &pkg
	if r.pkgEssence, err = library.EssenceOf(b); err != nil {
		r.pkgEssence = nil
	}
	if len(t.VersionSources) == 0 || !library.ValidID(t.VersionSources[0]) {
		r.unchecked = "its version names no source whose record could be read"
		return r
	}
	rel = "sources/" + t.VersionSources[0] + "/source.json"
	if b, err = os.ReadFile(filepath.Join(library.SourceDir(dir, t.VersionSources[0]), "source.json")); err != nil {
		r.unchecked = unreadable(rel, err)
		return r
	}
	var src sourceRecord
	if err := json.Unmarshal(b, &src); err != nil {
		r.unchecked = rel + " is no source record: " + err.Error()
		return r
	}
	r.src = &src
	if r.srcEssence, err = library.EssenceOf(b); err != nil {
		r.srcEssence = nil
	}
	return r
}

// unreadable says why the record rel of a title cannot be read.
func unreadable(rel string, err error) string {
	var pe *fs.PathError
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return rel + " is not there"
	case errors.As(err, &pe):
		return rel + " cannot be read: " + pe.Err.Error()
	}
	return rel + " cannot be read: " + err.Error()
}

// write keeps what the check found of each title, checked against hash from
// start on: a title marked to be checked again since start stays marked.
func (s *Service) write(ctx context.Context, results []result, hash string, start time.Time) error {
	if len(results) == 0 {
		return nil
	}
	n := len(results)
	ids, kinds, rules, findings, unchecked := make([]string, n), make([]string, n), make([]string, n), make([]string, n), make([]*string, n)
	orig, pkg, est := make([]*int64, n), make([]*int64, n), make([]*int64, n)
	blocked := make([]*string, n)
	res, codec, pkgAudio, srcAudio := make([]*string, n), make([]*string, n), make([]*string, n), make([]*string, n)
	rate, rateCap, dur := make([]*float64, n), make([]*float64, n), make([]*float64, n)
	estimated, sizeCap := make([]bool, n), make([]*int64, n)
	for i, r := range results {
		if r.Blocked != "" {
			blocked[i] = strp(r.Blocked)
		}
		m := r.Measures
		res[i], codec[i], pkgAudio[i], srcAudio[i] = m.Resolution, m.VideoCodec, m.PackageAudio, m.OriginalAudio
		rate[i], rateCap[i], dur[i], estimated[i], sizeCap[i] = m.VideoBitrate, m.BitrateCap, m.DurationSeconds,
			m.BitrateEstimated, m.SizeCap
		f := r.Findings
		if f == nil {
			f = []finding{}
		}
		b, err := json.Marshal(f)
		if err != nil {
			return err
		}
		ids[i], kinds[i], rules[i], findings[i] = r.ItemID, r.Kind, strings.Join(r.rules(), ","), string(b)
		orig[i], pkg[i], est[i] = r.OriginalBytes, r.PackageBytes, r.EstimatedBytes
		if r.Unchecked != "" {
			u := []rune(r.Unchecked)
			if len(u) > 500 {
				u = u[:500]
			}
			s := string(u)
			unchecked[i] = &s
		}
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO `+store.ComplianceTable+` AS c
			(item_id, kind, rules, findings, originalbytes, packagebytes, estimatedbytes, unchecked, blocked,
			 resolution, videocodec, videobitrate, bitrateestimated, bitratecap, sizecap, durationseconds, packageaudio,
			 originalaudio, ruleshash, staleat, evaluatedat)
		SELECT u.id, u.kind, string_to_array(u.rules, ',')::varchar[], u.findings::jsonb, u.ob, u.pb, u.eb, u.un, u.bl,
			u.res, u.codec, u.rate, u.est, u.rcap, u.scap, u.dur, u.pa, u.oa, $9, NULL, now()
		FROM unnest($1::text[], $2::text[], $3::text[], $4::text[], $5::bigint[], $6::bigint[], $7::bigint[], $8::text[],
			$11::text[], $12::text[], $13::text[], $14::float8[], $15::boolean[], $16::float8[], $17::bigint[],
			$18::float8[], $19::text[], $20::text[])
			AS u(id, kind, rules, findings, ob, pb, eb, un, bl, res, codec, rate, est, rcap, scap, dur, pa, oa)
		ON CONFLICT (item_id) DO UPDATE SET kind = EXCLUDED.kind, rules = EXCLUDED.rules, findings = EXCLUDED.findings,
			originalbytes = EXCLUDED.originalbytes, packagebytes = EXCLUDED.packagebytes,
			estimatedbytes = EXCLUDED.estimatedbytes, unchecked = EXCLUDED.unchecked, blocked = EXCLUDED.blocked,
			resolution = EXCLUDED.resolution, videocodec = EXCLUDED.videocodec, videobitrate = EXCLUDED.videobitrate,
			bitrateestimated = EXCLUDED.bitrateestimated, bitratecap = EXCLUDED.bitratecap, sizecap = EXCLUDED.sizecap,
			durationseconds = EXCLUDED.durationseconds, packageaudio = EXCLUDED.packageaudio,
			originalaudio = EXCLUDED.originalaudio,
			ruleshash = EXCLUDED.ruleshash, staleat = CASE WHEN c.staleat > $10 THEN c.staleat END, evaluatedat = now()`,
		ids, kinds, rules, findings, orig, pkg, est, unchecked, hash, start, blocked,
		res, codec, rate, estimated, rateCap, sizeCap, dur, pkgAudio, srcAudio)
	return err
}
