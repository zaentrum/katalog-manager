package main

import (
	"bytes"
	"context"
	"log"
	"os"
	"strings"
	"testing"

	"github.com/zaentrum/katalog-manager/internal/store/storetest"
)

// At startup the empty job tables of the retired integration go, and the log
// says which in one line; one that holds rows is kept, and the log says so
// once, with its rows; with nothing to drop the log says nothing.
func TestTheStartupSaysWhatItDidWithTheRetiredJobTables(t *testing.T) {
	var logged bytes.Buffer
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	st := storetest.Open(t)
	storetest.Exec(t, st, `CREATE TABLE com_nalet_katalog_downloadjobs (id VARCHAR(36) PRIMARY KEY, adapter VARCHAR(40) NOT NULL,
		clientjobid VARCHAR(255) NOT NULL);
		CREATE VIEW katalogservice_downloadjobs AS SELECT id FROM com_nalet_katalog_downloadjobs;
		CREATE TABLE com_nalet_katalog_trailerjobs (id VARCHAR(36) PRIMARY KEY, item_id VARCHAR(36) NOT NULL);
		INSERT INTO com_nalet_katalog_downloadjobs VALUES ('d1', 'x', '1'), ('d2', 'x', '2'), ('d3', 'x', '3')`)

	dropRetiredJobTables(context.Background(), st)
	lines := strings.Split(strings.TrimSpace(logged.String()), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], "dropped the empty job tables of a retired integration (migration 035): com_nalet_katalog_trailerjobs") ||
		!strings.Contains(lines[1], "kept the job tables of a retired integration that hold rows: com_nalet_katalog_downloadjobs (3 rows)") {
		t.Errorf("the startup's log:\n%s", logged.String())
	}
	if n := storetest.Count(t, st, `SELECT count(*) FROM katalogservice_downloadjobs`); n != 3 {
		t.Errorf("the kept table's view reads %d rows, want 3", n)
	}

	logged.Reset()
	storetest.Exec(t, st, `DELETE FROM com_nalet_katalog_downloadjobs`)
	dropRetiredJobTables(context.Background(), st)
	if got := strings.TrimSpace(logged.String()); strings.Count(got, "\n") != 0 ||
		!strings.Contains(got, "dropped the empty job tables of a retired integration (migration 035): com_nalet_katalog_downloadjobs") {
		t.Errorf("the next start's log, the table emptied:\n%s", got)
	}
	logged.Reset()
	dropRetiredJobTables(context.Background(), st)
	if logged.Len() != 0 {
		t.Errorf("a start with nothing to drop logged:\n%s", logged.String())
	}
}
