package tmdb

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg" // image.DecodeConfig reads a profile's size
	_ "image/png"
	"log"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// The roles a credit links a person to a title in.
const (
	roleActor    = "actor"
	roleDirector = "director"
)

// peopleReady reports whether migration 030 is in place. Without it a person
// is only a name, and credits link people by name as they always did.
func (s *Service) peopleReady(ctx context.Context) bool {
	if s.peopleOK.Load() {
		return true
	}
	if s.peopleCheck == nil {
		return false
	}
	ok, err := s.peopleCheck(ctx)
	if err != nil || !ok {
		return false
	}
	s.peopleOK.Store(true)
	return true
}

// applyCredits stores a title's credits: each credited person is found by
// their TMDB id and linked to the item in their role, and a person the catalog
// has not read from TMDB yet gets their details and profile. A person whose
// details cannot be had is logged and left for later: it never fails the title.
func (s *Service) applyCredits(ctx context.Context, itemID string, c *tmdbCredits) {
	if !s.peopleReady(ctx) {
		for _, d := range c.Crew {
			s.upsertPersonByName(ctx, itemID, d.Name, roleDirector)
		}
		for _, a := range c.Cast {
			s.upsertPersonByName(ctx, itemID, a.Name, roleActor)
		}
		return
	}
	links := s.linkCredits(ctx, itemID, c)
	s.fetchUnreadPeople(ctx, links.people)
}

// fetchUnreadPeople reads TMDB's details of each of the given people that the
// catalog has never read from TMDB, unless their record is locked. Failures
// are logged: the person stays unread, and the next title that credits them,
// the backfill or the change list tries again.
func (s *Service) fetchUnreadPeople(ctx context.Context, personIDs []string) {
	if len(personIDs) == 0 {
		return
	}
	unread, err := s.peopleToRead(ctx, `id = ANY($1::text[]) AND tmdbfetchedat IS NULL AND NOT metadatalocked`, personIDs)
	if err != nil {
		log.Printf("tmdb: credited people: %v", err)
		return
	}
	for _, p := range unread {
		if _, err := s.refreshPerson(ctx, p.id, p.tmdbID, nil); err != nil {
			log.Printf("tmdb: person %s (TMDB %d): %v; left unread for now", p.id, p.tmdbID, err)
		}
	}
}

// personRef is a person and their TMDB id.
type personRef struct {
	id     string
	tmdbID int64
}

