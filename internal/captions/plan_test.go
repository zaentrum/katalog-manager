package captions

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zaentrum/katalog-manager/internal/library"
	"github.com/zaentrum/katalog-manager/internal/store/storetest"
)

// claimedOf is claimed of an original whose source record is record.
func claimedOf(t *testing.T, record string) (*Service, string, string) {
	t.Helper()
	st := storetest.Open(t)
	dir := t.TempDir()
	media := filepath.Join(dir, "film.mkv")
	if err := os.WriteFile(media, []byte("a film"), 0o644); err != nil {
		t.Fatal(err)
	}
	rec := writeRecord(t, dir, record)
	storetest.AddItem(t, st, "r1", "movie", "A Film", "")
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_itemsources (id, item_id, filename, arrivalpath, sizebytes, state,
		recordedat, recorddir) VALUES ('r1-src', 'r1', 'film.mkv', $1, 6, 'present', now(), $2)`, media, rec)
	svc := New(st).WithClock(at(time.Date(2026, 11, 16, 23, 0, 0, 0, time.UTC)))
	c, err := svc.Claim(context.Background(), "analyzer-0", 1)
	if err != nil || len(c.Items) != 1 {
		t.Fatalf("the claim: %+v, %v", c, err)
	}
	return svc, "r1-src", c.Items[0].Token
}

// fortySeconds is the source record of a 40 s film.
const fortySeconds = `{"schema": "zaentrum.library.source/2", "essence": {}, "container": {"format": "matroska,webm", "durationMs": 40000},
	"streams": [{"index": 0, "type": "video", "codec": "h264", "fieldOrder": "progressive"}]}`

// wholeBody is a report of a read of the whole picture of a 40 s film, every
// packet but those before its first keyframe (leading; below 0: none said)
// decoded (frames of 960), the span it read from 0 to end.
func wholeBody(frames, leading int, end string) string {
	l := ""
	if leading >= 0 {
		l = `, "leading": ` + itoa(leading)
	}
	return `{"captionWindows": [{"at": 0, "start": 0, "end": 39.958, "frames": ` + itoa(frames) + `, "packets": 960` + l +
		`, "captions": false, "interlacedFrames": 0, "repeatFrames": 0, "whole": true}], "duration": 40, "span": {"start": 0, "end": ` + end +
		`}, "sizeBytes": 6, "ffprobe": "7.1.5"}`
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for ; n > 0; n /= 10 {
		b = append([]byte{byte('0' + n%10)}, b...)
	}
	return string(b)
}

// The PUT holds a read to the plan's details: a short picture's read of the
// whole of it, every packet after its first keyframe decoded, is a read in
// full, not handed out again; one a packet short, a short picture read in
// windows, and a read whose span is not its picture's (by more than a frame
// step and half a second), or says none, are failed reads, each waiting its
// backoff; a long picture read whole is a read too.
func TestThePutHoldsAReadToThePlansDetails(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		name, record, body string
		read               bool
		why                string
	}{
		{"40 s read whole", fortySeconds, wholeBody(960, 0, "40"), true, ""},
		{"40 s read whole, leading 3: three pictures before its first keyframe not decoded", fortySeconds,
			wholeBody(957, 3, "40"), true, ""},
		{"40 s read whole, leading 3, its decoder outputting them too", fortySeconds, wholeBody(960, 3, "40"), true, ""},
		{"40 s read whole, leading 3, a packet more short", fortySeconds, wholeBody(956, 3, "40"), false,
			"not every packet"},
		{"40 s read whole, a packet short", fortySeconds, wholeBody(959, 0, "40"), false, "not every packet"},
		{"40 s read whole, no leading said", fortySeconds, wholeBody(960, -1, "40"), false, "says no leading pictures"},
		{"40 s read whole, leading 17", fortySeconds, wholeBody(943, 17, "40"), false, "decoded no frame it could place"},
		{"40 s read in windows", fortySeconds, strings.Replace(fullRead, `"duration": 600, "span": {"start": 0, "end": 600}`,
			`"duration": 40, "span": {"start": 0, "end": 40}`, 1), false, "is read whole"},
		{"a span's end off by 0.6 s", tenMinuteRecord, strings.Replace(fullRead, `"span": {"start": 0, "end": 600}`,
			`"span": {"start": 0, "end": 600.6}`, 1), false, "it read a picture from 0 s to 600.6 s"},
		{"a span shifted by 0.6 s", tenMinuteRecord, strings.Replace(fullRead, `"span": {"start": 0, "end": 600}`,
			`"span": {"start": 0.6, "end": 600.6}`, 1), false, "it read a picture from 0.6 s to 600.6 s"},
		{"a span off by 0.5 s at each end", tenMinuteRecord, strings.Replace(fullRead, `"span": {"start": 0, "end": 600}`,
			`"span": {"start": 0.5, "end": 599.5}`, 1), true, ""},
		{"no span", tenMinuteRecord, strings.Replace(fullRead, ` "span": {"start": 0, "end": 600},`, ``, 1), false,
			"says no span"},
		{"600 s read whole", tenMinuteRecord, strings.NewReplacer(`"end": 39.958, "frames": 960, "packets": 960`,
			`"end": 599.958, "frames": 14400, "packets": 14400`, `"duration": 40, "span": {"start": 0, "end": 40}`,
			`"duration": 600, "span": {"start": 0, "end": 600}`).Replace(wholeBody(960, 0, "40")), true, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			svc, source, token := claimedOf(t, c.record)
			r := record(t, ctx, svc, source, token, c.body)
			switch {
			case c.read && (r.State != StateRead || r.Failures != 0 || r.NextAttemptAt != nil):
				t.Errorf("the read: %+v; want a read in full", r)
			case !c.read && (r.State == StateRead && r.Reason == nil || r.Failures != 1 || r.Reason == nil ||
				!strings.Contains(*r.Reason, c.why)):
				t.Errorf("the read: %+v (%s); want a failed one, %q, waiting its backoff", r, deref(r.Reason), c.why)
			}
			if cl, err := svc.Claim(ctx, "analyzer-0", 1); err != nil || len(cl.Items) != 0 {
				t.Errorf("handed out again a moment later: %+v, %v", cl, err)
			}
		})
	}
}

// A read that says its HEVC picture's captions can't be told without its
// packets' signature (captionsKnown false) is kept as the finding
// unscanned: no backoff, not handed out again while its record is as it was;
// once the record is written again (with its caption signature), it is.
func TestAnUnscannedReadIsAFindingOfItsRecord(t *testing.T) {
	ctx := context.Background()
	svc, source, token := claimedOf(t, tenMinuteRecord)
	r := record(t, ctx, svc, source, token, `{"captionsKnown": false, "reason": "`+library.UnscannedReason+
		` (its suffix SEI)", "duration": 600, "span": {"start": 0, "end": 600}, "sizeBytes": 6, "ffprobe": "7.1.5"}`)
	if r.Finding == nil || *r.Finding != library.FindingUnscanned || r.Failures != 0 || r.NextAttemptAt != nil {
		t.Fatalf("the read: %+v; want the finding unscanned, no backoff", r)
	}
	if cl, err := svc.Claim(ctx, "analyzer-0", 1); err != nil || len(cl.Items) != 0 {
		t.Errorf("an unscanned original was handed out again: %+v, %v", cl, err)
	}
	storetest.Exec(t, svc.st, `UPDATE com_nalet_katalog_itemsources SET recordedat = now() + interval '1 second'
		WHERE id = $1`, source)
	if cl, err := svc.Claim(ctx, "analyzer-0", 1); err != nil || len(cl.Items) != 1 {
		t.Errorf("not handed out again once its record was written again: %+v, %v", cl, err)
	}
	// any other reason the read gives of not knowing is a failure
	svc, source, token = claimedOf(t, tenMinuteRecord)
	r = record(t, ctx, svc, source, token, `{"captionsKnown": false, "reason": "a bound was hit", "duration": 600,
		"span": {"start": 0, "end": 600}, "sizeBytes": 6, "ffprobe": "7.1.5"}`)
	if r.Finding != nil || r.Failures != 1 || r.NextAttemptAt == nil {
		t.Errorf("a read that can't tell for another reason: %+v; want a failure waiting its backoff", r)
	}
}

