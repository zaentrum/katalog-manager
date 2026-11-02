// Package picture keeps each title's picture area: the part of its coded frame
// its picture fills (migration 054, README "Picture areas").
//
// Many sources keep a full frame with a wider picture letterboxed inside it,
// black bars baked into the video (a 1920x1080 frame whose picture is
// 1920x804), or a narrower one pillarboxed. The players fit the picture area
// to the screen rather than the frame. The analyzer measures it: its pass of a
// new title reads the original, and the backfill (Service.Claim) hands it the
// titles that have none, a package's video rendition where there is one. It
// reports each measurement (PUT /api/picture/items/{id}, an extra's
// /api/picture/extras/{id}), and the catalog keeps it in pixels with the frame
// it was measured in, with what it was measured of: an original (its source),
// a version's package, an extra's package. katalog-api answers the area of
// what a title plays now (the view com_nalet_katalog_currentpictureareas): a
// title whose file changed since has none until it is measured again.
package picture

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/zaentrum/katalog-manager/internal/library"
	"github.com/zaentrum/katalog-manager/internal/store"
)

// The kinds of title a picture area is of.
const (
	KindItem  = "item"  // a movie or an episode
	KindExtra = "extra" // a title's extra
)

// The states of a picture area.
const (
	StatePending  = "pending"  // the backfill handed it out; nothing measured yet
	StateMeasured = "measured" // its frame and area are known (the area the frame itself: no bars)
	StateUnknown  = "unknown"  // the analyzer could not tell (Reason)
	StateFailed   = "failed"   // the analyzer could not read the file (Reason); tried again from NextAttemptAt
)

// ErrNotReady says migration 054 is not in place.
var ErrNotReady = errors.New("the picture areas' migration (db/migrations/054_picture_areas.sql) is not applied")

// ErrNoTitle says there is no such item or extra.
var ErrNoTitle = errors.New("no such title")

// BadReport is a report that can't be taken: what is wrong with it.
type BadReport struct{ Why string }

func (e *BadReport) Error() string { return e.Why }

// Area is a title's picture area as the catalog keeps it.
type Area struct {
	ID, Kind, ItemID, State string
	// FrameWidth and FrameHeight are the coded frame measured, bars included;
	// X, Y, Width and Height the picture's area in it, pixels from its top
	// left corner. Set when measured.
	FrameWidth, FrameHeight *int32
	X, Y, Width, Height     *int32
	// Path is the file measured; SourceID, VersionID and PackageID what it is
	// (an original, a version's package, an extra's package).
	Path, SourceID, VersionID, PackageID *string
	Method, Reason                       *string
	Samples, Informative                 *int32
	DurationMs                           *int64
	Seconds, CPUSeconds                  *float64
	Evidence                             json.RawMessage
	MeasuredAt                           *time.Time
	ClaimedAt                            *time.Time
	ClaimedBy                            *string
	LastClaimedAt                        *time.Time
	Attempts, Failures                   int32
	NextAttemptAt                        *time.Time
	ModifiedAt                           time.Time
	// Current says it is of what the title plays now.
	Current bool
}

// Full reports whether the picture fills the frame: measured, with no bars.
func (a *Area) Full() bool {
	return a.State == StateMeasured && a.X != nil && a.Y != nil && a.Width != nil && a.Height != nil &&
		a.FrameWidth != nil && a.FrameHeight != nil && *a.X == 0 && *a.Y == 0 &&
		*a.Width == *a.FrameWidth && *a.Height == *a.FrameHeight
}

// areaColumns are the columns an Area is read from, of the table aliased pa.
const areaColumns = `pa.id, pa.kind, pa.item_id, pa.state, pa.framewidth, pa.frameheight, pa.areax, pa.areay,
	pa.areawidth, pa.areaheight, pa.path, pa.sourceid, pa.versionid, pa.packageid, pa.method, pa.reason,
	pa.samples, pa.informative, pa.durationms, pa.seconds, pa.cpuseconds, pa.evidence, pa.measuredat,
	pa.claimedat, pa.claimedby, pa.lastclaimedat, pa.attempts, pa.failures, pa.nextattemptat, pa.modifiedat,
	EXISTS (SELECT 1 FROM ` + store.CurrentPictureAreasView + ` c WHERE c.id = pa.id)`

