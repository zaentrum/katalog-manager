package rest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/zaentrum/katalog-manager/internal/library"
)

// settings reads the library's settings (library.ReadSettings); handlers
// without a store have the defaults, the legacy layout.
func (h *Handlers) settings(ctx context.Context) (library.Settings, error) {
	if h.d.Store == nil {
		return library.Defaults(), nil
	}
	return library.ReadSettings(ctx, h.d.Store.Pool())
}

// paths are where the configuration puts the library.
func (h *Handlers) paths() library.Paths { return library.PathsOf(h.d.Cfg) }

// recordContract is the version of the library's contract a worker record's
// library key follows (platform-library/1).
const recordContract = 1

// itemLibrary is the library key of an item's worker record, with the
// setting library.layout=v2: where the item's records go and what the run
// works on. The workers compute no library path themselves.
type itemLibrary struct {
	Contract int            `json:"contract"`
	Root     string         `json:"root"`
	ItemDir  *string        `json:"itemDir"`
	Blocked  *string        `json:"blocked"` // why the item cannot be recorded; the packager fails its step with it
	Source   *recordSource  `json:"source"`
	InboxDir string         `json:"inboxDir"`
	Build    *recordBuild   `json:"build"`
	Current  *recordVersion `json:"current"`
}

// recordSource is the original the run works on: the item's source behind
// its primary asset.
type recordSource struct {
	SourceID    string  `json:"sourceId"`
	Recorded    bool    `json:"recorded"` // sources/<id>/ is written
	RecordDir   *string `json:"recordDir"`
	LibraryPath *string `json:"libraryPath"`
	SizeBytes   int64   `json:"sizeBytes"`
	QH1         *string `json:"qh1"`
}

// recordBuild is the version the run builds, and the marks it carries: what
// the run does (mode: establish, takein, add or repackage, library.RunOf),
// and the name the original gets in the version's folder when the run
// renames it in (establish, takein; null for add and repackage, whose
// original is in a version's folder already, or stays where it arrived).
// For add, versionDir is the folder taken in that holds the original, which
// the package is added to.
type recordBuild struct {
	VersionID    string          `json:"versionId"`
	StagingDir   string          `json:"stagingDir"`
	VersionDir   string          `json:"versionDir"`
	Mode         string          `json:"mode"`
	OriginalName *string         `json:"originalName"`
	CreatedBy    string          `json:"createdBy"`
	Chapters     []recordChapter `json:"chapters"`
	ChaptersFrom *string         `json:"chaptersFrom"`
	Segments     []recordSegment `json:"segments"`
}

type recordChapter struct {
	StartMs int64   `json:"startMs"`
	EndMs   int64   `json:"endMs"`
	Title   *string `json:"title"`
}

type recordSegment struct {
	Kind       string   `json:"kind"`
	StartMs    int64    `json:"startMs"`
	EndMs      int64    `json:"endMs"`
	Detector   *string  `json:"detector"`
	Confidence *float64 `json:"confidence"`
	Label      *string  `json:"label"`
}

// recordVersion is the version that plays.
type recordVersion struct {
	VersionID string `json:"versionId"`
	Dir       string `json:"dir"`
}

// recordSegmentKinds are the kinds of a version's detected range; any other is
// other, labelled with the catalog's kind (lower-case), as the library's
// record logic writes it.
var recordSegmentKinds = map[string]bool{"intro": true, "recap": true, "credits": true, "preview": true, "commercial": true,
	"other": true}

