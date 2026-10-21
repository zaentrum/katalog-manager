package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/zaentrum/katalog-manager/db/migrations"
)

// The catalog's search (db/migrations/049_catalog_search.sql): the functions
// that normalize a title, a name or a plot, and the indexes katalog-api's
// ranked search reads them by. katalog-api searches as before until every
// index it needs is valid, so the service runs without them, and builds them
// in the background (EnsureCatalogSearch).

// searchIndex is one of 049's indexes: its name, the statement that builds it
// CONCURRENTLY, and the column an earlier migration adds that it indexes, as
// table.column ("" when it needs none).
type searchIndex struct {
	name, build, needs string
}

// searchIndexNeeds are the columns of earlier migrations that an index of 049
// reads: a series' scantitle (046) and a person's other names (030). Without
// the column the index is not built; katalog-api then leaves that tier out.
var searchIndexNeeds = map[string]string{
	"idx_items_search_scantitle": "com_nalet_katalog_items.scantitle",
	"idx_people_search_aka":      "com_nalet_katalog_people.alsoknownas",
}

// catalogSearchFunctions are 049's functions, by signature.
var catalogSearchFunctions = []string{
	"com_nalet_katalog_search_text(text)", "com_nalet_katalog_search_joined(text)",
	"com_nalet_katalog_search_words(text)", "com_nalet_katalog_search_plot(text)",
}

// indexMarker starts the lines of 049 that build one index: "--@index <name>".
const indexMarker = "--@index "

// splitCatalogSearch splits 049 into the statement that comes before its
// indexes (the functions) and its indexes, each with the statement after its
// marker line.
func splitCatalogSearch(sql string) (functions string, indexes []searchIndex, err error) {
	var head strings.Builder
	var cur *searchIndex
	var body strings.Builder
	flush := func() {
		if cur != nil {
			cur.build = strings.TrimSpace(body.String())
			indexes = append(indexes, *cur)
			body.Reset()
		}
	}
	for _, line := range strings.SplitAfter(sql, "\n") {
		if strings.HasPrefix(line, indexMarker) {
			flush()
			name := strings.TrimSpace(strings.TrimPrefix(line, indexMarker))
			cur = &searchIndex{name: name, needs: searchIndexNeeds[name]}
			continue
		}
		if cur == nil {
			head.WriteString(line)
		} else {
			body.WriteString(line)
		}
	}
	flush()
	for _, ix := range indexes {
		if ix.name == "" || !strings.Contains(ix.build, " "+ix.name+" ") ||
			!strings.HasPrefix(ix.build, "CREATE INDEX CONCURRENTLY IF NOT EXISTS ") {
			return "", nil, fmt.Errorf("049: the statement after %q builds no index of that name", indexMarker+ix.name)
		}
	}
	if len(indexes) == 0 {
		return "", nil, errors.New("049: no index")
	}
	return strings.TrimSpace(head.String()), indexes, nil
}

// indexState is how far an index of 049 is: missing, invalid (a build that
// was cut short, which reads nothing and must be built again) or valid.
type indexState int

const (
	indexMissing indexState = iota
	indexInvalid
	indexValid
)

