// Package scanner ports the CAP NfsScanner + ScanController scan lifecycle
// (SPEC §2 / 30-integrations). It walks the NFS media root, classifies files by
// extension + path, and upserts com_nalet_katalog_items + a primary
// playbackasset (plus subtitle sidecars), keyed on the absolute
// playbackassets.path. Re-scans are idempotent: an existing item only gets its
// modifiedat heartbeat bumped — title/sort/year are owned by TMDB enrichment
// and must never be clobbered. A title's trailers and other bonus material are
// no items: behind the setting extras.scan the scanner takes them in as its
// extras (extras.go), and without it skips them.
//
// The scan itself runs asynchronously: Trigger inserts a 'running' scanjobs row,
// kicks off the walk in a goroutine, and returns the job id immediately. The
// goroutine stamps the job 'done' or 'failed' via FinishScanJob.
//
// The goroutine is this process's: a process that stops while it scans never
// stamps the job. So a job names the process that runs it (Runner), and the
// walk gives it a word every so often (its heartbeat). At startup the service
// fails, interrupted, the jobs a previous process of its host left running
// (FailInterrupted), and the reaper (retry.Service.ReapScans) fails a job
// silent for longer than the scan's timeout, timed out.
package scanner

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/zaentrum/katalog-manager/internal/config"
	"github.com/zaentrum/katalog-manager/internal/events"
	"github.com/zaentrum/katalog-manager/internal/processing"
	"github.com/zaentrum/katalog-manager/internal/store"
)

// deletedByScanner is how the deletion log names the scanner when it removes an
// item on its own.
const deletedByScanner = "katalog-manager/scanner"

// InterruptedReason is what a scan job a previous process left running says
// once it is failed at startup.
const InterruptedReason = "interrupted: the service restarted while the scan ran"

// runner is this process, as the scan jobs it runs name it (Runner).
var runner = func() string {
	host, _ := os.Hostname()
	host = strings.ReplaceAll(strings.TrimSpace(host), "/", "-")
	if host == "" {
		host = "unknown"
	}
	tag := make([]byte, 4)
	_, _ = rand.Read(tag)
	return host + "/" + hex.EncodeToString(tag)
}()

// Runner is this process, as a scan job it runs names it: "<host>/<boot>", the
// host it runs on (a pod's name) and a tag it drew when it started. A job of
// this host with another tag is a previous process's.
func Runner() string { return runner }

// Scanner is the NFS filesystem walker / upserter.
type Scanner struct {
	st    *store.Store
	cfg   config.Config
	steps *processing.Steps
	prod  *events.Producer // nil-safe: emits stube.catalog.item.discovered on new items
	// runner is the process the scan jobs it starts name (Runner).
	runner string
	// beat is how often at most a walk gives its job a word (beatInterval).
	beat time.Duration
	// extras sends the triggers of the extras the scanner takes in; nil:
	// they wait for the sweep.
	extras ExtraSender
}

// New constructs a Scanner. Matches graph.ScanRunner structurally via Trigger.
// prod may be nil (events disabled) — the producer is nil-safe.
func New(st *store.Store, cfg config.Config, steps *processing.Steps, prod *events.Producer) *Scanner {
	return &Scanner{st: st, cfg: cfg, steps: steps, prod: prod, runner: Runner(),
		beat: beatInterval(cfg.RetryPolicy().Timeout("scan"))}
}

// beatInterval is how often at most a walk gives its job a word: every 30
// seconds, or every third of the scan's timeout when that is shorter, so a
// scan that walks is never silent for as long as its timeout.
func beatInterval(timeout time.Duration) time.Duration {
	return min(30*time.Second, timeout/3)
}

// FailInterrupted fails the scan jobs a previous process left running
// (store.FailInterruptedScanJobs), saying InterruptedReason, and returns how
// many: a scan runs in the process that started it, so one a restart cut short
// never ends. main calls it at startup, before this process starts a scan.
func (s *Scanner) FailInterrupted(ctx context.Context) (int, error) {
	return s.st.FailInterruptedScanJobs(ctx, s.runner, InterruptedReason)
}

// Trigger validates the source, inserts a 'running' scan job, launches the walk
// asynchronously, and returns the new job id. Only source 'nfs' is accepted
// (mirrors ScanController POST /api/scan returning 400 for anything else).
func (s *Scanner) Trigger(ctx context.Context, source string) (string, error) {
	if source != "nfs" {
		return "", errors.New("unsupported scan source: " + source)
	}
	job, err := s.st.StartScanJob(ctx, "nfs", s.runner)
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
	res, err := s.walk(ctx, s.heartbeat(ctx, jobID))
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

// heartbeat is what a walk calls at every entry it visits: at most every
// s.beat it gives the job a word (store.BeatScanJob). A scan that walks is so
// never taken for lost, and one stuck on a single entry (a mount that hangs)
// falls silent, for the reaper. A word that cannot be written is let go; the
// next one may be.
func (s *Scanner) heartbeat(ctx context.Context, jobID string) func() {
	last := time.Now()
	return func() {
		if time.Since(last) < s.beat {
			return
		}
		last = time.Now()
		_ = s.st.BeatScanJob(ctx, jobID)
	}
}

// walk performs the filesystem traversal, calling beat at every entry it
// visits. If the root does not exist it returns an empty result and nil error
// (graceful no-op, like NfsScanner.scan).
func (s *Scanner) walk(ctx context.Context, beat func()) (scanResult, error) {
	var res scanResult
	root := s.cfg.NFSRoot

	info, statErr := os.Stat(root)
	if statErr != nil || !info.IsDir() {
		// Root missing/unreadable: warn-equivalent no-op, no error.
		return res, nil
	}

	xs := newWalkState(s.extrasOn(ctx))
	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		beat()
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
		s.processFile(ctx, root, path, d, &res, xs)
		return nil
	})
	// The extras once every title's file is in: a trailer is walked before
	// the film it names ("Sintel-trailer.mkv" before "Sintel.mkv").
	if walkErr == nil && xs.on {
		s.takeExtras(ctx, root, xs.found)
		s.reconcileExtras(ctx, root)
	}
	return res, walkErr
}

