package captions

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zaentrum/katalog-manager/internal/library"
	"github.com/zaentrum/katalog-manager/internal/store"
	"github.com/zaentrum/katalog-manager/internal/store/storetest"
)

// A version names a release as ffprobe -version does; the catalog takes
// reads of 7.1 and later only, and a build that names no release not at all.
func TestTheFFprobeAReadNamesIsARelease(t *testing.T) {
	for v, want := range map[string]string{
		"7.1": "7.1", "7.1.1": "7.1", "n7.1.1": "7.1", "7.1.1-static": "7.1", "7.1.1-1+b1": "7.1", "8.0": "8.0",
		"ffprobe version 7.1.1 Copyright (c) 2007-2025 the FFmpeg developers": "7.1", "4.4.2-0ubuntu0.22.04.1": "4.4",
		"ffprobe version n7.1.5-12-g1fdbca85aa-20260731": "7.1", "n7.1.5-12-g1fdbca85aa-20260731": "7.1",
		"10.2": "10.2", "N-117000-gabcdef": "", "7": "", "": "", "git-2024": "",
	} {
		rel, err := FFprobeRelease(v)
		got := ""
		if err == nil {
			got = strconv.Itoa(rel[0]) + "." + strconv.Itoa(rel[1])
		}
		if got != want {
			t.Errorf("%q: %q (%v), want %q", v, got, err, want)
		}
	}
	one := planned(nil)
	// 7.1 alone, the release tested: 8.x and 9.x (which says no stream's
	// closed_captions) not until they are
	for v, ok := range map[string]bool{"7.1": true, "7.1.5": true, "n7.1.5-12-g1fdbca85aa-20260731": true,
		"7.1 by its libraries (ffprobe version 1fdbca8: libavutil 59.39.100)": true, "7.2": false, "8.0": false,
		"9.0.2": false, "7.0.2": false, "6.1.1": false, "4.4.2": false} {
		_, err := (&FileReport{CaptionWindows: one, Duration: &tenMinutes, SizeBytes: &aSize, FFprobe: v}).check()
		if (err == nil) != ok {
			t.Errorf("a read of ffprobe %s: %v, want taken %v", v, err, ok)
		}
	}
}

// tenMinutes and aSize are a read's length and the size of the file it read.
var (
	tenMinutes = 600.0
	aSize      = int64(6)
)

// planned are the windows of a read of every planned window of a
// ten-minute video, each 48 frames none of which interlaced or repeating a
// field, none with captions, changed as change says.
func planned(change func([]library.CaptionWindow)) []library.CaptionWindow {
	none := 0
	var out []library.CaptionWindow
	for _, at := range []float64{0, 0.1, 0.3, 0.5, 0.7, 0.9} {
		out = append(out, library.CaptionWindow{At: at, Start: at * 600, End: at*600 + 1.88, Frames: 48,
			InterlacedFrames: &none, RepeatFrames: &none})
	}
	if change != nil {
		change(out)
	}
	return out
}

