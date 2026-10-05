package extras

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/zaentrum/katalog-manager/internal/events"
	"github.com/zaentrum/katalog-manager/internal/store"
)

// Send sends the trigger of each extra of ids that waits to be sent and is
// due (all that are, with ids nil): it is queued, its trigger noted as sent,
// and catalog.extra.queued goes, keyed by the extra. The trigger of an extra
// that failed before is a retry (status and source "retry"); any other's is
// "queued", from source (its own way in when source is ""). An extra whose
// trigger could not be sent is put back, pending, and sent again a backoff
// later. Without an event bus nothing is sent, and the extras wait. It
// returns the triggers sent and those that could not be.
func (s *Service) Send(ctx context.Context, ids []string, source string) (sent, notSent int, err error) {
	if (ids != nil && len(ids) == 0) || !s.bus() {
		return 0, 0, nil
	}
	for {
		claimed, err := s.st.ClaimDueExtras(ctx, ids, batch)
		if errors.Is(err, store.ErrNoExtras) {
			return sent, notSent, nil
		}
		if err != nil {
			return sent, notSent, fmt.Errorf("claim the extras to send: %w", err)
		}
		msgs := make([]events.ExtraMessage, len(claimed))
		for i, c := range claimed {
			ev := events.NewExtraEvent(c.ID, c.ItemID, c.Kind)
			switch {
			case c.Failures > 0:
				ev.Status, ev.Source = "retry", "retry"
			case source != "":
				ev.Source = source
			default:
				ev.Source = c.RegisteredBy
			}
			msgs[i] = events.ExtraMessage{Topic: events.TopicExtraQueued, Event: ev}
		}
		n, failed := s.publish(ctx, msgs)
		sent += n
		notSent += len(failed)
		if len(failed) > 0 {
			var first error
			var back []string
			for _, f := range failed {
				back = append(back, f.id)
				if first == nil {
					first = f.err
				}
			}
			if err := s.st.PutBackExtras(context.WithoutCancel(ctx), back, "its trigger could not be sent: "+first.Error(),
				s.pol.Backoff); err != nil {
				log.Printf("extras: %d extras whose trigger could not be sent could not be put back either: %v", len(back), err)
			}
		}
		if ids != nil || len(claimed) < batch || len(failed) > 0 || ctx.Err() != nil {
			return sent, notSent, ctx.Err()
		}
	}
}

type failedSend struct {
	id  string
	err error
}

// publish sends msgs, and says how many went and which did not.
func (s *Service) publish(ctx context.Context, msgs []events.ExtraMessage) (int, []failedSend) {
	if len(msgs) == 0 {
		return 0, nil
	}
	errs := s.pub.PublishExtras(ctx, msgs)
	sent := 0
	var failed []failedSend
	for i, err := range errs {
		if err != nil {
			failed = append(failed, failedSend{id: msgs[i].Event.ExtraID, err: err})
			continue
		}
		sent++
	}
	return sent, failed
}

// SendDue sends every extra due to be sent (Send): those taken in or
// packaged again whose trigger has not gone, and the retries due.
func (s *Service) SendDue(ctx context.Context) (int, error) {
	n, _, err := s.Send(ctx, nil, "")
	return n, err
}

