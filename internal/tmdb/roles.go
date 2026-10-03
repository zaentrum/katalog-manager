package tmdb

import (
	"sort"
	"strings"

	"github.com/zaentrum/katalog-manager/internal/model"
)

// Which of TMDB's credits the catalog keeps, and in which role. A title's
// cast are its actors; of its crew, a job the catalog knows credits the person
// in that job's role (crewRole), and every other job is not kept; a series'
// creators come from its details. One credit per person and role: a person's
// jobs in one role make one credit, their jobs joined.

// The roles TMDB's credits give, model.CreditRoles.
const (
	roleActor           = "actor"
	roleCreator         = "creator"
	roleDirector        = "director"
	roleWriter          = "writer"
	roleProducer        = "producer"
	roleComposer        = "composer"
	roleCinematographer = "cinematographer"
	roleEditor          = "editor"
)

// The most people a title is credited with in a role. A film's cast is its
// first billed; a series gathers its regulars over all its seasons, so it
// keeps more, ranked by the episodes each is in, and so are its directors. A
// film keeps every director, a series every creator, and any other role its
// first otherRoleCap.
const (
	movieCastCap    = 12
	seriesCreditCap = 20
	otherRoleCap    = 10
)

// creditCap is the most credits a film or a series keeps in role; 0 is all.
func creditCap(role string, series bool) int {
	switch role {
	case roleActor:
		if series {
			return seriesCreditCap
		}
		return movieCastCap
	case roleDirector:
		if series {
			return seriesCreditCap
		}
		return 0
	case roleCreator:
		return 0
	}
	return otherRoleCap
}

// crewRole is the role a crew job in a department credits a person in: a
// director for Directing's Director, Co-Director and Series Director; a writer
// for any job in Writing; a producer for a job in Production that is a
// producer of some kind; a composer for Sound's Original Music Composer, Music
// and Composer; a cinematographer for Camera's Director of Photography; an
// editor for Editing's Editor. "" for any other job: it is not kept.
func crewRole(department, job string) string {
	department, job = strings.TrimSpace(department), strings.TrimSpace(job)
	is := func(names ...string) bool {
		for _, n := range names {
			if strings.EqualFold(job, n) {
				return true
			}
		}
		return false
	}
	switch {
	case job == "":
		return ""
	case strings.EqualFold(department, "Directing") && is("Director", "Co-Director", "Series Director"):
		return roleDirector
	case strings.EqualFold(department, "Writing"):
		return roleWriter
	case strings.EqualFold(department, "Production") && strings.Contains(strings.ToLower(job), "producer"):
		return roleProducer
	case strings.EqualFold(department, "Sound") && is("Original Music Composer", "Music", "Composer"):
		return roleComposer
	case strings.EqualFold(department, "Camera") && is("Director of Photography"):
		return roleCinematographer
	case strings.EqualFold(department, "Editing") && is("Editor"):
		return roleEditor
	}
	return ""
}

// tmdbCredits is a title's credits as the catalog keeps them from TMDB: each
// person once per role, the roles in the order of model.CreditRoles, each
// ranked and cut to its cap (creditCap).
type tmdbCredits struct {
	List []tmdbCredit
}

// tmdbCredit is one credit: TMDB's id for the person, their name, the role,
// and what TMDB says of the credit.
type tmdbCredit struct {
	ID        int64
	Name      string
	Role      string
	Job       string // the person's jobs in the role, joined with ", "; "" for an actor
	Character string // whom an actor plays, "" when TMDB does not say
	Order     int    // the credit's place in its role, 0 first: a film actor's billing order, otherwise the rank
	Episodes  *int   // the episodes of a series the person is credited in, in the role; nil for a film and a creator
}

// creditList gathers a title's credits as TMDB lists them, a person's entries
// in one role into one credit, and then ranks each role (credits).
type creditList struct {
	series  bool
	entries map[creditKey]*creditEntry
	keys    []creditKey // in the order TMDB first lists each
}

// creditKey is a person in a role; a person TMDB gives no id is known by name.
type creditKey struct {
	id   int64
	name string
	role string
}

// creditEntry is what TMDB says of a person in a role, from all their entries.
type creditEntry struct {
	credit   tmdbCredit
	seen     int     // where TMDB first lists them in the role
	billing  int     // a cast's billing order, the first of their entries; crew has none
	episodes int     // a series: the episodes they are in, in the role
	jobs     []named // their jobs: a film's in TMDB's order, a series' with the episodes of each
	roles    []named // whom they play: in a film with the billing of each, in a series with the episodes
	total    int     // a series' crew: the episodes TMDB counts them in the department
	partial  bool    // a series' crew: a job of theirs in the department is not in the role
}

