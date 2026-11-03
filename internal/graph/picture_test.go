package graph

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/zaentrum/katalog-manager/internal/library"
	"github.com/zaentrum/katalog-manager/internal/model"
	"github.com/zaentrum/katalog-manager/internal/picture"
	"github.com/zaentrum/katalog-manager/internal/store/storetest"
)

// A title says the part of its frame its picture fills, as the analyzer
// measured it, in pixels, and whether it is of what the title plays now; a
// title never measured says none. pictureAreas, an admin's, says how far the
// titles are, and why the backfill hands out nothing: it is off.
func TestAnItemSaysItsPictureArea(t *testing.T) {
	st := storetest.Open(t)
	storetest.AddItem(t, st, "m1", "movie", "A Film", "")
	storetest.AddItem(t, st, "m2", "movie", "Another Film", "")
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_playbackassets (id, item_id, path, isprimary, kind, sourceid) VALUES
		('a1', 'm1', '/media/m1.mkv', true, 'primary', 'src1'), ('a2', 'm2', '/media/m2.mkv', true, 'primary', 'src2')`)
	area := picture.Rect{X: 0, Y: 138, Width: 1920, Height: 804}
	if _, err := picture.Record(context.Background(), st, picture.KindItem, "m1", picture.Report{Path: "/media/m1.mkv",
		Method: "picture/1", Frame: &picture.Size{Width: 1920, Height: 1080}, Area: &area, CPUSeconds: f(60),
		DurationMs: i64(3_600_000)}, time.Date(2026, 11, 3, 3, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	m1, err := newItemResolver(&model.Item{ID: "m1", Type: "movie"}, st, library.Paths{}).PictureArea(ctx)
	if err != nil || m1 == nil {
		t.Fatalf("m1: %v, %v", m1, err)
	}
	got := []any{m1.State(), m1.Current(), *m1.FrameWidth(), *m1.FrameHeight(), *m1.X(), *m1.Y(), *m1.Width(),
		*m1.Height(), m1.Full(), string(*m1.SourceID()), m1.MeasuredAt().Time.Format(time.RFC3339)}
	want := []any{"measured", true, int32(1920), int32(1080), int32(0), int32(138), int32(1920), int32(804), false,
		"src1", "2026-11-03T03:00:00Z"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("m1: %v, want %v", got, want)
	}
	if m2, err := newItemResolver(&model.Item{ID: "m2", Type: "movie"}, st, library.Paths{}).PictureArea(ctx); err != nil || m2 != nil {
		t.Errorf("m2, never measured: %v, %v", m2, err)
	}

	schema := MustSchema(NewResolver(st, testConfig, Services{}))
	resp := schema.Exec(as(admin), `{
		pictureAreas { titles letterboxed full toMeasure cpuSecondsPerHour backfill window maxInFlight maxPerHour lease idle problems }
	}`, "", nil)
	if len(resp.Errors) > 0 {
		t.Fatal(resp.Errors)
	}
	wantJSON := `{"pictureAreas":{"titles":2,"letterboxed":1,"full":0,"toMeasure":1,"cpuSecondsPerHour":60,"backfill":false,` +
		`"window":"22:00-17:00 UTC","maxInFlight":1,"maxPerHour":240,"lease":"6h","idle":"picture.backfill is off","problems":[]}}`
	if got := string(resp.Data); got != wantJSON {
		t.Fatalf("got  %s\nwant %s", got, wantJSON)
	}
	// pictureAreas is an admin's, as every root field is.
	if resp := schema.Exec(as(viewer), `{ pictureAreas { titles } }`, "", nil); len(resp.Errors) == 0 ||
		!strings.Contains(resp.Errors[0].Message, "pictureAreas") {
		t.Errorf("a viewer read pictureAreas: %+v", resp)
	}
}

func f(v float64) *float64 { return &v }

func i64(v int64) *int64 { return &v }
