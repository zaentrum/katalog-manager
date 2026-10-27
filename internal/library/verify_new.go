package library

import (
	"context"
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/zaentrum/katalog-manager/internal/config"
)

// The verifier of new versions. A version the packager completed carries its
// chain (.complete over package.json, package.json over checksums.sha256,
// which lists the record and every file of the package), and nothing proves
// its bytes until something hashes them: the retire job does at the moment
// it retires an original (library.verify, the chain alone within
// library.verify.maxAge of a full verification), while a list of originals
// to retire (retireOriginals) takes a version verified before. The verifier
// verifies each complete version no one verified, one at a time, the one
// completed first first, in full (every listed file's hash), and notes it as
// the fill run notes one: verifiedat the moment it began, verifiedlevel full,
// only while the version is what it read (complete, in that folder, of the
// package its package.json names, and noted by no one meanwhile). A version
// whose chain does not hold, whose checksums are pending, whose folder lies
// outside the library or whose package.json is another package's, one whose
// folder or package the catalog changed while it was read, and one whose
// note could not be written, is left as it is, said in the log, and tried
// again after VerifyNewBackoff, twice as long each time after, at most
// VerifyNewBackoffMax (the waits are the process's: a restart tries each
// again). One that went or that another verified meanwhile waits for no one.
// It reads the library and writes nothing in it, all of it on one
// connection, the one that holds its lock; a shutdown stops it at the file
// it is at, and one that comes once every file is hashed lets the note be
// written. It runs with library.layout=v2 and library.verify.new on (the
// default), every VerifyNewInterval, in one instance at a time.

// VerifyNewInterval is how often the verifier looks for versions to verify.
const VerifyNewInterval = time.Minute

// VerifyNewBackoff is how long a version the verifier left waits the first
// time; VerifyNewBackoffMax the longest it waits.
const (
	VerifyNewBackoff    = 6 * time.Hour
	VerifyNewBackoffMax = 7 * 24 * time.Hour
)

// verifyLock is the verifier's lock, of its schema.
const verifyLock = `hashtext('library-verifier/' || current_schema())`

// Verifier verifies the catalog's new versions.
type Verifier struct {
	pool *pgxpool.Pool
	cfg  config.Config
	now  func() time.Time
	read func(ctx context.Context, dir string, full bool) (*Chain, error) // VerifyVersionContext

	mu   sync.Mutex
	left map[string]leftVersion
	said string // the problem of its setting said last
}

// leftVersion is a version the verifier left: when, and how many times.
type leftVersion struct {
	at    time.Time
	times int
}

// NewVerifier is the verifier of the catalog pool, its library where cfg
// puts it.
func NewVerifier(pool *pgxpool.Pool, cfg config.Config) *Verifier {
	return &Verifier{pool: pool, cfg: cfg, now: time.Now, read: VerifyVersionContext, left: map[string]leftVersion{}}
}

// VerifyReport is what a pass did.
type VerifyReport struct {
	Verified int   // versions verified in full and noted so
	Bytes    int64 // the bytes of their packages' files
	Left     int   // versions left unverified, to be tried again after their wait
	Changed  int   // versions that went, or that another verified, while they were read
}

// backoff is how long a version left times times waits.
func backoff(times int) time.Duration {
	d := VerifyNewBackoff
	for i := 1; i < times && d < VerifyNewBackoffMax; i++ {
		d *= 2
	}
	return min(d, VerifyNewBackoffMax)
}