// processFile classifies one regular file and upserts the catalog rows for it.
// Errors are swallowed (logged-equivalent) so the walk continues.
func (s *Scanner) processFile(ctx context.Context, root, path string, d fs.DirEntry, res *scanResult, xs *walkState) {
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

	// A title's extra is no item of its own (extras.go).
	if isVideo {
		if x, ok := xs.extraFileOf(absPath, name); ok {
			s.dropTrailerRow(ctx, absPath)
			if xs.on {
				xs.found = append(xs.found, x)
			}
			return
		}
	}

	typ := classify(rel, isVideo, isAudio)
	title := extractTitle(name, typ)
	year := extractYear(name)
	var seasonNumber, episodeNumber *int32
	var parentID *string
	var newSeriesID string // set when this file created a series parent (emit one discovered)
	if typ == "episode" {
		seasonNumber, episodeNumber = episodeCoords(name)
		if sid, created := s.resolveSeriesParent(ctx, pool, rel, name); sid != "" {
			parentID = &sid
			if created {
				newSeriesID = sid
			}
		}
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
			   (id, type, title, sorttitle, year, parent_id, seasonnumber, episodenumber, createdat, modifiedat)
			 VALUES (gen_random_uuid()::varchar, $1, $2, $3, $4, $5, $6, $7, now(), now())
			 RETURNING id`,
			typ, title, strings.ToLower(title), year, parentID, seasonNumber, episodeNumber).Scan(&itemID); err != nil {
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

		// A brand-new series parent (created by this episode) also enters the
		// pipeline: its discovered event drives enrichSeries (TMDB TV match +
		// child-episode backfill). Emitted once, only when newly created.
		if newSeriesID != "" {
			sev := events.NewItemEvent(newSeriesID)
			sev.Type = "series"
			sev.Step = "tmdb"
			sev.Source = "scan"
			s.prod.EmitItem(ctx, events.TopicDiscovered, sev)
		}
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

// resolveSeriesParent finds (or creates) the series parent item for an episode
// file, returning its id and whether it was newly created (so the caller emits a
// single discovered event for a new series). Match is on the normalised show
// title (sorttitle). The series row is metadata-only — it has NO playback asset,
// so it never enters analyze/transcode/package (gated in the enrich consumer);
// TMDB fills its title/year/artwork via enrichSeries. Best-effort: any DB error
// yields ("", false) and the episode is ingested without a parent.
//
// NOTE (find-or-create is safe within a single scan only): filepath.WalkDir
// invokes its callback sequentially, so per show the first episode creates the
// parent and the rest find it — no race. TWO overlapping scans could each
// SELECT-miss and INSERT, duplicating the series. Hardening follow-up: a partial
// unique index on (sorttitle) WHERE type='series' + ON CONFLICT upsert, or
// serialize Trigger against an existing 'running' scanjob.
func (s *Scanner) resolveSeriesParent(ctx context.Context, pool *pgxpool.Pool, rel, filename string) (string, bool) {
	title := seriesTitleFor(rel, filename)
	if strings.TrimSpace(title) == "" {
		return "", false
	}
	sort := strings.ToLower(title)

	var id string
	err := pool.QueryRow(ctx,
		`SELECT id FROM com_nalet_katalog_items WHERE type = 'series' AND sorttitle = $1 LIMIT 1`,
		sort).Scan(&id)
	if err == nil {
		return id, false
	}
	if err != pgx.ErrNoRows {
		return "", false
	}

	if err := pool.QueryRow(ctx,
		`INSERT INTO com_nalet_katalog_items (id, type, title, sorttitle, createdat, modifiedat)
		 VALUES (gen_random_uuid()::varchar, 'series', $1, $2, now(), now())
		 RETURNING id`,
		title, sort).Scan(&id); err != nil {
		return "", false
	}
	// The series parent is "scanned" the moment its first episode is seen — record
	// the step so it surfaces in the activity monitor alongside its episodes.
	_ = s.steps.Upsert(ctx, id, "scan", processing.StatusDone, nil, nil)
	return id, true
}

// scanSidecars finds subtitle files in the video's directory sharing the video
// basename (optional language suffix) and upserts subtitleassets rows. Ports
// NfsScanner.scanSidecars. Missing-table errors are swallowed.
//
// No subtitle file is the default: which one would be is the viewer's
// language to decide, not the order of the directory (Film.de.srt before
// Film.en.srt), and the packager packages each as a subtitle that is never
// the default unless forced. A row an earlier scan marked default is so no
// longer.
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
				`UPDATE com_nalet_katalog_subtitleassets SET item_id = $1, format = $2, lang = $3, label = $4, isdefault = false
				 WHERE path = $5`,
				itemID, format, langArg, labelArg, absPath)
		} else {
			_, _ = pool.Exec(ctx,
				`INSERT INTO com_nalet_katalog_subtitleassets
				   (id, item_id, path, format, lang, label, isdefault)
				 VALUES (gen_random_uuid()::varchar, $1, $2, $3, $4, $5, false)`,
				itemID, absPath, format, langArg, labelArg)
		}
	}
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
