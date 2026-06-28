# 20 — REST: Items / Search / Scan / Enrich / Artwork / Play

Source (READ-ONLY): `stube/katalog-manager-api`, SAP CAP (Java/Spring Boot) catalog-management API.
Scope of this doc: the six hand-written Spring `@Controller`/`@RestController` classes under
`com.nalet.katalog.web` — **not** the CAP-generated OData surface.

These are **machine + browser contracts**. The Go+GraphQL rewrite must keep `ArtworkController`
(binary bytes + cache headers + stream-token query auth) and `PlayController` (HTTP byte-range
streaming) byte-compatible. The mutation/ingest endpoints (`packaging-complete`, scan, enrich)
are called by worker pods and must keep their JSON shapes.

## Global facts

- **No servlet context-path.** `server.port: 8080`, no `server.servlet.context-path`. All paths
  below are absolute from `/`. The `/katalog-api/` prefix the Fiori console UI uses is added by the
  **console reverse proxy**, not this app — the app sees `/api/...`.
- **All DB identifiers are lowercase.** Base tables `com_nalet_katalog_*`; `katalogservice_*` are
  views (not touched by these controllers). Access is raw `JdbcTemplate` (no ORM).
- **DB roles:** app connects as `cloud_katalog`; some tables (`subtitleassets`, `settings`) are
  owned by `postgres` and cannot be `ALTER`ed from the app (see `packaging-complete` note).
- **PVC path constant:** `PACKAGES_ROOT = /var/lib/katalog/packages` (hard-coded, every pod mounts
  the same PVC here). Package layout is **sharded**:
  `/var/lib/katalog/packages/{category}/{shard}/{itemId}/` where
  `category = movie→movies | episode→shows | track→music | else→items`,
  `shard = itemId.substring(0,2)` (or `"00"` if id < 2 chars).
  A package is "complete" iff the file `{pkgRoot}/.complete` exists.

### Auth (`security/SecurityConfig.java`)

- Stateless OAuth2 **resource server**, JWT bearer. CSRF disabled, no sessions.
- Public (permitAll, no token): `/healthz`, `/actuator/health/**`, `/katalog/**` (static UI bundle).
- When `auth.disabled=true` (dev/test): **everything** is permitAll.
- Otherwise **every** other request (all six controllers below) requires a valid JWT, **except**
  `/api/artwork/**` which is permitAll at the matcher level so it can be reached via stream-token.
- **JWT validation:** issuer-only by default
  (`issuer-uri` default `https://sso.nalet.cloud/realms/nalet.cloud`). Audience `katalog`
  (`KATALOG_AUDIENCE`) is **only enforced** when `katalog.audience.required=true` (default `false`).
  No scope/role checks — any valid issuer-token passes.
- **Stream-token alt auth** (`StreamTokenAuthFilter`, runs before the JWT filter, `shouldNotFilter`
  unless URI starts with `/api/artwork/`):
  - Reads `?stream=<token>` query param. If `STREAM_SIGNING_KEY` configured and token verifies,
    sets a Spring `Authentication` with principal = embedded userID, authority `ROLE_STREAM`.
  - **Token format** (HMAC-SHA256, must stay compatible with chino-api `internal/auth/stream.go`):
    `base64url(payload) "." base64url(HMAC-SHA256(payload, key))` where
    `payload = base64url( UTF8( "<userID>|<expUnix>" ) )`. Verify steps (`StreamTokenSigner.verify`):
    split on first `.`; recompute HMAC-SHA256 of the **ASCII bytes of the `payload` substring**
    (the pre-dot base64url string, *not* its decode); constant-time compare; base64url-decode payload;
    split on first `|`; reject if `now().epochSecond > expUnix`; return userID. Any malformed/expired
    token → `null` → falls through to bearer-JWT. Key is base64, **≥16 bytes** decoded. Unset key →
    verification disabled, artwork still works with a bearer JWT.

---

## SearchController — `@RequestMapping("/api/search")`

Tier-1 full-text search. Postgres path uses `tsvector` + `pg_trgm` + `unaccent`; H2 dev path falls
back to `ILIKE`. Branch chosen by `spring.datasource.platform` (default `postgres`) containing
"postgres".

