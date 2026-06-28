# 40 — Platform & Runtime Contract

Source of record: `stube/katalog-manager-api` (SAP CAP Java + Spring Boot 3.5.10, JDK 21,
CDS 4.7.0). This document is the runtime/platform contract the Go + GraphQL rewrite must
reproduce. The catalog data contract (tables/columns) is in `live_schema.txt` /
`live_views.sql` / `live_indexes.sql`; this file covers config, auth, ports, topics, image,
and the **consumed OData/REST surface** the Fiori UI actually calls.

All Postgres identifiers are lowercase. Base tables are `com_nalet_katalog_*`;
`katalogservice_*` are VIEWS with computed columns (the CAP OData projections — see §7).

---

## 1. Environment variables / config properties

Resolution order (Spring): env var → `application.yaml` default. Property names below use the
yaml key; the `${ENV:default}` form shows the env override + the baked default. Values in the
**k8s** column are what `k8s/deployment.yaml` sets in the `stube` namespace.

### 1.1 Server / management

| yaml key | env var | default | k8s | purpose |
|---|---|---|---|---|
| `server.port` | `SERVER_PORT` | `8080` | `8080` | HTTP listen port |
| `server.forward-headers-strategy` | — | `native` | — | honour `X-Forwarded-*` from console proxy |
| `management.endpoints.web.exposure.include` | — | `health,info` | — | only health+info actuators exposed |
| `management.endpoint.health.probes.enabled` | — | `true` | — | enables `/actuator/health/liveness` + `/readiness` |
| — | `CDS_INDEX_PAGE_ENABLED` | (unset) | `"true"` | CAP index page at `/` for ops curl (no Fiori bundled here) |

### 1.2 Security / OIDC

| yaml key | env var | default | k8s | purpose |
|---|---|---|---|---|
| `spring.security.oauth2.resourceserver.jwt.issuer-uri` | `SPRING_SECURITY_OAUTH2_RESOURCESERVER_JWT_ISSUER_URI` | `https://sso.nalet.cloud/realms/nalet.cloud` | same | OIDC issuer; JWKS auto-discovered |
| `katalog.audience` | `KATALOG_AUDIENCE` | `katalog` | `katalog` | required audience **only if** `katalog.audience.required=true` |
| `katalog.audience.required` | — | `false` | (unset→false) | when false, **issuer-only** validation (audience NOT enforced in MVP) |
| `auth.disabled` | `AUTH_DISABLED` | `false` | (unset→false) | when true: `anyRequest().permitAll()`, no JWT, no stream filter |
| `stream.signing-key` | `STREAM_SIGNING_KEY` | `` (empty) | secret `chino-stream-signing/key` (optional) | base64 HMAC key for `?stream=` tokens (see §5) |
| `cds.security.authentication.authConfig.enabled` | — | `false` | — | CAP's own security filter chain DISABLED; Spring Security is the only enforcement |

### 1.3 Datasource (see §2)

| env var | source | purpose |
|---|---|---|
| `SPRING_DATASOURCE_URL` | secret `katalog-db/url` | JDBC URL for Postgres `katalog` |
| `SPRING_DATASOURCE_USERNAME` | secret `katalog-db/user` | DB user (`cloud_katalog`; some objects owned by `postgres`) |
| `SPRING_DATASOURCE_PASSWORD` | secret `katalog-db/password` | DB password |
| `spring.datasource.platform` | `postgres` (SearchController default) | drives Postgres-vs-H2 SQL branch in `/api/search` |

Profile `default` (dev/test) overrides to H2: `spring.sql.init.platform=h2`,
`cds.data-source.auto-config.enabled=true`. **Production runs the non-default profile against
Postgres.**

### 1.4 Kafka / download plane (see §3)

