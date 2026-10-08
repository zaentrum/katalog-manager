package store

import (
	"context"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/zaentrum/katalog-manager/db/migrations"
	"github.com/zaentrum/katalog-manager/internal/model"
)

// The tables db/migrations/040_library_v2.sql creates: a title's originals
// and its versions. Their rows hang off an item and go with it (DeleteItems).
const (
	SourcesTable  = "com_nalet_katalog_itemsources"
	VersionsTable = "com_nalet_katalog_itemversions"
)

// libraryColumns are the columns 040 adds to the tables it does not create,
// as table.column.
var libraryColumns = []string{
	"com_nalet_katalog_items.recordedat", "com_nalet_katalog_items.libraryprojectedat",
	"com_nalet_katalog_items.retirehold", "com_nalet_katalog_people.libraryprojectedat",
	"com_nalet_katalog_playbackassets.sourceid", "com_nalet_katalog_playbackassets.versionid",
	"com_nalet_katalog_itemextras.packageid", "com_nalet_katalog_itemextras.recordedat",
	"com_nalet_katalog_itemextras.sourcedeletedat",
}

// libraryRelations are the tables and indexes 040 creates.
var libraryRelations = []string{SourcesTable, VersionsTable, "idx_itemsources_item", "idx_itemsources_fixity",
	"idx_itemsources_arrival", "idx_itemversions_item", "idx_itemversions_building", "idx_itemversions_current"}

