package library

import (
	"encoding/json"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
)

// The projections, an item's metadata.json and a person's person.json: the
// Go port of what library-v2-from-catalog.py writes of them (metadata_json,
// person and their images), so that the catalog and the tool make the same
// file of the same row. A row is an item or a person as the catalog's export
// holds one (db/export/library-export.sql), a Doc; its images come apart
// (ImageIn), so that the projector reads their bytes only for an image it
// writes. Where the catalog projects, it differs from the tool as the
// contract says: projectedBy katalog-manager, asOf the moment it writes, the
// version that plays (a covered episode's its holder's), the extras'
// decisions, the current reference ids (externalIds), and a series' seasons
// from its episodes under a season too.

// TextLanguage is the language the catalog's texts are projected under: it
// keeps no word of theirs (the tool's --text-language by default).
const TextLanguage = "und"

var (
	imageKinds       = map[string]bool{"poster": true, "backdrop": true, "logo": true, "still": true, "banner": true, "thumb": true}
	personImageKinds = map[string]bool{"profile": true}
	creditRoles      = []string{"actor", "creator", "director", "writer", "producer", "composer", "cinematographer", "editor"}
	roleRE           = regexp.MustCompile(`^[a-z][a-z0-9-]{0,39}$`)
	personIDRE       = regexp.MustCompile(`^[0-9a-f-]{36}$`)
	languageKeyRE    = regexp.MustCompile(`^([a-z]{2,3}(-[A-Za-z0-9]{2,8})*|und|zxx|mul)$`)
	dateRE           = regexp.MustCompile(`^([0-9]{4})(-[0-9]{2}(-[0-9]{2})?)?`)
	timestampRE      = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\.[0-9]+)?(Z|[+-][0-9]{2}:[0-9]{2})$`)
	fieldOrigins     = map[string]bool{"tmdb": true, "legacy-catalog": true, "filename": true, "folder-name": true,
		"file-tags": true, "manual": true}
	extOf = map[string]string{"image/jpeg": "jpg", "image/png": "png", "image/webp": "webp"}
)

// pyStrip is s without the whitespace around it, as Python's str.strip()
// takes whitespace.
func pyStrip(s string) string {
	return strings.TrimFunc(s, func(r rune) bool { return unicode.IsSpace(r) || (r >= 0x1c && r <= 0x1f) })
}

// pyText is the tool's text(): a one-line string, nil for none.
func pyText(v any) *string {
	var s string
	switch x := v.(type) {
	case nil:
		return nil
	case string:
		s = x
	case json.Number:
		s = x.String()
	case bool:
		s = map[bool]string{true: "True", false: "False"}[x]
	default:
		return nil
	}
	s = pyStrip(strings.NewReplacer("\r", " ", "\n", " ").Replace(s))
	if s == "" {
		return nil
	}
	return &s
}

// text is pyText of d's key.
func text(d Doc, key string) *string {
	v, _ := d.Get(key)
	return pyText(v)
}

// textOr is pyText of d's key, or "".
func textOr(d Doc, key string) string {
	if t := text(d, key); t != nil {
		return *t
	}
	return ""
}

// get is d's key.
func get(d Doc, key string) any {
	v, _ := d.Get(key)
	return v
}

// truthyAt reports whether d's key is truthy, as Python takes a value.
func truthyAt(d Doc, key string) bool { return truthy(get(d, key)) }

// pyNumber is v as Python's json reads it: an integer (int64), a float
// (float64), or nothing.
func pyNumber(v any) (any, bool) {
	n, ok := v.(json.Number)
	if !ok {
		switch x := v.(type) {
		case int64:
			return x, true
		case int:
			return int64(x), true
		case float64:
			return x, true
		}
		return nil, false
	}
	if !strings.ContainsAny(n.String(), ".eE") {
		if i, err := n.Int64(); err == nil {
			return i, true
		}
	}
	f, err := n.Float64()
	if err != nil {
		return nil, false
	}
	return f, true
}

// pyInt is the tool's int(v) of a number it holds.
func pyInt(v any) (int64, bool) {
	n, ok := pyNumber(v)
	if !ok {
		return 0, false
	}
	switch x := n.(type) {
	case int64:
		return x, true
	case float64:
		return int64(x), true
	}
	return 0, false
}

// whole is the tool's whole(): a whole number (3.0 is 3), nil for none or
// anything else, or a count below least.
func whole(v any, least *int64) any {
	n, ok := pyNumber(v)
	if !ok {
		return nil
	}
	var i int64
	switch x := n.(type) {
	case int64:
		i = x
	case float64:
		if x != float64(int64(x)) {
			return nil
		}
		i = int64(x)
	}
	if least != nil && i < *least {
		return nil
	}
	return i
}

// pyTS is the tool's ts(): a timestamp as RFC 3339 with an upper-case T and
// Z, nil for none.
func pyTS(v any) *string {
	s, ok := v.(string)
	if !ok || s == "" {
		return nil
	}
	s = strings.ReplaceAll(strings.TrimSpace(s), " ", "T")
	if !regexp.MustCompile(`(Z|[+-]\d\d:?\d\d)$`).MatchString(s) {
		s += "Z"
	}
	s = regexp.MustCompile(`([+-]\d\d)(\d\d)$`).ReplaceAllString(s, "$1:$2")
	s = strings.ReplaceAll(s, "+00:00", "Z")
	return &s
}

// moment is the tool's moment(): a timestamp as a record holds one, nil when
// there is none or it is no moment.
func moment(v any) *string {
	s := pyTS(v)
	if s == nil || !timestampRE.MatchString(*s) {
		return nil
	}
	return s
}

// date is the tool's date(): a date as a record holds one (a year, a year and
// month, a day), from a date or a timestamp's date; nil for anything else.
func date(v any) *string {
	s, ok := v.(string)
	if !ok || s == "" {
		return nil
	}
	m := dateRE.FindString(pyStrip(s))
	if m == "" {
		return nil
	}
	return &m
}

// freshness is the tool's freshness(): how fresh a projection is, from the
// row it projects.
func freshness(row Doc) Doc {
	out := Doc{}
	if u := moment(get(row, "modifiedAt")); u != nil {
		out = append(out, Field{"databaseUpdatedAt", *u})
	}
	fetched := moment(get(row, "tmdbFetchedAt"))
	changed := date(get(row, "tmdbChangedAt"))
	if fetched != nil {
		tm := Doc{{"fetchedAt", *fetched}}
		if changed != nil {
			tm = append(tm, Field{"changedAt", *changed})
		}
		out = append(out, Field{"sources", Doc{{"tmdb", tm}}})
	}
	return out
}

// ImageIn is an image of a row as the projection reads it: its kind, the
// hash and size of its bytes, its first bytes (which tell its type and its
// size in pixels), and its bytes, read when they are needed; what the row
// says of it (primary, where it was fetched from, when).
type ImageIn struct {
	Kind       string
	SHA256     string // hex
	Size       int64
	Head       []byte
	Bytes      func() ([]byte, error)
	Primary    bool
	SourcePath *string
	FetchedAt  any
}

// ImageFile is an image a projection lists that its folder must hold: its
// name (its hash and extension) and its bytes.
type ImageFile struct {
	Name  string
	Bytes func() ([]byte, error)
}

// images is the tool's artwork(): the entries a projection lists of the
// images ins of kinds, and the files they name. An image is named by the
// hash of its bytes, which decide its type and size; one of a kind no
// record holds, without bytes, or that is no JPEG, PNG or WebP is left out.
// Every row is an image of its kind: byte-identical rows of two kinds (a
// backdrop that is the poster) are two entries sharing one file, written
// once; byte-identical rows of one kind are one entry, primary when either
// is. At most one of a kind is primary, the first. A series' images name no
// season.
func images(ins []ImageIn, kinds map[string]bool, series bool) ([]any, []ImageFile, error) {
	var out []Doc
	byEntry := map[string]int{} // its file and kind: where it is in out
	written := map[string]bool{}
	var files []ImageFile
	for _, in := range ins {
		kind := strings.ToLower(in.Kind)
		if !kinds[kind] || in.Size == 0 {
			continue
		}
		ctype, w, h := SniffImage(in.Head)
		if ctype == "image/jpeg" && w == nil && in.Size > int64(len(in.Head)) && in.Bytes != nil {
			full, err := in.Bytes()
			if err != nil {
				return nil, nil, err
			}
			ctype, w, h = SniffImage(full)
		}
		ext, ok := extOf[ctype]
		if !ok {
			continue
		}
		name := in.SHA256 + "." + ext
		if i, seen := byEntry[name+"\x00"+kind]; seen {
			if in.Primary {
				out[i] = out[i].With("primary", true)
			}
			continue
		}
		fetched := moment(in.FetchedAt)
		var ref *string
		if in.SourcePath != nil {
			ref = pyText(*in.SourcePath)
		}
		entry := Doc{{"kind", kind}}
		if in.Primary {
			entry = append(entry, Field{"primary", true})
		}
		origin := Doc{{"source", "legacy-catalog"}}
		if ref != nil {
			origin = Doc{{"source", "tmdb"}, {"ref", *ref}}
		}
		if fetched != nil {
			origin = append(origin, Field{"fetchedAt", *fetched})
		}
		entry = append(entry, Field{"file", name}, Field{"sha256", "sha256:" + in.SHA256}, Field{"contentType", ctype},
			Field{"sizeBytes", in.Size}, Field{"width", ptrInt(w)}, Field{"height", ptrInt(h)}, Field{"language", nil},
			Field{"sourceUrl", nil}, Field{"fetchedAt", ptrStr(fetched)}, Field{"origin", origin})
		if series {
			entry = append(entry, Field{"season", nil})
		}
		out = append(out, entry)
		byEntry[name+"\x00"+kind] = len(out) - 1
		if !written[name] {
			written[name] = true
			files = append(files, ImageFile{Name: name, Bytes: in.Bytes})
		}
	}
	first := map[string]bool{}
	list := make([]any, len(out))
	for i, e := range out {
		if p, _ := e.Get("primary"); p == true {
			kind := str(e, "kind")
			if first[kind] {
				e = e.Without("primary")
			}
			first[kind] = true
		}
		list[i] = e
	}
	return list, files, nil
}

// sortImages orders an item's images as the tool's image_order(): by kind,
// then by season (the series' own images first), then by file, the hash of
// its bytes: the export lists an item's artwork in no particular order.
func sortImages(list []any) {
	key := func(e any) (string, bool, int64, string) {
		d, _ := e.(Doc)
		season, _ := d.Get("season")
		n, ok := toFloat(season)
		return str(d, "kind"), season != nil, map[bool]int64{true: int64(n)}[ok], str(d, "file")
	}
	sort.SliceStable(list, func(i, j int) bool {
		ki, si, ni, fi := key(list[i])
		kj, sj, nj, fj := key(list[j])
		switch {
		case ki != kj:
			return ki < kj
		case si != sj:
			return !si
		case ni != nj:
			return ni < nj
		}
		return fi < fj
	})
}

func ptrInt(p *int64) any {
	if p == nil {
		return nil
	}
	return *p
}

func ptrStr(p *string) any {
	if p == nil {
		return nil
	}
	return *p
}

// SniffImage is an image's content type and size in pixels from its header
// alone ("", nil, nil when it is no image it knows), as the library's tools
// read it.
func SniffImage(b []byte) (string, *int64, *int64) {
	be := func(x []byte) *int64 {
		var n int64
		for _, c := range x {
			n = n<<8 | int64(c)
		}
		return &n
	}
	le := func(x []byte) int64 {
		var n int64
		for i := len(x) - 1; i >= 0; i-- {
			n = n<<8 | int64(x[i])
		}
		return n
	}
	if len(b) >= 24 && string(b[:8]) == "\x89PNG\r\n\x1a\n" {
		return "image/png", be(b[16:20]), be(b[20:24])
	}
	if len(b) >= 12 && string(b[:4]) == "RIFF" && string(b[8:12]) == "WEBP" {
		chunk := ""
		if len(b) >= 16 {
			chunk = string(b[12:16])
		}
		switch {
		case chunk == "VP8X" && len(b) >= 30:
			w, h := 1+le(b[24:27]), 1+le(b[27:30])
			return "image/webp", &w, &h
		case chunk == "VP8 " && len(b) >= 30:
			w, h := le(b[26:28])&0x3FFF, le(b[28:30])&0x3FFF
			return "image/webp", &w, &h
		case chunk == "VP8L" && len(b) >= 25:
			v := le(b[21:25])
			w, h := (v&0x3FFF)+1, ((v>>14)&0x3FFF)+1
			return "image/webp", &w, &h
		}
		return "image/webp", nil, nil
	}
	if len(b) >= 2 && b[0] == 0xff && b[1] == 0xd8 {
		i := 2
		for i+9 < len(b) {
			if b[i] != 0xFF {
				i++
				continue
			}
			m := b[i+1]
			switch {
			case m == 0xC0 || m == 0xC1 || m == 0xC2 || m == 0xC3 || m == 0xC5 || m == 0xC6 || m == 0xC7 ||
				m == 0xC9 || m == 0xCA || m == 0xCB || m == 0xCD || m == 0xCE || m == 0xCF:
				return "image/jpeg", be(b[i+7 : i+9]), be(b[i+5 : i+7])
			case m == 0xD8 || m == 0x01 || (m >= 0xD0 && m <= 0xD7):
				i += 2
				continue
			}
			i += 2 + int(*be(b[i+2 : i+4]))
		}
		return "image/jpeg", nil, nil
	}
	return "", nil, nil
}

// creditKey is where a credit stands: by role (the roles the catalog knows in
// their order, any other after them, by role), then by order (none last),
// then by name and person.
type creditKey struct {
	rank           int
	role           string
	noOrder        bool
	order          int64
	name, personID string
}

func (a creditKey) less(b creditKey) bool {
	switch {
	case a.rank != b.rank:
		return a.rank < b.rank
	case a.role != b.role:
		return a.role < b.role
	case a.noOrder != b.noOrder:
		return !a.noOrder
	case a.order != b.order:
		return a.order < b.order
	case a.name != b.name:
		return a.name < b.name
	}
	return a.personID < b.personID
}

// credits are the row's people as metadata.json lists them.
func credits(row Doc) []any {
	type credit struct {
		doc Doc
		key creditKey
	}
	var cs []credit
	zero := int64(0)
	for _, e := range asSlice(get(row, "people")) {
		p, _ := e.(Doc)
		pid := text(p, "personId")
		if pid == nil || !personIDRE.MatchString(*pid) {
			continue
		}
		name := textOr(p, "name")
		role := "actor"
		if r := text(p, "role"); r != nil {
			role = *r
		}
		if !roleRE.MatchString(role) {
			continue
		}
		order := whole(get(p, "order"), nil)
		doc := Doc{{"personId", *pid}, {"name", name}, {"role", role}, {"job", ptrStr(text(p, "job"))},
			{"character", ptrStr(text(p, "character"))}, {"order", order},
			{"episodeCount", whole(get(p, "episodeCount"), &zero)}, {"tmdbPerson", nil}}
		rank := len(creditRoles)
		for i, r := range creditRoles {
			if r == role {
				rank = i
			}
		}
		k := creditKey{rank: rank, role: role, noOrder: order == nil, name: name, personID: *pid}
		if order != nil {
			k.order = order.(int64)
		}
		cs = append(cs, credit{doc, k})
	}
	sort.SliceStable(cs, func(i, j int) bool { return cs[i].key.less(cs[j].key) })
	out := make([]any, len(cs))
	for i, c := range cs {
		out[i] = c.doc
	}
	return out
}

func asSlice(v any) []any {
	l, _ := v.([]any)
	return l
}

// sortedStrings is the tool's sorted({x for x in list if x}).
func sortedStrings(v any) []any {
	set := map[string]bool{}
	for _, e := range asSlice(v) {
		if s, ok := e.(string); ok && s != "" {
			set[s] = true
		}
	}
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]any, len(keys))
	for i, k := range keys {
		out[i] = k
	}
	return out
}

// videos are the row's trailer links as metadata.json lists them.
func videos(row Doc) []any {
	out := []any{}
	for _, e := range asSlice(get(row, "trailers")) {
		t, _ := e.(Doc)
		site := text(t, "site")
		if site == nil {
			site = text(t, "source")
		}
		key := text(t, "externalId")
		if key == nil {
			if u, ok := get(t, "url").(string); ok && u != "" {
				key = pyText(u[strings.LastIndex(u, "/")+1:])
			}
		}
		if site == nil || key == nil {
			continue
		}
		source := strings.ToLower(textOr(t, "source"))
		origin := "legacy-catalog"
		if source == "tmdb" || source == "manual" {
			origin = source
		}
		var duration any
		if d, ok := pyInt(get(t, "durationSec")); ok && d != 0 {
			duration = d * 1000
		}
		out = append(out, Doc{{"site", *site}, {"key", *key}, {"url", ptrStr(text(t, "url"))}, {"name", ptrStr(text(t, "title"))},
			{"kind", "trailer"}, {"language", nil}, {"durationMs", duration}, {"publishedAt", nil}, {"origin", origin}})
	}
	return out
}

// ItemProjection is what an item's metadata.json says beside its row: when
// it is written, its images, a series' season numbers, the version that
// plays, the extras' decisions; what the tool fills in itself
// (ProjectedBy, AsOf) is the projector's. Of one file of several episodes,
// the holder's numbering ends at the last episode its file covers
// (EpisodeEnd), and every other one it covers names it (CoveredBy) and plays
// its version (PrimaryVersionID, the holder's).
type ItemProjection struct {
	ProjectedBy      string
	AsOf             string
	Images           []ImageIn
	Seasons          []int64 // a series' episodes' seasons, each once
	PrimaryVersionID string
	CoveredBy        string
	EpisodeEnd       *int64
	Extras           Doc // extraId: {order, hidden, label}, as a person decided
	ExternalIDs      bool
}

// MetadataDoc is the item's metadata.json, the port of the tool's
// metadata_json(), and the images its metadata/ folder holds.
func MetadataDoc(row Doc, p ItemProjection) (Doc, []ImageFile, error) {
	typ := textOr(row, "type")
	id := textOr(row, "id")
	body := Doc{}
	if t := text(row, "title"); t != nil {
		body = append(body, Field{"title", *t})
	}
	if t := text(row, "sortTitle"); t != nil {
		body = append(body, Field{"sortTitle", *t})
	}
	if v, ok := get(row, "tagline").(string); ok && v != "" {
		body = append(body, Field{"tagline", pyStrip(v)})
	}
	if v, ok := get(row, "description").(string); ok && v != "" {
		body = append(body, Field{"overview", pyStrip(v)})
	}
	localized := Doc{}
	if len(body) > 0 {
		localized = Doc{{TextLanguage, body}}
	}
	doc := Doc{{"schema", "zaentrum.library.metadata/2"}, {"itemId", id}, {"type", typ}, {"asOf", p.AsOf},
		{"projectedBy", p.ProjectedBy}}
	doc = append(doc, freshness(row)...)
	ids := ExternalIDs(typ, externalIDPairs(row))
	if p.ExternalIDs {
		doc = append(doc, Field{"externalIds", ids})
	}
	var release any
	if y, ok := pyInt(get(row, "year")); ok && y != 0 {
		release = strconv.FormatInt(y, 10)
	}
	var rating any
	if n, ok := pyNumber(get(row, "rating")); ok {
		if f, _ := toFloat(n); f >= 0 && f <= 10 {
			rating = n
		}
	}
	doc = append(doc, Field{"titles", Doc{{"primary", textOr(row, "title")}, {"original", nil},
		{"sort", ptrStr(text(row, "sortTitle"))}, {"qualifier", nil}, {"localized", localized}}},
		Field{"releaseDate", release}, Field{"genres", sortedStrings(get(row, "genres"))},
		Field{"tags", sortedStrings(get(row, "tags"))}, Field{"rating", rating}, Field{"contentRating", nil},
		Field{"credits", credits(row)})
	switch typ {
	case "movie":
		doc = append(doc, Field{"collection", nil})
	case "series":
		seasons := []any{}
		for _, n := range p.Seasons {
			seasons = append(seasons, Doc{{"number", n}, {"tmdbSeason", nil}, {"name", nil}, {"overview", nil},
				{"airDate", nil}, {"episodeCountReference", nil}})
		}
		doc = append(doc, Field{"series", Doc{{"seasons", seasons}}})
	}
	status := "unmatched"
	for _, f := range ids {
		if strings.HasPrefix(f.Key, "tmdb") {
			status = "matched"
		}
	}
	lib := Doc{{"match", Doc{{"status", status}, {"decidedBy", "legacy-catalog"}, {"decidedAt", p.AsOf}}}}
	if p.PrimaryVersionID != "" {
		lib = append(lib, Field{"primaryVersionId", p.PrimaryVersionID})
	}
	if p.CoveredBy != "" {
		lib = append(lib, Field{"coveredBy", p.CoveredBy})
	}
	if d, ok := pyInt(get(row, "durationMs")); ok && d != 0 {
		lib = append(lib, Field{"reference", Doc{{"runtimeMs", d}, {"runtimeSource", "legacy-catalog"}}})
	}
	if typ == "series" {
		lib = append(lib, Field{"defaultOrdering", "aired"})
	}
	if typ == "episode" {
		s, okS := pyInt(get(row, "seasonNumber"))
		e, okE := pyInt(get(row, "episodeNumber"))
		if okS && okE {
			lib = append(lib, Field{"numbering", Doc{{"aired", Doc{{"season", s}, {"episode", e}, {"episodeEnd", ptrInt(p.EpisodeEnd)}}}}})
		}
	}
	if len(p.Extras) > 0 {
		lib = append(lib, Field{"extras", p.Extras})
	}
	imgs, files, err := images(p.Images, imageKinds, typ == "series")
	sortImages(imgs)
	if err != nil {
		return nil, nil, err
	}
	vids := videos(row)
	doc = append(doc, Field{"library", lib}, Field{"images", imgs}, Field{"videos", vids},
		Field{"curation", Doc{{"metadataLocked", truthyAt(row, "metadataLocked")}, {"lockedFields", []any{}}, {"notes", nil}}})
	origins := Doc{}
	titles, _ := doc.Get("titles")
	for _, f := range []Field{{"titles.primary", get(titles.(Doc), "primary")}, {"titles.sort", get(titles.(Doc), "sort")},
		{"releaseDate", release}, {"genres", get(doc, "genres")}, {"tags", get(doc, "tags")}, {"rating", rating},
		{"credits", get(doc, "credits")}, {"images", imgs}, {"videos", vids}} {
		if truthy(f.Value) || isNonEmptyList(f.Value) {
			origins = append(origins, Field{f.Key, "legacy-catalog"})
		}
	}
	doc = append(doc, Field{"fieldOrigins", origins})
	return doc, files, nil
}

func isNonEmptyList(v any) bool {
	l, ok := v.([]any)
	return ok && len(l) > 0
}

// externalIDPairs are the row's reference ids as (source, id).
func externalIDPairs(row Doc) [][2]string {
	var out [][2]string
	for _, e := range asSlice(get(row, "externalIds")) {
		d, _ := e.(Doc)
		src, _ := get(d, "source").(string)
		val := ""
		if v := pyText(get(d, "externalId")); v != nil {
			val = *v
		}
		out = append(out, [2]string{src, val})
	}
	return out
}

// PersonProjection is what a person's person.json says beside their row.
type PersonProjection struct {
	ProjectedBy string
	AsOf        string
	Images      []ImageIn
	CreditedAs  []string // the names their credits give them
}

// personIDFields are the reference ids person.json keys a person by, with
// the form each must have.
var personIDFields = map[string]*regexp.Regexp{"tmdbPerson": regexp.MustCompile(`^[0-9]+$`),
	"imdb": regexp.MustCompile(`^nm[0-9]+$`), "tvdb": regexp.MustCompile(`^[0-9]+$`),
	"wikidata": regexp.MustCompile(`^Q[0-9]+$`)}

// personIDSources are the sources an export may name a person's id by, and
// the field each is.
var personIDSources = map[string]string{"tmdb": "tmdbPerson", "themoviedb": "tmdbPerson", "tmdb-person": "tmdbPerson",
	"tmdbperson": "tmdbPerson", "imdb": "imdb", "tvdb": "tvdb", "wikidata": "wikidata"}

// PersonDoc is the person's person.json, the port of the tool's person(),
// and the portraits beside it; nil when nothing names the person.
func PersonDoc(pid string, row Doc, p PersonProjection) (Doc, []ImageFile, error) {
	name := text(row, "name")
	if name == nil && len(p.CreditedAs) > 0 {
		name = &p.CreditedAs[0]
	}
	if name == nil {
		return nil, nil, nil
	}
	aka := map[string]bool{}
	for _, e := range asSlice(get(row, "alsoKnownAs")) {
		if t := pyText(e); t != nil && *t != *name {
			aka[*t] = true
		}
	}
	akaList := make([]string, 0, len(aka))
	for k := range aka {
		akaList = append(akaList, k)
	}
	sort.Strings(akaList)
	bio := Doc{}
	switch b := get(row, "biography").(type) {
	case Doc:
		for _, f := range b {
			if !languageKeyRE.MatchString(f.Key) {
				continue
			}
			if s, ok := f.Value.(string); ok && pyStrip(s) != "" {
				bio = append(bio, Field{f.Key, pyStrip(s)})
			}
		}
	case string:
		if pyStrip(b) != "" {
			bio = Doc{{TextLanguage, pyStrip(b)}}
		}
	}
	ids := Doc{}
	add := func(source string, value any) {
		v := pyText(value)
		if v == nil {
			return
		}
		field := source
		if _, ok := personIDFields[field]; !ok {
			field = personIDSources[strings.ToLower(source)]
		}
		if re, ok := personIDFields[field]; ok && re.MatchString(*v) {
			ids = ids.With(field, *v)
		}
	}
	switch x := get(row, "externalIds").(type) {
	case Doc:
		for _, f := range x {
			add(f.Key, f.Value)
		}
	case []any:
		for _, e := range x {
			d, _ := e.(Doc)
			s, _ := get(d, "source").(string)
			add(s, get(d, "externalId"))
		}
	}
	imgs, files, err := images(p.Images, personImageKinds, false)
	if err != nil {
		return nil, nil, err
	}
	locked := map[string]bool{}
	for _, e := range asSlice(get(row, "lockedFields")) {
		if t := pyText(e); t != nil {
			locked[*t] = true
		}
	}
	lockedList := make([]string, 0, len(locked))
	for k := range locked {
		lockedList = append(lockedList, k)
	}
	sort.Strings(lockedList)
	doc := Doc{{"schema", "zaentrum.library.person/2"}, {"personId", pid}, {"asOf", p.AsOf}, {"projectedBy", p.ProjectedBy}}
	doc = append(doc, freshness(row)...)
	doc = append(doc, Field{"name", *name}, Field{"sortName", ptrStr(text(row, "sortName"))}, Field{"alsoKnownAs", akaList},
		Field{"birthDate", ptrStr(date(get(row, "birthDate")))}, Field{"deathDate", ptrStr(date(get(row, "deathDate")))},
		Field{"birthPlace", ptrStr(text(row, "birthPlace"))}, Field{"knownForDepartment", ptrStr(text(row, "knownForDepartment"))},
		Field{"biography", bio}, Field{"externalIds", ids}, Field{"images", imgs},
		Field{"curation", Doc{{"metadataLocked", truthyAt(row, "metadataLocked")}, {"lockedFields", lockedList}, {"notes", nil}}})
	if fo, ok := get(row, "fieldOrigins").(Doc); ok {
		origins := Doc{}
		for _, f := range fo {
			key := pyText(f.Key)
			if s, ok := f.Value.(string); key != nil && ok && fieldOrigins[s] {
				origins = origins.With(*key, s)
			}
		}
		doc = append(doc, Field{"fieldOrigins", origins})
	} else {
		origins := Doc{}
		for _, k := range []string{"name", "sortName", "alsoKnownAs", "birthDate", "deathDate", "birthPlace",
			"knownForDepartment", "biography", "externalIds", "images"} {
			if v := get(doc, k); truthy(v) || isNonEmptyList(v) || isNonEmptyStrings(v) {
				origins = append(origins, Field{k, "legacy-catalog"})
			}
		}
		doc = append(doc, Field{"fieldOrigins", origins})
	}
	return doc, files, nil
}

func isNonEmptyStrings(v any) bool {
	l, ok := v.([]string)
	return ok && len(l) > 0
}
