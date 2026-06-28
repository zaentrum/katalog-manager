// Package itemactions ports the CAP ItemActionsController operator actions:
// packageItem (enqueue packaging) and validateItem (on-disk validation).
//
// Source of truth (READ-ONLY reference): SAP CAP (Java/Spring)
// com.nalet.katalog.web.ItemActionsController. Behaviour, codes, message
// strings and SQL semantics are reproduced verbatim. These two actions map to
// GraphQL mutations packageItem / validateItem (see SPEC §3, §7); the
// packaging-complete ingest sink stays REST and lives elsewhere.
package itemactions

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/zaentrum/katalog-manager/internal/config"
	"github.com/zaentrum/katalog-manager/internal/graph"
	"github.com/zaentrum/katalog-manager/internal/processing"
	"github.com/zaentrum/katalog-manager/internal/store"
)

// ErrUnknownItem is returned when no items row exists for the given id. Callers
// (the GraphQL/REST layer) map this to HTTP 404; the wrapped message reproduces
// the Java body string "unknown item: <id>".
var ErrUnknownItem = errors.New("unknown item")

// ErrNotPackageable is returned when an item is neither movie, episode nor
// series. Callers map this to HTTP 400; the wrapped message reproduces the Java
// body string "Packaging is only available for movies and episodes.".
var ErrNotPackageable = errors.New("not packageable")

// Service implements graph.Packager (PackageItem) and graph.Validator
// (ValidateItem).
type Service struct {
	st    *store.Store
	cfg   config.Config
	steps *processing.Steps
}

// New constructs the item-actions service.
func New(st *store.Store, cfg config.Config, steps *processing.Steps) *Service {
	return &Service{st: st, cfg: cfg, steps: steps}
}

func ptrStr(s string) *string { return &s }
func ptrBool(b bool) *bool    { return &b }
func ptrI32(n int32) *int32   { return &n }

// ---------------------------------------------------------------- package

