package chapters

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
	"time"
)

func str(s string) *string { return &s }

func i32(v int32) *int32 { return &v }

// A chapter's title is kept as its column holds it: no NUL (no text column
// holds one) nor other control characters, at most 120 characters, none when
// nothing is left.
func TestAChaptersTitleIsKeptAsItsColumnHoldsIt(t *testing.T) {
	long := strings.Repeat("é", TitleMax+5)
	for _, tc := range []struct {
		in   *string
		want *string
	}{
		{nil, nil},
		{str(""), nil},
		{str("   "), nil},
		{str("\x00\x01"), nil},
		{str("Act\x001"), str("Act1")},
		{str("Act\t1\nCold Open"), str("Act 1 Cold Open")},
		{str("  Opening  "), str("Opening")},
		{&long, str(strings.Repeat("é", TitleMax))},
		{str("bad \xff byte"), str("bad  byte")},
	} {
		got := CleanTitle(tc.in)
		if (got == nil) != (tc.want == nil) || (got != nil && *got != *tc.want) {
			t.Errorf("CleanTitle(%q) = %v, want %v", deref(tc.in), deref(got), deref(tc.want))
		}
	}
}

func deref(s *string) string {
	if s == nil {
		return "<nil>"
	}
	return *s
}

// A report says what it leaves its title in: a read that found chapters, one
// that found none, one that failed, a claim given back; one that can't be
// taken says why.
func TestAReportSaysWhatItLeavesItsTitleIn(t *testing.T) {
	ch := []Chapter{{StartMs: 0, EndMs: 1000, Ordinal: 1}}
	for _, tc := range []struct {
		name string
		r    Report
		want string
		bad  string
	}{
		{"chapters found", Report{Read: ReadReport{Path: "/m.mkv", Found: i32(1)}, Chapters: ch}, StateRead, ""},
		{"none found", Report{Read: ReadReport{Path: "/m.mkv", Found: i32(0)}, Chapters: []Chapter{}}, StateNone, ""},
		{"marks without a length", Report{Read: ReadReport{Path: "/m.mkv", Found: i32(2)}, Chapters: []Chapter{}}, StateNone, ""},
		{"failed", Report{Read: ReadReport{Path: "/m.mkv", Error: "ffprobe did not answer within 120 s"}}, StateFailed, ""},
		{"released", Report{Released: true, Token: "t"}, "", ""},
		{"no path", Report{Chapters: ch}, "", "read.path"},
		{"neither chapters nor an error", Report{Read: ReadReport{Path: "/m.mkv"}}, "", "chapters, [] for none"},
		{"failed with chapters", Report{Read: ReadReport{Path: "/m.mkv", Error: "x"}, Chapters: ch}, "", "found no chapters"},
		{"a chapter of no length", Report{Read: ReadReport{Path: "/m.mkv"}, Chapters: []Chapter{{StartMs: 5, EndMs: 5}}}, "",
			"of chapter 1"},
		{"a negative start", Report{Read: ReadReport{Path: "/m.mkv"}, Chapters: []Chapter{{StartMs: -1, EndMs: 5}}}, "",
			"need 0 <= start < end"},
		{"found below 0", Report{Read: ReadReport{Path: "/m.mkv", Found: i32(-1)}, Chapters: ch}, "", "read.found"},
	} {
		got, err := tc.r.state()
		switch {
		case tc.bad != "":
			if err == nil || !strings.Contains(err.Error(), tc.bad) {
				t.Errorf("%s: %q, %v; want it refused saying %q", tc.name, got, err, tc.bad)
			}
		case err != nil || got != tc.want:
			t.Errorf("%s: %q, %v; want %q", tc.name, got, err, tc.want)
		}
	}
}

// A failure waits an hour, twice as long after each in a row, a day at most:
// the fifth, which settles the title as unreadable, comes 15 hours after the
// first at the earliest.
func TestAFailureWaitsItsBackoff(t *testing.T) {
	want := []time.Duration{time.Hour, 2 * time.Hour, 4 * time.Hour, 8 * time.Hour, 16 * time.Hour, 24 * time.Hour, 24 * time.Hour}
	var sum time.Duration
	for i, w := range want {
		if got := backoff(int32(i + 1)); got != w {
			t.Errorf("after failure %d: %v, want %v", i+1, got, w)
		}
		if i+1 < MaxFailures {
			sum += w
		}
	}
	if sum != 15*time.Hour {
		t.Errorf("the reads before the %dth failure span %v, want 15h", MaxFailures, sum)
	}
}

