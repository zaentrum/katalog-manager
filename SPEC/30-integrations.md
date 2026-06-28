# 30 — External Integrations (TMDB, NFS scanner, download-gateway, oDownloader)

Authoritative behavioural spec for the Go+GraphQL rewrite of the SAP CAP
katalog-manager-api. Derived from the live source (read-only). All DB
identifiers are **lowercase**; base tables are `com_nalet_katalog_*`,
`katalogservice_*` are read-only VIEWS. Schema cross-checked against
`SPEC/live_schema.txt`, `SPEC/live_indexes.sql`, and the in-repo
`db/migrations/*.sql`.

> **Schema-dump gaps you must honour (the dump is incomplete):**
> - `com_nalet_katalog_trailerjobs` is **referenced by `TrailerIngestionService`
>   but is NOT in `live_schema.txt`**. Its DDL lives only in
>   `db/migrations/020_trailerjobs.sql`. The rewrite must (re)create it — see §4.
> - `com_nalet_katalog_downloadjobs` in the dump shows **only a PK on `id`**, but
>   `DownloadEventConsumer` does `ON CONFLICT (adapter, clientjobid)`. The
>   required `UNIQUE (adapter, clientjobid)` index exists only in
>   `db/migrations/025_download_jobs.sql` (`idx_downloadjobs_client`). The
>   rewrite **must** create this unique index or the upsert breaks.

Five integrations:

| # | Integration | Direction | Trigger | Transport |
|---|---|---|---|---|
| 1 | **TMDB v3** | outbound | on-demand (HTTP) / background sweep | REST + CDN |
| 2 | **NFS scanner** | inbound (filesystem) | on-demand (HTTP `POST /api/scan`), async worker | filesystem walk |
| 3 | **download-gateway (command)** | outbound | HTTP `POST/DELETE /api/downloads/*` | REST |
| 3 | **download-gateway (read model)** | inbound | Kafka consume `stube.download.client.*` | Kafka (mTLS) |
| 4 | **oDownloader** | outbound + scheduled | HTTP enqueue + `@Scheduled` poller | REST |
| 5 | **chaptersdb.com** | outbound (sidecar of TMDB) | inside TMDB enrich, opt-in | REST (out of scope; noted) |

The processing-step audit table `com_nalet_katalog_itemprocessingsteps` is the
shared spine — TMDB enrichment and the scanner both write step rows through it.
See §6.

---

## 1. TMDB Enrichment (`tmdb/EnrichmentService`, `tmdb/TmdbClient`)

### 1.1 Purpose
Per-item metadata enrichment for **movies** and **series** (+ child episodes).
Fills `items` text/rating/runtime, genres, people (cast+directors), poster +
backdrop bytes, trailer links, and (opt-in) chaptersdb chapter markers.

### 1.2 Trigger
On-demand HTTP via `EnrichmentController` (`@RequestMapping("/api/enrich")`),
auth = JWT resource-server (audience `katalog`):

| Method + path | Behaviour |
|---|---|
| `GET  /api/enrich/status` | `{ "tmdbEnabled": <bool> }` |
| `POST /api/enrich/items/{id}` | **synchronous** `enrichOne(id)`; returns `Result.toMap()` |
| `POST /api/enrich/pending?limit=50&type=movie\|series` | **background** sweep (single-thread executor `katalog-enrich`); 202 `{queued, type}` |
| `POST /api/enrich/backfill-episode-backdrops` | one-shot SQL: clone episode `kind='poster'` artwork rows → `kind='backdrop'` (both `itemartworkdata` and `itemartwork`); idempotent |
| `POST /api/enrich/retry-not-found?type=` | reset `itemprocessingsteps` rows `step='tmdb' AND status='skipped'` back to `pending` |

`enrichPending(limit, type)` queue selection (the rewrite must reproduce):
```sql
SELECT i.id, i.type, i.title, i.year FROM com_nalet_katalog_items i
WHERE <type IN ('movie','series') | type = ?>
  AND ( EXISTS (SELECT 1 FROM com_nalet_katalog_itemprocessingsteps s
                 WHERE s.item_id = i.id AND s.step='tmdb' AND s.status='pending')
        OR NOT EXISTS (SELECT 1 FROM com_nalet_katalog_itemprocessingsteps s
                        WHERE s.item_id = i.id AND s.step='tmdb') )
ORDER BY i.createdat ASC LIMIT ?
```
`limit`: clamp to `50` if `<=0 || >1000`. `type` null/blank ⇒ both movie+series.

### 1.3 Credentials / config (env-driven)
| Property | Env (Spring relaxed binding) | Default | Meaning |
|---|---|---|---|
| `tmdb.api-key` | `TMDB_API_KEY` | `""` | **v4 bearer token**, sent as `Authorization: Bearer <key>`. Blank ⇒ client disabled, every call → empty, enrich is a no-op. |
| `tmdb.language` | `TMDB_LANGUAGE` | `en-US` | `language=` query param on every call. |
| `chaptersdb.enabled` | `CHAPTERSDB_ENABLED` | false | opt-in chaptersdb sidecar (see §1.9). |

Bases: API `https://api.themoviedb.org/3`, images `https://image.tmdb.org/t/p`.
HTTP client timeouts: connect 10s, JSON read 15s, image fetch 30s. Follows
redirects. Only HTTP 200 is treated as success (else logged + empty).

