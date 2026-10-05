package store_test

import (
	"context"
	"strings"
	"testing"

	"github.com/zaentrum/katalog-manager/db/migrations"
	"github.com/zaentrum/katalog-manager/internal/store/storetest"
)

// 038 gives a subtitle isforced, false for one older than it; running it
// again changes nothing, and the startup check applies it where it is
// missing, and only then.
func TestSubtitleForcedMigration(t *testing.T) {
	st := storetest.OpenBase(t)
	ctx := context.Background()
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_subtitleassets (id, item_id, path) VALUES ('s1', 'm1', '/m/a.srt')`)
	if ready, err := st.SubtitleForcedReady(ctx); err != nil || ready {
		t.Fatalf("SubtitleForcedReady on a catalog without 038: %v, %v", ready, err)
	}
	for run := 1; run <= 2; run++ {
		if err := st.EnsureSubtitleForced(ctx); err != nil {
			t.Fatalf("EnsureSubtitleForced, %d. time: %v", run, err)
		}
	}
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_subtitleassets (id, item_id, path, isforced) VALUES ('s2', 'm1', '/p/0.vtt', true)`)
	for run := 3; run <= 4; run++ {
		if _, err := st.Pool().Exec(ctx, migrations.SubtitleForced); err != nil {
			t.Fatalf("applying 038 for the %d. time: %v", run, err)
		}
	}
	if ready, err := st.SubtitleForcedReady(ctx); err != nil || !ready {
		t.Fatalf("SubtitleForcedReady after EnsureSubtitleForced: %v, %v", ready, err)
	}
	if got := columns(t, st, "com_nalet_katalog_subtitleassets"); !strings.HasSuffix(got, "\nisforced boolean NOT NULL") {
		t.Errorf("subtitles after 038:\n%s\nwant isforced last", got)
	}
	if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_subtitleassets
		WHERE (id = 's1' AND NOT isforced) OR (id = 's2' AND isforced)`); n != 2 {
		t.Error("a subtitle older than 038 is not unforced, or applying 038 again changed one")
	}
}

// storetest.Open gives a test every migration the service applies at
// startup, 038 among them.
func TestTheTestSchemaHasTheSubtitleForced(t *testing.T) {
	if ready, err := storetest.Open(t).SubtitleForcedReady(context.Background()); err != nil || !ready {
		t.Fatalf("SubtitleForcedReady on the test schema: %v, %v", ready, err)
	}
}