| yaml key | env var | default | k8s | purpose |
|---|---|---|---|---|
| `download-gateway.url` | `DOWNLOAD_GATEWAY_URL` | `` | `http://download-gateway.stube.svc.cluster.local:8080` | command side REST base URL |
| `download-gateway.events-enabled` | `DOWNLOAD_GATEWAY_EVENTS_ENABLED` | `false` | `"false"` | start Kafka projection consumer (OFF by default) |
| `download-gateway.kafka.brokers` | `KAFKA_BROKERS` | `` | `platform-kafka-kafka-bootstrap.platform-event-streaming.svc:9093` | Kafka bootstrap |
| `download-gateway.kafka.group-id` | `KAFKA_GROUP_ID` | `stube-katalog-manager` | `stube-katalog-manager` | consumer group |
| `download-gateway.kafka.tls-cert` | `KAFKA_TLS_CERT` | `/etc/kafka-cert/user.crt` | same | mTLS client cert (PEM) |
| `download-gateway.kafka.tls-key` | `KAFKA_TLS_KEY` | `/etc/kafka-cert/user.key` | same | mTLS client key (PEM) |
| `download-gateway.kafka.tls-ca` | `KAFKA_TLS_CA` | `/etc/kafka-cert/ca.crt` | same | mTLS truststore CA (PEM) |

### 1.5 Scanner / TMDB / oDownloader

| yaml key | env var | default | k8s | purpose |
|---|---|---|---|---|
| `scanner.nfs.root` | `NFS_ROOT` | `/var/lib/katalog/media` | same | NFS media mount root (read-only PVC `media`) |
| `tmdb.api-key` | `TMDB_API_KEY` | `` | secret `katalog-tmdb/api-key` (optional) | TMDB key; empty → enrichment disabled, `tmdbEnabled=false` |
| `tmdb.language` | `TMDB_LANGUAGE` | `en-US` | (unset) | TMDB locale |
| `odownloader.url` | `ODOWNLOADER_API_URL` | `http://odownloader.cloud-nalet-odownloader.svc.cluster.local:8686` | `http://odownloader.stube.svc.cluster.local:8686` | in-cluster oDownloader daemon |
| `odownloader.token` | `ODOWNLOADER_API_TOKEN` | `` | secret `odownloader-api/token` (optional) | static bearer; empty → whole integration no-ops |
| `odownloader.poll-interval-seconds` | `ODOWNLOADER_POLL_INTERVAL_SECONDS` | `15` | (unset) | `@Scheduled` trailer-ingestion poll cadence |
| `odownloader.timeout-minutes` | `ODOWNLOADER_TIMEOUT_MINUTES` | `60` | (unset) | download timeout |
| `odownloader.inbox-root` | `ODOWNLOADER_INBOX_ROOT` | `/var/lib/katalog/packages/_inbox` | (unset→default) | where finished trailer downloads land (RW PVC `packages`) |

### 1.6 Other env (k8s only)

| env var | k8s | purpose |
|---|---|---|
| `CHAPTERSDB_ENABLED` | `"false"` | toggles `ChaptersDbClient` (external chapters DB lookup); off |
| `KEYCLOAK_KATALOG_CLIENT_ID` / `_SECRET` | secret `katalog-oidc` (optional) | reserved; not used by resource-server JWT validation in MVP |
| `JAVA_TOOL_OPTIONS` | `-XX:MaxRAMPercentage=80.0 -XX:+UseContainerSupport` | heap obeys container limits (set in Dockerfile) |

---

## 2. Datasource

- **DB**: Postgres database `katalog` (live, authoritative). All identifiers lowercase.
- **Driver**: `cds-feature-postgresql` (runtime). H2 only on the `default` dev/test profile.
- **JDBC URL/user/pass** injected from k8s secret `katalog-db` (keys `url`/`user`/`password`).
- **Ownership split (important for the rewrite)**: app user is `cloud_katalog`; some objects
  (e.g. `subtitleassets` columns) are owned by `postgres` and the app **cannot ALTER** them —
  schema changes require a postgres-owner migration. `gen_random_uuid()` and `unaccent()` are
  used in app SQL (pgcrypto + unaccent extensions present).
