VIEW com_nalet_katalog_itemoverallstatus
 SELECT i.id AS item_id,
        CASE
            WHEN (count(s.id) FILTER (WHERE ((s.status)::text <> ALL ((ARRAY['skipped'::character varying, 'not_applicable'::character varying])::text[]))) = 0) THEN 'pending'::text
            WHEN ((count(*) FILTER (WHERE ((s.status)::text = 'failed'::text)) > 0) AND (count(*) FILTER (WHERE ((s.status)::text = ANY ((ARRAY['pending'::character varying, 'in_progress'::character varying])::text[]))) = 0) AND (count(*) FILTER (WHERE ((s.status)::text = 'done'::text)) > 0)) THEN 'partial_failure'::text
            WHEN ((count(*) FILTER (WHERE ((s.status)::text = 'failed'::text)) > 0) AND (count(*) FILTER (WHERE ((s.status)::text = ANY ((ARRAY['pending'::character varying, 'in_progress'::character varying])::text[]))) = 0)) THEN 'failed'::text
            WHEN (count(*) FILTER (WHERE ((s.status)::text = 'in_progress'::text)) > 0) THEN 'processing'::text
            WHEN (count(*) FILTER (WHERE ((s.status)::text = 'pending'::text)) > 0) THEN 'queued'::text
            WHEN (count(*) FILTER (WHERE ((s.status)::text = 'done'::text)) > 0) THEN 'complete'::text
            ELSE 'not_applicable'::text
        END AS overallstatus,
    count(*) FILTER (WHERE ((s.status)::text = 'done'::text)) AS donecount,
    count(*) FILTER (WHERE ((s.status)::text = 'pending'::text)) AS pendingcount,
    count(*) FILTER (WHERE ((s.status)::text = 'failed'::text)) AS failedcount,
    count(*) FILTER (WHERE ((s.status)::text = 'in_progress'::text)) AS inprogresscount,
    count(*) FILTER (WHERE ((s.status)::text = 'not_applicable'::text)) AS notapplicablecount,
    count(s.id) AS totalsteps,
    max(s.finishedat) AS laststepfinishedat
   FROM (com_nalet_katalog_items i
     LEFT JOIN com_nalet_katalog_itemprocessingsteps s ON (((s.item_id)::text = (i.id)::text)))
  GROUP BY i.id;
VIEW katalogservice_albums
 SELECT id,
    createdat,
    createdby,
    modifiedat,
    modifiedby,
    type,
    title,
    sorttitle,
    year,
    description,
    rating,
    durationms,
    parent_id,
    seasonnumber,
    episodenumber,
    tagline,
    (('/katalog-api/api/artwork/'::text || (id)::text) || '/poster'::text) AS posterurl,
    (('/katalog-api/api/artwork/'::text || (id)::text) || '/backdrop'::text) AS backdropurl,
    (durationms / 60000) AS runtimemin,
    (year)::character varying(255) AS yeartext
   FROM com_nalet_katalog_items items_0
  WHERE ((type)::text = 'album'::text);
VIEW katalogservice_downloadjobs
 SELECT id,
    createdat,
    createdby,
    modifiedat,
    modifiedby,
    adapter,
    clientjobid,
    title,
    wanteditemid,
    state,
    progresspct,
    downloadedbytes,
    sizebytes,
    speedbps,
    etasec,
    files,
    errormessage,
    startedat,
    completedat,
    lasteventat,
        CASE state
            WHEN 'failed'::text THEN 1
            WHEN 'downloading'::text THEN 2
            WHEN 'queued'::text THEN 2
            WHEN 'completed'::text THEN 3
            ELSE 0
        END AS statecriticality
   FROM com_nalet_katalog_downloadjobs downloadjobs_0;
VIEW katalogservice_enrichmentstatuscodes
 SELECT code,
    name
   FROM com_nalet_katalog_enrichmentstatuscodes enrichmentstatuscodes_0;
