package store

import (
	"strings"
	"testing"

	"github.com/zaentrum/katalog-manager/db/migrations"
)

// 049 is its functions, then its indexes, each built on its own,
// CONCURRENTLY, by the name its marker line gives.
func TestCatalogSearchSplits(t *testing.T) {
	functions, indexes, err := splitCatalogSearch(migrations.CatalogSearch)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(functions, "-- 049") || !strings.Contains(functions, "DO $migration$") ||
		strings.Contains(functions, "CREATE INDEX CONCURRENTLY IF NOT EXISTS") {
		t.Errorf("the statement before the indexes is not the functions:\n%s", functions)
	}
	var names []string
	for _, ix := range indexes {
		names = append(names, ix.name)
		if strings.Count(ix.build, ";") != 1 || !strings.HasSuffix(ix.build, ";") {
			t.Errorf("%s is not one statement: %q", ix.name, ix.build)
		}
	}
	want := "idx_items_search_words idx_items_search_prefix idx_items_search_trigrams idx_items_search_scantitle " +
		"idx_items_search_plot idx_people_search_words idx_people_search_prefix idx_people_search_trigrams " +
		"idx_people_search_aka idx_itempeople_person"
	if got := strings.Join(names, " "); got != want {
		t.Errorf("indexes %q, want %q", got, want)
	}
	for _, ix := range indexes {
		if want := searchIndexNeeds[ix.name]; ix.needs != want {
			t.Errorf("%s needs %q, want %q", ix.name, ix.needs, want)
		}
	}

	for _, bad := range []string{
		"SELECT 1;\n",
		"SELECT 1;\n--@index idx_a\nCREATE INDEX CONCURRENTLY IF NOT EXISTS idx_b ON t (c);\n",
		"SELECT 1;\n--@index idx_a\nCREATE INDEX IF NOT EXISTS idx_a ON t (c);\n",
	} {
		if _, _, err := splitCatalogSearch(bad); err == nil {
			t.Errorf("split %q: no error", bad)
		}
	}
}
