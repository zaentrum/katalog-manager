package store

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/zaentrum/katalog-manager/db/migrations"
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

// deleteLibraryOf removes, in tx, the originals and the versions the catalog
// keeps of the items of ids, on a catalog that has their tables (040).
func deleteLibraryOf(ctx context.Context, tx pgx.Tx, ids []string) error {
	for _, t := range []string{SourcesTable, VersionsTable} {
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