### `GET /api/search/items`
- **Query params** (all optional): `q` (string, free text), `type` (string, exact match on
  `items.type`), `genre` (string, `ILIKE` against `genres.name`), `year` (int, exact),
  `limit` (int; clamp: `null|<=0|>200 → 50`), `offset` (int; `null|<0 → 0`).
- **Response** `200`, `application/json`, shape:
  ```json
  { "items": [ {row...} ], "total": <int = rows.size()>, "limit": <int>, "offset": <int> }
  ```
  `total` is the count of the **returned page**, NOT the full match count.
- **Row shape (Postgres):** `id, type, title, year, rating, score` (score = `ts_rank_cd` or `0`).
  **Row shape (H2):** `id, type, title, year, rating, score(=0)`.
- **Reads:** `com_nalet_katalog_items` (cols `id,type,title,year,rating,search_vector,createdat`);
  genre filter joins `com_nalet_katalog_itemgenres ig` + `com_nalet_katalog_genres g`.
- **Writes:** none. **External calls:** none.
- **Postgres query specifics (preserve ranking semantics):**
  - `score = CASE WHEN q empty/null THEN 0 ELSE ts_rank_cd(search_vector, websearch_to_tsquery('simple', unaccent(q))) END`.
  - When `q` non-blank, WHERE adds `(search_vector @@ websearch_to_tsquery('simple', unaccent(q)) OR unaccent(title) ILIKE '%q%')`.
  - `type` → `AND type = ?`; `year` → `AND year = ?`; `genre` → `AND EXISTS(... g.name ILIKE ?)`.
  - ORDER: with `q` → `score DESC, similarity(unaccent(title), unaccent(q)) DESC, createdat DESC`;
    without `q` → `createdat DESC`. Then `LIMIT ? OFFSET ?`.
  - **`genre` and `year` filters are silently ignored on the H2 branch** (`genre` entirely; only
    `q`/`type`/`year` honored there — actually H2 does honor `year` and `type`, drops `genre`).
- **Auth:** JWT required.

---

## ItemActionsController — `@Controller @RequestMapping("/api/items")`

Operator actions from the Fiori Object Pages + the packager ingest sink. `@ResponseBody` on each
method, JSON.

Helper service: `ProcessingStepService.upsert(itemId, step, status, error, details)` writes
`com_nalet_katalog_itemprocessingsteps` (see ProcessingStep contract at bottom).

### `POST /api/items/{itemId}/package`  — enqueue packaging
- **Path param:** `itemId` (string). **Body:** none.
- **Reads:** `com_nalet_katalog_items` (`id,type,title`); for series:
  `items` (child episodes), `com_nalet_katalog_playbackassets` (primary asset existence),
  `com_nalet_katalog_itemprocessingsteps` (active step check).
- **Writes:** `itemprocessingsteps` via `steps.upsert(id, "transcode", "pending", null, ...)`.
- **Behavior:**
  - Item not found → `404` `{ "error": "unknown item: <id>" }`.
  - Base body always: `{ itemId, type, title }`.
  - **type=series:** fan-out. Select episodes where `parent_id=itemId AND type='episode'` AND a
    primary playbackasset exists AND **not** already having a `transcode`/`package` step in
    `('done','in_progress','pending')`. For each, `upsert(epId,"transcode","pending",...)`.
    Response `200`: adds `episodesEnqueued`(int), `episodesTotal`(int = count of episodes), `message`.
  - **type not movie/episode/series:** `400` body `{ itemId,type,title, message:"Packaging is only available for movies and episodes." }`.
  - **type=movie|episode:** if `activeChainStep(itemId)` non-null (any `transcode`/`package` row in
    `done|in_progress|pending`, ordered by step → returns `"<step> <status>"`), `200` with
    `{ ..., status:"<step> <status>", alreadyActive:true, message:"Already <...> — no change." }`.
    Else `upsert(itemId,"transcode","pending",...)` and `200`
    `{ ..., status:"pending", alreadyActive:false, message:"Queued for transcoding. ..." }`.
