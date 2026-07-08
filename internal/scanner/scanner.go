// Package scanner ports the CAP NfsScanner + ScanController scan lifecycle
// (SPEC §2 / 30-integrations). It walks the NFS media root, classifies files by
// extension + path, and upserts com_nalet_katalog_items + a primary
// playbackasset (plus subtitle sidecars and kind='trailer' extras), keyed on the
// absolute playbackassets.path. Re-scans are idempotent: an existing item only
// gets its modifiedat heartbeat bumped — title/sort/year are owned by TMDB
// enrichment and must never be clobbered.
//
// The scan itself runs asynchronously: Trigger inserts a 'running' scanjobs row,
// kicks off the walk in a goroutine, and returns the job id immediately. The
// goroutine stamps the job 'done' or 'failed' via FinishScanJob.
package scanner

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/zaentrum/katalog-manager/internal/config"
	"github.com/zaentrum/katalog-manager/internal/events"
	"github.com/zaentrum/katalog-manager/internal/processing"
	"github.com/zaentrum/katalog-manager/internal/store"
)

// Scanner is the NFS filesystem walker / upserter.
type Scanner struct {
	st    *store.Store
	cfg   config.Config
	steps *processing.Steps
	prod  *events.Producer // nil-safe: emits stube.catalog.item.discovered on new items
}

// New constructs a Scanner. Matches graph.ScanRunner structurally via Trigger.
// prod may be nil (events disabled) — the producer is nil-safe.
func New(st *store.Store, cfg config.Config, steps *processing.Steps, prod *events.Producer) *Scanner {
	return &Scanner{st: st, cfg: cfg, steps: steps, prod: prod}
}

// Trigger validates the source, inserts a 'running' scan job, launches the walk
// asynchronously, and returns the new job id. Only source 'nfs' is accepted
// (mirrors ScanController POST /api/scan returning 400 for anything else).
func (s *Scanner) Trigger(ctx context.Context, source string) (string, error) {
	if source != "nfs" {
		return "", errors.New("unsupported scan source: " + source)
	}
	job, err := s.st.InsertScanJob(ctx, "nfs", "running")
	if err != nil {
		return "", err
	}
	// Detach from the request context so the walk is not cancelled when the
	// triggering request returns; the goroutine owns the job's terminal state.
	go s.runScan(context.Background(), job.ID)
	return job.ID, nil
}

// scanResult accumulates the walk counters (mirrors NfsScanner.Result).
type scanResult struct {
	filesSeen     int32
	itemsInserted int32
	itemsUpdated  int32
}

// runScan executes the walk and finalises the scan job. It never panics out: a
// missing root finishes the job cleanly (status done, zero counters — matching
// the Java "warn + empty result, no error"); a walk error finishes it failed.
func (s *Scanner) runScan(ctx context.Context, jobID string) {
	res, err := s.walk(ctx)
	if err != nil {
		msg := err.Error()
		// Java's failure branch updates only status/finishedat/errormessage, leaving
		// the counters at their INSERT-time zeros — match that (don't persist partials).
		_ = s.st.FinishScanJob(ctx, jobID, store.ScanJobResult{
			Status:       "failed",
			ErrorMessage: &msg,
		})
		return
	}
	_ = s.st.FinishScanJob(ctx, jobID, store.ScanJobResult{
		Status:        "done",
		FilesSeen:     res.filesSeen,
		ItemsInserted: res.itemsInserted,
		ItemsUpdated:  res.itemsUpdated,
	})
}

// walk performs the filesystem traversal. If the root does not exist it returns
// an empty result and nil error (graceful no-op, like NfsScanner.scan).
func (s *Scanner) walk(ctx context.Context) (scanResult, error) {
	var res scanResult
	root := s.cfg.NFSRoot

	info, statErr := os.Stat(root)
	if statErr != nil || !info.IsDir() {
		// Root missing/unreadable: warn-equivalent no-op, no error.
		return res, nil
	}

	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// Per-entry errors (e.g. unreadable dir) are skipped, not fatal —
			// mirrors the per-file try/catch in NfsScanner.visitFile.
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		// Per-file processing is best-effort; a failure on one file must not
		// abort the whole walk.
		s.processFile(ctx, root, path, d, &res)
		return nil
	})
	return res, walkErr
}

