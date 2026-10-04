package tmdb

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/zaentrum/katalog-manager/internal/ratings"
	"github.com/zaentrum/katalog-manager/internal/store"
	"github.com/zaentrum/katalog-manager/internal/store/storetest"
)

// The fixtures are TMDB's answers as its API reference records them: the
// release dates of film 550 and the content ratings of series 1399, the
// release notes that named a commercial service written as what they were
// (VOD, TV).
func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// A film's certifications are those of its theatrical and digital releases,
// per country in TMDB's order: a premiere, a limited, a physical or a TV
// release does not rate it, nor does a release without a certification.
func TestAFilmsCertificationsAreItsTheatricalAndDigitalReleases(t *testing.T) {
	certs, err := movieCertifications(fixture(t, "movie-550-release_dates.json"))
	if err != nil {
		t.Fatal(err)
	}
	for country, want := range map[string][]string{
		"CH": {"18", "18"},             // two theatrical releases, in German and in French
		"US": {"R"},                    // the premieres have none
		"NL": {"16"},                   // the DVD and the TV release do not count
		"FR": {"16", "16", "18", "16"}, // the theatrical and the four digital ones, one of them without
		"IN": {"A"},                    // a digital release alone
		"JP": {"PG12 "},                // as TMDB writes it
		"DE": {"18"},
		"IT": {"VM14"},
	} {
		if got := certs[country]; !reflect.DeepEqual(got, want) {
			t.Errorf("%s: %q, want %q", country, got, want)
		}
	}
	for _, country := range []string{"SK", "ID", "AE", "PL", "CZ"} { // releases without a certification, or none of type 3 or 4
		if got, ok := certs[country]; ok {
			t.Errorf("%s: %q, want none", country, got)
		}
	}
	for list, want := range map[string]string{
		ratings.DefaultCountries: "18 CH 18",
		"US,DE":                  "R US 17",
		"FR":                     "18 FR 18", // the strictest of its releases: a digital one
		"JP":                     "PG12 JP 12",
		"IT":                     "VM14 IT 14",
		"CA":                     "18A CA 18",
		"AU":                     "R18+ AU 18",
		"SG,MX":                  "M18 SG 18",
		"BE,HK,MY":               "", // certifications the table does not rate
		"AT":                     "",
	} {
		if got := chosen(ratings.Countries(list), certs); got != want {
			t.Errorf("film 550 in %s: %q, want %q", list, got, want)
		}
	}
}

// A series' certifications are its content ratings, one per country.
func TestASeriesCertificationsAreItsContentRatings(t *testing.T) {
	certs, err := tvCertifications(fixture(t, "tv-1399-content_ratings.json"))
	if err != nil {
		t.Fatal(err)
	}
	if got := certs["US"]; !reflect.DeepEqual(got, []string{"TV-MA"}) {
		t.Errorf("US: %q", got)
	}
	if len(certs) != 16 {
		t.Errorf("%d countries, want the 16 of the answer", len(certs))
	}
	for list, want := range map[string]string{
		ratings.DefaultCountries: "16 DE 16", // TMDB has no content rating of Switzerland
		"US":                     "TV-MA US 17",
		"GB":                     "18 GB 18",
		"KR":                     "19 KR 19",
		"SG":                     "R21 SG 21",
		"CA":                     "18+ CA 18",
		"IN,HU,MX":               "C MX 18",
	} {
		if got := chosen(ratings.Countries(list), certs); got != want {
			t.Errorf("series 1399 in %s: %q, want %q", list, got, want)
		}
	}
}

