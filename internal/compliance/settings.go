package compliance

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/zaentrum/katalog-manager/internal/library"
)

// The check's settings, read from the settings table on every pass, as the
// library's are: an admin changes them in the catalog's settings, and one
// that sets none keeps the defaults.
const (
	// SettingPass is on (the default) or off: whether the background pass
	// checks the titles that need it. Off, the report keeps what the last
	// pass found, and the titles marked to be checked again wait.
	SettingPass = "compliance.pass"
	// SettingReaders is how many records the pass reads at once (4).
	SettingReaders = "compliance.readers"
	// SettingNightly is when every title is checked again each day,
	// "HH:MM" in the service's local time (03:00), or off: from the facts
	// the check keeps of their records, reading again only those of the
	// versions and the originals that changed.
	SettingNightly = "compliance.nightly"
)

// The settings' defaults.
const (
	defaultReaders = 4
	maxReaders     = 64
)

// defaultNightly is when the nightly check falls due, after midnight.
const defaultNightly = 3 * time.Hour

// passSettings are the check's settings as they are read: whether the pass
// runs, how many records it reads at once, and when the nightly check
// falls due after midnight (nightlyOff: never); problems says of each
// setting that could not be read what it held and what is used instead.
type passSettings struct {
	on         bool
	readers    int
	nightly    time.Duration
	nightlyOff bool
	problems   []string
}

var clockRE = regexp.MustCompile(`^([01][0-9]|2[0-3]):([0-5][0-9])$`)

// readPassSettings reads the check's settings: each key without the spaces
// around it, of a key the table holds twice the row of the lowest id, as the
// library's settings are read.
func readPassSettings(ctx context.Context, q library.Querier) (passSettings, error) {
	s := passSettings{on: true, readers: defaultReaders, nightly: defaultNightly}
	rows, err := q.Query(ctx, `SELECT DISTINCT ON (btrim(key)) btrim(key), valuetext FROM com_nalet_katalog_settings
		WHERE btrim(key) LIKE 'compliance.%' ORDER BY btrim(key), id`)
	if err != nil {
		return s, err
	}
	defer rows.Close()
	for rows.Next() {
		var key string
		var value *string
		if err := rows.Scan(&key, &value); err != nil {
			return s, err
		}
		if value != nil {
			s.set(key, *value)
		}
	}
	return s, rows.Err()
}

// set takes the value of one setting.
func (s *passSettings) set(key, value string) {
	v := strings.ToLower(strings.TrimSpace(value))
	if v == "" {
		return
	}
	bad := func(what string) {
		s.problems = append(s.problems, fmt.Sprintf("%s is %q, which is not %s", key, value, what))
	}
	switch key {
	case SettingPass:
		switch v {
		case "on", "true":
			s.on = true
		case "off", "false":
			s.on = false
		default:
			bad("on or off; the pass runs")
		}
	case SettingReaders:
		if n, err := strconv.Atoi(v); err == nil && n >= 1 && n <= maxReaders {
			s.readers = n
		} else {
			bad(fmt.Sprintf("a number of records from 1 to %d; it is %d", maxReaders, defaultReaders))
		}
	case SettingNightly:
		if v == "off" {
			s.nightlyOff = true
			return
		}
		m := clockRE.FindStringSubmatch(v)
		if m == nil {
			bad("HH:MM, as 03:00, or off; it is 03:00")
			return
		}
		h, _ := strconv.Atoi(m[1])
		mi, _ := strconv.Atoi(m[2])
		s.nightly = time.Duration(h)*time.Hour + time.Duration(mi)*time.Minute
	}
}

// cutoff is when the nightly check last fell due at now: the last nightly
// moment of the service's local time at or before now, a title checked
// before it checked again; the zero time when it is off.
func (s passSettings) cutoff(now time.Time) time.Time {
	if s.nightlyOff {
		return time.Time{}
	}
	l := now.Local()
	at := time.Date(l.Year(), l.Month(), l.Day(), 0, 0, 0, 0, l.Location()).Add(s.nightly)
	if at.After(l) {
		at = at.AddDate(0, 0, -1)
	}
	return at
}

// clock is the database's now, which the check's moments are of: a row's
// stamp, the nightly check's last moment.
func (s *Service) clock(ctx context.Context) (time.Time, error) {
	var now time.Time
	err := s.pool.QueryRow(ctx, `SELECT now()`).Scan(&now)
	return now, err
}

// nightlyCutoff is when the nightly check last fell due, by the database's
// clock and the check's settings.
func (s *Service) nightlyCutoff(ctx context.Context) (time.Time, passSettings, error) {
	set, err := readPassSettings(ctx, s.pool)
	if err != nil {
		return time.Time{}, set, err
	}
	now, err := s.clock(ctx)
	if err != nil {
		return time.Time{}, set, err
	}
	return set.cutoff(now), set, nil
}
