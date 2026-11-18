package library

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/zaentrum/katalog-manager/internal/store/storetest"
)

// gopRead is read of a ten-minute video's windows ws, of a read that found
// the gop gop (nil: none said).
func gopRead(gop *float64, ws ...CaptionWindow) GateFacts {
	f := read(600, ws...)
	f.Captions.Gop = gop
	return f
}

// A window lies at its place by the plan's slack: its first frame within
// the greater of 10 s and the read's gop and 0.5 s of it, before or after;
// up to 30 s before it where the read says it starts at the keyframe
// before it (keyframeBefore), never after it so; of a gop of 0, or none
// said, only at its own place, within a frame step and 0.5 s, keyframeBefore
// or not. A gop that is no length from 0 says nothing.
func TestAWindowLiesAtItsPlaceByThePlansSlack(t *testing.T) {
	gop := func(g float64) *float64 { return &g }
	for _, c := range []struct {
		name           string
		gop            *float64
		off            float64 // how far after its place the window at 50 % starts (before: below 0)
		keyframeBefore bool
		inPlace        bool
	}{
		{"a gop of 2 s, 9.9 s before", gop(2), -9.9, false, true},
		{"a gop of 2 s, 10.1 s before", gop(2), -10.1, false, false},
		{"a gop of 2 s, 9.9 s after", gop(2), 9.9, false, true},
		{"a gop of 2 s, 10.1 s after", gop(2), 10.1, false, false},
		{"a gop of 12 s, 12.4 s before", gop(12), -12.4, false, true},
		{"a gop of 12 s, 12.6 s before", gop(12), -12.6, false, false},
		{"a gop of 12 s, 12.4 s after", gop(12), 12.4, false, true},
		{"a gop of 12 s, 12.6 s after", gop(12), 12.6, false, false},
		{"the keyframe before, 29.9 s before", gop(2), -29.9, true, true},
		{"the keyframe before, 30.1 s before", gop(2), -30.1, true, false},
		{"the keyframe before said, 15 s after", gop(2), 15, true, false},
		{"a gop of 0, 0.5 s before", gop(0), -0.5, false, true},
		{"a gop of 0, 0.6 s before", gop(0), -0.6, false, false},
		{"a gop of 0, 0.6 s after", gop(0), 0.6, false, false},
		{"a gop of 0, the keyframe before, 20 s before", gop(0), -20, true, false},
		{"no gop said, 0.5 s after", nil, 0.5, false, true},
		{"no gop said, 5 s before", nil, -5, false, false},
	} {
		ws := every()
		ws[3].Start += c.off
		ws[3].End += c.off
		ws[3].KeyframeBefore = c.keyframeBefore
		carried, unknown := captionsRule([]Doc{film(600)}, planProbes, gopRead(c.gop, ws...))
		off := strings.Contains(unknown, "its windows at 50% lie off their places")
		if carried != "" || off == c.inPlace || (c.inPlace && unknown != "") {
			t.Errorf("%s: carried %q, unknown %q; want in place %v", c.name, carried, unknown, c.inPlace)
		}
	}
	for _, g := range []float64{-1, -0.001} {
		if _, unknown := captionsRule([]Doc{film(600)}, planProbes, gopRead(&g, every()...)); !strings.Contains(unknown,
			"its gop is no length from 0") {
			t.Errorf("a gop of %g: unknown %q; want it not known", g, unknown)
		}
	}
}

