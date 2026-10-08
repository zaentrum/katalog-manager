package retry

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/zaentrum/katalog-manager/internal/library"
	"github.com/zaentrum/katalog-manager/internal/processing"
)

// The take-in (the library's v2 layout): a title that gets no package now,
// and whose source has no version yet, is taken in. The packager renames its
// original into a version folder of its own (versions/<versionId>/
// original.<ext>) with its version.json and no package, the version is
// taken, and the title plays from its original as a title without a package
// does; a package is added to that version later (reencodeItem,
// packageItem). A title gets no package now when:
//   - its transcode was refused: the transcoder keeps the original of a
//     picture no package would show as it is, and says so
//     (processing.RefusedMark); that transcode is not retried by itself any
//     more, as encoding it again cannot help;
//   - its transcode or its package failed with no attempt left.
//
// An admin takes a title in by hand too (TakeInItem). The take-in is the
// title's step takein, katalog-manager's to send as the packager's trigger
// (transcoded, its step "takein", marked takein): it waits for the packager,
// which reports it as a worker reports its step, and the sweep retries,
// reaps and counts it as any other. A title is taken in while its source has
// no version (none taken, complete or superseded is made of it), nothing
// reads its original (no step that does waits or runs), and its step takein
// is none yet or done (of a source before); a take-in that failed is the
// retries'. It needs migration 044, and is sent once for a title: the claim
// of its step is one statement.

// asTakeIn marks the trigger of a take-in: no retry, so the packager's guard
// finds its step waiting and runs it.
var asTakeIn = send{mark: "takein", what: "the take-in"}

// takeInBatch is the most titles a pass takes in.
const takeInBatch = 50

// takeInRows are the titles due a take-in, of $1 when it names any: a movie
// or an episode whose primary asset's source is present and has no version,
// whose step takein is none or done, nothing reading its original waiting
// or running ($4), and whose transcode was refused ($3 ends its error) or
// failed with no attempt left, or whose package did, or any such title when
// an admin asks ($2), at most $5: each with its type, where the catalog has
// its original, whether its transcode was refused, and why it is due. A
// title whose file is a disc image is never taken in: the library holds no
// disc image.
var takeInRows = `SELECT i.id, i.type, src.arrivalpath,
			COALESCE(t.status = 'failed' AND COALESCE(t.error, t.lasterror, '') LIKE '%' || $3::text, false) AS refused,
			CASE WHEN t.status = 'failed' AND COALESCE(t.error, t.lasterror, '') LIKE '%' || $3::text
				THEN 'the transcode was refused: ' || COALESCE(t.error, t.lasterror)
			WHEN t.status = 'failed' AND t.nextretryat IS NULL
				THEN 'the transcode failed with no attempt left: ' || COALESCE(t.error, t.lasterror, 'no error given')
			WHEN p.status = 'failed' AND p.nextretryat IS NULL
				THEN 'the package failed with no attempt left: ' || COALESCE(p.error, p.lasterror, 'no error given')
			ELSE 'an admin takes it in' END AS why
		FROM com_nalet_katalog_items i
		JOIN LATERAL (SELECT a.sourceid FROM com_nalet_katalog_playbackassets a
			WHERE a.item_id = i.id AND a.isprimary = true AND COALESCE(a.kind, 'primary') = 'primary'
			ORDER BY a.id LIMIT 1) a ON true
		JOIN com_nalet_katalog_itemsources src ON src.id = a.sourceid AND src.state = 'present'
		LEFT JOIN ` + tbl + ` t ON t.item_id = i.id AND t.step = 'transcode'
		LEFT JOIN ` + tbl + ` p ON p.item_id = i.id AND p.step = 'package'
		LEFT JOIN ` + tbl + ` k ON k.item_id = i.id AND k.step = 'takein'
		WHERE lower(i.type) IN ('movie', 'episode')
		  AND (cardinality($1::text[]) = 0 OR i.id = ANY($1::text[]))
		  AND (k.id IS NULL OR k.status = 'done')
		  AND ($2::bool
		       OR (t.status = 'failed' AND (t.nextretryat IS NULL OR COALESCE(t.error, t.lasterror, '') LIKE '%' || $3::text))
		       OR (p.status = 'failed' AND p.nextretryat IS NULL))
		  AND NOT EXISTS (SELECT 1 FROM com_nalet_katalog_itemversions v WHERE v.item_id = i.id AND src.id = ANY(v.sourceids)
			AND v.state IN ('taken', 'complete', 'superseded'))
		  AND NOT EXISTS (SELECT 1 FROM ` + tbl + ` o WHERE o.item_id = i.id AND o.step = ANY($4::text[])
			AND o.status IN ('pending', 'in_progress'))
		  AND NOT ` + discImageOfI + `
		ORDER BY i.id
		LIMIT $5`

