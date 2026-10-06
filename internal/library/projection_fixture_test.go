package library

import (
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zaentrum/katalog-manager/internal/config"
	"github.com/zaentrum/katalog-manager/internal/store"
	"github.com/zaentrum/katalog-manager/internal/store/storetest"
)

// The projection's fixture: two films, a series with three episodes, and
// three people, with what the tool's port has to get right (among it a
// thumb that is the poster's bytes, and a series' second poster whose hash
// sorts before the first's).
const (
	fxFilm    = "f001aeff-9c18-4183-b51b-51403af2515e"
	fxPlain   = "f2f2f2f2-0000-4000-8000-000000000002"
	fxSeries  = "5e5e5e5e-0000-4000-8000-000000000001"
	fxEp1     = "e1e1e1e1-0000-4000-8000-000000000011"
	fxEp2     = "e1e1e1e1-0000-4000-8000-000000000012"
	fxEp3     = "e1e1e1e1-0000-4000-8000-000000000021"
	fxAda     = "a1a1a1a1-0000-4000-8000-000000000001"
	fxBen     = "b2b2b2b2-0000-4000-8000-000000000002"
	fxCy      = "c3c3c3c3-0000-4000-8000-000000000003"
	fxAsOf    = "2026-10-06T12:00:00Z"
	fxItemsAt = "2026-10-01 06:00:01"
)

// png is a PNG header of w by h, and some bytes.
func pngOf(w, h uint32, tail string) []byte {
	b := []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\x0dIHDR")
	b = binary.BigEndian.AppendUint32(b, w)
	b = binary.BigEndian.AppendUint32(b, h)
	return append(b, []byte("\x08\x02\x00\x00\x00"+tail)...)
}

// jpegOf is a JPEG with an APP0 segment, then its SOF0 of w by h.
func jpegOf(w, h uint16, tail string) []byte {
	b := []byte{0xff, 0xd8, 0xff, 0xe0, 0x00, 0x10}
	b = append(b, []byte("JFIF\x00\x01\x01\x00\x00\x01\x00\x01\x00\x00")...)
	b = append(b, 0xff, 0xc0, 0x00, 0x11, 0x08)
	b = binary.BigEndian.AppendUint16(b, h)
	b = binary.BigEndian.AppendUint16(b, w)
	return append(b, []byte("\x03\x01\x22\x00"+tail)...)
}

// webpOf is a VP8X WebP of w by h.
func webpOf(w, h uint32) []byte {
	b := []byte("RIFF\x30\x00\x00\x00WEBPVP8X\x0a\x00\x00\x00\x00\x00\x00\x00")
	b = append(b, byte(w-1), byte((w-1)>>8), byte((w-1)>>16), byte(h-1), byte((h-1)>>8), byte((h-1)>>16))
	return append(b, []byte("....")...)
}

