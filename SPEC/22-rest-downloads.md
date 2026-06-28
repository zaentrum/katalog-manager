# 22 — REST: Downloads, Trailers, Settings, Manager (CQRS split)

Source (READ-ONLY, SAP CAP Java): `stube/katalog-manager-api`.
Files covered:
- `web/DownloadConsoleController.java` — download command side (forwards to download-gateway)
- `web/TrailerActionsController.java` — trailer fetch (forwards to oDownloader)
- `web/SettingsController.java` — read-only settings HTTP surface + in-process helpers
- `manager/web/KatalogManagerRestController.java` — service-to-service write surface (stub, no endpoints yet)
- Supporting: `download/DownloadGatewayClient.java`, `download/DownloadGatewayProperties.java`,
  `download/DownloadEventConsumer.java`, `odownloader/TrailerIngestionService.java`,
  `odownloader/OdownloaderClient.java`, `odownloader/OdownloaderProperties.java`

All controllers sit behind the JWT resource-server filter chain. Reached via the console reverse
proxy which prepends the `/katalog-api/` prefix. All DB identifiers are lowercase in Postgres
(CAP source uses CamelCase in SQL; Postgres folds unquoted identifiers to lowercase).

---

## 1. Download CQRS split (ADR-019 / ADR-020)

Two directions, never crossed:

- **COMMAND side** (this controller → download-gateway over REST). `DownloadConsoleController`
  → `DownloadGatewayClient`. Issues add / remove / list-clients. Never queries the DB.
- **READ side** (gateway → Kafka → projection table). `DownloadEventConsumer` consumes
  `stube.download.client.*` and projects into `com_nalet_katalog_downloadjobs`. The Fiori
  "Downloads" tile binds the OData entity `DownloadJobs` (view `katalogservice_downloadjobs`).
  The command controller NEVER reads this table.

### 1.1 `DownloadConsoleController` — `@RequestMapping("/api/downloads")`

#### POST `/api/downloads` — enqueue a download
- Request body (JSON object, all strings):
  - `adapter` (required) — selects client: `odownloader` | `qbittorrent` | `nzbget`
  - `source` (required) — URL / magnet / NZB the client understands
  - `title` (optional)
  - `wantedItemId` (optional)
- Validation: `adapter` and `source` must be non-blank (trimmed) → else `400` with
  `{ "error": "adapter and source are required" }`.
- Forwards to `DownloadGatewayClient.add(adapter, source, title, wantedItemId)`:
  - `POST {gateway.url}/api/v1/downloads`
  - gateway request body (snake_case): `{ "adapter", "source", "title", "wanted_item_id" }`
    (null title/wantedItemId sent as `""`).
  - gateway response parsed as `Map`; controller reads `client_job_id`.
- Success → `202 Accepted`:
  `{ "ok": true, "adapter": <adapter>, "clientJobId": <client_job_id|"">, "message": "Queued on <adapter>" }`
- Gateway error (`GatewayException`) → `502 Bad Gateway` with `{ "error": <message> }`.
- DB: NONE (command side does not touch the DB).

#### DELETE `/api/downloads/{adapter}/{clientJobId}` — cancel / remove
- Path vars: `adapter`, `clientJobId` (job identity = `(adapter, clientJobId)`).
- Forwards to `DownloadGatewayClient.remove(adapter, clientJobId)`:
  - `DELETE {gateway.url}/api/v1/downloads/{adapter}/{clientJobId}` (both segments
    URL-path-encoded; `+` → `%20`; clientJobId is opaque = packageId / infohash).
- Success → `200 OK` `{ "ok": true, "message": "Cancelled" }`.
- Gateway error → `502` `{ "error": <message> }`.
- DB: NONE.

#### GET `/api/downloads/clients` — list configured adapters
- No params. Forwards to `DownloadGatewayClient.clients()`:
  - `GET {gateway.url}/api/v1/clients`
- Returns the gateway's raw JSON body **verbatim as a `String`** (Content negotiation:
  returns `ResponseEntity<String>`). Example body: `["odownloader"]`.
- Drives the "New download" dialog's adapter dropdown.
- When gateway disabled or non-2xx → returns literal `"[]"` (never errors).
- DB: NONE.

### 1.2 `DownloadGatewayClient` (command client) — config + contracts

`DownloadGatewayProperties` (`@ConfigurationProperties("download-gateway")`):
- `download-gateway.url` (default `""`) — base URL of gateway control plane,
  e.g. `http://download-gateway.stube.svc.cluster.local:8080`.
