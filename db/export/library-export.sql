-- The catalog as one JSON document, for building or comparing the library
-- record on storage (zaentrum/schemas tools/library-v2-*.py). Run with
--   psql -U <user> -d <db> -At -f library-export.sql > catalog.json
-- Read-only. Artwork bytes travel base64-encoded.
-- One JSON document with everything the v2 record builder needs from a katalog
-- database. Artwork bytes travel base64-encoded; everything else is plain JSON.
-- Run it as before: psql -Atf - < catalog-export.sql > catalog.json
--
-- An item's people are its credits, in the order a title lists them: by role
-- (actor, creator, director, writer, producer, composer, cinematographer,
-- editor, then any other role, by role), then by order (unknown last), then
-- by name:
--   [{"personId", "name", "role", "job", "character", "order", "episodeCount"}]
-- job is the person's jobs in the role joined with ", ", character whom an
-- actor plays, order the credit's place in its role (0 first), episodeCount a
-- series' episodes the person is credited in, in the role; each null when
-- unknown, as on a catalog without migration 032, whose credits are read
-- through to_jsonb of their rows for that reason.
--
-- deletedItems is katalog-manager's deletion log (db/migrations/029), so that a
-- verification can tell a record the catalog deleted (an orphan: sweep it) from
-- one it lost (restore it): [{"id","type","deletedAt","deletedBy"}], ordered by
-- id, deletedAt in UTC, one entry per id (its latest deletion). type is the
-- item's type (movie, series, episode, ...), or person for a person the catalog
-- deleted because no title credits them any more. An id that is in both items
-- (or people) and deletedItems was re-created after it was deleted: present
-- wins. On a catalog without the log (older than 029) it is null, not [],
-- because nothing there can be called deleted.
--
-- people is every person the catalog holds (db/migrations/030), ordered by id:
--   [{"id", "name", "sortName", "alsoKnownAs": [], "birthDate", "deathDate",
--     "birthPlace", "biography": {<language>: text},
--     "externalIds": {"tmdbPerson", "imdb"}, "knownForDepartment",
--     "metadataLocked", "lockedFields": [], "fieldOrigins": {<field>: origin},
--     "tmdbFetchedAt", "tmdbChangedAt", "modifiedAt",
--     "artwork": [{"kind": "profile", "contentType", "base64", "sha256",
--                  "width", "height", "isPrimary", "sourcePath", "fetchedAt"}]}]
-- Timestamps are UTC (YYYY-MM-DDTHH:MM:SSZ), dates YYYY-MM-DD, sha256 is
-- "sha256:<hex>" as the library record writes it, base64 has no line breaks,
-- the primary image comes first. lockedFields and fieldOrigins name fields as
-- person.json does (images is the artwork); sourcePath is TMDB's file path.
-- On a catalog without 030 it is null, not [], like deletedItems without 029:
-- such a catalog holds no person records, only the names its credits carry.
--
-- extras, versions and sources are what the library-v2 migration
-- (library-v2-from-catalog.py --platform) stages records from, beside the
-- items: each item's extras that are part of it (db/migrations/039, removedat
-- null), its package runs that are not removed and its originals
-- (db/migrations/040), ordered by itemId and id:
--   extras:   [{"id", "itemId", "kind", "title", "localizedTitles": {}, "language",
--               "seasonNumber", "origin": {}|null, "sourcePath", "sourceSize", "sourceQh1",
--               "recordPath", "registeredBy", "sortOrder", "hidden", "label", "state",
--               "packageId", "packagePath", "packagedAt", "recordedAt", "durationMs",
--               "createdAt", "createdBy", "modifiedAt"}]
--   versions: [{"id", "itemId", "state", "sourceIds": [], "packageId", "dir", "completedAt",
--               "verifiedAt", "verifiedLevel", "supersededBy", "supersededAt"}]
--   sources:  [{"id", "itemId", "filename", "arrivalPath", "libraryPath", "sizeBytes", "qh1",
--               "state", "recordedAt", "recordDir", "sidecars": []|null, "deletedAt"}]
-- On a catalog without 039 extras is null, and without 040 versions and
-- sources are, like deletedItems without 029.
--
-- shard narrows the export to one shard of the library: the items whose
-- folder is in it, by left(coalesce(seriesId, id), 2) — an episode's and a
-- season's seriesId is their series' (an episode under a season, its
-- grandparent), so a series and its episodes are one shard — and the people
-- and the extras, versions and sources of those items. deletedItems is never
-- narrowed. shard is null in the document when it was not asked for:
--   psql -U <user> -d <db> -At -v shard=f0 -f library-export.sql > catalog-f0.json
-- Every timestamp below is formatted in the session's zone: pin it, so the
-- export says the same thing whatever the server's default zone is. Image
-- bytes read through table_to_xml come in the session's xmlbinary: pin that
-- too.
\if :{?shard}
\else
\set shard ''
\endif
\set QUIET on
set time zone 'UTC';
set xmlbinary to base64;
with shard as (
  -- every item of the shard asked for, by the shard its library folder is in
  select i.id
  from com_nalet_katalog_items i
  left join com_nalet_katalog_items p on p.id = i.parent_id
  where :'shard' = ''
     or left(coalesce(case when i.type = 'episode' then case when p.type = 'season' then p.parent_id else p.id end
                           when i.type = 'season' then i.parent_id end, i.id), 2) = :'shard'
)
select json_build_object(
    'exportedAt', to_char(now() at time zone 'utc', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'),
    'shard', nullif(:'shard', ''),
    'items', coalesce((select json_agg(x order by x->>'id') from (
      select json_build_object(
        'id', i.id, 'type', i.type, 'title', i.title, 'sortTitle', i.sorttitle, 'year', i.year,
        'description', i.description, 'tagline', i.tagline, 'rating', i.rating,
        'durationMs', i.durationms, 'parentId', i.parent_id,
        'seasonNumber', i.seasonnumber, 'episodeNumber', i.episodenumber,
        'metadataLocked', i.metadatalocked,
        'createdAt', to_char(i.createdat at time zone 'utc', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'),
        'createdBy', i.createdby,
        'modifiedAt', to_char(i.modifiedat at time zone 'utc', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'),
        'externalIds', (select coalesce(json_agg(json_build_object('source', e.source, 'externalId', e.externalid)), '[]') from com_nalet_katalog_itemexternalids e where e.item_id = i.id),
        'genres', (select coalesce(json_agg(g.name order by g.name), '[]') from com_nalet_katalog_itemgenres ig join com_nalet_katalog_genres g on g.id = ig.genre_id where ig.item_id = i.id),
        'tags', (select coalesce(json_agg(t.tag order by t.tag), '[]') from com_nalet_katalog_itemtags t where t.item_id = i.id),
        'people', (select coalesce(json_agg(json_build_object('personId', c.person_id, 'name', c.name, 'role', c.role, 'job', c.d->>'job', 'character', c.d->>'charactername', 'order', (c.d->>'ordinal')::int, 'episodeCount', (c.d->>'episodecount')::int)
                     order by array_position(array['actor', 'creator', 'director', 'writer', 'producer', 'composer', 'cinematographer', 'editor'], c.role::text) nulls last, c.role, (c.d->>'ordinal')::int nulls last, c.name, c.person_id, c.id), '[]')
                   from (select ip.id, ip.person_id, ip.role, p.name, to_jsonb(ip) as d from com_nalet_katalog_itempeople ip join com_nalet_katalog_people p on p.id = ip.person_id where ip.item_id = i.id) c),
        'chapters', (select coalesce(json_agg(json_build_object('startMs', c.startms, 'endMs', c.endms, 'title', c.title, 'ordinal', c.ordinal) order by c.ordinal), '[]') from com_nalet_katalog_itemchapters c where c.item_id = i.id),
        'segments', (select coalesce(json_agg(json_build_object('kind', s.kind, 'startMs', s.startms, 'endMs', s.endms, 'source', s.source, 'confidence', s.confidence, 'label', s.label) order by s.startms), '[]') from com_nalet_katalog_mediasegments s where s.item_id = i.id),
        'playbackAssets', (select coalesce(json_agg(json_build_object('id', a.id, 'path', a.path, 'kind', a.kind, 'codec', a.codec, 'resolution', a.resolution, 'bitrateKbps', a.bitratekbps, 'sizeBytes', a.sizebytes, 'hash', a.hash, 'isPrimary', a.isprimary, 'audioCodec', a.audiocodec, 'audioLanguage', a.audiolanguage, 'audioChannels', a.audiochannels, 'audioBitrateKbps', a.audiobitratekbps, 'audioTrackCount', a.audiotrackcount, 'subtitleTrackCount', a.subtitletrackcount, 'durationMs', a.durationms)), '[]') from com_nalet_katalog_playbackassets a where a.item_id = i.id),
        'subtitleAssets', (select coalesce(json_agg(json_build_object('id', s.id, 'path', s.path, 'format', s.format, 'language', s.lang, 'label', s.label, 'isDefault', s.isdefault)), '[]') from com_nalet_katalog_subtitleassets s where s.item_id = i.id),
        'trailers', (select coalesce(json_agg(json_build_object('source', tl.source, 'site', tl.site, 'externalId', tl.externalid, 'url', tl.url, 'title', tl.title, 'durationSec', tl.durationsec, 'localPath', tl.localpath)), '[]') from com_nalet_katalog_itemtrailerlinks tl where tl.item_id = i.id),
        'artwork', (select coalesce(json_agg(json_build_object('kind', d.kind, 'contentType', d.contenttype, 'fetchedAt', to_char(d.fetchedat at time zone 'utc', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'), 'base64', replace(encode(d.bytes, 'base64'), E'\n', ''))), '[]') from com_nalet_katalog_itemartworkdata d where d.item_id = i.id and d.bytes is not null)
      ) as x
      from com_nalet_katalog_items i
      where i.id in (select id from shard)
    ) s), '[]'),
    -- The log is read through table_to_xml, which takes the table by name when
    -- the query runs: naming it directly would fail the whole export wherever
    -- the table does not exist yet.
    'deletedItems', case when to_regclass('com_nalet_katalog_deleteditems') is null then null else (
      select coalesce(json_agg(json_build_object(
               'id', d.id,
               'type', d.type,
               'deletedAt', to_char(d.deletedat, 'YYYY-MM-DD"T"HH24:MI:SS"Z"'),
               'deletedBy', d.deletedby) order by d.id), '[]')
      from xmltable('//row'
             passing table_to_xml(to_regclass('com_nalet_katalog_deleteditems'), false, false, '')
             columns id text path 'id', type text path 'type', deletedat timestamp path 'deletedat',
                     deletedby text path 'deletedby') d
    ) end,
    -- The query is planned even where it does not run, so it may not name what
    -- a catalog older than 030 lacks: a person is read through to_jsonb of their
    -- row, which carries the columns the table has, and their images through
    -- table_to_xml, like the log.
    'people', case when to_regclass('com_nalet_katalog_personartwork') is null then null else (
      with art as (
        select a.person_id, json_agg(json_build_object(
                 'kind', a.kind,
                 'contentType', a.contenttype,
                 'base64', translate(a.b64, E'\r\n', ''),
                 'sha256', 'sha256:' || a.sha256,
                 'width', a.width,
                 'height', a.height,
                 'isPrimary', coalesce(a.isprimary, false),
                 'sourcePath', a.sourcepath,
                 'fetchedAt', to_char(a.fetchedat at time zone 'utc', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'))
               order by a.isprimary desc nulls last, a.fetchedat desc nulls last, a.id) as list
        from xmltable('//row'
               passing table_to_xml(to_regclass('com_nalet_katalog_personartwork'), false, false, '')
               columns id text path 'id', person_id text path 'person_id', kind text path 'kind',
                       contenttype text path 'contenttype', b64 text path 'bytes', sha256 text path 'sha256',
                       width integer path 'width', height integer path 'height',
                       isprimary boolean path 'isprimary', sourcepath text path 'sourcepath',
                       fetchedat timestamptz path 'fetchedat') a
        where a.b64 is not null
        group by a.person_id
      )
      select coalesce(json_agg(json_build_object(
          'id', p.r->>'id',
          'name', p.r->>'name',
          'sortName', p.r->>'sortname',
          'alsoKnownAs', case when jsonb_typeof(p.r->'alsoknownas') = 'array' then p.r->'alsoknownas' else '[]' end,
          'birthDate', to_char((p.r->>'birthdate')::date, 'YYYY-MM-DD'),
          'deathDate', to_char((p.r->>'deathdate')::date, 'YYYY-MM-DD'),
          'birthPlace', p.r->>'birthplace',
          'biography', case when jsonb_typeof(p.r->'biography') = 'object' then p.r->'biography' else '{}' end,
          'externalIds', json_build_object('tmdbPerson', p.r->>'tmdbpersonid', 'imdb', p.r->>'imdbid'),
          'knownForDepartment', p.r->>'knownfordepartment',
          'metadataLocked', coalesce((p.r->>'metadatalocked')::boolean, false),
          'lockedFields', case when jsonb_typeof(p.r->'lockedfields') = 'array' then p.r->'lockedfields' else '[]' end,
          'fieldOrigins', case when jsonb_typeof(p.r->'fieldorigins') = 'object' then p.r->'fieldorigins' else '{}' end,
          'tmdbFetchedAt', to_char((p.r->>'tmdbfetchedat')::timestamptz at time zone 'utc', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'),
          'tmdbChangedAt', to_char((p.r->>'tmdbchangedat')::date, 'YYYY-MM-DD'),
          'modifiedAt', to_char((p.r->>'modifiedat')::timestamptz at time zone 'utc', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'),
          'artwork', coalesce((select art.list from art where art.person_id = p.r->>'id'), '[]')
        ) order by p.r->>'id'), '[]')
      from (select to_jsonb(x) as r from com_nalet_katalog_people x
            where :'shard' = '' or x.id in (select ip.person_id from com_nalet_katalog_itempeople ip
                                            where ip.item_id in (select id from shard))) p
    ) end,
    -- 039 and 040 are read through query_to_xml, which plans the query it is
    -- given only when it runs: naming their tables directly would fail the
    -- whole export wherever they do not exist yet. Each row comes as its
    -- row_to_json, so arrays, objects and timestamps keep their types.
    'extras', case when to_regclass('com_nalet_katalog_itemextras') is null then null else (
      select coalesce(json_agg(json_build_object(
          'id', x.r->>'id', 'itemId', x.r->>'item_id', 'kind', x.r->>'kind', 'title', x.r->>'title',
          'localizedTitles', coalesce(x.r->'localizedtitles', '{}'), 'language', x.r->>'language',
          'seasonNumber', (x.r->>'seasonnumber')::int, 'origin', x.r->'origin',
          'sourcePath', x.r->>'sourcepath', 'sourceSize', (x.r->>'sourcesize')::bigint,
          'sourceQh1', x.r->>'sourceqh1', 'recordPath', x.r->>'recordpath', 'registeredBy', x.r->>'registeredby',
          'sortOrder', (x.r->>'sortorder')::int, 'hidden', coalesce((x.r->>'hidden')::boolean, false),
          'label', x.r->>'label', 'state', x.r->>'state', 'packageId', x.r->>'packageid',
          'packagePath', x.r->>'packagepath',
          'packagedAt', to_char((x.r->>'packagedat')::timestamptz at time zone 'utc', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'),
          'recordedAt', to_char((x.r->>'recordedat')::timestamptz at time zone 'utc', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'),
          'durationMs', (x.r->>'durationms')::bigint,
          'createdAt', to_char((x.r->>'createdat')::timestamptz at time zone 'utc', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'),
          'createdBy', x.r->>'createdby',
          'modifiedAt', to_char((x.r->>'modifiedat')::timestamptz at time zone 'utc', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'))
        order by x.r->>'item_id', x.r->>'id'), '[]')
      from (select j::jsonb as r from xmltable('//row'
              passing query_to_xml('select row_to_json(t)::text as j from com_nalet_katalog_itemextras t', false, false, '')
              columns j text path 'j')) x
      where x.r->>'removedat' is null and x.r->>'item_id' in (select id from shard)
    ) end,
    'versions', case when to_regclass('com_nalet_katalog_itemversions') is null then null else (
      select coalesce(json_agg(json_build_object(
          'id', v.r->>'id', 'itemId', v.r->>'item_id', 'state', v.r->>'state',
          'sourceIds', coalesce(v.r->'sourceids', '[]'), 'packageId', v.r->>'packageid', 'dir', v.r->>'dir',
          'completedAt', to_char((v.r->>'completedat')::timestamptz at time zone 'utc', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'),
          'verifiedAt', to_char((v.r->>'verifiedat')::timestamptz at time zone 'utc', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'),
          'verifiedLevel', v.r->>'verifiedlevel', 'supersededBy', v.r->>'supersededby',
          'supersededAt', to_char((v.r->>'supersededat')::timestamptz at time zone 'utc', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'))
        order by v.r->>'item_id', v.r->>'id'), '[]')
      from (select j::jsonb as r from xmltable('//row'
              passing query_to_xml('select row_to_json(t)::text as j from com_nalet_katalog_itemversions t', false, false, '')
              columns j text path 'j')) v
      where v.r->>'state' <> 'removed' and v.r->>'item_id' in (select id from shard)
    ) end,
    'sources', case when to_regclass('com_nalet_katalog_itemsources') is null then null else (
      select coalesce(json_agg(json_build_object(
          'id', o.r->>'id', 'itemId', o.r->>'item_id', 'filename', o.r->>'filename',
          'arrivalPath', o.r->>'arrivalpath', 'libraryPath', o.r->>'librarypath',
          'sizeBytes', (o.r->>'sizebytes')::bigint, 'qh1', o.r->>'qh1', 'state', o.r->>'state',
          'recordedAt', to_char((o.r->>'recordedat')::timestamptz at time zone 'utc', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'),
          'recordDir', o.r->>'recorddir', 'sidecars', o.r->'sidecars',
          'deletedAt', to_char((o.r->>'deletedat')::timestamptz at time zone 'utc', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'))
        order by o.r->>'item_id', o.r->>'id'), '[]')
      from (select j::jsonb as r from xmltable('//row'
              passing query_to_xml('select row_to_json(t)::text as j from com_nalet_katalog_itemsources t', false, false, '')
              columns j text path 'j')) o
      where o.r->>'item_id' in (select id from shard)
    ) end
  );
