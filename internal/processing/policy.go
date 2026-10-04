package processing

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Policy is how the service retries a step that fails, and how long a step
// may be in progress without a word from its worker.
//
// A step that fails is retried by itself after a backoff: Backoff after its
// first failure, doubled with every failure in a row after that, at most
// BackoffMax. It gets MaxAttempts runs in a row, its first included; a step
// whose failures reach MaxAttempts stays failed, for an admin to retry. A step
// in progress whose worker has been silent for longer than its timeout is
// taken for a failed run, and retried the same way.
type Policy struct {
	// MaxAttempts is the runs a step gets in a row before the service stops
	// retrying it by itself. 1 (or less) retries nothing by itself.
	MaxAttempts int
	// Backoff is the wait before the first retry, BackoffMax the longest.
	Backoff, BackoffMax time.Duration
	// Timeouts are how long a step may be in progress without a word from
	// its worker, by step; DefaultTimeout for a step that has none.
	Timeouts       map[string]time.Duration
	DefaultTimeout time.Duration
}

// DefaultTimeouts are the steps' timeouts by default: generous, as a step is
// timed from its worker's last word, and the workers report only when a run
// starts and when it ends. A transcode of a long film on a CPU takes hours.
var DefaultTimeouts = map[string]time.Duration{
	"scan":        15 * time.Minute,
	"tmdb":        15 * time.Minute,
	"tidb":        2 * time.Hour,
	"chapter":     2 * time.Hour,
	"chromaprint": 2 * time.Hour,
	"blackframe":  2 * time.Hour,
	"silence":     2 * time.Hour,
	"subtitle":    2 * time.Hour,
	"transcode":   6 * time.Hour,
	"package":     2 * time.Hour,
}

// DefaultPolicy retries a failed step twice, a minute and then two after its
// failures (three runs in a row), never waits longer than an hour, and times
// steps out by DefaultTimeouts.
func DefaultPolicy() Policy {
	t := make(map[string]time.Duration, len(DefaultTimeouts))
	for k, v := range DefaultTimeouts {
		t[k] = v
	}
	return Policy{MaxAttempts: 3, Backoff: time.Minute, BackoffMax: time.Hour, Timeouts: t, DefaultTimeout: 2 * time.Hour}
}

// Delay is the wait before the retry that follows a step's failures-th
// failure in a row: Backoff doubled failures-1 times, at most BackoffMax.
func (p Policy) Delay(failures int) time.Duration {
	d := p.Backoff
	for i := 1; i < failures && d < p.BackoffMax; i++ {
		d *= 2
	}
	return min(d, p.BackoffMax)
}

// Retries reports whether a step that has failed failures times in a row is
// retried by itself.
func (p Policy) Retries(failures int) bool { return failures >= 1 && failures < p.MaxAttempts }

// delays are the seconds to wait after a step's 1st, 2nd, ... failure in a
// row, one for each failure that is retried by itself: the Upsert picks the
// one of a step's failures, and a failure beyond them schedules nothing.
func (p Policy) delays() []float64 {
	out := []float64{}
	for n := 1; p.Retries(n); n++ {
		out = append(out, p.Delay(n).Seconds())
	}
	return out
}

// Timeout is how long step may be in progress without a word from its
// worker.
func (p Policy) Timeout(step string) time.Duration {
	if d, ok := p.Timeouts[step]; ok && d > 0 {
		return d
	}
	return p.DefaultTimeout
}

// timeoutTable is every step's timeout, in seconds, as two parallel arrays
// for SQL (unnest), the default for any step not among them.
func (p Policy) timeoutTable() ([]string, []float64) {
	steps := make([]string, 0, len(validSteps))
	for s := range validSteps {
		steps = append(steps, s)
	}
	for s := range p.Timeouts {
		if !validSteps[s] {
			steps = append(steps, s)
		}
	}
	sort.Strings(steps)
	secs := make([]float64, len(steps))
	for i, s := range steps {
		secs[i] = p.Timeout(s).Seconds()
	}
	return steps, secs
}

// ParseTimeouts reads step timeouts written "transcode=12h,package=3h": each
// a step of the pipeline and a Go duration above zero.
func ParseTimeouts(v string) (map[string]time.Duration, error) {
	out := map[string]time.Duration{}
	for _, tok := range strings.Split(v, ",") {
		tok = strings.TrimSpace(tok)
		if tok == "" {
			continue
		}
		step, dur, ok := strings.Cut(tok, "=")
		step, dur = strings.TrimSpace(step), strings.TrimSpace(dur)
		if !ok || !validSteps[step] {
			return nil, fmt.Errorf("%q is not a step's timeout: a step (%s) and a duration, as transcode=12h",
				tok, strings.Join(StepOrder, ", "))
		}
		d, err := time.ParseDuration(dur)
		if err != nil || d <= 0 {
			return nil, fmt.Errorf("%q: the timeout of %s is a duration above zero, as 90m or 12h", tok, step)
		}
		out[step] = d
	}
	return out, nil
}
