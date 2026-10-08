package scanner

import (
	"context"
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/zaentrum/katalog-manager/internal/events"
	"github.com/zaentrum/katalog-manager/internal/library"
	"github.com/zaentrum/katalog-manager/internal/model"
	"github.com/zaentrum/katalog-manager/internal/processing"
)

// One file of several episodes (library/covers.go): a file whose name numbers
// a range (S05E15-E16) is taken in for its first episode, its holder, as any
// episode's file is, and each other episode of the range, of the holder's
// series and season, is linked to it once the walk has taken every file in
// (an episode's own file may be walked after the file that covers it): the
// catalog's episode of that number, or, when it has none, one the scan makes
// as it makes any episode (its scan step done, its discovered event sent, so
// that the enricher reads its texts and images). Its steps of a file do not
// apply. An episode of that number with a file of its own (a playback row but
// a trailer's: its original, its package) is left alone, its own file wins,
// and one another file covers already stays that file's: the scan's report
// says so. An episode the file covered that its name covers no more is
// unlinked. A file the scan took in before it read its name's numbers
// (S01E01E02 was none to it) gets them. Without migration 045 a file covers
// its first episode alone, said once in the log.

// covering is a file of the walk that covers episodes besides its first, or
// did: its holder, its place, the holder's parent and numbers, how many more
// episodes its name numbers, and the title and year its name gives.
type covering struct {
	holder, path, parent string
	season, episode      int32
	span                 int
	title                string
	year                 *int32
}

// coverEpisodes notes, for the walk to link once it is through
// (linkCovered), the episodes the file at path of the episode holder covers
// besides it, as its name's token tok numbers them (numbered: the name has
// one), and those it covered and covers no more.
func (s *Scanner) coverEpisodes(ctx context.Context, xs *walkState, holder, path string, tok episodeToken, numbered bool,
	title string, year *int32) {
	span := 0
	if numbered {
		span = tok.last - tok.first
	}
	if !xs.covers {
		if span > 0 && !xs.coversSaid {
			log.Printf("scanner: migration 045 (db/migrations/045_multi_episode_files.sql) is not applied: %s covers episodes "+
				"%d to %d of its season, and is the first one's alone, as every file of several episodes is until it is",
				path, tok.first, tok.last)
			xs.coversSaid = true
		}
		return
	}
	pool := s.st.Pool()
	var typ string
	var parent *string
	var season, episode *int32
	var did bool
	if err := pool.QueryRow(ctx, `SELECT i.type, i.parent_id, i.seasonnumber, i.episodenumber,
			EXISTS (SELECT 1 FROM com_nalet_katalog_items c WHERE c.coveredby = i.id)
		FROM com_nalet_katalog_items i WHERE i.id = $1`, holder).Scan(&typ, &parent, &season, &episode, &did); err != nil {
		log.Printf("scanner: the episodes %s covers could not be read: %v", path, err)
		return
	}
	if typ != "episode" || (span == 0 && !did) {
		return
	}
	if numbered && (season == nil || episode == nil) {
		if err := pool.QueryRow(ctx, `UPDATE com_nalet_katalog_items SET seasonnumber = COALESCE(seasonnumber, $2),
				episodenumber = COALESCE(episodenumber, $3) WHERE id = $1 RETURNING seasonnumber, episodenumber`,
			holder, tok.season, tok.first).Scan(&season, &episode); err != nil {
			log.Printf("scanner: episode %s could not be numbered as the name of %s numbers it: %v", holder, path, err)
			return
		}
	}
	c := covering{holder: holder, path: path, span: span, title: title, year: year}
	if parent == nil || season == nil || episode == nil {
		if span > 0 {
			log.Printf("scanner: %s covers episodes %d to %d, and episode %s has no series or numbers to find them by", path,
				tok.first, tok.last, holder)
		}
		c.span = 0
	} else {
		c.parent, c.season, c.episode = *parent, *season, *episode
	}
	xs.covering = append(xs.covering, c)
}

// linkCovered links the episodes the files the walk noted cover
// (coverEpisodes), once every file of the walk is in, and unlinks those they
// cover no more.
func (s *Scanner) linkCovered(ctx context.Context, xs *walkState, res *scanResult) {
	for _, c := range xs.covering {
		s.linkCovering(ctx, res, c)
	}
}

// linkCovering links the episodes the file of c covers besides its holder,
// counted from the holder's own number (an episode an admin renumbered covers
// the episodes after its number), and unlinks those it covered and covers no
// more.
func (s *Scanner) linkCovering(ctx context.Context, res *scanResult, c covering) {
	pool := s.st.Pool()
	want := map[int32]bool{}
	for n := 1; n <= c.span; n++ {
		want[c.episode+int32(n)] = true
	}
	covered, err := library.CoveredOf(ctx, pool, c.holder)
	if err != nil {
		log.Printf("scanner: the episodes %s covers could not be read: %v", c.path, err)
		return
	}
	var gone []string
	for _, e := range covered {
		if e.Season == nil || e.Episode == nil || *e.Season != c.season || !want[*e.Episode] {
			gone = append(gone, e.ID)
		}
	}
	if len(gone) > 0 {
		if _, err := library.Unlink(ctx, pool, gone, "covers it no more: its name numbers it no more"); err != nil {
			log.Printf("scanner: the episodes %s covers no more could not be unlinked: %v", c.path, err)
		} else {
			log.Printf("scanner: %s covers episodes %s no more", c.path, strings.Join(gone, ", "))
		}
	}
	if len(want) == 0 {
		return
	}
	var series string
	if err := pool.QueryRow(ctx, `SELECT CASE WHEN p.type = 'season' THEN COALESCE(p.parent_id, p.id) ELSE p.id END
		FROM com_nalet_katalog_items p WHERE p.id = $1`, c.parent).Scan(&series); err != nil {
		log.Printf("scanner: the series of episode %s could not be read: %v", c.holder, err)
		return
	}
	for n := c.episode + 1; n <= c.episode+int32(c.span); n++ {
		s.coverEpisode(ctx, res, c.holder, c.path, series, c.parent, c.season, n, c.title, c.year)
	}
}

