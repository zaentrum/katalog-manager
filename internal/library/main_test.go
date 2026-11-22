package library

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/zaentrum/katalog-manager/internal/library/librarytest"
)

// TestMain has the packages the tests write say the encoder that made their
// picture, as the packager's do (librarytest.VideoEncoder): the copy rule
// holds an original whose package says none. A read a test builds by hand,
// with no report, gets the report the analyzer would have sent of it
// (reportOfRead), which the contract's gate reads.
func TestMain(m *testing.M) {
	librarytest.VideoEncoder = "libx264"
	reportOfRead = testReport
	os.Exit(m.Run())
}

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