// LibraryReady reports whether migration 040 is in place: its tables, their
// indexes, and every column it adds. Without it the catalog keeps nothing of
// the library record, and the setting library.layout=v2 cannot take effect.
func (s *Store) LibraryReady(ctx context.Context) (bool, error) {
	var ok bool
	err := s.pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM unnest($1::text[]) AS r(name) WHERE to_regclass(r.name) IS NOT NULL) = cardinality($1::text[])
		AND (SELECT count(*) FROM unnest($2::text[]) AS c(name)
		     JOIN pg_attribute a ON a.attrelid = to_regclass(split_part(c.name, '.', 1))
		      AND a.attname = split_part(c.name, '.', 2) AND a.attnum > 0 AND NOT a.attisdropped) = cardinality($2::text[])`,
		libraryRelations, libraryColumns).Scan(&ok)
	return ok, err
}

// projectionFunctions are the functions 041 creates; projectionTriggers its
// triggers, as table.trigger, those on the tables of migrations 030 and 039
// only where those tables exist.
var projectionFunctions = []string{"com_nalet_katalog_library_items_touched", "com_nalet_katalog_library_extras_touched",
	"com_nalet_katalog_library_item_changed", "com_nalet_katalog_library_episodes_touched",
	"com_nalet_katalog_library_episode_moved", "com_nalet_katalog_library_portraits_touched",
	"com_nalet_katalog_library_person_renamed"}

func projectionTriggers() []string {
	var out []string
	for _, t := range []string{"com_nalet_katalog_itemgenres", "com_nalet_katalog_itemtags", "com_nalet_katalog_itempeople",
		"com_nalet_katalog_itemexternalids", "com_nalet_katalog_itemartworkdata", "com_nalet_katalog_itemtrailerlinks",
		"?com_nalet_katalog_personartwork"} {
		for _, tr := range []string{"library_insert", "library_update", "library_delete"} {
			out = append(out, t+"."+tr)
		}
	}
	return append(out, "?com_nalet_katalog_itemextras.library_update", "com_nalet_katalog_items.library_changed",
		"com_nalet_katalog_items.library_episodes_insert", "com_nalet_katalog_items.library_episodes_delete",
		"com_nalet_katalog_items.library_episode_moved", "com_nalet_katalog_people.library_renamed")
}

// LibraryProjectionReady reports whether migration 041 is in place: its
// functions and its triggers (one marked ? on a table that may not exist,
// only where it does). Without it nothing tells the projector what changed.
func (s *Store) LibraryProjectionReady(ctx context.Context) (bool, error) {
	var ok bool
	err := s.pool.QueryRow(ctx, `SELECT
		(SELECT count(DISTINCT f.name) FROM unnest($1::text[]) AS f(name)
		 JOIN pg_proc p ON p.proname = f.name AND pg_function_is_visible(p.oid)) = cardinality($1::text[])
		AND NOT EXISTS (
			SELECT 1 FROM unnest($2::text[]) AS t(spec),
			LATERAL (SELECT left(t.spec, 1) = '?' AS optional,
			                split_part(ltrim(t.spec, '?'), '.', 1) AS rel, split_part(t.spec, '.', 2) AS trigger) x
			WHERE (to_regclass(x.rel) IS NOT NULL OR NOT x.optional)
			  AND NOT EXISTS (SELECT 1 FROM pg_trigger g WHERE g.tgrelid = to_regclass(x.rel) AND g.tgname = x.trigger))`,
		projectionFunctions, projectionTriggers()).Scan(&ok)
	return ok, err
}

// EnsureLibrary applies db/migrations/040_library_v2.sql, then
// 041_library_projection.sql and 042_playback_item_index.sql, each when any
// of its objects is missing; 040 needs the extras' table (039), so it runs
// after EnsureItemExtras. The checks come first for the reason
// EnsureDeletionLog gives.
func (s *Store) EnsureLibrary(ctx context.Context) error {
	ready, err := s.LibraryReady(ctx)
	if err != nil {
		return err
	}
	if !ready {
		if _, err := s.pool.Exec(ctx, migrations.LibraryV2); err != nil {
			return err
		}
	}
	if ready, err = s.LibraryProjectionReady(ctx); err != nil {
		return err
	}
	if !ready {
		if _, err := s.pool.Exec(ctx, migrations.LibraryProjection); err != nil {
			return err
		}
	}
	var indexed bool
	if err := s.pool.QueryRow(ctx, `SELECT to_regclass('idx_playbackassets_item') IS NOT NULL`).Scan(&indexed); err != nil || indexed {
		return err
	}
	_, err = s.pool.Exec(ctx, migrations.PlaybackItemIndex)
	return err
}

// LayoutSetting is the setting of the library's layout: v2, or legacy (any
// other value, and none).
const LayoutSetting = "library.layout"

// LayoutOf is the layout a value of LayoutSetting says.
func LayoutOf(value string) string {
	if strings.EqualFold(strings.TrimSpace(value), "v2") {
		return "v2"
	}
	return "legacy"
}

// LayoutRows are the rows of LayoutSetting, by id: the first is the one the
// library reads.
func (s *Store) LayoutRows(ctx context.Context) ([]*model.Setting, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+settingCols+` FROM com_nalet_katalog_settings
		WHERE btrim(key) = $1 ORDER BY id`, LayoutSetting)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.Setting
	for rows.Next() {
		var x model.Setting
		if err := scanSetting(rows, &x); err != nil {
			return nil, err
		}
		out = append(out, &x)
	}
	return out, rows.Err()
}

// LayoutWaits counts what is between its transcode and its package: the
// titles whose transcode runs, or is finished while their package waits,
// runs or is to be retried, or whose take-in (the v2 layout's) does, and the
// extras transcoding, transcoded or packaging. The transcode's handoff to the
// packager lies in the inbox of the layout it ran in, and a take-in is the
// v2 layout's, so the layout does not change while any is.
func (s *Store) LayoutWaits(ctx context.Context) (titles, extras int, err error) {
	if err = s.pool.QueryRow(ctx, `SELECT count(DISTINCT t.item_id) FROM com_nalet_katalog_itemprocessingsteps t
		WHERE (t.step = 'transcode' AND (t.status = 'in_progress' OR (t.status IN ('done', 'not_applicable', 'skipped')
		  AND EXISTS (SELECT 1 FROM com_nalet_katalog_itemprocessingsteps p WHERE p.item_id = t.item_id AND p.step = 'package'
		              AND (p.status IN ('pending', 'in_progress') OR (p.status = 'failed' AND p.nextretryat IS NOT NULL))))))
		   OR (t.step = 'takein' AND (t.status IN ('pending', 'in_progress') OR (t.status = 'failed' AND t.nextretryat IS NOT NULL)))`).
		Scan(&titles); err != nil {
		return 0, 0, err
	}
	err = s.pool.QueryRow(ctx, `SELECT count(*) FROM `+extrasTable+`
		WHERE removedat IS NULL AND state IN ('transcoding', 'transcoded', 'packaging')`).Scan(&extras)
	if undefinedTable(err) {
		return titles, 0, nil
	}
	return titles, extras, err
}

// deleteLibraryOf removes, in tx, the originals and the versions the catalog
// keeps of the items of ids, and their places in the re-encode queue, on a
// catalog that has their tables (040, 043).
func deleteLibraryOf(ctx context.Context, tx pgx.Tx, ids []string) error {
	for _, t := range []string{SourcesTable, VersionsTable, ReencodeQueueTable} {
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

// ReencodeQueueTable is the table db/migrations/043_reencode_queue.sql
// creates: the titles queued to be encoded again.
const ReencodeQueueTable = "com_nalet_katalog_reencodequeue"

// ReencodeQueueReady reports whether migration 043 is in place: its table and
// its indexes.
func (s *Store) ReencodeQueueReady(ctx context.Context) (bool, error) {
	var ok bool
	err := s.pool.QueryRow(ctx, `SELECT to_regclass($1::text) IS NOT NULL AND to_regclass('idx_reencodequeue_live') IS NOT NULL
		AND to_regclass('idx_reencodequeue_state') IS NOT NULL`, ReencodeQueueTable).Scan(&ok)
	return ok, err
}

// EnsureReencodeQueue applies db/migrations/043_reencode_queue.sql when any of
// its objects is missing. Without it no title is queued to be encoded again.
func (s *Store) EnsureReencodeQueue(ctx context.Context) error {
	ready, err := s.ReencodeQueueReady(ctx)
	if err != nil || ready {
		return err
	}
	_, err = s.pool.Exec(ctx, migrations.ReencodeQueue)
	return err
}

// TakeInReady reports whether migration 044 is in place: a version's state
// may be taken (its folder holds its original and no package). It is not on
// a catalog without migration 040, which keeps no versions.
func (s *Store) TakeInReady(ctx context.Context) (bool, error) {
	var ok bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_constraint
		WHERE conrelid = to_regclass($1::text) AND contype = 'c' AND pg_get_constraintdef(oid) LIKE '%''taken''%')`,
		VersionsTable).Scan(&ok)
	return ok, err
}

