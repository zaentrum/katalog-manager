package odownloader

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// RunPoller drives the ingestion loop: every cfg.ODownloaderPollSec seconds
// (after an initial 30s delay) it refreshes every active trailerjob from
// oDownloader and imports FINISHED video variants. It no-ops cleanly when the
// integration is disabled or when ctx is cancelled. main should run this in a
// goroutine: go svc.RunPoller(ctx).
func (s *Service) RunPoller(ctx context.Context) {
	if !s.cfg.ODownloaderEnabled() {
		log.Printf("odownloader: poller disabled (missing url or token)")
		return
	}

	interval := time.Duration(s.cfg.ODownloaderPollSec) * time.Second
	if interval <= 0 {
		interval = 15 * time.Second
	}

	// Initial delay of 30s mirrors the Java @Scheduled(initialDelay=30000).
	select {
	case <-ctx.Done():
		return
	case <-time.After(30 * time.Second):
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	s.pollAndIngest(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.pollAndIngest(ctx)
		}
	}
}

// trailerJob is the row snapshot the poller works over.
type trailerJob struct {
	ID         string
	ItemID     string
	LinkID     *string
	SourceURL  string
	PackageID  *string
	DownloadID *string
	State      string
	StartedAt  *time.Time
	Attempts   int
}

// pollAndIngest walks every active trailerjobs row and advances it. Mirrors
// TrailerIngestionService.pollAndIngest.
func (s *Service) pollAndIngest(ctx context.Context) {
	if !s.client.isEnabled() {
		return
	}

	rows, err := s.st.Pool().Query(ctx,
		`SELECT id, item_id, trailer_link_id, source_url, package_id,
		        download_id, state, started_at, attempts
		 FROM com_nalet_katalog_trailerjobs
		 WHERE state IN ('queued','running','downloaded')
		 ORDER BY createdat ASC LIMIT 50`)
	if err != nil {
		log.Printf("odownloader: poll query failed: %v", err)
		return
	}
	var active []trailerJob
	for rows.Next() {
		var j trailerJob
		if err := rows.Scan(&j.ID, &j.ItemID, &j.LinkID, &j.SourceURL, &j.PackageID,
			&j.DownloadID, &j.State, &j.StartedAt, &j.Attempts); err != nil {
			rows.Close()
			log.Printf("odownloader: poll scan failed: %v", err)
			return
		}
		active = append(active, j)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		log.Printf("odownloader: poll iterate failed: %v", err)
		return
	}
	if len(active) == 0 {
		return
	}

	cutoff := time.Now().Add(-time.Duration(s.cfg.ODownloaderTimeout) * time.Minute)
	for _, j := range active {
		if err := s.processOne(ctx, j, cutoff); err != nil {
			log.Printf("odownloader: poll.error job=%s url=%s: %v", j.ID, j.SourceURL, err)
			s.bumpAttempts(ctx, j.ID, err.Error())
		}
	}
}

// processOne advances a single trailerjob. Mirrors
// TrailerIngestionService.processOne.
func (s *Service) processOne(ctx context.Context, j trailerJob, cutoff time.Time) error {
	// Time-bounded: don't poll forever on a wedged download.
	started := time.Now()
	if j.StartedAt != nil {
		started = *j.StartedAt
	}
	if started.Before(cutoff) {
		s.markTerminal(ctx, j.ID, "timeout",
			fmt.Sprintf("Exceeded %d min without finishing.", s.cfg.ODownloaderTimeout))
		log.Printf("odownloader: timeout job=%s item=%s", j.ID, j.ItemID)
		return nil
	}
	if j.PackageID == nil {
		return nil
	}

	all := s.client.listDownloadsByPackage(ctx, *j.PackageID)
	if len(all) == 0 {
		return nil
	}

	best := pickBestVideoVariant(all)
	if best == nil {
		// Nothing video-shaped yet; record motion and wait for the next tick.
		msg := fmt.Sprintf("Waiting for video variant (.mp4/.mkv/.mov) — currently "+
			"%d variant(s) in package, e.g. %s", len(all), all[0].Name)
		_, err := s.st.Pool().Exec(ctx,
			`UPDATE com_nalet_katalog_trailerjobs
			 SET message = $1, started_at = COALESCE(started_at, now()), modifiedat = now()
			 WHERE id = $2`, truncate(msg, 500), j.ID)
		return err
	}

	// Pin the chosen video variant so subsequent ticks re-poll the same one.
	if _, err := s.st.Pool().Exec(ctx,
		`UPDATE com_nalet_katalog_trailerjobs
		 SET download_id = $1, state = $2, message = $3,
		     bytes_done = $4, bytes_total = $5,
		     started_at = COALESCE(started_at, now()), modifiedat = now()
		 WHERE id = $6`,
		best.ID, mapState(best.State), truncate(best.Message, 500),
		best.BytesDone, best.BytesTotal, j.ID); err != nil {
		return fmt.Errorf("pin variant: %w", err)
	}

	if !strings.EqualFold(best.State, "FINISHED") {
		return nil
	}

	// Stream the finished bytes into the inbox.
	filename := sanitizeFilename(best.Name, best.ID)
	target := filepath.Join(s.cfg.ODownloaderInbox, j.ItemID, filename)
	if err := s.importContent(ctx, best.ID, target); err != nil {
		_ = os.Remove(target)
		s.markTerminal(ctx, j.ID, "failed", "Import I/O error: "+err.Error())
		return err
	}

	size, err := fileSize(target)
	if err != nil {
		_ = os.Remove(target)
		s.markTerminal(ctx, j.ID, "failed", "Import I/O error: "+err.Error())
		return err
	}

	if err := s.writeTrailerAsset(ctx, j.ItemID, target, size); err != nil {
		return fmt.Errorf("write trailer asset: %w", err)
	}
	if j.LinkID != nil && *j.LinkID != "" {
		if _, err := s.st.Pool().Exec(ctx,
			`UPDATE com_nalet_katalog_itemtrailerlinks
			 SET localpath = $1, downloadedat = now(), modifiedat = now()
			 WHERE id = $2`, target, *j.LinkID); err != nil {
			return fmt.Errorf("stamp trailer link: %w", err)
		}
	}
	if err := s.markImported(ctx, j.ID, target, size); err != nil {
		return fmt.Errorf("mark imported: %w", err)
	}
	log.Printf("odownloader: imported job=%s item=%s bytes=%d path=%s variant=%s",
		j.ID, j.ItemID, size, target, best.Name)
	return nil
}

