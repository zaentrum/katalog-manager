package library

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zaentrum/katalog-manager/internal/config"
	"github.com/zaentrum/katalog-manager/internal/library/librarytest"
	"github.com/zaentrum/katalog-manager/internal/processing"
	"github.com/zaentrum/katalog-manager/internal/store"
	"github.com/zaentrum/katalog-manager/internal/store/storetest"
)

// A store before the library, staged by a run: a film with its original, a
// sidecar it was packaged with and an extra; a film whose original was gone
// before the library recorded it; a series and its episode; a person.
const (
	mgFilm   = "a0a0a0a0-0000-4000-8000-00000000000a"
	mgGone   = "b0b0b0b0-0000-4000-8000-00000000000b"
	mgShow   = "c0c0c0c0-0000-4000-8000-00000000000c"
	mgEp     = "d0d0d0d0-0000-4000-8000-00000000000d"
	mgExtra  = "e0e0e0e0-0000-4000-8000-00000000000e"
	mgPerson = "f0f0f0f0-0000-4000-8000-00000000000f"
	mgRun    = "2026-10-07a"
)

type migrationFixture struct {
	st     *store.Store
	cfg    config.Config
	p      Paths
	dir    string
	runDir string
	units  map[string]*Unit
	// into has packaged move an original into its version's folder, as
	// the tool stages it now, not to the arrivals.
	into bool
}

// mgPackage is a package's record of the fixture: its renditions, as
// packaging-complete reads the packaged row from them.
var mgPackage = map[string]any{
	"renditions": map[string]any{
		"video": []any{map[string]any{"id": "v0", "dir": "hls/v0", "codec": "hvc1.1.6.L120.90", "width": 1920, "height": 1080}},
		"audio": []any{map[string]any{"id": "a0", "dir": "hls/a0", "codec": "mp4a.40.2", "language": "en", "channels": 2,
			"default": true, "bitrateBps": 128000}}},
	"subtitles":        []any{map[string]any{"id": "s0", "path": "subs/0.vtt"}, map[string]any{"id": "s1", "path": "subs/1.vtt"}},
	"peakBandwidthBps": 4000000, "durationMs": 600000,
	"essence": map[string]any{"surround": false, "maxAudioChannels": 2},
}

func newMigrationFixture(t *testing.T) *migrationFixture {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{Share: dir, NFSRoot: filepath.Join(dir, "media"), PackagesRoot: filepath.Join(dir, "packages")}
	f := &migrationFixture{st: storetest.Open(t), cfg: cfg, p: PathsOf(cfg), dir: dir, units: map[string]*Unit{}}
	f.runDir = f.p.MigrationDir(mgRun)
	storetest.AddItem(t, f.st, mgFilm, "movie", "Example Film", "")
	storetest.AddItem(t, f.st, mgGone, "movie", "Gone Film", "")
	storetest.AddItem(t, f.st, mgShow, "series", "Example Show", "")
	storetest.AddItem(t, f.st, mgEp, "episode", "Pilot", mgShow)
	storetest.Exec(t, f.st, `UPDATE com_nalet_katalog_items SET seasonnumber = 1, episodenumber = 1 WHERE id = $1`, mgEp)
	storetest.Exec(t, f.st, `INSERT INTO com_nalet_katalog_people (id, name, modifiedat) VALUES ($1, 'Ada Example', now())`, mgPerson)
	// As the export had them, a while before the adoption.
	storetest.Exec(t, f.st, `UPDATE com_nalet_katalog_items SET modifiedat = '2026-10-01 09:00:00.25'`)
	storetest.Exec(t, f.st, `UPDATE com_nalet_katalog_people SET modifiedat = '2026-10-01 09:00:00.25+00'`)

	f.packaged(t, mgFilm, "media/Film (2020)/Film.mkv", true)
	f.packaged(t, mgGone, "", false)
	f.packaged(t, mgEp, "media/Example Show/Season 01/Example Show S01E01.mkv", false)
	staged := f.stagedDir(mgShow)
	f.record(t, mgShow, staged)
	f.unit(t, mgShow, "series", []Move{{MovePublish, staged, f.itemDir(mgShow)}}, nil, UnitDB{})

	person := filepath.Join(f.runDir, "staged-people", mgPerson)
	librarytest.Write(t, filepath.Join(person, "person.json"), librarytest.JSON(t, map[string]any{
		"schema": "zaentrum.library.person/2", "personId": mgPerson, "databaseUpdatedAt": f.updatedAt(t, "people", mgPerson)}))
	return f
}

func (f *migrationFixture) stagedDir(id string) string {
	return filepath.Join(f.runDir, "staged", id, "item")
}

func (f *migrationFixture) itemDir(id string) string {
	pl, err := PlaceOf(context.Background(), f.st.Pool(), id)
	if err != nil {
		panic(err)
	}
	return f.p.ItemDir(pl)
}

