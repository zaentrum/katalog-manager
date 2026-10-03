package graph

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/zaentrum/katalog-manager/internal/auth"
)

// typeRef is a type as introspection gives it, wrapped in NON_NULL and LIST.
type typeRef struct {
	Kind   string   `json:"kind"`
	Name   string   `json:"name"`
	OfType *typeRef `json:"ofType"`
}

func (t *typeRef) named() *typeRef {
	for t.OfType != nil {
		t = t.OfType
	}
	return t
}

type rootField struct {
	Name string `json:"name"`
	Args []struct {
		Name string  `json:"name"`
		Type typeRef `json:"type"`
	} `json:"args"`
	Type typeRef `json:"type"`
}

const typeFields = `kind name ofType { kind name ofType { kind name ofType { kind name } } }`

// rootFields reads the schema's root fields by introspection, which any
// caller may run: "Query.items" and "Mutation.deleteItem", each with its
// arguments and type.
func rootFields(t *testing.T) map[string]rootField {
	t.Helper()
	resp := MustSchema(NewResolver(nil, testConfig, Services{})).Exec(context.Background(),
		`{ __schema { queryType { fields { name args { name type { `+typeFields+` } } type { `+typeFields+` } } }
		  mutationType { fields { name args { name type { `+typeFields+` } } type { `+typeFields+` } } } } }`, "", nil)
	if len(resp.Errors) > 0 {
		t.Fatal(resp.Errors)
	}
	var s struct {
		Schema struct {
			QueryType, MutationType struct{ Fields []rootField }
		} `json:"__schema"`
	}
	if err := json.Unmarshal(resp.Data, &s); err != nil {
		t.Fatal(err)
	}
	out := map[string]rootField{}
	for _, f := range s.Schema.QueryType.Fields {
		out["Query."+f.Name] = f
	}
	for _, f := range s.Schema.MutationType.Fields {
		out["Mutation."+f.Name] = f
	}
	return out
}

// document calls a root field with a value for each argument it requires,
// selecting __typename of an object.
func document(op string, f rootField) string {
	var args []string
	for _, a := range f.Args {
		if a.Type.Kind != "NON_NULL" {
			continue
		}
		v := `"x"`
		switch {
		case a.Type.OfType.Kind == "LIST":
			v = "[]"
		case a.Type.OfType.Kind == "INPUT_OBJECT":
			v = "{}"
		case a.Type.OfType.Name == "Int":
			v = "1"
		case a.Type.OfType.Name == "Boolean":
			v = "true"
		}
		args = append(args, a.Name+": "+v)
	}
	call := f.Name
	if len(args) > 0 {
		call += "(" + strings.Join(args, ", ") + ")"
	}
	if f.Type.named().Kind == "OBJECT" {
		call += " { __typename }"
	}
	return op + " { " + call + " }"
}

// Every root field of the schema has a rule, and every rule a field: a field
// added to the schema without one would be refused to everyone, and the
// table is the one place that says who may call what.
func TestEveryRootFieldHasARule(t *testing.T) {
	fields := rootFields(t)
	for f := range fields {
		if _, ok := fieldAccess[f]; !ok {
			t.Errorf("%s has no access rule", f)
		}
	}
	for f := range fieldAccess {
		if _, ok := fields[f]; !ok {
			t.Errorf("the access rule of %s names no field of the schema", f)
		}
	}
	if len(fields) < 40 {
		t.Errorf("introspection found %d root fields, fewer than the schema has", len(fields))
	}
}

