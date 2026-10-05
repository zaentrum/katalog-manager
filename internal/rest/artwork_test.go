package rest

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/zaentrum/katalog-manager/internal/auth"
	"github.com/zaentrum/katalog-manager/internal/auth/authtest"
	"github.com/zaentrum/katalog-manager/internal/config"
	"github.com/zaentrum/katalog-manager/internal/itemactions"
	"github.com/zaentrum/katalog-manager/internal/processing"
	"github.com/zaentrum/katalog-manager/internal/store"
	"github.com/zaentrum/katalog-manager/internal/store/storetest"
)

var streamKey = []byte("0123456789abcdef0123456789abcdef")

// streamToken mints a stream token as chino-api does, for subject, expiring at exp.
func streamToken(subject string, exp time.Time) string {
	payload := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf("%s|%d", subject, exp.Unix())))
	mac := hmac.New(sha256.New, streamKey)
	mac.Write([]byte(payload))
	return payload + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// testConfig tells callers apart as the platform's realm does, and roots the
// library in dir.
func testConfig(dir string) config.Config {
	return config.Config{AdminRole: "zaentrum-admin", AddonRole: "zaentrum-addon", RolesClaim: auth.DefaultRolesClaim,
		ServiceClients: []string{"zaentrum-manager"}, NFSRoot: dir + "/media", PackagesRoot: dir + "/packages"}
}

// router serves the REST routes as the service does: behind the auth
// middleware, which takes a stream token and the bearer tokens of a test
// issuer.
func router(t *testing.T, st *store.Store) http.Handler {
	h, _ := server(t, st, testConfig(t.TempDir()))
	return h
}

// server is router with cfg, and the issuer whose tokens it takes.
func server(t *testing.T, st *store.Store, cfg config.Config) (http.Handler, *authtest.Issuer) {
	t.Helper()
	iss := authtest.NewIssuer(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel) // stops the verifier's discovery retries, if any
	jwt, err := auth.NewJWTVerifier(ctx, iss.URL, "chino", false, false)
	if err != nil {
		t.Fatal(err)
	}
	stream, err := auth.NewStreamVerifier(base64.StdEncoding.EncodeToString(streamKey))
	if err != nil {
		t.Fatal(err)
	}
	r := chi.NewRouter()
	r.Group(func(pr chi.Router) {
		pr.Use(auth.NewMiddleware(jwt, stream).Handler)
		steps := processing.New(st.Pool())
		New(Deps{Store: st, Cfg: cfg, Steps: steps, Packager: itemactions.New(st, cfg, steps, nil)}).Register(pr)
	})
	return r, iss
}

func get(h http.Handler, path string, header ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

// A person's portrait is their primary profile image, at both mount points,
// with its content type, an ETag of its sha256 and the caching of a title's
// artwork; a caller holding it gets 304, and a person without one 404.
func TestPersonPortrait(t *testing.T) {
	st := storetest.Open(t)
	primary, older := []byte("\x89PNG the primary portrait"), []byte("\xff\xd8\xff an older one")
	sum := sha256.Sum256(primary)
	etag := `"` + hex.EncodeToString(sum[:]) + `"`
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_people (id, name) VALUES ('p1', 'Ada'), ('p2', 'Ben'), ('p3', 'Cy')`)
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_personartwork (id, person_id, kind, contenttype, bytes, sha256, isprimary)
		VALUES ('a1', 'p1', 'profile', 'image/jpeg', $1, encode(sha256($1), 'hex'), false),
		       ('a2', 'p1', 'profile', 'image/png', $2, $3, true),
		       ('a3', 'p2', 'profile', 'image/jpeg', $1, encode(sha256($1), 'hex'), false)`,
		older, primary, hex.EncodeToString(sum[:]))
	h := router(t, st)
	tok := "?stream=" + streamToken("viewer-1", time.Now().Add(time.Hour))

	for _, base := range []string{"/api/artwork/person/", "/api/manage/artwork/person/"} {
		w := get(h, base+"p1/profile"+tok)
		if w.Code != http.StatusOK || !bytes.Equal(w.Body.Bytes(), primary) {
			t.Fatalf("%sp1/profile: %d %q, want 200 and the primary portrait", base, w.Code, w.Body.Bytes())
		}
		for k, want := range map[string]string{"Content-Type": "image/png", "ETag": etag, "Cache-Control": "public, max-age=604800"} {
			if got := w.Header().Get(k); got != want {
				t.Errorf("%s: %s %q, want %q", base, k, got, want)
			}
		}
		for _, held := range []string{etag, "W/" + etag, `"other", ` + etag, "*"} {
			w := get(h, base+"p1/profile"+tok, "If-None-Match", held)
			if w.Code != http.StatusNotModified || w.Body.Len() != 0 || w.Header().Get("ETag") != etag ||
				w.Header().Get("Cache-Control") == "" {
				t.Errorf("If-None-Match %s: %d, %d bytes, ETag %q, want 304 with no body and the ETag",
					held, w.Code, w.Body.Len(), w.Header().Get("ETag"))
			}
		}
		if w := get(h, base+"p1/profile"+tok, "If-None-Match", `"other"`); w.Code != http.StatusOK {
			t.Errorf("If-None-Match of another image: %d, want 200", w.Code)
		}
		for _, person := range []string{"p2", "p3", "nobody"} { // only images that are not primary, none, no person
			if w := get(h, base+person+"/profile"+tok); w.Code != http.StatusNotFound {
				t.Errorf("%s%s/profile: %d, want 404", base, person, w.Code)
			}
		}
	}
}