- **Idempotent.** Chain semantics: enqueue always starts at `transcode=pending`; transcoder then
  packager run in sequence (`transcode → package` strict chain). A *failed* step is re-enqueueable.
- **External calls:** none (just writes the step row; workers poll the table).
- **Auth:** JWT required.

### `POST /api/items/{itemId}/validate`  — on-disk validation
- **Path param:** `itemId`. **Body:** none.
- **Reads:** `items` (`id,type,title`, child episode ids, `type`); `playbackassets`
  (primary `path`; `kind='packaged'` `codec`; all assets `id,kind,path,sizebytes`);
  `com_nalet_katalog_settings` via `SettingsController.getInt("validate.small_file_threshold_mb", 5)`.
  Filesystem stats under `PACKAGES_ROOT` + the source path.
- **Writes:** none.
- **Behavior:**
  - Not found → `404` `{ "error": "unknown item: <id>" }`.
  - **type=series:** per-episode roll-up. Response `200`:
    `{ itemId, type, title, episodes:<count>, ok, noPackage, sourceMissing, stale, codecMismatch, withFindings, message:"<n> ok, <n> not packaged, ..." }`.
  - **single item:** Response `200`:
    `{ itemId, type, title, code, message, sourcePath?, packagePath?, findings?[] }`.
- **`validateOne(itemId)` codes** (exact strings — UI keys off them):
  - `not_applicable` — no primary playback asset row.
  - `source_missing` — primary asset row exists but file gone. Carries `sourcePath`, maybe `packagePath`.
  - `no_package` — source exists, no `.complete` on disk. Carries `sourcePath`.
  - `stale` — `.complete` exists but source mtime > `.complete` mtime. Carries both paths.
  - `codec_mismatch` — `kind='packaged'` asset codec is non-blank and **not** prefixed `hev1.`/`hvc1.`
    (HEVC invariant; target `hev1.1.6.L120.B0`). Carries both paths.
  - `findings` — hygiene issues (precedence over `ok`, below the harder codes). `findings[]` entries:
    - `{ type:"stray_small_file", assetId, kind, path, sizeBytes, thresholdMb }` for any non-`primary`
      asset with `0 < size < threshold_mb*1024*1024`.
    - `{ type:"duplicate_assets", kind, count }` for any non-`primary` kind appearing >1×.
  - `ok` — source + package present, mtimes consistent, codec HEVC.
- **mtime read failure** (`IOException`) is logged and treated as `ok` (not fatal).
- **External calls:** none. **Auth:** JWT required.

### `POST /api/items/{itemId}/packaging-complete`  — packager ingest sink (machine contract)
Called by `katalog-packager` after writing `.complete`. **Idempotent** (safe retry).
- **Path param:** `itemId`. **Body:** `application/json` — the packager's `manifest.json` verbatim.
- **Request body fields consumed** (Jackson → `Map<String,Object>`, lenient coercion):
  - `source: { videoCodec, resolution, bitrateBps, size, durationMs }`
  - `renditions: { video: [ {codec, width, height, bitrateBps} ], audio: [ {codec, language, channels, bitrateBps, default(bool)} ] }`
  - `subtitles: [ {path(rel), format, language, title, default(bool)} ]`
  - `durationMs` (top-level, for packaged row)
- **Three mutations (per call):**
  1. **UPDATE** primary `playbackassets` (`isprimary=true AND kind='primary'`):
     `codec, resolution, bitratekbps, sizebytes` via `COALESCE(?, col)` from `source.*`.
     `srcBitrateKbps` = `bitrateBps/1000` if present else `(size*8)/durationMs` (bytes*8/ms = kbps).
  2. **DELETE** then **INSERT** the `kind='packaged'` `playbackassets` row (single). Inserted cols:
     `id(uuid), item_id, path(=<pkgRoot>/manifest.json), codec, resolution(=WxH from video[0]),
     bitratekbps, sizebytes, isprimary=false, kind='packaged', audiocodec, audiolanguage,
     audiochannels, audiobitratekbps, audiotrackcount, subtitletrackcount, durationms`.
     - `pkgKbps` = max `BANDWIDTH=` parsed from `<pkgRoot>/hls/master.m3u8` (regex `BANDWIDTH=(\d+)`,
       /1000); fallback `video[0].bitrateBps/1000`.
     - `pkgSize` = `Files.walk(pkgRoot)` sum of all regular file sizes (NFS walk).
     - primary audio = first `default:true` else `audio[0]`.
     - Only written when `renditions.video` is non-empty.
  3. **DELETE** all `com_nalet_katalog_subtitleassets` for item, then **INSERT** one row per
     `subtitles[]`: `id(uuid), item_id, path(=<pkgRoot>/<relPath>), format, lang(=language),
     label(=title), isdefault(=default==true)`. (`visible` flag is **not** mirrored — table owned by
     `postgres`.)