// keyframeBefore of a window that decoded frames is true, false or not
// given (false): null or any other value fails the window, which keeps what
// it found; a window that decoded none (which the analyzer sends with null)
// fails as it is; a read of the whole picture that says it fails.
func TestAWindowsKeyframeBeforeIsABooleanOrNotGiven(t *testing.T) {
	decoded := func(kb string) string {
		return `{"at": 0.5, "start": 300, "end": 301.88, "frames": 48, "captions": true, "interlacedFrames": 0, ` +
			`"repeatFrames": 0` + kb + `}`
	}
	for _, c := range []struct {
		name, js string
		fails    bool
		keyed    bool
	}{
		{"not given", decoded(""), false, false},
		{"false", decoded(`, "keyframeBefore": false`), false, false},
		{"true", decoded(`, "keyframeBefore": true`), false, true},
		{"null", decoded(`, "keyframeBefore": null`), true, false},
		{"a string", decoded(`, "keyframeBefore": "true"`), true, false},
		{"a number", decoded(`, "keyframeBefore": 1`), true, false},
		{"in another case, null", decoded(`, "KeyframeBefore": null`), true, false},
		{"a whole read, true", `{"at": 0, "start": 0, "end": 39.958, "frames": 960, "packets": 960, "captions": false, ` +
			`"interlacedFrames": 0, "repeatFrames": 0, "whole": true, "keyframeBefore": true}`, true, false},
		{"a whole read, false", `{"at": 0, "start": 0, "end": 39.958, "frames": 960, "packets": 960, "captions": false, ` +
			`"interlacedFrames": 0, "repeatFrames": 0, "whole": true, "keyframeBefore": false}`, false, false},
	} {
		var w CaptionWindow
		if err := json.Unmarshal([]byte(c.js), &w); err != nil {
			t.Fatal(err)
		}
		failed := w.Unreadable != "" && strings.Contains(w.Unreadable, "keyframeBefore")
		switch {
		case failed != c.fails || w.KeyframeBefore != c.keyed:
			t.Errorf("%s: %+v; want failed %v, keyframeBefore %v", c.name, w, c.fails, c.keyed)
		case c.fails && !strings.Contains(c.name, "whole") && !w.Captions:
			t.Errorf("%s: %+v; want the captions it showed kept", c.name, w)
		}
	}
	var w CaptionWindow
	if err := json.Unmarshal([]byte(`{"at": 0.3, "start": null, "end": null, "frames": 0, "captions": null,
		"interlacedFrames": null, "repeatFrames": null, "keyframeBefore": null}`), &w); err != nil {
		t.Fatal(err)
	}
	if w.Frames != 0 || w.Unreadable != "" || w.KeyframeBefore {
		t.Errorf("a window that decoded none, keyframeBefore null: %+v; want a failed window as it is", w)
	}
}

// On the retire path: the stored read's gop lays its windows (a gop of 12 s
// takes a window 12.4 s from its place, not 12.6 s; the keyframe before
// takes one 29.9 s before it, not 30.1 s; a read that said no gop takes
// none 1 s off), and a window whose keyframeBefore is null keeps the
// original.
func TestTheRetireJobLaysWindowsByThePlansSlack(t *testing.T) {
	const fifty = `{"at": 0.5, "start": 300, "end": 301.88, "frames": 48, "captions": false, "interlacedFrames": 0, ` +
		`"repeatFrames": 0}`
	moved := func(window string, gop *float64) func(f *retireFixture) {
		return func(f *retireFixture) {
			storetest.Exec(t, f.st, `UPDATE com_nalet_katalog_sourcecaptions SET captionwindows = $2::jsonb, readgop = $3
				WHERE source_id = $1`, rtSource, strings.Replace(noCaptionsWindows, fifty, window, 1), gop)
		}
	}
	gop := func(g float64) *float64 { return &g }
	// the window at 50 % starting at start, keyframeBefore as kb says it
	at := func(start float64, kb string) string {
		return strings.TrimSuffix(strings.NewReplacer(`"start": 300`, `"start": `+ftoa(start), `"end": 301.88`,
			`"end": `+ftoa(start+1.88)).Replace(fifty), "}") + kb + "}"
	}
	for _, c := range []struct {
		name   string
		change func(f *retireFixture)
		kept   string
	}{
		{"a gop of 12 s, 12.4 s before", moved(at(287.6, ""), gop(12)), ""},
		{"a gop of 12 s, 12.6 s before", moved(at(287.4, ""), gop(12)), "lie off their places"},
		{"the keyframe before, 29.9 s before", moved(at(270.1, `, "keyframeBefore": true`), gop(2)), ""},
		{"the keyframe before, 30.1 s before", moved(at(269.9, `, "keyframeBefore": true`), gop(2)),
			"lie off their places"},
		{"no gop said, 1 s after", moved(at(301, ""), nil), "lie off their places"},
		{"keyframeBefore null", moved(at(300, `, "keyframeBefore": null`), gop(2)), CaptionsUnknown},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newRetireFixtureOf(t, sameEssence, sameEssence)
			c.change(f)
			if c.kept == "" {
				f.deletedBy(t, c.name)
				return
			}
			f.keptFor(t, c.name, c.kept)
		})
	}
}

// ftoa is f as a JSON number.
func ftoa(f float64) string {
	b, _ := json.Marshal(f)
	return string(b)
}