// The portrait takes what a title's artwork takes: a stream token (or a
// bearer JWT), and nothing less.
func TestPersonPortraitIsReadAsArtworkIs(t *testing.T) {
	st := storetest.Open(t)
	storetest.AddItem(t, st, "m1", "movie", "A Film", "")
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_itemartworkdata (id, item_id, kind, contenttype, bytes)
		VALUES ('d1', 'm1', 'poster', 'image/jpeg', '\xffd8')`)
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_people (id, name) VALUES ('p1', 'Ada')`)
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_personartwork (id, person_id, contenttype, bytes, sha256, isprimary)
		VALUES ('a1', 'p1', 'image/jpeg', '\xffd8', repeat('a', 64), true)`)
	h := router(t, st)
	for _, path := range []string{"/api/artwork/m1/poster", "/api/artwork/person/p1/profile",
		"/api/manage/artwork/m1/poster", "/api/manage/artwork/person/p1/profile"} {
		for _, tc := range []struct {
			query string
			code  int
		}{
			{"", http.StatusUnauthorized},
			{"?stream=" + streamToken("viewer-1", time.Now().Add(-time.Minute)), http.StatusUnauthorized},
			{"?stream=forged.token", http.StatusUnauthorized},
			{"?stream=" + streamToken("viewer-1", time.Now().Add(time.Hour)), http.StatusOK},
		} {
			if w := get(h, path+tc.query, "Authorization", "Bearer not-a-token"); w.Code != tc.code {
				t.Errorf("%s%s: %d, want %d", path, tc.query, w.Code, tc.code)
			}
		}
	}
}

// A catalog older than migration 030 keeps no images of people: every
// portrait is 404, not a failure.
func TestPersonPortraitWithoutThePeopleMigration(t *testing.T) {
	st := storetest.OpenBase(t)
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_people (id, name) VALUES ('p1', 'Ada')`)
	w := get(router(t, st), "/api/artwork/person/p1/profile?stream="+streamToken("viewer-1", time.Now().Add(time.Hour)))
	if w.Code != http.StatusNotFound {
		t.Errorf("a portrait without 030: %d, want 404", w.Code)
	}
}

// A keyframe the analyzer uploads becomes the image of its kind and is marked
// as one, also beside a URL row a match left whose image could not be
// fetched; uploaded again, it is marked once.
func TestAnUploadedKeyframeIsMarkedAsOne(t *testing.T) {
	st := storetest.Open(t)
	storetest.AddItem(t, st, "m1", "movie", "A Film", "")
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_itemartwork (id, item_id, kind, url)
		VALUES ('tmdb-backdrop', 'm1', 'backdrop', 'https://image.example.com/t/p/w1280/b.jpg')`)
	h, iss := server(t, st, testConfig(t.TempDir()))
	svc := iss.Service(t, "zaentrum-manager")
	for run := 1; run <= 2; run++ {
		if w := do(h, http.MethodPut, "/api/artwork/m1/backdrop", "a keyframe", svc); w.Code != http.StatusNoContent {
			t.Fatalf("PUT the keyframe, %d. time: %d %s", run, w.Code, w.Body.String())
		}
	}
	if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_itemartwork
		WHERE item_id = 'm1' AND kind = 'backdrop' AND url = 'extracted:keyframe'`); n != 1 {
		t.Errorf("%d keyframe markers, want 1", n)
	}
	if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_itemartworkdata
		WHERE item_id = 'm1' AND kind = 'backdrop' AND bytes = 'a keyframe'`); n != 1 {
		t.Error("the keyframe is not the backdrop")
	}
}
