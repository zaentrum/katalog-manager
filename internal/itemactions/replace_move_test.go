package itemactions

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/zaentrum/katalog-manager/internal/graph"
)

// A move's undo takes back what it made alone: its link in the arrivals goes
// while it is the file handed over still; a name there that is another file
// now, or the last name of the file handed over, whose name in .work/replace
// went, stays.
func TestAMovesUndoTakesBackItsOwnLinkAlone(t *testing.T) {
	dir := t.TempDir()
	from, to := filepath.Join(dir, "replace", "Film.mkv"), filepath.Join(dir, "incoming", "Film.mkv")
	write := func(path, content string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	read := func(path string) string {
		b, err := os.ReadFile(path)
		if err != nil {
			return "-"
		}
		return string(b)
	}
	write(from, "the file handed over")
	mv, err := reserve(from, to)
	if err != nil {
		t.Fatal(err)
	}
	mv.undo()
	if read(to) != "-" || read(from) != "the file handed over" {
		t.Fatalf("undone: the arrivals' name %q, the name handed over %q", read(to), read(from))
	}
	// another file at the name meanwhile
	if mv, err = reserve(from, to); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(to); err != nil {
		t.Fatal(err)
	}
	write(to, "another's file")
	mv.undo()
	if read(to) != "another's file" {
		t.Errorf("another's file at the name: %q after the undo", read(to))
	}
	// the name handed over gone: the arrivals' is the file's last
	if err := os.Remove(to); err != nil {
		t.Fatal(err)
	}
	if mv, err = reserve(from, to); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(from); err != nil {
		t.Fatal(err)
	}
	mv.undo()
	if read(to) != "the file handed over" {
		t.Errorf("the file's last name: %q after the undo", read(to))
	}
}

// On a filesystem that links no file, the file handed over is renamed into
// the arrivals onto a name taken first: a free name takes it, and the undo
// hands it back; a name there already is refused (SOURCE_CONFLICT), nothing
// moved; a file another puts at the name meanwhile finds it taken, and is
// never written over; a rename that fails leaves no name behind.
func TestAFilesystemThatLinksNoFileHasTheFileRenamedOntoANameTakenFirst(t *testing.T) {
	defer func(l, r func(string, string) error) { linkFile, renameFile = l, r }(linkFile, renameFile)
	linkFile = func(from, to string) error { return &os.LinkError{Op: "link", Old: from, New: to, Err: syscall.EPERM} }
	dir := t.TempDir()
	from, to := filepath.Join(dir, "replace", "Film.mkv"), filepath.Join(dir, "incoming", "Film.mkv")
	write := func(path, content string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	read := func(path string) string {
		b, err := os.ReadFile(path)
		if err != nil {
			return "-"
		}
		return string(b)
	}
	write(from, "the file handed over")
	mv, err := reserve(from, to)
	if err != nil || !mv.renamed || read(to) != "the file handed over" || read(from) != "-" {
		t.Fatalf("a free name: %+v, %v; the arrivals' %q, the name handed over %q", mv, err, read(to), read(from))
	}
	mv.undo()
	if read(from) != "the file handed over" || read(to) != "-" {
		t.Fatalf("undone: the name handed over %q, the arrivals' %q", read(from), read(to))
	}

	write(to, "another's file")
	var refused *graph.SourceRefused
	if _, err := reserve(from, to); !errors.As(err, &refused) || refused.Code != codeSourceConflict {
		t.Errorf("a name there already: %v", err)
	}
	if read(to) != "another's file" || read(from) != "the file handed over" {
		t.Errorf("after the refusal: the arrivals' %q, the name handed over %q", read(to), read(from))
	}
	if err := os.Remove(to); err != nil {
		t.Fatal(err)
	}

	// another puts its file at the name while the file is renamed there
	var theirs error
	renameFile = func(a, b string) error {
		f, err := os.OpenFile(b, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if theirs = err; err == nil {
			_, _ = f.WriteString("another's file")
			_ = f.Close()
		}
		return os.Rename(a, b)
	}
	if mv, err = reserve(from, to); err != nil || !errors.Is(theirs, os.ErrExist) {
		t.Fatalf("another's file meanwhile: %v; another's create %v, want it to find the name taken", err, theirs)
	}
	if read(to) != "the file handed over" {
		t.Errorf("the arrivals' name holds %q", read(to))
	}
	mv.undo()

	renameFile = func(string, string) error { return &os.LinkError{Op: "rename", Err: syscall.EIO} }
	if _, err := reserve(from, to); !errors.As(err, &refused) || refused.Code != codeSourceBusy {
		t.Errorf("a rename that fails: %v", err)
	}
	if read(to) != "-" || read(from) != "the file handed over" {
		t.Errorf("after a rename that failed: the arrivals' %q, the name handed over %q", read(to), read(from))
	}
}
