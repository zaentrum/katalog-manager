package tmdb

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/zaentrum/katalog-manager/internal/config"
	"github.com/zaentrum/katalog-manager/internal/model"
	"github.com/zaentrum/katalog-manager/internal/store"
	"github.com/zaentrum/katalog-manager/internal/store/storetest"
)

// creditLines lists an item's credits with what each says, as
// "role name (TMDB id) job | character | order | episodes" (a dash for
// NULL), in the order of their roles, places and names.
func creditLines(t *testing.T, st *store.Store, itemID string) string {
	t.Helper()
	rows, err := st.Pool().Query(context.Background(), `SELECT ip.role, p.name, COALESCE(p.tmdbpersonid, '-'),
			COALESCE(ip.job, '-'), COALESCE(ip.charactername, '-'), COALESCE(ip.ordinal::text, '-'),
			COALESCE(ip.episodecount::text, '-')
		FROM com_nalet_katalog_itempeople ip JOIN com_nalet_katalog_people p ON p.id = ip.person_id
		WHERE ip.item_id = $1
		ORDER BY array_position($2::text[], ip.role::text) NULLS LAST, ip.role, ip.ordinal NULLS LAST, p.name`,
		itemID, model.CreditRoles)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v [7]string
		if err := rows.Scan(&v[0], &v[1], &v[2], &v[3], &v[4], &v[5], &v[6]); err != nil {
			t.Fatal(err)
		}
		out = append(out, fmt.Sprintf("%s %s (%s) %s | %s | %s | %s", v[0], v[1], v[2], v[3], v[4], v[5], v[6]))
	}
	return strings.Join(out, "\n")
}

// Every role the catalog knows comes from TMDB's credits, and a title's
// credits come in the order of the roles, however TMDB lists them.
func TestEveryRoleTheCatalogKnowsComesFromTMDB(t *testing.T) {
	var n tvAggregateJSON
	crew := []any{}
	for i, dj := range [][2]string{{"Editing", "Editor"}, {"Camera", "Director of Photography"}, {"Sound", "Music"},
		{"Production", "Producer"}, {"Writing", "Teleplay"}, {"Directing", "Series Director"}} {
		crew = append(crew, map[string]any{"id": 10 + i, "name": dj[1], "department": dj[0], "total_episode_count": 1,
			"jobs": []any{map[string]any{"job": dj[1], "episode_count": 1}}})
	}
	decode(t, map[string]any{"crew": crew, "cast": []any{map[string]any{"id": 1, "name": "Plays", "total_episode_count": 1}}}, &n)
	l := newCreditList(true)
	n.list(l)
	l.creator(2, "Creates")
	var roles []string
	for _, c := range l.credits().List {
		if len(roles) == 0 || roles[len(roles)-1] != c.Role {
			roles = append(roles, c.Role)
		}
	}
	if strings.Join(roles, " ") != strings.Join(model.CreditRoles, " ") {
		t.Errorf("roles %v, want every role the catalog knows, in its order: %v", roles, model.CreditRoles)
	}
	for _, role := range model.CreditRoles {
		if !model.ValidRole(role) {
			t.Errorf("%q is not a role", role)
		}
	}
}

// Of a crew, the catalog keeps the jobs it knows, each in its role, and
// nothing else.
func TestCrewRoles(t *testing.T) {
	for _, tc := range []struct{ department, job, role string }{
		{"Directing", "Director", roleDirector},
		{"Directing", "Co-Director", roleDirector},
		{"Directing", "Series Director", roleDirector},
		{"Directing", "Assistant Director", ""},
		{"Directing", "Second Unit Director", ""},
		{"Writing", "Writer", roleWriter},
		{"Writing", "Screenplay", roleWriter},
		{"Writing", "Story", roleWriter},
		{"Writing", "Co-Writer", roleWriter},
		{"Writing", "Original Series Creator", roleWriter},
		{"Writing", "", ""},
		{"Production", "Producer", roleProducer},
		{"Production", "Executive Producer", roleProducer},
		{"Production", "Co-Producer", roleProducer},
		{"Production", "Line Producer", roleProducer},
		{"Production", "Casting", ""},
		{"Production", "Production Manager", ""},
		{"Sound", "Original Music Composer", roleComposer},
		{"Sound", "Music", roleComposer},
		{"Sound", "Composer", roleComposer},
		{"Sound", "Music Supervisor", ""},
		{"Sound", "Sound Designer", ""},
		{"Camera", "Director of Photography", roleCinematographer},
		{"Camera", "Camera Operator", ""},
		{"Editing", "Editor", roleEditor},
		{"Editing", "Colorist", ""},
		{"Art", "Art Direction", ""},
		{"Crew", "Director", ""}, // the department counts, not the job's name alone
		{"Camera", "Director", ""},
		{" directing ", " director ", roleDirector}, // as TMDB spells it or not
	} {
		if got := crewRole(tc.department, tc.job); got != tc.role {
			t.Errorf("%s / %s: %q, want %q", tc.department, tc.job, got, tc.role)
		}
	}
}

