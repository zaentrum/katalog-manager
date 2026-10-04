package graph

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/zaentrum/katalog-manager/internal/store"
	"github.com/zaentrum/katalog-manager/internal/store/storetest"
)

const ageRatingFields = `ageRating { certification country minAge minAgeOverride effectiveMinAge fetchedAt }`

// ratedCatalog holds a film TMDB rated, one it did not, a series rated with
// an episode rated as it and one an admin rated by hand, and an episode
// whose series is not in the catalog.
func ratedCatalog(t *testing.T) *store.Store {
	t.Helper()
	st := storetest.Open(t)
	withItemView(t, st)
	storetest.AddItem(t, st, "m1", "movie", "A Film", "")
	storetest.AddItem(t, st, "m2", "movie", "Unrated", "")
	storetest.AddItem(t, st, "s1", "series", "A Series", "")
	storetest.AddItem(t, st, "e1", "episode", "Pilot", "s1")
	storetest.AddItem(t, st, "e2", "episode", "Finale", "s1")
	storetest.AddItem(t, st, "e3", "episode", "Orphan", "gone")
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_items SET certification = '12', certification_country = 'DE',
		min_age = 12, certification_fetched_at = '2026-10-01 08:00:00+00' WHERE id = 'm1'`)
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_items SET certification_fetched_at = '2026-10-01 08:00:00+00' WHERE id = 'm2'`)
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_items SET certification = 'TV-14', certification_country = 'US',
		min_age = 14 WHERE id = 's1'`)
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_items SET min_age_override = 18 WHERE id = 'e2'`)
	return st
}

// A title's age rating is what TMDB certifies it as, in its country, with
// the age it means; an episode is held to its series' unless an admin rated
// it, and one whose series is not there is unrated.
func TestAgeRating(t *testing.T) {
	st := ratedCatalog(t)
	for id, want := range map[string]string{
		"m1": `{"certification":"12","country":"DE","minAge":12,"minAgeOverride":null,"effectiveMinAge":12,"fetchedAt":"2026-10-01T08:00:00Z"}`,
		"m2": `{"certification":null,"country":null,"minAge":null,"minAgeOverride":null,"effectiveMinAge":null,"fetchedAt":"2026-10-01T08:00:00Z"}`,
		"s1": `{"certification":"TV-14","country":"US","minAge":14,"minAgeOverride":null,"effectiveMinAge":14,"fetchedAt":null}`,
		"e1": `{"certification":null,"country":null,"minAge":null,"minAgeOverride":null,"effectiveMinAge":14,"fetchedAt":null}`,
		"e2": `{"certification":null,"country":null,"minAge":null,"minAgeOverride":18,"effectiveMinAge":18,"fetchedAt":null}`,
		"e3": `{"certification":null,"country":null,"minAge":null,"minAgeOverride":null,"effectiveMinAge":null,"fetchedAt":null}`,
	} {
		if got := query(t, st, `{ item(id: "`+id+`") { `+ageRatingFields+` } }`); got != `{"item":{"ageRating":`+want+`}}` {
			t.Errorf("%s:\n got  %s\n want %s", id, got, want)
		}
	}
	// A list reads each title's rating.
	if got, want := query(t, st, `{ series { id ageRating { effectiveMinAge } children { id ageRating { effectiveMinAge } } } }`),
		`{"series":[{"id":"s1","ageRating":{"effectiveMinAge":14},"children":[{"id":"e2","ageRating":{"effectiveMinAge":18}},`+
			`{"id":"e1","ageRating":{"effectiveMinAge":14}}]}]}`; got != want {
		t.Errorf("series:\n got  %s\n want %s", got, want)
	}
}