// peopleToRead lists the people with a TMDB id that match where (a condition
// on com_nalet_katalog_people with its arguments), by id.
func (s *Service) peopleToRead(ctx context.Context, where string, args ...any) ([]personRef, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, tmdbpersonid FROM com_nalet_katalog_people
		WHERE tmdbpersonid IS NOT NULL AND (`+where+`) ORDER BY id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []personRef
	for rows.Next() {
		var p personRef
		var key string
		if err := rows.Scan(&p.id, &key); err != nil {
			return nil, err
		}
		if p.tmdbID, err = strconv.ParseInt(key, 10, 64); err != nil {
			continue
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ===================== a person's details =====================

// A person's fields, named as the library record's person.json names them
// (plus knownForDepartment). lockedfields and fieldorigins use these names;
// sortName is one too, but TMDB has none to write.
const (
	fieldName               = "name"
	fieldAlsoKnownAs        = "alsoKnownAs"
	fieldBirthDate          = "birthDate"
	fieldDeathDate          = "deathDate"
	fieldBirthPlace         = "birthPlace"
	fieldBiography          = "biography"
	fieldKnownForDepartment = "knownForDepartment"
	fieldExternalIDs        = "externalIds"
	fieldImages             = "images"
)

const (
	originTMDB  = "tmdb"
	profileSize = "h632" // TMDB's profile size for a person page: 632 pixels high
)

// locks is what a person's record says automation must leave alone.
type locks struct {
	all    bool     // metadatalocked: every field
	fields []string // lockedfields
}

// has reports whether field is locked: the whole record is, or a lock names
// the field, a part of it ("externalIds.imdb" locks externalIds) or something
// it is part of.
func (l locks) has(field string) bool {
	if l.all {
		return true
	}
	for _, f := range l.fields {
		if f == field || strings.HasPrefix(f, field+".") || strings.HasPrefix(field, f+".") {
			return true
		}
	}
	return false
}

// How a person refresh went, when it did not fail.
type personOutcome int

const (
	personRefreshed personOutcome = iota // read from TMDB and stored, as far as the locks allow
	personLocked                         // the record is locked: TMDB was not asked
	personGone                           // TMDB does not know the id (any more): nothing changed
)

// refreshPerson reads a person's details and primary profile image from TMDB
// and stores them, except the fields their locks name, and nothing at all when
// their record is locked. It stores everything TMDB gave or, when any part of
// it fails, nothing. changedOn, when set, is the day TMDB's change list named
// the person.
func (s *Service) refreshPerson(ctx context.Context, personID string, tmdbID int64, changedOn *time.Time) (personOutcome, error) {
	var lk locks
	var lockedRaw []byte
	var keptPath *string
	var hasProfile bool
	err := s.pool.QueryRow(ctx, `SELECT p.metadatalocked, p.lockedfields,
			(SELECT a.sourcepath FROM com_nalet_katalog_personartwork a
			 WHERE a.person_id = p.id AND a.kind = 'profile' AND a.isprimary),
			EXISTS (SELECT 1 FROM com_nalet_katalog_personartwork a WHERE a.person_id = p.id AND a.kind = 'profile')
		FROM com_nalet_katalog_people p WHERE p.id = $1`, personID).Scan(&lk.all, &lockedRaw, &keptPath, &hasProfile)
	if err != nil {
		return 0, fmt.Errorf("read person %s: %w", personID, err)
	}
	if lk.all {
		return personLocked, nil
	}
	lk.fields = jsonStrings(lockedRaw)

	lang := s.tmdb.lang()
	p, err := s.tmdb.getPerson(ctx, tmdbID, lang, true)
	if isNotFound(err) {
		return personGone, nil
	}
	if err != nil {
		return 0, err
	}
	bios := map[string]string{languageKey(lang): paragraphs(p.Biography)}
	if languageKey(lang) != "en" {
		en, err := s.tmdb.getPerson(ctx, tmdbID, "en-US", false)
		if err != nil && !isNotFound(err) {
			return 0, err
		}
		if en != nil {
			bios["en"] = paragraphs(en.Biography)
		}
	}
	var img *profileImage
	if !lk.has(fieldImages) {
		kept := ""
		if keptPath != nil {
			kept = *keptPath
		}
		if img, err = s.profileFor(ctx, p.ProfilePath, kept, hasProfile); err != nil {
			return 0, err
		}
	}
	return s.storePerson(ctx, personID, p, bios, img, changedOn)
}

// profileImage is a profile image to keep, or with gone set, the word that the
// person's kept profile goes.
type profileImage struct {
	gone          bool
	bytes         []byte
	contentType   string
	sha256        string // lower-case hex
	width, height *int
	sourcePath    string // TMDB's file path
}

// profileFor decides on a person's profile image: nil keeps what is kept
// (TMDB's is the same, or its image host does not have it), gone drops the kept
// one (TMDB has none), anything else is the new image.
func (s *Service) profileFor(ctx context.Context, path, keptPath string, hasProfile bool) (*profileImage, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		if hasProfile {
			return &profileImage{gone: true}, nil
		}
		return nil, nil
	}
	if path == keptPath {
		return nil, nil
	}
	b, err := s.tmdb.download(ctx, s.tmdb.imageURL(path, profileSize))
	if isNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("profile %s: %w", path, err)
	}
	img := &profileImage{bytes: b, contentType: imageType(b), sourcePath: path}
	if img.contentType == "" {
		return nil, fmt.Errorf("profile %s is not a JPEG, PNG or WebP image", path)
	}
	sum := sha256.Sum256(b)
	img.sha256 = hex.EncodeToString(sum[:])
	if cfg, _, err := image.DecodeConfig(bytes.NewReader(b)); err == nil {
		w, h := cfg.Width, cfg.Height
		img.width, img.height = &w, &h
	}
	return img, nil
}

// storePerson writes what TMDB gave into the fields the locks leave open, in
// one transaction with the profile image. A field TMDB has nothing for is
// cleared. Each field written says it came from TMDB; one cleared says
// nothing. modifiedat moves only when a value does; tmdbfetchedat always.
func (s *Service) storePerson(ctx context.Context, personID string, p *tmdbPerson, bios map[string]string, img *profileImage, changedOn *time.Time) (personOutcome, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)

	var lk locks
	var lockedRaw, originsRaw, bioRaw []byte
	var birth, death *string
	err = tx.QueryRow(ctx, `SELECT metadatalocked, lockedfields, fieldorigins, biography,
			to_char(birthdate, 'YYYY-MM-DD'), to_char(deathdate, 'YYYY-MM-DD')
		FROM com_nalet_katalog_people WHERE id = $1 FOR UPDATE`, personID).
		Scan(&lk.all, &lockedRaw, &originsRaw, &bioRaw, &birth, &death)
	if err != nil {
		return 0, fmt.Errorf("lock person %s: %w", personID, err)
	}
	if lk.all { // locked while TMDB was being read
		return personLocked, nil
	}
	lk.fields = jsonStrings(lockedRaw)
	if lk.has(fieldImages) { // locked while TMDB was being read
		img = nil
	}
	u := personUpdate{locks: lk, origins: jsonStringMap(originsRaw), args: []any{personID}}

	if name := clip(oneLine(p.Name), 255); name != "" {
		u.field(fieldName, "name", "text", name, true)
	}
	aka := aliases(p.AlsoKnownAs, oneLine(p.Name))
	u.field(fieldAlsoKnownAs, "alsoknownas", "jsonb", jsonOf(aka), len(aka) > 0)
	born, died := tmdbDate(p.Birthday), tmdbDate(p.Deathday)
	if lk.has(fieldBirthDate) && birth != nil {
		born = *birth // a death is checked against the birth the person keeps
	}
	if born != "" && died != "" && died < born {
		died = "" // TMDB's dates contradict each other; the record keeps no death before birth
	}
	u.field(fieldBirthDate, "birthdate", "date", tmdbDate(p.Birthday), tmdbDate(p.Birthday) != "")
	u.field(fieldDeathDate, "deathdate", "date", died, died != "")
	place := oneLine(p.PlaceOfBirth)
	u.field(fieldBirthPlace, "birthplace", "text", place, place != "")
	dept := oneLine(p.KnownForDepartment)
	u.field(fieldKnownForDepartment, "knownfordepartment", "text", dept, dept != "")
	imdb := imdbPersonID(p.ImdbID)
	u.field(fieldExternalIDs, "imdbid", "text", imdb, imdb != "")
	if !lk.has(fieldExternalIDs) {
		u.origins[fieldExternalIDs] = originTMDB // the TMDB id at least is TMDB's
	}
	if !lk.has(fieldBiography) {
		bio, fromTMDB := jsonStringMap(bioRaw), false
		for lang, text := range bios {
			if text == "" {
				delete(bio, lang)
				continue
			}
			bio[lang], fromTMDB = text, true
		}
		u.field(fieldBiography, "biography", "jsonb", jsonOf(bio), len(bio) > 0)
		if len(bio) > 0 && !fromTMDB {
			u.origins = restore(u.origins, jsonStringMap(originsRaw), fieldBiography) // not TMDB's text
		}
	}
	if img != nil {
		if img.gone {
			delete(u.origins, fieldImages)
		} else {
			u.origins[fieldImages] = originTMDB
		}
		if _, err := tx.Exec(ctx, `DELETE FROM com_nalet_katalog_personartwork
			WHERE person_id = $1 AND kind = 'profile'`, personID); err != nil {
			return 0, err
		}
		if !img.gone {
			if _, err := tx.Exec(ctx, `INSERT INTO com_nalet_katalog_personartwork
				(id, person_id, kind, contenttype, bytes, sha256, width, height, isprimary, sourcepath, fetchedat)
				VALUES (gen_random_uuid()::varchar, $1, 'profile', $2, $3, $4, $5, $6, true, $7, now())`,
				personID, img.contentType, img.bytes, img.sha256, img.width, img.height, img.sourcePath); err != nil {
				return 0, err
			}
		}
	}

	var changed any
	if changedOn != nil {
		changed = changedOn.UTC().Format(time.DateOnly)
	}
	sets, diffs := u.sets, u.diffs
	u.args = append(u.args, jsonOf(u.origins))
	n := strconv.Itoa(len(u.args))
	sets = append(sets, "fieldorigins = $"+n+"::jsonb")
	diffs = append(diffs, "fieldorigins IS DISTINCT FROM $"+n+"::jsonb")
	u.args = append(u.args, img != nil, changed)
	imageChanged, changedDay := "$"+strconv.Itoa(len(u.args)-1)+"::boolean", "$"+strconv.Itoa(len(u.args))+"::date"
	if _, err := tx.Exec(ctx, `UPDATE com_nalet_katalog_people SET `+strings.Join(sets, ", ")+`,
			tmdbfetchedat = now(),
			tmdbchangedat = GREATEST(tmdbchangedat, `+changedDay+`),
			modifiedat = CASE WHEN `+imageChanged+` OR `+strings.Join(diffs, " OR ")+`
			                  THEN now() ELSE modifiedat END
		WHERE id = $1`, u.args...); err != nil {
		return 0, fmt.Errorf("store person %s: %w", personID, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return personRefreshed, nil
}

// personUpdate collects the columns one person refresh writes.
type personUpdate struct {
	locks   locks
	origins map[string]string
	sets    []string // column = $n::type
	diffs   []string // column IS DISTINCT FROM $n::type
	args    []any
}

// field writes value into column unless field is locked; set false clears the
// column. The field's origin follows: TMDB when set, none when cleared.
func (u *personUpdate) field(field, column, typ string, value any, set bool) {
	if u.locks.has(field) {
		return
	}
	if !set {
		value = nil
	}
	u.args = append(u.args, value)
	ref := "$" + strconv.Itoa(len(u.args)) + "::" + typ
	u.sets = append(u.sets, column+" = "+ref)
	u.diffs = append(u.diffs, column+" IS DISTINCT FROM "+ref)
	if set {
		u.origins[field] = originTMDB
	} else {
		delete(u.origins, field)
	}
}

// restore puts field's origin back to what it was in was.
func restore(origins, was map[string]string, field string) map[string]string {
	if o, ok := was[field]; ok {
		origins[field] = o
	} else {
		delete(origins, field)
	}
	return origins
}

// ===================== normalising what TMDB says =====================

// languageKey is the key a biography read in a TMDB language is kept under:
// its primary language subtag ("de-DE" → "de"), which a reader matches its
// viewer's language against.
func languageKey(tmdbLanguage string) string {
	l := strings.ToLower(strings.TrimSpace(tmdbLanguage))
	if i := strings.IndexAny(l, "-_"); i >= 0 {
		l = l[:i]
	}
	if len(l) < 2 || len(l) > 3 || strings.Trim(l, "abcdefghijklmnopqrstuvwxyz") != "" {
		return "en" // what TMDB answers in when the language is not one it knows
	}
	return l
}

// paragraphs is a text that may span lines: line breaks normalised to \n,
// control characters gone, and no space around it.
func paragraphs(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.Map(func(r rune) rune {
		switch {
		case r == '\r':
			return '\n'
		case r == '\n' || r == '\t':
			return r
		case r < 0x20 || r == 0x7f:
			return -1
		}
		return r
	}, s)
	return strings.TrimSpace(s)
}

// aliases are the other names a person is known by, as the record keeps them:
// each on one line, none blank or the name itself, each once, sorted.
func aliases(names []string, name string) []string {
	seen := map[string]bool{name: true, "": true}
	var out []string
	for _, n := range names {
		if n = oneLine(n); !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

// clip cuts s to at most n characters, what VARCHAR(n) holds.
func clip(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return strings.TrimSpace(string(r[:n]))
	}
	return s
}

// tmdbDate is a YYYY-MM-DD date TMDB gives, "" for anything else.
func tmdbDate(s string) string {
	s = strings.TrimSpace(s)
	if _, err := time.Parse(time.DateOnly, s); err != nil {
		return ""
	}
	return s
}

var imdbPersonRE = regexp.MustCompile(`^nm[0-9]+$`)

// imdbPersonID is an IMDb person id ("nm…"), "" for anything else.
func imdbPersonID(s string) string {
	if s = strings.TrimSpace(s); imdbPersonRE.MatchString(s) {
		return s
	}
	return ""
}

// imageType is the type of an image TMDB serves, from its bytes; "" when it is
// not a JPEG, PNG or WebP image.
func imageType(b []byte) string {
	switch t := http.DetectContentType(b); t {
	case "image/jpeg", "image/png", "image/webp":
		return t
	}
	return ""
}

// jsonOf is v as JSON, for a jsonb parameter.
func jsonOf(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

// jsonStrings reads a JSON array, keeping its strings.
func jsonStrings(raw []byte) []string {
	var vs []any
	_ = json.Unmarshal(raw, &vs)
	var out []string
	for _, v := range vs {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// jsonStringMap reads a JSON object, keeping its string values.
func jsonStringMap(raw []byte) map[string]string {
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	out := map[string]string{}
	for k, v := range m {
		if s, ok := v.(string); ok {
			out[k] = s
		}
	}
	return out
}

// creditLinks says what storing one title's credits did.
type creditLinks struct {
	matched  int      // people without a TMDB id that a credit gave theirs
	created  int      // credited people the catalog did not hold yet
	relinked int      // links to a namesake, replaced by a link to the credited person
	people   []string // the credited people, by catalog id, each once
	failed   int      // credits that could not be stored
}

// linkCredits finds or creates every credited person by their TMDB id and links
// them to the item. A link the item has to someone else of a credited name, in
// the same role, is a credit that was once matched by name alone, to a
// namesake: it goes, the link to the credited person stays. Links to names the
// credits do not carry are left alone.
func (s *Service) linkCredits(ctx context.Context, itemID string, c *tmdbCredits) creditLinks {
	var out creditLinks
	type credited struct{ names, ids []string }
	byRole := map[string]*credited{}
	seen := map[string]bool{}
	add := func(role string, cr tmdbCredit) {
		name := clip(oneLine(cr.Name), 255)
		if name == "" {
			return
		}
		personID, how, err := s.findOrCreatePerson(ctx, cr.ID, name)
		if err != nil {
			log.Printf("tmdb: credit %q (TMDB person %d) of item %s: %v", name, cr.ID, itemID, err)
			out.failed++
			return
		}
		switch how {
		case personMatched:
			out.matched++
		case personCreated:
			out.created++
		}
		if err := s.linkPerson(ctx, itemID, personID, role); err != nil {
			log.Printf("tmdb: link %s as %s of item %s: %v", personID, role, itemID, err)
			out.failed++
			return
		}
		r := byRole[role]
		if r == nil {
			r = &credited{}
			byRole[role] = r
		}
		r.names, r.ids = append(r.names, name), append(r.ids, personID)
		if !seen[personID] {
			seen[personID] = true
			out.people = append(out.people, personID)
		}
	}
	for _, d := range c.Crew {
		add(roleDirector, d)
	}
	for _, a := range c.Cast {
		add(roleActor, a)
	}
	for role, r := range byRole {
		tag, err := s.pool.Exec(ctx, `DELETE FROM com_nalet_katalog_itempeople ip
			USING com_nalet_katalog_people p
			WHERE ip.item_id = $1 AND ip.role = $2 AND p.id = ip.person_id
			  AND p.name = ANY($3::text[]) AND NOT (ip.person_id = ANY($4::text[]))`,
			itemID, role, r.names, r.ids)
		if err != nil {
			log.Printf("tmdb: replace links to namesakes of item %s: %v", itemID, err)
			continue
		}
		out.relinked += int(tag.RowsAffected())
	}
	return out
}

// How findOrCreatePerson came by a person.
type personFind int

const (
	personFound   personFind = iota // by TMDB id, or by name for a credit without one
	personMatched                   // a person without a TMDB id, by name: they carry it now
	personCreated                   // new
)

// findOrCreatePerson returns the person a credit names: the one with its TMDB
// id; failing that the first person of that name who has no TMDB id yet (the
// people saved before TMDB ids were kept), who gets the id; failing that a new
// person. A person who has a TMDB id is never taken for another TMDB id, so two
// people of one name stay two. Two enrichments that meet the same new person at
// once end up with the same row: the TMDB id is unique.
func (s *Service) findOrCreatePerson(ctx context.Context, tmdbID int64, name string) (string, personFind, error) {
	if tmdbID <= 0 {
		return s.personByName(ctx, name)
	}
	key := strconv.FormatInt(tmdbID, 10)
	for attempt := 0; attempt < 3; attempt++ {
		var id string
		err := s.pool.QueryRow(ctx,
			`SELECT id FROM com_nalet_katalog_people WHERE tmdbpersonid = $1`, key).Scan(&id)
		if err == nil {
			return id, personFound, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return "", 0, err
		}
		err = s.pool.QueryRow(ctx, `UPDATE com_nalet_katalog_people SET
				tmdbpersonid = $1,
				fieldorigins = COALESCE(fieldorigins, '{}'::jsonb) || '{"externalIds": "tmdb"}'::jsonb,
				modifiedat = now()
			WHERE id = (SELECT id FROM com_nalet_katalog_people
			            WHERE name = $2 AND tmdbpersonid IS NULL ORDER BY id LIMIT 1)
			  AND tmdbpersonid IS NULL
			RETURNING id`, key, name).Scan(&id)
		if err == nil {
			return id, personMatched, nil
		}
		if isUniqueViolation(err) {
			continue // the id was given to someone meanwhile: find them
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return "", 0, err
		}
		err = s.pool.QueryRow(ctx, `INSERT INTO com_nalet_katalog_people
				(id, name, tmdbpersonid, fieldorigins, createdat, modifiedat)
			VALUES (gen_random_uuid()::varchar, $2, $1, '{"name": "tmdb", "externalIds": "tmdb"}', now(), now())
			ON CONFLICT (tmdbpersonid) WHERE tmdbpersonid IS NOT NULL DO NOTHING
			RETURNING id`, key, name).Scan(&id)
		if err == nil {
			return id, personCreated, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return "", 0, err
		}
		// created meanwhile by another enrichment: find them on the next pass
	}
	return "", 0, fmt.Errorf("TMDB person %s was created and taken concurrently; giving up", key)
}

// personByName is how a credit without a TMDB id finds its person: by name,
// preferring someone without a TMDB id, as before ids were kept.
func (s *Service) personByName(ctx context.Context, name string) (string, personFind, error) {
	var id string
	err := s.pool.QueryRow(ctx, `SELECT id FROM com_nalet_katalog_people WHERE name = $1
		ORDER BY tmdbpersonid IS NULL DESC, id LIMIT 1`, name).Scan(&id)
	if err == nil {
		return id, personFound, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", 0, err
	}
	err = s.pool.QueryRow(ctx, `INSERT INTO com_nalet_katalog_people (id, name, fieldorigins, createdat, modifiedat)
		VALUES (gen_random_uuid()::varchar, $1, '{"name": "tmdb"}', now(), now()) RETURNING id`, name).Scan(&id)
	return id, personCreated, err
}

// linkPerson links a person to an item in a role, once.
func (s *Service) linkPerson(ctx context.Context, itemID, personID, role string) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO com_nalet_katalog_itempeople (id, item_id, person_id, role)
		SELECT gen_random_uuid()::varchar, $1::text, $2::text, $3::text
		WHERE NOT EXISTS (SELECT 1 FROM com_nalet_katalog_itempeople
		                  WHERE item_id = $1::text AND person_id = $2::text AND role = $3::text)`,
		itemID, personID, role)
	return err
}

// upsertPersonByName is how credits were stored before migration 030: the
// person found or created by name, and linked to the item in the role.
func (s *Service) upsertPersonByName(ctx context.Context, itemID, name, role string) {
	if strings.TrimSpace(name) == "" {
		return
	}
	var personID string
	err := s.pool.QueryRow(ctx,
		`SELECT id FROM com_nalet_katalog_people WHERE name = $1`, name).Scan(&personID)
	if err == pgx.ErrNoRows {
		personID = ""
	} else if err != nil {
		return
	}
	if personID == "" {
		if err := s.pool.QueryRow(ctx,
			`INSERT INTO com_nalet_katalog_people (id, name) VALUES (gen_random_uuid()::varchar, $1) RETURNING id`,
			name).Scan(&personID); err != nil {
			return
		}
	}
	_ = s.linkPerson(ctx, itemID, personID, role)
}

func isUniqueViolation(err error) bool {
	var pe *pgconn.PgError
	return errors.As(err, &pe) && pe.Code == "23505"
}

// oneLine is a single-line text as the library record keeps one: line breaks
// become spaces, control characters go, and so does the space around it.
func oneLine(s string) string {
	s = strings.Map(func(r rune) rune {
		switch {
		case r == '\r' || r == '\n' || r == '\t':
			return ' '
		case r < 0x20 || r == 0x7f:
			return -1
		}
		return r
	}, s)
	return strings.TrimSpace(s)
}
