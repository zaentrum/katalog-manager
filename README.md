# katalog-manager

Catalog-management API for the **zaentrum** platform — a Go + GraphQL service.
It owns the catalog write/admin surface (items, artwork, processing-step audit,
downloads read-model, settings) and drives enrichment, scanning, packaging and
trailer ingestion. A rewrite of the former SAP CAP/Java service onto Go.

## Architecture

The surface is split deliberately:

- **GraphQL** (`/query`) — the operator/UI read graph and mutations: catalog
  reads (`items`, `movies`, `series`, `episodes`, `albums`, `item` with nested
  facets + computed fields; an item's `people` are its credits, each a person
  in a role with its job, character, order and episode count), `searchItems`,
  `scanJobs`, `downloadJobs`,
  `settings`, `deletedItems` (the deletion log, read-only), `people` and
  `person` (a person's TMDB details, locks and field origins), `referenceSync`
  (the change-list refresh's cursors and last runs, read-only), and the operator
  actions (`triggerScan`, `enrichOne`/`enrichPending`, `refreshPeople`,
  `packageItem`, `validateItem`, `fetchTrailers`, `addDownload`/`cancelDownload`,
  item + settings CRUD). Schema-first via
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
  - `POST /api/analyze/claim`, `PUT /api/analyze/items/{id}/steps/{step}`,
    `PUT/DELETE /api/segments|chapters/items/{id}`,
    `POST /api/items/{id}/packaging-complete` — the analyzer/packager worker
    protocol.
  - Kafka `stube.download.client.*` consumer — projects the downloads read model.

## Data

The service reuses the existing `katalog` Postgres database **unchanged** — the
lowercase `com_nalet_katalog_*` tables and the computed `katalogservice_*` views.
No destructive migration. The files in `db/migrations/` apply on top of the base
schema in the order of their numbers, and each is idempotent:

- `028_go_rewrite.sql` fills two gaps (the `trailerjobs` table and a
  `downloadjobs (adapter, clientjobid)` unique index).
- `029_deleted_items.sql` adds the deletion log, `com_nalet_katalog_deleteditems`:
  every item the catalog deletes, and every person it deletes because no title
  credits them any more (type `person`). A title's credits follow TMDB: read
  from TMDB, they replace the title's credits, and a person left uncredited
  goes. They come in every role TMDB's credits give — its cast as actors, a
  series' creators, and of the crew its directors, writers, producers,
  composers, cinematographers and editors — one credit per person and role.
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
`TMDB_API_KEY`, `SCANNER_NFS_ROOT`, `DOWNLOAD_GATEWAY_URL`,
`DOWNLOAD_GATEWAY_EVENTS_ENABLED`, `KAFKA_BROKERS`, `ODOWNLOADER_URL/TOKEN`.
`AUTH_DISABLED=true` turns off auth for local dev.

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