// processFile classifies one regular file and upserts the catalog rows for it.
// Errors are swallowed (logged-equivalent) so the walk continues.
func (s *Scanner) processFile(ctx context.Context, root, path string, d fs.DirEntry, res *scanResult) {
	name := d.Name()
	// Skip hidden / transcoder-scratch dotfiles + extensionless files.
	if strings.HasPrefix(name, ".") {
		return
	}
	dot := strings.LastIndex(name, ".")
	if dot < 0 {
		return
	}
	ext := strings.ToLower(name[dot:])
	isVideo := videoExts[ext]
	isAudio := audioExts[ext]
	if !isVideo && !isAudio {
		return
	}

	res.filesSeen++

	absPath, err := filepath.Abs(path)
	if err != nil {
		absPath = path
	}
	rel, err := filepath.Rel(root, path)
	if err != nil {
		rel = path
	}
	rel = filepath.ToSlash(rel)

	pool := s.st.Pool()

	// Trailer / extras: attach to parent movie, do not create a new item.
	if isVideo && isTrailerPath(absPath, name) {
		s.attachTrailer(ctx, pool, path, absPath, res)
		return
	}

	typ := classify(rel, isVideo, isAudio)
	title := extractTitle(name, typ)
	year := extractYear(name)
	var seasonNumber, episodeNumber *int32
	if typ == "episode" {
		seasonNumber, episodeNumber = episodeCoords(name)
	}

	size := fileSize(d)

	var existingItemID *string
	if err := pool.QueryRow(ctx,
		`SELECT item_id FROM com_nalet_katalog_playbackassets WHERE path = $1 LIMIT 1`,
		absPath).Scan(&existingItemID); err != nil && err != pgx.ErrNoRows {
		return
	}

	var itemID string
	if existingItemID == nil {
		// New item: INSERT items + primary asset + seed the 'scan' step.
		if err := pool.QueryRow(ctx,
			`INSERT INTO com_nalet_katalog_items
			   (id, type, title, sorttitle, year, seasonnumber, episodenumber, createdat, modifiedat)
			 VALUES (gen_random_uuid()::varchar, $1, $2, $3, $4, $5, $6, now(), now())
			 RETURNING id`,
			typ, title, strings.ToLower(title), year, seasonNumber, episodeNumber).Scan(&itemID); err != nil {
			return
		}
		res.itemsInserted++

		if _, err := pool.Exec(ctx,
			`INSERT INTO com_nalet_katalog_playbackassets
			   (id, item_id, path, sizebytes, isprimary)
			 VALUES (gen_random_uuid()::varchar, $1, $2, $3, true)`,
			itemID, absPath, size); err != nil {
			return
		}

		// Seed processing steps for the freshly-ingested item (the scan step is
		// done the moment the scanner records the file). Best-effort: a step
		// failure must not abort ingestion.
		_ = s.steps.Upsert(ctx, itemID, "scan", processing.StatusDone, nil, nil)

		// Event-driven trigger: a brand-new item enters the pipeline. Emit
		// discovered -> the enricher consumes it (replaces the old poll ticker).
		// Only on INSERT — a re-scan of an existing file must not re-fire.
		ev := events.NewItemEvent(itemID)
		ev.Type = typ
		ev.Step = "tmdb"
		ev.Source = "scan"
		s.prod.EmitItem(ctx, events.TopicDiscovered, ev)
	} else {
		// Existing item: bump modifiedat ONLY (never clobber TMDB-owned fields),
		// and refresh the asset's size + primary flag.
		itemID = *existingItemID
		if _, err := pool.Exec(ctx,
			`UPDATE com_nalet_katalog_items SET modifiedat = now() WHERE id = $1`, itemID); err != nil {
			return
		}
		res.itemsUpdated++

		if _, err := pool.Exec(ctx,
			`UPDATE com_nalet_katalog_playbackassets SET sizebytes = $1, isprimary = true WHERE path = $2`,
			size, absPath); err != nil {
			return
		}
	}

	if isVideo {
		s.scanSidecars(ctx, pool, path, itemID)
	}
}

