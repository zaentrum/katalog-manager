package library

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

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
// original it set or cleared, and the font rows it wrote or removed.
type StyledCounts struct {
	Subtitles int `json:"subtitles"`
	Fonts     int `json:"fonts"`
}

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
func SyncStyled(ctx context.Context, q Querier, itemID, root, versionID string, pkg map[string]any) (StyledCounts, error) {
	var n StyledCounts
	if ready, err := StyledReady(ctx, q); err != nil || !ready {
		return n, err
	}
	for _, s := range jsonMaps(pkg["subtitles"]) {
		rel, ok := s["path"].(string)
		if !ok || rel == "" {
			continue
		}
		path, ok := under(root, rel)
		if !ok {
			continue
		}
		var styledPath, styledFormat *string
		if st, ok := s["styled"].(map[string]any); ok {
			p, _ := st["path"].(string)
			f, _ := st["format"].(string)
			if full, ok := under(root, p); ok && p != "" && StyledFormats[f] {
				styledPath, styledFormat = &full, &f
			}
		}
		tag, err := q.Exec(ctx, `UPDATE com_nalet_katalog_subtitleassets SET styledpath = $3, styledformat = $4
			WHERE item_id = $1 AND path = $2 AND (styledpath IS DISTINCT FROM $3 OR styledformat IS DISTINCT FROM $4)`,
			itemID, path, styledPath, styledFormat)
		if err != nil {
			return n, err
		}
		n.Subtitles += int(tag.RowsAffected())
	}
	var keep []string
	for i, f := range jsonMaps(pkg["fonts"]) {
		rel, _ := f["path"].(string)
		path, ok := under(root, rel)
		if !ok || rel == "" {
			continue
		}
		var format *string
		if v, ok := f["format"].(string); ok && FontFormats[v] {
			format = &v
		}
		names := []string{}
		for _, x := range jsonList(f["names"]) {
			if s, ok := x.(string); ok && strings.TrimSpace(s) != "" {
				names = append(names, s)
			}
		}
		namesJSON, err := json.Marshal(names)
		if err != nil {
			return n, err
		}
		id := FontID(itemID, versionID, i)
		keep = append(keep, id)
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
			id, itemID, nullable(versionID), path, format, string(namesJSON), i, optionalInt(f["sourceStreamIndex"]))
		if err != nil {
			return n, err
		}
		n.Fonts += int(tag.RowsAffected())
	}
	tag, err := q.Exec(ctx, `DELETE FROM com_nalet_katalog_fontassets WHERE item_id = $1 AND id <> ALL($2)`,
		itemID, append([]string{}, keep...))
	if err != nil {
		return n, err
	}
	n.Fonts += int(tag.RowsAffected())
	return n, nil
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
// what it changed of them, and the titles it left, why.
type StyledReport struct {
	Items     int          `json:"items"`
	Subtitles int          `json:"subtitles"`
	Fonts     int          `json:"fonts"`
	Skipped   []StyledSkip `json:"skipped"`
	Stopped   string       `json:"stopped,omitempty"`
}

// StyledSkip is a title RegisterStyled left.
type StyledSkip struct {
	ItemID string `json:"itemId"`
	Reason string `json:"reason"`
}

// RegisterStyled keeps the catalog's styled originals and fonts of the
// packages titles play as their records say (SyncStyled): for each title with
// a packaged row (kind packaged) — only those of items, when it names some —
// the package's record that row names (a version's package.json, or the
// package store's manifest.json) is read, and SyncStyled run on it in the
// title's transaction under the lock every writer of its versions takes. It
// registers what a package made before the catalog kept them carries, and
// changes nothing called again. A title whose record can't be read is left,
// with why.
func RegisterStyled(ctx context.Context, pool *pgxpool.Pool, items []string) (StyledReport, error) {
	rep := StyledReport{Skipped: []StyledSkip{}}
	sql := `SELECT item_id, path, COALESCE(versionid, '') FROM com_nalet_katalog_playbackassets WHERE kind = 'packaged'`
	args := []any{}
	if len(items) > 0 {
		sql += ` AND item_id = ANY($1)`
		args = append(args, items)
	}
	rows, err := pool.Query(ctx, sql+` ORDER BY item_id, path`, args...)
	if err != nil {
		return rep, err
	}
	type packaged struct{ item, path, version string }
	var todo []packaged
	for rows.Next() {
		var p packaged
		if err := rows.Scan(&p.item, &p.path, &p.version); err != nil {
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
		pkg, err := readPackageRecord(p.path)
		if err != nil {
			rep.Skipped = append(rep.Skipped, StyledSkip{ItemID: p.item, Reason: err.Error()})
			continue
		}
		n, err := syncStyledLocked(context.WithoutCancel(ctx), pool, p.item, filepath.Dir(p.path), p.version, pkg)
		if err != nil {
			rep.Skipped = append(rep.Skipped, StyledSkip{ItemID: p.item, Reason: "failed: " + err.Error()})
			continue
		}
		rep.Items++
		rep.Subtitles += n.Subtitles
		rep.Fonts += n.Fonts
	}
	return rep, nil
}

// syncStyledLocked is SyncStyled in a transaction of its own under the
// item's lock.
func syncStyledLocked(ctx context.Context, pool *pgxpool.Pool, itemID, root, versionID string,
	pkg map[string]any) (StyledCounts, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return StyledCounts{}, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	if err := LockItem(ctx, tx, itemID); err != nil {
		return StyledCounts{}, err
	}
	n, err := SyncStyled(ctx, tx, itemID, root, versionID, pkg)
	if err != nil {
		return n, err
	}
	return n, tx.Commit(ctx)
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
