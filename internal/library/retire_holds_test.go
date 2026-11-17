package library

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zaentrum/katalog-manager/internal/library/librarytest"
	"github.com/zaentrum/katalog-manager/internal/store/storetest"
)

// rewriteRecord writes the fixture's source record again as change leaves
// it, its copies of its sidecars and its stored probe as they were (probe,
// when not nil, its stored probe's content: "" none).
func (f *retireFixture) rewriteRecord(t *testing.T, change func(rec map[string]any), probe *string) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(f.sourceDir, "source.json"))
	if err != nil {
		t.Fatal(err)
	}
	var rec map[string]any
	if err := json.Unmarshal(b, &rec); err != nil {
		t.Fatal(err)
	}
	change(rec)
	copies := map[string]string{}
	entries, err := os.ReadDir(f.sourceDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() == "source.json" || e.Name() == SumsFile {
			continue
		}
		c, err := os.ReadFile(filepath.Join(f.sourceDir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		copies[e.Name()] = string(c)
	}
	if probe != nil {
		delete(copies, ProbeFile)
		if err := os.Remove(filepath.Join(f.sourceDir, ProbeFile)); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		if *probe != "" {
			copies[ProbeFile] = *probe
		}
	}
	librarytest.WriteSource(t, f.sourceDir, rec, copies)
}

// rewritePackage writes the fixture's version again, its package record as
// change leaves packageRecord's, its version.json's fields besides what
// WriteVersion writes version.
func (f *retireFixture) rewritePackage(t *testing.T, change func(pkg map[string]any), version map[string]any) {
	t.Helper()
	pkg := packageRecord(f.pkg)
	if change != nil {
		change(pkg)
	}
	librarytest.WriteVersion(t, f.versionDir, librarytest.Version{VersionID: rtVersion, PackageID: rtPackage,
		SourceIDs: []string{rtSource}, CreatedAt: "2026-10-06T10:00:00Z", Package: pkg, Version: version})
}

// keptFor runs a pass of the retire job, which keeps the fixture's original,
// saying why: its source's error has want in it.
func (f *retireFixture) keptFor(t *testing.T, name, want string) {
	t.Helper()
	if rep := f.pass(t); rep.Originals != 0 {
		t.Errorf("%s: the pass deleted it: %+v", name, rep)
	}
	f.kept(t)
	if s := f.source(t, rtSource); !strings.Contains(deref(s.Error), want) {
		t.Errorf("%s: kept for %q; want %q", name, deref(s.Error), want)
	}
}

// deletedBy runs a pass that deletes the fixture's original.
func (f *retireFixture) deletedBy(t *testing.T, name string) {
	t.Helper()
	if rep := f.pass(t); rep.Originals != 1 {
		t.Errorf("%s: the pass: %+v (%s); want the original deleted", name, rep, deref(f.source(t, rtSource).Error))
	}
}

// sameEssence is an essence both the fixture's records share, so that nothing but
// the case decides.
var sameEssence = map[string]any{"surround": false, "maxAudioChannels": 2, "audioLanguages": []string{"en"},
	"subtitleLanguages": []string{"en", "de"}}

// The copy rule: an original whose package copies its picture (its top
// video rendition's encoder copy) is kept, as no record proves the copy
// carries it whole, and so is one whose package says no encoder; one whose
// package states its encoder goes.
func TestTheCopyRuleKeepsTheOriginalOfACopy(t *testing.T) {
	video := func(v0 map[string]any) func(pkg map[string]any) {
		return func(pkg map[string]any) {
			pkg["renditions"] = map[string]any{"video": []any{v0}}
		}
	}
	for _, c := range []struct {
		name   string
		change func(pkg map[string]any)
		want   string
	}{
		{"encoder copy", video(map[string]any{"id": "v0", "encoder": "copy"}), CopyUnproven},
		{"encoder Copy, spaced", video(map[string]any{"id": "v0", "encoder": " Copy "}), CopyUnproven},
		{"encoder copy, a scan that verified it progressive", func(pkg map[string]any) {
			video(map[string]any{"id": "v0", "encoder": "copy"})(pkg)
			pkg["scan"] = verifiedProgressive()
		}, CopyUnproven},
		{"no encoder", video(map[string]any{"id": "v0", "encoder": nil}), "states no encoder"},
		{"an encoder that is no word", video(map[string]any{"id": "v0", "encoder": 7}), "states no encoder"},
		{"no video rendition", func(pkg map[string]any) { pkg["renditions"] = map[string]any{"video": []any{}} },
			"lists no video rendition"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newRetireFixtureOf(t, sameEssence, sameEssence)
			f.rewritePackage(t, c.change, nil)
			f.keptFor(t, c.name, c.want)
		})
	}
	f := newRetireFixtureOf(t, sameEssence, sameEssence)
	f.rewritePackage(t, video(map[string]any{"id": "v0", "encoder": "hevc_nvenc"}), nil)
	f.deletedBy(t, "an encode")
}

