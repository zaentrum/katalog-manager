package store

import (
	"context"

	"github.com/zaentrum/katalog-manager/db/migrations"
)

// The tables and the view db/migrations/055_chapter_reads.sql creates: the
// last read of each title's chapters, the backfill's claims, and the titles
// whose original is still needed for their chapters.
const (
	ChapterReadsTable   = "com_nalet_katalog_chapterreads"
	ChapterClaimsTable  = "com_nalet_katalog_chapterclaims"
	ChaptersMissingView = "com_nalet_katalog_chaptersmissing"
)

// chapterReadRelations are the tables, the view and the indexes 055 creates.
var chapterReadRelations = []string{ChapterReadsTable, ChapterClaimsTable, ChaptersMissingView,
	"idx_chapterreads_claimed", "idx_chapterreads_recordat", "idx_chapterclaims_claimedat"}

// ChapterReadsReady reports whether migration 055 is in place.
func (s *Store) ChapterReadsReady(ctx context.Context) (bool, error) {
	return chapterReadsReady(ctx, s.pool)
}

// chapterReadsReady is ChapterReadsReady read by q: the pool, or the
// transaction that holds the migrations' lock.
func chapterReadsReady(ctx context.Context, q querier) (bool, error) {
	var ok bool
	err := q.QueryRow(ctx, `SELECT (SELECT count(*) FROM unnest($1::text[]) AS r(name)
		WHERE to_regclass(r.name) IS NOT NULL) = cardinality($1::text[])`, chapterReadRelations).Scan(&ok)
	return ok, err
}

// EnsureChapterReads applies db/migrations/055_chapter_reads.sql when any of
// its objects is missing, under the migrations' lock (applyLocked). Its view
// reads the originals (040) and the compliance check's records (051), so it
// runs after EnsureCompliance. Without it a read of a title's chapters keeps
// its chapters and nothing else, and no backfill reads them again.
func (s *Store) EnsureChapterReads(ctx context.Context) error {
	return s.applyLocked(ctx, chapterReadsReady, migrations.ChapterReads)
}
