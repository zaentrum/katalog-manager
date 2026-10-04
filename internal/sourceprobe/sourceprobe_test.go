package sourceprobe

import (
	"context"
	"fmt"
	"testing"

	"github.com/zaentrum/katalog-manager/internal/store"
	"github.com/zaentrum/katalog-manager/internal/store/storetest"
)

func show(p Probe) string {
	s := func(v *string) string {
		if v == nil {
			return "-"
		}
		return *v
	}
	n := func(v *int64) string {
		if v == nil {
			return "-"
		}
		return fmt.Sprint(*v)
	}
	return s(p.Codec) + " " + s(p.Resolution) + " " + n(p.DurationMs) + " " + n(p.BitrateKbps) + " " + n(p.SizeBytes)
}

// The transcoder's details say the source's codec and resolution, as it
// reports them when it encodes and when it copies; nothing else counts.
func TestFromTranscodeDetails(t *testing.T) {
	for details, want := range map[string]string{
		"profile=hevc-1080p src_codec=h264 res=1920x1080 maxrate=8Mbps ladder=v0:hevc_nvenc:1920x1080 kf=150/6s out_mb=812.4 dur_s=301.2": "h264 1920x1080 - - -",
		"profile=x265-804p src_codec=H264 res=1920x804 ladder=v0:libx265:1920x804,v1:libx264:1280x536 kf=144/6s out_mb=90 dur_s=99":       "h264 1920x804 - - -",
		"skip codec=hevc res=3840x1608 reason=source_already_hevc:hevc":                                                                   "hevc 3840x1608 - - -",
		"skip codec=h264 res=1280x720 reason=browser_friendly_h264_no_gpu":                                                                "h264 1280x720 - - -",
		"auto-promoted after transcode=done": "- - - - -",
		"codec=hevc res=1920x1080":           "- 1920x1080 - - -", // a codec only where the transcoder skips
		"profile=x src_codec= res=0x0":       "- - - - -",
		"":                                   "- - - - -",
		"skip codec=hevc res=1920x1080x2 reason=garbled":         "hevc - - - -",
		"ladder=v0:libx265:1920x804 src_codec=hevc res=720x576 ": "hevc 720x576 - - -",
	} {
		if got := show(FromTranscodeDetails(details)); got != want {
			t.Errorf("FromTranscodeDetails(%q) = %s, want %s", details, got, want)
		}
	}
}

// ffprobe's output says it all: the first video stream that is no cover
// picture, the format's duration (rounded to the millisecond), bit rate and
// size; output it cannot read says nothing.
func TestFromFFprobe(t *testing.T) {
	out := `{"streams":[
		{"index":0,"codec_type":"video","codec_name":"mjpeg","width":600,"height":900,"disposition":{"attached_pic":1}},
		{"index":1,"codec_type":"video","codec_name":"H264","width":1920,"height":804,"disposition":{"attached_pic":0}},
		{"index":2,"codec_type":"audio","codec_name":"aac"}],
		"format":{"duration":"629.887000","bit_rate":"4312345","size":"339540000"}}`
	if got := show(FromFFprobe(out)); got != "h264 1920x804 629887 4312 339540000" {
		t.Errorf("FromFFprobe = %s", got)
	}
	for _, bad := range []string{"", "not json", `{"streams":[{"codec_type":"audio"}],"format":{"duration":"N/A"}}`} {
		if p := FromFFprobe(bad); !p.Empty() {
			t.Errorf("FromFFprobe(%q) = %s, want nothing", bad, show(p))
		}
	}
}

func asset(t *testing.T, st *store.Store, item string) string {
	t.Helper()
	var out string
	if err := st.Pool().QueryRow(context.Background(), `SELECT COALESCE(codec, '-') || ' ' || COALESCE(resolution, '-') || ' ' ||
		COALESCE(durationms::text, '-') || ' ' || COALESCE(bitratekbps::text, '-') || ' ' || COALESCE(sizebytes::text, '-')
		FROM com_nalet_katalog_playbackassets WHERE item_id = $1 AND isprimary`, item).Scan(&out); err != nil {
		t.Fatalf("the source of %s: %v", item, err)
	}
	return out
}