// searchIndexStates reads the state of each index named, and whether each
// column an index needs is there.
func searchIndexStates(ctx context.Context, q querier, indexes []searchIndex) (map[string]indexState, map[string]bool, error) {
	names := make([]string, len(indexes))
	var cols []string
	for i, ix := range indexes {
		names[i] = ix.name
		if ix.needs != "" {
			cols = append(cols, ix.needs)
		}
	}
	states := make(map[string]indexState, len(names))
	rows, err := q.Query(ctx, `SELECT n.name, x.indexrelid IS NOT NULL, COALESCE(x.indisvalid AND x.indisready, false)
		FROM unnest($1::text[]) AS n(name) LEFT JOIN pg_index x ON x.indexrelid = to_regclass(n.name)`, names)
	if err != nil {
		return nil, nil, err
	}
	for rows.Next() {
		var (
			name         string
			exists, good bool
		)
		if err := rows.Scan(&name, &exists, &good); err != nil {
			rows.Close()
			return nil, nil, err
		}
		switch {
		case good:
			states[name] = indexValid
		case exists:
			states[name] = indexInvalid
		default:
			states[name] = indexMissing
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	have := map[string]bool{}
	rows, err = q.Query(ctx, `SELECT c.name FROM unnest($1::text[]) AS c(name)
		JOIN pg_attribute a ON a.attrelid = to_regclass(split_part(c.name, '.', 1))
		 AND a.attname = split_part(c.name, '.', 2) AND a.attnum > 0 AND NOT a.attisdropped`, cols)
	if err != nil {
		return nil, nil, err
	}
	found, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, nil, err
	}
	for _, c := range found {
		have[c] = true
	}
	return states, have, nil
}

// searchFunctionsReady reports whether every function of 049 is there.
func searchFunctionsReady(ctx context.Context, q querier) (bool, error) {
	var ok bool
	err := q.QueryRow(ctx, `SELECT bool_and(to_regprocedure(f) IS NOT NULL) FROM unnest($1::text[]) AS f`,
		catalogSearchFunctions).Scan(&ok)
	return ok, err
}

// CatalogSearchReady reports whether migration 049 is in place: its functions,
// and every one of its indexes valid whose column is there (the index of
// 046's scantitle needs that column, the one of 030's other names that one).
func (s *Store) CatalogSearchReady(ctx context.Context) (bool, error) {
	_, indexes, err := splitCatalogSearch(migrations.CatalogSearch)
	if err != nil {
		return false, err
	}
	if ok, err := searchFunctionsReady(ctx, s.pool); err != nil || !ok {
		return false, err
	}
	states, have, err := searchIndexStates(ctx, s.pool, indexes)
	if err != nil {
		return false, err
	}
	for _, ix := range indexes {
		if (ix.needs == "" || have[ix.needs]) && states[ix.name] != indexValid {
			return false, nil
		}
	}
	return true, nil
}

// catalogSearchLock is the advisory lock (with the schema's hash) that one
// instance holds while it builds 049, so that two never build an index at once.
const catalogSearchLock = 49

// EnsureCatalogSearch applies db/migrations/049_catalog_search.sql where any of
// it is missing: the functions, then each index on its own, CONCURRENTLY, so
// that a build neither blocks the catalog's writes nor rewrites the table. A
// build that was cut short (a restart in the middle of one) left its index
// invalid: it is dropped (CONCURRENTLY too) and built again. Each step is
// said through logf.
//
// It works on a connection of its own, outside the pool: a build waits for
// the transactions that began before it, which can take a while, and holds
// the connection meanwhile. One instance builds at a time: another waits for
// the advisory lock, asking for it every few seconds, so that it holds no
// snapshot a build of the first would wait for. The session runs without a
// statement timeout, which a build may outlast.
func (s *Store) EnsureCatalogSearch(ctx context.Context, logf func(format string, args ...any)) error {
	functions, indexes, err := splitCatalogSearch(migrations.CatalogSearch)
	if err != nil {
		return err
	}
	if ready, err := s.CatalogSearchReady(ctx); err != nil || ready {
		return err
	}
	conn, err := pgx.ConnectConfig(ctx, s.pool.Config().ConnConfig.Copy())
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer conn.Close(context.Background())
	if _, err := conn.Exec(ctx, `SET statement_timeout = 0`); err != nil {
		return err
	}
	for {
		var locked bool
		if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1, hashtext(current_schema()))`,
			catalogSearchLock).Scan(&locked); err != nil {
			return err
		}
		if locked {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
	defer conn.Exec(context.Background(), `SELECT pg_advisory_unlock($1, hashtext(current_schema()))`, catalogSearchLock)

	// Another instance may have built it all while this one waited.
	if ok, err := searchFunctionsReady(ctx, conn); err != nil {
		return err
	} else if !ok {
		// No arguments, so pgx sends it over the simple protocol: one statement.
		if _, err := conn.Exec(ctx, functions); err != nil {
			return fmt.Errorf("the search functions: %w", err)
		}
		logf("catalog search (049): the functions are in place")
	}
	// gin_trgm_ops is pg_trgm's, which may be in a schema the search_path
	// leaves out: it is named with that schema.
	var trgm string
	if err := conn.QueryRow(ctx, `SELECT quote_ident(n.nspname) FROM pg_extension e
		JOIN pg_namespace n ON n.oid = e.extnamespace WHERE e.extname = 'pg_trgm'`).Scan(&trgm); err != nil {
		return fmt.Errorf("the schema of pg_trgm: %w", err)
	}
	states, have, err := searchIndexStates(ctx, conn, indexes)
	if err != nil {
		return err
	}
	for _, ix := range indexes {
		if ix.needs != "" && !have[ix.needs] {
			logf("catalog search (049): %s is left out: the catalog has no %s", ix.name, ix.needs)
			continue
		}
		switch states[ix.name] {
		case indexValid:
			continue
		case indexInvalid:
			if _, err := conn.Exec(ctx, `DROP INDEX CONCURRENTLY IF EXISTS `+ix.name); err != nil {
				return fmt.Errorf("drop the invalid %s: %w", ix.name, err)
			}
			logf("catalog search (049): dropped %s, which a build cut short had left invalid", ix.name)
		}
		build := strings.ReplaceAll(ix.build, " gin_trgm_ops", " "+trgm+".gin_trgm_ops")
		start := time.Now()
		if _, err := conn.Exec(ctx, build); err != nil {
			return fmt.Errorf("build %s: %w", ix.name, err)
		}
		logf("catalog search (049): built %s in %s", ix.name, time.Since(start).Round(time.Millisecond))
	}
	return nil
}
