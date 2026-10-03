package tmdb

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/zaentrum/katalog-manager/internal/graph"
	"github.com/zaentrum/katalog-manager/internal/store/storetest"
)

const film3 = "f3f3f3f3-0000-4000-8000-000000000004"

// counts is a refresh result without its times, for comparing.
func counts(r graph.PeopleRefreshResult) graph.PeopleRefreshResult {
	r.StartedAt, r.FinishedAt = time.Time{}, time.Time{}
	return r
}

// The demo's catalog knows its people by name only: the backfill reads each
// title's credits again to give them their TMDB ids, then their details; a
// credit TMDB no longer lists goes, and so does its person when no title
// credits them any more. What it cannot do (a title whose credits fail, a
// person TMDB does not know, a locked record) it reports, and a second run
// without all picks up only what is left.
func TestRefreshPeopleBackfillsPeopleKnownByName(t *testing.T) {
	st := storetest.Open(t)
	f := newFakeTMDB(t)
	s := newTestService(t, st, f, "en-US")
	ctx := context.Background()
	addTitle(t, st, film1, "movie", "First Film", 10)
	addTitle(t, st, film2, "movie", "Second Film", 11)
	addTitle(t, st, show1, "series", "A Show", 20)
	addTitle(t, st, film3, "movie", "Third Film", 12)
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_people (id, name, metadatalocked) VALUES
		('ada', 'Ada Example', false), ('ben', 'Ben Example', false), ('cy', 'Cy Example', true),
		('dot', 'Dot Example', false), ('old', 'Old Credit', false), ('fay', 'Fay Example', false)`)
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_itempeople (id, item_id, person_id, role) VALUES
		('l1', $1, 'ada', 'actor'), ('l2', $1, 'ben', 'director'), ('l3', $2, 'cy', 'actor'),
		('l4', $2, 'old', 'actor'), ('l5', $3, 'ada', 'actor'), ('l6', $3, 'dot', 'actor'), ('l7', $4, 'fay', 'actor')`,
		film1, film2, show1, film3)
	f.cast("movie/10", []string{"101", "Ada Example"}, []string{"102", "Ben Example", "Director"})
	f.cast("movie/11", []string{"103", "Cy Example"}, []string{"105", "New Person"})
	f.tv(20, "A Show")
	f.cast("tv/20", []string{"101", "Ada Example"}, []string{"104", "Dot Example"})
	f.failing("/3/movie/12/credits", 500)
	for id, name := range map[int64]string{101: "Ada Example", 102: "Ben Example", 103: "Cy Example", 105: "New Person"} {
		f.person(id, &fakePerson{Name: name, Bio: map[string]string{"en": "About " + name + "."}})
	} // 104 is a person TMDB does not know

	res, err := s.RefreshPeople(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	// The five credits that stay take what TMDB says of them (their order, a job).
	if got, want := counts(res), (graph.PeopleRefreshResult{TitlesRead: 3, TitlesFailed: 1, PeopleMatched: 4,
		PeopleCreated: 1, CreditsAdded: 1, CreditsUpdated: 5, CreditsDropped: 1, PeopleDeleted: 1, PeopleFetched: 3,
		PeopleLocked: 1, PeopleNotFound: 1, PeopleWithoutTmdbID: 1}); got != want {
		t.Errorf("first run:\n got  %+v\n want %+v", got, want)
	}
	if res.StartedAt.IsZero() || res.FinishedAt.Before(res.StartedAt) {
		t.Errorf("run from %s to %s", res.StartedAt, res.FinishedAt)
	}
	for id, want := range map[string]string{"ada": "101", "ben": "102", "cy": "103", "dot": "104", "fay": "-"} {
		if got := tmdbIDOf(t, st, id); got != want {
			t.Errorf("%s has TMDB id %s, want %s", id, got, want)
		}
	}
	if d, ok := storetest.Deleted(t, st, "old"); !ok || d.Type != "person" || d.Title != "Old Credit" ||
		d.DeletedBy != "katalog-manager/tmdb" || d.Reason == nil || !strings.Contains(*d.Reason, `"Second Film"`) {
		t.Errorf("the person TMDB no longer credits and no title credits: log row %+v (%v)", d, ok)
	}
	samePerson(t, st, "ada", `{"name": "Ada Example", "sortName": null, "aka": null, "birth": null, "death": null,
		"place": null, "bio": {"en": "About Ada Example."}, "tmdb": "101", "imdb": null, "dept": null, "locked": false,
		"lockedFields": null, "origins": {"name": "tmdb", "biography": "tmdb", "externalIds": "tmdb"},
		"changed": null, "fetched": true}`)
	if got, want := credits(t, st, film2), "actor Cy Example (103), actor New Person (105)"; got != want {
		t.Errorf("second film: %s, want %s (exactly TMDB's)", got, want)
	}

	// Again, without all: only what is left.
	f.forget()
	res, err = s.RefreshPeople(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := counts(res), (graph.PeopleRefreshResult{TitlesFailed: 1, PeopleLocked: 1,
		PeopleNotFound: 1, PeopleWithoutTmdbID: 1}); got != want {
		t.Errorf("second run:\n got  %+v\n want %+v", got, want)
	}
	if got := f.calls("/3/person/101"); len(got) != 0 {
		t.Errorf("a person already read was read again: %q", got)
	}

	// With all, and the third film's credits back: everything again.
	f.failing("/3/movie/12/credits", 0)
	f.cast("movie/12", []string{"106", "Fay Example"})
	f.person(106, &fakePerson{Name: "Fay Example"})
	res, err = s.RefreshPeople(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := counts(res), (graph.PeopleRefreshResult{TitlesRead: 4, PeopleMatched: 1, CreditsUpdated: 1,
		PeopleFetched: 4, PeopleLocked: 1, PeopleNotFound: 1}); got != want {
		t.Errorf("run with all:\n got  %+v\n want %+v", got, want)
	}
}

// One refresh runs at a time, it needs TMDB and the people migration, and a
// caller who stops waiting does not stop it.
func TestRefreshPeopleRunsOnceAndToTheEnd(t *testing.T) {
	st := storetest.Open(t)
	f := newFakeTMDB(t)
	s := newTestService(t, st, f, "en-US")
	addTitle(t, st, film1, "movie", "First Film", 10)
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_people (id, name) VALUES ('ada', 'Ada Example')`)
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_itempeople (id, item_id, person_id, role) VALUES ('l1', $1, 'ada', 'actor')`, film1)
	f.cast("movie/10", []string{"101", "Ada Example"})
	f.person(101, &fakePerson{Name: "Ada Example"})
	arrived, release := f.hold("/3/movie/10/credits")
	defer release()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := s.RefreshPeople(ctx, false)
		done <- err
	}()
	<-arrived
	if _, err := s.RefreshPeople(context.Background(), true); err == nil || !strings.Contains(err.Error(), "already running") {
		t.Fatalf("a second refresh while one runs: %v, want it refused", err)
	}
	cancel() // the caller gives up
	release()
	if err := <-done; err != nil {
		t.Fatalf("the first refresh: %v", err)
	}
	if got := tmdbIDOf(t, st, "ada"); got != "101" {
		t.Errorf("a refresh whose caller stopped waiting did not finish: Ada has TMDB id %s", got)
	}
	if _, err := s.RefreshPeople(context.Background(), false); err != nil {
		t.Errorf("a refresh after the first finished: %v", err)
	}

	off := newTestService(t, st, f, "en-US")
	off.tmdb.key = func() string { return "" }
	if _, err := off.RefreshPeople(context.Background(), true); err == nil {
		t.Error("a refresh without a TMDB key ran")
	}
	base := storetest.OpenBase(t)
	if _, err := newTestService(t, base, f, "en-US").RefreshPeople(context.Background(), true); err == nil {
		t.Error("a refresh on a catalog without the people migration ran")
	}
}
