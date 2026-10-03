package tmdb

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/zaentrum/katalog-manager/internal/auth"
	"github.com/zaentrum/katalog-manager/internal/graph"
	"github.com/zaentrum/katalog-manager/internal/store"
	"github.com/zaentrum/katalog-manager/internal/store/storetest"
)

// The demo's two shorts, as they were after the first refreshPeople: each was
// first matched to the wrong film and kept that film's credits, a director and
// twelve actors, saved by name; when it was matched right, the right director
// was credited beside them, and the backfill gave him his TMDB id. TMDB lists
// that director, and no cast.
const (
	spring = "57575757-0000-4000-8000-000000000001" // "Spring" (2019), TMDB movie 593048
	hero   = "48484848-0000-4000-8000-000000000002" // "HERO" (2018), TMDB movie 615324
	other  = "07070707-0000-4000-8000-000000000003" // a title nobody touches
)

var (
	springStale = []string{"Harmony Korine", // the director of "Spring Breakers", then its cast
		"James Franco", "Selena Gomez", "Vanessa Hudgens", "Ashley Benson", "Rachel Anna Simon", "Gucci Mane",
		"Heather Morris", "Ash Lendzion", "Emma Holzer", "Lee Irby", "Jeffrey Jarrett", "Russell Stuart"}
	heroStale = []string{"Robert Vince", // the director of the "Hero" it was taken for, then its cast
		"Abigail Breslin", "Makenzie Vega", "Irene Olga López", "Louis Ferreira", "Christine Tucci", "Maurice Godin",
		"Ethan Phillips", "Fred Ewanuick", "Tony Alcantar", "Laurie Bekker", "Margot Berner", "Lindsay Bourne"}
)

// The directors' TMDB person ids here are stand-ins.
const springDirector, heroDirector = 9_000_001, 9_000_002

// twoShorts gives the catalog the two shorts as the demo had them, and TMDB
// what it lists for them.
func twoShorts(t *testing.T, st *store.Store, f *fakeTMDB) {
	t.Helper()
	addTitle(t, st, spring, "movie", "Spring", 593048)
	addTitle(t, st, hero, "movie", "HERO", 615324)
	addTitle(t, st, other, "movie", "Untouched", 1)
	f.movie(593048, "Spring")
	f.movie(615324, "HERO")
	f.cast("movie/593048", []string{fmt.Sprint(springDirector), "Andreas Goralczyk", "Director"})
	f.cast("movie/615324", []string{fmt.Sprint(heroDirector), "Daniel Martínez Lara", "Director"})
	f.person(springDirector, &fakePerson{Name: "Andreas Goralczyk"})
	f.person(heroDirector, &fakePerson{Name: "Daniel Martínez Lara"})
	for item, names := range map[string][]string{spring: springStale, hero: heroStale} {
		for i, name := range names {
			id, role := fmt.Sprintf("%s-%02d", item[:8], i), roleActor
			if i == 0 {
				role = roleDirector
			}
			storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_people (id, name) VALUES ($1, $2)`, id, name)
			storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_itempeople (id, item_id, person_id, role)
				VALUES (gen_random_uuid()::varchar, $1, $2, $3)`, item, id, role)
		}
	}
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_people (id, name, tmdbpersonid, tmdbfetchedat, fieldorigins) VALUES
		('andreas', 'Andreas Goralczyk', $1, now(), '{"name": "tmdb", "externalIds": "tmdb"}'),
		('daniel', 'Daniel Martínez Lara', $2, now(), '{"name": "tmdb", "externalIds": "tmdb"}'),
		('kept', 'Someone Kept', '77', now(), NULL)`, fmt.Sprint(springDirector), fmt.Sprint(heroDirector))
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_itempeople (id, item_id, person_id, role) VALUES
		('right-1', $1, 'andreas', 'director'), ('right-2', $2, 'daniel', 'director'), ('kept-1', $3, 'kept', 'actor')`,
		spring, hero, other)
}

