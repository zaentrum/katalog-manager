# katalog-manager

katalog-manager is the catalog's write and admin side: a Go service with a
GraphQL API (`/query`, the schema in `internal/graph/schema.graphql`) and REST
routes. It owns the catalog's PostgreSQL tables and applies its migrations
(`db/migrations/`) when it starts. Files enter the catalog through its scanner
and `POST /api/ingest`, extras through `POST /api/extras`; it enriches titles
and people from TMDB, drives the processing steps (analyzer, transcoder,
packager) over Kafka, and keeps artwork, settings, the library's records and
its compliance report. katalog-manager-ui and the portal use the GraphQL API
(on the platform also at `/api/manage/query`), the pipeline's workers and
chino-api the REST routes, and katalog-api reads the tables it writes.
How a change is proposed and what a pull request has to pass is in the
[guide to contributing](https://github.com/zaentrum/.github/blob/main/CONTRIBUTING.md);
what may be built is in [building from the specification](https://github.com/zaentrum/zaentrum/blob/main/AGENTS.md).

## Build

```sh
go build ./...
docker build -t katalog-manager .
```

It needs Go 1.24 (`go.mod`). The image builds a static binary in
`golang:1.24-alpine` with `GOTOOLCHAIN=local` and runs it on distroless as a
non-root user, on port 8080.

## Test

```sh
go vet ./...
KATALOG_TEST_DATABASE_URL='postgres://postgres@127.0.0.1:5432/postgres?sslmode=disable' go test ./...
```

The workflow `.github/workflows/build.yml` runs `go build ./...`, `go vet
./...` and these tests on every pull request and on main, against a
`postgres:17` service that trusts every connection. The tests that read and
write a catalog, most of them, need `KATALOG_TEST_DATABASE_URL`: a PostgreSQL
in which they may create and drop schemas. Each makes a schema of its own
(`internal/store/storetest`) and drops it; without the variable they are
skipped. The test that runs `db/export/library-export.sql` through `psql`
skips without `psql` on the `PATH`. `internal/graph/schema_test.go` checks
that the GraphQL schema binds to its resolvers, as the service does at start.
The differential of the deletion gate against the schemas' record logic
(`internal/captions/differential_test.go`) needs `python3`; the test that
runs the schemas' validator on a library a retirement wrote in
(`internal/library/contract_e2e_test.go`) needs a `python3` with `jsonschema`
and `referencing` (`pip install "jsonschema[format-nongpl]>=4.23"
referencing`; `KATALOG_TEST_PYTHON` names another python). Without them
they are skipped, except in CI (`CI` set), where they fail.

## Run it

```sh
createdb -h 127.0.0.1 -U postgres katalog
psql -h 127.0.0.1 -U postgres -d katalog -f schema-postgres.sql
psql -h 127.0.0.1 -U postgres -d katalog -v ON_ERROR_STOP=1 -f schema-extra.sql
SPRING_DATASOURCE_URL='postgres://postgres@127.0.0.1:5432/katalog' AUTH_DISABLED=true go run ./cmd/server
curl -fsS http://localhost:8080/healthz
```

The two files are the base schema, from the platform's chart
(`operator/platform/chart/files/sql/` in zaentrum-operator); the first reports
one view that only the second can make. The service applies its own
migrations on top and answers GraphQL at `POST /query`, on `SERVER_PORT`
(8080). With `AUTH_DISABLED=true`, or with no issuer set, every caller may do
anything; with `OIDC_ISSUER` set, a bearer of it is checked and its roles
(`KATALOG_ADMIN_ROLE`, `KATALOG_ADDON_ROLE`) decide what it may do. Without a
TMDB key (`TMDB_API_KEY`) the refresh from TMDB idles; without
`KAFKA_BROKERS` no event is sent or read, and the retries idle. The library's
folders are `LIBRARY_ROOT` and the roots beside it, which the README's
Configuration names. The reference demo serves the GraphQL API at
`/api/manage/query` of its address, behind its sign-in
([how it is deployed](https://github.com/zaentrum/zaentrum/blob/main/docs/reference-demo.md)).

## What the tests leave unchecked

- Kafka: events go to a fake writer, and no test talks to a broker.
- TMDB, fanart.tv and OMDB answer from fake servers. `internal/chaptersdb`
  and `internal/stream` have no test file of their own, but tests run both:
  `internal/tmdb/chaptersdb_test.go` runs the ChaptersDB client against a
  stand-in server, and `cmd/server/routes_test.go` checks who may open the
  console's live stream (`/api/manage/stream`) and that it answers as
  `text/event-stream`. What the stream sends is untested.
- The base schema: the tests make the tables from their own copy of the part
  the service touches (`internal/store/storetest`), not from the chart's files.
- CI runs PostgreSQL 17; the platform's chart runs PostgreSQL 16.
- Storage: the scanner, the library and retiring originals are tested in
  temporary folders, not on the platform's shared storage.
- `TestWriteTheProjectionFixture` only writes a fixture, with
  `KATALOG_PROJECTION_FIXTURE` set; it is skipped otherwise.
