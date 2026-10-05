package rest

import (
	"context"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/zaentrum/katalog-manager/internal/events"
	"github.com/zaentrum/katalog-manager/internal/model"
	"github.com/zaentrum/katalog-manager/internal/sourceprobe"
	"github.com/zaentrum/katalog-manager/internal/sourcetracks"
)

// packagingComplete ports ItemActionsController#ingestPackagingManifest: the
// packager machine sink. Idempotent; four writes (source-asset enrich, packaged
// asset replace, the package's subtitles replaced, the source's tracks
// recorded). Returns
// {itemId, sourceEnriched, packagedAssetWritten, subtitlesWritten, audioTracks}.
//
// A language the catalog derives from the package is the one the track plays
// as: an admin's (migration 037) where one is set, else the manifest's.
func (h *Handlers) packagingComplete(w http.ResponseWriter, r *http.Request) {
	itemID := chi.URLParam(r, "id")
	ctx := reqCtx(r)
	pool := h.d.Store.Pool()

	ok, err := itemExists(ctx, pool, itemID)
	if err != nil {
		http.Error(w, "item lookup failed", http.StatusInternalServerError)
		return
	}
	if !ok {
		writeError(w, http.StatusNotFound, "unknown item: "+itemID)
		return
	}

	var manifest map[string]any
	if err := decodeJSON(r, &manifest); err != nil || manifest == nil {
		manifest = map[string]any{}
	}

	source := asMap(manifest["source"])
	renditions := asMap(manifest["renditions"])
	videoRends := asListOfMap(renditions["video"])
	audioRends := asListOfMap(renditions["audio"])
	subtitles := asListOfMap(manifest["subtitles"])

	// The languages an admin set for the source's tracks.
	set, err := h.d.Store.TrackLanguages(ctx, itemID)
	if err != nil {
		http.Error(w, "track languages lookup failed", http.StatusInternalServerError)
		return
	}
	setLanguage := map[string]string{}
	for _, l := range set {
		setLanguage[l.Kind+"/"+strconv.Itoa(int(l.Ordinal))] = l.Language
	}
	playsAs := func(kind string, ordinal int32, reported *string) *string {
		if l, ok := setLanguage[kind+"/"+strconv.Itoa(int(ordinal))]; ok {
			return &l
		}
		return reported
	}

	// 1. Source PlaybackAsset enrichment: what a source block says of the
	// source (a present value wins), then the packaged duration for a source
	// that has none (the packager probes what it packages: the source for a
	// copy, the transcode, equal up to a frame, for an encode). The v2
	// manifest has no source block; the transcoder's report filled the codec
	// and resolution (putStep).
	src := sourceFromManifest(source)
	if _, err := sourceprobe.Fill(ctx, pool, itemID, src); err != nil {
		http.Error(w, "source enrich failed", http.StatusInternalServerError)
		return
	}
	if d, ok := asLong(manifest["durationMs"]); ok && d > 0 {
		if err := sourceprobe.FillEmpty(ctx, pool, itemID, sourceprobe.Probe{DurationMs: &d}); err != nil {
			http.Error(w, "source enrich failed", http.StatusInternalServerError)
			return
		}
	}

	// 2. Packaged PlaybackAsset row: DELETE then INSERT (only when video rends exist).
	if _, err := pool.Exec(ctx,
		`DELETE FROM com_nalet_katalog_playbackassets WHERE item_id = $1 AND kind = 'packaged'`,
		itemID); err != nil {
		http.Error(w, "packaged delete failed", http.StatusInternalServerError)
		return
	}

	packageRoot, err := h.packageRootFor(ctx, itemID)
	if err != nil {
		http.Error(w, "package root resolve failed", http.StatusInternalServerError)
		return
	}

	packagedWritten := len(videoRends) > 0
	if packagedWritten {
		v := videoRends[0]
		pkgCodec := asString(v["codec"])
		width, wOK := asInt(v["width"])
		height, hOK := asInt(v["height"])
		var pkgRes *string
		if wOK && hOK {
			s := strconv.Itoa(width) + "x" + strconv.Itoa(height)
			pkgRes = &s
		}

		var pkgKbps *int
		if k := readPackagedBitrateKbps(packageRoot); k != nil {
			pkgKbps = k
		} else if bps, ok := asLong(v["bitrateBps"]); ok && bps > 0 {
			k := int(bps / 1000)
			pkgKbps = &k
		}

		pkgSize := sumPackageBytes(packageRoot)

		// primary audio = first default:true else audio[0].
		var primaryAudio map[string]any
		primaryAt := 0
		for i, a := range audioRends {
			if def, ok := a["default"].(bool); ok && def {
				primaryAudio, primaryAt = a, i
				break
			}
		}
		if primaryAudio == nil && len(audioRends) > 0 {
			primaryAudio = audioRends[0]
		}
		var audioCodec, audioLanguage *string
		var audioChannels, audioBitrateKbps *int
		if primaryAudio != nil {
			audioCodec = asString(primaryAudio["codec"])
			audioLanguage = asString(primaryAudio["language"])
			if ordinal, ok := sourcetracks.AudioOrdinal(primaryAudio, primaryAt); ok {
				audioLanguage = playsAs(model.TrackAudio, ordinal, audioLanguage)
			}
			if ch, ok := asInt(primaryAudio["channels"]); ok {
				audioChannels = &ch
			}
			if abps, ok := asLong(primaryAudio["bitrateBps"]); ok && abps > 0 {
				k := int(abps / 1000)
				audioBitrateKbps = &k
			}
		}
		audioTrackCount := len(audioRends)
		subtitleTrackCount := len(subtitles)
		var durationMs *int64
		if d, ok := asLong(manifest["durationMs"]); ok {
			durationMs = &d
		}

		if _, err := pool.Exec(ctx, `
			INSERT INTO com_nalet_katalog_playbackassets
			(id, item_id, path, codec, resolution, bitratekbps, sizebytes, isprimary, kind,
			 audiocodec, audiolanguage, audiochannels, audiobitratekbps,
			 audiotrackcount, subtitletrackcount, durationms)
			VALUES (gen_random_uuid()::varchar, $1, $2, $3, $4, $5, $6, false, 'packaged',
			        $7, $8, $9, $10, $11, $12, $13)`,
			itemID,
			packageRoot+"/manifest.json",
			pkgCodec, pkgRes, pkgKbps, pkgSize,
			audioCodec, audioLanguage, audioChannels, audioBitrateKbps,
			audioTrackCount, subtitleTrackCount, durationMs); err != nil {
			http.Error(w, "packaged insert failed", http.StatusInternalServerError)
			return
		}
	}

	// 3. SubtitleAssets: the package's are replaced by the manifest's. The
	// subtitle files beside the source, which the scanner keeps, stay: they
	// are no package's (sidecarSubtitle), and the worker record hands them to
	// the packager as subtitleFiles. A manifest subtitle that is no stream of
	// the source (an id other than sub<N>: the packager's rendition of such a
	// file) gets no row of its own, as the file's row stands for it.
	if err := h.deletePackageSubtitles(ctx, itemID, packageRoot); err != nil {
		http.Error(w, "subtitle delete failed", http.StatusInternalServerError)
		return
	}
	subsWritten := 0
	for _, s := range subtitles {
		ordinal, stream := sourcetracks.SubtitleOrdinal(s)
		if !stream {
			continue
		}
		relPath := asString(s["path"])
		var fullPath *string
		if relPath != nil {
			p := packageRoot + "/" + *relPath
			fullPath = &p
		}
		isDefault := false
		if def, ok := s["default"].(bool); ok {
			isDefault = def
		}
		if _, err := pool.Exec(ctx, `
			INSERT INTO com_nalet_katalog_subtitleassets
			(id, item_id, path, format, lang, label, isdefault)
			VALUES (gen_random_uuid()::varchar, $1, $2, $3, $4, $5, $6)`,
			itemID, fullPath, asString(s["format"]), playsAs(model.TrackSubtitle, ordinal, asString(s["language"])),
			asString(s["title"]), isDefault); err != nil {
			http.Error(w, "subtitle insert failed", http.StatusInternalServerError)
			return
		}
		subsWritten++
	}

	// 4. The source's tracks, as the package reports them (best-effort, as
	// the promotion: the package is in place whatever the catalog keeps of
	// its source).
	if _, err := h.d.Store.RecordSourceTracks(ctx, itemID, sourcetracks.FromManifest(manifest)); err != nil {
		log.Printf("packagingComplete: the tracks of %s: %v", itemID, err)
	}

	// The pipeline's end: the packaged (playable) asset is now in the catalog —
	// announce it so live-refresh surfaces update at the exact became-watchable
	// moment. Emitted only on a real packaged write (nil-safe without a bus).
	if packagedWritten {
		var typ *string
		_ = pool.QueryRow(ctx, `SELECT type FROM com_nalet_katalog_items WHERE id = $1`, itemID).Scan(&typ)
		ev := events.NewItemEvent(itemID)
		if typ != nil {
			ev.Type = *typ
		}
		ev.Status = "done"
		ev.Source = "katalog-manager"
		h.d.Events.EmitItem(ctx, events.TopicPackaged, ev)
	}

	sourceEnriched := src.Codec != nil || src.Resolution != nil || src.BitrateKbps != nil ||
		(src.SizeBytes != nil && src.DurationMs != nil && *src.DurationMs > 0)
	writeJSON(w, http.StatusOK, map[string]any{
		"itemId":               itemID,
		"sourceEnriched":       sourceEnriched,
		"packagedAssetWritten": packagedWritten,
		"subtitlesWritten":     subsWritten,
		"audioTracks":          len(audioRends),
	})
}

