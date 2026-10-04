# katalog-manager

Catalog-management API for the **zaentrum** platform — a Go + GraphQL service.
It owns the catalog write/admin surface (items, artwork, processing-step audit,
settings) and drives enrichment, scanning and packaging. A rewrite of the
former SAP CAP/Java service onto Go. Files enter the catalog through the
scanner and the neutral `POST /api/ingest`, and through nothing else.

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
  their last errors, and how the service retries), and the operator actions
  (`triggerScan`, `enrichOne`/`enrichPending`, `refreshPeople`, `packageItem`,
  `validateItem`, `retryStep`/`retryFailed` (see
  [The pipeline heals itself](#the-pipeline-heals-itself)),
  `backfillSourceProbes`, item + settings CRUD; a delete removes files from
  disk only when asked; a secret setting, such as an API key, is write-only:
  `setSecretSetting`/`clearSecretSetting`, and no field returns its value;
  `setSecretSetting` checks a TMDB token with TMDB's authentication endpoint
  first, for 4 seconds at most: a token TMDB refuses (401) is not stored and
  the answer is an error with the code `SECRET_REFUSED`, one TMDB takes is
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
    protocol.
  - `GET /api/settings` — the settings that are no secret, for the workers:
    `{"<key>": {"valueText": "...", "valueType": "..."}}`. The packager reads
    its language whitelist from it (`packager.language_whitelist`, comma
    separated) and `packager.keep_original_if_single`. A secret setting is
    left out, set or not.
  - `POST /api/ingest` — an addon hands a file on disk to the catalog.
  - `POST /api/items/{id}/package` — an admin's packaging action, as
    chino-api's admin route forwards it with the admin's token: what
    GraphQL's `packageItem` does, answered as the CAP service did
    (`{status, alreadyActive, message}`, a series'
    `{episodesEnqueued, episodesTotal, message}`; 404 unknown, 400 not
    packageable).

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
| `GET /api/manage/stream` (the console's live stream) | admin |
| `GET /api/artwork/...`, `/api/manage/artwork/...` (also a person's portrait) | any signed-in caller, or a stream token |
| `GET /api/play/...`, `GET /api/subtitles/...` | any signed-in caller |
| `PUT /api/artwork/...`, `/api/analyze/*`, segments, chapters, `packaging-complete`, `GET /api/settings` | admin, service account |
| `POST /api/ingest` | admin, service account, addon |
| `POST /api/items/{id}/package` | admin |

A refused GraphQL field answers with an error whose `extensions.code` is
`FORBIDDEN` and whose message names the role; a refused route answers 403
with `{"error": "..."}`. Introspection and `__typename` answer any signed-in
caller.

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

## The pipeline heals itself

A step that fails is retried: the event that triggers its worker is sent
again — `discovered` for `tmdb` (the enricher) and the `scan` step (the item's
pipeline from its start), `enriched` for the analyzer's passes, `analyzed` for
`transcode`, `transcoded` for `package` — one event per item and worker. Each
worker passes the chain on, and its own guard skips work that is done.

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
  analyzer's passes and `package`, 6h for `transcode`
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
- An event that could not be sent puts its steps back, failed; the sweep
  sends it again a backoff later. Every `KATALOG_RETRY_INTERVAL` (30s) the
  sweep reaps and sends what is due; without an event bus or migration 033
  nothing is retried, and the overview says why.

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

## Configuration

Env vars mirror the previous service so existing manifests keep working — see
`internal/config/config.go`. Key ones: `SPRING_DATASOURCE_URL/USERNAME/PASSWORD`,
`SPRING_SECURITY_OAUTH2_RESOURCESERVER_JWT_ISSUER_URI`, `STREAM_SIGNING_KEY`,
`TMDB_API_KEY`, `SCANNER_NFS_ROOT`, `KAFKA_BROKERS`.
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