// PackageItem enqueues packaging for a single item (movie or episode) or fans
// out over every episode under a series. Idempotent: a re-click while the chain
// is already done / pending / in_progress is a no-op with an explanatory
// result. Mirrors ItemActionsController.enqueuePackaging.
//
//   - unknown id            -> ErrUnknownItem (caller -> 404)
//   - non-media type        -> ErrNotPackageable (caller -> 400)
//   - series                -> {EpisodesEnqueued, EpisodesTotal, Message}
//   - movie/episode active  -> {Status:"<step> <status>", AlreadyActive:true, Message}
//   - movie/episode fresh   -> {Status:"pending", AlreadyActive:false, Message}
func (s *Service) PackageItem(ctx context.Context, id string) (graph.PackageResult, error) {
	var typ, title string
	err := s.st.Pool().QueryRow(ctx,
		`SELECT type, title FROM com_nalet_katalog_items WHERE id = $1`, id).
		Scan(&typ, &title)
	if err != nil {
		if isNoRows(err) {
			return graph.PackageResult{}, fmt.Errorf("%w: %s", ErrUnknownItem, id)
		}
		return graph.PackageResult{}, err
	}

	lower := strings.ToLower(typ)

	if lower == "series" {
		// Fan out to every episode that isn't already finished. We start the
		// chain at transcode=pending; the transcoder (skip / NVENC) and
		// packager (shaka) then run in sequence. An episode is "active" if
		// either transcode or package is in a non-terminal-bad state, which
		// keeps a re-click idempotent.
		rows, err := s.st.Pool().Query(ctx, `
			SELECT e.id FROM com_nalet_katalog_items e
			WHERE e.parent_id = $1 AND e.type = 'episode'
			  AND EXISTS (SELECT 1 FROM com_nalet_katalog_playbackassets p
			              WHERE p.item_id = e.id AND p.isprimary = true)
			  AND NOT EXISTS (SELECT 1 FROM com_nalet_katalog_itemprocessingsteps ps
			                  WHERE ps.item_id = e.id
			                    AND ps.step IN ('transcode','package')
			                    AND ps.status IN ('done','in_progress','pending'))`, id)
		if err != nil {
			return graph.PackageResult{}, err
		}
		var epIDs []string
		for rows.Next() {
			var epID string
			if err := rows.Scan(&epID); err != nil {
				rows.Close()
				return graph.PackageResult{}, err
			}
			epIDs = append(epIDs, epID)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return graph.PackageResult{}, err
		}

		enqueued := int32(0)
		for _, epID := range epIDs {
			if err := s.steps.Upsert(ctx, epID, "transcode", processing.StatusPending,
				nil, ptrStr("enqueued from series action")); err != nil {
				return graph.PackageResult{}, err
			}
			enqueued++
		}

		total, err := s.episodeCount(ctx, id)
		if err != nil {
			return graph.PackageResult{}, err
		}

		var msg string
		if enqueued == 0 {
			msg = "Nothing to enqueue — every eligible episode is already done, pending, or in progress."
		} else {
			msg = fmt.Sprintf("Enqueued %d episodes for packaging.", enqueued)
		}
		return graph.PackageResult{
			EpisodesEnqueued: ptrI32(enqueued),
			EpisodesTotal:    ptrI32(total),
			Message:          ptrStr(msg),
		}, nil
	}

	if lower != "movie" && lower != "episode" {
		// Albums and tracks have no packaging path today.
		return graph.PackageResult{
			Message: ptrStr("Packaging is only available for movies and episodes."),
		}, fmt.Errorf("%w: type=%s", ErrNotPackageable, typ)
	}

	// Movie / episode — single-item enqueue. Idempotent: refuse if either
	// chain step is already done / pending / in_progress. A failed step is
	// fine to retry.
	active, err := s.activeChainStep(ctx, id)
	if err != nil {
		return graph.PackageResult{}, err
	}
	if active != "" {
		return graph.PackageResult{
			Status:        ptrStr(active),
			AlreadyActive: ptrBool(true),
			Message:       ptrStr("Already " + active + " — no change."),
		}, nil
	}

	if err := s.steps.Upsert(ctx, id, "transcode", processing.StatusPending,
		nil, ptrStr("enqueued from object-page action")); err != nil {
		return graph.PackageResult{}, err
	}
	return graph.PackageResult{
		Status:        ptrStr("pending"),
		AlreadyActive: ptrBool(false),
		Message: ptrStr("Queued for transcoding. " +
			"Once the transcoder finishes (or skips, when the source is " +
			"already HEVC) the packager picks it up automatically."),
	}, nil
}

// activeChainStep returns "<step> <status>" when either chain step
// (transcode|package) is live (done|in_progress|pending) for the item, ordered
// by step so transcode wins ties; empty string otherwise.
func (s *Service) activeChainStep(ctx context.Context, itemID string) (string, error) {
	var step, status string
	err := s.st.Pool().QueryRow(ctx, `
		SELECT step, status FROM com_nalet_katalog_itemprocessingsteps
		WHERE item_id = $1 AND step IN ('transcode','package')
		  AND status IN ('done','in_progress','pending')
		ORDER BY step
		LIMIT 1`, itemID).Scan(&step, &status)
	if err != nil {
		if isNoRows(err) {
			return "", nil
		}
		return "", err
	}
	return step + " " + status, nil
}

func (s *Service) episodeCount(ctx context.Context, seriesID string) (int32, error) {
	var n int32
	err := s.st.Pool().QueryRow(ctx, `
		SELECT COUNT(*) FROM com_nalet_katalog_items
		WHERE parent_id = $1 AND type = 'episode'`, seriesID).Scan(&n)
	if err != nil {
		return 0, err
	}
	return n, nil
}

// ---------------------------------------------------------------- validate