// deletePackageSubtitles removes the item's subtitles that are its package's,
// and leaves the files beside its source (sidecarSubtitle).
func (h *Handlers) deletePackageSubtitles(ctx context.Context, itemID, root string) error {
	pool := h.d.Store.Pool()
	rows, err := pool.Query(ctx, `SELECT id, path FROM com_nalet_katalog_subtitleassets WHERE item_id = $1`, itemID)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id, path string
		if err := rows.Scan(&id, &path); err != nil {
			rows.Close()
			return err
		}
		if !h.sidecarSubtitle(path, root) {
			ids = append(ids, id)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil || len(ids) == 0 {
		return err
	}
	_, err = pool.Exec(ctx, `DELETE FROM com_nalet_katalog_subtitleassets WHERE id = ANY($1)`, ids)
	return err
}

// sourceFromManifest reads a manifest's source block: the v1 manifest's names
// (videoCodec, resolution, bitrateBps, size, durationMs) or the transcoder's
// contract's (codec, width, height, bitRate), as a packager may forward it.
func sourceFromManifest(source map[string]any) sourceprobe.Probe {
	var p sourceprobe.Probe
	for _, k := range []string{"videoCodec", "codec"} {
		if c := asString(source[k]); c != nil && trimBlank(*c) != "" {
			v := trimBlank(*c)
			p.Codec = &v
			break
		}
	}
	if r := asString(source["resolution"]); r != nil && trimBlank(*r) != "" {
		v := trimBlank(*r)
		p.Resolution = &v
	} else if wd, ok := asInt(source["width"]); ok && wd > 0 {
		if ht, ok := asInt(source["height"]); ok && ht > 0 {
			v := strconv.Itoa(wd) + "x" + strconv.Itoa(ht)
			p.Resolution = &v
		}
	}
	for _, k := range []string{"bitrateBps", "bitRate", "bit_rate"} {
		if bps, ok := asLong(source[k]); ok && bps > 0 {
			v := bps / 1000
			p.BitrateKbps = &v
			break
		}
	}
	if n, ok := asLong(source["size"]); ok && n > 0 {
		p.SizeBytes = &n
	}
	if d, ok := asLong(source["durationMs"]); ok && d > 0 {
		p.DurationMs = &d
	}
	return p
}

// packageRootFor reconstructs the sharded package directory for an item,
// mirroring ItemActionsController#packageRootFor (category by type, 2-char shard).
func (h *Handlers) packageRootFor(ctx context.Context, itemID string) (string, error) {
	var typ *string
	err := h.d.Store.Pool().QueryRow(ctx,
		`SELECT type FROM com_nalet_katalog_items WHERE id = $1`, itemID).Scan(&typ)
	if err != nil {
		return "", err
	}
	t := ""
	if typ != nil {
		t = *typ
	}
	var category string
	switch t {
	case "movie":
		category = "movies"
	case "episode":
		category = "shows"
	case "track":
		category = "music"
	default:
		category = "items"
	}
	shard := "00"
	if len(itemID) >= 2 {
		shard = itemID[:2]
	}
	return filepath.Join(h.d.Cfg.PackagesRoot, category, shard, itemID), nil
}

var bandwidthRe = regexp.MustCompile(`BANDWIDTH=(\d+)`)

// readPackagedBitrateKbps pulls the highest BANDWIDTH= from the HLS master
// playlist (/1000). nil when the playlist is absent or has no BANDWIDTH.
func readPackagedBitrateKbps(packageRoot string) *int {
	master := filepath.Join(packageRoot, "hls", "master.m3u8")
	content, err := os.ReadFile(master)
	if err != nil {
		return nil
	}
	var max int64
	for _, m := range bandwidthRe.FindAllSubmatch(content, -1) {
		v, perr := strconv.ParseInt(string(m[1]), 10, 64)
		if perr == nil && v > max {
			max = v
		}
	}
	if max <= 0 {
		return nil
	}
	k := int(max / 1000)
	return &k
}

// sumPackageBytes walks the package root summing regular file sizes. nil when
// the root is not a directory. A package being staged (.next) and one replaced
// moments ago and kept for a grace period (*.old-<stamp>) are not the package:
// the packager builds the new one beside the live one and swaps it in.
func sumPackageBytes(packageRoot string) *int64 {
	info, err := os.Stat(packageRoot)
	if err != nil || !info.IsDir() {
		return nil
	}
	var total int64
	_ = filepath.WalkDir(packageRoot, func(_ string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if d.IsDir() && notThePackage(d.Name()) {
			return filepath.SkipDir
		}
		if d.Type().IsRegular() && !notThePackage(d.Name()) {
			if fi, ferr := d.Info(); ferr == nil {
				total += fi.Size()
			}
		}
		return nil
	})
	return &total
}

// notThePackage names what the packager keeps beside a live package: the one
// it stages (.next) and the ones it replaced, until their grace ends (X.old-<stamp>).
func notThePackage(name string) bool {
	return name == ".next" || strings.Contains(name, ".old-")
}

// asMap mirrors Java asMap: a JSON object passes through, anything else -> {}.
func asMap(o any) map[string]any {
	if m, ok := o.(map[string]any); ok {
		return m
	}
	return map[string]any{}
}

// asListOfMap mirrors Java asListOfMap: a JSON array of objects; non-object
// entries are skipped (Java would CCE, but a skip is the safe equivalent here).
func asListOfMap(o any) []map[string]any {
	list, ok := o.([]any)
	if !ok {
		return nil
	}
	out := make([]map[string]any, 0, len(list))
	for _, e := range list {
		if m, ok := e.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}
