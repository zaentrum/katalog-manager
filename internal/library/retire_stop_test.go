package library

import (
	"context"
	"errors"
	"testing"

	"github.com/zaentrum/katalog-manager/internal/store/storetest"
)

// A list whose context ends as it checks a title (a shutdown) records
// nothing of the title: the run goes on with it once it is worked again, and
// says of it what a check that ran to its end says.
func TestAListStoppedAsItChecksATitleChecksItAgain(t *testing.T) {
	for _, dry := range []bool{true, false} {
		f := newListFixture(t)
		req := ListRequest{Items: []string{rtFilm}, DryRun: true, By: "an-admin"}
		if !dry {
			run, _ := f.dry(t, rtFilm)
			req = ListRequest{FromRun: run.ID, By: "an-admin"}
		}
		ctx, cancel := context.WithCancel(context.Background())
		// the compliance check is asked right before the title's place is read
		f.gate.turn = func(string) (string, bool) {
			cancel()
			return "", false
		}
		if _, err := f.l.Start(context.Background(), req); err != nil {
			t.Fatalf("dry %v: Start: %v", dry, err)
		}
		_ = f.l.Work(ctx)
		if run, items := f.latest(t); run.State != RunRunning || len(items) != 1 || items[0].Result != nil {
			t.Fatalf("dry %v: stopped as it checks the title: %+v, %s", dry, run, said(items[0]))
		}
		f.gate.turn = nil
		if err := f.l.Work(context.Background()); err != nil {
			t.Fatal(err)
		}
		run, items := f.latest(t)
		switch {
		case dry && (run.State != RunDone || !wouldRetire(items[0])):
			t.Errorf("the dry run worked again: %+v, %s", run, said(items[0]))
		case !dry && (run.State != RunDone || said(items[0]) != "retired: "):
			t.Errorf("the run worked again: %+v, %s", run, said(items[0]))
		}
	}
}

// The retire path says nothing once its context ended: what failed may be
// the ending itself. The source and the retire step are as they were.
func TestTheRetirePathSaysNothingOnceItsContextEnded(t *testing.T) {
	f := newRetireFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rt := &retirement{src: f.source(t, rtSource), itemDir: f.itemDir, auth: byPolicy}
	if err := f.r.fail(ctx, rt, "its version does not verify (full): context canceled"); !errors.Is(err, context.Canceled) {
		t.Errorf("fail once the context ended: %v", err)
	}
	if s := f.source(t, rtSource); s.State != SourcePresent || s.Error != nil {
		t.Errorf("the source: %s, %v", s.State, deref(s.Error))
	}
	if n := storetest.Count(t, f.st, `SELECT count(*) FROM com_nalet_katalog_itemprocessingsteps
		WHERE item_id = $1 AND step = 'retire' AND status = 'failed'`, rtFilm); n != 0 {
		t.Errorf("the retire step failed %d times", n)
	}
	if err := f.r.fail(context.Background(), rt, "a failure"); err == nil || f.source(t, rtSource).Error == nil {
		t.Errorf("fail with its context: %v", err)
	}
}
