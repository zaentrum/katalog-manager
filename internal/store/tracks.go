package store

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/zaentrum/katalog-manager/db/migrations"
	"github.com/zaentrum/katalog-manager/internal/languages"
	"github.com/zaentrum/katalog-manager/internal/model"
)

// trackTables are the tables db/migrations/037_track_languages.sql creates:
// a title's source tracks, and an admin's language of a track. Their rows hang
// off an item and go with it (DeleteItems).
var trackTables = []string{"com_nalet_katalog_itemtracks", "com_nalet_katalog_itemtracklanguages"}

// errNoTrackLanguages says what an admin's track language needs and lacks.
var errNoTrackLanguages = errors.New("the track languages migration (db/migrations/037_track_languages.sql) is not applied")

// TrackLanguagesReady reports whether migration 037 is in place: both of its
// tables. Without it the catalog knows no source's tracks, and no track's
// language can be set.
func (s *Store) TrackLanguagesReady(ctx context.Context) (bool, error) {
	var ok bool
	err := s.pool.QueryRow(ctx, `SELECT to_regclass('com_nalet_katalog_itemtracks') IS NOT NULL
		AND to_regclass('com_nalet_katalog_itemtracklanguages') IS NOT NULL`).Scan(&ok)
	return ok, err
}

// EnsureTrackLanguages applies db/migrations/037_track_languages.sql when any
// of its tables is missing. The check comes first for the reason
// EnsureDeletionLog gives.
func (s *Store) EnsureTrackLanguages(ctx context.Context) error {
	ready, err := s.TrackLanguagesReady(ctx)
	if err != nil || ready {
		return err
	}
	_, err = s.pool.Exec(ctx, migrations.TrackLanguages)
	return err
}

// undefinedTable reports whether err is Postgres saying a table does not
// exist (42P01): here, a catalog older than migration 037.
func undefinedTable(err error) bool {
	var pe *pgconn.PgError
	return errors.As(err, &pe) && pe.Code == "42P01"
}

// ValidTrackKind reports whether kind is a kind of track: audio or subtitle.
func ValidTrackKind(kind string) bool { return kind == model.TrackAudio || kind == model.TrackSubtitle }

// trackOrder lists a title's tracks as a source is read: its audio, then its
// subtitles, each by ordinal.
const trackOrder = `CASE kind WHEN 'audio' THEN 0 ELSE 1 END, ordinal`

