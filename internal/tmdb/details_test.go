package tmdb

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zaentrum/katalog-manager/internal/store"
	"github.com/zaentrum/katalog-manager/internal/store/storetest"
)

// testJPEG and testPNG are small real images, so their size can be read back.
func testJPEG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	img.Set(0, 0, color.RGBA{200, 10, 10, 255})
	var b bytes.Buffer
	if err := jpeg.Encode(&b, img, nil); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func testPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, image.NewRGBA(image.Rect(0, 0, w, h))); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func sha(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// samePerson fails t unless the person's row is want: a JSON object of the
// columns below (fetched: whether tmdbfetchedat is set).
func samePerson(t *testing.T, st *store.Store, id, want string) {
	t.Helper()
	var got string
	var same bool
	err := st.Pool().QueryRow(context.Background(), `SELECT j::text, j = $2::jsonb FROM (SELECT jsonb_build_object(
			'name', name, 'sortName', sortname, 'aka', alsoknownas, 'birth', birthdate, 'death', deathdate,
			'place', birthplace, 'bio', biography, 'tmdb', tmdbpersonid, 'imdb', imdbid,
			'dept', knownfordepartment, 'locked', metadatalocked, 'lockedFields', lockedfields,
			'origins', fieldorigins, 'changed', tmdbchangedat, 'fetched', tmdbfetchedat IS NOT NULL) AS j
		FROM com_nalet_katalog_people WHERE id = $1) x`, id, want).Scan(&got, &same)
	if err != nil {
		t.Fatalf("person %s: %v", id, err)
	}
	if !same {
		t.Errorf("person %s:\n got  %s\n want %s", id, got, want)
	}
}

// profile is a person's profile images as "sourcepath type sha256 w×h primary", one per line.
func profile(t *testing.T, st *store.Store, personID string) string {
	t.Helper()
	rows, err := st.Pool().Query(context.Background(), `SELECT COALESCE(sourcepath, '-'), contenttype, sha256,
			COALESCE(width, 0), COALESCE(height, 0), isprimary, sha256 = encode(sha256(bytes), 'hex'), fetchedat IS NOT NULL
		FROM com_nalet_katalog_personartwork WHERE person_id = $1 AND kind = 'profile' ORDER BY id`, personID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var path, ct, sum string
		var w, h int
		var primary, hashOK, fetched bool
		if err := rows.Scan(&path, &ct, &sum, &w, &h, &primary, &hashOK, &fetched); err != nil {
			t.Fatal(err)
		}
		if !hashOK || !fetched {
			t.Errorf("profile %s of %s: sha256 matches its bytes %v, fetchedat set %v", path, personID, hashOK, fetched)
		}
		line := path + " " + ct + " " + sum + " " + strconv.Itoa(w) + "×" + strconv.Itoa(h)
		if primary {
			line += " primary"
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

// adaOnTMDB is what TMDB holds about one person, in German and English.
func adaOnTMDB() *fakePerson {
	return &fakePerson{
		Name:       "Ada Example",
		Aliases:    []string{"Ада Пример", "A. Example", "Ada Example", "  ", "A.\nExample"},
		Bio:        map[string]string{"de": " Deutsch.\r\nZweite Zeile\u0007 ", "en": "English. "},
		Birthday:   "1815-12-10",
		Deathday:   "1852-11-27",
		Place:      "London,\nEngland",
		Department: "Writing",
		Imdb:       "nm0000001",
		Profile:    "/ada.jpg",
	}
}

const adaStored = `{"name": "Ada Example", "sortName": null, "aka": ["A. Example", "Ада Пример"],
	"birth": "1815-12-10", "death": "1852-11-27", "place": "London, England",
	"bio": {"de": "Deutsch.\nZweite Zeile", "en": "English."}, "tmdb": "101", "imdb": "nm0000001",
	"dept": "Writing", "locked": false, "lockedFields": null, "changed": null, "fetched": true,
	"origins": {"name": "tmdb", "alsoKnownAs": "tmdb", "birthDate": "tmdb", "deathDate": "tmdb",
	            "birthPlace": "tmdb", "biography": "tmdb", "knownForDepartment": "tmdb",
	            "externalIds": "tmdb", "images": "tmdb"}}`

// A credited person the catalog has not read yet gets their details and their
// profile when the title is enriched: in the catalog's language and in
// English, kept as the library record keeps them, each field marked as TMDB's.
// Once read, a title crediting them again does not read them again.
func TestCreditedPeopleGetTheirDetails(t *testing.T) {
	st := storetest.Open(t)
	f := newFakeTMDB(t)
	s := newTestService(t, st, f, "de-DE")
	addTitle(t, st, film1, "movie", "First Film", 10)
	f.movie(10, "First Film")
	f.cast("movie/10", []string{"101", "Ada Example"})
	f.person(101, adaOnTMDB())
	portrait := testJPEG(t, 3, 2)
	f.image("/ada.jpg", portrait)

	enrich(t, s, film1)

	var ada string
	if err := st.Pool().QueryRow(context.Background(),
		`SELECT id FROM com_nalet_katalog_people WHERE tmdbpersonid = '101'`).Scan(&ada); err != nil {
		t.Fatal(err)
	}
	samePerson(t, st, ada, adaStored)
	if got, want := profile(t, st, ada), "/ada.jpg image/jpeg "+sha(portrait)+" 3×2 primary"; got != want {
		t.Errorf("profile: %s, want %s", got, want)
	}
	if got := f.calls("/3/person/101"); len(got) != 2 ||
		got[0] != "/3/person/101?language=de-DE&append_to_response=external_ids,images" ||
		got[1] != "/3/person/101?language=en-US" {
		t.Errorf("person requests: %q, want the details in German with ids and images, then English", got)
	}
	if got := f.calls("/t/p/"); len(got) != 1 || !strings.HasPrefix(got[0], "/t/p/h632/ada.jpg") {
		t.Errorf("image requests: %q", got)
	}

	f.forget()
	enrich(t, s, film1)
	if got := f.calls("/3/person/"); len(got) != 0 {
		t.Errorf("a person already read was read again with the title: %q", got)
	}
}

// In English the biography needs one request.
func TestDetailsInEnglishAskOnce(t *testing.T) {
	st := storetest.Open(t)
	f := newFakeTMDB(t)
	s := newTestService(t, st, f, "en-US")
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_people (id, name, tmdbpersonid) VALUES ('ada', 'Ada', '101')`)
	f.person(101, adaOnTMDB())

	if out, err := s.refreshPerson(context.Background(), "ada", 101, nil); err != nil || out != personRefreshed {
		t.Fatalf("refreshPerson: %v %v", out, err)
	}
	if got := f.calls("/3/person/"); len(got) != 1 {
		t.Errorf("person requests: %q, want one", got)
	}
	var bio string
	storetestScan(t, st, `SELECT biography::text FROM com_nalet_katalog_people WHERE id = 'ada'`, &bio)
	if bio != `{"en": "English."}` {
		t.Errorf("biography %s", bio)
	}
}

func storetestScan(t *testing.T, st *store.Store, sql string, dst ...any) {
	t.Helper()
	if err := st.Pool().QueryRow(context.Background(), sql).Scan(dst...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

// A refresh leaves alone every field a lock names and keeps that field's
// origin; a locked record is not even read from TMDB.
func TestPersonRefreshHonoursLocks(t *testing.T) {
	st := storetest.Open(t)
	f := newFakeTMDB(t)
	s := newTestService(t, st, f, "en-US")
	kept := testPNG(t, 4, 4)
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_people
		(id, name, tmdbpersonid, imdbid, biography, birthplace, lockedfields, fieldorigins, metadatalocked) VALUES
		('some', 'Ada By Hand', '101', 'nm0000009', '{"en": "Written by hand."}', 'Old Place',
		 '["biography", "images", "externalIds.imdb", "name"]',
		 '{"name": "manual", "biography": "manual", "images": "manual", "externalIds": "manual", "birthPlace": "manual"}', false),
		('all', 'Locked Person', '102', NULL, NULL, NULL, NULL, '{"name": "manual"}', true)`)
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_personartwork
		(id, person_id, contenttype, bytes, sha256, width, height, isprimary, sourcepath, fetchedat)
		VALUES ('kept', 'some', 'image/png', $1, $2, 4, 4, true, NULL, now())`, kept, sha(kept))
	f.person(101, adaOnTMDB())
	f.person(102, adaOnTMDB())
	f.image("/ada.jpg", testJPEG(t, 3, 2))

	ctx := context.Background()
	if out, err := s.refreshPerson(ctx, "some", 101, nil); err != nil || out != personRefreshed {
		t.Fatalf("refreshPerson(some): %v %v", out, err)
	}
	samePerson(t, st, "some", `{"name": "Ada By Hand", "sortName": null, "aka": ["A. Example", "Ада Пример"],
		"birth": "1815-12-10", "death": "1852-11-27", "place": "London, England",
		"bio": {"en": "Written by hand."}, "tmdb": "101", "imdb": "nm0000009", "dept": "Writing",
		"locked": false, "lockedFields": ["biography", "images", "externalIds.imdb", "name"], "changed": null, "fetched": true,
		"origins": {"name": "manual", "biography": "manual", "images": "manual", "externalIds": "manual",
		            "birthPlace": "tmdb", "alsoKnownAs": "tmdb", "birthDate": "tmdb", "deathDate": "tmdb",
		            "knownForDepartment": "tmdb"}}`)
	if got, want := profile(t, st, "some"), "- image/png "+sha(kept)+" 4×4 primary"; got != want {
		t.Errorf("a locked profile changed: %s, want %s", got, want)
	}
	if got := f.calls("/t/p/"); len(got) != 0 {
		t.Errorf("a locked profile was fetched: %q", got)
	}

	f.forget()
	if out, err := s.refreshPerson(ctx, "all", 102, nil); err != nil || out != personLocked {
		t.Fatalf("refreshPerson(all): %v %v, want locked", out, err)
	}
	if got := f.calls("/"); len(got) != 0 {
		t.Errorf("a locked record was read from TMDB: %q", got)
	}
	samePerson(t, st, "all", `{"name": "Locked Person", "sortName": null, "aka": null, "birth": null, "death": null,
		"place": null, "bio": null, "tmdb": "102", "imdb": null, "dept": null, "locked": true, "lockedFields": null,
		"origins": {"name": "manual"}, "changed": null, "fetched": false}`)

	// A title crediting a locked person does not read them either.
	addTitle(t, st, film1, "movie", "First Film", 10)
	f.movie(10, "First Film")
	f.cast("movie/10", []string{"102", "Someone Else"})
	enrich(t, s, film1)
	if got := f.calls("/3/person/"); len(got) != 0 {
		t.Errorf("enrichment read a locked person: %q", got)
	}
}

// A person whose details cannot be read never fails the title that credits
// them, and nothing of them is stored half: when the profile cannot be had,
// neither are the details. The next enrichment tries again.
func TestFailingPersonFetchDoesNotFailTheTitle(t *testing.T) {
	for _, broken := range []string{"/3/person/101", "/t/p/h632/ada.jpg"} {
		t.Run(broken, func(t *testing.T) {
			st := storetest.Open(t)
			f := newFakeTMDB(t)
			s := newTestService(t, st, f, "de-DE")
			addTitle(t, st, film1, "movie", "Filename Title", 10)
			f.movie(10, "First Film")
			f.cast("movie/10", []string{"101", "Ada Example"})
			f.person(101, adaOnTMDB())
			f.image("/ada.jpg", testJPEG(t, 3, 2))
			f.failing(broken, 500)

			enrich(t, s, film1) // done, although the person could not be read
			if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_items WHERE id = $1 AND title = 'First Film'`, film1); n != 1 {
				t.Error("the title was not enriched")
			}
			if got := credits(t, st, film1); got != "actor Ada Example (101)" {
				t.Errorf("credits %q", got)
			}
			samePerson(t, st, personWithTMDB(t, st, "101"), `{"name": "Ada Example", "sortName": null, "aka": null,
				"birth": null, "death": null, "place": null, "bio": null, "tmdb": "101", "imdb": null, "dept": null,
				"locked": false, "lockedFields": null, "origins": {"name": "tmdb", "externalIds": "tmdb"},
				"changed": null, "fetched": false}`)

			f.failing(broken, 0)
			enrich(t, s, film1)
			samePerson(t, st, personWithTMDB(t, st, "101"), adaStored)
		})
	}
}