// A read is its windows: each where in the video it is (0 to 1), the
// presentation seconds it covered, the frames it decoded and whether they
// showed captions, and the size of the file it read. A window that decoded
// no frame shows none, and a read with one is kept, its failed window said,
// to be read again; one none of whose windows decoded a frame is a failure,
// and so is one that says nothing of its video (short of its planned
// windows, as the gate takes none); a read of no window, of a window that is
// none, or that says no size of its file, is refused.
func TestAReadOfClosedCaptionsIsItsWindows(t *testing.T) {
	unread := func(js string) []library.CaptionWindow {
		var ws []library.CaptionWindow
		if err := json.Unmarshal([]byte("["+js+"]"), &ws); err != nil {
			t.Fatal(err)
		}
		return ws
	}
	w := func(at, start, end float64, frames int, captions bool) library.CaptionWindow {
		return library.CaptionWindow{At: at, Start: start, End: end, Frames: frames, Captions: captions}
	}
	for _, c := range []struct {
		name    string
		windows []library.CaptionWindow
		noSize  bool
		bad     string // what the refusal says; "" taken
		read    bool
		why     string
	}{
		{name: "every planned window, one with captions", windows: planned(func(ws []library.CaptionWindow) {
			ws[1].Captions = true
		}), read: true},
		{name: "a window that decoded no frame", windows: planned(func(ws []library.CaptionWindow) {
			ws[3] = library.CaptionWindow{At: 0.5}
		}), read: true, why: "its windows at 50% decoded no frame"},
		{name: "no window decoded a frame", windows: []library.CaptionWindow{w(0.1, 600, 600, 0, false)},
			why: "no window of the read decoded a video frame"},
		{name: "short of its planned windows", windows: planned(nil)[:2], why: "it has no window at 30%, 50%, 70%, 90%"},
		{name: "a window at the keyframe 25 s before its place", windows: planned(func(ws []library.CaptionWindow) {
			ws[3].Start, ws[3].End, ws[3].KeyframeBefore = 275, 276.88, true
		}), read: true},
		{name: "a window 25 s before its place, no keyframe said", windows: planned(func(ws []library.CaptionWindow) {
			ws[3].Start, ws[3].End = 275, 276.88
		}), why: "its windows at 50% lie more than 15 s from their places"},
		{name: "no window", bad: "names the windows it decoded (captionWindows"},
		{name: "no size of its file", windows: planned(nil), noSize: true, bad: "sizeBytes"},
		// a window that is no window is a failed one, kept and said
		{name: "a window past the video", windows: unread(`{"at": 1.2, "start": 600, "end": 602, "frames": 48, "captions": false}`),
			why: "captionWindows[0]: its at is no fraction of the video from 0 to 1"},
		{name: "a window that ends before it starts", windows: unread(`{"at": 0.1, "start": 602, "end": 600, "frames": 48,
			"captions": false}`), why: "end not before start"},
		{name: "a window of no frame with captions", windows: unread(`{"at": 0.1, "start": 600, "end": 602, "frames": 0,
			"captions": true}`), why: "a window that decoded no frame shows no captions"},
		{name: "a window of fewer than no frames", windows: unread(`{"at": 0.1, "start": 600, "end": 602, "frames": -1,
			"captions": false}`), why: "its frames is no whole number of frames"},
	} {
		r := FileReport{CaptionWindows: c.windows, Duration: &tenMinutes, SizeBytes: &aSize, FFprobe: "7.1.1"}
		if c.noSize {
			r.SizeBytes = nil
		}
		o, err := r.check()
		if err == nil {
			// held to a claim of a ten-minute record
			ms, start, end := int64(600000), 0.0, 600.0
			o.holdTo(&ms, &start, &end)
		}
		switch {
		case c.bad != "":
			if err == nil || !strings.Contains(err.Error(), c.bad) {
				t.Errorf("%s: %v, want it refused saying %q", c.name, err, c.bad)
			}
		case err != nil || o.read != c.read || !strings.Contains(o.why, c.why) || (c.why == "") != (o.why == ""):
			t.Errorf("%s: %+v, %v; want read %v, why %q", c.name, o, err, c.read, c.why)
		}
	}
}

// The backfill's settings: on by default, any time, two at a time, 600 an
// hour, a lease of 10m; off stops it; a window set holds as set, one set
// empty is any time, as the default; a value that can't be read is its
// default, said, and a window that can't be read hands out nothing.
func TestTheCaptionsBackfillsSettings(t *testing.T) {
	d := DefaultSettings()
	if !d.On || d.Window.String() != "any time" || d.Inflight != 2 || d.Rate != 600 || d.Lease != 10*time.Minute {
		t.Errorf("the defaults: %+v", d)
	}
	for h := 0; h < 24; h++ {
		if !d.Window.Contains(time.Date(2026, 11, 16, h, 30, 0, 0, time.UTC)) {
			t.Errorf("the default window holds not at %02d:30 UTC", h)
		}
	}
	quiet := time.Date(2026, 11, 16, 8, 0, 0, 0, time.UTC)
	s := DefaultSettings()
	s.set(SettingWindow, "02:00-07:00")
	if s.Window.Contains(quiet) || !s.Window.Contains(quiet.Add(-3*time.Hour)) || s.Window.String() != "02:00-07:00 UTC" {
		t.Errorf("a window set: %s", s.Window)
	}
	if s.set(SettingBackfill, "off"); s.On {
		t.Error("captions.backfill off leaves it on")
	}
	if s.set(SettingBackfill, " On "); !s.On {
		t.Error("captions.backfill On leaves it off")
	}
	s.set(SettingWindow, "")
	if !s.Window.Contains(quiet) || s.Window.String() != "any time" || len(s.Problems) != 0 {
		t.Errorf("a window set empty: %s, %v", s.Window, s.Problems)
	}
	s.set(SettingWindow, "later")
	s.set(SettingInflight, "0")
	s.set(SettingBackfill, "yes")
	if !s.Window.Never || s.Window.Contains(quiet.Add(6*time.Hour)) || s.Inflight != 2 || !s.On || len(s.Problems) != 3 ||
		!strings.Contains(s.Problems[2], "the backfill is on") {
		t.Errorf("values that can't be read: %+v", s)
	}
}

