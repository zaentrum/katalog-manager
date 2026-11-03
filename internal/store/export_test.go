package store

import "time"

// SetStyledLockWaits makes, for a test, migration 053 wait for each lock no
// longer than timeout, attempts times, pause apart; it answers what sets them
// back.
func SetStyledLockWaits(timeout string, attempts int, pause time.Duration) func() {
	t, a, p := styledLockTimeout, styledAttempts, styledPause
	styledLockTimeout, styledAttempts, styledPause = timeout, attempts, pause
	return func() { styledLockTimeout, styledAttempts, styledPause = t, a, p }
}