func personWithTMDB(t *testing.T, st *store.Store, tmdbID string) string {
	t.Helper()
	var id string
	storetestScan(t, st, `SELECT id FROM com_nalet_katalog_people WHERE tmdbpersonid = '`+tmdbID+`'`, &id)
	return id
}

// Reading a person again changes only what TMDB changed: the same answer moves
// tmdbfetchedat but not modifiedat, and fetches no image; a new profile
// replaces the old; what TMDB no longer has is cleared, origin and all.
func TestPersonRefreshFollowsTMDB(t *testing.T) {
	st := storetest.Open(t)
	f := newFakeTMDB(t)
	s := newTestService(t, st, f, "en-US")
	ctx := context.Background()
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_people (id, name, tmdbpersonid) VALUES ('ada', 'Ada', '101')`)
	ada := adaOnTMDB()
	f.person(101, ada)
	first := testJPEG(t, 3, 2)
	f.image("/ada.jpg", first)
	refresh := func(changedOn *time.Time) {
		t.Helper()
		if out, err := s.refreshPerson(ctx, "ada", 101, changedOn); err != nil || out != personRefreshed {
			t.Fatalf("refreshPerson: %v %v", out, err)
		}
	}
	stamps := func() (fetched, modified time.Time) {
		t.Helper()
		storetestScan(t, st, `SELECT tmdbfetchedat, modifiedat FROM com_nalet_katalog_people WHERE id = 'ada'`, &fetched, &modified)
		return
	}

	refresh(nil)
	fetched1, modified1 := stamps()
	f.forget()
	refresh(nil)
	fetched2, modified2 := stamps()
	if !fetched2.After(fetched1) || !modified2.Equal(modified1) {
		t.Errorf("the same answer again: fetched %s → %s, modified %s → %s; want fetched to move and modified not",
			fetched1, fetched2, modified1, modified2)
	}
	if got := f.calls("/t/p/"); len(got) != 0 {
		t.Errorf("an unchanged profile was fetched again: %q", got)
	}

	second := testPNG(t, 5, 7)
	f.image("/ada-2.png", second)
	ada.Profile, ada.Deathday, ada.Place = "/ada-2.png", "", ""
	day := time.Date(2026, 9, 30, 23, 0, 0, 0, time.FixedZone("UTC+3", 3*3600)) // the 30th in UTC
	refresh(&day)
	if got, want := profile(t, st, "ada"), "/ada-2.png image/png "+sha(second)+" 5×7 primary"; got != want {
		t.Errorf("a new profile: %s, want %s", got, want)
	}
	if _, modified3 := stamps(); !modified3.After(modified2) {
		t.Error("modifiedat did not move although the person changed")
	}
	var changed string
	storetestScan(t, st, `SELECT to_char(tmdbchangedat, 'YYYY-MM-DD') FROM com_nalet_katalog_people WHERE id = 'ada'`, &changed)
	if changed != "2026-09-30" {
		t.Errorf("tmdbchangedat %s, want the day the change list named, 2026-09-30", changed)
	}
	var death, place *string
	var origins string
	storetestScan(t, st, `SELECT deathdate::text, birthplace, fieldorigins::text FROM com_nalet_katalog_people WHERE id = 'ada'`,
		&death, &place, &origins)
	if death != nil || place != nil || strings.Contains(origins, "deathDate") || strings.Contains(origins, "birthPlace") {
		t.Errorf("what TMDB no longer has stays: death %v, place %v, origins %s", death, place, origins)
	}

	ada.Profile = ""
	refresh(nil)
	if got := profile(t, st, "ada"); got != "" {
		t.Errorf("TMDB has no profile any more, the catalog still does: %s", got)
	}
	storetestScan(t, st, `SELECT fieldorigins::text FROM com_nalet_katalog_people WHERE id = 'ada'`, &origins)
	if strings.Contains(origins, "images") {
		t.Errorf("the origin of a removed profile stays: %s", origins)
	}
	// An older change-list day never moves tmdbchangedat back.
	older := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	refresh(&older)
	storetestScan(t, st, `SELECT to_char(tmdbchangedat, 'YYYY-MM-DD') FROM com_nalet_katalog_people WHERE id = 'ada'`, &changed)
	if changed != "2026-09-30" {
		t.Errorf("tmdbchangedat moved back to %s", changed)
	}
}

// TMDB not knowing a person changes nothing about them; a death TMDB dates
// before the birth is not kept.
func TestPersonGoneAndContradictoryDates(t *testing.T) {
	st := storetest.Open(t)
	f := newFakeTMDB(t)
	s := newTestService(t, st, f, "en-US")
	ctx := context.Background()
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_people (id, name, tmdbpersonid) VALUES ('gone', 'Gone', '999'), ('odd', 'Odd', '103')`)

	if out, err := s.refreshPerson(ctx, "gone", 999, nil); err != nil || out != personGone {
		t.Fatalf("refreshPerson of an id TMDB does not know: %v %v, want gone", out, err)
	}
	samePerson(t, st, "gone", `{"name": "Gone", "sortName": null, "aka": null, "birth": null, "death": null,
		"place": null, "bio": null, "tmdb": "999", "imdb": null, "dept": null, "locked": false,
		"lockedFields": null, "origins": null, "changed": null, "fetched": false}`)

	f.person(103, &fakePerson{Name: "Odd", Birthday: "1990-05-01", Deathday: "1890-05-01", Imdb: "tt0000001"})
	if _, err := s.refreshPerson(ctx, "odd", 103, nil); err != nil {
		t.Fatal(err)
	}
	samePerson(t, st, "odd", `{"name": "Odd", "sortName": null, "aka": null, "birth": "1990-05-01", "death": null,
		"place": null, "bio": null, "tmdb": "103", "imdb": null, "dept": null, "locked": false, "lockedFields": null,
		"origins": {"name": "tmdb", "birthDate": "tmdb", "externalIds": "tmdb"}, "changed": null, "fetched": true}`)
}
