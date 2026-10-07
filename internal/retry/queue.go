package retry

// The re-encode queue (migration 043) holds the titles to encode again under
// the pipeline's current settings, as a migration's job or an admin asks
// (POST /api/library/reencode): titles named (a series its episodes), the
// titles whose retire is held for their surround (library.HeldForSurround),
// or every packaged movie and episode. The sweep sends them at the pace the
// settings allow: at most library.reencode.rate titles a pass, no more while
// library.reencode.inflight of them are sent and not done, and only within
// library.reencode.window. A title is sent as reencodeItem sends one (reset,
// with the same busy rules: a busy title stays queued, and the next one
// goes), done once its package completes, and failed when its transcode or
// its package fails with no attempt left, or its package is skipped. A title
// is in the queue once while it waits or is sent; done and failed ones stay
// until an admin clears them.

import (
	"context"
	"errors"
	"fmt"
	"log"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/zaentrum/katalog-manager/internal/auth"
	"github.com/zaentrum/katalog-manager/internal/graph"
	"github.com/zaentrum/katalog-manager/internal/library"
	"github.com/zaentrum/katalog-manager/internal/store"
)

const queueTbl = store.ReencodeQueueTable

// queueLock is the advisory lock a pass over the queue holds, the catalog's
// (its schema's): one instance sends the queue at a time, so its rate and
// its titles in flight hold across them.
const queueLock = `hashtext('reencode-queue/' || current_schema())`

// The queue's states.
const (
	QueueQueued = "queued"
	QueueSent   = "sent"
	QueueDone   = "done"
	QueueFailed = "failed"
)

// queueStates are the states in their order.
var queueStates = []string{QueueQueued, QueueSent, QueueDone, QueueFailed}

// ErrNoQueue says the catalog has no re-encode queue: migration 043 is not
// applied (or the table cannot be read).
var ErrNoQueue = errors.New("no re-encode queue")

// queueTable says why there is no queue, "" when there is.
func (s *Service) queueTable(ctx context.Context) string {
	if !s.queue.Load() {
		ok, err := s.st.ReencodeQueueReady(ctx)
		if err != nil {
			return "the re-encode queue could not be read: " + err.Error()
		}
		if !ok {
			return "migration 043 (db/migrations/043_reencode_queue.sql) is not applied"
		}
		s.queue.Store(true)
	}
	return ""
}

