package itemactions

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/zaentrum/katalog-manager/internal/config"
	"github.com/zaentrum/katalog-manager/internal/events"
	"github.com/zaentrum/katalog-manager/internal/library"
	"github.com/zaentrum/katalog-manager/internal/processing"
	"github.com/zaentrum/katalog-manager/internal/store/storetest"
)

// An episode another's file covers is packaged with that file: packageItem
// of it enqueues its holder, the result the holder's and saying so, and its
// own transcode is never made. A title whose file is a disc image is not
// packaged (ErrDiscImage), nor is a series' episode whose file is one.
func TestPackageItemOfACoveredEpisodeOrADiscImage(t *testing.T) {
	st := storetest.Open(t)
	ctx := context.Background()
	storetest.AddItem(t, st, series, "series", "A Series", "")
	storetest.AddItem(t, st, episode1, "episode", "Pilot", series)
	storetest.AddItem(t, st, episode2, "episode", "Pilot, Part Two", series)
	storetest.AddItem(t, st, "disc", "movie", "On A Disc", "")
	storetest.AddItem(t, st, "discep", "episode", "On A Disc Too", series)
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_playbackassets (id, item_id, path, isprimary) VALUES
		('a1', $1, '/media/Pilot.S01E01E02.mkv', true), ('a2', 'disc', '/media/Disc.ISO', true), ('a3', 'discep', '/media/Ep.img', true)`,
		episode1)
	if _, err := library.Link(ctx, st.Pool(), episode1, episode2); err != nil {
		t.Fatal(err)
	}
	b := &fakeBus{}
	svc := New(st, config.Config{}, processing.New(st.Pool()), nil)
	svc.events = b

	res, err := svc.PackageItem(ctx, episode2)
	if err != nil || res.Status == nil || *res.Status != "pending" || !strings.HasPrefix(*res.Message, library.CoveredNote(episode2, episode1)) {
		t.Fatalf("package the covered episode: %+v, %v", res, err)
	}
	if got := b.take(); got != events.TopicAnalyzed+" "+episode1+" episode transcode package" {
		t.Errorf("sent %q, want the holder's transcode", got)
	}
	if got := transcode(t, st, episode2); got != "not_applicable sent=false retry=false "+processing.CoveredReason(episode1) {
		t.Errorf("the covered episode's transcode: %s", got)
	}

	res, err = svc.PackageItem(ctx, "disc")
	if !errors.Is(err, ErrDiscImage) || res.Message == nil || !strings.Contains(*res.Message, processing.DiscImageReason) {
		t.Errorf("package a disc image: %+v, %v", res, err)
	}
	res, err = svc.PackageItem(ctx, series)
	if err != nil || *res.EpisodesEnqueued != 0 {
		t.Errorf("package the series, its holder busy and an episode a disc image: %+v, %v", res, err)
	}
	if got := b.take(); got != "" {
		t.Errorf("sent %q for a disc image", got)
	}
}
