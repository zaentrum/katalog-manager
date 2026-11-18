package library

import (
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Every writer names its source: an original-deleted event that names no
// source is not written (the schemas refuse one from 2026-11-16 on, one
// event a source, so that the error shows when it is written), and each of
// katalog-manager's writers of one names the source whose original it
// deletes - the adopt's of an original gone before the library was recorded
// (goneBefore), the retire job's (prepare) and a superseded original's
// (prepareSuperseded): no other function writes one.
func TestEveryWriterOfADeletionNamesItsSource(t *testing.T) {
	item := t.TempDir()
	reason := "a test"
	ev := Event{ID: "1f2e3d4c-0000-4000-8000-000000000009", At: time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC),
		Kind: EventOriginalDeleted, VersionID: "9a2e0000-0000-4000-8000-000000000002", Reason: &reason}
	for _, none := range []string{"", "  "} {
		ev.SourceID = none
		if _, err := WriteEvent(item, ev); err == nil || !strings.Contains(err.Error(), "names the source") {
			t.Errorf("an original-deleted event of the source %q: %v; want it not written", none, err)
		}
	}
	if entries, _ := os.ReadDir(filepath.Join(item, "events")); len(entries) > 0 {
		t.Errorf("an event that names no source left %d folders", len(entries))
	}
	ev.SourceID = "0b6c0000-0000-4000-8000-000000000003"
	if _, err := WriteEvent(item, ev); err != nil {
		t.Errorf("an event that names its source: %v", err)
	}
	// the writers, as the package's code builds their events
	want := map[string]string{"goneBefore": "s.SourceID", "prepare": "s.ID", "prepareSuperseded": "s.ID"}
	got := map[string]string{}
	fset := token.NewFileSet()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				lit, ok := n.(*ast.CompositeLit)
				if !ok {
					return true
				}
				if id, isIdent := lit.Type.(*ast.Ident); !isIdent || id.Name != "Event" {
					return true
				}
				fields := map[string]string{}
				for _, e := range lit.Elts {
					if kv, ok := e.(*ast.KeyValueExpr); ok && isIdentExpr(kv.Key) {
						var b strings.Builder
						if err := printer.Fprint(&b, fset, kv.Value); err != nil {
							t.Fatal(err)
						}
						fields[kv.Key.(*ast.Ident).Name] = b.String()
					}
				}
				if fields["Kind"] == "EventOriginalDeleted" {
					if _, twice := got[fn.Name.Name]; twice {
						t.Errorf("%s writes more than one original-deleted event", fn.Name.Name)
					}
					got[fn.Name.Name] = fields["SourceID"]
				}
				return true
			})
		}
	}
	for fn, src := range want {
		if got[fn] != src {
			t.Errorf("the writer %s names the source %q; want %s, the source whose original it deletes", fn, got[fn], src)
		}
	}
	for fn, src := range got {
		if _, known := want[fn]; !known {
			t.Errorf("%s writes an original-deleted event (its source %q), and no test pins it", fn, src)
		}
	}
}

// isIdentExpr reports whether e is a name.
func isIdentExpr(e ast.Expr) bool {
	_, ok := e.(*ast.Ident)
	return ok
}