// EnqueueReencode queues the titles req names to be encoded again: the movies
// and episodes of req.Items, a series' episodes (under it or a season of it),
// with req.Held every title whose retire is held for its surround, with
// req.All every packaged movie and episode; each title once, in that order.
// A title with no file to encode, or whose original was deleted after
// packaging, is skipped, saying why, and so is a name that is no movie,
// episode or series. A title queued already, waiting or sent, stays as it is
// (alreadyQueued). The caller's subject is noted as who queued them.
func (s *Service) EnqueueReencode(ctx context.Context, req graph.ReencodeRequest) (graph.ReencodeEnqueued, error) {
	out := graph.ReencodeEnqueued{Skipped: []graph.ReencodeSkipped{}}
	if why := s.queueTable(ctx); why != "" {
		return out, fmt.Errorf("%w: %s", ErrNoQueue, why)
	}
	if len(req.Items) == 0 && !req.Held && !req.All {
		return out, errors.New(`name the titles: {"items": [...]}, {"held": true} or {"all": true}`)
	}
	pool := s.st.Pool()
	var titles, selections []string
	picked := map[string]bool{}
	skipped := map[string]bool{}
	pick := func(id, selection string) {
		if !picked[id] && !skipped[id] {
			picked[id] = true
			titles, selections = append(titles, id), append(selections, selection)
		}
	}
	skip := func(id, why string) {
		if !picked[id] && !skipped[id] {
			skipped[id] = true
			out.Skipped = append(out.Skipped, graph.ReencodeSkipped{ItemID: id, Reason: why})
		}
	}

	var names []string
	for _, id := range req.Items {
		if id = strings.TrimSpace(id); id != "" {
			names = append(names, id)
		}
	}
	if len(names) > 0 {
		types, err := s.typesOf(ctx, names)
		if err != nil {
			return out, err
		}
		for _, id := range names {
			typ, ok := types[id]
			switch {
			case picked[id] || skipped[id]:
			case !ok:
				skip(id, "unknown item")
			case typ == "movie" || typ == "episode":
				pick(id, "items")
			case typ == "series":
				episodes, err := s.episodesOf(ctx, id)
				if err != nil {
					return out, err
				}
				if len(episodes) == 0 {
					skip(id, "the series has no episode to encode")
				}
				for _, e := range episodes {
					pick(e, "items")
				}
			default:
				skip(id, "only a movie, an episode or a series is encoded again, and it is of type "+typ)
			}
		}
	}
	for _, sel := range []struct {
		on        bool
		name, sql string
	}{
		{req.Held, "held", `SELECT i.id FROM com_nalet_katalog_items i
			WHERE lower(i.type) IN ('movie', 'episode') AND ` + library.HeldForSurround("i.id") + `
			ORDER BY i.id`},
		{req.All, "all", `SELECT i.id FROM com_nalet_katalog_items i
			WHERE lower(i.type) IN ('movie', 'episode') AND EXISTS (SELECT 1 FROM com_nalet_katalog_playbackassets a
				WHERE a.item_id = i.id AND (a.codec LIKE 'hev1%' OR a.codec LIKE 'hvc1%'))
			ORDER BY i.id`},
	} {
		if !sel.on {
			continue
		}
		ids, err := s.ids(ctx, sel.sql)
		if err != nil {
			return out, err
		}
		for _, id := range ids {
			pick(id, sel.name)
		}
	}

	// Of the titles picked, those with nothing to encode are skipped.
	why, err := s.unencodable(ctx, titles)
	if err != nil {
		return out, err
	}
	var queue, of []string
	for i, id := range titles {
		if w := why[id]; w != "" {
			out.Skipped = append(out.Skipped, graph.ReencodeSkipped{ItemID: id, Reason: w})
			continue
		}
		queue, of = append(queue, id), append(of, selections[i])
	}
	if len(queue) == 0 {
		return out, nil
	}
	tag, err := pool.Exec(ctx, `INSERT INTO `+queueTbl+` (id, item_id, selection, enqueuedby)
		SELECT gen_random_uuid()::varchar, t.item, t.selection, $3
		FROM unnest($1::text[], $2::text[]) WITH ORDINALITY AS t(item, selection, n)
		ORDER BY t.n
		ON CONFLICT (item_id) WHERE state IN ('queued', 'sent') DO NOTHING`,
		queue, of, auth.Actor(ctx, "katalog-manager"))
	if err != nil {
		return out, fmt.Errorf("queue the titles: %w", err)
	}
	out.Queued = int32(tag.RowsAffected())
	out.AlreadyQueued = int32(len(queue)) - out.Queued
	return out, nil
}

// episodesOf are a series' episodes, under it or under a season of it, by
// season and episode.
func (s *Service) episodesOf(ctx context.Context, series string) ([]string, error) {
	return s.ids(ctx, `SELECT e.id FROM com_nalet_katalog_items e
		WHERE lower(e.type) = 'episode'
		  AND (e.parent_id = $1 OR e.parent_id IN (SELECT id FROM com_nalet_katalog_items WHERE parent_id = $1))
		ORDER BY e.seasonnumber NULLS LAST, e.episodenumber NULLS LAST, e.id`, series)
}