// An admin rates a title by hand: the age wins over TMDB's, a series' over
// its episodes' that have none of their own, and null clears it. An age is 0
// to 21; a title there is not is null.
func TestSetMinAgeOverride(t *testing.T) {
	st := ratedCatalog(t)
	schema := MustSchema(NewResolver(st, testConfig, Services{}))
	exec := func(q string) (string, string) {
		t.Helper()
		resp := schema.Exec(as(admin), q, "", nil)
		errs := make([]string, 0, len(resp.Errors))
		for _, e := range resp.Errors {
			errs = append(errs, e.Message)
		}
		return string(resp.Data), strings.Join(errs, "; ")
	}

	got, errs := exec(`mutation { setMinAgeOverride(id: "s1", minAge: 6) { id ageRating { minAge minAgeOverride effectiveMinAge } } }`)
	if want := `{"setMinAgeOverride":{"id":"s1","ageRating":{"minAge":14,"minAgeOverride":6,"effectiveMinAge":6}}}`; got != want || errs != "" {
		t.Errorf("rating a series 6:\n got  %s %s\n want %s", got, errs, want)
	}
	for id, want := range map[string]string{"e1": "6", "e2": "18", "m1": "12"} {
		if got := query(t, st, `{ item(id: "`+id+`") { ageRating { effectiveMinAge } } }`); got != `{"item":{"ageRating":{"effectiveMinAge":`+want+`}}}` {
			t.Errorf("%s after its series was rated by hand: %s, want %s", id, got, want)
		}
	}
	if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_items WHERE id = 's1'
		AND modifiedby = 'admin-1' AND modifiedat > now() - interval '1 minute'`); n != 1 {
		t.Error("rating a title by hand does not modify it as the admin")
	}

	if got, errs := exec(`mutation { setMinAgeOverride(id: "s1") { ageRating { minAgeOverride effectiveMinAge } } }`); got != `{"setMinAgeOverride":{"ageRating":{"minAgeOverride":null,"effectiveMinAge":14}}}` || errs != "" {
		t.Errorf("clearing it: %s %s", got, errs)
	}
	if got, errs := exec(`mutation { setMinAgeOverride(id: "e1", minAge: 0) { ageRating { effectiveMinAge } } }`); got != `{"setMinAgeOverride":{"ageRating":{"effectiveMinAge":0}}}` || errs != "" {
		t.Errorf("rating an episode 0: %s %s", got, errs)
	}
	for _, age := range []string{"-1", "22"} {
		if _, errs := exec(`mutation { setMinAgeOverride(id: "m1", minAge: ` + age + `) { id } }`); !strings.Contains(errs, "0 to 21") {
			t.Errorf("an age of %s: %q, want it refused", age, errs)
		}
	}
	if got, errs := exec(`mutation { setMinAgeOverride(id: "nothing", minAge: 12) { id } }`); got != `{"setMinAgeOverride":null}` || errs != "" {
		t.Errorf("a title there is not: %s %s", got, errs)
	}
	if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_items WHERE id = 'm1' AND min_age_override IS NULL`); n != 1 {
		t.Error("an age refused was stored")
	}
}

// On a catalog without migration 036 every title is unrated, and nothing can
// be rated by hand.
func TestAgeRatingWithoutTheMigration(t *testing.T) {
	st := storetest.OpenBase(t)
	withItemView(t, st)
	storetest.AddItem(t, st, "m1", "movie", "A Film", "")
	if got, want := query(t, st, `{ item(id: "m1") { `+ageRatingFields+` } }`), `{"item":{"ageRating":{"certification":null,`+
		`"country":null,"minAge":null,"minAgeOverride":null,"effectiveMinAge":null,"fetchedAt":null}}}`; got != want {
		t.Errorf("without 036:\n got  %s\n want %s", got, want)
	}
	resp := MustSchema(NewResolver(st, testConfig, Services{})).Exec(as(admin),
		`mutation { setMinAgeOverride(id: "m1", minAge: 12) { id } }`, "", nil)
	if len(resp.Errors) != 1 || !strings.Contains(resp.Errors[0].Message, "036_item_ratings.sql") {
		t.Errorf("rating by hand without 036: %v, want an error naming the migration", resp.Errors)
	}
}

// fakeRatings records the call the backfillRatings mutation makes.
type fakeRatings struct{ all *bool }

func (f *fakeRatings) BackfillRatings(_ context.Context, all bool) (RatingsBackfillResult, error) {
	f.all = &all
	return RatingsBackfillResult{TitlesRead: 7, TitlesRated: 5, TitlesUnrated: 2, TitlesFailed: 1,
		Countries: []string{"CH", "DE", "US"}, StartedAt: time.Date(2026, 10, 4, 8, 0, 0, 0, time.UTC),
		FinishedAt: time.Date(2026, 10, 4, 8, 0, 3, 0, time.UTC)}, nil
}

// backfillRatings runs the backfill (all defaults to false) and reports its
// counts and the countries it asked.
func TestBackfillRatingsMutation(t *testing.T) {
	f := &fakeRatings{}
	schema := MustSchema(NewResolver(nil, testConfig, Services{Ratings: f}))
	for q, all := range map[string]bool{
		`mutation { backfillRatings { titlesRead titlesRated titlesUnrated titlesFailed countries startedAt finishedAt } }`:            false,
		`mutation { backfillRatings(all: true) { titlesRead titlesRated titlesUnrated titlesFailed countries startedAt finishedAt } }`: true,
	} {
		resp := schema.Exec(as(admin), q, "", nil)
		want := `{"backfillRatings":{"titlesRead":7,"titlesRated":5,"titlesUnrated":2,"titlesFailed":1,` +
			`"countries":["CH","DE","US"],"startedAt":"2026-10-04T08:00:00Z","finishedAt":"2026-10-04T08:00:03Z"}}`
		if len(resp.Errors) > 0 || string(resp.Data) != want || f.all == nil || *f.all != all {
			t.Errorf("%s: %s %v (all %v), want %s with all %v", q, resp.Data, resp.Errors, f.all, want, all)
		}
	}
	resp := MustSchema(NewResolver(nil, testConfig, Services{})).Exec(as(admin), `mutation { backfillRatings { titlesRead } }`, "", nil)
	if len(resp.Errors) != 1 || resp.Errors[0].Message != errNotConfigured.Error() {
		t.Errorf("without the enricher: %v", resp.Errors)
	}
}