// A source record that lists no streams of its file (none, null, an empty
// list) tells the gate nothing of its tracks and its picture: the original
// is kept.
func TestARecordWithoutStreamsKeepsItsOriginal(t *testing.T) {
	for name, change := range map[string]func(rec map[string]any){
		"no streams":   func(rec map[string]any) { delete(rec, "streams") },
		"streams null": func(rec map[string]any) { rec["streams"] = nil },
		"no stream":    func(rec map[string]any) { rec["streams"] = []any{} },
	} {
		t.Run(name, func(t *testing.T) {
			f := newRetireFixtureOf(t, fullEssence(), fullEssence())
			f.rewriteRecord(t, change, nil)
			f.keptFor(t, name, "lists no stream")
		})
	}
}

// The HEVC rule on the retire path: a film of hevc, or one whose codec
// nothing states, is kept while its record says of no read of every packet
// of its picture that it found no caption signature; one whose record says
// so (captionSignature false) goes, as an h264 film does.
func TestTheRetireJobHoldsAnHEVCOriginalUntilItsPacketsAreRead(t *testing.T) {
	none := ""
	hevcProbe := `{"streams": [{"index": 0, "codec_type": "video", "codec_name": "hevc", "field_order": "progressive"}]}`
	for _, c := range []struct {
		name  string
		film  map[string]any
		probe *string
		goes  bool
	}{
		{"hevc", map[string]any{"codec": "hevc"}, nil, false},
		{"hevc by its probe, h264 by its record", map[string]any{"codec": "h264"}, &hevcProbe, false},
		{"no codec, no probe", map[string]any{}, &none, false},
		{"codec unknown, no probe", map[string]any{"codec": "unknown"}, &none, false},
		{"codec a number, no probe", map[string]any{"codec": 265}, &none, false},
		{"hevc, its packets read whole and no signature found", map[string]any{"codec": "hevc", "captionSignature": false},
			nil, true},
		{"h264", map[string]any{"codec": "h264"}, nil, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newRetireFixtureOf(t, sameEssence, sameEssence)
			f.rewriteRecord(t, func(rec map[string]any) {
				film := map[string]any{"index": 0, "type": "video", "fieldOrder": "progressive"}
				for k, v := range c.film {
					film[k] = v
				}
				rec["streams"] = []any{film}
			}, c.probe)
			if c.goes {
				f.deletedBy(t, c.name)
				return
			}
			f.keptFor(t, c.name, CaptionsUnscanned)
		})
	}
}