// updatedAt is the row's modifiedat as the export writes it, its session in
// UTC: to the second (an item's is a timestamp without its zone, written as
// it is; a person's is one with it).
func (f *migrationFixture) updatedAt(t *testing.T, table, id string) string {
	t.Helper()
	at := "modifiedat"
	if table == "people" {
		at = "modifiedat AT TIME ZONE 'UTC'"
	}
	var out string
	if err := f.st.Pool().QueryRow(context.Background(), `SELECT to_char(`+at+`, 'YYYY-MM-DD"T"HH24:MI:SS"Z"')
		FROM com_nalet_katalog_`+table+` WHERE id = $1`, id).Scan(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

// record stages an item's records: item.json and its checksums, a metadata
// projection.
func (f *migrationFixture) record(t *testing.T, id, staged string) {
	t.Helper()
	if err := WriteCovered(staged, map[string][]byte{ItemFile: librarytest.JSON(t, map[string]any{
		"schema": "zaentrum.library.item/2", "itemId": id})}); err != nil {
		t.Fatal(err)
	}
	librarytest.Write(t, filepath.Join(staged, "metadata.json"), librarytest.JSON(t, map[string]any{"itemId": id}))
}

// move moves a file or folder of the fixture.
func move(t *testing.T, from, to string) {
	t.Helper()
	if err := renameInto(from, to); err != nil {
		t.Fatal(err)
	}
}

// packaged stages the item id packaged in the store before the library: its
// original at media (none when ""), its package in the package store, the
// version its package becomes in the staging, its source record; the film
// gets a sidecar its package made a subtitle of, and an extra.
func (f *migrationFixture) packaged(t *testing.T, id, media string, withExtra bool) {
	t.Helper()
	ctx := context.Background()
	staged, final := f.stagedDir(id), f.itemDir(id)
	var typ string
	if err := f.st.Pool().QueryRow(ctx, `SELECT type FROM com_nalet_katalog_items WHERE id = $1`, id).Scan(&typ); err != nil {
		t.Fatal(err)
	}
	cat := map[string]string{"movie": "movies", "episode": "shows"}[typ]
	old := filepath.Join(f.cfg.PackagesRoot, cat, id[:2], id)
	sid, vid, pid := IDOf(id+":source"), IDOf(id+":version"), IDOf(id+":package")
	f.record(t, id, staged)
	build := t.TempDir()
	librarytest.WriteVersion(t, build, librarytest.Version{VersionID: vid, PackageID: pid, SourceIDs: []string{sid},
		CreatedAt: "2026-09-01T10:00:00Z", Package: mgPackage})
	vdir := filepath.Join(staged, "versions", vid)
	for _, rec := range []string{VersionFile, SumsFile, PackageFile, CompleteFile} {
		move(t, filepath.Join(build, rec), filepath.Join(vdir, rec))
	}
	var packages []Move
	for _, sub := range []string{"hls", "subs", "trickplay"} {
		move(t, filepath.Join(build, sub), filepath.Join(old, sub))
		packages = append(packages, Move{MovePackage, filepath.Join(old, sub), filepath.Join(vdir, sub)})
	}
	librarytest.Write(t, filepath.Join(old, "manifest.json"), []byte(`{"packagedAt": "2026-09-01T10:00:00Z"}`))
	librarytest.Write(t, filepath.Join(old, CompleteFile), []byte("done\n"))
	legacy := []Move{{MoveLegacy, old, filepath.Join(f.runDir, "legacy", cat, id[:2], id)}}
	fdir := filepath.Join(final, "versions", vid)
	db := UnitDB{RecordedAt: "2026-10-07T10:00:00Z", Versions: []UnitVersion{{VersionID: vid, PackageID: pid, Dir: fdir,
		CompletedAt: "2026-09-01T10:00:00Z", SourceIDs: []string{sid}, VerifiedAt: ptr("2026-10-07T09:00:00Z"),
		VerifiedLevel: ptr(VerifyFull)}}}
	updated := f.updatedAt(t, "items", id)
	db.ProjectedDatabaseUpdatedAt = &updated
	storetest.Exec(t, f.st, `INSERT INTO com_nalet_katalog_playbackassets (id, item_id, path, isprimary, kind, codec, sizebytes) VALUES
		('pk-' || left($1::varchar, 4), $1::varchar, $2, false, 'packaged', 'hev1.old', 7),
		('pk-' || left($1::varchar, 4) || '-old', $1::varchar, $3, false, 'packaged', 'avc1.old', 7)`,
		id, filepath.Join(old, "manifest.json"), filepath.Join(f.cfg.PackagesRoot, "older", id, "manifest.json"))
	storetest.Exec(t, f.st, `INSERT INTO com_nalet_katalog_subtitleassets (id, item_id, path, format, lang, isdefault)
		VALUES ('sp-' || left($1::varchar, 4), $1::varchar, $2, 'webvtt', 'de', true)`, id, filepath.Join(old, "subs/0.vtt"))
	db.Assets = append(db.Assets, UnitAsset{ID: "pk-" + id[:4], Path: filepath.Join(fdir, PackageFile), VersionID: &vid})
	db.Subtitles = append(db.Subtitles, UnitSubtitle{ID: "sp-" + id[:4], Path: filepath.Join(fdir, "subs/0.vtt")})
	record := map[string]any{"sourceId": sid, "file": map[string]any{"name": "x.mkv", "sizeBytes": 1},
		"essence": map[string]any{"surround": false, "maxAudioChannels": 2}}
	copies := map[string]string{}
	var originals []Move
	src := UnitSource{SourceID: sid, Filename: "x.mkv", RecordDir: ptr(filepath.Join(final, "sources", sid))}
	if media == "" {
		// gone before the library: its row is a retired original's
		storetest.Exec(t, f.st, `INSERT INTO com_nalet_katalog_playbackassets (id, item_id, path, isprimary, kind)
			VALUES ('pa-' || left($1::varchar, 4), $1::varchar, $2, true, 'primary')`, id, filepath.Join(f.cfg.NFSRoot, "Gone.mkv"))
		db.Assets = append(db.Assets, UnitAsset{ID: "pa-" + id[:4], Path: filepath.Join(final, "sources", sid), SourceID: &sid})
	} else {
		path := filepath.Join(f.dir, media)
		arrival := filepath.Join(f.p.Arrivals, strings.TrimPrefix(media, "media/"))
		librarytest.Write(t, path, []byte("the original of "+id))
		size, qh1, err := QH1(path)
		if err != nil {
			t.Fatal(err)
		}
		into := arrival
		if f.into {
			into = filepath.Join(fdir, OriginalName(path, 0))
			src.Filename = OriginalName(path, 0)
		}
		src.ArrivalPath, src.QH1, src.SizeBytes = &into, &qh1, size
		src.LibraryPath = ptr(strings.TrimPrefix(media, "media/"))
		originals = append(originals, Move{MoveOriginal, path, into})
		storetest.Exec(t, f.st, `INSERT INTO com_nalet_katalog_playbackassets (id, item_id, path, isprimary, kind, sizebytes)
			VALUES ('pa-' || left($1::varchar, 4), $1::varchar, $2, true, 'primary', $3)`, id, path, size)
		db.Assets = append(db.Assets, UnitAsset{ID: "pa-" + id[:4], Path: into, SourceID: &sid})
		if withExtra {
			sub := strings.TrimSuffix(path, ".mkv") + ".en.srt"
			subArrival := strings.TrimSuffix(arrival, ".mkv") + ".en.srt"
			librarytest.Write(t, sub, []byte("1\n00:00:01,000 --> 00:00:02,000\nHello\n"))
			storetest.Exec(t, f.st, `INSERT INTO com_nalet_katalog_subtitleassets (id, item_id, path, format, lang, isdefault)
				VALUES ('se-' || left($1::varchar, 4), $1::varchar, $2, 'srt', 'en', false)`, id, sub)
			originals = append(originals, Move{MoveSidecar, sub, subArrival})
			db.Subtitles = append(db.Subtitles, UnitSubtitle{ID: "se-" + id[:4], Path: subArrival})
			src.Sidecars = []mappedSidecar{{SubtitleAssetID: "se-" + id[:4], Rendition: "s1", Path: "subs/1.vtt"}}
			record["sidecars"] = []any{map[string]any{"file": "sources/" + sid + "/" + filepath.Base(sub),
				"originalName": filepath.Base(sub), "kind": "subtitle"}}
			copies[filepath.Base(sub)] = "1\n00:00:01,000 --> 00:00:02,000\nHello\n"
			packages, originals, legacy, db = f.extra(t, id, staged, final, packages, originals, legacy, db)
		}
	}
	librarytest.WriteSource(t, filepath.Join(staged, "sources", sid), record, copies)
	db.Sources = []UnitSource{src}
	listing, err := ListingSHA256(froms(packages))
	if err != nil {
		t.Fatal(err)
	}
	manifest, _, _ := SHA256File(filepath.Join(old, "manifest.json"))
	mtime, _ := MtimeOf(filepath.Join(old, CompleteFile))
	moves := append(append(append(packages, Move{MovePublish, staged, final}), originals...), legacy...)
	f.unit(t, id, typ, moves, &Guards{ManifestSha256: ptr("sha256:" + manifest), CompleteMtime: &mtime, ListingSha256: &listing}, db)
}

// extra stages the film's extra: its package in the store's extras, its
// original among the extras taken in before, its records in the staging.
func (f *migrationFixture) extra(t *testing.T, id, staged, final string, packages, originals, legacy []Move,
	db UnitDB) ([]Move, []Move, []Move, UnitDB) {
	t.Helper()
	old := filepath.Join(f.cfg.PackagesRoot, "extras", mgExtra[:2], mgExtra)
	xdir := filepath.Join(staged, "extras", mgExtra)
	pid := IDOf(mgExtra + ":package")
	build := t.TempDir()
	librarytest.WriteExtra(t, build, librarytest.Extra{ExtraID: mgExtra, PackageID: pid, CreatedAt: "2026-09-01T10:00:00Z"})
	for _, rec := range []string{ExtraFile, SumsFile, PackageFile, CompleteFile} {
		move(t, filepath.Join(build, rec), filepath.Join(xdir, rec))
	}
	move(t, filepath.Join(build, "hls"), filepath.Join(old, "hls"))
	librarytest.Write(t, filepath.Join(old, "manifest.json"), []byte("{}"))
	original := filepath.Join(f.dir, "extras", "film", "trailer.mov")
	arrival := filepath.Join(f.p.Extras, "film", "trailer.mov")
	librarytest.Write(t, original, []byte("a trailer"))
	size, qh1, err := QH1(original)
	if err != nil {
		t.Fatal(err)
	}
	storetest.Exec(t, f.st, `INSERT INTO com_nalet_katalog_itemextras (id, item_id, kind, title, registeredby, state, sourcepath,
			sourcesize, sourceqh1, packagepath, packagedat) VALUES ($1, $2, 'trailer', 'Trailer', 'api', 'ready', $3, $5, $6, $4, now())`,
		mgExtra, id, original, old, size, qh1)
	db.Extras = []UnitExtra{{ID: mgExtra, Dir: ptr(filepath.Join(final, "extras", mgExtra)), PackageID: &pid, SourcePath: &arrival}}
	return append(packages, Move{MovePackage, filepath.Join(old, "hls"), filepath.Join(xdir, "hls")}),
		append(originals, Move{MoveOriginal, original, arrival}),
		append(legacy, Move{MoveLegacy, old, filepath.Join(f.runDir, "legacy", "extras", mgExtra[:2], mgExtra)}), db
}

func froms(moves []Move) []string {
	var out []string
	for _, m := range moves {
		out = append(out, m.From)
	}
	return out
}

func ptr(s string) *string { return &s }

// unit writes the item's plan.
func (f *migrationFixture) unit(t *testing.T, id, typ string, moves []Move, guards *Guards, db UnitDB) {
	t.Helper()
	if guards == nil {
		guards = &Guards{}
	}
	if db.RecordedAt == "" {
		db.RecordedAt = "2026-10-07T10:00:00Z"
		updated := f.updatedAt(t, "items", id)
		db.ProjectedDatabaseUpdatedAt = &updated
	}
	u := &Unit{Schema: UnitSchema, Run: mgRun, ItemID: id, Type: typ, ItemDir: f.itemDir(id), StagedDir: f.stagedDir(id),
		Moves: moves, Guards: *guards, DB: db, Problems: []Problem{}}
	f.units[id] = u
	b, err := json.Marshal(u)
	if err != nil {
		t.Fatal(err)
	}
	librarytest.Write(t, filepath.Join(f.runDir, "units", id+".json"), b)
}

// migration is the fixture's run.
func (f *migrationFixture) migration(t *testing.T) *Migration {
	t.Helper()
	m, err := NewMigration(f.st.Pool(), f.cfg, mgRun)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// rows is a table's rows of the item, as JSON, by id.
func (f *migrationFixture) rows(t *testing.T, table, where string, args ...any) string {
	t.Helper()
	var out *string
	if err := f.st.Pool().QueryRow(context.Background(), `SELECT jsonb_agg(to_jsonb(t) ORDER BY t.id)::text
		FROM com_nalet_katalog_`+table+` t WHERE `+where, args...).Scan(&out); err != nil {
		t.Fatal(err)
	}
	if out == nil {
		return "[]"
	}
	return *out
}

// state is what the adoption changes of the fixture's database, as JSON.
func (f *migrationFixture) state(t *testing.T) string {
	t.Helper()
	ids := []any{[]string{mgFilm, mgGone, mgShow, mgEp}}
	return strings.Join([]string{
		f.rows(t, "playbackassets", "t.item_id = ANY($1)", ids...),
		f.rows(t, "subtitleassets", "t.item_id = ANY($1)", ids...),
		f.rows(t, "itemextras", "t.item_id = ANY($1)", ids...),
		f.rows(t, "itemsources", "t.item_id = ANY($1)", ids...),
		f.rows(t, "itemversions", "t.item_id = ANY($1)", ids...),
		f.rows(t, "items", "t.id = ANY($1)", ids...),
		f.rows(t, "people", "true"),
	}, "\n")
}

// tree lists every file of the fixture's share but the run's journal, with
// its size.
func (f *migrationFixture) tree(t *testing.T) string {
	t.Helper()
	var out []string
	_ = filepath.WalkDir(f.dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || filepath.Base(path) == "journal.jsonl" {
			return nil
		}
		fi, _ := d.Info()
		rel, _ := filepath.Rel(f.dir, path)
		out = append(out, rel+" "+strconv.FormatInt(fi.Size(), 10))
		return nil
	})
	return strings.Join(out, "\n")
}

// An adopt puts every unit of a run in place, a series before its episode:
// the old packages' folders into the staged versions, the item folders to
// their places, the originals and the sidecars to the arrivals, the rest of
// the old package folders to the run's legacy/, then the staged people; and
// changes the database as packaging-complete leaves it: the sources and the
// versions (verified in full by the stage), the original's row at the
// arrivals, the packaged row from package.json (the item's other packaged
// rows gone), the subtitle rows where their files went, their defaults kept,
// the extra recorded, the items recorded and projected as the export had
// them; an item the adoption itself marked changed (the film, whose extra
// it recorded) is projected again from the catalog, as the projector
// projects it. An original gone before the library has its event and a
// retired original's row. Every action is journaled, and adopting again
// skips what is adopted.
func TestTheAdoptOfAMigrationRun(t *testing.T) {
	f := newMigrationFixture(t)
	ctx := context.Background()
	rep, err := f.migration(t).Adopt(ctx, nil)
	if err != nil || rep.Adopted != 4 || rep.People != 1 || rep.Failed+rep.Stale+rep.Refused+rep.Busy != 0 {
		t.Fatalf("Adopt: %+v, %v", rep, err)
	}
	if rep.Units[0].ItemID != mgShow || rep.Units[3].ItemID != mgEp {
		t.Errorf("the order: %+v", rep.Units)
	}
	film := f.units[mgFilm]
	for _, path := range []string{filepath.Join(film.ItemDir, ItemFile), filepath.Join(f.itemDir(mgEp), ItemFile),
		filepath.Join(f.p.Arrivals, "Film (2020)", "Film.mkv"), filepath.Join(f.p.Arrivals, "Film (2020)", "Film.en.srt"),
		filepath.Join(f.p.Extras, "film", "trailer.mov"), filepath.Join(film.ItemDir, "extras", mgExtra, "hls", "master.m3u8"),
		filepath.Join(f.runDir, "legacy", "movies", mgFilm[:2], mgFilm, "manifest.json"), filepath.Join(f.p.PersonDir(mgPerson), "person.json")} {
		if !statOK(path) {
			t.Errorf("%s is not there", path)
		}
	}
	for _, gone := range []string{f.stagedDir(mgFilm), filepath.Join(f.cfg.PackagesRoot, "movies", mgFilm[:2], mgFilm),
		filepath.Join(f.cfg.NFSRoot, "Film (2020)")} {
		if _, err := os.Stat(gone); !os.IsNotExist(err) {
			t.Errorf("%s is there: %v", gone, err)
		}
	}
	for _, id := range []string{mgFilm, mgGone, mgEp} {
		v, _ := Current(ctx, f.st.Pool(), id)
		if v == nil || deref(v.VerifiedLevel) != VerifyFull {
			t.Fatalf("%s's version: %+v", id, v)
		}
		if _, err := VerifyVersion(*v.Dir, true); err != nil {
			t.Errorf("%s's version does not verify where it is: %v", id, err)
		}
	}
	vid := IDOf(mgFilm + ":version")
	fdir := VersionDir(film.ItemDir, vid)
	for sql, want := range map[string]string{
		`SELECT path || ' ' || kind || ' ' || isprimary || ' ' || sourceid FROM com_nalet_katalog_playbackassets WHERE id = 'pa-a0a0'`: filepath.Join(f.p.Arrivals, "Film (2020)", "Film.mkv") + " primary true " + IDOf(mgFilm+":source"),
		`SELECT path || ' ' || kind || ' ' || codec || ' ' || resolution || ' ' || bitratekbps || ' ' || audiocodec || ' ' ||
			audiochannels || ' ' || audiotrackcount || ' ' || subtitletrackcount || ' ' || durationms || ' ' || versionid
			FROM com_nalet_katalog_playbackassets WHERE id = 'pk-a0a0'`: filepath.Join(fdir, PackageFile) +
			" packaged hvc1.1.6.L120.90 1920x1080 4000 mp4a.40.2 2 1 2 600000 " + vid,
		`SELECT count(*)::text FROM com_nalet_katalog_playbackassets WHERE id = 'pk-a0a0-old'`:       "0",
		`SELECT path || ' ' || isdefault FROM com_nalet_katalog_subtitleassets WHERE id = 'sp-a0a0'`: filepath.Join(fdir, "subs/0.vtt") + " true",
		`SELECT path || ' ' || isdefault FROM com_nalet_katalog_subtitleassets WHERE id = 'se-a0a0'`: filepath.Join(f.p.Arrivals,
			"Film (2020)", "Film.en.srt") + " false",
		`SELECT state || ' ' || arrivalpath || ' ' || (recordedat IS NOT NULL) || ' ' || sidecars::text FROM com_nalet_katalog_itemsources
			WHERE item_id = '` + mgFilm + `'`: "present " + filepath.Join(f.p.Arrivals, "Film (2020)", "Film.mkv") +
			` true [{"path": "subs/1.vtt", "rendition": "s1", "subtitleAssetId": "se-a0a0"}]`,
		// a source with a version keeps no name it arrived under, nor where
		`SELECT filename || ' ' || (librarypath IS NULL) FROM com_nalet_katalog_itemsources WHERE item_id = '` + mgFilm + `'`: "original.mkv true",
		`SELECT packagepath || ' ' || (recordedat IS NOT NULL) || ' ' || sourcepath FROM com_nalet_katalog_itemextras`: filepath.Join(film.ItemDir,
			"extras", mgExtra) + " true " + filepath.Join(f.p.Extras, "film", "trailer.mov"),
		`SELECT (recordedat IS NOT NULL) || ' ' || (libraryprojectedat = modifiedat) FROM com_nalet_katalog_items WHERE id = '` + mgFilm + `'`: "true true",
		`SELECT (libraryprojectedat = modifiedat)::text FROM com_nalet_katalog_people`:                                                         "true",
		`SELECT state || ' ' || (retireeventid IS NOT NULL) || ' ' || deletedby FROM com_nalet_katalog_itemsources
			WHERE item_id = '` + mgGone + `'`: "deleted true " + MigratedBy,
		`SELECT kind || ' ' || isprimary FROM com_nalet_katalog_playbackassets WHERE id = 'pa-b0b0'`: "original false",
	} {
		var got string
		if err := f.st.Pool().QueryRow(ctx, sql).Scan(&got); err != nil || got != want {
			t.Errorf("%s:\n got  %q, %v\n want %q", sql, got, err, want)
		}
	}
	// The film's extra recorded marked it changed: its projection was
	// written again from the catalog, the others' are the staged ones.
	var modified string
	if err := f.st.Pool().QueryRow(ctx, `SELECT to_char(modifiedat, 'YYYY-MM-DD"T"HH24:MI:SS"Z"') FROM com_nalet_katalog_items
		WHERE id = $1`, mgFilm).Scan(&modified); err != nil || modified == "2026-10-01T09:00:00Z" {
		t.Fatalf("the film's modifiedat: %s, %v; want it marked by its extra", modified, err)
	}
	b, _ := os.ReadFile(filepath.Join(film.ItemDir, "metadata.json"))
	md, err := DecodeDoc(b)
	if err != nil {
		t.Fatal(err)
	}
	lib, _ := md.Get("library")
	primary, _ := lib.(Doc).Get("primaryVersionId")
	if by, _ := md.Get("projectedBy"); by != "katalog-manager" || primary != vid {
		t.Errorf("the film's projection: by %v, primary version %v:\n%s", by, primary, b)
	}
	if at, _ := md.Get("databaseUpdatedAt"); at != modified {
		t.Errorf("the film's projection reflects %v, and the film is of %s", at, modified)
	}
	if b, _ := os.ReadFile(filepath.Join(f.itemDir(mgGone), "metadata.json")); string(b) != "{\n  \"itemId\": \""+mgGone+"\"\n}\n" {
		t.Errorf("the gone film's projection is not the staged one:\n%s", b)
	}
	events, err := ReadEvents(f.itemDir(mgGone))
	if err != nil || len(events) != 1 {
		t.Fatalf("the gone film's events: %v, %v", events, err)
	}
	b, _ = Encode(events[0])
	for _, want := range []string{`"kind": "original-deleted"`, `"reason": "gone before the library was recorded"`, `"accepted": []`,
		`"by": "katalog-manager (migration)"`, `"sourceId": "` + IDOf(mgGone+":source") + `"`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("the event lacks %s:\n%s", want, b)
		}
	}
	journal, _ := os.ReadFile(filepath.Join(f.runDir, "journal.jsonl"))
	if n := strings.Count(string(journal), "\n"); n != 33 || !strings.Contains(string(journal), `"op":"projection"`) {
		t.Errorf("the journal holds %d lines:\n%s", n, journal)
	}

	again, err := f.migration(t).Adopt(ctx, nil)
	if err != nil || again.Skipped != 4 || again.Adopted+again.People != 0 {
		t.Errorf("Adopt again: %+v, %v", again, err)
	}
}

// An adopted original is retired as any other once the layout is v2 and
// the policy deletes originals, its version's full verification by the stage
// standing in for the job's; and a revert brings it back from the trash with
// everything else of the run: the renames undone, the published records back
// in staging (the staged projection of an item projected again too), the
// database as it was, the people back in staging. The run is adopted again
// after.
func TestARevertBringsTheRunBack(t *testing.T) {
	f := newMigrationFixture(t)
	ctx := context.Background()
	before, tree := f.state(t), f.tree(t)
	if rep, err := f.migration(t).Adopt(ctx, nil); err != nil || rep.Adopted != 4 {
		t.Fatalf("Adopt: %+v, %v", rep, err)
	}
	storetest.Exec(t, f.st, `INSERT INTO com_nalet_katalog_settings (id, key, valuetext) VALUES
		('s1', 'library.layout', 'v2'), ('s2', 'library.originals', 'delete-after-package'), ('s3', 'library.retire.delay', '0')`)
	r := NewRetirer(f.st.Pool(), f.cfg, processing.New(f.st.Pool()))
	if rep, err := r.Pass(ctx); err != nil || rep.Originals != 2 || rep.Extras != 1 {
		t.Fatalf("the retire pass: %+v, %v", rep, err)
	}
	if statOK(filepath.Join(f.p.Arrivals, "Film (2020)", "Film.mkv")) {
		t.Fatal("the film's original was not retired")
	}
	storetest.Exec(t, f.st, `DELETE FROM com_nalet_katalog_settings`)
	storetest.Exec(t, f.st, `DELETE FROM com_nalet_katalog_itemprocessingsteps`)

	rep, err := f.migration(t).Revert(ctx, nil)
	if err != nil || rep.Reverted != 4 || rep.People != 1 || rep.Refused+rep.Failed != 0 {
		t.Fatalf("Revert: %+v, %v", rep, err)
	}
	if rep.Units[0].ItemID != mgEp || rep.Units[3].ItemID != mgShow {
		t.Errorf("the order: %+v", rep.Units)
	}
	if got := f.state(t); got != before {
		t.Errorf("the database after the revert:\n%s\nbefore the adopt:\n%s", got, before)
	}
	if got := f.tree(t); got != tree {
		t.Errorf("the tree after the revert:\n%s\nbefore the adopt:\n%s", got, tree)
	}

	if rep, err := f.migration(t).Adopt(ctx, nil); err != nil || rep.Adopted != 4 {
		t.Errorf("Adopt after the revert: %+v, %v", rep, err)
	}
}

// A unit whose old package changed since it was staged is stale and left;
// one the pipeline works on is busy and left; one whose rename fails puts
// back what it renamed, and the database stays as it was. An episode whose
// series is not adopted is refused.
func TestAUnitThatCannotBeAdoptedIsLeft(t *testing.T) {
	f := newMigrationFixture(t)
	ctx := context.Background()
	librarytest.Write(t, filepath.Join(f.cfg.PackagesRoot, "movies", mgFilm[:2], mgFilm, "hls", "v0", "seg0.m4s"), []byte("another segment"))
	storetest.Exec(t, f.st, `INSERT INTO com_nalet_katalog_itemprocessingsteps (id, item_id, step, status)
		VALUES ('t', $1, 'transcode', 'in_progress')`, mgGone)
	if err := os.Remove(filepath.Join(f.cfg.NFSRoot, "Example Show", "Season 01", "Example Show S01E01.mkv")); err != nil {
		t.Fatal(err)
	}
	before, tree := f.state(t), f.tree(t)
	rep, err := f.migration(t).Adopt(ctx, []string{mgFilm, mgGone, mgEp})
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range rep.Units {
		want := map[string]string{mgFilm: StateStale, mgGone: StateBusy, mgEp: StateRefused}[u.ItemID]
		if u.State != want {
			t.Errorf("%s: %s %s, want %s", u.ItemID, u.State, u.Reason, want)
		}
	}
	if !strings.HasPrefix(rep.Units[0].Reason, "stale plan, re-stage: the package files are not the ones staged") {
		t.Errorf("the stale unit: %s", rep.Units[0].Reason)
	}
	if got := f.state(t); got != before {
		t.Errorf("the database changed:\n%s\nwant\n%s", got, before)
	}
	if got := f.tree(t); got != tree {
		t.Errorf("the tree changed:\n%s\nwant\n%s", got, tree)
	}

	// The series adopted, the episode whose original is gone fails, its
	// renames put back.
	if rep, err := f.migration(t).Adopt(ctx, []string{mgShow, mgEp}); err != nil || rep.Adopted != 1 || rep.Failed != 1 ||
		!strings.Contains(rep.Units[1].Reason, "its renames are put back") {
		t.Fatalf("Adopt the series and its episode: %+v, %v", rep, err)
	}
	if statOK(filepath.Join(f.itemDir(mgEp), ItemFile)) || !statOK(filepath.Join(f.stagedDir(mgEp), ItemFile)) ||
		!statOK(filepath.Join(f.cfg.PackagesRoot, "shows", mgEp[:2], mgEp, "hls", "master.m3u8")) {
		t.Error("the failed episode's renames were not put back")
	}
	if v, _ := Current(ctx, f.st.Pool(), mgEp); v != nil {
		t.Errorf("the failed episode has a version: %+v", v)
	}
}

// An adoption a crash stopped is put right before the next: renamed and not
// in the database, its renames are put back (its staged projection too, and
// what a projection of it added) and it is adopted afresh.
func TestAnAdoptionACrashStoppedIsPutRight(t *testing.T) {
	f := newMigrationFixture(t)
	ctx := context.Background()
	m := f.migration(t)
	j, err := OpenJournal(filepath.Join(f.runDir, "journal.jsonl"), time.Now)
	if err != nil {
		t.Fatal(err)
	}
	for _, mv := range f.units[mgFilm].Moves[:5] { // the packages moved, the item published: then the crash
		move(t, mv.From, mv.To)
		if err := j.Add(JournalEntry{ItemID: mgFilm, Op: OpMove, Kind: mv.Kind, From: mv.From, To: mv.To, State: StateDone}); err != nil {
			t.Fatal(err)
		}
	}
	// its projection written again, then the crash
	md := filepath.Join(f.itemDir(mgFilm), "metadata.json")
	staged, _ := os.ReadFile(md)
	librarytest.Write(t, md, []byte("{\"projected\": true}\n"))
	librarytest.Write(t, filepath.Join(f.itemDir(mgFilm), "metadata", "new.jpg"), []byte("an image"))
	un, _ := json.Marshal(unprojection{Metadata: staged, Images: []string{"new.jpg"}})
	if err := j.Add(JournalEntry{ItemID: mgFilm, Op: OpProjection, Before: un, State: StateDone}); err != nil {
		t.Fatal(err)
	}
	_ = j.Close()
	rep, err := m.Adopt(ctx, []string{mgFilm})
	if err != nil || rep.Adopted != 1 {
		t.Fatalf("Adopt after the crash: %+v, %v", rep, err)
	}
	journal, _ := os.ReadFile(filepath.Join(f.runDir, "journal.jsonl"))
	if !strings.Contains(string(journal), `"state":"undone"`) ||
		!strings.Contains(string(journal), "a crash stopped its adoption: its renames are put back") {
		t.Errorf("the journal:\n%s", journal)
	}
	if v, _ := Current(ctx, f.st.Pool(), mgFilm); v == nil {
		t.Error("the film is not adopted")
	}
	if statOK(filepath.Join(f.itemDir(mgFilm), "metadata", "new.jpg")) {
		t.Error("the image the crashed projection wrote is there")
	}
}

// A revert is refused for an item that changed since it was adopted (a
// version made since), and for one whose retired original's trash is
// purged; the item stays adopted.
func TestARevertIsRefusedForAChangedItem(t *testing.T) {
	f := newMigrationFixture(t)
	ctx := context.Background()
	if rep, err := f.migration(t).Adopt(ctx, []string{mgShow, mgFilm, mgGone}); err != nil || rep.Adopted != 3 {
		t.Fatalf("Adopt: %+v, %v", rep, err)
	}
	storetest.Exec(t, f.st, `INSERT INTO com_nalet_katalog_itemversions (id, item_id, state) VALUES ('later', $1, 'building')`, mgGone)
	storetest.Exec(t, f.st, `UPDATE com_nalet_katalog_itemsources SET state = 'deleted', arrivalpath = NULL, trashpath = $2
		WHERE item_id = $1`, mgFilm, filepath.Join(f.p.Work, WorkTrash, "20261007", "nothing"))
	if err := os.Remove(filepath.Join(f.p.Arrivals, "Film (2020)", "Film.mkv")); err != nil {
		t.Fatal(err)
	}
	rep, err := f.migration(t).Revert(ctx, []string{mgFilm, mgGone})
	if err != nil || rep.Refused != 2 {
		t.Fatalf("Revert: %+v, %v", rep, err)
	}
	for _, u := range rep.Units {
		want := map[string]string{mgFilm: "was deleted for good", mgGone: "the item changed since it was adopted"}[u.ItemID]
		if !strings.Contains(u.Reason, want) {
			t.Errorf("%s: %s, want it to say %q", u.ItemID, u.Reason, want)
		}
	}
	if !statOK(filepath.Join(f.itemDir(mgFilm), ItemFile)) {
		t.Error("the refused film is not adopted any more")
	}
}