// named is a job or a character, and a number that orders it.
type named struct {
	name string
	n    int
}

func newCreditList(series bool) *creditList {
	return &creditList{series: series, entries: map[creditKey]*creditEntry{}}
}

// entry is the person's entry in role, made when they are first listed in it;
// nil for someone TMDB gives no name, who is not credited.
func (l *creditList) entry(id int64, name, role string) *creditEntry {
	if name = oneLine(name); name == "" {
		return nil
	}
	k := creditKey{id: id, role: role}
	if id <= 0 {
		k.name = name
	}
	e := l.entries[k]
	if e == nil {
		e = &creditEntry{credit: tmdbCredit{ID: id, Name: name, Role: role}, seen: len(l.keys)}
		l.entries[k] = e
		l.keys = append(l.keys, k)
	}
	return e
}

// filmActor lists someone of a film's cast and whom they play, with TMDB's
// billing order. Someone billed twice, as two characters, is one credit.
func (l *creditList) filmActor(id int64, name, character string, billing int) {
	if e := l.entry(id, name, roleActor); e != nil {
		if len(e.roles) == 0 || billing < e.billing {
			e.billing = billing
		}
		e.roles = append(e.roles, named{character, billing})
	}
}

// filmCrew lists a job of a film's crew, in role.
func (l *creditList) filmCrew(id int64, name, role, job string) {
	if e := l.entry(id, name, role); e != nil {
		e.jobs = append(e.jobs, named{job, 0})
	}
}

// seriesActor lists someone of a series' cast: the episodes they are in, in
// all, their billing order, and whom they play in how many episodes.
func (l *creditList) seriesActor(id int64, name string, episodes, billing int, roles []named) {
	if e := l.entry(id, name, roleActor); e != nil {
		if len(e.roles) == 0 || billing < e.billing {
			e.billing = billing
		}
		e.episodes = max(e.episodes, episodes)
		e.roles = append(e.roles, roles...)
	}
}

// seriesCrew lists someone's entry of a series' crew in a department: the
// episodes TMDB counts them in it, and their jobs there, with the episodes of
// each. Each job a role takes adds to their credit in that role.
func (l *creditList) seriesCrew(id int64, name, department string, total int, jobs []named) {
	var credited []*creditEntry
	for _, j := range jobs {
		if role := crewRole(department, j.name); role != "" {
			if e := l.entry(id, name, role); e != nil {
				e.jobs = append(e.jobs, j)
				credited = append(credited, e)
			}
		}
	}
	for _, e := range credited {
		e.total = max(e.total, total)
		for _, j := range jobs {
			if crewRole(department, j.name) != e.credit.Role {
				e.partial = true
			}
		}
	}
}

// creator lists a series' creator, in TMDB's order.
func (l *creditList) creator(id int64, name string) {
	if e := l.entry(id, name, roleCreator); e != nil {
		e.jobs = []named{{"Creator", 0}}
	}
}

// credits ranks the credits gathered and cuts each role to its cap. A film's
// actors rank by TMDB's billing order, which is their order; its crew and a
// series' creators come in TMDB's order. A series' actors and crew rank by the
// episodes each is credited in, in the role (an actor's in all; a crew
// member's as TMDB counts them in the department when every job of theirs
// there is in the role, else the most of any one job in it); a tie goes to
// TMDB's billing order (a series' crew has none), then the lower TMDB id. The
// order of every credit but a film actor's is its rank. An actor's characters
// are joined with " / " and a person's jobs with ", ": a film's in TMDB's
// order, a series' the most episodes first.
func (l *creditList) credits() *tmdbCredits {
	byRole := map[string][]*creditEntry{}
	for _, k := range l.keys {
		e := l.entries[k]
		if l.series && e.credit.Role != roleActor && e.credit.Role != roleCreator {
			for _, j := range e.jobs {
				e.episodes = max(e.episodes, j.n)
			}
			if !e.partial && e.total > 0 {
				e.episodes = e.total
			}
		}
		byRole[e.credit.Role] = append(byRole[e.credit.Role], e)
	}
	out := &tmdbCredits{}
	for _, role := range model.CreditRoles {
		es := byRole[role]
		sort.SliceStable(es, func(i, j int) bool { return l.before(es[i], es[j]) })
		if n := creditCap(role, l.series); n > 0 && len(es) > n {
			es = es[:n]
		}
		for i, e := range es {
			c := e.credit
			c.Order = i
			switch {
			case role == roleActor && l.series:
				c.Character = joined(mostFirst(e.roles), " / ")
			case role == roleActor:
				c.Order, c.Character = e.billing, joined(leastFirst(e.roles), " / ")
			case l.series:
				c.Job = joined(mostFirst(e.jobs), ", ")
			default:
				c.Job = joined(e.jobs, ", ")
			}
			if l.series && role != roleCreator {
				n := e.episodes
				c.Episodes = &n
			}
			out.List = append(out.List, c)
		}
	}
	return out
}

