package processing

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

// The backoff doubles with every failure in a row, from Backoff, and never
// exceeds BackoffMax; a step is retried by itself until its failures reach
// MaxAttempts.
func TestThePolicysBackoffAndAttempts(t *testing.T) {
	p := Policy{MaxAttempts: 5, Backoff: time.Minute, BackoffMax: 5 * time.Minute}
	for n, want := range map[int]time.Duration{1: time.Minute, 2: 2 * time.Minute, 3: 4 * time.Minute, 4: 5 * time.Minute, 9: 5 * time.Minute, 60: 5 * time.Minute} {
		if got := p.Delay(n); got != want {
			t.Errorf("Delay(%d) = %v, want %v", n, got, want)
		}
	}
	for n, want := range map[int]bool{0: false, 1: true, 4: true, 5: false, 6: false} {
		if got := p.Retries(n); got != want {
			t.Errorf("Retries(%d) = %v, want %v", n, got, want)
		}
	}
	if got := p.delays(); !reflect.DeepEqual(got, []float64{60, 120, 240, 300}) {
		t.Errorf("delays = %v, want a delay for failures 1 to 4", got)
	}
	for _, max := range []int{1, 0, -1} {
		if got := (Policy{MaxAttempts: max, Backoff: time.Minute, BackoffMax: time.Hour}).delays(); len(got) != 0 {
			t.Errorf("MaxAttempts %d retries %v by itself, want nothing", max, got)
		}
	}
	d := DefaultPolicy()
	if d.MaxAttempts != 3 || !reflect.DeepEqual(d.delays(), []float64{60, 120}) || d.BackoffMax != time.Hour {
		t.Errorf("the default policy: %+v, delays %v", d, d.delays())
	}
	d.Timeouts["transcode"] = time.Second
	if DefaultTimeouts["transcode"] != 6*time.Hour {
		t.Error("a policy's timeouts are its own: changing them changed the defaults")
	}
}

// A step's timeout is its own, or the default; every step has one.
func TestThePolicysTimeouts(t *testing.T) {
	p := DefaultPolicy()
	for step, want := range map[string]time.Duration{"transcode": 6 * time.Hour, "package": 2 * time.Hour,
		"tmdb": 15 * time.Minute, "subtitle": 2 * time.Hour, "elsewhere": 2 * time.Hour} {
		if got := p.Timeout(step); got != want {
			t.Errorf("Timeout(%s) = %v, want %v", step, got, want)
		}
	}
	p.Timeouts = map[string]time.Duration{"transcode": 12 * time.Hour, "package": 0}
	p.DefaultTimeout = time.Hour
	steps, secs := p.timeoutTable()
	if len(steps) != len(StepOrder) || len(secs) != len(steps) {
		t.Fatalf("timeoutTable: %v %v", steps, secs)
	}
	for i, s := range steps {
		want := 3600.0
		if s == "transcode" {
			want = 12 * 3600
		}
		if secs[i] != want {
			t.Errorf("timeoutTable: %s %v, want %v", s, secs[i], want)
		}
	}
}

func TestParseTimeouts(t *testing.T) {
	got, err := ParseTimeouts(" transcode=12h, package = 90m ,,")
	if err != nil || !reflect.DeepEqual(got, map[string]time.Duration{"transcode": 12 * time.Hour, "package": 90 * time.Minute}) {
		t.Errorf("ParseTimeouts: %v, %v", got, err)
	}
	if got, err := ParseTimeouts(""); err != nil || len(got) != 0 {
		t.Errorf("ParseTimeouts of nothing: %v, %v", got, err)
	}
	for in, says := range map[string]string{
		"transcode":       "is not a step's timeout",
		"encode=1h":       "is not a step's timeout",
		"transcode=soon":  "a duration above zero",
		"transcode=0s":    "a duration above zero",
		"transcode=-1h":   "a duration above zero",
		"=1h":             "is not a step's timeout",
		"package=1h,x=2h": `"x=2h" is not a step's timeout`,
	} {
		if _, err := ParseTimeouts(in); err == nil || !strings.Contains(err.Error(), says) {
			t.Errorf("ParseTimeouts(%q): %v, want it refused saying %q", in, err, says)
		}
	}
}