func scanArea(row pgx.Row) (*Area, error) {
	a := &Area{}
	var seconds, cpu *float32
	err := row.Scan(&a.ID, &a.Kind, &a.ItemID, &a.State, &a.FrameWidth, &a.FrameHeight, &a.X, &a.Y,
		&a.Width, &a.Height, &a.Path, &a.SourceID, &a.VersionID, &a.PackageID, &a.Method, &a.Reason,
		&a.Samples, &a.Informative, &a.DurationMs, &seconds, &cpu, &a.Evidence, &a.MeasuredAt,
		&a.ClaimedAt, &a.ClaimedBy, &a.LastClaimedAt, &a.Attempts, &a.Failures, &a.NextAttemptAt, &a.ModifiedAt,
		&a.Current)
	if err != nil {
		return nil, err
	}
	a.Seconds, a.CPUSeconds = f64(seconds), f64(cpu)
	return a, nil
}

func f64(p *float32) *float64 {
	if p == nil {
		return nil
	}
	v := float64(*p)
	return &v
}

// Ready reports whether migration 054 is in place.
func Ready(ctx context.Context, st *store.Store) (bool, error) {
	return st.PictureAreasReady(ctx)
}

// Of reads the picture area of the title id of kind; nil when it has none. A
// covered episode is answered its holder's: it plays its holder's file.
func Of(ctx context.Context, q library.Querier, kind, id string) (*Area, error) {
	if kind == KindItem {
		holder, err := library.HolderOrSelf(ctx, q, id)
		if err != nil {
			return nil, err
		}
		id = holder
	}
	a, err := scanArea(q.QueryRow(ctx, `SELECT `+areaColumns+` FROM `+store.PictureAreasTable+` pa
		WHERE pa.id = $1 AND pa.kind = $2`, id, kind))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return a, err
}

// Size is a frame's size, in pixels.
type Size struct {
	Width  int32 `json:"width"`
	Height int32 `json:"height"`
}

// Rect is an area of a frame, in pixels from its top left corner.
type Rect struct {
	X      int32 `json:"x"`
	Y      int32 `json:"y"`
	Width  int32 `json:"width"`
	Height int32 `json:"height"`
}

// Report is what the analyzer reports of a measurement. A measurement that
// found the area has Frame and Area (the frame itself when there are no
// bars); one that could not tell has Frame, no Area, and Reason; one that
// could not read the file has Error. Released gives a title the backfill
// handed out back unmeasured (its run was stopped): nothing else is read.
type Report struct {
	Path        string          `json:"path"`
	Method      string          `json:"method"`
	Frame       *Size           `json:"frame"`
	Area        *Rect           `json:"area"`
	Samples     *int32          `json:"samples"`
	Informative *int32          `json:"informative"`
	DurationMs  *int64          `json:"durationMs"`
	Seconds     *float64        `json:"seconds"`
	CPUSeconds  *float64        `json:"cpuSeconds"`
	Sides       json.RawMessage `json:"sides"`
	Reason      string          `json:"reason"`
	Error       string          `json:"error"`
	Released    bool            `json:"released"`
}

// state is the state the report leaves its title in, or why it can't be
// taken.
func (r Report) state() (string, error) {
	if r.Released {
		return "", nil
	}
	if strings.TrimSpace(r.Path) == "" {
		return "", &BadReport{"a report names the file it measured (path)"}
	}
	if len(r.Sides) > 0 && string(r.Sides) != "null" {
		var o map[string]json.RawMessage
		if json.Unmarshal(r.Sides, &o) != nil {
			return "", &BadReport{"sides is a JSON object, of each side's bar"}
		}
	}
	if strings.TrimSpace(r.Error) != "" {
		return StateFailed, nil
	}
	if r.Frame == nil || r.Frame.Width <= 0 || r.Frame.Height <= 0 {
		return "", &BadReport{"a measurement names the frame it measured (frame: {width, height}, above 0)"}
	}
	if r.Area == nil {
		return StateUnknown, nil
	}
	a, f := r.Area, r.Frame
	if a.X < 0 || a.Y < 0 || a.Width <= 0 || a.Height <= 0 || a.X+a.Width > f.Width || a.Y+a.Height > f.Height {
		return "", &BadReport{fmt.Sprintf("the area %dx%d at %d,%d does not lie within the frame %dx%d",
			a.Width, a.Height, a.X, a.Y, f.Width, f.Height)}
	}
	return StateMeasured, nil
}

