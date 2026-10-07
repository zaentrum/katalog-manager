package library

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// The settings of the library, read from the settings table on every use, as
// extras.scan is: a deployment switches an environment by setting them, and
// one that sets none keeps the legacy layout and every original.
const (
	// SettingLayout is legacy (the default) or v2: with v2 the worker
	// records hand the workers the library's paths, the scanner walks
	// ARRIVALS_ROOT and the projector writes the record's projections.
	SettingLayout = "library.layout"
	// SettingOriginals is keep (the default) or delete-after-package: the
	// retire job's policy.
	SettingOriginals = "library.originals"
	// SettingRetireDelay is how long a version is complete at least before
	// the original it was made from is retired (10m).
	SettingRetireDelay = "library.retire.delay"
	// SettingRetireRate is how many originals the retire job retires a
	// minute at most (30).
	SettingRetireRate = "library.retire.rate"
	// SettingVerify is how a version is verified before its original is
	// retired: full (every file's hash, the default) or chain.
	SettingVerify = "library.verify"
	// SettingVerifyMaxAge is how recent a full verification may be for the
	// chain alone to do (30d).
	SettingVerifyMaxAge = "library.verify.maxAge"
	// SettingSupersededGrace is how long a superseded version stays before it
	// is removed (24h).
	SettingSupersededGrace = "library.superseded.grace"
	// SettingTrashGrace is how long a retired original stays in the trash
	// (24h); 0 unlinks it at once.
	SettingTrashGrace = "library.trash.grace"
	// SettingReencodeRate is how many titles of the re-encode queue the
	// sweep sends a pass at most (4); 0 sends none.
	SettingReencodeRate = "library.reencode.rate"
	// SettingReencodeInflight is how many titles of the queue may be sent and
	// not done at once (4): the sweep sends no more while they are encoded.
	SettingReencodeInflight = "library.reencode.inflight"
	// SettingReencodeWindow is when the queue is sent, "HH:MM-HH:MM" in the
	// service's local time ("23:00-07:00" crosses midnight); empty, any time.
	SettingReencodeWindow = "library.reencode.window"
)

// The values of the settings.
const (
	LayoutLegacy     = "legacy"
	LayoutV2         = "v2"
	OriginalsKeep    = "keep"
	OriginalsDelete  = "delete-after-package"
	VerifyFull       = "full"
	VerifyChain      = "chain"
	defaultRetireNum = 30
	defaultReencode  = 4
)

// Settings are the library's settings as they are read.
type Settings struct {
	Layout          string
	Originals       string
	RetireDelay     time.Duration
	RetireRate      int
	Verify          string
	VerifyMaxAge    time.Duration
	SupersededGrace time.Duration
	TrashGrace      time.Duration
	// ReencodeRate, ReencodeInflight and ReencodeWindow are how the sweep
	// sends the re-encode queue (migration 043).
	ReencodeRate     int
	ReencodeInflight int
	ReencodeWindow   string
	// Problems says of each setting that could not be read what it held and
	// what is used instead.
	Problems []string
}

// Defaults are the settings of a catalog that sets none.
func Defaults() Settings {
	return Settings{Layout: LayoutLegacy, Originals: OriginalsKeep, RetireDelay: 10 * time.Minute,
		RetireRate: defaultRetireNum, Verify: VerifyFull, VerifyMaxAge: 30 * 24 * time.Hour,
		SupersededGrace: 24 * time.Hour, TrashGrace: 24 * time.Hour,
		ReencodeRate: defaultReencode, ReencodeInflight: defaultReencode}
}

// V2 reports whether the environment runs the v2 layout.
func (s Settings) V2() bool { return s.Layout == LayoutV2 }

// DeleteOriginals reports whether the retire job deletes originals.
func (s Settings) DeleteOriginals() bool { return s.Originals == OriginalsDelete }

// Querier reads rows: a pool or a transaction.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// ReadSettings reads the library's settings: each key without the spaces
// around it, and of a key the table holds twice the row of the lowest id, as
// the workers' settings route reads them. A value that cannot be read is the
// default, said in Problems; one that is not set is the default.
func ReadSettings(ctx context.Context, q Querier) (Settings, error) {
	s := Defaults()
	rows, err := q.Query(ctx, `SELECT DISTINCT ON (btrim(key)) btrim(key), valuetext FROM com_nalet_katalog_settings
		WHERE btrim(key) LIKE 'library.%' ORDER BY btrim(key), id`)
	if err != nil {
		return s, err
	}
	defer rows.Close()
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return s, err
		}
		s.set(key, value)
	}
	return s, rows.Err()
}

