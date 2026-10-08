package main

import (
	"context"
	"testing"

	"github.com/zaentrum/katalog-manager/internal/processing"
	"github.com/zaentrum/katalog-manager/internal/store/storetest"
)

// An item enriched whose file is a disc image goes no further: no analysis
// is sent, and its steps that read its file fail for good, saying why. One
// with a file of another kind, and a series with none, are no disc image.
func TestAnEnrichedDiscImageIsNotAnalyzed(t *testing.T) {
	st := storetest.Open(t)
	ctx := context.Background()
	storetest.AddItem(t, st, "disc", "movie", "On A Disc", "")
	storetest.AddItem(t, st, "film", "movie", "A Film", "")
	storetest.AddItem(t, st, "show", "series", "A Show", "")
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_playbackassets (id, item_id, path, isprimary) VALUES
		('a1', 'disc', '/media/On A Disc.iso', true), ('a2', 'film', '/media/A Film.mkv', true)`)
	steps := processing.New(st.Pool())
	if !discImage(ctx, st, steps, "disc") || discImage(ctx, st, steps, "film") || discImage(ctx, st, steps, "show") {
		t.Fatal("discImage does not tell the disc image alone")
	}
	if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_itemprocessingsteps WHERE item_id = 'disc'
		AND step IN ('transcode', 'package') AND status = 'failed' AND error = $1 AND nextretryat IS NULL`, processing.DiscImageReason); n != 2 {
		t.Errorf("%d of the disc image's transcode and package failed for good, want both", n)
	}
	if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_itemprocessingsteps WHERE item_id <> 'disc'`); n != 0 {
		t.Errorf("%d steps of the others were written", n)
	}
}
