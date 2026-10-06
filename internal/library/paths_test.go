package library

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/zaentrum/katalog-manager/internal/config"
	"github.com/zaentrum/katalog-manager/internal/store/storetest"
)

const (
	movieID   = "f001aeff-9c18-4183-b51b-51403af2515e"
	seriesID  = "5e5e5e5e-0000-4000-8000-000000000001"
	seasonID  = "5ea50000-0000-4000-8000-000000000002"
	episodeID = "e1e1e1e1-0000-4000-8000-000000000003"
	nestedID  = "e2e2e2e2-0000-4000-8000-000000000004"
)

var testPaths = PathsOf(config.Config{LibraryRoot: "/var/lib/katalog", WorkRoot: "/var/lib/katalog/.work",
	ArrivalsRoot: "/var/lib/katalog/.work/incoming", ExtrasRoot: "/var/lib/katalog/.work/extras"})

// Every path of the record and of the work folder by the rules: a movie and a
// series by their shard, an episode in its series' folder, a person in
// people/, a source, a version, an extra and an event in their item's, the
// inbox, the staging and the trash in the work folder.
func TestThePathRules(t *testing.T) {
	p := testPaths
	at := time.Date(2026, 10, 6, 8, 30, 15, 999, time.FixedZone("CEST", 2*3600))
	movie := p.ItemDir(Place{ID: movieID, Type: "movie"})
	for got, want := range map[string]string{
		movie: "/var/lib/katalog/movies/f0/" + movieID,
		p.ItemDir(Place{ID: seriesID, Type: "series"}):                       "/var/lib/katalog/series/5e/" + seriesID,
		p.ItemDir(Place{ID: episodeID, Type: "episode", SeriesID: seriesID}): "/var/lib/katalog/series/5e/" + seriesID + "/episodes/" + episodeID,
		p.PersonDir("a1a1a1a1-0000-4000-8000-000000000001"):                  "/var/lib/katalog/people/a1/a1a1a1a1-0000-4000-8000-000000000001",
		SourceDir(movie, "0b6c"):                                             "/var/lib/katalog/movies/f0/" + movieID + "/sources/0b6c",
		VersionDir(movie, "9a2e"):                                            "/var/lib/katalog/movies/f0/" + movieID + "/versions/9a2e",
		ExtraDir(movie, "16aa"):                                              "/var/lib/katalog/movies/f0/" + movieID + "/extras/16aa",
		EventDir(movie, at, "1f2e3d4c-0000-4000-8000-000000000001", EventOriginalDeleted): "/var/lib/katalog/movies/f0/" + movieID +
			"/events/20261006T063015Z-1f2e3d4c-original-deleted",
		p.InboxDir(movieID):           "/var/lib/katalog/.work/inbox/" + movieID,
		p.ExtraInboxDir("16aa"):       "/var/lib/katalog/.work/inbox/extra-16aa",
		p.StagingDir("9a2e"):          "/var/lib/katalog/.work/staging/9a2e",
		p.ExtraStagingDir("16aa"):     "/var/lib/katalog/.work/staging/extra-16aa",
		p.TrashDir(at, "0b6c"):        "/var/lib/katalog/.work/trash/20261006/0b6c",
		p.ExtraTrashDir(at, "16aa"):   "/var/lib/katalog/.work/trash/20261006/extra-16aa",
		p.ReplaceDir():                "/var/lib/katalog/.work/replace",
		p.MigrationDir("2026-10-07a"): "/var/lib/katalog/.work/migration/2026-10-07a",
		p.LegacyDir():                 "/var/lib/katalog/.work/legacy",
	} {
		if got != want {
			t.Errorf("%s, want %s", got, want)
		}
	}
	if !Within("/var/lib/katalog/.work", "/var/lib/katalog/.work/inbox/x") || Within("/var/lib/katalog/.work", "/var/lib/katalog/.workx") ||
		Within("/", "/etc") || Within("/var/lib/katalog", "/var/lib/katalog") {
		t.Error("Within")
	}
}

// Where an item goes is read from the catalog: a movie, a series, an episode
// in its series' folder, under a season of it too (its series is its
// parent's parent). Anything else has no folder: an episode without a series,
// one under a parent that is no series nor a season of one, a season itself,
// an id that is no lower-case UUID.
func TestPlaceOf(t *testing.T) {
	st := storetest.Open(t)
	ctx := context.Background()
	storetest.AddItem(t, st, movieID, "movie", "Sintel", "")
	storetest.AddItem(t, st, seriesID, "series", "Pioneer One", "")
	storetest.AddItem(t, st, seasonID, "season", "Season 1", seriesID)
	storetest.AddItem(t, st, episodeID, "episode", "Earthfall", seriesID)
	storetest.AddItem(t, st, nestedID, "episode", "Alone in the Night", seasonID)
	storetest.AddItem(t, st, "e3e3e3e3-0000-4000-8000-000000000005", "episode", "Orphan", "")
	storetest.AddItem(t, st, "e4e4e4e4-0000-4000-8000-000000000006", "episode", "Under a film", movieID)
	storetest.AddItem(t, st, "AAAAAAAA-0000-4000-8000-000000000007", "movie", "Upper case", "")
	for id, want := range map[string]string{
		movieID:   "/var/lib/katalog/movies/f0/" + movieID,
		seriesID:  "/var/lib/katalog/series/5e/" + seriesID,
		episodeID: "/var/lib/katalog/series/5e/" + seriesID + "/episodes/" + episodeID,
		nestedID:  "/var/lib/katalog/series/5e/" + seriesID + "/episodes/" + nestedID,
	} {
		pl, err := PlaceOf(ctx, st.Pool(), id)
		if err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		if got := testPaths.ItemDir(pl); got != want {
			t.Errorf("%s: %s, want %s", id, got, want)
		}
	}
	for id, says := range map[string]string{
		seasonID:                               "a season is not recorded in the library",
		"e3e3e3e3-0000-4000-8000-000000000005": "an episode needs its series before it is recorded",
		"e4e4e4e4-0000-4000-8000-000000000006": "is no series nor a season of one",
		"AAAAAAAA-0000-4000-8000-000000000007": "a lower-case UUID",
	} {
		_, err := PlaceOf(ctx, st.Pool(), id)
		var u *Unplaced
		if !errors.As(err, &u) || !strings.Contains(u.Reason, says) {
			t.Errorf("%s: %v, want it unplaced saying %q", id, err, says)
		}
	}
	if _, err := PlaceOf(ctx, st.Pool(), "nothing"); !errors.Is(err, ErrNoItem) {
		t.Errorf("an item there is not: %v", err)
	}
}
