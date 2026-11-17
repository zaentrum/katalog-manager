package rest

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zaentrum/katalog-manager/internal/library"
	"github.com/zaentrum/katalog-manager/internal/store"
	"github.com/zaentrum/katalog-manager/internal/store/storetest"
)

const (
	ccFilm    = "c1"
	ccSource  = "c1-src"
	ccVersion = "c1-ver"
)

// ccFixture is a film whose original lies on disk and whose version is
// complete; its retirement held as the retire job holds one for want of the
// original's closed captions.
func ccFixture(t *testing.T) (h http.Handler, token, media string, s *store.Store) {
	t.Helper()
	s = storetest.Open(t)
	dir := t.TempDir()
	h, iss := server(t, s, testConfig(dir))
	media = filepath.Join(dir, "media", "film.mkv")
	if err := os.MkdirAll(filepath.Dir(media), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(media, []byte("a film"), 0o644); err != nil {
		t.Fatal(err)
	}
	// its record: a ten-minute film, the length and the span its read is
	// held to
	record := filepath.Join(dir, "sources", ccSource)
	if err := os.MkdirAll(record, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(record, "source.json"), []byte(`{"schema": "zaentrum.library.source/2",
		"container": {"format": "matroska,webm", "durationMs": 600000}, "streams": [{"index": 0, "type": "video",
		"codec": "h264", "fieldOrder": "progressive"}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	storetest.AddItem(t, s, ccFilm, "movie", "A Film", "")
	held := library.CaptionsUnknown + ": the retire job's why"
	storetest.Exec(t, s, `INSERT INTO com_nalet_katalog_itemsources (id, item_id, filename, arrivalpath, sizebytes, state, error,
		recordedat, recorddir) VALUES ($1, $2, 'film.mkv', $3, 6, 'present', $4, now(), $5)`, ccSource, ccFilm, media, held,
		record)
	storetest.Exec(t, s, `INSERT INTO com_nalet_katalog_itemversions (id, item_id, sourceids, state, dir, completedat)
		VALUES ($1, $2, ARRAY[$3::varchar], 'complete', $4, now())`, ccVersion, ccFilm, ccSource,
		filepath.Join(dir, "movies", ccFilm, "versions", ccVersion))
	storetest.Exec(t, s, `INSERT INTO com_nalet_katalog_itemprocessingsteps (id, createdat, modifiedat, item_id, step, status,
			attempts, error, details) VALUES (gen_random_uuid()::varchar, now(), now(), $1, 'retire', 'failed', 1, $2, $3)`,
		ccFilm, held, library.HeldDetails(ccVersion))
	return h, iss.Service(t, "zaentrum-manager"), media, s
}

// ccAnyTime has the captions backfill, on by default, hand out at any time.
func ccAnyTime(t *testing.T, st *store.Store) {
	t.Helper()
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_settings (id, key, valuetext) VALUES ('cw', 'captions.backfill.window', '')`)
}

type ccClaim struct {
	Items []struct {
		SourceID, ItemID, Path, Token string
		Packets                       int
	}
	Deadline          *string
	Idle              string
	RetryAfterSeconds int
}

func claimCaptions(t *testing.T, h http.Handler, token string) ccClaim {
	t.Helper()
	w := do(h, http.MethodPost, "/api/captions/claim", `{"worker": "analyzer-0", "limit": 4}`, token)
	var c ccClaim
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &c) != nil {
		t.Fatalf("claim: %d %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "package") {
		t.Errorf("a claim names a package: %s", w.Body.String())
	}
	return c
}

// windowsOf are the planned windows of a read of a ten-minute video, the
// one at 10 % showing captions as captions says.
func windowsOf(captions string) string {
	return `"captionWindows": [{"at": 0, "start": 0, "end": 1.88, "frames": 48, "captions": false, "interlacedFrames": 0, "repeatFrames": 0},
		{"at": 0.1, "start": 60, "end": 61.88, "frames": 48, "captions": ` + captions + `, "interlacedFrames": 0,
			"repeatFrames": 0},
		{"at": 0.3, "start": 180, "end": 181.88, "frames": 48, "captions": false, "interlacedFrames": 0, "repeatFrames": 0},
		{"at": 0.5, "start": 300, "end": 301.88, "frames": 48, "captions": false, "interlacedFrames": 0, "repeatFrames": 0},
		{"at": 0.7, "start": 420, "end": 421.88, "frames": 48, "captions": false, "interlacedFrames": 0, "repeatFrames": 0},
		{"at": 0.9, "start": 540, "end": 541.88, "frames": 48, "captions": false, "interlacedFrames": 0, "repeatFrames": 0}], "duration": 600, "sizeBytes": 6`
}

// The analyzer's captions backfill asks for the originals to read and is
// told why there are none while the backfill is set off; on, as by default,
// it is handed the original on disk, how many packets to decode a window
// and a token, once, and no package. A report that names no ffprobe, or one
// that is not 7.1, or no windows, is refused and changes nothing, and so is
// one whose token is not the claim's; a read none of whose windows decoded a
// frame is kept as a failed one and lets go of nothing; one with a window
// that decoded none is kept, its window said, and handed out again after
// its backoff; a read in full, with captions or without, lets go of the
// retirement held for want of them, and the original is not handed out
// again. A report's package part, of an older analyzer, is taken no notice
// of.
func TestTheAnalyzerReadsTheClosedCaptionsTheBackfillHandsOut(t *testing.T) {
	for _, captions := range []string{"true", "false"} {
		t.Run("captions "+captions, func(t *testing.T) {
			h, token, media, st := ccFixture(t)
			count := func(sql string, args ...any) int { return storetest.Count(t, st, sql, args...) }
			storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_settings (id, key, valuetext)
				VALUES ('cb', 'captions.backfill', 'off')`)
			if c := claimCaptions(t, h, token); len(c.Items) != 0 || c.Idle != "captions.backfill is off" ||
				c.RetryAfterSeconds != 900 {
				t.Fatalf("the backfill set off: %+v", c)
			}
			storetest.Exec(t, st, `DELETE FROM com_nalet_katalog_settings WHERE id = 'cb'`)
			ccAnyTime(t, st)
			c := claimCaptions(t, h, token)
			if len(c.Items) != 1 || c.Deadline == nil {
				t.Fatalf("the claim: %+v", c)
			}
			job := c.Items[0]
			if job.SourceID != ccSource || job.ItemID != ccFilm || job.Packets != 48 || job.Token == "" || job.Path != media {
				t.Fatalf("the job: %+v", job)
			}
			if again := claimCaptions(t, h, token); len(again.Items) != 0 {
				t.Errorf("an original handed out was handed out again: %+v", again)
			}

			put := func(body string) (int, string) {
				w := do(h, http.MethodPut, "/api/captions/sources/"+ccSource, body, token)
				return w.Code, w.Body.String()
			}
			held := func() int {
				return count(`SELECT count(*) FROM com_nalet_katalog_itemprocessingsteps WHERE item_id = $1 AND step = 'retire'`,
					ccFilm) + count(`SELECT count(*) FROM com_nalet_katalog_itemsources WHERE id = $1 AND error IS NOT NULL`, ccSource)
			}
			read := windowsOf(captions)
			tok := `"token": "` + job.Token + `"`
			for _, r := range []struct {
				body, says string
				code       int
			}{
				{`{` + tok + `, "original": {` + read + `}}`, "names the ffprobe that read it", 400},
				{`{` + tok + `, "original": {` + read + `, "ffprobeVersion": "7.1.1"}}`, "names the ffprobe that read it", 400},
				{`{` + tok + `, "original": {` + read + `, "ffprobe": "4.4.2"}}`, "is not 7.1", 400},
				{`{` + tok + `, "original": {` + read + `, "ffprobe": "9.0.2"}}`, "is not 7.1", 400},
				{`{` + tok + `, "original": {` + read + `, "ffprobe": "N-117000-gabcdef"}}`, "names no release", 400},
				{`{` + tok + `, "original": {"captions": true, "framesDecoded": 48, "ffprobe": "7.1.1"}}`,
					"names the windows it decoded", 400},
				{`{` + tok + `, "package": {` + read + `, "ffprobe": "7.1.1"}}`, "what a read found of the original", 400},
				{`{` + tok + `}`, "what a read found of the original", 400},
				{`{"original": {` + read + `, "ffprobe": "7.1"}}`, "not the claim's", 409},
				{`{"token": "another-claim", "original": {` + read + `, "ffprobe": "7.1"}}`, "not the claim's", 409},
			} {
				if code, body := put(r.body); code != r.code || !strings.Contains(body, r.says) {
					t.Errorf("%s: %d %s, want %d saying %q", r.body, code, body, r.code, r.says)
				}
			}
			if n := count(`SELECT count(*) FROM com_nalet_katalog_sourcecaptions WHERE source_id = $1 AND state = 'pending'
				AND claimedat IS NOT NULL AND ffprobeversion IS NULL`, ccSource); n != 1 || held() != 2 {
				t.Errorf("a refused report changed what is kept (%d rows as handed out, %d of the hold)", n, held())
			}

			// a read none of whose windows decoded a frame: a failure, which
			// lets go of nothing - a window that is no window (captions of
			// one that decoded no frame) is a failed one, kept and said
			code, body := put(`{` + tok + `, "original": {"captionWindows": [{"at": 0.1, "start": null,
				"end": null, "frames": 0, "captions": null}, {"at": 0.3, "start": 180, "end": 182, "frames": 0,
				"captions": true}], "ffprobe": "7.1.1"}}`)
			var got struct {
				Original struct {
					State, FFprobe string
					CaptionWindows []map[string]any
					Captions       *bool
					FramesDecoded  *int
					Duration       *float64
					Reason         *string
				}
				Failures int
			}
			if code != http.StatusOK || json.Unmarshal([]byte(body), &got) != nil || got.Original.State != "failed" ||
				got.Original.Captions != nil || got.Original.Reason == nil ||
				!strings.Contains(*got.Original.Reason, "decoded a video frame") ||
				!strings.Contains(*got.Original.Reason, "captionWindows[1]: a window that decoded no frame shows no captions") ||
				got.Failures != 1 {
				t.Fatalf("a read of no frame: %d %s", code, body)
			}
			if held() != 2 {
				t.Error("a read that decoded no frame let go of the retirement held for the captions")
			}

			// handed out again after its backoff: a window that decoded no
			// frame is kept, said, and handed out again after its backoff
			storetest.Exec(t, st, `UPDATE com_nalet_katalog_sourcecaptions SET nextattemptat = now() - interval '1 minute'`)
			c = claimCaptions(t, h, token)
			if len(c.Items) != 1 || c.Items[0].Path != media {
				t.Fatalf("the claim after the backoff: %+v", c)
			}
			tok = `"token": "` + c.Items[0].Token + `"`
			code, body = put(`{` + tok + `, "original": {"captionWindows": [
				{"at": 0, "start": 0, "end": 1.88, "frames": 48, "captions": false, "interlacedFrames": 0, "repeatFrames": 0},
				{"at": 0.1, "start": 60, "end": 61.88, "frames": 48, "captions": false, "interlacedFrames": 0, "repeatFrames": 0},
				{"at": 0.3, "start": 180, "end": 181.88, "frames": 48, "captions": false, "interlacedFrames": 0, "repeatFrames": 0},
				{"at": 0.5, "start": null, "end": null, "frames": 0, "captions": null},
				{"at": 0.7, "start": 420, "end": 421.88, "frames": 48, "captions": false, "interlacedFrames": 0, "repeatFrames": 0},
				{"at": 0.9, "start": 540, "end": 541.88, "frames": 48, "captions": false, "interlacedFrames": 0, "repeatFrames": 0}],
				"duration": 600, "sizeBytes": 6, "ffprobe": "7.1.1"}}`)
			if code != http.StatusOK || json.Unmarshal([]byte(body), &got) != nil || got.Original.State != "read" ||
				len(got.Original.CaptionWindows) != 6 || got.Original.Reason == nil ||
				*got.Original.Reason != "its windows at 50% decoded no frame" || got.Failures != 2 {
				t.Fatalf("a read with a window of no frame: %d %s", code, body)
			}
			if again := claimCaptions(t, h, token); len(again.Items) != 0 {
				t.Errorf("a read with a window of no frame was handed out again within its backoff: %+v", again)
			}
			storetest.Exec(t, st, `UPDATE com_nalet_katalog_sourcecaptions SET nextattemptat = now() - interval '1 minute'`)
			c = claimCaptions(t, h, token)
			if len(c.Items) != 1 {
				t.Fatalf("the claim of a read with a window of no frame, after its backoff: %+v", c)
			}
			tok = `"token": "` + c.Items[0].Token + `"`
			code, body = put(`{` + tok + `, "original": {` + read + `, "ffprobe": "7.1.1-static"},
				"package": {"versionId": "` + ccVersion + `", ` + read + `, "ffprobe": "7.1.5"}}`)
			got.Original.Reason = nil
			if code != http.StatusOK || json.Unmarshal([]byte(body), &got) != nil || got.Original.State != "read" ||
				got.Original.Captions == nil || *got.Original.Captions != (captions == "true") ||
				got.Original.FFprobe != "7.1.1-static" || got.Original.FramesDecoded == nil ||
				*got.Original.FramesDecoded != 288 || len(got.Original.CaptionWindows) != 6 || got.Original.Reason != nil ||
				got.Original.Duration == nil || *got.Original.Duration != 600 || got.Failures != 0 ||
				strings.Contains(body, `"package"`) {
				t.Fatalf("the read: %d %s", code, body)
			}
			if n := held(); n != 0 {
				t.Errorf("the read of captions %s left the retirement held (%d)", captions, n)
			}
			if c := claimCaptions(t, h, token); len(c.Items) != 0 {
				t.Errorf("an original read was handed out again: %+v", c)
			}
			if w := do(h, http.MethodPut, "/api/captions/sources/no-such-source", `{`+tok+`, "original": {`+read+
				`, "ffprobe": "7.1"}}`, token); w.Code != http.StatusNotFound {
				t.Errorf("a source there is not: %d %s", w.Code, w.Body.String())
			}
			if w := do(h, http.MethodPut, "/api/captions/sources/"+ccSource, `{`+tok+`, `+tok+`, "original": {`+read+
				`, "ffprobe": "7.1"}}`, token); w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(),
				"given twice") {
				t.Errorf("a report that gives its token twice: %d %s; want it refused", w.Code, w.Body.String())
			}
		})
	}
}

// A read of another original of the same title lets go of no retirement
// held for this one's captions.
func TestAReadOfAnotherOriginalLetsGoOfNoHold(t *testing.T) {
	h, token, _, st := ccFixture(t)
	ccAnyTime(t, st)
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_itemsources (id, item_id, filename, sizebytes, state)
		VALUES ('c1-other', $1, 'other.mkv', 6, 'present')`, ccFilm)
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_sourcecaptions (source_id, item_id, state, claimedat, claimtoken)
		VALUES ('c1-other', $1, 'pending', now(), 'other-claim')`, ccFilm)
	if w := do(h, http.MethodPut, "/api/captions/sources/c1-other", `{"token": "other-claim", "original": {`+
		windowsOf("false")+`, "ffprobe": "7.1.1"}}`, token); w.Code != http.StatusOK {
		t.Fatalf("the read of the other original: %d %s", w.Code, w.Body.String())
	}
	if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_itemsources WHERE id = $1 AND error IS NOT NULL`,
		ccSource); n != 1 {
		t.Error("a read of another original of the title let go of this one's hold")
	}
}