// One original at a time with captions.backfill.inflight 1, the backfill on
// as by default: a claim while it
// is handed out hands out nothing, saying so; once its lease ends it counts a
// failure and waits its backoff, and its run's report, late, counts no
// other; outside a window set nothing is handed out.
func TestTheCaptionsBackfillKeepsItsPace(t *testing.T) {
	ctx := context.Background()
	st := storetest.Open(t)
	dir := t.TempDir()
	for _, id := range []string{"f1", "f2"} {
		media := filepath.Join(dir, id+".mkv")
		if err := os.WriteFile(media, []byte("a film"), 0o644); err != nil {
			t.Fatal(err)
		}
		storetest.AddItem(t, st, id, "movie", "Film "+id, "")
		storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_itemsources (id, item_id, filename, arrivalpath, sizebytes, state,
			recordedat, createdat) VALUES ($1 || '-src', $1, $1 || '.mkv', $2, 6, 'present', now(),
			now() - ($1 = 'f2')::int * interval '1 day')`,
			id, media)
	}
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_settings (id, key, valuetext)
		VALUES ('b', 'captions.backfill.inflight', '1')`)
	at := time.Date(2026, 11, 16, 23, 0, 0, 0, time.UTC)
	s := New(st).WithClock(func() time.Time { return at })
	c, err := s.Claim(ctx, "analyzer-0", 5)
	if err != nil || len(c.Items) != 1 || c.Items[0].SourceID != "f2-src" {
		t.Fatalf("the first claim: %+v, %v; want the oldest original", c, err)
	}
	lapsed := c.Items[0].Token
	if c, err := s.Claim(ctx, "analyzer-0", 5); err != nil || len(c.Items) != 0 ||
		c.Idle != "1 originals are being read (captions.backfill.inflight 1)" {
		t.Errorf("a claim while one is in flight: %+v, %v", c, err)
	}
	at = at.Add(DefaultLease + time.Minute)
	c, err = s.Claim(ctx, "analyzer-0", 5)
	if err != nil || len(c.Items) != 1 || c.Items[0].SourceID != "f1-src" {
		t.Fatalf("the claim after the lease: %+v, %v", c, err)
	}
	r, err := Of(ctx, st.Pool(), "f2-src")
	if err != nil || r.State != StateFailed || r.Failures != 1 || r.NextAttemptAt == nil ||
		!r.NextAttemptAt.Equal(at.Add(FirstBackoff)) || r.Reason == nil || !strings.Contains(*r.Reason, "did not report back") {
		t.Fatalf("the original whose lease ended: %+v, %v", r, err)
	}
	if r, err := s.Record(ctx, "f2-src", Report{Token: lapsed, Original: &FileReport{FFprobe: "7.1.1",
		Error: "stopped"}}); err != nil || r.Failures != 1 || r.Reason == nil || *r.Reason != "stopped" {
		t.Errorf("the lapsed run's late failure: %+v, %v; want it counted once, and why", r, err)
	}
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_settings (id, key, valuetext)
		VALUES ('w', 'captions.backfill.window', '02:00-07:00')`)
	at = time.Date(2026, 11, 17, 8, 0, 0, 0, time.UTC)
	if c, err := s.Claim(ctx, "analyzer-0", 5); err != nil || len(c.Items) != 0 ||
		c.Idle != "outside captions.backfill.window (02:00-07:00 UTC)" || c.RetryAfterSeconds != 18*3600 {
		t.Errorf("a claim at 08:00 UTC: %+v, %v", c, err)
	}
}

// With no setting of its window, the backfill hands out at any hour of the
// day; once a window is set, it hands out inside it alone, as before.
func TestTheCaptionsBackfillHandsOutAtAnyHourByDefault(t *testing.T) {
	ctx := context.Background()
	st := storetest.Open(t)
	dir := t.TempDir()
	for h := 0; h < 24; h++ {
		id := fmt.Sprintf("h%02d", h)
		media := filepath.Join(dir, id+".mkv")
		if err := os.WriteFile(media, []byte("a film"), 0o644); err != nil {
			t.Fatal(err)
		}
		storetest.AddItem(t, st, id, "movie", "Film "+id, "")
		storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_itemsources (id, item_id, filename, arrivalpath, sizebytes, state,
			recordedat) VALUES ($1 || '-src', $1, $1 || '.mkv', $2, 6, 'present', now())`, id, media)
	}
	if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_settings WHERE key = 'captions.backfill.window'`); n != 0 {
		t.Fatalf("%d window settings", n)
	}
	for h := 0; h < 24; h++ {
		at := time.Date(2026, 11, 16, h, 30, 0, 0, time.UTC)
		c, err := New(st).WithClock(func() time.Time { return at }).Claim(ctx, "analyzer-0", 1)
		if err != nil || len(c.Items) != 1 {
			t.Errorf("a claim at %02d:30 UTC with no window set: %+v, %v", h, c, err)
		}
		storetest.Exec(t, st, `UPDATE com_nalet_katalog_sourcecaptions SET claimedat = NULL, claimtoken = NULL`)
	}
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_settings (id, key, valuetext)
		VALUES ('w', 'captions.backfill.window', '02:00-07:00')`)
	for _, c := range []struct {
		hour  int
		hands bool
	}{{1, false}, {3, true}, {6, true}, {7, false}, {12, false}} {
		at := time.Date(2026, 11, 17, c.hour, 30, 0, 0, time.UTC)
		cl, err := New(st).WithClock(func() time.Time { return at }).Claim(ctx, "analyzer-0", 1)
		if err != nil || (len(cl.Items) == 1) != c.hands {
			t.Errorf("a claim at %02d:30 UTC in a window of 02:00-07:00: %+v, %v; want it handed out %v", c.hour, cl, err,
				c.hands)
		}
		storetest.Exec(t, st, `UPDATE com_nalet_katalog_sourcecaptions SET claimedat = NULL, claimtoken = NULL`)
	}
}