func (s *Service) ids(ctx context.Context, sql string, args ...any) ([]string, error) {
	rows, err := s.st.Pool().Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// unencodable says of each title of ids that has nothing to encode why:
// gone from the catalog, its original deleted after packaging (or being
// deleted), or no file (a primary asset).
func (s *Service) unencodable(ctx context.Context, ids []string) (map[string]string, error) {
	why := map[string]string{}
	if len(ids) == 0 {
		return why, nil
	}
	pool := s.st.Pool()
	rows, err := pool.Query(ctx, `SELECT x.id, lower(i.type),
			EXISTS (SELECT 1 FROM com_nalet_katalog_playbackassets p WHERE p.item_id = x.id AND p.isprimary = true)
		FROM unnest($1::text[]) AS x(id) LEFT JOIN com_nalet_katalog_items i ON i.id = x.id`, ids)
	if err != nil {
		return nil, err
	}
	type title struct {
		typ  *string
		file bool
	}
	of := map[string]title{}
	for rows.Next() {
		var id string
		var t title
		if err := rows.Scan(&id, &t.typ, &t.file); err != nil {
			rows.Close()
			return nil, err
		}
		of[id] = t
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	originals, err := library.OriginalsOf(ctx, pool, ids)
	if err != nil {
		return nil, err
	}
	for _, id := range ids {
		t := of[id]
		switch o := originals[id]; {
		case t.typ == nil:
			why[id] = "unknown item"
		case o.Gone():
			why[id] = o.Why() + "; " + newArrival
		case !t.file:
			why[id] = "the " + *t.typ + " has no file to encode"
		}
	}
	return why, nil
}

// ReencodeQueue says what the queue holds: its titles by state, the one
// waiting or sent that was queued first and the one queued last, and why the
// sweep sends none now ("" when it may).
func (s *Service) ReencodeQueue(ctx context.Context) (graph.ReencodeQueue, error) {
	var q graph.ReencodeQueue
	if why := s.queueTable(ctx); why != "" {
		return q, fmt.Errorf("%w: %s", ErrNoQueue, why)
	}
	pool := s.st.Pool()
	if err := pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE state = 'queued'), count(*) FILTER (WHERE state = 'sent'),
			count(*) FILTER (WHERE state = 'done'), count(*) FILTER (WHERE state = 'failed')
		FROM `+queueTbl).Scan(&q.Queued, &q.Sent, &q.Done, &q.Failed); err != nil {
		return q, err
	}
	for _, end := range []struct {
		order string
		to    **graph.QueuedTitle
	}{{"ASC", &q.Oldest}, {"DESC", &q.Newest}} {
		var t graph.QueuedTitle
		err := pool.QueryRow(ctx, `SELECT item_id, state, enqueuedat FROM `+queueTbl+`
			WHERE state IN ('queued', 'sent') ORDER BY seq `+end.order+` LIMIT 1`).Scan(&t.ItemID, &t.State, &t.EnqueuedAt)
		if err == nil {
			*end.to = &t
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return q, err
		}
	}
	set, err := library.ReadSettings(ctx, pool)
	if err != nil {
		return q, err
	}
	if why := s.queueIdle(ctx, set); why != "" {
		q.Idle = &why
	}
	return q, nil
}

// ClearReencodeQueue deletes the queue's titles in states, by default those
// queued, done and failed: a title sent is being encoded, and its end is
// still noted (clearing it frees its place in flight, and its encoding goes
// on). It answers how many it deleted.
func (s *Service) ClearReencodeQueue(ctx context.Context, states []string) (int32, error) {
	if why := s.queueTable(ctx); why != "" {
		return 0, fmt.Errorf("%w: %s", ErrNoQueue, why)
	}
	if len(states) == 0 {
		states = []string{QueueQueued, QueueDone, QueueFailed}
	}
	for _, st := range states {
		if !slices.Contains(queueStates, st) {
			return 0, fmt.Errorf("unknown state %q: one of %s", st, strings.Join(queueStates, ", "))
		}
	}
	tag, err := s.st.Pool().Exec(ctx, `DELETE FROM `+queueTbl+` WHERE state = ANY($1)`, states)
	if err != nil {
		return 0, err
	}
	return int32(tag.RowsAffected()), nil
}

// QueuePass is what a pass over the queue did: the titles sent done or
// failed since, the titles sent now, those that wait, being busy, and those
// whose event could not be sent (both stay queued), the titles failed before
// they were sent (gone, retired, no file), and why the pass sent none, ""
// when it could, with how many titles wait then.
type QueuePass struct {
	Done, Failed, Sent, Busy, NotSent, Refused int
	Idle                                       string
	Waiting                                    int
}

func (p QueuePass) String() string {
	var parts []string
	for _, c := range []struct {
		n    int
		what string
	}{{p.Sent, "sent"}, {p.Done, "done"}, {p.Failed, "failed"}, {p.Busy, "busy, waiting"},
		{p.NotSent, "not sent, waiting"}, {p.Refused, "failed before they were sent"}} {
		if c.n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", c.n, c.what))
		}
	}
	return strings.Join(parts, ", ")
}

// queueIdle says why the sweep sends none of the queue now, "" when it may:
// no event bus (or no migration 033), library.reencode.rate 0, or a window
// that cannot be read or that now lies outside of.
func (s *Service) queueIdle(ctx context.Context, set library.Settings) string {
	if why := s.unavailable(ctx); why != "" {
		return why
	}
	if set.ReencodeRate <= 0 {
		return library.SettingReencodeRate + " is 0"
	}
	w, err := library.ParseWindow(set.ReencodeWindow)
	if err != nil {
		return library.SettingReencodeWindow + ": " + err.Error() + "; nothing is sent until it reads"
	}
	if !w.Contains(s.now()) {
		return "outside " + library.SettingReencodeWindow + " (" + strings.TrimSpace(set.ReencodeWindow) + ")"
	}
	return ""
}

// DrainQueue is one pass over the queue, in one instance at a time: the
// titles sent whose package completed are done, those whose transcode or
// package failed for good are failed; then, while the sweep may send
// (queueIdle), the titles queued first go, as many as library.reencode.rate
// allows a pass and library.reencode.inflight leaves room for. A title busy
// stays queued, saying why, and the next one goes; one gone, retired or with
// no file to encode fails.
func (s *Service) DrainQueue(ctx context.Context) (QueuePass, error) {
	var p QueuePass
	if why := s.queueTable(ctx); why != "" {
		p.Idle = why
		return p, nil
	}
	if why := s.stepTable(ctx); why != "" {
		p.Idle = why
		return p, nil
	}
	conn, err := s.st.Pool().Acquire(ctx)
	if err != nil {
		return p, err
	}
	defer conn.Release()
	var locked bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock(`+queueLock+`)`).Scan(&locked); err != nil {
		return p, err
	}
	if !locked {
		// another instance sends the queue now
		return p, nil
	}
	defer func() {
		_, _ = conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock(`+queueLock+`)`)
	}()

	if p.Done, p.Failed, err = s.finishSent(ctx); err != nil {
		return p, err
	}
	set, err := library.ReadSettings(ctx, s.st.Pool())
	if err != nil {
		return p, fmt.Errorf("the library's settings: %w", err)
	}
	if p.Idle = s.queueIdle(ctx, set); p.Idle != "" {
		err = s.st.Pool().QueryRow(ctx, `SELECT count(*) FROM `+queueTbl+` WHERE state = 'queued'`).Scan(&p.Waiting)
		return p, err
	}
	err = s.sendQueued(ctx, set, &p)
	return p, err
}

// finishSent ends the titles sent: done once the package is, failed when the
// transcode or the package failed with no retry scheduled (the pipeline
// tries it no more), or the package was skipped, or the steps are gone. The
// reset of a sent title left its package waiting, so a package done since is
// the one it was sent for.
func (s *Service) finishSent(ctx context.Context) (done, failed int, err error) {
	rows, err := s.st.Pool().Query(ctx, `WITH ends AS (
			SELECT q.id,
				CASE
					WHEN p.status = 'done' THEN NULL
					WHEN t.status = 'failed' AND t.nextretryat IS NULL THEN
						'transcode failed: ' || COALESCE(t.error, t.lasterror, 'no error given')
					WHEN p.status = 'failed' AND p.nextretryat IS NULL THEN
						'package failed: ' || COALESCE(p.error, p.lasterror, 'no error given')
					WHEN p.status IN ('skipped', 'not_applicable') THEN
						'package ' || replace(p.status, '_', ' ') || COALESCE(': ' || p.error, '')
					ELSE 'its transcode and package are gone'
				END AS reason
			FROM `+queueTbl+` q
			LEFT JOIN `+tbl+` t ON t.item_id = q.item_id AND t.step = 'transcode'
			LEFT JOIN `+tbl+` p ON p.item_id = q.item_id AND p.step = 'package'
			WHERE q.state = 'sent'
			  AND (p.status IN ('done', 'skipped', 'not_applicable')
			       OR (t.status = 'failed' AND t.nextretryat IS NULL)
			       OR (p.status = 'failed' AND p.nextretryat IS NULL)
			       OR (t.id IS NULL AND p.id IS NULL))
			FOR UPDATE OF q SKIP LOCKED)
		UPDATE `+queueTbl+` q SET state = CASE WHEN ends.reason IS NULL THEN 'done' ELSE 'failed' END,
			doneat = now(), reason = left(ends.reason, 500)
		FROM ends WHERE q.id = ends.id
		RETURNING q.state`)
	if err != nil {
		return 0, 0, fmt.Errorf("end the titles sent: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var st string
		if err := rows.Scan(&st); err != nil {
			return done, failed, err
		}
		if st == QueueDone {
			done++
		} else {
			failed++
		}
	}
	return done, failed, rows.Err()
}

// sendQueued sends the titles queued first, as many as set allows now.
func (s *Service) sendQueued(ctx context.Context, set library.Settings, p *QueuePass) error {
	pool := s.st.Pool()
	var inflight int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM `+queueTbl+` WHERE state = 'sent'`).Scan(&inflight); err != nil {
		return err
	}
	budget := min(set.ReencodeRate, set.ReencodeInflight-inflight)
	if budget <= 0 {
		return nil
	}
	type row struct{ id, item string }
	var queued []row
	rows, err := pool.Query(ctx, `SELECT id, item_id FROM `+queueTbl+` WHERE state = 'queued' ORDER BY seq LIMIT $1`, batch)
	if err != nil {
		return err
	}
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.item); err != nil {
			rows.Close()
			return err
		}
		queued = append(queued, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if len(queued) == 0 {
		return nil
	}
	items := make([]string, len(queued))
	for i, r := range queued {
		items[i] = r.item
	}
	why, err := s.unencodable(ctx, items)
	if err != nil {
		return err
	}
	types, err := s.typesOf(ctx, items)
	if err != nil {
		return err
	}

	// No title reset is left without its event, nor its row unmarked: an
	// error ends the walk, and what was reset goes.
	ctx = context.WithoutCancel(ctx)
	rowOf := map[string]string{}
	var reset []claimed
	var failed error
	for _, r := range queued {
		if len(reset) == budget {
			break
		}
		if w := why[r.item]; w != "" {
			if failed = s.markQueued(ctx, r.id, QueueFailed, w); failed != nil {
				break
			}
			p.Refused++
			continue
		}
		tr, busy, err := s.reset(ctx, r.item)
		if err != nil {
			failed = fmt.Errorf("re-encode %s: %w", r.item, err)
			break
		}
		if busy != "" {
			if failed = s.markQueued(ctx, r.id, QueueQueued, busy); failed != nil {
				break
			}
			p.Busy++
			continue
		}
		rowOf[r.item] = r.id
		reset = append(reset, claimed{id: tr, itemID: r.item, step: "transcode", itemType: types[r.item]})
	}
	// A title whose event could not be sent is put back failed, with no retry
	// of its own: it stays queued, and the queue sends it again on a later
	// pass.
	_, _, notSent, first := s.dispatch(ctx, reset, false, asReencode)
	back := map[string]bool{}
	for _, c := range notSent {
		back[c.itemID] = true
	}
	var sent []string
	for _, c := range reset {
		if !back[c.itemID] {
			sent = append(sent, rowOf[c.itemID])
		}
	}
	errs := []error{failed}
	if len(sent) > 0 {
		if _, err := pool.Exec(ctx, `UPDATE `+queueTbl+` SET state = 'sent', sentat = now(), reason = NULL
			WHERE id = ANY($1) AND state = 'queued'`, sent); err != nil {
			errs = append(errs, fmt.Errorf("note the titles sent: %w", err))
		} else {
			p.Sent = len(sent)
		}
	}
	for _, c := range notSent {
		if err := s.markQueued(ctx, rowOf[c.itemID], QueueQueued, "the re-encode could not be sent: "+first.Error()); err != nil {
			errs = append(errs, err)
			break
		}
		p.NotSent++
	}
	return errors.Join(errs...)
}