// sintel gives the catalog a short film shaped like Sintel's credits on TMDB
// (the people are stand-ins): two voices, a director who also wrote its story,
// a writer, a producer, a composer credited for its music, and an art director
// and a casting director the catalog does not credit.
func sintel(t *testing.T, st *store.Store, f *fakeTMDB) {
	t.Helper()
	addTitle(t, st, film1, "movie", "Sintel", 45745)
	f.movie(45745, "Sintel")
	f.titleCredits("movie/45745", []map[string]any{
		{"id": 701, "name": "First Voice", "character": "Sintel", "order": 0},
		{"id": 702, "name": "Second Voice", "character": "Shaman", "order": 1},
	}, []map[string]any{
		{"id": 801, "name": "The Director", "department": "Directing", "job": "Director"},
		{"id": 802, "name": "The Writer", "department": "Writing", "job": "Writer"},
		{"id": 801, "name": "The Director", "department": "Writing", "job": "Story"},
		{"id": 803, "name": "The Producer", "department": "Production", "job": "Producer"},
		{"id": 804, "name": "The Composer", "department": "Sound", "job": "Music"},
		{"id": 805, "name": "The Art Director", "department": "Art", "job": "Art Direction"},
		{"id": 806, "name": "The Casting Director", "department": "Production", "job": "Casting"},
	})
}

const sintelCredits = `actor First Voice (701) - | Sintel | 0 | -
actor Second Voice (702) - | Shaman | 1 | -
director The Director (801) Director | - | 0 | -
writer The Writer (802) Writer | - | 0 | -
writer The Director (801) Story | - | 1 | -
producer The Producer (803) Producer | - | 0 | -
composer The Composer (804) Music | - | 0 | -`

// A film's credits come in every role TMDB lists the catalog knows: its cast
// as actors with whom they play, its director, writers, producer and composer
// with their jobs, each in TMDB's order; an art director is not credited. One
// who directed it and wrote it has two credits and is one person.
func TestAFilmIsCreditedInEveryRole(t *testing.T) {
	st := storetest.Open(t)
	f := newFakeTMDB(t)
	s := newTestService(t, st, f, "en-US")
	sintel(t, st, f)

	enrich(t, s, film1)
	if got := creditLines(t, st, film1); got != sintelCredits {
		t.Errorf("credits:\n%s\nwant\n%s", got, sintelCredits)
	}
	if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_people WHERE tmdbpersonid = '801'`); n != 1 {
		t.Errorf("the director who wrote it is %d people, want one", n)
	}
	if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_people`); n != 6 {
		t.Errorf("%d people, want the six credited: no art or casting director", n)
	}

	// Again: nothing changes.
	res, err := s.RefreshPeople(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	if res.CreditsAdded+res.CreditsUpdated+res.CreditsDropped+res.PeopleDeleted != 0 {
		t.Errorf("reading the same credits again changed them: %+v", counts(res))
	}
	if got := creditLines(t, st, film1); got != sintelCredits {
		t.Errorf("credits after reading them again:\n%s", got)
	}
}

