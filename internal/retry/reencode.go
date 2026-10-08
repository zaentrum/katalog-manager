package retry

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/zaentrum/katalog-manager/internal/events"
	"github.com/zaentrum/katalog-manager/internal/graph"
	"github.com/zaentrum/katalog-manager/internal/library"
	"github.com/zaentrum/katalog-manager/internal/processing"
)

// chain are the steps a re-encode runs again: the transcode, and the package
// that follows it.
var chain = []string{"transcode", "package"}

// reencodeSet is what a re-encode does to a title's transcode and package:
// each waits for its worker afresh, as processing.Steps.ResetForItems resets
// a step (its run's times and error cleared, no failures in a row, no retry
// scheduled; its attempts and last error kept), and the transcode is noted as
// sent, its trigger going out now. The package's trigger is the transcoder's
// to send once it is done, so the package is not noted as sent: the reaper
// would take it for lost after the package's timeout, while the transcode may
// run for longer.
const reencodeSet = `status = 'pending', startedat = NULL, finishedat = NULL, error = NULL, modifiedat = now(),
	failures = 0, nextretryat = NULL, dispatchedat = CASE WHEN s.step = 'transcode' THEN now() END`

// ReencodeItem encodes a title again with the pipeline's current settings
// (the transcoder's ladder and encoder, the packager's): a movie or an episode
// with a file (a primary asset), or every episode of a series that has one.
// The title's transcode and package wait for their workers afresh
// (reencodeSet), and the transcoder is told: the event it consumes
// (analyzed), not marked as a retry, so its guard finds the step waiting and
// runs it; the packager follows when the transcode is done.
//
// A title whose transcode or package is running, or waiting for its worker,
// within the step's timeout is left alone: encoding it again would run the
// step twice (busy). Each title is reset in a transaction of its own that
// locks its steps first, so two re-encodes at once, or a re-encode and the
// sweep, reset and send a title once. An event that could not be sent puts
// its transcode back: failed, sent again a backoff later when the sweep runs.
//
// The current package keeps playing while the transcoder encodes: it writes
// its handoff beside the package, not into it. The packager writes a package
// in place, though, clearing the old one as it starts: from then until it is
// done the title plays by on-demand transcoding, and a viewer watching the
// old package then loses it.
//
// An episode another episode's file covers is encoded with that file: the
// re-encode is its holder's, and the result the holder's, saying so. A title
// whose file is a disc image is not encoded (processing.DiscImageReason), and
// a series' episode whose file is one is no title of its re-encode.
func (s *Service) ReencodeItem(ctx context.Context, id string) (graph.ReencodeResult, error) {
	holder, err := library.HolderOf(ctx, s.st.Pool(), id)
	if err != nil {
		return graph.ReencodeResult{ItemID: id}, err
	}
	if holder == "" {
		return s.reencodeItem(ctx, id)
	}
	res, err := s.reencodeItem(ctx, holder)
	res.Message = library.CoveredNote(id, holder) + res.Message
	return res, err
}

// reencodeItem is ReencodeItem of the title id, which no other's file covers.
func (s *Service) reencodeItem(ctx context.Context, id string) (graph.ReencodeResult, error) {
	res := graph.ReencodeResult{ItemID: id}
	var typ string
	err := s.st.Pool().QueryRow(ctx, `SELECT type FROM com_nalet_katalog_items WHERE id = $1`, id).Scan(&typ)
	if errors.Is(err, pgx.ErrNoRows) {
		return res, fmt.Errorf("unknown item: %s", id)
	}
	if err != nil {
		return res, err
	}
	typ = strings.ToLower(typ)
	if typ != "movie" && typ != "episode" && typ != "series" {
		return res, fmt.Errorf("only a movie, an episode or a series is encoded again, and %s is of type %s", id, typ)
	}
	if why := s.unavailable(ctx); why != "" {
		return res, errors.New("cannot re-encode: " + why)
	}
	if typ != "series" {
		if disc, err := library.IsDiscImageTitle(ctx, s.st.Pool(), id); err != nil {
			return res, err
		} else if disc {
			res.Message = "its file is a " + processing.DiscImageReason
			return res, nil
		}
	}

	titles, gone, err := s.titlesOf(ctx, id, typ)
	if err != nil {
		return res, err
	}
	res.Titles = int32(len(titles))
	if len(titles) == 0 {
		res.Message = "the " + typ + " has no file to encode"
		switch {
		case typ != "series" && gone.Gone():
			res.Message = gone.Why() + "; " + newArrival
		case typ == "series" && gone.State != "":
			res.Message = "the series has no episode with a file to encode: " + retiredEpisodes(gone)
		case typ == "series":
			res.Message = "the series has no episode with a file to encode"
		}
		return res, nil
	}

	// A caller that stops waiting leaves no title reset without its event.
	ctx = context.WithoutCancel(ctx)
	titleType := typ
	if typ == "series" {
		titleType = "episode"
	}
	var reset []claimed
	var whys []string
	var failed error
	for _, t := range titles {
		row, why, err := s.reset(ctx, t)
		if err != nil {
			failed = fmt.Errorf("re-encode %s: %w", t, err)
			break
		}
		if why != "" {
			whys = append(whys, why)
			continue
		}
		reset = append(reset, claimed{id: row, itemID: t, step: "transcode", itemType: titleType})
	}
	sent, _, notSent, first := s.dispatch(ctx, reset, s.automatic(), asReencode)
	if failed != nil {
		return res, failed
	}
	res.Reencoded, res.Busy, res.NotSent = int32(sent), int32(len(whys)), int32(len(notSent))
	res.Message = s.reencodeMessage(typ, res, whys, first)
	if typ == "series" && gone.State != "" {
		res.Message += "; " + retiredEpisodes(gone)
	}
	return res, nil
}