### 1.4 TMDB endpoints used
| Method | URL (all + `?language=<lang>`) | Returns / parsed fields |
|---|---|---|
| `searchMovie` | `/search/movie?query=<t>&year=<y>` | first `results[].id` (>0) |
| `getMovie` | `/movie/{id}` | `id,title,original_title,tagline,overview,release_date,runtime,vote_average,poster_path,backdrop_path,imdb_id,genres[].name` |
| `getCredits` | `/movie/{id}/credits` | `cast[].name` (first 12), `crew[].name where job=="Director"` |
| `getMovieVideos` | `/movie/{id}/videos` | `results[]` filtered type∈{Trailer,Teaser}, site∈{YouTube,Vimeo}: `key,site,name,published_at` |
| `searchTv` | `/search/tv?query=<t>&first_air_date_year=<y>` | first `results[].id` |
| `getTv` | `/tv/{id}` | `id,name,original_name,tagline,overview,first_air_date,episode_run_time[0],vote_average,poster_path,backdrop_path,genres[].name` |
| `getTvCredits` | `/tv/{id}/credits` | same shape as movie credits |
| `getTvVideos` | `/tv/{id}/videos` | same filter as movie videos |
| `getTvEpisode` | `/tv/{id}/season/{s}/episode/{e}` | `id,name,overview,air_date,runtime,vote_average,still_path,season_number,episode_number` |
| image fetch | `https://image.tmdb.org/t/p/{size}{path}` | raw bytes (200 only) |

**Search fallback:** `searchMovie`/`searchTv` first query *with* the year; if 0
results **and** year was non-null, retry the same query *without* the year
filter. First hit (results[0].id) wins. Empty title ⇒ skip.

**Image sizes:** poster `w780`, backdrop `w1280`, episode still `w500`. Format
inferred from URL suffix: `.png` ⇒ `image/png`, else `image/jpeg`.

### 1.5 Per-movie flow (`enrichMovie`)
1. `markStatus(id,"in_progress")` → step `tmdb` = `in_progress`.
2. Resolve TMDB id: existing `itemexternalids(source='tmdb')` **or** `searchMovie(cleanTitle(title), year)`. None ⇒ `markStatus("not_found")` (→ step `skipped`), return NOT_FOUND.
3. `upsertExternalId(id,"tmdb", tmdbId)`.
4. `getMovie` → `applyMovie` (§1.7). Null detail ⇒ step `failed`.
5. `getCredits` → `applyCredits` (people).
6. `getMovieVideos` → `applyTrailerLinks` (§1.8).
7. `applyChaptersDb` (§1.9, opt-in).
8. Poster (`w780` from `poster_path`) + backdrop (`w1280` from `backdrop_path`) → `persistArtwork` (§1.10).
9. `markStatus("done")`.
Any `RuntimeException` ⇒ step `failed` + message.

### 1.6 Per-series flow (`enrichSeries`) + episodes
Same skeleton with TV endpoints; `applyTv` instead of `applyMovie`; **no
chaptersdb** for series. After `applyTv`+credits+videos+artwork it calls
`enrichEpisodesOf(seriesId, tmdbTvId)`:
- Selects child items: `WHERE parent_id=? AND type='episode' AND seasonnumber IS NOT NULL AND episodenumber IS NOT NULL`.
- For each, `getTvEpisode(tvId, sNum, eNum)`; skip if null/blank name.
- `applyEpisode` (§1.7) — best-effort; missing episode leaves filename title intact.

### 1.7 Field → column mapping (`items` table updates)
All updates use `COALESCE(?, col)` so a TMDB null never blanks an existing
value (EXCEPT `tagline`, which is set unconditionally on movie/tv). `sorttitle`
= `lower(canonicalTitle)`. `modifiedat = now()`.

**Movie (`applyMovie`):**
| TMDB field | → `com_nalet_katalog_items` column | transform |
|---|---|---|
| `title` (non-blank) | `title` | canonicalTitle |
| ″ | `sorttitle` | `lower(title)` |
| `overview` | `description` | — |
| `vote_average` (>0) | `rating` (numeric) | else null |
| `runtime` (>0, minutes) | `durationms` (bigint) | `runtime * 60000` |
| `release_date` | `year` (int) | `LocalDate.parse(...).getYear()` |
| `tagline` | `tagline` | **set unconditionally** |
| `genres[].name` | → `genres`+`itemgenres` | §1.7a |
| `imdb_id` (non-blank) | → `itemexternalids(source='imdb')` | §1.7c |

**TV (`applyTv`):** identical, with `name`→title, `overview`→description,
`first_air_date`→year, `episode_run_time[0]`→durationms, `tagline` set
unconditionally, `genres` upserted. **No imdb_id** for TV.

**Episode (`applyEpisode`):** `name`→title(+sorttitle), `overview`→description,
`vote_average>0`→rating, `runtime>0`→durationms (`*60000`), `air_date`→year.
Then `steps.upsert(itemId,"tmdb","done")`; if `episode.id>0` →
`itemexternalids(source='tmdb-episode')`; if `still_path` present, fetch `w500`
still and `persistArtwork` under **both** `poster` and `backdrop` kinds.

**(1.7a) Genres** — `upsertGenres(itemId, names)`, dedup within call:
- find `com_nalet_katalog_genres WHERE name=?`; if missing INSERT `(id=uuid, name)`.
- link: if no `itemgenres WHERE item_id=? AND genre_id=?`, INSERT `(id=uuid, item_id, genre_id)`.
- `genres.name` is `varchar(80)`.