- `download-gateway.events-enabled` (default `false`) — gate for the Kafka read-side consumer.
- `isEnabled()` ⇔ `url` non-blank. When disabled, `add`/`remove` throw
  `GatewayException("download-gateway integration is disabled (download-gateway.url unset)")`;
  `clients()` returns `"[]"`.

Gateway REST contract (consumed):
| Method | Path | Req body | Resp |
|---|---|---|---|
| POST | `/api/v1/downloads` | `{adapter, source, title, wanted_item_id}` | `{ client_job_id, ... }` (full map echoed) |
| DELETE | `/api/v1/downloads/{adapter}/{id}` | — | 2xx = ok |
| GET | `/api/v1/clients` | — | JSON array of adapter names |

HTTP: JDK `HttpClient`, connect timeout 5s, request timeout 15s (add/remove) / 5s (clients).
Non-2xx or transport error → `GatewayException` carrying `HTTP <code> <body-excerpt(≤200ch)>`.
Gateway is unauthenticated inside the namespace (fronted by console proxy + OData auth inbound).

### 1.3 `DownloadEventConsumer` (read-side projection) — Kafka → DownloadJobs

- Bean only present when `download-gateway.events-enabled=true` (`@ConditionalOnProperty`).
- `@KafkaListener` `topicPattern = "stube\\.download\\.client\\..*"`,
  `groupId = ${download-gateway.kafka.group-id:stube-katalog-manager}`,
  container factory `downloadKafkaListenerContainerFactory`.
- Event kind = last dot-segment of topic: `started` | `progress` | `completed` | `failed`.
  Unknown kinds ignored. Payload is JSON (gateway `events.go`, snake_case).
- Common required fields: `adapter`, `client_id` (drop event + WARN if either missing).
- Row identity: PK `id` = deterministic `UUID.nameUUIDFromBytes("<adapter>:<client_id>")`.
  Upserts use `ON CONFLICT (adapter, clientJobId)` (composite natural key).
- Poison-message safe: any exception is logged + swallowed (never stalls the partition).

Event field → column mapping (table `com_nalet_katalog_downloadjobs`):

| Event | Topic suffix | Event fields read | Columns written |
|---|---|---|---|
| started | `started` | `title`, `wanted_item_id`, `size_bytes`, `started_at`(ms) | title, wantedItemId, state=`queued`, sizeBytes, startedAt, lastEventAt |
| progress | `progress` | `state`(default `downloading`), `progress_pct`, `downloaded_bytes`, `size_bytes`, `speed_bps`, `eta_sec`, `emitted_at`(ms) | state*, progressPct, downloadedBytes, sizeBytes(coalesce), speedBps, etaSec, lastEventAt |
| completed | `completed` | `wanted_item_id`, `size_bytes`, `files`(JSON arr→text), `completed_at`(ms) | state=`completed`, progressPct=100, wantedItemId(coalesce), sizeBytes(coalesce), files, completedAt, lastEventAt |
| failed | `failed` | `error`, `failed_at`(ms) | state=`failed`, errorMessage, lastEventAt |

\* progress state is sticky: once row state is `completed` or `failed`, progress events do NOT
overwrite it (`CASE WHEN state IN ('completed','failed') THEN keep ELSE new`).

Timestamps: gateway sends epoch-**millis**; missing/zero → `now()`.
`files` stored as raw JSON text (default `"[]"`).

State vocabulary in the table: `queued`, `downloading`, `completed`, `failed` (gateway-driven).

---

## 2. Trailer ingestion (oDownloader) — `TrailerActionsController`

`@RequestMapping("/api/items")`. Single endpoint. Movie/Episode Object Page "Download Trailer"
button. This is a SEPARATE pipeline from the generic download-gateway above; it talks directly to
the oDownloader daemon and has its own job table + scheduled poller.

### POST `/api/items/{itemId}/fetch-trailers`
- Path var: `itemId`.
- Server flow:
  1. `SELECT title FROM com_nalet_katalog_items WHERE id = ?` → `404`
     `{ "error": "unknown item: <itemId>" }` if absent.
  2. `SELECT id, url, title FROM com_nalet_katalog_itemtrailerlinks
      WHERE item_id = ? AND (localpath IS NULL OR localpath = '')`
     (links are pre-populated by the Refresh-from-TMDB action which writes YouTube URLs there;
     already-downloaded links carry a non-null `localpath` and are skipped).
  3. If no eligible links → `200 OK`
     `{ itemId, title, enqueued: 0, message: "No trailers to fetch. ..." }`.
  4. De-dup links by `url` (preserve order), keep first `trailer_link_id` per url.
  5. `TrailerIngestionService.enqueue(itemId, title, uniqueUrls, trailerLinkIdByUrl)`.
  6. `200 OK`:
     `{ itemId, title, enqueued: <jobIds.size>, packageId: <firstPkg|null>, jobIds: [...], message }`.
