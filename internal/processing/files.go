package processing

import (
	"context"
	"path/filepath"
	"strings"
)

// What the pipeline runs of a title's file, and what it runs nothing of.
//
// One file of several episodes (migration 045) belongs to the first episode
// it covers, its holder, whose run is the file's: every other episode it
// covers names its holder, has no file of its own, and its steps of a file
// do not apply, saying which episode's file covers it (CoveredReason). The
// scan, which saw the file, and the enrichment, which reads the episode's own
// texts and images, are the episode's own.
//
// A disc image (an .iso or .img file) is no file the library holds: the
// scanner passes over one, and a title whose file is one runs nothing of it,
// its steps failed for good with DiscImageReason, until it is given a single
// file (replaceSource) or removed.

// CoveredSteps are the steps an episode another's file covers does not run:
// the steps of a file, its holder's.
var CoveredSteps = []string{"tidb", "chapter", "chromaprint", "blackframe", "silence", "subtitle", "transcode", "package"}

// OwnSteps are the steps an episode runs whatever covers it: the scan and
// the enrichment.
var OwnSteps = []string{"scan", "tmdb"}

// CoveredReason is why a step of an episode the file of the episode holder
// covers does not apply.
func CoveredReason(holder string) string {
	return "covered by the file of episode " + holder + ", which runs the pipeline for every episode it holds"
}

// UncoveredReason is why the steps of an episode the file of the episode
// holder covered no more do not apply: it has no file, as why says.
func UncoveredReason(holder, why string) string {
	return "no file: the file of episode " + holder + " " + why
}

// DiscImageReason is why the pipeline runs nothing of a disc image.
const DiscImageReason = "disc image: convert it to a single file"

// discImageExts are the extensions of a disc image, lower-case.
var discImageExts = map[string]bool{".iso": true, ".img": true}

// IsDiscImage reports whether path names a disc image, by its extension,
// letter case aside.
func IsDiscImage(path string) bool {
	return discImageExts[strings.ToLower(filepath.Ext(path))]
}

// DiscImageOf is the SQL condition that the title item's file (its primary
// asset) is a disc image.
func DiscImageOf(item string) string {
	return `EXISTS (SELECT 1 FROM com_nalet_katalog_playbackassets di WHERE di.item_id = ` + item +
		` AND di.isprimary = true AND lower(di.path) ~ '\.(iso|img)$')`
}

// FileSteps are the steps that read a title's file, which a disc image fails.
var FileSteps = []string{"tidb", "chapter", "chromaprint", "blackframe", "silence", "subtitle", "transcode", "package", "takein"}

// FailDiscImage fails, for good, the steps of the title itemID that read its
// file, which is a disc image: its transcode and its package, made failed
// when it has none, and every other of them it has that is not over (done,
// skipped, not applicable), each with DiscImageReason and no retry by
// itself. A step that says so already is left as it is.
func (s *Steps) FailDiscImage(ctx context.Context, itemID string) error {
	rows, err := s.pool.Query(ctx, `SELECT step, status, COALESCE(error, '') FROM `+tbl+` WHERE item_id = $1`, itemID)
	if err != nil {
		return err
	}
	has := map[string][2]string{}
	for rows.Next() {
		var step, status, msg string
		if err := rows.Scan(&step, &status, &msg); err != nil {
			rows.Close()
			return err
		}
		has[step] = [2]string{status, msg}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	reason := DiscImageReason
	var failed []string
	for _, step := range FileSteps {
		cur, ok := has[step]
		switch {
		case !ok && step != "transcode" && step != "package":
			continue
		case ok && (cur[0] == StatusDone || cur[0] == StatusSkipped || cur[0] == StatusNotApplicable):
			continue
		case ok && cur[0] == StatusFailed && cur[1] == reason:
			continue
		}
		if err := s.Upsert(ctx, itemID, step, StatusFailed, &reason, nil); err != nil {
			return err
		}
		failed = append(failed, step)
	}
	if len(failed) == 0 || s.legacy.Load() {
		return nil
	}
	// Failed for good: nothing retries it by itself.
	_, err = s.pool.Exec(ctx, `UPDATE `+tbl+` SET nextretryat = NULL WHERE item_id = $1 AND step = ANY($2) AND status = 'failed'`,
		itemID, failed)
	if missingRetryColumns(err) {
		return nil
	}
	return err
}