// importContent streams the download's content to target, creating parent dirs.
func (s *Service) importContent(ctx context.Context, downloadID, target string) error {
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	rc, err := s.client.openContent(ctx, downloadID)
	if err != nil {
		return err
	}
	if rc == nil {
		return fmt.Errorf("content stream was null")
	}
	defer rc.Close()

	f, err := os.Create(target)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, rc); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// writeTrailerAsset idempotently inserts the kind='trailer' playbackasset.
func (s *Service) writeTrailerAsset(ctx context.Context, itemID, path string, size int64) error {
	if _, err := s.st.Pool().Exec(ctx,
		`DELETE FROM com_nalet_katalog_playbackassets
		 WHERE item_id = $1 AND kind = 'trailer' AND path = $2`, itemID, path); err != nil {
		return err
	}
	_, err := s.st.Pool().Exec(ctx,
		`INSERT INTO com_nalet_katalog_playbackassets
		 (id, item_id, path, sizebytes, isprimary, kind)
		 VALUES (gen_random_uuid()::varchar, $1, $2, $3, false, 'trailer')`,
		itemID, path, size)
	return err
}

func (s *Service) markImported(ctx context.Context, jobID, path string, size int64) error {
	_, err := s.st.Pool().Exec(ctx,
		`UPDATE com_nalet_katalog_trailerjobs
		 SET state = 'imported', final_path = $1, bytes_done = $2,
		     finished_at = COALESCE(finished_at, now()), modifiedat = now()
		 WHERE id = $3`, path, size, jobID)
	return err
}

func (s *Service) markTerminal(ctx context.Context, jobID, state, message string) {
	if _, err := s.st.Pool().Exec(ctx,
		`UPDATE com_nalet_katalog_trailerjobs
		 SET state = $1, message = $2,
		     finished_at = COALESCE(finished_at, now()), modifiedat = now()
		 WHERE id = $3`, state, truncate(message, 500), jobID); err != nil {
		log.Printf("odownloader: markTerminal job=%s failed: %v", jobID, err)
	}
}

func (s *Service) bumpAttempts(ctx context.Context, jobID, message string) {
	if _, err := s.st.Pool().Exec(ctx,
		`UPDATE com_nalet_katalog_trailerjobs
		 SET attempts = attempts + 1, message = $1, modifiedat = now()
		 WHERE id = $2`, truncate(message, 500), jobID); err != nil {
		log.Printf("odownloader: bumpAttempts job=%s failed: %v", jobID, err)
	}
}

// pickBestVideoVariant returns the largest .mp4/.mkv/.mov variant by
// max(bytesTotal, bytesDone); nil when no video variant exists yet. Mirrors
// TrailerIngestionService.pickBestVideoVariant (excludes audio-only/sidecars).
func pickBestVideoVariant(all []downloadStatus) *downloadStatus {
	var best *downloadStatus
	var bestSize int64 = -1
	for i := range all {
		d := &all[i]
		if d.Name == "" {
			continue
		}
		lower := strings.ToLower(d.Name)
		if !(strings.HasSuffix(lower, ".mp4") ||
			strings.HasSuffix(lower, ".mkv") ||
			strings.HasSuffix(lower, ".mov")) {
			continue
		}
		sz := d.BytesTotal
		if d.BytesDone > sz {
			sz = d.BytesDone
		}
		if sz > bestSize {
			bestSize = sz
			best = d
		}
	}
	return best
}

// mapState maps oDownloader state strings to our lowercase column vocabulary.
// 'downloaded' is the synonym for FINISHED-before-import.
func mapState(odState string) string {
	switch strings.ToUpper(odState) {
	case "RUNNING":
		return "running"
	case "FINISHED":
		return "downloaded"
	case "FAILED", "ERROR":
		return "failed"
	case "QUEUED", "PENDING":
		return "queued"
	default:
		return "queued"
	}
}

// sanitizeFilename makes an oDownloader content name safe for the NFS path:
// replace '/', '\', NUL with '_', trim, fall back to "<id>.bin" when blank.
// Unicode is preserved (foreign-language trailers keep their names).
func sanitizeFilename(raw, fallback string) string {
	if strings.TrimSpace(raw) == "" {
		return fallback + ".bin"
	}
	cleaned := strings.NewReplacer("/", "_", "\\", "_", "\x00", "_").Replace(raw)
	cleaned = strings.TrimSpace(cleaned)
	if cleaned == "" {
		return fallback + ".bin"
	}
	return cleaned
}

func fileSize(path string) (int64, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	return fi.Size(), nil
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
