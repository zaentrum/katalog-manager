package compliance

import (
	"context"
	"strings"
	"testing"

	"github.com/zaentrum/katalog-manager/internal/config"
	"github.com/zaentrum/katalog-manager/internal/library"
	"github.com/zaentrum/katalog-manager/internal/library/librarytest"
	"github.com/zaentrum/katalog-manager/internal/store/storetest"
)

// The check's word on retiring an original (Passed): a title it checked
// against the rules both workers report now, not marked to be checked again
// since, that breaks no rule, passes; one that breaks a rule, one it never
// checked, one marked to be checked again, one checked against other rules,
// one whose records it couldn't read, one processing again can't help, and
// any while a worker hasn't reported, don't, each saying why.
func TestTheChecksWordOnRetiringAnOriginal(t *testing.T) {
	s, st, _ := library4(t)
	ctx := context.Background()
	if _, err := s.Pass(ctx); err != nil {
		t.Fatal(err)
	}
	passed := func(ids ...string) map[string]string {
		t.Helper()
		got, err := s.Passed(ctx, ids)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	for id, want := range map[string]string{
		uuid(2):            "",
		uuid(1):            "the compliance check finds it breaks OVER_BITRATE_CAP, OVER_SIZE_CAP",
		uuid(4):            "the compliance check finds it breaks NO_SURROUND, IMAGE_SUBTITLES_MISSING",
		uuid(5):            "the compliance check finds it breaks NOT_PACKAGED",
		"an-unknown-title": "the compliance check hasn’t checked it",
	} {
		if got := passed(id)[id]; got != want {
			t.Errorf("%s: %q, want %q", id, got, want)
		}
	}
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_compliance SET unchecked = 'versions/v/package.json is larger than 4 MiB, more than any record'
		WHERE item_id = $1`, uuid(2))
	if got := passed(uuid(2))[uuid(2)]; got != "the compliance check couldn’t read its records: versions/v/package.json is larger "+
		"than 4 MiB, more than any record" {
		t.Errorf("records unread: %q", got)
	}
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_compliance SET unchecked = NULL, blocked = 'dolbyvision:7' WHERE item_id = $1`, uuid(2))
	if got := passed(uuid(2))[uuid(2)]; got != "the compliance check found that processing it again can’t help: "+
		"Dolby Vision profile 7 needs a tone-mapping encode" {
		t.Errorf("blocked: %q", got)
	}
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_compliance SET blocked = NULL WHERE item_id = $1`, uuid(2))
	s.Touch(ctx, uuid(2))
	if got := passed(uuid(2))[uuid(2)]; got != "it changed since the compliance check checked it: it waits to be checked again" {
		t.Errorf("marked to be checked again: %q", got)
	}
	if _, err := s.Pass(ctx); err != nil {
		t.Fatal(err)
	}
	if got := passed(uuid(2))[uuid(2)]; got != "" {
		t.Errorf("checked again: %q", got)
	}
	// rules that change: no title is within them until it is checked again
	reportFor(t, s, Transcoder, strings.Replace(transcoderReport, `"movie1080Mbps": 8`, `"movie1080Mbps": 9`, 1))
	if got := passed(uuid(2))[uuid(2)]; !strings.HasPrefix(got, "the compliance check checked it against other rules") {
		t.Errorf("rules changed since: %q", got)
	}
	if _, err := s.Pass(ctx); err != nil {
		t.Fatal(err)
	}
	if got := passed(uuid(2))[uuid(2)]; got != "" {
		t.Errorf("checked against the rules reported now: %q", got)
	}
	storetest.Exec(t, st, `DELETE FROM com_nalet_katalog_workerrules WHERE worker = 'packager'`)
	if got := passed(uuid(2))[uuid(2)]; got != "the packager hasn’t reported its rules, so the compliance check can’t tell "+
		"whether it is within them" {
		t.Errorf("the packager not reported: %q", got)
	}
}

// The rules in force a packaged title could not be held against, as the
// check marks them: what one needs of it not known (its bucket, its bit rate,
// its codec, its original's size, a record), or the limit its kind and bucket
// would be held to not reported. A cap of 0 allows any, and a browser-friendly
// H.264 copy a transcoder keeps is held to none.
func TestTheRulesAPackagedTitleCouldNotBeHeldAgainst(t *testing.T) {
	rs := stateOf(t, transcoderReport, packagerReport)
	full := recordsOf(t, pkgJSON(1920, 1080, 5e6, "copy"), srcJSON(1920, 1080))
	noRate := recordsOf(t, strings.NewReplacer(`"sizeBytes": 9000000000, "durationMs": 7200000,`, ``,
		`, "bitrateBps": 5000000`, ``).Replace(pkgJSON(1920, 1080, 5e6, "copy")), strings.Replace(srcJSON(1920, 1080),
		`"file": {"sizeBytes": 9500000000}, "container": {"durationMs": 7200000},`, `"container": {},`, 1))
	cpu := stateOf(t, strings.NewReplacer(`"codec": "hevc_nvenc"`, `"codec": "libx265"`, `"ladder": "source:hevc"`,
		`"ladder": "source"`).Replace(transcoderReport), packagerReport)
	h264 := recordsOf(t, strings.Replace(pkgJSON(1920, 1080, 20e6, "copy"), `"hvc1.2.4.L153.B0"`, `"avc1.640028"`, 1),
		strings.Replace(srcJSON(1920, 1080), `"codec": "hevc"`, `"codec": "h264", "profile": "High", "pixelFormat": "yuv420p"`, 1))
	for _, c := range []struct {
		name string
		ti   title
		r    records
		rs   rulesState
		want string
	}{
		{"every fact known", packagedTitle("movie"), full, rs, ""},
		{"no bit rate, no size, no duration", packagedTitle("movie"), noRate, rs, "OVER_BITRATE_CAP OVER_SIZE_CAP"},
		{"no records at all", packagedTitle("movie"), records{}, rs,
			"OVER_BITRATE_CAP OVER_SIZE_CAP NO_SURROUND IMAGE_SUBTITLES_MISSING VIDEO_NOT_HEVC"},
		{"its kind's cap not reported", packagedTitle("episode"), full,
			stateOf(t, strings.Replace(transcoderReport, `"episode1080Mbps": 6, `, ``, 1), packagerReport), "OVER_BITRATE_CAP"},
		{"its kind's cap 0, no bit rate", packagedTitle("movie"), noRate,
			stateOf(t, strings.Replace(transcoderReport, `"movie1080Mbps": 8`, `"movie1080Mbps": 0`, 1), packagerReport),
			"OVER_SIZE_CAP"},
		{"no 2160p bucket reported", packagedTitle("movie"), full,
			stateOf(t, strings.Replace(transcoderReport, `"bucket2160": {"minHeight": 2000, "minWidth": 3200},`, ``, 1), packagerReport),
			"OVER_BITRATE_CAP"},
		{"a browser-friendly H.264 copy a CPU transcoder keeps", packagedTitle("movie"), h264, cpu, ""},
		{"no renditions reported", packagedTitle("movie"), full,
			stateOf(t, strings.Replace(transcoderReport, `"ladder": "source:hevc", `, ``, 1), packagerReport), "VIDEO_NOT_HEVC"},
		{"no surround track and no image subtitles reported", packagedTitle("movie"), full,
			stateOf(t, transcoderReport, `{"stereoAudio": "aac", "stereoBitrate": "192k"}`), "NO_SURROUND IMAGE_SUBTITLES_MISSING"},
		{"no surround track made, no image subtitles kept, no records", packagedTitle("movie"), records{},
			stateOf(t, transcoderReport, strings.NewReplacer(`"surroundAudio": "eac3"`, `"surroundAudio": "off"`,
				`["pgs", "vobsub"]`, `[]`).Replace(packagerReport)), "OVER_BITRATE_CAP OVER_SIZE_CAP VIDEO_NOT_HEVC"},
	} {
		if got := strings.Join(evaluate(c.ti, c.r, c.rs).UncheckedRules, " "); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

// The check's word on retiring an original is of the title's current
// version, as checked since the nightly check last fell due: a version
// completed since its row was written (nothing marked it), and a row older
// than the nightly check, are no word.
func TestTheChecksWordIsOfTheCurrentVersion(t *testing.T) {
	s, st, dir := library4(t)
	ctx := context.Background()
	if _, err := s.Pass(ctx); err != nil {
		t.Fatal(err)
	}
	id := uuid(2)
	passed := func() string {
		t.Helper()
		got, err := s.Passed(ctx, []string{id})
		if err != nil {
			t.Fatal(err)
		}
		return got[id]
	}
	if got := passed(); got != "" {
		t.Fatalf("before: %q", got)
	}
	// a row older than the nightly check, of a version older still
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_itemversions SET completedat = now() - interval '3 days' WHERE item_id = $1`, id)
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_compliance SET evaluatedat = now() - interval '2 days' WHERE item_id = $1`, id)
	if got := passed(); got != "the compliance check last checked it before its nightly check fell due: it waits to be checked again" {
		t.Errorf("a row older than the nightly check: %q", got)
	}
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_settings (id, key, valuetext) VALUES ('n', 'compliance.nightly', 'off')`)
	if got := passed(); got != "" {
		t.Errorf("no nightly check: %q", got)
	}
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_compliance SET evaluatedat = now() WHERE item_id = $1`, id)

	// a version completes, its package over the 1080p cap, and nothing marks
	// the title to be checked again
	p := packaged{n: 2}
	newVer := uuid(7002)
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_itemversions SET state = 'superseded' WHERE item_id = $1`, id)
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_itemversions (id, item_id, sourceids, state, packageid, completedat)
		VALUES ($1, $2, ARRAY[$3]::varchar[], 'complete', $4, now())`, newVer, id, p.source(), uuid(7003))
	itemDir := library.Paths{Root: dir}.ItemDir(library.Place{ID: id, Type: "movie"})
	librarytest.WriteVersion(t, library.VersionDir(itemDir, newVer), librarytest.Version{
		VersionID: newVer, PackageID: uuid(7003), SourceIDs: []string{p.source()}, CreatedAt: "2026-10-27T08:00:00Z",
		Package: map[string]any{"sizeBytes": 9e9, "durationMs": 7200000,
			"renditions": map[string]any{"video": []any{map[string]any{"id": "v0", "codec": "hvc1.2.4.L153.B0",
				"width": 1920, "height": 1080, "bitrateBps": 12e6, "encoder": "hevc_nvenc"}},
				"audio": []any{map[string]any{"channels": 2, "bitrateBps": 192000}}},
			"essence": map[string]any{"surround": false, "maxAudioChannels": 2}},
	})
	want := "the compliance check checked it before its version " + newVer + " was complete: it waits to be checked again"
	if got := passed(); got != want {
		t.Errorf("a version completed since: %q", got)
	}
	if _, err := s.Pass(ctx); err != nil {
		t.Fatal(err)
	}
	if got := passed(); got != want {
		t.Errorf("a pass that has no cause to check it again: %q", got)
	}
	s.Touch(ctx, id)
	if _, err := s.Pass(ctx); err != nil {
		t.Fatal(err)
	}
	if got := passed(); got != "the compliance check finds it breaks OVER_BITRATE_CAP" {
		t.Errorf("its version checked: %q", got)
	}

	// a row of the version's records, written before it was complete, and
	// one written after, of another version's records
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_compliance SET rules = '{}', findings = '[]'::jsonb WHERE item_id = $1`, id)
	if got := passed(); got != "" {
		t.Fatalf("its row of no rule: %q", got)
	}
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_compliance SET evaluatedat = now() - interval '1 hour' WHERE item_id = $1`, id)
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_itemversions SET completedat = now() - interval '1 minute' WHERE id = $1`, newVer)
	if got := passed(); got != want {
		t.Errorf("a row of its version written before it was complete: %q", got)
	}
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_compliance SET evaluatedat = now(), recordkey = replace(recordkey, $2, $3)
		WHERE item_id = $1`, id, "v:"+newVer+"@", "v:"+uuid(7004)+"@")
	if got := passed(); got != "the compliance check read the records of another version than its version "+newVer+
		": it waits to be checked again" {
		t.Errorf("a row of another version's records: %q", got)
	}
}

// A packaged title whose video's bit rate is not measured, held to no cap, is
// no title within every rule.
func TestTheChecksWordOfATitleItCouldNotHoldToACap(t *testing.T) {
	s, st, dir := checkOf(t)
	ctx := context.Background()
	p := packaged{n: 20, kind: "movie", title: "A Film Of Unknown Rate", width: 1920, height: 1080, rate: 0, encoder: "hevc_nvenc",
		original: 30e9, size: 20e9, srcCh: 2, pkgCh: 2}
	p.add(t, st, dir)
	itemDir := library.Paths{Root: dir}.ItemDir(library.Place{ID: p.id(), Type: "movie"})
	librarytest.WriteVersion(t, library.VersionDir(itemDir, p.version()), librarytest.Version{
		VersionID: p.version(), PackageID: uuid(3000 + p.n), SourceIDs: []string{p.source()}, CreatedAt: "2026-10-01T08:00:00Z",
		Package: map[string]any{
			"renditions": map[string]any{"video": []any{map[string]any{"id": "v0", "codec": "hvc1.2.4.L153.B0",
				"width": 1920, "height": 1080, "encoder": "hevc_nvenc"}},
				"audio": []any{map[string]any{"channels": 2, "bitrateBps": 192000}}},
			"essence": map[string]any{"surround": false, "maxAudioChannels": 2}},
	})
	if _, err := s.Pass(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := s.Passed(ctx, []string{p.id()})
	if err != nil || got[p.id()] != "the compliance check couldn’t hold it against OVER_BITRATE_CAP: what that takes of it isn’t known" {
		t.Errorf("a title of no bit rate measured: %q, %v", got[p.id()], err)
	}
	if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_compliance WHERE item_id = $1
		AND uncheckedrules = ARRAY['OVER_BITRATE_CAP']::varchar[] AND rules = '{}'`, p.id()); n != 1 {
		t.Error("its row says no rule it could not be held against")
	}
}

// Without the transcoder's report, or without migration 051, no title is
// within every rule.
func TestTheChecksWordWithoutTheTranscoderOrItsTables(t *testing.T) {
	s, st, _ := library4(t)
	ctx := context.Background()
	if _, err := s.Pass(ctx); err != nil {
		t.Fatal(err)
	}
	storetest.Exec(t, st, `DELETE FROM com_nalet_katalog_workerrules WHERE worker = 'transcoder'`)
	if got, err := s.Passed(ctx, []string{uuid(2)}); err != nil || got[uuid(2)] != "the transcoder hasn’t reported its rules, "+
		"so the compliance check can’t tell whether it is within them" {
		t.Errorf("the transcoder not reported: %q, %v", got[uuid(2)], err)
	}
	bare := New(storetest.OpenBase(t), config.Config{LibraryRoot: t.TempDir()})
	if got, err := bare.Passed(ctx, []string{uuid(2)}); err != nil ||
		!strings.HasPrefix(got[uuid(2)], "the library’s compliance isn’t checked: ") {
		t.Errorf("without migration 051: %q, %v", got[uuid(2)], err)
	}
}
