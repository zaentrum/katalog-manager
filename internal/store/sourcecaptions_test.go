package store_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/zaentrum/katalog-manager/internal/store/storetest"
)

// A catalog that applied an earlier shape of migration 058 (each one the
// pull request carried before its release) is brought to the final one at
// start: its reads, of an earlier analyzer, read again, the columns it lacks
// added, those of the package read dropped, its checks made again; applied
// again, it changes nothing of a read in the final shape.
func TestAnEarlierShapeOfTheCaptionsMigrationIsBroughtToItsLast(t *testing.T) {
	ctx := context.Background()
	shapes, err := filepath.Glob("testdata/058_before_*.sql")
	if err != nil || len(shapes) == 0 {
		t.Fatalf("the earlier shapes: %v %v", shapes, err)
	}
	for _, shape := range shapes {
		t.Run(filepath.Base(shape), func(t *testing.T) {
			sql, err := os.ReadFile(shape)
			if err != nil {
				t.Fatal(err)
			}
			st := storetest.OpenBase(t)
			if _, err := st.Pool().Exec(ctx, string(sql)); err != nil {
				t.Fatal(err)
			}
			// a read as that shape kept one
			storetest.Exec(t, st, map[string]string{
				"058_before_2dba39f.sql": `INSERT INTO com_nalet_katalog_sourcecaptions (source_id, item_id, state, captions,
					probe, packets, readat) VALUES ('s1', 'i1', 'read', false, 'ffprobe 7.1', 48, now())`,
				"058_before_8d42980.sql": `INSERT INTO com_nalet_katalog_sourcecaptions (source_id, item_id, state, captions,
					ffprobeversion, framesdecoded, readat) VALUES ('s1', 'i1', 'read', false, '7.1.1', 48, now())`,
				"058_before_bf8d910.sql": `INSERT INTO com_nalet_katalog_sourcecaptions (source_id, item_id, state, captions,
					captionwindows, ffprobeversion, framesdecoded, readat) VALUES ('s1', 'i1', 'read', false,
					'[{"at": 0.1, "start": 60, "end": 61.88, "frames": 48, "captions": false}]', '7.1.1', 48, now())`,
			}[filepath.Base(shape)])
			if ok, err := st.SourceCaptionsReady(ctx); err != nil || ok {
				t.Fatalf("an earlier shape is taken for the last: %v %v", ok, err)
			}
			if err := st.EnsureSourceCaptions(ctx); err != nil {
				t.Fatal(err)
			}
			if ok, err := st.SourceCaptionsReady(ctx); err != nil || !ok {
				t.Fatalf("after the migration: %v %v", ok, err)
			}
			if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_sourcecaptions
				WHERE state = 'read'`); n != 0 {
				t.Errorf("%d reads kept without windows stay read", n)
			}
			storetest.Exec(t, st, `UPDATE com_nalet_katalog_sourcecaptions SET state = 'read', captions = false,
				captionwindows = '[{"at": 0, "start": 0, "end": 1.88, "frames": 48, "captions": false}]',
				ffprobeversion = '7.1.1', framesdecoded = 48, duration = 600 WHERE source_id = 's1'`)
			if _, err := st.Pool().Exec(ctx, `UPDATE com_nalet_katalog_sourcecaptions SET state = 'read',
				captionwindows = NULL WHERE source_id = 's1'`); err == nil {
				t.Error("a read without windows is kept")
			}
			// applied again, it changes nothing of a read in its last shape
			if _, err := st.Pool().Exec(ctx, migrationSQL(t)); err != nil {
				t.Fatal(err)
			}
			if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_sourcecaptions WHERE state = 'read'`); n != 1 {
				t.Errorf("applied again, the migration changed a read (%d left)", n)
			}
		})
	}
}

func migrationSQL(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("../../db/migrations/058_source_captions.sql")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
