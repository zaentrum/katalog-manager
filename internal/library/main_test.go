package library

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/zaentrum/katalog-manager/internal/library/librarytest"
)

// TestMain has the packages the tests write say the encoder that made their
// picture, as the packager's do (librarytest.VideoEncoder): the copy rule
// holds an original whose package says none. A read a test builds by hand,
// with no report, gets the report the analyzer would have sent of it
// (reportOfRead), which the contract's gate reads; and a source record a
// test writes by hand, of what the gate reads alone, gets what the record
// logic writes beside it before the contract's schema reads it
// (fixtureRecordCheck). The tests of the schema's own rule use
// sourceRecordRefused.
func TestMain(m *testing.M) {
	librarytest.VideoEncoder = "libx264"
	reportOfRead = testReport
	sourceRecordCheck = fixtureRecordCheck
	os.Exit(m.Run())
}

// fixtureID is the source id a record a test writes is given where its own
// (and its folder's) is none the schema takes (a, b, s1: no UUID).
const fixtureID = "f1f1f1f1-0000-4000-8000-00000000f1f1"

// fixtureRecordCheck is sourceRecordRefused of the record rec of the folder
// folder as the record logic would have written it: the facts the schema
// requires that rec doesn't give (fixtureFacts, and those of its file), and,
// where rec names no id or one the schema takes for none, an id that names
// its folder.
func fixtureRecordCheck(rec Doc, folder string) string {
	full := append(Doc(nil), rec...)
	for _, f := range fixtureFacts {
		if !pyHas(full, f.Key) {
			full = append(full, f)
		}
	}
	if file, ok := pyGet(full, "file").(Doc); ok {
		for _, f := range fixtureFileFacts {
			if !pyHas(file, f.Key) {
				file = append(file, f)
			}
		}
		full = full.With("file", file)
	}
	id, _ := pyGet(full, "sourceId").(string)
	if !pyHas(full, "sourceId") || (id == folder && !uuidRE.MatchString(id)) {
		full, folder = full.With("sourceId", fixtureID), fixtureID
		if uuidRE.MatchString(id) {
			full, folder = full.With("sourceId", id), id
		}
	}
	return sourceRecordRefused(full, folder)
}

// fixtureFacts are the record logic's facts of a source record the gate
// doesn't read, as it writes them of a file it took.
var fixtureFacts = Doc{
	{"takenAt", "2026-10-01T10:00:00Z"},
	{"takenBy", "analyzer test"},
	{"file", Doc{{"name", "original.mkv"}, {"sizeBytes", json.Number("1")}}},
	{"fidelity", Doc{{"class", "original"}, {"fingerprint", nil}, {"evidence", []any{}}}},
	{"probe", Doc{{"tool", "ffprobe"}, {"version", nil}, {"at", "2026-10-01T10:00:00Z"}, {"file", nil}, {"sha256", nil},
		{"note", nil}}},
}

// fixtureFileFacts are those of its file.
var fixtureFileFacts = Doc{{"kind", "stream-container"}, {"mtime", "2026-10-01T09:00:00Z"},
	{"fixity", Doc{{"qh1", "sha256:" + strings.Repeat("0", 64)}}}, {"part", nil}}

// testReport is the report of the read r as the analyzer sends one: its
// windows, its span and its gop where it says them.
func testReport(r *CaptionsRead) []byte {
	rep := map[string]any{"captionWindows": r.Windows}
	if r.Windows == nil {
		rep["captionWindows"] = []any{}
	}
	if r.Span != nil {
		rep["span"] = map[string]any{"start": r.Span.Start, "end": r.Span.End}
	}
	if r.Gop != nil {
		rep["gop"] = *r.Gop
	}
	b, err := json.Marshal(rep)
	if err != nil {
		return nil
	}
	return b
}