**(1.7b) People** — `applyCredits` then `upsertPerson(itemId, name, role)`:
- `crew` (directors only) → role `director`; `cast` (≤12) → role `actor`.
- find `people WHERE name=?`; if missing INSERT `(id=uuid, name)`.
- link: if no `itempeople WHERE item_id=? AND person_id=? AND role=?`, INSERT `(id=uuid, item_id, person_id, role)`.
- `people.name` `varchar(255)`, `itempeople.role` `varchar(40)`.

**(1.7c) External ids** — `upsertExternalId(itemId, source, externalId)`:
- if a row `(item_id, source)` exists ⇒ UPDATE `externalid`; else INSERT `(id=uuid,...)`.
- sources used: `tmdb` (movie/tv id), `tmdb-episode` (episode id), `imdb` (imdb_id).
- `itemexternalids.source` `varchar(30)`, `externalid` `varchar(120)`.

### 1.8 Trailer links (`applyTrailerLinks` ← TMDB videos)
Idempotent replace of TMDB-sourced, not-yet-downloaded rows:
```sql
DELETE FROM com_nalet_katalog_itemtrailerlinks
 WHERE item_id=? AND source='tmdb' AND downloadedat IS NULL;
INSERT INTO com_nalet_katalog_itemtrailerlinks
 (id, createdat, modifiedat, item_id, source, site, externalid, url, title, publishedat)
 VALUES (uuid, now, now, ?, 'tmdb', <site>, <key>, <url>, <name>, <published_at>);
```
- `site` = `"YouTube"|"Vimeo"`; `externalid` = TMDB video `key`.
- `url` = `https://www.youtube.com/watch?v=<key>` or `https://vimeo.com/<key>`.
- `publishedat` = parse `published_at` ISO-8601, null on parse error.
- manually-added rows (source≠tmdb) and already-downloaded rows survive.

### 1.9 chaptersdb sidecar (`applyChaptersDb`, movies only, opt-in)
Only when `CHAPTERSDB_ENABLED=true`. `chaptersDb.findShow(title, year, "movie")`
then `getMovieChapters(showId)`. Replaces prior `source='chaptersdb'` rows:
```sql
DELETE FROM com_nalet_katalog_mediasegments WHERE item_id=? AND source='chaptersdb';
DELETE FROM com_nalet_katalog_itemchapters WHERE item_id=?;
```
Per entry, end = next entry start (or runtime, or start+1000ms). Name
classification (`classifyChapterName`, case-insensitive):
- credits → `\b(end credits|closing credits|credits)\b`
- intro → `\b(opening credits|opening titles|main titles|intro(duction)?|title sequence)\b`
- recap → `\b(recap|previously on)\b`

Labelled (kind≠null) → `mediasegments(id,createdat,modifiedat,item_id,kind,startms,endms,source='chaptersdb',confidence=0.95,label)`; unlabelled → `itemchapters(id,createdat,modifiedat,item_id,startms,endms,title,ordinal=i+1)`. `label` truncated to 120 chars. (chaptersdb HTTP client is `chaptersdb/ChaptersDbClient` — out of scope here.)

### 1.10 Artwork byte-fetch flow (`persistArtwork(itemId, kind, url)`)
1. URL row: if no `itemartwork WHERE item_id=? AND kind=? AND url=?`, INSERT `(id=uuid, item_id, kind, url)`. (`url` varchar(2048))
2. `tmdb.fetchImage(url)` → bytes (HTTP 200 only). Null/empty ⇒ stop.
3. `contenttype` = `.png`→`image/png` else `image/jpeg`.
4. Byte row keyed `(item_id, kind)`: if `itemartworkdata WHERE item_id=? AND kind=?` exists ⇒ UPDATE `contenttype,bytes,fetchedat=now()`; else INSERT `(id=uuid, item_id, kind, contenttype, bytes, fetchedat)`. `bytes` is `bytea`.
- kinds: `poster`, `backdrop`. Episodes write the single still to both.

### 1.11 Title cleanup (`EnrichmentService.cleanTitle`, **public static, shared by NfsScanner**)
Strips Sonarr/Radarr release-group / quality / codec tokens before TMDB
search. Case-insensitive regex list (order: composite before single):
`Remux-Np`, `WEB-DL/WEBRip-Np`, `Bluray-Np`, `HDTV/BDRip-Np`, `DVDRip/DVDScr/BRRip`,
`\d{3,4}p`, `\d{3,4}i`, `(2160|1080|720|480)p`, `WEB-DL`, `WEB`, `Bluray`,
`SDTV`, `DVD`, `TELESYNC`, `Proper`, `Repack`, `Remastered`, `Internal`,
`Limited`, `HDR(10+)?`, `DV`, `Dolby Vision`, `(h|x)264/265`, `HEVC`, `AVC`,
`DTS(-HD)?`, `DDP?5.1`, `AAC`, `TrueHD`, `Atmos`, `IMAX`, `4K`, `UHD`,
`Extended`, `Director's Cut`, `Unrated`, `MultiSubs?`, `Multi`, `Dual-Audio`,
`(Eng|Ger|Fre|Spa|Ita|Jpn|Chi)(Sub|Audio)?`. Then strips brackets, trailing
`[-_.]`, collapses whitespace. Returns original if result is empty.
Year also re-extracted from title in `enrichRow` via `\((19|20)\d{2}\)` when
`year` column is null.