// The backfill's settings: off, 22:00-17:00 UTC, one at a time, 120 an hour
// and a lease of 30 minutes unless set; a value that can't be read is its
// default and said so, and a window that can't be read hands out nothing.
func TestTheBackfillsSettings(t *testing.T) {
	s := DefaultSettings()
	if s.On || s.Window.String() != "22:00-17:00 UTC" || s.Inflight != 1 || s.Rate != 120 || s.Records != 3600 ||
		s.Lease != 30*time.Minute {
		t.Fatalf("defaults: %+v", s)
	}
	for _, at := range []struct {
		hour int
		in   bool
	}{{16, true}, {17, false}, {21, false}, {22, true}, {3, true}} {
		if got := s.Window.Contains(time.Date(2026, 11, 4, at.hour, 30, 0, 0, time.UTC)); got != at.in {
			t.Errorf("%02d:30 UTC in the window: %v, want %v", at.hour, got, at.in)
		}
	}
	s.set(SettingBackfill, " ON ")
	s.set(SettingInflight, "3")
	s.set(SettingRate, "0")
	s.set(SettingRecords, "0")
	s.set(SettingLease, "2h")
	if !s.On || s.Inflight != 3 || s.Rate != 0 || s.Records != 0 || s.Lease != 2*time.Hour || len(s.Problems) != 0 {
		t.Errorf("set: %+v", s)
	}
	s = DefaultSettings()
	s.set(SettingBackfill, "yes")
	s.set(SettingInflight, "0")
	s.set(SettingRate, "-1")
	s.set(SettingRecords, "many")
	s.set(SettingLease, "10s")
	s.set(SettingWindow, "evenings")
	if s.On || s.Inflight != 1 || s.Rate != 120 || s.Records != 3600 || s.Lease != 30*time.Minute || len(s.Problems) != 6 {
		t.Errorf("unreadable: %+v", s)
	}
	if !s.Window.Never || s.Window.Contains(time.Date(2026, 11, 4, 3, 0, 0, 0, time.UTC)) ||
		!strings.HasPrefix(s.Window.String(), "never") || !s.Window.Closes(time.Now()).IsZero() {
		t.Errorf("a window that can't be read: %+v %s", s.Window, s.Window.String())
	}
	s = DefaultSettings()
	s.set(SettingWindow, "")
	if !s.Window.Contains(time.Date(2026, 11, 4, 18, 0, 0, 0, time.UTC)) || s.Window.String() != "any time" {
		t.Errorf("no window: %s", s.Window.String())
	}
}

// A read takes the chapters in the catalog so: its chapters replace those of
// another original, and those of no original known when they are as many or
// more; chapters that are its own are kept; fewer, or none, than those of no
// original known, and fewer than an earlier read of the same original wrote,
// hold the title; a none takes those of another original away, and leaves an
// earlier read's of the same original.
func TestHowAReadTakesTheChaptersInTheCatalog(t *testing.T) {
	s1, s2 := str("s1"), str("s2")
	three := []Chapter{{StartMs: 0, EndMs: 10, Ordinal: 1}, {StartMs: 10, EndMs: 20, Ordinal: 2},
		{StartMs: 20, EndMs: 30, Ordinal: 3}}
	four := append(append([]Chapter{}, three...), Chapter{StartMs: 30, EndMs: 40, Ordinal: 4})
	other := []Chapter{{StartMs: 5, EndMs: 10, Ordinal: 1}, {StartMs: 10, EndMs: 20, Ordinal: 2},
		{StartMs: 20, EndMs: 30, Ordinal: 3}}
	for _, tc := range []struct {
		name      string
		have, got []Chapter
		rows      *string
		want      int
	}{
		{"none in the catalog", nil, three, nil, takeWrite},
		{"its own, of no original known", three, three, nil, takeKeep},
		{"its own, of another original", three, three, s2, takeKeep},
		{"as many, of no original known", three, other, nil, takeWrite},
		{"more, of no original known", three, four, nil, takeWrite},
		{"fewer, of no original known", four, three, nil, takeHold},
		{"fewer, of another original", four, three, s2, takeWrite},
		{"fewer, of an earlier read of the same", four, three, s1, takeHold},
		{"more, of an earlier read of the same", three, four, s1, takeWrite},
		{"none, none in the catalog", nil, nil, nil, takeNone},
		{"none, of no original known", three, nil, nil, takeHold},
		{"none, of another original", three, nil, s2, takeNone},
		{"none, of an earlier read of the same", three, nil, s1, takeStay},
	} {
		if got, why := take(tc.have, tc.got, tc.rows, s1); got != tc.want || (got == takeHold) != (why != "") {
			t.Errorf("%s: %d %q, want %d", tc.name, got, why, tc.want)
		}
	}
}

// A source record is taken only when its source.json names its probe (the
// sha256 of its bytes) and an original of the size the catalog holds, and
// the probe says that size where it says one.
func TestARecordIsVerifiedAgainstItsSourceJSON(t *testing.T) {
	probe := []byte(`{"chapters": [], "format": {"duration": "10.0", "size": "6"}}`)
	sum := sha256.Sum256(probe)
	named := "sha256:" + hex.EncodeToString(sum[:])
	six, seven := int64(6), int64(7)
	for _, tc := range []struct {
		name, src string
		size      *int64
		why       string
	}{
		{"its own", `{"file": {"sizeBytes": 6}, "probe": {"sha256": "` + named + `"}}`, &six, ""},
		{"a size the catalog does not know", `{"file": {"sizeBytes": 6}, "probe": {"sha256": "` + named + `"}}`, nil, ""},
		{"another probe", `{"file": {"sizeBytes": 6}, "probe": {"sha256": "sha256:00"}}`, &six, "not the one its source.json names"},
		{"no probe named", `{"file": {"sizeBytes": 6}}`, &six, "not the one its source.json names"},
		{"another original", `{"file": {"sizeBytes": 6}, "probe": {"sha256": "` + named + `"}}`, &seven,
			"of a file of 6 bytes, the catalog's original of 7"},
		{"no size", `{"probe": {"sha256": "` + named + `"}}`, &six, "names no size"},
		{"the probe of another size", `{"file": {"sizeBytes": 7}, "probe": {"sha256": "` + named + `"}}`, &seven,
			"its probe is of a file of 6 bytes, its source.json of 7"},
		{"no record", `[`, &six, "is no record"},
	} {
		if got := verify(probe, []byte(tc.src), tc.size); (tc.why == "") != (got == "") || !strings.Contains(got, tc.why) {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.why)
		}
	}
}
