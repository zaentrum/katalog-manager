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
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

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
	if err := st.EnsureDeletionLog(ctx); err != nil {
		t.Fatalf("apply the deletion log migration: %v", err)
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
`