// discImageOfI is the condition that the title i's file is a disc image.
var discImageOfI = processing.DiscImageOf("i.id")

// takeInDue claims the take-in of the titles due (takeInRows): its step
// takein waits for the packager, the trigger noted as sent. It answers the
// step's row, the title and its type, and whether its transcode was refused.
var takeInDue = `WITH due AS (` + takeInRows + `),
	claimed AS (
		INSERT INTO ` + tbl + ` (id, createdat, modifiedat, item_id, step, status, attempts, details, failures, dispatchedat)
		SELECT gen_random_uuid()::varchar, now(), now(), due.id, 'takein', 'pending', 0, due.why, 0, now() FROM due
		ON CONFLICT (item_id, step) DO UPDATE SET status = 'pending', startedat = NULL, finishedat = NULL, error = NULL,
			details = EXCLUDED.details, failures = 0, nextretryat = NULL, dispatchedat = now(), modifiedat = now()
		WHERE ` + tbl + `.status = 'done'
		RETURNING id, item_id)
	SELECT claimed.id, claimed.item_id, due.type, due.refused FROM claimed JOIN due ON due.id = claimed.item_id`

// takeInReady says why no title is taken in, "" when they are: the layout is
// legacy, the catalog lacks migration 044, or no step can be sent
// (unavailable).
func (s *Service) takeInReady(ctx context.Context) (string, error) {
	set, err := library.ReadSettings(ctx, s.st.Pool())
	if err != nil {
		return "", fmt.Errorf("the library's settings: %w", err)
	}
	if !set.V2() {
		return "the library's layout is legacy: a title is taken in with the v2 layout only", nil
	}
	if !s.takeInMigrated.Load() {
		ok, err := s.st.TakeInReady(ctx)
		if err != nil {
			return "", err
		}
		if !ok {
			return "migration 044 (db/migrations/044_library_takein.sql) is not applied: no title is taken in until it is", nil
		}
		s.takeInMigrated.Store(true)
	}
	if why := s.unavailable(ctx); why != "" {
		return "cannot take in: " + why, nil
	}
	return "", nil
}

// TakeIn takes in the titles that are due (see above), of ids when it names
// any, at most a batch: their step takein waits for the packager, and its
// trigger goes; a refused transcode is retried by itself no more. A title
// whose trigger could not be sent has its step takein failed, as dispatch
// puts a step back. A take-in reported failed whose version is taken in is
// done first (settleTakeIns). It answers how many triggers went. With the
// legacy layout, without migration 044 or without an event bus it does
// nothing.
func (s *Service) TakeIn(ctx context.Context, ids []string) (int, error) {
	if why, err := s.takeInReady(ctx); err != nil || why != "" {
		return 0, err
	}
	if err := s.settleTakeIns(ctx, ids); err != nil {
		return 0, err
	}
	rows, err := s.claimTakeIns(ctx, ids, false)
	if err != nil {
		return 0, err
	}
	sent, _, _, first := s.dispatch(ctx, rows, s.automatic(), asTakeIn)
	return sent, first
}

