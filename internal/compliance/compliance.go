// Package compliance holds the library's titles against the rules the
// pipeline's workers report they run with, and keeps what it finds.
//
// The transcoder and the packager report their rules at their start and
// every ten minutes (ReportRules): the transcoder its caps, its 2160 bucket,
// its encoder and its ladder, the packager its 5.1 companion, its audio bit
// rates and the image subtitles it carries. A package made before a rule came
// keeps what the pipeline did then — a 4K original's video copied as it was,
// a source's surround left out — and nothing packages it again by itself.
// The check finds each movie and episode that breaks a rule reported now:
//
//   - NOT_PACKAGED: its original is present, it has no package, and no
//     transcode or package waits or runs;
//   - PACKAGING_FAILED: no package, and its transcode or its package failed
//     or was skipped (a disc image is not encoded at all);
//   - OVER_BITRATE_CAP: its package's video is above the transcoder's cap of
//     its kind and bucket;
//   - OVER_SIZE_CAP: a movie whose original is above the transcoder's file
//     size cap and whose package copies its video;
//   - NO_SURROUND: the packager makes a 5.1 companion, and the package lacks
//     the surround its source had;
//   - IMAGE_SUBTITLES_MISSING: its source has image subtitles of a form the
//     packager carries, and its package carries none in their language;
//   - VIDEO_NOT_HEVC: its package's video is not HEVC, which the
//     transcoder's rules make it.
//
// A rule of a worker that never reported is not checked, and nothing assumes
// a worker's defaults: the report says what is not checked (notEvaluated).
//
// The check reads the catalog's tables and, with the library's v2 layout,
// the small records of a packaged title's version (versions/<id>/
// package.json) and of its source (sources/<id>/source.json), never a media
// file, a few at a time: the library lies on slow storage. What it finds is
// kept (migration 051), and the report reads that, so it answers at once
// however large the library. A pass in the background checks again the
// titles that need it, one instance at a time: every title when nothing was
// found yet, every title once a night, the titles found against rules that
// changed since (each row keeps the hash of the rules it was checked against),
// those an admin asks for (Refresh), a title whose package completed or whose
// pipeline ended (Touch), and a title the catalog gained.
//
// The check never queues a title: an admin has what it finds processed again
// through the re-encode queue (GraphQL queueRework, queueReworkMatching).
package compliance

import (
	"context"
	"errors"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/zaentrum/katalog-manager/internal/config"
	"github.com/zaentrum/katalog-manager/internal/graph"
	"github.com/zaentrum/katalog-manager/internal/library"
	"github.com/zaentrum/katalog-manager/internal/store"
)

// readers is how many records the check reads at once.
const readers = 8

// batch is how many titles a pass checks at once.
const batch = 200

// tick is how often the background pass looks for titles to check again.
const tick = time.Minute

// nightly is when, in the service's local time, every title is checked again,
// once a day.
const nightly = 3 * time.Hour

// passLock is the advisory lock a pass holds, the catalog's (its schema's):
// one instance checks at a time.
const passLock = `hashtext('library-compliance/' || current_schema())`

// Service is the library's compliance check.
type Service struct {
	st    *store.Store
	pool  *pgxpool.Pool
	paths library.Paths
	now   func() time.Time
	// pokes wakes the background pass: titles wait to be checked again.
	pokes chan struct{}
	// running is set while a pass runs in this instance.
	running atomic.Bool
	// ready51 is set once migration 051, and those the check reads by, are
	// seen in place (none is undone under a running service).
	ready51 atomic.Bool
	// said is what the pass last said went wrong, said once each time it
	// changes (under mu).
	mu   sync.Mutex
	said string
	// retries says the sweep that retries a failed step runs.
	retries bool
}

// New is the compliance check of st's catalog, reading the library's records
// where cfg puts them.
func New(st *store.Store, cfg config.Config) *Service {
	return &Service{st: st, pool: st.Pool(), paths: library.PathsOf(cfg), now: time.Now, pokes: make(chan struct{}, 1),
		retries: cfg.RetryInterval > 0}
}

var _ graph.Compliance = (*Service)(nil)

