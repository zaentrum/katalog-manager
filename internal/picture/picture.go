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
	// (an original, a version's package, an extra's package), MeasuredOn which
	// of the two the analyzer read and FileSize the original's size.
	Path, SourceID, VersionID, PackageID *string
	MeasuredOn                           *string
	FileSize                             *int64
	Method, Reason                       *string
	Samples, Informative                 *int32
	DurationMs, BytesRead                *int64
	Seconds, CPUSeconds                  *float64
	Evidence, Sampling                   json.RawMessage
	MeasuredAt                           *time.Time
	ClaimedAt                            *time.Time
	ClaimedBy, ClaimToken                *string
	LastClaimedAt                        *time.Time
	Attempts, Failures, Releases         int32
	NextAttemptAt                        *time.Time
	LapsedToken                          *string
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
	pa.areawidth, pa.areaheight, pa.path, pa.sourceid, pa.versionid, pa.packageid, pa.measuredon, pa.filesize,
	pa.method, pa.reason, pa.samples, pa.informative, pa.durationms, pa.bytesread, pa.seconds, pa.cpuseconds,
	pa.evidence, pa.sampling, pa.measuredat, pa.claimedat, pa.claimedby, pa.claimtoken, pa.lastclaimedat,
	pa.attempts, pa.failures, pa.releases, pa.nextattemptat, pa.lapsedtoken, pa.modifiedat,
	EXISTS (SELECT 1 FROM ` + store.CurrentPictureAreasView + ` c WHERE c.id = pa.id)`

func scanArea(row pgx.Row) (*Area, error) {
	a := &Area{}
	var seconds, cpu *float32
	err := row.Scan(&a.ID, &a.Kind, &a.ItemID, &a.State, &a.FrameWidth, &a.FrameHeight, &a.X, &a.Y,
		&a.Width, &a.Height, &a.Path, &a.SourceID, &a.VersionID, &a.PackageID, &a.MeasuredOn, &a.FileSize,
		&a.Method, &a.Reason, &a.Samples, &a.Informative, &a.DurationMs, &a.BytesRead, &seconds, &cpu,
		&a.Evidence, &a.Sampling, &a.MeasuredAt, &a.ClaimedAt, &a.ClaimedBy, &a.ClaimToken, &a.LastClaimedAt,
		&a.Attempts, &a.Failures, &a.Releases, &a.NextAttemptAt, &a.LapsedToken, &a.ModifiedAt,
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

// NeedsOriginal reports whether the item id still needs its original source
// sourceID for its picture area: it has no area of what it plays now that
// says the picture's area (measured), and none measured of that original that
// says it can't tell (unknown) either. A deletion wave keeps the original
// while it does. A covered episode is answered its holder's; a catalog
// without migration 054 is ErrNotReady.
func NeedsOriginal(ctx context.Context, q library.Querier, id, sourceID string) (bool, error) {
	var ready bool
	if err := q.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, store.CurrentPictureAreasView).
		Scan(&ready); err != nil {
		return true, err
	}
	if !ready {
		return true, ErrNotReady
	}
	holder, err := library.HolderOrSelf(ctx, q, id)
	if err != nil {
		return true, err
	}
	var settled bool
	err = q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM `+store.CurrentPictureAreasView+` c
		WHERE c.id = $1 AND c.kind = 'item'
		  AND (c.state = 'measured'
		    OR (c.state = 'unknown' AND c.measuredon = 'original' AND c.sourceid = $2)))`, holder, sourceID).
		Scan(&settled)
	return !settled, err
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
// Token is the claim's the run measured for (Job.Token), none for the
// analyzer's own pass of a new title. MeasuredOn says what the analyzer read
// (original or package, and its size); Sampling, Video and Decoder how it
// sampled, what the video is and its decoder's version, kept as they come.
type Report struct {
	Path        string          `json:"path"`
	Method      string          `json:"method"`
	Token       string          `json:"token"`
	Frame       *Size           `json:"frame"`
	Area        *Rect           `json:"area"`
	Samples     *int32          `json:"samples"`
	Informative *int32          `json:"informative"`
	DurationMs  *int64          `json:"durationMs"`
	Seconds     *float64        `json:"seconds"`
	CPUSeconds  *float64        `json:"cpuSeconds"`
	BytesRead   *int64          `json:"bytesRead"`
	MeasuredOn  *MeasuredOn     `json:"measuredOn"`
	Sampling    json.RawMessage `json:"sampling"`
	Video       json.RawMessage `json:"video"`
	Decoder     string          `json:"decoder"`
	Sides       json.RawMessage `json:"sides"`
	Reason      string          `json:"reason"`
	Error       string          `json:"error"`
	Released    bool            `json:"released"`
}

