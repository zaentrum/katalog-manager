// Package storetest runs tests against a real PostgreSQL.
//
// A test that calls Open is skipped unless KATALOG_TEST_DATABASE_URL names a
// database in which it may create and drop schemas, for example
//
//	KATALOG_TEST_DATABASE_URL='postgres://postgres@127.0.0.1:5432/postgres?sslmode=disable' go test ./...
//
// Each test gets a schema of its own, dropped when the test ends. It holds the
// base-schema tables this service reads and writes, lowercase as Postgres folded
// the CAP DDL, with every migration the service applies at startup, so the store
// meets the shape it meets in a deployment.
package storetest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/zaentrum/katalog-manager/internal/model"
	"github.com/zaentrum/katalog-manager/internal/store"
)

// EnvURL names the variable that points the tests at a database.
const EnvURL = "KATALOG_TEST_DATABASE_URL"

// Open returns a Store whose sessions work in a fresh schema, or skips t when
// no test database is configured.
func Open(t testing.TB) *store.Store {
	t.Helper()
	return OpenInTimeZone(t, "")
}

// OpenInTimeZone is Open with every session's TimeZone set to tz, e.g. a zone
// far from UTC to prove that a timestamp does not depend on it. An empty tz
// keeps the server's default.
func OpenInTimeZone(t testing.TB, tz string) *store.Store {
	t.Helper()
	st := open(t, tz)
	ctx := context.Background()
	if err := st.EnsureDeletionLog(ctx); err != nil {
		t.Fatalf("apply the deletion log migration: %v", err)
	}
	if err := st.EnsurePeople(ctx); err != nil {
		t.Fatalf("apply the people migration: %v", err)
	}
	if err := st.EnsureItemLockedFields(ctx); err != nil {
		t.Fatalf("apply the item locked fields migration: %v", err)
	}
	if err := st.EnsureCreditDetails(ctx); err != nil {
		t.Fatalf("apply the credit details migration: %v", err)
	}
	if err := st.EnsureStepRetries(ctx); err != nil {
		t.Fatalf("apply the step retries migration: %v", err)
	}
	if err := st.EnsureScanJobRunner(ctx); err != nil {
		t.Fatalf("apply the scan job runner migration: %v", err)
	}
	if err := st.EnsureItemRatings(ctx); err != nil {
		t.Fatalf("apply the item ratings migration: %v", err)
	}
	if err := st.EnsureTrackLanguages(ctx); err != nil {
		t.Fatalf("apply the track languages migration: %v", err)
	}
	return st
}

// OpenBase is Open without the migrations the service applies at startup: the
// base tables as a catalog older than them has them, for a test that applies
// a migration itself or proves something works without it.
func OpenBase(t testing.TB) *store.Store {
	t.Helper()
	return open(t, "")
}

func open(t testing.TB, tz string) *store.Store {
	t.Helper()
	dsn := os.Getenv(EnvURL)
	if dsn == "" {
		t.Skipf("set %s to run the tests that need PostgreSQL", EnvURL)
	}
	ctx := context.Background()
	schema := "katalog_test_" + randomHex(t, 6)

	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect to %s: %v", EnvURL, err)
	}
	defer admin.Close(ctx)
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatalf("create schema %s: %v", schema, err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		c, err := pgx.Connect(ctx, dsn)
		if err != nil {
			t.Errorf("drop schema %s: %v", schema, err)
			return
		}
		defer c.Close(ctx)
		if _, err := c.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE"); err != nil {
			t.Errorf("drop schema %s: %v", schema, err)
		}
	})

	params := map[string]string{"search_path": schema}
	if tz != "" {
		params["timezone"] = tz
	}
	st, err := store.New(ctx, withSession(dsn, params), "", "")
	if err != nil {
		t.Fatalf("open the store: %v", err)
	}
	t.Cleanup(st.Close) // runs before the schema is dropped

	if _, err := st.Pool().Exec(ctx, baseSchema); err != nil {
		t.Fatalf("create the base tables: %v", err)
	}
	return st
}

// Exec runs one statement in the test's schema and fails t on error.
func Exec(t testing.TB, st *store.Store, sql string, args ...any) {
	t.Helper()
	if _, err := st.Pool().Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("%s: %v", firstLine(sql), err)
	}
}

// Count returns the one number a query selects, e.g. a count(*).
func Count(t testing.TB, st *store.Store, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := st.Pool().QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", firstLine(sql), err)
	}
	return n
}

