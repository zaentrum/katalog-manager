package retry

import (
	"context"
	"fmt"
	"log"
)

// ExtraJobs are the sweep's jobs for the titles' extras (extras.Service):
// taking the extras stuck in their packaging for failed runs, sending the
// triggers due, and deleting the packages of the extras removed a day ago.
type ExtraJobs interface {
	Reap(ctx context.Context) (int, error)
	SendDue(ctx context.Context) (int, error)
	DeleteRemovedPackages(ctx context.Context) (int, error)
}

// WithExtras has the sweep run x's jobs too, every interval, by the same
// policy as the steps' retries. They run whatever the steps' retries can do
// (migration 033 is the steps'); without an event bus the extras wait, and
// only the removed packages are deleted.
func (s *Service) WithExtras(x ExtraJobs) *Service {
	s.extras = x
	return s
}

// SweepExtras runs the extras' jobs once, in that order: an extra the
// reaper puts back is due a backoff later, a retry due now goes, and a
// removed extra's package goes once its grace is over. A job that fails is
// said in the error, and the others run.
func (s *Service) SweepExtras(ctx context.Context) (reaped, sent, deleted int, err error) {
	if s.extras == nil {
		return 0, 0, 0, nil
	}
	var errs []error
	if reaped, err = s.extras.Reap(ctx); err != nil {
		errs = append(errs, err)
	}
	if sent, err = s.extras.SendDue(ctx); err != nil {
		errs = append(errs, err)
	}
	if deleted, err = s.extras.DeleteRemovedPackages(ctx); err != nil {
		errs = append(errs, err)
	}
	if len(errs) > 0 {
		return reaped, sent, deleted, fmt.Errorf("the extras' sweep: %v", errs)
	}
	return reaped, sent, deleted, nil
}

// sweepExtras is SweepExtras as Run calls it, saying what it did.
func (s *Service) sweepExtras(ctx context.Context) {
	reaped, sent, deleted, err := s.SweepExtras(ctx)
	if err != nil {
		log.Printf("retry: %v", err)
	}
	if reaped+sent+deleted > 0 {
		log.Printf("retry: extras: %d stuck reaped, %d triggers sent, %d removed extras' packages deleted", reaped, sent, deleted)
	}
}

// LibraryJobs are the library's jobs (library.Retirer): with library.layout
// v2 they delete an original after packaging, a superseded version after its
// grace, and what the trash and the legacy folder hold after theirs. Sweep
// runs them when a minute has passed since they last ran, and says what
// they did ("" when nothing).
type LibraryJobs interface {
	Sweep(ctx context.Context) (string, error)
}

// WithLibrary has the sweep run the library's jobs too: on each of its
// rounds, which runs them once a minute; with no sweep, once a minute by
// themselves. They need no event bus.
func (s *Service) WithLibrary(l LibraryJobs) *Service {
	s.library = l
	return s
}

// sweepLibrary runs the library's jobs, saying what they did.
func (s *Service) sweepLibrary(ctx context.Context) {
	if s.library == nil {
		return
	}
	did, err := s.library.Sweep(ctx)
	if err != nil {
		log.Printf("retry: the library's jobs: %v", err)
	}
	if did != "" {
		log.Printf("retry: the library's jobs: %s", did)
	}
}