// What TMDB says of a credit changes in place: the credit stays (the same
// row), and so does its person; refreshPeople counts it updated.
func TestACharacterChangeUpdatesTheCreditInPlace(t *testing.T) {
	st := storetest.Open(t)
	f := newFakeTMDB(t)
	s := newTestService(t, st, f, "en-US")
	sintel(t, st, f)
	enrich(t, s, film1)
	var credit, person string
	storetestScan(t, st, `SELECT ip.id, ip.person_id FROM com_nalet_katalog_itempeople ip
		JOIN com_nalet_katalog_people p ON p.id = ip.person_id WHERE p.tmdbpersonid = '701'`, &credit, &person)

	f.titleCredits("movie/45745", []map[string]any{
		{"id": 701, "name": "First Voice", "character": "Sintel (voice)", "order": 0},
		{"id": 702, "name": "Second Voice", "character": "Shaman", "order": 1},
	}, []map[string]any{
		{"id": 801, "name": "The Director", "department": "Directing", "job": "Director"},
		{"id": 802, "name": "The Writer", "department": "Writing", "job": "Writer"},
		{"id": 801, "name": "The Director", "department": "Writing", "job": "Story"},
		{"id": 803, "name": "The Producer", "department": "Production", "job": "Producer"},
		{"id": 804, "name": "The Composer", "department": "Sound", "job": "Music"},
	})
	res, err := s.RefreshPeople(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	if got := counts(res); got.CreditsUpdated != 1 || got.CreditsAdded != 0 || got.CreditsDropped != 0 ||
		got.CreditsRelinked != 0 || got.PeopleDeleted != 0 || got.PeopleCreated != 0 {
		t.Errorf("refreshPeople %+v: want one credit updated and nothing else", got)
	}
	if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_itempeople
		WHERE id = $1 AND person_id = $2 AND role = 'actor' AND charactername = 'Sintel (voice)' AND ordinal = 0`,
		credit, person); n != 1 {
		t.Error("the credit was not updated in place: its row is gone, or it does not say the new character")
	}
	if want := strings.Replace(sintelCredits, "| Sintel |", "| Sintel (voice) |", 1); creditLines(t, st, film1) != want {
		t.Errorf("credits:\n%s\nwant\n%s", creditLines(t, st, film1), want)
	}
	if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_deleteditems`); n != 0 {
		t.Errorf("%d deletions logged for a changed character", n)
	}
}

// A film keeps its first 12 billed actors and every director, and 10 of each
// other role, in TMDB's order; a series its 20 actors and 20 directors in the
// most episodes, and 10 of each other role.
func TestCreditCaps(t *testing.T) {
	jobs := map[string][2]string{roleDirector: {"Directing", "Director"}, roleWriter: {"Writing", "Writer"},
		roleProducer: {"Production", "Producer"}, roleComposer: {"Sound", "Composer"},
		roleCinematographer: {"Camera", "Director of Photography"}, roleEditor: {"Editing", "Editor"}}
	var filmCast, filmCrew, seriesCast, seriesCrew []any
	id := 0
	add := func(n int, role string) { // n people in role, billed and in episodes in the order added
		for i := 0; i < n; i++ {
			id++
			name := fmt.Sprintf("%s %02d", role, i+1)
			if role == roleActor {
				filmCast = append(filmCast, map[string]any{"id": id, "name": name, "order": i})
				seriesCast = append(seriesCast, map[string]any{"id": id, "name": name, "order": i, "total_episode_count": 100 - i})
				continue
			}
			job := jobs[role]
			filmCrew = append(filmCrew, map[string]any{"id": id, "name": name, "department": job[0], "job": job[1]})
			seriesCrew = append(seriesCrew, map[string]any{"id": id, "name": name, "department": job[0],
				"total_episode_count": 100 - i, "jobs": []any{map[string]any{"job": job[1], "episode_count": 100 - i}}})
		}
	}
	add(25, roleActor)
	add(23, roleDirector)
	for _, role := range []string{roleWriter, roleProducer, roleComposer, roleCinematographer, roleEditor} {
		add(12, role)
	}
	var film movieCreditsJSON
	var series tvAggregateJSON
	decode(t, map[string]any{"cast": filmCast, "crew": filmCrew}, &film)
	decode(t, map[string]any{"cast": seriesCast, "crew": seriesCrew}, &series)
	l := newCreditList(true)
	for i := 1; i <= 12; i++ { // a series keeps every creator
		l.creator(int64(500+i), fmt.Sprintf("creator %02d", i))
	}
	series.list(l)
	for _, tc := range []struct {
		what    string
		credits *tmdbCredits
		want    string
	}{
		{"film", film.credits(), "actor 12, director 23, writer 10, producer 10, composer 10, cinematographer 10, editor 10"},
		{"series", l.credits(), "actor 20, creator 12, director 20, writer 10, producer 10, composer 10, cinematographer 10, editor 10"},
	} {
		n, last := map[string]int{}, map[string]string{}
		for _, c := range tc.credits.List {
			n[c.Role]++
			last[c.Role] = c.Name
		}
		var got []string
		for _, role := range model.CreditRoles {
			if n[role] > 0 {
				got = append(got, fmt.Sprintf("%s %d", role, n[role]))
			}
		}
		if strings.Join(got, ", ") != tc.want {
			t.Errorf("%s keeps %s, want %s", tc.what, strings.Join(got, ", "), tc.want)
		}
		// The first of each role are kept: billed first, or in the most episodes.
		if last[roleWriter] != "writer 10" || last[roleActor] != fmt.Sprintf("actor %02d", n[roleActor]) {
			t.Errorf("%s: the last writer kept is %q and the last actor %q, want the first of each", tc.what,
				last[roleWriter], last[roleActor])
		}
	}
}