// claimTakeIns claims the take-in of the titles due among ids (all of them
// when admin), and has a refused transcode retried by itself no more, in one
// transaction. A title whose original is not where the catalog has it is
// passed over, said in the log: a take-in takes an original from there, and
// one a run renamed into a version's folder whose handover was lost is that
// version's, whose run is reported again.
func (s *Service) claimTakeIns(ctx context.Context, ids []string, admin bool) ([]claimed, error) {
	if ids == nil {
		ids = []string{}
	}
	due, err := s.takeInCandidates(ctx, ids, admin)
	if err != nil || len(due) == 0 {
		return nil, err
	}
	tx, err := s.st.Pool().Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	rows, err := tx.Query(ctx, takeInDue, due, admin, processing.RefusedMark, processing.OriginalSteps, takeInBatch)
	if err != nil {
		return nil, fmt.Errorf("claim the titles to take in: %w", err)
	}
	var out []claimed
	var refused []string
	for rows.Next() {
		c := claimed{step: processing.StepTakeIn}
		var wasRefused bool
		if err := rows.Scan(&c.id, &c.itemID, &c.itemType, &wasRefused); err != nil {
			rows.Close()
			return nil, err
		}
		c.itemType = strings.ToLower(c.itemType)
		out = append(out, c)
		if wasRefused {
			refused = append(refused, c.itemID)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(refused) > 0 {
		if _, err := tx.Exec(ctx, `UPDATE `+tbl+` SET nextretryat = NULL, modifiedat = now()
			WHERE item_id = ANY($1) AND step = 'transcode' AND status = 'failed'`, refused); err != nil {
			return nil, err
		}
	}
	return out, tx.Commit(ctx)
}

// settleTakeIns has the failed take-in of each title (of ids when it names
// any) whose source's version is there done, saying which: the packager took
// the title in, and the catalog took the version, but the packager did not
// hear the answer (a handover answered and lost), and reported its step
// failed. Sending it again would run a take-in that is done. It needs
// migration 044: without it there is no version taken in, and it does
// nothing.
func (s *Service) settleTakeIns(ctx context.Context, ids []string) error {
	if !s.takeInMigrated.Load() {
		ok, err := s.st.TakeInReady(ctx)
		if err != nil || !ok {
			return err
		}
		s.takeInMigrated.Store(true)
	}
	if ids == nil {
		ids = []string{}
	}
	_, err := s.st.Pool().Exec(ctx, `UPDATE `+tbl+` k SET status = 'done', finishedat = now(), modifiedat = now(), error = NULL,
			failures = 0, nextretryat = NULL, dispatchedat = NULL,
			details = 'taken in: version ' || v.id || ' holds its original (its report was lost)'
		FROM com_nalet_katalog_playbackassets a
		JOIN com_nalet_katalog_itemversions v ON v.item_id = a.item_id AND a.sourceid = ANY(v.sourceids)
			AND v.state IN ('taken', 'complete', 'superseded')
		WHERE k.step = 'takein' AND k.status = 'failed' AND (cardinality($1::text[]) = 0 OR k.item_id = ANY($1::text[]))
		  AND a.item_id = k.item_id AND a.isprimary = true AND COALESCE(a.kind, 'primary') = 'primary'`, ids)
	if err != nil {
		return fmt.Errorf("settle the take-ins done: %w", err)
	}
	return nil
}

// takeInCandidates are the titles due a take-in among ids (takeInRows) whose
// original lies where the catalog has it; the others are said in the log.
func (s *Service) takeInCandidates(ctx context.Context, ids []string, admin bool) ([]string, error) {
	rows, err := s.st.Pool().Query(ctx, takeInRows, ids, admin, processing.RefusedMark, processing.OriginalSteps, takeInBatch)
	if err != nil {
		return nil, fmt.Errorf("the titles to take in: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id, typ, why string
		var at *string
		var refused bool
		if err := rows.Scan(&id, &typ, &at, &refused, &why); err != nil {
			return nil, err
		}
		if at == nil || !fileAt(*at) {
			log.Printf("retry: item %s is not taken in: its original is not where the catalog has it (%s)", id, deref(at))
			continue
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// fileAt reports whether a file is at path.
func fileAt(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.Mode().IsRegular()
}

func deref(s *string) string {
	if s == nil {
		return "none"
	}
	return *s
}

// sweepTakeIns is TakeIn of every title due, as Run calls it, saying what it
// sent.
func (s *Service) sweepTakeIns(ctx context.Context) {
	n, err := s.TakeIn(ctx, nil)
	if err != nil {
		log.Printf("retry: the take-ins: %v", err)
	}
	if n > 0 {
		log.Printf("retry: %d titles that get no package now are taken in (their step takein sent)", n)
	}
}

// TakeInResult says what an admin's take-in of a title did.
type TakeInResult struct {
	ItemID  string `json:"itemId"`
	Sent    bool   `json:"sent"`
	Message string `json:"message"`
}

// TakeInItem takes the title id in now, as an admin asks, whether or not its
// transcode or package failed: its original goes into a version of its own,
// with no package, as for a title that gets none (see above). A title whose
// source has a version, whose original something reads, whose take-in
// waits, runs or failed, or whose file is a disc image is left as it is, and
// so is every title while none can be taken in (the legacy layout, migration
// 044 missing, no event bus): the result says why. An episode another
// episode's file covers is taken in with that file: the take-in is its
// holder's, and the result the holder's, saying so.
func (s *Service) TakeInItem(ctx context.Context, id string) (TakeInResult, error) {
	holder, err := library.HolderOf(ctx, s.st.Pool(), id)
	if err != nil {
		return TakeInResult{ItemID: id}, err
	}
	if holder == "" {
		return s.takeInItem(ctx, id)
	}
	res, err := s.takeInItem(ctx, holder)
	res.Message = library.CoveredNote(id, holder) + res.Message
	return res, err
}

// takeInItem is TakeInItem of the title id, which no other's file covers.
func (s *Service) takeInItem(ctx context.Context, id string) (TakeInResult, error) {
	res := TakeInResult{ItemID: id}
	if why, err := s.takeInReady(ctx); err != nil || why != "" {
		res.Message = why
		return res, err
	}
	rows, err := s.claimTakeIns(ctx, []string{id}, true)
	if err != nil {
		return res, err
	}
	if len(rows) == 0 {
		res.Message, err = s.whyNoTakeIn(ctx, id)
		return res, err
	}
	_, _, notSent, first := s.dispatch(context.WithoutCancel(ctx), rows, false, asTakeIn)
	if len(notSent) > 0 {
		res.Message = fmt.Sprintf("the take-in could not be sent (%v); its step takein is failed: retry it", first)
		return res, nil
	}
	res.Sent, res.Message = true, "taken in: its step takein waits for the packager, which renames its original into a "+
		"version of its own, with no package"
	return res, nil
}

// whyNoTakeIn says why the title id is not taken in.
func (s *Service) whyNoTakeIn(ctx context.Context, id string) (string, error) {
	var typ, takein *string
	var file, versioned, busy, disc bool
	err := s.st.Pool().QueryRow(ctx, `SELECT
			(SELECT type FROM com_nalet_katalog_items WHERE id = $1),
			EXISTS (SELECT 1 FROM com_nalet_katalog_playbackassets a JOIN com_nalet_katalog_itemsources src ON src.id = a.sourceid
				WHERE a.item_id = $1 AND a.isprimary = true AND COALESCE(a.kind, 'primary') = 'primary' AND src.state = 'present'),
			EXISTS (SELECT 1 FROM com_nalet_katalog_playbackassets a JOIN com_nalet_katalog_itemversions v
				ON v.item_id = a.item_id AND a.sourceid = ANY(v.sourceids)
				WHERE a.item_id = $1 AND a.isprimary = true AND v.state IN ('taken', 'complete', 'superseded')),
			EXISTS (SELECT 1 FROM `+tbl+` o WHERE o.item_id = $1 AND o.step = ANY($2::text[]) AND o.status IN ('pending', 'in_progress')),
			(SELECT status FROM `+tbl+` WHERE item_id = $1 AND step = 'takein'),
			`+processing.DiscImageOf("$1::varchar"),
		id, processing.OriginalSteps).Scan(&typ, &file, &versioned, &busy, &takein, &disc)
	switch {
	case err != nil:
		return "", err
	case typ == nil:
		return "unknown item: " + id, nil
	case !strings.EqualFold(*typ, "movie") && !strings.EqualFold(*typ, "episode"):
		return fmt.Sprintf("only a movie or an episode is taken in, and %s is a %s", id, *typ), nil
	case !file:
		return "the title has no original to take in (none, or one retired)", nil
	case disc:
		return "its file is a " + processing.DiscImageReason + ": the library holds no disc image", nil
	case versioned:
		return "its original has a version already: a package is added to it, or made anew, by reencodeItem", nil
	case takein != nil && *takein == processing.StatusFailed:
		return "its take-in failed: retry its step takein", nil
	case takein != nil && *takein != processing.StatusDone:
		return "its take-in is " + *takein + " already", nil
	case busy:
		return "the pipeline reads its original now: take it in once its steps are over", nil
	}
	var at *string
	if err := s.st.Pool().QueryRow(ctx, `SELECT src.arrivalpath FROM com_nalet_katalog_playbackassets a
			JOIN com_nalet_katalog_itemsources src ON src.id = a.sourceid
		WHERE a.item_id = $1 AND a.isprimary = true AND COALESCE(a.kind, 'primary') = 'primary' LIMIT 1`, id).Scan(&at); err == nil &&
		(at == nil || !fileAt(*at)) {
		return fmt.Sprintf("its original is not where the catalog has it (%s): a run that renamed it into a version's folder "+
			"is reported again by its package step", deref(at)), nil
	}
	return "it is not taken in now", nil
}
