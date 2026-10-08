package library

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/zaentrum/katalog-manager/internal/config"
)

// The projector writes the library's projections from the database: an
// item's metadata.json and the images it lists in metadata/, a person's
// person.json and their portraits. It follows the database's mark (migration
// 041): an item whose modifiedat is newer than the libraryprojectedat its
// last projection reflects, a person likewise; only a recorded item (its
// item.json written) and a person a recorded item credits are projected. It
// never reads the tree, but to leave an image written already as it is (an
// image is named by its bytes) and to see that an item's folder holds its
// record. A person the catalog deleted loses their folder. It runs with the
// setting library.layout=v2, every 15 seconds, in one instance at a time.

// ProjectorInterval is how often the projector looks for what changed.
const ProjectorInterval = 15 * time.Second

// projectorBatch is how many items or people a pass reads at once.
const projectorBatch = 200

// imageHead is how much of an image's bytes tell its type and size.
const imageHead = 64 << 10

// Projector writes the projections of a catalog.
type Projector struct {
	pool *pgxpool.Pool
	cfg  config.Config
	now  func() time.Time

	mu      sync.Mutex
	cursor  time.Time       // the deletion log is read from here
	missing map[string]bool // items whose folder holds no record, said once
	said    string
}

// NewProjector is the projector of the catalog pool, its library where cfg
// puts it.
func NewProjector(pool *pgxpool.Pool, cfg config.Config) *Projector {
	return &Projector{pool: pool, cfg: cfg, now: time.Now, missing: map[string]bool{}}
}

// Run projects every interval until ctx ends, while the layout is v2.
func (p *Projector) Run(ctx context.Context) {
	t := time.NewTicker(ProjectorInterval)
	defer t.Stop()
	for {
		if items, people, err := p.Pass(ctx); err != nil {
			p.say("library: the projector: " + err.Error())
		} else {
			p.said = ""
			if items+people > 0 {
				log.Printf("library: projected %d items and %d people", items, people)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// say logs a problem once while it lasts.
func (p *Projector) say(msg string) {
	if msg != p.said {
		log.Print(msg)
		p.said = msg
	}
}

// Pass projects what changed, once, under the projector's lock: another
// instance projecting, this one does nothing. It answers how many items and
// people it projected; with the legacy layout, none.
func (p *Projector) Pass(ctx context.Context) (items, people int, err error) {
	set, err := ReadSettings(ctx, p.pool)
	if err != nil || !set.V2() {
		return 0, 0, err
	}
	conn, err := p.pool.Acquire(ctx)
	if err != nil {
		return 0, 0, err
	}
	defer conn.Release()
	var locked bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock(hashtext('library-projector'))`).Scan(&locked); err != nil || !locked {
		return 0, 0, err
	}
	defer func() {
		_, _ = conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock(hashtext('library-projector'))`)
	}()
	p.mu.Lock()
	defer p.mu.Unlock()
	if items, err = p.items(ctx); err != nil {
		return items, 0, err
	}
	if people, err = p.people(ctx); err != nil {
		return items, people, err
	}
	return items, people, p.deleted(ctx)
}

// items projects the items whose projection is behind, a batch at a time.
func (p *Projector) items(ctx context.Context) (int, error) {
	done := 0
	for {
		rows, err := p.pool.Query(ctx, `SELECT id FROM com_nalet_katalog_items
			WHERE recordedat IS NOT NULL AND (libraryprojectedat IS NULL OR modifiedat > libraryprojectedat)
			  AND NOT (id = ANY($2::text[]))
			ORDER BY modifiedat NULLS FIRST, id LIMIT $1`, projectorBatch, p.skipped())
		if err != nil {
			return done, err
		}
		var ids []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return done, err
			}
			ids = append(ids, id)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return done, err
		}
		for _, id := range ids {
			ok, err := p.ProjectItem(ctx, id)
			if err != nil {
				return done, fmt.Errorf("the projection of item %s: %w", id, err)
			}
			if ok {
				done++
			}
		}
		if len(ids) < projectorBatch || ctx.Err() != nil {
			return done, ctx.Err()
		}
	}
}

