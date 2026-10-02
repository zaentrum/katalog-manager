-- The catalog as one JSON document, for building or comparing the library
-- record on storage (zaentrum/schemas tools/library-v2-*.py). Run with
--   psql -U <user> -d <db> -At -f library-export.sql > catalog.json
-- Read-only. Artwork bytes travel base64-encoded.
-- One JSON document with everything the v2 record builder needs from a katalog
-- database. Artwork bytes travel base64-encoded; everything else is plain JSON.
-- Run it as before: psql -Atf - < catalog-export.sql > catalog.json
--
-- deletedItems is katalog-manager's deletion log (db/migrations/029), so that a
-- verification can tell a record the catalog deleted (an orphan: sweep it) from
-- one it lost (restore it): [{"id","deletedAt","deletedBy"}], ordered by id,
-- deletedAt in UTC, one entry per id (its latest deletion). An id that is in
-- both items and deletedItems was re-created after it was deleted: present wins.
-- On a catalog without the log (older than 029) it is null, not [], because
-- nothing there can be called deleted.
-- Every timestamp below is formatted in the session's zone: pin it, so the
-- export says the same thing whatever the server's default zone is.
set time zone 'UTC';
select json_build_object(
    'exportedAt', to_char(now() at time zone 'utc', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'),
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
        'people', (select coalesce(json_agg(json_build_object('personId', p.id, 'name', p.name, 'role', ip.role)), '[]') from com_nalet_katalog_itempeople ip join com_nalet_katalog_people p on p.id = ip.person_id where ip.item_id = i.id),
        'chapters', (select coalesce(json_agg(json_build_object('startMs', c.startms, 'endMs', c.endms, 'title', c.title, 'ordinal', c.ordinal) order by c.ordinal), '[]') from com_nalet_katalog_itemchapters c where c.item_id = i.id),
        'segments', (select coalesce(json_agg(json_build_object('kind', s.kind, 'startMs', s.startms, 'endMs', s.endms, 'source', s.source, 'confidence', s.confidence, 'label', s.label) order by s.startms), '[]') from com_nalet_katalog_mediasegments s where s.item_id = i.id),
        'playbackAssets', (select coalesce(json_agg(json_build_object('id', a.id, 'path', a.path, 'kind', a.kind, 'codec', a.codec, 'resolution', a.resolution, 'bitrateKbps', a.bitratekbps, 'sizeBytes', a.sizebytes, 'hash', a.hash, 'isPrimary', a.isprimary, 'audioCodec', a.audiocodec, 'audioLanguage', a.audiolanguage, 'audioChannels', a.audiochannels, 'audioBitrateKbps', a.audiobitratekbps, 'audioTrackCount', a.audiotrackcount, 'subtitleTrackCount', a.subtitletrackcount, 'durationMs', a.durationms)), '[]') from com_nalet_katalog_playbackassets a where a.item_id = i.id),
        'subtitleAssets', (select coalesce(json_agg(json_build_object('id', s.id, 'path', s.path, 'format', s.format, 'language', s.lang, 'label', s.label, 'isDefault', s.isdefault)), '[]') from com_nalet_katalog_subtitleassets s where s.item_id = i.id),
        'trailers', (select coalesce(json_agg(json_build_object('source', tl.source, 'site', tl.site, 'externalId', tl.externalid, 'url', tl.url, 'title', tl.title, 'durationSec', tl.durationsec, 'localPath', tl.localpath)), '[]') from com_nalet_katalog_itemtrailerlinks tl where tl.item_id = i.id),
        'artwork', (select coalesce(json_agg(json_build_object('kind', d.kind, 'contentType', d.contenttype, 'fetchedAt', to_char(d.fetchedat at time zone 'utc', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'), 'base64', replace(encode(d.bytes, 'base64'), E'\n', ''))), '[]') from com_nalet_katalog_itemartworkdata d where d.item_id = i.id and d.bytes is not null)
      ) as x
      from com_nalet_katalog_items i
    ) s), '[]'),
    -- The log is read through table_to_xml, which takes the table by name when
    -- the query runs: naming it directly would fail the whole export wherever
    -- the table does not exist yet.
    'deletedItems', case when to_regclass('com_nalet_katalog_deleteditems') is null then null else (
      select coalesce(json_agg(json_build_object(
               'id', d.id,
               'deletedAt', to_char(d.deletedat, 'YYYY-MM-DD"T"HH24:MI:SS"Z"'),
               'deletedBy', d.deletedby) order by d.id), '[]')
      from xmltable('//row'
             passing table_to_xml(to_regclass('com_nalet_katalog_deleteditems'), false, false, '')
             columns id text path 'id', deletedat timestamp path 'deletedat', deletedby text path 'deletedby') d
    ) end
  );