// The PUT lays a read's windows by the plan's slack and the gop it says: of
// a gop of 12 s a window 12.4 s from its place is in place and the read is
// kept with its gop, one 12.6 s off is a failed read; at the keyframe
// before (keyframeBefore) one 29.9 s before its place is in place, not one
// 30.1 s before; a read that says no gop holds its windows to their places
// (1 s off: a failed read); a window whose keyframeBefore is null fails
// (a read kept with a failed window, said in its reason, which says none of
// nothing); a gop below 0 is refused.
func TestThePutLaysWindowsByThePlansSlack(t *testing.T) {
	ctx := context.Background()
	const fifty = `{"at": 0.5, "start": 300, "end": 301.88, "frames": 48, "captions": false, "interlacedFrames": 0, "repeatFrames": 0}`
	body := func(start, end, kb, gop string) string {
		w := strings.TrimSuffix(strings.NewReplacer(`"start": 300`, `"start": `+start, `"end": 301.88`, `"end": `+end).
			Replace(fifty), "}") + kb + "}"
		b := strings.Replace(fullRead, fifty, w, 1)
		if gop != "" {
			b = strings.Replace(b, `"span": {"start": 0, "end": 600},`, `"span": {"start": 0, "end": 600}, "gop": `+gop+`,`, 1)
		}
		return b
	}
	for _, c := range []struct {
		name, body string
		read       bool
		why        string
		gop        float64
	}{
		{"a gop of 12 s, 12.4 s before", body("287.6", "289.48", "", "12"), true, "", 12},
		{"a gop of 12 s, 12.6 s before", body("287.4", "289.28", "", "12"), false, "lie off their places", 0},
		{"the keyframe before, 29.9 s before", body("270.1", "271.98", `, "keyframeBefore": true`, "2"), true, "", 2},
		{"the keyframe before, 30.1 s before", body("269.9", "271.78", `, "keyframeBefore": true`, "2"), false,
			"lie off their places", 0},
		{"no gop said, 1 s after", body("301", "302.88", "", ""), false, "lie off their places", 0},
		{"keyframeBefore null", body("300", "301.88", `, "keyframeBefore": null`, "2"), true,
			"its keyframeBefore is no boolean", 2},
	} {
		t.Run(c.name, func(t *testing.T) {
			svc, source, token := claimedOf(t, tenMinuteRecord)
			r := record(t, ctx, svc, source, token, c.body)
			switch {
			case c.read && (r.State != StateRead || (c.why == "" && r.Failures != 0) || r.ReadGop == nil ||
				*r.ReadGop != c.gop || !strings.Contains(deref(r.Reason), c.why)):
				t.Errorf("the read: %+v (%s); want it kept with its gop %g, %q", r, deref(r.Reason), c.gop, c.why)
			case !c.read && (r.State == StateRead && r.Reason == nil || r.Failures != 1 ||
				!strings.Contains(deref(r.Reason), c.why)):
				t.Errorf("the read: %+v (%s); want a failed one, %q, waiting its backoff", r, deref(r.Reason), c.why)
			}
		})
	}
	st, source, _, token := claimed(t)
	_, err := Record(ctx, st, source, report(t, `{"token": "`+token+`", "original": `+body("300", "301.88", "", "-1")+`}`),
		time.Now())
	var bad *BadReport
	if !errors.As(err, &bad) || !strings.Contains(bad.Why, "gop is the longest run") {
		t.Errorf("a gop below 0: %v; want it refused", err)
	}
}

