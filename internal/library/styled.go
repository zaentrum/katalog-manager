package library

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// StyledFormats are the formats of a subtitle's styled original the catalog
// keeps: an ASS or an SSA script (libass reads either; its [Script Info] says
// which), as package.json names it (subtitles[].styled.format).
var StyledFormats = map[string]bool{"ass": true}

// FontFormats are the formats a package's font file is by its bytes
// (fonts[].format); a font of none of them keeps no format.
var FontFormats = map[string]bool{"ttf": true, "otf": true, "ttc": true, "woff": true, "woff2": true}

// StyledCounts is what SyncStyled changed: the subtitle rows whose styled
// original it set or cleared, and the font rows it wrote or removed; and
// Left, the styled originals and fonts of the record it did not keep, each
// logged: one the catalog cannot hold as the record gives it, or whose write
// failed. Left out, they are what POST /api/library/styled keeps once they
// can be.
type StyledCounts struct {
	Subtitles int `json:"subtitles"`
	Fonts     int `json:"fonts"`
	Left      int `json:"left"`
}

// pathWidth is how many characters a path column of the catalog holds
// (styledpath, the fonts' path).
const pathWidth = 2048

// FontID is the id of the font of a package at its place among the
// package's fonts, ordinal from 0: a name-based id, so the same font of the
// same package keeps its id, and its URL, however often it is written. A
// font of the package store's (no version) is the title's at that place.
func FontID(itemID, versionID string, ordinal int) string {
	if versionID == "" {
		versionID = "store"
	}
	return IDOf(strings.Join([]string{itemID, "font", versionID, strconv.Itoa(ordinal)}, ":"))
}