- **Reads:** `items` (existence check; `type` for `packageRootFor`). **Writes:** the three tables above.
- **Response `200`:**
  `{ itemId, sourceEnriched:<bool>, packagedAssetWritten:<bool>, subtitlesWritten:<int>, audioTracks:<int> }`.
  Not found → `404` `{ "error": "unknown item: <id>" }`.
- **External calls:** none (filesystem only). **Auth:** JWT required (worker presents a token).

---

## ScanController — `@RestController @RequestMapping("/api/scan")`

Manual NFS scan trigger + status. Scan runs async on a single-thread daemon executor.

### `POST /api/scan`  — trigger a scan
- **Query param:** `source` (default `"nfs"`). Only `"nfs"` accepted; else `400`
  `{ "error": "unsupported scan source: <source>" }`.
- **Side-effects:** generates `jobId=UUID`; **INSERT** into `com_nalet_katalog_scanjobs`
  `(id, source, status='running', startedat, filesseen=0, itemsinserted=0, itemsupdated=0)`;
  submits `runScan(jobId)` to the executor. `runScan` calls `NfsScanner.scan()` (walks the NFS mount,
  inserts/updates `items` + `playbackassets` etc. — see scanner spec) then **UPDATE**s the scanjob row
  to `status='done'` with `finishedat, filesseen, itemsinserted, itemsupdated`, or on exception to
  `status='failed'` with `finishedat, errormessage`.
- **Response `202 Accepted`:**
  `{ "ID": <jobId>, "source": <source>, "status": "running", "startedAt": <Timestamp.toString()> }`.
  **NOTE the key is uppercase `ID`** (CAP convention), not `id` — preserve exactly.
- **Reads/Writes:** writes `com_nalet_katalog_scanjobs`; the async scan reads/writes catalog tables
  via `NfsScanner`.
- **External:** NFS filesystem (via scanner). **Auth:** JWT required (audience=katalog).

### `GET /api/scan/{id}`  — scan job status
- **Path param:** `id`. **Response `200`** = the scanjob row:
  `id, source, status, startedat, finishedat, errormessage, filesseen, itemsinserted, itemsupdated`.
  Not found → `404` (empty body). **Reads:** `com_nalet_katalog_scanjobs`. **Auth:** JWT required.

### `GET /api/scan`  — recent scan jobs
- **Query param:** `limit` (default `50`; clamp `<=0|>200 → 50`).
- **Response `200`:** JSON **array** of scanjob rows (cols as above), `ORDER BY startedat DESC`.
- **Reads:** `com_nalet_katalog_scanjobs`. **Auth:** JWT required.

`NfsScanner.Result` = `{ int filesSeen, int itemsInserted, int itemsUpdated }`.

---

## EnrichmentController — `@RestController @RequestMapping("/api/enrich")`

TMDB metadata enrichment + maintenance. Sync single-item; async bulk sweeps on a single-thread
daemon executor. Backed by `EnrichmentService` + `TmdbClient` + `ChaptersDbClient`.

### `GET /api/enrich/status`
- **Response `200`:** `{ "tmdbEnabled": <bool> }` (= `TmdbClient.isEnabled()`, i.e. `TMDB_API_KEY` set).
- **Reads/Writes:** none. **External:** none. **Auth:** JWT required.

