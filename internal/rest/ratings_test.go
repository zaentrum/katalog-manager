package rest

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zaentrum/katalog-manager/internal/store"
	"github.com/zaentrum/katalog-manager/internal/store/storetest"
)

// ratedTitles gives the catalog a title for each case a cap tells apart, each
// with artwork, a playback file and a subtitle (sub-<id>): films rated 12 and
// 16, an unrated one, a series rated 16 by an admin over TMDB's 12 with an
// episode rated as it, and an episode an admin rated 6.
func ratedTitles(t *testing.T, st *store.Store) {
	t.Helper()
	dir := t.TempDir()
	for _, it := range []struct{ id, typ, parent string }{
		{"m12", "movie", ""}, {"m16", "movie", ""}, {"unrated", "movie", ""},
		{"s16", "series", ""}, {"e-of-s16", "episode", "s16"}, {"e6", "episode", "s16"},
	} {
		storetest.AddItem(t, st, it.id, it.typ, it.id, it.parent)
		file := filepath.Join(dir, it.id+".mkv")
		if err := os.WriteFile(file, []byte("media of "+it.id), 0o644); err != nil {
			t.Fatal(err)
		}
		vtt := filepath.Join(dir, it.id+".vtt")
		if err := os.WriteFile(vtt, []byte("WEBVTT\n\n00:00:01.000 --> 00:00:02.000\n"+it.id+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_itemartworkdata (id, item_id, kind, contenttype, bytes)
			VALUES ('art-' || $1::varchar, $1, 'poster', 'image/jpeg', convert_to('poster of ' || $1::varchar, 'UTF8'))`, it.id)
		storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_playbackassets (id, item_id, path, isprimary)
			VALUES ('asset-' || $1::varchar, $1, $2, true)`, it.id, file)
		storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_subtitleassets (id, item_id, path, format, lang, label)
			VALUES ('sub-' || $1::varchar, $1, $2, 'vtt', 'en', 'English')`, it.id, vtt)
	}
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_items SET certification = r.c, certification_country = 'DE', min_age = r.a
		FROM (VALUES ('m12', '12', 12), ('m16', '16', 16), ('s16', '12', 12)) AS r(id, c, a)
		WHERE com_nalet_katalog_items.id = r.id`)
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_items SET min_age_override = 16 WHERE id = 's16'`)
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_items SET min_age_override = 6 WHERE id = 'e6'`)
}

// What a caller is served of each title, on the routes of one title: the
// poster, the playback file, its subtitle list and a subtitle, at each mount
// point the routes have. "served" or what a title there is not gets.
func servedOf(t *testing.T, h http.Handler, id string, auth []string) map[string]string {
	t.Helper()
	out := map[string]string{}
	check := func(route string, w *httptest.ResponseRecorder, served, missing func(code int, body string) bool) {
		t.Helper()
		switch code, body := w.Code, w.Body.String(); {
		case served(code, body):
			out[route] = "served"
		case missing(code, body):
			out[route] = "as missing"
		default:
			out[route] = http.StatusText(code) + " " + body
		}
	}
	for _, base := range []string{"/api/artwork/", "/api/manage/artwork/"} {
		check(base, get(h, base+id+"/poster"+auth[0], auth[1:]...),
			func(c int, b string) bool { return c == 200 && b == "poster of "+id },
			func(c int, b string) bool { return c == 404 && b == "404 page not found\n" })
	}
	check("play", get(h, "/api/play/"+id+auth[0], auth[1:]...),
		func(c int, b string) bool { return c == 200 && b == "media of "+id },
		func(c int, b string) bool { return c == 404 && b == "no playback asset for item\n" })
	check("subtitles", get(h, "/api/subtitles/items/"+id+auth[0], auth[1:]...),
		func(c int, b string) bool { return c == 200 && strings.Contains(b, `"sub-`+id+`"`) },
		func(c int, b string) bool { return c == 200 && b == "{\"subtitles\":[]}\n" })
	check("subtitle", get(h, "/api/subtitles/sub-"+id+auth[0], auth[1:]...),
		func(c int, b string) bool { return c == 200 && strings.Contains(b, "\n"+id+"\n") },
		func(c int, b string) bool { return c == 404 && b == "unknown subtitle\n" })
	return out
}