VIEW katalogservice_episodes
 SELECT id,
    createdat,
    createdby,
    modifiedat,
    modifiedby,
    type,
    title,
    sorttitle,
    year,
    description,
    rating,
    durationms,
    parent_id,
    seasonnumber,
    episodenumber,
    tagline,
    (('/katalog-api/api/artwork/'::text || (id)::text) || '/poster'::text) AS posterurl,
    (('/katalog-api/api/artwork/'::text || (id)::text) || '/backdrop'::text) AS backdropurl,
    (durationms / 60000) AS runtimemin,
    (year)::character varying(255) AS yeartext,
        CASE
            WHEN (EXISTS ( SELECT 1 AS dummy
               FROM com_nalet_katalog_mediasegments _segments_exists_1
              WHERE (((_segments_exists_1.item_id)::text = (items_0.id)::text) AND ((_segments_exists_1.kind)::text = 'intro'::text)))) THEN true
            ELSE false
        END AS hasintro,
        CASE
            WHEN (EXISTS ( SELECT 1 AS dummy
               FROM com_nalet_katalog_mediasegments _segments_exists_2
              WHERE (((_segments_exists_2.item_id)::text = (items_0.id)::text) AND ((_segments_exists_2.kind)::text = 'credits'::text)))) THEN true
            ELSE false
        END AS hascredits,
        CASE
            WHEN (EXISTS ( SELECT 1 AS dummy
               FROM com_nalet_katalog_mediasegments _segments_exists_3
              WHERE (((_segments_exists_3.item_id)::text = (items_0.id)::text) AND ((_segments_exists_3.kind)::text = 'recap'::text)))) THEN true
            ELSE false
        END AS hasrecap,
        CASE
            WHEN (EXISTS ( SELECT 1 AS dummy
               FROM com_nalet_katalog_playbackassets _assets_exists_4
              WHERE (((_assets_exists_4.item_id)::text = (items_0.id)::text) AND (((_assets_exists_4.codec)::text ~~ 'hev1%'::text) OR ((_assets_exists_4.codec)::text ~~ 'hvc1%'::text))))) THEN true
            ELSE false
        END AS ispackaged
   FROM com_nalet_katalog_items items_0
  WHERE ((type)::text = 'episode'::text);
VIEW katalogservice_genres
 SELECT id,
    name
   FROM com_nalet_katalog_genres genres_0;
VIEW katalogservice_itemartwork
 SELECT id,
    item_id,
    kind,
    url
   FROM com_nalet_katalog_itemartwork itemartwork_0;
VIEW katalogservice_itemartworkdata
 SELECT id,
    item_id,
    kind,
    contenttype,
    bytes,
    fetchedat
   FROM com_nalet_katalog_itemartworkdata itemartworkdata_0;
VIEW katalogservice_itemchapters
 SELECT id,
    createdat,
    createdby,
    modifiedat,
    modifiedby,
    item_id,
    startms,
    endms,
    title,
    ordinal
   FROM com_nalet_katalog_itemchapters itemchapters_0;
VIEW katalogservice_itemdiagnostics
 SELECT id,
    item_id,
    generatedat,
    sourcepath,
    sourcesize,
    sourcemtime,
    ffprobedata,
    folderlisting,
    notes
   FROM com_nalet_katalog_itemdiagnostics itemdiagnostics_0;
VIEW katalogservice_itemexternalids
 SELECT id,
    item_id,
    source,
    externalid
   FROM com_nalet_katalog_itemexternalids itemexternalids_0;
VIEW katalogservice_itemgenres
 SELECT id,
    item_id,
    genre_id
   FROM com_nalet_katalog_itemgenres itemgenres_0;
VIEW katalogservice_itemoverallstatus
 SELECT item_id,
    overallstatus,
    donecount,
    pendingcount,
    failedcount,
    inprogresscount,
    notapplicablecount,
    totalsteps,
    laststepfinishedat
   FROM com_nalet_katalog_itemoverallstatus;
VIEW katalogservice_itempeople
 SELECT id,
    item_id,
    person_id,
    role
   FROM com_nalet_katalog_itempeople itempeople_0;
VIEW katalogservice_itemprocessingsteps
 SELECT id,
    createdat,
    createdby,
    modifiedat,
    modifiedby,
    item_id,
    step,
    status,
    startedat,
    finishedat,
    attempts,
    error,
    details,
        CASE status
            WHEN 'failed'::text THEN 1
            WHEN 'in_progress'::text THEN 2
            WHEN 'pending'::text THEN 2
            WHEN 'done'::text THEN 3
            ELSE 0
        END AS statuscriticality
   FROM com_nalet_katalog_itemprocessingsteps s;
VIEW katalogservice_items
 SELECT id,
    createdat,
    createdby,
    modifiedat,
    modifiedby,
    type,
    title,
    sorttitle,
    year,
    description,
    rating,
    durationms,
    parent_id,
    seasonnumber,
    episodenumber,
    tagline,
    (('/katalog-api/api/artwork/'::text || (id)::text) || '/poster'::text) AS posterurl,
    (('/katalog-api/api/artwork/'::text || (id)::text) || '/backdrop'::text) AS backdropurl,
    (durationms / 60000) AS runtimemin,
    (year)::character varying(255) AS yeartext
   FROM com_nalet_katalog_items items_0;
VIEW katalogservice_itemtags
 SELECT id,
    item_id,
    tag
   FROM com_nalet_katalog_itemtags itemtags_0;