// Reap takes the extras stuck in their packaging for failed runs
// (store.ReapExtras, WaitTimeout and SilentTimeout): one waiting for a worker
// that none started, one whose worker has gone silent. A transcoded one with
// an attempt left stays transcoded, and the transcoder is sent a trigger that
// is no retry, which has it announce the transcode again, so a packager that
// missed it starts; any other with an attempt left is put back to pending, to
// be sent again a backoff later, which runs its chain again from the
// transcode; one without is failed. Without an event bus nothing is reaped.
// It returns how many it took.
func (s *Service) Reap(ctx context.Context) (int, error) {
	if !s.bus() {
		return 0, nil
	}
	t := store.ExtraTimeouts{Wait: WaitTimeout, Silent: SilentTimeout, WaitLabel: label(WaitTimeout), SilentLabel: label(SilentTimeout)}
	reaped := 0
	for {
		rs, err := s.st.ReapExtras(ctx, t, s.pol, batch)
		if errors.Is(err, store.ErrNoExtras) {
			return reaped, nil
		}
		if err != nil {
			return reaped, fmt.Errorf("reap the stuck extras: %w", err)
		}
		reaped += len(rs)
		var msgs []events.ExtraMessage
		for _, r := range rs {
			if r.Was == "transcoded" && r.State == "transcoded" {
				ev := events.NewExtraEvent(r.ID, r.ItemID, r.Kind)
				ev.Source = "reaper"
				msgs = append(msgs, events.ExtraMessage{Topic: events.TopicExtraQueued, Event: ev})
			}
		}
		_, failed := s.publish(ctx, msgs)
		if len(failed) > 0 {
			var back []string
			for _, f := range failed {
				back = append(back, f.id)
			}
			if err := s.st.UnreapExtras(context.WithoutCancel(ctx), back, "its transcode could not be announced again: "+
				failed[0].err.Error()); err != nil {
				log.Printf("extras: %d transcoded extras whose trigger could not be sent could not be put back either: %v", len(back), err)
			}
		}
		// Those put back are due again at once: the next sweep takes them.
		if len(rs) < batch || len(failed) > 0 || ctx.Err() != nil {
			return reaped, ctx.Err()
		}
	}
}

// DeleteRemovedPackages deletes what the package store holds of the extras
// removed RemovedGrace ago or longer: the package, the packages it replaced
// and kept for their grace (<id>.old-<stamp>), and the transcoder's handoff
// left in the inbox (_inbox/extra-<id>/). Each is deleted once; one that
// could not be is tried again an hour later. It needs no event bus, and
// returns how many extras' leftovers it deleted.
func (s *Service) DeleteRemovedPackages(ctx context.Context) (int, error) {
	deleted := 0
	for {
		due, err := s.st.ClaimRemovedPackages(ctx, batch)
		if errors.Is(err, store.ErrNoExtras) {
			return deleted, nil
		}
		if err != nil {
			return deleted, fmt.Errorf("claim the removed extras' packages: %w", err)
		}
		var done, back []string
		var why string
		for _, p := range due {
			if err := s.deleteLeftovers(p); err != nil {
				back = append(back, p.ID)
				if why == "" {
					why = err.Error()
				}
				log.Printf("extras: the package of removed extra %s could not be deleted: %v", p.ID, err)
				continue
			}
			done = append(done, p.ID)
		}
		bg := context.WithoutCancel(ctx)
		if err := s.st.RemovedPackagesDeleted(bg, done); err != nil {
			return deleted, err
		}
		if err := s.st.PutBackRemovedPackages(bg, back, "its package could not be deleted: "+why, time.Hour); err != nil {
			return deleted, err
		}
		deleted += len(done)
		if len(due) < batch || ctx.Err() != nil {
			return deleted, ctx.Err()
		}
	}
}

// deleteLeftovers deletes the package of the removed extra p, the ones it
// replaced and kept for their grace, and its handoff in the inbox, each only
// inside the package store.
func (s *Service) deleteLeftovers(p store.RemovedPackage) error {
	root := s.cfg.PackagesRoot
	dir := packageDir(root, p.ID)
	targets := []string{dir, inboxDir(root, p.ID)}
	if p.PackagePath != nil && filepath.Clean(*p.PackagePath) != dir {
		targets = append(targets, filepath.Clean(*p.PackagePath))
	}
	if olds, err := filepath.Glob(dir + ".old-*"); err == nil {
		targets = append(targets, olds...)
	}
	for _, t := range targets {
		if !within(filepath.Join(root, "extras"), t) && !within(filepath.Join(root, "_inbox"), t) {
			continue // never anything outside the extras' part of the store
		}
		if err := os.RemoveAll(t); err != nil {
			return err
		}
	}
	// The shard folder goes when it is empty.
	if shard := filepath.Dir(dir); strings.HasPrefix(shard, filepath.Join(root, "extras")+string(filepath.Separator)) {
		_ = os.Remove(shard)
	}
	return nil
}
