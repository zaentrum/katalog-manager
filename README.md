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
  rather than paged), `scanJobs`, `settings`, `deletedItems` (the deletion
  log, read-only), `people` and
  `person` (a person's TMDB details, locks and field origins, and their
  `credits`: every title that credits them, newest first, each with its role,
  job, character, order and episode count), `referenceSync`
  (the change-list refresh's cursors and last runs, read-only),
  `processingOverview` (every step's items by state, the failed steps with
  their last errors, how the service retries, and the re-encode queue), and
  the operator actions
  (`triggerScan`, `enrichOne`/`enrichPending`, `refreshPeople`, `packageItem`,
  `validateItem`, `retryStep`/`retryFailed` (see
  [The pipeline heals itself](#the-pipeline-heals-itself)), `reencodeItem`
  (see [Encoding a title again](#encoding-a-title-again)),
  `clearReencodeQueue` (see [The re-encode queue](#the-re-encode-queue)),
  `replaceSource` (see [Replacing a title's file](#replacing-a-titles-file)),
  `backfillSourceProbes`, `backfillRatings` and `setMinAgeOverride` (see
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
  - `GET /api/settings` — the settings that are no secret, for the workers:
    `{"<key>": {"valueText": "...", "valueType": "..."}}`. The packager reads
    its language whitelist from it (`packager.language_whitelist`, comma
    separated) and `packager.keep_original_if_single`. A secret setting is
    left out, set or not.
  - `POST /api/ingest` — an addon hands a file on disk to the catalog.
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
| `PUT /api/artwork/...`, `/api/analyze/*` (an extra's record and steps too), segments, chapters, `packaging-complete` (an item's and an extra's), `GET /api/settings` | admin, service account |
| `POST /api/library/migrations/{run}/adopt`, `…/revert` (the library's migration), `POST /api/library/projections` | admin, service account |
| `POST /api/library/reencode`, `GET /api/library/reencode` (the re-encode queue) | admin, service account |
| `POST /api/ingest`, `POST /api/extras` | admin, service account, addon |
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
together, each title once:

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
packager gives in its manifest, the bit rate from size and duration.
`backfillSourceProbes` fills what the sources probed before lack, from the
catalog's records.

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
then it waits where it arrived, in `.work/incoming`. katalog-manager decides
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
its folder, the package added to it going out.

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
full, unless the migration's stage did that within `library.verify.maxAge`),
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
goes.

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