// decode reads v as TMDB would send it into dst.
func decode(t *testing.T, v any, dst any) {
	t.Helper()
	b, err := json.Marshal(v)
	if err == nil {
		err = json.Unmarshal(b, dst)
	}
	if err != nil {
		t.Fatal(err)
	}
}

// A film's actors rank by TMDB's billing order, which is each one's order;
// someone billed twice, as two characters, is one credit, with both
// characters in billing order and the first billing. Someone with two jobs in
// one role is one credit, with both jobs in TMDB's order, and the crew ranks
// as TMDB lists it. Nobody without a name is credited.
func TestFilmCreditsOfOnePersonInOneRole(t *testing.T) {
	var n movieCreditsJSON
	decode(t, map[string]any{
		"cast": []any{
			map[string]any{"id": 1, "name": "Twin", "character": "Alice's Sister", "order": 2},
			map[string]any{"id": 2, "name": "Other", "character": "Bob", "order": 5},
			map[string]any{"id": 1, "name": "Twin", "character": "Alice", "order": 0},
			map[string]any{"id": 3, "name": "  ", "character": "Nobody", "order": 1},
			map[string]any{"id": 6, "name": "Billed Before Other", "character": "Carol", "order": 3},
		},
		"crew": []any{
			map[string]any{"id": 5, "name": "Second Writer", "department": "Writing", "job": "Novel"},
			map[string]any{"id": 4, "name": "Writes Twice", "department": "Writing", "job": "Story"},
			map[string]any{"id": 4, "name": "Writes Twice", "department": "Writing", "job": "Screenplay"},
			map[string]any{"id": 4, "name": "Writes Twice", "department": "Writing", "job": "Story"},
		},
	}, &n)
	var got []string
	for _, c := range n.credits().List {
		got = append(got, fmt.Sprintf("%s %s %q %q %d %v", c.Role, c.Name, c.Job, c.Character, c.Order, c.Episodes))
	}
	want := `actor Twin "" "Alice / Alice's Sister" 0 <nil>
actor Billed Before Other "" "Carol" 3 <nil>
actor Other "" "Bob" 5 <nil>
writer Second Writer "Novel" "" 0 <nil>
writer Writes Twice "Story, Screenplay" "" 1 <nil>`
	if strings.Join(got, "\n") != want {
		t.Errorf("credits:\n%s\nwant\n%s", strings.Join(got, "\n"), want)
	}
}