### `POST /api/enrich/items/{id}`  — enrich one item synchronously
- **Path param:** `id`. **Body:** none.
- **Behavior:** `service.enrichOne(id)` → `Result.toMap()`. Always `200` `application/json`:
  `{ itemId, status:("done"|"not_found"|"failed"|"skipped"), message? }`. (Item-not-found is reported
  as `status:"failed"` in the body, still HTTP `200`.)
- **Reads:** `items`; `itemexternalids`; `genres`/`itemgenres`; `people`/`itempeople`; `mediasegments`;
  `itemchapters`; for series → child episode `items`. **Writes:** `items` (description/rating/
  durationms/tagline/year/sorttitle/title via COALESCE), `itemexternalids` (tmdb/imdb/tmdb-episode),
  `genres`+`itemgenres`, `people`+`itempeople`, `itemtrailerlinks` (source='tmdb', replace-where-
  `downloadedat IS NULL`), `itemartwork` (url rows), `itemartworkdata` (bytes), `mediasegments`+
  `itemchapters` (chaptersdb), `itemprocessingsteps` (`tmdb` step transitions).
- **External calls:**
  - **TMDB v3** (`https://api.themoviedb.org/3`, `Authorization: Bearer <TMDB_API_KEY>`):
    `search/movie`, `movie/{id}`, `movie/{id}/credits`, `movie/{id}/videos`, `search/tv`,
    `tv/{id}`, `tv/{id}/credits`, `tv/{id}/videos`, `tv/{id}/season/{s}/episode/{e}`.
    Image bytes from `https://image.tmdb.org/t/p/{size}{path}` (poster `w780`, backdrop `w1280`,
    episode still `w500`).
  - **chaptersdb.com** (only if `CHAPTERSDB_ENABLED`): findShow + getMovieChapters.
  - Cast capped to 12; crew filtered to `job=="Director"`. Trailers filtered to type Trailer/Teaser
    on site YouTube/Vimeo.
- **Enrichment status mapping** into `itemprocessingsteps.step='tmdb'`:
  `in_progress→in_progress, done→done, not_found→skipped, failed→failed`.
- **Auth:** JWT required.

### `POST /api/enrich/pending`  — async bulk sweep
- **Query params:** `limit` (default `50`; clamp `<=0|>1000 → 50`), `type` (optional `movie|series`;
  blank→both `type IN ('movie','series')`).
- **Behavior:** returns immediately `202 Accepted` with `{ queued:<lim>, type:(type|"movie+series") }`,
  then in background `service.enrichPending(lim, typeFilter)`. Queue = items whose `tmdb` step is
  `pending` **or** have no `tmdb` step row, `ORDER BY items.createdat ASC LIMIT lim`.
- **Reads/Writes/External:** same surface as single enrich, batched. **Auth:** JWT required.

### `POST /api/enrich/backfill-episode-backdrops`  — one-shot backfill
- **Body/params:** none.
- **Behavior:** for every `type='episode'` item with a `kind='poster'` artwork row but **no**
  `kind='backdrop'` row, clone poster→backdrop. Two INSERT...SELECTs:
  - `com_nalet_katalog_itemartworkdata` (`id=gen_random_uuid()::text, item_id, kind='backdrop',
    contenttype, bytes, fetchedat`).
  - `com_nalet_katalog_itemartwork` (`id=gen_random_uuid()::text, item_id, kind='backdrop', url`).
  - Idempotent (NOT EXISTS guard on backdrop).
- **Response `200`:** `{ "artworkData": <rows_inserted>, "artwork": <rows_inserted> }`.
- **Reads/Writes:** `itemartworkdata`, `itemartwork`, joins `items`. **External:** none. **Auth:** JWT.

### `POST /api/enrich/retry-not-found`  — reset skipped tmdb steps
- **Query param:** `type` (optional). **Behavior:** set every `itemprocessingsteps` row with
  `step='tmdb' AND status='skipped'` back to `status='pending'`
  (`startedat=NULL, finishedat=NULL, error=NULL, modifiedat=now()`); `type` narrows via join to
  `items.type`.
- **Response `200`:** `{ "reset": <int rows>, "type": (type|"*") }`.
- **Reads/Writes:** `itemprocessingsteps` (+ `items` join when typed). **External:** none. **Auth:** JWT.