func addSource(t *testing.T, st *store.Store, item, sets string) {
	t.Helper()
	storetest.AddItem(t, st, item, "movie", "Film "+item, "")
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_playbackassets (id, item_id, path, isprimary, kind, sizebytes)
		VALUES ('src-'||$1::varchar, $1::varchar, '/media/'||$1::varchar||'.mkv', true, 'primary', 80000000)`, item)
	if sets != "" {
		storetest.Exec(t, st, `UPDATE com_nalet_katalog_playbackassets SET `+sets+` WHERE id = 'src-'||$1::varchar`, item)
	}
}

func s(v string) *string { return &v }
func n(v int64) *int64   { return &v }

// Fill writes what a probe says over what the source held, and derives the
// bit rate from its size and duration when nothing says it; FillEmpty fills
// only what is empty. A package is never touched, and an item without a
// source fills nothing.
func TestFillAndFillEmpty(t *testing.T) {
	st := storetest.Open(t)
	ctx := context.Background()
	addSource(t, st, "m1", "")
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_playbackassets (id, item_id, path, isprimary, kind, codec)
		VALUES ('pkg-m1', 'm1', '/p/m1/manifest.json', false, 'packaged', 'hev1.1.6.L120.90')`)

	if ok, err := Fill(ctx, st.Pool(), "m1", Probe{Codec: s("h264"), Resolution: s("1920x804")}); err != nil || !ok {
		t.Fatalf("Fill: %v %v", ok, err)
	}
	if got := asset(t, st, "m1"); got != "h264 1920x804 - - 80000000" {
		t.Errorf("after the transcoder's report: %s", got)
	}
	if err := FillEmpty(ctx, st.Pool(), "m1", Probe{Codec: s("hevc"), DurationMs: n(640000)}); err != nil {
		t.Fatal(err)
	}
	if got := asset(t, st, "m1"); got != "h264 1920x804 640000 1000 80000000" {
		t.Errorf("after the packaged duration: %s, want the codec kept, the duration filled, the bit rate derived", got)
	}
	if _, err := Fill(ctx, st.Pool(), "m1", Probe{Codec: s("hevc"), Resolution: s("3840x1608"), BitrateKbps: n(9000)}); err != nil {
		t.Fatal(err)
	}
	if got := asset(t, st, "m1"); got != "hevc 3840x1608 640000 9000 80000000" {
		t.Errorf("after a re-probe: %s, want what it says", got)
	}
	var pkg string
	if err := st.Pool().QueryRow(ctx, `SELECT codec FROM com_nalet_katalog_playbackassets WHERE id = 'pkg-m1'`).Scan(&pkg); err != nil || pkg != "hev1.1.6.L120.90" {
		t.Errorf("the package was touched: %q %v", pkg, err)
	}
	// A probe that says the duration but no bit rate (a v1 source block with
	// size and duration): the bit rate follows from them.
	addSource(t, st, "m2", "")
	if _, err := Fill(ctx, st.Pool(), "m2", Probe{DurationMs: n(320000)}); err != nil {
		t.Fatal(err)
	}
	if got := asset(t, st, "m2"); got != "- - 320000 2000 80000000" {
		t.Errorf("a duration without a bit rate: %s, want the bit rate from the size", got)
	}
	if ok, err := Fill(ctx, st.Pool(), "nope", Probe{Codec: s("h264")}); err != nil || ok {
		t.Errorf("Fill of an item without a source: %v %v", ok, err)
	}
	if ok, err := Fill(ctx, st.Pool(), "m1", Probe{}); err != nil || ok {
		t.Errorf("Fill of nothing: %v %v", ok, err)
	}
}

