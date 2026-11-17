package library

import (
	"context"

	"github.com/zaentrum/katalog-manager/internal/store"
)

// What a package can't show keeps its original too, until it is known
// (ADR-0015, decision 2): a title's chapters, which live in the catalog and
// are read from the original (migration 055), and its picture area, which a
// package's keyframes can't tell from a letterbox (migration 054). A
// retirement waits for both; a catalog without the migrations keeps every
// original. The chapters and picture packages answer the same of a title
// (their NeedsOriginal), and their backfills read and measure the titles
// that wait first.

// ChaptersNeedOriginal reports whether the item itemID still needs its
// original sourceID ("" for its current one) for its chapters: it is among
// com_nalet_katalog_chaptersmissing, of that original. ready is false on a
// catalog without migration 055, which needs every original.
func ChaptersNeedOriginal(ctx context.Context, q Querier, itemID, sourceID string) (needs, ready bool, err error) {
	if ready, err = relationReady(ctx, q, store.ChaptersMissingView); err != nil || !ready {
		return true, ready, err
	}
	err = q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM `+store.ChaptersMissingView+` m
		WHERE m.item_id = $1 AND ($2 = '' OR m.sourceid = $2))`, itemID, sourceID).Scan(&needs)
	return needs, true, err
}

// PictureAreaNeedsOriginal reports whether the item itemID still needs its
// original sourceID for its picture area: it has no area of what it plays now
// measured of an original (measuredon original) that says the picture's area
// (measured) — a package's keyframes can't tell a letterbox from picture
// (ADR-0015) —, and none measured of that original that says it can't tell
// (unknown) either. A covered episode is answered its
// holder's. ready is false on a catalog without migration 054, which needs
// every original.
func PictureAreaNeedsOriginal(ctx context.Context, q Querier, itemID, sourceID string) (needs, ready bool, err error) {
	if ready, err = relationReady(ctx, q, store.CurrentPictureAreasView); err != nil || !ready {
		return true, ready, err
	}
	holder, err := HolderOrSelf(ctx, q, itemID)
	if err != nil {
		return true, true, err
	}
	var settled bool
	err = q.QueryRow(ctx, `SELECT `+pictureAreaSettled("$1", "$2"), holder, sourceID).Scan(&settled)
	return !settled, true, err
}

// pictureAreaSettled is the SQL condition that the item item's picture area
// is settled for its original source: as PictureAreaNeedsOriginal tells it.
func pictureAreaSettled(item, source string) string {
	return `EXISTS (SELECT 1 FROM ` + store.CurrentPictureAreasView + ` c
		WHERE c.id = ` + item + ` AND c.kind = 'item'
		  AND c.measuredon = 'original' AND (c.state = 'measured' OR (c.state = 'unknown' AND c.sourceid = ` + source + `)))`
}

// chaptersSettled is the SQL condition that the item item's chapters don't
// need its original source: as ChaptersNeedOriginal tells it.
func chaptersSettled(item, source string) string {
	return `NOT EXISTS (SELECT 1 FROM ` + store.ChaptersMissingView + ` m WHERE m.item_id = ` + item + ` AND m.sourceid = ` +
		source + `)`
}

// unshownReady reports whether the catalog can tell what a package can't
// show: migrations 054 and 055 are applied.
func unshownReady(ctx context.Context, q Querier) (bool, error) {
	for _, rel := range []string{store.ChaptersMissingView, store.CurrentPictureAreasView} {
		if ok, err := relationReady(ctx, q, rel); err != nil || !ok {
			return false, err
		}
	}
	return true, nil
}

// Unshown says why the original sourceID of the item itemID is still needed
// for what its package can't show, "" when it isn't: the catalog lacks its
// chapters, or its picture area isn't measured, or the catalog can't tell
// either.
func Unshown(ctx context.Context, q Querier, itemID, sourceID string) (string, error) {
	needs, ready, err := ChaptersNeedOriginal(ctx, q, itemID, sourceID)
	switch {
	case err != nil:
		return "", err
	case !ready:
		return "the catalog can't tell its chapters (migration 055 is not applied): its original is kept", nil
	case needs:
		return unshownChapters, nil
	}
	needs, ready, err = PictureAreaNeedsOriginal(ctx, q, itemID, sourceID)
	switch {
	case err != nil:
		return "", err
	case !ready:
		return "the catalog can't tell its picture area (migration 054 is not applied): its original is kept", nil
	case needs:
		return unshownPicture, nil
	}
	return "", nil
}

// Why an original is kept for what its package can't show.
const (
	unshownChapters = "the catalog lacks its chapters: its original is kept until they are read"
	unshownPicture  = "its picture area is not measured on its original: its original is kept until it is"
)

// relationReady reports whether the table or view rel is there.
func relationReady(ctx context.Context, q Querier, rel string) (bool, error) {
	var ok bool
	err := q.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, rel).Scan(&ok)
	return ok, err
}