// The scan of the version the original is retired from is read whatever its
// version.json says, and a version.json that names no list of the
// originals the version was made of keeps it.
func TestTheRetireJobReadsItsVersionsOwnScan(t *testing.T) {
	fields := func(pkg map[string]any) {
		pkg["scan"] = map[string]any{"found": "fields", "done": "deinterlaced-frame"}
	}
	for name, ver := range map[string]map[string]any{
		"sourceIds null":       {"sourceIds": nil},
		"sourceIds of another": {"sourceIds": []string{"another"}},
	} {
		t.Run(name, func(t *testing.T) {
			f := newRetireFixtureOf(t, sameEssence, sameEssence)
			f.rewritePackage(t, fields, ver)
			f.keptFor(t, name, "the record of its version "+rtVersion+" (version.json) doesn't name it")
			g, err := f.r.gateOf(context.Background(), f.itemDir, rtVersion, f.source(t, rtSource))
			if err != nil || !strings.HasPrefix(g.Scan, FieldsKept+": the scan of its package of version "+rtVersion+" found fields") {
				t.Errorf("%s: the gate %+v, %v; want its own scan read", name, g, err)
			}
		})
	}
}

// A stored probe that says its picture carries closed captions keeps the
// original for good.
func TestAProbesClosedCaptionsKeepTheOriginal(t *testing.T) {
	f := newRetireFixtureOf(t, sameEssence, sameEssence)
	probe := `{"streams": [{"index": 0, "codec_type": "video", "codec_name": "h264", "field_order": "progressive",
		"closed_captions": 1}]}`
	f.rewriteRecord(t, func(map[string]any) {}, &probe)
	f.keptFor(t, "closed_captions 1", "its stored probe says closed_captions of its picture")
}

// A record that says no length keeps the original, whatever its read says:
// the read's own length places nothing.
func TestARecordWithoutALengthKeepsTheOriginalOnTheRetirePath(t *testing.T) {
	for name, change := range map[string]func(rec map[string]any){
		"no container":        func(rec map[string]any) { delete(rec, "container") },
		"durationMs a string": func(rec map[string]any) { rec["container"].(map[string]any)["durationMs"] = "600000" },
		"durationMs 0":        func(rec map[string]any) { rec["container"].(map[string]any)["durationMs"] = 0 },
	} {
		t.Run(name, func(t *testing.T) {
			f := newRetireFixtureOf(t, sameEssence, sameEssence)
			f.rewriteRecord(t, change, nil)
			// a read of its own length, ten seconds, its windows in them
			storetest.Exec(t, f.st, `UPDATE com_nalet_katalog_sourcecaptions SET duration = 10, captionwindows = $2::jsonb
				WHERE source_id = $1`, rtSource, `[{"at": 0, "start": 0, "end": 0.9, "frames": 48, "captions": false,
				"interlacedFrames": 0, "repeatFrames": 0}, {"at": 0.1, "start": 1, "end": 1.9, "frames": 48, "captions": false,
				"interlacedFrames": 0, "repeatFrames": 0}, {"at": 0.3, "start": 3, "end": 3.9, "frames": 48, "captions": false,
				"interlacedFrames": 0, "repeatFrames": 0}, {"at": 0.5, "start": 5, "end": 5.9, "frames": 48, "captions": false,
				"interlacedFrames": 0, "repeatFrames": 0}]`)
			f.keptFor(t, name, CaptionsNoLength)
		})
	}
}

// The original is checked against its record again right before it is moved
// or unlinked, on every path: a file put in its place after its retirement
// recorded its event (resumed) is not moved, nor unlinked.
func TestTheOriginalIsCheckedAgainRightBeforeItGoes(t *testing.T) {
	for _, grace := range []string{"", "0"} {
		f := newRetireFixture(t)
		if grace != "" {
			f.set(t, SettingTrashGrace, grace)
		}
		rt := f.recorded(t)
		b, err := os.ReadFile(f.original)
		if err != nil {
			t.Fatal(err)
		}
		other := []byte(strings.Repeat("x", len(b)))
		librarytest.Write(t, f.original, other)
		set, _ := ReadSettings(context.Background(), f.st.Pool())
		if err := f.r.deleteFiles(f.p, set, rt); err == nil || !strings.Contains(err.Error(), "changed since it was recorded") {
			t.Errorf("trash grace %q: the files of another file: %v; want it refused", grace, err)
		}
		if rep := f.passKeeping(t); rep.Originals != 0 {
			t.Errorf("trash grace %q: the resumed retirement: %+v; want nothing deleted", grace, rep)
		}
		if got, _ := os.ReadFile(f.original); string(got) != string(other) {
			t.Errorf("trash grace %q: the file put in its place is gone", grace)
		}
	}
}