// orderFixture is three originals on disk, each of its own film, taken in
// oldest first: one whose film has no complete version ("unpackaged"), one
// whose film has ("packaged"), and one whose film has and whose retirement
// the retire job notes waits on its captions alone ("waiting").
func orderFixture(t *testing.T, inflight int) (*store.Store, map[string]string) {
	t.Helper()
	st := storetest.Open(t)
	dir := t.TempDir()
	ids := map[string]string{}
	for i, name := range []string{"unpackaged", "packaged", "waiting"} {
		item, source := "o"+strconv.Itoa(i), "o"+strconv.Itoa(i)+"-src"
		ids[source] = name
		media := filepath.Join(dir, name+".mkv")
		if err := os.WriteFile(media, []byte("a film"), 0o644); err != nil {
			t.Fatal(err)
		}
		storetest.AddItem(t, st, item, "movie", "Film "+item, "")
		storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_itemsources (id, item_id, filename, arrivalpath, sizebytes, state,
			createdat, recordedat) VALUES ($1, $2, $3, $4, 6, 'present', now() - interval '3 days' + $5 * interval '1 day',
			now())`, source, item, name+".mkv", media, i)
		if name != "unpackaged" {
			storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_itemversions (id, item_id, sourceids, state, dir, completedat)
				VALUES ($1 || '-ver', $1, ARRAY[$2::varchar], 'complete', $3, now())`, item, source, filepath.Join(dir, item))
		}
		if name == "waiting" {
			storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_sourcecaptions (source_id, item_id, state, retirewaitingsince)
				VALUES ($1, $2, 'pending', now())`, source, item)
		}
	}
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_settings (id, key, valuetext)
		VALUES ('i', 'captions.backfill.inflight', $1)`, strconv.Itoa(inflight))
	return st, ids
}

