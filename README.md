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
  in a role with its job, character, order and episode count), `searchItems`,
  `scanJobs`, `settings`, `deletedItems` (the deletion log, read-only), `people` and
  `person` (a person's TMDB details, locks and field origins, and their
  `credits`: every title that credits them, newest first, each with its role,
  job, character, order and episode count), `referenceSync`
  (the change-list refresh's cursors and last runs, read-only), and the operator
  actions (`triggerScan`, `enrichOne`/`enrichPending`, `refreshPeople`,
  `packageItem`, `validateItem`, item + settings CRUD; a secret setting, such as an API key, is write-only:
  `setSecretSetting`/`clearSecretSetting`, and no field returns its value).
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
No destructive migration. The files in `db/migrations/` apply on top of the base
schema in the order of their numbers, and each is idempotent:

- `028_go_rewrite.sql` creates nothing any more: the two objects it added
  served an integration the core no longer carries (see the file). A catalog
  that applied it keeps them; nothing reads or writes them.
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
