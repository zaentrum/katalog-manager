package rest

import (
	"context"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"

	"github.com/go-chi/chi/v5"
)

// packagingComplete ports ItemActionsController#ingestPackagingManifest: the
// packager machine sink. Idempotent; three writes (source-asset enrich, packaged
// asset replace, subtitle replace). Returns
// {itemId, sourceEnriched, packagedAssetWritten, subtitlesWritten, audioTracks}.
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

	// 1. Source PlaybackAsset enrichment (COALESCE only the ffprobe-sourced cols).
	srcCodec := asString(source["videoCodec"])
	srcResolution := asString(source["resolution"])
	srcBitrateBps, srcBitrateOK := asLong(source["bitrateBps"])
	srcSize, srcSizeOK := asLong(source["size"])
	srcDurMs, srcDurOK := asLong(source["durationMs"])

	var srcBitrateKbps *int
	switch {
	case srcBitrateOK && srcBitrateBps > 0:
		v := int(srcBitrateBps / 1000)
		srcBitrateKbps = &v
	case srcSizeOK && srcDurOK && srcDurMs > 0:
		v := int((srcSize * 8) / srcDurMs)
		srcBitrateKbps = &v
	}
	var srcSizePtr *int64
	if srcSizeOK {
		srcSizePtr = &srcSize
	}

	if _, err := pool.Exec(ctx, `
		UPDATE com_nalet_katalog_playbackassets
		SET codec       = COALESCE($1, codec),
		    resolution  = COALESCE($2, resolution),
		    bitratekbps = COALESCE($3, bitratekbps),
		    sizebytes   = COALESCE($4, sizebytes)
		WHERE item_id = $5 AND isprimary = true AND kind = 'primary'`,
		srcCodec, srcResolution, srcBitrateKbps, srcSizePtr, itemID); err != nil {
		http.Error(w, "source enrich failed", http.StatusInternalServerError)
		return
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
		for _, a := range audioRends {
			if def, ok := a["default"].(bool); ok && def {
				primaryAudio = a
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

	// 3. SubtitleAssets — replace the whole set for this item.
	if _, err := pool.Exec(ctx,
		`DELETE FROM com_nalet_katalog_subtitleassets WHERE item_id = $1`, itemID); err != nil {
		http.Error(w, "subtitle delete failed", http.StatusInternalServerError)
		return
	}
	subsWritten := 0
	for _, s := range subtitles {
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
			itemID, fullPath, asString(s["format"]), asString(s["language"]),
			asString(s["title"]), isDefault); err != nil {
			http.Error(w, "subtitle insert failed", http.StatusInternalServerError)
			return
		}
		subsWritten++
	}

	sourceEnriched := srcCodec != nil || srcResolution != nil || srcBitrateKbps != nil
	writeJSON(w, http.StatusOK, map[string]any{
		"itemId":               itemID,
		"sourceEnriched":       sourceEnriched,
		"packagedAssetWritten": packagedWritten,
		"subtitlesWritten":     subsWritten,
		"audioTracks":          len(audioRends),
	})
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
// the root is not a directory.
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
		if d.Type().IsRegular() {
			if fi, ferr := d.Info(); ferr == nil {
				total += fi.Size()
			}
		}
		return nil
	})
	return &total
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