// ready says why the catalog keeps no check, nil when it does: migration 051
// is not in place, or one the check reads by is not (033, a step's retries,
// and 040, a title's originals and versions).
func (s *Service) ready(ctx context.Context) error {
	if s.ready51.Load() {
		return nil
	}
	for _, m := range []struct {
		ready func(context.Context) (bool, error)
		why   error
	}{
		{s.st.ComplianceReady, ErrNotReady},
		{s.st.StepRetriesReady, errors.New("migration 033 (db/migrations/033_step_retries.sql) is not applied")},
		{s.st.LibraryReady, errors.New("migration 040 (db/migrations/040_library_v2.sql) is not applied")},
	} {
		ok, err := m.ready(ctx)
		if err != nil {
			return err
		}
		if !ok {
			return m.why
		}
	}
	s.ready51.Store(true)
	return nil
}

// poke wakes the background pass, without waiting.
func (s *Service) poke() {
	select {
	case s.pokes <- struct{}{}:
	default:
	}
}

// Touch has the titles ids checked again on the next pass: their package
// completed, or their pipeline ended. A title the check holds no row of yet
// gets one that waits for it (no rule found, nothing checked). It is
// best-effort: what it cannot mark the nightly pass checks.
func (s *Service) Touch(ctx context.Context, ids ...string) {
	if len(ids) == 0 || s.ready(ctx) != nil {
		return
	}
	if _, err := s.pool.Exec(context.WithoutCancel(ctx), `INSERT INTO `+store.ComplianceTable+` AS c
			(item_id, kind, ruleshash, staleat)
		SELECT i.id, lower(i.type), '', now() FROM com_nalet_katalog_items i
		WHERE i.id = ANY($1) AND lower(i.type) IN ('movie', 'episode')
		ON CONFLICT (item_id) DO UPDATE SET staleat = now()`, ids); err != nil {
		log.Printf("compliance: %d titles could not be marked to be checked again: %v", len(ids), err)
		return
	}
	s.poke()
}

// Refresh has every title of kind ("" every kind) checked again by the next
// pass, which it wakes. It answers true: the pass is set going.
func (s *Service) Refresh(ctx context.Context, kind string) (bool, error) {
	if why := s.ready(ctx); why != nil {
		return false, why
	}
	if _, err := s.pool.Exec(ctx, `UPDATE `+store.ComplianceTable+` SET staleat = now()
		WHERE staleat IS NULL AND ($1 = '' OR kind = $1)`, kind); err != nil {
		return false, err
	}
	s.poke()
	return true, nil
}

// Evaluating reports whether a pass runs, in this instance or in another.
func (s *Service) Evaluating(ctx context.Context) (bool, error) {
	if s.running.Load() {
		return true, nil
	}
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return false, err
	}
	defer conn.Release()
	var free bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock(`+passLock+`)`).Scan(&free); err != nil {
		return false, err
	}
	if free {
		_, err = conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock(`+passLock+`)`)
	}
	return !free, err
}

// Run checks the titles that need it now and on every tick, and whenever it
// is woken, until ctx ends.
func (s *Service) Run(ctx context.Context) {
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		if why := s.ready(ctx); why != nil {
			s.say("compliance: the library's compliance is not checked: " + why.Error())
		} else if n, err := s.Pass(ctx); err != nil {
			s.say("compliance: a pass stopped: " + err.Error())
		} else {
			s.say("")
			if n > 0 {
				log.Printf("compliance: %d titles checked against the workers' rules", n)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-s.pokes:
		}
	}
}

// say logs msg once each time it changes; "" clears it.
func (s *Service) say(msg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if msg != s.said && msg != "" {
		log.Print(msg)
	}
	s.said = msg
}

// cutoff is when the nightly pass last fell due: the last nightly moment of
// the service's local time at or before now. A title checked before it is
// checked again.
func cutoff(now time.Time) time.Time {
	l := now.Local()
	at := time.Date(l.Year(), l.Month(), l.Day(), 0, 0, 0, 0, l.Location()).Add(nightly)
	if at.After(l) {
		at = at.AddDate(0, 0, -1)
	}
	return at
}
