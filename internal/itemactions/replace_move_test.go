package itemactions

import (
	"os"
	"path/filepath"
	"testing"
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