// before reports whether a ranks before b in their role.
func (l *creditList) before(a, b *creditEntry) bool {
	switch {
	case a.credit.Role == roleCreator:
		return a.seen < b.seen
	case l.series:
		if a.episodes != b.episodes {
			return a.episodes > b.episodes
		}
		if a.billing != b.billing {
			return a.billing < b.billing
		}
		return a.credit.ID < b.credit.ID
	case a.credit.Role == roleActor && a.billing != b.billing:
		return a.billing < b.billing
	}
	return a.seen < b.seen
}

// mostFirst and leastFirst order names by their number; a tie keeps the order
// given.
func mostFirst(ns []named) []named {
	out := append([]named(nil), ns...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].n > out[j].n })
	return out
}

func leastFirst(ns []named) []named {
	out := append([]named(nil), ns...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].n < out[j].n })
	return out
}

// joined joins the names in their order, each once, blank ones left out.
func joined(ns []named, sep string) string {
	var out []string
	seen := map[string]bool{}
	for _, n := range ns {
		if s := oneLine(n.name); s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return strings.Join(out, sep)
}

// --- TMDB's answers ---

// movieCreditsJSON is GET /movie/{id}/credits as far as the catalog reads it.
type movieCreditsJSON struct {
	Cast []struct {
		ID        int64  `json:"id"`
		Name      string `json:"name"`
		Character string `json:"character"`
		Order     int    `json:"order"`
	} `json:"cast"`
	Crew []struct {
		ID         int64  `json:"id"`
		Name       string `json:"name"`
		Department string `json:"department"`
		Job        string `json:"job"`
	} `json:"crew"`
}

// credits are a film's credits: the first movieCastCap of its cast, in TMDB's
// billing order, every director, and of each other role the first
// otherRoleCap of its crew, in TMDB's order.
func (n *movieCreditsJSON) credits() *tmdbCredits {
	l := newCreditList(false)
	for _, p := range n.Cast {
		l.filmActor(p.ID, p.Name, p.Character, p.Order)
	}
	for _, p := range n.Crew {
		if role := crewRole(p.Department, p.Job); role != "" {
			l.filmCrew(p.ID, p.Name, role, p.Job)
		}
	}
	return l.credits()
}

// tvAggregateJSON is a series' credits over every season (aggregate_credits)
// as far as the catalog reads them. TMDB lists a crew member once per
// department.
type tvAggregateJSON struct {
	Cast []struct {
		ID       int64  `json:"id"`
		Name     string `json:"name"`
		Order    int    `json:"order"`
		Episodes int    `json:"total_episode_count"`
		Roles    []struct {
			Character string `json:"character"`
			Episodes  int    `json:"episode_count"`
		} `json:"roles"`
	} `json:"cast"`
	Crew []struct {
		ID         int64  `json:"id"`
		Name       string `json:"name"`
		Department string `json:"department"`
		Episodes   int    `json:"total_episode_count"`
		Jobs       []struct {
			Job      string `json:"job"`
			Episodes int    `json:"episode_count"`
		} `json:"jobs"`
	} `json:"crew"`
}

// list gathers a series' cast and crew into l.
func (n *tvAggregateJSON) list(l *creditList) {
	for _, p := range n.Cast {
		var roles []named
		for _, r := range p.Roles {
			roles = append(roles, named{r.Character, r.Episodes})
		}
		l.seriesActor(p.ID, p.Name, p.Episodes, p.Order, roles)
	}
	for _, p := range n.Crew {
		var jobs []named
		for _, j := range p.Jobs {
			jobs = append(jobs, named{j.Job, j.Episodes})
		}
		l.seriesCrew(p.ID, p.Name, p.Department, p.Episodes, jobs)
	}
}
