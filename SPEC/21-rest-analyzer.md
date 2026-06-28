# 21 — REST: Analyzer / Packager / Segments / Chapters / Subtitles

Machine-to-machine REST contracts consumed by the **GPU analyzer**, **transcoder**,
**packager**, and **player clients**. These are integration contracts other services
depend on; the Go+GraphQL rewrite **must reproduce paths, JSON field names, casing,
and semantics EXACTLY**. (GraphQL can be added on top, but these REST routes stay.)

Source controllers (CAP/Java, READ-ONLY reference):
`web/AnalyzerController.java`, `web/SegmentsController.java`, `web/ChaptersController.java`,
`web/SubtitlesController.java`, `web/ProcessingStepService.java`, `chaptersdb/ChaptersDbClient.java`.

All identifiers in Postgres are **lowercase**. Base tables are `com_nalet_katalog_*`.
Item ids are `character varying(36)` (UUID-as-string). All `*Ms` are `bigint`. JSON `id`
fields are always emitted as **strings** (`String.valueOf(...)`), even though some DB
columns are numeric-ish — preserve string emission.

---

## Tables touched (authoritative columns)

### com_nalet_katalog_itemprocessingsteps  (audit / work-queue state)
| column | type | notes |
|---|---|---|
| id | varchar(36) | PK; `gen_random_uuid()::varchar` on insert |
| createdat | timestamp | set on insert |
| createdby | varchar(255) | (untouched by these endpoints) |
| modifiedat | timestamp | set on every update |
| modifiedby | varchar(255) | (untouched) |
| item_id | varchar(36) | |
| step | varchar(20) | one of ALLOWED_STEPS |
| status | varchar(20) | one of ALLOWED_STATUS |
| startedat | timestamp | sticky: set on FIRST transition into `in_progress` |
| finishedat | timestamp | sticky-ish: set when status in (`done`,`failed`,`skipped`); cleared on reset |
| attempts | integer | auto-increments on every upsert conflict |
| error | varchar(500) | truncated to 500 chars |
| details | text | |

**Unique constraint:** `idx_processingsteps_item_step UNIQUE (item_id, step)` — the
ON CONFLICT target. Partial indexes exist: `idx_processingsteps_pending (step) WHERE status='pending'`
and `idx_processingsteps_inflight (startedat) WHERE status='in_progress'`.
View `katalogservice_itemprocessingsteps` adds computed `statuscriticality integer`.

### com_nalet_katalog_mediasegments  (TIDB-aligned skippable segments)
| column | type | notes |
|---|---|---|
| id | varchar(36) | PK; `UUID.randomUUID()` per row |
| createdat / modifiedat | timestamp | both set to same `now()` on insert |
| item_id | varchar(36) | |
| kind | varchar(20) | one of ALLOWED_KINDS |
| startms | bigint | |
| endms | bigint | |
| source | varchar(30) | one of ALLOWED_SOURCES |
| confidence | numeric | nullable |
| label | varchar(120) | nullable |

### com_nalet_katalog_itemchapters  (file-internal descriptive chapter atoms)
| column | type | notes |
|---|---|---|
| id | varchar(36) | PK; `UUID.randomUUID()` per row |
| createdat / modifiedat | timestamp | |
| item_id | varchar(36) | |
| startms | bigint | |
| endms | bigint | |
| title | varchar(120) | nullable |
| ordinal | integer | client-supplied OR 1-based array position fallback |

### com_nalet_katalog_subtitleassets  (sidecar subtitle tracks; read-only here)
`id, item_id, path varchar(2048), format varchar(10), lang varchar(10), label varchar(120), isdefault boolean`.

### com_nalet_katalog_playbackassets  (read for path resolution)
Key cols used: `item_id, path varchar(2048), isprimary boolean, codec, resolution, durationms`.
Claim/lookup queries always join on `isprimary = true`.

### com_nalet_katalog_items / com_nalet_katalog_itemexternalids
`items`: `id, type varchar(20), title, year int, durationms bigint, parent_id, seasonnumber int, episodenumber int, createdat`.
`itemexternalids`: `item_id, source varchar(30), externalid varchar(120)` — TMDB id via `source='tmdb'`.

---

## Enumerations (validation vocabularies — reproduce EXACTLY)

**ALLOWED_STEPS** (`ProcessingStepService`): `scan, tmdb, tidb, chapter, chromaprint,
blackframe, silence, subtitle, transcode, package`.

