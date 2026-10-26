package rest

import (
	"encoding/csv"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/zaentrum/katalog-manager/internal/compliance"
	"github.com/zaentrum/katalog-manager/internal/store/storetest"
)

// The compliance report's CSV, for admins: a header row, then every title
// the filter matches that breaks a rule, read a batch at a time and not a
// page, in the order asked; numbers raw, times RFC 3339, the console's
// no-break characters plain, quoted as RFC 4180 has it and each row ended by
// CRLF; named by its kind and when the check found its titles. A kind, a
// rule, a sort or a descending it doesn't know is 400, and no CSV.
func TestTheReportIsACSV(t *testing.T) {
	st := storetest.Open(t)
	h, iss := server(t, st, testConfig(t.TempDir()))
	admin := iss.Admin(t)
	storetest.AddItem(t, st, "m1", "movie", `Dinner, "Again"`, "")
	storetest.AddItem(t, st, "m2", "movie", "A Disc", "")
	storetest.AddItem(t, st, "m3", "movie", "=Cut Above", "")
	storetest.AddItem(t, st, "s1", "series", "A Show", "")
	storetest.AddItem(t, st, "p1", "season", "Season 1", "s1")
	storetest.AddItem(t, st, "e1", "episode", "The First", "p1")
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_items SET year = 2019 WHERE id = 'm1'`)
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_items SET seasonnumber = 1, episodenumber = 2 WHERE id = 'e1'`)
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_playbackassets (id, item_id, path, isprimary) VALUES
		('a1', 'm1', '/media/m1.mkv', true), ('a2', 'm2', '/media/m2.iso', true), ('a3', 'e1', '/media/e1.mkv', true),
		('a4', 'm3', '/media/m3.mkv', true)`)
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_compliance (item_id, kind, rules, findings, originalbytes,
			packagebytes, estimatedbytes, resolution, videocodec, videobitrate, bitrateestimated, bitratecap, sizecap,
			durationseconds, packageaudio, originalaudio, ruleshash, evaluatedat) VALUES
		('m1', 'movie', '{OVER_BITRATE_CAP,NO_SURROUND}', '[{"rule": "OVER_BITRATE_CAP", "text": "-"},
			{"rule": "NO_SURROUND", "text": "-"}]', 70000000000, 65700000000, 12600000000, '3840×2160', 'HEVC', 73000000.4,
			false, 14000000, 16106127360, 7200, '2.0 AAC', $1, 'h', '2026-10-26T13:43:05Z'),
		('m2', 'movie', '{PACKAGING_FAILED}', '[{"rule": "PACKAGING_FAILED", "text": "-"}]', 40000000000, NULL, NULL,
			NULL, NULL, NULL, false, NULL, 16106127360, NULL, NULL, NULL, 'h', '2026-10-26T14:00:00Z'),
		('e1', 'episode', '{NOT_PACKAGED}', '[{"rule": "NOT_PACKAGED", "text": "-"}]', 1500000000, NULL, NULL,
			'1920×1080', 'H.264', 4539942.5, true, 8000000, NULL, 2643.5, NULL, $2, 'h', '2026-10-26T14:30:00Z'),
		('m3', 'movie', '{VIDEO_NOT_HEVC}', '[{"rule": "VIDEO_NOT_HEVC", "text": "-"}]', NULL, 2000000000, NULL,
			NULL, 'H.264', NULL, false, NULL, NULL, NULL, '-2.0 AAC', NULL, 'h', '2026-10-26T15:30:00Z')`,
		"7.1 TrueHD", "5.1 E‑AC‑3 + 2.0 AAC")
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_reencodequeue (id, item_id, state) VALUES ('q1', 'm1', 'queued'),
		('q2', 'e1', 'sent'), ('q3', 'm3', 'failed')`)
	// More titles than a batch holds, twice over: a film each, its package
	// the smaller the lower its number.
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_items (id, type, title, createdat, modifiedat)
		SELECT 'f' || lpad(n::text, 4, '0'), 'movie', 'Filler ' || n, now(), now() FROM generate_series(1, 1200) n`)
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_playbackassets (id, item_id, path, isprimary)
		SELECT 'fa' || n, 'f' || lpad(n::text, 4, '0'), '/media/f' || n || '.mkv', true FROM generate_series(1, 1200) n`)
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_compliance (item_id, kind, rules, findings, packagebytes, ruleshash,
			evaluatedat)
		SELECT 'f' || lpad(n::text, 4, '0'), 'movie', '{VIDEO_NOT_HEVC}', '[{"rule": "VIDEO_NOT_HEVC", "text": "-"}]',
			n * 1000, 'h', '2026-10-26T15:00:00Z' FROM generate_series(1, 1200) n`)

	header := strings.Join(compliance.CSVColumns, ",") + "\r\n"
	if header != "item_id,kind,title,year,series,season,episode,width,height,video_codec,video_bitrate_bps,"+
		"bitrate_estimated,bitrate_cap_bps,size_cap_bytes,duration_s,original_bytes,package_bytes,estimated_bytes,"+
		"package_audio,original_audio,rules,outcome,blocked_reason,queue,checked_at\r\n" {
		t.Errorf("the header row is %q", header)
	}
	rows := map[string]string{
		"m1": `m1,MOVIE,"Dinner, ""Again""",2019,,,,3840,2160,HEVC,73000000,false,14000000,16106127360,7200,70000000000,` +
			`65700000000,12600000000,2.0 AAC,7.1 TrueHD,OVER_BITRATE_CAP;NO_SURROUND,REENCODE,,QUEUED,2026-10-26T13:43:05Z`,
		"m2": `m2,MOVIE,A Disc,,,,,,,,,false,,16106127360,,40000000000,,,,,PACKAGING_FAILED,BLOCKED,` +
			`"its file is a disc image, which the transcoder doesn’t encode",,2026-10-26T14:00:00Z`,
		"e1": `e1,EPISODE,The First,,A Show,1,2,1920,1080,H.264,4539943,true,8000000,,2643.5,1500000000,,,,` +
			`5.1 E-AC-3 + 2.0 AAC,NOT_PACKAGED,FIRST_PACKAGE,,PROCESSING,2026-10-26T14:30:00Z`,
		// a text that begins as a formula does is a text, a ' before it
		"m3": `m3,MOVIE,'=Cut Above,,,,,,,H.264,,false,,,,,2000000000,,'-2.0 AAC,,VIDEO_NOT_HEVC,REENCODE,,NEEDS_ATTENTION,` +
			`2026-10-26T15:30:00Z`,
	}
	// ids reads a CSV's rows, each title's id in turn, and checks them.
	ids := func(what, body string) []string {
		t.Helper()
		if !strings.HasPrefix(body, header) || !strings.HasSuffix(body, "\r\n") {
			t.Fatalf("%s: no header row, or a row not ended by CRLF: %.200q", what, body)
		}
		if strings.ContainsAny(body, " ‑") {
			t.Errorf("%s holds a no-break space or hyphen", what)
		}
		records, err := csv.NewReader(strings.NewReader(body)).ReadAll()
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		var out []string
		for _, line := range strings.Split(strings.TrimSuffix(body, "\r\n"), "\r\n")[1:] {
			id, _, _ := strings.Cut(line, ",")
			if want, ok := rows[id]; ok && line != want {
				t.Errorf("%s: the row of %s is\n%s\nwant\n%s", what, id, line, want)
			}
		}
		for _, r := range records[1:] {
			if len(r) != len(compliance.CSVColumns) {
				t.Fatalf("%s: a row of %d columns: %q", what, len(r), r)
			}
			out = append(out, r[0])
		}
		return out
	}

	w := do(h, http.MethodGet, "/api/manage/compliance.csv", "", admin)
	if w.Code != http.StatusOK || w.Header().Get("Content-Type") != "text/csv; charset=utf-8; header=present" ||
		w.Header().Get("Content-Disposition") != "attachment; filename=rules-all-2026-10-26T1343Z.csv" {
		t.Fatalf("GET: %d %q %q", w.Code, w.Header().Get("Content-Type"), w.Header().Get("Content-Disposition"))
	}
	want := []string{"m1", "m3"}
	for n := 1200; n >= 1; n-- {
		want = append(want, fmt.Sprintf("f%04d", n))
	}
	want = append(want, "m2", "e1")
	if got := ids("every title", w.Body.String()); strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("every title: %d rows, %v … %v; want the %d by their package's size, the largest first, "+
			"then those without one by their original's", len(got), got[:min(3, len(got))], got[max(0, len(got)-3):], len(want))
	}

	// The same filter as the report's, and its order; the file named by the
	// kind and the oldest check of the titles of its kind and search.
	for _, tc := range []struct {
		query, name string
		first       []string // the titles of its first rows
		rows        int
	}{
		{"kind=EPISODE", "rules-episodes-2026-10-26T1430Z.csv", []string{"e1"}, 1},
		{"kind=movie&rules=NOT_PACKAGED,PACKAGING_FAILED", "rules-movies-2026-10-26T1343Z.csv", []string{"m2"}, 1},
		{"rules=NOT_PACKAGED&rules=packaging_failed&descending=false", "rules-all-2026-10-26T1343Z.csv",
			[]string{"e1", "m2"}, 2},
		{"search=dinner", "rules-all-2026-10-26T1343Z.csv", []string{"m1"}, 1},
		{"search=filler%201&sort=TITLE&descending=false", "rules-all-2026-10-26T1500Z.csv",
			[]string{"f0001", "f0010", "f0100", "f1000", "f1001", "f1002"}, 312},
		{"sort=title&search=A", "rules-all-2026-10-26T1343Z.csv", []string{"m1", "e1", "m2", "m3"}, 4},
	} {
		w := do(h, http.MethodGet, "/api/manage/compliance.csv?"+tc.query, "", admin)
		if w.Code != http.StatusOK || w.Header().Get("Content-Disposition") != "attachment; filename="+tc.name {
			t.Errorf("?%s: %d %q, want 200 named %s", tc.query, w.Code, w.Header().Get("Content-Disposition"), tc.name)
			continue
		}
		got := ids("?"+tc.query, w.Body.String())
		if len(got) != tc.rows || strings.Join(got[:min(len(got), len(tc.first))], " ") != strings.Join(tc.first, " ") {
			t.Errorf("?%s: %d rows, %v first; want %d, %v", tc.query, len(got), got[:min(len(got), len(tc.first))],
				tc.rows, tc.first)
		}
	}

	for query, says := range map[string]string{
		"kind=SHOW":       `kind is \"SHOW\": MOVIE or EPISODE`,
		"rules=LOUD":      `unknown rule \"LOUD\"`,
		"sort=LENGTH":     `unknown sort \"LENGTH\"`,
		"descending=most": `descending is \"most\": true or false`,
	} {
		w := do(h, http.MethodGet, "/api/manage/compliance.csv?"+query, "", admin)
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), says) || w.Header().Get("Content-Disposition") != "" {
			t.Errorf("?%s: %d %s, want 400 saying %s", query, w.Code, w.Body.String(), says)
		}
	}
	if w := do(h, http.MethodGet, "/api/manage/compliance.csv", "", iss.Viewer(t)); w.Code != http.StatusForbidden ||
		strings.Contains(w.Body.String(), "item_id") {
		t.Errorf("a viewer: %d %.80s, want 403 and no CSV", w.Code, w.Body.String())
	}
}