### 1.12 Result / BulkResult shapes
- `Result`: `{itemId, status: done|not_found|failed|skipped, message?}`.
- `BulkResult`: `{itemsConsidered, itemsEnriched, itemsFailed}`.
- (View `katalogservice_items` exposes computed `posterurl`, `backdropurl`,
  `runtimemin`, `yeartext` — derived from the above writes; read-only.)

---

## 2. NFS Scanner (`scanner/NfsScanner`) + ScanJobs lifecycle

### 2.1 Purpose
Filesystem walker that classifies media files under one mount root by
extension + path, then upserts `items` + primary `playbackassets` (+ subtitle
sidecars + trailers). **No remote lookup** — enrichment is separate.

### 2.2 Trigger + ScanJobs lifecycle (`ScanController`, `/api/scan`)
The scanner itself has **no job tracking** — `ScanController` owns it.
| Method + path | Behaviour |
|---|---|
| `POST /api/scan?source=nfs` | only `nfs` accepted (else 400). INSERT scanjobs row `status='running'`, submit to single-thread executor `katalog-scan`, return **202** `{ID, source, status:"running", startedAt}` |
| `GET /api/scan/{id}` | one scanjobs row or 404 |
| `GET /api/scan?limit=50` | scanjobs ordered `startedat DESC` (limit clamp 50 if `<=0||>200`) |

ScanJobs lifecycle (`com_nalet_katalog_scanjobs`):
```sql
-- on POST:
INSERT (id, source, status='running', startedat=now, filesseen=0, itemsinserted=0, itemsupdated=0)
-- on success:
UPDATE status='done', finishedat=now, filesseen=?, itemsinserted=?, itemsupdated=?
-- on failure:
UPDATE status='failed', finishedat=now, errormessage=?
```
Columns: `id, source(varchar20), status(varchar20), startedat, finishedat,
errormessage(text), filesseen, itemsinserted, itemsupdated`.

### 2.3 Config
| Property | Env | Default |
|---|---|---|
| `scanner.nfs.root` | `SCANNER_NFS_ROOT` | `/var/lib/katalog/media` |

If root doesn't exist ⇒ warn + empty result (no error). Walk is
`Files.walkFileTree`; per-file exceptions are caught + skipped, not fatal.

### 2.4 File classification rules
Extensions (lowercased):
- video: `.mkv .mp4 .avi .mov .m4v .webm`
- audio: `.flac .mp3 .ogg .m4a .opus .wav`
- subtitle (sidecar): `.srt .vtt .ass .ssa`

Skip rules: filename starting with `.` (dotfiles / resource forks /
transcoder scratch) is skipped entirely; files with no extension skipped;
non-video/non-audio skipped. `filesseen++` only for video/audio.

**Type classification (`classify(rel, isVideo, isAudio)`):**
- audio ⇒ `track`
- video AND (`rel` lowercased contains `/series/` or `/tv/`, OR `SxxEyy` matches anywhere) ⇒ `episode`
- else ⇒ `movie`

**Episode coordinates** — `EPISODE_PATTERN = (?i)\bS(\d{1,2})E(\d{1,3})\b`.
For `type='episode'`, capture `seasonnumber`=group1, `episodenumber`=group2.

**Year extraction (`extractYear`)** — prefer `\(((19|20)\d{2})\)` (paren form);
else first bare `\b(19|20)\d{2}\b`.

**Title extraction (`extractTitle(filename, type)`):**
1. strip extension; replace `[._]+` with space (`CLEANUP_PATTERN`).
2. if paren-year present, remove `\((19|20)\d{2}\)`; else strip trailing bare year `\s+(19|20)\d{2}(?=\s|$)` (numeric titles like `1917`/`2012`/`2067` survive when no paren year).
3. remove brackets `()[]`; for episodes remove `SxxEyy` token.
4. collapse whitespace; finally pass through `EnrichmentService.cleanTitle`.

### 2.5 Trailer recognition (`isTrailerPath`) — video files only
A video file is a trailer if (base = filename minus ext, lowercased):
- base == `trailer`, OR
- base endsWith `-trailer` / `.trailer` / `_trailer` / ` trailer`, OR
- absolute path contains `/trailers/`.
Trailers are **attached to the parent movie** as `kind='trailer'`
playbackasset, NOT inserted as a new item (`attachTrailer`).

### 2.6 Item + asset upsert (non-trailer)
Key = `playbackassets.path` (absolute path string).
```sql
existing := SELECT item_id FROM com_nalet_katalog_playbackassets WHERE path=? LIMIT 1
```
- **new** (existing null): INSERT items `(id=uuid, type, title, sorttitle=lower(title),
  year, seasonnumber, episodenumber, createdat=now, modifiedat=now)`; `itemsInserted++`.
- **existing**: UPDATE items SET `modifiedat=now` ONLY (title/sort/year are
  owned by TMDB enrichment — scanner must NOT clobber them on re-scan);
  `itemsUpdated++`.
- Asset: existing ⇒ UPDATE `playbackassets SET sizebytes=?, isprimary=true WHERE path=?`;
  new ⇒ INSERT `(id=uuid, item_id, path, sizebytes, isprimary=true)`.