- **Full-text search** (`/api/search`): `search_vector tsvector` column + `pg_trgm`
  (`websearch_to_tsquery('simple', unaccent(?))`, `ts_rank_cd`, `similarity()`). H2 branch
  falls back to `ILIKE`.
- App writes raw SQL via `JdbcTemplate` against base tables `com_nalet_katalog_*` (NOT the
  views) for all custom controllers. CAP OData reads/writes go through the `katalogservice_*`
  views.

---

## 3. Kafka

- **Bootstrap**: `platform-kafka-kafka-bootstrap.platform-event-streaming.svc:9093` (shared
  platform cluster; stube is a tenant).
- **Group**: `stube-katalog-manager`.
- **Security**: **mTLS**, native Kafka PEM (`security.protocol=SSL`,
  `ssl.keystore.type=PEM` inline cert+key, `ssl.truststore.type=PEM` inline CA). **No SASL.**
  Certs are PEM files mounted from secret `stube-katalog-manager` at `/etc/kafka-cert/`
  (`user.crt`, `user.key`, `ca.crt`). KafkaUser name: `stube-katalog-manager`.
- **Consumer config**: `auto.offset.reset=earliest`, String key+value deserializers.
- **Consumed topics** (read-model projection, CONSUME only): topic pattern
  `stube\.download\.client\..*` — concretely:
  - `stube.download.client.started`
  - `stube.download.client.progress`
  - `stube.download.client.completed`
  - `stube.download.client.failed`
