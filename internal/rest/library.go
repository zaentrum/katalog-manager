package rest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
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

// recordBuild is the version the run builds, and the marks it carries.
type recordBuild struct {
	VersionID    string          `json:"versionId"`
	StagingDir   string          `json:"stagingDir"`
	VersionDir   string          `json:"versionDir"`
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
// other, labelled with the catalog's kind.
var recordSegmentKinds = map[string]bool{"intro": true, "recap": true, "credits": true, "preview": true, "commercial": true,
	"other": true}

// itemLibraryOf is the library key of the item's worker record: the item is
// recorded first when it is not (its folder gets item.json); the source behind
// its primary asset is made when it has none (a title from before 040); the
// version the run builds is the one the pipeline builds for the item, made
// when there is none. An item that cannot be recorded says why in blocked.
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
	v, err := library.EnsureBuilding(ctx, pool, itemID, sourceIDs)
	if err != nil {
		return nil, err
	}
	b := &recordBuild{VersionID: v.ID, StagingDir: p.StagingDir(v.ID), VersionDir: library.VersionDir(dir, v.ID),
		CreatedBy: "katalog-manager", Chapters: []recordChapter{}, Segments: []recordSegment{}}
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
			label := sg.Kind
			sg.Kind, sg.Label = "other", &label
		}
		b.Segments = append(b.Segments, sg)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	lib.Build = b
	return lib, nil
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
// done and its package is next. Best-effort, as the promotion: the worker
// record makes it when it is missing.
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
	if _, err := library.EnsureBuilding(ctx, pool, itemID, []string{src.ID}); err != nil {
		log.Printf("putStep: the version of %s: %v", itemID, err)
	}
}