// itemLibraryOf is the library key of the item's worker record: the item is
// recorded first when it is not (its folder gets item.json); the source behind
// its primary asset is made when it has none (a title from before 040); the
// version the run builds is the one the pipeline builds for the item, made
// when there is none, or, when the source's version holds its original
// alone (taken), that one, which the run adds its package to. The run's mode
// is the source's (library.RunOf): it takes the title in while its step
// takein waits or runs. An item that cannot be recorded says why in blocked.
func (h *Handlers) itemLibraryOf(ctx context.Context, itemID string) (*itemLibrary, error) {
	p := h.paths()
	pool := h.d.Store.Pool()
	lib := &itemLibrary{Contract: recordContract, Root: p.Root, InboxDir: p.InboxDir(itemID)}
	block := func(why string) {
		if lib.Blocked == nil {
			lib.Blocked = &why
		}
	}
	dir, err := p.EnsureItemRecord(ctx, pool, itemID)
	var blocked *library.Blocked
	switch {
	case errors.As(err, &blocked):
		block(blocked.Reason)
	case err != nil:
		return nil, err
	}
	if dir != "" {
		lib.ItemDir = &dir
	}
	src, err := p.EnsureSource(ctx, pool, itemID)
	if err != nil {
		log.Printf("the worker record of %s: %v", itemID, err)
		block(err.Error())
	}
	var sourceIDs []string
	if src != nil {
		sourceIDs = []string{src.ID}
		lib.Source = &recordSource{SourceID: src.ID, Recorded: src.RecordedAt != nil, LibraryPath: src.LibraryPath,
			SizeBytes: src.SizeBytes, QH1: src.QH1}
		if dir != "" {
			rd := library.SourceDir(dir, src.ID)
			lib.Source.RecordDir = &rd
		}
	}
	cur, err := library.Current(ctx, pool, itemID)
	if err != nil {
		return nil, err
	}
	if cur != nil && cur.Dir != nil {
		lib.Current = &recordVersion{VersionID: cur.ID, Dir: *cur.Dir}
	}
	if dir == "" {
		return lib, nil
	}
	mode, taken := library.ModeEstablish, (*library.Version)(nil)
	if src != nil {
		takeIn, err := takingIn(ctx, pool, itemID)
		if err != nil {
			return nil, err
		}
		if mode, taken, err = library.RunOf(ctx, pool, src, takeIn); err != nil {
			return nil, err
		}
	}
	var b *recordBuild
	if taken != nil {
		vdir := library.VersionDir(dir, taken.ID)
		if taken.Dir != nil {
			vdir = filepath.Clean(*taken.Dir)
		}
		b = &recordBuild{VersionID: taken.ID, StagingDir: p.StagingDir(taken.ID), VersionDir: vdir}
	} else {
		v, err := library.EnsureBuilding(ctx, pool, itemID, sourceIDs)
		if err != nil {
			return nil, err
		}
		b = &recordBuild{VersionID: v.ID, StagingDir: p.StagingDir(v.ID), VersionDir: library.VersionDir(dir, v.ID)}
		if src != nil && (mode == library.ModeEstablish || mode == library.ModeTakeIn) {
			name := library.OriginalName(arrivalName(src), 0)
			b.OriginalName = &name
		}
	}
	b.Mode, b.CreatedBy, b.Chapters, b.Segments = mode, "katalog-manager", []recordChapter{}, []recordSegment{}
	rows, err := pool.Query(ctx, `SELECT startms, COALESCE(NULLIF(endms, 0), startms), title FROM com_nalet_katalog_itemchapters
		WHERE item_id = $1 ORDER BY COALESCE(ordinal, 0), startms, id`, itemID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var c recordChapter
		if err := rows.Scan(&c.StartMs, &c.EndMs, &c.Title); err != nil {
			rows.Close()
			return nil, err
		}
		c.Title = oneLine(c.Title)
		b.Chapters = append(b.Chapters, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(b.Chapters) > 0 {
		from := "original-file"
		b.ChaptersFrom = &from
	}
	rows, err = pool.Query(ctx, `SELECT kind, startms, COALESCE(NULLIF(endms, 0), startms), source, confidence::float8, label
		FROM com_nalet_katalog_mediasegments WHERE item_id = $1 ORDER BY startms, id`, itemID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var sg recordSegment
		var source *string
		if err := rows.Scan(&sg.Kind, &sg.StartMs, &sg.EndMs, &source, &sg.Confidence, &sg.Label); err != nil {
			return nil, err
		}
		sg.Detector, sg.Label = oneLine(source), oneLine(sg.Label)
		if kind := strings.ToLower(sg.Kind); recordSegmentKinds[kind] {
			sg.Kind = kind
		} else {
			sg.Kind, sg.Label = "other", &kind
		}
		b.Segments = append(b.Segments, sg)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	lib.Build = b
	return lib, nil
}

// takingIn reports whether the item is being taken in: its step takein waits
// for the packager, or runs.
func takingIn(ctx context.Context, q library.Querier, itemID string) (bool, error) {
	var ok bool
	err := q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM com_nalet_katalog_itemprocessingsteps
		WHERE item_id = $1 AND step = 'takein' AND status IN ('pending', 'in_progress'))`, itemID).Scan(&ok)
	return ok, err
}

// arrivalName is the name the source's original arrived with, whose
// extension its name in the library keeps: its file's where it lies, else
// the one the catalog keeps.
func arrivalName(s *library.Source) string {
	if s.ArrivalPath != nil && *s.ArrivalPath != "" {
		return filepath.Base(*s.ArrivalPath)
	}
	return s.Filename
}

// sidecarCopies are, for an original that lies in its version's folder, the
// subtitle files that came with it: the folder it arrived in, whose files
// named after it are its sidecars (as the scanner paired them, their rows
// pointing at them until it is retired), and the copies its source's record
// keeps of them (sources/<sourceId>/<name>), by the content its source.json
// lists of each.
type sidecarCopies struct {
	folder  string
	itemDir string
	byHash  map[string][]string // a copy's sha256 (hex): the copies of it, relative to the item's folder
}

// of is the copy the record keeps of the subtitle file at path, each copy
// for one file; a file the record keeps no copy of is itself.
func (c *sidecarCopies) of(path string) string {
	digest, _, err := library.SHA256File(path)
	if err != nil {
		return path
	}
	files := c.byHash[digest]
	if len(files) == 0 {
		return path
	}
	c.byHash[digest] = files[1:]
	return filepath.Join(c.itemDir, filepath.FromSlash(files[0]))
}

// sidecarCopiesOf are the sidecar copies of the item's original at source
// when it lies in its version's folder; nil when it does not (the files
// beside it are its subtitle files), or when the item has no folder.
func (h *Handlers) sidecarCopiesOf(ctx context.Context, itemID, source string) (*sidecarCopies, error) {
	pool := h.d.Store.Pool()
	pl, err := library.PlaceOf(ctx, pool, itemID)
	var unplaced *library.Unplaced
	switch {
	case errors.As(err, &unplaced) || errors.Is(err, library.ErrNoItem):
		return nil, nil
	case err != nil:
		return nil, err
	}
	p := h.paths()
	itemDir := p.ItemDir(pl)
	if _, ok := library.VersionFolderOf(itemDir, source); !ok {
		return nil, nil
	}
	src, err := library.SourceAt(ctx, pool, filepath.Clean(source))
	if err != nil || src == nil {
		return nil, err
	}
	c := &sidecarCopies{itemDir: itemDir, byHash: map[string][]string{}}
	if arrival := p.ArrivalOf(src); arrival != "" {
		c.folder = filepath.Dir(arrival)
	}
	b, err := os.ReadFile(filepath.Join(library.SourceDir(itemDir, src.ID), "source.json"))
	if err != nil {
		return c, nil // no record: the files are named as they are
	}
	var rec struct {
		Sidecars []struct {
			File   string `json:"file"`
			Kind   string `json:"kind"`
			SHA256 string `json:"sha256"`
		} `json:"sidecars"`
	}
	if json.Unmarshal(b, &rec) != nil {
		return c, nil
	}
	prefix := "sources/" + src.ID + "/"
	for _, s := range rec.Sidecars {
		if s.Kind != "subtitle" || !strings.HasPrefix(s.File, prefix) || strings.Contains(s.File, "..") {
			continue
		}
		digest := strings.TrimPrefix(s.SHA256, "sha256:")
		c.byHash[digest] = append(c.byHash[digest], s.File)
	}
	return c, nil
}

// oneLine is a record's one-line text, nil when there is none.
func oneLine(s *string) *string {
	if s == nil {
		return nil
	}
	v := strings.TrimSpace(strings.NewReplacer("\r", " ", "\n", " ").Replace(*s))
	if v == "" {
		return nil
	}
	return &v
}

// extraLibrary is the library key of an extra's worker record, with the
// setting library.layout=v2: where its folder goes in its title's, where the
// transcoder hands it to the packager and where the packager stages it, what
// its extra.json records, and the original it is packaged from.
type extraLibrary struct {
	Contract   int            `json:"contract"`
	ItemDir    *string        `json:"itemDir"`
	InboxDir   string         `json:"inboxDir"`
	StagingDir string         `json:"stagingDir"`
	ExtraDir   *string        `json:"extraDir"`
	Recorded   bool           `json:"recorded"`
	Record     extraRecordOf  `json:"record"`
	Original   *extraOriginal `json:"original"`
}

// extraRecordOf is what the extra's extra.json records of it.
type extraRecordOf struct {
	Kind            string          `json:"kind"`
	Title           string          `json:"title"`
	LocalizedTitles json.RawMessage `json:"localizedTitles"`
	Language        *string         `json:"language"`
	SeasonNumber    *int32          `json:"seasonNumber"`
	Origin          json.RawMessage `json:"origin"`
	CreatedAt       string          `json:"createdAt"`
	CreatedBy       string          `json:"createdBy"`
}

// extraOriginal is the file the extra is packaged from.
type extraOriginal struct {
	Name      string  `json:"name"`
	SizeBytes *int64  `json:"sizeBytes"`
	QH1       *string `json:"qh1"`
}

// extraLibraryOf is the library key of the extra id's worker record, its
// title recorded first when it is not.
func (h *Handlers) extraLibraryOf(ctx context.Context, id, itemID string) (*extraLibrary, error) {
	p := h.paths()
	pool := h.d.Store.Pool()
	lib := &extraLibrary{Contract: recordContract, InboxDir: p.ExtraInboxDir(id), StagingDir: p.ExtraStagingDir(id)}
	dir, err := p.EnsureItemRecord(ctx, pool, itemID)
	var blocked *library.Blocked
	if errors.As(err, &blocked) {
		log.Printf("the worker record of extra %s: its title %s cannot be recorded: %s", id, itemID, blocked.Reason)
		dir = ""
	} else if err != nil {
		return nil, err
	}
	if dir != "" {
		x := library.ExtraDir(dir, id)
		lib.ItemDir, lib.ExtraDir = &dir, &x
	}
	var localized, origin []byte
	var createdAt time.Time
	var registeredBy string
	var sourcePath *string
	var size *int64
	var qh1 *string
	if err := pool.QueryRow(ctx, `SELECT kind, title, localizedtitles, language, seasonnumber, origin, createdat, registeredby,
			recordedat IS NOT NULL, sourcepath, sourcesize, sourceqh1
		FROM com_nalet_katalog_itemextras WHERE id = $1`, id).Scan(&lib.Record.Kind, &lib.Record.Title, &localized,
		&lib.Record.Language, &lib.Record.SeasonNumber, &origin, &createdAt, &registeredBy, &lib.Recorded, &sourcePath,
		&size, &qh1); err != nil {
		return nil, fmt.Errorf("the extra %s: %w", id, err)
	}
	lib.Record.LocalizedTitles = json.RawMessage("{}")
	if len(localized) > 0 {
		lib.Record.LocalizedTitles = localized
	}
	lib.Record.Origin = json.RawMessage("null")
	if len(origin) > 0 {
		lib.Record.Origin = origin
	}
	lib.Record.CreatedAt = library.Timestamp(createdAt)
	lib.Record.CreatedBy = "katalog-manager/" + registeredBy
	if sourcePath != nil {
		lib.Original = &extraOriginal{Name: filepath.Base(*sourcePath), SizeBytes: size, QH1: qh1}
	}
	return lib, nil
}

// mintVersion makes, with the v2 layout, the version the package's run of the
// item builds, of the source behind its primary asset: when its transcode is
// done and its package is next. A source whose version holds its original
// alone (taken) gets none: the run adds its package to that one. Best-effort,
// as the promotion: the worker record makes it when it is missing.
func (h *Handlers) mintVersion(ctx context.Context, itemID string) {
	set, err := h.settings(ctx)
	if err != nil || !set.V2() {
		return
	}
	pool := h.d.Store.Pool()
	src, err := h.paths().EnsureSource(ctx, pool, itemID)
	if err != nil || src == nil {
		if err != nil {
			log.Printf("putStep: the source of %s: %v", itemID, err)
		}
		return
	}
	if taken, err := library.TakenOf(ctx, pool, src); err != nil || taken != nil {
		if err != nil {
			log.Printf("putStep: the versions of %s: %v", itemID, err)
		}
		return
	}
	if _, err := library.EnsureBuilding(ctx, pool, itemID, []string{src.ID}); err != nil {
		log.Printf("putStep: the version of %s: %v", itemID, err)
	}
}
