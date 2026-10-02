package tmdb

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/zaentrum/katalog-manager/internal/store"
	"github.com/zaentrum/katalog-manager/internal/store/storetest"
)

const (
	film1 = "f1f1f1f1-0000-4000-8000-000000000001"
	film2 = "f2f2f2f2-0000-4000-8000-000000000002"
	show1 = "5e5e5e5e-0000-4000-8000-000000000003"
)

// addTitle puts a movie or series in the catalog, matched to a TMDB id.
func addTitle(t *testing.T, st *store.Store, id, typ, title string, tmdbID int64) {
	t.Helper()
	storetest.AddItem(t, st, id, typ, title, "")
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_itemexternalids (id, item_id, source, externalid)
		VALUES (gen_random_uuid()::varchar, $1, 'tmdb', $2)`, id, fmt.Sprint(tmdbID))
}

// credits lists an item's credits as "role name (TMDB id)", sorted.
func credits(t *testing.T, st *store.Store, itemID string) string {
	t.Helper()
	rows, err := st.Pool().Query(context.Background(), `SELECT ip.role, p.name, COALESCE(p.tmdbpersonid, '-')
		FROM com_nalet_katalog_itempeople ip JOIN com_nalet_katalog_people p ON p.id = ip.person_id
		WHERE ip.item_id = $1`, itemID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var role, name, tmdb string
		if err := rows.Scan(&role, &name, &tmdb); err != nil {
			t.Fatal(err)
		}
		out = append(out, fmt.Sprintf("%s %s (%s)", role, name, tmdb))
	}
	sort.Strings(out)
	return strings.Join(out, ", ")
}

// tmdbIDOf is a person's TMDB id, "-" when they have none.
func tmdbIDOf(t *testing.T, st *store.Store, personID string) string {
	t.Helper()
	var id *string
	if err := st.Pool().QueryRow(context.Background(),
		`SELECT tmdbpersonid FROM com_nalet_katalog_people WHERE id = $1`, personID).Scan(&id); err != nil {
		t.Fatalf("person %s: %v", personID, err)
	}
	if id == nil {
		return "-"
	}
	return *id
}

func enrich(t *testing.T, s *Service, itemID string) {
	t.Helper()
	status, msg, err := s.EnrichOne(context.Background(), itemID)
	if err != nil || status != statusDone {
		t.Fatalf("EnrichOne(%s) = %s %q %v, want done", itemID, status, msg, err)
	}
}

// A credit finds its person by TMDB id. A person saved before ids were kept is
// found by name once and carries the id from then on; a person who has an id is
// never taken for another, so namesakes stay apart, and a credit that was
// matched to a namesake by name moves to the person it credits.
func TestCreditsFindTheirPeopleByTMDBID(t *testing.T) {
	st := storetest.Open(t)
	f := newFakeTMDB(t)
	s := newTestService(t, st, f, "en-US")
	addTitle(t, st, film1, "movie", "First Film", 10)
	addTitle(t, st, film2, "movie", "Second Film", 11)
	f.movie(10, "First Film")
	f.movie(11, "Second Film")
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_people (id, name, tmdbpersonid) VALUES
		('old-ada', 'Ada Example', NULL),     -- saved by name, before ids were kept
		('old-john', 'John Namesake', NULL),  -- one row the name match gave two people
		('known-cara', 'Cara Old Name', '300'),
		('other-dan', 'Dan Twin', '400')`) // a namesake with an id of his own
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_itempeople (id, item_id, person_id, role) VALUES
		('l1', $1, 'old-ada', 'actor'), ('l2', $1, 'old-john', 'actor'), ('l3', $2, 'old-john', 'actor')`, film1, film2)
	f.cast("movie/10", []string{"101", "Ada Example"}, []string{"501", "John Namesake"},
		[]string{"300", "Cara New Name"}, []string{"401", "Dan Twin"}, []string{"601", "Dee Director", "Director"},
		[]string{"602", "Writer Not Kept", "Screenplay"})
	f.cast("movie/11", []string{"502", "John Namesake"})

	enrich(t, s, film1)
	enrich(t, s, film2)

	if got, want := credits(t, st, film1), "actor Ada Example (101), actor Cara Old Name (300), actor Dan Twin (401), "+
		"actor John Namesake (501), director Dee Director (601)"; got != want {
		t.Errorf("first film credits:\n %s\nwant\n %s", got, want)
	}
	if got, want := credits(t, st, film2), "actor John Namesake (502)"; got != want {
		t.Errorf("second film credits: %s, want %s (moved off the namesake)", got, want)
	}
	for id, want := range map[string]string{"old-ada": "101", "old-john": "501", "known-cara": "300", "other-dan": "400"} {
		if got := tmdbIDOf(t, st, id); got != want {
			t.Errorf("%s has TMDB id %s, want %s", id, got, want)
		}
	}
	if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_people`); n != 7 {
		t.Errorf("%d people, want 7: the 4 there were, the second John, the second Dan and the director", n)
	}
	if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_itempeople WHERE person_id = 'other-dan'`); n != 0 {
		t.Error("a credit was linked to a namesake who has another TMDB id")
	}

	// Again: nothing new.
	enrich(t, s, film1)
	enrich(t, s, film2)
	if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_people`); n != 7 {
		t.Errorf("enriching again made people: %d, want 7", n)
	}
	if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_itempeople`); n != 6 {
		t.Errorf("enriching again changed the links: %d, want 6", n)
	}
}

// Enrichments that meet the same new person at once make one person.
func TestFindOrCreatePersonOnceUnderConcurrency(t *testing.T) {
	st := storetest.Open(t)
	s := newTestService(t, st, newFakeTMDB(t), "en-US")
	for round := 0; round < 5; round++ {
		var wg sync.WaitGroup
		ids := make([]string, 8)
		errs := make([]error, 8)
		for i := range ids {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				ids[i], _, errs[i] = s.findOrCreatePerson(context.Background(), int64(700+round), "Same Person")
			}(i)
		}
		wg.Wait()
		for i := range ids {
			if errs[i] != nil || ids[i] != ids[0] {
				t.Fatalf("round %d: %d. caller got %q, %v; the first got %q", round, i, ids[i], errs[i], ids[0])
			}
		}
	}
	if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_people`); n != 5 {
		t.Fatalf("%d people for 5 TMDB ids", n)
	}
}

// A catalog without migration 030 keeps linking credits by name.
func TestCreditsWithoutThePeopleMigrationLinkByName(t *testing.T) {
	st := storetest.OpenBase(t)
	if err := st.EnsureDeletionLog(context.Background()); err != nil {
		t.Fatal(err)
	}
	f := newFakeTMDB(t)
	s := newTestService(t, st, f, "en-US")
	addTitle(t, st, film1, "movie", "First Film", 10)
	f.movie(10, "First Film")
	f.cast("movie/10", []string{"101", "Ada Example"}, []string{"601", "Dee Director", "Director"})

	enrich(t, s, film1)
	enrich(t, s, film1)

	if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_people`); n != 2 {
		t.Errorf("%d people, want 2", n)
	}
	if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_itempeople WHERE item_id = $1`, film1); n != 2 {
		t.Errorf("%d links, want 2", n)
	}
	if len(f.calls("/3/person/")) != 0 {
		t.Error("people were fetched from TMDB on a catalog that cannot keep them")
	}
}