// MaxBackoff is the longest a failed title waits before the backfill hands it
// out again; the first wait is FirstBackoff, doubled after each failure.
const (
	FirstBackoff = time.Hour
	MaxBackoff   = 7 * 24 * time.Hour
)

// backoff is the wait after the failures-th failure in a row.
func backoff(failures int32) time.Duration {
	d := FirstBackoff
	for i := int32(1); i < failures && d < MaxBackoff; i++ {
		d *= 2
	}
	return min(d, MaxBackoff)
}

// Record keeps what the analyzer reported of the title id of kind, and answers
// the title's picture area as it is now. A measurement (measured or unknown)
// replaces what was kept, and ends any claim of it. A failure counts as one
// and waits its backoff, and leaves an area of what the title plays now as it
// is. Released ends the title's claim, nothing else. What the file measured
// is: of an item, the version whose folder holds it (and the original that
// version was made of), else its original (the source of its primary asset of
// that path); of an extra, its package, when the file is in the package's
// folder. A covered episode is measured with its holder (400), a series or a
// season has no frame (400), an unknown title is ErrNoTitle.
func Record(ctx context.Context, st *store.Store, kind, id string, r Report, now time.Time) (*Area, error) {
	state, err := r.state()
	if err != nil {
		return nil, err
	}
	pool := st.Pool()
	var itemID string
	var sourceID, versionID, packageID *string
	switch kind {
	case KindItem:
		var typ string
		var covered *string
		err := pool.QueryRow(ctx, `SELECT type, coveredby FROM com_nalet_katalog_items WHERE id = $1`, id).
			Scan(&typ, &covered)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNoTitle
		}
		if err != nil {
			return nil, err
		}
		if covered != nil && *covered != "" {
			return nil, &BadReport{"item " + id + " plays the file of " + *covered + ", which is measured instead"}
		}
		if typ != "movie" && typ != "episode" {
			return nil, &BadReport{"a " + typ + " has no frame of its own: its movies or episodes are measured"}
		}
		itemID = id
		if !r.Released {
			if sourceID, versionID, err = itemFileOf(ctx, pool, id, r.Path); err != nil {
				return nil, err
			}
		}
	case KindExtra:
		var dir *string
		var pid *string
		err := pool.QueryRow(ctx, `SELECT item_id, packagepath, packageid FROM com_nalet_katalog_itemextras WHERE id = $1`, id).
			Scan(&itemID, &dir, &pid)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNoTitle
		}
		if err != nil {
			return nil, err
		}
		if dir != nil && underDir(*dir, r.Path) {
			packageID = pid
		}
	default:
		return nil, &BadReport{"a picture area is an item's or an extra's"}
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if r.Released {
		if _, err := tx.Exec(ctx, `UPDATE `+store.PictureAreasTable+` SET claimedat = NULL, claimedby = NULL,
			modifiedat = $3 WHERE id = $1 AND kind = $2`, id, kind, now); err != nil {
			return nil, err
		}
		if err := tx.Commit(ctx); err != nil {
			return nil, err
		}
		return Of(ctx, pool, kind, id)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO `+store.PictureAreasTable+` (id, kind, item_id, state, createdat, modifiedat)
		VALUES ($1, $2, $3, 'pending', $4, $4) ON CONFLICT (id) DO NOTHING`, id, kind, itemID, now); err != nil {
		return nil, err
	}
	var kept *Area
	if kept, err = scanArea(tx.QueryRow(ctx, `SELECT `+areaColumns+` FROM `+store.PictureAreasTable+` pa
		WHERE pa.id = $1 FOR UPDATE OF pa`, id)); err != nil {
		return nil, err
	}
	if kept.Kind != kind {
		return nil, &BadReport{"id " + id + " has the picture area of an " + kept.Kind}
	}
	evidence := sidesEvidence(r.Sides)
	if state == StateFailed {
		why := clip(strings.TrimSpace(r.Error), 500)
		failures := kept.Failures + 1
		next := now.Add(backoff(failures))
		if kept.Current && (kept.State == StateMeasured || kept.State == StateUnknown) {
			// A measurement of what plays now stays: the failure only counts.
			_, err = tx.Exec(ctx, `UPDATE `+store.PictureAreasTable+` SET failures = $2, claimedat = NULL,
				claimedby = NULL, modifiedat = $3 WHERE id = $1`, id, failures, now)
		} else {
			_, err = tx.Exec(ctx, `UPDATE `+store.PictureAreasTable+` SET state = 'failed', item_id = $2,
				framewidth = $3, frameheight = $4, areax = NULL, areay = NULL, areawidth = NULL, areaheight = NULL,
				path = $5, sourceid = $6, versionid = $7, packageid = $8, method = NULLIF($9, ''), reason = $10,
				samples = $11, informative = $12, durationms = $13, seconds = $14, cpuseconds = $15, evidence = $16,
				failures = $17, nextattemptat = $18, claimedat = NULL, claimedby = NULL, modifiedat = $19
				WHERE id = $1`, id, itemID, width(r.Frame), height(r.Frame), r.Path, sourceID, versionID, packageID,
				r.Method, why, r.Samples, r.Informative, r.DurationMs, r.Seconds, r.CPUSeconds, evidence,
				failures, next, now)
		}
	} else {
		var x, y, w, h *int32
		if r.Area != nil {
			x, y, w, h = &r.Area.X, &r.Area.Y, &r.Area.Width, &r.Area.Height
		}
		var why *string
		if state == StateUnknown {
			s := clip(strings.TrimSpace(r.Reason), 500)
			if s == "" {
				s = "the analyzer could not tell"
			}
			why = &s
		}
		_, err = tx.Exec(ctx, `UPDATE `+store.PictureAreasTable+` SET state = $2, item_id = $3,
			framewidth = $4, frameheight = $5, areax = $6, areay = $7, areawidth = $8, areaheight = $9,
			path = $10, sourceid = $11, versionid = $12, packageid = $13, method = NULLIF($14, ''), reason = $15,
			samples = $16, informative = $17, durationms = $18, seconds = $19, cpuseconds = $20, evidence = $21,
			measuredat = $22, failures = 0, nextattemptat = NULL, claimedat = NULL, claimedby = NULL, modifiedat = $22
			WHERE id = $1`, id, state, itemID, r.Frame.Width, r.Frame.Height, x, y, w, h, r.Path, sourceID, versionID,
			packageID, r.Method, why, r.Samples, r.Informative, r.DurationMs, r.Seconds, r.CPUSeconds, evidence, now)
	}
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return Of(ctx, pool, kind, id)
}

// itemFileOf says what the file at path of the item id is: in a folder of one
// of its versions, that version and the first original it was made of; else,
// as its primary asset's path, that asset's original (none in the legacy
// layout).
func itemFileOf(ctx context.Context, q library.Querier, id, path string) (sourceID, versionID *string, err error) {
	var v string
	var sources []string
	err = q.QueryRow(ctx, `SELECT id, sourceids FROM com_nalet_katalog_itemversions
		WHERE item_id = $1 AND dir IS NOT NULL AND dir <> '' AND starts_with($2, rtrim(dir, '/') || '/')
		ORDER BY state = 'complete' DESC, id LIMIT 1`, id, path).Scan(&v, &sources)
	switch {
	case err == nil:
		versionID = &v
		if len(sources) > 0 {
			sourceID = &sources[0]
		}
		return sourceID, versionID, nil
	case !errors.Is(err, pgx.ErrNoRows):
		return nil, nil, err
	}
	err = q.QueryRow(ctx, `SELECT sourceid FROM com_nalet_katalog_playbackassets
		WHERE item_id = $1 AND isprimary AND COALESCE(kind, 'primary') = 'primary' AND path = $2
		ORDER BY id LIMIT 1`, id, path).Scan(&sourceID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, nil
	}
	return sourceID, nil, err
}

// underDir reports whether path lies in the folder dir.
func underDir(dir, path string) bool {
	dir = strings.TrimRight(dir, "/")
	return dir != "" && strings.HasPrefix(path, dir+"/")
}

// sidesEvidence is the evidence kept of a report: its sides, as the analyzer
// wrote them, when they are a JSON object.
func sidesEvidence(sides json.RawMessage) []byte {
	if len(sides) == 0 || string(sides) == "null" {
		return nil
	}
	return sides
}

func width(s *Size) *int32 {
	if s == nil || s.Width <= 0 {
		return nil
	}
	return &s.Width
}

func height(s *Size) *int32 {
	if s == nil || s.Height <= 0 {
		return nil
	}
	return &s.Height
}

// clip is s cut to at most n bytes, at a character's start.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