- **Event payloads**: JSON, **snake_case** (mirrors the gateway's Go structs). Fields read:
  `adapter`, `client_id` (identity), `title`, `wanted_item_id`, `state`, `size_bytes`,
  `downloaded_bytes`, `speed_bps`, `eta_sec`, `progress_pct`, `files` (array), `error`,
  and epoch-millis timestamps `started_at`/`emitted_at`/`completed_at`/`failed_at`.
  → projected (UPSERT on `(adapter, clientJobId)`) into `com_nalet_katalog_DownloadJobs`.
  Row `ID` = `UUIDv3(nameUUIDFromBytes("adapter:client_id"))` (deterministic).
  State precedence: once `completed`/`failed`, progress events do NOT downgrade state.
- **CRITICAL boot trap**: there is deliberately **no** `spring.kafka` / `spring.ssl.bundle`
  config — Spring resolves SSL bundles eagerly and a missing cert crashes the whole service
  (CrashLoopBackOff). All Kafka wiring is built in Java, gated `@ConditionalOnProperty(
  download-gateway.events-enabled=true)`; if certs are missing it sets `autoStartup=false`
  and the app stays healthy. **Default is events DISABLED.** The Go rewrite must keep the
  download-event consumer optional and non-fatal when broker/cert is absent.
- **Producing**: none today. (The future `KatalogManagerRestController` `/svc/v1` is a stub
  documenting an intent to emit `stube.library.item.*` after writes — not implemented.)

---

## 4. OIDC / Security model

### 4.1 Authentication

- **Type**: OAuth2 Resource Server, Bearer JWT (`Authorization: Bearer <jwt>`).
- **Issuer**: `https://sso.nalet.cloud/realms/nalet.cloud` (Keycloak). JWKS auto-discovered
  via `NimbusJwtDecoder.withIssuerLocation(issuer)`.
- **Validation**: default validators + issuer (`JwtValidators.createDefaultWithIssuer`).
  Audience binding is **wired but off** (`katalog.audience.required=false`) — MVP validates
  issuer only. Flipping `katalog.audience.required=true` enforces `aud` contains `katalog`
  (the `audience-katalog` Keycloak protocol mapper already sets it on upstream clients).
- **Session**: `STATELESS`. **CSRF disabled.** No roles/scopes are checked (no
  `hasRole`/`hasAuthority` anywhere) — any valid issuer JWT is authorized for all protected
  paths. (Service-to-service role `cloud_katalog_admin` is mentioned only in a doc comment on
  the unimplemented `/svc/v1` stub.)

### 4.2 Public vs protected paths

Public (`permitAll`, no auth):
- `/healthz`
- `/actuator/health/**`
- `/katalog/**` (static UI bundle path — served by the UI nginx, not this API)

Conditionally public:
- `/api/artwork/**` — `permitAll` at the matcher, but accepts **either** a Bearer JWT **or**
  a valid `?stream=` token (see §5). The stream-token filter runs first; on success it sets a
  `ROLE_STREAM` auth and the request short-circuits.

Everything else (`anyRequest().authenticated()`) requires a valid Bearer JWT. This includes
all OData (`/odata/v4/katalog-admin/**`), `/api/play/**`, `/api/scan`, `/api/search`,
`/api/enrich`, `/api/items/**`, `/api/analyze/**`, `/api/segments/**`, `/api/chapters/**`,
`/api/subtitles/**`, `/api/settings`, `/api/downloads/**`.

When `auth.disabled=true`, the whole chain is `permitAll` and neither JWT nor stream filter
is installed (dev/test only).

---

## 5. StreamToken signing scheme (MUST reproduce EXACTLY)

Used to let `<img src=".../api/artwork/{id}/{kind}?stream=TOKEN">` survive OIDC silent renews
(stable URLs). Minted by **chino-api** (`internal/auth/stream.go`, `auth.Signer`); verified
here by `StreamTokenSigner`. chino-stream / chino-web verify the same way — **do not change**.

- **Algorithm**: HMAC-SHA256 (`HmacSHA256`).
- **Shared secret**: env `STREAM_SIGNING_KEY`, **base64-encoded**, decoded to raw bytes.
  Must be ≥ 16 bytes after base64-decode. Same value across chino-api, katalog, chino-stream
  (k8s secret `chino-stream-signing`, key `key`). Empty/unset → verification disabled
  (verify returns null; artwork falls back to Bearer-only).
- **Token format** (opaque to clients):
  ```
  token   = base64url(payload) "." base64url(HMAC-SHA256(payload_ascii_bytes, key))
  payload = base64url( userID "|" expUnix )           # UTF-8, then base64url
  ```
  - The HMAC is computed over the **base64url payload string's US-ASCII bytes** (i.e. over the
    left segment as it appears in the token, NOT over the raw `userID|expUnix`).
  - Both segments use **URL-safe base64** (`Base64.getUrlDecoder()` on verify). Padding: Java's
    URL decoder accepts with or without `=` padding.
  - Inner payload decodes to UTF-8 string `userID|expUnix`; split on the **first** `|`.
  - `expUnix` = expiry as **Unix epoch seconds** (`Long`). Reject if
    `now_epoch_seconds > expUnix`.
- **Transport**: query parameter named `stream` (`?stream=<token>`). Only honoured on
  `/api/artwork/**` (filter `shouldNotFilter` = path not starting with `/api/artwork/`).
- **On valid token**: sets a Spring `UsernamePasswordAuthenticationToken` with principal =
  `userID`, authority `ROLE_STREAM`. Malformed/expired/badsig → returns null, filter falls
  through to Bearer JWT (never throws).
- **Constant-time compare**: `MessageDigest.isEqual` on the HMAC bytes.

Pseudocode (verify) the Go side must match:
```
dot = indexOf('.'); reject if dot<1 or dot==len-1
payloadStr = token[0:dot]; sigStr = token[dot+1:]
expected = HMAC_SHA256(key, ASCII_bytes(payloadStr))
got      = base64url_decode(sigStr)
body     = base64url_decode(payloadStr)
reject unless constant_time_eq(expected, got)
s = utf8(body); pipe = indexOf(s,'|'); reject if pipe<1
userID = s[0:pipe]; expUnix = parseLong(s[pipe+1:])
reject if now_unix_seconds > expUnix
return userID
```

---

## 6. Ports, context path, health, metrics, image