**ALLOWED_STATUS**: `pending, in_progress, done, failed, skipped, not_applicable`.
Terminal-with-finishedat set = (`done`, `failed`, `skipped`). `not_applicable` does
**NOT** set finishedat in the upsert CASE.

**ANALYZER_STEPS** (`AnalyzerController` — steps a per-file run owns):
`chapter, chromaprint, blackframe, silence, subtitle, tidb`.
(Note: `scan`, `tmdb`, `transcode`, `package` are valid steps but NOT analyzer-owned.)

**ALLOWED_PASSES** (claim): `per_file, tidb_first, transcoder, packager`.

**Segment ALLOWED_KINDS**: `intro, recap, credits, preview`.

**Segment ALLOWED_SOURCES**: `tidb, chapter, subtitle, silence, blackframe,
chromaprint, whisper, transnet, manual`.

---

## ProcessingStepService — the shared upsert engine

Centralised SQL+validation for `itemprocessingsteps`. Used by AnalyzerController and
(future) tmdb/transcode services. Two public methods:

### `upsert(itemId, step, status, error, details) -> int (affected rows)`
- Throws `IllegalArgumentException` if `itemId` blank, `step` not in ALLOWED_STEPS,
  or `status` not in ALLOWED_STATUS. (Controllers map this to HTTP 400.)
- `error` truncated to 500 chars.
- Single statement: `INSERT ... ON CONFLICT (item_id, step) DO UPDATE`.
- **Insert path:** `id = gen_random_uuid()::varchar`, `createdat = now`, `attempts = 1`,
  `startedat = now IF status='in_progress' ELSE NULL`,
  `finishedat = now IF status IN (done,failed,skipped) ELSE NULL`.
- **Conflict/update path:** `status = new`, `modifiedat = now`, `attempts = attempts + 1`,
  `startedat = now ONLY IF new status='in_progress' AND existing startedat IS NULL`
  (sticky), `finishedat = now IF new status IN (done,failed,skipped) ELSE keep existing`
  (i.e. a re-pend keeps the old finishedat; only terminal transitions overwrite it),
  `error = new`, `details = new`.
- Returns affected rows (1 normally; 0 only on DB failure → controllers return 500
  "upsert affected 0 rows").
- Go note: the Java uses explicit `::timestamp` casts to dodge Postgres inferring CASE
  placeholders as text. In Go (lib/pq / pgx) bind `time.Time` and this is a non-issue,
  but keep the sticky-startedat / terminal-finishedat semantics byte-for-byte.

### `resetForItems(itemIds, steps) -> int`
- No-op (returns 0) on empty inputs.
- `UPDATE ... SET status='pending', startedat=NULL, finishedat=NULL, error=NULL,
  modifiedat=now() WHERE item_id = ANY(?) AND step = ANY(?)`.
- **attempts is intentionally preserved** (audit trail survives a reset).

---

## AnalyzerController — `/api/analyze/*` (work-queue API)

### POST `/api/analyze/claim?pass=<pass>&limit=<N>`
Atomic dequeue. `pass` default `per_file`; `limit` default `4`, clamped to **[1,32]**
(`Math.max(1, Math.min(limit,32))`). Unknown pass → **400** `{"error":"unknown pass '<p>'; allowed: [...]"}`.

Common claim shape (CTE `next_items`): filter `items.type IN ('movie','episode')`,
order `createdat DESC NULLS LAST`, `FOR UPDATE SKIP LOCKED LIMIT ?`. The lock + the
`NOT EXISTS (status='in_progress' on same item)` clause keep concurrent workers race-free.

Per-pass candidate predicate (all also require `NOT EXISTS in_progress` on the item):

| pass | extra item predicate | playback-asset required? | claim-time step flip |
|---|---|---|---|
| `per_file` (default) | `EXISTS step.status='pending' AND step = ANY(ANALYZER_STEPS)` | yes (`playbackassets.isprimary`) | **none** (worker flips its own first detector step) |
| `tidb_first` | must have TMDB id (own for movie / `parent_id`'s for episode) AND `EXISTS step='tidb' status='pending'` | **no** (TIDB doesn't read file; `path=NULL`) | flips `tidb` → `in_progress` for each claimed |
| `transcoder` | `EXISTS step='transcode' status='pending'` | yes | flips `transcode` → `in_progress` |
| `packager` | `EXISTS step='package' status='pending'` AND `NOT EXISTS step='transcode' status='pending'` | yes | flips `package` → `in_progress` |

