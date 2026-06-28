# 10 — OData Service Surface (KatalogService)

Source of record: `stube/katalog-manager-api` (SAP CAP / Java), files
`srv/katalog-service.cds`, `db/data-model.cds`, and the Spring MVC controllers
under `src/main/java/com/nalet/katalog/web/` (+ `manager/web/`).

This document maps the **entire** runtime HTTP surface the Go+GraphQL rewrite
must reproduce. It has two distinct planes:

1. **OData v4 (CAP-generated CRUD)** — the Fiori admin UI plane. Base path
   `/odata/v4/katalog-admin/` (service `@(path:'katalog-admin')`). Pure
   projections; **no OData actions/functions, no CAP drafts**.
2. **Spring REST (`/api/**`, `/svc/v1/**`)** — every "action/function" lives
   here, NOT in OData. The CAP event handler (`KatalogServiceHandler`) is an
   empty placeholder. This is deliberate (mirrors alm's `AiController`).

Key fact for the rewrite: the live Postgres `katalogservice_*` objects are
**VIEWS** over `com_nalet_katalog_*` **base tables**; every OData entity below
binds to one view. Computed columns in the CDS (`posterUrl`, `runtimeMin`,
`isPackaged`, criticality, etc.) are baked into those views — the rewrite can
either read the views or recompute in resolvers.

---

## 1. Exposed OData entities / projections

Service: `KatalogService` @ `/odata/v4/katalog-admin/`. All identifiers
lowercase in Postgres. Each entity = projection on `db.<Entity>` (= base table
`com_nalet_katalog_<entity>`) surfaced as view `katalogservice_<entity>`.

Legend: **RW** = full CAP CRUD over OData; **RO** = `@readonly` or backed by a
DB view CAP can't write; **redir** = `@cds.redirection.target` flag.

| OData EntitySet | Base table (`com_nalet_katalog_*`) / view | R/W | Filter `type=` | Notes / projected & computed fields |
|---|---|---|---|---|
| **Items** | `items` / `katalogservice_items` | RW | none (all types) | `@cds.redirection.target` (canonical assoc target). Power-user view. Computed: `posterUrl`, `backdropUrl` (String), `runtimeMin`=`durationMs/60000` (Integer), `yearText`=`cast(year as String)`. `@cds.search:{title,description}`. |
| **Movies** | `items` / `katalogservice_movies` | RO | `type='movie'` | `@readonly`, `redirection.target:false`. Same 4 computed cols + `isPackaged:Boolean` = `exists assets[codec like 'hev1%' or 'hvc1%']`. |
| **Series** | `items` / `katalogservice_series` | RO | `type='series'` | `@readonly`. Computed cols + `children: redirected to Episodes`; `isPackaged` = every episode child has hev1/hvc1 asset (guarded by `exists children[type='episode']`). |
| **Episodes** | `items` / `katalogservice_episodes` | RO | `type='episode'` | `@readonly`. Computed cols + `isPackaged`, plus segment-exists booleans: `hasIntro` (`segments[kind='intro']`), `hasCredits` (`kind='credits'`), `hasRecap` (`kind='recap'`). `seasonNumber`/`episodeNumber` UI-readonly. |
| **Albums** | `items` / `katalogservice_albums` | RO | `type='album'` | `@readonly`. 4 computed cols only (no isPackaged/segments). |
| **ScanJobs** | `scanjobs` / `katalogservice_scanjobs` | RW | — | Scan history. (Rows actually written by `ScanController`, not OData.) |
| **Genres** | `genres` | RW | — | `name` has self value-help (`Common.ValueList` CollectionPath=Genres). |
| **People** | `people` | RW | — | name lookup. |
| **ItemGenres** | `itemgenres` | RW | — | link entity (item↔genre). |
| **ItemPeople** | `itempeople` | RW | — | link + `role`. |
| **ItemTags** | `itemtags` | RW | — | item↔tag string. |
| **ItemArtwork** | `itemartwork` | RW | — | artwork URL rows (kind/url). |
| **ItemExternalIds** | `itemexternalids` | RW | — | (source, externalId). |
| **PlaybackAssets** | `playbackassets` / `katalogservice_playbackassets` | RW | — | Computed `sizeMB`=`sizeBytes/1048576` (Integer). |
| **SubtitleAssets** | `subtitleassets` | RW | — | sidecar subs. |
| **MediaSegments** | `mediasegments` | RW | — | skippable segments. |
| **ItemChapters** | `itemchapters` | RW | — | ffprobe chapter atoms. |
| **ItemTrailerLinks** | `itemtrailerlinks` | RW | — | remote trailer URLs; `url @Core.IsURL`. |
| **ItemDiagnostics** | `itemdiagnostics` | RW | — | per-item ffprobe/folder JSON snapshot. |
| **ItemProcessingSteps** | `itemprocessingsteps` / `katalogservice_itemprocessingsteps` | RW | — | Computed `statusCriticality:Integer` (1 failed,2 in_progress/pending,3 done,0 else) `@UI.Hidden`. |
| **ItemOverallStatus** | `itemoverallstatus` (view, `@cds.persistence.exists`) | RO | — | `@readonly`. Rollup view; key=`item`. Cols: `overallStatus`, `doneCount`, `pendingCount`, `failedCount`, `inProgressCount`, `notApplicableCount`, `totalSteps`, `lastStepFinishedAt`. |
| **EnrichmentStatusCodes** | `enrichmentstatuscodes` | RO | — | `@readonly`. Code list (code,name). Value-help only. |
| **Settings** | `settings` / `katalogservice_settings` | RW | — | UUID PK + operator-visible `key` (`![key]`, `FieldControl:#ReadOnly`). CRUD via standard CAP. |
| **DownloadJobs** | `downloadjobs` / `katalogservice_downloadjobs` | RO | — | `@readonly`. Kafka projection (write side = gateway). Computed `stateCriticality:Integer` (1 failed,2 downloading/queued,3 completed,0 else). |

**Not exposed over OData** (base entities with no projection in
`katalog-service.cds`): `ItemArtworkData` (raw blobs — served via REST
`/api/artwork/**`), `EnrichmentJobs` (defined in data-model, never projected).
NOTE: the *generated EDMX* on disk (`src/main/resources/edmx/...`) is **stale** —
it lists `ItemArtworkData`/`ItemTags` as sets and is missing `DownloadJobs`
(pre-migration-025). The CDS is authoritative; trust the table above.

### 1a. Base `Items` entity model (the discriminated store)

One table holds movie|series|season|episode|album|track|book via `type`.
Self-association `parent` (+ reverse `children on children.parent=$self`).
Compositions (cascade children, surfaced as Fiori facets / GraphQL nested):
`externalIds, artwork, artworkData, assets, subtitles, segments, trailerLinks,
diagnostics, processingSteps`. Associations: `overallStatus` (to-one, RO view),
`genres, people, tags`. Episode coords: `seasonNumber`, `episodeNumber`.
Scalar fields: `type,title,sortTitle,year(Integer),description(LargeString),
rating(Decimal(3,1)),durationMs(Integer64),tagline`. Mixins `cuid`
(`ID` UUID) + `managed` (`createdAt,modifiedAt,createdBy,modifiedBy`).

### 1b. Behaviorally-relevant annotations (non-layout)

- **Auth**: NOT per-entity. Whole surface gated by Spring Security
  (`SecurityConfig`): every request needs a valid JWT EXCEPT `/healthz`,
  `/actuator/health/**`, `/katalog/**` (static UI) = public, and
  `/api/artwork/**` = permitAll (accepts bearer JWT **or** an HMAC stream
  token via `StreamTokenAuthFilter`). CAP's own auth chain is **disabled**
  (`cds.security.authentication.authConfig.enabled=false`). Issuer-only JWT
  validation today; audience (`katalog`) check is wired but
  `katalog.audience.required=false` (flip-ready). No role checks enforced in
  code (the `cloud_katalog_admin` role on `/svc/v1` is documented intent only).
- **`@readonly`**: `Movies, Series, Episodes, Albums, ItemOverallStatus,
  EnrichmentStatusCodes, DownloadJobs`.
- **Default sort** (`PresentationVariant.SortOrder` — affects default list
  order GraphQL should mirror): Movies/Series/Albums/Items → `createdAt desc`;
  Episodes → `seasonNumber, episodeNumber` (GroupBy season); ItemProcessingSteps
  & DownloadJobs → `modifiedAt`/`lastEventAt desc`; ScanJobs → `startedAt desc`;
  ItemTrailerLinks → `publishedAt desc`; MediaSegments → `startMs`;
  ItemPeople → `role, person.name`; ItemGenres → `genre.name`;
  ItemExternalIds → `source`; Settings → `key`.
- **`@cds.search`** (free-text `$search` contains-match): Items, Movies,
  Series, Albums → `{title, description}`.
- **Value-help / criticality logic affecting data shape** (NOTE per task):
  - `Genres.name` → self value-list (filter `genres/any(g: g/genre/name eq ...)`).
  - `EnrichmentStatusCodes` → value-help lookup for the legacy enrichment code
    set (drives a badge on the `tmdb` pipeline row).
  - **Criticality computed columns** (data, not layout) live in the views:
    `ItemProcessingSteps.statusCriticality`, `DownloadJobs.stateCriticality`.
  - `isPackaged` / `hasIntro|hasCredits|hasRecap` are computed boolean columns
    (exists-predicates) the rewrite must reproduce.
- **`@Common.IsCalendarYear`** on `year`; URLs flagged `Core.IsURL`/
  `UI.IsImageURL` (rendering only — skipped otherwise).

### 1c. Drafts

**None.** Every entity carries `@odata.draft.enabled: false`. No DraftRoot /
DraftNode. The rewrite needs no draft state machine.

---

## 2. Action / Function catalogue (ALL via Spring REST — none are OData)

The CAP handler is empty; all "actions/functions" are Spring MVC endpoints.
Each table row: METHOD path, params, body, response, implementing class, effect.
All paths are also reachable behind the console proxy's `/katalog-api/` prefix.
Auth: JWT (except `/api/artwork/**` and the play/stream paths noted).

### 2a. Scan plane — `ScanController` (`/api/scan`)

| Method + Path | Params / Body | Returns | Effect |
|---|---|---|---|
| `POST /api/scan` | query `source` (default `nfs`; only `nfs` allowed) | 202 `{ID, source, status:"running", startedAt}` | Inserts a `scanjobs` row (status `running`), runs `NfsScanner.scan()` on a single daemon thread; on done updates `status=done, filesseen, itemsinserted, itemsupdated`; on error `status=failed, errormessage`. |
| `GET /api/scan/{id}` | path `id` | `{id,source,status,startedat,finishedat,errormessage,filesseen,itemsinserted,itemsupdated}` or 404 | Read one scan job (raw SQL, not OData). |
| `GET /api/scan` | query `limit` (default 50, clamp 1–200) | list of scan-job rows, `startedat desc` | List recent scans. |

### 2b. Full-text search — `SearchController` (`/api/search`)

| Method + Path | Params | Returns | Effect |
|---|---|---|---|
| `GET /api/search/items` | `q?`, `type?`, `genre?`, `year?` (Integer), `limit?` (default 50, max 200), `offset?` (default 0) | `{items:[{id,type,title,year,rating,score}], total, limit, offset}` | Postgres `tsvector`/`pg_trgm` ranked search (`search_vector @@ websearch_to_tsquery('simple', unaccent(q))` + trigram fallback); orders by score then similarity then `createdat desc`. `genre` filter via EXISTS over itemgenres/genres. (H2 fallback = ILIKE.) Rewrite: implement on the `items` search_vector column. |

### 2c. Artwork bytes — `ArtworkController` (`/api/artwork`)

| Method + Path | Returns | Effect / auth |
|---|---|---|
| `GET /api/artwork/{itemId}/{kind}` | image bytes (`contenttype`), `Cache-Control: public max-age=7d`, or 404 | Serves `com_nalet_katalog_itemartworkdata.bytes` for `(item_id,kind)`. `kind` ∈ poster/backdrop/logo/thumbnail. **permitAll** — bearer JWT OR HMAC `?stream=` token. The `posterUrl`/`backdropUrl` computed columns point here (`/katalog-api/api/artwork/{ID}/poster|backdrop`). |

### 2d. Direct streaming — `PlayController` (`/api/play`)

| Method + Path | Returns | Effect |
|---|---|---|
| `GET /api/play/{itemId}` | video bytes; `Accept-Ranges: bytes`, HTTP 200 or 206 partial; `Content-Range`; `416` on bad range | Byte-range streams the primary `playbackassets.path` (falls back to first asset) straight from NFS. Manual range parsing (suffix + open-ended supported). 404 if no asset or file missing. |

### 2e. Subtitles — `SubtitlesController` (`/api/subtitles`)

| Method + Path | Returns | Effect |
|---|---|---|
| `GET /api/subtitles/items/{itemId}` | `{subtitles:[{id,lang,label,format?,url:"/api/subtitles/{id}",default?}]}` | Lists `subtitleassets` for item (`isdefault desc, label asc`). Empty list (not error) if table missing. |
| `GET /api/subtitles/{subId}` | subtitle stream | Serves the track. Text (srt/vtt) → normalized to `text/vtt` (SRT→VTT on the fly, comma→dot ms, WEBVTT header). Bitmap: `pgs`→`application/pgs`, `vobsub`→`application/x-vobsub`, `dvb`→`application/dvb-subtitles` (no transcode). `Cache-Control: private max-age=300`. 404 if row/file missing. |

### 2f. Metadata enrichment (TMDB) — `EnrichmentController` (`/api/enrich`)

| Method + Path | Params / Body | Returns | Effect |
|---|---|---|---|
| `GET /api/enrich/status` | — | `{tmdbEnabled:bool}` | Reports `TmdbClient.isEnabled()` (TMDB_API_KEY set?). |
| `POST /api/enrich/items/{id}` | path `id` | `{itemId, status:(done|not_found|failed|skipped), message?}` | **Synchronous** single-item TMDB enrich (`EnrichmentService.enrichOne`). |
| `POST /api/enrich/pending` | `limit?` (default 50, max 1000), `type?` (movie|series) | 202 `{queued:<limit>, type}` | Background sweep on a daemon thread; logs `{itemsConsidered,itemsEnriched,itemsFailed}`. |
| `POST /api/enrich/backfill-episode-backdrops` | — | `{artworkData:<n>, artwork:<n>}` | One-shot: clone each episode's `kind='poster'` artwork row (bytes + url tables) into a `kind='backdrop'` row, idempotent. |
| `POST /api/enrich/retry-not-found` | `type?` | `{reset:<n>, type}` | Reset `itemprocessingsteps` rows `step='tmdb' AND status='skipped'` → `pending` (optionally narrowed by item type). |

### 2g. Analyzer work-queue — `AnalyzerController` (`/api/analyze`)

The pipeline orchestrator. Steps catalogue (see `ProcessingStepService`):
**ALLOWED_STEPS** = `scan, tmdb, tidb, chapter, chromaprint, blackframe,
silence, subtitle, transcode, package`. **ALLOWED_STATUS** = `pending,
in_progress, done, failed, skipped, not_applicable`. **ANALYZER_STEPS** (per-file
ML subset) = `chapter, chromaprint, blackframe, silence, subtitle, tidb`.

| Method + Path | Params / Body | Returns | Effect |
|---|---|---|---|
| `POST /api/analyze/claim` | `pass?` (default `per_file`; ∈ `per_file, tidb_first, transcoder, packager`), `limit?` (default 4, clamp 1–32) | `{pass, claimed:<n>, items:[{id,type,title,year,durationMs,path,seasonNumber,episodeNumber,seriesTitle?,seriesTmdbId,movieTmdbId}]}` | Atomic dequeue via `SELECT … FOR UPDATE SKIP LOCKED`. Claim filter per pass (transcode/package/tidb step pending, no in_progress sibling). Flips claimed step → `in_progress` for tidb_first/transcoder/packager. Orders `createdat DESC`. |
| `GET /api/analyze/items/{itemId}` | path | `{id,type,title,year,durationMs,path}` or 404 | Single item + primary playback path (packager resolves source). 404 if no primary asset. |
| `GET /api/analyze/items/{itemId}/steps` | path | `{itemId, steps:{<step>:<status>,...}}` | Current status of every step row for the item. |
| `POST /api/analyze/items/{itemId}/steps/skip` | body `{steps:[...], reason?}` | `{itemId, updated:<n>}` | Bulk-mark listed steps `not_applicable` (tidb_first short-circuit). |
| `PUT /api/analyze/items/{itemId}/steps/{step}` | body `{status, error?, details?}` | `{itemId, step, status}` (400 unknown step/status, 500 if 0 rows) | Upsert one step (see ProcessingStepService.upsert). **Chain promotion**: when `step=transcode` reaches `done|not_applicable|skipped`, auto-seeds `package=pending` (best-effort, never overwrites). |
| `POST /api/analyze/items/{itemId}/fail` | body `{reason?}` | `{itemId, status:"failed", stepsSkipped:<n>}` | Marks synthetic `scan` step `failed`; marks all still-pending/in_progress ANALYZER_STEPS `skipped`. |
| `GET /api/analyze/items/{itemId}/siblings` | `limit?` (default 5, clamp 1–12) | `{itemId, items:[{id,type,title,year,durationMs,path}]}` | Same-season sibling episodes w/ primary path (chromaprint cross-episode). |
| `POST /api/analyze/series/{seriesId}/reset` | path | `{seriesId, episodes:<n>, stepsReset:<n>, segmentsPurged:<n>}` | Bumps episode `createdat=now()` (re-queue head), resets ANALYZER_STEPS → pending (attempts preserved), deletes `mediasegments` where `source='chromaprint'`. |

### 2h. Segments ingest — `SegmentsController` (`/api/segments`)

ALLOWED_KINDS = `intro, recap, credits, preview`. ALLOWED_SOURCES = `tidb,
chapter, subtitle, silence, blackframe, chromaprint, whisper, transnet, manual`.

| Method + Path | Body | Returns | Effect |
|---|---|---|---|
| `PUT /api/segments/items/{itemId}` | `{segments:[{kind,source,startMs,endMs,confidence?,label?}]}` | `{itemId, written:<n>}` (400 on invalid kind/source/range, 404 unknown item) | Idempotent DELETE-then-batch-INSERT of `mediasegments` for the item. Validates `0<=start<end`. |
| `DELETE /api/segments/items/{itemId}` | — | `{itemId, removed:<n>}` | Clears all segments for item. |

### 2i. Chapters ingest — `ChaptersController` (`/api/chapters`)

| Method + Path | Body | Returns | Effect |
|---|---|---|---|
| `PUT /api/chapters/items/{itemId}` | `{chapters:[{startMs,endMs,title?,ordinal?}]}` | `{itemId, written:<n>}` (400 invalid range, 404 unknown item) | Idempotent DELETE-then-INSERT of `itemchapters`; ordinal defaults to array index. |
| `DELETE /api/chapters/items/{itemId}` | — | `{itemId, removed:<n>}` | Clears chapters for item. |

### 2j. Item operator actions — `ItemActionsController` (`/api/items`)

| Method + Path | Body | Returns | Effect |
|---|---|---|---|
| `POST /api/items/{itemId}/package` | — | item/series summary (`status`/`alreadyActive`/`episodesEnqueued`/`message`); 404 unknown, 400 for album/track | Enqueue packaging. Movie/episode → upsert `transcode=pending` unless transcode/package already in `done/in_progress/pending`. Series → fan-out to eligible episodes (have primary asset, not already done/in-progress/pending on transcode/package). |
| `POST /api/items/{itemId}/validate` | — | per-item `{code,message,sourcePath?,packagePath?,findings?}` or per-series rollup counts; 404 unknown | Filesystem+SQL validation. Codes: `ok, no_package, source_missing, stale, codec_mismatch, findings, not_applicable`. Checks `.complete` marker under sharded `/var/lib/katalog/packages/{cat}/{shard}/{itemId}/`, source mtime, packaged codec must be `hev1.`/`hvc1.`, hygiene findings (duplicate non-primary kinds, stray files < `validate.small_file_threshold_mb`). |
| `POST /api/items/{itemId}/packaging-complete` | full packager `manifest.json` (`{source{videoCodec,resolution,bitrateBps,size,durationMs}, renditions{video[],audio[]}, subtitles[], durationMs}`) | `{itemId, sourceEnriched, packagedAssetWritten, subtitlesWritten, audioTracks}`; 404 unknown | Packager sink. 3 mutations: (1) enrich primary `playbackassets` codec/res/bitrate/size; (2) replace `kind='packaged'` row (codec/res/bitrate from m3u8 BANDWIDTH, size = NFS walk, primary audio + track counts + durationMs); (3) replace all `subtitleassets` from manifest.subtitles[]. |

### 2k. Trailer fetch — `TrailerActionsController` (`/api/items`)

| Method + Path | Returns | Effect |
|---|---|---|
| `POST /api/items/{itemId}/fetch-trailers` | `{itemId,title,enqueued,packageId?,jobIds?,message}`; 404 unknown | Reads `itemtrailerlinks` where `localpath` null/empty, de-dups by URL, enqueues an oDownloader package via `TrailerIngestionService.enqueue`. Files land async via a `@Scheduled` poller. |

### 2l. Download console — `DownloadConsoleController` (`/api/downloads`)

Command side of the Downloads tile (read side = `DownloadJobs` OData + Kafka).

| Method + Path | Body / Params | Returns | Effect |
|---|---|---|---|
| `POST /api/downloads` | `{adapter, source, title?, wantedItemId?}` | 202 `{ok,adapter,clientJobId,message}`; 400 missing fields; 502 gateway error | Forwards to download-gateway `add` (adapter ∈ odownloader/qbittorrent/nzbget). |
| `DELETE /api/downloads/{adapter}/{clientJobId}` | path | `{ok,message}`; 502 on gateway error | Gateway cancel/remove. |
| `GET /api/downloads/clients` | — | raw JSON (gateway's configured adapters) | Drives the dialog dropdown. |

### 2m. Settings read — `SettingsController` (`/api/settings`)

| Method + Path | Returns | Effect |
|---|---|---|
| `GET /api/settings` | `{<key>:{valueText,valueType},...}` | Read-only map of all `settings` rows (workers cache ~5 min). CRUD itself goes through OData `Settings`. Also exposes in-process helpers `getOrNull/getCsv/getInt/getBool` (consumed by Validate). |

### 2n. Service-to-service write surface — `KatalogManagerRestController` (`/svc/v1`)

**Stub today** — class is empty (`// TODO`). Documented intent: gated on
`cloud_katalog_admin` role; future write endpoints `acquire`, `subtitles`,
`katalog-ingest`, `metadata-enricher`, each emitting `stube.library.item.*`
Kafka events. No live endpoints to port; capture as a planned seam.

---

## 3. `ProcessingStepService.upsert` semantics (shared by analyzer + enrichment)

Table `com_nalet_katalog_itemprocessingsteps`, unique `(item_id, step)`.
- Validates step ∈ ALLOWED_STEPS, status ∈ ALLOWED_STATUS (else
  `IllegalArgumentException` → 400). `error` truncated to 500 chars.
- INSERT … ON CONFLICT(item_id,step) DO UPDATE.
- `startedAt` sticky: set on **first** transition into `in_progress`.
- `finishedAt` sticky: set on transition into terminal (`done|failed|skipped`).
- `attempts` auto-increments on every conflict (starts at 1).
- `resetForItems(itemIds, steps)`: bulk reset rows → `pending`
  (`startedat/finishedat/error` nulled, `attempts` preserved).

`ItemOverallStatus` view rolls these per item into `overallStatus` ∈
`complete | partial_failure | failed | processing | queued | pending |
not_applicable` (+ the count columns).

---

## 4. Kafka / event surface (read-model wiring, for completeness)

- **Inbound** consumed by `DownloadEventConsumer` →
  `stube.download.client.{started,progress,completed,failed}` → upsert into
  `downloadjobs` (deterministic UUID = `nameUUIDFromBytes("adapter:clientJobId")`).
  Gated by `download-gateway.events-enabled` (default false).
- **Outbound** (planned, not yet emitted): `stube.library.item.*` from
  `/svc/v1`.

---

## 5. Quick digest for the Go+GraphQL rewrite

- **OData plane** = 23 entity sets at `/odata/v4/katalog-admin/`, pure CRUD,
  no actions/functions, no drafts. 5 are read-only; the 4 type-filtered media
  projections (Movies/Series/Episodes/Albums) + ItemOverallStatus +
  EnrichmentStatusCodes + DownloadJobs are read-only. Everything else writable.
- **All behavior** = the `/api/**` Spring controllers (sections 2a–2m) +
  `ProcessingStepService` step machine. Reproduce those as GraphQL
  mutations/queries (or keep as REST) — they are the real contract.
- **Computed columns** to reproduce (in views or resolvers): `posterUrl`,
  `backdropUrl`, `runtimeMin`, `yearText`, `sizeMB`, `isPackaged`,
  `hasIntro/hasCredits/hasRecap`, `statusCriticality`, `stateCriticality`.
- **Auth** = JWT issuer-only (Keycloak `nalet.cloud`), audience `katalog`
  optional; `/api/artwork/**` also accepts HMAC stream tokens; health + static
  + artwork are public.
</content>
</invoke>
