package processing_test

import (
	"context"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/zaentrum/katalog-manager/internal/processing"
	"github.com/zaentrum/katalog-manager/internal/store/storetest"
)

// A worker's error reaches the table cleaned: no credential, and a long one
// cut on a character, which Postgres takes (a cut inside one it refused, and
// the report was lost).
func TestAStepsErrorIsKeptClean(t *testing.T) {
	st := storetest.Open(t)
	steps := processing.New(st.Pool())
	long := strings.Repeat("Fehler bei der Größe ", 40) // multibyte, over 500 characters
	for item, msg := range map[string]string{
		"m1": "POST http://km:hunter2@katalog/x?token=" + strings.Repeat("t", 40) + " failed",
		"m2": long,
	} {
		if err := steps.Upsert(context.Background(), item, "package", processing.StatusFailed, &msg, nil); err != nil {
			t.Fatalf("upsert %s: %v", item, err)
		}
	}
	var e1, e2 string
	if err := st.Pool().QueryRow(context.Background(), `SELECT
		(SELECT error FROM com_nalet_katalog_itemprocessingsteps WHERE item_id = 'm1'),
		(SELECT error FROM com_nalet_katalog_itemprocessingsteps WHERE item_id = 'm2')`).Scan(&e1, &e2); err != nil {
		t.Fatal(err)
	}
	if e1 != "POST http://REDACTED@katalog/x?token=REDACTED failed" {
		t.Errorf("the error kept a credential: %q", e1)
	}
	if utf8.RuneCountInString(e2) != processing.MaxErrorLength || !strings.HasSuffix(e2, "…") {
		t.Errorf("a long error: %d characters", utf8.RuneCountInString(e2))
	}
}