VIEW katalogservice_itemtrailerlinks
 SELECT id,
    createdat,
    createdby,
    modifiedat,
    modifiedby,
    item_id,
    source,
    site,
    externalid,
    url,
    title,
    durationsec,
    publishedat,
    downloadedat,
    localpath
   FROM com_nalet_katalog_itemtrailerlinks itemtrailerlinks_0;
VIEW katalogservice_mediasegments
 SELECT id,
    createdat,
    createdby,
    modifiedat,
    modifiedby,
    item_id,
    kind,
    startms,
    endms,
    source,
    confidence,
    label
   FROM com_nalet_katalog_mediasegments mediasegments_0;
VIEW katalogservice_movies
 SELECT id,
    createdat,
    createdby,
    modifiedat,
    modifiedby,
    type,
    title,
    sorttitle,
    year,
    description,
    rating,
    durationms,
    parent_id,
    seasonnumber,
    episodenumber,
    tagline,
    (('/katalog-api/api/artwork/'::text || (id)::text) || '/poster'::text) AS posterurl,
    (('/katalog-api/api/artwork/'::text || (id)::text) || '/backdrop'::text) AS backdropurl,
    (durationms / 60000) AS runtimemin,
    (year)::character varying(255) AS yeartext,
        CASE
            WHEN (EXISTS ( SELECT 1 AS dummy
               FROM com_nalet_katalog_playbackassets _assets_exists_1
              WHERE (((_assets_exists_1.item_id)::text = (items_0.id)::text) AND (((_assets_exists_1.codec)::text ~~ 'hev1%'::text) OR ((_assets_exists_1.codec)::text ~~ 'hvc1%'::text))))) THEN true
            ELSE false
        END AS ispackaged
   FROM com_nalet_katalog_items items_0
  WHERE ((type)::text = 'movie'::text);
VIEW katalogservice_people
 SELECT id,
    name
   FROM com_nalet_katalog_people people_0;
VIEW katalogservice_playbackassets
 SELECT id,
    item_id,
    path,
    codec,
    resolution,
    bitratekbps,
    sizebytes,
    hash,
    isprimary,
    kind,
    audiocodec,
    audiolanguage,
    audiochannels,
    audiobitratekbps,
    audiotrackcount,
    subtitletrackcount,
    durationms,
    (sizebytes / 1048576) AS sizemb
   FROM com_nalet_katalog_playbackassets playbackassets_0;
VIEW katalogservice_scanjobs
 SELECT id,
    source,
    status,
    startedat,
    finishedat,
    errormessage,
    filesseen,
    itemsinserted,
    itemsupdated
   FROM com_nalet_katalog_scanjobs scanjobs_0;
VIEW katalogservice_series
 SELECT id,
    createdat,
    createdby,
    modifiedat,
    modifiedby,
    type,
    title,
    sorttitle,
    year,
    description,
    rating,
    durationms,
    parent_id,
    seasonnumber,
    episodenumber,
    tagline,
    (('/katalog-api/api/artwork/'::text || (id)::text) || '/poster'::text) AS posterurl,
    (('/katalog-api/api/artwork/'::text || (id)::text) || '/backdrop'::text) AS backdropurl,
    (durationms / 60000) AS runtimemin,
    (year)::character varying(255) AS yeartext,
        CASE
            WHEN ((EXISTS ( SELECT 1 AS dummy
               FROM com_nalet_katalog_items _children_exists_1
              WHERE (((_children_exists_1.parent_id)::text = (items_0.id)::text) AND ((_children_exists_1.type)::text = 'episode'::text)))) AND (NOT (EXISTS ( SELECT 1 AS dummy
               FROM com_nalet_katalog_items _children_exists_2
              WHERE (((_children_exists_2.parent_id)::text = (items_0.id)::text) AND (((_children_exists_2.type)::text = 'episode'::text) AND (NOT (EXISTS ( SELECT 1 AS dummy
                       FROM com_nalet_katalog_playbackassets _assets_exists_3
                      WHERE (((_assets_exists_3.item_id)::text = (_children_exists_2.id)::text) AND (((_assets_exists_3.codec)::text ~~ 'hev1%'::text) OR ((_assets_exists_3.codec)::text ~~ 'hvc1%'::text)))))))))))) THEN true
            ELSE false
        END AS ispackaged
   FROM com_nalet_katalog_items items_0
  WHERE ((type)::text = 'series'::text);
VIEW katalogservice_settings
 SELECT id,
    createdat,
    createdby,
    modifiedat,
    modifiedby,
    key,
    valuetext,
    valuetype,
    description
   FROM com_nalet_katalog_settings settings_0;
VIEW katalogservice_subtitleassets
 SELECT id,
    item_id,
    path,
    format,
    lang,
    label,
    isdefault
   FROM com_nalet_katalog_subtitleassets subtitleassets_0;