- **Listen port**: `8080` (containerPort `http`).
- **Context path**: NONE on the app itself (root `/`). The **`/katalog-api` prefix is added by
  the console reverse-proxy**, not by this service. The app serves `/odata/...`, `/api/...`
  directly; the proxy maps `console.../katalog-api/*` → service `/*`.
  - **Computed artwork URLs are baked WITH the prefix**: CAP projections emit
    `'/katalog-api/api/artwork/' || ID || '/poster'` (and `/backdrop`) as `posterUrl` /
    `backdropUrl`. The Go rewrite must emit the same `/katalog-api/api/artwork/{id}/{kind}`
    strings so existing UIs/clients resolve images. (Path prefix is hard-coded in the view
    SQL; see migration 003.)
- **Health**:
  - `GET /healthz` — public, ops.
  - `GET /actuator/health/liveness` — k8s liveness probe (initialDelay 30s, period 30s).
  - `GET /actuator/health/readiness` — k8s readiness probe (initialDelay 15s, period 15s).
- **Metrics**: actuator exposure limited to `health,info` — **no `/actuator/prometheus`
  or `/metrics` exposed** today.
- **UI bundle**: NOT served by this API (API-only). Fiori SPA lives in `katalog-manager-ui`
  (nginx static, also port 8080); console proxy fronts both.
- **Image / build**:
  - Runtime image: `eclipse-temurin:21-jre` (distroless-ish JRE). Runs as UID 1001, GID 0,
    non-root, `runAsNonRoot`, seccomp `RuntimeDefault`. HOME `/app`, jar `/app/app.jar`.
  - Build stage: `maven:3.9-eclipse-temurin-21` → `cds build --for java` + `mvn package`
    → `katalog-manager-api-exec.jar`.
  - Deployed image: `registry.nalet.cloud/stube/katalog-manager-api:latest`, namespace
    `stube`, `replicas: 1`, RollingUpdate `maxUnavailable:0/maxSurge:1`.
  - Resources: requests `cpu 100m / mem 512Mi`; limits `cpu 1 / mem 1Gi`.
  - Volumes: PVC `media` (RO, `/var/lib/katalog/media`), PVC `packages` (RW,
    `/var/lib/katalog/packages`), secret `stube-katalog-manager` (RO,
    `/etc/kafka-cert`, optional).
  - ServiceAccount `katalog-manager-api`.

---

## 7. Consumed surface — OData entity sets + actions/functions the Fiori UI calls

OData service root: **`/katalog-api/odata/v4/katalog-admin/`** (CDS service `KatalogService`,
`@path:'katalog-admin'`). Model: `manifest.json` `dataSources.KatalogService`. UI is Fiori
Elements (`sap.fe.templates` ListReport + ObjectPage), so it issues standard OData V4
`$filter`/`$select`/`$expand`/`$orderby`/`$search`/`$count` plus CRUD. `groupId:$direct`,
`operationMode:Server`, `autoExpandSelect:true`.

### 7.1 OData entity sets bound by the UI (routes/targets in manifest.json)

| Entity set | UI usage | Mutability via OData |
|---|---|---|
| `Movies` | ListReport + ObjectPage (`where type='movie'`) | `@readonly` (read) |
| `Series` | ListReport + ObjectPage (`where type='series'`) | `@readonly` |
| `Episodes` | ObjectPage + nested under `Series/children` (`where type='episode'`) | `@readonly` |
| `Albums` | ListReport + ObjectPage (`where type='album'`) | `@readonly` |
| `Items` | ListReport + ObjectPage (unified power-user view) | **CRUD** (redirection target) |
| `ScanJobs` | ListReport + ObjectPage | read (rows created via REST) |
| `ItemProcessingSteps` | ListReport ("Processing" tile) + ObjectPage + per-item facet | read |
| `Settings` | ListReport + ObjectPage | **CRUD** (operator edits key/value) |
| `DownloadJobs` | ListReport ("Downloads" tile) + ObjectPage | `@readonly` (Kafka projection) |