**Response 200** `{ "pass": <pass>, "claimed": <int>, "items": [ Item... ] }`.

**Item object** (LinkedHashMap order):
```
id (string), type, title, year (int), durationMs (bigint),
path (string|null — null for tidb_first),
seasonNumber (int|null), episodeNumber (int|null),
seriesTitle (string)   // ONLY present for pass=packager (parent_item join); omitted otherwise
seriesTmdbId (string|null),  // parent series' tmdb externalid (episodes)
movieTmdbId (string|null)    // item's own tmdb externalid (movies)
```
Note camelCase JSON keys map to lowercase DB columns: `durationMs`←`durationms`,
`seasonNumber`←`seasonnumber`, etc. `seriesTitle` is conditionally included only when
the row map contains `series_title` (packager pass only).

### GET `/api/analyze/items/{itemId}/steps`
Returns current status of every step row for the item.
**200** `{ "itemId": <id>, "steps": { "<step>": "<status>", ... } }` (map; missing
steps simply absent). Used by per_file worker to skip already `done`/`not_applicable` pipelines.

### POST `/api/analyze/items/{itemId}/steps/skip`
Bulk-mark steps `not_applicable`. Body `{ "steps": ["blackframe","silence",...], "reason"?: "..." }`.
Missing/empty `steps` array → **400** `{"error":"missing 'steps' array"}`. Default reason
`"tidb_first short-circuited the per_file pipeline"`. Iterates `steps.upsert(id, step,
"not_applicable", reason, null)`; unknown step names are silently skipped (caught
IllegalArgumentException, batch continues). **200** `{ "itemId", "updated": <count> }`.

### GET `/api/analyze/items/{itemId}`
Single-item lookup with primary playback path. Joins `playbackassets isprimary=true`.
**404** if no primary asset. **200** `{ id, type, title, year, durationMs, path }`.
Used by the packager flow (`POST /api/package/{itemId}` in katalog-analyzer) to resolve
on-disk source.

### GET `/api/analyze/items/{itemId}/siblings?limit=<N>`
Same-series, same-season sibling episodes with primary paths (for chromaprint cross-episode
fingerprinting). `limit` default 5, clamped **[1,12]**. Join: `s.parent_id = me.parent_id
AND s.id <> me.id AND s.seasonnumber = me.seasonnumber AND s.type='episode'`, ordered
`s.episodenumber NULLS LAST`. **200** `{ "itemId", "items": [ {id,type,title,year,durationMs,path}... ] }`.
Empty list when not an episode / no qualifying siblings.

### POST `/api/analyze/series/{seriesId}/reset`
Operator-triggered re-analysis of a whole series. Steps:
1. `UPDATE items SET createdat = now() WHERE parent_id = seriesId AND type='episode'`
   (bumps episodes to head of DESC claim queue).
2. `resetForItems(episodeIds, ANALYZER_STEPS)` → re-pend analyzer steps (attempts preserved).
3. `DELETE FROM mediasegments WHERE source='chromaprint' AND item_id IN (episodes)`
   (purge stale chromaprint segments).
**200** `{ "seriesId", "episodes": <n>, "stepsReset": <n>, "segmentsPurged": <n> }`.

### POST `/api/analyze/items/{itemId}/fail`
Catastrophic worker failure. Body (optional) `{ "reason": "..." }`; default
`"unspecified analyzer error"`.
1. `upsert(itemId, "scan", "failed", reason, null)` — failures not pinnable to a
   pipeline are attributed to the synthetic `scan` step. If 0 rows → **500**
   `{"error":"step upsert affected 0 rows"}`.
2. `UPDATE itemprocessingsteps SET status='skipped', finishedat=COALESCE(finishedat,now()),
   modifiedat=now() WHERE item_id=? AND status IN ('pending','in_progress') AND step=ANY(ANALYZER_STEPS)`.
**200** `{ "itemId", "status":"failed", "stepsSkipped": <n> }`.

### PUT `/api/analyze/items/{itemId}/steps/{step}`  ← PRIMARY step-status contract
Per-pipeline status write (one PUT per pipeline per item). Body
`{ "status": "<status>", "error"?: "...", "details"?: "..." }`. `status` required/non-blank
→ else **400** `{"error":"status is required"}`. Delegates to `steps.upsert`; bad
step/status → **400** with the exception message; 0 rows → **500**.