// An answer that is no JSON is an error; one without results rates nothing.
func TestCertificationAnswersTMDBMightGive(t *testing.T) {
	if _, err := movieCertifications([]byte(`<html>`)); err == nil {
		t.Error("a film's release dates that are no JSON: no error")
	}
	if _, err := tvCertifications([]byte(`{"results": "x"}`)); err == nil {
		t.Error("content ratings of the wrong shape: no error")
	}
	for _, body := range []string{`{}`, `{"results": null}`, `{"id": 9, "results": []}`,
		`{"results": [{"iso_3166_1": "DE", "release_dates": null}]}`} {
		if certs, err := movieCertifications([]byte(body)); err != nil || len(certs) != 0 {
			t.Errorf("release dates %s: %v %v, want none", body, certs, err)
		}
		if certs, err := tvCertifications([]byte(body)); err != nil || len(certs) != 0 {
			t.Errorf("content ratings %s: %v %v, want none", body, certs, err)
		}
	}
	certs, _ := tvCertifications([]byte(`{"results": [{"iso_3166_1": "de", "rating": " 12 "}, {"iso_3166_1": "US", "rating": ""}]}`))
	if !reflect.DeepEqual(certs, map[string][]string{"DE": {" 12 "}}) {
		t.Errorf("a lower-case country and an empty rating: %q", certs)
	}
}

func chosen(countries []string, certs map[string][]string) string {
	r, ok := ratings.Choose(countries, certs)
	if !ok {
		return ""
	}
	return fmt.Sprintf("%s %s %d", r.Certification, r.Country, r.MinAge)
}

// settingsOf is the setting lookup main wires: the settings table.
func settingsOf(st *store.Store) SettingLookup {
	return func(ctx context.Context, key string) (string, bool) {
		row, err := st.GetSettingByKey(ctx, key)
		if err != nil || row == nil {
			return "", false
		}
		return row.ValueText, true
	}
}

// rating is a title's rating as "certification country min_age", "-" for
// each unknown, with " fetched" when TMDB was read for it.
func rating(t *testing.T, st *store.Store, id string) string {
	t.Helper()
	var got string
	storetestScan(t, st, `SELECT concat_ws(' ', COALESCE(certification, '-'), COALESCE(certification_country, '-'),
			COALESCE(min_age::text, '-'), CASE WHEN certification_fetched_at IS NOT NULL THEN 'fetched' END)
		FROM com_nalet_katalog_items WHERE id = '`+id+`'`, &got)
	return got
}

// stamps are when a title was last modified, and when TMDB was last read for
// its certifications.
func stamps(t *testing.T, st *store.Store, id string) (modified, fetched time.Time) {
	t.Helper()
	storetestScan(t, st, `SELECT modifiedat, certification_fetched_at FROM com_nalet_katalog_items WHERE id = '`+id+`'`,
		&modified, &fetched)
	return modified, fetched
}

// Enriching a film rates it from TMDB's release dates, in the first country
// of ratings.countries that rates it (CH, DE, US when it names none), in one
// request; a title is rated whatever its locks, and modified with its rating.
func TestEnrichmentRatesAFilm(t *testing.T) {
	st := storetest.Open(t)
	f := newFakeTMDB(t)
	s := newTestService(t, st, f, "en-US")
	s.lookup = settingsOf(st)
	addTitle(t, st, film1, "movie", "Fight", 550)
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_items SET metadatalocked = true, modifiedat = '2001-01-01' WHERE id = $1`, film1)
	f.movie(550, "Fight")
	f.certify("movie/550", fixture(t, "movie-550-release_dates.json"))

	enrich(t, s, film1)
	if got := rating(t, st, film1); got != "18 CH 18 fetched" {
		t.Errorf("rated %q, want 18 in CH", got)
	}
	if got := f.calls("/3/movie/550/release_dates"); len(got) != 1 || got[0] != "/3/movie/550/release_dates?" {
		t.Errorf("release dates read with %q, want one request", got)
	}
	if modified, _ := stamps(t, st, film1); modified.Year() == 2001 {
		t.Errorf("a locked title rated anew kept modifiedat %s", modified)
	}

	// The setting's countries, in its order.
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_settings (id, key, valuetext) VALUES ('r', 'ratings.countries', 'us, de')`)
	enrich(t, s, film1)
	if got := rating(t, st, film1); got != "R US 17 fetched" {
		t.Errorf("with ratings.countries us, de: %q, want R in US", got)
	}
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_settings SET valuetext = 'FR' WHERE id = 'r'`)
	enrich(t, s, film1)
	if got := rating(t, st, film1); got != "18 FR 18 fetched" {
		t.Errorf("with ratings.countries FR: %q, want its strictest release, 18", got)
	}
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_settings SET valuetext = 'AT, BE' WHERE id = 'r'`)
	enrich(t, s, film1)
	if got := rating(t, st, film1); got != "- - - fetched" {
		t.Errorf("with countries that do not rate it: %q, want it unrated", got)
	}
}

