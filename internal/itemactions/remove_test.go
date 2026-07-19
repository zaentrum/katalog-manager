package itemactions

import (
	"os"
	"path/filepath"
	"testing"
)

func TestUnderRoot(t *testing.T) {
	cases := []struct {
		root, path string
		want       bool
	}{
		{"/var/lib/katalog/media", "/var/lib/katalog/media/Movie (2020).mp4", true},
		{"/var/lib/katalog/media", "/var/lib/katalog/media/series/Show/ep.mp4", true},
		{"/var/lib/katalog/media", "/var/lib/katalog/media", false},        // the root itself is never removable
		{"/var/lib/katalog/media", "/var/lib/katalog/mediaX/file.mp4", false}, // sibling prefix trick
		{"/var/lib/katalog/media", "/etc/passwd", false},
		{"/var/lib/katalog/media", "/var/lib/katalog/media/../secrets", false}, // traversal
		{"/var/lib/katalog/media", "", false},
		{"", "/anything", false},
		{"/", "/anything", false}, // a root of "/" must never authorize removals
	}
	for _, c := range cases {
		if got := underRoot(c.root, c.path); got != c.want {
			t.Errorf("underRoot(%q, %q) = %v, want %v", c.root, c.path, got, c.want)
		}
	}
}

func TestPackageRoot(t *testing.T) {
	if got := packageRoot("/p", "movie", "ea886f9b-x"); got != filepath.Join("/p", "movies", "ea", "ea886f9b-x") {
		t.Errorf("movie root = %q", got)
	}
	if got := packageRoot("/p", "episode", "ab12"); got != filepath.Join("/p", "shows", "ab", "ab12") {
		t.Errorf("episode root = %q", got)
	}
	if got := packageRoot("/p", "series", "cd34"); got != filepath.Join("/p", "items", "cd", "cd34") {
		t.Errorf("series root = %q", got)
	}
}

func TestPruneEmptyDirs(t *testing.T) {
	base := t.TempDir()
	deep := filepath.Join(base, "series", "Show")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	// A sibling file keeps "series" non-empty after "Show" is pruned.
	sibling := filepath.Join(base, "series", "keep.txt")
	if err := os.WriteFile(sibling, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	pruneEmptyDirs(deep, base)
	if _, err := os.Stat(deep); !os.IsNotExist(err) {
		t.Error("empty Show dir should be pruned")
	}
	if _, err := os.Stat(filepath.Join(base, "series")); err != nil {
		t.Error("non-empty series dir must survive")
	}
	if _, err := os.Stat(base); err != nil {
		t.Error("the stop root must survive")
	}

	// Fully empty chain prunes up to (excluding) the stop root.
	deep2 := filepath.Join(base, "a", "b", "c")
	_ = os.MkdirAll(deep2, 0o755)
	pruneEmptyDirs(deep2, base)
	if _, err := os.Stat(filepath.Join(base, "a")); !os.IsNotExist(err) {
		t.Error("empty chain should prune to the root")
	}
}
