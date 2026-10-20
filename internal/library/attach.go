package library

import (
	"bytes"
	"context"
	"errors"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The ways an attached subtitle came to be (com_nalet_katalog_subtitleassets
// .origin, migration 048).
const (
	// OriginSource: it came with the title's file, and was not taken in with it.
	OriginSource = "source"
	// OriginProvider: it was found elsewhere, made for this file or one like it.
	OriginProvider = "provider"
	// OriginTranscribed: made from the title's audio by speech recognition.
	OriginTranscribed = "transcribed"
	// OriginTranslated: made from another of the title's subtitles by translation.
	OriginTranslated = "translated"
	// OriginConverted: another script of one of the title's subtitles.
	OriginConverted = "converted"
)

// Origins are the ways an attached subtitle came to be.
var Origins = []string{OriginSource, OriginProvider, OriginTranscribed, OriginTranslated, OriginConverted}

// Subtitle is a subtitle of a title as the attach seam says it: one of the
// title's own (its package's, a file beside its source), whose Origin is nil,
// or one attached to it.
type Subtitle struct {
	ID         string     `json:"id"`
	Language   string     `json:"language"`
	Label      *string    `json:"label"`
	Format     string     `json:"format"`
	Forced     bool       `json:"forced"`
	SDH        bool       `json:"sdh"`
	Origin     *string    `json:"origin"`
	Attached   bool       `json:"attached"`
	VersionID  *string    `json:"versionId,omitempty"`
	AttachedBy *string    `json:"attachedBy,omitempty"`
	AttachedAt *time.Time `json:"attachedAt,omitempty"`
	Path       string     `json:"-"`
}

// Attachment is a subtitle to attach to a title: its WebVTT file
// (webvtt.Normalize), its language tag (languages.Tag), what it says of
// itself and who attaches it (the client of the caller's token).
type Attachment struct {
	VTT      []byte
	Language string
	Label    *string
	Forced   bool
	SDH      bool
	Origin   string
	By       string
}

// AttachLabel is the label of an attached subtitle that was given none: what
// a viewer is told of it besides its language — "SDH" for one for deaf and
// hard-of-hearing viewers, "AI" for one a model made (transcribed,
// translated), both "SDH · AI" — and nil when there is nothing to tell. A
// player names it by its language and this ("German · AI").
func AttachLabel(origin string, sdh bool) *string {
	var parts []string
	if sdh {
		parts = append(parts, "SDH")
	}
	if origin == OriginTranscribed || origin == OriginTranslated {
		parts = append(parts, "AI")
	}
	if len(parts) == 0 {
		return nil
	}
	label := strings.Join(parts, " · ")
	return &label
}

// NotAttachable says why a title takes no attached subtitle.
type NotAttachable struct{ Reason string }

func (e *NotAttachable) Error() string { return e.Reason }

// The refusals of a subtitle's removal.
var (
	// ErrNoSuchSubtitle: the title has no subtitle of that id.
	ErrNoSuchSubtitle = errors.New("the title has no subtitle of that id")
	// ErrNotAttached: the subtitle is the title's own, its package's or a file
	// beside its source, which no caller removes.
	ErrNotAttached = errors.New("the subtitle is the title's own (its package's, or a file beside its source): only one attached to it can be removed")
	// ErrAttachedByOther: another caller attached it.
	ErrAttachedByOther = errors.New("another client attached the subtitle: only it, or an admin, can remove it")
)

const subtitleCols = `id, COALESCE(lang, ''), label, COALESCE(format, ''), COALESCE(isforced, false), COALESCE(issdh, false),
	origin, versionid, attachedby, attachedat, path`

func scanSubtitle(row pgx.Row, s *Subtitle) error {
	if err := row.Scan(&s.ID, &s.Language, &s.Label, &s.Format, &s.Forced, &s.SDH, &s.Origin, &s.VersionID,
		&s.AttachedBy, &s.AttachedAt, &s.Path); err != nil {
		return err
	}
	s.Attached = s.Origin != nil
	return nil
}

// SubtitlesOf lists the subtitles of the item id: its own first, then the
// ones attached to it, each by language.
func SubtitlesOf(ctx context.Context, q Querier, id string) ([]Subtitle, error) {
	rows, err := q.Query(ctx, `SELECT `+subtitleCols+` FROM com_nalet_katalog_subtitleassets WHERE item_id = $1
		ORDER BY origin IS NOT NULL, lang NULLS LAST, id`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Subtitle{}
	for rows.Next() {
		var s Subtitle
		if err := scanSubtitle(rows, &s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// AttachableVersion is the version of the item id a subtitle is attached to:
// the complete one, the one that plays, in its folder. NotAttachable when it
// has none: a title not packaged yet, or packaged before the library.
func AttachableVersion(ctx context.Context, q Querier, id string) (*Version, error) {
	v, err := Current(ctx, q, id)
	if err != nil {
		return nil, err
	}
	if v == nil || v.Dir == nil || *v.Dir == "" {
		return nil, &NotAttachable{Reason: "the title has no package in the library to attach a subtitle to: it is not packaged yet, or was packaged before the library"}
	}
	return v, nil
}

// Attach attaches a to the item id: its file written into the subs/ folder
// of the version that plays, named by its bytes (AttachedName), and its row
// recorded with how it was made, the version it follows and who attached it,
// not default; under the item's lock, in one transaction. The same file of
// the same language, forced and SDH, attached again is the subtitle there is
// (created false), its file written again when it is gone. NotAttachable
// when the item has no version to attach it to.
func Attach(ctx context.Context, pool *pgxpool.Pool, id string, a Attachment, now time.Time) (Subtitle, bool, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return Subtitle{}, false, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	if err := LockItem(ctx, tx, id); err != nil {
		return Subtitle{}, false, err
	}
	v, err := AttachableVersion(ctx, tx, id)
	if err != nil {
		return Subtitle{}, false, err
	}
	sum := SHA256(a.VTT)
	path := AttachedPath(*v.Dir, AttachedName(sum, a.Language, a.Forced, a.SDH))
	if err := writeAttached(path, a.VTT); err != nil {
		return Subtitle{}, false, err
	}
	var s Subtitle
	err = scanSubtitle(tx.QueryRow(ctx, `SELECT `+subtitleCols+` FROM com_nalet_katalog_subtitleassets
		WHERE item_id = $1 AND path = $2 ORDER BY id LIMIT 1`, id, path), &s)
	switch {
	case err == nil:
		return s, false, tx.Commit(ctx)
	case !errors.Is(err, pgx.ErrNoRows):
		return Subtitle{}, false, err
	}
	label := a.Label
	if label == nil || strings.TrimSpace(*label) == "" {
		label = AttachLabel(a.Origin, a.SDH)
	}
	err = scanSubtitle(tx.QueryRow(ctx, `INSERT INTO com_nalet_katalog_subtitleassets
			(id, item_id, path, format, lang, label, isdefault, isforced, issdh, origin, versionid, sha256, attachedby, attachedat)
		VALUES (gen_random_uuid()::varchar, $1, $2, 'webvtt', $3, $4, false, $5, $6, $7, $8, $9, $10, $11)
		RETURNING `+subtitleCols, id, path, a.Language, FitLabel(label), a.Forced, a.SDH, a.Origin, v.ID, sum,
		nonEmpty(a.By), now.UTC()), &s)
	if err != nil {
		return Subtitle{}, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Subtitle{}, false, err
	}
	return s, true, nil
}

// writeAttached writes the file of an attached subtitle unless it is there
// with those bytes: named by its bytes, it is written once.
func writeAttached(path string, vtt []byte) error {
	if have, err := os.ReadFile(path); err == nil && bytes.Equal(have, vtt) {
		return nil
	}
	return WriteFile(path, vtt)
}

// Detach removes the subtitle sub attached to the item id, for by (the client
// of the caller's token; admin: any attached subtitle): its row, and its file
// once no row names it, unless the version's checksums list the file (a
// migration's fill hashed it into the chain, which must keep holding).
// ErrNoSuchSubtitle, ErrNotAttached and ErrAttachedByOther refuse it.
func Detach(ctx context.Context, pool *pgxpool.Pool, id, sub, by string, admin bool) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	if err := LockItem(ctx, tx, id); err != nil {
		return err
	}
	var s Subtitle
	err = scanSubtitle(tx.QueryRow(ctx, `SELECT `+subtitleCols+` FROM com_nalet_katalog_subtitleassets
		WHERE id = $1 AND item_id = $2`, sub, id), &s)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return ErrNoSuchSubtitle
	case err != nil:
		return err
	case !s.Attached:
		return ErrNotAttached
	case !admin && (s.AttachedBy == nil || *s.AttachedBy != by):
		return ErrAttachedByOther
	}
	if _, err := tx.Exec(ctx, `DELETE FROM com_nalet_katalog_subtitleassets WHERE id = $1`, sub); err != nil {
		return err
	}
	var others int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM com_nalet_katalog_subtitleassets WHERE path = $1`, s.Path).Scan(&others); err != nil {
		return err
	}
	if others == 0 && !listedInChain(s.Path) {
		if err := os.Remove(s.Path); err != nil && !errors.Is(err, os.ErrNotExist) {
			log.Printf("subtitles: the file of the subtitle %s removed from %s stays: %v", sub, id, err)
		}
	}
	return tx.Commit(ctx)
}

// listedInChain reports whether the checksums of the version folder an
// attached subtitle's file lies in list it.
func listedInChain(path string) bool {
	dir := filepath.Dir(filepath.Dir(path)) // <version>/subs/<name>
	listed, err := ReadChecksums(filepath.Join(dir, SumsFile))
	if err != nil {
		return false
	}
	_, ok := listed["subs/"+filepath.Base(path)]
	return ok
}

// CarryAttached carries, in tx, the subtitles attached to the item id over to
// its version versionID, whose package completes in versionDir: one attached
// to a version made of the same sources is timed to this one too, so its
// file is copied into this version's subs/ under its name and its row points
// there; one attached to a version of another file is not. It answers the
// ids of the subtitles it carried; the others' rows go with the package's.
// A file that cannot be copied is not carried, and said in the log: a
// package is never refused for an attached subtitle. A catalog without
// migration 048 has none: its columns are looked for first, since a query
// that names a column there is not would end tx.
func CarryAttached(ctx context.Context, tx pgx.Tx, id, versionDir, versionID string) ([]string, error) {
	if ok, err := AttachedReady(ctx, tx); err != nil || !ok {
		return nil, err
	}
	var to []string
	if err := tx.QueryRow(ctx, `SELECT sourceids FROM com_nalet_katalog_itemversions WHERE id = $1`, versionID).Scan(&to); err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, `SELECT s.id, s.path, s.versionid, v.sourceids FROM com_nalet_katalog_subtitleassets s
		LEFT JOIN com_nalet_katalog_itemversions v ON v.id = s.versionid
		WHERE s.item_id = $1 AND s.origin IS NOT NULL ORDER BY s.id`, id)
	if err != nil {
		return nil, err
	}
	type attached struct {
		id, path string
		version  *string
		sources  []string
	}
	var list []attached
	for rows.Next() {
		var a attached
		if err := rows.Scan(&a.id, &a.path, &a.version, &a.sources); err != nil {
			rows.Close()
			return nil, err
		}
		list = append(list, a)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var carried []string
	for _, a := range list {
		if a.version != nil && *a.version == versionID {
			carried = append(carried, a.id)
			continue
		}
		if len(to) == 0 || !sameSources(a.sources, to) {
			continue
		}
		dst := AttachedPath(versionDir, filepath.Base(a.path))
		b, err := os.ReadFile(a.path)
		if err == nil {
			err = writeAttached(dst, b)
		}
		if err != nil {
			log.Printf("subtitles: the subtitle %s attached to %s is not carried over to version %s: %v", a.id, id, versionID, err)
			continue
		}
		if _, err := tx.Exec(ctx, `UPDATE com_nalet_katalog_subtitleassets SET path = $2, versionid = $3 WHERE id = $1`,
			a.id, dst, versionID); err != nil {
			return nil, err
		}
		carried = append(carried, a.id)
	}
	return carried, nil
}

// AttachedReady reports whether migration 048 is in place in the catalog q
// reads (store.AttachedSubtitlesReady asks the same): the columns of a
// subtitle attached to a title.
func AttachedReady(ctx context.Context, q Querier) (bool, error) {
	var ok bool
	err := q.QueryRow(ctx, `SELECT count(*) = 6 FROM pg_attribute
		WHERE attrelid = to_regclass('com_nalet_katalog_subtitleassets') AND attnum > 0 AND NOT attisdropped
		  AND attname IN ('origin', 'issdh', 'versionid', 'sha256', 'attachedby', 'attachedat')`).Scan(&ok)
	return ok, err
}

// sameSources reports whether a and b name the same sources.
func sameSources(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	x, y := slices.Clone(a), slices.Clone(b)
	slices.Sort(x)
	slices.Sort(y)
	return slices.Equal(x, y)
}

func nonEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