- External: oDownloader (NOT download-gateway). DB: reads `com_nalet_katalog_items`,
  `com_nalet_katalog_itemtrailerlinks`.

### 2.1 `TrailerIngestionService.enqueue` (synchronous, `@Transactional`)
- Disabled (oDownloader off) → `EnqueueResult([], null, "oDownloader integration disabled ...")`.
- Empty urls → `EnqueueResult([], null, "No URLs supplied.")`.
- **One URL = one oDownloader package** (JD plugin fans each YouTube watch URL into ~5 variants
  `.srt/.txt/.jpg/.m4a/.mp4`; separate packages keep job↔variant mapping unambiguous).
- Per URL: `OdownloaderClient.addLinks([url], packageName=title, comment="katalog item=<itemId>")`
  → `AddLinksResult{packageId, downloadIds}`. On null (rejected) → skip + WARN.
- Inserts one row into `com_nalet_katalog_trailerjobs`:
  `(id=UUID, createdat, modifiedat, item_id, trailer_link_id, source_url, package_id,
    download_id=NULL, state='queued', attempts=0)`.
- `EnqueueResult` record: `{ List<String> jobIds, String packageId (first pkg), String message }`.
  All rejected → `EnqueueResult([], null, "oDownloader rejected every URL. ...")`.

### 2.2 `TrailerIngestionService.pollAndIngest` — `@Scheduled` ingestion poller
- `fixedDelay = ${odownloader.poll-interval-seconds:15} * 1000` ms, `initialDelay=30_000`.
  Single-threaded.
- No-op when oDownloader disabled.
- Claims active rows:
  `SELECT ... FROM com_nalet_katalog_trailerjobs WHERE state IN ('queued','running','downloaded')
   ORDER BY createdat ASC LIMIT 50`.
- Timeout cutoff = `now() - odownloader.timeout-minutes` (default 60). If `started_at < cutoff`
  → mark `state='timeout'` and stop.
- Per row: `OdownloaderClient.listDownloadsByPackage(package_id)` →
  `pickBestVideoVariant` = largest file whose name ends `.mp4|.mkv|.mov`
  (excludes audio-only `.m4a`/opus-webm + sidecars `.srt/.txt/.jpg`). Null ⇒ no video yet,
  update `message` (progress text) and wait.
- Pin chosen variant: UPDATE `download_id`, `state=mapState(odState)`, `message`,
  `bytes_done`, `bytes_total`, `started_at=COALESCE(...)`, `modifiedat`.
- On oDownloader `state == "FINISHED"`:
  - `client.openContent(downloadId)` streams bytes (auth `Bearer <token>`) into
    `inboxRoot/{itemId}/{sanitizedFilename}` where
    `inboxRoot = ${odownloader.inbox-root:/var/lib/katalog/packages/_inbox}`.
  - `writeTrailerAsset`: DELETE then INSERT `com_nalet_katalog_playbackassets`
    `(id=UUID, item_id, path, sizebytes, isprimary=false, kind='trailer')` (idempotent by item+path).
  - If `trailer_link_id` present: UPDATE `com_nalet_katalog_itemtrailerlinks`
    SET `localpath=<target>`, `downloadedat=now()`, `modifiedat=now()`.
  - `markImported`: UPDATE trailerjobs `state='imported'`, `final_path`, `bytes_done`,
    `finished_at=COALESCE(...)`, `modifiedat`.
  - I/O error → delete partial file, `state='failed'`, message `Import I/O error: ...`, rethrow.
- Per-row exception → `bumpAttempts` (`attempts = attempts + 1`, message ≤500ch).

`mapState` (oDownloader → trailerjobs.state):
`RUNNING→running`, `FINISHED→downloaded`, `FAILED|ERROR→failed`, `QUEUED|PENDING→queued`,
default→`queued`. Terminal/other states set by service: `imported`, `timeout`, `failed`.

`sanitizeFilename`: replace `/ \ \0` with `_`, trim, keep unicode; blank → `<downloadId>.bin`.

### 2.3 `OdownloaderClient` — config + contracts
`OdownloaderProperties` (`@ConfigurationProperties("odownloader")`):
- `odownloader.url` (default `""`) — in-cluster Service hostname (bypasses public route /
  oauth2-proxy), e.g. `http://odownloader.cloud-nalet-odownloader.svc.cluster.local:8686`.