- (No `MERGE`/`ON CONFLICT` — H2-portability; explicit select-then-branch.)

### 2.7 Subtitle sidecars (`scanSidecars`, video files only)
Scans the video's directory for sub files sharing the video basename
(optional language suffix `LANG_SUFFIX = \.([a-zA-Z]{2,3}([-_][a-zA-Z]{2,4})?)$`).
First matching sidecar marked default, rest alternates.
- key = `subtitleassets.path`. existing ⇒ UPDATE `item_id, format, lang, label`;
  else INSERT `(id=uuid, item_id, path, format=ext-without-dot, lang, label, isdefault=!defaultPicked)`.
- `lang` = lowercased matched suffix; `label` from `languageLabel(lang)` map
  (en→English, de/deu/ger→Deutsch, … fallback `primary.toUpperCase()`); default
  label `"Subtitles"` when no lang suffix matched.
- `DataAccessException` (table missing) ⇒ log debug, keep walking.

### 2.8 Trailer attach (`attachTrailer`)
- parent dir = file's parent; if that dir is named `trailers` (case-insens.) go up one more.
- parent movie id:
  `SELECT item_id FROM playbackassets WHERE isprimary=true AND path LIKE '<movieDir>/%' LIMIT 1`.
  Null ⇒ skip (parent not ingested yet; next sweep).
- asset by path: new ⇒ INSERT `playbackassets (id=uuid, item_id=parent, path, sizebytes, isprimary=false, kind='trailer')` (`itemsInserted++`);
  existing ⇒ UPDATE `item_id=parent, sizebytes, kind='trailer', isprimary=false` (`itemsUpdated++`), and if the trailer previously had its **own** orphan item with no remaining assets, DELETE that orphan item.
- finally bump parent `items.modifiedat=now`.
- `playbackassets.kind` values: `primary`(default) | `trailer` | `sample` | `featurette` | `behindthescenes`.

---

## 3. download-gateway (CQRS: REST command + Kafka read model)

ADR-019/020. **Command** side = `DownloadGatewayClient` (REST out). **Read**
side = `DownloadEventConsumer` (Kafka in → `downloadjobs` projection). Commands
never write the table; reads never call the gateway.

### 3.1 Command side — `DownloadGatewayClient` + `DownloadConsoleController`
HTTP client over the gateway control plane (in-cluster Service, unauthenticated
inside namespace). Connect timeout 5s; per-call timeouts 15s (add/remove), 5s (clients).

| Gateway endpoint | Client method | Request | Response |
|---|---|---|---|
| `POST /api/v1/downloads` | `add(adapter, source, title, wantedItemId)` | JSON `{adapter, source, title, wanted_item_id}` (null→`""`) | parsed Map; controller echoes `client_job_id` |
| `DELETE /api/v1/downloads/{adapter}/{id}` | `remove(adapter, clientJobId)` | path segs URL-encoded (`+`→`%20`) | — |
| `GET /api/v1/clients` | `clients()` | — | raw JSON body, or `"[]"` |

Non-2xx / transport error ⇒ `GatewayException` (add/remove). `clients()` swallows
errors → `"[]"`.

Controller `DownloadConsoleController` (`/api/downloads`, JWT auth):
| Method + path | Notes |
|---|---|
| `POST /api/downloads` | body `{adapter, source, title?, wantedItemId?}`; `adapter`+`source` required else 400; 202 `{ok, adapter, clientJobId, message}`; GatewayException → 502 |
| `DELETE /api/downloads/{adapter}/{clientJobId}` | 200 `{ok, message}` / 502 |
| `GET /api/downloads/clients` | passthrough of gateway `clients()` JSON |

`adapter` ∈ `odownloader | qbittorrent | nzbget`; `source` = URL / magnet / NZB.

Config (`DownloadGatewayProperties`, prefix `download-gateway`):
| Property | Env | Default | Meaning |
|---|---|---|---|
| `download-gateway.url` | `DOWNLOAD_GATEWAY_URL` | `""` | base URL, e.g. `http://download-gateway.stube.svc.cluster.local:8080`. **Blank ⇒ command side disabled** (`isEnabled()=false`); add/remove throw "disabled", `clients()`→`"[]"`. |
| `download-gateway.events-enabled` | `DOWNLOAD_GATEWAY_EVENTS_ENABLED` | false | gate for the Kafka consumer + its config beans. |

### 3.2 Read side — `DownloadEventConsumer` (Kafka → `downloadjobs`)
Bean exists only when `download-gateway.events-enabled=true`
(`@ConditionalOnProperty`). Projects gateway events into
`com_nalet_katalog_downloadjobs` (read by `katalogservice_downloadjobs` view +
Fiori "Downloads" tile).

**Consumes (topic pattern):** `stube\.download\.client\..*`
i.e. `stube.download.client.{started,progress,completed,failed}`.
**PRODUCES to NO topics** — pure read-model projection.

- group id: `${download-gateway.kafka.group-id:stube-katalog-manager}`.
- container factory: `downloadKafkaListenerContainerFactory`.
- payload: JSON string (gateway v1 wire format, **snake_case**). `kind` =
  substring after last `.` of the topic. Poison messages logged + skipped.

