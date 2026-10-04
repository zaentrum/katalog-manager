package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/zaentrum/katalog-manager/internal/auth"
	"github.com/zaentrum/katalog-manager/internal/auth/authtest"
	"github.com/zaentrum/katalog-manager/internal/config"
	"github.com/zaentrum/katalog-manager/internal/graph"
	"github.com/zaentrum/katalog-manager/internal/processing"
	"github.com/zaentrum/katalog-manager/internal/rest"
	"github.com/zaentrum/katalog-manager/internal/store"
	"github.com/zaentrum/katalog-manager/internal/store/storetest"
	"github.com/zaentrum/katalog-manager/internal/stream"
)

// fakes stands in for every integration the GraphQL operations call, and
// records each call.
type fakes struct {
	st    *store.Store
	mu    sync.Mutex
	calls []string
}

func (f *fakes) called(c string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, c)
}

// take returns the calls made since the last take.
func (f *fakes) take() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.calls
	f.calls = nil
	return c
}

func (f *fakes) Trigger(ctx context.Context, source string) (string, error) {
	f.called("scan " + source)
	j, err := f.st.InsertScanJob(ctx, source, "running")
	if err != nil {
		return "", err
	}
	return j.ID, nil
}
func (f *fakes) EnrichOne(_ context.Context, id string) (string, string, error) {
	f.called("enrich " + id)
	return "done", "", nil
}
func (f *fakes) IdentifyOne(_ context.Context, id, _ string, _ *int64) (string, string, error) {
	f.called("identify " + id)
	return "done", "", nil
}
func (f *fakes) EnrichPending(context.Context, int32, string) (int32, error) {
	f.called("enrich pending")
	return 0, nil
}
func (f *fakes) BackfillEpisodeBackdrops(context.Context) (int32, int32, error) {
	f.called("backfill backdrops")
	return 0, 0, nil
}
func (f *fakes) RetryNotFound(context.Context, string) (int32, error) {
	f.called("retry not found")
	return 0, nil
}
func (f *fakes) RefreshPeople(context.Context, bool) (graph.PeopleRefreshResult, error) {
	f.called("refresh people")
	return graph.PeopleRefreshResult{}, nil
}
func (f *fakes) PackageItem(_ context.Context, id string) (graph.PackageResult, error) {
	f.called("package " + id)
	return graph.PackageResult{}, nil
}
func (f *fakes) ValidateItem(_ context.Context, id string) (graph.ValidateResult, error) {
	f.called("validate " + id)
	return graph.ValidateResult{Code: "ok", Message: "ok"}, nil
}
func (f *fakes) RemoveItem(_ context.Context, id string, files, packages bool, _ string) (graph.RemoveResult, error) {
	f.called(fmt.Sprintf("remove %s, files %v, packages %v", id, files, packages))
	return graph.RemoveResult{Deleted: true, ItemsRemoved: 1}, nil
}
func (f *fakes) RetryStep(_ context.Context, itemID, step string) (graph.RetryStepResult, error) {
	f.called("retry " + itemID + " " + step)
	return graph.RetryStepResult{ItemID: itemID, Step: step, Message: "left alone"}, nil
}
func (f *fakes) RetryFailed(_ context.Context, step string) (graph.RetryFailedResult, error) {
	f.called("retry the failed " + step)
	return graph.RetryFailedResult{Message: "none"}, nil
}
func (f *fakes) Overview(context.Context, string, int32, int32) (graph.ProcessingOverview, error) {
	f.called("processing overview")
	return graph.ProcessingOverview{}, nil
}

// The secrets the instance holds; no answer may carry one.
const tmdbSecret, fanartSecret, omdbSecret = "tmdb-secret-token", "fanart-secret-key", "omdb-secret-key"

// instance is the service as main wires it, on a test database, with fakes
// for its integrations and a test issuer whose tokens it takes.
type instance struct {
	url string
	iss *authtest.Issuer
	st  *store.Store
	f   *fakes
	// every answer's body, to be searched for a secret
	mu      sync.Mutex
	answers []string
}

