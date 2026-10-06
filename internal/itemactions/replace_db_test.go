package itemactions

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zaentrum/katalog-manager/internal/auth"
	"github.com/zaentrum/katalog-manager/internal/config"
	"github.com/zaentrum/katalog-manager/internal/graph"
	"github.com/zaentrum/katalog-manager/internal/processing"
	"github.com/zaentrum/katalog-manager/internal/scanner"
	"github.com/zaentrum/katalog-manager/internal/store"
	"github.com/zaentrum/katalog-manager/internal/store/storetest"
)

// fakeReencoder records the titles it is asked to encode again, and answers
// res for each, or err.
type fakeReencoder struct {
	mu  sync.Mutex
	ids []string
	res graph.ReencodeResult
	err error
}

func (f *fakeReencoder) ReencodeItem(_ context.Context, id string) (graph.ReencodeResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ids = append(f.ids, id)
	r := f.res
	r.ItemID = id
	return r, f.err
}

func (f *fakeReencoder) take() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	ids := f.ids
	f.ids = nil
	return ids
}

const bunny = "b0b0b0b0-0000-4000-8000-00000000000b"

// replacing is a catalog whose titles are given other files: under a media
// root beside a package store, Big Buck Bunny's small copy with the subtitle
// file named after it, and the original it is upgraded to; and of the film
// what hangs off it: its facets, its probe, its package and the package's
// subtitle, the tracks a package reported of its file and the languages an
// admin set, a trailer, its steps and its diagnostics.
type replacing struct {
	st       *store.Store
	cfg      config.Config
	dir      string
	re       *fakeReencoder
	svc      *Service
	old, cur string // the film's file, and the one it is given
}