// ValidateItem inspects the on-disk state of an item (or rolls up every episode
// of a series). Read-only: filesystem stats + a couple of SQL lookups, no
// writes. Mirrors ItemActionsController.validateItem / validateOne.
//
//   - unknown id -> ErrUnknownItem (caller -> 404)
//   - series     -> Code="series", Message is the per-episode rollup summary,
//     and Findings carries one entry per episode-bucket count.
//   - single     -> Code in {ok|no_package|source_missing|stale|codec_mismatch|
//     findings|not_applicable}, with SourcePath/PackagePath/Findings as set.
func (s *Service) ValidateItem(ctx context.Context, id string) (graph.ValidateResult, error) {
	var typ, title string
	err := s.st.Pool().QueryRow(ctx,
		`SELECT type, title FROM com_nalet_katalog_items WHERE id = $1`, id).
		Scan(&typ, &title)
	if err != nil {
		if isNoRows(err) {
			return graph.ValidateResult{}, fmt.Errorf("%w: %s", ErrUnknownItem, id)
		}
		return graph.ValidateResult{}, err
	}

	if strings.EqualFold(typ, "series") {
		return s.validateSeries(ctx, id)
	}

	res, err := s.validateOne(ctx, id)
	if err != nil {
		return graph.ValidateResult{}, err
	}
	return res, nil
}

// validateSeries performs the per-episode roll-up. The graph ValidateResult has
// no numeric counter fields, so the counts are reproduced in the message string
// (verbatim Java format) and surfaced per-bucket via Findings[].
func (s *Service) validateSeries(ctx context.Context, seriesID string) (graph.ValidateResult, error) {
	rows, err := s.st.Pool().Query(ctx,
		`SELECT id FROM com_nalet_katalog_items WHERE parent_id = $1 AND type = 'episode'`, seriesID)
	if err != nil {
		return graph.ValidateResult{}, err
	}
	var epIDs []string
	for rows.Next() {
		var epID string
		if err := rows.Scan(&epID); err != nil {
			rows.Close()
			return graph.ValidateResult{}, err
		}
		epIDs = append(epIDs, epID)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return graph.ValidateResult{}, err
	}

	var ok, sourceMissing, noPackage, stale, codecMismatch, findingsCount int
	for _, epID := range epIDs {
		r, err := s.validateOne(ctx, epID)
		if err != nil {
			return graph.ValidateResult{}, err
		}
		switch r.Code {
		case "ok":
			ok++
		case "source_missing":
			sourceMissing++
		case "no_package":
			noPackage++
		case "stale":
			stale++
		case "codec_mismatch":
			codecMismatch++
		case "findings":
			findingsCount++
		default:
			// not_applicable etc. — not counted, mirrors Java.
		}
	}

	episodes := len(epIDs)
	message := fmt.Sprintf(
		"%d ok, %d not packaged, %d stale, %d source missing, %d codec mismatch, %d with findings (of %d episodes)",
		ok, noPackage, stale, sourceMissing, codecMismatch, findingsCount, episodes)

	findings := []graph.ValidateFinding{
		{Code: "episodes", Message: strconv.Itoa(episodes)},
		{Code: "ok", Message: strconv.Itoa(ok)},
		{Code: "no_package", Message: strconv.Itoa(noPackage)},
		{Code: "stale", Message: strconv.Itoa(stale)},
		{Code: "source_missing", Message: strconv.Itoa(sourceMissing)},
		{Code: "codec_mismatch", Message: strconv.Itoa(codecMismatch)},
		{Code: "with_findings", Message: strconv.Itoa(findingsCount)},
	}

	return graph.ValidateResult{
		Code:     "series",
		Message:  message,
		Findings: findings,
	}, nil
}