// markQueued puts a queued title in state, saying why: one busy or not sent
// stays queued, one that cannot be encoded fails.
func (s *Service) markQueued(ctx context.Context, id, state, why string) error {
	_, err := s.st.Pool().Exec(ctx, `UPDATE `+queueTbl+` SET state = $2, reason = left($3, 500),
			doneat = CASE WHEN $4 THEN now() END
		WHERE id = $1 AND state = 'queued'`, id, state, why, state == QueueFailed)
	return err
}

// typesOf are the titles' types, as their events name them.
func (s *Service) typesOf(ctx context.Context, ids []string) (map[string]string, error) {
	rows, err := s.st.Pool().Query(ctx, `SELECT id, lower(type) FROM com_nalet_katalog_items WHERE id = ANY($1)`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id, typ string
		if err := rows.Scan(&id, &typ); err != nil {
			return nil, err
		}
		out[id] = typ
	}
	return out, rows.Err()
}

// sweepQueue is DrainQueue as Run calls it, saying what it did, and, while
// titles wait, why none is sent, once each time that changes.
func (s *Service) sweepQueue(ctx context.Context) {
	p, err := s.DrainQueue(ctx)
	if err != nil {
		log.Printf("retry: the re-encode queue: %v", err)
	}
	if did := p.String(); did != "" {
		log.Printf("retry: the re-encode queue: %s", did)
	}
	idle := p.Idle
	if p.Waiting == 0 {
		idle = ""
	}
	if idle != s.queueSaid {
		if idle != "" {
			log.Printf("retry: the re-encode queue waits, %d titles queued: %s", p.Waiting, idle)
		}
		s.queueSaid = idle
	}
}
