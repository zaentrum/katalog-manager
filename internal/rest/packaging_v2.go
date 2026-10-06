package rest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/zaentrum/katalog-manager/internal/events"
	"github.com/zaentrum/katalog-manager/internal/library"
	"github.com/zaentrum/katalog-manager/internal/model"
	"github.com/zaentrum/katalog-manager/internal/sourceprobe"
	"github.com/zaentrum/katalog-manager/internal/sourcetracks"
	"github.com/zaentrum/katalog-manager/internal/store"
)

// The v2 packaging-complete (platform-library/1, 2.5 and 2.6): the packager
// built a version (an extra's folder) in staging and renamed it into the
// record; the catalog takes it once what it says holds, reading the small
// files of its chain only, and the run is done when it answers 2xx. A version
// it refuses is moved out of the record, so the next run renames a whole one
// into its place.

// refused is a v2 packaging-complete the catalog does not take: a stale run
// (409) or a chain that does not hold (422), saying why.
type refused struct {
	status int
	reason string
}

func (e *refused) Error() string { return e.reason }

func stale(format string, args ...any) error {
	return &refused{status: http.StatusConflict, reason: fmt.Sprintf(format, args...)}
}

func brokenChain(format string, args ...any) error {
	return &refused{status: http.StatusUnprocessableEntity, reason: fmt.Sprintf(format, args...)}
}

// v2Payload is what the packager sends for a version (2.5) or an extra (2.6).
type v2Payload struct {
	Layout         string          `json:"layout"`
	VersionID      string          `json:"versionId"`
	ExtraID        string          `json:"extraId"`
	PackageID      string          `json:"packageId"`
	VersionDir     string          `json:"versionDir"`
	ExtraDir       string          `json:"extraDir"`
	Complete       string          `json:"complete"`
	SourceID       string          `json:"sourceId"`
	SourceRecorded bool            `json:"sourceRecorded"`
	Package        json.RawMessage `json:"package"`
	Sidecars       []v2Sidecar     `json:"sidecars"`
	Source         map[string]any  `json:"source"`
}

// v2Sidecar maps a subtitle file the scanner paired (its subtitle asset) to
// the rendition the packager made of it.
type v2Sidecar struct {
	SubtitleAssetID string `json:"subtitleAssetId"`
	Rendition       string `json:"rendition"`
	Path            string `json:"path"`
}

// isV2 reports whether a packaging-complete body is the v2 payload.
func isV2(body map[string]any) bool {
	l, _ := body["layout"].(string)
	return l == "v2"
}

// chainOf reads the small files of a package folder's chain and checks them
// against the payload: .complete is complete, the hash of package.json,
// whose packageId is the payload's. It answers package.json, decoded.
func chainOf(dir, complete, packageID string) (map[string]any, []byte, error) {
	marker, err := os.ReadFile(filepath.Join(dir, library.CompleteFile))
	if err != nil {
		return nil, nil, brokenChain("%s: %v", filepath.Join(dir, library.CompleteFile), err)
	}
	body, err := os.ReadFile(filepath.Join(dir, library.PackageFile))
	if err != nil {
		return nil, nil, brokenChain("%s: %v", filepath.Join(dir, library.PackageFile), err)
	}
	sum := sha256.Sum256(body)
	hash := "sha256:" + hex.EncodeToString(sum[:])
	switch {
	case strings.TrimSpace(string(marker)) != complete:
		return nil, nil, brokenChain(".complete holds %s, and the packager says %s", strings.TrimSpace(string(marker)), complete)
	case complete != hash:
		return nil, nil, brokenChain("package.json's hash is %s, and .complete says %s", hash, complete)
	}
	var pkg map[string]any
	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.UseNumber()
	if err := dec.Decode(&pkg); err != nil || pkg == nil {
		return nil, nil, brokenChain("package.json is no JSON object: %v", err)
	}
	if id := asString(pkg["packageId"]); id == nil || *id != packageID {
		return nil, nil, brokenChain("package.json's packageId is %v, and the packager says %s", deref(id), packageID)
	}
	return pkg, body, nil
}