- `odownloader.token` (default `""`) — static bearer token from daemon's `odownloader-api` secret.
- `odownloader.poll-interval-seconds` (default `15`).
- `odownloader.timeout-minutes` (default `60`).
- `odownloader.inbox-root` (default `/var/lib/katalog/packages/_inbox`) — read in
  `TrailerIngestionService` via `@Value`, not on the props bean.
- `isEnabled()` ⇔ both `url` and `token` non-blank. Disabled ⇒ every client method
  short-circuits (returns null/empty).

oDownloader REST contract (consumed; all auth `Authorization: Bearer <token>`,
`Accept: application/json`):
| Method | Path | Req | Resp |
|---|---|---|---|
| POST | `/api/v1/links/add` | `{ urls:[...], packageName, comment, autostart:true }` | `AddLinksResult{ packageId, downloadIds:[...] }` |
| GET | `/api/v1/downloads?packageId=<id>&size=200` | — | `{ items: [ DownloadStatus ] }` |
| GET | `/api/v1/downloads/{id}` | — | `DownloadStatus` |
| GET | `/api/v1/downloads/{id}/content` | — | raw file bytes (stream) |

`DownloadStatus` record (subset, `@JsonIgnoreProperties(ignoreUnknown=true)`):
`{ id, packageId, name, bytesDone:long, bytesTotal:long, speedBytesPerSecond:long, state, message }`.
oDownloader `state`: `RUNNING|FINISHED|FAILED|QUEUED|PENDING|ERROR` (mapped above).
HTTP: connect 5s; GET timeout 15s, POST 30s. Best-effort: failures log WARN, return null/empty.

---

## 3. Settings — `SettingsController` (`@RequestMapping("/api/settings")`)

Read-only HTTP surface so worker pods pull global settings with one cheap GET (no OData).
CRUD lives on the CAP OData entity `/odata/v4/katalog-admin/Settings` (Fiori Settings tile) —
NOT here. Workers cache the GET ~5 min.

### GET `/api/settings`
- No params. `SELECT key, valueText, valueType FROM com_nalet_katalog_settings`.
- Response: JSON object keyed by setting `key`; each value `{ valueText, valueType }`.
  Inactive/unknown keys are simply absent (caller falls back to compile-time default).
- DB: `com_nalet_katalog_settings`.

### In-process helpers (NOT HTTP endpoints — used by other Java controllers, e.g. Validate)
- `getOrNull(key)` → `SELECT valueText ... WHERE key = ?`, trimmed string or `null`.
- `getCsv(key)` → split on `,`, trim, lowercase, drop empties; unset → `[]`.
- `getInt(key, fallback)` → parse int, bad/missing → `fallback`.
- `getBool(key, fallback)` → true for `true|1|yes`, false for `false|0|no` (case-insensitive),
  else `fallback`.

### Settings keys referenced

| Key | Type | Consumer | Default / behavior |
|---|---|---|---|
| `validate.small_file_threshold_mb` | int | `ItemActionsController` (line 370) via `settings.getInt(...)` | fallback `5` |
| packager language whitelist | csv | Python packager worker (via GET `/api/settings`) | named in SettingsController javadoc; exact key set by worker, not in this repo |
| anime fallback | bool | Python worker (via GET `/api/settings`) | named in javadoc; key owned by worker |

NOTE: the only Settings key with a concrete literal in *this* Java codebase is
`validate.small_file_threshold_mb`. The "packager language whitelist" and "anime fallback"
settings are described in the controller javadoc but their literal keys live in the Python
worker code (out of this repo). The Go rewrite must serve whatever keys the workers request
generically — the GET endpoint is key-agnostic (returns all rows).

`com_nalet_katalog_settings` columns: `id, key, valuetext(varchar 2000), valuetype(varchar 20),
description(text), createdat, createdby, modifiedat, modifiedby`. `key` is varchar(120).

---

## 4. `KatalogManagerRestController` — `@RestController @RequestMapping("/svc/v1")`

- Service-to-service WRITE surface (sibling stube services mutate catalog without Fiori).
- Gated on role `cloud_katalog_admin`. Read-only consumers use `katalog-api` instead.
- **Currently a STUB — zero endpoints implemented.** Body is a single
  `// TODO: inject CdsRuntime / JdbcTemplate / KafkaProducer once endpoints land.`
- Documented (javadoc-only) future endpoints, each must validate caller service-account audience
  (per-service Keycloak client id), emit an audit log line, and produce a
  `stube.library.item.*` Kafka event AFTER the DB write commits:
  - `acquire` — register newly-discovered candidate items (ADR-017); confirm release group +
    quality profile.
  - `subtitles` — attach a downloaded subtitle blob to an item (ADR-017, replaces Bazarr write).
  - `katalog-ingest` — bulk-upsert items from a scheduled Kafka consume cycle (ADR-006);
    idempotent by external id.
  - `metadata-enricher` — patch metadata fields from the LLM enrichment job (ADR-005).