---

## ArtworkController — `@RestController @RequestMapping("/api/artwork")`  (BINARY/BROWSER CONTRACT)

Serves cached artwork bytes from DB. Reachable with **bearer JWT OR `?stream=<token>`** (see auth).

### `GET /api/artwork/{itemId}/{kind}`
- **Path params:** `itemId` (string), `kind` (string, e.g. `poster` / `backdrop`).
- **Query param (auth only):** `stream` (optional signed token; consumed by the filter, not the
  handler).
- **Reads:** `com_nalet_katalog_itemartworkdata` (`SELECT contenttype, bytes WHERE item_id=? AND kind=?`).
  Takes the **first** row. **Writes:** none. **External:** none.
- **Response:**
  - `200 OK` with body = raw `bytes` (the `bytea`).
    - `Content-Type` = row `contenttype` (parsed via `MediaType.parseMediaType`), default `image/jpeg`.
    - `Cache-Control: max-age=604800, public` (`maxAge(7 days).cachePublic()`).
  - `404 Not Found` (empty body) when: no row, OR `bytes` is not a `byte[]`, OR `bytes.length == 0`.
- **Auth:** **NOT** behind the JWT requirement at the matcher (permitAll), BUT serves only to a request
  that is either bearer-JWT-authenticated on the same chain or stream-token-authenticated. In
  `auth.disabled` mode it's fully open. Keep `?stream=` URL stability (survives OIDC silent renew) —
  `<img src>` URLs depend on it.

---

## PlayController — `@Controller @RequestMapping("/api/play")`  (BYTE-RANGE STREAMING CONTRACT)

Direct HTTP byte-range streaming of the source media file from the NFS mount. Writes straight to the
servlet `OutputStream` (bypasses Spring's `ResourceRegion` converter, which refuses
`video/x-matroska`).

### `GET /api/play/{itemId}`
- **Path param:** `itemId`. **Request header (optional):** `Range: bytes=...`.
- **Asset resolution (reads `com_nalet_katalog_playbackassets`):**
  1. `SELECT path WHERE item_id=? AND isprimary=true LIMIT 1`.
  2. Fallback if none: `SELECT path WHERE item_id=? ORDER BY path LIMIT 1`.
  3. Still none → `sendError(404, "no playback asset for item")`.
  - File-not-found / unreadable on disk → `sendError(404, "file not found on filesystem")`.
- **Writes:** none. **External:** NFS file read only.
- **Always-set response headers (before body):**
  - `Accept-Ranges: bytes`
  - `Content-Disposition: inline; filename="<file name>"`
  - `Content-Type` = `MediaTypeFactory.getMediaType(file)` else `application/octet-stream`.
- **No `Range` header → full body:** status `200 OK`, `Content-Length: <fileSize>`, streams whole file
  in 64 KiB buffers.
- **`Range: bytes=...` parsing (must stay compatible):**
  - Only handled when header value starts with `bytes=`. Spec after `bytes=` is trimmed.
  - No `-` in spec → `416` with `Content-Range: bytes */<len>`.
  - Multi-range (comma) → **only the first range honored** (`end` truncated at first comma).
  - Suffix range (`bytes=-N`): `start = max(0, len-N)`, `end = len-1`; `N<=0` → `416`.
  - `bytes=S-`   → `start=S`, `end=len-1`.
  - `bytes=S-E`  → `start=S`, `end=E`.
  - Non-numeric → `416` with `Content-Range: bytes */<len>`.
  - Out of bounds (`start<0 || end>=len || start>end`) → `416` with `Content-Range: bytes */<len>`.
  - Valid partial → status `206 Partial Content`,
    `Content-Range: bytes <start>-<end>/<len>`, `Content-Length: <end-start+1>`. Skips to `start`
    (with read+discard fallback when `skip()` returns 0), streams `length` bytes in 64 KiB buffers.
- **Client abort** (browser seek/close → `ClientAbortException`) logged at debug, not an error.
- **Auth:** JWT required (no stream-token path here — only `/api/artwork/**` gets the filter).

---