// Tracks lists the tracks of the item's source, as its packages reported them,
// with the language an admin set for each; a track only an admin's language
// names comes too, not reported. Audio first, then subtitles, each by
// ordinal. A catalog without migration 037 knows none.
func (s *Store) Tracks(ctx context.Context, itemID string) ([]*model.Track, error) {
	rows, err := s.pool.Query(ctx, `SELECT kind, ordinal, language, title, format, forced, override, reported FROM (
		SELECT COALESCE(t.kind, o.kind) AS kind, COALESCE(t.ordinal, o.ordinal) AS ordinal,
		       t.language, t.title, t.format, COALESCE(t.forced, false) AS forced,
		       o.language AS override, t.item_id IS NOT NULL AS reported
		FROM (SELECT * FROM com_nalet_katalog_itemtracks WHERE item_id = $1) t
		FULL JOIN (SELECT * FROM com_nalet_katalog_itemtracklanguages WHERE item_id = $1) o
		  ON o.kind = t.kind AND o.ordinal = t.ordinal) tracks
		ORDER BY `+trackOrder, itemID)
	if undefinedTable(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.Track
	for rows.Next() {
		var t model.Track
		if err := rows.Scan(&t.Kind, &t.Ordinal, &t.Language, &t.Title, &t.Format, &t.Forced, &t.Override, &t.Reported); err != nil {
			return nil, err
		}
		out = append(out, &t)
	}
	if undefinedTable(rows.Err()) {
		return nil, nil
	}
	return out, rows.Err()
}

// TrackLanguages lists the languages an admin set for the tracks of the
// item's source: audio first, then subtitles, each by ordinal. A catalog
// without migration 037 has none.
func (s *Store) TrackLanguages(ctx context.Context, itemID string) ([]model.TrackLanguage, error) {
	rows, err := s.pool.Query(ctx, `SELECT kind, ordinal, language FROM com_nalet_katalog_itemtracklanguages
		WHERE item_id = $1 ORDER BY `+trackOrder, itemID)
	if undefinedTable(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.TrackLanguage
	for rows.Next() {
		var l model.TrackLanguage
		if err := rows.Scan(&l.Kind, &l.Ordinal, &l.Language); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	if undefinedTable(rows.Err()) {
		return nil, nil
	}
	return out, rows.Err()
}

// SetTrackLanguage sets the language of a track of the item's source by hand,
// as by says who: kind audio or subtitle, ordinal the track's place among the
// source's streams of its kind (0 first), language an ISO 639-2 code of three
// lowercase letters (languages.IsCode; zxx: no dialogue, und: undetermined),
// which wins over the source's tag; nil clears it. It reports whether there
// is such an item. An item without a source file has no tracks to set. Once a
// package has reported the source's audio tracks, an audio ordinal it did not
// report is refused, as a package carries every audio stream of its source; a
// subtitle's is not, as an encode leaves out the subtitle streams it cannot
// copy (mov_text) and its package then reports fewer than the source has.
// Clearing one is never refused. A change modifies the item, as by. A catalog
// without migration 037 has nowhere to keep it: an error.
func (s *Store) SetTrackLanguage(ctx context.Context, itemID, kind string, ordinal int32, language *string, by string) (bool, error) {
	if !ValidTrackKind(kind) {
		return false, fmt.Errorf("a track is of kind audio or subtitle, not %q", kind)
	}
	if ordinal < 0 {
		return false, fmt.Errorf("a track's ordinal is its place among the source's %s streams, 0 or more, not %d", kind, ordinal)
	}
	if language != nil && !languages.IsCode(*language) {
		return false, fmt.Errorf("a track's language is an ISO 639-2 code, three lowercase letters "+
			"(zxx: no dialogue, und: unknown), not %q", *language)
	}
	by = clip(strings.TrimSpace(by), 255)

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)

	var exists, hasSource bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM com_nalet_katalog_items WHERE id = $1),
		EXISTS (SELECT 1 FROM com_nalet_katalog_playbackassets WHERE item_id = $1 AND isprimary = true)`, itemID).
		Scan(&exists, &hasSource); err != nil {
		return false, err
	}
	if !exists {
		return false, nil
	}
	if !hasSource {
		return true, fmt.Errorf("item %s has no source file, so no tracks to set the language of", itemID)
	}

	var changed int64
	if language == nil {
		tag, err := tx.Exec(ctx, `DELETE FROM com_nalet_katalog_itemtracklanguages
			WHERE item_id = $1 AND kind = $2 AND ordinal = $3`, itemID, kind, ordinal)
		if undefinedTable(err) {
			return true, errNoTrackLanguages
		}
		if err != nil {
			return true, err
		}
		changed = tag.RowsAffected()
	} else {
		var reported, match int
		err := tx.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE ordinal = $3)
			FROM com_nalet_katalog_itemtracks WHERE item_id = $1 AND kind = $2`, itemID, kind, ordinal).Scan(&reported, &match)
		if undefinedTable(err) {
			return true, errNoTrackLanguages
		}
		if err != nil {
			return true, err
		}
		if kind == model.TrackAudio && reported > 0 && match == 0 {
			return true, fmt.Errorf("the source of %s has no audio track %d: its package reported %d, ordinals 0 to %d",
				itemID, ordinal, reported, reported-1)
		}
		tag, err := tx.Exec(ctx, `INSERT INTO com_nalet_katalog_itemtracklanguages AS l
			(item_id, kind, ordinal, language, modifiedat, modifiedby) VALUES ($1, $2, $3, $4, now(), $5)
			ON CONFLICT (item_id, kind, ordinal) DO UPDATE SET language = EXCLUDED.language,
				modifiedat = EXCLUDED.modifiedat, modifiedby = EXCLUDED.modifiedby
			WHERE l.language IS DISTINCT FROM EXCLUDED.language`, itemID, kind, ordinal, *language, by)
		if undefinedTable(err) {
			return true, errNoTrackLanguages
		}
		if err != nil {
			return true, err
		}
		changed = tag.RowsAffected()
	}
	if changed > 0 {
		if _, err := tx.Exec(ctx, `UPDATE com_nalet_katalog_items SET modifiedat = now(), modifiedby = $2
			WHERE id = $1`, itemID, by); err != nil {
			return true, err
		}
	}
	return true, tx.Commit(ctx)
}