// A series' creators come in TMDB's order, whatever their ids or the episodes
// they are in otherwise, each with the job Creator and no episodes.
func TestCreatorsComeInTMDBsOrder(t *testing.T) {
	l := newCreditList(true)
	var n tvAggregateJSON
	decode(t, map[string]any{"crew": []any{map[string]any{"id": 10, "name": "Second", "department": "Writing",
		"total_episode_count": 9, "jobs": []any{map[string]any{"job": "Writer", "episode_count": 9}}}}}, &n)
	n.list(l)
	for _, c := range []struct {
		id   int64
		name string
	}{{30, "First"}, {10, "Second"}, {20, "Third"}, {30, "First"}} {
		l.creator(c.id, c.name)
	}
	var got []string
	for _, c := range l.credits().List {
		if c.Role == roleCreator {
			got = append(got, fmt.Sprintf("%s %q %d %v", c.Name, c.Job, c.Order, c.Episodes))
		}
	}
	if want := `First "Creator" 0 <nil>, Second "Creator" 1 <nil>, Third "Creator" 2 <nil>`; strings.Join(got, ", ") != want {
		t.Errorf("creators %s, want %s", strings.Join(got, ", "), want)
	}
}

// A series' credit says the episodes the person is credited in, in the role:
// an actor's in all, as each character joined the most episodes first; a crew
// member's jobs in one role are one credit, the jobs joined the most episodes
// first, their episodes TMDB's count in the department when every job of
// theirs there is in the role, otherwise the most of any one job in it.
func TestSeriesCreditDetails(t *testing.T) {
	var n tvAggregateJSON
	if err := json.Unmarshal([]byte(`{
		"cast": [
			{"id": 11, "name": "Lead", "order": 1, "total_episode_count": 6, "roles": [
				{"character": "Young Lead", "episode_count": 2}, {"character": "Lead", "episode_count": 6},
				{"character": "", "episode_count": 1}, {"character": "Lead", "episode_count": 1}]},
			{"id": 12, "name": "Guest", "order": 0, "total_episode_count": 1, "roles": [{"character": "Guest", "episode_count": 1}]}
		],
		"crew": [
			{"id": 21, "name": "Writes", "department": "Writing", "total_episode_count": 7, "jobs": [
				{"job": "Co-Writer", "episode_count": 2}, {"job": "Writer", "episode_count": 5}]},
			{"id": 21, "name": "Writes", "department": "Directing", "total_episode_count": 9, "jobs": [
				{"job": "Director", "episode_count": 3}, {"job": "Second Unit Director", "episode_count": 6}]},
			{"id": 22, "name": "Produces", "department": "Production", "total_episode_count": 8, "jobs": [
				{"job": "Producer", "episode_count": 4}, {"job": "Executive Producer", "episode_count": 8}]}
		]}`), &n); err != nil {
		t.Fatal(err)
	}
	l := newCreditList(true)
	n.list(l)
	var got []string
	for _, c := range l.credits().List {
		got = append(got, fmt.Sprintf("%s %s %q %q %d %d", c.Role, c.Name, c.Job, c.Character, c.Order, *c.Episodes))
	}
	want := `actor Lead "" "Lead / Young Lead" 0 6
actor Guest "" "Guest" 1 1
director Writes "Director" "" 0 3
writer Writes "Writer, Co-Writer" "" 0 7
producer Produces "Executive Producer, Producer" "" 0 8`
	if strings.Join(got, "\n") != want {
		t.Errorf("credits:\n%s\nwant\n%s", strings.Join(got, "\n"), want)
	}
}