// skipped are the items whose folder holds no record, which the pass leaves
// alone.
func (p *Projector) skipped() []string {
	out := []string{}
	for id := range p.missing {
		out = append(out, id)
	}
	return out
}

// ProjectItem writes the item id's projection: the images it lists that its
// metadata/ folder does not hold yet, then metadata.json; then the catalog
// notes the state it reflects. An item whose folder holds no record is left
// alone (false), said once.
func (p *Projector) ProjectItem(ctx context.Context, id string) (bool, error) {
	pr, err := p.projectItem(ctx, p.pool, id)
	if err != nil || pr == nil {
		return false, err
	}
	_, err = p.pool.Exec(ctx, `UPDATE com_nalet_katalog_items SET libraryprojectedat = COALESCE($2::timestamp, '-infinity')
		WHERE id = $1`, id, pr.modified)
	return err == nil, err
}

// projected is what writing an item's projection did: its folder, the
// modifiedat the projection reflects, and the images it wrote that the
// folder did not hold.
type projected struct {
	dir      string
	modified *time.Time
	images   []string
}

// projectItem writes the item id's projection as q reads the catalog (the
// pool, or a transaction whose changes it reflects): nil for an item whose
// folder holds no record, said once. The caller notes the state it
// reflects.
func (p *Projector) projectItem(ctx context.Context, q Querier, id string) (*projected, error) {
	r, err := p.render(ctx, q, id, Timestamp(p.now()), "katalog-manager")
	if err != nil || r == nil {
		return nil, err
	}
	images, err := writeImages(filepath.Join(r.dir, "metadata"), r.files)
	if err != nil {
		return nil, err
	}
	if err := WriteRecord(filepath.Join(r.dir, "metadata.json"), r.doc); err != nil {
		return nil, err
	}
	return &projected{dir: r.dir, modified: r.modified, images: images}, nil
}

// rendered is an item's projection as the projector writes it, not
// written: its folder, metadata.json's record, the images it lists, and
// the modifiedat it reflects.
type rendered struct {
	dir      string
	doc      Doc
	files    []ImageFile
	modified *time.Time
}

// render makes the item id's projection as q reads the catalog, its asOf
// and projectedBy as given: nil for an item whose folder holds no record,
// said once. It writes nothing.
func (p *Projector) render(ctx context.Context, q Querier, id, asOf, by string) (*rendered, error) {
	paths := PathsOf(p.cfg)
	pl, err := PlaceOf(ctx, q, id)
	var unplaced *Unplaced
	if errors.As(err, &unplaced) || errors.Is(err, ErrNoItem) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	dir := paths.ItemDir(pl)
	if _, err := os.Stat(filepath.Join(dir, ItemFile)); err != nil {
		if !p.missing[id] {
			log.Printf("library: item %s is recorded, and its folder %s holds no item.json: no projection of it is written", id, dir)
			p.missing[id] = true
		}
		return nil, nil
	}
	var rowJSON []byte
	var modified *time.Time
	if err := q.QueryRow(ctx, itemSnapshot, id).Scan(&rowJSON, &modified); err != nil {
		return nil, err
	}
	row, err := DecodeDoc(rowJSON)
	if err != nil {
		return nil, err
	}
	proj := ItemProjection{ProjectedBy: by, AsOf: asOf, ExternalIDs: true}
	if proj.Images, err = p.artwork(ctx, q, `SELECT id, kind, encode(sha256(bytes), 'hex'), length(bytes),
			substring(bytes FROM 1 FOR $2), false, NULL::text, to_char(fetchedat, 'YYYY-MM-DD"T"HH24:MI:SS"Z"')
		FROM com_nalet_katalog_itemartworkdata WHERE item_id = $1 AND bytes IS NOT NULL ORDER BY kind, id`,
		`SELECT bytes FROM com_nalet_katalog_itemartworkdata WHERE id = $1`, id); err != nil {
		return nil, err
	}
	if pl.Type == "series" {
		if proj.Seasons, err = p.seasons(ctx, q, id); err != nil {
			return nil, err
		}
	}
	// An episode another's file covers plays its holder's version, and names
	// it; a holder numbers itself up to the last episode its file covers.
	plays := id
	if pl.Type == "episode" {
		if proj.CoveredBy, err = HolderOf(ctx, q, id); err != nil {
			return nil, err
		}
		if proj.CoveredBy != "" {
			plays = proj.CoveredBy
		} else if proj.EpisodeEnd, err = episodeEndOf(ctx, q, id); err != nil {
			return nil, err
		}
	}
	if v, err := Current(ctx, q, plays); err != nil {
		return nil, err
	} else if v != nil {
		proj.PrimaryVersionID = v.ID
	}
	if pl.Type != "episode" {
		if proj.Extras, err = p.extras(ctx, q, id); err != nil {
			return nil, err
		}
	}
	doc, files, err := MetadataDoc(row, proj)
	if err != nil {
		return nil, err
	}
	return &rendered{dir: dir, doc: doc, files: files, modified: modified}, nil
}