// AddItem inserts an item; parentID may be empty.
func AddItem(t testing.TB, st *store.Store, id, typ, title, parentID string) {
	t.Helper()
	var parent *string
	if parentID != "" {
		parent = &parentID
	}
	Exec(t, st, `INSERT INTO com_nalet_katalog_items (id, type, title, parent_id, createdat, modifiedat)
		VALUES ($1, $2, $3, $4, now(), now())`, id, typ, title, parent)
}

// facets are the tables whose rows hang off an item and go with it, each with
// a statement that gives an item ($1) one row there.
var facets = []struct{ table, insert string }{
	{"com_nalet_katalog_itemgenres", `(id, item_id, genre_id) VALUES (gen_random_uuid()::varchar, $1, 'g')`},
	{"com_nalet_katalog_itempeople", `(id, item_id, person_id, role) VALUES (gen_random_uuid()::varchar, $1, 'p', 'actor')`},
	{"com_nalet_katalog_itemtags", `(id, item_id, tag) VALUES (gen_random_uuid()::varchar, $1, 'tag')`},
	{"com_nalet_katalog_itemexternalids", `(id, item_id, source, externalid) VALUES (gen_random_uuid()::varchar, $1, 'tmdb', '1')`},
	{"com_nalet_katalog_itemartwork", `(id, item_id, kind, url) VALUES (gen_random_uuid()::varchar, $1, 'poster', 'u')`},
	{"com_nalet_katalog_itemartworkdata", `(id, item_id, kind, contenttype) VALUES (gen_random_uuid()::varchar, $1, 'poster', 'image/png')`},
	{"com_nalet_katalog_playbackassets", `(id, item_id, path) VALUES (gen_random_uuid()::varchar, $1::varchar, '/media/' || $1::varchar)`},
	{"com_nalet_katalog_subtitleassets", `(id, item_id, path) VALUES (gen_random_uuid()::varchar, $1::varchar, '/media/' || $1::varchar || '.srt')`},
	{"com_nalet_katalog_mediasegments", `(id, item_id, kind, startms, endms, source) VALUES (gen_random_uuid()::varchar, $1, 'intro', 0, 1, 'manual')`},
	{"com_nalet_katalog_itemchapters", `(id, item_id, startms, endms) VALUES (gen_random_uuid()::varchar, $1, 0, 1)`},
	{"com_nalet_katalog_itemtrailerlinks", `(id, item_id, source, url) VALUES (gen_random_uuid()::varchar, $1, 'tmdb', 'u')`},
	{"com_nalet_katalog_itemdiagnostics", `(id, item_id) VALUES (gen_random_uuid()::varchar, $1)`},
	{"com_nalet_katalog_itemprocessingsteps", `(id, item_id, step) VALUES (gen_random_uuid()::varchar, $1, 'scan')`},
	{"com_nalet_katalog_itemtracks", `(item_id, kind, ordinal, language) VALUES ($1, 'audio', 0, 'und')`},
	{"com_nalet_katalog_itemtracklanguages", `(item_id, kind, ordinal, language) VALUES ($1, 'audio', 0, 'zxx')`},
}

// AddFacets gives an item one row in every table that hangs off it.
func AddFacets(t testing.TB, st *store.Store, itemID string) {
	t.Helper()
	for _, f := range facets {
		Exec(t, st, `INSERT INTO `+f.table+` `+f.insert, itemID)
	}
}

// FacetRows counts the rows that hang off an item, across those tables.
func FacetRows(t testing.TB, st *store.Store, itemID string) int {
	t.Helper()
	n := 0
	for _, f := range facets {
		n += Count(t, st, `SELECT count(*) FROM `+f.table+` WHERE item_id = $1`, itemID)
	}
	return n
}

// Facets is how many rows AddFacets gives an item.
var Facets = len(facets)

// Deleted reads the deletion log's row for id; ok is false when there is none.
func Deleted(t testing.TB, st *store.Store, id string) (d model.DeletedItem, ok bool) {
	t.Helper()
	err := st.Pool().QueryRow(context.Background(), `SELECT id, type, title, deletedat, deletedby, reason
		FROM com_nalet_katalog_deleteditems WHERE id = $1`, id).
		Scan(&d.ID, &d.Type, &d.Title, &d.DeletedAt, &d.DeletedBy, &d.Reason)
	if errors.Is(err, pgx.ErrNoRows) {
		return d, false
	}
	if err != nil {
		t.Fatalf("read the deletion log row of %s: %v", id, err)
	}
	return d, true
}