**Chain promotion (transcode → package):** when `step=="transcode"` AND status in
(`done`, `not_applicable`, `skipped`), best-effort
`upsert(itemId, "package", "pending", null, "auto-promoted after transcode=<status>")`.
This is what feeds the `packager` claim pass. Inserted only when no package row exists
(ON CONFLICT updates but the intent is: failed transcodes do NOT promote; a re-run must
not clobber an already-`done` package — operator Resets to redo). Promotion failure is
swallowed (still returns 200). **200** `{ "itemId", "step", "status" }`.

---

## SegmentsController — `/api/segments/*`  ← analyzer fused-output contract

Idempotent replace (DELETE-then-batch-INSERT) of TIDB-aligned skippable segments.

### PUT `/api/segments/items/{itemId}`
Body `{ "segments": [ Segment... ] }`.
- Item existence check (`COUNT(*) items WHERE id=?`); unknown → **404** `{"error":"unknown item: <id>"}`.
- Missing `segments` array → **400** `{"error":"missing 'segments' array"}`.
- Each segment must be an object → else **400** `{"error":"segment entries must be objects"}`.

**Segment fields:**
```
kind        (required, ∈ ALLOWED_KINDS)     → else 400 "kind must be one of [...]"
source      (required, ∈ ALLOWED_SOURCES)   → else 400 "source must be one of [...]"
startMs     (required, long)                ┐ need 0 <= start < end → else 400
endMs       (required, long)                ┘ "startMs/endMs invalid (need 0 <= start < end)"
confidence  (optional, double|null)
label       (optional, string|null)
```
Numeric coercion: accepts JSON number OR numeric string for `startMs/endMs/confidence`.
Validation order: kind → source → start/end. Then atomically
`DELETE mediasegments WHERE item_id=?` + `batchUpdate INSERT (id,createdat,modifiedat,
item_id,kind,startms,endms,source,confidence,label)` with fresh UUID + single `now()` per row.
Does **NOT** touch processing-step rows (legacy `items.analyzerstatus` removed in migration 017;
steps are written separately via the PUT step endpoint).
**200** `{ "itemId", "written": <count> }`.

### DELETE `/api/segments/items/{itemId}`
`DELETE mediasegments WHERE item_id=?`. **200** `{ "itemId", "removed": <n> }` (plain map, no 404 check).

---

## ChaptersController — `/api/chapters/*`  ← file-internal chapter atoms

Sibling of segments; chapters are descriptive/non-skippable (migration 018 split).

### PUT `/api/chapters/items/{itemId}`  ← analyzer chapter-ingest contract
Body `{ "chapters": [ Chapter... ] }`.
- Item existence check → unknown **404** `{"error":"unknown item: <id>"}`.
- Missing `chapters` array → **400** `{"error":"missing 'chapters' array"}`.
- Each entry must be object → else **400** `{"error":"chapter entries must be objects"}`.