## ProcessingStep contract (`ProcessingStepService`) — shared by package/enrich

Table `com_nalet_katalog_itemprocessingsteps`. **Unique key `(item_id, step)`** (ON CONFLICT upsert).
- **ALLOWED_STEPS:** `scan, tmdb, tidb, chapter, chromaprint, blackframe, silence, subtitle,
  transcode, package`. **ALLOWED_STATUS:** `pending, in_progress, done, failed, skipped,
  not_applicable`. Unknown step/status → `IllegalArgumentException` (→ HTTP 500 unless mapped).
- **upsert(itemId, step, status, error, details):** insert
  `(id=gen_random_uuid()::varchar, createdat, item_id, step, status, startedat, finishedat,
  attempts=1, error, details)`; on conflict updates `status, modifiedat, attempts+1`,
  **sticky** `startedat` (set once on first `in_progress`), **sticky** `finishedat`
  (set when status ∈ done/failed/skipped). `error` truncated to 500 chars. Note the explicit
  `::timestamp` casts inside CASE branches (Postgres infers `text` otherwise — Go impl must cast too).
- `transcode → package` is a strict chain: package only becomes pending after transcode reaches
  done/not_applicable. transcode owned by GPU transcoder (NVENC, skip→not_applicable when already
  HEVC); package owned by CPU packager (shaka → CMAF/HLS).
- `resetForItems(ids, steps)`: bulk reset to `pending` via `item_id = ANY(?) AND step = ANY(?)`.

## SettingsController (`/api/settings`) — referenced by validate

`GET /api/settings` → `{ "<key>": { valueText, valueType }, ... }` from `com_nalet_katalog_settings`.
In-process helpers used here: `getInt("validate.small_file_threshold_mb", 5)`. (Also `getCsv`,
`getBool`, `getOrNull` exist for workers; table owned by `postgres`.)

---

## Tables touched (summary)

| Table | Read by | Written by |
|---|---|---|
| `com_nalet_katalog_items` | search, package, validate, enrich, scan(async) | enrich (UPDATE metadata), scan |
| `com_nalet_katalog_playbackassets` | package, validate, packaging-complete, play | packaging-complete (UPDATE primary; DELETE/INSERT packaged), scan |
| `com_nalet_katalog_itemprocessingsteps` | package(active check), enrich(queue) | package, enrich, retry-not-found, packaging chain |
| `com_nalet_katalog_scanjobs` | scan GET/list | scan POST + async runScan |
| `com_nalet_katalog_itemartworkdata` | artwork GET, validate(no), enrich | enrich (bytes), backfill-episode-backdrops |
| `com_nalet_katalog_itemartwork` | enrich | enrich (url), backfill-episode-backdrops |
| `com_nalet_katalog_subtitleassets` | — | packaging-complete (DELETE/INSERT) |
| `com_nalet_katalog_genres` / `_itemgenres` | search(genre), enrich | enrich |
| `com_nalet_katalog_people` / `_itempeople` | — | enrich |
| `com_nalet_katalog_itemexternalids` | enrich | enrich |
| `com_nalet_katalog_itemtrailerlinks` | — | enrich |
| `com_nalet_katalog_mediasegments` / `_itemchapters` | — | enrich (chaptersdb) |
| `com_nalet_katalog_settings` | validate, settings GET | — (read-only here) |

## External services (summary)

| Service | Caller | Config | Auth |
|---|---|---|---|
| TMDB v3 + image CDN | EnrichmentService/TmdbClient | `TMDB_API_KEY`, `TMDB_LANGUAGE`(en-US) | `Authorization: Bearer <key>` |
| chaptersdb.com | EnrichmentService/ChaptersDbClient | `CHAPTERSDB_ENABLED` | (per client) |
| NFS scanner | ScanController/NfsScanner | filesystem mount | n/a |
| stream-token mint (chino-api) | (upstream) verified here | `STREAM_SIGNING_KEY` (base64, ≥16 bytes) | HMAC-SHA256 |
| Keycloak OIDC | SecurityConfig | `issuer-uri` (`sso.nalet.cloud/realms/nalet.cloud`) | bearer JWT |
