# katalog-manager — Consolidated Implementation Contract (Go + GraphQL rewrite)

**Source of truth:** SAP CAP (Java) `katalog-manager-api`. This file consolidates the per-area specs
(`10-odata-surface`, `20-rest-items`, `21-rest-analyzer`, `22-rest-downloads`, `30-integrations`,
`40-platform`) into one decision-ready contract, cross-checked against `db/data-model.cds` and
`live_schema.txt`.

**Invariants that bind every section:**
- Base tables are `com_nalet_katalog_<entity>`; OData projections are VIEWS `katalogservice_<entity>`
  (with computed columns). **All DB identifiers are lowercase.** Associations are `*_id` columns
  (e.g. `parent_id`, `item_id`, `genre_id`, `person_id`).
- PK is `id varchar(36)` (CDS `cuid`). `managed` adds `createdat/createdby/modifiedat/modifiedby`.
- JSON ids are always strings; JSON is camelCase, DB is lowercase (`durationMs` ↔ `durationms`).
- The on-disk EDMX is STALE — the CDS is authoritative. **No OData actions/functions exist** in the
  CAP model; every "action" is a Spring REST endpoint. The CAP `KatalogServiceHandler` is empty.
- No app context-path. The `/katalog-api/` prefix is added by the console proxy. The poster/backdrop
  URL prefix `/katalog-api/api/artwork/...` is baked into the view SQL and MUST be reproduced.

---

## 1. ENTITY CATALOG