// itemSnapshot reads an item as the catalog's export holds one
// (db/export/library-export.sql), in one statement, and the modifiedat it
// reflects. Its timestamps are UTC without a zone, written as they are.
const itemSnapshot = `SELECT json_build_object(
		'id', i.id, 'type', i.type, 'title', i.title, 'sortTitle', i.sorttitle, 'year', i.year,
		'description', i.description, 'tagline', i.tagline, 'rating', i.rating,
		'durationMs', i.durationms, 'parentId', i.parent_id,
		'seasonNumber', i.seasonnumber, 'episodeNumber', i.episodenumber,
		'metadataLocked', i.metadatalocked,
		'createdAt', to_char(i.createdat, 'YYYY-MM-DD"T"HH24:MI:SS"Z"'),
		'createdBy', i.createdby,
		'modifiedAt', to_char(i.modifiedat, 'YYYY-MM-DD"T"HH24:MI:SS"Z"'),
		'externalIds', (SELECT coalesce(json_agg(json_build_object('source', e.source, 'externalId', e.externalid)
			ORDER BY e.id), '[]') FROM com_nalet_katalog_itemexternalids e WHERE e.item_id = i.id),
		'genres', (SELECT coalesce(json_agg(g.name ORDER BY g.name), '[]') FROM com_nalet_katalog_itemgenres ig
			JOIN com_nalet_katalog_genres g ON g.id = ig.genre_id WHERE ig.item_id = i.id),
		'tags', (SELECT coalesce(json_agg(t.tag ORDER BY t.tag), '[]') FROM com_nalet_katalog_itemtags t WHERE t.item_id = i.id),
		'people', (SELECT coalesce(json_agg(json_build_object('personId', c.person_id, 'name', c.name, 'role', c.role,
				'job', c.d->>'job', 'character', c.d->>'charactername', 'order', (c.d->>'ordinal')::int,
				'episodeCount', (c.d->>'episodecount')::int) ORDER BY c.person_id, c.role, c.id), '[]')
			FROM (SELECT ip.id, ip.person_id, ip.role, p.name, to_jsonb(ip) AS d FROM com_nalet_katalog_itempeople ip
				JOIN com_nalet_katalog_people p ON p.id = ip.person_id WHERE ip.item_id = i.id) c),
		'trailers', (SELECT coalesce(json_agg(json_build_object('source', tl.source, 'site', tl.site,
				'externalId', tl.externalid, 'url', tl.url, 'title', tl.title, 'durationSec', tl.durationsec,
				'localPath', tl.localpath) ORDER BY tl.id), '[]')
			FROM com_nalet_katalog_itemtrailerlinks tl WHERE tl.item_id = i.id)
	)::text, i.modifiedat
	FROM com_nalet_katalog_items i WHERE i.id = $1`

// artwork reads the images of a row (list: id, kind, sha256 hex, size,
// first bytes, primary, source path, fetched), each read whole by one
// (whole) when it is written, as q reads them.
func (p *Projector) artwork(ctx context.Context, q Querier, list, whole string, owner string) ([]ImageIn, error) {
	rows, err := q.Query(ctx, list, owner, imageHead)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ImageIn
	for rows.Next() {
		var id string
		var in ImageIn
		var fetched *string
		if err := rows.Scan(&id, &in.Kind, &in.SHA256, &in.Size, &in.Head, &in.Primary, &in.SourcePath, &fetched); err != nil {
			return nil, err
		}
		if fetched != nil {
			in.FetchedAt = *fetched
		}
		in.Bytes = func() ([]byte, error) {
			var b []byte
			err := q.QueryRow(ctx, whole, id).Scan(&b)
			return b, err
		}
		out = append(out, in)
	}
	return out, rows.Err()
}

