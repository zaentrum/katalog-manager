package compliance

import (
	"context"
	"strings"

	"github.com/zaentrum/katalog-manager/internal/store"
)

// Passed says of each title of ids whether the library's compliance check
// found it within every rule, as an admin's list of originals to retire asks
// before it retires one (library.RetireList): "" of a title it did, else why
// not, as the clause a sentence says after a colon. It did of a title it
// checked against the rules both workers report now (its row's hash of them,
// with the library's layout), that was not marked to be checked again since
// (its package completed, a step of it ended), whose records it read, that
// breaks no rule, and that processing again can help.
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
	unreported := ""
	switch {
	case rs.transcoder == nil || rs.transcoder.t == nil:
		unreported = "the transcoder hasn’t reported its rules, so the compliance check can’t tell whether it is within them"
	case rs.packager == nil || rs.packager.p == nil:
		unreported = "the packager hasn’t reported its rules, so the compliance check can’t tell whether it is within them"
	}
	rows, err := s.pool.Query(ctx, `SELECT x.id, c.item_id IS NOT NULL, COALESCE(c.ruleshash = $2, false),
			c.staleat IS NOT NULL, c.unchecked, c.blocked, COALESCE(c.rules, '{}')
		FROM unnest($1::text[]) AS x(id) LEFT JOIN `+store.ComplianceTable+` c ON c.item_id = x.id`, ids, rs.hash())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var checked, current, stale bool
		var unchecked, blocked *string
		var rules []string
		if err := rows.Scan(&id, &checked, &current, &stale, &unchecked, &blocked, &rules); err != nil {
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
		case unchecked != nil:
			out[id] = "the compliance check couldn’t read its records: " + *unchecked
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
