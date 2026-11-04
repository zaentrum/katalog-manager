package store

import "context"

// What a title has on disk, as the catalog records it, for the catalog's
// list: the sizes of its originals and of its package and whether it has an
// original there (GraphQL Item.originalBytes, packageBytes, hasOriginal), read
// for a page of items at once, and the list's filter of the titles that have
// one (ItemFilter.HasOriginal), which holds them to the same SQL.

// ItemSizes is what an item has on disk: the size of its originals there,
// nil when it has none (its original retired after packaging, or no file),
// the size of its package, nil when it has none, and whether it has an
// original there, which is whether it has its OriginalBytes. An episode
// another's file covers has neither a file nor a package of its own, its
// sizes nil, the file counted once, on its holder; it has an original while
// its holder has one. A container (a series, a season, an album) has no file
// of its own: all three are nil.
type ItemSizes struct {
	OriginalBytes, PackageBytes *int64
	HasOriginal                 *bool
}

// containers are the types of an item that has no file of its own, whose
// children have theirs: a series, a season and an album.
const containers = `('series', 'season', 'album')`

// mayHaveFile is the SQL condition that the item of the row alias is no
// container: one that may have a file of its own.
func mayHaveFile(alias string) string {
	return `lower(` + alias + `.type) NOT IN ` + containers
}

// presentOf is the SQL condition that the row o of the sources is a source of
// the title item present on disk. Every one counts: the parts of a version in
// parts, and an old file replaceSource kept beside the new one, present or
// superseded until the retire job retires it, which library.OriginalsOf (the
// retire job, the compliance check) does not take for the title's original,
// as it plays the other.
func presentOf(item string) string {
	return `o.item_id = ` + item + ` AND o.state IN ('present', 'superseded')`
}

// fileOf is the SQL condition that the row f of the playback assets is the
// file of the title item, with its size: its primary asset that is no
// package (the row of an original retired after packaging is of kind
// original, and no longer primary). With migration 040 (sources) it is one
// that names no source, of the legacy layout, whose library keeps none: a
// file the library knows has its source counted instead.
func fileOf(item string, sources bool) string {
	cond := `f.item_id = ` + item + ` AND f.isprimary = true AND COALESCE(f.kind, 'primary') = 'primary' AND f.sizebytes IS NOT NULL`
	if sources {
		cond += ` AND f.sourceid IS NULL`
	}
	return cond
}

// originalBytesOf is the SQL of the size of the originals the title item has
// on disk, null when it has none: the sum of its sources present, else the
// size of its file of no source; without migration 040, its file's.
func originalBytesOf(item string, sources bool) string {
	file := `(SELECT f.sizebytes FROM com_nalet_katalog_playbackassets f WHERE ` + fileOf(item, sources) + ` ORDER BY f.id LIMIT 1)`
	if !sources {
		return file
	}
	return `COALESCE((SELECT sum(o.sizebytes)::bigint FROM ` + SourcesTable + ` o WHERE ` + presentOf(item) + `), ` + file + `)`
}

// hasOriginalOf is the SQL condition that the item of the row alias has an
// original on disk: that its holder, or it, has a source present or a file of
// no source with its size, so that its originalBytesOf is not null. It is
// two EXISTS, for a filter to take as they are (NOT for none), and no
// comparison: the planner costs a page of such a filter at a small part of
// the catalog (25,000 of one of 78,000 titles), where a comparison, of the
// size ((… IS NOT NULL) = false) or of an EXISTS, it plans as a subquery of
// nearly every row (650,000 and more), enough to have the query compiled
// (JIT, from jit_above_cost, 100,000) for longer than it runs.
func hasOriginalOf(alias string, sources, covers bool) string {
	item := holderOf(alias, covers)
	file := `EXISTS (SELECT 1 FROM com_nalet_katalog_playbackassets f WHERE ` + fileOf(item, sources) + `)`
	if !sources {
		return `(` + file + `)`
	}
	return `(EXISTS (SELECT 1 FROM ` + SourcesTable + ` o WHERE ` + presentOf(item) + `) OR ` + file + `)`
}

// holderOf is the SQL of the item whose file is the file of the item of the
// row alias (of com_nalet_katalog_items): its holder where another episode's
// file covers it (coveredby, migration 045), else the item itself, as on a
// catalog without 045, where no episode is covered.
func holderOf(alias string, covers bool) string {
	if !covers {
		return alias + ".id"
	}
	return `COALESCE(` + alias + `.coveredby, ` + alias + `.id)`
}

// packageBytesOf is the SQL of the size of the title item's package, null
// when it has none: that of its packaged row, which packaging-complete and a
// migration's adopt write from its version's package.json (sizeBytes), the
// legacy packaging from its package's files, the first of them by id. The
// compliance check reads the same where it does not read the record itself,
// so the list and the rules report agree (ComplianceRow.packageBytes). A
// title taken in without a package has none.
func packageBytesOf(item string) string {
	return `(SELECT a.sizebytes FROM com_nalet_katalog_playbackassets a WHERE a.item_id = ` + item +
		` AND a.kind = 'packaged' ORDER BY a.id LIMIT 1)`
}

// hasOriginalFilter is the SQL condition of the list's filter on the row of
// katalogservice_items: the items that may have a file of their own and
// have an original on disk (has), or have none (!has); no container either
// way. The view has no coveredby (045 came after it), so the condition is
// held on the item's row.
func hasOriginalFilter(has, sources, covers bool) string {
	cond := hasOriginalOf("i", sources, covers)
	if !has {
		cond = `NOT ` + cond
	}
	return `id IN (SELECT i.id FROM com_nalet_katalog_items i WHERE ` + mayHaveFile("i") + ` AND ` + cond + `)`
}

// originalsReady reports whether migrations 040 and 045, which what a title
// has on disk reads, are in place, each remembered once it is.
func (s *Store) originalsReady(ctx context.Context) (sources, covers bool, err error) {
	if !s.sources.Load() {
		ok, err := s.LibraryReady(ctx)
		if err != nil {
			return false, false, err
		}
		s.sources.Store(ok)
	}
	if !s.covers.Load() {
		ok, err := s.MultiEpisodeFilesReady(ctx)
		if err != nil {
			return false, false, err
		}
		s.covers.Store(ok)
	}
	return s.sources.Load(), s.covers.Load(), nil
}

// sizesSQL is the query of SizesOf, of the items $1: of each whether it may
// have a file, the size of its originals and of its package, and whether it
// has an original. A page's few hundred rows plan far below the cost that
// has a plan compiled (jit_above_cost).
func sizesSQL(sources, covers bool) string {
	return `SELECT i.id, ` + mayHaveFile("i") + `, ` + originalBytesOf("i.id", sources) + `,
			` + packageBytesOf("i.id") + `, ` + hasOriginalOf("i", sources, covers) + `
		FROM com_nalet_katalog_items i WHERE i.id = ANY($1)`
}

// SizesOf reads what each of the items ids has on disk (ItemSizes), all of
// them in one query: the catalog's list asks it of a page. An id the catalog
// does not hold has nothing.
func (s *Store) SizesOf(ctx context.Context, ids []string) (map[string]ItemSizes, error) {
	out := make(map[string]ItemSizes, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	sources, covers, err := s.originalsReady(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, sizesSQL(sources, covers), ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var file, has bool
		var z ItemSizes
		if err := rows.Scan(&id, &file, &z.OriginalBytes, &z.PackageBytes, &has); err != nil {
			return nil, err
		}
		if file {
			z.HasOriginal = &has
		} else {
			z = ItemSizes{}
		}
		out[id] = z
	}
	return out, rows.Err()
}