// A catalog without migration 032 keeps its credits following TMDB, their
// roles alone; once 032 is in place a refresh gives them what TMDB says.
func TestCreditsWithoutMigration032KeepTheirRoles(t *testing.T) {
	st := storetest.OpenBase(t)
	ctx := context.Background()
	for _, ensure := range []func(context.Context) error{st.EnsureDeletionLog, st.EnsurePeople, st.EnsureItemLockedFields} {
		if err := ensure(ctx); err != nil {
			t.Fatal(err)
		}
	}
	f := newFakeTMDB(t)
	s := newTestService(t, st, f, "en-US")
	sintel(t, st, f)
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_people (id, name) VALUES ('stale', 'Stale Actor')`)
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_itempeople (id, item_id, person_id, role) VALUES ('l1', $1, 'stale', 'actor')`, film1)

	enrich(t, s, film1)
	if got, want := credits(t, st, film1), "actor First Voice (701), actor Second Voice (702), composer The Composer (804), "+
		"director The Director (801), producer The Producer (803), writer The Director (801), writer The Writer (802)"; got != want {
		t.Errorf("credits without 032:\n %s\nwant\n %s", got, want)
	}
	if _, ok := storetest.Deleted(t, st, "stale"); !ok {
		t.Error("the person TMDB no longer credits was not deleted and logged")
	}

	if err := st.EnsureCreditDetails(ctx); err != nil {
		t.Fatal(err)
	}
	res, err := s.RefreshPeople(ctx, true) // the same service: it sees 032 once it is there
	if err != nil {
		t.Fatal(err)
	}
	if res.CreditsUpdated != 7 || res.CreditsAdded != 0 || res.CreditsDropped != 0 {
		t.Errorf("the refresh after 032: %+v, want the seven credits updated", counts(res))
	}
	if got := creditLines(t, st, film1); got != sintelCredits {
		t.Errorf("credits after 032:\n%s\nwant\n%s", got, sintelCredits)
	}
}

// Credits follow TMDB in the roles KATALOG_CREDIT_ROLES names: once a role is
// no longer among them, refreshing a title drops its credits in that role,
// like credits TMDB no longer lists, and a person no title credits after that
// is deleted and logged, as before; a person still credited in another role
// stays. A catalog without migration 030 links only the roles named.
func TestARoleNoLongerNamedIsDroppedOnRefresh(t *testing.T) {
	t.Setenv("KATALOG_CREDIT_ROLES", "actor,director,producer,composer") // no writers
	loaded, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{CreditRoles: loaded.CreditRoles}
	withoutWriters := strings.Join(slices.DeleteFunc(strings.Split(sintelCredits, "\n"),
		func(l string) bool { return strings.HasPrefix(l, "writer ") }), "\n")
	for _, path := range []string{"refreshPeople", "enrichment"} {
		t.Run(path, func(t *testing.T) {
			st := storetest.Open(t)
			f := newFakeTMDB(t)
			sintel(t, st, f)
			enrich(t, newTestService(t, st, f, "en-US"), film1) // every role
			s := newTestServiceWith(t, st, f, cfg)
			if path == "refreshPeople" {
				res, err := s.RefreshPeople(context.Background(), true)
				if err != nil {
					t.Fatal(err)
				}
				if got := counts(res); got.CreditsDropped != 2 || got.PeopleDeleted != 1 || got.CreditsAdded != 0 ||
					got.CreditsUpdated != 0 {
					t.Errorf("refreshPeople %+v: want the two writer credits dropped and the writer deleted", got)
				}
			} else {
				enrich(t, s, film1)
			}
			if got := creditLines(t, st, film1); got != withoutWriters {
				t.Errorf("credits:\n%s\nwant\n%s", got, withoutWriters)
			}
			want := `The Writer by katalog-manager/tmdb: no title credits them any more: TMDB's credits of "Sintel" no longer list them`
			if got := loggedPeople(t, st); strings.Join(got, "\n") != want {
				t.Errorf("the deletion log's people:\n%s\nwant\n%s", strings.Join(got, "\n"), want)
			}
			if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_people WHERE tmdbpersonid = '801'`); n != 1 {
				t.Error("the director who also wrote it was deleted with the writer credit")
			}
		})
	}

	st := storetest.OpenBase(t)
	if err := st.EnsureDeletionLog(context.Background()); err != nil {
		t.Fatal(err)
	}
	f := newFakeTMDB(t)
	sintel(t, st, f)
	enrich(t, newTestServiceWith(t, st, f, config.Config{CreditRoles: []string{"actor"}}), film1)
	var linked string
	storetestScan(t, st, `SELECT string_agg(ip.role || ' ' || p.name, ', ' ORDER BY ip.role, p.name)
		FROM com_nalet_katalog_itempeople ip JOIN com_nalet_katalog_people p ON p.id = ip.person_id`, &linked)
	if linked != "actor First Voice, actor Second Voice" {
		t.Errorf("without 030, with actors alone named, the credits are %s", linked)
	}
}