// seasons are the season numbers of the series' episodes, under it or under
// a season of it, each once.
func (p *Projector) seasons(ctx context.Context, q Querier, seriesID string) ([]int64, error) {
	rows, err := q.Query(ctx, `SELECT DISTINCT e.seasonnumber FROM com_nalet_katalog_items e
		LEFT JOIN com_nalet_katalog_items s ON s.id = e.parent_id
		WHERE e.type = 'episode' AND e.seasonnumber IS NOT NULL
		  AND (e.parent_id = $1 OR (s.type <> 'series' AND s.parent_id = $1))
		ORDER BY 1`, seriesID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var n int64
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// episodeEndOf is the number of the last episode the file of the episode
// holder covers, of its own season; nil when it covers no other.
func episodeEndOf(ctx context.Context, q Querier, holder string) (*int64, error) {
	covered, err := CoveredOf(ctx, q, holder)
	if err != nil || len(covered) == 0 {
		return nil, err
	}
	var season *int32
	if err := q.QueryRow(ctx, `SELECT seasonnumber FROM com_nalet_katalog_items WHERE id = $1`, holder).Scan(&season); err != nil {
		return nil, err
	}
	var end *int64
	for _, c := range covered {
		if c.Episode != nil && season != nil && c.Season != nil && *c.Season == *season {
			n := int64(*c.Episode)
			if end == nil || n > *end {
				end = &n
			}
		}
	}
	return end, nil
}

// extras are the decisions a person made about how the item's recorded
// extras are shown: an order, hidden, a label; an extra without any is not
// listed.
func (p *Projector) extras(ctx context.Context, q Querier, itemID string) (Doc, error) {
	rows, err := q.Query(ctx, `SELECT id, sortorder, hidden, label FROM com_nalet_katalog_itemextras
		WHERE item_id = $1 AND recordedat IS NOT NULL AND removedat IS NULL ORDER BY id`, itemID)
	if isUndefinedTable(err) || isUndefinedColumn(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := Doc{}
	for rows.Next() {
		var id string
		var order *int32
		var hidden bool
		var label *string
		if err := rows.Scan(&id, &order, &hidden, &label); err != nil {
			return nil, err
		}
		d := Doc{}
		if order != nil && *order >= 0 {
			d = append(d, Field{"order", int64(*order)})
		}
		if hidden {
			d = append(d, Field{"hidden", true})
		}
		if label != nil && strings.TrimSpace(*label) != "" {
			d = append(d, Field{"label", strings.TrimSpace(*label)})
		}
		if len(d) > 0 {
			out = append(out, Field{id, d})
		}
	}
	return out, rows.Err()
}

func isUndefinedColumn(err error) bool {
	var pe *pgconn.PgError
	return errors.As(err, &pe) && pe.Code == "42703"
}

// writeImages writes the images a projection lists that dir does not hold
// yet: an image is written once, by the name of its bytes. It answers the
// names it wrote.
func writeImages(dir string, files []ImageFile) ([]string, error) {
	var wrote []string
	for _, f := range files {
		path := filepath.Join(dir, f.Name)
		if _, err := os.Stat(path); err == nil {
			continue
		}
		if f.Bytes == nil {
			return wrote, fmt.Errorf("the bytes of %s cannot be read", f.Name)
		}
		b, err := f.Bytes()
		if err != nil {
			return wrote, err
		}
		if err := WriteFile(path, b); err != nil {
			return wrote, err
		}
		wrote = append(wrote, f.Name)
	}
	return wrote, nil
}

// people projects the people a recorded item credits whose projection is
// behind; a catalog without migration 030 keeps no person to project.
func (p *Projector) people(ctx context.Context) (int, error) {
	done := 0
	for {
		rows, err := p.pool.Query(ctx, `SELECT p.id FROM com_nalet_katalog_people p
			WHERE (p.libraryprojectedat IS NULL OR p.modifiedat > p.libraryprojectedat)
			  AND EXISTS (SELECT 1 FROM com_nalet_katalog_itempeople ip JOIN com_nalet_katalog_items i ON i.id = ip.item_id
			              WHERE ip.person_id = p.id AND i.recordedat IS NOT NULL)
			ORDER BY p.id LIMIT $1`, projectorBatch)
		if isUndefinedColumn(err) {
			return done, nil
		}
		if err != nil {
			return done, err
		}
		var ids []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return done, err
			}
			ids = append(ids, id)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return done, err
		}
		for _, id := range ids {
			if err := p.ProjectPerson(ctx, id); err != nil {
				return done, fmt.Errorf("the projection of person %s: %w", id, err)
			}
			done++
		}
		if len(ids) < projectorBatch || ctx.Err() != nil {
			return done, ctx.Err()
		}
	}
}

// personSnapshot reads a person as the catalog's export holds one, and the
// modifiedat it reflects.
const personSnapshot = `SELECT json_build_object(
		'id', p.r->>'id', 'name', p.r->>'name', 'sortName', p.r->>'sortname',
		'alsoKnownAs', CASE WHEN jsonb_typeof(p.r->'alsoknownas') = 'array' THEN p.r->'alsoknownas' ELSE '[]' END,
		'birthDate', to_char((p.r->>'birthdate')::date, 'YYYY-MM-DD'),
		'deathDate', to_char((p.r->>'deathdate')::date, 'YYYY-MM-DD'),
		'birthPlace', p.r->>'birthplace',
		'biography', CASE WHEN jsonb_typeof(p.r->'biography') = 'object' THEN p.r->'biography' ELSE '{}' END,
		'externalIds', json_build_object('tmdbPerson', p.r->>'tmdbpersonid', 'imdb', p.r->>'imdbid'),
		'knownForDepartment', p.r->>'knownfordepartment',
		'metadataLocked', coalesce((p.r->>'metadatalocked')::boolean, false),
		'lockedFields', CASE WHEN jsonb_typeof(p.r->'lockedfields') = 'array' THEN p.r->'lockedfields' ELSE '[]' END,
		'fieldOrigins', CASE WHEN jsonb_typeof(p.r->'fieldorigins') = 'object' THEN p.r->'fieldorigins' ELSE '{}' END,
		'tmdbFetchedAt', to_char((p.r->>'tmdbfetchedat')::timestamptz AT TIME ZONE 'utc', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'),
		'tmdbChangedAt', to_char((p.r->>'tmdbchangedat')::date, 'YYYY-MM-DD'),
		'modifiedAt', to_char((p.r->>'modifiedat')::timestamptz AT TIME ZONE 'utc', 'YYYY-MM-DD"T"HH24:MI:SS"Z"')
	)::text, (p.r->>'modifiedat')::timestamptz
	FROM (SELECT to_jsonb(x) AS r FROM com_nalet_katalog_people x WHERE x.id = $1) p`

// ProjectPerson writes the person id's projection, person.json and the
// portraits it lists, and the catalog notes the state it reflects.
func (p *Projector) ProjectPerson(ctx context.Context, id string) error {
	var rowJSON []byte
	var modified *time.Time
	err := p.pool.QueryRow(ctx, personSnapshot, id).Scan(&rowJSON, &modified)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	row, err := DecodeDoc(rowJSON)
	if err != nil {
		return err
	}
	proj := PersonProjection{ProjectedBy: "katalog-manager", AsOf: Timestamp(p.now())}
	if proj.Images, err = p.artwork(ctx, p.pool, `SELECT id, kind, encode(sha256(bytes), 'hex'), length(bytes),
			substring(bytes FROM 1 FOR $2), COALESCE(isprimary, false), sourcepath,
			to_char(fetchedat AT TIME ZONE 'utc', 'YYYY-MM-DD"T"HH24:MI:SS"Z"')
		FROM com_nalet_katalog_personartwork WHERE person_id = $1 AND bytes IS NOT NULL
		ORDER BY isprimary DESC NULLS LAST, fetchedat DESC NULLS LAST, id`,
		`SELECT bytes FROM com_nalet_katalog_personartwork WHERE id = $1`, id); err != nil && !isUndefinedTable(err) {
		return err
	}
	doc, files, err := PersonDoc(id, row, proj)
	if err != nil || doc == nil {
		return err
	}
	dir := PathsOf(p.cfg).PersonDir(id)
	if _, err := writeImages(dir, files); err != nil {
		return err
	}
	if err := WriteRecord(filepath.Join(dir, "person.json"), doc); err != nil {
		return err
	}
	_, err = p.pool.Exec(ctx, `UPDATE com_nalet_katalog_people SET libraryprojectedat = COALESCE($2::timestamptz, '-infinity')
		WHERE id = $1`, id, modified)
	return err
}

// deleted removes the folders of the people the catalog deleted (the
// deletion log's people that are not there again): read from where the last
// pass left off, a few minutes back, so that a deletion committed late is
// seen too.
func (p *Projector) deleted(ctx context.Context) error {
	rows, err := p.pool.Query(ctx, `SELECT d.id, d.deletedat FROM com_nalet_katalog_deleteditems d
		WHERE d.type = 'person' AND d.deletedat > $1
		  AND NOT EXISTS (SELECT 1 FROM com_nalet_katalog_people x WHERE x.id = d.id)
		ORDER BY d.deletedat, d.id`, p.cursor.Add(-10*time.Minute))
	if isUndefinedTable(err) {
		return nil
	}
	if err != nil {
		return err
	}
	type gone struct {
		id string
		at time.Time
	}
	var all []gone
	for rows.Next() {
		var g gone
		if err := rows.Scan(&g.id, &g.at); err != nil {
			rows.Close()
			return err
		}
		all = append(all, g)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	sort.Slice(all, func(i, j int) bool { return all[i].at.Before(all[j].at) })
	paths := PathsOf(p.cfg)
	for _, g := range all {
		if !ValidID(g.id) {
			continue
		}
		dir := paths.PersonDir(g.id)
		if _, err := os.Stat(dir); err == nil {
			if err := os.RemoveAll(dir); err != nil {
				return err
			}
			log.Printf("library: person %s was deleted by the catalog: their folder %s is gone", g.id, dir)
			_ = os.Remove(filepath.Dir(dir)) // the shard, once empty
		}
		if g.at.After(p.cursor) {
			p.cursor = g.at
		}
	}
	return nil
}

// ErrProjectorBusy says another projection held the projector's lock for as
// long as a refresh waits for it.
var ErrProjectorBusy = errors.New("another projection runs")

// refreshWait is how long a refresh waits for the projector's lock.
const refreshWait = 30 * time.Second

// RefreshResult is what a refresh did to an item's projection: projected,
// unchanged (it reflects the item already) or failed, and why.
type RefreshResult struct {
	ItemID string `json:"itemId"`
	State  string `json:"state"`
	Reason string `json:"reason,omitempty"`
}

// RefreshReport is what a refresh did.
type RefreshReport struct {
	Projected int             `json:"projected"`
	Unchanged int             `json:"unchanged"`
	Failed    int             `json:"failed"`
	Items     []RefreshResult `json:"items"`
}

// Refresh projects now, whatever the layout, the items ids, or with none
// named every recorded item, whose projection on storage is not what the
// projector would write now: each is rendered and compared, byte for byte,
// with its metadata.json (its asOf and projectedBy the file's own), and the
// images it lists looked for. One that differs, or lacks an image, is
// written: projected, its reason "behind" when the time marks say so too
// (the file's databaseUpdatedAt is not the item's modifiedat, or the
// catalog marks it behind), "shape" when they do not (a projection the
// marks call current, written in a shape from before). One that is the
// same is left unchanged, its mark set when it was behind; one not recorded
// fails. With items named the report lists each of them, with none the ones
// projected or failed. It waits for the projector's lock
// (ErrProjectorBusy).
func (p *Projector) Refresh(ctx context.Context, ids []string) (RefreshReport, error) {
	rep := RefreshReport{Items: []RefreshResult{}}
	conn, err := p.pool.Acquire(ctx)
	if err != nil {
		return rep, err
	}
	defer conn.Release()
	deadline := time.Now().Add(refreshWait)
	for {
		var locked bool
		if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock(hashtext('library-projector'))`).Scan(&locked); err != nil {
			return rep, err
		}
		if locked {
			break
		}
		if time.Now().After(deadline) {
			return rep, ErrProjectorBusy
		}
		select {
		case <-ctx.Done():
			return rep, ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
	defer func() {
		_, _ = conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock(hashtext('library-projector'))`)
	}()
	p.mu.Lock()
	defer p.mu.Unlock()
	all := len(ids) == 0
	if all {
		rows, err := p.pool.Query(ctx, `SELECT id FROM com_nalet_katalog_items WHERE recordedat IS NOT NULL ORDER BY id`)
		if err != nil {
			return rep, err
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return rep, err
			}
			ids = append(ids, id)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return rep, err
		}
	}
	for _, id := range ids {
		if ctx.Err() != nil {
			return rep, ctx.Err()
		}
		res := p.refresh(ctx, id)
		switch res.State {
		case "projected":
			rep.Projected++
		case "unchanged":
			rep.Unchanged++
		default:
			rep.Failed++
		}
		if !all || res.State != "unchanged" {
			rep.Items = append(rep.Items, res)
		}
	}
	return rep, nil
}