// fillProjection gives the catalog the fixture, every row as modified at a
// moment of its own.
func fillProjection(t *testing.T, st *store.Store) {
	t.Helper()
	ex := func(sql string, args ...any) { storetest.Exec(t, st, sql, args...) }
	ex(`INSERT INTO com_nalet_katalog_items (id, type, title, sorttitle, year, description, tagline, rating, durationms,
		parent_id, seasonnumber, episodenumber, metadatalocked, createdat, createdby) VALUES
		($1, 'movie', 'Sintel', 'sintel', 2010, '  A girl searches
for her dragon.  ', '   ', 7.5, 888000, NULL, NULL, NULL, false, '2026-09-01 10:00:00', 'katalog-manager/ingest'),
		($2, 'movie', 'Plain', NULL, NULL, NULL, NULL, 0, NULL, NULL, NULL, NULL, true, '2026-09-02 10:00:00', NULL),
		($3, 'series', 'Pioneer One', 'pioneer one', 2010, 'A probe falls.', 'Not alone.', 8.0, 2400000, NULL, NULL, NULL,
		 false, '2026-09-03 10:00:00', NULL),
		($4, 'episode', 'Earthfall', NULL, 2010, 'The pilot.', NULL, 7.1, 2100000, $3, 1, 1, false, '2026-09-03 10:00:01', NULL),
		($5, 'episode', 'The Man From Mars', NULL, NULL, NULL, NULL, NULL, NULL, $3, 1, 2, false, '2026-09-03 10:00:02', NULL),
		($6, 'episode', 'Sea Change', NULL, NULL, NULL, NULL, NULL, NULL, $3, 2, 1, false, '2026-09-03 10:00:03', NULL)`,
		fxFilm, fxPlain, fxSeries, fxEp1, fxEp2, fxEp3)
	ex(`INSERT INTO com_nalet_katalog_itemexternalids (id, item_id, source, externalid) VALUES
		('x1', $1, 'tmdb', '45745'), ('x2', $1, 'imdb', 'tt1727587'), ('x3', $1, 'omdb', 'z'),
		('x4', $2, 'tmdb', 'not-a-number'), ('x5', $3, 'tmdb', '39272'), ('x6', $4, 'tmdb-episode', '937631')`,
		fxFilm, fxPlain, fxSeries, fxEp1)
	ex(`INSERT INTO com_nalet_katalog_genres (id, name) VALUES ('g1', 'Fantasy'), ('g2', 'Animation'), ('g3', 'Science Fiction')`)
	ex(`INSERT INTO com_nalet_katalog_itemgenres (id, item_id, genre_id) VALUES ('ig1', $1, 'g1'), ('ig2', $1, 'g2'), ('ig3', $2, 'g3')`,
		fxFilm, fxSeries)
	ex(`INSERT INTO com_nalet_katalog_itemtags (id, item_id, tag) VALUES ('t1', $1, 'open-movie'), ('t2', $1, 'blender'),
		('t3', $1, 'blender')`, fxFilm)
	ex(`INSERT INTO com_nalet_katalog_people (id, name, sortname, alsoknownas, birthdate, deathdate, birthplace, biography,
		tmdbpersonid, imdbid, knownfordepartment, metadatalocked, lockedfields, fieldorigins, tmdbfetchedat, tmdbchangedat) VALUES
		($1, 'Ada Example', 'Example, Ada', '["Ada Example", "A. Example", "Ада Пример"]', '1815-12-10', '1852-11-27',
		 'London, England', '{"en": "  English.\nA second paragraph.  ", "de": "Deutsch.", "x y": "no language", "zh-Hans": ""}',
		 '101', 'nm0000001', 'Writing', false, '["biography", "biography", "name"]',
		 '{"name": "tmdb", "biography": "manual", "images": "tmdb", "bogus": "somewhere"}', '2026-10-01 08:00:00.75+02', '2026-09-30'),
		($2, 'Ben Example', NULL, NULL, NULL, NULL, NULL, NULL, '102', NULL, NULL, true, NULL, '{"name": "manual"}', NULL, NULL),
		($3, 'Cy Example', NULL, NULL, '1990-05-17', NULL, NULL, NULL, NULL, NULL, NULL, false, NULL, NULL, NULL, NULL)`, fxAda, fxBen, fxCy)
	ex(`INSERT INTO com_nalet_katalog_itempeople (id, item_id, person_id, role, ordinal, job, charactername, episodecount) VALUES
		('c1', $1, $4, 'actor', 1, NULL, 'Sintel', NULL),
		('c2', $1, $5, 'director', 0, 'Director', NULL, NULL),
		('c3', $1, $6, 'actor', NULL, NULL, NULL, NULL),
		('c4', $1, $6, 'Production Designer', NULL, NULL, NULL, NULL),
		('c5', $2, $4, 'creator', 0, 'Creator', NULL, 6),
		('c6', $2, $5, 'actor', 2, NULL, 'Lead', 6),
		('c7', $3, $5, 'actor', 0, NULL, 'Lead', NULL)`, fxFilm, fxSeries, fxEp1, fxAda, fxBen, fxCy)
	ex(`INSERT INTO com_nalet_katalog_itemtrailerlinks (id, item_id, source, site, externalid, url, title, durationsec) VALUES
		('l1', $1, 'tmdb', 'YouTube', 'eRsGyueVLvQ', 'https://www.youtube.com/watch?v=eRsGyueVLvQ', 'Trailer', 52),
		('l2', $1, 'manual', NULL, NULL, 'https://example.org/trailers/sintel-teaser', ' Teaser
 ', NULL),
		('l3', $1, 'legacy', NULL, NULL, 'https://example.org/', NULL, 0)`, fxFilm)
	poster, backdrop, still := pngOf(1000, 1500, "the poster"), jpegOf(1920, 1080, "the backdrop"), webpOf(640, 360)
	ex(`INSERT INTO com_nalet_katalog_itemartworkdata (id, item_id, kind, contenttype, bytes, fetchedat) VALUES
		('a1', $1, 'backdrop', 'image/jpeg', $3, '2026-09-01 10:00:00'),
		('a2', $1, 'fanart', 'image/png', $4, NULL),
		('a3', $1, 'logo', 'image/png', 'no image at all', NULL),
		('a4', $1, 'poster', 'image/png', $4, '2026-09-01 10:00:01'),
		('a5', $1, 'still', 'image/webp', $5, NULL),
		('a6', $1, 'thumb', 'image/png', $4, NULL),
		('a7', $2, 'poster', 'image/png', $6, '2026-09-03 10:00:00'),
		('a8', $2, 'poster', 'image/png', $7, NULL)`, fxFilm, fxSeries, backdrop, poster, still,
		pngOf(680, 1000, "the series' poster"), pngOf(680, 1000, "another poster 0"))
	ex(`INSERT INTO com_nalet_katalog_personartwork (id, person_id, kind, contenttype, bytes, sha256, width, height,
		isprimary, sourcepath, fetchedat) VALUES
		('pa1', $1, 'profile', 'image/png', $2, encode(sha256($2), 'hex'), NULL, NULL, false, NULL, '2026-09-01 02:00:00+02'),
		('pa2', $1, 'profile', 'image/jpeg', $3, encode(sha256($3), 'hex'), 421, 999, true, '/ada.jpg', '2026-10-01 06:00:00+00')`,
		fxAda, pngOf(185, 278, "an older portrait"), jpegOf(421, 632, "the portrait"))
	// Every row as modified at a moment of its own: what the triggers set
	// while the rows went in is the fixture's no more.
	ex(`UPDATE com_nalet_katalog_items SET modifiedat = $1::timestamp + (createdat - '2026-09-01')`, fxItemsAt)
	ex(`UPDATE com_nalet_katalog_people SET modifiedat = '2026-10-01 06:00:01+00', createdat = '2026-09-01 12:00:00+00'`)
}