**Chapter fields:**
```
startMs   (required, long)   ┐ need 0 <= start < end → else 400
endMs     (required, long)   ┘ "startMs/endMs invalid (need 0 <= start < end)"
title     (optional, string) → blank/missing stored as NULL
ordinal   (optional, int)    → fallback = 1-based position in array (matches migration-018 row_number backfill)
```
Atomic `DELETE itemchapters WHERE item_id=?` + `batchUpdate INSERT (id,createdat,
modifiedat,item_id,startms,endms,title,ordinal)`. **200** `{ "itemId", "written": <count> }`.
No kind/source/confidence on chapters (that's the segments table).

### DELETE `/api/chapters/items/{itemId}`
`DELETE itemchapters WHERE item_id=?`. **200** `{ "itemId", "removed": <n> }`.

---

## SubtitlesController — `/api/subtitles/*`  (client-facing read/serve)

### GET `/api/subtitles/items/{itemId}`
List available tracks. Query `SELECT id,format,lang,label,isdefault FROM subtitleassets
WHERE item_id=? ORDER BY isdefault DESC, label ASC`. **DataAccessException is swallowed**
(table-may-not-exist) → returns `{ "subtitles": [] }` so the player hides captions.
**200** `{ "subtitles": [ { id, lang, label, format?, url: "/api/subtitles/<id>", default?: true } ] }`.
- `format` omitted if null; `default` key present (=`true`) only when `isdefault` is true.
- Clients pick renderer by `format`: text (`webvtt`,`srt`) → native `<track>`/ExoPlayer text;
  bitmap (`pgs`,`vobsub`,`dvb`) → image overlay.

### GET `/api/subtitles/{subId}`  (serve track; raw servlet write, not JSON)
`SELECT path,format FROM subtitleassets WHERE id=? LIMIT 1`. Unknown id → **404** "unknown
subtitle". File missing/unreadable on disk → **404** "file missing". Sets
`Cache-Control: private, max-age=300`.
- `pgs` → `Content-Type: application/pgs`, raw `Files.copy`, no transcode.
- `vobsub` → `application/x-vobsub`, raw copy.
- `dvb` → `application/dvb-subtitles`, raw copy.
- else → `Content-Type: text/vtt; charset=utf-8`; if `format=srt` run SRT→VTT; else if
  body doesn't start with `WEBVTT` prepend `"WEBVTT\n\n"`.
- **SRT→VTT** (`srtToVtt`): normalise CRLF→LF, regex replace `(\d{2}:\d{2}:\d{2}),(\d{3})`
  → `$1.$2` (comma→dot ms), prepend `"WEBVTT\n\n"`. Cue numbers preserved.

---

## ChaptersDbClient — external ChaptersDB ("theintrodb.org-style") client

Community chapter DB keyed by tvdbId/imdbId; chapter entries often carry names
("Opening Credits", "End Credits") used to classify chapter→intro/credits.
Anonymous read. **Disabled by default.**

Config: `chaptersdb.enabled` (env `CHAPTERSDB_ENABLED`, default `false`);
`chaptersdb.base-url` (env `CHAPTERSDB_BASE_URL`, default `https://chaptersdb.com`,
trailing slashes stripped). HTTP: connect timeout 10s, per-request timeout 15s, follow
redirects NORMAL, `Accept: application/json`, only status 200 parsed (else empty).

### `findShow(title, year, type) -> Optional<Show>`
No-op (empty) if disabled / blank title. `GET {base}/api/shows/search?q=<urlencoded title>`.
Body expected JSON array. Match logic: filter by `type` (case-insensitive: "movie"|"tv");
remember first type-match as fallback; if a show's `year` string-equals `year.toString()`,
take it immediately (break). Returns yearMatch ?? typeMatch.

### `getMovieChapters(showId) -> List<ChapterEntry>`
No-op (empty) if disabled / null id. `GET {base}/api/chapters/by-show/<showId>`. Body is
an array of chapter *sets*; **first set wins** (`body.get(0)`), read `set.entries[]`. Each
entry: `time` (parsed `HH:MM:SS.mmm`→ms) + `name`. Entries with unparseable time skipped;
result sorted ascending by `startMs`.

### Types
- `Show { id, slug, title, type ("movie"|"tv"), year, tvdbId, imdbId }` — all String; null
  `id` ⇒ record discarded.
- `ChapterEntry { long startMs, String name }`.
- `parseTimestampMs("HH:MM:SS.mmm")` = `h*3_600_000 + m*60_000 + sec*1000`; requires
  exactly 3 colon-parts; NumberFormat errors → null.

---

## Error / status conventions (HTTP)
- 400: validation (unknown pass/kind/source/step/status, bad start/end, missing array, blank status).
- 404: unknown item (segments/chapters PUT), unknown subtitle id, missing subtitle file,
  no primary playback asset (analyze GET item).
- 500: `upsert affected 0 rows` (DB failure on step write).
- 200: all success paths; bodies are `LinkedHashMap` (preserve key order where it matters
  for snapshot tests, though JSON consumers shouldn't depend on it).

## Concurrency / queue invariants (preserve in Go)
- Claim uses `FOR UPDATE SKIP LOCKED` on the `next_items` CTE; combined with the
  per-item `NOT EXISTS status='in_progress'` guard this is the only thing preventing
  double-claim. Non-`per_file` passes flip their owning step to `in_progress` at claim
  time (per_file relies on the worker flipping its first detector step within ms).
- Strict chain: `transcode` → (done/n_a/skipped) → auto-seed `package=pending` →
  packager claim requires `package=pending AND NOT transcode=pending`.
- `attempts` is an append-only audit counter; never reset (not by resetForItems, not by
  series reset).