// newArrival is how a title whose original was deleted after packaging gets
// a better version: from a file it is given anew.
const newArrival = "a better version is a new arrival: put it in .work/replace and call replaceSource"

// retired are a series' episodes whose originals were deleted after
// packaging, which a re-encode skips: how many, and the last one's.
type retired struct {
	library.Original
	episodes int
}

// retiredEpisodes says which episodes a re-encode of a series skipped.
func retiredEpisodes(r retired) string {
	event, at := r.EventID, "an unknown time"
	if r.At != nil {
		at = library.Timestamp(*r.At)
	}
	if r.episodes == 1 {
		return fmt.Sprintf("1 episode is skipped, its original deleted after packaging (event %s, %s); %s", event, at, newArrival)
	}
	return fmt.Sprintf("%d episodes are skipped, their originals deleted after packaging (the last: event %s, %s); %s",
		r.episodes, event, at, newArrival)
}

// titlesOf are the titles a re-encode of id encodes: the movie or episode
// when it has a primary asset, a series' episodes (under it or under a season
// of it) that have one, which is no disc image, by season and episode; of
// those whose original was deleted after packaging, or is being deleted,
// none: it answers what became of the movie's or the episode's original, or
// of the series' episodes'.
func (s *Service) titlesOf(ctx context.Context, id, typ string) ([]string, retired, error) {
	pool := s.st.Pool()
	if typ != "series" {
		var ok bool
		err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM com_nalet_katalog_playbackassets
			WHERE item_id = $1 AND isprimary = true)`, id).Scan(&ok)
		if err != nil {
			return nil, retired{}, err
		}
		o, err := library.OriginalOf(ctx, pool, id)
		if err != nil {
			return nil, retired{}, err
		}
		if !ok || o.Gone() {
			return nil, retired{Original: o}, nil
		}
		return []string{id}, retired{}, nil
	}
	rows, err := pool.Query(ctx, `SELECT e.id,
			EXISTS (SELECT 1 FROM com_nalet_katalog_playbackassets p WHERE p.item_id = e.id AND p.isprimary = true)
				AND NOT `+processing.DiscImageOf("e.id")+`
		FROM com_nalet_katalog_items e
		WHERE e.type = 'episode'
		  AND (e.parent_id = $1 OR e.parent_id IN (SELECT id FROM com_nalet_katalog_items WHERE parent_id = $1))
		ORDER BY e.seasonnumber NULLS LAST, e.episodenumber NULLS LAST, e.id`, id)
	if err != nil {
		return nil, retired{}, err
	}
	var all []string
	has := map[string]bool{}
	for rows.Next() {
		var e string
		var file bool
		if err := rows.Scan(&e, &file); err != nil {
			rows.Close()
			return nil, retired{}, err
		}
		all = append(all, e)
		has[e] = file
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, retired{}, err
	}
	originals, err := library.OriginalsOf(ctx, pool, all)
	if err != nil {
		return nil, retired{}, err
	}
	var out []string
	var gone retired
	for _, e := range all {
		o := originals[e]
		if o.Gone() {
			gone.episodes++
			if gone.At == nil || (o.At != nil && o.At.After(*gone.At)) {
				gone.Original = o
			}
			continue
		}
		if has[e] {
			out = append(out, e)
		}
	}
	return out, gone, nil
}

// chainStep is a title's transcode or package as a re-encode finds it.
type chainStep struct {
	status string
	sent   bool       // its trigger was sent again, and no worker has reported since
	within bool       // running or waiting within its timeout
	since  *time.Time // its worker's last word, or when its trigger was sent
}

// reset claims a title's transcode and package for a re-encode, in one
// transaction: the steps it lacks are added (waiting, no run yet), its steps
// are locked, and unless the title is busy both are reset (reencodeSet). It
// returns the transcode's row, or why the title was left alone, in which
// case nothing changed (the steps added go with the transaction).
func (s *Service) reset(ctx context.Context, itemID string) (transcode, why string, err error) {
	tx, err := s.st.Pool().Begin(ctx)
	if err != nil {
		return "", "", err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	added := map[string]bool{}
	rows, err := tx.Query(ctx, `INSERT INTO `+tbl+` (id, createdat, modifiedat, item_id, step, status, attempts)
		SELECT gen_random_uuid()::varchar, now(), now(), $1, st, 'pending', 0 FROM unnest($2::text[]) AS st
		ON CONFLICT (item_id, step) DO NOTHING
		RETURNING step`, itemID, chain)
	if err != nil {
		return "", "", err
	}
	for rows.Next() {
		var step string
		if err := rows.Scan(&step); err != nil {
			rows.Close()
			return "", "", err
		}
		added[step] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return "", "", err
	}

	secs := make([]float64, len(chain))
	for i, step := range chain {
		secs[i] = s.pol.Timeout(step).Seconds()
	}
	rows, err = tx.Query(ctx, `SELECT s.step, s.status, s.dispatchedat IS NOT NULL,
			CASE s.status
				WHEN 'in_progress' THEN `+heard+` >= localtimestamp - make_interval(secs => t.secs)
				WHEN 'pending' THEN COALESCE(s.dispatchedat, (`+heard+`)::timestamptz) >= now() - make_interval(secs => t.secs)
				ELSE false END,
			COALESCE(s.dispatchedat, COALESCE(s.modifiedat, s.startedat, s.createdat)::timestamptz)
		FROM `+tbl+` s JOIN unnest($2::text[], $3::float8[]) AS t(step, secs) ON t.step = s.step
		WHERE s.item_id = $1
		ORDER BY s.step
		FOR UPDATE OF s`, itemID, chain, secs)
	if err != nil {
		return "", "", err
	}
	cur := map[string]chainStep{}
	for rows.Next() {
		var step string
		var c chainStep
		if err := rows.Scan(&step, &c.status, &c.sent, &c.within, &c.since); err != nil {
			rows.Close()
			return "", "", err
		}
		if !added[step] {
			cur[step] = c
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return "", "", err
	}
	if why := s.busy(cur); why != "" {
		return "", why, nil
	}

	rows, err = tx.Query(ctx, `UPDATE `+tbl+` s SET `+reencodeSet+`
		WHERE s.item_id = $1 AND s.step = ANY($2)
		RETURNING s.id, s.step`, itemID, chain)
	if err != nil {
		return "", "", err
	}
	for rows.Next() {
		var row, step string
		if err := rows.Scan(&row, &step); err != nil {
			rows.Close()
			return "", "", err
		}
		if step == "transcode" {
			transcode = row
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return "", "", err
	}
	return transcode, "", tx.Commit(ctx)
}

// busy says why a title's transcode and package are left alone, "" when they
// are not: one of them is running (in progress, its worker heard within the
// step's timeout), or waiting for its worker within it (pending since its
// trigger was sent, or since it was written). A package waiting while its
// transcode has not finished waits for that transcode, which decides; only a
// package sent again (its retry on the way to the packager) counts then. cur
// holds the steps the title had before the re-encode.
func (s *Service) busy(cur map[string]chainStep) string {
	tr, has := cur["transcode"]
	transcoded := !has || tr.status == processing.StatusDone || tr.status == processing.StatusNotApplicable ||
		tr.status == processing.StatusSkipped
	for _, step := range chain {
		c, ok := cur[step]
		if !ok || !c.within {
			continue
		}
		since := "an unknown time"
		if c.since != nil {
			since = c.since.UTC().Format(time.RFC3339)
		}
		timeout := Label(s.pol.Timeout(step))
		switch c.status {
		case processing.StatusInProgress:
			return fmt.Sprintf("%s is running: its worker last reported at %s, and encoding it again would run it twice; "+
				"wait for it, or for its timeout of %s", step, since, timeout)
		case processing.StatusPending:
			if step == "package" && !c.sent && !transcoded {
				continue
			}
			return fmt.Sprintf("%s is waiting for its worker since %s, and encoding it again would run it twice; "+
				"wait for it, or for its timeout of %s", step, since, timeout)
		}
	}
	return ""
}

// reencodeMessage says what a re-encode did.
func (s *Service) reencodeMessage(typ string, res graph.ReencodeResult, whys []string, first error) string {
	trigger := events.TopicAnalyzed
	again := ": failed, and the sweep sends it again a backoff later"
	if !s.automatic() {
		again = ": failed; retry its transcode"
	}
	if typ != "series" {
		switch {
		case res.Busy > 0:
			return whys[0]
		case res.NotSent > 0:
			return fmt.Sprintf("the re-encode could not be sent (%v); its transcode is put back%s", first, again)
		}
		return fmt.Sprintf("encoding it again: its transcode and package wait for their workers (%s sent); "+
			"the current package plays until the packager starts on the new one", trigger)
	}
	parts := []string{fmt.Sprintf("encoding %d of its %d episodes with a file again (%s sent for each)", res.Reencoded, res.Titles, trigger)}
	if res.Reencoded == 0 {
		parts[0] = fmt.Sprintf("encoding none of its %d episodes with a file again", res.Titles)
	}
	if res.Busy > 0 {
		parts = append(parts, fmt.Sprintf("%d left alone, a transcode or package of them running or waiting for its worker "+
			"(the first: %s)", res.Busy, whys[0]))
	}
	if res.NotSent > 0 {
		parts = append(parts, fmt.Sprintf("%d could not be sent (%v)%s", res.NotSent, first, again))
	}
	return strings.Join(parts, "; ")
}