// validateOne validates a single item. Codes (exact strings; the UI keys off
// them):
//
//	not_applicable — no primary playback asset for this item
//	source_missing — primary asset row exists but the file is gone
//	no_package     — source exists, no .complete on disk
//	stale          — .complete exists but source mtime is newer
//	codec_mismatch — packaged codec is non-blank and not hev1./hvc1.
//	findings       — hygiene issues (stray_small_file / duplicate_assets)
//	ok             — source + package present, mtimes consistent, codec HEVC
func (s *Service) validateOne(ctx context.Context, itemID string) (graph.ValidateResult, error) {
	// Primary playback asset (source path).
	var sourcePath string
	err := s.st.Pool().QueryRow(ctx, `
		SELECT path FROM com_nalet_katalog_playbackassets
		WHERE item_id = $1 AND isprimary = true LIMIT 1`, itemID).Scan(&sourcePath)
	if err != nil {
		if isNoRows(err) {
			return graph.ValidateResult{
				Code:    "not_applicable",
				Message: "No primary playback asset for this item.",
			}, nil
		}
		return graph.ValidateResult{}, err
	}

	srcInfo, srcStatErr := os.Stat(sourcePath)
	srcExists := srcStatErr == nil

	// Sharded layout: <PackagesRoot>/{category}/{shard}/{itemId}/
	pkgRoot, err := s.packageRootFor(ctx, itemID)
	if err != nil {
		return graph.ValidateResult{}, err
	}
	completePath := filepath.Join(pkgRoot, ".complete")
	completeInfo, completeStatErr := os.Stat(completePath)
	pkgExists := completeStatErr == nil
	var pkgPathPtr *string
	if pkgExists {
		pkgPathPtr = ptrStr(pkgRoot)
	}

	if !srcExists {
		return graph.ValidateResult{
			Code:        "source_missing",
			Message:     "Source file not found at " + sourcePath,
			SourcePath:  ptrStr(sourcePath),
			PackagePath: pkgPathPtr,
		}, nil
	}
	if !pkgExists {
		return graph.ValidateResult{
			Code:       "no_package",
			Message:    "Source ok, but no packaged output on disk.",
			SourcePath: ptrStr(sourcePath),
		}, nil
	}
	// Stale check: source mtime vs .complete mtime. An mtime read failure is
	// suspicious but not fatal — treat as ok (here both stats already
	// succeeded, so this just compares them).
	if srcInfo.ModTime().UnixMilli() > completeInfo.ModTime().UnixMilli() {
		return graph.ValidateResult{
			Code:        "stale",
			Message:     "Source modified after packaging (re-package recommended).",
			SourcePath:  ptrStr(sourcePath),
			PackagePath: pkgPathPtr,
		}, nil
	}

	// Codec invariant: every packaged output must be HEVC (hev1.*/hvc1.*).
	// avc1/h264 on the packaged row means the transcoder skipped when it
	// shouldn't have, or the row is from a pre-split historical run.
	var pkgCodec *string
	err = s.st.Pool().QueryRow(ctx, `
		SELECT codec FROM com_nalet_katalog_playbackassets
		WHERE item_id = $1 AND kind = 'packaged' LIMIT 1`, itemID).Scan(&pkgCodec)
	if err != nil && !isNoRows(err) {
		return graph.ValidateResult{}, err
	}
	if err == nil && pkgCodec != nil {
		c := *pkgCodec
		if strings.TrimSpace(c) != "" &&
			!strings.HasPrefix(c, "hev1.") &&
			!strings.HasPrefix(c, "hvc1.") {
			return graph.ValidateResult{
				Code: "codec_mismatch",
				Message: "Packaged codec '" + c + "' is not HEVC; re-Migrate to " +
					"produce hev1.* via the transcoder.",
				SourcePath:  ptrStr(sourcePath),
				PackagePath: pkgPathPtr,
			}, nil
		}
	}

	// Files-facet hygiene: stray_small_file + duplicate_assets. Take
	// precedence over `ok` but not over the harder failures above.
	findings, err := s.hygieneFindings(ctx, itemID)
	if err != nil {
		return graph.ValidateResult{}, err
	}
	if len(findings) > 0 {
		return graph.ValidateResult{
			Code: "findings",
			Message: fmt.Sprintf("%d hygiene finding(s); see findings[] for detail.",
				len(findings)),
			SourcePath:  ptrStr(sourcePath),
			PackagePath: pkgPathPtr,
			Findings:    findings,
		}, nil
	}

	return graph.ValidateResult{
		Code:        "ok",
		Message:     "Source + package present, mtimes consistent, codec is HEVC.",
		SourcePath:  ptrStr(sourcePath),
		PackagePath: pkgPathPtr,
	}, nil
}

