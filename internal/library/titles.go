package library

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// TitleText names a title as a list of them does: a movie or a series by its
// title and year ("A Film (1991)"), an episode by its series, its number and
// its title ("A Show · S01E02 · Its Title"), a season by its series and its
// number ("A Show · S01"). A part the catalog doesn't hold is left out, and a
// title of which it holds nothing is its id.
func TitleText(id string, typ, title, series *string, year, season, episode *int32) string {
	str := func(s *string) string {
		if s == nil {
			return ""
		}
		return strings.TrimSpace(*s)
	}
	name := str(title)
	var parts []string
	add := func(p string) {
		if p != "" {
			parts = append(parts, p)
		}
	}
	switch str(typ) {
	case "episode":
		add(str(series))
		switch {
		case season != nil && episode != nil:
			add(fmt.Sprintf("S%02dE%02d", *season, *episode))
		case episode != nil:
			add(fmt.Sprintf("E%02d", *episode))
		}
		add(name)
	case "season":
		add(str(series))
		if season != nil {
			add(fmt.Sprintf("S%02d", *season))
		} else {
			add(name)
		}
	default:
		if name != "" && year != nil && *year > 0 {
			name += " (" + strconv.Itoa(int(*year)) + ")"
		}
		add(name)
	}
	if len(parts) == 0 {
		return id
	}
	return strings.Join(parts, " · ")
}

// TitlesOf names each title of ids the catalog holds as a list of them does
// (TitleText); one it doesn't hold has no name.
func TitlesOf(ctx context.Context, q Querier, ids []string) (map[string]string, error) {
	rows, err := q.Query(ctx, `SELECT x.id, lower(i.type), i.title, i.year, i.seasonnumber, i.episodenumber,
			CASE WHEN lower(p.type) = 'series' THEN p.title WHEN lower(g.type) = 'series' THEN g.title END
		FROM unnest($1::text[]) AS x(id)
		JOIN com_nalet_katalog_items i ON i.id = x.id
		LEFT JOIN com_nalet_katalog_items p ON p.id = i.parent_id
		LEFT JOIN com_nalet_katalog_items g ON g.id = p.parent_id`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id string
		var typ, title, series *string
		var year, season, episode *int32
		if err := rows.Scan(&id, &typ, &title, &year, &season, &episode, &series); err != nil {
			return nil, err
		}
		out[id] = TitleText(id, typ, title, series, year, season, episode)
	}
	return out, rows.Err()
}
