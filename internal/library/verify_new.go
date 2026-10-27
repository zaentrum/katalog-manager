package library

import (
	"context"
	"errors"
	"fmt"
	"log"
	"path/filepath"
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
// it retires an original (library.verify), while a list of originals to
// retire (retireOriginals) takes a version verified before. The verifier
// verifies each complete version no one verified, one at a time, the one
// completed first first, in full (every listed file's hash), and notes it as
// the fill run notes one: verifiedat the moment it began, verifiedlevel full,
// only while the version is what it read (complete, in that folder, of the
// package its package.json names, and noted by no one meanwhile). A version
// whose chain does not hold, whose checksums are pending, whose folder lies
// outside the library or whose package.json is another package's, and one
// whose row changed while it was read, is left as it is, said in the log,
// and tried again after VerifyNewBackoff. It reads the library and writes
// nothing in it. It runs with library.layout=v2 and library.verify.new on
// (the default), every VerifyNewInterval, in one instance at a time.

// VerifyNewInterval is how often the verifier looks for versions to verify.
const VerifyNewInterval = time.Minute

// VerifyNewBackoff is how long a version the verifier left waits before it
// is tried again.
const VerifyNewBackoff = 6 * time.Hour

// Verifier verifies the catalog's new versions.
type Verifier struct {
	pool *pgxpool.Pool
	cfg  config.Config
	now  func() time.Time
	read func(dir string, full bool) (*Chain, error) // VerifyVersion

	mu   sync.Mutex
	left map[string]time.Time // a version the verifier left → when
}

// NewVerifier is the verifier of the catalog pool, its library where cfg
// puts it.
func NewVerifier(pool *pgxpool.Pool, cfg config.Config) *Verifier {
	return &Verifier{pool: pool, cfg: cfg, now: time.Now, read: VerifyVersion, left: map[string]time.Time{}}
}

// VerifyReport is what a pass did.
type VerifyReport struct {
	Verified int   // versions verified in full and noted so
	Bytes    int64 // the bytes of their packages' files
	Left     int   // versions left unverified, to be tried again after VerifyNewBackoff
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
// settings stop it (read again after each) or ctx ends, under the verifier's
// lock: another instance verifying, this one does nothing.
func (v *Verifier) Pass(ctx context.Context) (VerifyReport, error) {
	var rep VerifyReport
	if on, err := v.on(ctx); err != nil || !on {
		return rep, err
	}
	conn, err := v.pool.Acquire(ctx)
	if err != nil {
		return rep, err
	}
	defer conn.Release()
	var locked bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock(hashtext('library-verifier'))`).Scan(&locked); err != nil || !locked {
		return rep, err
	}
	defer func() {
		_, _ = conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock(hashtext('library-verifier'))`)
	}()
	v.mu.Lock()
	defer v.mu.Unlock()
	for ctx.Err() == nil {
		w, err := v.next(ctx)
		if err != nil || w == nil {
			return rep, err
		}
		if err := v.verify(ctx, *w, &rep); err != nil {
			return rep, err
		}
		if on, err := v.on(ctx); err != nil || !on {
			return rep, err
		}
	}
	return rep, nil
}

// on reports whether the settings let the verifier verify.
func (v *Verifier) on(ctx context.Context) (bool, error) {
	set, err := ReadSettings(ctx, v.pool)
	if err != nil {
		return false, err
	}
	return set.V2() && set.VerifyNew, nil
}

// waiting is a version the verifier verifies, as its row has it.
type waiting struct {
	id, itemID, dir, packageID string
}

// next is the complete version no one verified that was completed first, of
// those the verifier did not leave within VerifyNewBackoff; nil when there is
// none.
func (v *Verifier) next(ctx context.Context) (*waiting, error) {
	now := v.now()
	skip := make([]string, 0, len(v.left)) // never nil: NOT (id = ANY(NULL)) would match no row
	for id, at := range v.left {
		if now.Sub(at) < VerifyNewBackoff {
			skip = append(skip, id)
		} else {
			delete(v.left, id)
		}
	}
	var w waiting
	err := v.pool.QueryRow(ctx, `SELECT id, item_id, dir, packageid FROM com_nalet_katalog_itemversions
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

// verify verifies the version w in full and notes it verified, or leaves it,
// saying why; it counts what it did in rep. An error is the catalog's.
func (v *Verifier) verify(ctx context.Context, w waiting, rep *VerifyReport) error {
	leave := func(why string) {
		v.left[w.id] = v.now()
		rep.Left++
		log.Printf("library: the verifier left version %s of %s unverified, to try again in %s: %s", w.id, w.itemID,
			DurationText(VerifyNewBackoff), why)
	}
	dir := filepath.Clean(w.dir)
	if !Within(PathsOf(v.cfg).Root, dir) {
		leave("its folder " + w.dir + " lies outside the library")
		return nil
	}
	began := v.now()
	c, err := v.read(dir, true)
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
	tag, err := v.pool.Exec(ctx, `UPDATE com_nalet_katalog_itemversions SET verifiedat = $5, verifiedlevel = $6,
			modifiedat = now()
		WHERE id = $1 AND item_id = $2 AND state = 'complete' AND dir = $3 AND packageid = $4 AND verifiedat IS NULL`,
		w.id, w.itemID, w.dir, w.packageID, began, VerifyFull)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		leave("its row changed while it was read")
		return nil
	}
	rep.Verified++
	rep.Bytes += c.SizeBytes
	log.Printf("library: the verifier verified version %s of %s in full: %d files, %d bytes, in %s", w.id, w.itemID,
		len(c.Listed), c.SizeBytes, v.now().Sub(began).Round(time.Second))
	return nil
}