**Kafka wiring (`DownloadKafkaConfig`, only when events-enabled):**
| Env property | Default | Meaning |
|---|---|---|
| `download-gateway.kafka.brokers` | `""` | bootstrap servers |
| `download-gateway.kafka.group-id` | `stube-katalog-manager` | consumer group |
| `download-gateway.kafka.tls-cert` | `/etc/kafka-cert/user.crt` | mTLS client cert (PEM) |
| `download-gateway.kafka.tls-key` | `/etc/kafka-cert/user.key` | mTLS client key (PEM) |
| `download-gateway.kafka.tls-ca` | `/etc/kafka-cert/ca.crt` | CA (PEM) |

Consumer: `StringDeserializer` key+value, `auto.offset.reset=earliest`. mTLS via
Kafka **native PEM** (`security.protocol=SSL`, `ssl.keystore.type=PEM` with
inline PEM strings — no JKS, no Spring SSL bundle). **If any cert file is
missing/unreadable, the container factory sets `autoStartup=false`** — listener
never starts, app stays healthy (deliberate: avoid CrashLoopBackOff).

### 3.3 Event payloads → `downloadjobs` upsert
**Deterministic id:** `derivedId(adapter, clientId) = UUID.nameUUIDFromBytes((adapter + ":" + clientId) UTF-8)` (UUIDv3, name-based) — i.e. `id = uuidV3(adapter + ":" + client_id)`. Stable so all four event kinds upsert the same row. Conflict target = `(adapter, clientjobid)` (unique index `idx_downloadjobs_client`).

Common fields read: `adapter`, `client_id` (→ `clientjobid`). If either is
null/empty the event is dropped.

| Event (`kind`) | Source fields → columns | state / fixed cols | timestamp |
|---|---|---|---|
| `started` | `title→title`, `wanted_item_id→wanteditemid`, `size_bytes→sizebytes` | `state='queued'`, progressPct passed null→0 | `started_at`(ms)→`startedat`+`lasteventat` |
| `progress` | `state` (default `'downloading'`), `progress_pct→progresspct`, `downloaded_bytes→downloadedbytes`, `size_bytes→sizebytes` (coalesce), `speed_bps→speedbps`, `eta_sec→etasec` | state NOT overwritten if existing already `completed`/`failed` | `emitted_at`(ms)→`lasteventat` |
| `completed` | `wanted_item_id→wanteditemid` (coalesce), `size_bytes→sizebytes` (coalesce), `files`(raw JSON array text, default `[]`)→`files` | `state='completed', progresspct=100` | `completed_at`(ms)→`completedat`+`lasteventat` |
| `failed` | `error→errormessage` | `state='failed'` | `failed_at`(ms)→`lasteventat` |

Timestamps are **epoch-millis** numeric fields; absent/≤0 ⇒ `now()`.
The `progress` upsert is the key SQL pattern the rewrite must reproduce:
```sql
INSERT INTO com_nalet_katalog_DownloadJobs
 (ID, createdAt, adapter, clientJobId, state, progressPct, downloadedBytes,
  sizeBytes, speedBps, etaSec, lastEventAt) VALUES (...)
ON CONFLICT (adapter, clientJobId) DO UPDATE SET
  state = CASE WHEN <existing>.state IN ('completed','failed')
               THEN <existing>.state ELSE EXCLUDED.state END,
  progressPct=EXCLUDED.progressPct, downloadedBytes=EXCLUDED.downloadedBytes,
  sizeBytes=COALESCE(EXCLUDED.sizeBytes,<existing>.sizeBytes),
  speedBps=EXCLUDED.speedBps, etaSec=EXCLUDED.etaSec,
  lastEventAt=EXCLUDED.lastEventAt;
```
`completed`/`failed` force their state unconditionally; `progress`/`started`
defer to a terminal state already present.

### 3.4 `downloadjobs` table + view
Table `com_nalet_katalog_downloadjobs` (DDL `025_download_jobs.sql`):
`id varchar36 PK, createdat, createdby, modifiedat, modifiedby, adapter
varchar40 NOT NULL, clientjobid varchar255 NOT NULL, title varchar500,
wanteditemid varchar80, state varchar20 NOT NULL default 'queued', progresspct
decimal(5,2) default 0, downloadedbytes bigint default 0, sizebytes bigint,
speedbps bigint, etasec int, files text, errormessage text, startedat,
completedat, lasteventat`.
**Indexes the rewrite MUST recreate:** `UNIQUE (adapter, clientjobid)`
(`idx_downloadjobs_client`) — required by the upsert; `(lasteventat DESC)`.
View `katalogservice_downloadjobs` adds `statecriticality` (failed→1,
downloading/queued→2, completed→3, else 0).

---

## 4. oDownloader trailer ingestion (`odownloader/*`)

### 4.1 Purpose
Fetch trailer files via the in-cluster oDownloader daemon and bridge the bytes
into the catalog: enqueue URLs → poll → import finished `.mp4/.mkv/.mov` into the
packages inbox → create `kind='trailer'` playbackasset + stamp
`itemtrailerlinks.localpath/downloadedat`.

### 4.2 Trigger
Two paths:
1. **Enqueue (sync)** — `TrailerActionsController` `POST /api/items/{itemId}/fetch-trailers`:
   - load item title; pull `itemtrailerlinks WHERE item_id=? AND (localpath IS NULL OR localpath='')`; if none → 200 with `enqueued:0` guidance.
   - de-dup URLs (LinkedHashSet), build `trailerLinkIdByUrl`; call `ingestion.enqueue(itemId, title, urls, map)`.
   - 200 `{itemId, title, enqueued, packageId, jobIds, message}`. (`itemtrailerlinks` is populated earlier by TMDB "Refresh from TMDB".)