// names are the originals of c by what they are.
func names(c Claim, ids map[string]string) []string {
	var out []string
	for _, j := range c.Items {
		out = append(out, ids[j.SourceID])
	}
	return out
}

// The backfill hands out first the originals whose retirement waits on their
// captions alone (the retire job's note), then those of titles with a
// complete version, then the rest, though the waiting one was taken in last
// and the unpackaged one first.
func TestTheCaptionsBackfillHandsOutWhatARetirementWaitsOnFirst(t *testing.T) {
	st, ids := orderFixture(t, 3)
	at := time.Date(2026, 11, 16, 23, 0, 0, 0, time.UTC)
	c, err := New(st).WithClock(func() time.Time { return at }).Claim(context.Background(), "analyzer-0", 3)
	if got := names(c, ids); err != nil || strings.Join(got, " ") != "waiting packaged unpackaged" {
		t.Errorf("the claim: %v, %v; want waiting, packaged, unpackaged", got, err)
	}
}

// The order is the catalog's, as the leases and the backoffs are: a
// backfill started anew (each claim here a new one, as after a restart) goes
// on with it, a claim's lease holds its original until it ends, and an
// original whose lease ended waits out its backoff while those after it go;
// once the backoff is over, the waiting one goes first again.
func TestTheCaptionsBackfillsOrderHoldsAcrossRestartsLeasesAndBackoffs(t *testing.T) {
	ctx := context.Background()
	st, ids := orderFixture(t, 1)
	t0 := time.Date(2026, 11, 16, 23, 0, 0, 0, time.UTC)
	claim := func(at time.Time) []string {
		t.Helper()
		c, err := New(st).WithClock(func() time.Time { return at }).Claim(ctx, "analyzer-0", 3)
		if err != nil {
			t.Fatal(err)
		}
		return names(c, ids)
	}
	if got := claim(t0); strings.Join(got, " ") != "waiting" {
		t.Fatalf("the first claim: %v", got)
	}
	if got := claim(t0.Add(time.Minute)); len(got) != 0 {
		t.Errorf("a claim within the lease, started anew: %v; want none (inflight 1)", got)
	}
	// the lease ends: the waiting one waits its backoff, the packaged one goes
	t1 := t0.Add(DefaultLease + time.Minute)
	if got := claim(t1); strings.Join(got, " ") != "packaged" {
		t.Errorf("the claim after the lease: %v; want packaged", got)
	}
	r, err := Of(ctx, st.Pool(), "o2-src")
	if err != nil || r.Failures != 1 || r.NextAttemptAt == nil || !r.NextAttemptAt.Equal(t1.Add(FirstBackoff)) {
		t.Fatalf("the waiting original after its lease: %+v, %v", r, err)
	}
	// the packaged one given back, waiting out its release; the unpackaged
	// one goes
	var token string
	if err := st.Pool().QueryRow(ctx, `SELECT claimtoken FROM com_nalet_katalog_sourcecaptions WHERE source_id = 'o1-src'`).
		Scan(&token); err != nil {
		t.Fatal(err)
	}
	if _, err := New(st).WithClock(func() time.Time { return t1 }).Record(ctx, "o1-src",
		Report{Token: token, Released: true}); err != nil {
		t.Fatal(err)
	}
	if got := claim(t1.Add(time.Minute)); strings.Join(got, " ") != "unpackaged" {
		t.Errorf("the claim with the waiting one in its backoff and the packaged one given back: %v; want unpackaged", got)
	}
	// past the waiting one's backoff (the unpackaged one's lease over): it
	// goes first again
	if got := claim(t1.Add(FirstBackoff + time.Minute)); strings.Join(got, " ") != "waiting" {
		t.Errorf("the claim past the backoff: %v; want waiting", got)
	}
}