// MeasuredOn is what a measurement read: an original or a package's video
// rendition (Kind), and the original's size in bytes.
type MeasuredOn struct {
	Kind string `json:"kind"`
	Size *int64 `json:"size"`
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
	for name, raw := range map[string]json.RawMessage{"sides": r.Sides, "sampling": r.Sampling, "video": r.Video} {
		if len(raw) > 0 && string(raw) != "null" {
			var o map[string]json.RawMessage
			if json.Unmarshal(raw, &o) != nil {
				return "", &BadReport{name + " is a JSON object"}
			}
		}
	}
	if m := r.MeasuredOn; m != nil && m.Kind != "" && m.Kind != "original" && m.Kind != "package" {
		return "", &BadReport{"measuredOn.kind is original or package, not " + m.Kind}
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
// out again; the first wait is FirstBackoff, doubled after each failure. A
// title given back unmeasured waits FirstRelease, doubled after each release
// in a row, MaxRelease at most: the titles after it go first.
const (
	FirstBackoff = time.Hour
	MaxBackoff   = 7 * 24 * time.Hour
	FirstRelease = 15 * time.Minute
	MaxRelease   = 24 * time.Hour
)

// backoff is the wait after the failures-th failure in a row.
func backoff(failures int32) time.Duration {
	return doubled(FirstBackoff, MaxBackoff, failures)
}

// releaseBackoff is the wait after the releases-th release in a row.
func releaseBackoff(releases int32) time.Duration {
	return doubled(FirstRelease, MaxRelease, releases)
}

func doubled(first, most time.Duration, n int32) time.Duration {
	d := first
	for i := int32(1); i < n && d < most; i++ {
		d *= 2
	}
	return min(d, most)
}

// Record keeps what the analyzer reported of the title id of kind, and answers
// the title's picture area as it is now. A measurement (measured or unknown)
// replaces what was kept. A failure counts as one and waits its backoff, and
// leaves an area of what the title plays now as it is. Released gives the
// title's claim back, which waits its release backoff before the backfill
// hands the title out again, nothing else. A report ends the claim it
// names (its token) and only that one: one of a claim whose lease ended
// leaves the claim made since as it is, its failure counted already when the
// lease ended; one without a token (the analyzer's pass of a new title)
// leaves any claim as it is. What the file measured is: of an item, the
// version whose folder holds it (and the original that version was made of),
// else its original (the source of its primary asset of that path); of an
// extra, its package, when the file is in the package's folder. A covered
// episode is measured with its holder (400), a series or a season has no
// frame (400), an unknown title is ErrNoTitle.
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
	token := strings.TrimSpace(r.Token)
	// owns: the report is of the claim the title is under; lapsed: of the
	// claim whose lease ended last, counted a failure then
	owns := kept.ClaimedAt != nil && (kept.ClaimToken == nil || token == *kept.ClaimToken)
	if token == "" {
		owns = false
	}
	lapsed := token != "" && kept.LapsedToken != nil && token == *kept.LapsedToken
	if r.Released {
		if r.Token == "" && kept.ClaimedAt != nil && kept.ClaimToken == nil {
			owns = true // a claim of an analyzer that sends no token
		}
		if owns {
			releases := kept.Releases + 1
			if _, err := tx.Exec(ctx, `UPDATE `+store.PictureAreasTable+` SET claimedat = NULL, claimedby = NULL,
				claimtoken = NULL, releases = $3, nextattemptat = GREATEST(nextattemptat, $4), modifiedat = $5
				WHERE id = $1 AND kind = $2`, id, kind, releases, now.Add(releaseBackoff(releases)), now); err != nil {
				return nil, err
			}
		}
		if err := tx.Commit(ctx); err != nil {
			return nil, err
		}
		return Of(ctx, pool, kind, id)
	}
	// What the claim fields become: the claim ends with the report of it.
	claim := `claimedat = CASE WHEN $2 THEN NULL ELSE claimedat END,
		claimedby = CASE WHEN $2 THEN NULL ELSE claimedby END,
		claimtoken = CASE WHEN $2 THEN NULL ELSE claimtoken END,
		lapsedtoken = CASE WHEN $3 THEN NULL ELSE lapsedtoken END`
	evidence := rawObject(r.Sides)
	sampling := samplingOf(r)
	measuredOn, fileSize := measuredOnOf(r, versionID, packageID)
	if state == StateFailed {
		why := clip(strings.TrimSpace(r.Error), 500)
		failures := kept.Failures
		if !lapsed {
			failures++
		}
		next := now.Add(backoff(failures))
		switch {
		case lapsed:
			// counted when its lease ended: this only says why
			_, err = tx.Exec(ctx, `UPDATE `+store.PictureAreasTable+` SET `+claim+`,
				reason = CASE WHEN state IN ('pending', 'failed') THEN $4 ELSE reason END, modifiedat = $5
				WHERE id = $1`, id, owns, lapsed, why, now)
		case kept.Current && (kept.State == StateMeasured || kept.State == StateUnknown):
			// A measurement of what plays now stays: the failure only counts.
			_, err = tx.Exec(ctx, `UPDATE `+store.PictureAreasTable+` SET `+claim+`, failures = $4,
				modifiedat = $5 WHERE id = $1`, id, owns, lapsed, failures, now)
		default:
			_, err = tx.Exec(ctx, `UPDATE `+store.PictureAreasTable+` SET `+claim+`, state = 'failed', item_id = $4,
				framewidth = $5, frameheight = $6, areax = NULL, areay = NULL, areawidth = NULL, areaheight = NULL,
				path = $7, sourceid = $8, versionid = $9, packageid = $10, method = NULLIF($11, ''), reason = $12,
				samples = $13, informative = $14, durationms = $15, seconds = $16, cpuseconds = $17, evidence = $18,
				failures = $19, nextattemptat = $20, measuredon = $21, filesize = $22, bytesread = $23, sampling = $24,
				modifiedat = $25
				WHERE id = $1`, id, owns, lapsed, itemID, width(r.Frame), height(r.Frame), r.Path, sourceID, versionID,
				packageID, r.Method, why, r.Samples, r.Informative, r.DurationMs, r.Seconds, r.CPUSeconds, evidence,
				failures, next, measuredOn, fileSize, r.BytesRead, sampling, now)
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
		_, err = tx.Exec(ctx, `UPDATE `+store.PictureAreasTable+` SET `+claim+`, state = $4, item_id = $5,
			framewidth = $6, frameheight = $7, areax = $8, areay = $9, areawidth = $10, areaheight = $11,
			path = $12, sourceid = $13, versionid = $14, packageid = $15, method = NULLIF($16, ''), reason = $17,
			samples = $18, informative = $19, durationms = $20, seconds = $21, cpuseconds = $22, evidence = $23,
			measuredon = $24, filesize = $25, bytesread = $26, sampling = $27,
			measuredat = $28, failures = 0, releases = 0, nextattemptat = NULL, modifiedat = $28
			WHERE id = $1`, id, owns, lapsed, state, itemID, r.Frame.Width, r.Frame.Height, x, y, w, h, r.Path,
			sourceID, versionID, packageID, r.Method, why, r.Samples, r.Informative, r.DurationMs, r.Seconds,
			r.CPUSeconds, evidence, measuredOn, fileSize, r.BytesRead, sampling, now)
	}
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return Of(ctx, pool, kind, id)
}

// measuredOnOf is what a report measured (original or package) and the
// original's size: as it says, else by what its file is.
func measuredOnOf(r Report, versionID, packageID *string) (*string, *int64) {
	kind := ""
	var size *int64
	if m := r.MeasuredOn; m != nil {
		kind, size = m.Kind, m.Size
	}
	if kind == "" {
		kind = "original"
		if versionID != nil || packageID != nil || strings.HasSuffix(strings.ToLower(r.Path), ".m3u8") {
			kind = "package"
		}
	}
	if kind != "original" || (size != nil && *size <= 0) {
		size = nil
	}
	return &kind, size
}

// samplingOf is what is kept of how a report sampled: its sampling, with
// the video and the decoder it names.
func samplingOf(r Report) []byte {
	out := map[string]json.RawMessage{}
	if raw := rawObject(r.Sampling); raw != nil {
		if json.Unmarshal(raw, &out) != nil {
			out = map[string]json.RawMessage{}
		}
	}
	if raw := rawObject(r.Video); raw != nil {
		out["video"] = raw
	}
	if d := strings.TrimSpace(r.Decoder); d != "" {
		b, _ := json.Marshal(clip(d, 200))
		out["decoder"] = b
	}
	if len(out) == 0 {
		return nil
	}
	b, err := json.Marshal(out)
	if err != nil {
		return nil
	}
	return b
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

// rawObject is a report's JSON object as the analyzer wrote it; nil for
// none (state() refuses one that is no object).
func rawObject(raw json.RawMessage) []byte {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	return raw
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