// TestWriteTheProjectionFixture writes the fixture's export and a tree that
// holds its records but no projection, for library-v2-from-catalog.py
// --projections-only to project (see testdata/projection/README): run with
// KATALOG_PROJECTION_FIXTURE=<a folder>.
func TestWriteTheProjectionFixture(t *testing.T) {
	out := os.Getenv("KATALOG_PROJECTION_FIXTURE")
	if out == "" {
		t.Skip("set KATALOG_PROJECTION_FIXTURE to write the projection's fixture")
	}
	st := storetest.Open(t)
	fillProjection(t, st)
	raw, err := os.ReadFile("../../db/export/library-export.sql")
	if err != nil {
		t.Fatal(err)
	}
	var sql []string
	for _, line := range strings.Split(string(raw), "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), `\`) {
			sql = append(sql, line)
		}
	}
	ctx := context.Background()
	conn, err := st.Pool().Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// psql's variable shard, empty: the whole catalog
	results, err := conn.Conn().PgConn().Exec(ctx, strings.ReplaceAll(strings.Join(sql, "\n"), ":'shard'", "''")).ReadAll()
	conn.Conn().Close(ctx)
	conn.Release()
	if err != nil {
		t.Fatal(err)
	}
	doc, err := Decode(results[len(results)-1].Rows[0][0])
	if err != nil {
		t.Fatal(err)
	}
	d := doc.(Doc).With("exportedAt", fxAsOf)
	b, err := Encode(d)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(out, "export.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	p := PathsOf(config.Config{LibraryRoot: filepath.Join(out, "tree")})
	for _, id := range []string{fxFilm, fxPlain, fxSeries, fxEp1, fxEp2, fxEp3} {
		if _, err := p.EnsureItemRecord(ctx, st.Pool(), id); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []string{fxAda, fxBen, fxCy} {
		if err := WriteFile(filepath.Join(p.PersonDir(id), "person.json"), []byte("{}\n")); err != nil {
			t.Fatal(err)
		}
	}
}
