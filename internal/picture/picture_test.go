package picture

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func i32(v int32) *int32 { return &v }

// A report is taken as the analyzer means it: an area within its frame is a
// measurement, a frame without one could not tell, an error a failure, a
// release nothing else; anything else is 400 and says why.
func TestAReportIsReadAsTheAnalyzerMeansIt(t *testing.T) {
	frame := &Size{Width: 1920, Height: 1080}
	for _, tc := range []struct {
		name string
		r    Report
		want string // the state, or what the refusal says
	}{
		{"a letterbox", Report{Path: "/f.mkv", Frame: frame, Area: &Rect{0, 138, 1920, 804}}, StateMeasured},
		{"the whole frame", Report{Path: "/f.mkv", Frame: frame, Area: &Rect{0, 0, 1920, 1080}}, StateMeasured},
		{"could not tell", Report{Path: "/f.mkv", Frame: frame, Reason: "too few frames"}, StateUnknown},
		{"could not read it", Report{Path: "/f.mkv", Error: "ffprobe could not read the file"}, StateFailed},
		{"given back", Report{Released: true}, ""},
		{"no file", Report{Frame: frame, Area: &Rect{0, 0, 1, 1}}, "names the file"},
		{"no frame", Report{Path: "/f.mkv", Area: &Rect{0, 0, 1, 1}}, "names the frame"},
		{"outside the frame", Report{Path: "/f.mkv", Frame: frame, Area: &Rect{0, 138, 1920, 1000}}, "does not lie within"},
		{"a negative edge", Report{Path: "/f.mkv", Frame: frame, Area: &Rect{-2, 0, 1920, 1080}}, "does not lie within"},
		{"sides of the wrong kind", Report{Path: "/f.mkv", Frame: frame, Sides: json.RawMessage(`[1]`)}, "sides is a JSON object"},
	} {
		got, err := tc.r.state()
		if err != nil {
			got = err.Error()
		}
		if !strings.Contains(got, tc.want) || (tc.want == "" && got != "") {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
}

// A failure waits an hour, twice as long after each in a row, a week at most.
func TestAFailureWaitsItsBackoff(t *testing.T) {
	for failures, want := range map[int32]time.Duration{1: time.Hour, 2: 2 * time.Hour, 4: 8 * time.Hour,
		8: 128 * time.Hour, 9: MaxBackoff, 40: MaxBackoff} {
		if got := backoff(failures); got != want {
			t.Errorf("after %d failures: %s, want %s", failures, got, want)
		}
	}
}

// The window is UTC, across midnight too: 22:00-17:00 never holds from
// 17:00 to 22:00, closes at 17:00 and opens again at 22:00.
func TestTheWindowIsUTC(t *testing.T) {
	w, err := ParseWindow("22:00-17:00")
	if err != nil {
		t.Fatal(err)
	}
	at := func(s string) time.Time {
		v, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	for when, in := range map[string]bool{"2026-11-03T16:59:00Z": true, "2026-11-03T17:00:00Z": false,
		"2026-11-03T21:59:00Z": false, "2026-11-03T22:00:00Z": true, "2026-11-03T03:00:00Z": true,
		// a time of another zone is read in UTC: 17:30+01:00 is 16:30 UTC
		"2026-11-03T17:30:00+01:00": true, "2026-11-03T18:30:00+01:00": false} {
		if got := w.Contains(at(when)); got != in {
			t.Errorf("%s: in the window %v, want %v", when, got, in)
		}
	}
	if got := w.Closes(at("2026-11-03T03:00:00Z")); !got.Equal(at("2026-11-03T17:00:00Z")) {
		t.Errorf("closes %s, want 17:00 the same day", got)
	}
	if got := w.Closes(at("2026-11-03T23:00:00Z")); !got.Equal(at("2026-11-04T17:00:00Z")) {
		t.Errorf("closes %s, want 17:00 the next day", got)
	}
	if got := w.Opens(at("2026-11-03T18:00:00Z")); !got.Equal(at("2026-11-03T22:00:00Z")) {
		t.Errorf("opens %s, want 22:00", got)
	}
	if w.String() != "22:00-17:00 UTC" {
		t.Errorf("%q", w.String())
	}
	if any, _ := ParseWindow(""); !any.Contains(at("2026-11-03T19:00:00Z")) || !any.Closes(at("2026-11-03T19:00:00Z")).IsZero() {
		t.Error("no window is any time, and never closes")
	}
	if _, err := ParseWindow("10pm-5pm"); err == nil {
		t.Error("10pm-5pm read as a window")
	}
}

// The settings are their defaults unless set; one that can't be read is its
// default and says so, a window that can't be read hands out nothing.
func TestTheBackfillsSettings(t *testing.T) {
	s := DefaultSettings()
	if s.On || s.Inflight != 1 || s.Rate != 240 || s.Lease != 30*time.Minute || s.Window.String() != "22:00-17:00 UTC" {
		t.Fatalf("defaults %+v", s)
	}
	s.set(SettingBackfill, " on ")
	s.set(SettingInflight, "2")
	s.set(SettingRate, "60")
	s.set(SettingLease, "1h")
	if !s.On || s.Inflight != 2 || s.Rate != 60 || s.Lease != time.Hour || len(s.Problems) != 0 {
		t.Fatalf("set: %+v", s)
	}
	s.set(SettingInflight, "none")
	s.set(SettingLease, "5s")
	s.set(SettingWindow, "nights")
	if s.Inflight != 2 || s.Lease != time.Hour || len(s.Problems) != 3 {
		t.Errorf("unreadable values: %+v", s)
	}
	if s.Window.Contains(time.Now()) {
		t.Error("a window that can't be read holds")
	}
	off := DefaultSettings()
	off.set(SettingBackfill, "yes")
	if off.On || len(off.Problems) != 1 {
		t.Errorf("yes: %+v, want off and a problem", off)
	}
}

func TestClipCutsAtACharactersStart(t *testing.T) {
	if got := clip("ab€cd", 4); got != "ab" {
		t.Errorf("%q", got)
	}
	if got := clip("abcd", 3); got != "abc" {
		t.Errorf("%q", got)
	}
}

func TestAFullAreaIsTheFrame(t *testing.T) {
	a := &Area{State: StateMeasured, FrameWidth: i32(1920), FrameHeight: i32(1080), X: i32(0), Y: i32(0),
		Width: i32(1920), Height: i32(1080)}
	if !a.Full() {
		t.Error("the frame itself is not full")
	}
	a.Y, a.Height = i32(138), i32(804)
	if a.Full() {
		t.Error("a letterbox is full")
	}
}