// loggedPeople lists the deletion log's people: "name by why", sorted.
func loggedPeople(t *testing.T, st *store.Store) []string {
	t.Helper()
	rows, err := st.Pool().Query(context.Background(), `SELECT title, deletedby, COALESCE(reason, '')
		FROM com_nalet_katalog_deleteditems WHERE type = 'person'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name, by, why string
		if err := rows.Scan(&name, &by, &why); err != nil {
			t.Fatal(err)
		}
		out = append(out, name+" by "+by+": "+why)
	}
	sort.Strings(out)
	return out
}

// The demo's case: refreshPeople reads the two shorts' credits again, and each
// then credits exactly whom TMDB lists, its director. The 13 people each kept
// from the wrong match are credited by no title any more: they are deleted and
// recorded in the deletion log, as people, attributed to the operator who ran
// the refresh.
func TestCreditsFollowTMDBForTheTwoShorts(t *testing.T) {
	st := storetest.Open(t)
	f := newFakeTMDB(t)
	s := newTestService(t, st, f, "en-US")
	twoShorts(t, st, f)
	ctx := auth.WithPrincipal(context.Background(), &auth.Principal{Subject: "operator-1"})

	res, err := s.RefreshPeople(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	// The directors' credits stay, and take what TMDB says of them: their job and order.
	if got, want := counts(res), (graph.PeopleRefreshResult{TitlesRead: 2, CreditsUpdated: 2, CreditsDropped: 26,
		PeopleDeleted: 26}); got != want {
		t.Errorf("refreshPeople:\n got  %+v\n want %+v", got, want)
	}
	if got, want := credits(t, st, spring), fmt.Sprintf("director Andreas Goralczyk (%d)", springDirector); got != want {
		t.Errorf("Spring credits %s, want exactly TMDB's: %s", got, want)
	}
	if got, want := credits(t, st, hero), fmt.Sprintf("director Daniel Martínez Lara (%d)", heroDirector); got != want {
		t.Errorf("HERO credits %s, want exactly TMDB's: %s", got, want)
	}
	if got := credits(t, st, other); got != "actor Someone Kept (77)" {
		t.Errorf("a title nobody refreshed changed: %s", got)
	}
	if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_people`); n != 3 {
		t.Errorf("%d people left, want the two directors and the one the other title credits", n)
	}
	var want []string
	for title, names := range map[string][]string{"Spring": springStale, "HERO": heroStale} {
		for _, name := range names {
			want = append(want, fmt.Sprintf("%s by operator-1: no title credits them any more: TMDB's credits of %q no longer list them", name, title))
		}
	}
	sort.Strings(want)
	if got := loggedPeople(t, st); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("the deletion log's people:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_deleteditems
		WHERE type = 'person' AND deletedat > (now() AT TIME ZONE 'utc') - interval '1 minute'`); n != 26 {
		t.Errorf("%d of the 26 log rows are of now, in UTC", n)
	}

	// Nothing is left to do: a second run reads no title and deletes no one.
	res, err = s.RefreshPeople(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := counts(res); got != (graph.PeopleRefreshResult{}) {
		t.Errorf("a second run: %+v, want nothing", got)
	}
}

// Enriching a title and refreshing it from the change list replace its
// credits the same way; the service is who deletes when no one asked.
func TestCreditsFollowTMDBWhenEnrichedAndFromTheChangeList(t *testing.T) {
	for _, path := range []string{"enrichment", "change list"} {
		t.Run(path, func(t *testing.T) {
			st := storetest.Open(t)
			f := newFakeTMDB(t)
			s := newTestService(t, st, f, "en-US")
			twoShorts(t, st, f)
			if path == "enrichment" {
				enrich(t, s, spring)
				enrich(t, s, hero)
			} else {
				f.changed("movie", "2026-10-02", 593048, 615324)
				runSync(t, s, syncNow)
				if got := cursor(t, st, "movie"); got != "2026-10-02 2/2/2/0/0 -" {
					t.Errorf("movie cursor %s", got)
				}
			}
			if got := credits(t, st, spring) + " | " + credits(t, st, hero); got !=
				fmt.Sprintf("director Andreas Goralczyk (%d) | director Daniel Martínez Lara (%d)", springDirector, heroDirector) {
				t.Errorf("credits after %s: %s", path, got)
			}
			got := loggedPeople(t, st)
			if len(got) != 26 || !strings.HasPrefix(got[0], "Abigail Breslin by katalog-manager/tmdb: ") {
				t.Errorf("after %s the log holds %d people: %.200q", path, len(got), got)
			}
		})
	}
}

// A title that keeps its credits — its metadata locked, or credits or people
// among its locked fields — keeps them all: TMDB neither adds nor drops one,
// and no one is deleted.
func TestCreditsOfALockedTitleStay(t *testing.T) {
	for _, lock := range []string{
		`metadatalocked = true`, `lockedfields = '["credits"]'`, `lockedfields = '["people"]'`, `lockedfields = '["credits.cast"]'`,
	} {
		t.Run(lock, func(t *testing.T) {
			st := storetest.Open(t)
			f := newFakeTMDB(t)
			s := newTestService(t, st, f, "en-US")
			twoShorts(t, st, f)
			storetest.Exec(t, st, `UPDATE com_nalet_katalog_items SET `+lock+` WHERE id = $1`, spring)
			before := credits(t, st, spring)

			res, err := s.RefreshPeople(context.Background(), true)
			if err != nil {
				t.Fatal(err)
			}
			if got := credits(t, st, spring); got != before {
				t.Errorf("a locked title's credits changed:\n %s\nto\n %s", before, got)
			}
			if res.TitlesLocked != 1 || res.PeopleDeleted != 13 || res.CreditsDropped != 13 {
				t.Errorf("counts %+v: want Spring locked, and only HERO's 13 dropped and deleted", counts(res))
			}
			for _, d := range loggedPeople(t, st) {
				if strings.Contains(d, `"Spring"`) {
					t.Errorf("someone of the locked title was deleted: %s", d)
				}
			}
			enrich(t, s, spring) // enriching it keeps them too
			if got := credits(t, st, spring); got != before {
				t.Errorf("enriching a locked title changed its credits to %s", got)
			}
		})
	}
	// Locks on other fields leave credits to TMDB.
	st := storetest.Open(t)
	f := newFakeTMDB(t)
	s := newTestService(t, st, f, "en-US")
	twoShorts(t, st, f)
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_items SET lockedfields = '["title", "tagline"]' WHERE id = $1`, spring)
	enrich(t, s, spring)
	if got := credits(t, st, spring); got != fmt.Sprintf("director Andreas Goralczyk (%d)", springDirector) {
		t.Errorf("a lock on other fields kept the credits: %s", got)
	}
}