// planBody is a report of a read of the plan's six windows of a picture of
// length seconds, each at its fraction and its place (Span.Place), the
// windows at ats said at the place given there instead (start, at), the
// window at drop left out (-1: none).
func planBody(length float64, said map[float64][2]float64, drop float64) string {
	sp := library.Span{End: length}
	var ws []string
	for _, at := range []float64{0, 0.1, 0.3, 0.5, 0.7, 0.9} {
		if at == drop {
			continue
		}
		start, a := sp.Place(at), at
		if s, ok := said[at]; ok {
			start, a = s[0], s[1]
		}
		ws = append(ws, fmt.Sprintf(`{"at": %g, "start": %g, "end": %g, "frames": 48, "captions": false, `+
			`"interlacedFrames": 0, "repeatFrames": 0}`, a, start, start+1.88))
	}
	return fmt.Sprintf(`{"captionWindows": [%s], "duration": %g, "span": {"start": 0, "end": %g}, "sizeBytes": 6, `+
		`"ffprobe": "7.1.5"}`, strings.Join(ws, ", "), length, length)
}

// recordOf is the source record of a film of length seconds.
func recordOf(length float64) string {
	return strings.Replace(tenMinuteRecord, `"durationMs": 600000`, fmt.Sprintf(`"durationMs": %d`, int(length*1000)), 1)
}