// refresh projects the item id when its projection on storage does not
// reflect it.
func (p *Projector) refresh(ctx context.Context, id string) RefreshResult {
	res := RefreshResult{ItemID: id}
	fail := func(why string) RefreshResult {
		res.State, res.Reason = "failed", why
		return res
	}
	var recorded, projectedAt, modified *time.Time
	var updated *string
	err := p.pool.QueryRow(ctx, `SELECT recordedat, libraryprojectedat, modifiedat,
			to_char(modifiedat, 'YYYY-MM-DD"T"HH24:MI:SS"Z"')
		FROM com_nalet_katalog_items WHERE id = $1`, id).Scan(&recorded, &projectedAt, &modified, &updated)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return fail("unknown item")
	case err != nil:
		return fail(err.Error())
	case recorded == nil:
		return fail("not recorded: its item.json is not written")
	}
	pl, err := PlaceOf(ctx, p.pool, id)
	if err != nil {
		return fail("not recorded: " + err.Error())
	}
	dir := PathsOf(p.cfg).ItemDir(pl)
	if !statOK(filepath.Join(dir, ItemFile)) {
		return fail("not recorded: " + dir + " holds no item.json")
	}
	// The time marks: whether the file says the item's modifiedat, and the
	// catalog marks it current.
	fresh, asOf, by := false, "", ""
	disk, err := os.ReadFile(filepath.Join(dir, "metadata.json"))
	if err == nil {
		if d, err := DecodeDoc(disk); err == nil {
			v, has := d.Get("databaseUpdatedAt")
			at, _ := v.(string)
			fresh = (has && updated != nil && at == *updated) || (!has && updated == nil)
			asOf, by = str(d, "asOf"), str(d, "projectedBy")
		}
	}
	marked := projectedAt != nil && (modified == nil || !modified.After(*projectedAt))
	// The content: the projection as the projector would write it now, but
	// for its moment and its writer, which are the file's.
	same := false
	if disk != nil && asOf != "" && by != "" {
		r, err := p.render(ctx, p.pool, id, asOf, by)
		if err != nil {
			return fail(err.Error())
		}
		if r == nil {
			return fail("not recorded: " + dir + " holds no item.json")
		}
		b, err := Encode(r.doc)
		if err != nil {
			return fail(err.Error())
		}
		same = bytes.Equal(b, disk)
		for _, f := range r.files {
			if same && !statOK(filepath.Join(dir, "metadata", f.Name)) {
				same = false
			}
		}
	}
	if same {
		if !marked {
			if _, err := p.pool.Exec(ctx, `UPDATE com_nalet_katalog_items SET libraryprojectedat = COALESCE($2::timestamp, '-infinity')
				WHERE id = $1`, id, modified); err != nil {
				return fail(err.Error())
			}
		}
		res.State = "unchanged"
		return res
	}
	ok, err := p.ProjectItem(ctx, id)
	switch {
	case err != nil:
		return fail(err.Error())
	case !ok:
		return fail("not recorded: " + dir + " holds no item.json")
	}
	res.Reason = "behind"
	if fresh && marked {
		res.Reason = "shape"
	}
	res.State = "projected"
	return res
}