// scanSidecars finds subtitle files in the video's directory sharing the video
// basename (optional language suffix) and upserts subtitleassets rows. Ports
// NfsScanner.scanSidecars. Missing-table errors are swallowed.
func (s *Scanner) scanSidecars(ctx context.Context, pool *pgxpool.Pool, videoPath, itemID string) {
	videoBase := stripExt(filepath.Base(videoPath))
	dir := filepath.Dir(videoPath)
	if dir == "" {
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	defaultPicked := false
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		dot := strings.LastIndex(name, ".")
		if dot < 0 {
			continue
		}
		ext := strings.ToLower(name[dot:])
		if !subExts[ext] {
			continue
		}
		base := stripExt(name)

		var lang, label string
		baseNoLang := base
		if m := langSuffix.FindStringSubmatchIndex(base); m != nil {
			// m[0]==start of the matched ".lang" suffix; compare the prefix to
			// the video basename, case-insensitively.
			prefix := base[:m[0]]
			if strings.EqualFold(prefix, videoBase) {
				baseNoLang = prefix
				lang = strings.ToLower(base[m[2]:m[3]])
				label = languageLabel(lang)
			}
		}
		if !strings.EqualFold(baseNoLang, videoBase) {
			continue
		}
		if label == "" {
			label = "Subtitles"
		}

		absPath, err := filepath.Abs(filepath.Join(dir, name))
		if err != nil {
			absPath = filepath.Join(dir, name)
		}
		format := ext[1:] // ext without leading dot

		var langArg, labelArg *string
		if lang != "" {
			langArg = &lang
		}
		labelArg = &label

		var exists *int
		err = pool.QueryRow(ctx,
			`SELECT 1 FROM com_nalet_katalog_subtitleassets WHERE path = $1 LIMIT 1`,
			absPath).Scan(&exists)
		if err != nil && err != pgx.ErrNoRows {
			// Table missing / access error — log-equivalent debug, keep walking.
			continue
		}
		if exists != nil {
			_, _ = pool.Exec(ctx,
				`UPDATE com_nalet_katalog_subtitleassets SET item_id = $1, format = $2, lang = $3, label = $4 WHERE path = $5`,
				itemID, format, langArg, labelArg, absPath)
		} else {
			if _, err := pool.Exec(ctx,
				`INSERT INTO com_nalet_katalog_subtitleassets
				   (id, item_id, path, format, lang, label, isdefault)
				 VALUES (gen_random_uuid()::varchar, $1, $2, $3, $4, $5, $6)`,
				itemID, absPath, format, langArg, labelArg, !defaultPicked); err == nil {
				defaultPicked = true
			}
		}
	}
}

// attachTrailer attaches a trailer video to the parent movie's item as a
// kind='trailer' playbackasset instead of creating a new item. Ports
// NfsScanner.attachTrailer. If the parent movie is not yet ingested the trailer
// is skipped (picked up on the next sweep).
func (s *Scanner) attachTrailer(ctx context.Context, pool *pgxpool.Pool, path, absPath string, res *scanResult) {
	movieDir := filepath.Dir(path)
	if movieDir != "" && strings.EqualFold(filepath.Base(movieDir), "trailers") {
		movieDir = filepath.Dir(movieDir)
	}
	if movieDir == "" {
		return
	}
	absMovieDir, err := filepath.Abs(movieDir)
	if err != nil {
		absMovieDir = movieDir
	}
	parentPrefix := absMovieDir + "/" + "%"

	var parentItemID *string
	if err := pool.QueryRow(ctx,
		`SELECT item_id FROM com_nalet_katalog_playbackassets
		 WHERE isprimary = true AND path LIKE $1 LIMIT 1`,
		parentPrefix).Scan(&parentItemID); err != nil && err != pgx.ErrNoRows {
		return
	}
	if parentItemID == nil {
		// Parent movie not yet ingested; catch up next scan.
		return
	}

	var size int64
	if info, err := os.Stat(path); err == nil {
		size = info.Size()
	}

	var existingItemID *string
	if err := pool.QueryRow(ctx,
		`SELECT item_id FROM com_nalet_katalog_playbackassets WHERE path = $1 LIMIT 1`,
		absPath).Scan(&existingItemID); err != nil && err != pgx.ErrNoRows {
		return
	}

	if existingItemID == nil {
		if _, err := pool.Exec(ctx,
			`INSERT INTO com_nalet_katalog_playbackassets
			   (id, item_id, path, sizebytes, isprimary, kind)
			 VALUES (gen_random_uuid()::varchar, $1, $2, $3, false, 'trailer')`,
			*parentItemID, absPath, size); err != nil {
			return
		}
		res.itemsInserted++
	} else {
		if _, err := pool.Exec(ctx,
			`UPDATE com_nalet_katalog_playbackassets
			   SET item_id = $1, sizebytes = $2, kind = 'trailer', isprimary = false
			 WHERE path = $3`,
			*parentItemID, size, absPath); err != nil {
			return
		}
		res.itemsUpdated++
		// If the trailer previously had its own orphan item with no remaining
		// assets, delete that orphan.
		if *parentItemID != *existingItemID {
			var remaining int
			if err := pool.QueryRow(ctx,
				`SELECT COUNT(*) FROM com_nalet_katalog_playbackassets WHERE item_id = $1`,
				*existingItemID).Scan(&remaining); err == nil && remaining == 0 {
				_, _ = pool.Exec(ctx,
					`DELETE FROM com_nalet_katalog_items WHERE id = $1`, *existingItemID)
			}
		}
	}

	// Bump the parent movie's modifiedat heartbeat.
	_, _ = pool.Exec(ctx,
		`UPDATE com_nalet_katalog_items SET modifiedat = now() WHERE id = $1`, *parentItemID)
}

// fileSize returns the file's byte size from the DirEntry, falling back to 0 if
// the underlying FileInfo is unavailable.
func fileSize(d fs.DirEntry) int64 {
	info, err := d.Info()
	if err != nil {
		return 0
	}
	return info.Size()
}