Entity sets bound only as **facets / value-helps / expands** (still must exist in the GraphQL
surface): `ItemGenres`, `ItemPeople`, `PlaybackAssets` (Files facet; computed `sizeMB`),
`SubtitleAssets`, `MediaSegments` (Segments facet + Timeline `$expand segments($select=kind,
startMs,endMs,source)`), `ItemChapters`, `ItemTrailerLinks`, `ItemExternalIds`,
`ItemDiagnostics` (read via `bindList(<item>/diagnostics, $select=ID,sourcePath,sourceSize,
sourceMtime,generatedAt,ffprobeData,folderListing,notes)`), `Genres` (value-help),
`People`, `ItemArtwork`, `ItemTags`, `EnrichmentStatusCodes` (lookup), `ItemOverallStatus`
(`@readonly`, navigated as `overallStatus.overallStatus` on every item projection).

### 7.2 Computed / projected columns the UI relies on (must be in GraphQL types)

- `Items`/`Movies`/`Series`/`Episodes`/`Albums`: `posterUrl`, `backdropUrl`
  (`/katalog-api/api/artwork/{ID}/{poster|backdrop}`), `runtimeMin` (=`durationMs/60000`),
  `yearText` (=`cast(year as String)`).
- `Movies`/`Series`/`Episodes`: `isPackaged : Boolean` (exists asset codec `hev1%`/`hvc1%`;
  Series = all child episodes packaged).
- `Episodes`: `hasIntro`/`hasCredits`/`hasRecap : Boolean` (exists `segments[kind=...]`).
- `PlaybackAssets`: `sizeMB : Integer` (=`sizeBytes/1048576`).
- `ItemProcessingSteps`: `statusCriticality : Integer` (failed→1, in_progress/pending→2,
  done→3, else 0; `@UI.Hidden`).
- `DownloadJobs`: `stateCriticality : Integer` (failed→1, downloading/queued→2, completed→3,
  else 0); plus `@UI.DataPoint` Progress (`progressPct`/100) + State.
- `ItemOverallStatus.overallStatus : String` (complete | partial_failure | failed |
  processing | queued | pending | not_applicable) — surfaced as a filter + column on lists.

### 7.3 OData actions / functions

**None.** The CDS service defines **no bound/unbound actions or functions** (the comment in
`katalog-service.cds` explicitly keeps scan/search/operator-actions out of OData). All
operator "actions" are custom **REST** endpoints (§8) invoked by `fetch()` from the FE
extension controllers — they are NOT OData operations. The GraphQL rewrite should model these
as **mutations** (or REST shims) rather than as OData actions.

### 7.4 OData `$search` / `$filter` fields

`@cds.search` enabled on `Movies`/`Series`/`Albums`/`Items` over `{title, description}`
(contains match → FE filter-bar free-text box). SelectionFields (filterable) per list:
- Movies/Series: `title, year, rating, overallStatus.overallStatus, isPackaged,
  genres.genre.name`
- Albums: `title, year, rating, genres.genre.name`
- Items: `type, year, rating, overallStatus.overallStatus, genres.genre.name`
- ItemProcessingSteps: `status, step, modifiedAt, item.type, item.title`
- ScanJobs: `source, status`; Settings: `key, valueType`; DownloadJobs: `adapter, state`
- Genre filter uses `$filter=genres/any(g: g/genre/name eq …)` via the `Genres.name` value-help.

---

## 8. Custom REST surface (Spring MVC controllers) — also part of the consumed contract

All under root (proxy adds `/katalog-api`). All require Bearer JWT except `/api/artwork/**`
(JWT or `?stream=`). These back the FE buttons and the worker/service pipeline. The GraphQL
rewrite must cover (as queries/mutations or kept-as-REST) at least the UI-invoked ones; the
worker-facing ones may stay REST.

### 8.1 UI-invoked (from FE extension controllers — load-bearing for the UI)

