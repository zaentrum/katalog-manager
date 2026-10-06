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
	// Problems says of each setting that could not be read what it held and
	// what is used instead.
	Problems []string
}

// Defaults are the settings of a catalog that sets none.
func Defaults() Settings {
	return Settings{Layout: LayoutLegacy, Originals: OriginalsKeep, RetireDelay: 10 * time.Minute,
		RetireRate: defaultRetireNum, Verify: VerifyFull, VerifyMaxAge: 30 * 24 * time.Hour,
		SupersededGrace: 24 * time.Hour, TrashGrace: 24 * time.Hour}
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
