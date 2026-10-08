package graph

import (
	"context"
	"testing"

	"github.com/zaentrum/katalog-manager/internal/library"
	"github.com/zaentrum/katalog-manager/internal/model"
	"github.com/zaentrum/katalog-manager/internal/store"
	"github.com/zaentrum/katalog-manager/internal/store/storetest"
)

// An episode whose file covers others lists them (covers), in episode
// order, and each of them names it (coveredBy); an episode of a file of one
// covers none and is covered by none. A scan job's report lists what its
// scan passed over and left alone; a job without one lists nothing.
func TestCoversAndAScanJobsReport(t *testing.T) {
	st := storetest.Open(t)
	withItemView(t, st)
	ctx := context.Background()
	storetest.AddItem(t, st, "s1", "series", "A Show", "")
	for _, e := range []struct {
		id string
		n  int
	}{{"e1", 1}, {"e3", 3}, {"e2", 2}, {"e4", 4}} {
		storetest.AddItem(t, st, e.id, "episode", "Episode "+e.id, "s1")
		storetest.Exec(t, st, `UPDATE com_nalet_katalog_items SET seasonnumber = 1, episodenumber = $2 WHERE id = $1`, e.id, e.n)
	}
	for _, id := range []string{"e3", "e2"} {
		if _, err := library.Link(ctx, st.Pool(), "e1", id); err != nil {
			t.Fatal(err)
		}
	}
	for id, want := range map[string]string{
		"e1": `{"item":{"coveredBy":null,"covers":[{"id":"e2","episodeNumber":2},{"id":"e3","episodeNumber":3}]}}`,
		"e2": `{"item":{"coveredBy":{"id":"e1","episodeNumber":1},"covers":[]}}`,
		"e4": `{"item":{"coveredBy":null,"covers":[]}}`,
	} {
		if got := query(t, st, `{ item(id: "`+id+`") { coveredBy { id episodeNumber } covers { id episodeNumber } } }`); got != want {
			t.Errorf("%s:\n got  %s\n want %s", id, got, want)
		}
	}
	job, err := st.StartScanJob(ctx, "nfs", "test/00000000")
	if err != nil {
		t.Fatal(err)
	}
	item := "e1"
	if err := st.FinishScanJob(ctx, job.ID, store.ScanJobResult{Status: "done", Report: []model.ScanNote{
		{Kind: model.ScanNoteUnsupported, Path: "/media/Disc.iso", Reason: "disc image: convert it to a single file"},
		{Kind: model.ScanNoteNotLinked, Path: "/media/Show.S01E01-E02.mkv", ItemID: &item, Reason: "S01E02 has a file of its own"}}}); err != nil {
		t.Fatal(err)
	}
	want := `{"scanJobs":[{"report":[{"kind":"unsupported","path":"/media/Disc.iso","itemId":null,` +
		`"reason":"disc image: convert it to a single file"},{"kind":"not-linked","path":"/media/Show.S01E01-E02.mkv","itemId":"e1",` +
		`"reason":"S01E02 has a file of its own"}]}]}`
	if got := query(t, st, `{ scanJobs { report { kind path itemId reason } } }`); got != want {
		t.Errorf("the scan jobs:\n got  %s\n want %s", got, want)
	}
	if _, err := st.StartScanJob(ctx, "nfs", "test/00000000"); err != nil {
		t.Fatal(err)
	}
	if got := query(t, st, `{ scanJobs(limit: 1) { status report { kind } } }`); got != `{"scanJobs":[{"status":"running","report":[]}]}` {
		t.Errorf("a job that runs: %s", got)
	}
}
