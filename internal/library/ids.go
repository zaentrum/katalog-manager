package library

import (
	"context"
	"crypto/rand"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// NewID is a new id for a record: a random (version 4) lower-case UUID.
func NewID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return format(b)
}

// IDOf is the id a name always has: a name-based (version 5, SHA-1) UUID in
// the library's namespace, so that a run done again names what it writes the
// same.
func IDOf(name string) string {
	h := sha1.New()
	h.Write(namespace[:])
	h.Write([]byte(name))
	var b [16]byte
	copy(b[:], h.Sum(nil))
	b[6] = b[6]&0x0f | 0x50
	b[8] = b[8]&0x3f | 0x80
	return format(b)
}

// namespace is the library's namespace, as its tools name it:
// uuid5(NAMESPACE_URL, "https://zaentrum.github.io/schemas/library").
var namespace = func() [16]byte {
	url := [16]byte{0x6b, 0xa7, 0xb8, 0x11, 0x9d, 0xad, 0x11, 0xd1, 0x80, 0xb4, 0x00, 0xc0, 0x4f, 0xd4, 0x30, 0xc8}
	h := sha1.New()
	h.Write(url[:])
	h.Write([]byte("https://zaentrum.github.io/schemas/library"))
	var b [16]byte
	copy(b[:], h.Sum(nil))
	b[6] = b[6]&0x0f | 0x50
	b[8] = b[8]&0x3f | 0x80
	return b
}()

func format(b [16]byte) string {
	s := hex.EncodeToString(b[:])
	return fmt.Sprintf("%s-%s-%s-%s-%s", s[0:8], s[8:12], s[12:16], s[16:20], s[20:32])
}

// LockItem holds, until tx ends, the item's lock that every writer of its
// versions and sources takes: packaging-complete, the retire job, the flip.
func LockItem(ctx context.Context, tx pgx.Tx, itemID string) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('com_nalet_katalog_library:' || $1))`, itemID)
	return err
}

// LegacyDay is the legacy folder of the day t: <WORK>/legacy/<YYYYMMDD>,
// what was taken out of the record that day, deleted after its grace.
func (p Paths) LegacyDay(t time.Time) string {
	return p.LegacyDir() + "/" + t.UTC().Format("20060102")
}
