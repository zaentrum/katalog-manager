# katalog-manager

Catalog-management API for the **zaentrum** platform — a Go + GraphQL service.
It owns the catalog write/admin surface (items, artwork, processing-step audit,
settings) and drives enrichment, scanning and packaging. A rewrite of the
former SAP CAP/Java service onto Go. Files enter the catalog through the
scanner and the neutral `POST /api/ingest`, and through nothing else; a
title's extras through `POST /api/extras` (GraphQL `addExtra`) and the
scanner's extras convention (see [Extras](#extras)).

## Architecture

The surface is split deliberately:

- **GraphQL** (`/query`) — the operator/UI read graph and mutations: catalog
  reads (`items`, `movies`, `series`, `episodes`, `albums`, `item` with nested
  facets + computed fields; an item's `people` are its credits, each a person
  in a role with its job, character, order and episode count), `searchItems`
  (a page of matches, and in `total` how many match in all), `catalogStats`
  (how many movies, series, episodes and people the catalog holds, counted
  rather than paged), `scanJobs` (each with its `report`, see
  [Disc images](#disc-images)), `settings`, `deletedItems` (the deletion
  log, read-only), `people` and
  `person` (a person's TMDB details, locks and field origins, and their
  `credits`: every title that credits them, newest first, each with its role,
  job, character, order and episode count), `referenceSync`
  (the change-list refresh's cursors and last runs, read-only),
  `processingOverview` (every step's items by state, the failed steps with
  their last errors, how the service retries, and the re-encode queue),
  `pipelineRules` and `libraryCompliance` (the rules the workers report, and
  the titles that break them, see [Library compliance](#library-compliance)),
  and the operator actions
  (`triggerScan`, `enrichOne`/`enrichPending`, `refreshPeople`, `packageItem`,
  `validateItem`, `retryStep`/`retryFailed` (see
  [The pipeline heals itself](#the-pipeline-heals-itself)), `reencodeItem`
  (see [Encoding a title again](#encoding-a-title-again)),
  `clearReencodeQueue` (see [The re-encode queue](#the-re-encode-queue)),
  `queueRework`, `queueReworkMatching` and `refreshCompliance` (see
  [Library compliance](#library-compliance)),
  `replaceSource` (see [Replacing a title's file](#replacing-a-titles-file)),
  `backfillSourceProbes`, `backfillSourceSizes`, `backfillRatings` and `setMinAgeOverride` (see
  [Ratings](#ratings); an item's `ageRating` says what it is rated),
  `setTrackLanguage` and `backfillSourceTracks` (see
  [Track languages](#track-languages); an item's `tracks` are its source's
  audio and subtitle tracks with the language each plays as), `addExtra`,
  `removeExtra`, `packageExtra` and `packageExtras` (see [Extras](#extras);
  an item's `extras` are its trailers and other bonus material), item +
  settings CRUD; `identify` re-matches a title (see
  [Identify](#identify)); a delete removes files from
  disk only when asked; a secret setting, such as an API key, is write-only:
  `setSecretSetting`/`clearSecretSetting`, and no field returns its value;
  `setSecretSetting` checks a TMDB token with TMDB's authentication endpoint
  first, for 4 seconds at most: a token TMDB refuses (401), or one with a
  control character in it, is not stored and the answer is an error with the
  code `SECRET_REFUSED`, one TMDB takes is
  stored `valid`, and one TMDB could not be asked about is stored all the
  same, `unchecked`, as the answer's `check` says).
  It is the catalog console's: every field is an administrator's (see
  [Who may do what](#who-may-do-what)). Schema-first via
  [graph-gophers/graphql-go](https://github.com/graph-gophers/graphql-go) — the
  SDL is `internal/graph/schema.graphql`; resolvers are plain Go methods.
- **REST** (`/api/*`) — everything that is byte-oriented or a machine contract,
  kept exactly compatible with the clients and workers that depend on it:
  - `GET /api/artwork/{id}/{kind}` — raw image bytes (bearer JWT **or** a
    `?stream=` HMAC token, verified byte-for-byte against chino-api's minter).
  - `GET /api/artwork/person/{personId}/profile` — a person's primary
    portrait, authorized and cached like a title's artwork, with an `ETag` of
    its sha256 (`If-None-Match` gets a 304); 404 when they have none.
  - `GET /api/play/{itemId}` — HTTP byte-range streaming.
  - `GET /api/subtitles/...` — VTT/SRT→VTT/passthrough.
  - `GET /api/analyze/items/{id}`, `PUT /api/analyze/items/{id}/steps/{step}`,
    `PUT/DELETE /api/segments|chapters/items/{id}`,
    `POST /api/items/{id}/packaging-complete` — the analyzer/packager worker
    protocol. The item record names the languages an admin set for the
    source's tracks and the subtitle files beside the source (see
    [Track languages](#track-languages)); packaging-complete replaces the
    package's subtitles, each with whether it is forced, keeps those files
    (the packager's rendition of one gets no row of its own), and records
    the source's tracks the manifest lists.
  - `PUT /api/analyze/rules/{worker}` — the transcoder and the packager
    report the rules they run with (see
    [Library compliance](#library-compliance)).
  - `GET /api/settings` — the settings that are no secret, for the workers:
    `{"<key>": {"valueText": "...", "valueType": "..."}}`. The packager reads
    its language whitelist from it (`packager.language_whitelist`, comma
    separated) and `packager.keep_original_if_single`. A secret setting is
    left out, set or not.
  - `POST /api/ingest` — an addon hands a file on disk to the catalog, a
    title by its catalog ids or by its identity (see [Ingest](#ingest); a
    disc image is refused, 400: see [Disc images](#disc-images)).
  - `POST /api/extras` — a file taken in as a title's extra, and
    `GET /api/analyze/extras/{id}`, `PUT /api/analyze/extras/{id}/steps/{step}`,
    `POST /api/extras/{id}/packaging-complete` — an extra's worker protocol
    (see [Extras](#extras)).
  - `POST /api/items/{id}/package` — an admin's packaging action, as
    chino-api's admin route forwards it with the admin's token: what
    GraphQL's `packageItem` does, answered as the CAP service did
    (`{status, alreadyActive, message}`, a series'
    `{episodesEnqueued, episodesTotal, message}`; 404 unknown, 400 not
    packageable).
  - `POST /api/items/{id}/takein` — an admin's take-in of a title with the
    library's v2 layout (see [The library](#the-library)), answered
    `{itemId, sent, message}`.

## Who may do what

The service authenticates bearer tokens of its issuer (and, on an artwork
read, a stream token), then tells three callers apart: an **administrator**,
whose token carries the admin role; the platform's **service account**, a
token issued to one of its clients (`azp`), which the pipeline workers and a
deployment's scan Job mint with client credentials; and an **addon**'s service
account, whose token carries the addon role. Everyone else signed in is a
**viewer**.

| Operation | Who may |
|---|---|
| every GraphQL query and mutation (the catalog with its paths on disk, scan jobs, activity, settings, the deletion log, every change) | admin |
| GraphQL `triggerScan` | admin, service account |
| GraphQL `replaceSource` | admin, service account (`deleteOldFile: true` an admin's) |
| `GET /api/manage/stream` (the console's live stream) | admin |
| `GET /api/artwork/...`, `/api/manage/artwork/...` (also a person's portrait) | any signed-in caller, or a stream token; a capped viewer is answered for a title above its cap as for a title there is not |
| `GET /api/play/...`, `GET /api/subtitles/...` | any signed-in caller; a capped viewer as for the artwork |
| `GET /api/library/subtitles/languages` (the languages every title is to have a subtitle in) | any signed-in caller |
| `PUT /api/artwork/...`, `/api/analyze/*` (an extra's record and steps too, the workers' rules), segments, chapters, `packaging-complete` (an item's and an extra's), `GET /api/settings` | admin, service account |
| `POST /api/library/migrations/{run}/adopt`, `…/revert` (the library's migration), `POST /api/library/projections` | admin, service account |
| `POST /api/library/reencode`, `GET /api/library/reencode` (the re-encode queue) | admin, service account |
| `POST /api/ingest`, `POST /api/extras` | admin, service account, addon |
| `GET`, `POST /api/library/items/{id}/subtitles`, `DELETE …/subtitles/{subtitleId}` (attached subtitles) | admin, service account, addon; a removal the caller's own attach, an admin any |
| `POST /api/items/{id}/package`, `POST /api/items/{id}/takein` | admin |

A refused GraphQL field answers with an error whose `extensions.code` is
`FORBIDDEN` and whose message names the role; a refused route answers 403
with `{"error": "..."}`. Introspection and `__typename` answer any signed-in
caller.

A **capped viewer** is one whose access token carries `max_rating`, a whole
number of years (a kid's account), or whose stream token carries the cap
chino-api minted it with. On the artwork, playback and subtitle routes it is
served a title rated at most its cap; one above it, and an unrated one unless
`ratings.unrated_for_capped` says `show`, is answered as a title there is not
(404, or an empty subtitle list), so the answer does not say it exists (see
[Ratings](#ratings)). A claim that is no whole number of years holds its
caller to the strictest cap, 0, and the service says so once in its log. A
token without the claim is not capped.

## Data

The service reuses the existing `katalog` Postgres database **unchanged** — the
lowercase `com_nalet_katalog_*` tables and the computed `katalogservice_*` views.
No migration destroys data: the one that drops anything (035) drops only tables
nothing reads that hold no row. The files in `db/migrations/` apply on top of
the base schema in the order of their numbers, and each is idempotent:

- `028_go_rewrite.sql` creates nothing any more: the two objects it added
  served an integration the core no longer carries (see the file). A catalog
  that applied it keeps them until 035 drops them; nothing reads or writes
  them.
- `029_deleted_items.sql` adds the deletion log, `com_nalet_katalog_deleteditems`:
  every item the catalog deletes, and every person it deletes because no title
  credits them any more (type `person`). A title's credits follow TMDB: read
  from TMDB, they replace the title's credits, and a person left uncredited
  goes. They come in every role TMDB's credits give — its cast as actors, a
  series' creators, and of the crew its directors, writers, producers,
  composers, cinematographers and editors — one credit per person and role.
  A title whose credits change is modified with them (`modifiedat`,
  `modifiedby`), so a record projected from it knows it is stale.
  The service creates the log at startup when it is missing; where its role
  may not create tables, apply the file by hand.
- `030_people.sql` gives a person their TMDB id and details (dates, places,
  biography per language, also-known-as names, locks and field origins), adds
  their images (`com_nalet_katalog_personartwork`) and the cursors of TMDB's
  change lists (`com_nalet_katalog_referencesync`). The service applies it at
  startup when any of it is missing; where its role may not alter the people
  table, apply the file by hand.
- `031_item_locked_fields.sql` gives a title `lockedfields`: with `credits` (or
  `people`) among them, TMDB neither adds nor drops a credit of the title, as
  with `metadatalocked`. Applied at startup like 030.
- `032_credit_details.sql` gives a credit what TMDB says of it besides its
  role: the job (`job`), the character (`charactername`), its place in the
  role (`ordinal`) and a series' episodes (`episodecount`), all unknown for a
  credit older than it. A credit is still the title, the person and the role;
  these change in place. Applied at startup like 030.
- `033_step_retries.sql` gives a processing step what the service keeps to
  retry it: its failures in a row (`failures`), the error of its last failed
  run (`lasterror`), when it is retried by itself (`nextretryat`) and when its
  trigger was last sent again (`dispatchedat`). A step older than it has no
  retry scheduled: the service retries what fails after it, an admin what
  failed before. Applied at startup like 030; without it the pipeline runs
  as before, and nothing retries a step.
- `034_scan_job_runner.sql` gives a scan job the process that runs it
  (`runner`, its host and a tag of the process's start) and its scanner's
  last word (`heartbeatat`), so that a scan the service lost is told from one
  that walks (see [Lost scans](#lost-scans)). A job older than it names no
  runner. Applied at startup like 030; without it a scan a restart cuts short
  says running, as before.
- `035_retired_job_tables.sql` drops what is left of the integration 028
  served: its job table (`com_nalet_katalog_trailerjobs`) and the base
  schema's read model of its job events (`com_nalet_katalog_downloadjobs`,
  with its view `katalogservice_downloadjobs` and the index 028 put on it),
  each only while it holds no row and never with anything else that hangs off
  it. A table that holds rows is kept as it is, and the service says so once
  at startup. Applied at every start: a base schema that creates the read
  model again gets it dropped again.
- `036_item_ratings.sql` gives a title its age rating: the certification TMDB
  gives it (`certification`, "12", "PG-13"), its country
  (`certification_country`, ISO 3166-1 alpha-2), the minimum age it means
  (`min_age`, 0 to 21), an admin's rating that wins over it
  (`min_age_override`) and when TMDB was last read
  (`certification_fetched_at`), and `idx_items_rated_age`, the index of the
  age a title without a parent is held to. An episode carries no
  certification: it is rated as its series. Applied at startup like 030;
  without it no title is rated, and a capped viewer is served nothing.
- `037_track_languages.sql` keeps a title's source tracks as the packager
  read them (`com_nalet_katalog_itemtracks`: each audio and subtitle track's
  language tag, title tag, a subtitle's format and whether it is forced) and
  an admin's language of a track (`com_nalet_katalog_itemtracklanguages`, an
  ISO 639-2 code), both keyed by the item, the kind and the ordinal (see
  [Track languages](#track-languages)). Applied at startup like 030; without
  it no track's language can be set, and the packager labels every track as
  its source tags it.
- `038_subtitle_forced.sql` gives a subtitle `isforced`: whether it is
  forced, as its package says; false for a subtitle file beside a source,
  and for a subtitle older than it until its title is packaged again.
  Applied at startup like 030; without it no subtitle is kept as forced.
- `039_item_extras.sql` keeps a title's extras
  (`com_nalet_katalog_itemextras`, one row per extra keyed by its extraId):
  its kind, title, language and season, its file with its size and quick
  hash, how it was taken in, what a viewer is shown (order, hidden, label),
  where its packaging stands, its package and its removal, with the indexes
  of an item's extras, of a live extra's file (one extra per file) and of
  the extras the sweep sends and heals (see [Extras](#extras)). An item's
  rows go with it. Applied at startup when its table or an index of it is
  missing; without it no extra is taken in, and katalog-api lists none. A
  read-only role (katalog-api's) needs a grant on the new table.
- `040_library_v2.sql` keeps the library record's paths (see
  [The library](#the-library)): a title's originals
  (`com_nalet_katalog_itemsources`: where each arrived, its size and quick
  hash, whether it is present, being retired, deleted after packaging or
  removed, its record and its deletion's event) and its package runs
  (`com_nalet_katalog_itemversions`: building, complete, superseded or
  removed, one complete one at most, where each is and how far it was
  verified); when a title's `item.json` was written (`recordedat`), the
  `modifiedat` its projection reflects (`libraryprojectedat`, a person's
  too) and whether its originals are held (`retirehold`); the source and
  the version a playback row is of; an extra's package, record and the
  deletion of its original. Nothing reads them while `library.layout` is
  `legacy`. Applied at startup when any of it is missing; katalog-api's
  read-only role is granted the new tables.
- `041_library_projection.sql` marks a title changed (`modifiedat`) when
  anything its projection holds changes: its genres, tags, credits,
  reference ids, artwork and trailer links, a recorded extra's order,
  visibility, label or removal, an episode added, moved or deleted (its
  series), a credited person renamed, a person's portraits (the person).
  Its triggers are statement-level, so a statement marks a title once.
  Applied at startup after 040.
- `042_playback_item_index.sql` indexes a title's playback rows
  (`idx_playbackassets_item`), which katalog-api reads by title.
- `043_reencode_queue.sql` adds the re-encode queue
  (`com_nalet_katalog_reencodequeue`): a title a row, its state (`queued`,
  `sent`, `done`, `failed`), what queued it (`items`, `held`, `all`), when
  and by whom, when it was sent and ended, and why it waits or failed; one
  row a title while it waits or is sent. A deleted item's rows go with it.
  Applied at startup when missing; katalog-api's read-only role may read it.
- `044_library_takein.sql` lets a version be `taken`: its folder holds its
  original and no package (see [The library](#the-library)). Applied at
  startup when missing.
- `045_multi_episode_files.sql` gives an episode that another episode's file
  covers `coveredby`, the id of that episode, its holder (see
  [One file, several episodes](#one-file-several-episodes)), null on every
  other item, and `idx_items_coveredby`, the index of a holder's covered
  episodes; and a scan job its `report`, what its scan passed over and left
  alone (see [Disc images](#disc-images)). Applied at startup when any of it
  is missing; without it a file covers the episode its name numbers first
  and no other, as before, and a scan says what it passed over in the log
  alone.
- `046_series_scan_title.sql` gives a series `scantitle`, the name the
  scanner made it under, in lower case, and `idx_items_series_scantitle`, the
  index of it: the scanner finds a series by it before its sort title, so a
  show enrichment renamed stays one series. Applied at startup when any of
  it is missing; without it the scanner finds a series by its sort title
  alone, as before.
- `047_identity_indexes.sql` indexes what a title is found by its identity
  by (`idx_itemexternalids_identity`, a source and its id), an item's ids
  (`idx_itemexternalids_item`) and an item's children (`idx_items_parent`),
  which [ingest by identity](#by-identity) and katalog-api's holdings read.
  Applied at startup when any of them is missing.
- `048_attached_subtitles.sql` gives a subtitle what the catalog keeps of
  one [attached to a title](#attached-subtitles) once its version is in the
  library: `origin` (how it was made; null for the title's own), `issdh`,
  `versionid` (the version it follows), `sha256` (its file's hash, which
  names the file), `attachedby` and `attachedat`. Applied at startup when
  any of them is missing; without it no subtitle can be attached, and a
  package is taken in as before.
- `049_catalog_search.sql` is what katalog-api's ranked search finds titles
  and people by. Four functions normalize a title, a name or a plot as a
  search reads it: lower case, accents off (unaccent's rules), apostrophes
  dropped ("Widow's" is "widows"), every other mark a space
  (`com_nalet_katalog_search_text`); the same with the marks inside a word
  dropped too, so "Spider-Man" is also "spiderman" and "S.W.A.T." "swat"
  (`com_nalet_katalog_search_joined`); the words of both
  (`com_nalet_katalog_search_words`) and the words of a plot
  (`com_nalet_katalog_search_plot`). They are immutable, and a change of
  what one returns makes new functions and new indexes, never an edit of
  these. Ten indexes hold them: a title's words, its text by type (for "the
  title is the query" and "begins with it"), its trigrams (a title close to
  the query), a series' `scantitle` (046), a plot's words, a person's name
  the same three ways and their other names (030), and a person's credits
  (`idx_itempeople_person`). It needs the `unaccent` and `pg_trgm`
  extensions, which it creates when they are missing and the role may.
  Applied in the background after startup, each index on its own and
  CONCURRENTLY: a build blocks neither the catalog's reads nor its writes and
  rewrites nothing, but waits for the transactions that began before it. A
  build that was cut short (a restart in the middle) leaves an invalid index,
  which the next start drops and builds again; one instance builds at a time
  (an advisory lock); a build that failed is tried again every ten minutes.
  A catalog of 78,000 items and 16,000 people builds them in seconds.
  katalog-api searches as before until all of them are valid. Where the role
  may not create them, run the file by hand with psql, each statement after
  a `--@index` line on its own, outside a transaction.
- `050_library_write_through.sql` marks a title changed when its marks
  change, its chapters (`com_nalet_katalog_itemchapters`) or its intro,
  recap and credits ranges (`com_nalet_katalog_mediasegments`), which its
  projection shows (`metadata.json` `marks`), whoever writes them: the
  analyzer's `PUT` and `DELETE` of them, a series' reset, the chapter
  database's; as 041 does, a statement marks a title once. It also marks a
  title when one of its extras is inserted or deleted recorded and not
  removed, the items of a genre renamed, and a person whose projected
  columns change unless the statement sets `modifiedat` itself (when the
  catalog last asked TMDB about them moves nothing). The version that plays
  and the file that covers an episode are marked by their writers
  (packaging-complete, the links of covered episodes), not by a trigger: an
  adopt writes the versions and links its staged projections show already.
  Applied at startup after 041 when any of it is missing.
- `051_library_compliance.sql` keeps the rules the transcoder and the
  packager report they run with (`com_nalet_katalog_workerrules`: a worker
  a row, its version, its rules as it reported them, the hash of what the
  check reads of them, when it reported and when they last changed) and
  what the library's compliance check found of each movie and episode
  (`com_nalet_katalog_compliance`: the rules it breaks and the findings,
  the sizes of its original and its package and the estimate once it is
  processed again, why a rule was not checked of it, why processing it
  again can't help as its original's record or its failed step tells, the
  hash of the rules it was checked against, when it was marked to be checked
  again and when it was checked; see [Library compliance](#library-compliance)). A deleted item's
  row goes with it. Applied at startup when any of it is missing; without it
  no rules are kept and nothing is checked.

## Track languages

A source's audio and subtitle streams are tagged with a language or not, and
not always rightly: a film without dialogue tagged `und`, English tagged
`und`. An admin sets the language a track plays as.

- **Codes.** A language is an ISO 639-2 code, three lowercase letters, in the
  B (`ger`) or the T form (`deu`). `zxx` is no linguistic content, a film
  without dialogue, shown as "No dialogue"; `und` is undetermined, shown as
  "Unknown".
- **A track** is its kind, `audio` or `subtitle`, and its ordinal: its place
  among the original source's streams of that kind in ffprobe's order, 0
  first (audio 0 is the first audio stream), counting a stream the
  transcoder leaves out too (a `mov_text` subtitle). It plays as the
  language an admin set, else the one its source tags it with, else `und`.
- **Setting one.** `setTrackLanguage(itemId, kind, ordinal, language)` sets
  it; `language: null` clears it. An item without a source file has no
  tracks. Once a package has reported the source's audio tracks an audio
  ordinal it did not report is refused, as a package carries every audio
  stream of its source; a subtitle's ordinal is not checked. The packager
  labels the track with it, found by its ordinal in the source, when it
  next packages the title: `reencodeItem` packages a packaged title again,
  and the packager reads the record then.
- **The source's tracks** are what the packager's manifest lists:
  `renditions.audio`, one entry per audio stream at its `idx`, and the
  `subtitles` with an id `sub<N>`, the subtitle at N among those of the file
  it packaged, except one marked `external` (the packager's rendition of a
  subtitle file). An entry that names its `ordinal` among the source's
  streams is at that ordinal. An encode leaves out the subtitle streams
  Matroska cannot copy, and the subtitles after one count lower in its
  package than in the source: for such a title the ordinals of the
  subtitles listed are the package's. packaging-complete records them;
  `backfillSourceTracks` reads them from the manifests of the packages on
  disk, for those packaged before. An item's `tracks` lists each with its
  `sourceLanguage`, `languageOverride` and `effectiveLanguage`; a language a
  package reports that equals an admin's says nothing of the source's tag,
  which the track keeps. The manifest's languages are the ones the tracks
  play as, as the packager labels them, and so are the subtitles and the
  packaged asset's audio language packaging-complete writes.
- **The worker record** (`GET /api/analyze/items/{id}`) names the languages
  set and the subtitle files the scanner found beside the source
  (`<video>.<lang>.srt|vtt|ass|ssa`), each key left out when there is none,
  so an older packager reads it as before. A file is named as the packager
  takes it: at its absolute path on the library storage, in the source's
  folder or below it, a `.srt`, `.vtt`, `.ass` or `.ssa` file there is, of
  at most 50 MB, by path:

  ```json
  {"id": "…", "type": "movie", "title": "Sintel", "path": "/var/lib/katalog/media/Sintel/Sintel.mkv", "…": "…",
   "hasOwnPoster": true, "hasOwnBackdrop": true,
   "trackLanguages": [{"kind": "audio", "ordinal": 0, "language": "eng"},
                      {"kind": "subtitle", "ordinal": 0, "language": "ger"}],
   "subtitleFiles": [{"path": "/var/lib/katalog/media/Sintel/Sintel.en.srt", "language": "eng",
                      "label": "English", "forced": false}]}
  ```

  A subtitle file's language is the code its name gives as an ISO 639-2 code
  (`de` as `deu`), `und` when it gives none; `forced` is a JSON boolean,
  false for every file the scanner records. The packager converts each file
  to WebVTT and packages it after the source's own subtitles, marked
  `external`, never the default unless forced; the scanner marks no subtitle
  file the default.

## Identify

`identify(id, title, tmdbId)` matches a title by hand: a TMDB id pinned, or a
search with a corrected title. It is a re-match whatever the match it lands
on: the title's genres, trailers and artwork are the match's, and what an
earlier match left goes. The match's genres replace the title's (none when
it has none); TMDB's trailers without a local copy go, also when the match
has none, while one with a local copy (`downloadedAt`, `localPath`) and one
added by hand stay; every poster and backdrop row goes but the marker of a
keyframe the analyzer extracted from the title's own file, and the image of
a kind the match gives none of goes unless it is that keyframe. A series'
episodes the new match knows are re-matched alike. An identify that finds no
match changes none of them. A refresh (enrichment, the sweep, the change
lists) adds genres, keeps the trailers when TMDB cannot be asked, and
replaces TMDB's trailers with the list TMDB answers, an empty one too.

## Replacing a title's file

`replaceSource` gives a movie or an episode another file, its source, and
keeps the title: for a title upgraded to a better file of the same work (a
demo's 320×180 copy to its 1080p original, a copy in another container,
`Film.mp4` to `Film.mkv`). Without it the new file would be a new title at
the next scan, which finds a file's title by its path, and the old title
would be left without its file.

```graphql
mutation {
  replaceSource(itemPath: "/var/lib/katalog/media/BigBuckBunny_320x180.mp4",
                path: "/var/lib/katalog/media/Big Buck Bunny (2008).mov", deleteOldFile: true) {
    itemId oldPath path replaced oldFileDeleted oldSidecars
    reencode { reencoded busy notSent message }
    message
  }
}
```

- **The title** is named one way: `itemId`, or `itemPath`, the path of the
  file replaced, which a demo's reset keeps where it changes ids. It is a
  movie or an episode with a file: a series has none (its episodes are
  replaced each on its own), and a title with two files is named by the one
  replaced.
- **`path`** is the new file: an absolute path of an existing video file
  under the media root, never under the package store, never a link that
  leads out of the media root, that a scan takes for a title's file (not
  hidden, a video by the scanner's extensions `.avi`, `.m4v`, `.mkv`, `.mov`,
  `.mp4`, `.webm`, and no extra by the scanner's convention as it reads with
  `extras.scan` on), and no file of the catalog yet: another title's file or
  an extra's is refused, naming it. One replace onto a file runs at a time.
  A path that is the title's file already changes nothing (`replaced:
  false`).
- **What stays**: the title's id and everything that hangs off it, its
  metadata, external ids, artwork, credits, genres, tags, extras, segments,
  chapters and steps; its package and its subtitles, which play until the
  new package is in place; the languages an admin set for its tracks, which
  name a track by its kind and its place among the source's streams, and so
  apply to the new file's track at that place (one the new file does not
  have is passed over); and everything kept elsewhere by the title's id, as
  everyone's progress and ratings. The title is modified (`modifiedAt`,
  `modifiedBy`).
- **What goes** is what described the old file: the source asset's hash,
  probe (codec, resolution, bit rate, duration), audio and track counts (its
  size is the new file's), the tracks a package reported of the old file and
  the title's diagnostics. The workers report them again of the new file.
- **The subtitle files** beside the old file and named after it stay the
  title's as they are, and the answer counts them (`oldSidecars`). Beside a
  new file of the same name they pair with it as they did; otherwise the next
  scan pairs the files named after the new one, and the packager takes
  subtitle files only from the new file's folder or below it.
- **`deleteOldFile`** (false when omitted) deletes the old file once the
  title has the new one: only under the media root and never under the
  package store, only when no title's file or extra is it any more and the
  new file does not lead to it; the folders it leaves empty go. An old file
  kept under the media root is taken in as a title of its own by the next
  scan: move it out of the media root, or delete it.
- **`reencode`** (true when omitted) encodes the title again from the new
  file, as `reencodeItem` does (see
  [Encoding a title again](#encoding-a-title-again)), and `reencode` is its
  answer. A title whose transcode or package is running is left alone
  (`busy`): that run is the old file's, and what it reports (the probe, the
  tracks) describes the old file until the title is encoded again, which an
  admin asks once it is done; a run whose old file was deleted under it may
  fail instead, and its retry reads the new file. Without an event bus or
  migration 033 nothing is encoded again, and `reencode.message` says why.
- **A refusal** is an error whose `extensions.code` is `NOT_FOUND` (a title
  there is not), `SOURCE_CONFLICT` (the file is another title's or an
  extra's, named in `itemId` and `extraId`) or `SOURCE_REFUSED`, and nothing
  changes. The old file's deletion and the re-encode never fail the call:
  the title has its new file whatever they do, and `message` says what they
  did. Every replace is logged, with whoever asked it.

Replace a file before a scan meets it: a file a scan took in first is
another title's, and refused. `replaceSource` is an administrator's and the
platform's service account's, the one field besides `triggerScan` a
deployment's Job may ask (see [Who may do what](#who-may-do-what)): a seed
Job gives a title its better file with the service account, then deletes the
old file itself. `deleteOldFile: true` stays an administrator's: the service
account may move a title onto a file, never have the catalog delete one.

## Ratings

Kids' accounts are capped at an age, the `max_rating` claim of their access
token: katalog-api leaves every title rated above the cap out of what it
serves them, chino-api answers 404 for one asked for by id, and this service
answers its artwork, playback and subtitle routes as for a title there is not
(see [Who may do what](#who-may-do-what)).

A title's rating comes from TMDB. Enriching a movie reads its release dates
(`GET /movie/{id}/release_dates`): its certifications in a country are those
of its theatrical (type 3) and digital (type 4) releases there. Enriching a
series reads its content ratings (`GET /tv/{id}/content_ratings`). The
countries asked are the setting `ratings.countries`, ISO 3166-1 alpha-2 codes
separated by commas, in order (`CH,DE,US` when it names none): the first
country with a certification the table rates wins, and of its certifications
the strictest. The title keeps the certification as TMDB gives it, the
country and the minimum age it means; none of them when no country of the
list rates it. TMDB failing keeps the rating a title had. `metadataLocked`
does not stop it; an admin rates a title by hand with `setMinAgeOverride`,
whose age wins over TMDB's, and a series' over its episodes' (one with its
own override keeps it). An episode is rated as its series. Identify, the
change lists and `backfillRatings` (the titles TMDB was never read for, or
with `all` every one) rate as enrichment does.

The table (`internal/ratings`) says what each certification means in years,
for the countries TMDB lists certifications of (AU, BR, CA, CH, DE, DK, ES,
FI, FR, GB, IE, IT, JP, KR, LU, MX, NL, NO, NZ, PT, RU, SE, SG, US), films and
series alike, and it is conservative: an age a certification names is that
age; one that admits younger children only with an adult (12A, 15A, 14A,
PG12) is the age it names; parental guidance without an age is the age the
board names for it, else 10; restricted, refused and banned titles are 18.
Not rated (NR) and exemptions are no rating, and the next country is asked.

| Board | Certification → minimum age |
|---|---|
| FSK (DE) | 0 → 0, 6 → 6, 12 → 12, 16 → 16, 18 → 18 |
| Switzerland (CH) | 0, 6, 8, 10, 12, 14, 16, 18 → the age |
| MPA (US films) | G → 0, PG → 10, PG-13 → 13, R → 17, NC-17 → 18 |
| TV Parental Guidelines (US series) | TV-Y → 0, TV-Y7 → 7, TV-G → 0, TV-PG → 10, TV-14 → 14, TV-MA → 17 |
| BBFC (GB) | U → 0, PG → 8, 12 and 12A → 12, 15 → 15, 18 and R18 → 18 |

The rest of the table is in `internal/ratings/ratings.go`, each row with its
test. `ratings.unrated_for_capped` (`hide`, the default, or `show`) says
whether a capped viewer is served the titles nothing rates; katalog-api and
this service read it from the settings.

## The pipeline heals itself

A step that fails is retried: the event that triggers its worker is sent
again — `discovered` for `tmdb` (the enricher) and the `scan` step (the item's
pipeline from its start), `enriched` for the analyzer's passes, `analyzed` for
`transcode`, `transcoded` for `package` and `takein` — one event per item and
worker. Each worker passes the chain on, and its own guard skips work that is
done.

- **Backoff and attempts.** A failure is retried after `KATALOG_RETRY_BACKOFF`
  (1m), doubled with every failure in a row, at most
  `KATALOG_RETRY_BACKOFF_MAX` (1h), until the step has run
  `KATALOG_RETRY_MAX_ATTEMPTS` (3) times in a row; then it stays failed for an
  admin. A step records its failures in a row, its last error (at most 500
  characters, credentials redacted: a URL's user and password, tokens and
  keys in a query, bearers, JWTs, passwords written out) and its next retry.
- **The reaper.** A step in progress whose worker has been silent for longer
  than the step's timeout, or one sent again that no worker started within
  it, is taken for a failed run and retried the same way. A step is timed
  from its worker's last word (a worker that reports in progress again keeps
  it alive); the timeouts are 15m for `scan` and `tmdb`, 2h for the
  analyzer's passes, `package` and `takein`, 6h for `transcode`
  (`KATALOG_STEP_TIMEOUTS`).
- **No step runs twice.** A retry claims its step in the database before it
  sends anything, in one statement whose rows only one caller gets, so two
  instances, or an admin and the sweep, never send a step's retry twice, and
  a retry asked while one waits is refused. A step in progress or waiting for
  its worker within its timeout is left alone. `done`, `not_applicable` and
  `skipped` are terminal: nothing retries them.
- **By hand.** `retryStep(itemId, step)` retries a failed step (with or
  without attempts left) or a silent one now, its failures afresh;
  `retryFailed(step)` every failed step. The result says why a step was left
  alone.
- **Runs.** A step counts the runs it started (`attempts`): one more each
  time it turns in progress, and one for its worker's first report of it. A
  worker saying in progress again (the analyzer's heartbeat), a run's end
  and a failure reported again count nothing, and a step only enqueued
  (pending) has run none.
- An event that could not be sent puts its steps back, failed; the sweep
  sends it again a backoff later. Every `KATALOG_RETRY_INTERVAL` (30s) the
  sweep reaps and sends what is due; without an event bus or migration 033
  nothing is retried, and the overview says why.
- **Extras.** The sweep heals a title's extras by the same policy, each a
  chain of its own (see [Extras](#extras)); migration 033 is the steps', so
  it does whatever the steps' retries can.

### Encoding a title again

`reencodeItem(id)` encodes a title again with the pipeline's current
settings: the transcoder's ladder and encoder (its `LADDER` and `ENCODER`,
set per instance), then the packager's. A new ladder reaches only the titles
encoded after it; this is how a title that finished gets it. It takes a
movie or an episode with a file (a primary asset), or a series, whose
episodes with a file (under it or under a season of it) it encodes again
each as a title of its own.

- **What it does.** The title's `transcode` and `package` wait for their
  workers afresh, as a reset leaves a step (failures in a row and retries
  cleared; attempts and the last error kept), and the transcoder is sent
  `analyzed`, not marked as a retry: its guard finds the step waiting and
  runs it, and the packager follows once the transcode is done. A step the
  title lacks is added. The transcode is noted as sent, so the reaper heals
  a start that never comes; the package's trigger is the transcoder's to
  send, and the package waits for it, not reaped, however long the
  transcode runs.
- **Left alone.** A title whose transcode or package is running (its
  worker heard within the step's timeout), or waiting for its worker within
  it, is left alone: encoding it again would run the step twice. A package
  waiting for a transcode that has not finished waits for that transcode,
  which decides. Each title is reset under the lock of its steps, so two
  re-encodes at once, or a re-encode and the sweep, send it once. The
  answer counts the titles looked at, encoded again, left alone and not
  sent, and says why one was left alone.
- **Not sent.** An event that could not be sent puts the transcode back,
  failed; the sweep sends it again a backoff later.
- **Playback meanwhile.** The current package plays while the transcoder
  encodes (its handoff goes beside the package, not into it) and while the
  packager packages: it builds the new package in the item's `.next/` and
  swaps it in only once it is complete and checked, keeping the old one for a
  grace period. A packaging that fails leaves the old package as it was. A
  viewer mid-film when the swap lands may need to start again where a rung
  was encoded anew (its old init segment no longer matches).

Without an event bus or migration 033 a re-encode is refused, as a retry is.

### The re-encode queue

Many titles are encoded again through the queue, which the sweep sends a
few at a time so the transcoder is not flooded. `POST /api/library/reencode`
(the service account and admins) queues titles by its body, any of them
together, each title once (the compliance report has the titles it finds
processed again by name the same way, see
[Library compliance](#library-compliance)):

- `{"items": ["<itemId>", …]}`: movies and episodes, a series its episodes
  (under it or a season of it), by season and episode;
- `{"held": true}`: every title whose retire is held for its surround (see
  [The library](#the-library)), as the retire job holds it now;
- `{"all": true}`: every packaged movie and episode.

It answers `{"queued", "alreadyQueued", "skipped": [{"itemId", "reason"}]}`:
a title queued already, waiting or sent, stays as it is; one with nothing to
encode (unknown, no file, its original deleted after packaging, no movie,
episode or series) is skipped, saying why. A body that names none is 400.
`GET /api/library/reencode` answers the queue's titles by state, `{"queued",
"sent", "done", "failed"}`, of those waiting or sent the one queued first and
the one queued last (`"oldest"`, `"newest"`: `{"itemId", "state",
"enqueuedAt"}`, null when none), and `"idle"`, why the sweep sends none now
(left out when it may). Both are 503 without migration 043. The console
reads the same in `processingOverview.reencodeQueue`; an admin clears it
with `clearReencodeQueue(states:)`, by default the titles queued, done and
failed (a title sent is being encoded: clearing it frees its place in
flight, and its encoding goes on).

On each round the sweep ends the titles sent: done once their package is,
failed when their transcode or package failed with no retry left, or the
package was skipped or is not applicable, saying why (a step failed with a
retry scheduled is still being tried). Then it sends the titles queued
first, as reencodeItem sends one:

- `library.reencode.rate` (`4`): titles a pass at most; `0` sends none;
- `library.reencode.inflight` (`4`): no more while this many are sent and
  not done;
- `library.reencode.window` (empty, any time): when, `HH:MM-HH:MM` of the
  service's local time, as `23:00-07:00` across midnight; one that is no
  window sends nothing until it is.

A busy title (its transcode or package running, or waiting for its worker)
stays queued, saying why, and takes no place of the pass: the next one goes.
One whose event could not be sent stays queued too, its transcode failed
with no retry of its own; the queue sends it again on a later pass. One
whose original was retired while it waited fails. One instance sends the
queue at a time. With no sweep (`KATALOG_RETRY_INTERVAL` off) or no event
bus the queue waits.

### Library compliance

A package made before a rule came keeps what the pipeline did then: a 4K
original's video copied as it was before the transcoder's caps, a source's
surround left out before the packager's 5.1 companion. Nothing packages a
title again by itself. The compliance check holds every movie and episode
with a file or a package (an episode another's file covers is its
holder's) against the rules the workers run with now, keeps what it finds,
and an admin has what it finds processed again through the re-encode
queue.

**The rules.** The transcoder and the packager report the rules they run
with, at their start and every ten minutes, on `PUT
/api/analyze/rules/{worker}` (the service account and admins):

```json
{"version": "3f2a9c1", "rules": {
  "caps": {"movie1080Mbps": 8, "movie2160Mbps": 14, "movieMaxGiB": 15, "episode1080Mbps": 6, "episode2160Mbps": 8},
  "bucket2160": {"minHeight": 2000, "minWidth": 3200},
  "encoder": {"codec": "hevc_nvenc", "preset": "p5", "cq": 23, "maxrate1080Mbps": null, "maxrate2160Mbps": null},
  "ladder": "source:hevc", "extraLadder": "source:hevc",
  "dolbyVision": {"profile5": "reshaped", "profile7": "refused"}}}
```

```json
{"version": "3f2a9c1", "rules": {"surroundAudio": "eac3", "surroundLayout": "5.1", "surroundBitrate": "448k",
  "stereoAudio": "aac", "stereoBitrate": "192k", "imageSubtitles": ["pgs", "vobsub"]}}
```

Each value is the one the worker runs with (a cap of 0 a rule switched
off; see each worker's README, "Rules report"). The rules are kept as they
came, and a value the check reads must be of its kind (a cap a number of 0
or more, a bit rate as `448k`): anything else is 400, as is a worker that
reports no rules here (`transcoder` and `packager` do); 503 without
migration 051. It answers `{"worker", "version", "reportedAt", "changed"}`:
`changed` when what the check reads of the rules is not what the worker
reported last (another version, another preset or another extras' ladder
change nothing). A worker meets an older catalog's 404 or 405, says so
once, and goes on.

**What it finds.** A rule whose worker never reported is not checked, and
no default stands in for it: the report says so (`notEvaluated`), and so
it does of a value a report does not give, a rule switched off, a layout
that keeps no records, and records that cannot be read (how many; the log
of the pass names the first of them and why, `compliance: 1 title whose
records can't be read …`).

| Rule | A title breaks it when |
| --- | --- |
| `NOT_PACKAGED` | its original is present, it has no package, and its transcode and its package neither wait nor run ("It has no package, and none is being made."; one taken in without a package: "It was taken in without a package and plays from its original.") |
| `PACKAGING_FAILED` | it has no package, and its transcode or its package failed or was skipped, saying which, and why where the words know the worker's reason ("The transcoder failed.", its detail "ffmpeg exited 1"; "The transcoder left it as it is: Dolby Vision profile 7 needs a tone-mapping encode."; "The packager failed, and tries again by itself." while a retry is really scheduled), or its file is a disc image ("Its file is a disc image, which the transcoder doesn’t encode.") |
| `OVER_BITRATE_CAP` | its package's video is above the bit rate the transcoder allows for its kind and bucket ("The video runs at 73 Mbit/s, above the 14 Mbit/s the transcoder allows for a 2160p movie.") |
| `OVER_SIZE_CAP` | a movie's original is larger than `movieMaxGiB`, and its package copies its video as it is ("The original is larger than the 16.1 GB the transcoder allows for a movie, and the package copied its video.") |
| `NO_SURROUND` | the packager makes a surround track, and the package lacks the surround its original had, as the retire job holds an original for it ("The package has 2 audio channels, and the original has 8. The 5.1 E-AC-3 track the packager makes is missing.") |
| `IMAGE_SUBTITLES_MISSING` | the original has image subtitles of a form the packager keeps, and the package has none of them in their language ("The original’s PGS subtitles in German and Portuguese (Brazil), and its VobSub subtitles in Italian, are missing from the package. The packager keeps PGS and VobSub subtitles.") |
| `VIDEO_NOT_HEVC` | the package's video is not HEVC (`hev1`, `hvc1`) where the transcoder's rules make every package HEVC: its ladder names the source rung's codec, or its encoder is no CPU one ("The video is H.264, and the transcoder makes every video HEVC.", its detail the codec, "avc1.640028") |

The cap bucket is the transcoder's: 2160 from `minHeight` lines tall or
`minWidth` wide, of the source's tallest and widest video stream (cover art
aside), as its record lists them. The video's bit rate is measured by the
most reliable record there is: the package's top rendition's `bitrateBps`,
what the packager measured of the video it packaged (its segments' sizes
over their durations, the stream alone); for a video copied without it, the
bit rate the source record gives the stream copied; else the package's size
over its duration less its audio, which errs high. The packaged playback
row's bit rate is none of them: it is the master playlist's peak, the
video's peaks and the audio together. A video within 3% of its cap is
within it, and an encode whose transcode says it was capped at the cap
(`maxrate=` in its details) is never above it: an encode capped so is
within it on average. Whether the video was copied is the top rendition's
`encoder` (`copy`, else the encoder that made it), else the transcode's
status (not applicable: nothing was encoded). With the library's legacy
layout, which keeps no records, the bit rate, surround and image subtitles
are not checked.

Each title has `originalBytes`, `packageBytes` and `estimatedBytes`, what
its package is to be at most once it is processed again: processing that
encodes the video makes it at the transcoder's maxrate (the bit rate it
allows for the title's kind and bucket, or its `NVENC_MAXRATE_*` override)
with the packager's stereo and surround audio for one language, for the
title's duration; processing that only adds tracks keeps the package and
adds the surround track. A title that processing again can't help has none,
is no queueable one, and says why as the clause the console says after a
colon (`reworkBlocked`, "its original was deleted after packaging"): as the
queue would skip it (its original deleted after packaging or being deleted,
no file, a disc image), or as the transcoder is certain to refuse it again,
which its original's record (`sources/<id>/source.json`: its streams, its
video's `hdr.dolbyVision.profile`) or its failed step tells, and the
transcoder's report of what it can do now (`dolbyVision`): an original with
no video but cover art ("the original has no video"), Dolby Vision profile
7, which it never plans, or a base layer of compatibility 0 its step says
it refused ("Dolby Vision profile 7 needs a tone-mapping encode"), and
profile 5 only while the transcoder reports it can't reshape it (a
transcoder that doesn't report it is taken to). When the facts are in doubt
(a record never probed, a profile the transcoder reports nothing of), the
title is offered.

**The words.** Every text the check writes for the report (a finding's
`text`, `measured` and `limit`, `notEvaluated`, `queueReason`,
`reworkBlocked`, the rules panel's labels and values, the mutations' texts
and the reason of each title they skipped) is in sentence case, with the
typographic apostrophe ("can’t"), and gives the reason, never an
instruction: a skip's reason and a row's `reworkBlocked` are the clause the
console says after a colon ("1 title wasn’t queued: its file is a disc
image, which the transcoder doesn’t encode."), a toast has no full stop, and
the rest are sentences. A format's or a codec's name keeps its hyphens on
one line, no-break ones ("E‑AC‑3", "MPEG‑4").
A title's file is its original, never its source; a height has its p
("2160p"); a bit rate is two words. What a tool said is never in a text
but in a detail: a finding's `detail` (an exit code, "ffmpeg exited 1"; a
codec id, "avc1.640028"), a row's `queueDetail` (what a worker said, when
its worker last reported, an event) and a rule's `detail` (its
configuration as reported, "source:hevc"); a worker's reason the words
know is said in the reader's words ("Dolby Vision profile 7 needs a
tone-mapping encode"), and a failed step says it is tried again only while
a retry is really scheduled: its next retry set, and the sweep that sends
it running (`KATALOG_RETRY_INTERVAL`). A limit is the one its worker
reports, read when the text is written and named by the worker that
allows it ("the 14 Mbit/s the transcoder allows for a 2160p movie"): no
number is typed into a text. Sizes are written as the clients write them,
in decimal units, GB with one decimal and no trailing zero from 1 GB up and
MB below it ("58.8 GB", "820 MB"), so a file size reported as 15 GiB reads
16.1 GB; bit rates in Mbit/s, kbit/s below one; a no-break space keeps a
number on the line of its unit. A size the row's columns show is not said
again (a movie's original too large is its `originalBytes`, and the finding
has no `measured`), a bit rate read off the package's size is "about" one,
languages are named in English, with the region or the script a stream's
tag names in brackets ("German", "Portuguese (Brazil)", "Chinese
(Simplified)"), and what the queue does to a title is to process it
again. The words are part of the check's version,
which each row's hash of its rules holds, so new words check every title
again. `go test ./internal/compliance -run TestTheServerTexts` with
`COMPLIANCE_TEXTS` set to a file writes every text there with real values,
for a review of the words.

**When it looks.** The check reads the catalog's tables and, with the v2
layout, the small records of a packaged title's version and source
(`versions/<id>/package.json`, `sources/<id>/source.json`), never a media
file, eight at a time, two hundred titles a batch, in one instance at a
time (an advisory lock). What it finds is kept (migration 051), and the
report reads that. A pass in the background checks, every minute, the
titles that need it: every title when none was checked (at the first
start), every title once a night (after 03:00 of the service's time),
every title checked against rules that changed since (each row keeps the
hash of the rules it was checked against, with the layout), a title the
catalog gained, a title whose package completed or whose transcode,
package or take-in ended (packaging-complete and the step routes mark it),
a title whose re-encode in the queue is done or failed, and the titles of
a kind an admin asks for (`refreshCompliance`). A title that is no title
to check any more (deleted, covered by another's file, with neither file
nor package) leaves the report.

**GraphQL** (admins):

- `pipelineRules` — each worker's last report: its version, when, and
  each rule a row of the rules panel (`{key, label, value, detail}`):
  "Video bit rate, 2160p movies", "at most 14 Mbit/s" (a limit of 0 "any");
  "File size, movies", "at most 16.1 GB (15 GiB)"; "Renditions", the
  ladder in words, "one, at the original’s size, in HEVC", its spec the
  detail; "Surround track", "5.1 E-AC-3, 448 kbit/s"; "Stereo track",
  "AAC, 192 kbit/s" (the codec where the packager reports it); the
  configuration as reported the detail where the value says it in words.
- `libraryCompliance(kind, rules, search, first, after)` — a page of the
  titles that break a rule (any of `rules` when given), of `kind`, whose
  title or series' title holds `search`, the largest package first, then
  the largest original: `first` (100, at most 500) after the cursor
  `after` (the last page's `nextCursor`). Each row has its findings
  (`{rule, text, measured, limit, detail}`), sizes, its place in the
  re-encode queue (`queued`, `sent`, `done` or `failed`, why in words,
  `queueReason`, and that reason's technical part, `queueDetail`) and when
  it was checked. `total` counts the titles the whole filter (`kind`, `rules`,
  `search`) matches, and `matching` sums them up, every one of them and not
  only the page's (`{titles, originalBytes, packageBytes,
  estimatedBytes}`); `summary`, for every rule, those of `kind` and `search`
  that break it, whatever `rules` asks, with the size of their packages and
  their estimate; `evaluatedAt` is the oldest check of those (null when none
  was checked), `evaluating` whether a pass runs, `pending` how many titles
  wait for the next one; `queueable` sums up the titles of `matching` that
  a request to process them again queues, what `queueReworkMatching`
  queues: none waiting or sent in the re-encode queue (which keeps such a
  title as it is; one done or failed there is queued again), and none that
  processing again can't help (`reworkBlocked`, which the queue skips);
  `queue` counts the re-encode queue, all its titles, by state (`{queued,
  processing, processed, failed}`: a title sent is processing, one done
  processed).
- `queueRework(items)`, `queueReworkMatching(kind, rules, search)` — the
  titles named, or those the filter matches that it queues (`queueable`,
  exactly: what the console offers is what is queued), into the re-encode
  queue (as `POST /api/library/reencode` with `items`: a series is its
  episodes), which sends them at its pace: `{queued, alreadyQueued,
  skipped, skippedTitles, queuedText, alreadyText, notQueuedText,
  message}`, a title waiting or sent already counted in `alreadyQueued`,
  one the queue does not take (not in the catalog, no movie, episode or
  series, a series without episodes, no file, its original deleted after
  packaging, a disc image) in `skipped` and, each of them, in
  `skippedTitles` (`{itemId, title, reason}`: the title as a list names it,
  "A Film (1991)" or "A Show · S01E02 · Its Title", an id the catalog
  doesn't hold as it is, and the reason as the queue tells it, in the
  reader's words, "its original was deleted after packaging"). The console
  says it in three texts, each null when it has nothing to say: a toast of
  the titles queued, without a full stop (`queuedText`, "12 titles queued
  to be processed again"), a plain one of those already in the queue
  (`alreadyText`, "3 titles already in the queue"), and a banner line of
  those not queued, with why when they share one ("2 titles weren’t
  queued: their originals were deleted after packaging.") and the reasons
  composed when they don't ("4 titles weren’t queued: the originals of 2
  were deleted after packaging, 1 has no file and 1 is a disc image."), or
  of nothing queued at all ("Nothing was queued: no title was selected.";
  by a filter, "Nothing was queued: no title fits the filter anymore.")
  (`notQueuedText`); `message` is the three in one text.
- `refreshCompliance(kind)` — the titles of `kind` (every kind when
  omitted) checked again by the next pass, at once.

The check never queues a title: only these mutations do.

### Lost scans

A scan runs in the process that started it, which writes its end into its
scan job. A process that stops while it scans never does, so the job is
failed for it, and `errorMessage` says how:

- **At startup**, before it starts a scan of its own, the service fails every
  job still running that an earlier process of its host ran, and every one
  that names no runner (older than migration 034): `interrupted: the service
  restarted while the scan ran`. A job another host runs is left alone; its
  process may be alive.
- **The reaper.** A scan gives its job a word while it walks, every 30
  seconds at most (a third of the timeout when that is shorter). A job
  running without a word for longer than the scan's timeout (the `scan`
  step's, 15m, `KATALOG_STEP_TIMEOUTS`) is failed by the sweep: `timed out: no
  word from its scanner for 15m (the scan's timeout)`. A scan is not retried
  by itself, so this needs no event bus; `KATALOG_RETRY_INTERVAL=off` turns
  it off with the rest of the sweep.

A scan that ends after all, late, writes its end over the failure.

A title's source asset keeps what the workers probed it as: the codec and
resolution the transcoder reports in its step's details, the duration the
packager gives in its manifest, the bit rate from size and duration; and the
size its source records once the original is recorded (packaging-complete,
the adopt), which the scanner takes from the file when it measures its quick
hash. A probe that says nothing of a value leaves the value the asset had.
`backfillSourceProbes` fills what the sources probed before lack, from the
catalog's records; `backfillSourceSizes` gives every source asset whose
original is recorded and there the size its source records, where it has
another or none, and answers how many.

## Ingest

`POST /api/ingest` (an admin, the service account, an addon) takes a file
on disk in as a title, as the scanner takes in one it finds: the item, the
file as its primary asset (with the v2 layout from the arrivals alone, with
its source), its scan done and its `discovered` event sent, so that the
pipeline runs. It knows nothing of how the file got there.

```json
{"path": "/var/lib/katalog/media/Big Buck Bunny (2008).mkv", "type": "movie",
 "title": "Big Buck Bunny", "year": 2008, "identity": {"tmdb": "10378", "imdb": "tt1254207"}}
```

- **`path`, `type` and `title`** are required; `year`, `description`,
  `sortTitle`, `sizeBytes` and `metadataLocked` (enrichment leaves the
  metadata given alone) go with the title. A disc image is refused (see
  [Disc images](#disc-images)).
- **An episode names its series one way**, with its `seasonNumber` and
  `episodeNumber`: by its catalog id (`parentId`, a series or a season of
  one) or by its identity (`series`). One with neither, or with both, is
  refused (400): an episode with no series would play as an orphan.
- **An identity** is an object of ids by reference database, one or more of
  `tmdb`, `tvdb` and `imdb`, each a string of its form (digits; `tt` and
  digits); any other key or form is refused (400). `series` is the series'
  of an episode, `identity` the title's own: a movie's, or an episode's,
  whose TMDB id is kept where the catalog keeps an episode's, under
  `tmdb-episode`.
- **Idempotent on the path:** the same file again is its title, `created:
  false`, and nothing happens.

The answer is the catalog's ids. `seriesId` answers an episode handed over
by its series' identity, `covered` one whose body says where its file ends:

```json
{"itemId": "8c1cee73-…", "created": true, "seriesId": "f3ab0c45-…", "covered": ["43afc194-…"]}
```

### By identity

An addon holds no catalog ids: it hands a title over by the identity it has
outside any one library, and the catalog, its sole writer, finds or makes
the rows:

```json
{"path": "/var/lib/katalog/media/Example Show/Example Show - S04E07E08.mkv", "type": "episode",
 "title": "Example Show", "year": 2020, "series": {"tmdb": "97546", "tvdb": "371572"},
 "seasonNumber": 4, "episodeNumber": 7, "episodeEnd": 8}
```

- **The series** is found by its ids (`com_nalet_katalog_itemexternalids`):
  by the first of `tmdb`, `tvdb` and `imdb` the body names that finds a
  series, and of a show the library holds twice the series made first, the
  one katalog-api's holdings answer for. A series found keeps its ids as
  they are. When none holds any of them, the series is made as the scanner
  makes one it finds: the body's title and year, the title in lower case as
  its sort title and the name the scanner finds it by (`scantitle`), a row
  for each of its ids, its scan done and its `discovered` event sent, so
  that enrichment matches it by its TMDB id and, with the v2 layout, records
  it (`item.json`, with its ids) before its first episode.
- **The episode** is made under the series, its numbers the body's.
- **One file of several episodes in a row** says its last in `episodeEnd`,
  ten at most from `episodeNumber`. Its first episode holds the file, and
  each other is linked to it as the scanner links the episodes a file's name
  numbers (see [One file, several episodes](#one-file-several-episodes)):
  the series' episode of that number, or one made for it when the series has
  none, its scan done and its `discovered` event sent; one with a file of its
  own is left alone (its own file wins), and so is one another's file covers
  already. `covered` lists the episodes the file covers besides its first,
  in episode order. An episode handed over by its `parentId` takes
  `episodeEnd` too, and covers the episodes of its parent's series.
- **A movie** gets a row for each of its ids: it arrives matched, not found
  by its title.
- **Refused (409)**, with `{"error", "itemId"}` naming the item that holds
  it, and nothing written:
  - an identity that contradicts what the catalog holds of the path: a title
    there of another type; an episode of other numbers, or of another series
    than the identity's; a movie other than the identity's; an episode's own
    id of a source another. A series or a movie is the identity's when the
    first source that finds one finds it, or, when the identity finds none,
    when it holds no other id of a source the identity names. A title is
    never relinked to an identity;
  - a title the library holds already as another item: an episode of that
    series, season and number with a file of its own (a playback row but a
    trailer's), or one another's file covers; a movie the identity finds,
    with a file. Replacing a held title's file is another decision
    (`replaceSource`), never an ingest's. An episode of those numbers with no
    file holds none: the file becomes an episode of its own, as a scanned
    one does.

Ingests are taken in one at a time, each in one transaction, and one onto a
file at a time, as `replaceSource` takes one: a series is made once, however
many of its episodes arrive together. Re-ingesting a path with the identity
it was taken in with answers it again, `created: false`, with its series and
the episodes its file covers.

## Attached subtitles

A title plays the subtitles its package made of its source (a title taken
in without one, the subtitle streams embedded in its original) and the
subtitle files beside its source. A subtitle can also be **attached** to it
once its version is in the library — its package completed, or its original
taken in without one — made from its audio or from another of its
subtitles, or found elsewhere, by an admin, the service account or an addon,
and every player lists and plays it as any other: nothing is encoded or
packaged again.

`POST /api/library/items/{id}/subtitles` attaches one:

```json
{"language": "de", "label": null, "forced": false, "sdh": false, "origin": "provider",
 "format": "srt", "content": "1\n00:00:01,000 --> 00:00:02,500\nGuten Tag\n"}
```

- **`language`, `origin` and `content` are required.** `language` is a BCP 47
  tag (`de`, `en`, `zh-Hans`, `pt-BR`), kept as people's software spells one:
  the ISO 639-1 code where the language has one (`deu` and `ger` are `de`),
  its script and region after it; `und`, `zxx`, `mul` and `mis` name no
  language a subtitle is in. `origin` says how it came to be: `source` (it
  came with the title's file and was not taken in with it), `provider`
  (found elsewhere), `transcribed` (from the title's audio), `translated`
  (from another of its subtitles) or `converted` (another script of one).
- **`content`** is WebVTT or SubRip text in UTF-8, `format` (`webvtt`, `srt`)
  saying which; without it the text does. It is kept as WebVTT: SubRip is
  converted (its numbers go, its times are WebVTT's, the styling of other
  formats it carries goes), a WebVTT file keeps its blocks with its times
  spelled as WebVTT spells them. Text that is not UTF-8, or holds U+FFFD (what
  is left of text that was not), one with no cue, and a cue that ends before
  it starts are refused (422), saying why; a body of more than 8 MiB is 413.
- **`label`** is what a player says of it besides its language. Without
  one, it is `SDH` for one for deaf and hard-of-hearing viewers, `AI` for one
  a model made (`transcribed`, `translated`), both `SDH · AI`, else none: a
  player names it "German", "German · SDH", "German · AI". `forced` is a
  subtitle a player shows by itself (signs, a line in another language).

The file is written into the `subs/` folder of the version it follows — the
title's complete version, whose package plays, else the version its file is
taken in to (its original and no package, which the title plays from) — as
`attached-<sha256>.<language>[.forced][.sdh].vtt`, `<sha256>` its bytes'
hash, and its row is the title's subtitle (`com_nalet_katalog_subtitleassets`),
never the default, with its origin, the version it follows, its hash and the
client of the caller's token that attached it (`attachedby`). The answer is
`201 {"itemId", "created": true, "subtitle": {…}}`; the same file of the same
language and flags attached again is the subtitle there is, `200` with
`created: false`. A title with no such version — neither packaged nor taken
in (its file not in the library yet, or a new one since), packaged before the
library, a series — is `409`, saying so; an episode another's file covers
answers for its holder, whose file it is (`itemId`).

Players reach it as they reach every subtitle of a title: katalog-api lists
the title's rows (`include=subtitles`), chino-api gives each its URL, and
chino-stream serves the file from the library. The package's records stay as
they were written: `package.json` and `checksums.sha256` do not list an
attached subtitle, its name being its check instead — a version's chain
holds with it, and a full verification holds its bytes to its name (see
[The library](#the-library)). When a package is added to a version taken in,
packaging-complete keeps the version, its id and its folder, and the
subtitles attached to it stay with it; a package added and refused goes out
of the record without them.

`GET /api/library/items/{id}/subtitles` lists the title's subtitles, its own
first (`origin: null`, `attached: false`) and then those attached to it,
each with its language, label, format, forced, SDH, origin, the version it
follows and who attached it when; and the version a subtitle is attached to
now (`versionId`, `attachable`: complete, or taken in), or why there is none
(`reason`). With `?fingerprint=true` it reads 128 KiB of the original and
says what subtitle sources match a file by — `original: {sizeBytes, sum64,
labels}`: its size, the 16 hex digits of its size plus its first and last
64 KiB summed as little-endian 64-bit words, and what its name claimed (its
source record's `labels`) — or `original: null` and why (`originalNote`:
deleted after packaging, unreadable).

A title **taken in** — no complete version: it plays the original its
version's folder holds, whose embedded subtitle streams chino-stream
extracts as a player asks for one — has those streams for its own too,
listed after its rows of its own and before those attached. The catalog
keeps no row of them: the seam reads them from the original's source record
(`sources/<sourceId>/source.json`, its `streams`), one entry each, in the
file's order:

```json
{"id": "source-0b6c…-4", "language": "zh-Hans", "label": "Chinese (Simplified)", "format": "subrip",
 "forced": false, "sdh": false, "origin": null, "attached": false,
 "embedded": true, "stream": 4, "form": "text", "purpose": "dialogue"}
```

- **`id`** is `source-<sourceId>-<index>`, `stream` the stream's index in the
  file: no row's, and the same as long as the record is, which is written
  once. `embedded`, `stream`, `form` and `purpose` are an embedded stream's
  alone; a row's entry is as it was.
- **`language`** is the stream's language as a tag (else the code its file
  carried; none when that names no language either), with the script its
  `variant` names for Chinese (`Simplified`: `zh-Hans`, `Traditional`:
  `zh-Hant`) and the region it names cleanly (`Latin American`: `419`,
  `Brazilian`: `BR`, `Canadian`: `CA`), where the tag has none of its own.
  `label` is the stream's title, `format` its codec (`subrip`, `ass`,
  `mov_text`, …) and `form` `text`. A stream of pictures (PGS, VobSub, DVB:
  `form` `image` in the record, else by its codec) is left out: an original's
  subtitle streams are served as text, which a picture is not, so a viewer
  has no such subtitle, and a client counting the title's languages must not
  count it.
- **`forced`** is the file's flag, or the record's `purpose` `forced` or
  `signs-songs`; **`sdh`** the file's hearing-impaired flag, or the purpose
  `sdh`. A stream of a commentary or of song lyrics alone (its purpose, or
  the file's flag) is left out: it is no subtitle of the dialogue.

A record that is not there, cannot be read or lists none of the original's
streams leaves the list at the title's rows, and `embeddedNote` says why.
Once a package is added to the version, the package's subtitles are the
title's own, as every complete version's are, and the streams are listed no
more.

`GET /api/library/subtitles/languages` names the languages every title is
to have a subtitle in, as tags, in the order the setting names them:
`{"languages": ["en", "de", "zh"], "from": "packager.language_whitelist"}` —
`subtitles.languages` when it is set, else the packager's language
whitelist, the languages a player shows; none when neither is. Nothing in
it is secret, and any signed-in caller reads it, a viewer's client too; the
title's routes of the seam are an admin's, the service account's and an
addon's.

`DELETE /api/library/items/{id}/subtitles/{subtitleId}` removes a subtitle
attached to the title: one the caller attached (its token's client), or, for
an admin, any; its row and its file go, and the version's `subs/` with the
last file in it. It is `204`, also for a subtitle the title has not (removed
already); one of the title's own is `409` (a stream embedded in its original
too), one another client attached `403`.

**A new version** of the title made of the same sources carries the attached
subtitles over: their files are copied into its `subs/` and their rows follow
it. A version of another file — the title given a new file — does not, as
its timing may be another's: their rows go with the package's.

## Item events

The catalog's item events (`<KAFKA_TOPIC_PREFIX>catalog.item.*`, keyed by
the item) carry its id, its type, the step they unblock, a status and where
they come from. `discovered` (the scanner's, the ingest's and a retry's),
`packaged` (packaging-complete's) and `removed` (a removal's, of the item as
it was) also name its identities as the catalog holds them, so that a
consumer reconciles what it knows of a title without a lookup:

```json
{"eventId": "9f2b…", "itemId": "8c1cee73-…", "type": "episode", "step": "tmdb",
 "occurredAt": "2026-10-15T08:00:00Z", "source": "ingest",
 "externalIds": {"tmdb-episode": "937631"}, "seriesExternalIds": {"tmdb": "97546", "tvdb": "371572"},
 "seasonNumber": 4, "episodeNumber": 7}
```

- `externalIds` are the item's own, each source to its id as the catalog
  keeps them: a movie's and a series' TMDB id under `tmdb`, an episode's
  under `tmdb-episode`, IMDb's and TheTVDB's under `imdb` and `tvdb`;
- of an episode, `seriesExternalIds` are its series' (its parent's, or its
  season's series'), and `seasonNumber` and `episodeNumber` its numbers.

Each is left out when the catalog knows none, so the event of a title not
matched yet is the envelope it always was; identities that cannot be read
are left out too, said in the log, and the event goes.

## Extras

An extra is bonus material of a movie or a series: a trailer or a teaser
that is a file of its own, a featurette, a making-of, a deleted scene, an
interview, a gag reel, a short, anything else (`other`). An episode has
none; its series has, and a series' extra may name a season. An extra is
no item and no playback asset of one: it is packaged on a chain of its own,
from its own file, into `packages/extras/<aa>/<extraId>/` of the package
store, never into its title's package, and a viewer is served it beside its
title (katalog-api lists the extras that play under `include=extras`).

### Taking an extra in

`POST /api/extras` (an admin, the service account, an addon) and GraphQL
`addExtra` (an admin) take a file in as an extra:

```json
{"itemPath": "/var/lib/katalog/media/BigBuckBunny_320x180.mp4",
 "path": "/var/lib/katalog/extras/big-buck-bunny/trailer.mov",
 "kind": "trailer", "title": "Trailer", "language": "en"}
```

- **The title** is named one way: `itemId`, `itemPath` (the path of its
  file, its primary asset, which a demo's reset keeps where it changes ids)
  or `tmdbId` with `itemType` (`movie` or `series`). It is a movie or a
  series.
- **`seasonNumber`** is only a series', and only of a season it has
  episodes of.
- **`path`** is an absolute path of an existing video file under the media
  root, `LIBRARY_ROOT` or `EXTRAS_ROOT`, never under the package store,
  never a link that leads out of those roots, and no title's own file.
- **`kind`** is one of `featurette`, `behind-the-scenes`, `making-of`,
  `deleted-scene`, `interview`, `trailer`, `teaser`, `gag-reel`, `short`,
  `other`; `title` is the kind's name when omitted ("Trailer"); `language`
  is BCP 47 or an ISO 639-2 code.
- **The file's size and quick hash** are stored with it: `sha256:` and the
  SHA-256 of its first 64 KiB, its last 64 KiB and its size as a big-endian
  uint64, the library's `qh1`, which tells the file after a move.
- **Idempotent on the file:** the same file again for the same title is its
  extra (`created: false`); for another title it is refused. A removed
  extra's file may be taken in again, as a new extra.

The answer is 201 for an extra taken in, 200 for the one the file was
already:

```json
{"extraId": "1b5c2a8e-…", "itemId": "ea886f9b-…", "created": true, "kind": "trailer",
 "title": "Trailer", "state": "queued"}
```

A refusal says why, with its status: 400 `EXTRA_REFUSED`, 404 `NOT_FOUND`
for a title there is not (a Job may wait for the scan that makes it), 409
`EXTRA_CONFLICT` with the extra in the way, 503 on a catalog without
migration 039:

```json
{"error": "/var/lib/katalog/extras/…/trailer.mov is extra 1b5c2a8e-… of item ea886f9b-… already",
 "code": "EXTRA_CONFLICT", "extraId": "1b5c2a8e-…", "itemId": "ea886f9b-…"}
```

GraphQL's `addExtra` takes the same arguments and answers
`{created, extra}`; a refusal is an error whose `extensions.code` is the
code. An item's `extras` lists them as a viewer sees them (by `sortOrder`,
none last, then as they were taken in; `removed: true` the removed ones
too), each with its state, its package and whether it plays (`playable`).

### Its packaging

An extra waits to be sent (`pending`), and its trigger goes at once when
the service has an event bus:

```json
{"eventId": "9f2b…", "extraId": "1b5c2a8e-…", "parentId": "ea886f9b-…", "type": "extra",
 "kind": "trailer", "step": "transcode", "status": "queued", "occurredAt": "2026-10-06T08:00:00Z",
 "source": "api"}
```

on `<KAFKA_TOPIC_PREFIX>catalog.extra.queued`, keyed by the extraId. It
names no `itemId`: an item worker pointed at the topic by mistake skips it.
A trigger after a failed run is a retry (`"status": "retry"`, `"source":
"retry"`). The transcoder encodes the extra with its extras ladder and
announces it on `catalog.extra.transcoded`; the packager packages it and
reports its package. The workers' protocol:

- `GET /api/analyze/extras/{id}` is the extra's record, 404 for one there
  is not and for a removed one (a worker skips it):

  ```json
  {"id": "1b5c2a8e-…", "type": "extra", "parentId": "ea886f9b-…", "parentType": "movie",
   "parentTitle": "Big Buck Bunny", "kind": "trailer", "title": "Trailer", "language": "en",
   "seasonNumber": null, "path": "/var/lib/katalog/extras/big-buck-bunny/trailer.mov", "state": "queued"}
  ```

- `PUT /api/analyze/extras/{id}/steps/{transcode|package}` takes the body
  an item's step takes, `{"status": "in_progress|done|not_applicable|failed",
  "error": "…", "details": "…"}`, and answers `{extraId, step, status,
  state}`: the transcode's start is `transcoding`, its end (`done`, or
  `not_applicable`: the packager packages the source as it is) `transcoded`,
  the package's start `packaging`; the package's end says nothing, as
  packaging-complete makes the extra ready. A failed run (a package's too
  that fails before it says it started) counts a failure: `pending`, sent
  again a backoff later, while the retry policy has attempts left, else
  `failed`. A report of a run the extra is past (a transcode's end after
  its package began, a failure after `ready`) changes nothing. 404 for an
  extra there is not or a removed one, 409 for one whose file is missing,
  400 for a step or a status an extra does not have.
- `POST /api/extras/{id}/packaging-complete` takes the package's manifest
  (an item's with `"type": "extra"`, the extraId as `itemId`, `parentId` and
  `extraKind`; no trickplay) and answers `{"extraId", "itemId", "packaged":
  true, "durationMs"}`. The extra is `ready`, its failures over, and keeps
  its package's folder (`packages/extras/<aa>/<extraId>`), how long it
  plays, its top rendition's codec and size, the highest `BANDWIDTH` its
  master playlist names (the top rendition's bit rate when the playlist
  cannot be read) and its size. It is announced on `catalog.extra.packaged`
  in the shape of an item event of its title, never on
  `catalog.item.packaged`, which says the title itself became watchable:

  ```json
  {"eventId": "…", "itemId": "ea886f9b-…", "type": "movie", "step": "extra", "status": "done",
   "occurredAt": "…", "source": "katalog-manager", "extraId": "1b5c2a8e-…", "kind": "trailer"}
  ```

  A manifest of another package, or one without video, is refused (400),
  and the packager fails its step. A missing extra's package is recorded
  and kept for when the file is back, not announced.

An extra plays once it is packaged, until it is removed, unless it is
hidden or its file is missing. A package made again plays the old one
until the packager swaps the new one in.

### The sweep, re-encoding, removal

Every `KATALOG_RETRY_INTERVAL` the sweep, by the `KATALOG_RETRY_*` policy:

- **reaps** an extra stuck in its packaging, counting a failed run: one
  queued with no transcoder started within 24h, or transcoding with its
  transcoder silent for 2h, or packaging with its packager silent for 2h,
  goes back to `pending` before anything is sent again, so its chain runs
  again from the transcode; one transcoded with no packager started within
  24h stays transcoded, and the transcoder is sent a trigger that is no
  retry, which has it announce the transcode again;
- **sends** the triggers due, claiming each extra once (`FOR UPDATE SKIP
  LOCKED`), so two instances never send one twice;
- **deletes** what the package store holds of an extra removed a day ago:
  its package, the ones it replaced and kept for their grace, and the
  transcoder's handoff left in `_inbox/extra-<id>/`.

Without an event bus extras wait, pending, and only the removed packages
are deleted. `packageExtra(id)` and `packageExtras(itemId)` package an
extra, or every extra of a title, again with the pipeline's current
settings: it leaves `ready` for `pending`, its failures cleared, and its
trigger goes; one in its packaging within its timeout is left alone, and so
is one whose file is missing until the file is back. `reencodeItem` leaves
a title's extras alone, and so does `identify`. `removeExtra(id, reason)`
removes an extra: it stays, removed (who, when, why), plays no more, and
its package is deleted a day later; one recorded in the library is
refused, as its record, written before the database, is retired by an
`extra-removed` event of its own. Deleting a title deletes its extras;
with `deleteFiles` their files under the media root or `EXTRAS_ROOT` go too
(a library record's is written once and stays), with `deletePackages`
their packages and handoffs.

### The scanner's convention

Behind the setting `extras.scan` (`true`, `on`, `yes` or `1`; off by
default, so a library does not start packaging its bonus material by
itself) the scanner takes a file in as an extra of a title when:

- its name is the name of the title's file in the same folder, then a kind
  and a label maybe: `<stem><sep><kind>[<sep><label>].<ext>`, the
  separators `-`, `.`, `_` and space, the longest stem winning
  (`Sintel-trailer.mkv`, `Sintel - Behind the Scenes - Music.mkv` beside
  `Sintel.mkv`); the kinds `trailer`, `teaser`, `featurette`,
  `behind the scenes`/`behindthescenes`, `making of`/`makingof`,
  `deleted`/`deleted scene`, `interview`, `gag reel`/`bloopers`, `short`,
  `other`/`extra`;
- it lies in a folder of extras (`trailers/`, `teasers/`, `featurettes/`,
  `behind the scenes/`, `making of/`, `deleted scenes/`, `interviews/`,
  `bloopers/`, `extras/`) whose parent holds the one title's file;
- its name is a kind alone (`trailer.mkv`, `teaser-2.mp4`; not a word that
  may be a film's name, as `short` or `other`), or names a trailer as the
  scanner always took one (`… - trailer.mkv`), beside the one title's file;
- it lies in a show's folder of extras: `series/<Show>/trailers/` is the
  series', `series/<Show>/Season 01/extras/` its first season's, of the one
  series the episodes under `series/<Show>/` belong to.

Anything ambiguous (a flat folder of many titles, two series) is skipped
and said in the log; such a file is no item either. A file found at a new
path with the size and quick hash of an extra of its title whose file is
gone is that extra moved, and keeps its id and its package. After a walk
that went through, an extra the scanner took in whose file is gone is
`missing` and hidden, until it is back. A trailer once scanned as a title of
its own becomes an extra of its film, and the title left without a file is
removed, in the deletion log. With the setting off, a file the scanner
always took for a trailer is skipped and no extra is taken in. Whatever the
setting, the scanner writes no trailer asset rows (`kind = 'trailer'`) any
more, and deletes the one a file it meets has.

## One file, several episodes

A file may hold more than one episode, a double-length finale, and it is
never split: there is no clean boundary to cut at. It is packaged once and
serves every episode it holds (migration 045, the library's published v2
model).

- **Its name** numbers them: `S05E15-E16`, `S05E15E16`, `S05E15-16`, also
  `.E16`, `_E16` or ` E16` after the first, and more than two
  (`S05E15-E17`, `S05E15E16E17`), letter case aside; the file covers every
  episode from the first to the last, ten at most. A number after a dash
  that a digit or a `p` follows is none (`S05E15-720p` is the fifteenth
  episode in 720p), nor is one that does not come after the one before it:
  the name then numbers the episodes before it. The token stands apart from
  the words around it, as a single episode's did.
- **Its holder** is the first episode it covers, the lowest number: the file
  is the holder's as any episode's file is its episode's, and its source,
  its versions, its package, its pipeline run and its retire are the
  holder's, in the holder's folder. The holder runs the pipeline as any
  episode does.
- **The other episodes it covers**, of the holder's series and season, are
  linked to it once the scan has walked every file (an episode's own file
  may be walked after the file that covers it): the catalog's episode of
  that number, or one the scan makes as it makes any episode (its scan step
  done, its discovered event sent, so that the enricher reads its title,
  overview and images). Each keeps an item of its own and names its holder
  (`coveredby`); it has no file of its own and runs nothing, its steps of a
  file (`tidb` to `package`) not applicable, saying whose file covers it
  ("covered by the file of episode <holder>, which runs the pipeline for
  every episode it holds"). An episode of that number with a file of its own
  is left alone, its own file wins, and one another file covers already
  stays that file's; the scan's report says so. An episode the file covered
  that its name numbers no more (the holder given another file) is unlinked,
  and so has no file. A file the scanner took in before it read its name's
  numbers (`S01E01E02` was none to it) gets them.
- **Acting on a covered episode acts on its holder**: `reencodeItem`, the
  re-encode queue, `packageItem` (`POST /api/items/{id}/package`), the
  take-in (`POST /api/items/{id}/takein`) and `replaceSource` by its id each
  act on the holder, the answer the holder's and saying so; the analyzer's
  reset of a series leaves a covered episode alone.
- **The library's records**: the worker record of the holder names the
  episodes its file covers in its `library.source` (`covers`: the holder
  first, then the others in episode order), which the packager writes in its
  source record; a file of one episode names none. The holder's
  `metadata.json` numbers it up to the last episode its file covers
  (`library.numbering.aired.episodeEnd`), and a covered episode's plays the
  holder's current version (`library.primaryVersionId`) and names it
  (`library.coveredBy`); a version taken marks the episodes the holder's file
  covers changed, so that their projections follow. The migration's adopt
  links the episodes its plan's sources cover (`db.sources[].covers`, the
  item first) in the item's transaction, refusing a plan whose covers the
  catalog cannot link, and a revert puts the links back as they were; the
  catalog's export names each item's `coveredBy`.
- **Removing** a covered episode touches no file (it has none), and its
  holder's file covers one episode fewer. Removing the holder keeps the
  episodes its file covered, unlinked, with no file now, and the answer
  names them (`unlinked`); removing their series takes them all.

The console reads an episode's `coveredBy` (the episode whose file covers
it) and `covers` (the others its file covers, in episode order).

## Disc images

A disc image (an `.iso` or `.img` file) is no file the library holds: it is
converted to a single file first, or stays out. The scanner passes over one,
making no title and no asset, and the scan job's `report` (GraphQL
`scanJobs { report { kind path itemId reason } }`, the first 1000 entries;
the log says every one) lists it as `unsupported`, "disc image: convert it to
a single file". A title whose file is one already (taken in before) is named
in the report, and runs nothing of it: its steps that read its file fail for
good with that reason (its transcode and package made failed when it has
none), which the processing overview lists among the failed steps; the
enrichment sends it no further, and it is not encoded again, queued, packaged
(409), taken in nor retried. `POST /api/ingest` refuses one (400), and
`replaceSource` gives no title one. Such a title is given a single file with
`replaceSource`, or removed.

The report lists, as `not-linked`, the episodes a file's name covers that
the scan left alone (see
[One file, several episodes](#one-file-several-episodes)), each with the
file and the episode.

## The library

With the setting `library.layout` at `v2` the share's root is the library:
`movies/`, `series/` and `people/` hold its record, written once, and
`.work/` everything else (the arrivals in `.work/incoming`, the extras taken
in by the API in `.work/extras`, the files handed to replaceSource in
`.work/replace`, the workers' handoffs, the trash, the migration's runs).
katalog-manager decides every path in it, by the path rules of
`internal/library/paths.go`, and hands the workers theirs in their worker
records (`library`, contract 1). With `legacy`, the default, nothing of it
is read or written: the service works as before.

The library's settings are read on every use, as `extras.scan` is:

- `library.layout`: `legacy` (default) or `v2`. With `v2` the scanner walks
  the arrivals and knows an original it has seen by its size and quick hash
  (a copy of one is skipped, a moved one followed, a deleted one named);
  ingest, replaceSource and addExtra take files from the arrivals alone; a
  title is recorded (`item.json`) once it is enriched; the transcode's end
  makes the version its package builds, and packaging-complete takes the v2
  payload of a version or an extra (a refused one's folder goes to
  `.work/legacy/<day>/refused/`); the projector writes `metadata.json` and
  `person.json` from the catalog. The layout does not change while a
  title's or an extra's transcode waits for its packager (`LAYOUT_BUSY`).
- `library.originals`: `keep` (default) or `delete-after-package`: the
  retire job's policy.
- `library.retire.delay` (`10m`), `library.retire.rate` (`30` a minute),
  `library.verify` (`full` or `chain`), `library.verify.maxAge` (`30d`),
  `library.superseded.grace` (`24h`), `library.trash.grace` (`24h`; `0`
  deletes a retired original at once). A duration is a Go duration or days
  (`30d`).
- `library.reencode.rate`, `library.reencode.inflight`,
  `library.reencode.window`: how the sweep sends the re-encode queue (see
  [The re-encode queue](#the-re-encode-queue)), in either layout.

A title's original lives in the library from its first version on, in that
version's folder: `versions/<versionId>/original.<ext>`, named by its
extension alone (lower-cased when it is 1 to 8 letters and digits, else
`bin`; `original-<n>.<ext>` for a version in parts), as the schemas' record
logic names it: nothing in the library says where a file came from. Until
then it waits where it arrived, in `.work/incoming`. The catalog keeps the
name it arrived under, and its place among the arrivals (its source's
`libraryPath`), only until its source's record is written, as
packaging-complete, a take-in and the adoption leave it: from then on the
source's name is the library's, and its original is known by where it lies.
katalog-manager decides
what the packager's run does with a title's source, in the worker record's
`library.build`: `mode` is `establish` while the source has no version (the
package and the original, renamed into the version's folder with it, under
`originalName`), `takein` while its step `takein` waits or runs (the original
alone, no package), `add` while the source's version holds its original alone
(the package is added to that folder, `versionDir`), and `repackage` once a
version of it is packaged (a new version, its package alone, the original
staying where it lies: an older version's folder, or where it arrived; the
worker record's `path` is where it lies). packaging-complete takes the
payload's `original: {path, name}` with the version, in one transaction: the
source and the asset of the title's file point at it there; a path outside
the version's folder, a name the library does not give one, one
`version.json` does not name, or a file that is not the source's is refused
(422). `takenIn: true` records a version with no package, `taken` (migration
044): the title plays from its original as one without a package does. A
refused folder goes out of the record only once the original a run renamed
into it is back where the catalog says it lies, and a version taken in keeps
its folder, its original and the subtitles attached to it, the package added
to it going out.

A run whose handover was lost (its version's folder placed, the original
renamed into it, nothing recorded) is taken again the same: the version
being built is kept until it is recorded, so the run taken again is handed
the same version, folder, mode and name for the original, and reports the
folder it finds there again with the same payload. Its workers are handed
the original where it lies (found in that unrecorded version's folder by the
source's size and quick hash), never the arrival it left; that version is
never made another file's (`replaceSource` waits, `SOURCE_CONFLICT`), no
title is taken in from where its original is not, and a removal that keeps
the files puts the original back where the catalog has it.

A title is taken in when it gets no package now and its source has no
version: its transcode was refused (the transcoder keeps the original of a
picture no package would show as it is, its error ending "kept the
original"; that transcode is retried no more), or its transcode or its
package failed with no attempt left. The service sends the packager its step
`takein` (`transcoded`, step `takein`) at once when the worker reports the
failure, and the sweep sends what that missed; the step is counted in the
processing overview, retried and reaped as any other, and holds the layout
(`LAYOUT_BUSY`). A take-in reported failed whose version was taken all the
same (the answer to its handover lost) is done, not sent again. `POST /api/items/{id}/takein` takes a title in by hand. A
package is added to the version later by `reencodeItem` or `packageItem`.

The retire job runs once a minute in the sweep, in one instance at a time.
With `delete-after-package` it deletes a title's original once a complete
version of it is as old as the delay, the title is not held, and every step
that reads the original is over: claimed (the step `retire` runs), the
original checked against its size and quick hash, the version verified (in
full, unless the migration's stage did that within `library.verify.maxAge`;
an [attached subtitle](#attached-subtitles) in its `subs/` is no file of the
package the chain covers, and in full its bytes are held to its name),
its `original-deleted` event recorded with what the package does not carry
of it, the original moved to `.work/trash/<day>/<sourceId>/` from its
version's folder (or from where it arrived, one from before), the files
that came with it from where it arrived, then the catalog says so: the
playback row is an original's, which points at its record, and the
sidecars' subtitle rows point at the package's renditions or the copies in
the record (found by their content), their ids kept. A mismatch fails the step with
the file's name and keeps the original. No original is retired before its
title's current package carries the surround it had: when the package's
essence (which counts its 5.1 companions) lacks the `surround` of the
original's, or holds fewer channels than a 5.1 of it would (a 7.1 original's
5.1 is enough, a 5.1 original's stereo is not), the original is kept, its
source saying why, and its retire step waits — failed, with no retry of its
own, its error "its package has no 5.1 of the source's surround; re-encode it
first", its details `held for version <versionId>` — until another version
of the title is complete, or an admin retries the step. `{"held": true}`
queues these titles to be encoded again (see
[The re-encode queue](#the-re-encode-queue)). The same job deletes an extra's
original once its folder is recorded and verifies, removes a superseded
version after its grace (`version-removed`, its folder deleted) once its
folder holds no original (one there is retired first, against the newest
package of its source), moves a package folder of the store before the
library a version replaced to `.work/legacy/`, and empties the trash's and
the legacy folder's days after their grace. retryStep of `retire` has it
wait for the job's next pass. A removal that keeps the title's files puts an
original in its version's folder back where it arrived before the folder
goes, where the catalog still knows that place (a source recorded before it
kept none); the folder of one whose place it does not know stays, said in the
removal's errors.

Once a title's original is retired, nothing reads it any more: reencodeItem
says so, naming the event (a better version is a new arrival:
`.work/replace` and replaceSource), packageItem refuses it (409), the retries
leave the steps that read it, a series' reset leaves its episode, and
validateItem says `retired` (or `lost` when its package's chain is broken).
A recorded title keeps its type and its parent (`IDENTITY_KEPT`). A recorded
extra is written once: its removal is its `extra-removed` event (its folder
goes after the grace), and it is not packaged again. The console reads what
the library holds of a title in `item.library` (its record, versions,
originals, events), and holds its originals with `holdOriginal`.

The migration of a catalog's store before the library is staged by the
schemas' `library-v2-from-catalog.py --platform` under
`.work/migration/<run>/`, and adopted by `POST
/api/library/migrations/<run>/adopt` (the service account and admins): unit
by unit, a series before its episodes, its guards checked (a package changed
since is stale), the planned renames made (an original into its version's
folder, named as the library names it; a title nothing packaged gets a
version taken in), the database changed as packaging-complete leaves it,
each action journaled in the run's `journal.jsonl`; a failure puts the
unit's renames back, and adopting again skips what is adopted. A plan that
puts an original anywhere else in the record is refused. `POST …/revert` replays the journal backwards while
the originals are not purged from the trash. Both take
`{"items": ["<itemId>", …]}` to work on some units only. An item the
adoption itself marks changed (an extra it records) is projected again
within its unit; revert puts the staged projection back.

A tree written before 2026-10-08 is given the names the library gives its
files by the schemas' `library-v2-neutral-names.py` (an original in its
version's folder becomes `original.<ext>`, a subtitle file's copy in its
source's record `subtitle-<n>.<lang>….<ext>`, any other copy leaves the
record for the run's `removed/`), which changes no database. `POST
/api/library/migrations/<run>/names` (the service account and admins; the
tool's run is `neutral-names` unless it named another) points the catalog
at them by the run's `journal.jsonl`: for each item the run finished, in one
transaction under the item's lock, every row of the item naming a file the
run renamed or took out of the record (the asset of the title's file, a
source's place, a subtitle row, an extra's original, its diagnostics) names
it where it is now, and each of its recorded sources gets the name its
record gives its file and keeps no place among the arrivals. A row is
changed only while it names a file where it was, so calling it again changes
nothing. It takes `{"items": ["<itemId>", …]}` to work on some items only,
and answers `{"run", "items", "rows", "skipped": [{"itemId", "reason"}]}`:
how many items it took, how many rows it changed, and those it left and why
(one the run did not finish, one the catalog does not keep where the journal
has it, one a row of which would name a file that is not there). A journal
that names a file outside the folder of its item is refused whole (422), and
nothing changes; 409 while an adopt or a revert of the run runs.

A run staged beside the store before it (the schemas' `--originals link
--checksums pending`) is adopted the same way. A move of kind `link` gives
the file a second name where an original's move or a sidecar's would have
put it — a hard link, so the store before keeps playing its file and nothing
is copied; it needs both names on one filesystem, one mount of the share, and
says so when they are not. A failure, and a revert, take the second name away
and never the first; a first name removed since gets its file back. A version
whose package's checksums are pending is adopted complete and not verified:
its chain is held to the files and the bytes its `package.json` counted, and
since that proves no byte the retire job deletes no original on it (the
retirement fails saying so, and the original is kept). Once the schemas'
`library-v2-fill-checksums.py` has closed the chains, `POST
/api/library/migrations/<run>/checksums` (the service account and admins)
notes them by the run's `checksums.jsonl`: every version the tool filled is
verified in full at the moment it was, while it is what the tool left — the
catalog keeps it, complete and of that package, in the folder the journal
names, and its chain holds from the `package.json` the tool wrote. It takes
`{"items": ["<itemId>", …]}` to work on some items only, and answers `{"run",
"versions", "unchanged", "skipped": [{"itemId", "versionId", "reason"}]}`;
calling it again changes nothing. Where the library record holds subtitles the catalog
has no rows for — the migration's subtitles phase finds them with the
schemas' `library-v2-rebuild.py --compare --differences` and plans them in
the run's `subtitles-plan.json` — `POST /api/library/migrations/<run>/subtitles`
(the service account and admins) writes each planned row the catalog still
lacks, as it writes a package's: the path in the version's folder, the format
and language the package gives, its name or else its title as the label,
never the default, forced as the package says. Each row is looked at again in
its item's transaction, under the lock every writer of the item's versions
takes: the catalog keeps that version complete in that folder, the package
lists the subtitle there and says what the plan says of it, the file is there,
and no row of the item names that file or one of that language and file name;
any other row is left, with why. It takes `{"items": [...]}`, answers
`{"run", "rows", "unchanged", "skipped": [{"itemId", "path", "reason"}]}`,
journals each row in the run's `subtitles.jsonl`, and writes nothing when
called again. An original retired while the store before
still names it frees no space until that name is removed too.

`POST /api/library/projections` writes the items' projections now, in
either layout (the migration's verify runs before the layout is v2, which
the projector waits for): of those named, `{"items": ["<itemId>", …]}`, or
with `{}` of every recorded item, each whose `metadata.json` is not what the
projector would write now (rendered and compared byte for byte, its `asOf`
and `projectedBy` the file's own) or lacks an image it lists. It answers
`{"projected", "unchanged", "failed", "items": [{"itemId", "state",
"reason"}]}`, each named item or, with none named, each projected or failed;
a projected item's reason is `behind` when its time marks said so too (its
`databaseUpdatedAt`, its `libraryprojectedat`) and `shape` when they did not
(a projection of a shape from before). An item not recorded fails. 409 while
another projection runs.

A movie's or an episode's projection carries its marks as the catalog holds
them (`marks`): its chapters, each with its ordinal, and its intro, recap and
credits ranges, each with its detector, in the order of the timeline. A
version's `version.json` keeps the marks the version was made with, written
once; the analyzer, run again, or a person changes them later, and the
projection follows (migration 050 marks the title). A rebuild restores the
catalog's marks from the projection, and from the version that plays for a
projection written before it carried them. One written before is of another
shape: a refresh writes it again (`shape`).

## Configuration

Env vars mirror the previous service so existing manifests keep working — see
`internal/config/config.go`. Key ones: `SPRING_DATASOURCE_URL/USERNAME/PASSWORD`,
`SPRING_SECURITY_OAUTH2_RESOURCESERVER_JWT_ISSUER_URI`, `STREAM_SIGNING_KEY`,
`TMDB_API_KEY`, `SCANNER_NFS_ROOT`, `KAFKA_BROKERS`. An extra's file lives
under the media root, `LIBRARY_ROOT` (default `/var/lib/katalog/library`)
or `EXTRAS_ROOT` (default `/var/lib/katalog/extras`, a root the scanner
never walks). With the library's v2 layout the roots are the library's:
`LIBRARY_ROOT` (default `/var/lib/katalog`), `WORK_ROOT` (default
`<LIBRARY_ROOT>/.work`), `ARRIVALS_ROOT` (default `<WORK_ROOT>/incoming`, the
scan root) and `EXTRAS_ROOT` (default `<WORK_ROOT>/extras`). A root the
environment sets wins in either layout, and the layout is read where a root
is used, so switching it needs no restart. The extras' topics are `<KAFKA_TOPIC_PREFIX>catalog.extra.queued`,
`.transcoded` and `.packaged`; where the broker creates no topic by itself
they must be provisioned.
The enricher reads `<KAFKA_TOPIC_PREFIX>catalog.item.discovered` in the
consumer group `KAFKA_ENRICHER_GROUP` (default `katalog-enricher`). Two
instances that share a broker each need a group of their own: a group two
instances share is led by one of them, which cannot see the other's topics
where the broker's ACLs keep them apart, and the other's enricher is given
no partition and enriches nothing.
`AUTH_DISABLED=true` turns off auth for local dev: every caller may then do
anything.

Who may do what: `KATALOG_ADMIN_ROLE` (default `zaentrum-admin`, as the
portal's `PORTAL_ADMIN_ROLE`) is the role an administrator's token carries,
`KATALOG_ADDON_ROLE` (default `zaentrum-addon`) an addon's.
`KATALOG_ROLES_CLAIM` (default `realm_access.roles`, where Keycloak puts realm
roles) is where a token carries its roles, a dot-separated path into its
claims; one with an empty step stops the service at startup.
`KATALOG_SERVICE_CLIENTS` is a comma-separated list of the service account's
OIDC clients; unset, it is `KEYCLOAK_KATALOG_CLIENT_ID` (the client a
deployment gives the workers), else `zaentrum-manager`. Name only confidential
clients that mint tokens for themselves alone: any token issued to one counts
as the service account.

`TMDB_REFRESH_INTERVAL` (a Go duration, default `24h`; `0` or `off` turns it
off) is how often the people and titles the catalog holds are refreshed from
TMDB's change lists: each list (person, movie, tv) is read from its cursor up
to today and only what the catalog holds is refreshed; the cursor moves on once
a run went through. It idles while there is no TMDB key, and one instance runs
it at a time.

`KATALOG_CREDIT_ROLES` (a comma-separated list, by default every role:
`actor,creator,director,writer,producer,composer,cinematographer,editor`) are
the roles a title's credits follow TMDB in: TMDB's credits in them are read,
and a refresh drops a title's credits in any other role, as it drops those
TMDB no longer lists (a person no title credits after that is deleted, in the
deletion log). Anything in it that is not one of these roles stops the
service at startup with an error that says so.

The retries (see [The pipeline heals itself](#the-pipeline-heals-itself)):
`KATALOG_RETRY_MAX_ATTEMPTS` (default 3; 1 retries nothing by itself),
`KATALOG_RETRY_BACKOFF` (default `1m`), `KATALOG_RETRY_BACKOFF_MAX` (default
`1h`), `KATALOG_RETRY_INTERVAL` (default `30s`; `0` or `off` turns the
automatic retries and the reaper off) and `KATALOG_STEP_TIMEOUTS`
(`transcode=12h,package=3h`, over the defaults). A value that is no number or
duration, or a timeout of a step the pipeline does not have, stops the
service at startup with an error that says so.

## Develop

```bash
go build ./...
go test ./...                 # includes the GraphQL schema-binding test
go run ./cmd/server           # needs a reachable Postgres + the env above
```

Tests that touch the database need a PostgreSQL and are skipped without one.
Point them at a database where they may create and drop schemas; each test works
in a schema of its own and drops it afterwards:

```bash
KATALOG_TEST_DATABASE_URL='postgres://postgres@127.0.0.1:5432/postgres?sslmode=disable' go test ./...
```

GraphQL endpoint: `POST /query`. Health: `/healthz`,
`/actuator/health/{liveness,readiness}`.

## Build the container

```bash
docker build -t zaentrum/katalog-manager .
```

Static non-root binary on `:8080`. Build and push to your own registry.

## License

[MPL-2.0](LICENSE).