// The backfill fills what each source lacks, and only that: from the item's
// diagnostics first, then its transcode's details and its package's duration,
// the bit rate from size and duration; a source nothing says more of is
// unknown, and a second run changes nothing.
func TestBackfill(t *testing.T) {
	st := storetest.Open(t)
	ctx := context.Background()
	// from the transcoder's report and the package
	addSource(t, st, "a", "")
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_itemprocessingsteps (id, item_id, step, status, details)
		VALUES ('ta', 'a', 'transcode', 'done', 'profile=x265-804p src_codec=h264 res=1920x804 ladder=v0:libx265:1920x804 dur_s=99')`)
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_playbackassets (id, item_id, path, isprimary, kind, durationms)
		VALUES ('pa', 'a', '/p/a/manifest.json', false, 'packaged', 629880)`)
	// from the diagnostics, which win over the report
	addSource(t, st, "b", "")
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_itemprocessingsteps (id, item_id, step, status, details)
		VALUES ('tb', 'b', 'transcode', 'not_applicable', 'skip codec=hevc res=1280x720 reason=x')`)
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_itemdiagnostics (id, item_id, ffprobedata) VALUES ('db', 'b',
		'{"streams":[{"codec_type":"video","codec_name":"h264","width":1920,"height":1080}],"format":{"duration":"629.887","bit_rate":"5000000"}}')`)
	// complete already, and one with a value of its own
	addSource(t, st, "c", `codec = 'h264', resolution = '1920x804', durationms = 600000, bitratekbps = 1066`)
	addSource(t, st, "d", `codec = 'mpeg4'`)
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_itemprocessingsteps (id, item_id, step, status, details)
		VALUES ('td', 'd', 'transcode', 'done', 'profile=x src_codec=h264 res=640x480 dur_s=1')`)
	// nothing known
	addSource(t, st, "e", "")

	res, err := Backfill(ctx, st.Pool())
	if err != nil {
		t.Fatal(err)
	}
	if res != (BackfillResult{Assets: 4, Filled: 3, Codecs: 2, Resolutions: 3, Durations: 2, Bitrates: 2, Unknown: 1}) {
		t.Errorf("Backfill: %+v", res)
	}
	for item, want := range map[string]string{
		"a": "h264 1920x804 629880 1016 80000000",
		"b": "h264 1920x1080 629887 5000 80000000",
		"c": "h264 1920x804 600000 1066 80000000",
		"d": "mpeg4 640x480 - - 80000000",
		"e": "- - - - 80000000",
	} {
		if got := asset(t, st, item); got != want {
			t.Errorf("%s: %s, want %s", item, got, want)
		}
	}
	again, err := Backfill(ctx, st.Pool())
	if err != nil || again.Filled != 0 || again.Assets != 2 || again.Unknown != 2 {
		t.Errorf("a second run: %+v, %v; want nothing filled", again, err)
	}
}

// More sources than a batch are all filled.
func TestBackfillOfMoreThanABatch(t *testing.T) {
	st := storetest.Open(t)
	for i := 0; i < backfillBatch+20; i++ {
		id := fmt.Sprintf("m%04d", i)
		addSource(t, st, id, "")
		storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_itemprocessingsteps (id, item_id, step, status, details)
			VALUES ('t'||$1::varchar, $1::varchar, 'transcode', 'done', 'skip codec=hevc res=1920x1080 reason=x')`, id)
	}
	res, err := Backfill(context.Background(), st.Pool())
	if err != nil || res.Filled != backfillBatch+20 || res.Codecs != backfillBatch+20 {
		t.Errorf("Backfill: %+v, %v", res, err)
	}
	if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_playbackassets WHERE codec = 'hevc' AND resolution = '1920x1080'`); n != backfillBatch+20 {
		t.Errorf("%d sources filled", n)
	}
}