// SyncStyled keeps, in q, what the catalog holds of a package's styled
// originals and fonts as the package's record says (package.json, or the
// package store's manifest.json, pkg; root is the folder its paths are
// relative to, versionID its version, "" for the store's): each of the item's
// subtitle rows that names a subtitle of the package, by its file, has the
// styled original that subtitle names beside it (styledpath, styledformat),
// and none where it names none; the item's font rows are the package's fonts
// (FontID), a row kept where it is the same, written where it is new or
// changed, removed where the package has it no more. A subtitle row of no
// file of the package — a sidecar's, an attached subtitle's — is left as it
// is, and so is a path that leaves root. It answers what it changed: called
// again, nothing. On a catalog without migration 053 it changes nothing.
//
// What the catalog cannot hold as the record gives it is left out, logged,
// and the rest kept: a path or a font's name with a NUL, which PostgreSQL's
// text holds none of, a path longer than its column, an attachment's index no
// integer column holds. In a transaction each write is a savepoint of its
// own, so one that fails anyway takes back its own changes alone; only a
// transaction that cannot be taken back to its savepoint ends it with an
// error.
func SyncStyled(ctx context.Context, q Querier, itemID, root, versionID string, pkg map[string]any) (StyledCounts, error) {
	var n StyledCounts
	if ready, err := StyledReady(ctx, q); err != nil || !ready {
		return n, err
	}
	of := "the package store's package"
	if versionID != "" {
		of = "version " + versionID
	}
	leave := func(what, why string) {
		n.Left++
		log.Printf("library: %s of %s, %s, is not kept: %s", what, itemID, of, why)
	}
	for i, s := range jsonMaps(pkg["subtitles"]) {
		rel, ok := s["path"].(string)
		if !ok || rel == "" {
			continue
		}
		path, ok := under(root, rel)
		if !ok {
			continue
		}
		what := "subtitle " + strconv.Itoa(i)
		if id, ok := s["id"].(string); ok && id != "" {
			what = "subtitle " + strconv.Quote(id)
		}
		var styledPath, styledFormat *string
		if st, ok := s["styled"].(map[string]any); ok {
			p, _ := st["path"].(string)
			f, _ := st["format"].(string)
			if full, ok := under(root, p); ok && p != "" && StyledFormats[f] {
				styledPath, styledFormat = &full, &f
			}
		}
		// a subtitle whose own path the catalog cannot hold has no row to
		// give a styled original to
		if why := unkeepable(path, pathWidth); why != "" {
			if styledPath != nil {
				leave("the styled original of "+what, "the subtitle's path "+why)
			}
			continue
		}
		if styledPath != nil {
			if why := unkeepable(*styledPath, pathWidth); why != "" {
				leave("the styled original of "+what, "its path "+why)
				styledPath, styledFormat = nil, nil
			}
		}
		var changed int64
		lost, err := step(ctx, q, func(q Querier) error {
			tag, err := q.Exec(ctx, `UPDATE com_nalet_katalog_subtitleassets SET styledpath = $3, styledformat = $4
				WHERE item_id = $1 AND path = $2 AND (styledpath IS DISTINCT FROM $3 OR styledformat IS DISTINCT FROM $4)`,
				itemID, path, styledPath, styledFormat)
			changed = tag.RowsAffected()
			return err
		})
		if lost {
			return n, err
		}
		if err != nil {
			leave("the styled original of "+what, err.Error())
			continue
		}
		n.Subtitles += int(changed)
	}
	keep := []string{}
	for i, f := range jsonMaps(pkg["fonts"]) {
		rel, _ := f["path"].(string)
		path, ok := under(root, rel)
		if !ok || rel == "" {
			continue
		}
		what := "font " + strconv.Itoa(i)
		if why := unkeepable(path, pathWidth); why != "" {
			leave(what, "its path "+why)
			continue
		}
		var format *string
		if v, ok := f["format"].(string); ok && FontFormats[v] {
			format = &v
		}
		names, why := []string{}, ""
		for _, x := range jsonList(f["names"]) {
			if s, ok := x.(string); ok && strings.TrimSpace(s) != "" {
				if bad := unkeepable(s, 0); bad != "" && why == "" {
					why = "its name " + strconv.Quote(s) + " " + bad
				}
				names = append(names, s)
			}
		}
		index := optionalInt(f["sourceStreamIndex"])
		if index != nil && (*index < math.MinInt32 || *index > math.MaxInt32) {
			why = fmt.Sprintf("its attachment's index %d is none an integer column holds", *index)
		}
		if why != "" {
			leave(what, why)
			continue
		}
		namesJSON, err := json.Marshal(names)
		if err != nil {
			leave(what, err.Error())
			continue
		}
		id := FontID(itemID, versionID, i)
		var changed int64
		lost, err := step(ctx, q, func(q Querier) error {
			tag, err := q.Exec(ctx, `INSERT INTO com_nalet_katalog_fontassets
					(id, item_id, versionid, path, format, names, ordinal, sourcestreamindex)
				VALUES ($1, $2, $3, $4, $5, $6::jsonb, $7, $8)
				ON CONFLICT (id) DO UPDATE SET item_id = EXCLUDED.item_id, versionid = EXCLUDED.versionid,
					path = EXCLUDED.path, format = EXCLUDED.format, names = EXCLUDED.names, ordinal = EXCLUDED.ordinal,
					sourcestreamindex = EXCLUDED.sourcestreamindex
				WHERE (com_nalet_katalog_fontassets.item_id, com_nalet_katalog_fontassets.versionid,
					com_nalet_katalog_fontassets.path, com_nalet_katalog_fontassets.format, com_nalet_katalog_fontassets.names,
					com_nalet_katalog_fontassets.ordinal, com_nalet_katalog_fontassets.sourcestreamindex)
					IS DISTINCT FROM (EXCLUDED.item_id, EXCLUDED.versionid, EXCLUDED.path, EXCLUDED.format, EXCLUDED.names,
					EXCLUDED.ordinal, EXCLUDED.sourcestreamindex)`,
				id, itemID, nullable(versionID), path, format, string(namesJSON), i, index)
			changed = tag.RowsAffected()
			return err
		})
		if lost {
			return n, err
		}
		if err != nil {
			leave(what, err.Error())
			continue
		}
		keep = append(keep, id)
		n.Fonts += int(changed)
	}
	var removed int64
	lost, err := step(ctx, q, func(q Querier) error {
		tag, err := q.Exec(ctx, `DELETE FROM com_nalet_katalog_fontassets WHERE item_id = $1 AND id <> ALL($2)`,
			itemID, keep)
		removed = tag.RowsAffected()
		return err
	})
	if lost {
		return n, err
	}
	if err != nil {
		leave("the fonts it carries no more", err.Error())
	}
	n.Fonts += int(removed)
	return n, nil
}