2. **Poll + ingest (scheduled)** — `TrailerIngestionService.pollAndIngest`,
   `@Scheduled(fixedDelay = ${odownloader.poll-interval-seconds:15}*1000, initialDelay=30000)`.

### 4.3 Config (`OdownloaderProperties`, prefix `odownloader`)
| Property | Env | Default | Meaning |
|---|---|---|---|
| `odownloader.url` | `ODOWNLOADER_URL` | `""` | base URL, e.g. `http://odownloader.cloud-nalet-odownloader.svc.cluster.local:8686` (in-cluster, bypasses oauth2-proxy). |
| `odownloader.token` | `ODOWNLOADER_TOKEN` | `""` | static bearer token (daemon `odownloader-api` secret). |
| `odownloader.poll-interval-seconds` | `ODOWNLOADER_POLL_INTERVAL_SECONDS` | 15 | poller cadence. |
| `odownloader.timeout-minutes` | `ODOWNLOADER_TIMEOUT_MINUTES` | 60 | per-job give-up window. |
| `odownloader.inbox-root` | `ODOWNLOADER_INBOX_ROOT` | `/var/lib/katalog/packages/_inbox` | import staging dir. |
**Disabled** when `url` OR `token` blank ⇒ every client method short-circuits and the poller is a no-op. Auth header `Authorization: Bearer <token>` on every call.

### 4.4 oDownloader endpoints (`OdownloaderClient`)
Connect timeout 5s; GET 15s, POST 30s.
| Method | Endpoint | Body / params | Returns |
|---|---|---|---|
| `addLinks(urls, packageName, comment)` | `POST /api/v1/links/add` | JSON `{urls:[...], packageName, comment, autostart:true}` | `AddLinksResult{packageId, downloadIds[]}` (null on ≥400) |
| `listDownloadsByPackage(packageId)` | `GET /api/v1/downloads?packageId=<enc>&size=200` | — | reads `items[]` → `List<DownloadStatus>` |
| `getDownload(downloadId)` | `GET /api/v1/downloads/{enc}` | — | `DownloadStatus` |
| `openContent(downloadId)` | `GET /api/v1/downloads/{enc}/content` | — | `InputStream` (caller closes) |

`DownloadStatus` = `{id, packageId, name, bytesDone(long), bytesTotal(long),
speedBytesPerSecond(long), state(String), message}`. One source URL fans into
multiple variants (YouTube via JD plugin → `.srt .txt .jpg .m4a .mp4`).

### 4.5 `enqueue(itemId, packageName, urls, trailerLinkIdByUrl)` (`@Transactional`)
- disabled ⇒ EnqueueResult `(empty, null, "…disabled…")`; empty urls ⇒ `(empty,null,"No URLs supplied.")`.
- **one URL = one oDownloader package** (so the poller can map variants back to a job). Per URL: `addLinks([url], packageName, "katalog item="+itemId)`; on null skip; else INSERT one trailerjobs row:
```sql
INSERT INTO com_nalet_katalog_trailerjobs
 (id, createdat, modifiedat, item_id, trailer_link_id, source_url,
  package_id, download_id, state, attempts)
 VALUES (uuid, now, now, ?, <linkRef|null>, <url>, <result.packageId>, NULL, 'queued', 0);
```
- result `(jobIds, firstPackageId, "Queued N download(s)." | "…rejected every URL…")`.

