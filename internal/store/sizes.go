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
// its holder has one.
type ItemSizes struct {
	OriginalBytes, PackageBytes *int64
	HasOriginal                 bool
}

// originalBytesOf is the SQL of the size of the originals the title item has
// on disk, null when it has none. With migration 040 they are its sources
// present, all of them (a version in parts has several), and, of a title
// whose file names no source (the legacy layout's: its library keeps none),
// that file; without 040, its file. A title's file is its primary asset that
// is no package; the row of an original retired after packaging is of kind
// original and no longer primary, and its source is deleted.
func originalBytesOf(item string, sources bool) string {
	file := `SELECT f.sizebytes FROM com_nalet_katalog_playbackassets f WHERE f.item_id = ` + item +
		` AND f.isprimary = true AND COALESCE(f.kind, 'primary') = 'primary'`
	if !sources {
		return `(` + file + ` ORDER BY f.id LIMIT 1)`
	}
	return `COALESCE((SELECT sum(o.sizebytes)::bigint FROM ` + SourcesTable + ` o WHERE o.item_id = ` + item +
		` AND o.state = 'present'), (` + file + ` AND f.sourceid IS NULL ORDER BY f.id LIMIT 1))`
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

// hasOriginalOf is the SQL condition that the item of the row alias has an
// original on disk: that its holder, or it, has its originalBytesOf.
func hasOriginalOf(alias string, sources, covers bool) string {
	return originalBytesOf(holderOf(alias, covers), sources) + ` IS NOT NULL`
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
	rows, err := s.pool.Query(ctx, `SELECT i.id, `+originalBytesOf("i.id", sources)+`, `+packageBytesOf("i.id")+`,
			`+hasOriginalOf("i", sources, covers)+`
		FROM com_nalet_katalog_items i WHERE i.id = ANY($1)`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var z ItemSizes
		if err := rows.Scan(&id, &z.OriginalBytes, &z.PackageBytes, &z.HasOriginal); err != nil {
			return nil, err
		}
		out[id] = z
	}
	return out, rows.Err()
}