// EnsureTakeIn applies db/migrations/044_library_takein.sql when a version's
// state may not be taken yet, on a catalog that keeps versions (040). Without
// it no title is taken in, and a migration's unit of a title staged
// unpackaged is not adopted.
func (s *Store) EnsureTakeIn(ctx context.Context) error {
	var versions bool
	if err := s.pool.QueryRow(ctx, `SELECT to_regclass($1::text) IS NOT NULL`, VersionsTable).Scan(&versions); err != nil ||
		!versions {
		return err
	}
	ready, err := s.TakeInReady(ctx)
	if err != nil || ready {
		return err
	}
	_, err = s.pool.Exec(ctx, migrations.TakeIn)
	return err
}

// multiEpisodeColumns are the columns db/migrations/045_multi_episode_files.sql
// adds, as table.column.
var multiEpisodeColumns = []string{"com_nalet_katalog_items.coveredby", "com_nalet_katalog_scanjobs.report"}

// MultiEpisodeFilesReady reports whether migration 045 is in place: an
// episode may name the episode whose file covers it (coveredby), the index
// that finds them, and a scan job keeps what its scan passed over (report).
func (s *Store) MultiEpisodeFilesReady(ctx context.Context) (bool, error) {
	var ok bool
	err := s.pool.QueryRow(ctx, `SELECT to_regclass('idx_items_coveredby') IS NOT NULL
		AND (SELECT count(*) FROM unnest($1::text[]) AS c(name)
		     JOIN pg_attribute a ON a.attrelid = to_regclass(split_part(c.name, '.', 1))
		      AND a.attname = split_part(c.name, '.', 2) AND a.attnum > 0 AND NOT a.attisdropped) = cardinality($1::text[])`,
		multiEpisodeColumns).Scan(&ok)
	return ok, err
}

// EnsureMultiEpisodeFiles applies db/migrations/045_multi_episode_files.sql
// when any of its objects is missing. Without it a file covers the episode
// its name numbers first and no other, as before, and a scan says what it
// passed over in the log alone.
func (s *Store) EnsureMultiEpisodeFiles(ctx context.Context) error {
	ready, err := s.MultiEpisodeFilesReady(ctx)
	if err != nil || ready {
		return err
	}
	_, err = s.pool.Exec(ctx, migrations.MultiEpisodeFiles)
	return err
}