// The PUT holds a read to the plan's six windows: a picture two of whose
// places lie closer than a window reaches (a 100 s one: 70 and 90 % at 70 s
// both) read in windows is a failed read, one whose places lie apart (120 s)
// read in its six windows a read; a read of five is a failed one; a window
// the tail short of the end says its fraction (0.9) and starts where it lies
// (170 s of 200 s): said as where it lies (0.85), it is a failed read. A span
// that is one number, not {start, end}, is refused.
func TestThePutHoldsAReadToThePlansSixWindows(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		name, record, body string
		read               bool
		why                string
	}{
		{"100 s in windows", recordOf(100), planBody(100, nil, -1), false, "is read whole"},
		{"120 s in its six windows", recordOf(120), planBody(120, nil, -1), true, ""},
		{"600 s in five windows, none at 30 %", tenMinuteRecord, planBody(600, nil, 0.3), false, "it has no window at 30%"},
		{"200 s, the tail window at its fraction", recordOf(200), planBody(200, nil, -1), true, ""},
		{"200 s, the tail window said where it lies", recordOf(200),
			planBody(200, map[float64][2]float64{0.9: {170, 0.85}}, -1), false, "its windows at 85% are at no fraction"},
	} {
		t.Run(c.name, func(t *testing.T) {
			svc, source, token := claimedOf(t, c.record)
			r := record(t, ctx, svc, source, token, c.body)
			switch {
			case c.read && (r.State != StateRead || r.Failures != 0 || r.Reason != nil || r.ReadSpanStart == nil ||
				*r.ReadSpanStart != 0 || r.ReadSpanEnd == nil || r.Duration == nil || *r.ReadSpanEnd != *r.Duration):
				t.Errorf("the read: %+v (%s); want a read in full, its span kept", r, deref(r.Reason))
			case !c.read && (r.State == StateRead && r.Reason == nil || r.Failures != 1 ||
				!strings.Contains(deref(r.Reason), c.why)):
				t.Errorf("the read: %+v (%s); want a failed one, %q", r, deref(r.Reason), c.why)
			}
		})
	}
	var r Report
	if err := json.Unmarshal([]byte(`{"token": "t", "original": `+strings.Replace(fullRead,
		`"span": {"start": 0, "end": 600}`, `"span": 600`, 1)+`}`), &r); err == nil {
		t.Error("a span of one number: read; want it no report")
	}
	st, source, _, token := claimed(t)
	_, err := Record(ctx, st, source, report(t, `{"token": "`+token+`", "original": `+strings.Replace(fullRead,
		`"span": {"start": 0, "end": 600}`, `"span": {"start": 600, "end": 0}`, 1)+`}`), time.Now())
	var bad *BadReport
	if !errors.As(err, &bad) || !strings.Contains(bad.Why, "span is the picture's span it read") {
		t.Errorf("a span that ends before it starts: %v; want it refused", err)
	}
}

