# Implementation contract (read before writing code)

The shared core is DONE, compiles, and its SQL is verified against the live DB.
You implement ONE leaf package. Do not change the contract.

## Hard rules
- **Module:** `github.com/zaentrum/katalog-manager`. Go toolchain: use `/tmp/gobin/go`
  (a wrapper with GOROOT/GOPATH set). e.g. `/tmp/gobin/go build ./internal/<yourpkg>/`.
- **Build ONLY your own package(s)**: `/tmp/gobin/go build ./internal/<pkg>/` and
  `/tmp/gobin/go vet ./internal/<pkg>/`. Do NOT run `go build ./...` (other packages are
  being written concurrently). Your package MUST compile clean on its own.
- **Do NOT edit** these (they are fixed): `internal/{model,config,store,processing,auth,graph}`,
  `cmd/server`, `go.mod`, `go.sum`, `Dockerfile`, SPEC/*. Only create files in YOUR package dir.
- **Do NOT add external dependencies** (no `go get`). Available: stdlib, `github.com/go-chi/chi/v5`,
  `github.com/jackc/pgx/v5` (+ pgxpool), `github.com/segmentio/kafka-go`, `github.com/google/...` NO.
  Use stdlib `net/http`, `crypto/md5`, etc. If you think you need a new dep, DON'T — use stdlib.
- **All DB identifiers are lowercase** (`com_nalet_katalog_items`, `sorttitle`, `parent_id`, …).
  New ids: `gen_random_uuid()::varchar`. Do bespoke SQL via `st.Pool()` (a `*pgxpool.Pool`) inside
  YOUR package — do NOT add methods to the store package.
- Read `SPEC/00-OVERVIEW.md` + your area's detail file before coding. Reproduce contracts exactly.

## Shared types you can use

**`store.Store`** (`internal/store`) — `st.Pool() *pgxpool.Pool` for bespoke SQL; plus existing
reads you may reuse: `GetItem`, `GetItemBase`, `ListItems`, `AssetsByItem`, `SegmentsByItem`,
`StepsByItem`, `InsertScanJob(ctx,source,status)`, `FinishScanJob(ctx,id,store.ScanJobResult)`,
`UpsertDownloadJob(ctx,store.DownloadUpsert)` (upserts on the deterministic PK id), `GetSettingByKey`.
Inspect `internal/store/*.go` for exact signatures.

**`processing.Steps`** (`internal/processing`) — `New(pool)`, `Upsert(ctx,itemID,step,status string,errMsg,details *string) error`,
`ResetForItems(ctx,itemIDs,steps []string)(int64,error)`, `PromoteTranscodeToPackage(ctx,itemID,status) error`,
`ValidStep/ValidStatus(string) bool`, status consts `processing.Status{Pending,InProgress,Done,Failed,Skipped,NotApplicable}`,
errors `processing.Err{BadStep,BadStatus}`.

**`config.Config`** (`internal/config`) — fields incl. `NFSRoot`, `PackagesRoot`, `TMDBAPIKey`,
`TMDBLanguage`, `TMDBEnabled()`, `ChaptersDBEnabled`, `ChaptersDBBaseURL`, `DownloadGatewayURL`,
`DownloadGatewayEnabled()`, `DownloadEventsEnabled`, `KafkaBrokers`, `KafkaGroupID`, `KafkaCertDir`,
`ODownloaderURL`, `ODownloaderToken`, `ODownloaderEnabled()`, `ODownloaderPollSec`, `ODownloaderInbox`,
`ODownloaderTimeout`. Inspect `internal/config/config.go`.

**`model`** (`internal/model`) — domain structs + vocab vars (`model.Steps`, `model.AnalyzerSteps`,
`model.Statuses`, `model.Passes`, `model.SegmentKinds`, `model.SegmentSources`).

## Interfaces to satisfy (your service is assigned to one)

main wires your service into `graph.Services` by STRUCTURAL match — expose a `New(...)` constructor
and methods with EXACTLY these signatures (from `internal/graph/resolver.go`):

```go
// scanner -> graph.ScanRunner
Trigger(ctx context.Context, source string) (jobID string, err error)

// tmdb -> graph.Enricher
EnrichOne(ctx context.Context, id string) (status string, message string, err error)
EnrichPending(ctx context.Context, limit int32, typ string) (queued int32, err error)
BackfillEpisodeBackdrops(ctx context.Context) (artworkData, artwork int32, err error)
RetryNotFound(ctx context.Context, typ string) (reset int32, err error)

// itemactions -> graph.Packager + graph.Validator
PackageItem(ctx context.Context, id string) (graph.PackageResult, error)   // returns the struct type
ValidateItem(ctx context.Context, id string) (graph.ValidateResult, error)
// NOTE: to avoid importing graph (cycle-free anyway since graph doesn't import you),
// return YOUR OWN result struct whose FIELDS match graph.PackageResult / graph.ValidateResult
// shape; main will adapt. Simpler: define matching structs in your package and a small adapter
// in main. Confirm field names with graph/results.go (PackageResult{Status,AlreadyActive,Message,
// EpisodesEnqueued,EpisodesTotal *...}; ValidateResult{Code,Message string; SourcePath,PackagePath
// *string; Findings []ValidateFinding{Code,Message string}}).

// odownloader -> graph.TrailerFetcher
FetchTrailers(ctx context.Context, id string) (graph.FetchTrailersResult, error) // same note as above

// downloads -> graph.DownloadGateway
Add(ctx context.Context, adapter, source, title, wantedItemID string) (clientJobID, message string, err error)
Cancel(ctx context.Context, adapter, clientJobID string) (message string, err error)
Clients(ctx context.Context) (string, error)
```

To keep packages cycle-free and main simple, the graph result structs (`graph.PackageResult`,
`graph.ValidateResult`, `graph.FetchTrailersResult`) are PLAIN structs in `internal/graph/results.go`
with exported fields — you MAY import `internal/graph` ONLY for those structs (graph does not import
your package, so no cycle). Return them directly. That is the simplest path; do that.

## Constructor signatures main expects
```go
scanner.New(st *store.Store, cfg config.Config, steps *processing.Steps) *scanner.Scanner
chaptersdb.New(cfg config.Config) *chaptersdb.Client
tmdb.New(st *store.Store, cfg config.Config, steps *processing.Steps, ch *chaptersdb.Client) *tmdb.Service
downloads.NewGateway(cfg config.Config) *downloads.Gateway
downloads.NewConsumer(st *store.Store, cfg config.Config) *downloads.Consumer   // + (c *Consumer) Run(ctx) error
odownloader.New(st *store.Store, cfg config.Config, steps *processing.Steps) *odownloader.Service // + RunPoller(ctx)
itemactions.New(st *store.Store, cfg config.Config, steps *processing.Steps) *itemactions.Service
```

## Determinism note (downloads consumer)
The download read-model id must equal Java `UUID.nameUUIDFromBytes("adapter:clientJobId")` (MD5-based
UUIDv3, NO namespace). Implement with `crypto/md5`: `h := md5.Sum([]byte(adapter+":"+clientJobId))`,
set version nibble `h[6] = h[6]&0x0f | 0x30`, variant `h[8] = h[8]&0x3f | 0x80`, then format
`xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx`. Pass as `store.DownloadUpsert.ID`.
