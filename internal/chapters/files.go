package chapters

// The library's files the catalog reads for the chapters — a source record's
// ffprobe.json and source.json, an original's place on disk — lie on a
// storage that may not answer for a while (a busy NAS, an NFS mount that
// hangs), and a read of one can't be called off once it is started. So each
// runs in a goroutine of its own and is waited for FileWait at most: one not
// answered by then is left to finish on its own, keeping one of the process's
// fileSlots until it does, and a read asked for while every slot is kept is
// not started at all. A claim starts no read once one of its reads was not
// answered in time, nor past its budget (claimBudget): it answers in time
// whatever the storage does, and what it could not read waits a backoff,
// which counts as no failure of its title.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/zaentrum/katalog-manager/internal/library"
)

// FileWait is how long a read of the library's files is waited for.
var FileWait = 5 * time.Second

// claimBudget is how long a claim takes at most, whatever the storage does:
// its record step ends after recordsFor, its hand-out by claimBudget.
var claimBudget = 20 * time.Second

// fileSlots is how many reads of the library's files run at once in the
// process at most: so many are left behind, at most, by reads not answered in
// time.
const fileSlots = 2

var slots = make(chan struct{}, fileSlots)

// ErrStorageSlow says a read of the library's files was started and not
// answered within its wait (it goes on, and its slot with it, until the
// storage answers); ErrStorageBusy that a read was not started, the reads
// before it not answered yet (every slot kept, or the claim's own read before
// it); errNoTime that it was not started, the claim's budget being spent.
var (
	ErrStorageSlow = errors.New("the library's storage did not answer in time")
	ErrStorageBusy = errors.New("the library's storage has not answered the reads before this one yet")
	errNoTime      = errors.New("the claim's time is up")
)

// The library's files as the claim looks at them: os.Stat and os.Lstat (a
// test's stand in for a storage that does not answer).
var (
	statFile  = os.Stat
	lstatFile = os.Lstat
)

// unread reports whether err says a read of the library's files was not
// answered or not started (ErrStorageSlow, ErrStorageBusy, errNoTime): the
// storage said nothing of the file.
func unread(err error) bool {
	return errors.Is(err, ErrStorageSlow) || errors.Is(err, ErrStorageBusy) || errors.Is(err, errNoTime)
}

// reads are a claim's reads of the library's files: once one was not
// answered in time, or not started for the reads before it (stalled: why),
// it starts no other.
type reads struct{ stalled error }

// read is f, a read of the library's files, run in a goroutine of its own
// and waited for FileWait at most: what it answered, or why it did not
// (ErrStorageSlow: f goes on, keeping its slot until it returns;
// ErrStorageBusy, errNoTime: f was not started). It is started only when its
// whole wait ends by until (the zero time: any time). rs are the reads of
// the claim it is one of (nil: of none).
func read[T any](ctx context.Context, rs *reads, until time.Time, f func() T) (T, error) {
	var zero T
	if rs != nil && rs.stalled != nil {
		return zero, fmt.Errorf("%w: a read of this claim before it was not answered", ErrStorageBusy)
	}
	wait := FileWait
	if !until.IsZero() && time.Until(until) < wait {
		return zero, errNoTime
	}
	select {
	case slots <- struct{}{}:
	default:
		err := fmt.Errorf("%w: %d reads not answered", ErrStorageBusy, fileSlots)
		if rs != nil {
			rs.stalled = err
		}
		return zero, err
	}
	out := make(chan T, 1)
	go func() {
		defer func() { <-slots }()
		out <- f()
	}()
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case v := <-out:
		return v, nil
	case <-t.C:
		err := fmt.Errorf("%w (%s)", ErrStorageSlow, library.DurationText(wait.Round(time.Millisecond)))
		if rs != nil {
			rs.stalled = err
		}
		return zero, err
	case <-ctx.Done():
		if rs != nil {
			rs.stalled = ctx.Err()
		}
		return zero, ctx.Err()
	}
}