// withSession adds session settings to dsn. pgx passes parameters it does not
// know on to the server, which applies them to every session it opens.
func withSession(dsn string, params map[string]string) string {
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		if u, err := url.Parse(dsn); err == nil {
			q := u.Query()
			for k, v := range params {
				q.Set(k, v)
			}
			u.RawQuery = q.Encode()
			return u.String()
		}
	}
	for k, v := range params {
		dsn += " " + k + "=" + v
	}
	return dsn
}

func randomHex(t testing.TB, n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i] + " …"
	}
	return s
}

// baseSchema is the part of the CAP base schema this service touches, column for
// column as the cds-generated DDL declares it (lowercase, as Postgres folded it),
// plus the processing-step upsert key a later migration added.
const baseSchema = `
CREATE TABLE com_nalet_katalog_items (
  id VARCHAR(36) NOT NULL PRIMARY KEY,
  createdat TIMESTAMP, createdby VARCHAR(255), modifiedat TIMESTAMP, modifiedby VARCHAR(255),
  type VARCHAR(20) NOT NULL,
  title VARCHAR(255) NOT NULL,
  sorttitle VARCHAR(255), year INTEGER, description TEXT, rating DECIMAL(3, 1), durationms BIGINT,
  parent_id VARCHAR(36), seasonnumber INTEGER, episodenumber INTEGER, tagline VARCHAR(500),
  metadatalocked BOOLEAN NOT NULL DEFAULT false
);
CREATE TABLE com_nalet_katalog_itemexternalids (
  id VARCHAR(36) NOT NULL PRIMARY KEY, item_id VARCHAR(36) NOT NULL,
  source VARCHAR(30) NOT NULL, externalid VARCHAR(120) NOT NULL
);
CREATE TABLE com_nalet_katalog_itemartwork (
  id VARCHAR(36) NOT NULL PRIMARY KEY, item_id VARCHAR(36) NOT NULL,
  kind VARCHAR(20) NOT NULL, url VARCHAR(2048) NOT NULL
);
CREATE TABLE com_nalet_katalog_itemartworkdata (
  id VARCHAR(36) NOT NULL PRIMARY KEY, item_id VARCHAR(36) NOT NULL,
  kind VARCHAR(20) NOT NULL, contenttype VARCHAR(80) NOT NULL, bytes BYTEA, fetchedat TIMESTAMP
);
CREATE TABLE com_nalet_katalog_playbackassets (
  id VARCHAR(36) NOT NULL PRIMARY KEY, item_id VARCHAR(36) NOT NULL,
  path VARCHAR(2048) NOT NULL, codec VARCHAR(40), resolution VARCHAR(40), bitratekbps INTEGER,
  sizebytes BIGINT, hash VARCHAR(160), isprimary BOOLEAN DEFAULT FALSE, kind VARCHAR(20) DEFAULT 'primary',
  audiocodec VARCHAR(40), audiolanguage VARCHAR(10), audiochannels INTEGER, audiobitratekbps INTEGER,
  audiotrackcount INTEGER, subtitletrackcount INTEGER, durationms BIGINT
);
CREATE TABLE com_nalet_katalog_subtitleassets (
  id VARCHAR(36) NOT NULL PRIMARY KEY, item_id VARCHAR(36) NOT NULL,
  path VARCHAR(2048) NOT NULL, format VARCHAR(10), lang VARCHAR(10), label VARCHAR(120),
  isdefault BOOLEAN DEFAULT FALSE
);
CREATE TABLE com_nalet_katalog_mediasegments (
  id VARCHAR(36) NOT NULL PRIMARY KEY,
  createdat TIMESTAMP, createdby VARCHAR(255), modifiedat TIMESTAMP, modifiedby VARCHAR(255),
  item_id VARCHAR(36) NOT NULL, kind VARCHAR(20) NOT NULL, startms BIGINT NOT NULL, endms BIGINT NOT NULL,
  source VARCHAR(30) NOT NULL, confidence DECIMAL(3, 2), label VARCHAR(120)
);
CREATE TABLE com_nalet_katalog_itemtrailerlinks (
  id VARCHAR(36) NOT NULL PRIMARY KEY,
  createdat TIMESTAMP, createdby VARCHAR(255), modifiedat TIMESTAMP, modifiedby VARCHAR(255),
  item_id VARCHAR(36) NOT NULL, source VARCHAR(20) NOT NULL, site VARCHAR(40), externalid VARCHAR(120),
  url VARCHAR(2048) NOT NULL, title VARCHAR(255), durationsec INTEGER, publishedat TIMESTAMP,
  downloadedat TIMESTAMP, localpath VARCHAR(2048)
);
CREATE TABLE com_nalet_katalog_itemdiagnostics (
  id VARCHAR(36) NOT NULL PRIMARY KEY, item_id VARCHAR(36) NOT NULL,
  generatedat TIMESTAMP, sourcepath VARCHAR(2048), sourcesize BIGINT, sourcemtime TIMESTAMP,
  ffprobedata TEXT, folderlisting TEXT, notes VARCHAR(1024)
);
CREATE TABLE com_nalet_katalog_itemprocessingsteps (
  id VARCHAR(36) NOT NULL PRIMARY KEY,
  createdat TIMESTAMP, createdby VARCHAR(255), modifiedat TIMESTAMP, modifiedby VARCHAR(255),
  item_id VARCHAR(36) NOT NULL, step VARCHAR(20) NOT NULL, status VARCHAR(20) NOT NULL DEFAULT 'pending',
  startedat TIMESTAMP, finishedat TIMESTAMP, attempts INTEGER DEFAULT 0, error VARCHAR(500), details TEXT
);
CREATE UNIQUE INDEX idx_processingsteps_item_step ON com_nalet_katalog_itemprocessingsteps (item_id, step);
CREATE TABLE com_nalet_katalog_genres (
  id VARCHAR(36) NOT NULL PRIMARY KEY, name VARCHAR(80) NOT NULL
);
CREATE TABLE com_nalet_katalog_itemgenres (
  id VARCHAR(36) NOT NULL PRIMARY KEY, item_id VARCHAR(36) NOT NULL, genre_id VARCHAR(36) NOT NULL
);
CREATE TABLE com_nalet_katalog_people (
  id VARCHAR(36) NOT NULL PRIMARY KEY, name VARCHAR(255) NOT NULL
);
CREATE TABLE com_nalet_katalog_itempeople (
  id VARCHAR(36) NOT NULL PRIMARY KEY, item_id VARCHAR(36) NOT NULL,
  person_id VARCHAR(36) NOT NULL, role VARCHAR(40) NOT NULL
);
CREATE TABLE com_nalet_katalog_itemtags (
  id VARCHAR(36) NOT NULL PRIMARY KEY, item_id VARCHAR(36) NOT NULL, tag VARCHAR(120) NOT NULL
);
CREATE TABLE com_nalet_katalog_itemchapters (
  id VARCHAR(36) NOT NULL PRIMARY KEY,
  createdat TIMESTAMP, createdby VARCHAR(255), modifiedat TIMESTAMP, modifiedby VARCHAR(255),
  item_id VARCHAR(36) NOT NULL, startms BIGINT NOT NULL, endms BIGINT NOT NULL, title VARCHAR(120),
  ordinal INTEGER
);
CREATE TABLE com_nalet_katalog_settings (
  id VARCHAR(36) NOT NULL PRIMARY KEY,
  createdat TIMESTAMP, createdby VARCHAR(255), modifiedat TIMESTAMP, modifiedby VARCHAR(255),
  key VARCHAR(120) NOT NULL, valuetext VARCHAR(2000) NOT NULL DEFAULT '',
  valuetype VARCHAR(20) NOT NULL DEFAULT 'string', description TEXT
);
CREATE TABLE com_nalet_katalog_scanjobs (
  id VARCHAR(36) NOT NULL PRIMARY KEY, source VARCHAR(20) NOT NULL,
  status VARCHAR(20) NOT NULL DEFAULT 'queued', startedat TIMESTAMP, finishedat TIMESTAMP,
  errormessage TEXT, filesseen INTEGER DEFAULT 0, itemsinserted INTEGER DEFAULT 0, itemsupdated INTEGER DEFAULT 0
);
CREATE TABLE com_nalet_katalog_enrichmentstatuscodes (
  code VARCHAR(20) NOT NULL PRIMARY KEY, name VARCHAR(40)
);
`