// Every root field is an admin's, and triggerScan the service account's too:
// a viewer, an addon, the service account (on anything else) and a context
// without a caller are refused with FORBIDDEN before the field reads or
// changes anything. The resolvers have no store and no services here, so a
// field that did not refuse first would fail some other way.
func TestRootFieldsRefuseWhomTheyAreNotFor(t *testing.T) {
	schema := MustSchema(NewResolver(nil, testConfig, Services{}))
	fields := rootFields(t)
	names := make([]string, 0, len(fields))
	for f := range fields {
		names = append(names, f)
	}
	sort.Strings(names)
	for _, name := range names {
		op, field := "query", fields[name]
		if strings.HasPrefix(name, "Mutation.") {
			op = "mutation"
		}
		doc := document(op, field)
		callers := map[string]context.Context{"a viewer": as(viewer), "an addon": as(addon), "no caller": context.Background()}
		if fieldAccess[name] == auth.Admin {
			callers["the service account"] = as(service)
		}
		for who, ctx := range callers {
			resp := schema.Exec(ctx, doc, "", nil)
			if len(resp.Errors) != 1 || resp.Errors[0].Extensions["code"] != "FORBIDDEN" ||
				resp.Errors[0].Message != "forbidden: "+field.Name+" requires "+testConfig.Policy().Requirement(fieldAccess[name]) {
				t.Errorf("%s, %s: %v, want it refused with FORBIDDEN", who, doc, resp.Errors)
			}
		}
	}
}

// fakeScanner records the scans it is asked for.
type fakeScanner struct{ sources []string }

func (f *fakeScanner) Trigger(_ context.Context, source string) (string, error) {
	f.sources = append(f.sources, source)
	return "", fmt.Errorf("no store to record the scan in")
}

// The scan Job runs triggerScan with the platform's service account, and an
// admin may too; the service account may do nothing else.
func TestTriggerScanIsTheServiceAccountsToo(t *testing.T) {
	sc := &fakeScanner{}
	schema := MustSchema(NewResolver(nil, testConfig, Services{Scanner: sc}))
	for who, ctx := range map[string]context.Context{"the service account": as(service), "an admin": as(admin)} {
		resp := schema.Exec(ctx, `mutation { triggerScan { id } }`, "", nil)
		if len(resp.Errors) == 0 || resp.Errors[0].Extensions["code"] == "FORBIDDEN" {
			t.Errorf("%s: %v, want the scan triggered", who, resp.Errors)
		}
	}
	if strings.Join(sc.sources, " ") != "nfs nfs" {
		t.Errorf("scans triggered: %q, want two, of nfs", sc.sources)
	}
	resp := schema.Exec(as(viewer), `mutation { triggerScan { id } }`, "", nil)
	if len(resp.Errors) != 1 || resp.Errors[0].Message != "forbidden: triggerScan requires the zaentrum-admin role or the platform's service account" {
		t.Errorf("a viewer's scan: %v", resp.Errors)
	}
	if len(sc.sources) != 2 {
		t.Errorf("a viewer's scan was triggered")
	}
}

// A refusal is a GraphQL error at the field, with the code FORBIDDEN and the
// admin role in its extensions, and the field's service is never called.
func TestARefusalIsAGraphQLError(t *testing.T) {
	rm := &fakeRemover{}
	schema := MustSchema(NewResolver(nil, testConfig, Services{Remover: rm}))
	resp := schema.Exec(as(viewer), `mutation { deleteItem(id: "m1", deleteFiles: true) { deleted } }`, "", nil)
	if rm.id != "" {
		t.Errorf("the remover was asked to remove %q for a viewer", rm.id)
	}
	if string(resp.Data) != "null" || len(resp.Errors) != 1 {
		t.Fatalf("data %s, errors %v; want no data and one error", resp.Data, resp.Errors)
	}
	e := resp.Errors[0]
	got, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"message":"forbidden: deleteItem requires the zaentrum-admin role",` +
		`"path":["deleteItem"],"extensions":{"code":"FORBIDDEN","role":"zaentrum-admin"}}`
	if string(got) != want {
		t.Errorf("the error\n got  %s\n want %s", got, want)
	}
}

// A root field without a rule is refused to everyone, an admin too.
func TestAFieldWithoutARuleIsRefused(t *testing.T) {
	r := NewResolver(nil, testConfig, Services{})
	err := r.allow(as(admin), "Mutation.dropEverything")
	if err == nil || err.Error() != "forbidden: dropEverything requires an access rule, and it has none" {
		t.Errorf("an admin's call of a field without a rule: %v", err)
	}
	if err := r.allow(as(admin), "Mutation.deleteItem"); err != nil {
		t.Errorf("an admin's deleteItem: %v", err)
	}
}