// claimedWithProbe is claimedOf of an original whose stored probe is probe
// ("": none).
func claimedWithProbe(t *testing.T, record, probe string) (*Service, string, string) {
	t.Helper()
	st := storetest.Open(t)
	dir := t.TempDir()
	media := filepath.Join(dir, "film.mkv")
	if err := os.WriteFile(media, []byte("a film"), 0o644); err != nil {
		t.Fatal(err)
	}
	rec := writeRecord(t, dir, record)
	if err := os.Remove(filepath.Join(rec, library.ProbeFile)); err != nil {
		t.Fatal(err)
	}
	if probe != "" && !strings.Contains(probe, `"format"`) {
		// as ffprobe writes one: with its format
		probe = `{"format": {"format_name": "matroska,webm"}, ` + strings.TrimPrefix(strings.TrimSpace(probe), "{")
	}
	if probe != "" {
		if err := os.WriteFile(filepath.Join(rec, library.ProbeFile), []byte(probe), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	storetest.AddItem(t, st, "r1", "movie", "A Film", "")
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_itemsources (id, item_id, filename, arrivalpath, sizebytes, state,
		recordedat, recorddir) VALUES ('r1-src', 'r1', 'film.mkv', $1, 6, 'present', now(), $2)`, media, rec)
	svc := New(st).WithClock(at(time.Date(2026, 11, 16, 23, 0, 0, 0, time.UTC)))
	c, err := svc.Claim(context.Background(), "analyzer-0", 1)
	if err != nil || len(c.Items) != 1 {
		t.Fatalf("the claim: %+v, %v", c, err)
	}
	return svc, "r1-src", c.Items[0].Token
}

// The PUT holds a read to the plan plan_of gives of the original's stored
// probe: of no probe, of one that gives no start or no frame rate, the read
// is the finding noPlan - no backoff, awaiting its record, not handed out
// again while the record is as it was, handed out once it is written again;
// a rate in r_frame_rate where avg_frame_rate is 0/0 places it, the
// picture's own duration or else the record's length; a Matroska DURATION
// tag places nothing.
func TestThePutHoldsAReadToThePlanOfItsProbe(t *testing.T) {
	ctx := context.Background()
	pic := func(fields string) string {
		return `{"streams": [{"index": 0, "codec_type": "video", "codec_name": "h264", "field_order": "progressive"` +
			fields + `}]}`
	}
	for _, c := range []struct {
		name, probe string
		plan        bool
		why         string
	}{
		{"no stored probe", "", false, "gave no plan of its picture"},
		{"no start", pic(`, "avg_frame_rate": "24/1"`), false, "gave no plan of its picture"},
		{"rates of 0/0", pic(`, "start_time": "0", "avg_frame_rate": "0/0", "r_frame_rate": "0/0"`), false,
			"gave no plan of its picture"},
		{"avg_frame_rate 0/0, r_frame_rate 24000/1001", pic(`, "start_time": "0", "avg_frame_rate": "0/0",
			"r_frame_rate": "24000/1001"`), true, ""},
		{"no avg_frame_rate, r_frame_rate 24/1", pic(`, "start_time": "0", "r_frame_rate": "24/1"`), false,
			"gave no plan of its picture"},
		{"the picture's own duration", pic(`, "start_time": "0", "duration": "600.000000", "avg_frame_rate": "24/1"`),
			true, ""},
		{"a Matroska DURATION tag of 590 s", pic(`, "start_time": "0", "avg_frame_rate": "24/1",
			"tags": {"DURATION": "00:09:50.000000000"}`), true, ""},
		// a cover marked by its tags before the film is passed over; a webp
		// image so tagged is no cover, a second video stream: no plan
		{"a tagged cover before the film", `{"streams": [{"index": 0, "codec_type": "video", "codec_name": "mjpeg",
			"tags": {"filename": "cover.jpg", "mimetype": "image/jpeg"}}, {"index": 1, "codec_type": "video",
			"codec_name": "h264", "start_time": "0", "avg_frame_rate": "24/1"}]}`, true, ""},
		{"a webp image before the film", `{"streams": [{"index": 0, "codec_type": "video", "codec_name": "webp",
			"tags": {"filename": "cover.webp", "mimetype": "image/webp"}}, {"index": 1, "codec_type": "video",
			"codec_name": "h264", "start_time": "0", "avg_frame_rate": "24/1"}]}`, false, "gave no plan of its picture"},
	} {
		t.Run(c.name, func(t *testing.T) {
			svc, source, token := claimedWithProbe(t, tenMinuteRecord, c.probe)
			r := record(t, ctx, svc, source, token, fullRead)
			if c.plan {
				if r.State != StateRead || r.Failures != 0 || r.Reason != nil {
					t.Errorf("the read: %+v (%s); want a read in full", r, deref(r.Reason))
				}
				return
			}
			if r.Finding == nil || *r.Finding != library.FindingNoPlan || r.Failures != 0 || r.NextAttemptAt != nil ||
				!strings.Contains(deref(r.Reason), c.why) {
				t.Fatalf("the read: %+v (%s); want the finding noPlan, no backoff", r, deref(r.Reason))
			}
			if p, err := svc.Progress(ctx); err != nil || p.AwaitingRecord != 1 || p.ToRead != 0 {
				t.Errorf("the progress: %+v, %v; want it awaiting its record", p, err)
			}
			if cl, err := svc.Claim(ctx, "analyzer-0", 1); err != nil || len(cl.Items) != 0 {
				t.Errorf("handed out again, its record as it was: %+v, %v", cl, err)
			}
			storetest.Exec(t, svc.st, `UPDATE com_nalet_katalog_itemsources SET recordedat = now() + interval '1 second'
				WHERE id = $1`, source)
			if cl, err := svc.Claim(ctx, "analyzer-0", 1); err != nil || len(cl.Items) != 1 {
				t.Errorf("not handed out again once its record was written again: %+v, %v", cl, err)
			}
		})
	}
}

// The PUT holds a read to what the second look ruled: a probe that states a
// rate below one frame a second, or whose cover's disposition says "0" (no
// mark: two films), gives no plan (the finding noPlan); a claim whose span's
// step is not finite is no plan either; a gop longer than the first 1200
// packets last at the frame step, and a read of the whole picture of too few
// packets for its span, are failed reads; a report that writes NaN is no
// report.
func TestThePutHoldsAReadToTheSecondLooksRulings(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct{ name, probe string }{
		{"a rate below one frame a second", `{"streams": [{"index": 0, "codec_type": "video", "codec_name": "h264",
			"start_time": "0", "avg_frame_rate": "1/2"}]}`},
		{"a cover marked \"0\" before the film", `{"streams": [{"index": 0, "codec_type": "video", "codec_name": "mjpeg",
			"disposition": {"attached_pic": "0"}}, {"index": 1, "codec_type": "video", "codec_name": "h264",
			"start_time": "0", "avg_frame_rate": "24/1"}]}`},
	} {
		svc, source, token := claimedWithProbe(t, tenMinuteRecord, c.probe)
		if r := record(t, ctx, svc, source, token, fullRead); r.Finding == nil || *r.Finding != library.FindingNoPlan {
			t.Errorf("%s: %+v (%s); want the finding noPlan", c.name, r, deref(r.Reason))
		}
	}
	svc, source, token := claimedOf(t, tenMinuteRecord)
	storetest.Exec(t, svc.st, `UPDATE com_nalet_katalog_sourcecaptions SET claimspanstep = 'NaN'::float8 WHERE source_id = $1`,
		source)
	if r := record(t, ctx, svc, source, token, fullRead); r.Finding == nil || *r.Finding != library.FindingNoPlan {
		t.Errorf("a claim's step of NaN: %+v (%s); want the finding noPlan", r, deref(r.Reason))
	}
	for _, c := range []struct{ name, record, body, why string }{
		{"a gop of 51 s", tenMinuteRecord, strings.Replace(fullRead, `"span": {"start": 0, "end": 600},`,
			`"span": {"start": 0, "end": 600}, "gop": 51,`, 1), "is longer than the 1200"},
		{"a whole read of 900 packets of 40 s", fortySeconds, strings.NewReplacer(`"frames": 960`, `"frames": 900`,
			`"packets": 960`, `"packets": 900`).Replace(wholeBody(960, 0, "40")), "fewer than its picture's span"},
	} {
		svc, source, token := claimedOf(t, c.record)
		if r := record(t, ctx, svc, source, token, c.body); r.State == StateRead || r.Failures != 1 ||
			!strings.Contains(deref(r.Reason), c.why) {
			t.Errorf("%s: %+v (%s); want a failed read, %q", c.name, r, deref(r.Reason), c.why)
		}
	}
	var r Report
	if err := json.Unmarshal([]byte(`{"token": "t", "original": `+strings.Replace(fullRead, `"duration": 600`,
		`"duration": NaN`, 1)+`}`), &r); err == nil {
		t.Error("a report that writes NaN: read; want it no report")
	}
}