// hygieneFindings reproduces the stray_small_file + duplicate_assets scan over
// all of an item's playback assets. The graph ValidateFinding only carries
// {code,message}, so the structured Java fields are folded into the message.
func (s *Service) hygieneFindings(ctx context.Context, itemID string) ([]graph.ValidateFinding, error) {
	rows, err := s.st.Pool().Query(ctx, `
		SELECT id, kind, path, COALESCE(sizebytes, 0) AS sizebytes
		FROM com_nalet_katalog_playbackassets WHERE item_id = $1`, itemID)
	if err != nil {
		return nil, err
	}
	type asset struct {
		id, kind, path string
		bytes          int64
	}
	var assets []asset
	for rows.Next() {
		var a asset
		if err := rows.Scan(&a.id, &a.kind, &a.path, &a.bytes); err != nil {
			rows.Close()
			return nil, err
		}
		assets = append(assets, a)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	smallThresholdMb := s.smallFileThresholdMb(ctx)
	smallThresholdBytes := int64(smallThresholdMb) * 1024 * 1024

	// Iteration order preserved (insertion order) to match Java's
	// LinkedHashMap semantics for both findings lists.
	var findings []graph.ValidateFinding
	kindCounts := map[string]int{}
	var kindOrder []string
	for _, a := range assets {
		if _, seen := kindCounts[a.kind]; !seen {
			kindOrder = append(kindOrder, a.kind)
		}
		kindCounts[a.kind]++
		if a.kind != "primary" && a.bytes > 0 && a.bytes < smallThresholdBytes {
			findings = append(findings, graph.ValidateFinding{
				Code: "stray_small_file",
				Message: fmt.Sprintf(
					"asset %s kind=%s path=%s sizeBytes=%d thresholdMb=%d",
					a.id, a.kind, a.path, a.bytes, smallThresholdMb),
			})
		}
	}
	for _, kind := range kindOrder {
		if kind != "primary" && kindCounts[kind] > 1 {
			findings = append(findings, graph.ValidateFinding{
				Code:    "duplicate_assets",
				Message: fmt.Sprintf("kind=%s count=%d", kind, kindCounts[kind]),
			})
		}
	}
	return findings, nil
}

// smallFileThresholdMb mirrors SettingsController.getInt(
// "validate.small_file_threshold_mb", 5): bad/missing values quietly fall back
// to 5.
func (s *Service) smallFileThresholdMb(ctx context.Context) int {
	const fallback = 5
	row, err := s.st.GetSettingByKey(ctx, "validate.small_file_threshold_mb")
	if err != nil || row == nil {
		return fallback
	}
	n, perr := strconv.Atoi(strings.TrimSpace(row.ValueText))
	if perr != nil {
		return fallback
	}
	return n
}

// packageRootFor reconstructs the sharded package root for an item id; mirrors
// packager.py's _item_root. category = movie->movies | episode->shows |
// track->music | else->items; shard = first 2 chars of id (or "00").
func (s *Service) packageRootFor(ctx context.Context, itemID string) (string, error) {
	var typ *string
	err := s.st.Pool().QueryRow(ctx,
		`SELECT type FROM com_nalet_katalog_items WHERE id = $1`, itemID).Scan(&typ)
	if err != nil && !isNoRows(err) {
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
	return filepath.Join(s.cfg.PackagesRoot, category, shard, itemID), nil
}

// isNoRows reports whether err is the pgx empty-result sentinel.
func isNoRows(err error) bool {
	return errors.Is(err, pgx.ErrNoRows)
}
