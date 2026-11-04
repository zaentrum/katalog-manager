package library

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zaentrum/katalog-manager/internal/library/librarytest"
)

// A present source another file took the place of is superseded by that
// file's source, since now; one being retired is not, nor one superseded
// already, each saying what it is. One recorded without its quick hash gets
// it from its file while that is of the size it recorded, never from a file
// that changed.
func TestSupersede(t *testing.T) {
	f := newRetireFixture(t)
	ctx := context.Background()
	pool := f.st.Pool()
	if ready, err := SupersedeReady(ctx, pool); err != nil || !ready {
		t.Fatalf("SupersedeReady: %v, %v", ready, err)
	}
	f.execSQL(t, `UPDATE com_nalet_katalog_itemsources SET qh1 = NULL WHERE id = $1`, rtSource)
	if err := Supersede(ctx, pool, rtSource, "n-1"); err != nil {
		t.Fatal(err)
	}
	s := f.source(t, rtSource)
	_, qh1, _ := QH1(f.original)
	if s.State != SourceSuperseded || deref(s.QH1) != qh1 {
		t.Errorf("the source: %s, quick hash %v; want superseded with %s", s.State, s.QH1, qh1)
	}
	if by, err := SupersededBy(ctx, pool, rtSource); err != nil || by != "n-1" {
		t.Errorf("superseded by %q, %v", by, err)
	}
	var notPresent *ErrNotPresent
	if err := Supersede(ctx, pool, rtSource, "n-2"); !errors.As(err, &notPresent) || notPresent.State != SourceSuperseded {
		t.Errorf("a superseded source again: %v", err)
	}
	f.execSQL(t, `UPDATE com_nalet_katalog_itemsources SET state = 'retiring' WHERE id = $1`, rtSource)
	if err := Supersede(ctx, pool, rtSource, "n-2"); !errors.As(err, &notPresent) || !strings.Contains(err.Error(), "is being retired") {
		t.Errorf("a source being retired: %v", err)
	}
	// A file that changed keeps no quick hash.
	f.execSQL(t, `UPDATE com_nalet_katalog_itemsources SET state = 'present', qh1 = NULL WHERE id = $1`, rtSource)
	librarytest.Write(t, f.original, []byte("another file"))
	if err := Supersede(ctx, pool, rtSource, "n-3"); err != nil {
		t.Fatal(err)
	}
	if s := f.source(t, rtSource); s.State != SourceSuperseded || s.QH1 != nil {
		t.Errorf("a changed file's source: %s, quick hash %v; want superseded, none", s.State, s.QH1)
	}
	if by, err := SupersededBy(ctx, pool, "no-such-source"); err != nil || by != "" {
		t.Errorf("superseded by of none: %q, %v", by, err)
	}
}

// The note of what became of a title's old original, in the title's folder,
// names its source when its record is there; a title with no folder gets
// none, and that is no error.
func TestNoteReplaced(t *testing.T) {
	f := newRetireFixture(t)
	s := f.source(t, rtSource)
	at := time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC)
	id, err := NoteReplaced(f.itemDir, "replaceSource, asked by someone", s, "Example Film 4K.mkv", true, at)
	if err != nil || id == "" {
		t.Fatalf("the note: %q, %v", id, err)
	}
	evs, err := ReadEvents(f.itemDir)
	if err != nil || len(evs) != 1 {
		t.Fatalf("the events: %v, %v", evs, err)
	}
	if sid, _ := evs[0].Get("sourceId"); sid != rtSource {
		t.Errorf("the note names source %v, want %s", sid, rtSource)
	}
	// unrecorded: no source named
	s.RecordedAt = nil
	if _, err := NoteReplaced(f.itemDir, "replaceSource, asked by someone", s, "Example Film 8K.mkv", false, at.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	evs, _ = ReadEvents(f.itemDir)
	if len(evs) != 2 {
		t.Fatalf("the events: %v", evs)
	}
	if sid, ok := evs[1].Get("sourceId"); ok {
		t.Errorf("a note of a source with no record names %v", sid)
	}
	if reason, _ := evs[1].Get("reason"); !strings.Contains(reason.(string), "its original Example Film.mkv stays a source of the title") {
		t.Errorf("the note's reason: %v", reason)
	}
	gone := filepath.Join(t.TempDir(), "none")
	if id, err := NoteReplaced(gone, "x", s, "y.mkv", true, at); err != nil || id != "" {
		t.Errorf("a title with no folder: %q, %v", id, err)
	}
	if _, err := os.Stat(gone); !os.IsNotExist(err) {
		t.Errorf("the note made the folder: %v", err)
	}
}