// RecordSourceTracks records what a package says of the item's source: for
// each kind it lists (a key of tracks, with no track at all too), the
// source's tracks of the kind, and the kind's tracks it does not list go. A
// track keeps the language a package reported for it, except that a
// language equal to the one an admin set for the track is no word on the
// source's tag (a packager labels a track with the admin's language): the
// track keeps the tag it had, or none. Of tracks reported twice the first
// counts. It returns how many tracks it recorded; on a catalog without
// migration 037, none.
func (s *Store) RecordSourceTracks(ctx context.Context, itemID string, tracks map[string][]model.SourceTrack) (int, error) {
	if len(tracks) == 0 {
		return 0, nil
	}
	ready, err := s.TrackLanguagesReady(ctx)
	if err != nil || !ready {
		return 0, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)

	set, err := tx.Query(ctx, `SELECT kind, ordinal, language FROM com_nalet_katalog_itemtracklanguages
		WHERE item_id = $1`, itemID)
	if err != nil {
		return 0, err
	}
	overrides := map[string]string{}
	for set.Next() {
		var l model.TrackLanguage
		if err := set.Scan(&l.Kind, &l.Ordinal, &l.Language); err != nil {
			set.Close()
			return 0, err
		}
		overrides[trackKey(l.Kind, l.Ordinal)] = l.Language
	}
	set.Close()
	if err := set.Err(); err != nil {
		return 0, err
	}

	kinds := make([]string, 0, len(tracks))
	for kind := range tracks {
		if !ValidTrackKind(kind) {
			return 0, fmt.Errorf("a track is of kind audio or subtitle, not %q", kind)
		}
		kinds = append(kinds, kind)
	}
	sort.Strings(kinds)
	recorded := 0
	for _, kind := range kinds {
		ordinals := []int32{}
		seen := map[int32]bool{}
		for _, t := range tracks[kind] {
			if t.Ordinal < 0 || seen[t.Ordinal] {
				continue
			}
			seen[t.Ordinal] = true
			ordinals = append(ordinals, t.Ordinal)
			lang := trackLanguage(t.Language)
			upsert := recordTrack
			if o, ok := overrides[trackKey(kind, t.Ordinal)]; ok && lang != nil && *lang == o {
				upsert, lang = recordTrackKeepingItsLanguage, nil
			}
			if _, err := tx.Exec(ctx, upsert, itemID, kind, t.Ordinal, lang, clipped(t.Title, 255),
				clipped(t.Format, 20), t.Forced); err != nil {
				return 0, fmt.Errorf("record %s track %d of %s: %w", kind, t.Ordinal, itemID, err)
			}
			recorded++
		}
		if _, err := tx.Exec(ctx, `DELETE FROM com_nalet_katalog_itemtracks
			WHERE item_id = $1 AND kind = $2 AND ordinal <> ALL($3::int[])`, itemID, kind, ordinals); err != nil {
			return 0, err
		}
	}
	return recorded, tx.Commit(ctx)
}

// recordTrack writes a reported track over what the catalog held of it;
// recordTrackKeepingItsLanguage does so but for its language, which stays.
const (
	recordTrackInsert = `INSERT INTO com_nalet_katalog_itemtracks AS t
		(item_id, kind, ordinal, language, title, format, forced, updatedat)
		VALUES ($1, $2, $3, $4, $5, $6, $7, now())
		ON CONFLICT (item_id, kind, ordinal) DO UPDATE SET `
	recordTrack = recordTrackInsert + `language = EXCLUDED.language, title = EXCLUDED.title,
		format = EXCLUDED.format, forced = EXCLUDED.forced, updatedat = EXCLUDED.updatedat`
	recordTrackKeepingItsLanguage = recordTrackInsert + `title = EXCLUDED.title,
		format = EXCLUDED.format, forced = EXCLUDED.forced, updatedat = EXCLUDED.updatedat`
)

func trackKey(kind string, ordinal int32) string { return fmt.Sprintf("%s/%d", kind, ordinal) }

// trackLanguage is a reported language as the catalog keeps it: trimmed,
// lowercase, at most 35 characters; nil for none.
func trackLanguage(l *string) *string {
	if l == nil {
		return nil
	}
	v := strings.ToLower(strings.TrimSpace(*l))
	if v == "" {
		return nil
	}
	v = clip(v, 35)
	return &v
}

// clipped is s cut to n characters; nil for nil or a blank s.
func clipped(s *string, n int) *string {
	if s == nil || strings.TrimSpace(*s) == "" {
		return nil
	}
	v := clip(*s, n)
	return &v
}

// deleteTracksOf removes, in tx, the items' source tracks and the languages
// an admin set for them, on a catalog that has the tables (migration 037).
func deleteTracksOf(ctx context.Context, tx pgx.Tx, ids []string) error {
	for _, t := range trackTables {
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT to_regclass($1::text) IS NOT NULL`, t).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			continue
		}
		if _, err := tx.Exec(ctx, `DELETE FROM `+t+` WHERE item_id = ANY($1)`, ids); err != nil {
			return err
		}
	}
	return nil
}