### 4.6 `pollAndIngest` / `processOne`
- disabled ⇒ return. Select active jobs:
```sql
SELECT id,item_id,trailer_link_id,source_url,package_id,download_id,state,started_at,attempts
FROM com_nalet_katalog_trailerjobs
WHERE state IN ('queued','running','downloaded') ORDER BY createdat ASC LIMIT 50
```
- cutoff = `now - timeout-minutes`. Per row (`processOne`):
  - if `started_at < cutoff` ⇒ `markTerminal(id,'timeout',msg)` (set state+message+finished_at), continue.
  - `package_id` null ⇒ return.
  - `listDownloadsByPackage(package_id)`; empty ⇒ return.
  - `pickBestVideoVariant`: largest by `max(bytesTotal,bytesDone)` whose name ends `.mp4/.mkv/.mov` (excludes `.m4a/.srt/.webm-audio`/sidecars). None yet ⇒ UPDATE message "Waiting for video variant…", set `started_at=COALESCE(started_at,now())`, return.
  - else pin the variant: UPDATE `download_id=best.id, state=mapState(best.state), message, bytes_done, bytes_total, started_at=COALESCE(...), modifiedat=now`.
  - if `best.state` eq (case-insens) `FINISHED`:
    - filename = `sanitizeFilename(best.name, best.id)` (`/`,`\`,NUL→`_`; blank→`<id>.bin`; keeps unicode).
    - target = `<inbox-root>/{itemId}/{filename}`; create dirs; stream `openContent(best.id)` → file (null stream ⇒ IOException).
    - `writeTrailerAsset(itemId, target, size)`: DELETE existing `playbackassets WHERE item_id=? AND kind='trailer' AND path=?` then INSERT `(id=uuid, item_id, path, sizebytes=size, isprimary=false, kind='trailer')`.
    - if `trailer_link_id` set: UPDATE `itemtrailerlinks SET localpath=<target>, downloadedat=now(), modifiedat=now() WHERE id=?`.
    - `markImported`: trailerjobs `state='imported', final_path=target, bytes_done=size, finished_at=COALESCE(finished_at,now()), modifiedat=now`.
    - on IOException: delete target, `markTerminal(id,'failed',msg)`, rethrow.
  - any exception in `processOne` ⇒ `bumpAttempts(id, msg[:500])`.

**`mapState(odState)`:** RUNNING→`running`, FINISHED→`downloaded`,
FAILED/ERROR→`failed`, QUEUED/PENDING→`queued`, default→`queued` (null→`queued`).
Note column synonym: `downloaded` = FINISHED-but-not-yet-imported.

### 4.7 `trailerjobs` table (DDL only in `020_trailerjobs.sql` — NOT in live_schema.txt)
`com_nalet_katalog_trailerjobs`:
`id varchar36 PK, createdat NOT NULL default now, modifiedat NOT NULL default now,
item_id varchar36 NOT NULL, trailer_link_id varchar36, source_url varchar2048
NOT NULL, package_id varchar120, download_id varchar120, state varchar20 NOT
NULL default 'queued', message varchar500, bytes_done bigint, bytes_total
bigint, final_path varchar2048, attempts int NOT NULL default 0, started_at,
finished_at`.
States: `queued | running | downloaded | imported | failed | timeout`.
Indexes: partial `(state) WHERE state IN ('queued','running','downloaded')`
(`idx_trailerjobs_active`); `(item_id)` (`idx_trailerjobs_item`).

---

## 5. Loop closure (trailers, full lifecycle)
1. TMDB enrich writes `itemtrailerlinks(source='tmdb', url, …)`.
2. `POST /api/items/{id}/fetch-trailers` → `enqueue` → oDownloader package + `trailerjobs(state='queued')`.
3. `@Scheduled` poller imports the video variant → file in `_inbox/{itemId}/`, `playbackassets(kind='trailer')`, `itemtrailerlinks.localpath+downloadedat`, `trailerjobs(state='imported')`.
4. Next NFS scan sees the file under `_inbox` (or final tree) and (re)attaches via `isTrailerPath`/`attachTrailer` — idempotent.

---

## 6. Shared processing-step audit (`web/ProcessingStepService`)
`com_nalet_katalog_itemprocessingsteps`, **conflict key `(item_id, step)`**.
Both TMDB enrichment and the scanner-pipeline write here.
- `ALLOWED_STEPS`: `scan, tmdb, tidb, chapter, chromaprint, blackframe, silence, subtitle, transcode, package`.
- `ALLOWED_STATUS`: `pending, in_progress, done, failed, skipped, not_applicable`.
- `upsert(itemId, step, status, error, details)`:
  - validates step+status (else `IllegalArgumentException`); error truncated to 500.
  - INSERT `(id=gen_random_uuid()::varchar, createdat, item_id, step, status, startedat, finishedat, attempts=1, error, details)` where `startedat` set only when status=`in_progress`, `finishedat` set only when status∈{done,failed,skipped}.
  - `ON CONFLICT (item_id, step) DO UPDATE`: `status=EXCLUDED.status`, `modifiedat=now`, `attempts=existing.attempts+1`, `startedat` sticky (set once on first in_progress), `finishedat` set on terminal status, `error/details=EXCLUDED`.
  - **TMDB status mapping (`EnrichmentService.markStatus`):** `in_progress→in_progress`, `done→done`, `not_found→skipped`, `failed→failed`.
- `resetForItems(ids, steps)`: bulk back-to-pending (`WHERE item_id=ANY(?) AND step=ANY(?)`), keeps attempts.
- View `katalogservice_itemprocessingsteps` adds `statuscriticality`.

---

## 7. Env-var quick reference (all integrations)
| Env | Default | Used by |
|---|---|---|
| `TMDB_API_KEY` | (empty→disabled) | TMDB (v4 bearer) |
| `TMDB_LANGUAGE` | `en-US` | TMDB |
| `CHAPTERSDB_ENABLED` | false | chaptersdb sidecar |
| `SCANNER_NFS_ROOT` | `/var/lib/katalog/media` | NFS scanner |
| `DOWNLOAD_GATEWAY_URL` | (empty→cmd disabled) | gateway command |
| `DOWNLOAD_GATEWAY_EVENTS_ENABLED` | false | gateway Kafka consumer gate |
| `DOWNLOAD_GATEWAY_KAFKA_BROKERS` | (empty) | Kafka bootstrap |
| `DOWNLOAD_GATEWAY_KAFKA_GROUP_ID` | `stube-katalog-manager` | Kafka group |
| `DOWNLOAD_GATEWAY_KAFKA_TLS_CERT/KEY/CA` | `/etc/kafka-cert/{user.crt,user.key,ca.crt}` | Kafka mTLS PEM |
| `ODOWNLOADER_URL` | (empty→disabled) | oDownloader |
| `ODOWNLOADER_TOKEN` | (empty→disabled) | oDownloader bearer |
| `ODOWNLOADER_POLL_INTERVAL_SECONDS` | 15 | poller cadence |
| `ODOWNLOADER_TIMEOUT_MINUTES` | 60 | job timeout |
| `ODOWNLOADER_INBOX_ROOT` | `/var/lib/katalog/packages/_inbox` | import dir |