// KeepStyled is SyncStyled for a writer of a package's rows that must not
// fail for what it keeps of the package's styled originals and fonts —
// packaging-complete, an adoption: in a savepoint of tx, so that whatever
// fails of it takes back its own writes alone, and the writer goes on with
// the rest. What it could not keep is logged; POST /api/library/styled
// keeps it once it can be.
func KeepStyled(ctx context.Context, tx pgx.Tx, itemID, root, versionID string, pkg map[string]any) StyledCounts {
	sp, err := tx.Begin(ctx)
	if err != nil {
		log.Printf("library: the styled originals and fonts of %s are not kept: %v", itemID, err)
		return StyledCounts{}
	}
	n, err := SyncStyled(ctx, sp, itemID, root, versionID, pkg)
	if err == nil {
		err = sp.Commit(ctx)
	}
	if err != nil {
		_ = sp.Rollback(ctx)
		log.Printf("library: the styled originals and fonts of %s are not kept: %v", itemID, err)
		return StyledCounts{}
	}
	return n
}

// step runs write in a savepoint of q when q is a transaction, so that a
// write that fails takes back its own changes alone and the transaction goes
// on; on the pool each statement is its own already. It answers the write's
// error, and lost true when the transaction could not be taken back to the
// savepoint either: nothing more can be written in it.
func step(ctx context.Context, q Querier, write func(Querier) error) (lost bool, err error) {
	tx, ok := q.(pgx.Tx)
	if !ok {
		return false, write(q)
	}
	sp, err := tx.Begin(ctx)
	if err != nil {
		return true, err
	}
	if err := write(sp); err != nil {
		if rerr := sp.Rollback(ctx); rerr != nil {
			return true, err
		}
		return false, err
	}
	if err := sp.Commit(ctx); err != nil {
		return true, err
	}
	return false, nil
}

// unkeepable says why the catalog cannot keep text in a column as it is: a
// NUL, which PostgreSQL's text and jsonb hold none of, or more than width
// characters (0: any number); "" when it can.
func unkeepable(text string, width int) string {
	if strings.ContainsRune(text, 0) {
		return "holds a NUL"
	}
	if width > 0 && utf8.RuneCountInString(text) > width {
		return fmt.Sprintf("is longer than the %d characters its column holds", width)
	}
	return ""
}

// StyledReady reports whether migration 053 is in place: a subtitle's styled
// original (styledpath, styledformat) and the table of a package's fonts.
func StyledReady(ctx context.Context, q Querier) (bool, error) {
	var ok bool
	err := q.QueryRow(ctx, `SELECT to_regclass('com_nalet_katalog_fontassets') IS NOT NULL
		AND (SELECT count(*) = 2 FROM pg_attribute WHERE attrelid = to_regclass('com_nalet_katalog_subtitleassets')
			AND attname IN ('styledpath', 'styledformat') AND NOT attisdropped)`).Scan(&ok)
	return ok, err
}

// under is rel, a path a package's record names relative to its folder,
// joined to root: false for a path that is absolute or leaves root.
func under(root, rel string) (string, bool) {
	if rel == "" || filepath.IsAbs(rel) || strings.HasPrefix(rel, "/") {
		return "", false
	}
	full := filepath.Join(root, filepath.FromSlash(rel))
	return full, Within(root, full)
}

func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func jsonList(v any) []any {
	l, _ := v.([]any)
	return l
}

// optionalInt is a JSON number as an integer, nil for anything else.
func optionalInt(v any) *int64 {
	if v == nil {
		return nil
	}
	if n, ok := jsonInt(v); ok {
		return &n
	}
	return nil
}

// StyledReport is what RegisterStyled did: the titles whose package it read,
// what it changed of them and left out of their records (StyledCounts), and
// the titles it left, why.
type StyledReport struct {
	Items     int          `json:"items"`
	Subtitles int          `json:"subtitles"`
	Fonts     int          `json:"fonts"`
	Left      int          `json:"left"`
	Skipped   []StyledSkip `json:"skipped"`
	Stopped   string       `json:"stopped,omitempty"`
}

// StyledSkip is a title RegisterStyled left.
type StyledSkip struct {
	ItemID string `json:"itemId"`
	Reason string `json:"reason"`
}

// packagedRow is a title's packaged row as RegisterStyled read it: the
// row's id, the record it names, and the version that is ("" the package
// store's).
type packagedRow struct{ item, id, path, version string }