// A superseded original's retirement asks the gate again after its master
// is verified, right before its event is written: a read that found captions
// since it was asked first keeps it.
func TestASupersededOriginalIsAskedAgainBeforeItsEvent(t *testing.T) {
	f := newRetireFixture(t)
	f.supersede(t, true, withFiveOne)
	beforeSupersededGateAgain = func() {
		storetest.Exec(t, f.st, `UPDATE com_nalet_katalog_sourcecaptions SET captions = true, captionwindows = $2::jsonb
			WHERE source_id = $1`, rtSource, strings.Replace(noCaptionsWindows, `"at": 0.5, "start": 300, "end": 301.88, "frames": 48, `+
			`"captions": false`, `"at": 0.5, "start": 300, "end": 301.88, "frames": 48, "captions": true`, 1))
	}
	t.Cleanup(func() { beforeSupersededGateAgain = nil })
	if rep := f.passKeeping(t); rep.Superseded != 0 {
		t.Errorf("the pass: %+v; want the superseded original kept", rep)
	}
	if !exists(f.original) || len(f.originalDeletedEvents(t)) != 0 {
		t.Error("the superseded original went, or its event was written")
	}
}

// A superseded version made of an original that is still there (present,
// retiring or superseded) waits: its package's scan is what the gate reads
// of that original's fields. Once the original is retired, it is removed.
func TestASupersededVersionWaitsWhileItsOriginalIsThere(t *testing.T) {
	f := newRetireFixture(t)
	f.supersede(t, true, withFiveOne)
	f.set(t, SettingSupersededGrace, "0")
	f.execSQL(t, `UPDATE com_nalet_katalog_items SET retirehold = true WHERE id = $1`, rtFilm)
	if rep := f.pass(t); rep.Versions != 0 || !exists(f.versionDir) {
		t.Errorf("the pass while its original is superseded: %+v; want its version kept", rep)
	}
	f.execSQL(t, `UPDATE com_nalet_katalog_itemsources SET state = 'deleted' WHERE id = $1`, rtSource)
	if rep := f.pass(t); rep.Versions != 1 || exists(f.versionDir) {
		t.Errorf("the pass once its original is retired: %+v; want its version removed", rep)
	}
}

// A hold of an HEVC original (its captions not told by windows alone) is
// among those a read lets go of: once a rewritten record carries its
// caption signature, the read that comes of it releases the retirement.
func TestAReadReleasesAnHEVCHold(t *testing.T) {
	f := newRetireFixture(t)
	held := CaptionsUnscanned + ": hevc carries them where a read of windows may not see them"
	f.execSQL(t, `UPDATE com_nalet_katalog_itemsources SET error = $2 WHERE id = $1`, rtSource, held)
	storetest.Exec(t, f.st, `INSERT INTO com_nalet_katalog_itemprocessingsteps (id, createdat, modifiedat, item_id, step,
		status, attempts, error, details) VALUES (gen_random_uuid()::varchar, now(), now(), $1, 'retire', 'failed', 1, $2, $3)
		ON CONFLICT (item_id, step) DO UPDATE SET status = 'failed', error = $2, details = $3`, rtFilm, held, HeldDetails(rtVersion))
	if err := ReleaseCaptionsHold(context.Background(), f.st.Pool(), rtFilm, rtSource); err != nil {
		t.Fatal(err)
	}
	if s := f.source(t, rtSource); s.Error != nil {
		t.Errorf("the source's hold: %q; want it let go", *s.Error)
	}
	if got := f.step(t, rtFilm); got != "none" {
		t.Errorf("the retire step: %s; want it let go", got)
	}
}