// A dropped credit's person whom another title still credits stays; one who
// goes takes their images along.
func TestADroppedPersonAnotherTitleCreditsStays(t *testing.T) {
	st := storetest.Open(t)
	f := newFakeTMDB(t)
	s := newTestService(t, st, f, "en-US")
	twoShorts(t, st, f)
	franco := "57575757-01"
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_itempeople (id, item_id, person_id, role)
		VALUES ('franco-elsewhere', $1, $2, 'actor')`, other, franco)
	selena := "57575757-02"
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_personartwork (id, person_id, contenttype, bytes, sha256, isprimary)
		VALUES ('her-portrait', $1, 'image/jpeg', '\xffd8', repeat('a', 64), true)`, selena)

	enrich(t, s, spring)
	if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_people WHERE id = $1`, franco); n != 1 {
		t.Error("a person another title credits was deleted")
	}
	if _, ok := storetest.Deleted(t, st, franco); ok {
		t.Error("a person another title credits is in the deletion log")
	}
	if got := credits(t, st, other); got != "actor James Franco (-), actor Someone Kept (77)" {
		t.Errorf("the other title's credits: %s", got)
	}
	if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_personartwork WHERE person_id = $1`, selena); n != 0 {
		t.Error("a deleted person's images stayed")
	}
	if d, ok := storetest.Deleted(t, st, selena); !ok || d.Type != "person" || d.Title != "Selena Gomez" {
		t.Errorf("Selena Gomez's log row: %+v %v", d, ok)
	}
}

// Without the deletion log nobody can be deleted, so a title's credits that
// would leave someone uncredited stay as they were — none dropped, none added,
// no one made — and the title is enriched all the same.
func TestCreditsThatCannotBeRecordedStayAsTheyWere(t *testing.T) {
	st := storetest.Open(t)
	f := newFakeTMDB(t)
	s := newTestService(t, st, f, "en-US")
	twoShorts(t, st, f)
	f.cast("movie/593048", []string{fmt.Sprint(springDirector), "Andreas Goralczyk", "Director"},
		[]string{"9000003", "A New Voice"}) // TMDB lists someone new besides
	before := credits(t, st, spring)
	storetest.Exec(t, st, `DROP TABLE com_nalet_katalog_deleteditems`)

	enrich(t, s, spring) // done
	if got := credits(t, st, spring); got != before {
		t.Errorf("credits changed although their people could not be recorded:\n %s", got)
	}
	if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_itempeople WHERE item_id = $1`, spring); n != 14 {
		t.Errorf("Spring has %d credit rows, want its 14", n)
	}
	if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_people`); n != 29 {
		t.Errorf("%d people, want all 29 and no one new", n)
	}
	res, err := s.RefreshPeople(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if res.TitlesFailed != 2 || res.TitlesRead != 0 || res.PeopleDeleted != 0 {
		t.Errorf("refreshPeople without the log: %+v, want both titles failed", counts(res))
	}
}