// A series is rated from its content ratings, its episodes as it: they keep
// no certification of their own.
func TestEnrichmentRatesASeriesNotItsEpisodes(t *testing.T) {
	st := storetest.Open(t)
	f := newFakeTMDB(t)
	s := newTestService(t, st, f, "en-US")
	addTitle(t, st, show1, "series", "A Show", 1399)
	storetest.AddItem(t, st, "e1", "episode", "Pilot", show1)
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_items SET seasonnumber = 1, episodenumber = 1 WHERE id = 'e1'`)
	f.tv(1399, "A Show")
	f.certify("tv/1399", fixture(t, "tv-1399-content_ratings.json"))

	enrich(t, s, show1)
	if got := rating(t, st, show1); got != "16 DE 16 fetched" {
		t.Errorf("the series rated %q, want 16 in DE", got)
	}
	if got := rating(t, st, "e1"); got != "- - -" {
		t.Errorf("its episode rated %q, want nothing of its own", got)
	}
	if got := f.calls("/3/tv/1399/content_ratings"); len(got) != 1 {
		t.Errorf("content ratings read with %q, want one request", got)
	}
	if got := f.calls("/3/movie/"); len(got) != 0 {
		t.Errorf("a series' rating read a film's: %q", got)
	}
}

// A rating follows TMDB: an answer that rates the title in none of the
// countries leaves it unrated; the same answer again moves only when TMDB was
// read; TMDB failing, or not knowing the title, keeps the rating it had.
func TestARatingFollowsTMDB(t *testing.T) {
	st := storetest.Open(t)
	f := newFakeTMDB(t)
	s := newTestService(t, st, f, "en-US")
	addTitle(t, st, film1, "movie", "A Film", 10)
	f.movie(10, "A Film")
	f.certify("movie/10", []byte(`{"id": 10, "results": [{"iso_3166_1": "DE", "release_dates": [
		{"certification": "12", "type": 3}, {"certification": "16", "type": 5}]}]}`))

	enrich(t, s, film1)
	if got := rating(t, st, film1); got != "12 DE 12 fetched" {
		t.Fatalf("rated %q", got)
	}
	// Read again by itself, as backfillRatings reads it (enriching a title
	// modifies it anyway).
	ctx := context.Background()
	rate := func() {
		t.Helper()
		if _, err := s.rateTitle(ctx, film1, "movie", 10, ratings.Countries(ratings.DefaultCountries)); err != nil {
			t.Fatalf("rateTitle: %v", err)
		}
	}
	modified1, fetched1 := stamps(t, st, film1)
	rate()
	modified2, fetched2 := stamps(t, st, film1)
	if !fetched2.After(fetched1) || !modified2.Equal(modified1) {
		t.Errorf("the same rating again: fetched %s → %s, modified %s → %s; want fetched to move and modified not",
			fetched1, fetched2, modified1, modified2)
	}

	for _, broken := range []int{500, 404} {
		f.failing("/3/movie/10/release_dates", broken)
		if _, err := s.rateTitle(ctx, film1, "movie", 10, ratings.Countries(ratings.DefaultCountries)); err == nil {
			t.Errorf("TMDB answering %d: no error", broken)
		}
		enrich(t, s, film1) // the title is enriched all the same
		if got := rating(t, st, film1); got != "12 DE 12 fetched" {
			t.Errorf("TMDB answering %d: rated %q, want the rating kept", broken, got)
		}
		if _, fetched := stamps(t, st, film1); !fetched.Equal(fetched2) {
			t.Errorf("TMDB answering %d moved when it was read", broken)
		}
	}
	f.failing("/3/movie/10/release_dates", 0)

	f.certify("movie/10", []byte(`{"id": 10, "results": [{"iso_3166_1": "DE", "release_dates": [
		{"certification": "", "type": 3}, {"certification": "16", "type": 6}]}, {"iso_3166_1": "US",
		"release_dates": [{"certification": "NR", "type": 4}]}]}`))
	modified3, _ := stamps(t, st, film1)
	rate()
	if got := rating(t, st, film1); got != "- - - fetched" {
		t.Errorf("TMDB no longer rating it: %q, want it unrated", got)
	}
	if modified, _ := stamps(t, st, film1); !modified.After(modified3) {
		t.Error("a title whose rating went was not modified")
	}
}

// Without migration 036 a title is enriched as before, and its
// certifications are not read.
func TestEnrichmentWithoutTheRatingsMigration(t *testing.T) {
	st := storetest.Open(t)
	storetest.Exec(t, st, `DROP INDEX idx_items_rated_age`)
	storetest.Exec(t, st, `ALTER TABLE com_nalet_katalog_items DROP COLUMN min_age, DROP COLUMN min_age_override,
		DROP COLUMN certification, DROP COLUMN certification_country, DROP COLUMN certification_fetched_at`)
	f := newFakeTMDB(t)
	s := newTestService(t, st, f, "en-US")
	addTitle(t, st, film1, "movie", "Filename Title", 10)
	f.movie(10, "A Film")
	f.certify("movie/10", []byte(`{"results": [{"iso_3166_1": "DE", "release_dates": [{"certification": "12", "type": 3}]}]}`))

	enrich(t, s, film1)
	if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_items WHERE id = $1 AND title = 'A Film'`, film1); n != 1 {
		t.Error("the title was not enriched")
	}
	if got := f.calls("/3/movie/10/release_dates"); len(got) != 0 {
		t.Errorf("certifications read without anywhere to keep them: %q", got)
	}
	if got := strings.Join(f.calls("/3/movie/10?"), ","); got == "" {
		t.Error("the film's details were not read")
	}
}

