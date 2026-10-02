package tmdb

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/zaentrum/katalog-manager/internal/store/storetest"
)

// A long show: its latest season has a small cast, the seasons before had
// more people. TMDB's plain credits of a series are its latest season; its
// aggregate credits are every season.
func longShow(f *fakeTMDB) {
	f.tv(20, "A Long Show")
	f.cast("tv/20", []string{"301", "Lead One"}, []string{"308", "New In Season Three"}) // the latest season
	f.aggregate("tv/20",
		aggPerson{301, "Lead One", 30, 0, ""},
		aggPerson{302, "Lead Two", 24, 1, ""},
		aggPerson{305, "Seasons One And Two", 20, 2, ""},
		aggPerson{303, "Season One Regular", 10, 3, ""},
		aggPerson{304, "Season Two Regular", 10, 4, ""},
		aggPerson{308, "New In Season Three", 8, 5, ""},
		aggPerson{306, "Early Recurring", 4, 6, ""},
		aggPerson{307, "In One Episode", 1, 7, ""},
		aggPerson{401, "Pilot Director", 3, 0, "Director"},
		aggPerson{402, "Finale Director", 1, 0, "Director"},
		aggPerson{403, "Their Camera", 30, 0, "Director of Photography"})
}

const longShowCredits = "actor Early Recurring (306), actor In One Episode (307), actor Lead One (301), " +
	"actor Lead Two (302), actor New In Season Three (308), actor Season One Regular (303), " +
	"actor Season Two Regular (304), actor Seasons One And Two (305), " +
	"director Finale Director (402), director Pilot Director (401)"

// A series' credits are its people of every season: refreshing it — enriching
// it, the backfill, the change list — keeps everyone the earlier seasons had,
// although its latest season credits only two of them.
func TestSeriesCreditsComeFromEverySeason(t *testing.T) {
	for _, path := range []string{"enrichment", "refreshPeople", "change list"} {
		t.Run(path, func(t *testing.T) {
			st := storetest.Open(t)
			f := newFakeTMDB(t)
			s := newTestService(t, st, f, "en-US")
			addTitle(t, st, show1, "series", "A Long Show", 20)
			longShow(f)
			// The catalog credits them all already, from when the earlier seasons were the latest.
			for _, p := range []struct {
				id   int64
				name string
				role string
			}{{301, "Lead One", "actor"}, {302, "Lead Two", "actor"}, {305, "Seasons One And Two", "actor"},
				{303, "Season One Regular", "actor"}, {304, "Season Two Regular", "actor"},
				{306, "Early Recurring", "actor"}, {307, "In One Episode", "actor"}, {401, "Pilot Director", "director"}} {
				storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_people (id, name, tmdbpersonid, tmdbfetchedat)
					VALUES ($1, $2, $3, now())`, fmt.Sprint(p.id), p.name, fmt.Sprint(p.id))
				storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_itempeople (id, item_id, person_id, role)
					VALUES (gen_random_uuid()::varchar, $1, $2, $3)`, show1, fmt.Sprint(p.id), p.role)
			}

			switch path {
			case "enrichment":
				enrich(t, s, show1)
			case "refreshPeople":
				res, err := s.RefreshPeople(context.Background(), true)
				if err != nil {
					t.Fatal(err)
				}
				if res.CreditsDropped != 0 || res.PeopleDeleted != 0 || res.CreditsAdded != 2 {
					t.Errorf("refreshPeople %+v: want the two new credits added and none dropped", counts(res))
				}
			case "change list":
				f.changed("tv", "2026-10-02", 20)
				runSync(t, s, syncNow)
			}
			if got := credits(t, st, show1); got != longShowCredits {
				t.Errorf("after %s the series credits\n %s\nwant everyone of every season\n %s", path, got, longShowCredits)
			}
			if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_deleteditems WHERE type = 'person'`); n != 0 {
				t.Errorf("after %s %d people of earlier seasons were deleted", path, n)
			}
			if got := f.calls("/3/tv/20/credits"); len(got) != 0 {
				t.Errorf("the latest season's credits were read: %q", got)
			}
		})
	}
}

// Of a series' aggregate credits the catalog keeps the 20 in the most episodes,
// a tie going to TMDB's billing order, then the lower TMDB id; and the 20 who
// directed the most episodes, of the crew only those whose job is Director.
func TestSeriesCreditsKeepTheMostEpisodes(t *testing.T) {
	f := newFakeTMDB(t)
	var people []aggPerson
	for i := 1; i <= 25; i++ { // 25 actors: 100 episodes down to 76, but numbers 12 and 13 tie
		episodes := 101 - i
		if i == 13 {
			episodes = 101 - 12
		}
		people = append(people, aggPerson{int64(1000 + i), fmt.Sprintf("Actor %02d", i), episodes, 30 - i, ""})
	}
	people = append(people, aggPerson{1100, "Tied On Billing", 101 - 12, 30 - 12, ""}) // ties number 12 on order too
	for i := 1; i <= 22; i++ {
		people = append(people, aggPerson{int64(2000 + i), fmt.Sprintf("Director %02d", i), 50 - i, 0, "Director"})
	}
	people = append(people,
		aggPerson{3001, "Of Photography", 99, 0, "Director of Photography"},
		aggPerson{3002, "An Assistant", 98, 0, "Assistant Director"},
		aggPerson{3003, "A Writer", 97, 0, "Writer"})
	f.aggregate("tv/7", people...)
	c := newClient(func() string { return "test-token" }, "en-US")
	c.apiBase, c.http = f.srv.URL+"/3", &http.Client{Transport: loopbackOnly{}, Timeout: 10 * time.Second}

	cr, ok := c.getTvAggregateCredits(context.Background(), 7)
	if !ok {
		t.Fatal("aggregate credits not read")
	}
	var cast, crew []string
	for _, p := range cr.Cast {
		cast = append(cast, p.Name)
	}
	for _, p := range cr.Crew {
		crew = append(crew, p.Name)
	}
	// Billing order breaks the tie of 12 and 13 (13 is billed first: order 17
	// against 18), and the lower id the tie of 12 and "Tied On Billing".
	wantCast := "Actor 01, Actor 02, Actor 03, Actor 04, Actor 05, Actor 06, Actor 07, Actor 08, Actor 09, Actor 10, " +
		"Actor 11, Actor 13, Actor 12, Tied On Billing, Actor 14, Actor 15, Actor 16, Actor 17, Actor 18, Actor 19"
	if got := strings.Join(cast, ", "); got != wantCast {
		t.Errorf("cast\n %s\nwant\n %s", got, wantCast)
	}
	wantCrew := "Director 01, Director 02, Director 03, Director 04, Director 05, Director 06, Director 07, " +
		"Director 08, Director 09, Director 10, Director 11, Director 12, Director 13, Director 14, Director 15, " +
		"Director 16, Director 17, Director 18, Director 19, Director 20"
	if got := strings.Join(crew, ", "); got != wantCrew {
		t.Errorf("directors\n %s\nwant\n %s", got, wantCrew)
	}
	if got := f.calls("/3/tv/7/"); len(got) != 1 || !strings.HasPrefix(got[0], "/3/tv/7/aggregate_credits?") {
		t.Errorf("requests %q, want aggregate_credits only", got)
	}
}