| Method + path | Called by | Body / params | Returns |
|---|---|---|---|
| `POST /api/items/{itemId}/package` | ObjectPage "Migrate / Package" | — | `{message, status?, episodesEnqueued?, episodesTotal?}` |
| `POST /api/items/{itemId}/validate` | ObjectPage "Validate" | — | `{code, message, sourcePath?, packagePath?, findings?}` or series roll-up `{episodes, ok, noPackage, sourceMissing, stale, codecMismatch, withFindings, message}` |
| `POST /api/enrich/items/{id}` | ObjectPage "Refresh from TMDB" | — | enrichment `Result.toMap()` (`status`/`tmdbId`/`changes`/...) |
| `POST /api/items/{itemId}/fetch-trailers` | ObjectPage "Download Trailer" | — | `{enqueued, packageId, jobIds, message}` |
| `POST /api/downloads` | Downloads "New Download" dialog | `{adapter, source, title?, wantedItemId?}` JSON | `{ok, adapter, clientJobId, message}` |
| `DELETE /api/downloads/{adapter}/{clientJobId}` | Downloads "Cancel" | — | `{ok, message}` |
| `GET /api/downloads/clients` | (dialog dropdown source) | — | gateway clients JSON |
| `GET /api/artwork/{itemId}/{kind}` | `<img>` posterUrl/backdropUrl | path: kind (`poster`/`backdrop`) | image bytes, `Cache-Control public max-age=7d`; 404 if absent |

`kind` for artwork is the row's `kind` column (`poster`, `backdrop`, `logo`, `thumbnail`).
Bytes from `com_nalet_katalog_itemartworkdata(item_id, kind) → contenttype, bytes`.

### 8.2 Player / asset serving

| Method + path | Purpose |
|---|---|
| `GET /api/play/{itemId}` | Byte-range stream of the primary playback asset file (NFS). Honours `Range: bytes=`, emits `206`/`Accept-Ranges`/`Content-Range`, single-range only. Resolves `com_nalet_katalog_playbackassets WHERE item_id=? AND isprimary=true` (fallback any). |
| `GET /api/subtitles/items/{itemId}` | JSON list `{subtitles:[{id, lang, label, format, url:/api/subtitles/{id}, default?}]}` |
| `GET /api/subtitles/{subId}` | Serve track; SRT→WebVTT on the fly (`text/vtt`); PGS→`application/pgs`, VobSub→`application/x-vobsub`, DVB→`application/dvb-subtitles` (binary passthrough) |

### 8.3 Scan / search / settings / enrich (mixed UI + service)

| Method + path | Params | Notes |
|---|---|---|
| `POST /api/scan` | `?source=nfs` | async; inserts `scanjobs` row, returns `202 {ID,source,status,startedAt}` |
| `GET /api/scan/{id}` | — | one scan job |
| `GET /api/scan` | `?limit` (≤200, def 50) | scan history |
| `GET /api/search/items` | `?q,&type,&genre,&year,&limit(≤200/50),&offset` | `{items, total, limit, offset}`; Postgres FTS or H2 ILIKE |
| `GET /api/settings` | — | `{key:{valueText,valueType}}` (workers cache ~5min) |
| `GET /api/enrich/status` | — | `{tmdbEnabled}` |
| `POST /api/enrich/pending` | `?limit(≤1000/50),&type` | async sweep `{queued,type}` |
| `POST /api/enrich/backfill-episode-backdrops` | — | one-shot artwork clone |
| `POST /api/enrich/retry-not-found` | `?type` | reset `tmdb` steps `skipped→pending` |

### 8.4 Worker/service pipeline (analyzer, transcoder, packager, ingest)

`/api/analyze/*` (GPU/CPU worker queue), `/api/segments/*`, `/api/chapters/*`,
`/api/items/{id}/packaging-complete` — these are the event-driven pipeline's HTTP control
plane. Key contracts the rewrite must preserve:

| Method + path | Purpose / contract |
|---|---|
| `POST /api/analyze/claim` | `?pass=per_file|tidb_first|transcoder|packager` (def per_file), `?limit` (1..32, def 4). Atomic dequeue (`FOR UPDATE SKIP LOCKED`, order `createdat DESC`), flips claimed step→`in_progress`. Returns `{pass, claimed, items:[{id,type,title,year,durationMs,path,seasonNumber,episodeNumber,seriesTitle?,seriesTmdbId,movieTmdbId}]}` |
| `GET /api/analyze/items/{itemId}` | item + primary `path`; 404 if no primary asset |
| `GET /api/analyze/items/{itemId}/steps` | `{itemId, steps:{step:status}}` |
| `GET /api/analyze/items/{itemId}/siblings` | `?limit`(1..12/5) same-season episodes w/ paths (chromaprint) |
| `POST /api/analyze/items/{itemId}/steps/skip` | `{steps:[...], reason?}` → mark `not_applicable` |
| `POST /api/analyze/items/{itemId}/fail` | `{reason?}` → `scan` step failed + remaining analyzer steps skipped |
| `PUT /api/analyze/items/{itemId}/steps/{step}` | `{status, error?, details?}` upsert step. **Chain promotion**: `transcode`→done/n_a/skipped auto-seeds `package=pending` |
| `POST /api/analyze/series/{seriesId}/reset` | re-queue all episodes (bump createdat, reset steps, purge chromaprint segments) |
| `PUT /api/segments/items/{itemId}` | replace MediaSegments. `{segments:[{kind,source,startMs,endMs,confidence?,label?}]}`. kind∈{intro,recap,credits,preview}; source∈{tidb,chapter,subtitle,silence,blackframe,chromaprint,whisper,transnet,manual}; need `0<=start<end` |
| `DELETE /api/segments/items/{itemId}` | clear segments |
| `PUT /api/chapters/items/{itemId}` | replace chapters `{chapters:[{startMs,endMs,title?,ordinal?}]}` |
| `DELETE /api/chapters/items/{itemId}` | clear chapters |
| `POST /api/items/{itemId}/packaging-complete` | packager sink; body = on-disk `manifest.json`; updates primary asset codec/res/bitrate/size, replaces `kind='packaged'` PlaybackAsset row + SubtitleAssets |

**Processing-step vocabulary** (`ProcessingStepService`): steps =
`{scan, tmdb, tidb, chapter, chromaprint, blackframe, silence, subtitle, transcode, package}`;
status = `{pending, in_progress, done, failed, skipped, not_applicable}`. Unique
`(item_id, step)`; sticky `startedAt`/`finishedAt`; `attempts` auto-increments.
Step chain: `transcode → package` (package becomes pending only after transcode terminal).

`/svc/v1` (`KatalogManagerRestController`) — **empty stub**, no endpoints (future
service-to-service write surface; would be `cloud_katalog_admin`-gated and emit
`stube.library.item.*`).

---

## 9. Scheduled jobs

`@EnableScheduling` on the app. `TrailerIngestionService` runs a `@Scheduled` poller (cadence
`odownloader.poll-interval-seconds`, def 15s) that pulls finished oDownloader downloads into
`packages/_inbox/{itemId}/` and writes PlaybackAsset rows. Disabled when
`ODOWNLOADER_API_TOKEN` is unset (no-op). The Go rewrite needs an equivalent background loop
(or may externalize it) gated on the same token presence.

---

## 10. Package-path layout (used by validate + packaging-complete)

`PACKAGES_ROOT = /var/lib/katalog/packages` (hard-coded). Per-item:
`{root}/{category}/{shard}/{itemId}/` where category = `movies`(movie) / `shows`(episode) /
`music`(track) / `items`(other), shard = first 2 chars of itemId. `.complete` marker file +
`hls/master.m3u8` (BANDWIDTH parsed for packaged bitrate) + `manifest.json`. Packaged codec
invariant: must be `hev1.*`/`hvc1.*` (validate flags `codec_mismatch` otherwise).