Legend for **Computed source**: `view` = read straight from `katalogservice_*` (view already computes
it); `resolver` = must be computed in the Go resolver (the view's SQL is the formula to port). For the
Go rewrite either materialize the same view SQL OR compute in-resolver — both are listed so the choice
is explicit.

### 1.1 Items (`com_nalet_katalog_items`) — the canonical media table

Discriminated single-table model. `type ∈ {movie, series, season, episode, album, track, book}`.

| Column (DB) | Type | Notes |
|---|---|---|
| `id` | varchar(36) | PK (cuid) |
| `createdat/createdby/modifiedat/modifiedby` | ts/varchar | managed |
| `type` | varchar(20) | NOT NULL, discriminator |
| `title` | varchar(255) | NOT NULL |
| `sorttitle` | varchar(255) | `lower(title)` set by enrichment/scanner |
| `year` | int | |
| `description` | text | |
| `rating` | numeric(3,1) | |
| `durationms` | bigint | |
| `parent_id` | varchar(36) | self-FK (season→series, episode→season/series, track→album) |
| `seasonnumber` | int | episode coord (NULL non-episode) |
| `episodenumber` | int | episode coord |
| `tagline` | varchar(500) | |

**Views over Items** (all share the 14 base cols above plus computed cols):

| View | type filter | R/W | Computed columns (all in the view = `view`) |
|---|---|---|---|
| `katalogservice_items` | (none) | **RW** (CRUD, `@cds.redirection.target`) | `posterurl` text, `backdropurl` text, `runtimemin` bigint = `durationms/60000`, `yeartext` varchar = `cast(year as string)` |
| `katalogservice_movies` | `type='movie'` | RO | + `ispackaged` bool (exists hev1/hvc1 packaged asset) |
| `katalogservice_series` | `type='series'` | RO | + `ispackaged` bool (ALL episodes packaged); `children→Episodes` |
| `katalogservice_episodes` | `type='episode'` | RO | + `ispackaged`, `hasintro`, `hascredits`, `hasrecap` bool (segment-exists) |
| `katalogservice_albums` | `type='album'` | RO | + the 4 items computed cols (poster/backdrop/runtime/yeartext) |

- `posterurl` / `backdropurl` = `/katalog-api/api/artwork/<id>/poster` and `.../backdrop` — **literal
  prefix baked in view SQL; reproduce exactly** (chino clients depend on it). Read from `view`.
- `ispackaged`, `hasintro/hascredits/hasrecap` — defined by the view as EXISTS sub-selects. Cheapest to
  reproduce as the same view SQL; if computed in `resolver`, formulas:
  - `ispackaged` (movie/episode) = EXISTS packaged `playbackassets` whose codec is prefixed `hev1`/`hvc1`.
  - `ispackaged` (series) = every child episode is packaged.
  - `hasintro/hascredits/hasrecap` = EXISTS `mediasegments` row with `kind='intro'|'credits'|'recap'`.

### 1.2 Other entities & their computed columns

| Entity / view | Base table | Key | R/W | Computed col(s) — source |
|---|---|---|---|---|
| **PlaybackAssets** `katalogservice_playbackassets` | `playbackassets` | `id` | RW | `sizemb` bigint = `sizebytes/1048576` — **view** |
| **ItemProcessingSteps** `katalogservice_itemprocessingsteps` | `itemprocessingsteps` | `id` | RW | `statuscriticality` int = `failed→1, in_progress/pending→2, done→3, else 0` — **view** |
| **DownloadJobs** `katalogservice_downloadjobs` | `downloadjobs` | `id` | RO (Kafka-projected) | `statecriticality` int = `failed→1, downloading/queued→2, completed→3, else 0` — **view** |
| **ItemOverallStatus** `katalogservice_itemoverallstatus` | `itemoverallstatus` (`@cds.persistence.exists` view) | `item_id` | RO | `overallstatus` text + `donecount/pendingcount/failedcount/inprogresscount/notapplicablecount/totalsteps bigint`, `laststepfinishedat` ts — **view** (GROUP BY rollup) |
| **EnrichmentStatusCodes** `katalogservice_enrichmentstatuscodes` | `enrichmentstatuscodes` | `code` | RO value-help | — |
| **Settings** `katalogservice_settings` | `settings` | `id` | RW (`key` RO after create) | — |
| **ScanJobs** `katalogservice_scanjobs` | `scanjobs` | `id` | RW | — |
| **Genres** | `genres` | `id` | RW | `name` varchar(80) |
| **People** | `people` | `id` | RW | `name` varchar(255) |
| **ItemGenres** | `itemgenres` | `id` | RW | `item_id`, `genre_id` |
| **ItemPeople** | `itempeople` | `id` | RW | `item_id`, `person_id`, `role` varchar(40) |
| **ItemTags** | `itemtags` | `id` | RW | `item_id`, `tag` varchar(120) |
| **ItemArtwork** | `itemartwork` | `id` | RW | `item_id`, `kind` varchar(20), `url` varchar(2048) |
| **ItemArtworkData** | `itemartworkdata` | `id` | **NOT exposed on OData** (REST `/api/artwork` only) | `item_id`, `kind`, `contenttype` varchar(80), `bytes` bytea, `fetchedat` ts |
| **ItemExternalIds** | `itemexternalids` | `id` | RW | `item_id`, `source` varchar(30), `externalid` varchar(120) |
| **SubtitleAssets** | `subtitleassets` | `id` | RW | `item_id`, `path` varchar(2048), `format` varchar(10), `lang` varchar(10), `label` varchar(120), `isdefault` bool |
| **MediaSegments** | `mediasegments` | `id` | RW | `item_id`, `kind` varchar(20), `startms/endms` bigint, `source` varchar(30), `confidence` numeric(3,2), `label` varchar(120) |
| **ItemChapters** | `itemchapters` | `id` | RW | `item_id`, `startms/endms` bigint, `title` varchar(120), `ordinal` int |
| **ItemTrailerLinks** | `itemtrailerlinks` | `id` | RW | `item_id`, `source` varchar(20), `site` varchar(40), `externalid` varchar(120), `url` varchar(2048), `title` varchar(255), `durationsec` int, `publishedat` ts, `downloadedat` ts, `localpath` varchar(2048) |
| **ItemDiagnostics** | `itemdiagnostics` | `id` | RW | `item_id`, `generatedat` ts, `sourcepath` varchar(2048), `sourcesize` bigint, `sourcemtime` ts, `ffprobedata` text, `folderlisting` text, `notes` varchar(1024) |
| **EnrichmentJobs** | `enrichmentjobs` | `id` | modeled, **NEVER projected** (no view, no REST) | `status`, `startedat`, `finishedat`, `errormessage`, `itemsconsidered/itemsenriched/itemsfailed` int |

**PlaybackAssets full column set** (base + view `sizemb`): `id, item_id, path(2048), codec(40),
resolution(40), bitratekbps int, sizebytes bigint, hash(160), isprimary bool, kind(20) default
'primary', audiocodec(40), audiolanguage(10), audiochannels int, audiobitratekbps int, audiotrackcount
int, subtitletrackcount int, durationms bigint`. `kind ∈ {primary, trailer, sample, featurette,
behindthescenes, deleted_scene, interview}`.

**ScanJobs:** `source(20), status(20) default 'queued', startedat, finishedat, errormessage text,
filesseen/itemsinserted/itemsupdated int default 0`.

**DownloadJobs (read model):** `adapter(40), clientjobid(255), title(500), wanteditemid(80), state(20)
∈ {queued,downloading,completed,failed}, progresspct numeric(5,2), downloadedbytes bigint, sizebytes
bigint, speedbps bigint, etasec int, files text(JSON), errormessage text, startedat, completedat,
lasteventat`.

**Settings:** `key(120), valuetext(2000) default '', valuetype(20) default 'string' ∈
{string,list_csv,bool,int,float}, description text`.

### 1.3 SCHEMA-DUMP GAPS (must fix in the Go rewrite)
- **`com_nalet_katalog_trailerjobs`** — absent from `live_schema.txt`/views/indexes entirely; DDL only
  in `db/migrations/020_trailerjobs.sql`. Recreate. Cols: `id PK(36), createdat, modifiedat,
  item_id(36), trailer_link_id(36) NULL, source_url, package_id, download_id NULL, state(queued|running|
  downloaded|imported|failed|timeout), attempts int, started_at, finished_at, bytes_done, bytes_total,
  message(≤500), final_path`.
- **`com_nalet_katalog_downloadjobs`** — dump shows only PK(`id`), but the Kafka consumer upserts
  `ON CONFLICT (adapter, clientjobid)`. The Go rewrite MUST add `UNIQUE(adapter, clientjobid)` (the
  `idx_downloadjobs_client` index from `025_download_jobs.sql`) or upsert on the derived deterministic
  `id` — otherwise projection breaks.

---

## 2. RELATIONSHIP MAP (Items associations & compositions)

Self-association: `Items.parent → Items` (`parent_id`); reverse `Items.children → many Items`
(no column; `parent_id = $self.id`). Used for series→episodes (Episodes view exposes `children`).

| Items navigation | Kind | Target table | Join | Cardinality |
|---|---|---|---|---|
| `parent` | Association to-one | items | `parent_id` | 0..1 |
| `children` | Association to-many | items | `children.parent_id = $self.id` | 0..* |
| `externalIds` | Composition | itemexternalids | `item_id` | 0..* |
| `artwork` | Composition | itemartwork | `item_id` | 0..* |
| `artworkData` | Composition | itemartworkdata | `item_id` | 0..* (not OData-exposed) |
| `assets` | Composition | playbackassets | `item_id` | 0..* |
| `subtitles` | Composition | subtitleassets | `item_id` | 0..* |
| `segments` | Composition | mediasegments | `item_id` | 0..* |
| `chapters` | Composition* | itemchapters | `item_id` | 0..* (own entity; not on Items in CDS but FK-linked) |
| `trailerLinks` | Composition | itemtrailerlinks | `item_id` | 0..* |
| `diagnostics` | Composition | itemdiagnostics | `item_id` | 0..1 (modeled as many) |
| `processingSteps` | Composition | itemprocessingsteps | `item_id`, UNIQUE `(item_id,step)` | 0..* |
| `overallStatus` | Association to-one | itemoverallstatus (view) | `item_id` | 0..1 (computed rollup) |
| `genres` | Association to-many | itemgenres → genres | `item_id` / `genre_id` | 0..* |
| `people` | Association to-many | itempeople → people | `item_id` / `person_id` (+`role`) | 0..* |
| `tags` | Association to-many | itemtags | `item_id` | 0..* |

Note: `ItemChapters` is a standalone CDS entity (not declared as an Items composition in `data-model.cds`)
but is FK-linked by `item_id` and written via `PUT /api/chapters/items/{id}`; treat it as an Items facet.

**Default sort to mirror** (port verbatim): media + Items → `createdat DESC`; Episodes →
`seasonnumber, episodenumber`; ItemProcessingSteps → `modifiedat DESC`; DownloadJobs → `lasteventat
DESC`; ScanJobs → `startedat DESC`; ItemTrailerLinks → `publishedat DESC`; MediaSegments → `startms`.

---

## 3. ACTION / FUNCTION CATALOG  (→ GraphQL mutations/queries)

**There are NO OData actions/functions.** Every "operator action" is a Spring REST endpoint. The table
below is the canonical list of *operations* to expose as GraphQL mutations (writes) / queries (reads).
Param/return/impl are the REST contract; column 4 is the GraphQL target.

| Operation | Params | Return | Impl summary | GraphQL |
|---|---|---|---|---|
| **triggerScan** | `source` (def `nfs`; else 400) | `{ID,source,status:"running",startedAt}` (key uppercase `ID`) | INSERT scanjobs running; async `NfsScanner.scan`; 202 | mutation |
| **getScanJob** | `id` | scanjob row; 404 empty | SELECT | query |
| **listScanJobs** | `limit` (≤0/>200→50, def 50) | array, `ORDER BY startedat DESC` | SELECT | query |
| **searchItems** | `q,type,genre,year,limit(clamp ≤0/>200→50),offset(<0→0)` | `{items:[{id,type,title,year,rating,score}],total=pageSize,limit,offset}` | pg tsvector+trgm | query |
| **enrichStatus** | — | `{tmdbEnabled}` | config read | query |
| **enrichOne** | `id` (sync) | `{itemId,status,message?}` (status `done\|not_found\|failed\|skipped`; item-missing→`failed`, still 200) | TMDB sync | mutation |
| **enrichPending** | `limit(≤0/>1000→50),type(movie\|series\|blank)` | `{queued,type}` 202 then async sweep | queue items w/ tmdb step pending/absent `ORDER BY createdat ASC` | mutation |
| **backfillEpisodeBackdrops** | — | `{artworkData:n,artwork:n}` | clone episode poster→backdrop (idempotent) | mutation |
| **retryNotFound** | `type?` | `{reset:n,type}` | tmdb step `skipped`→`pending` | mutation |
| **claim** | `pass(per_file\|tidb_first\|transcoder\|packager),limit(1–32 def 4)` | `{pass,claimed,items:[{id,type,title,year,durationMs,path,seasonNumber,episodeNumber,seriesTitle?,seriesTmdbId,movieTmdbId}]}` | `FOR UPDATE SKIP LOCKED` dequeue + per-item `NOT EXISTS in_progress`; flips step→in_progress per pass | **KEEP-REST** (worker contract) |
| **getAnalyzeItem** | `id` | `{id,type,title,year,durationMs,path}`; 404 no primary asset | SELECT | query (or REST) |
| **getSteps** | `id` | `{itemId,steps:{step:status}}` | SELECT | query |
| **skipSteps** | `id`, `{steps[],reason?}` | `{itemId,updated}` (unknown steps silently skipped → `not_applicable`) | upsert | mutation |
| **putStep** | `id`,`step`, `{status(req),error?,details?}` | `{itemId,step,status}` | primary status write; **chain promote** transcode done/n_a/skipped→seed `package=pending` (failed transcode does NOT promote) | **KEEP-REST** (worker contract) |
| **failItem** | `id`, `{reason?}` (def "unspecified analyzer error") | `{itemId,status:"failed",stepsSkipped}` | `scan=failed`, then pending/in_progress analyzer steps→`skipped` | mutation |
| **getSiblings** | `id`,`limit(1–12 def 5)` | `{itemId,items:[...]}` (same parent+season) | SELECT | query |
| **resetSeries** | `id` | `{seriesId,episodes,stepsReset,segmentsPurged}` | bump episode createdat=now, resetForItems(ANALYZER_STEPS), DELETE chromaprint segments | mutation |
| **putSegments** | `id`, `{segments:[{kind(req),source(req),startMs,endMs,confidence?,label?}]}` (`0<=start<end`) | `{itemId,written}`; 404 item-missing | atomic DELETE+batch-INSERT; does NOT touch steps | **KEEP-REST** (fused analyzer output) |
| **deleteSegments** | `id` | `{itemId,removed}` | DELETE | **KEEP-REST** |
| **putChapters** | `id`, `{chapters:[{startMs,endMs,title?,ordinal?}]}` (ordinal fallback=1-based idx, blank title→NULL) | `{itemId,written}` | DELETE+batch-INSERT | **KEEP-REST** |
| **deleteChapters** | `id` | `{itemId,removed}` | DELETE | **KEEP-REST** |
| **packageItem** | `id` | movie/episode `{status,alreadyActive,message}`; series fan-out `{episodesEnqueued,episodesTotal,...}`; non-media→400; 404 unknown | upsert `transcode=pending`; idempotent | mutation |
| **validateItem** | `id` | single `{code,message,sourcePath?,packagePath?,findings?[]}`; series rollup; codes `ok\|no_package\|source_missing\|stale\|codec_mismatch\|findings\|not_applicable` | FS + DB read, no writes | mutation (or query — read-only) |
| **packagingComplete** | `id`, manifest.json body | `{itemId,sourceEnriched,packagedAssetWritten,subtitlesWritten,audioTracks}`; 404 unknown | packager machine sink, idempotent, 3 writes | **KEEP-REST** (packager machine sink) |
| **fetchTrailers** | `id` | `{itemId,title,enqueued,packageId,jobIds[],message}` | reads undownloaded itemtrailerlinks, de-dups, oDownloader enqueue | mutation |
| **addDownload** | `{adapter,source,title?,wantedItemId?}` | `202 {ok,adapter,clientJobId,message}`; gateway err 502 | → gateway `POST /api/v1/downloads`; no DB | mutation |
| **cancelDownload** | `adapter,clientJobId` | `{ok,message}`/502 | → gateway DELETE; no DB | mutation |
| **listDownloadClients** | — | raw JSON string (e.g. `["odownloader"]`); disabled→`"[]"` | → gateway `GET /api/v1/clients` | query |
| **getSettings** | — | `{key:{valueText,valueType}}` (all rows) | SELECT settings | query |
| **CRUD Settings** | OData | — | create/update/delete via OData (key RO after create) | mutations |

`/svc/v1` = empty stub (planned acquire/subtitles/katalog-ingest/metadata-enricher, role
`cloud_katalog_admin`, future write surface emitting `stube.library.item.*`). No live endpoints.

**Step vocab** (port verbatim): STEPS `{scan,tmdb,tidb,chapter,chromaprint,blackframe,silence,subtitle,
transcode,package}`; STATUS `{pending,in_progress,done,failed,skipped,not_applicable}`; ANALYZER_STEPS
(per-file) `{chapter,chromaprint,blackframe,silence,subtitle,tidb}`; PASSES `{per_file,tidb_first,
transcoder,packager}`. Segment KINDS `{intro,recap,credits,preview}`; SOURCES `{tidb,chapter,subtitle,
silence,blackframe,chromaprint,whisper,transnet,manual}`.

**ProcessingStepService.upsert** (port verbatim): INSERT…ON CONFLICT `(item_id,step)`; insert
`id=gen_random_uuid()::varchar, attempts=1`; on conflict `attempts++`, `startedat` **sticky** (now on
first→in_progress when null), `finishedat` set on terminal `(done|failed|skipped)` — `not_applicable`
does NOT set finishedat — `error` truncated 500. Bad step/status→400; 0 rows→500.
`resetForItems(ids,steps)`: status=pending, startedat/finishedat/error=NULL, modifiedat=now; **attempts
preserved**.

---

## 4. REST ENDPOINT CATALOG (by controller)

- **ScanController** `/api/scan`: `POST /` (source, 202 key `ID`), `GET /{id}`, `GET /?limit=`.
- **SearchController** `/api/search`: `GET /items?q,type,genre,year,limit,offset`.
- **EnrichmentController** `/api/enrich`: `GET /status`, `POST /items/{id}`, `POST /pending?limit,type`,
  `POST /backfill-episode-backdrops`, `POST /retry-not-found?type`.
- **AnalyzerController** `/api/analyze`: `POST /claim?pass,limit`, `GET /items/{id}`,
  `GET /items/{id}/steps`, `POST /items/{id}/steps/skip`, `PUT /items/{id}/steps/{step}`,
  `POST /items/{id}/fail`, `GET /items/{id}/siblings?limit`, `POST /series/{id}/reset`.
- **Segments** `/api/segments`: `PUT|DELETE /items/{itemId}`.
- **Chapters** `/api/chapters`: `PUT|DELETE /items/{itemId}`.
- **ItemActionsController** `/api/items`: `POST /{id}/package`, `POST /{id}/validate`,
  `POST /{id}/packaging-complete`.
- **TrailerActionsController** `/api/items`: `POST /{id}/fetch-trailers`.
- **DownloadConsoleController** `/api/downloads`: `POST /`, `DELETE /{adapter}/{clientJobId}`,
  `GET /clients`.
- **SettingsController** `/api/settings`: `GET /` (read-only; CRUD via OData).
- **ArtworkController** `/api/artwork`: `GET /{itemId}/{kind}` (permitAll; JWT or `?stream=` token) —
  raw bytea, `Content-Type` from row (def `image/jpeg`), `Cache-Control: max-age=604800, public`; 404
  empty when no row.
- **PlayController** `/api/play`: `GET /{itemId}` — byte-range (200/206/416), `Accept-Ranges: bytes`,
  primary asset (fallback ORDER BY path); JWT only.
- **SubtitlesController** `/api/subtitles`: `GET /items/{itemId}` (list), `GET /{subId}` (VTT; SRT→VTT
  live; pgs/vobsub/dvb passthrough; `Cache-Control: private, max-age=300`).
- **KatalogManagerRestController** `/svc/v1`: STUB, zero endpoints, role `cloud_katalog_admin`.
- **OData CRUD** `/odata/v4/katalog-admin/` (CAP, Fiori): entity-set reads/writes per §1.

---

## 5. INTEGRATION CATALOG

| Integration | Direction | Trigger | Topics / endpoints | Data flow |
|---|---|---|---|---|
| **TMDB** | outbound HTTP | EnrichmentController (sync `/items/{id}`, async `/pending`, retry, backfill) | `api.themoviedb.org/3` (Bearer `TMDB_API_KEY` v4, `TMDB_LANGUAGE=en-US`); `image.tmdb.org/t/p` poster w780/backdrop w1280/still w500. blank key→disabled→no-op. Only HTTP 200=success. Search year-fallback (retry w/o year). | Writes (COALESCE) items, itemexternalids (tmdb/tmdb-episode/imdb), genres+itemgenres, people+itempeople (cast cap 12→actor, crew Director→director), itemtrailerlinks (source='tmdb', DELETE where downloadedat NULL then INSERT, Trailer/Teaser+YouTube/Vimeo), itemartwork(url)+itemartworkdata(bytes), mediasegments+itemchapters (chaptersdb), itemprocessingsteps step='tmdb'. Status map `not_found→skipped`. `cleanTitle()` shared with scanner. |
| **NfsScanner** | filesystem, on-demand | `POST /api/scan?source=nfs` → async single-thread | `SCANNER_NFS_ROOT=/var/lib/katalog/media` (RO PVC) | classify audio→track, video+/series//tv//SxxEyy→episode, else movie. Upsert key=`playbackassets.path` (abs). New→INSERT items+asset(isprimary); existing→UPDATE items.modifiedat ONLY (never clobber TMDB), UPDATE asset sizebytes/isprimary. Subtitle sidecars, trailers (kind='trailer'). scanjobs lifecycle running→done/failed. |
| **download-gateway (command)** | outbound REST (CQRS write) | DownloadConsoleController `/api/downloads` (JWT) | `POST/DELETE /api/v1/downloads`, `GET /api/v1/clients`; `DOWNLOAD_GATEWAY_URL` blank→disabled. adapter∈odownloader\|qbittorrent\|nzbget; non-2xx→502. | No DB. Command side NEVER reads downloadjobs. |
| **download-gateway (read)** | inbound Kafka (CQRS read) | `DownloadEventConsumer`, only if `DOWNLOAD_GATEWAY_EVENTS_ENABLED=true` (default **false**) | **CONSUMES** topic pattern `stube\.download\.client\..*` (`.started/.progress/.completed/.failed`), group `stube-katalog-manager`, JSON snake_case, epoch-millis. **PRODUCES nothing.** mTLS PEM. | UPSERT downloadjobs `ON CONFLICT (adapter,clientjobid)`; deterministic id = UUIDv3 `nameUUIDFromBytes("adapter:client_id")`. started→queued, progress→downloading (sticky, won't overwrite completed/failed), completed→completed/pct=100/files, failed→failed/errormessage. |
| **oDownloader** | outbound REST + `@Scheduled` poller | `POST /api/items/{id}/fetch-trailers`; poller `fixedDelay=poll-interval×1000, initialDelay=30s` | `ODOWNLOADER_URL`+`ODOWNLOADER_TOKEN` (both req, Bearer; else disabled). `POST /api/v1/links/add`, `GET /api/v1/downloads?packageId&size=200`, `GET /api/v1/downloads/{id}/content`. inbox `/var/lib/katalog/packages/_inbox`. | enqueue (1 URL=1 package) → trailerjobs; poll picks best `.mp4/.mkv/.mov` variant, on FINISHED stream→inbox, INSERT playbackassets kind='trailer', stamp itemtrailerlinks.localpath/downloadedat, trailerjobs→imported. |
| **chaptersdb** | outbound HTTP, default **DISABLED** | inside TMDB enrichment | `CHAPTERSDB_ENABLED=false`, `CHAPTERSDB_BASE_URL=https://chaptersdb.com`. `/api/shows/search?q=`, `/api/chapters/by-show/{id}`. 10s/15s timeouts, only 200 parsed. | findShow (type+year match) → getMovieChapters → mediasegments+itemchapters. |

**Produces nothing today.** Future write surface (planned): `stube.library.item.*` from `/svc/v1`.
Topic convention `stube.{domain}.{event}`.

---

## 6. CONFIG / AUTH CATALOG

**Ports / health / image:** port 8080; no app context-path (`/katalog-api` added by console proxy).
Health `/healthz`, `/actuator/health/liveness`, `/actuator/health/readiness`. No prometheus/metrics
endpoint. Image `registry.nalet.cloud/stube/katalog-manager-api:latest`, `eclipse-temurin:21-jre`, UID
1001/GID 0, ns `stube`, 1 replica.

**Env vars:**
- `SERVER_PORT`=8080.
- `SPRING_DATASOURCE_URL|USERNAME|PASSWORD` ← secret `katalog-db`. User `cloud_katalog` (some objects
  owned by `postgres`, app can't ALTER). Needs pgcrypto (`gen_random_uuid`), unaccent, pg_trgm,
  tsvector `search_vector`.
- `SPRING_SECURITY_OAUTH2_RESOURCESERVER_JWT_ISSUER_URI`=`https://sso.nalet.cloud/realms/nalet.cloud`.
- `KATALOG_AUDIENCE`=`katalog`, `katalog.audience.required`=false → **issuer-only validation in MVP**.
- `AUTH_DISABLED` (def false) → full permitAll.
- `STREAM_SIGNING_KEY` ← secret `chino-stream-signing/key` (base64, ≥16 bytes decoded).
- Kafka: `KAFKA_BROKERS`=`platform-kafka-kafka-bootstrap.platform-event-streaming.svc:9093`,
  `KAFKA_GROUP_ID`/`DOWNLOAD_GATEWAY_KAFKA_GROUP_ID`=`stube-katalog-manager`,
  `DOWNLOAD_GATEWAY_EVENTS_ENABLED`=**false**,
  `DOWNLOAD_GATEWAY_URL`=`http://download-gateway.stube.svc.cluster.local:8080`, TLS PEM at
  `/etc/kafka-cert/{user.crt,user.key,ca.crt}`.
- `NFS_ROOT`/`SCANNER_NFS_ROOT`=`/var/lib/katalog/media` (RO PVC `media`); packages RW PVC at
  `/var/lib/katalog/packages` (`PACKAGES_ROOT`).
- `TMDB_API_KEY` (opt), `TMDB_LANGUAGE`=en-US. `CHAPTERSDB_ENABLED`=false (+ `CHAPTERSDB_BASE_URL`).
- oDownloader: `ODOWNLOADER_URL`/`ODOWNLOADER_API_URL`=`http://odownloader.stube.svc.cluster.local:8686`,
  `ODOWNLOADER_TOKEN`/`ODOWNLOADER_API_TOKEN` (opt; unset→no-op), poll 15s, inbox
  `/var/lib/katalog/packages/_inbox`, timeout 60m.
- `spring.datasource.platform` (Postgres vs H2; H2 search/genre branches differ).

**Auth model:** Bearer JWT resource server, STATELESS, CSRF off, **no role/scope checks** (any valid
issuer JWT authorizes everything). Public: `/healthz`, `/actuator/health/**`, `/katalog/**`.
`/api/artwork/**` = JWT **or** `?stream=` token. Everything else authenticated.

**Stream token (reproduce EXACTLY — chino-stream/web verify it):**
- HMAC-SHA256. Key = base64-decoded `STREAM_SIGNING_KEY`, ≥16 bytes raw. Unset→disabled.
- `token = base64url(payload) "." base64url(HMAC_SHA256(ASCII_bytes(base64url_payload_string), key))`,
  where `payload = base64url(userID "|" expUnix)`.
- **HMAC is over the base64url payload *string's* ASCII bytes**, not the raw `userID|expUnix`. Both
  segments URL-safe base64. `expUnix` = epoch **seconds**; reject if `now > expUnix`. Constant-time
  compare. Query param `stream`, only on `/api/artwork/**`. Valid → principal=userID, authority
  `ROLE_STREAM`. Filter never throws → falls through to Bearer.

**Kafka consumer trap:** no `spring.kafka`/`spring.ssl.bundle` (eager SSL bundle crashes app); consumer
is `@ConditionalOnProperty(events-enabled=true)` and non-fatal on missing cert — keep optional.
`auto.offset.reset=earliest`, native PEM mTLS (`security.protocol=SSL`, `ssl.keystore.type=PEM`).

---

## 7. GraphQL vs REST SPLIT  ← single most important design output

**KEEP-REST** = binary/file streaming, machine/worker contracts, external-system-verified token
formats, Kafka. **MOVE-TO-GRAPHQL** = UI reads, item/settings mutations, search, enrichment triggers,
downloads read+commands, trailers.

| Surface | Verdict | Reason |
|---|---|---|
| `GET /api/artwork/{id}/{kind}` | **KEEP-REST** | Returns raw image bytes; poster/backdrop URLs baked into view SQL + consumed by chino clients via `<img src>`; stream-token auth. |
| `GET /api/play/{itemId}` | **KEEP-REST** | HTTP byte-range streaming (200/206/416); not expressible over GraphQL. |
| `GET /api/subtitles/items/{id}` + `GET /api/subtitles/{subId}` | **KEEP-REST** | Serves raw VTT/PGS/VobSub/DVB bytes with content-type negotiation; player fetches by URL. |
| Stream-token verification (`?stream=`) | **KEEP-REST** | Exact HMAC format verified by external chino-stream/web; must stay a query-param on artwork. |
| `POST /api/analyze/claim` | **KEEP-REST** | Worker dequeue (SKIP LOCKED); polled by Python workers; keep stable machine contract. |
| `PUT /api/analyze/items/{id}/steps/{step}` | **KEEP-REST** | Primary worker status write + transcode→package chain promotion; called by workers. |
| `PUT|DELETE /api/segments/items/{id}` | **KEEP-REST** | Fused analyzer output (batch DELETE+INSERT); machine-written. |
| `PUT|DELETE /api/chapters/items/{id}` | **KEEP-REST** | Chapter atoms from analyzer; machine-written batch. |
| `POST /api/items/{id}/packaging-complete` | **KEEP-REST** | Packager machine sink ingesting manifest.json; idempotent worker contract. |
| `GET /api/analyze/items/{id}[/steps|/siblings]`, `POST .../fail`, `.../steps/skip`, `POST /series/{id}/reset` | **KEEP-REST** | Part of the analyzer worker protocol; co-locate with claim/putStep for one consistent machine surface. |
| Kafka `stube.download.client.*` consumer | **KEEP-REST** (not HTTP — event sink) | Async event ingestion into read model; not a request/response API. |
| `POST/GET /api/scan` (trigger + status) | **MOVE-TO-GRAPHQL** | Operator action from Fiori/console UI; mutation `triggerScan` + queries `scanJob(s)`. (If a machine ever calls it, keep a REST shim.) |
| `GET /api/search/items` | **MOVE-TO-GRAPHQL** | UI read; map to `query searchItems(...)`. |
| `GET /api/enrich/status`, `POST /api/enrich/{items/{id},pending,backfill-episode-backdrops,retry-not-found}` | **MOVE-TO-GRAPHQL** | UI-triggered enrichment ops → mutations + `enrichStatus` query. |
| `POST /api/items/{id}/package` | **MOVE-TO-GRAPHQL** | UI button; mutation `packageItem`. |
| `POST /api/items/{id}/validate` | **MOVE-TO-GRAPHQL** | UI button, read-only result; mutation/query `validateItem`. |
| `POST /api/items/{id}/fetch-trailers` | **MOVE-TO-GRAPHQL** | UI button; mutation `fetchTrailers`. |
| `POST /api/downloads`, `DELETE /api/downloads/{adapter}/{id}`, `GET /api/downloads/clients` | **MOVE-TO-GRAPHQL** | CQRS command side, UI-driven; mutations `addDownload`/`cancelDownload` + query `downloadClients`. |
| DownloadJobs read (OData view) | **MOVE-TO-GRAPHQL** | UI read of the projection; `query downloadJobs`. |
| `GET /api/settings` + Settings CRUD (OData) | **MOVE-TO-GRAPHQL** | UI reads/writes config; `query settings` + CRUD mutations (key RO after create). |
| OData entity-set reads: Items/Movies/Series/Episodes/Albums, PlaybackAssets, ItemProcessingSteps, ItemOverallStatus, Genres, People, ItemGenres/People/Tags, ItemArtwork, ItemExternalIds, SubtitleAssets, MediaSegments, ItemChapters, ItemTrailerLinks, ItemDiagnostics, EnrichmentStatusCodes | **MOVE-TO-GRAPHQL** | Fiori/console catalog reads; the core GraphQL read graph (with computed cols + associations from §1/§2). |
| Items / facet entity writes (CRUD) | **MOVE-TO-GRAPHQL** | Operator edits from UI; GraphQL mutations replace OData CRUD. |
| `$search`/`$filter` (genre via `genres/any`) | **MOVE-TO-GRAPHQL** | UI filter bar; GraphQL args + the search resolver. |
| `/svc/v1` stub | N/A (future) | Empty today; future write surface emitting `stube.library.item.*`. |

---

## RETURN SUMMARY (verbatim drivers for next phase)

### A. GraphQL vs REST split

| Surface | Verdict | Reason |
|---|---|---|
| `GET /api/artwork/{id}/{kind}` | KEEP-REST | Raw image bytes; URLs baked into view SQL + consumed by chino `<img src>`; stream-token auth. |
| `GET /api/play/{itemId}` | KEEP-REST | HTTP byte-range streaming (200/206/416); not GraphQL-expressible. |
| `GET /api/subtitles/items/{id}` + `/{subId}` | KEEP-REST | Raw VTT/PGS/VobSub/DVB bytes; player fetches by URL. |
| Stream-token `?stream=` | KEEP-REST | Exact HMAC format verified by external chino-stream/web. |
| `POST /api/analyze/claim` | KEEP-REST | Worker dequeue (SKIP LOCKED), polled by Python workers. |
| `PUT /api/analyze/items/{id}/steps/{step}` | KEEP-REST | Worker status write + transcode→package chain promotion. |
| `PUT|DELETE /api/segments/items/{id}` | KEEP-REST | Fused analyzer output (batch DELETE+INSERT), machine-written. |
| `PUT|DELETE /api/chapters/items/{id}` | KEEP-REST | Chapter atoms from analyzer, machine-written batch. |
| `POST /api/items/{id}/packaging-complete` | KEEP-REST | Packager machine sink ingesting manifest.json. |
| analyzer `GET items/{id}[/steps|/siblings]`, `POST .../fail|/steps/skip|/series/{id}/reset` | KEEP-REST | Analyzer worker protocol; keep one machine surface. |
| Kafka `stube.download.client.*` consumer | KEEP-REST (event sink) | Async ingestion into read model; not request/response. |
| `POST/GET /api/scan` | MOVE-TO-GRAPHQL | Operator action from UI → `triggerScan` mutation + `scanJob(s)` queries. |
| `GET /api/search/items` | MOVE-TO-GRAPHQL | UI read → `searchItems` query. |
| `GET /api/enrich/status` + enrich POSTs | MOVE-TO-GRAPHQL | UI-triggered enrichment → mutations + `enrichStatus` query. |
| `POST /api/items/{id}/package` | MOVE-TO-GRAPHQL | UI button → `packageItem`. |
| `POST /api/items/{id}/validate` | MOVE-TO-GRAPHQL | UI button, read-only result → `validateItem`. |
| `POST /api/items/{id}/fetch-trailers` | MOVE-TO-GRAPHQL | UI button → `fetchTrailers`. |
| `/api/downloads` POST/DELETE/clients | MOVE-TO-GRAPHQL | CQRS command side, UI-driven → `addDownload`/`cancelDownload`/`downloadClients`. |
| DownloadJobs read | MOVE-TO-GRAPHQL | UI read of the projection → `downloadJobs` query. |
| `GET /api/settings` + Settings CRUD | MOVE-TO-GRAPHQL | UI config read/write (key RO after create). |
| OData catalog reads (Items/Movies/Series/Episodes/Albums + all facets/value-helps) | MOVE-TO-GRAPHQL | Core GraphQL read graph with computed cols + associations. |
| Items/facet writes (CRUD) | MOVE-TO-GRAPHQL | Operator edits → GraphQL mutations. |
| `$search`/`$filter` (genre via `genres/any`) | MOVE-TO-GRAPHQL | UI filter bar → resolver args. |

### B. Entity catalog (base table | key | R/W | computed-in-view cols)

| Entity | Base table | Key | R/W | Computed (view) |
|---|---|---|---|---|
| Items | items | id | RW | posterurl, backdropurl, runtimemin=durationms/60000, yeartext=str(year) |
| Movies | items (`type='movie'`) | id | RO | + ispackaged |
| Series | items (`type='series'`) | id | RO | + ispackaged (all episodes); children→Episodes |
| Episodes | items (`type='episode'`) | id | RO | + ispackaged, hasintro, hascredits, hasrecap |
| Albums | items (`type='album'`) | id | RO | + poster/backdrop/runtimemin/yeartext |
| PlaybackAssets | playbackassets | id | RW | sizemb=sizebytes/1048576 |
| ItemProcessingSteps | itemprocessingsteps | id | RW | statuscriticality (failed1/inprog-pending2/done3/else0) |
| DownloadJobs | downloadjobs | id | RO (Kafka) | statecriticality (failed1/downloading-queued2/completed3/else0) |
| ItemOverallStatus | itemoverallstatus (view) | item_id | RO | overallstatus + done/pending/failed/inprogress/notapplicable/total counts, laststepfinishedat |
| EnrichmentStatusCodes | enrichmentstatuscodes | code | RO | — |
| Settings | settings | id | RW (key RO post-create) | — |
| ScanJobs | scanjobs | id | RW | — |
| Genres | genres | id | RW | — |
| People | people | id | RW | — |
| ItemGenres | itemgenres | id | RW | — |
| ItemPeople | itempeople | id | RW | role |
| ItemTags | itemtags | id | RW | — |
| ItemArtwork | itemartwork | id | RW | — |
| ItemArtworkData | itemartworkdata | id | NOT on OData (REST artwork only) | bytes |
| ItemExternalIds | itemexternalids | id | RW | — |
| SubtitleAssets | subtitleassets | id | RW | — |
| MediaSegments | mediasegments | id | RW | — |
| ItemChapters | itemchapters | id | RW | — |
| ItemTrailerLinks | itemtrailerlinks | id | RW | — |
| ItemDiagnostics | itemdiagnostics | id | RW | — |
| EnrichmentJobs | enrichmentjobs | id | modeled, NEVER projected | — |

Gaps: `trailerjobs` absent from dump (recreate from migration 020); `downloadjobs` needs
`UNIQUE(adapter,clientjobid)` added for the Kafka upsert.

### C. Action/function catalog (→ GraphQL mutations/queries)

| Operation | Params | Return | GraphQL target |
|---|---|---|---|
| triggerScan | source(def nfs) | {ID,source,status,startedAt} | mutation |
| scanJob / scanJobs | id / limit | row / array DESC startedat | query |
| searchItems | q,type,genre,year,limit,offset | {items[{id,type,title,year,rating,score}],total,limit,offset} | query |
| enrichStatus | — | {tmdbEnabled} | query |
| enrichOne | id | {itemId,status,message?} | mutation |
| enrichPending | limit,type | {queued,type} | mutation |
| backfillEpisodeBackdrops | — | {artworkData,artwork} | mutation |
| retryNotFound | type? | {reset,type} | mutation |
| packageItem | id | {status,alreadyActive,message} / series fan-out | mutation |
| validateItem | id | {code,message,...findings} / series rollup | mutation/query |
| fetchTrailers | id | {itemId,title,enqueued,packageId,jobIds[],message} | mutation |
| addDownload | adapter,source,title?,wantedItemId? | {ok,adapter,clientJobId,message} | mutation |
| cancelDownload | adapter,clientJobId | {ok,message} | mutation |
| downloadClients | — | raw JSON string | query |
| settings | — | {key:{valueText,valueType}} | query |
| Settings CRUD | OData | — | mutations (key RO post-create) |
| analyze: claim | pass,limit | {pass,claimed,items[...]} | KEEP-REST |
| analyze: putStep | id,step,{status,error?,details?} | {itemId,step,status} (transcode→package promote) | KEEP-REST |
| analyze: getItem/getSteps/getSiblings | id[,limit] | item / {steps} / {items} | KEEP-REST (query) |
| analyze: skipSteps | id,{steps[],reason?} | {itemId,updated} | KEEP-REST |
| analyze: failItem | id,{reason?} | {itemId,status,stepsSkipped} | KEEP-REST |
| analyze: resetSeries | id | {seriesId,episodes,stepsReset,segmentsPurged} | KEEP-REST |
| putSegments/deleteSegments | id,{segments[...]} | {itemId,written/removed} | KEEP-REST |
| putChapters/deleteChapters | id,{chapters[...]} | {itemId,written/removed} | KEEP-REST |
| packagingComplete | id, manifest.json | {itemId,sourceEnriched,packagedAssetWritten,subtitlesWritten,audioTracks} | KEEP-REST |

(No OData actions/functions exist; all above are Spring REST today.)