// What replacing credits counts: a credit added, one dropped, one moved off a
// namesake onto the person credited, and the people deleted.
func TestReplaceCreditsCountsWhatItDid(t *testing.T) {
	st := storetest.Open(t)
	f := newFakeTMDB(t)
	s := newTestService(t, st, f, "en-US")
	addTitle(t, st, film1, "movie", "First Film", 10)
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_people (id, name, tmdbpersonid) VALUES
		('john-501', 'John Namesake', '501'), ('gone', 'Gone Actor', NULL), ('stays', 'Stays', '300')`)
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_itempeople (id, item_id, person_id, role) VALUES
		('l1', $1, 'john-501', 'actor'), ('l2', $1, 'gone', 'actor'), ('l3', $1, 'stays', 'actor'),
		('l4', $1, 'stays', 'actor')`, film1) // a link stored twice

	ch, err := s.replaceCredits(context.Background(), film1, &tmdbCredits{List: []tmdbCredit{
		{ID: 502, Name: "John Namesake", Role: roleActor}, {ID: 300, Name: "Stays", Role: roleActor},
		{ID: 300, Name: "Stays", Role: roleActor}, {ID: 600, Name: "New Face", Role: roleActor},
		{ID: 300, Name: "Stays", Role: roleDirector},
	}})
	if err != nil {
		t.Fatal(err)
	}
	ch.people = nil
	if want := (creditChange{created: 2, added: 2, updated: 1, dropped: 2, relinked: 1, deleted: 2}); !reflect.DeepEqual(ch, want) {
		// added: New Face as actor, Stays as director; updated: Stays as actor, who now has an order;
		// relinked: John onto the other John; dropped: Gone Actor and the second copy of Stays;
		// deleted: John 501 and Gone Actor
		t.Errorf("change %+v, want %+v", ch, want)
	}
	if got := credits(t, st, film1); got != "actor John Namesake (502), actor New Face (600), actor Stays (300), director Stays (300)" {
		t.Errorf("credits %s", got)
	}
}

// Two titles at once: one drops the last credit of a person while the other
// credits them. The person stays: the deletion waits for the credit, and then
// sees it.
func TestADeletionWaitsForACreditGivenMeanwhile(t *testing.T) {
	st := storetest.Open(t)
	f := newFakeTMDB(t)
	s := newTestService(t, st, f, "en-US")
	ctx := context.Background()
	addTitle(t, st, film1, "movie", "Dropping", 10)
	addTitle(t, st, film2, "movie", "Crediting", 11)
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_people (id, name, tmdbpersonid) VALUES ('p', 'Shared', '42')`)
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_itempeople (id, item_id, person_id, role) VALUES ('l1', $1, 'p', 'actor')`, film1)

	crediting, err := st.Pool().Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer crediting.Rollback(ctx)
	id, _, err := findOrCreatePerson(ctx, crediting, 42, "Shared") // the other title has found them
	if err != nil || id != "p" {
		t.Fatalf("findOrCreatePerson: %s %v", id, err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := s.replaceCredits(ctx, film1, &tmdbCredits{}) // TMDB lists no one any more
		done <- err
	}()
	waitForALockWait(t, st)
	if _, err := crediting.Exec(ctx, `INSERT INTO com_nalet_katalog_itempeople (id, item_id, person_id, role)
		VALUES ('l2', $1, 'p', 'actor')`, film2); err != nil {
		t.Fatal(err)
	}
	if err := crediting.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_people WHERE id = 'p'`); n != 1 {
		t.Fatal("a person credited meanwhile was deleted")
	}
	if got := credits(t, st, film1) + " | " + credits(t, st, film2); got != " | actor Shared (42)" {
		t.Errorf("credits %q", got)
	}
}

// waitForALockWait returns once a session waits to lock people for deleting
// them.
func waitForALockWait(t *testing.T, st *store.Store) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		var waiting bool
		if err := st.Pool().QueryRow(context.Background(), `SELECT EXISTS (SELECT 1 FROM pg_stat_activity
			WHERE datname = current_database() AND wait_event_type = 'Lock'
			  AND query LIKE '%com_nalet_katalog_people%FOR UPDATE%')`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			return
		}
	}
	t.Fatal("no session came to wait for the lock")
}

// modified is whether an item was modified since modifiedOld set it back:
// "true <modifiedby>" when its modifiedat moved, "false <modifiedby>" when not.
func modified(t *testing.T, st *store.Store, itemID string) string {
	t.Helper()
	var got string
	if err := st.Pool().QueryRow(context.Background(), `SELECT (modifiedat > '2000-01-01')::text || ' ' ||
		COALESCE(modifiedby, '-') FROM com_nalet_katalog_items WHERE id = $1`, itemID).Scan(&got); err != nil {
		t.Fatal(err)
	}
	return got
}

// modifiedOld sets an item's modifiedat and modifiedby back to long ago and
// nobody.
func modifiedOld(t *testing.T, st *store.Store, itemID string) {
	t.Helper()
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_items SET modifiedat = '2000-01-01', modifiedby = 'nobody'
		WHERE id = $1`, itemID)
}