- Go rewrite: no contract to port yet; carry forward the `/svc/v1` prefix + admin role gate +
  audit + post-commit `stube.library.item.*` emission convention.

---

## 5. DB tables touched (authoritative live schema, all-lowercase)

### `com_nalet_katalog_downloadjobs` (download read model — view `katalogservice_downloadjobs`)
PK `id varchar(36)` (only index). Columns:
`id, createdat, createdby, modifiedat, modifiedby, adapter varchar(40), clientjobid varchar(255),
title varchar(500), wanteditemid varchar(80), state varchar(20), progresspct numeric,
downloadedbytes bigint, sizebytes bigint, speedbps bigint, etasec integer, files text,
errormessage text, startedat ts, completedat ts, lasteventat ts`.
View `katalogservice_downloadjobs` adds computed `statecriticality int`:
`failed→1, downloading→2, queued→2, completed→3, else 0`.

> **SPEC TRAP for the Go rewrite:** `DownloadEventConsumer` upserts with
> `ON CONFLICT (adapter, clientjobid)`, but `live_indexes.sql` shows ONLY the `id` PK index —
> there is **no unique index on (adapter, clientjobid)** in the captured schema. The CAP code
> assumes one exists. The Go rewrite MUST create a `UNIQUE (adapter, clientjobid)` constraint
> (or upsert on it) for the projection to work. `id` is a derived UUID of `adapter:clientjobid`,
> so a unique index on `id` is functionally equivalent and could also back the upsert.

### `com_nalet_katalog_trailerjobs` (trailer ingestion read/work model)
> **SPEC TRAP:** this table is **NOT present in `live_schema.txt` / `live_views.sql` /
> `live_indexes.sql`** — the captured live schema has zero rows for `trailerjobs`. The CAP
> ingestion service reads/writes it heavily. The Go rewrite MUST define it. Columns used by
> the code (infer types):
> `id (uuid/varchar36 PK), createdat ts, modifiedat ts, item_id varchar36, trailer_link_id varchar36 NULL,
> source_url text, package_id varchar, download_id varchar NULL, state varchar
> ('queued'|'running'|'downloaded'|'imported'|'failed'|'timeout'), attempts int,
> started_at ts NULL, finished_at ts NULL, bytes_done bigint, bytes_total bigint,
> message text(≤500), final_path text`.

### `com_nalet_katalog_itemtrailerlinks` (view `katalogservice_itemtrailerlinks`)
Read by trailer fetch; `localpath`+`downloadedat` stamped on successful import. PK `id`.
Cols: `id, item_id varchar36, url varchar(2048), title varchar(255), site varchar(40),
source varchar(20), externalid varchar(120), durationsec int, publishedat ts,
localpath varchar(2048), downloadedat ts, createdat/by, modifiedat/by`.

### `com_nalet_katalog_playbackassets` (view `katalogservice_playbackassets`)
Trailer import inserts `kind='trailer'` rows (idempotent delete+insert by item+path).
Insert cols used: `id, item_id, path, sizebytes, isprimary(=false), kind`.
Full cols include codec/audio/resolution metadata + computed `sizemb` in the view.

### `com_nalet_katalog_settings`
`id, key varchar(120), valuetext varchar(2000), valuetype varchar(20), description text,
createdat/by, modifiedat/by`.

### `com_nalet_katalog_items`
Read for title lookup in trailer fetch (`SELECT title WHERE id = ?`).

---

## 6. External integrations summary

| Integration | Transport | Direction | Enabled when | Endpoints |
|---|---|---|---|---|
| download-gateway | REST (JDK HttpClient) | command (out) | `download-gateway.url` set | POST/DELETE `/api/v1/downloads`, GET `/api/v1/clients` |
| download-gateway events | Kafka consumer | read (in) | `download-gateway.events-enabled=true` | topics `stube.download.client.{started,progress,completed,failed}` |
| oDownloader | REST (JDK HttpClient, Bearer token) | both (out + poll) | `odownloader.url` AND `odownloader.token` set | POST `/api/v1/links/add`, GET `/api/v1/downloads[?packageId]`, GET `/api/v1/downloads/{id}[/content]` |
| catalog manager svc | REST (`/svc/v1`) | write (in, stub) | — | none yet; future `stube.library.item.*` emit |

Kafka topic naming convention: `stube.{domain}.{event}` (download read side =
`stube.download.client.*`; manager write side will emit `stube.library.item.*`).