// coverEpisode links episode n of season of the series to the file at path
// of the episode holder (see coverEpisodes), making it under parent (the
// holder's) when the catalog has none.
func (s *Scanner) coverEpisode(ctx context.Context, res *scanResult, holder, path, series, parent string, season, n int32,
	title string, year *int32) {
	pool := s.st.Pool()
	code := fmt.Sprintf("S%02dE%02d", season, n)
	rows, err := pool.Query(ctx, `SELECT e.id, e.coveredby,
			EXISTS (SELECT 1 FROM com_nalet_katalog_playbackassets a WHERE a.item_id = e.id
				AND COALESCE(a.kind, 'primary') <> 'trailer')
		FROM com_nalet_katalog_items e
		WHERE e.type = 'episode' AND e.seasonnumber = $2 AND e.episodenumber = $3 AND e.id <> $4
		  AND (e.parent_id = $1 OR e.parent_id IN (SELECT x.id FROM com_nalet_katalog_items x
		                                           WHERE x.parent_id = $1 AND x.type = 'season'))
		ORDER BY e.createdat NULLS LAST, e.id`, series, season, n, holder)
	if err != nil {
		log.Printf("scanner: episode %s of the series of %s could not be read: %v", code, path, err)
		return
	}
	type episode struct {
		id   string
		by   *string
		file bool
	}
	var eps []episode
	for rows.Next() {
		var e episode
		if err := rows.Scan(&e.id, &e.by, &e.file); err != nil {
			rows.Close()
			log.Printf("scanner: episode %s of the series of %s could not be read: %v", code, path, err)
			return
		}
		eps = append(eps, e)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		log.Printf("scanner: episode %s of the series of %s could not be read: %v", code, path, err)
		return
	}
	for _, e := range eps {
		if !e.file {
			continue
		}
		if e.by != nil && *e.by == holder {
			if _, err := library.Unlink(ctx, pool, []string{e.id}, "covers it no more: it has a file of its own, which wins"); err != nil {
				log.Printf("scanner: episode %s could not be unlinked from %s: %v", e.id, path, err)
			}
		}
		id := e.id
		res.note(model.ScanNote{Kind: model.ScanNoteNotLinked, Path: path, ItemID: &id,
			Reason: fmt.Sprintf("%s has a file of its own, which wins: the file of episode %s does not cover it", code, holder)})
		return
	}
	if len(eps) > 0 {
		e := eps[0]
		if e.by != nil && *e.by != holder {
			id := e.id
			res.note(model.ScanNote{Kind: model.ScanNoteNotLinked, Path: path, ItemID: &id,
				Reason: fmt.Sprintf("%s is covered by the file of episode %s already", code, *e.by)})
			return
		}
		if linked, err := library.Link(ctx, pool, holder, e.id); err != nil {
			log.Printf("scanner: episode %s could not be linked to %s: %v", e.id, path, err)
		} else if linked {
			log.Printf("scanner: %s covers episode %s (%s) too", path, code, e.id)
		}
		return
	}
	// The catalog has no episode of that number: the scan makes it, as it
	// makes any episode.
	var id string
	if err := pool.QueryRow(ctx, `INSERT INTO com_nalet_katalog_items
			(id, type, title, sorttitle, year, parent_id, seasonnumber, episodenumber, createdat, modifiedat)
		VALUES (gen_random_uuid()::varchar, 'episode', $1, $2, $3, $4, $5, $6, now(), now())
		RETURNING id`, title, strings.ToLower(title), year, parent, season, n).Scan(&id); err != nil {
		log.Printf("scanner: episode %s, which %s covers, could not be made: %v", code, path, err)
		return
	}
	res.itemsInserted++
	_ = s.steps.Upsert(ctx, id, "scan", processing.StatusDone, nil, nil)
	if _, err := library.Link(ctx, pool, holder, id); err != nil {
		log.Printf("scanner: episode %s could not be linked to %s: %v", id, path, err)
	}
	ev := events.NewItemEvent(id)
	ev.Type = "episode"
	ev.Step = "tmdb"
	ev.Source = "scan"
	s.prod.EmitItem(ctx, events.TopicDiscovered, ev)
	log.Printf("scanner: %s covers episode %s too, made for it (item %s)", path, code, id)
}

// passOverDiscImage passes over the disc image at path: it is no title's
// file, so it makes no title and no asset, and the scan's report says so. A
// title whose file it is already (from before the scanner passed over disc
// images, or ingested) fails its steps that read it, for good
// (processing.FailDiscImage), and the report names it.
func (s *Scanner) passOverDiscImage(ctx context.Context, path string, res *scanResult) {
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	n := model.ScanNote{Kind: model.ScanNoteUnsupported, Path: path, Reason: processing.DiscImageReason}
	var item string
	err := s.st.Pool().QueryRow(ctx, `SELECT item_id FROM com_nalet_katalog_playbackassets WHERE path = $1 AND isprimary = true
		ORDER BY item_id LIMIT 1`, path).Scan(&item)
	switch {
	case err == nil:
		n.ItemID = &item
		if err := s.steps.FailDiscImage(ctx, item); err != nil {
			log.Printf("scanner: the steps of item %s, whose file %s is a disc image, could not be failed: %v", item, path, err)
		}
	case !errors.Is(err, pgx.ErrNoRows):
		log.Printf("scanner: the title of %s could not be read: %v", path, err)
	}
	res.note(n)
}