// A title whose credits change — one added, dropped or updated in place — is
// modified, by whoever changed them (the principal, or the service), so that
// a record projected from it knows it is stale; a refresh that changes none of
// its credits does not touch it, and neither does one of a locked title.
func TestCreditsThatChangeModifyTheTitle(t *testing.T) {
	st := storetest.Open(t)
	f := newFakeTMDB(t)
	s := newTestService(t, st, f, "en-US")
	sintel(t, st, f)
	operator := auth.WithPrincipal(context.Background(), &auth.Principal{Subject: "operator-1"})
	for _, step := range []struct {
		what      string
		ctx       context.Context
		character string
		producer  bool
		lock      bool
		want      string
	}{
		{"its credits added", operator, "Sintel", true, false, "true operator-1"},
		{"nothing changed", operator, "Sintel", true, false, "false nobody"},
		{"a character changed", context.Background(), "Sintel (voice)", true, false, "true katalog-manager/tmdb"},
		{"nothing changed again", context.Background(), "Sintel (voice)", true, false, "false nobody"},
		{"a credit dropped", operator, "Sintel (voice)", false, false, "true operator-1"},
		{"a locked title", operator, "Sintel", true, true, "false nobody"},
	} {
		f.titleCredits("movie/45745", sintelCast(step.character), sintelCrew(step.producer))
		if step.lock {
			storetest.Exec(t, st, `UPDATE com_nalet_katalog_items SET lockedfields = '["credits"]' WHERE id = $1`, film1)
		}
		modifiedOld(t, st, film1)
		res, err := s.RefreshPeople(step.ctx, true)
		if err != nil {
			t.Fatal(err)
		}
		if got := modified(t, st, film1); got != step.want {
			t.Errorf("%s (%+v): modified %s, want %s", step.what, counts(res), got, step.want)
		}
	}

	// Enrichment changes them the same way. (It moves modifiedat in any case,
	// for the title's other fields; modifiedby says who changed its credits.)
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_items SET lockedfields = NULL WHERE id = $1`, film1)
	f.titleCredits("movie/45745", sintelCast("Sintel"), sintelCrew(false))
	modifiedOld(t, st, film1)
	enrich(t, s, film1)
	if got := modified(t, st, film1); got != "true katalog-manager/tmdb" {
		t.Errorf("enriched with a character changed: modified %s", got)
	}
}

// The title is modified in the transaction that changes its credits: when
// that cannot go through, neither does the change to the title.
func TestCreditsThatCannotBeStoredDoNotModifyTheTitle(t *testing.T) {
	st := storetest.Open(t)
	f := newFakeTMDB(t)
	s := newTestService(t, st, f, "en-US")
	twoShorts(t, st, f)
	storetest.Exec(t, st, `DROP TABLE com_nalet_katalog_deleteditems`) // its stale people cannot be logged
	modifiedOld(t, st, spring)
	if _, err := s.RefreshPeople(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if got := modified(t, st, spring); got != "false nobody" {
		t.Errorf("credits that could not be replaced modified the title: %s", got)
	}
}

// A catalog without migration 030 links credits by name, and the title that
// gains one is modified; one that has them all already is not.
func TestLinkingByNameModifiesTheTitleThatGainsACredit(t *testing.T) {
	st := storetest.OpenBase(t)
	if err := st.EnsureDeletionLog(context.Background()); err != nil {
		t.Fatal(err)
	}
	f := newFakeTMDB(t)
	s := newTestService(t, st, f, "en-US")
	addTitle(t, st, film1, "movie", "First Film", 10)
	f.movie(10, "First Film")
	f.cast("movie/10", []string{"101", "Ada Example"}, []string{"601", "Dee Director", "Director"})
	operator := auth.WithPrincipal(context.Background(), &auth.Principal{Subject: "operator-1"})
	for _, want := range []string{"operator-1", "nobody"} { // linked; then linked already
		modifiedOld(t, st, film1)
		if status, msg, err := s.EnrichOne(operator, film1); err != nil || status != statusDone {
			t.Fatalf("EnrichOne: %s %q %v", status, msg, err)
		}
		var by string
		storetestScan(t, st, `SELECT modifiedby FROM com_nalet_katalog_items WHERE id = '`+film1+`'`, &by)
		if by != want {
			t.Errorf("modified by %s, want %s", by, want)
		}
	}
}
