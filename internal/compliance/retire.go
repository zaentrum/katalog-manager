package compliance

import (
	"context"
	"strings"
	"time"

	"github.com/zaentrum/katalog-manager/internal/store"
)

// Passed says of each title of ids whether the library's compliance check
// found it within every rule, as an admin's list of originals to retire asks
// before it retires one (library.RetireList): "" of a title it did, else why
// not, as the clause a sentence says after a colon. It did of a title it
// checked against the rules both workers report now (its row's hash of them,
// with the library's layout), of its current version (the records it read
// were that version's, read once the version was complete), since the
// nightly check last fell due, that was not marked to be checked again since
// (its package completed, a step of it ended), whose records it read, that it
// could hold against every rule in force (uncheckedRules), that breaks none,
// and that processing again can help.
func (s *Service) Passed(ctx context.Context, ids []string) (map[string]string, error) {
	out := make(map[string]string, len(ids))
	if why := s.ready(ctx); why != nil {
		for _, id := range ids {
			out[id] = "the library’s compliance isn’t checked: " + why.Error()
		}
		return out, nil
	}
	rs, err := s.state(ctx)
	if err != nil {
		return nil, err
	}
	cutoff, _, err := s.nightlyCutoff(ctx)
	if err != nil {
		return nil, err
	}
	unreported := ""
	switch {
	case rs.transcoder == nil || rs.transcoder.t == nil:
		unreported = "the transcoder hasn’t reported its rules, so the compliance check can’t tell whether it is within them"
	case rs.packager == nil || rs.packager.p == nil:
		unreported = "the packager hasn’t reported its rules, so the compliance check can’t tell whether it is within them"
	}
	rows, err := s.pool.Query(ctx, `SELECT x.id, c.item_id IS NOT NULL, COALESCE(c.ruleshash = $2, false),
			c.staleat IS NOT NULL, c.unchecked, c.blocked, COALESCE(c.rules, '{}'), COALESCE(c.uncheckedrules, '{}'), c.evaluatedat,
			v.id, COALESCE(position(('v:' || v.id || '@') IN c.recordkey) > 0 AND c.evaluatedat >= v.completedat, false)
		FROM unnest($1::text[]) AS x(id)
		LEFT JOIN `+store.ComplianceTable+` c ON c.item_id = x.id
		LEFT JOIN com_nalet_katalog_itemversions v ON v.item_id = x.id AND v.state = 'complete'`, ids, rs.hash())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var checked, current, stale, ofVersion bool
		var unchecked, blocked, version *string
		var rules, uncheckedRules []string
		var at *time.Time
		if err := rows.Scan(&id, &checked, &current, &stale, &unchecked, &blocked, &rules, &uncheckedRules, &at, &version,
			&ofVersion); err != nil {
			return nil, err
		}
		switch {
		case unreported != "":
			out[id] = unreported
		case !checked:
			out[id] = "the compliance check hasn’t checked it"
		case !current:
			out[id] = "the compliance check checked it against other rules than the workers report now: it waits to be " +
				"checked again"
		case stale:
			out[id] = "it changed since the compliance check checked it: it waits to be checked again"
		case version != nil && !ofVersion:
			out[id] = "the compliance check checked it before its version " + *version + " was complete: it waits to be " +
				"checked again"
		case !cutoff.IsZero() && at != nil && at.Before(cutoff):
			out[id] = "the compliance check last checked it before its nightly check fell due: it waits to be checked again"
		case unchecked != nil:
			out[id] = "the compliance check couldn’t read its records: " + *unchecked
		case len(uncheckedRules) > 0:
			out[id] = "the compliance check couldn’t hold it against " + strings.Join(uncheckedRules, ", ") +
				": what that takes of it isn’t known"
		case blocked != nil:
			out[id] = "the compliance check found that processing it again can’t help: " + skipReason(*blocked)
		case len(rules) > 0:
			out[id] = "the compliance check finds it breaks " + strings.Join(rules, ", ")
		default:
			out[id] = ""
		}
	}
	return out, rows.Err()
}