func deref(s *string) string {
	if s == nil {
		return "none"
	}
	return *s
}

// readRecord reads a record of a package folder (version.json, extra.json).
func readRecord(path string) (map[string]any, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, brokenChain("%s: %v", path, err)
	}
	var doc map[string]any
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.UseNumber()
	if err := dec.Decode(&doc); err != nil || doc == nil {
		return nil, brokenChain("%s is no JSON object", path)
	}
	return doc, nil
}

// supersedeEventID is the id of the event that records the supersession of
// the version old by next: the same for the same two, so a call taken again
// finds the event it wrote.
func supersedeEventID(old, next string) string {
	return library.IDOf("package-superseded:" + old + ":" + next)
}

// packagingCompleteV2 serves the v2 payload of POST
// /api/items/{id}/packaging-complete (2.5). It takes the version when:
//   - versionId is the item's version being built, or its complete one (a
//     report taken again: the same answer);
//   - versionDir is VersionDir(the item, versionId);
//   - .complete holds complete, which is the hash of package.json, whose
//     packageId is packageId;
//   - version.json names versionId, and sourceId among its sources, whose
//     record (sources/<id>/checksums.sha256) is written.
//
// A stale run is 409, a chain that does not hold 422; the version's folder is
// then moved out of the record unless it is a version the catalog keeps. A
// version taken: the one it supersedes gets its package-superseded event
// first, then one transaction records the version complete (the other
// superseded), its source recorded, the packaged asset of package.json, the
// package's subtitles, the source's tracks and probe, and the item modified.
func (h *Handlers) packagingCompleteV2(w http.ResponseWriter, r *http.Request, itemID string, raw map[string]any) {
	ctx := reqCtx(r)
	var in v2Payload
	if b, err := json.Marshal(raw); err != nil || json.Unmarshal(b, &in) != nil {
		writeError(w, http.StatusBadRequest, "the body is no v2 payload")
		return
	}
	for _, f := range [][2]string{{"versionId", in.VersionID}, {"packageId", in.PackageID}, {"versionDir", in.VersionDir},
		{"complete", in.Complete}, {"sourceId", in.SourceID}} {
		if strings.TrimSpace(f[1]) == "" {
			writeError(w, http.StatusBadRequest, "the v2 payload names no "+f[0])
			return
		}
	}
	answer, err := h.takeVersion(ctx, itemID, in)
	var no *refused
	switch {
	case errors.As(err, &no):
		if moved := h.dropRefusedVersion(ctx, itemID, in.VersionID); moved != "" {
			no.reason += "; the version's folder is moved out of the record, to " + moved
		}
		log.Printf("packagingComplete: %s version %s refused: %s", itemID, in.VersionID, no.reason)
		writeError(w, no.status, no.reason)
		return
	case errors.Is(err, library.ErrNoItem):
		writeError(w, http.StatusNotFound, "unknown item: "+itemID)
		return
	case err != nil:
		log.Printf("packagingComplete: %s version %s: %v", itemID, in.VersionID, err)
		writeError(w, http.StatusInternalServerError, "the version could not be recorded: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, answer)
}

// takeVersion validates and records the version of the payload in (see
// packagingCompleteV2), and answers what it did.
func (h *Handlers) takeVersion(ctx context.Context, itemID string, in v2Payload) (map[string]any, error) {
	pool := h.d.Store.Pool()
	p := h.paths()
	pl, err := library.PlaceOf(ctx, pool, itemID)
	var unplaced *library.Unplaced
	if errors.As(err, &unplaced) {
		return nil, stale("the item has no folder in the library: %s", unplaced.Reason)
	}
	if err != nil {
		return nil, err
	}
	itemDir := p.ItemDir(pl)
	v, err := library.VersionByID(ctx, pool, in.VersionID)
	if err != nil {
		return nil, err
	}
	switch {
	case v == nil || v.ItemID != itemID:
		return nil, stale("version %s is no version the item builds", in.VersionID)
	case v.State == library.VersionSuperseded || v.State == library.VersionRemoved:
		return nil, stale("version %s is %s: the run is stale", in.VersionID, v.State)
	}
	want := library.VersionDir(itemDir, in.VersionID)
	if filepath.Clean(in.VersionDir) != want {
		return nil, stale("versionDir is %s, and the version's folder is %s", in.VersionDir, want)
	}
	pkg, _, err := chainOf(want, in.Complete, in.PackageID)
	if err != nil {
		return nil, err
	}
	ver, err := readRecord(filepath.Join(want, library.VersionFile))
	if err != nil {
		return nil, err
	}
	if id := asString(ver["versionId"]); id == nil || *id != in.VersionID {
		return nil, brokenChain("version.json names version %s, not %s", deref(id), in.VersionID)
	}
	sources := []string{}
	for _, s := range asList(ver["sourceIds"]) {
		if id := asString(s); id != nil {
			sources = append(sources, *id)
		}
	}
	if !contains(sources, in.SourceID) {
		return nil, brokenChain("version.json's sources %v do not name source %s", sources, in.SourceID)
	}
	if _, err := os.Stat(filepath.Join(library.SourceDir(itemDir, in.SourceID), library.SumsFile)); err != nil {
		return nil, brokenChain("the source's record sources/%s is not written: %v", in.SourceID, err)
	}
	if v.State == library.VersionComplete {
		return h.versionAnswer(ctx, itemID, in.VersionID, pkg)
	}

	// The version it supersedes gets its event first: the record comes first.
	prev, err := library.Current(ctx, pool, itemID)
	if err != nil {
		return nil, err
	}
	var ev *library.Event
	if prev != nil {
		ev = &library.Event{ID: supersedeEventID(prev.ID, in.VersionID), At: time.Now().UTC().Truncate(time.Second),
			By: "katalog-manager", Kind: library.EventPackageSuperseded, VersionID: prev.ID,
			Successor: &library.Successor{VersionID: in.VersionID, PackageID: in.PackageID}}
		if prev.PackageID != nil {
			ev.PackageID = *prev.PackageID
		}
		if found, err := library.FindEvent(itemDir, ev.ID, ev.Kind); err != nil {
			return nil, err
		} else if found != "" {
			if at, err := time.Parse("20060102T150405Z", filepath.Base(found)[:16]); err == nil {
				ev.At = at
			}
		}
		if _, err := library.WriteEvent(itemDir, *ev); err != nil {
			return nil, err
		}
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	if err := library.LockItem(ctx, tx, itemID); err != nil {
		return nil, err
	}
	cur, err := library.VersionByID(ctx, tx, in.VersionID)
	if err != nil {
		return nil, err
	}
	if cur == nil || cur.State != library.VersionBuilding {
		if cur != nil && cur.State == library.VersionComplete {
			return h.versionAnswer(ctx, itemID, in.VersionID, pkg)
		}
		return nil, stale("version %s is no longer being built", in.VersionID)
	}
	if prev != nil {
		if _, err := tx.Exec(ctx, `UPDATE com_nalet_katalog_itemversions SET state = 'superseded', supersededby = $2,
				supersededat = $3, supersedeeventid = $4, modifiedat = now()
			WHERE id = $1 AND state = 'complete'`, prev.ID, in.VersionID, ev.At, ev.ID); err != nil {
			return nil, err
		}
	}
	completed := time.Now().UTC()
	if c := asString(pkg["createdAt"]); c != nil {
		if t, err := time.Parse(time.RFC3339, *c); err == nil {
			completed = t
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE com_nalet_katalog_itemversions SET state = 'complete', packageid = $2, dir = $3,
			completedat = $4, sourceids = $5, modifiedat = now()
		WHERE id = $1`, in.VersionID, in.PackageID, want, completed, sources); err != nil {
		return nil, err
	}
	sidecars, err := json.Marshal(in.Sidecars)
	if err != nil {
		return nil, err
	}
	if in.Sidecars == nil {
		sidecars = []byte("[]")
	}
	if _, err := tx.Exec(ctx, `UPDATE com_nalet_katalog_itemsources SET recordedat = COALESCE(recordedat, now()), recorddir = $2,
			sidecars = $3, modifiedat = now()
		WHERE id = $1`, in.SourceID, library.SourceDir(itemDir, in.SourceID), sidecars); err != nil {
		return nil, err
	}
	written, err := h.writePackaged(ctx, tx, itemID, itemDir, want, in.VersionID, pkg)
	if err != nil {
		return nil, err
	}
	if _, err := h.d.Store.RecordSourceTracksIn(ctx, tx, itemID,
		sourcetracks.FromManifest(tracksManifest(pkg, library.SourceDir(itemDir, in.SourceID)))); err != nil {
		return nil, err
	}
	if _, err := sourceprobe.Fill(ctx, tx, itemID, sourceFromManifest(in.Source)); err != nil {
		return nil, err
	}
	if d, ok := asLong(pkg["durationMs"]); ok && d > 0 {
		if err := sourceprobe.FillEmpty(ctx, tx, itemID, sourceprobe.Probe{DurationMs: &d}); err != nil {
			return nil, err
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE com_nalet_katalog_items SET modifiedat = now() WHERE id = $1`, itemID); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}

	ev2 := events.NewItemEvent(itemID)
	ev2.Type, ev2.Status, ev2.Source = pl.Type, "done", "katalog-manager"
	h.d.Events.EmitItem(ctx, events.TopicPackaged, ev2)
	answer := map[string]any{"itemId": itemID, "versionId": in.VersionID, "current": true, "superseded": nil,
		"packagedAssetWritten": true, "subtitlesWritten": written, "audioTracks": len(asListOfMap(asMap(pkg["renditions"])["audio"]))}
	if prev != nil {
		answer["superseded"] = prev.ID
	}
	return answer, nil
}

// versionAnswer is the answer to a version taken already: what its taking
// said.
func (h *Handlers) versionAnswer(ctx context.Context, itemID, versionID string, pkg map[string]any) (map[string]any, error) {
	var old *string
	if err := h.d.Store.Pool().QueryRow(ctx, `SELECT id FROM com_nalet_katalog_itemversions WHERE supersededby = $1
		ORDER BY supersededat DESC NULLS LAST LIMIT 1`, versionID).Scan(&old); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	answer := map[string]any{"itemId": itemID, "versionId": versionID, "current": true, "superseded": nil,
		"packagedAssetWritten": true, "subtitlesWritten": len(packageSubtitles(pkg)),
		"audioTracks": len(asListOfMap(asMap(pkg["renditions"])["audio"]))}
	if old != nil {
		answer["superseded"] = *old
	}
	return answer, nil
}

// packageSubtitles are the package's subtitle renditions a row is written
// for: those made of a stream of the source, not of a subtitle file beside it
// (fromSidecar), whose rows keep pointing at the file.
func packageSubtitles(pkg map[string]any) []map[string]any {
	var out []map[string]any
	for _, s := range asListOfMap(pkg["subtitles"]) {
		if from := asString(s["fromSidecar"]); from != nil && *from != "" {
			continue
		}
		out = append(out, s)
	}
	return out
}

// writePackaged replaces, in tx, the item's packaged asset with the one of the
// version (package.json is its record) and the package's subtitles with the
// version's; it answers how many subtitles it wrote. Every subtitle is
// written not default: which one comes on is the clients' to decide.
func (h *Handlers) writePackaged(ctx context.Context, tx pgx.Tx, itemID, itemDir, versionDir, versionID string,
	pkg map[string]any) (int, error) {
	if _, err := tx.Exec(ctx, `DELETE FROM com_nalet_katalog_playbackassets WHERE item_id = $1 AND kind = 'packaged'`, itemID); err != nil {
		return 0, err
	}
	ren := asMap(pkg["renditions"])
	video := asListOfMap(ren["video"])
	audio := asListOfMap(ren["audio"])
	var codec, res *string
	if len(video) > 0 {
		codec = asString(video[0]["codec"])
		if wd, ok := asInt(video[0]["width"]); ok {
			if ht, ok := asInt(video[0]["height"]); ok {
				s := strconv.Itoa(wd) + "x" + strconv.Itoa(ht)
				res = &s
			}
		}
	}
	var kbps *int64
	if peak, ok := asLong(pkg["peakBandwidthBps"]); ok && peak > 0 {
		k := peak / 1000
		kbps = &k
	}
	var size, duration *int64
	if n, ok := asLong(pkg["sizeBytes"]); ok {
		size = &n
	}
	if d, ok := asLong(pkg["durationMs"]); ok {
		duration = &d
	}
	var primary map[string]any
	for _, a := range audio {
		if def, _ := a["default"].(bool); def {
			primary = a
			break
		}
	}
	if primary == nil && len(audio) > 0 {
		primary = audio[0]
	}
	var aCodec, aLang *string
	var aChannels, aKbps *int
	if primary != nil {
		aCodec, aLang = asString(primary["codec"]), asString(primary["language"])
		if ch, ok := asInt(primary["channels"]); ok {
			aChannels = &ch
		}
		if bps, ok := asLong(primary["bitrateBps"]); ok && bps > 0 {
			k := int(bps / 1000)
			aKbps = &k
		}
	}
	if _, err := tx.Exec(ctx, `INSERT INTO com_nalet_katalog_playbackassets
		(id, item_id, path, codec, resolution, bitratekbps, sizebytes, isprimary, kind, audiocodec, audiolanguage,
		 audiochannels, audiobitratekbps, audiotrackcount, subtitletrackcount, durationms, versionid)
		VALUES (gen_random_uuid()::varchar, $1, $2, $3, $4, $5, $6, false, 'packaged', $7, $8, $9, $10, $11, $12, $13, $14)`,
		itemID, filepath.Join(versionDir, library.PackageFile), codec, res, kbps, size, aCodec, aLang, aChannels, aKbps,
		len(audio), len(asListOfMap(pkg["subtitles"])), duration, versionID); err != nil {
		return 0, err
	}
	// The package's rows go, those of every version and of the legacy
	// package store; a subtitle file's row beside the source stays.
	rows, err := tx.Query(ctx, `SELECT id, path FROM com_nalet_katalog_subtitleassets WHERE item_id = $1`, itemID)
	if err != nil {
		return 0, err
	}
	var gone []string
	versions := filepath.Join(itemDir, "versions")
	for rows.Next() {
		var id, path string
		if err := rows.Scan(&id, &path); err != nil {
			rows.Close()
			return 0, err
		}
		if library.Within(versions, path) || underRoot(h.d.Cfg.PackagesRoot, path) {
			gone = append(gone, id)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if len(gone) > 0 {
		if _, err := tx.Exec(ctx, `DELETE FROM com_nalet_katalog_subtitleassets WHERE id = ANY($1)`, gone); err != nil {
			return 0, err
		}
	}
	written := 0
	for _, s := range packageSubtitles(pkg) {
		rel := asString(s["path"])
		if rel == nil || *rel == "" {
			continue
		}
		label := asString(s["name"])
		if label == nil || strings.TrimSpace(*label) == "" {
			label = asString(s["title"])
		}
		forced, _ := s["forced"].(bool)
		if _, err := tx.Exec(ctx, `INSERT INTO com_nalet_katalog_subtitleassets
			(id, item_id, path, format, lang, label, isdefault, isforced)
			VALUES (gen_random_uuid()::varchar, $1, $2, $3, $4, $5, false, $6)`,
			itemID, filepath.Join(versionDir, *rel), asString(s["format"]), asString(s["language"]), label, forced); err != nil {
			return 0, err
		}
		written++
	}
	return written, nil
}

// tracksManifest is the package's word on its source's tracks in the terms
// sourcetracks reads (a legacy manifest's): each audio rendition at the
// ordinal of the stream it was made from among the source's audio streams,
// each subtitle made of a stream at its ordinal among the subtitle streams,
// by the source record's streams (its sourceStreamIndex); one made of a
// subtitle file beside the source is no stream of it. A rendition whose
// stream the record does not list counts by its place, as a legacy manifest's
// does.
func tracksManifest(pkg map[string]any, sourceDir string) map[string]any {
	ordinals := map[string]map[int64]int{}
	if src, err := readRecord(filepath.Join(sourceDir, "source.json")); err == nil {
		n := map[string]int{}
		for _, s := range asListOfMap(src["streams"]) {
			typ := ""
			if t := asString(s["type"]); t != nil {
				typ = *t
			}
			idx, ok := asLong(s["index"])
			if !ok || (typ != "audio" && typ != "subtitle") {
				continue
			}
			if ordinals[typ] == nil {
				ordinals[typ] = map[int64]int{}
			}
			ordinals[typ][idx] = n[typ]
			n[typ]++
		}
	}
	ordinalOf := func(kind string, r map[string]any) (int, bool) {
		if idx, ok := asLong(r["sourceStreamIndex"]); ok {
			o, ok := ordinals[kind][idx]
			return o, ok
		}
		return 0, false
	}
	out := map[string]any{}
	ren := asMap(pkg["renditions"])
	if list, ok := ren["audio"].([]any); ok {
		audio := []any{}
		for i, e := range list {
			a, ok := e.(map[string]any)
			if !ok {
				continue
			}
			c := map[string]any{"language": a["language"], "title": a["title"], "ordinal": i}
			if o, ok := ordinalOf("audio", a); ok {
				c["ordinal"] = o
			}
			audio = append(audio, c)
		}
		out["renditions"] = map[string]any{"audio": audio}
	}
	if list, ok := pkg["subtitles"].([]any); ok {
		subs := []any{}
		for _, e := range list {
			s, ok := e.(map[string]any)
			if !ok {
				continue
			}
			c := map[string]any{"id": s["id"], "language": s["language"], "title": s["title"], "format": s["format"],
				"forced": s["forced"]}
			if from := asString(s["fromSidecar"]); from != nil && *from != "" {
				c["external"] = true
			} else if o, ok := ordinalOf("subtitle", s); ok {
				c["ordinal"] = o
			}
			subs = append(subs, c)
		}
		out["subtitles"] = subs
	}
	return out
}

// dropRefusedVersion moves the folder of a version the catalog refused out of
// the record, into the work folder's legacy/, which the sweep empties after
// its grace: a version folder the catalog does not keep is no record. A
// version the catalog keeps (complete, superseded, removed) is never moved.
// It answers where the folder went, "" when nothing moved.
func (h *Handlers) dropRefusedVersion(ctx context.Context, itemID, versionID string) string {
	if !library.ValidID(versionID) {
		return ""
	}
	pool := h.d.Store.Pool()
	pl, err := library.PlaceOf(ctx, pool, itemID)
	if err != nil {
		return ""
	}
	v, err := library.VersionByID(ctx, pool, versionID)
	if err != nil || (v != nil && (v.ItemID != itemID || v.State != library.VersionBuilding)) {
		return ""
	}
	p := h.paths()
	dir := library.VersionDir(p.ItemDir(pl), versionID)
	if fi, err := os.Lstat(dir); err != nil || !fi.IsDir() {
		return ""
	}
	to := filepath.Join(p.LegacyDay(time.Now()), "refused", itemID+"-"+versionID+"-"+library.NewID()[:8])
	if err := library.MkdirAll(filepath.Dir(to)); err != nil {
		log.Printf("packagingComplete: the refused version %s cannot be moved out of the record: %v", dir, err)
		return ""
	}
	if err := os.Rename(dir, to); err != nil {
		log.Printf("packagingComplete: the refused version %s cannot be moved out of the record: %v", dir, err)
		return ""
	}
	return to
}

func asList(o any) []any {
	l, _ := o.([]any)
	return l
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// extraPackagingCompleteV2 serves the v2 payload of POST
// /api/extras/{id}/packaging-complete (2.6), for the live extra x of the
// title it: extraDir is ExtraDir(the title, the extra), its chain holds as a
// version's does (.complete, package.json, its packageId) and extra.json
// names the extra. The extra is then recorded, ready (one whose file is
// missing stays missing), its package's folder its record's, with what its
// top rendition and package.json say; announced on catalog.extra.packaged. An
// extra recorded already is written once: its own package again is the same
// answer, another is stale (409). A refused folder of an extra not recorded
// is moved out of the record.
func (h *Handlers) extraPackagingCompleteV2(w http.ResponseWriter, r *http.Request, x *model.Extra, it *model.Item,
	raw map[string]any) {
	ctx := reqCtx(r)
	var in v2Payload
	if b, err := json.Marshal(raw); err != nil || json.Unmarshal(b, &in) != nil {
		writeError(w, http.StatusBadRequest, "the body is no v2 payload")
		return
	}
	for _, f := range [][2]string{{"extraId", in.ExtraID}, {"extraDir", in.ExtraDir}, {"packageId", in.PackageID},
		{"complete", in.Complete}} {
		if strings.TrimSpace(f[1]) == "" {
			writeError(w, http.StatusBadRequest, "the v2 payload names no "+f[0])
			return
		}
	}
	done, pkg, err := h.takeExtra(ctx, x, in)
	var no *refused
	switch {
	case errors.As(err, &no):
		if moved := h.dropRefusedExtra(ctx, x); moved != "" {
			no.reason += "; the extra's folder is moved out of the record, to " + moved
		}
		log.Printf("extraPackagingComplete: extra %s refused: %s", x.ID, no.reason)
		writeError(w, no.status, no.reason)
		return
	case errors.Is(err, store.ErrExtraGone):
		writeError(w, http.StatusNotFound, "no such extra: "+x.ID)
		return
	case err != nil:
		log.Printf("extraPackagingComplete: %s: %v", x.ID, err)
		writeError(w, http.StatusInternalServerError, "the package could not be recorded: "+err.Error())
		return
	}
	if done.State == model.ExtraReady {
		ev := events.ExtraPackagedEvent{ItemEvent: events.NewItemEvent(done.ItemID), ExtraID: done.ID, Kind: done.Kind}
		ev.Type, ev.Step, ev.Status, ev.Source = it.Type, "extra", "done", "katalog-manager"
		h.d.Events.Emit(ctx, events.TopicExtraPackaged, done.ID, ev)
	}
	var duration *int64
	if d, ok := asLong(pkg["durationMs"]); ok && d >= 0 {
		duration = &d
	}
	writeJSON(w, http.StatusOK, map[string]any{"extraId": x.ID, "itemId": done.ItemID, "packaged": true, "durationMs": duration})
}

// takeExtra validates and records the extra's folder of the payload in (see
// extraPackagingCompleteV2): the extra as it is after, and its package.json.
func (h *Handlers) takeExtra(ctx context.Context, x *model.Extra, in v2Payload) (*model.Extra, map[string]any, error) {
	pool := h.d.Store.Pool()
	if in.ExtraID != x.ID {
		return nil, nil, stale("the payload is of extra %s, not %s", in.ExtraID, x.ID)
	}
	pl, err := library.PlaceOf(ctx, pool, x.ItemID)
	var unplaced *library.Unplaced
	if errors.As(err, &unplaced) {
		return nil, nil, stale("the extra's title has no folder in the library: %s", unplaced.Reason)
	}
	if err != nil {
		return nil, nil, err
	}
	want := library.ExtraDir(h.paths().ItemDir(pl), x.ID)
	if filepath.Clean(in.ExtraDir) != want {
		return nil, nil, stale("extraDir is %s, and the extra's folder is %s", in.ExtraDir, want)
	}
	var recorded bool
	var packageID *string
	if err := pool.QueryRow(ctx, `SELECT recordedat IS NOT NULL, packageid FROM com_nalet_katalog_itemextras WHERE id = $1`,
		x.ID).Scan(&recorded, &packageID); err != nil {
		return nil, nil, err
	}
	pkg, _, err := chainOf(want, in.Complete, in.PackageID)
	if err != nil {
		if recorded {
			return nil, nil, &refused{status: http.StatusConflict, reason: "the extra is recorded already, written once: " + err.Error()}
		}
		return nil, nil, err
	}
	rec, err := readRecord(filepath.Join(want, library.ExtraFile))
	if err != nil {
		return nil, nil, err
	}
	if id := asString(rec["extraId"]); id == nil || *id != x.ID {
		return nil, nil, brokenChain("extra.json names extra %s, not %s", deref(id), x.ID)
	}
	if recorded {
		if packageID == nil || *packageID != in.PackageID {
			return nil, nil, stale("the extra is recorded with package %s, written once; add the file again as a new extra",
				deref(packageID))
		}
		return x, pkg, nil
	}
	video := asListOfMap(asMap(pkg["renditions"])["video"])
	if len(video) == 0 {
		return nil, nil, brokenChain("package.json names no video rendition")
	}
	top := video[0]
	for _, v := range video[1:] {
		h1, _ := asInt(v["height"])
		h0, _ := asInt(top["height"])
		if h1 > h0 {
			top = v
		}
	}
	ep := store.ExtraPackage{Path: want, VideoCodec: asString(top["codec"])}
	if d, ok := asLong(pkg["durationMs"]); ok && d >= 0 {
		ep.DurationMs = &d
	}
	if wd, ok := asInt(top["width"]); ok && wd > 0 {
		v := int32(wd)
		ep.Width = &v
	}
	if ht, ok := asInt(top["height"]); ok && ht > 0 {
		v := int32(ht)
		ep.Height = &v
	}
	if peak, ok := asLong(pkg["peakBandwidthBps"]); ok && peak > 0 {
		ep.PeakBandwidthBps = &peak
	}
	if n, ok := asLong(pkg["sizeBytes"]); ok && n >= 0 {
		ep.SizeBytes = &n
	}
	done, err := h.d.Store.RecordExtraPackage(ctx, x.ID, ep, in.PackageID)
	return done, pkg, err
}

// dropRefusedExtra moves the folder of a refused extra out of the record,
// unless the extra is recorded (written once, it stays). It answers where
// the folder went, "" when nothing moved.
func (h *Handlers) dropRefusedExtra(ctx context.Context, x *model.Extra) string {
	pool := h.d.Store.Pool()
	var recorded bool
	if err := pool.QueryRow(ctx, `SELECT recordedat IS NOT NULL FROM com_nalet_katalog_itemextras WHERE id = $1`, x.ID).
		Scan(&recorded); err != nil || recorded {
		return ""
	}
	pl, err := library.PlaceOf(ctx, pool, x.ItemID)
	if err != nil {
		return ""
	}
	p := h.paths()
	dir := library.ExtraDir(p.ItemDir(pl), x.ID)
	if fi, err := os.Lstat(dir); err != nil || !fi.IsDir() {
		return ""
	}
	to := filepath.Join(p.LegacyDay(time.Now()), "refused", "extra-"+x.ID+"-"+library.NewID()[:8])
	if err := library.MkdirAll(filepath.Dir(to)); err != nil || os.Rename(dir, to) != nil {
		log.Printf("extraPackagingComplete: the refused extra %s cannot be moved out of the record", dir)
		return ""
	}
	return to
}