// A viewer capped at an age (a bearer's max_rating, or the cap its stream
// token carries) is served a title rated at most that age, and answered as
// for a title there is not for one above it and for an unrated one (the
// setting ratings.unrated_for_capped hides them unless it says show); an
// episode is held to its series' rating unless an admin rated it. Everyone
// else is served everything.
func TestACappedViewerIsServedOnlyWhatTheCapAllows(t *testing.T) {
	st := storetest.Open(t)
	ratedTitles(t, st)
	h, iss := server(t, st, testConfig(t.TempDir()))
	exp := time.Now().Add(time.Hour)
	bearer := func(claims map[string]any) []string {
		c := map[string]any{"sub": "kid-1", "azp": "zaentrum-web", "aud": "chino",
			"realm_access": map[string]any{"roles": []string{"zaentrum-user"}}}
		for k, v := range claims {
			c[k] = v
		}
		return []string{"", "Authorization", "Bearer " + iss.Token(t, c)}
	}
	stream := func(user string) []string { return []string{"?stream=" + streamToken(user, exp)} }

	titles := []string{"m12", "m16", "unrated", "s16", "e-of-s16", "e6"}
	everything := map[string]bool{"m12": true, "m16": true, "unrated": true, "s16": true, "e-of-s16": true, "e6": true}
	cases := []struct {
		name   string
		auth   []string
		served map[string]bool
	}{
		{"a bearer capped at 12", bearer(map[string]any{"max_rating": 12}), map[string]bool{"m12": true, "e6": true}},
		{"a bearer capped at 16", bearer(map[string]any{"max_rating": 16}),
			map[string]bool{"m12": true, "m16": true, "s16": true, "e-of-s16": true, "e6": true}},
		{"a bearer capped at 5", bearer(map[string]any{"max_rating": 5}), map[string]bool{}},
		{"a bearer whose cap is no number", bearer(map[string]any{"max_rating": "16"}), map[string]bool{}},
		{"a stream token capped at 12", stream("kid-1;max_rating=12"), map[string]bool{"m12": true, "e6": true}},
		{"a stream token capped at 6", stream("kid-1;max_rating=6"), map[string]bool{"e6": true}},
		{"a bearer without a cap", bearer(nil), everything},
		{"an admin", []string{"", "Authorization", "Bearer " + iss.Admin(t)}, everything},
		{"a stream token without a cap", stream("user-1"), everything},
	}
	for _, c := range cases {
		for _, id := range titles {
			for route, got := range servedOf(t, h, id, c.auth) {
				if route != "/api/artwork/" && route != "/api/manage/artwork/" && strings.HasPrefix(c.auth[0], "?stream=") {
					continue // a stream token reads artwork alone
				}
				want := "as missing"
				if c.served[id] {
					want = "served"
				}
				if got != want {
					t.Errorf("%s, %s of %s: %s, want %s", c.name, route, id, got, want)
				}
			}
		}
	}

	// ratings.unrated_for_capped says show: unrated titles are served to the
	// capped too (the setting is read anew within half a minute).
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_settings (id, key, valuetext) VALUES ('u', 'ratings.unrated_for_capped', ' Show ')`)
	h, iss = server(t, st, testConfig(t.TempDir()))
	tok := []string{"", "Authorization", "Bearer " + iss.Token(t, map[string]any{"sub": "kid-1", "max_rating": 12})}
	for id, want := range map[string]string{"unrated": "served", "m16": "as missing", "m12": "served"} {
		if got := servedOf(t, h, id, tok)["play"]; got != want {
			t.Errorf("unrated shown, %s: %s, want %s", id, got, want)
		}
	}
}