func newReplacing(t *testing.T) *replacing {
	t.Helper()
	st := storetest.Open(t)
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{NFSRoot: dir + "/media", PackagesRoot: dir + "/packages", LegacyLibraryRoot: dir + "/library",
		ExtrasRoot: dir + "/extras"}
	f := &replacing{st: st, cfg: cfg, dir: dir,
		re: &fakeReencoder{res: graph.ReencodeResult{Titles: 1, Reencoded: 1, Message: "encoding it again"}}}
	f.svc = New(st, cfg, processing.New(st.Pool()), nil).WithReencoder(f.re)
	f.old = f.write(t, "media/BigBuckBunny_320x180.mp4", 1000)
	f.cur = f.write(t, "media/Big Buck Bunny (2008).mov", 5000)
	pkg := filepath.Join(cfg.PackagesRoot, "movies", bunny[:2], bunny)
	storetest.AddItem(t, st, bunny, "movie", "Big Buck Bunny", "")
	storetest.AddFacets(t, st, bunny)
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_playbackassets (id, item_id, path, codec, resolution, bitratekbps,
		sizebytes, hash, isprimary, kind, audiocodec, audiolanguage, audiochannels, audiobitratekbps, audiotrackcount,
		subtitletrackcount, durationms) VALUES
		('a-bunny', $1, $2, 'h264', '320x180', 400, 1000, 'sha256:old', true, 'primary', 'aac', 'eng', 2, 128, 1, 0, 596000),
		('a-bunny-pkg', $1, $3, 'hvc1.1.6.L93.B0', '320x180', 300, 900, NULL, false, 'packaged', 'aac', 'eng', 2, 128, 1, 1, 596000)`,
		bunny, f.old, pkg+"/manifest.json")
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_subtitleassets (id, item_id, path, format, lang, label) VALUES
		('s-bunny-file', $1, $2, 'srt', 'en', 'English'), ('s-bunny-pkg', $1, $3, 'webvtt', 'eng', 'English')`,
		bunny, f.write(t, "media/BigBuckBunny_320x180.en.srt", 10), pkg+"/subs/en.vtt")
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_itemtracks (item_id, kind, ordinal, language, format)
		VALUES ($1, 'subtitle', 0, 'eng', 'webvtt')`, bunny)
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_itemtracklanguages (item_id, kind, ordinal, language)
		VALUES ($1, 'subtitle', 0, 'ger')`, bunny)
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_itemextras (id, item_id, kind, title, registeredby, sourcepath)
		VALUES ('x-bunny', $1, 'trailer', 'Trailer', 'api', $2)`, bunny, f.write(t, "extras/big-buck-bunny/trailer.mov", 100))
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_itemdiagnostics SET sourcepath = $2, sourcesize = 1000,
		ffprobedata = '{"streams":[{"codec_type":"video","codec_name":"h264","width":320,"height":180}]}' WHERE item_id = $1`, bunny, f.old)
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_itemprocessingsteps (id, item_id, step, status)
		VALUES ('st-bunny-t', $1, 'transcode', 'done'), ('st-bunny-p', $1, 'package', 'done')`, bunny)
	return f
}

// write makes a file of n bytes under the fixture's folder, and answers its
// path.
func (f *replacing) write(t *testing.T, rel string, n int) string {
	t.Helper()
	p := filepath.Join(f.dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, make([]byte, n), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// film adds a movie whose file is at rel, its source asset asset.
func (f *replacing) film(t *testing.T, id, asset, rel string) string {
	t.Helper()
	p := f.write(t, rel, 100)
	storetest.AddItem(t, f.st, id, "movie", filepath.Base(rel), "")
	storetest.Exec(t, f.st, `INSERT INTO com_nalet_katalog_playbackassets (id, item_id, path, sizebytes, isprimary)
		VALUES ($1, $2, $3, 100, true)`, asset, id, p)
	return p
}

var asOperator = auth.WithPrincipal(context.Background(), &auth.Principal{Subject: operator})

// keptOf is everything of the item a replace keeps: the item, but when and by
// whom it was modified, and every row that hangs off it but its source asset
// (sourceID), the tracks a package reported of its file and its diagnostics.
func keptOf(t *testing.T, st *store.Store, item, sourceID string) string {
	t.Helper()
	var b strings.Builder
	read := func(sql string, args ...any) {
		var rows string
		if err := st.Pool().QueryRow(context.Background(), `SELECT coalesce(string_agg(x::text, '|' ORDER BY x::text), '')
			FROM (`+sql+`) x`, args...).Scan(&rows); err != nil {
			t.Fatal(err)
		}
		b.WriteString(rows + "\n")
	}
	read(`SELECT id, type, title, sorttitle, year, description, rating, durationms, parent_id, seasonnumber, episodenumber,
		tagline, metadatalocked, createdat, createdby FROM com_nalet_katalog_items WHERE id = $1`, item)
	for _, table := range []string{"itemgenres", "itempeople", "itemtags", "itemexternalids", "itemartwork", "itemartworkdata",
		"subtitleassets", "mediasegments", "itemchapters", "itemtrailerlinks", "itemprocessingsteps", "itemtracklanguages", "itemextras"} {
		read(`SELECT * FROM com_nalet_katalog_`+table+` WHERE item_id = $1`, item)
	}
	read(`SELECT * FROM com_nalet_katalog_playbackassets WHERE item_id = $1 AND id <> $2`, item, sourceID)
	return b.String()
}

// assetOf is a source asset as a line: its file, its size, whether it is the
// primary and its kind, and what of a file's description it holds.
func assetOf(t *testing.T, st *store.Store, id string) string {
	t.Helper()
	var out string
	if err := st.Pool().QueryRow(context.Background(), `SELECT item_id || ' ' || path || ' ' || COALESCE(sizebytes::text, '-') ||
		' primary=' || isprimary || ' kind=' || kind || ' holds=' || concat_ws(',',
			CASE WHEN hash IS NOT NULL THEN 'hash' END, CASE WHEN codec IS NOT NULL THEN 'codec' END,
			CASE WHEN resolution IS NOT NULL THEN 'resolution' END, CASE WHEN bitratekbps IS NOT NULL THEN 'bitrate' END,
			CASE WHEN durationms IS NOT NULL THEN 'duration' END, CASE WHEN audiocodec IS NOT NULL THEN 'audiocodec' END,
			CASE WHEN audiolanguage IS NOT NULL THEN 'audiolanguage' END, CASE WHEN audiochannels IS NOT NULL THEN 'audiochannels' END,
			CASE WHEN audiobitratekbps IS NOT NULL THEN 'audiobitrate' END, CASE WHEN audiotrackcount IS NOT NULL THEN 'audiotracks' END,
			CASE WHEN subtitletrackcount IS NOT NULL THEN 'subtitletracks' END)
		FROM com_nalet_katalog_playbackassets WHERE id = $1`, id).Scan(&out); err != nil {
		t.Fatalf("the asset %s: %v", id, err)
	}
	return out
}

func gone(p string) bool {
	_, err := os.Lstat(p)
	return errors.Is(err, os.ErrNotExist)
}

func sourceRefusal(t *testing.T, err error) *graph.SourceRefused {
	t.Helper()
	var r *graph.SourceRefused
	if !errors.As(err, &r) {
		t.Fatalf("%v, want a refusal", err)
	}
	return r
}

// A film given its original by the path of its small copy keeps itself: the
// same asset row has the new file, with its size and nothing of the old
// file's description; the item, its credits, genres, artwork, extras, its
// package and its subtitles, the languages an admin set, its steps: all as
// they were; what described the old file (the tracks a package reported, the
// diagnostics) is gone. The old file is deleted when asked, the subtitle file
// named after it stays, said, and the film is encoded again. What is kept by
// the item's id elsewhere (everyone's progress and ratings) is kept with it.
func TestAFilmGivenItsOriginalKeepsItself(t *testing.T) {
	f := newReplacing(t)
	kept := keptOf(t, f.st, bunny, "a-bunny")
	sidecar := filepath.Join(f.cfg.NFSRoot, "BigBuckBunny_320x180.en.srt")

	res, err := f.svc.ReplaceSource(asOperator, graph.ReplaceSourceRequest{ItemPath: f.old, Path: " " + f.cur + " ",
		DeleteOldFile: true, Reencode: true})
	if err != nil || res.ItemID != bunny || res.OldPath != f.old || res.Path != f.cur || !res.Replaced || !res.OldFileDeleted ||
		res.OldSidecars != 1 || res.Reencode == nil || res.Reencode.Reencoded != 1 || res.Reencode.ItemID != bunny {
		t.Fatalf("ReplaceSource: %+v, %v", res, err)
	}
	if want := "the title's file is " + f.cur + " now, in place of " + f.old + "; the old file is deleted; " +
		"the subtitle files named after the old file (1) stay the title's as they are; the next scan pairs those named after the new one; " +
		"encoding it again"; res.Message != want {
		t.Errorf("the message:\n %s\nwant\n %s", res.Message, want)
	}
	if got := f.re.take(); len(got) != 1 || got[0] != bunny {
		t.Errorf("encoded again: %v, want the film", got)
	}
	if got, want := assetOf(t, f.st, "a-bunny"), bunny+" "+f.cur+" 5000 primary=true kind=primary holds="; got != want {
		t.Errorf("the film's source asset:\n %s\nwant\n %s", got, want)
	}
	if after := keptOf(t, f.st, bunny, "a-bunny"); after != kept {
		t.Errorf("what the film keeps changed:\nbefore %s\nafter  %s", kept, after)
	}
	for _, table := range []string{"com_nalet_katalog_itemtracks", "com_nalet_katalog_itemdiagnostics"} {
		if n := storetest.Count(t, f.st, `SELECT count(*) FROM `+table+` WHERE item_id = $1`, bunny); n != 0 {
			t.Errorf("%s keeps %d rows of the old file", table, n)
		}
	}
	if n := storetest.Count(t, f.st, `SELECT count(*) FROM com_nalet_katalog_items WHERE id = $1 AND modifiedby = $2
		AND modifiedat > localtimestamp - interval '1 minute'`, bunny, operator); n != 1 {
		t.Error("the film is not modified by the operator who gave it its file")
	}
	if !gone(f.old) || gone(f.cur) || gone(sidecar) || gone(f.cfg.NFSRoot) {
		t.Errorf("on disk: the old file gone %v, the new %v, the subtitle file %v, the media root %v; want only the old one gone",
			gone(f.old), gone(f.cur), gone(sidecar), gone(f.cfg.NFSRoot))
	}
	if n := storetest.Count(t, f.st, `SELECT count(*) FROM com_nalet_katalog_items`); n != 1 {
		t.Errorf("the catalog holds %d items, want the film alone", n)
	}

	// By its id, the old file kept and the title not encoded again: said so.
	sintel := f.film(t, "c1c1c1c1-0000-4000-8000-00000000000c", "a-sintel", "media/Sintel/Sintel.2010.720p.mkv")
	better := f.write(t, "media/Sintel/Sintel (2010).mkv", 3000)
	res, err = f.svc.ReplaceSource(asOperator, graph.ReplaceSourceRequest{ItemID: "c1c1c1c1-0000-4000-8000-00000000000c", Path: better})
	if err != nil || !res.Replaced || res.OldFileDeleted || res.OldSidecars != 0 || res.Reencode != nil || res.OldPath != sintel {
		t.Fatalf("ReplaceSource by id: %+v, %v", res, err)
	}
	if want := "the title's file is " + better + " now, in place of " + sintel + "; the old file stays, under the media root: " +
		"the next scan takes it in as a title of its own unless it is moved out of it or deleted; " +
		"it is not encoded again: a package it has is the old file's until reencodeItem encodes the new one"; res.Message != want {
		t.Errorf("the message:\n %s\nwant\n %s", res.Message, want)
	}
	if gone(sintel) || len(f.re.take()) != 0 {
		t.Error("the old file went, or the title was encoded again, unasked")
	}
	if got := assetOf(t, f.st, "a-sintel"); got != "c1c1c1c1-0000-4000-8000-00000000000c "+better+" 3000 primary=true kind=primary holds=" {
		t.Errorf("Sintel's source asset: %s", got)
	}
}

// An episode given a file of the same name in another container keeps the
// subtitle files named after it as its own: they pair with the new file as
// they did. The folders a deleted file leaves empty go, up to the media root.
func TestAnEpisodeGivenAFileOfItsNameKeepsItsSubtitleFiles(t *testing.T) {
	f := newReplacing(t)
	const show, pilot = "5a5a5a5a-0000-4000-8000-000000000001", "e0e0e0e0-0000-4000-8000-000000000002"
	storetest.AddItem(t, f.st, show, "series", "Pioneer One", "")
	storetest.AddItem(t, f.st, pilot, "episode", "Earthfall", show)
	old := f.write(t, "media/series/Pioneer One/Pioneer.One.S01E01.mp4", 100)
	cur := f.write(t, "media/series/Pioneer One/Pioneer.One.S01E01.mkv", 200)
	storetest.Exec(t, f.st, `INSERT INTO com_nalet_katalog_playbackassets (id, item_id, path, isprimary) VALUES ('a-pilot', $1, $2, true)`, pilot, old)
	for _, sub := range []string{"Pioneer.One.S01E01.en.srt", "Pioneer.One.S01E01.de.srt", "Pioneer.One.S01E02.en.srt"} {
		storetest.Exec(t, f.st, `INSERT INTO com_nalet_katalog_subtitleassets (id, item_id, path) VALUES (gen_random_uuid()::varchar, $1, $2)`,
			pilot, f.write(t, "media/series/Pioneer One/"+sub, 10))
	}
	res, err := f.svc.ReplaceSource(asOperator, graph.ReplaceSourceRequest{ItemPath: old, Path: cur, DeleteOldFile: true})
	if err != nil || !res.Replaced || !res.OldFileDeleted || res.OldSidecars != 2 ||
		!strings.Contains(res.Message, "; the subtitle files named after the old file (2) pair with the new one, as they did; ") {
		t.Fatalf("ReplaceSource of the episode: %+v, %v", res, err)
	}
	if n := storetest.Count(t, f.st, `SELECT count(*) FROM com_nalet_katalog_subtitleassets WHERE item_id = $1`, pilot); n != 3 {
		t.Errorf("the episode has %d subtitles, want its 3 kept", n)
	}

	// A file in a folder of its own, given one elsewhere: the folder it
	// leaves empty goes, the media root stays.
	lone := f.film(t, "d1d1d1d1-0000-4000-8000-00000000000d", "a-lone", "media/A Short/Old Copy/a-short.mp4")
	elsewhere := f.write(t, "media/A Short (2019).mkv", 300)
	if res, err := f.svc.ReplaceSource(asOperator, graph.ReplaceSourceRequest{ItemPath: lone, Path: elsewhere, DeleteOldFile: true}); err != nil ||
		!res.OldFileDeleted {
		t.Fatalf("ReplaceSource of the short: %+v, %v", res, err)
	}
	if !gone(filepath.Join(f.cfg.NFSRoot, "A Short")) || gone(f.cfg.NFSRoot) {
		t.Errorf("the emptied folders gone %v, the media root gone %v; want the folders gone and the root kept",
			!gone(filepath.Join(f.cfg.NFSRoot, "A Short")), gone(f.cfg.NFSRoot))
	}
}

// A file that is the title's already changes nothing, however it is spelled:
// nothing is deleted, nothing encoded again.
func TestATitlesOwnFileChangesNothing(t *testing.T) {
	f := newReplacing(t)
	before := keptOf(t, f.st, bunny, "") + assetOf(t, f.st, "a-bunny")
	for _, req := range []graph.ReplaceSourceRequest{
		{ItemID: bunny, Path: f.old, DeleteOldFile: true, Reencode: true},
		{ItemPath: f.old, Path: f.cfg.NFSRoot + "/./x/../" + filepath.Base(f.old), DeleteOldFile: true, Reencode: true},
	} {
		res, err := f.svc.ReplaceSource(asOperator, req)
		if err != nil || res.Replaced || res.OldFileDeleted || res.Reencode != nil || res.ItemID != bunny || res.OldPath != f.old ||
			res.Path != f.old || res.Message != f.old+" is the title's file already: nothing changed" {
			t.Errorf("%+v: %+v, %v", req, res, err)
		}
	}
	if after := keptOf(t, f.st, bunny, "") + assetOf(t, f.st, "a-bunny"); after != before {
		t.Errorf("the film changed:\nbefore %s\nafter  %s", before, after)
	}
	if n := storetest.Count(t, f.st, `SELECT count(*) FROM com_nalet_katalog_itemtracks WHERE item_id = $1`, bunny); n != 2 {
		t.Errorf("the film keeps %d of its 2 tracks", n)
	}
	if gone(f.old) || len(f.re.take()) != 0 {
		t.Error("its file went, or it was encoded again")
	}
}

// What cannot be replaced, or by what, is refused, and nothing changes: a
// title named no way or two ways, one there is not (NOT_FOUND), a series, an
// album, a film without a file or with two named by its id; a file that is
// no absolute path, outside the media root, the media root itself, under the
// package store, a link that leads out of the media root, none there, a
// folder, no video, hidden, an extra by the scanner's convention; a file of
// another title or one of its extras already (SOURCE_CONFLICT, naming it).
func TestWhatIsNotReplaced(t *testing.T) {
	f := newReplacing(t)
	const other, twice = "0f0f0f0f-0000-4000-8000-00000000000f", "2f2f2f2f-0000-4000-8000-00000000002f"
	const show, album, fileless = "3f3f3f3f-0000-4000-8000-00000000003f", "4f4f4f4f-0000-4000-8000-00000000004f",
		"6f6f6f6f-0000-4000-8000-00000000006f"
	theirs := f.film(t, other, "a-other", "media/Elephants Dream (2006).mov")
	storetest.Exec(t, f.st, `INSERT INTO com_nalet_katalog_itemextras (id, item_id, kind, title, registeredby, sourcepath)
		VALUES ('x-other', $1, 'featurette', 'The Score', 'api', $2)`, other, f.write(t, "media/Elephants Dream Score.mkv", 10))
	first := f.film(t, twice, "a-twice-1", "media/Twice/one.mkv")
	f.write(t, "media/Twice/two.mkv", 10)
	storetest.Exec(t, f.st, `INSERT INTO com_nalet_katalog_playbackassets (id, item_id, path, isprimary) VALUES ('a-twice-2', $1, $2, true)`,
		twice, filepath.Join(f.cfg.NFSRoot, "Twice", "two.mkv"))
	storetest.AddItem(t, f.st, show, "series", "Pioneer One", "")
	storetest.AddItem(t, f.st, album, "album", "An Album", "")
	storetest.AddItem(t, f.st, fileless, "movie", "Nothing On Disk", "")
	outside := f.write(t, "elsewhere/Big Buck Bunny (2008).mkv", 10)
	if err := os.Symlink(outside, filepath.Join(f.cfg.NFSRoot, "link.mkv")); err != nil {
		t.Fatal(err)
	}
	staged := f.write(t, "packages/_inbox/bunny.mkv", 10)
	if err := os.MkdirAll(filepath.Join(f.cfg.NFSRoot, "folder.mkv"), 0o755); err != nil {
		t.Fatal(err)
	}
	media := func(rel string) string { return filepath.Join(f.cfg.NFSRoot, rel) }
	f.write(t, "media/notes.txt", 10)
	f.write(t, "media/.Big Buck Bunny.mkv", 10)
	f.write(t, "media/Big Buck Bunny (2008)-trailer.mkv", 10)
	f.write(t, "media/clip.ts", 10)
	nested := New(f.st, config.Config{NFSRoot: f.dir, PackagesRoot: f.dir + "/packages"}, processing.New(f.st.Pool()), nil)
	before := keptOf(t, f.st, bunny, "") + assetOf(t, f.st, "a-bunny") + assetOf(t, f.st, "a-other")
	for _, c := range []struct {
		svc  *Service
		req  graph.ReplaceSourceRequest
		code string
		says string
	}{
		{nil, graph.ReplaceSourceRequest{ItemID: bunny}, "SOURCE_REFUSED", "path is required"},
		{nil, graph.ReplaceSourceRequest{ItemID: bunny, Path: "media/Big Buck Bunny (2008).mov"}, "SOURCE_REFUSED", "is no absolute path"},
		{nil, graph.ReplaceSourceRequest{Path: f.cur}, "SOURCE_REFUSED", "name the title one way: itemId, or itemPath"},
		{nil, graph.ReplaceSourceRequest{ItemID: bunny, ItemPath: f.old, Path: f.cur}, "SOURCE_REFUSED", "name the title one way"},
		{nil, graph.ReplaceSourceRequest{ItemID: "nope", Path: f.cur}, "NOT_FOUND", "unknown item: nope"},
		{nil, graph.ReplaceSourceRequest{ItemPath: media("Gone.mp4"), Path: f.cur}, "NOT_FOUND", "no item has the file " + media("Gone.mp4")},
		{nil, graph.ReplaceSourceRequest{ItemID: show, Path: f.cur}, "SOURCE_REFUSED",
			"item " + show + " is a series, and a series has no file: its episodes have, each replaced on its own"},
		{nil, graph.ReplaceSourceRequest{ItemID: album, Path: f.cur}, "SOURCE_REFUSED", "is a album: only a movie's or an episode's file is replaced"},
		{nil, graph.ReplaceSourceRequest{ItemID: fileless, Path: f.cur}, "SOURCE_REFUSED", "item " + fileless + " has no file to replace"},
		{nil, graph.ReplaceSourceRequest{ItemID: twice, Path: f.cur}, "SOURCE_REFUSED", "has 2 files (" + first + ", " +
			media("Twice/two.mkv") + "): name the one to replace by itemPath"},
		{nil, graph.ReplaceSourceRequest{ItemID: bunny, Path: outside}, "SOURCE_REFUSED", outside + " is not under the media root"},
		{nil, graph.ReplaceSourceRequest{ItemID: bunny, Path: f.cfg.NFSRoot + "/../elsewhere/Big Buck Bunny (2008).mkv"}, "SOURCE_REFUSED", "is not under the media root"},
		{nil, graph.ReplaceSourceRequest{ItemID: bunny, Path: f.cfg.NFSRoot}, "SOURCE_REFUSED", "is not under the media root"},
		{nil, graph.ReplaceSourceRequest{ItemID: bunny, Path: staged}, "SOURCE_REFUSED", "is not under the media root (or is under the package store)"},
		{nested, graph.ReplaceSourceRequest{ItemID: bunny, Path: staged}, "SOURCE_REFUSED", staged + " is not under the media root (or is under the package store)"},
		{nil, graph.ReplaceSourceRequest{ItemID: bunny, Path: media("link.mkv")}, "SOURCE_REFUSED", "link.mkv leads out of the media root"},
		{nil, graph.ReplaceSourceRequest{ItemID: bunny, Path: media("Missing (2008).mkv")}, "SOURCE_REFUSED", "there is no file at " + media("Missing (2008).mkv")},
		{nil, graph.ReplaceSourceRequest{ItemID: bunny, Path: media("folder.mkv")}, "SOURCE_REFUSED", "folder.mkv is no file"},
		{nil, graph.ReplaceSourceRequest{ItemID: bunny, Path: media("notes.txt")}, "SOURCE_REFUSED",
			"notes.txt is no title's file to a scan: it is no video file a scan takes (.avi, .m4v, .mkv, .mov, .mp4, .webm)"},
		{nil, graph.ReplaceSourceRequest{ItemID: bunny, Path: media("clip.ts")}, "SOURCE_REFUSED", "it is no video file a scan takes"},
		{nil, graph.ReplaceSourceRequest{ItemID: bunny, Path: media(".Big Buck Bunny.mkv")}, "SOURCE_REFUSED", "its name begins with a dot"},
		{nil, graph.ReplaceSourceRequest{ItemID: bunny, Path: media("Big Buck Bunny (2008)-trailer.mkv")}, "SOURCE_REFUSED",
			"is no title's file to a scan: it is an extra by the extras convention (trailer)"},
		{nil, graph.ReplaceSourceRequest{ItemID: bunny, Path: theirs}, "SOURCE_CONFLICT", theirs + " is a file of item " + other + " already"},
		{nil, graph.ReplaceSourceRequest{ItemPath: f.old, Path: media("Elephants Dream Score.mkv")}, "SOURCE_CONFLICT",
			"Elephants Dream Score.mkv is extra x-other of item " + other + " already"},
	} {
		svc := c.svc
		if svc == nil {
			svc = f.svc
		}
		c.req.DeleteOldFile, c.req.Reencode = true, true
		_, err := svc.ReplaceSource(asOperator, c.req)
		if r := sourceRefusal(t, err); r.Code != c.code || !strings.Contains(r.Message, c.says) {
			t.Errorf("%+v: %s %q, want %s saying %q", c.req, r.Code, r.Message, c.code, c.says)
		}
	}
	_, err := f.svc.ReplaceSource(asOperator, graph.ReplaceSourceRequest{ItemID: bunny, Path: theirs})
	if r := sourceRefusal(t, err); r.ItemID != other || r.ExtraID != "" {
		t.Errorf("another title's file names %q and %q, want the title", r.ItemID, r.ExtraID)
	}
	_, err = f.svc.ReplaceSource(asOperator, graph.ReplaceSourceRequest{ItemID: bunny, Path: media("Elephants Dream Score.mkv")})
	if r := sourceRefusal(t, err); r.ItemID != other || r.ExtraID != "x-other" {
		t.Errorf("an extra's file names %q and %q, want the extra and its title", r.ItemID, r.ExtraID)
	}
	if after := keptOf(t, f.st, bunny, "") + assetOf(t, f.st, "a-bunny") + assetOf(t, f.st, "a-other"); after != before {
		t.Errorf("a refusal changed the catalog:\nbefore %s\nafter  %s", before, after)
	}
	if gone(f.old) || gone(theirs) || len(f.re.take()) != 0 {
		t.Error("a refusal deleted a file, or encoded a title again")
	}
	// The title with two files is named by the one replaced.
	if res, err := f.svc.ReplaceSource(asOperator, graph.ReplaceSourceRequest{ItemPath: first, Path: f.write(t, "media/Twice/three.mkv", 10)}); err != nil ||
		!res.Replaced || res.ItemID != twice || res.OldPath != first {
		t.Errorf("one of the two files, by its path: %+v, %v", res, err)
	}
}

// The old file goes only from the media root, only when nothing of the
// catalog is it any more, and never when the new file leads to it; one gone
// already is said so. The title has its new file whatever becomes of the old.
func TestTheOldFileGoesOnlyWhenNothingHoldsIt(t *testing.T) {
	f := newReplacing(t)
	for i, c := range []struct {
		name  string
		setup func(id, old string) (cur string)
		says  string
	}{
		{"outside the media root", func(id, old string) string {
			p := f.write(t, "library/movies/"+id+".mkv", 10)
			storetest.Exec(t, f.st, `UPDATE com_nalet_katalog_playbackassets SET path = $2 WHERE item_id = $1`, id, p)
			return f.write(t, "media/Outside "+id+".mkv", 10)
		}, "the old file is kept: it is not under the media root"},
		{"another title's file still", func(id, old string) string {
			storetest.Exec(t, f.st, `INSERT INTO com_nalet_katalog_playbackassets (id, item_id, path, kind) VALUES ('a-trailer', $1, $2, 'trailer')`,
				bunny, old)
			return f.write(t, "media/Held "+id+".mkv", 10)
		}, "the old file is kept: it is a file of item " + bunny + " still"},
		{"an extra's file", func(id, old string) string {
			storetest.Exec(t, f.st, `INSERT INTO com_nalet_katalog_itemextras (id, item_id, kind, title, registeredby, sourcepath)
				VALUES ('x-held', $1, 'other', 'Other', 'api', $2)`, bunny, old)
			return f.write(t, "media/Extra "+id+".mkv", 10)
		}, "the old file is kept: it is extra x-held of item " + bunny},
		{"the new file a link to it", func(id, old string) string {
			p := filepath.Join(f.cfg.NFSRoot, "Linked "+id+".mkv")
			if err := os.Symlink(old, p); err != nil {
				t.Fatal(err)
			}
			return p
		}, "the old file is kept: the new one leads to it"},
		{"gone already", func(id, old string) string {
			if err := os.Remove(old); err != nil {
				t.Fatal(err)
			}
			return f.write(t, "media/After "+id+".mkv", 10)
		}, "the old file was gone already"},
	} {
		id, asset := fmt.Sprintf("%02d0f0f0f-0000-4000-8000-0000000000%02d", i, i), fmt.Sprintf("a-held-%02d", i)
		old := f.film(t, id, asset, fmt.Sprintf("media/Film %d.mkv", i))
		cur := c.setup(id, old)
		var was string
		_ = f.st.Pool().QueryRow(context.Background(), `SELECT path FROM com_nalet_katalog_playbackassets WHERE id = $1`, asset).Scan(&was)
		res, err := f.svc.ReplaceSource(asOperator, graph.ReplaceSourceRequest{ItemID: id, Path: cur, DeleteOldFile: true})
		if err != nil || !res.Replaced || res.OldFileDeleted || !strings.Contains(res.Message, "; "+c.says+";") {
			t.Errorf("%s: %+v, %v; want it replaced, saying %q", c.name, res, err, c.says)
			continue
		}
		if c.name != "gone already" && gone(was) {
			t.Errorf("%s: %s went", c.name, was)
		}
		if got := assetOf(t, f.st, asset); !strings.HasPrefix(got, id+" "+cur+" ") {
			t.Errorf("%s: the title's source asset: %s", c.name, got)
		}
	}

	// A package store inside the media root: a file a title had there (one
	// taken in through POST /api/ingest) stays.
	nested := New(f.st, config.Config{NFSRoot: f.dir, PackagesRoot: f.dir + "/packages"}, processing.New(f.st.Pool()), nil)
	staged := f.film(t, "990f0f0f-0000-4000-8000-000000000099", "a-staged", "packages/_staged/A Film.mkv")
	res, err := nested.ReplaceSource(asOperator, graph.ReplaceSourceRequest{ItemPath: staged, Path: f.write(t, "media/A Film.mkv", 10),
		DeleteOldFile: true})
	if err != nil || !res.Replaced || res.OldFileDeleted || !strings.Contains(res.Message, "; the old file is kept: it is under the package store;") ||
		gone(staged) {
		t.Errorf("a file under the package store: %+v, %v; want it replaced and the file kept", res, err)
	}
}

// The title is encoded again as reencodeItem answers: one left alone because
// its transcode runs says that run is the old file's; a re-encode that cannot
// run (no event bus) says why, and is no failure: the title has its new file.
// Without a re-encoder wired it is said too.
func TestATitleGivenAFileIsEncodedAgainAsReencodeItemAnswers(t *testing.T) {
	f := newReplacing(t)
	busy := "transcode is running: its worker last reported at 2026-10-05T08:00:00Z, and encoding it again would run it twice; " +
		"wait for it, or for its timeout of 6h"
	f.re.res = graph.ReencodeResult{Titles: 1, Busy: 1, Message: busy}
	res, err := f.svc.ReplaceSource(asOperator, graph.ReplaceSourceRequest{ItemID: bunny, Path: f.cur, Reencode: true})
	if err != nil || !res.Replaced || res.Reencode == nil || res.Reencode.Busy != 1 ||
		!strings.HasSuffix(res.Message, "; "+busy+"; that run is the old file's: encode the title again (reencodeItem) once it is done") {
		t.Fatalf("a title being encoded: %+v, %v", res, err)
	}

	noBus := errors.New("cannot re-encode: no event bus: a retry sends the step's trigger event again, and KAFKA_BROKERS is not set")
	f.re.res, f.re.err = graph.ReencodeResult{}, noBus
	next := f.write(t, "media/Big Buck Bunny (2008) Remastered.mkv", 10)
	res, err = f.svc.ReplaceSource(asOperator, graph.ReplaceSourceRequest{ItemID: bunny, Path: next, Reencode: true})
	if err != nil || !res.Replaced || res.Reencode == nil || res.Reencode.ItemID != bunny || res.Reencode.Message != noBus.Error() ||
		res.Reencode.Reencoded != 0 || !strings.HasSuffix(res.Message, "; "+noBus.Error()) {
		t.Fatalf("without an event bus: %+v, %v", res, err)
	}
	if got := assetOf(t, f.st, "a-bunny"); !strings.HasPrefix(got, bunny+" "+next+" ") {
		t.Errorf("the film's source asset: %s", got)
	}

	f.svc.reencoder = nil
	res, err = f.svc.ReplaceSource(asOperator, graph.ReplaceSourceRequest{ItemID: bunny, Path: f.cur, Reencode: true})
	if err != nil || res.Reencode == nil || res.Reencode.Message != "cannot re-encode: nothing encodes a title again here" {
		t.Errorf("without a re-encoder: %+v, %v", res, err)
	}
}

// Titles given one file at once: one takes it, the others are refused,
// naming it, and the file is one title's.
func TestTitlesGivenOneFileAtOnce(t *testing.T) {
	f := newReplacing(t)
	var ids []string
	for i := range 6 {
		id := fmt.Sprintf("%02d1e1e1e-0000-4000-8000-0000000000%02d", i, i)
		f.film(t, id, fmt.Sprintf("a-copy-%02d", i), fmt.Sprintf("media/Copy %d.mp4", i))
		ids = append(ids, id)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	won, refused := map[string]bool{}, map[string]string{}
	for _, id := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := f.svc.ReplaceSource(asOperator, graph.ReplaceSourceRequest{ItemID: id, Path: f.cur})
			mu.Lock()
			defer mu.Unlock()
			var r *graph.SourceRefused
			switch {
			case err == nil && res.Replaced:
				won[id] = true
			case errors.As(err, &r) && r.Code == "SOURCE_CONFLICT":
				refused[id] = r.ItemID
			default:
				t.Errorf("%s: %+v, %v", id, res, err)
			}
		}()
	}
	wg.Wait()
	if len(won) != 1 || len(refused) != 5 {
		t.Fatalf("%d took the file and %d were refused, want one and five", len(won), len(refused))
	}
	for id, in := range refused {
		if !won[in] {
			t.Errorf("%s was refused naming %s, which did not take the file", id, in)
		}
	}
	if n := storetest.Count(t, f.st, `SELECT count(*) FROM com_nalet_katalog_playbackassets WHERE path = $1`, f.cur); n != 1 {
		t.Errorf("%d asset rows have the file, want one", n)
	}
}

// After a replace a scan finds the new file its title's, takes nothing in
// for it and pairs the subtitle files named after it; an old file kept under
// the media root is taken in as a title of its own.
func TestAScanFindsTheNewFileItsTitles(t *testing.T) {
	f := newReplacing(t)
	sintel := f.film(t, "c1c1c1c1-0000-4000-8000-00000000000c", "a-sintel", "media/Sintel.2010.720p.mkv")
	better := f.write(t, "media/Sintel (2010).mkv", 3000)
	named := f.write(t, "media/Big Buck Bunny (2008).de.srt", 10)
	if _, err := f.svc.ReplaceSource(asOperator, graph.ReplaceSourceRequest{ItemPath: f.old, Path: f.cur, DeleteOldFile: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.ReplaceSource(asOperator, graph.ReplaceSourceRequest{ItemPath: sintel, Path: better}); err != nil {
		t.Fatal(err)
	}
	sc := scanner.New(f.st, f.cfg, processing.New(f.st.Pool()), nil)
	job, err := sc.Trigger(context.Background(), "nfs")
	if err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(10 * time.Second); storetest.Count(t, f.st,
		`SELECT count(*) FROM com_nalet_katalog_scanjobs WHERE id = $1 AND status = 'running'`, job) == 1; {
		if time.Now().After(deadline) {
			t.Fatal("the scan did not end")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if n := storetest.Count(t, f.st, `SELECT count(*) FROM com_nalet_katalog_scanjobs WHERE id = $1 AND status = 'done'
		AND itemsinserted = 1 AND itemsupdated = 2`, job); n != 1 {
		t.Error("the scan did not find the two new files their titles' and the old file kept a title of its own")
	}
	for path, want := range map[string]string{f.cur: bunny, better: "c1c1c1c1-0000-4000-8000-00000000000c"} {
		if n := storetest.Count(t, f.st, `SELECT count(*) FROM com_nalet_katalog_playbackassets WHERE path = $1 AND item_id = $2`, path, want); n != 1 {
			t.Errorf("%s is not item %s's file alone after the scan", path, want)
		}
	}
	if n := storetest.Count(t, f.st, `SELECT count(*) FROM com_nalet_katalog_playbackassets p JOIN com_nalet_katalog_items i ON i.id = p.item_id
		WHERE p.path = $1 AND i.id <> 'c1c1c1c1-0000-4000-8000-00000000000c'`, sintel); n != 1 {
		t.Error("the old file kept under the media root is no title of its own after the scan")
	}
	if n := storetest.Count(t, f.st, `SELECT count(*) FROM com_nalet_katalog_subtitleassets WHERE item_id = $1 AND path = ANY($2)`,
		bunny, []string{named, filepath.Join(f.cfg.NFSRoot, "BigBuckBunny_320x180.en.srt")}); n != 2 {
		t.Errorf("the film has %d of the subtitle files named after its new and its old file, want both", n)
	}
}

// On a catalog older than the migrations that keep a source's tracks (037)
// and a title's extras (039), a title is given its file all the same.
func TestAFileIsReplacedOnACatalogOlderThanTheTracksAndTheExtras(t *testing.T) {
	st := storetest.OpenBase(t)
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f := &replacing{st: st, dir: dir, cfg: config.Config{NFSRoot: dir + "/media", PackagesRoot: dir + "/packages"}}
	old := f.film(t, bunny, "a-bunny", "media/BigBuckBunny_320x180.mp4")
	cur := f.write(t, "media/Big Buck Bunny (2008).mov", 5000)
	svc := New(st, f.cfg, processing.New(st.Pool()), nil)
	res, err := svc.ReplaceSource(asOperator, graph.ReplaceSourceRequest{ItemPath: old, Path: cur, DeleteOldFile: true})
	if err != nil || !res.Replaced || !res.OldFileDeleted {
		t.Fatalf("ReplaceSource: %+v, %v", res, err)
	}
	if got := assetOf(t, st, "a-bunny"); got != bunny+" "+cur+" 5000 primary=true kind=primary holds=" {
		t.Errorf("the film's source asset: %s", got)
	}
}