// Run verifies every interval until ctx ends.
func (v *Verifier) Run(ctx context.Context) {
	t := time.NewTicker(VerifyNewInterval)
	defer t.Stop()
	for {
		if _, err := v.Pass(ctx); err != nil && ctx.Err() == nil {
			log.Print("library: the verifier: " + err.Error())
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Pass verifies the versions waiting, one at a time, until none is left, the
// settings stop it (read again after each) or ctx ends (no error then), on
// one connection, under the verifier's lock: another instance verifying,
// this one does nothing.
func (v *Verifier) Pass(ctx context.Context) (VerifyReport, error) {
	rep, err := v.pass(ctx)
	if ctx.Err() != nil {
		return rep, nil
	}
	return rep, err
}

func (v *Verifier) pass(ctx context.Context) (VerifyReport, error) {
	var rep VerifyReport
	v.mu.Lock()
	defer v.mu.Unlock()
	conn, err := v.pool.Acquire(ctx)
	if err != nil {
		return rep, err
	}
	defer conn.Release()
	if on, err := v.on(ctx, conn); err != nil || !on {
		return rep, err
	}
	var locked bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock(`+verifyLock+`)`).Scan(&locked); err != nil || !locked {
		return rep, err
	}
	defer func() {
		_, _ = conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock(`+verifyLock+`)`)
	}()
	for ctx.Err() == nil {
		w, err := v.next(ctx, conn)
		if err != nil || w == nil {
			return rep, err
		}
		if err := v.verify(ctx, conn, *w, &rep); err != nil {
			return rep, err
		}
		if on, err := v.on(ctx, conn); err != nil || !on {
			return rep, err
		}
	}
	return rep, nil
}

// on reports whether the settings let the verifier verify, and says once a
// value of library.verify.new it cannot read (one it takes for on).
func (v *Verifier) on(ctx context.Context, q Querier) (bool, error) {
	set, err := ReadSettings(ctx, q)
	if err != nil {
		return false, err
	}
	problem := ""
	for _, p := range set.Problems {
		if strings.HasPrefix(p, SettingVerifyNew+" ") {
			problem = p
		}
	}
	if problem != v.said && problem != "" {
		log.Print("library: the verifier: " + problem)
	}
	v.said = problem
	return set.V2() && set.VerifyNew, nil
}

// waiting is a version the verifier verifies, as its row has it.
type waiting struct {
	id, itemID, dir, packageID string
}

// next is the complete version no one verified that was completed first, of
// those the verifier left whose wait is not over; nil when there is none.
func (v *Verifier) next(ctx context.Context, q Querier) (*waiting, error) {
	now := v.now()
	skip := make([]string, 0, len(v.left)) // never nil: NOT (id = ANY(NULL)) would match no row
	for id, l := range v.left {
		switch since := now.Sub(l.at); {
		case since < backoff(l.times):
			skip = append(skip, id)
		case since > backoff(l.times)+VerifyNewBackoffMax:
			// not read again since its wait was over: the catalog holds it
			// unverified no more
			delete(v.left, id)
		}
	}
	var w waiting
	err := q.QueryRow(ctx, `SELECT id, item_id, dir, packageid FROM com_nalet_katalog_itemversions
		WHERE state = 'complete' AND verifiedat IS NULL AND dir IS NOT NULL AND packageid IS NOT NULL
		  AND NOT (id = ANY($1::text[]))
		ORDER BY completedat NULLS LAST, id LIMIT 1`, skip).Scan(&w.id, &w.itemID, &w.dir, &w.packageID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &w, nil
}

// verify verifies the version w in full and notes it verified on q, or
// leaves it, saying why; it counts what it did in rep. An error is the
// catalog's; ctx ending stops it at the file it is at, nothing noted, and
// once every file is hashed it notes the version all the same.
func (v *Verifier) verify(ctx context.Context, q Querier, w waiting, rep *VerifyReport) error {
	leave := func(why string) {
		l := v.left[w.id]
		l.at, l.times = v.now(), l.times+1
		v.left[w.id] = l
		rep.Left++
		log.Printf("library: the verifier left version %s of %s unverified, to try again in %s: %s", w.id, w.itemID,
			DurationText(backoff(l.times)), why)
	}
	dir := filepath.Clean(w.dir)
	if !Within(PathsOf(v.cfg).Root, dir) {
		leave("its folder " + w.dir + " lies outside the library")
		return nil
	}
	began := v.now()
	c, err := v.read(ctx, dir, true)
	if err != nil && ctx.Err() != nil {
		return nil // stopped at a file: it waits for the next pass
	}
	switch {
	case err != nil:
		leave("its chain does not hold: " + err.Error())
		return nil
	case c.Pending:
		leave(ErrChecksumsPending.Error())
		return nil
	}
	if id := str(c.Package, "packageId"); id != w.packageID {
		leave(fmt.Sprintf("its package.json is package %q's, and the catalog holds package %s", id, w.packageID))
		return nil
	}
	// proven: the note is written even when ctx ends now
	note := context.WithoutCancel(ctx)
	tag, err := q.Exec(note, `UPDATE com_nalet_katalog_itemversions SET verifiedat = $5, verifiedlevel = $6,
			modifiedat = now()
		WHERE id = $1 AND item_id = $2 AND state = 'complete' AND dir = $3 AND packageid = $4 AND verifiedat IS NULL`,
		w.id, w.itemID, w.dir, w.packageID, began, VerifyFull)
	if err != nil {
		leave("its note could not be written: " + err.Error())
		return err
	}
	if tag.RowsAffected() == 1 {
		delete(v.left, w.id)
		rep.Verified++
		rep.Bytes += c.SizeBytes
		log.Printf("library: the verifier verified version %s of %s in full: %d files, %d bytes, in %s", w.id, w.itemID,
			len(c.Listed), c.SizeBytes, v.now().Sub(began).Round(time.Second))
		return nil
	}
	var state string
	var verified *time.Time
	err = q.QueryRow(note, `SELECT state, verifiedat FROM com_nalet_katalog_itemversions WHERE id = $1`, w.id).
		Scan(&state, &verified)
	switch {
	case errors.Is(err, pgx.ErrNoRows) || err == nil && (state != VersionComplete || verified != nil):
		delete(v.left, w.id)
		rep.Changed++
		what := "it is " + state + " now"
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			what = "the catalog holds it no more"
		case verified != nil:
			what = "another verified it"
		}
		log.Printf("library: the verifier did not note version %s of %s: %s", w.id, w.itemID, what)
		return nil
	case err != nil:
		leave("its row could not be read again once its note was not written: " + err.Error())
		return err
	}
	leave("the catalog changed its folder or its package while it was read")
	return nil
}