var streamKey = []byte("0123456789abcdef0123456789abcdef")

func newInstance(t *testing.T) *instance {
	t.Helper()
	return newInstanceWith(t, nil)
}

// newInstanceWith is newInstance with the pipeline's retries pipeline makes
// of the instance's store (nil: a fake).
func newInstanceWith(t *testing.T, pipeline func(*store.Store) graph.Pipeline) *instance {
	t.Helper()
	st := storetest.Open(t)
	storetest.Exec(t, st, `CREATE VIEW katalogservice_items AS SELECT id, createdat, createdby, modifiedat, modifiedby,
		type, title, sorttitle, year, description, rating, durationms, parent_id, seasonnumber, episodenumber, tagline,
		NULL::varchar AS posterurl, NULL::varchar AS backdropurl, NULL::bigint AS runtimemin, NULL::varchar AS yeartext
		FROM com_nalet_katalog_items`)
	storetest.AddItem(t, st, "m1", "movie", "A Film", "")
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_itemprocessingsteps (id, createdat, modifiedat, item_id, step, status,
		attempts, error, failures, lasterror) VALUES ('st-m1', now(), now(), 'm1', 'transcode', 'failed', 3, 'ffmpeg exited 1', 1, 'ffmpeg exited 1')`)
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_people (id, name) VALUES ('p1', 'Ada')`)
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_settings (id, key, valuetext) VALUES
		('set-tmdb', 'tmdb.api_key', $1), ('set-fanart', 'fanart.api_key', $2),
		('set-langs', 'packager.languages', 'en,de'), ('set-gone', 'scanner.old', 'x')`, tmdbSecret, fanartSecret)

	iss := authtest.NewIssuer(t)
	dir := t.TempDir()
	cfg := config.Config{AdminRole: "zaentrum-admin", AddonRole: "zaentrum-addon", RolesClaim: auth.DefaultRolesClaim,
		ServiceClients: []string{"zaentrum-manager"}, NFSRoot: dir + "/media", PackagesRoot: dir + "/packages"}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	jwt, err := auth.NewJWTVerifier(ctx, iss.URL, cfg.Audience, false, false)
	if err != nil {
		t.Fatal(err)
	}
	sv, err := auth.NewStreamVerifier(base64.StdEncoding.EncodeToString(streamKey))
	if err != nil {
		t.Fatal(err)
	}
	f := &fakes{st: st}
	var pipe graph.Pipeline = f
	if pipeline != nil {
		pipe = pipeline(st)
	}
	schema := graph.MustSchema(graph.NewResolver(st, cfg, graph.Services{Scanner: f, Enricher: f, People: f,
		Packager: f, Validator: f, Remover: f, Pipeline: pipe}))
	r := chi.NewRouter()
	routes(r, auth.NewMiddleware(jwt, sv).Handler, cfg.Policy(), schema, stream.NewBroker().Handler,
		rest.New(rest.Deps{Store: st, Cfg: cfg, Steps: processing.New(st.Pool())}))
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	in := &instance{url: srv.URL, iss: iss, st: st, f: f}
	t.Cleanup(func() {
		for _, a := range in.answers {
			for _, s := range []string{tmdbSecret, fanartSecret, omdbSecret} {
				if strings.Contains(a, s) {
					t.Errorf("an answer carries a secret: %s", a)
				}
			}
		}
	})
	return in
}

type gqlAnswer struct {
	Data   json.RawMessage `json:"data"`
	Errors []struct {
		Message    string         `json:"message"`
		Extensions map[string]any `json:"extensions"`
	} `json:"errors"`
}

func (in *instance) record(body string) {
	in.mu.Lock()
	defer in.mu.Unlock()
	in.answers = append(in.answers, body)
}

// gql posts a document to path with token, and returns the HTTP status and
// the GraphQL answer.
func (in *instance) gql(t *testing.T, path, token, doc string) (int, gqlAnswer) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"query": doc})
	req, err := http.NewRequest(http.MethodPost, in.url+path, strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	in.record(string(raw))
	var a gqlAnswer
	if resp.StatusCode == http.StatusOK {
		if err := json.Unmarshal(raw, &a); err != nil {
			t.Fatalf("%s: %s is not a GraphQL answer", doc, raw)
		}
	}
	return resp.StatusCode, a
}

// fingerprint is everything the operations could change in the database.
func fingerprint(t *testing.T, st *store.Store) string {
	t.Helper()
	var b strings.Builder
	for _, table := range []string{"com_nalet_katalog_items", "com_nalet_katalog_itemgenres", "com_nalet_katalog_itemtags",
		"com_nalet_katalog_genres", "com_nalet_katalog_settings", "com_nalet_katalog_scanjobs",
		"com_nalet_katalog_itemprocessingsteps"} {
		var rows string
		if err := st.Pool().QueryRow(context.Background(),
			`SELECT coalesce(string_agg(x::text, '|' ORDER BY x::text), '') FROM `+table+` x`).Scan(&rows); err != nil {
			t.Fatal(err)
		}
		b.WriteString(table + ":" + rows + "\n")
	}
	return b.String()
}

// operations calls every root field of the schema, each with what its fakes
// record for an admin (none for a field that calls no integration).
var operations = []struct{ doc, calls string }{
	{`{ item(id: "m1") { id title processingSteps { step status failures lastError nextRetryAt dispatchedAt updatedAt } } }`, ""},
	{`{ items(limit: 5) { id } }`, ""},
	{`{ movies { id } }`, ""},
	{`{ series { id } }`, ""},
	{`{ episodes { id } }`, ""},
	{`{ albums { id } }`, ""},
	{`{ searchItems(q: "Film") { total items { id } } }`, ""},
	{`{ scanJob(id: "none") { id } }`, ""},
	{`{ scanJobs { id } }`, ""},
	{`{ activity { id } }`, ""},
	{`{ settings { key valueText isSecret isSet updatedAt } }`, ""},
	{`{ genres { name } }`, ""},
	{`{ people { id } }`, ""},
	{`{ person(id: "p1") { id name } }`, ""},
	{`{ enrichStatus { tmdbEnabled } }`, ""},
	{`{ enrichmentStatusCodes { code } }`, ""},
	{`{ deletedItems { id } }`, ""},
	{`{ referenceSync { kind } }`, ""},
	{`{ processingOverview(step: "transcode") { failedTotal steps { step failed } failed { itemId } retry { available } } }`, "processing overview"},

	{`mutation { triggerScan(source: "nfs") { id status } }`, "scan nfs"},
	{`mutation { enrichOne(id: "m1") { status } }`, "enrich m1"},
	{`mutation { identify(id: "m1", title: "A Film", tmdbId: 1) { status } }`, "identify m1"},
	{`mutation { enrichPending(limit: 1) { queued } }`, "enrich pending"},
	{`mutation { refreshPeople(all: false) { titlesRead } }`, "refresh people"},
	{`mutation { backfillEpisodeBackdrops { artwork } }`, "backfill backdrops"},
	{`mutation { backfillSourceProbes { assets filled unknown } }`, ""},
	{`mutation { retryNotFound { reset } }`, "retry not found"},
	{`mutation { retryStep(itemId: "m1", step: "transcode") { retried message } }`, "retry m1 transcode"},
	{`mutation { retryFailed(step: "package") { retried message } }`, "retry the failed package"},
	{`mutation { packageItem(id: "m1") { status } }`, "package m1"},
	{`mutation { validateItem(id: "m1") { code } }`, "validate m1"},
	{`mutation { createItem(input: {type: "movie", title: "Made Here"}) { id } }`, ""},
	{`mutation { updateItem(id: "m1", input: {tagline: "Edited"}) { id tagline } }`, ""},
	{`mutation { deleteItem(id: "m1", deleteFiles: true, deletePackages: true) { deleted } }`, "remove m1, files true, packages true"},
	{`mutation { setItemGenres(id: "m1", genres: ["Drama"]) { id } }`, ""},
	{`mutation { setItemTags(id: "m1", tags: ["cc"]) { id } }`, ""},
	{`mutation { createSetting(key: "scanner.depth", valueText: "3") { id } }`, ""},
	{`mutation { updateSetting(id: "set-langs", valueText: "en") { id valueText } }`, ""},
	{`mutation { deleteSetting(id: "set-gone") }`, ""},
	{`mutation { setSecretSetting(key: "omdb.api_key", value: "` + omdbSecret + `") { key valueText isSet } }`, ""},
	{`mutation { clearSecretSetting(key: "fanart.api_key") }`, ""},
}

var rootField = regexp.MustCompile(`^(?:mutation )?\{ (\w+)`)

// operations calls every root field of the schema, as introspection lists
// them.
func TestOperationsCallEveryRootField(t *testing.T) {
	in := newInstance(t)
	_, a := in.gql(t, "/api/manage/query", in.iss.Viewer(t),
		`{ __schema { queryType { fields { name } } mutationType { fields { name } } } }`)
	var s struct {
		Schema struct {
			QueryType, MutationType struct{ Fields []struct{ Name string } }
		} `json:"__schema"`
	}
	if err := json.Unmarshal(a.Data, &s); err != nil || len(a.Errors) > 0 {
		t.Fatalf("introspection as a viewer: %s %v", a.Data, a.Errors)
	}
	var fields, called []string
	for _, f := range append(s.Schema.QueryType.Fields, s.Schema.MutationType.Fields...) {
		fields = append(fields, f.Name)
	}
	for _, op := range operations {
		called = append(called, rootField.FindStringSubmatch(op.doc)[1])
	}
	sort.Strings(fields)
	sort.Strings(called)
	if !slices.Equal(fields, called) {
		t.Errorf("the root fields:\n %v\nthe operations call:\n %v", fields, called)
	}
}

// Every operation, through the service as main wires it and with the bearer
// tokens of a realm: refused with FORBIDDEN to a viewer, another
// confidential client, an addon and (on all but triggerScan) the service
// account, before anything is read or changed; done for an admin, also one
// signed in with the CLI, whose token names no audience; and no answer
// carries a secret.
func TestEveryOperationIsForWhomItIsFor(t *testing.T) {
	in := newInstance(t)
	iss := in.iss
	refused := []struct{ name, token string }{
		{"a viewer", iss.Viewer(t)},
		{"another confidential client", iss.Service(t, "zaentrum-other")},
		{"an addon", iss.Addon(t)},
	}
	service := iss.Service(t, "zaentrum-manager")
	allowed := []struct{ name, token string }{{"an admin", iss.Admin(t)}, {"an admin through the CLI", iss.CLIAdmin(t)}}

	before := fingerprint(t, in.st)
	for _, op := range operations {
		callers := refused
		if !strings.Contains(op.doc, "triggerScan") {
			callers = append(slices.Clone(refused), struct{ name, token string }{"the service account", service})
		}
		for _, c := range callers {
			code, a := in.gql(t, "/api/manage/query", c.token, op.doc)
			field := rootField.FindStringSubmatch(op.doc)[1]
			// a nullable field is null, a non-null one nulls the data
			nothing := string(a.Data) == "null" || string(a.Data) == `{"`+field+`":null}`
			if code != http.StatusOK || len(a.Errors) != 1 || a.Errors[0].Extensions["code"] != "FORBIDDEN" ||
				!strings.HasPrefix(a.Errors[0].Message, "forbidden: "+field+" requires the zaentrum-admin role") || !nothing {
				t.Errorf("%s: %s: %d %s %v, want it refused with FORBIDDEN", c.name, op.doc, code, a.Data, a.Errors)
			}
		}
	}
	if calls := in.f.take(); len(calls) > 0 {
		t.Errorf("refused callers reached the integrations: %q", calls)
	}
	if after := fingerprint(t, in.st); after != before {
		t.Errorf("refused callers changed the database:\nbefore %s\nafter  %s", before, after)
	}

	for _, op := range operations {
		callers := allowed
		if strings.Contains(op.doc, "triggerScan") {
			callers = append(slices.Clone(allowed), struct{ name, token string }{"the service account", service})
		}
		for _, c := range callers {
			code, a := in.gql(t, "/api/manage/query", c.token, op.doc)
			if code != http.StatusOK || len(a.Errors) > 0 || len(a.Data) == 0 || string(a.Data) == "null" {
				t.Errorf("%s: %s: %d %s %v, want it done", c.name, op.doc, code, a.Data, a.Errors)
			}
			if calls := strings.Join(in.f.take(), "; "); calls != op.calls {
				t.Errorf("%s: %s called %q, want %q", c.name, op.doc, calls, op.calls)
			}
		}
	}
	if n := storetest.Count(t, in.st, `SELECT count(*) FROM com_nalet_katalog_settings WHERE key = 'omdb.api_key' AND valuetext = $1`, omdbSecret); n != 1 {
		t.Errorf("the admins' setSecretSetting stored %d omdb keys, want it set", n)
	}
	if n := storetest.Count(t, in.st, `SELECT count(*) FROM com_nalet_katalog_settings WHERE key = 'fanart.api_key'`); n != 0 {
		t.Errorf("the admins' clearSecretSetting left %d fanart keys", n)
	}
}

// GraphQL answers at all four of its mount points, the same for each: a
// viewer is refused the settings, an admin is not; anyone may ask
// __typename (zae doctor does, to see the console's API is up). Without a
// token nothing answers but 401.
func TestGraphQLAtEveryMountPoint(t *testing.T) {
	in := newInstance(t)
	viewer, admin := in.iss.Viewer(t), in.iss.Admin(t)
	for _, path := range []string{"/query", "/graphql", "/api/manage/query", "/api/manage/graphql"} {
		if _, a := in.gql(t, path, viewer, `{ settings { key } }`); len(a.Errors) != 1 || a.Errors[0].Extensions["code"] != "FORBIDDEN" {
			t.Errorf("%s: a viewer's settings: %s %v", path, a.Data, a.Errors)
		}
		if _, a := in.gql(t, path, admin, `{ settings { key } }`); len(a.Errors) > 0 {
			t.Errorf("%s: an admin's settings: %v", path, a.Errors)
		}
		if _, a := in.gql(t, path, viewer, `{ __typename }`); string(a.Data) != `{"__typename":"Query"}` {
			t.Errorf("%s: a viewer's __typename: %s %v", path, a.Data, a.Errors)
		}
		if code, _ := in.gql(t, path, "", `{ __typename }`); code != http.StatusUnauthorized {
			t.Errorf("%s without a token: %d, want 401", path, code)
		}
	}
}

// The console's live stream, what the pipeline does to which title, is an
// admin's: refused to a viewer and the service account, and to no token.
func TestTheLiveStreamIsAnAdmins(t *testing.T) {
	in := newInstance(t)
	for _, tc := range []struct {
		name, token string
		code        int
	}{
		{"a viewer", in.iss.Viewer(t), http.StatusForbidden},
		{"the service account", in.iss.Service(t, "zaentrum-manager"), http.StatusForbidden},
		{"no token", "", http.StatusUnauthorized},
		{"an admin", in.iss.Admin(t), http.StatusOK},
	} {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, in.url+"/api/manage/stream", nil)
		if tc.token != "" {
			req.Header.Set("Authorization", "Bearer "+tc.token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			cancel()
			t.Fatalf("%s: %v", tc.name, err)
		}
		ct := resp.Header.Get("Content-Type")
		resp.Body.Close()
		cancel()
		if resp.StatusCode != tc.code || tc.code == http.StatusOK && !strings.HasPrefix(ct, "text/event-stream") {
			t.Errorf("%s: %d %s, want %d", tc.name, resp.StatusCode, ct, tc.code)
		}
	}
}