// set takes the value of one setting.
func (s *Settings) set(key, value string) {
	v := strings.TrimSpace(value)
	if v == "" {
		return
	}
	bad := func(what string) {
		s.Problems = append(s.Problems, fmt.Sprintf("%s is %q, which is not %s", key, value, what))
	}
	lower := strings.ToLower(v)
	switch key {
	case SettingLayout:
		if lower == LayoutLegacy || lower == LayoutV2 {
			s.Layout = lower
		} else {
			bad("legacy or v2; the layout is legacy")
		}
	case SettingOriginals:
		if lower == OriginalsKeep || lower == OriginalsDelete {
			s.Originals = lower
		} else {
			bad("keep or delete-after-package; every original is kept")
		}
	case SettingVerify:
		if lower == VerifyFull || lower == VerifyChain {
			s.Verify = lower
		} else {
			bad("full or chain; a version is verified in full")
		}
	case SettingRetireRate:
		if n, err := strconv.Atoi(v); err == nil && n >= 1 {
			s.RetireRate = n
		} else {
			bad(fmt.Sprintf("a number of originals a minute, 1 or more; it is %d", defaultRetireNum))
		}
	case SettingReencodeRate:
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			s.ReencodeRate = n
		} else {
			bad(fmt.Sprintf("a number of titles a pass, 0 or more; it is %d", defaultReencode))
		}
	case SettingReencodeInflight:
		if n, err := strconv.Atoi(v); err == nil && n >= 1 {
			s.ReencodeInflight = n
		} else {
			bad(fmt.Sprintf("a number of titles, 1 or more; it is %d", defaultReencode))
		}
	case SettingReencodeWindow:
		// kept as written: the sweep sends nothing of the queue in a window
		// it cannot read (ParseWindow), rather than at any time
		s.ReencodeWindow = v
		if _, err := ParseWindow(v); err != nil {
			bad("a window, HH:MM-HH:MM as 23:00-07:00; no title of the re-encode queue is sent until it is one")
		}
	case SettingRetireDelay, SettingVerifyMaxAge, SettingSupersededGrace, SettingTrashGrace:
		d, err := ParseDuration(v)
		if err != nil {
			bad("a duration, as 10m, 24h or 30d; it is its default")
			return
		}
		switch key {
		case SettingRetireDelay:
			s.RetireDelay = d
		case SettingVerifyMaxAge:
			s.VerifyMaxAge = d
		case SettingSupersededGrace:
			s.SupersededGrace = d
		case SettingTrashGrace:
			s.TrashGrace = d
		}
	}
}

var daysRE = regexp.MustCompile(`^(\d+)d(.*)$`)

// ParseDuration reads a duration as the settings write one: a Go duration
// ("90s", "10m", "24h"), days ("30d", "1d12h"), or 0. It is never negative.
func ParseDuration(v string) (time.Duration, error) {
	v = strings.ToLower(strings.TrimSpace(v))
	if v == "0" {
		return 0, nil
	}
	var d time.Duration
	if m := daysRE.FindStringSubmatch(v); m != nil {
		n, err := strconv.Atoi(m[1])
		if err != nil || n > 100000 {
			return 0, fmt.Errorf("%q is no duration", v)
		}
		d, v = time.Duration(n)*24*time.Hour, m[2]
		if v == "" {
			return d, nil
		}
	}
	rest, err := time.ParseDuration(v)
	if err != nil || rest < 0 {
		return 0, fmt.Errorf("%q is no duration", v)
	}
	return d + rest, nil
}

// Window is a daily span of the service's local time: from its start (in)
// to its end (out), which may be past midnight. A zero Window is any time.
type Window struct {
	set        bool
	start, end int // minutes since midnight
}

var windowRE = regexp.MustCompile(`^([01]\d|2[0-3]):([0-5]\d)-([01]\d|2[0-3]):([0-5]\d)$`)

// ParseWindow reads a window as library.reencode.window writes one,
// "HH:MM-HH:MM"; "" is any time. A window whose start is its end is the whole
// day.
func ParseWindow(v string) (Window, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return Window{}, nil
	}
	m := windowRE.FindStringSubmatch(strings.ReplaceAll(v, " ", ""))
	if m == nil {
		return Window{}, fmt.Errorf("%q is no window: HH:MM-HH:MM, as 23:00-07:00", v)
	}
	at := func(h, mm string) int {
		hh, _ := strconv.Atoi(h)
		mi, _ := strconv.Atoi(mm)
		return hh*60 + mi
	}
	return Window{set: true, start: at(m[1], m[2]), end: at(m[3], m[4])}, nil
}

// Contains reports whether t, in the service's local time, lies in w.
func (w Window) Contains(t time.Time) bool {
	if !w.set || w.start == w.end {
		return true
	}
	l := t.Local()
	now := l.Hour()*60 + l.Minute()
	if w.start < w.end {
		return now >= w.start && now < w.end
	}
	return now >= w.start || now < w.end
}