// RegisterStyled keeps the catalog's styled originals and fonts of the
// packages titles play as their records say (SyncStyled): for each title with
// a packaged row (kind packaged) — only those of items, when it names some —
// the package's record that row names (a version's package.json, or the
// package store's manifest.json) is read, and SyncStyled run on it in the
// title's transaction under the lock every writer of its versions takes,
// once that transaction has found the title and the same packaged row again
// and holds them (FOR KEY SHARE, FOR SHARE) to its end: a title packaged
// again or deleted while its record was read is left, as the writer that
// changed it keeps its rows, and none of it is written. It registers what a
// package made before the catalog kept them carries, and changes nothing
// called again. A title whose record can't be read is left, with why.
func RegisterStyled(ctx context.Context, pool *pgxpool.Pool, items []string) (StyledReport, error) {
	rep := StyledReport{Skipped: []StyledSkip{}}
	sql := `SELECT item_id, id, path, COALESCE(versionid, '') FROM com_nalet_katalog_playbackassets WHERE kind = 'packaged'`
	args := []any{}
	if len(items) > 0 {
		sql += ` AND item_id = ANY($1)`
		args = append(args, items)
	}
	rows, err := pool.Query(ctx, sql+` ORDER BY item_id, path`, args...)
	if err != nil {
		return rep, err
	}
	var todo []packagedRow
	for rows.Next() {
		var p packagedRow
		if err := rows.Scan(&p.item, &p.id, &p.path, &p.version); err != nil {
			rows.Close()
			return rep, err
		}
		todo = append(todo, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return rep, err
	}
	for _, p := range todo {
		if ctx.Err() != nil {
			rep.Stopped = "the caller went away: the titles left are not read"
			return rep, nil
		}
		// read before the title's lock is taken: a record read may wait on the
		// storage, and nothing waits on it meanwhile; the lock's transaction
		// then holds the title to the row that named it
		pkg, err := readPackageRecord(p.path)
		if err != nil {
			rep.Skipped = append(rep.Skipped, StyledSkip{ItemID: p.item, Reason: err.Error()})
			continue
		}
		n, left, err := syncStyledLocked(context.WithoutCancel(ctx), pool, p, pkg)
		switch {
		case err != nil:
			rep.Skipped = append(rep.Skipped, StyledSkip{ItemID: p.item, Reason: "failed: " + err.Error()})
			continue
		case left != "":
			rep.Skipped = append(rep.Skipped, StyledSkip{ItemID: p.item, Reason: left})
			continue
		}
		rep.Items++
		rep.Subtitles += n.Subtitles
		rep.Fonts += n.Fonts
		rep.Left += n.Left
	}
	return rep, nil
}

// syncStyledLocked is SyncStyled in a transaction of its own under the
// item's lock, once the transaction has found the item and its packaged row p
// as they were read, and holds both to its end: the item against a delete
// (FOR KEY SHARE), the row against any change (FOR SHARE). It answers why it
// wrote nothing when either is gone or changed.
func syncStyledLocked(ctx context.Context, pool *pgxpool.Pool, p packagedRow, pkg map[string]any) (StyledCounts, string, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return StyledCounts{}, "", err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	if err := LockItem(ctx, tx, p.item); err != nil {
		return StyledCounts{}, "", err
	}
	var id string
	err = tx.QueryRow(ctx, `SELECT id FROM com_nalet_katalog_items WHERE id = $1 FOR KEY SHARE`, p.item).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return StyledCounts{}, "the title was deleted while its package's record was read", nil
	}
	if err != nil {
		return StyledCounts{}, "", err
	}
	var path, version string
	err = tx.QueryRow(ctx, `SELECT path, COALESCE(versionid, '') FROM com_nalet_katalog_playbackassets
		WHERE id = $1 AND item_id = $2 AND kind = 'packaged' FOR SHARE`, p.id, p.item).Scan(&path, &version)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && (path != p.path || version != p.version) {
		return StyledCounts{}, "the title was packaged again while its package's record was read: " +
			"its packaging keeps the package it plays now, and called again this registers it", nil
	}
	if err != nil {
		return StyledCounts{}, "", err
	}
	n, err := SyncStyled(ctx, tx, p.item, filepath.Dir(p.path), p.version, pkg)
	if err != nil {
		return n, "", err
	}
	return n, "", tx.Commit(ctx)
}

// readPackageRecord reads the package's record at path: a JSON object, its
// numbers kept as they are written.
func readPackageRecord(path string) (map[string]any, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("its package's record %s is not there", path)
		}
		return nil, fmt.Errorf("its package's record cannot be read: %v", err)
	}
	var doc map[string]any
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.UseNumber()
	if err := dec.Decode(&doc); err != nil || doc == nil {
		return nil, fmt.Errorf("its package's record %s is no JSON object", path)
	}
	return doc, nil
}