// backfillRatings reads the certifications of the movies and series with a
// TMDB id that were never read, or of all of them, and rates each; a title
// TMDB cannot be read for is failed and keeps its rating, and the next run
// tries it again. Episodes and titles without a TMDB id are not read.
func TestBackfillRatings(t *testing.T) {
	st := storetest.Open(t)
	f := newFakeTMDB(t)
	s := newTestService(t, st, f, "en-US")
	ctx := context.Background()
	addTitle(t, st, film1, "movie", "Fight", 550)
	addTitle(t, st, "m2", "movie", "Read Before", 20)
	addTitle(t, st, "m3", "movie", "Nowhere Rated", 30)
	addTitle(t, st, "m4", "movie", "Broken", 40)
	addTitle(t, st, show1, "series", "A Show", 1399)
	storetest.AddItem(t, st, "e1", "episode", "Pilot", show1)
	storetest.AddItem(t, st, "m5", "movie", "No TMDB id", "")
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_items SET certification = '6', certification_country = 'DE', min_age = 6,
		certification_fetched_at = '2026-01-01' WHERE id = 'm2'`)
	f.certify("movie/550", fixture(t, "movie-550-release_dates.json"))
	f.certify("movie/20", []byte(`{"results": [{"iso_3166_1": "DE", "release_dates": [{"certification": "12", "type": 3}]}]}`))
	f.certify("movie/30", []byte(`{"results": [{"iso_3166_1": "GB", "release_dates": [{"certification": "15", "type": 3}]}]}`))
	f.certify("tv/1399", fixture(t, "tv-1399-content_ratings.json"))
	f.failing("/3/movie/40/release_dates", 503)

	res, err := s.BackfillRatings(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprintf("%d %d %d %d %v", res.TitlesRead, res.TitlesRated, res.TitlesUnrated, res.TitlesFailed, res.Countries); got != "3 2 1 1 [CH DE US]" {
		t.Errorf("the first run read/rated/unrated/failed %s, want 3 2 1 1 in CH, DE, US", got)
	}
	for id, want := range map[string]string{film1: "18 CH 18 fetched", show1: "16 DE 16 fetched", "m3": "- - - fetched",
		"m2": "6 DE 6 fetched", "m4": "- - -", "e1": "- - -", "m5": "- - -"} {
		if got := rating(t, st, id); got != want {
			t.Errorf("%s rated %q, want %q", id, got, want)
		}
	}
	if got := f.calls("/3/movie/20/"); len(got) != 0 {
		t.Errorf("a title read before was read again: %q", got)
	}
	if res.StartedAt.IsZero() || res.FinishedAt.Before(res.StartedAt) {
		t.Errorf("started %s, finished %s", res.StartedAt, res.FinishedAt)
	}

	// The next run reads what failed; all reads everything again.
	f.failing("/3/movie/40/release_dates", 0)
	f.certify("movie/40", []byte(`{"results": [{"iso_3166_1": "US", "release_dates": [{"certification": "PG-13", "type": 4}]}]}`))
	f.forget()
	if res, err = s.BackfillRatings(ctx, false); err != nil || res.TitlesRead != 1 || res.TitlesRated != 1 {
		t.Errorf("the second run: %+v %v, want the failed title read and rated", res, err)
	}
	if got := rating(t, st, "m4"); got != "PG-13 US 13 fetched" {
		t.Errorf("the title that failed, read again: %q", got)
	}
	if res, err = s.BackfillRatings(ctx, true); err != nil || res.TitlesRead != 5 || res.TitlesRated != 4 || res.TitlesUnrated != 1 {
		t.Errorf("all: %+v %v, want the five titles with a TMDB id read", res, err)
	}
	if got := rating(t, st, "m2"); got != "12 DE 12 fetched" {
		t.Errorf("a title read again with all: %q", got)
	}
}

// One backfill runs at a time; without a TMDB key, or without migration 036,
// there is none.
func TestBackfillRatingsRefuses(t *testing.T) {
	st := storetest.Open(t)
	f := newFakeTMDB(t)
	s := newTestService(t, st, f, "en-US")
	addTitle(t, st, film1, "movie", "Fight", 550)
	f.certify("movie/550", fixture(t, "movie-550-release_dates.json"))
	arrived, release := f.hold("/3/movie/550/release_dates")
	done := make(chan error, 1)
	go func() {
		_, err := s.BackfillRatings(context.Background(), true)
		done <- err
	}()
	<-arrived
	if _, err := s.BackfillRatings(context.Background(), true); err == nil || !strings.Contains(err.Error(), "already running") {
		t.Errorf("a second run while one runs: %v, want it refused", err)
	}
	release()
	if err := <-done; err != nil {
		t.Fatal(err)
	}

	s.tmdb.key = func() string { return "" }
	if _, err := s.BackfillRatings(context.Background(), true); err == nil || !strings.Contains(err.Error(), "TMDB API key") {
		t.Errorf("without a TMDB key: %v", err)
	}

	base := storetest.OpenBase(t)
	if _, err := newTestService(t, base, f, "en-US").BackfillRatings(context.Background(), true); err == nil ||
		!strings.Contains(err.Error(), "036_item_ratings.sql") {
		t.Errorf("without 036: %v", err)
	}
}
